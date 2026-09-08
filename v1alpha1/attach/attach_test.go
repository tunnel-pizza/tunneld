package attach

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"k8s.io/cri-streaming/pkg/streaming/remotecommand"
)

// fakeTarget stands in for a container. Every failure mode this package has to
// handle — no TTY, no stdin, a stream that ends — is a field here rather than a
// container somebody has to arrange, which is what makes them testable at all.
type fakeTarget struct {
	name   string
	tty    bool
	stdin  bool
	out    string      // written to stdout as soon as the attach begins
	seenIn chan string // what arrived on stdin
	seenSz chan remotecommand.TerminalSize
	done   chan struct{} // closed when AttachContainer returns
}

func newFakeTarget(name string, tty, stdin bool) *fakeTarget {
	return &fakeTarget{
		name:   name,
		tty:    tty,
		stdin:  stdin,
		seenIn: make(chan string, 4),
		seenSz: make(chan remotecommand.TerminalSize, 4),
		done:   make(chan struct{}),
	}
}

func (f *fakeTarget) Name() string { return f.name }
func (f *fakeTarget) TTY() bool    { return f.tty }
func (f *fakeTarget) Stdin() bool  { return f.stdin }
func (f *fakeTarget) Close() error { return nil }

func (f *fakeTarget) AttachContainer(ctx context.Context, _, _, _ string, in io.Reader, out, _ io.WriteCloser, _ bool, resize <-chan remotecommand.TerminalSize) error {
	defer close(f.done)
	if f.out != "" {
		_, _ = io.WriteString(out, f.out)
	}
	go func() {
		for size := range resize {
			f.seenSz <- size
		}
	}()
	// Stdin on a goroutine rather than inline, because that is the shape a
	// real provider has: it copies both directions at once and ends on its
	// context, not on stdin running dry. Reading it inline would make this
	// fake pass the shutdown tests below for a reason no real Target shares.
	if in != nil {
		go func() {
			buf := make([]byte, 64)
			for {
				n, err := in.Read(buf)
				if n > 0 {
					f.seenIn <- string(buf[:n])
				}
				if err != nil {
					return
				}
			}
		}()
	}
	<-ctx.Done()
	return nil
}

// serveFake starts a Server on a fake target and tears it down with the test.
// testBanner is the build line a served terminal carries. A fixture rather
// than the real one, which changes with every build.
const testBanner = "tunneld test (libtunnel test, built test)"

func serveFake(t *testing.T, target Target) *Server {
	t.Helper()
	return serveFakeOn(t, t.Context(), target)
}

// serveFakeOn is serveFake on a context the caller holds the cancel for, which
// is how a test shuts the tunnel down rather than the test ending.
func serveFakeOn(t *testing.T, ctx context.Context, target Target) *Server {
	t.Helper()
	s, err := Serve(ctx, target, testBanner, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestPage pins that the tunnel's own address answers with the terminal page
// and that nothing else on the origin answers at all. The origin exists to
// serve exactly two paths; anything else reaching it is a bug upstream, and a
// 404 says so instead of quietly returning the terminal page again.
func TestPage(t *testing.T) {
	s := serveFake(t, newFakeTarget("api", true, true))

	cases := []struct {
		name   string
		path   string
		status int
		want   string
	}{
		{"the root serves the terminal", "/", http.StatusOK, "@xterm/xterm@"},
		// The page is named by the terminal, not by the template: the frame
		// sets its window title once one is attached and the page raises it
		// to document.title. Until then there is nothing to say but this.
		{"the page is named until the terminal names it", "/", http.StatusOK, "<title>tunneld · attach</title>"},
		{"the page listens for the name", "/", http.StatusOK, "onTitleChange"},
		{"anything else is not found", "/favicon.ico", http.StatusNotFound, ""},
		{"a nested path is not found", "/app/index.html", http.StatusNotFound, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Get(s.URL().String() + tc.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("GET %s = %d, want %d", tc.path, resp.StatusCode, tc.status)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if tc.want != "" && !strings.Contains(string(body), tc.want) {
				t.Errorf("GET %s body does not contain %q", tc.path, tc.want)
			}
		})
	}
}

// TestURLIsLoopback pins that the origin never leaves the machine. The page it
// serves is unauthenticated by design; the only thing keeping it off the local
// network is the address it binds.
func TestURLIsLoopback(t *testing.T) {
	s := serveFake(t, newFakeTarget("api", true, true))
	u := s.URL()
	if u.Scheme != "http" {
		t.Errorf("scheme = %q, want http", u.Scheme)
	}
	if !strings.HasPrefix(u.Host, "127.0.0.1:") {
		t.Errorf("host = %q, want a 127.0.0.1 port", u.Host)
	}
}

// dial opens a v4.channel.k8s.io socket to a Server and closes it with the
// test. The subprotocol is what selects binary channel framing; without it the
// server negotiates the base64 variant and every assertion below shifts.
func dial(t *testing.T, s *Server) *websocket.Conn {
	t.Helper()
	d := websocket.Dialer{Subprotocols: []string{"v4.channel.k8s.io"}}
	c, resp, err := d.Dial("ws://"+s.URL().Host+"/attach", nil)
	if err != nil {
		t.Fatalf("dial /attach: %v", err)
	}
	if resp != nil {
		_ = resp.Body.Close()
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestCrossOriginHandshake pins the gate that keeps the socket from being the
// hole the loopback bind is assumed to have closed.
//
// A websocket handshake is not covered by the same-origin policy and the
// wsstream handshake never reads Origin, so without this any page in the
// operator's browser could dial ws://127.0.0.1:<port>/attach and hold the
// container's stdin. Dialed with the real handshake rather than a bare GET,
// since the whole question is what the upgrade does with the header.
//
// The absent-Origin row is as load-bearing as the refusal: no Origin means a
// non-browser client, and breaking curl buys nothing.
//
// The rows carrying a Host are the shape a request arrives in through the
// tunnel, where the page was served from the public hostname and libtunnel
// forwards the inbound Host rather than rewriting it to the origin's. It is
// also the multiview shape: a tile's document is served from that same
// hostname, so its socket's Origin is the same string again.
func TestCrossOriginHandshake(t *testing.T) {
	cases := []struct {
		name string
		// host overrides the Host header; "" leaves the dialed 127.0.0.1:port.
		host string
		// origin is given the effective Host. "" sends no Origin at all.
		origin func(host string) string
		want   bool // whether the handshake should succeed
	}{
		{"a foreign origin is refused", "", func(string) string { return "https://evil.example" }, false},
		{"a foreign origin on loopback is refused", "", func(string) string { return "http://127.0.0.1:1" }, false},
		{"an unparsable origin is refused", "", func(string) string { return "://" }, false},
		{"the page's own origin is accepted", "", func(host string) string { return "http://" + host }, true},
		{"no origin at all is accepted", "", func(string) string { return "" }, true},
		{"the tunnel's public hostname is accepted", "demo.tunnel.pizza",
			func(host string) string { return "https://" + host }, true},
		{"a foreign origin through the tunnel is refused", "demo.tunnel.pizza",
			func(string) string { return "https://evil.example" }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A Server per case, not one shared across the table: an accepted
			// handshake attaches, and a fake target that has been attached
			// twice is a fake with two goroutines closing one done channel.
			s := serveFake(t, newFakeTarget("api", true, true))

			header := http.Header{}
			host := s.URL().Host
			if tc.host != "" {
				header.Set("Host", tc.host)
				host = tc.host
			}
			if o := tc.origin(host); o != "" {
				header.Set("Origin", o)
			}
			d := websocket.Dialer{Subprotocols: []string{"v4.channel.k8s.io"}}
			c, resp, err := d.Dial("ws://"+s.URL().Host+"/attach", header)
			if resp != nil {
				defer resp.Body.Close()
			}
			if c != nil {
				defer func() { _ = c.Close() }()
			}

			if tc.want {
				if err != nil {
					t.Fatalf("handshake = %v, want it to succeed", err)
				}
				return
			}
			if err == nil {
				t.Fatal("handshake succeeded, want it refused")
			}
			if resp == nil {
				t.Fatalf("handshake failed without a response: %v", err)
			}
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
			}
		})
	}
}

// readFrame reads one binary frame and splits it into channel and payload.
func readFrame(t *testing.T, c *websocket.Conn) (byte, []byte) {
	t.Helper()
	kind, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if kind != websocket.BinaryMessage {
		t.Fatalf("frame type = %d, want binary", kind)
	}
	if len(data) == 0 {
		t.Fatal("frame is empty, want at least a channel byte")
	}
	return data[0], data[1:]
}

// writeFrame sends one channel-prefixed binary frame.
// stdoutUntil reads stdout frames until what they have carried between them
// contains want.
//
// No single frame is the answer any more. A viewer's socket carries a frame
// drawn around the screen rather than the container's own bytes, so it opens
// with the renderer asking the terminal what it supports and then arrives in
// as many pieces as the renderer chose to write.
func stdoutUntil(t *testing.T, c *websocket.Conn, want string) {
	t.Helper()

	// Bounded here rather than left to the test binary's own timeout. What
	// this waits for is a frame that may never be written — a renderer that
	// has stopped drawing, or one stripping the escapes being looked for —
	// and an unbounded read turns either into a hung package and a stack
	// dump instead of a sentence saying what arrived.
	deadline := time.Now().Add(10 * time.Second)
	var seen strings.Builder
	for !strings.Contains(seen.String(), want) {
		if err := c.SetReadDeadline(deadline); err != nil {
			t.Fatalf("set a read deadline: %v", err)
		}
		kind, data, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("never saw %q on stdout; got %q (%v)", want, seen.String(), err)
		}
		if kind == websocket.BinaryMessage && len(data) > 0 && data[0] == 1 {
			seen.Write(data[1:])
		}
	}
}

func writeFrame(t *testing.T, c *websocket.Conn, channel byte, payload string) {
	t.Helper()
	if err := c.WriteMessage(websocket.BinaryMessage, append([]byte{channel}, payload...)); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

// TestEstablishedFrame pins the one-byte frame the server sends on connect.
// The page keys "the terminal is live" on it — it is the only signal that the
// attach actually began, since a healthy container may say nothing for hours.
func TestEstablishedFrame(t *testing.T) {
	s := serveFake(t, newFakeTarget("api", true, true))
	channel, payload := readFrame(t, dial(t, s))
	if channel != 1 {
		t.Errorf("established frame channel = %d, want 1 (stdout)", channel)
	}
	if len(payload) != 0 {
		t.Errorf("established frame payload = %q, want empty", payload)
	}
}

// TestStdout pins that what the target writes reaches the browser on the
// stdout channel, unaltered.
func TestStdout(t *testing.T) {
	target := newFakeTarget("api", true, true)
	target.out = "hello from pid 1\r\n"
	c := dial(t, serveFake(t, target))

	readFrame(t, c) // the established frame

	// What a viewer receives is the screen, not the bytes that produced it:
	// the session attached before this connection existed, so what the target
	// wrote is already on the screen and arrives drawn into a frame. It is
	// read across frames because the first of them are the renderer asking
	// the terminal what it supports, and a frame is written in pieces.
	stdoutUntil(t, c, "hello from pid 1")
}

// TestColourSurvivesTheFrame pins that the frame does not flatten the screen
// it is drawing.
//
// Everything a viewer sees is now re-rendered rather than passed through, and
// the renderer applies a colour profile on the way out. It has nothing to
// detect that profile from — the output is a websocket, not a terminal — so
// left to itself it answers NoTTY and strips every escape the container wrote:
// colour, bold, and the frame's own status line alike, for a screen that is
// entirely monochrome and looks like nothing worse than a dull app.
//
// Driven through the real socket because that is the only place it goes wrong.
// A frame's View returns the styled string either way; it is the renderer
// underneath that does or does not keep it.
func TestColourSurvivesTheFrame(t *testing.T) {
	target := newFakeTarget("api", true, true)
	target.out = "\x1b[31mred\x1b[0m\r\n"
	c := dial(t, serveFake(t, target))

	readFrame(t, c) // the established frame
	stdoutUntil(t, c, "\x1b[31mred")
}

// TestStdin pins that keystrokes reach the target.
func TestStdin(t *testing.T) {
	target := newFakeTarget("api", true, true)
	c := dial(t, serveFake(t, target))
	readFrame(t, c) // the established frame

	writeFrame(t, c, 0, "echo hi\n")

	// What reaches the target is no longer a copy of what the browser sent: the
	// frame decodes the keystrokes and hands them to the emulator, which
	// encodes what a terminal in the app's current modes would write. For
	// ordinary typing the two are the same bytes, which is the point — a frame
	// standing in the way must not change what the shell reads.
	// It arrives in as many pieces as the emulator's reader happened to hand
	// over — one byte or all of them, and neither is worth pinning — so what
	// is asserted is that the pieces reassemble to exactly what was typed.
	const want = "echo hi\n"
	var read strings.Builder
	for read.String() != want {
		select {
		case got := <-target.seenIn:
			read.WriteString(got)
			if !strings.HasPrefix(want, read.String()) {
				t.Fatalf("target read %q, want it building %q", read.String(), want)
			}
		case <-t.Context().Done():
			t.Fatalf("target saw %q, want %q", read.String(), want)
		}
	}
}

// TestResize pins the resize wire format. It is JSON on a channel of its own,
// read by a streaming decoder, so successive sizes need no framing — which is
// what lets the page use it as a heartbeat.
func TestResize(t *testing.T) {
	target := newFakeTarget("api", true, true)
	c := dial(t, serveFake(t, target))
	readFrame(t, c) // the established frame

	writeFrame(t, c, 4, `{"Width":100,"Height":40}`)
	writeFrame(t, c, 4, `{"Width":120,"Height":50}`)

	// The target is told the pane, not the window: the frame keeps
	// chromeHeight rows and chromeWidth columns for its border, and a
	// container sized to the whole window would draw its last row and column
	// underneath one.
	//
	// The session's settled window reaches the target too, and at no fixed
	// point: a frame has no terminal to measure, so it answers the renderer's
	// empty first report with whatever the session had settled on, and that
	// answer races the page's own first size. Skipped rather than ordered,
	// because which of them lands first is not a property worth pinning.
	settled := remotecommand.TerminalSize{Width: defaultCols - chromeWidth, Height: defaultRows - chromeHeight}
	want := []remotecommand.TerminalSize{
		{Width: 100 - chromeWidth, Height: 40 - chromeHeight},
		{Width: 120 - chromeWidth, Height: 50 - chromeHeight},
	}
	for _, w := range want {
		for {
			select {
			case got := <-target.seenSz:
				if got == settled {
					continue
				}
				if got != w {
					t.Errorf("size = %+v, want %+v", got, w)
				}
			case <-t.Context().Done():
				t.Fatalf("target never saw %+v", w)
			}
			break
		}
	}
}

// TestNoticeOnThePage pins where the notice lives: in the served HTML, as an
// element of its own, and nowhere at all for a container that has nothing
// wrong with it.
//
// The page is the right home because the notice is not something the container
// said. Written into the stream it would scroll away from the reader who needs
// it, and come back on every reconnect for the one who does not; rendered as
// chrome it simply stays true for as long as the tab is open. The absence case
// is as load-bearing as the others — a container started with -it must get no
// element and no gap, because a terminal that gives up a row to say nothing is
// worse than no notice at all.
//
// It is now also the sole pin for the notice text itself, since the switch
// that computes it lives inline in the closure and has no test of its own.
func TestNoticeOnThePage(t *testing.T) {
	cases := []struct {
		name  string
		tty   bool
		stdin bool
		want  string // "" means: no notice element at all
	}{
		{"a full terminal renders no notice", true, true, ""},
		{"no tty", false, true, "no TTY (started without -t) — no line editing, no resize"},
		{"no stdin", true, false, "stdin closed (started without -i) — keystrokes go nowhere"},
		{"neither", false, false, "no TTY and no stdin (started without -it) — output only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := serveFake(t, newFakeTarget("api", tc.tty, tc.stdin))
			resp, err := http.Get(s.URL().String() + "/")
			if err != nil {
				t.Fatalf("GET /: %v", err)
			}
			defer resp.Body.Close()
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			body := string(raw)

			// The class is what the absence case turns on: asserting the text
			// is missing would also pass if the element rendered empty, which
			// is the failure that costs a row and says nothing.
			if tc.want == "" {
				if strings.Contains(body, `class="degraded"`) {
					t.Errorf("page carries a notice element for a container that earns none")
				}
				return
			}
			if !strings.Contains(body, `class="degraded"`) {
				t.Errorf("page has no notice element")
			}
			if !strings.Contains(body, tc.want) {
				t.Errorf("page does not contain %q", tc.want)
			}
		})
	}
}

// TestSessionOutlivesAVisitor pins the other half of the shared session: a tab
// closing takes the viewer with it and nothing else.
//
// This is what a refresh is made of. The attach the container sees is opened
// once and never restarted, so from inside the container a page reload is not
// an event at all — no second attach, no replayed backlog, and with OpenStdin
// no re-attach for the app to notice. The next visitor is handed the screen
// instead, which is why the assertion is that they can still see what was
// written before they arrived.
//
// The viewer itself must still be reaped, or an abandoned tab costs a
// goroutine and a queue for the life of the run.
func TestSessionOutlivesAVisitor(t *testing.T) {
	ctx, shutdown := context.WithCancel(t.Context())
	defer shutdown()

	target := newFakeTarget("api", true, true)
	target.out = "hello from pid 1\r\n"
	s := serveFakeOn(t, ctx, target)

	first := dial(t, s)
	readFrame(t, first) // the established frame
	readFrame(t, first) // the screen
	_ = first.Close()

	select {
	case <-target.done:
		t.Fatal("closing a tab ended the shared attach, want it to outlive the viewer")
	case <-time.After(500 * time.Millisecond):
	}

	// The viewer is gone even though the session is not.
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.session.mu.Lock()
		n := len(s.session.viewers)
		s.session.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d viewers still registered after the tab closed", n)
		}
		time.Sleep(10 * time.Millisecond)
	}

	second := dial(t, s)
	readFrame(t, second) // the established frame
	stdoutUntil(t, second, "hello from pid 1")
}

// TestSessionEnds pins the two ways the shared attach is over, from the point
// of view of a Target that is only reading output — the state a quiet
// container leaves it in for hours at a time.
//
// Closing a tab is deliberately not one of them; TestSessionOutlivesAVisitor
// covers that. Both routes here are the run itself ending, and neither is
// something a Target can see for itself: the socket is hijacked, so shutting
// the HTTP server down does not touch it, and a Target's own Close only reaps
// what is idle. Asserted on a deadline rather than a bare receive, so a
// regression fails in five seconds instead of hanging the lane.
func TestSessionEnds(t *testing.T) {
	cases := []struct {
		name  string
		stdin bool
		end   func(s *Server, c *websocket.Conn, shutdown context.CancelFunc)
	}{
		{"the tunnel shuts down", true, func(_ *Server, _ *websocket.Conn, shutdown context.CancelFunc) { shutdown() }},
		// Close with nobody having cancelled anything, which is the shape of
		// the tunnel failing on its own: run's defer closes what it bound and
		// the context is still live. Neither close inside reaches a live
		// session — net/http leaves hijacked connections alone by design, and
		// a Target's Close only reaps what is idle — so Close has to end the
		// session itself or mean two different things on two paths.
		{"the server is closed with the context still live", true,
			func(s *Server, _ *websocket.Conn, _ context.CancelFunc) { _ = s.Close() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, shutdown := context.WithCancel(t.Context())
			defer shutdown()

			target := newFakeTarget("api", true, tc.stdin)
			s := serveFakeOn(t, ctx, target)
			c := dial(t, s)
			readFrame(t, c) // the established frame

			tc.end(s, c, shutdown)

			select {
			case <-target.done:
			case <-time.After(5 * time.Second):
				t.Fatal("AttachContainer never returned after the session ended")
			}
		})
	}
}

// stubTarget is a container that never says anything. Bind only needs a
// Target to stand a server on; what the stream does is tested elsewhere in
// this package.
type stubTarget struct {
	name   string
	closed bool
}

func (s *stubTarget) Name() string { return s.name }
func (s *stubTarget) TTY() bool    { return true }
func (s *stubTarget) Stdin() bool  { return true }
func (s *stubTarget) Close() error { s.closed = true; return nil }
func (s *stubTarget) AttachContainer(ctx context.Context, _, _, _ string, _ io.Reader, _, _ io.WriteCloser, _ bool, _ <-chan remotecommand.TerminalSize) error {
	<-ctx.Done()
	return nil
}

// stubTargets stands in for the daemon: it records every reference it was
// asked for and answers with a stubTarget. failOn, when positive, makes that
// call fail instead — the unwinding case needs one success before one
// failure.
type stubTargets struct {
	asked  []string
	failOn int
	opened []*stubTarget
}

func (s *stubTargets) Open(_ context.Context, ref string, _ *slog.Logger) (Target, error) {
	s.asked = append(s.asked, ref)
	if s.failOn > 0 && len(s.asked) == s.failOn {
		return nil, errors.New("no such container")
	}
	target := &stubTarget{name: ref}
	s.opened = append(s.opened, target)
	return target, nil
}

// mustURLs parses raw as URLs, failing the test on the first one that is not.
func mustURLs(t *testing.T, raw ...string) []*url.URL {
	t.Helper()
	got := make([]*url.URL, 0, len(raw))
	for _, s := range raw {
		u, err := url.Parse(s)
		if err != nil {
			t.Fatalf("url.Parse(%q): %v", s, err)
		}
		got = append(got, u)
	}
	return got
}

// TestBindKeepsOrder pins the invariant the whole feature rests on: the
// dialable list is the same length and the same order as what the operator
// typed, so index n still means origin n everywhere downstream — ?n routing,
// PublicURL, the reported map, the multiview tiles.
func TestBindKeepsOrder(t *testing.T) {
	targets := &stubTargets{}
	display := mustURLs(t, "http://localhost:3000", "dockerd://api", "http://localhost:4000", "dockerd://db")

	dialable, closer, err := New(WithTargets(targets)).Bind(t.Context(), display, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	defer closer.Close()

	if len(dialable) != len(display) {
		t.Fatalf("dialable has %d entries, want %d", len(dialable), len(display))
	}
	if got := dialable[0].String(); got != "http://localhost:3000" {
		t.Errorf("dialable[0] = %q, want the http origin unchanged", got)
	}
	if got := dialable[2].String(); got != "http://localhost:4000" {
		t.Errorf("dialable[2] = %q, want the http origin unchanged", got)
	}
	for _, i := range []int{1, 3} {
		if !strings.HasPrefix(dialable[i].Host, "127.0.0.1:") {
			t.Errorf("dialable[%d] = %q, want a loopback origin", i, dialable[i])
		}
	}
	if want := []string{"api", "db"}; !slices.Equal(targets.asked, want) {
		t.Errorf("opened %q, want %q", targets.asked, want)
	}
}

// TestAnnounceReachesTheRightTerminal pins the other half of that invariant.
//
// A container is bound before the tunnel exists, so where it answers from
// outside is not known until later, and the only thing connecting a server to
// its address is the place its origin had in the list. Announce is handed the
// addresses in that order, and hands each server the one at its own index —
// give origin 3's address to origin 1 and every framed terminal names a URL
// that reaches a different container.
func TestAnnounceReachesTheRightTerminal(t *testing.T) {
	targets := &stubTargets{}
	display := mustURLs(t, "http://localhost:3000", "dockerd://api", "http://localhost:4000", "dockerd://db")

	_, closer, err := New(WithTargets(targets)).Bind(t.Context(), display, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	defer closer.Close()

	announcer, ok := closer.(interface{ Announce(public []string) })
	if !ok {
		t.Fatalf("Bind returned %T, want something the root can announce to", closer)
	}

	// One address per origin, in the order the operator typed them.
	announcer.Announce([]string{
		"https://example.test/?0",
		"https://example.test/?1",
		"https://example.test/?2",
		"https://example.test/?3",
	})

	servers, ok := closer.(bound)
	if !ok {
		t.Fatalf("closer is %T, want the bound list", closer)
	}
	if len(servers) != 2 {
		t.Fatalf("bound %d servers, want the two containers", len(servers))
	}
	for _, o := range servers {
		want := fmt.Sprintf("https://example.test/?%d", o.at)
		if got := o.srv.session.announced(); got != want {
			t.Errorf("the server at index %d was told %q, want %q", o.at, got, want)
		}
	}

	// And a caller with fewer addresses than origins leaves them unset rather
	// than reaching for one that is not there.
	short := bound{{at: 9, srv: servers[0].srv}}
	short.Announce([]string{"https://example.test/?0"})
	if got := servers[0].srv.session.announced(); got == "https://example.test/?0" {
		t.Error("an index past the addresses given took the first one, want it left alone")
	}
}

// TestBindWithoutContainers pins that a command with no dockerd:// URL starts
// nothing at all — the feature is inert until somebody asks for it.
func TestBindWithoutContainers(t *testing.T) {
	targets := &stubTargets{}
	display := mustURLs(t, "http://localhost:3000", "http://localhost:4000")

	dialable, closer, err := New(WithTargets(targets)).Bind(t.Context(), display, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	defer closer.Close()

	if len(targets.asked) != 0 {
		t.Errorf("opened %q, want nothing", targets.asked)
	}
	for i, u := range dialable {
		if u != display[i] {
			t.Errorf("dialable[%d] = %q, want the original origin", i, u)
		}
	}
}

// TestBindUnwindsOnFailure pins that a later container failing does not leave
// an earlier one's server listening. The command is about to return an error
// and exit; a leaked goroutine holding a port would outlive it in an
// embedding program.
func TestBindUnwindsOnFailure(t *testing.T) {
	targets := &stubTargets{failOn: 2}
	display := mustURLs(t, "dockerd://api", "dockerd://missing")

	if _, _, err := New(WithTargets(targets)).Bind(t.Context(), display, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("Bind succeeded, want an error")
	}
	if len(targets.opened) != 1 {
		t.Fatalf("opened %d targets, want 1", len(targets.opened))
	}
	if !targets.opened[0].closed {
		t.Error("the first container's target was left open")
	}
}

// TestBindWithoutTargets pins that a dockerd:// origin met with no Targets
// configured fails with a message naming the missing dependency, rather than
// panicking on a nil interface.
func TestBindWithoutTargets(t *testing.T) {
	display := mustURLs(t, "dockerd://api")

	_, _, err := New().Bind(t.Context(), display, slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("Bind succeeded, want an error")
	}
	if want := "attach: no Targets configured to open dockerd://api"; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}
