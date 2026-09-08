package attach

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/vt"
	v1 "github.com/tunnel-pizza/tunneld/v1"
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

	em := vt.NewSafeEmulator(defaultCols-chromeWidth, defaultRows-chromeHeight)
	em.SetScrollbackSize(scrollbackLines)

	s := &session{
		Target: newFakeTarget("api", true, true),
		log:    slog.New(slog.DiscardHandler),
		banner: testBanner,
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

// paste hands the frame pasted text the way the decoder hands it over: whole,
// as one message, rather than as a burst of keystrokes.
func (h *harness) paste(t *testing.T, text string) {
	t.Helper()
	model, _ := h.f.Update(tea.PasteMsg{Content: text})
	f, ok := model.(frame)
	if !ok {
		t.Fatalf("Update returned %T, want a frame", model)
	}
	h.f = f
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
//
// A capital is reported the way a keyboard produces one — the unshifted code,
// the shift modifier, and the text it actually produced — rather than as a
// bare uppercase rune. That distinction is the whole point of the fixture: a
// key rebuilt from the code and the modifier loses the capital, and a test
// that handed the frame a tidier key than a browser does would never notice.
func typing(r rune) tea.Key {
	if unicode.IsUpper(r) {
		return tea.Key{Code: unicode.ToLower(r), ShiftedCode: r, Mod: tea.ModShift, Text: string(r)}
	}
	return tea.Key{Code: r, Text: string(r)}
}

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

	for _, r := range "echo Hi There" {
		h.press(t, typing(r))
	}
	h.reached(t, "echo Hi There")
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

// TestViewFitsTheWindow pins that the frame draws the window it was given and
// not a column or row more. Either overrun paints over the container's screen,
// and on a small window there is not much of it to lose.
func TestViewFitsTheWindow(t *testing.T) {
	h := newFrameHarness(t)
	h.f.width, h.f.height = 20, 6

	lines := strings.Split(h.f.View().Content, "\n")
	if len(lines) != h.f.height {
		t.Errorf("view is %d lines, want exactly the window's %d", len(lines), h.f.height)
	}
	for i, line := range lines {
		if got := len([]rune(stripSGR(line))); got > h.f.width {
			t.Errorf("line %d is %d columns, want no more than the window's %d", i, got, h.f.width)
		}
	}
}

// TestViewIsBordered pins the frame's shape: a box drawn around the container,
// with what the session is written into the border rather than into a row of
// its own.
func TestViewIsBordered(t *testing.T) {
	h := newFrameHarness(t)
	h.f.width, h.f.height = 120, 8
	h.s.announce("https://striped-worm.tunneled.pizza/?0")

	lines := strings.Split(h.f.View().Content, "\n")
	top, bottom := stripSGR(lines[0]), stripSGR(lines[len(lines)-1])

	if !strings.HasPrefix(top, "\u256d") || !strings.HasSuffix(top, "\u256e") {
		t.Errorf("top line = %q, want it cornered", top)
	}
	if !strings.HasPrefix(bottom, "\u2570") || !strings.HasSuffix(bottom, "\u256f") {
		t.Errorf("bottom line = %q, want it cornered", bottom)
	}
	// The origin as it was typed, scheme and all: the same string pasted back
	// into a command line is a working origin, and the scheme is what says
	// this is a container rather than a web server.
	if want := v1.DockerScheme + "://" + h.s.Name(); !strings.Contains(top, want) {
		t.Errorf("top border = %q, want the origin %q in it", top, want)
	}
	if strings.Contains(top, "viewer") {
		t.Errorf("top border = %q, want the counts at the bottom instead", top)
	}
	if name, _ := os.Hostname(); name != "" && strings.Contains(top, name) {
		t.Errorf("top border = %q, want the host at the bottom instead", top)
	}
	// The address this viewer arrived on, against the far corner — the other
	// end of the same thing the origin names.
	if want := h.s.announced() + " ╮"; !strings.HasSuffix(top, want) {
		t.Errorf("top border = %q, want it ending %q", top, want)
	}

	// What the shell says it is doing, centred between the two.
	h.s.setTitle("sleep 2")
	titled := stripSGR(strings.Split(h.f.View().Content, "\n")[0])
	at := strings.Index(titled, "sleep 2")
	if at < 0 {
		t.Errorf("top border = %q, want the shell's title in it", titled)
	} else {
		if origin := strings.Index(titled, h.s.Name()); at < origin {
			t.Errorf("top border = %q, want the title after the origin", titled)
		}
		if addr := strings.Index(titled, h.s.announced()); addr > 0 && at > addr {
			t.Errorf("top border = %q, want the title before the address", titled)
		}
	}
	h.s.setTitle("")

	// And marked as a hyperlink, so a terminal that understands OSC 8 makes it
	// clickable. The markers carry no width, so the corner it is aligned
	// against is unmoved by them.
	raw := lines[0]
	if want := "\x1b]8;;" + h.s.announced(); !strings.Contains(raw, want) {
		t.Errorf("top border = %q, want the address marked as a link (%q)", raw, want)
	}

	// The keys take the bottom left and the counts the bottom right, hard
	// against the corner.
	if !strings.HasPrefix(bottom, "\u2570\u2500 ^D") {
		t.Errorf("bottom border = %q, want the keys at its left", bottom)
	}
	if !strings.Contains(bottom, "1 viewer") || !strings.Contains(bottom, "\u00d7") {
		t.Errorf("bottom border = %q, want the counts in it", bottom)
	}
	// The host leads the counts: it is the part of the frame that is not a
	// name somebody chose.
	if name, _ := os.Hostname(); name != "" {
		at, counts := strings.Index(bottom, name), strings.Index(bottom, "1 viewer")
		if at < 0 || at > counts {
			t.Errorf("bottom border = %q, want %q leading the counts", bottom, name)
		}
	}
	// Hard against the corner, mirroring the name's own gap at the top left.
	if want := "] \u256f"; !strings.HasSuffix(bottom, want) {
		t.Errorf("bottom border = %q, want the counts ending against the corner (%q)", bottom, want)
	}

	// The build sits centred between them, and is the first thing to go when
	// the row cannot hold all three: it is the least urgent of them.
	if !strings.Contains(bottom, testBanner) {
		t.Errorf("bottom border = %q, want the build line in it", bottom)
	}
	if at := strings.Index(bottom, testBanner); at > 0 {
		if keys := strings.Index(bottom, "^D"); at < keys {
			t.Errorf("bottom border = %q, want the build after the keys", bottom)
		}
		if counts := strings.Index(bottom, "1 viewer"); counts > 0 && at > counts {
			t.Errorf("bottom border = %q, want the build before the counts", bottom)
		}
	}

	// Narrower, and the three give way in order. First the build, which is the
	// least urgent.
	h.f.width = 80
	if got := stripSGR(bottomOf(h)); strings.Contains(got, testBanner) {
		t.Errorf("bottom border = %q, want the build dropped before the counts", got)
	} else if !strings.Contains(got, "1 viewer") {
		t.Errorf("bottom border = %q, want the counts kept", got)
	}

	// Then the counts: what to press matters more than how many are watching.
	h.f.width = 24
	defer func() { h.f.width = 120 }()
	if got := stripSGR(bottomOf(h)); strings.Contains(got, "viewer") {
		t.Errorf("bottom border = %q, want the counts dropped rather than overlapping the keys", got)
	} else if strings.Contains(got, testBanner) {
		t.Errorf("bottom border = %q, want the build dropped too", got)
	} else if !strings.Contains(got, "^D") {
		t.Errorf("bottom border = %q, want the keys kept", got)
	}
	h.f.width = 120

	// And the commands replace it once it is open, in the same row.
	h.press(t, ctrlD)
	after := strings.Split(h.f.View().Content, "\n")
	if got := stripSGR(after[len(after)-1]); !strings.Contains(got, "detach") {
		t.Errorf("bottom border in command mode = %q, want the commands in it", got)
	}
}

// bottomOf is the frame's bottom border row as it renders now.
func bottomOf(h *harness) string {
	lines := strings.Split(h.f.View().Content, "\n")
	return lines[len(lines)-1]
}

// TestBorderSurvivesAnOversizedScreen pins the frame against the one way the
// pane can escape it.
//
// Neither the emulator nor a styled line clips to the area it is handed; both
// clip to the screen. Drawn straight into the frame's buffer, a screen larger
// than the pane paints over the border and out of the window — and that is not
// a contrived state. A viewer whose window shrinks renders once with the new
// pane and the old emulator, every time.
func TestBorderSurvivesAnOversizedScreen(t *testing.T) {
	h := newFrameHarness(t)
	h.f.width, h.f.height = 20, 6

	// The emulator is left at the harness default, which is far larger than
	// the window just set — exactly the disagreement a shrink creates.
	if _, err := h.s.em.WriteString(strings.Repeat("wide output here\r\n", 12)); err != nil {
		t.Fatalf("fill the screen: %v", err)
	}

	lines := strings.Split(h.f.View().Content, "\n")
	if len(lines) != h.f.height {
		t.Fatalf("view is %d lines, want the window's %d", len(lines), h.f.height)
	}
	for i, line := range lines {
		plain := []rune(stripSGR(line))
		if len(plain) > h.f.width {
			t.Errorf("line %d is %d columns, want no more than %d", i, len(plain), h.f.width)
		}
	}
	if bottom := stripSGR(lines[len(lines)-1]); !strings.HasPrefix(bottom, "\u2570") || !strings.HasSuffix(bottom, "\u256f") {
		t.Errorf("bottom border = %q, want the pane clipped and the border intact", bottom)
	}
	for i, line := range lines[1 : len(lines)-1] {
		plain := []rune(stripSGR(line))
		if len(plain) == 0 || plain[0] != '\u2502' || plain[len(plain)-1] != '\u2502' {
			t.Errorf("row %d = %q, want the pane between two edges", i+1, string(plain))
		}
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
	// Offset by the border: the pane starts one row down and one column in,
	// and a cursor placed at the pane's own coordinates would sit on the frame.
	pane := h.f.pane()
	want := h.s.paneCursor()
	if view.Cursor.X != pane.Min.X+want.X || view.Cursor.Y != pane.Min.Y+want.Y {
		t.Errorf("cursor at (%d,%d), want the pane's (%d,%d) inside the border at (%d,%d)",
			view.Cursor.X, view.Cursor.Y, want.X, want.Y, pane.Min.X+want.X, pane.Min.Y+want.Y)
	}

	// Withheld where the live position means nothing: a command is pending, or
	// the pane is showing something that scrolled off.
	h.press(t, ctrlD)
	if h.f.View().Cursor != nil {
		t.Error("cursor shown in command mode, want it withheld")
	}
}

// stripSGR removes the escapes a rendered line carries — the styling, and the
// hyperlink markers around the address — so a width can be counted in columns
// and a border read as text.
func stripSGR(s string) string {
	var b strings.Builder
	for {
		start := strings.Index(s, "\x1b")
		if start < 0 || start+1 >= len(s) {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:start])

		switch s[start+1] {
		case '[': // CSI, ended by a letter
			end := strings.IndexFunc(s[start+2:], func(r rune) bool {
				return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
			})
			if end < 0 {
				return b.String()
			}
			s = s[start+2+end+1:]
		case ']': // OSC, ended by BEL or ST
			end := strings.IndexAny(s[start+2:], "\a\x1b")
			if end < 0 {
				return b.String()
			}
			s = s[start+2+end:]
			if strings.HasPrefix(s, "\a") {
				s = s[1:]
			} else if strings.HasPrefix(s, "\x1b\\\\") {
				s = s[2:]
			} else {
				s = s[1:]
			}
		default:
			s = s[start+2:]
		}
	}
}

// TestPasteReachesTheContainer pins that pasted text is not dropped.
//
// The frame's own renderer turns bracketed paste on in the viewer's terminal,
// so a paste stops arriving as a burst of keystrokes and arrives as one
// message instead. A model that only answers keystrokes silently swallows it,
// and pasting into the terminal does nothing at all.
func TestPasteReachesTheContainer(t *testing.T) {
	h := newFrameHarness(t)

	h.paste(t, "echo pasted")
	h.reached(t, "echo pasted")
}

// TestPasteIsBracketedWhenTheAppAsked pins that the difference between typing
// and pasting survives the frame.
//
// Bracketed paste is how an app is told the difference, and it is the app
// inside the container that asks for it — the emulator read that request, so
// it is the only thing here that knows. Text written past it would arrive as
// though it had been typed, which is what a shell running a half-finished
// command on a pasted newline looks like.
func TestPasteIsBracketedWhenTheAppAsked(t *testing.T) {
	h := newFrameHarness(t)

	// What an app writes when it wants its pastes marked.
	if _, err := h.s.em.WriteString("\x1b[?2004h"); err != nil {
		t.Fatalf("enable bracketed paste: %v", err)
	}
	h.paste(t, "echo pasted")
	h.reached(t, "\x1b[200~echo pasted\x1b[201~")
}

// TestPasteSnapsThePaneLive pins that pasting into a scrolled-back pane puts
// it back at the prompt, the same as typing does — what was pasted is meant
// for the prompt, and the prompt is at the bottom.
func TestPasteSnapsThePaneLive(t *testing.T) {
	h := newFrameHarness(t)
	if _, err := h.s.em.WriteString(strings.Repeat("a line\r\n", defaultRows*3)); err != nil {
		t.Fatalf("fill the screen: %v", err)
	}

	h.press(t, ctrlD)
	h.press(t, typing('k'))
	if h.f.scroll == 0 {
		t.Fatal("the pane did not scroll back")
	}

	h.paste(t, "x")
	if h.f.scroll != 0 {
		t.Errorf("scroll = %d after a paste, want the pane snapped live", h.f.scroll)
	}
}
