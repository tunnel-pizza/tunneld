package attach

import (
	"context"
	"io"
	"log/slog"
	"net/http"
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
func serveFake(t *testing.T, target Target) *Server {
	t.Helper()
	return serveFakeOn(t, t.Context(), target)
}

// serveFakeOn is serveFake on a context the caller holds the cancel for, which
// is how a test shuts the tunnel down rather than the test ending.
func serveFakeOn(t *testing.T, ctx context.Context, target Target) *Server {
	t.Helper()
	s, err := Serve(ctx, target, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestPage pins that the tunnel's own address answers with the terminal page
// and that nothing else on the origin answers at all. The origin exists to
// serve exactly two paths; anything else reaching it is a bug upstream, and a
// 404 says so instead of quietly returning the shell again.
func TestPage(t *testing.T) {
	s := serveFake(t, newFakeTarget("api", true, true))

	cases := []struct {
		name   string
		path   string
		status int
		want   string
	}{
		{"the root serves the terminal", "/", http.StatusOK, "@xterm/xterm@"},
		{"the root names the container", "/", http.StatusOK, "api"},
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
	channel, payload := readFrame(t, c)
	if channel != 1 {
		t.Errorf("channel = %d, want 1 (stdout)", channel)
	}
	if string(payload) != target.out {
		t.Errorf("payload = %q, want %q", payload, target.out)
	}
}

// TestStdin pins that keystrokes reach the target.
func TestStdin(t *testing.T) {
	target := newFakeTarget("api", true, true)
	c := dial(t, serveFake(t, target))
	readFrame(t, c) // the established frame

	writeFrame(t, c, 0, "echo hi\n")
	select {
	case got := <-target.seenIn:
		if got != "echo hi\n" {
			t.Errorf("target read %q, want %q", got, "echo hi\n")
		}
	case <-t.Context().Done():
		t.Fatal("target never saw stdin")
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

	want := []remotecommand.TerminalSize{{Width: 100, Height: 40}, {Width: 120, Height: 50}}
	for _, w := range want {
		select {
		case got := <-target.seenSz:
			if got != w {
				t.Errorf("size = %+v, want %+v", got, w)
			}
		case <-t.Context().Done():
			t.Fatalf("target never saw %+v", w)
		}
	}
}

// TestDegradedNotice pins the line a container earns by having been started
// without -t or -i. Half of docker attach's behaviour is decided before
// tunneld is involved, and a terminal that silently swallows keystrokes is the
// one outcome worth spending a line to prevent.
func TestDegradedNotice(t *testing.T) {
	cases := []struct {
		name  string
		tty   bool
		stdin bool
		want  string
	}{
		{"a full terminal says nothing", true, true, ""},
		{"no tty", false, true, "no TTY (started without -t) — no line editing, no resize"},
		{"no stdin", true, false, "stdin closed (started without -i) — keystrokes go nowhere"},
		{"neither", false, false, "no TTY and no stdin (started without -it) — output only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := degraded(newFakeTarget("api", tc.tty, tc.stdin))
			if got != tc.want {
				t.Errorf("degraded = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNoticeReachesTheTerminal pins that the notice is written to the stream
// rather than baked into the page, so it appears in order with the container's
// own first output and survives however the page was loaded.
func TestNoticeReachesTheTerminal(t *testing.T) {
	target := newFakeTarget("api", false, false)
	c := dial(t, serveFake(t, target))
	readFrame(t, c) // the established frame

	channel, payload := readFrame(t, c)
	if channel != 1 {
		t.Errorf("channel = %d, want 1 (stdout)", channel)
	}
	if !strings.Contains(string(payload), "output only") {
		t.Errorf("first frame = %q, want the degraded notice", payload)
	}
}

// TestSessionEnds pins the two ways a session is over, from the point of view
// of a Target that is only reading output — the state a quiet container leaves
// it in for hours at a time.
//
// Neither route is something such a Target can see for itself. The context
// ServeAttach hands it is the request's, and Go cancels that when the handler
// returns, which cannot happen while the handler is still inside the Target;
// the socket is hijacked, so shutting the HTTP server down does not touch it
// either. Get this wrong and every abandoned tab costs a goroutine and a
// daemon connection for the life of the process. Asserted on a deadline rather
// than a bare receive, so a regression fails in five seconds instead of
// hanging the lane.
func TestSessionEnds(t *testing.T) {
	cases := []struct {
		name string
		end  func(c *websocket.Conn, shutdown context.CancelFunc)
	}{
		{"the visitor closes the tab", func(c *websocket.Conn, _ context.CancelFunc) { _ = c.Close() }},
		{"the tunnel shuts down", func(_ *websocket.Conn, shutdown context.CancelFunc) { shutdown() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, shutdown := context.WithCancel(t.Context())
			defer shutdown()

			target := newFakeTarget("api", true, true)
			c := dial(t, serveFakeOn(t, ctx, target))
			readFrame(t, c) // the established frame

			tc.end(c, shutdown)

			select {
			case <-target.done:
			case <-time.After(5 * time.Second):
				t.Fatal("AttachContainer never returned after the session ended")
			}
		})
	}
}
