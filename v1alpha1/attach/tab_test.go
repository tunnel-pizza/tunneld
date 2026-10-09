package attach

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"k8s.io/cri-streaming/pkg/streaming/remotecommand"
)

// tabOut is a tab's STDOUT: what the session wrote, and a way to hold the
// writer up so the tee fills.
type tabOut struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	hold chan struct{} // nil: never held
}

func (o *tabOut) Write(p []byte) (int, error) {
	o.mu.Lock()
	hold := o.hold
	o.mu.Unlock()
	if hold != nil {
		<-hold
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.Write(p)
}
func (o *tabOut) Close() error { return nil }
func (o *tabOut) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

// openTab attaches one tab viewer to h's session and returns its output, its
// stdin writer, and a cancel that is the socket closing.
func openTab(t *testing.T, h *harness, out *tabOut) (typed *io.PipeWriter, cancel func()) {
	t.Helper()
	in, typed := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	resize := make(chan remotecommand.TerminalSize)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = h.s.AttachContainer(ctx, "", "", "", in, out, nil, true, resize)
	}()
	t.Cleanup(func() {
		cancel()
		_ = typed.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the tab never ended")
		}
	})
	return typed, cancel
}

// await waits for want in out.
func awaitTab(t *testing.T, out *tabOut, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("waited for %q; the tab got %q", want, out.String())
}

// chromeOf decodes the last chrome message in s.
func chromeOf(t *testing.T, s string) chrome {
	t.Helper()
	i := strings.LastIndex(s, "\x1b]7770;")
	if i < 0 {
		t.Fatalf("no chrome message in %q", s)
	}
	rest := s[i+len("\x1b]7770;"):]
	rest = rest[strings.IndexByte(rest, ';')+1:] // past the token
	end := strings.Index(rest, "\x1b\\")
	raw, err := base64.StdEncoding.DecodeString(rest[:end])
	if err != nil {
		t.Fatal(err)
	}
	var c chrome
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// TestTabGetsChromeSnapshotThenStream pins the order a tab's socket carries:
// chrome state, a snapshot of what is on screen already, then the bytes
// the program writes from then on, untouched.
func TestTabGetsChromeSnapshotThenStream(t *testing.T) {
	h := newFrameHarness(t)
	if _, err := (&sink{s: h.s}).Write([]byte("before\r\n")); err != nil {
		t.Fatal(err)
	}
	out := &tabOut{}
	openTab(t, h, out)
	awaitTab(t, out, "before")
	got := out.String()
	c := strings.Index(got, "\x1b]7770;")
	r := strings.Index(got, "\x1bc")
	if c != 0 || r < c {
		t.Fatalf("chrome at %d, reset at %d; want the chrome first then the snapshot: %q", c, r, got)
	}
	ch := chromeOf(t, got)
	if ch.Origin == "" || ch.Viewers != 2 || ch.Cols == 0 || ch.Rows == 0 {
		t.Errorf("chrome = %+v; want the origin, 2 viewers (the harness's frame and this tab), a size", ch)
	}
	if _, err := (&sink{s: h.s}).Write([]byte("\x1b]52;c;aGk=\x07after \x1b[31mred\x1b[m\r\n")); err != nil {
		t.Fatal(err)
	}
	awaitTab(t, out, "after \x1b[31mred\x1b[m")
	if !strings.Contains(out.String()[r:], "\x1b]52;c;aGk=\x07") {
		t.Error("an OSC the program wrote did not reach the tab as it was")
	}
}

// TestTabJoinSplitsTheStreamExactly pins that a tab joining between two
// chunks sees the first in its snapshot and the second in its stream, and
// neither twice.
func TestTabJoinSplitsTheStreamExactly(t *testing.T) {
	h := newFrameHarness(t)
	w := &sink{s: h.s}
	_, _ = w.Write([]byte("one\r\n"))
	out := &tabOut{}
	openTab(t, h, out)
	awaitTab(t, out, "one")
	_, _ = w.Write([]byte("two\r\n"))
	awaitTab(t, out, "two")
	got := out.String()
	if strings.Count(got, "one") != 1 || strings.Count(got, "two") != 1 {
		t.Errorf("one ×%d, two ×%d in %q; want each once", strings.Count(got, "one"), strings.Count(got, "two"), got)
	}
	if strings.Index(got, "one") > strings.Index(got, "two") {
		t.Error("two came before one")
	}
}

// TestTabFallingBehindIsResnapshotted pins the tee's limit: a tab that has not
// read a mebibyte is not fed stale bytes, and does not miss any either — it
// gets a fresh snapshot and everything after it.
func TestTabFallingBehindIsResnapshotted(t *testing.T) {
	h := newFrameHarness(t)
	out := &tabOut{hold: make(chan struct{})}
	openTab(t, h, out)
	time.Sleep(50 * time.Millisecond) // the first writes are parked on hold
	w := &sink{s: h.s}
	chunk := []byte(strings.Repeat("x", 64<<10))
	for range 20 { // 1.25 MiB, past the limit
		_, _ = w.Write(chunk)
	}
	_, _ = w.Write([]byte("LAST\r\n"))
	close(out.hold)
	awaitTab(t, out, "LAST")
	got := out.String()
	if strings.Count(got, "\x1bc") < 2 {
		t.Errorf("a tab past the limit should have been snapshotted again; resets: %d", strings.Count(got, "\x1bc"))
	}
	if strings.LastIndex(got, "\x1bc") > strings.LastIndex(got, "LAST") {
		t.Error("the last chunk came before the fresh snapshot")
	}
}

// TestTabTypesStraightToTheProgram pins a tab's STDIN reaching the program
// as it was typed: xterm encoded it, and nothing here decodes it again.
func TestTabTypesStraightToTheProgram(t *testing.T) {
	h := newFrameHarness(t)
	out := &tabOut{}
	typed, _ := openTab(t, h, out)
	awaitTab(t, out, "\x1bc")
	_, _ = typed.Write([]byte("ls\r\x1b[A"))
	select {
	case got := <-h.typed:
		if got != "ls\r\x1b[A" {
			t.Errorf("the program read %q, want the bytes as typed", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing reached the program")
	}
}

// TestTabEndsWhenTheRunDoes pins a tab's socket ending with the run, so the
// page can say so, and staying through a restart.
func TestTabEndsWhenTheRunDoes(t *testing.T) {
	h := newFrameHarness(t)
	out := &tabOut{}
	in, _ := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	resize := make(chan remotecommand.TerminalSize)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = h.s.AttachContainer(ctx, "", "", "", in, out, nil, true, resize)
	}()
	awaitTab(t, out, "\x1bc")
	close(h.s.done)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the tab outlived the run")
	}
}

// TestTabChromeFollowsTheSession pins the chrome being sent again when
// something it says changes: the title, the address, a viewer leaving.
func TestTabChromeFollowsTheSession(t *testing.T) {
	h := newFrameHarness(t)
	out := &tabOut{}
	openTab(t, h, out)
	awaitTab(t, out, "\x1bc")
	if _, err := h.s.scan.Write([]byte("\x1b]2;named\x07")); err != nil {
		t.Fatal(err)
	}
	awaitTab(t, out, base64.StdEncoding.EncodeToString([]byte(`"title":"named"`))[:12])
	h.s.announce("https://x.tunneled.test/")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && chromeOf(t, out.String()).Address == "" {
		time.Sleep(10 * time.Millisecond)
	}
	if c := chromeOf(t, out.String()); c.Address != "https://x.tunneled.test/" || c.Title != "named" {
		t.Errorf("chrome = %+v; want the address and the title", c)
	}
}

// TestARestartResetsTheTab pins that a tab is told the screen was reset when
// the program starts over: the session's emulator is cleared, and a tab
// still showing the last run under the new one would disagree with it.
func TestARestartResetsTheTab(t *testing.T) {
	target := newRerunTarget(true)
	target.holdFrom = 1
	t.Cleanup(target.release)
	s := serveFake(t, target)
	target.awaitRun(t, 1)

	c := dial(t, s)
	stdoutUntil(t, c, "run 1")

	s.session.restart()
	target.awaitRun(t, 2)

	deadline := time.Now().Add(10 * time.Second)
	var seen strings.Builder
	for !strings.Contains(seen.String(), "run 2") {
		if err := c.SetReadDeadline(deadline); err != nil {
			t.Fatal(err)
		}
		kind, data, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("never saw run 2; got %q (%v)", seen.String(), err)
		}
		if kind == websocket.BinaryMessage && len(data) > 0 && data[0] == 1 {
			seen.Write(data[1:])
		}
	}
	got := seen.String()
	if r := strings.Index(got, "\x1bc"); r < 0 || r > strings.Index(got, "run 2") {
		t.Errorf("the tab got %q after the restart; want a reset before run 2", got)
	}
}

// TestAProgramCannotForgeTheChrome pins that OSC 7770 is the session's
// alone: a program printing one has it dropped from what tabs are sent.
func TestAProgramCannotForgeTheChrome(t *testing.T) {
	h := newFrameHarness(t)
	out := &tabOut{}
	openTab(t, h, out)
	awaitTab(t, out, "\x1bc")
	payload := base64.StdEncoding.EncodeToString([]byte(`{"address":"javascript:alert(1)"}`))
	forged := "\x1b]7770;" + payload
	for _, p := range []string{forged[:4], forged[4:9], forged[9:] + "\x1b\\before", forged + "\x07after"} {
		if _, err := (&sink{s: h.s}).Write([]byte(p)); err != nil {
			t.Fatal(err)
		}
	}
	awaitTab(t, out, "after")
	if got := out.String(); strings.Contains(got, payload) || !strings.Contains(got, "beforeafter") {
		t.Errorf("the tab got %q; want the forged chrome dropped and the text around it kept", got)
	}
}

// TestATabJoiningMidSequenceGetsItWhole pins the join at sequence
// granularity: a CSI or a UTF-8 character cut across two chunks reaches a
// tab that joined between them whole, after its snapshot.
func TestATabJoiningMidSequenceGetsItWhole(t *testing.T) {
	for _, tc := range []struct{ name, first, second, want string }{
		{"a CSI", "x\x1b[38;5;2", "08mred", "\x1b[38;5;208mred"},
		{"a UTF-8 character", "x\xe6\x97", "\xa5y", "日y"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newFrameHarness(t)
			if _, err := (&sink{s: h.s}).Write([]byte(tc.first)); err != nil {
				t.Fatal(err)
			}
			out := &tabOut{}
			openTab(t, h, out)
			awaitTab(t, out, "\x1bc")
			if _, err := (&sink{s: h.s}).Write([]byte(tc.second)); err != nil {
				t.Fatal(err)
			}
			awaitTab(t, out, tc.want)
		})
	}
}

// TestEachTabHasItsOwnChromeToken pins the token the page checks: every
// chrome message to one tab carries the same one, and no two tabs share it.
func TestEachTabHasItsOwnChromeToken(t *testing.T) {
	h := newFrameHarness(t)
	tokens := func(s string) []string {
		var out []string
		for _, m := range strings.Split(s, "\x1b]7770;")[1:] {
			out = append(out, m[:strings.IndexByte(m, ';')])
		}
		return out
	}
	a, b := &tabOut{}, &tabOut{}
	openTab(t, h, a)
	awaitTab(t, a, "\x1bc")
	openTab(t, h, b)
	awaitTab(t, b, "\x1bc")
	deadline := time.Now().Add(5 * time.Second)
	for len(tokens(a.String())) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	ta, tb := tokens(a.String()), tokens(b.String())
	if len(ta) < 2 || len(tb) < 1 {
		t.Fatalf("chrome messages: %d to the first tab, %d to the second; want the second's join to reach the first", len(ta), len(tb))
	}
	for _, tok := range ta {
		if tok != ta[0] || len(tok) < 32 {
			t.Errorf("the first tab's tokens %q; want one, 32 hex digits", ta)
		}
	}
	if ta[0] == tb[0] {
		t.Errorf("both tabs have token %q", ta[0])
	}
}

// TestAConsoleResizeReachesTheTabs pins the pane's size reaching every tab
// whoever changed it: a console window resized renegotiates the pane, and
// the tabs are sent the new size before the program can redraw for it.
func TestAConsoleResizeReachesTheTabs(t *testing.T) {
	h := newFrameHarness(t)
	out := &tabOut{}
	openTab(t, h, out)
	awaitTab(t, out, "\x1bc")
	h.s.resizeViewer(h.f.v, 61, 21)
	want := 61 - chromeWidth
	deadline := time.Now().Add(5 * time.Second)
	for chromeOf(t, out.String()).Cols != want && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := chromeOf(t, out.String()); got.Cols != want || got.Rows != 21-chromeHeight-h.s.bannerRows() {
		t.Errorf("the tab's chrome says %d×%d after the console went to 61×21; want the pane", got.Cols, got.Rows)
	}
}

// TestACatchUpIsTheSnapshotAlone pins a stale tab's catch-up: chunks queued
// before it are already in the snapshot, and sending them too would draw
// them twice.
func TestACatchUpIsTheSnapshotAlone(t *testing.T) {
	h := newFrameHarness(t)
	v := &viewer{wake: make(chan struct{}, 1), said: make(chan []byte, 64), tab: newTee(), token: "t"}
	h.s.mu.Lock()
	h.s.viewers[v] = struct{}{}
	h.s.mu.Unlock()
	if _, err := (&sink{s: h.s}).Write([]byte("once\r\n")); err != nil {
		t.Fatal(err)
	}
	got := h.s.catchUp(v)
	if len(got) != 1 || strings.Count(string(got[0]), "once") != 1 {
		t.Errorf("catch-up = %q; want the snapshot alone, with the line once", got)
	}
}

// TestATabGetsTheRunsLastWords pins the end of a run reaching a tab: what
// the program printed before it exited is sent before the tab ends, not
// raced by the ending.
func TestATabGetsTheRunsLastWords(t *testing.T) {
	for range 20 {
		h := newFrameHarness(t)
		out := &tabOut{}
		in, _ := io.Pipe()
		resize := make(chan remotecommand.TerminalSize)
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = h.s.AttachContainer(t.Context(), "", "", "", in, out, nil, true, resize)
		}()
		awaitTab(t, out, "\x1bc")
		// The writer held on one chunk while the last is queued and the run
		// ends.
		hold := make(chan struct{})
		out.mu.Lock()
		out.hold = hold
		out.mu.Unlock()
		for _, line := range []string{"building\r\n", "Error: port in use\r\n"} {
			if _, err := (&sink{s: h.s}).Write([]byte(line)); err != nil {
				t.Fatal(err)
			}
			time.Sleep(10 * time.Millisecond) // the writer takes the first alone
		}
		close(h.s.done)
		time.Sleep(20 * time.Millisecond)
		close(hold)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the tab outlived the run")
		}
		if !strings.Contains(out.String(), "port in use") {
			t.Fatal("the tab ended without the run's last line")
		}
	}
}

// TestTypingSurvivesARunsStdinClosing pins a tab's keys across a restart: a
// key that meets the old run's closed stdin is dropped, not an error that
// ends the tab's input for good.
func TestTypingSurvivesARunsStdinClosing(t *testing.T) {
	h := newFrameHarness(t)
	_, pw := io.Pipe()
	_ = pw.Close()
	h.s.mu.Lock()
	h.s.stdin = pw
	h.s.mu.Unlock()
	if n, err := (rawStdin{h.s}).Write([]byte("x")); n != 1 || err != nil {
		t.Errorf("a key on a closed stdin = %d, %v; want it dropped quietly", n, err)
	}
}
