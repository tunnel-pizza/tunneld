package attach

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/vt"
	"k8s.io/cri-streaming/pkg/streaming/remotecommand"
)

// harness is a session with nothing but a screen and a stdin — no network, no
// container, no browser. A frame talks to exactly that much of one, so it is
// all these tests stand up.
type harness struct {
	s *session
	f frame

	// typed carries whatever reached the target's stdin, in whatever pieces
	// the emulator's reader handed over.
	typed chan string
}

func newFrameHarness(t *testing.T) *harness {
	t.Helper()

	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })

	em := vt.NewSafeEmulator(defaultCols, defaultRows-chromeHeight)
	em.SetScrollbackSize(scrollbackLines)

	s := &session{
		Target: newFakeTarget("api", true, true),
		log:    slog.New(slog.DiscardHandler),
		stdin:  pw,
		// Buffered, because nothing here is reading the far end: the frame
		// applies a size as a side effect of being told its window, and a test
		// that had to drain it would be testing the plumbing instead.
		resize:  make(chan remotecommand.TerminalSize, 8),
		done:    make(chan struct{}),
		em:      em,
		viewers: map[*viewer]struct{}{},
		size:    remotecommand.TerminalSize{Width: defaultCols, Height: defaultRows},
	}

	// The one wire the frame depends on: a key handed to the emulator comes
	// back out of it encoded, and that is what the container reads.
	go func() { _, _ = io.Copy(pw, em) }()

	h := &harness{s: s, typed: make(chan string, 64)}
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := pr.Read(buf)
			if n > 0 {
				h.typed <- string(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	v := &viewer{wake: make(chan struct{}, 1)}
	s.viewers[v] = struct{}{}
	h.f = frame{sess: s, v: v, width: defaultCols, height: defaultRows}
	return h
}

// press sends one keystroke to the frame and keeps whatever it became.
func (h *harness) press(t *testing.T, k tea.Key) tea.Cmd {
	t.Helper()
	model, cmd := h.f.Update(tea.KeyPressMsg(k))
	f, ok := model.(frame)
	if !ok {
		t.Fatalf("Update returned %T, want a frame", model)
	}
	h.f = f
	return cmd
}

// reached collects what the container read until it is want, and fails if
// something else shows up. The bytes arrive in as many pieces as the
// emulator's reader chose, so it accumulates rather than comparing once.
func (h *harness) reached(t *testing.T, want string) {
	t.Helper()
	var read strings.Builder
	for read.String() != want {
		select {
		case got := <-h.typed:
			read.WriteString(got)
			if !strings.HasPrefix(want, read.String()) {
				t.Fatalf("container read %q, want it building %q", read.String(), want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("container read %q, want %q", read.String(), want)
		}
	}
}

// silent fails if anything at all reached the container.
func (h *harness) silent(t *testing.T) {
	t.Helper()
	select {
	case got := <-h.typed:
		t.Fatalf("container read %q, want nothing", got)
	case <-time.After(100 * time.Millisecond):
	}
}

// ctrlD is the keystroke the frame keeps for itself.
var ctrlD = tea.Key{Code: 'd', Mod: tea.ModCtrl}

// typing is one printable key, the way a browser reports one.
func typing(r rune) tea.Key { return tea.Key{Code: r, Text: string(r)} }

// TestCtrlDNeverReachesTheContainer is the guard the frame exists to put on
// one key.
//
// A shell reads Ctrl-D as end of file and exits. The attach is shared and is
// never restarted, so before the frame it took one viewer pressing it — meant
// for their own session, as it is on any other terminal — to end the terminal
// for everybody watching and leave the origin serving a screen that could
// never produce another byte. The container must not see it.
func TestCtrlDNeverReachesTheContainer(t *testing.T) {
	h := newFrameHarness(t)

	h.press(t, ctrlD)
	h.silent(t)
	if !h.f.command {
		t.Error("Ctrl-D did not open command mode")
	}

	// And the mode closes on the next key rather than lingering, whatever
	// that key is: a viewer left in a mode they did not notice entering would
	// find their next keystrokes vanishing.
	h.press(t, tea.Key{Code: tea.KeyEscape})
	if h.f.command {
		t.Error("still in command mode after a key, want it closed")
	}
}

// TestOrdinaryKeysReachTheContainerUnchanged pins the other half: a frame in
// the way must not change what the shell reads. The keystroke is decoded by
// the frame and re-encoded by the emulator, and for ordinary typing the bytes
// that come out are the bytes that went in.
func TestOrdinaryKeysReachTheContainerUnchanged(t *testing.T) {
	h := newFrameHarness(t)

	for _, r := range "echo hi" {
		h.press(t, typing(r))
	}
	h.reached(t, "echo hi")
}

// TestCommandModeEndsTheSessionOnPurpose pins the deliberate half of the
// guard. Ctrl-D no longer ends a shared session by accident, so there has to
// be a way to end one on purpose, and q is it: the end of file the key used to
// deliver, asked for rather than tripped over.
func TestCommandModeEndsTheSessionOnPurpose(t *testing.T) {
	h := newFrameHarness(t)

	h.press(t, ctrlD)
	h.silent(t)

	if cmd := h.press(t, typing('q')); cmd == nil {
		t.Error("q returned no command, want the frame to quit with it")
	}
	h.reached(t, "\x04")
}

// TestDetachLeavesTheSessionAlone pins the difference between the two ways out
// of command mode. Detaching is this viewer's socket closing and nothing else
// — the stream is shared, and the people still watching it must not notice.
func TestDetachLeavesTheSessionAlone(t *testing.T) {
	h := newFrameHarness(t)

	h.press(t, ctrlD)
	if cmd := h.press(t, typing('d')); cmd == nil {
		t.Error("d returned no command, want the frame to quit with it")
	}
	h.silent(t)
}

// TestScrollbackIsReachedThroughTheFrame pins what the alternate screen takes
// away and the frame gives back.
//
// A framed viewer has no browser scrollback: the frame is on the alternate
// screen, which has none. So the emulator's own is what a viewer scrolls, and
// typing puts them back where a terminal puts them — at the prompt.
func TestScrollbackIsReachedThroughTheFrame(t *testing.T) {
	h := newFrameHarness(t)

	if _, err := h.s.em.WriteString(strings.Repeat("a line\r\n", defaultRows*3)); err != nil {
		t.Fatalf("fill the screen: %v", err)
	}

	h.press(t, ctrlD)
	h.press(t, typing('k'))
	if h.f.scroll != 1 {
		t.Errorf("scroll = %d after one k, want 1", h.f.scroll)
	}

	h.press(t, ctrlD)
	h.press(t, typing('j'))
	if h.f.scroll != 0 {
		t.Errorf("scroll = %d after j, want it back to live", h.f.scroll)
	}

	// Scrolled back, then typed at: the keystroke is meant for the prompt, and
	// the prompt is at the bottom.
	h.press(t, ctrlD)
	h.press(t, typing('k'))
	h.press(t, typing('x'))
	if h.f.scroll != 0 {
		t.Errorf("scroll = %d after typing, want the pane snapped live", h.f.scroll)
	}
	h.reached(t, "x")
}

// TestScrollStopsAtTheOldestLine pins that scrolling back cannot run off the
// end of what the emulator kept.
func TestScrollStopsAtTheOldestLine(t *testing.T) {
	h := newFrameHarness(t)

	for range h.s.em.ScrollbackLen() + 10 {
		h.press(t, ctrlD)
		h.press(t, typing('k'))
	}
	if got, want := h.f.scroll, h.s.em.ScrollbackLen(); got > want {
		t.Errorf("scrolled back %d lines, want no more than the %d kept", got, want)
	}
}

// TestViewFitsTheWindow pins the two ways a frame can overrun the window it
// was given, both of which paint over the container's screen.
func TestViewFitsTheWindow(t *testing.T) {
	h := newFrameHarness(t)
	h.f.width, h.f.height = 20, 6

	view := h.f.View()
	lines := strings.Split(view.Content, "\n")
	if len(lines) > h.f.height {
		t.Errorf("view is %d lines, want no more than the window's %d", len(lines), h.f.height)
	}

	// The status line is truncated rather than wrapped: a wrapped one costs a
	// second row the pane was drawn assuming it still had.
	status := lines[len(lines)-1]
	if got := len(stripSGR(status)); got > h.f.width {
		t.Errorf("status line is %d columns, want no more than the window's %d", got, h.f.width)
	}
}

// TestViewPlacesTheCursor pins the frame's second job. The pane's cursor is
// where the app inside believes it is, and a frame that did not place it there
// would leave it in the wrong cell for every viewer.
func TestViewPlacesTheCursor(t *testing.T) {
	h := newFrameHarness(t)
	if _, err := h.s.em.WriteString("hello"); err != nil {
		t.Fatalf("write: %v", err)
	}

	view := h.f.View()
	if view.Cursor == nil {
		t.Fatal("no cursor in the view, want it placed where the app believes it is")
	}
	if want := h.s.paneCursor(); view.Cursor.X != want.X || view.Cursor.Y != want.Y {
		t.Errorf("cursor at (%d,%d), want the pane's (%d,%d)", view.Cursor.X, view.Cursor.Y, want.X, want.Y)
	}

	// Withheld where the live position means nothing: a command is pending, or
	// the pane is showing something that scrolled off.
	h.press(t, ctrlD)
	if h.f.View().Cursor != nil {
		t.Error("cursor shown in command mode, want it withheld")
	}
}

// stripSGR removes the styling a rendered line carries, so a width can be
// counted in columns rather than in escape bytes.
func stripSGR(s string) string {
	var b strings.Builder
	for {
		start := strings.Index(s, "\x1b[")
		if start < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:start])
		end := strings.IndexByte(s[start:], 'm')
		if end < 0 {
			return b.String()
		}
		s = s[start+end+1:]
	}
}
