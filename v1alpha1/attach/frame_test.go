package attach

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
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

	s := &session{
		Target: newFakeTarget("api", true, true),
		log:    slog.New(slog.DiscardHandler),
		banner: testBanner,
		logs:   testLogs{},
		stdin:  pw,
		// Buffered, because nothing here is reading the far end: the frame
		// applies a size as a side effect of being told its window, and a test
		// that had to drain it would be testing the plumbing instead.
		resize:  make(chan remotecommand.TerminalSize, 8),
		done:    make(chan struct{}),
		em:      em,
		viewers: map[*viewer]struct{}{},
		size:    remotecommand.TerminalSize{Width: defaultCols, Height: defaultRows},

		// Built by hand rather than through newSession, so the grace a real
		// session gets by default has to be set here too — otherwise every
		// reply is "late" against a zero-length window.
		clipboardGrace: defaultClipboardGrace,
	}

	// The same reporting a real session installs, so what the frame reads back
	// about the terminal — its names, whether it wants a cursor — arrives the
	// way the emulator delivers it rather than by a test writing the field.
	s.watch()

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

	v := &viewer{wake: make(chan struct{}, 1), said: make(chan []byte, 64)}
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

// commandKey is the keystroke the frame keeps for itself, on every platform.
// The page is in the way of it only because the browser claims the chord for
// its address bar; what arrives is the byte a terminal sends.
var commandKey = tea.Key{Code: 'k', Mod: tea.ModCtrl}

// ctrlD is the program's again, and the most dangerous key a shared session
// has: a shell reads it as end of file and the stream is never reopened.
var ctrlD = tea.Key{Code: 'd', Mod: tea.ModCtrl}

// ctrlC is the other one: SIGINT to the program, and the end of a session that
// cannot be started again.
var ctrlC = tea.Key{Code: 'c', Mod: tea.ModCtrl}

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

// TestTheCommandKeyNeverReachesTheContainer pins the one key the frame keeps.
//
// Ctrl+K, which does cost the program a key — kill-to-end-of-line — and is
// the cheaper of the two keys on offer: the other one, Ctrl-D, ends a shared
// session for everybody watching.
func TestTheCommandKeyNeverReachesTheContainer(t *testing.T) {
	h := newFrameHarness(t)

	h.press(t, commandKey)
	h.silent(t)
	if !h.f.command {
		t.Error("the command key did not open command mode")
	}

	// And the mode closes on the next key rather than lingering, whatever
	// that key is: a viewer left in a mode they did not notice entering would
	// find their next keystrokes vanishing.
	h.press(t, tea.Key{Code: tea.KeyEscape})
	if h.f.command {
		t.Error("still in command mode after a key, want it closed")
	}
}

// TestSessionEndingKeysAreAskedForTwice pins the guard, and what decides
// whether there is one.
//
// Ctrl-C and Ctrl-D end the program, and where the program cannot be started
// again that ends the terminal for everybody watching, permanently. The frame
// asks a second time rather than refusing: the key still works, it just is not
// one keystroke away from taking a shared session down.
//
// The first press is not silent. The border says which key is waiting, because
// a key that appears to do nothing reads as a key that is broken.
func TestSessionEndingKeysAreAskedForTwice(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  tea.Key
		want string
	}{
		{"end of file", ctrlD, "\x04"},
		{"interrupt", ctrlC, "\x03"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newFrameHarness(t)

			h.press(t, tc.key)
			h.silent(t)
			if bottom := stripSGR(bottomOf(h)); !strings.Contains(bottom, "again to send it") {
				t.Errorf("bottom border = %q, want it saying the key is waiting", bottom)
			}

			h.press(t, tc.key)
			h.reached(t, tc.want)
		})
	}
}

// TestTypingOnSpendsTheArming pins that the question does not linger. Somebody
// who typed something else is no longer answering it, and a Ctrl-C much later
// should be the first press of a new pair rather than the second of an old
// one.
func TestTypingOnSpendsTheArming(t *testing.T) {
	h := newFrameHarness(t)

	h.press(t, ctrlC)
	h.silent(t)
	h.press(t, typing('x'))
	h.reached(t, "x")

	// Armed again, not sent: this is a first press.
	h.press(t, ctrlC)
	h.silent(t)
}

// TestAnArmingExpires pins that the question does not wait forever. A second
// press minutes later is a new intention rather than the other half of a
// double tap, so the arming lets go on its own and the border says so by
// going back to what it said before.
func TestAnArmingExpires(t *testing.T) {
	h := newFrameHarness(t)

	h.press(t, ctrlC)
	h.silent(t)

	// The tick, without waiting for it.
	model, _ := h.f.Update(disarmMsg{arming: h.f.arming})
	h.f = model.(frame)
	if h.f.armed != 0 {
		t.Error("still armed after the grace expired")
	}
	if bottom := stripSGR(bottomOf(h)); !strings.Contains(bottom, "commands") {
		t.Errorf("bottom border = %q, want it back to the ordinary hint", bottom)
	}

	// And the next press is a first press, not a second.
	h.press(t, ctrlC)
	h.silent(t)
}

// TestAStaleArmingTickIsIgnored pins the counter. Press, type on, press
// again inside the grace, and the first arming's tick must not disarm the
// second — which would leave a viewer's next Ctrl-C sending nothing while the
// border said it would.
func TestAStaleArmingTickIsIgnored(t *testing.T) {
	h := newFrameHarness(t)

	h.press(t, ctrlC)
	stale := h.f.arming
	h.press(t, typing('x'))
	h.reached(t, "x")
	h.press(t, ctrlC)

	model, _ := h.f.Update(disarmMsg{arming: stale})
	h.f = model.(frame)
	if h.f.armed != 'c' {
		t.Error("a spent arming's tick disarmed the live one")
	}
}

// TestTheLogsAreAViewAwayFromTheTerminal pins what `l` shows and how somebody
// gets back.
//
// tunneld's own lines go to the console it was started on, which is somewhere
// a viewer is not — and, on a mirrored console, underneath this very frame. So
// the frame is the only place they can be read from, and reading is not
// something anybody finishes in one keystroke: the view stays until escape.
func TestTheLogsAreAViewAwayFromTheTerminal(t *testing.T) {
	h := newFrameHarness(t)
	if _, err := h.s.em.WriteString("what the terminal was showing"); err != nil {
		t.Fatalf("write: %v", err)
	}

	h.press(t, commandKey)
	h.press(t, typing('l'))

	pane := stripSGR(h.f.View().Content)
	if !strings.Contains(pane, "a line tunneld wrote") {
		t.Errorf("pane = %q, want tunneld's own lines", pane)
	}
	if strings.Contains(pane, "what the terminal was showing") {
		t.Error("the terminal is still drawn under the logs, want the logs over it")
	}
	if bottom := stripSGR(bottomOf(h)); !strings.Contains(bottom, "back to the terminal") {
		t.Errorf("bottom border = %q, want it saying how to get back", bottom)
	}
	if h.f.View().Cursor != nil {
		t.Error("a cursor is drawn over the logs, where the live position means nothing")
	}

	// Reading is not typing: keys spent here reach no terminal.
	h.press(t, typing('q'))
	h.silent(t)
	if !h.f.logs {
		t.Error("a keystroke closed the log view, want only escape to")
	}

	h.press(t, tea.Key{Code: tea.KeyEscape})
	if h.f.logs {
		t.Error("escape did not go back to the terminal")
	}
	if pane := stripSGR(h.f.View().Content); !strings.Contains(pane, "what the terminal was showing") {
		t.Errorf("pane = %q, want the terminal back", pane)
	}
}

// TestTheLogsSayWhenThereAreNone pins that an empty pane never reads as a
// broken frame. A process that has been quiet has been quiet, and should say
// so.
func TestTheLogsSayWhenThereAreNone(t *testing.T) {
	h := newFrameHarness(t)
	h.s.logs = nil

	h.press(t, commandKey)
	h.press(t, typing('l'))

	if pane := stripSGR(h.f.View().Content); !strings.Contains(pane, "nothing logged yet") {
		t.Errorf("pane = %q, want it saying there is nothing rather than showing nothing", pane)
	}
}

// TestARestartableTargetIsNotGuarded pins the other half of the split. A
// program origin is a path, so the cost of a mistaken Ctrl-C is opening the
// page again — and a terminal that argues with Ctrl-C is not a terminal.
func TestARestartableTargetIsNotGuarded(t *testing.T) {
	h := newFrameHarness(t)
	program := newFakeTarget("prog", true, true)
	program.origin = v1.ExecScheme + "://" + "/usr/bin/prog"
	program.repeat = true
	h.s.Target = program

	h.press(t, ctrlD)
	if h.f.command {
		t.Error("Ctrl-D opened command mode, want it passed straight to the program")
	}
	h.reached(t, "\x04")
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

// TestCommandModeEndsTheRun pins x: the one way out of a terminal somebody
// opened from their own machine.
//
// It ends the run rather than the viewer or the target — the process, its
// context, and everything started under it — so what the frame does is ask,
// and the command is what stops. The keystroke reaches the target either way,
// which is the point of asking rather than writing an end of file into a
// shared stdin.
func TestCommandModeEndsTheRun(t *testing.T) {
	asked := make(chan struct{})
	h := newFrameHarness(t)
	h.s.quit = func() { close(asked) }

	h.press(t, commandKey)
	h.silent(t)

	if cmd := h.press(t, typing('x')); cmd == nil {
		t.Error("x returned no command, want the frame to quit with it")
	}
	select {
	case <-asked:
	default:
		t.Error("x did not ask the run to end")
	}
	h.silent(t) // and nothing was typed at the target on the way
}

// TestDetachLeavesTheSessionAlone pins the difference between the two ways out
// of command mode. Detaching is this viewer's socket closing and nothing else
// — the stream is shared, and the people still watching it must not notice.
func TestDetachLeavesTheSessionAlone(t *testing.T) {
	h := newFrameHarness(t)

	h.press(t, commandKey)
	if cmd := h.press(t, typing('d')); cmd == nil {
		t.Error("d returned no command, want the frame to quit with it")
	}
	h.silent(t)
}

// TestViewFitsTheWindow pins that the frame draws the window it was given and
// not a column or row more. Either overrun paints over the container's screen,
// and on a small window there is not much of it to lose.
func TestViewFitsTheWindow(t *testing.T) {
	h := newFrameHarness(t)
	h.window(20, 6)

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
	h.window(h.f.width, 8)
	h.s.announce("https://striped-worm.tunneled.pizza/?0")

	wide, tight := roomFor(h.f)
	h.window(wide, h.f.height)

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
	if want := h.s.Origin(); !strings.Contains(top, want) {
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

	// Both names the terminal goes by, centred between the two, shown together
	// when they differ.
	h.s.setTitle("sleep 2")
	h.s.setSubtitle("sleep")
	titled := stripSGR(strings.Split(h.f.View().Content, "\n")[0])
	at := strings.Index(titled, "sleep 2 · sleep")
	if at < 0 {
		t.Errorf("top border = %q, want both titles in it", titled)
	} else {
		if origin := strings.Index(titled, h.s.Name()); at < origin {
			t.Errorf("top border = %q, want the title after the origin", titled)
		}
		if addr := strings.Index(titled, h.s.announced()); addr > 0 && at > addr {
			t.Errorf("top border = %q, want the title before the address", titled)
		}
	}
	h.s.title, h.s.subtitle = "", ""

	// And marked as a hyperlink, so a terminal that understands OSC 8 makes it
	// clickable. The markers carry no width, so the corner it is aligned
	// against is unmoved by them.
	raw := lines[0]
	if want := "\x1b]8;;" + h.s.announced(); !strings.Contains(raw, want) {
		t.Errorf("top border = %q, want the address marked as a link (%q)", raw, want)
	}

	// The keys take the bottom left and the counts the bottom right, hard
	// against the corner.
	if !strings.HasPrefix(bottom, "\u2570\u2500 ^K") {
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
	h.window(tight, h.f.height)
	if got := stripSGR(bottomOf(h)); strings.Contains(got, testBanner) {
		t.Errorf("bottom border = %q, want the build dropped before the counts", got)
	} else if !strings.Contains(got, "1 viewer") {
		t.Errorf("bottom border = %q, want the counts kept", got)
	}

	// Then the counts: what to press matters more than how many are watching.
	h.window(24, h.f.height)
	defer func() { h.window(wide, h.f.height) }()
	if got := stripSGR(bottomOf(h)); strings.Contains(got, "viewer") {
		t.Errorf("bottom border = %q, want the counts dropped rather than overlapping the keys", got)
	} else if strings.Contains(got, testBanner) {
		t.Errorf("bottom border = %q, want the build dropped too", got)
	} else if !strings.Contains(got, "^K") {
		t.Errorf("bottom border = %q, want the keys kept", got)
	}
	h.window(wide, h.f.height)

	// And the commands replace it once it is open, in the same row.
	h.press(t, commandKey)
	after := strings.Split(h.f.View().Content, "\n")
	if got := stripSGR(after[len(after)-1]); !strings.Contains(got, "detach") {
		t.Errorf("bottom border in command mode = %q, want the commands in it", got)
	}
}

// roomFor is a window width that holds all three of the bottom row's labels,
// and one that holds every label but the build.
//
// Measured rather than chosen. The counts carry os.Hostname, and a machine's
// name is not a constant: fourteen characters on a laptop, sixty on a CI
// runner. A hardcoded width passes or fails on what the machine happens to be
// called, which is a test about the wrong thing.
//
// The build is centred on the frame, so fitting it takes more than the sum of
// the three: its half-width has to clear the keys on one side and the counts
// on the other, which is where the doubling comes from.
func roomFor(f frame) (all, withoutBuild int) {
	keys := uv.NewStyledString(f.hint()).UnicodeWidth()
	build := uv.NewStyledString(f.banner()).UnicodeWidth()
	counts := uv.NewStyledString(f.meta()).UnicodeWidth()

	return build + 2*max(keys, counts) + 10, keys + counts + 10
}

// window is the viewer's window being reported at a new size, the way a lone
// viewer's is: the session settles the emulator on it, so the shared screen
// follows the window. A test that wants the two to differ sets the fields.
func (h *harness) window(width, height int) {
	h.f.width, h.f.height = width, height
	h.s.em.Resize(max(1, width-chromeWidth), max(1, height-chromeHeight-h.s.bannerRows()))
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
	h.window(20, 6)

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

	// Withheld where the live position means nothing: a command is pending.
	h.press(t, commandKey)
	if h.f.View().Cursor != nil {
		t.Error("cursor shown in command mode, want it withheld")
	}
}

// TestViewWithholdsAHiddenCursor pins the third reason not to draw one: the
// program asked for no cursor.
//
// A full-screen program hides it at startup and then leaves the position
// wherever its last write ended, so a frame that draws one anyway shows a
// cursor skating around the screen on every redraw. The emulator keeps
// answering with a position either way — which is the trap.
func TestViewWithholdsAHiddenCursor(t *testing.T) {
	h := newFrameHarness(t)

	// DECTCEM the way a program sends it, through the scanner the session
	// installs — the emulator has no cursor-visibility getter, so the Mode
	// sink is the only path to cursorHidden.
	if _, err := h.s.scan.Write([]byte("hello\x1b[?25l")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !h.s.cursorHidden() {
		t.Fatal("the session did not record DECTCEM; the scanner is not wired")
	}
	if got := h.f.View().Cursor; got != nil {
		t.Errorf("cursor drawn at (%d,%d), want none — the program asked for none", got.X, got.Y)
	}

	// And it comes back, because a program that hides the cursor to redraw
	// shows it again to ask for something.
	if _, err := h.s.scan.Write([]byte("\x1b[?25h")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if h.s.cursorHidden() {
		t.Fatal("the session kept the cursor hidden after DECTCEM set it visible")
	}
	if h.f.View().Cursor == nil {
		t.Error("no cursor after the program asked for one again")
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

// TestFrameCarriesAClipboardReply pins that a ClipboardMsg from a viewer's
// terminal reaches the session, where the query guard decides its fate.
func TestFrameCarriesAClipboardReply(t *testing.T) {
	h := newFrameHarness(t)

	// A query outstanding, so the reply is accepted and reaches stdin.
	h.s.said(Sequence{Kind: OSC, Cmd: 52, Data: []byte("c;?"), Raw: []byte("\x1b]52;c;?\a")})
	if _, cmd := h.f.Update(tea.ClipboardMsg{Selection: 'c', Content: "hi"}); cmd != nil {
		t.Errorf("ClipboardMsg returned a command, want none")
	}
	select {
	case got := <-h.typed:
		if got != "\x1b]52;c;aGk=\a" {
			t.Errorf("stdin = %q, want the base64 reply", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the reply never reached stdin")
	}
}

// TestFirstDrawWaitsForTheWindow pins that a frame does not draw at a size it
// guessed.
//
// The renderer has no terminal to measure — the output is a websocket — so it
// opens by reporting nothing. Answering that immediately with the size the
// session settled on means the viewer's first frame is a box of somebody
// else's dimensions with the cursor somewhere inside it, redrawn the moment
// the page says how big it actually is. Nothing is a better first frame than
// something wrong, and the renderer paints nothing at zero on its own.
func TestFirstDrawWaitsForTheWindow(t *testing.T) {
	h := newFrameHarness(t)

	model, cmd := h.f.Update(tea.WindowSizeMsg{})
	f, ok := model.(frame)
	if !ok {
		t.Fatalf("Update returned %T, want a frame", model)
	}
	if cmd == nil {
		t.Fatal("no command for the empty size report, want the grace period armed")
	}
	if f.sized {
		t.Error("the frame took the empty report as its size")
	}
}

// TestTheGuessIsMadeLate pins the other half: a client that never says how big
// it is still gets a terminal, at whatever size the session settled on. The
// streaming protocol does not require a client to send a size, and one that
// does not must not be left staring at nothing.
func TestTheGuessIsMadeLate(t *testing.T) {
	h := newFrameHarness(t)

	_, cmd := h.f.Update(settleMsg{})
	if cmd == nil {
		t.Fatal("no command from the grace period expiring, want the session's size")
	}
	msg, ok := cmd().(tea.WindowSizeMsg)
	if !ok {
		t.Fatalf("the grace period produced %T, want a window size", cmd())
	}
	w, h2 := h.s.window()
	if msg.Width != w || msg.Height != h2 {
		t.Errorf("settled on %dx%d, want the session's %dx%d", msg.Width, msg.Height, w, h2)
	}
}

// TestThePagesSizeSurvivesTheGuess pins that a page which answered in time is
// not then overruled by the fallback. The renderer has to be told a size by
// the message that expires the grace period either way — leaving it at the
// zero it started with is the one outcome that never draws again — so what it
// is told has to be the page's own.
func TestThePagesSizeSurvivesTheGuess(t *testing.T) {
	h := newFrameHarness(t)

	model, _ := h.f.Update(tea.WindowSizeMsg{Width: 111, Height: 41})
	h.f = model.(frame)
	if !h.f.sized {
		t.Fatal("the page's size was not taken")
	}

	_, cmd := h.f.Update(settleMsg{})
	if cmd == nil {
		t.Fatal("no command from the grace period expiring")
	}
	msg, ok := cmd().(tea.WindowSizeMsg)
	if !ok {
		t.Fatalf("the grace period produced %T, want a window size", cmd())
	}
	if msg.Width != 111 || msg.Height != 41 {
		t.Errorf("settled on %dx%d, want the page's 111x41", msg.Width, msg.Height)
	}
}

// TestABlankTitleLeavesTheBorderWhole pins the frame against a title that
// takes up columns without drawing in them.
//
// A title is whatever the app decided to put there, and the label is written
// over the top border — so a run of characters that measure wide and paint
// blank clears the border it was written over and leaves a hole in the box.
// Seen in the wild: a frame with a clean gap at dead centre, which is where
// the title sits.
func TestABlankTitleLeavesTheBorderWhole(t *testing.T) {
	h := newFrameHarness(t)
	h.window(60, 6)

	whole := stripSGR(strings.Split(h.f.View().Content, "\n")[0])

	for _, title := range []string{
		"   ",
		"\x00\x01\x02",
		"\u200b\u200b\u200b\u200b",
		"\t\t",
		// What actually arrives from Claude Code. Its title is
		// "✳ Claude Code", ✳ is E2 9C B3, and the emulator's OSC parser stops
		// at 9C because that is the C1 string terminator — so the title is the
		// single byte E2, which is not a character, measures a column, and
		// paints nothing.
		"\xe2",
		" \xe2 ",
	} {
		h.s.title = title
		got := stripSGR(strings.Split(h.f.View().Content, "\n")[0])
		if got != whole {
			t.Errorf("title %q drew %q, want the border untouched %q", title, got, whole)
		}
	}

	// A title with something in it still draws, and what draws is the part
	// that shows.
	h.s.title = "  \x00sleep 2\x00  "
	if got := stripSGR(strings.Split(h.f.View().Content, "\n")[0]); !strings.Contains(got, "sleep 2") {
		t.Errorf("top border = %q, want the printable part of the title in it", got)
	}
}

// TestBothTitlesAreShown pins that the two names a terminal goes by are not
// assumed to be the same one.
//
// A prompt framework sets the tab title to the running command's name and the
// window title to its whole command line; an app that sets both with a single
// OSC 0 sets them to the same string. So they are shown together when they
// differ and once when they do not.
func TestBothTitlesAreShown(t *testing.T) {
	h := newFrameHarness(t)
	h.window(80, 6)

	for _, tc := range []struct{ name, title, subtitle, want string }{
		{"a shell reporting both", "sleep 2", "sleep", "sleep 2 · sleep"},
		{"one OSC 0 setting both", "◐ Claude Code", "◐ Claude Code", "◐ Claude Code"},
		{"only a subtitle", "", "~", "~"},
		{"only a title", "user@host:~", "", "user@host:~"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.s.title, h.s.subtitle = tc.title, tc.subtitle
			got := stripSGR(strings.Split(h.f.View().Content, "\n")[0])
			if !strings.Contains(got, tc.want) {
				t.Errorf("top border = %q, want %q in it", got, tc.want)
			}
			if tc.title == tc.subtitle && strings.Count(got, tc.want) != 1 {
				t.Errorf("top border = %q, want %q said once", got, tc.want)
			}
		})
	}
}

// TestTheFrameNamesTheOriginItServes pins that the corner says what the origin
// is, and that it comes from the target rather than from a guess in the frame:
// a program frames itself as exec:///usr/bin/htop where a container frames
// itself as attach://dockerd/api. The same string pasted back into a command
// line has to work, which is the whole reason it is spelled as an origin.
func TestTheFrameNamesTheOriginItServes(t *testing.T) {
	for _, tc := range []struct{ name, origin, want string }{
		{"api", "", "attach://dockerd/api"},
		// A program names itself by the path that will run, which is what the
		// origin carries: "top" says which program only on the machine that
		// resolved it.
		{"top", v1.ExecScheme + "://" + "/usr/bin/top", "exec:///usr/bin/top"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			h := newFrameHarness(t)
			target := newFakeTarget(tc.name, true, true)
			target.origin = tc.origin
			h.s.Target = target

			if got := h.f.title(); got != tc.want {
				t.Errorf("title() = %q, want %q", got, tc.want)
			}
			top := stripSGR(strings.Split(h.f.View().Content, "\n")[0])
			if !strings.Contains(top, tc.want) {
				t.Errorf("top border = %q, want %q in it", top, tc.want)
			}
		})
	}
}

// TestALongOriginGivesUpItsLeadingSegments pins which end of a program origin
// goes when the top border cannot hold it whole.
//
// The tail names the program and the head is what it costs, so the head is
// what is spent — one segment at a time, and only as many as the width
// demands. A container origin has nothing to spend: its one path segment is
// the container's name.
func TestALongOriginGivesUpItsLeadingSegments(t *testing.T) {
	const origin = "exec:///Users/christian/.local/bin/claude" // 41 columns

	for _, tc := range []struct {
		width int
		want  string
	}{
		{41, origin},
		{80, origin},
		// One short, so the first segment goes and no more.
		{40, "exec:///.../christian/.local/bin/claude"},
		{39, "exec:///.../christian/.local/bin/claude"},
		{38, "exec:///.../.local/bin/claude"},
		{29, "exec:///.../.local/bin/claude"},
		{28, "exec:///.../bin/claude"},
		{22, "exec:///.../bin/claude"},
		{21, "exec:///.../claude"},
		{18, "exec:///.../claude"},
		// Narrower than the program's own name: nothing left to give up, so
		// the origin comes back whole for the border to clip as it always has.
		{17, origin},
		{1, origin},
		{0, origin},
		{-1, origin},
	} {
		t.Run(fmt.Sprintf("%d columns", tc.width), func(t *testing.T) {
			if got := shorten(origin, tc.width); got != tc.want {
				t.Errorf("shorten(%d) = %q, want %q", tc.width, got, tc.want)
			}
		})
	}

	// Everything that is not a path with room in it is handed back untouched:
	// a container's name is the only segment it has, and the authority says
	// which daemon it is on.
	for _, whole := range []string{
		"attach://dockerd/api",
		"attach://dockerd/a-very-long-container-name-indeed",
		"exec:///claude",
		"exec://",
		"claude",
		"",
	} {
		if got := shorten(whole, 8); got != whole {
			t.Errorf("shorten(%q) = %q, want it untouched", whole, got)
		}
	}
}

// TestTheAddressSurvivesALongOrigin pins the two corners of the top border
// against each other, at four widths and in the order they give way.
//
// A program origin is an absolute path, so it is long by construction, and the
// address used to be what the row dropped to make space for it. It is now the
// other way round: the origin gives up its head, then gives up the row, and
// only takes it back below the width where the address fits at all.
func TestTheAddressSurvivesALongOrigin(t *testing.T) {
	h := newFrameHarness(t)
	h.window(h.f.width, 8)

	target := newFakeTarget("claude", true, true)
	target.origin = "exec:///Users/christian/.local/bin/claude"
	h.s.Target = target
	h.s.announce("https://striped-worm.tunneled.pizza/?0")

	// Wide enough for both, and the origin is whole.
	h.window(120, h.f.height)
	top := stripSGR(strings.Split(h.f.View().Content, "\n")[0])
	if !strings.Contains(top, target.origin) {
		t.Errorf("top border = %q, want the whole origin %q in it", top, target.origin)
	}

	// The window from the report: the origin no longer fits beside the
	// address, so it gives up its head and both are shown.
	h.window(84, h.f.height)
	top = stripSGR(strings.Split(h.f.View().Content, "\n")[0])

	if want := h.s.announced() + " \u256e"; !strings.HasSuffix(top, want) {
		t.Errorf("top border = %q, want it ending %q", top, want)
	}
	if !strings.Contains(top, ellipsis+"/.local/bin/claude") {
		t.Errorf("top border = %q, want the origin's tail kept behind an ellipsis", top)
	}
	if strings.Contains(top, "/Users/") {
		t.Errorf("top border = %q, want the origin's head given up", top)
	}
	if len([]rune(top)) != h.f.width {
		t.Errorf("top border is %d columns, want the window's %d", len([]rune(top)), h.f.width)
	}

	// Narrower again, and the two no longer both fit at any length the origin
	// can be reduced to. The address keeps the row: it is minted for this run
	// and said nowhere else, where the origin is a string somebody typed and
	// the page's own title still carries it whole.
	h.window(56, h.f.height)
	top = stripSGR(strings.Split(h.f.View().Content, "\n")[0])
	if want := h.s.announced() + " \u256e"; !strings.HasSuffix(top, want) {
		t.Errorf("top border = %q, want it ending %q", top, want)
	}
	if strings.Contains(top, "claude") || strings.Contains(top, ellipsis) {
		t.Errorf("top border = %q, want the origin given up rather than clipped", top)
	}
	if len([]rune(top)) != h.f.width {
		t.Errorf("top border is %d columns, want the window's %d", len([]rune(top)), h.f.width)
	}

	// Narrower still, and the address cannot be kept at any length the origin
	// could take. It is given up as it always was — but the origin is then
	// fitted to the whole row rather than clipped against the corner, because
	// a clipped one ends mid-path and says nothing about which program this is.
	h.window(40, h.f.height)
	top = stripSGR(strings.Split(h.f.View().Content, "\n")[0])
	if strings.Contains(top, h.s.announced()) {
		t.Errorf("top border = %q, want the address given up at this width", top)
	}
	if !strings.HasSuffix(strings.TrimRight(top, "\u2500\u256e "), "/claude") {
		t.Errorf("top border = %q, want it still naming the program", top)
	}
	if len([]rune(top)) != h.f.width {
		t.Errorf("top border is %d columns, want the window's %d", len([]rune(top)), h.f.width)
	}

	// What the frame calls the page is the origin as it was typed either way:
	// a browser tab is not short of columns.
	if got := h.f.View().WindowTitle; !strings.Contains(got, target.origin) {
		t.Errorf("page title = %q, want the whole origin in it", got)
	}
}

// TestPageTitleNamesTheTerminal pins what the browser tab displaying this
// terminal is called.
//
// It rides on the frame's own window title, which the renderer emits as an OSC
// and the page raises to document.title — the same mechanism the container
// uses to name its terminal, one level out. What the terminal is doing leads,
// because a browser tab loses its end: a row of them all starting attach://
// would be a row that says nothing.
func TestPageTitleNamesTheTerminal(t *testing.T) {
	h := newFrameHarness(t)
	title := h.s.Origin()

	for _, tc := range []struct{ name, said, sub, want string }{
		{"before the terminal says anything", "", "", title},
		{"one OSC 0 setting both", "◐ foo", "◐ foo", "◐ foo · " + title},
		{"a shell reporting both", "sleep 2", "sleep", "sleep 2 · sleep · " + title},
		{"only a subtitle", "", "~", "~ · " + title},
		{"a name with nothing showable in it", "\xe2", "", title},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.s.title, h.s.subtitle = tc.said, tc.sub
			if got := h.f.View().WindowTitle; got != tc.want {
				t.Errorf("page title = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTheLayoutSurvivesALongHostname pins the bottom row against the machine
// it is running on.
//
// The counts carry os.Hostname, and a machine's name is not a constant: a
// laptop's is fourteen characters and a CI runner's is sixty. A row laid out
// for the short one has no space for the build when the long one turns up, and
// a test that assumed the short one passed everywhere its author looked and
// failed on two of eight lanes.
func TestTheLayoutSurvivesALongHostname(t *testing.T) {
	// What the macOS runners are actually called.
	restore := host
	host = func() string { return "sjc20-bb661-6a8fb526-3115-4bb6-b587-043ab41116cd-2A8936E83050.local" }
	t.Cleanup(func() { host = restore })

	h := newFrameHarness(t)
	h.window(h.f.width, 8)
	wide, tight := roomFor(h.f)

	h.window(wide, h.f.height)
	bottom := stripSGR(bottomOf(h))
	for _, want := range []string{"^K", testBanner, "1 viewer", host()} {
		if !strings.Contains(bottom, want) {
			t.Errorf("bottom border = %q, want %q in it", bottom, want)
		}
	}
	if len([]rune(bottom)) != wide {
		t.Errorf("bottom border is %d columns, want the window's %d", len([]rune(bottom)), wide)
	}

	// And the order it gives way in does not change with the name either.
	h.window(tight, h.f.height)
	if got := stripSGR(bottomOf(h)); strings.Contains(got, testBanner) {
		t.Errorf("bottom border = %q, want the build dropped before the counts", got)
	} else if !strings.Contains(got, "1 viewer") {
		t.Errorf("bottom border = %q, want the counts kept", got)
	}
}

// wheel turns the wheel once at a frame position, the way the page reports it:
// one message per line, at the cell under the pointer.
func (h *harness) wheel(t *testing.T, b tea.MouseButton, x, y int) {
	t.Helper()
	model, _ := h.f.Update(tea.MouseWheelMsg{X: x, Y: y, Button: b})
	f, ok := model.(frame)
	if !ok {
		t.Fatalf("Update returned %T, want a frame", model)
	}
	h.f = f
}

// paneRow is one row of the pane as text, borders and styling gone.
func (h *harness) paneRow(t *testing.T, y int) string {
	t.Helper()
	lines := strings.Split(h.f.View().Content, "\n")
	if y+1 >= len(lines)-1 {
		t.Fatalf("pane row %d asked for, view has %d rows", y, len(lines))
	}
	row := stripSGR(lines[y+1])
	return strings.TrimSpace(strings.Trim(row, "│"))
}

// scrollOff writes n numbered lines from from, enough to push most of them
// off the pane and into the emulator's scrollback. The cursor is left at the
// end of the last line rather than on a fresh one, so the screen's last row is
// the last line written and the arithmetic in the tests holds.
func (h *harness) scrollOff(t *testing.T, from, n int) {
	t.Helper()
	var b strings.Builder
	for i := from; i < from+n; i++ {
		if i > 1 {
			b.WriteString("\r\n")
		}
		fmt.Fprintf(&b, "line %03d", i)
	}
	if _, err := h.s.em.WriteString(b.String()); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// TestTheWheelLooksBackThroughWhatScrolledOff pins the frame's own use of the
// wheel: on the main screen, with a program that never asked for the mouse,
// scrolling up shows what the emulator kept, the border says how far back the
// viewer is, and the cursor — a position in the present — is withheld.
// Scrolling down past the end is being live again.
func TestTheWheelLooksBackThroughWhatScrolledOff(t *testing.T) {
	h := newFrameHarness(t)
	rows := defaultRows - chromeHeight
	h.scrollOff(t, 1, 3*rows) // lines 001..066 on a 22-row pane: 001..044 are history

	if got := h.paneRow(t, 0); got != fmt.Sprintf("line %03d", 2*rows+1) {
		t.Fatalf("live top row = %q, want the first line still on screen", got)
	}

	h.wheel(t, tea.MouseWheelUp, 5, 5)
	if got := h.paneRow(t, 0); got != fmt.Sprintf("line %03d", 2*rows) {
		t.Errorf("top row after one notch = %q, want the line just above the screen", got)
	}
	if bottom := stripSGR(bottomOf(h)); !strings.Contains(bottom, "↑1") {
		t.Errorf("bottom border = %q, want it saying the viewer is one line back", bottom)
	}
	if h.f.View().Cursor != nil {
		t.Error("a cursor is drawn while scrolled back, where the live position means nothing")
	}
	h.silent(t) // the frame kept the wheel; the program saw no arrow keys

	// The counts give way on a narrow window — and on a CI runner whose
	// hostname is sixty characters — but the chip is the one part of them
	// that says this screen is not live, and it stays.
	// The window alone, not the screen: this is the frame being narrower
	// than what it shows, and the emulator's content has to survive it.
	wide := h.f.width
	h.f.width = 24
	if bottom := stripSGR(bottomOf(h)); strings.Contains(bottom, "viewer") || !strings.Contains(bottom, "↑1") {
		t.Errorf("bottom border at 24 columns = %q, want the counts dropped and the chip kept", bottom)
	}
	h.f.width = wide

	// Past the top is the top.
	for range 10 * rows {
		h.wheel(t, tea.MouseWheelUp, 5, 5)
	}
	if got := h.paneRow(t, 0); got != "line 001" {
		t.Errorf("top row at the top = %q, want the oldest line kept", got)
	}
	if bottom := stripSGR(bottomOf(h)); !strings.Contains(bottom, fmt.Sprintf("↑%d", 2*rows)) {
		t.Errorf("bottom border = %q, want the whole history counted", bottom)
	}

	// And past the bottom is live.
	for range 10 * rows {
		h.wheel(t, tea.MouseWheelDown, 5, 5)
	}
	if got := h.paneRow(t, 0); got != fmt.Sprintf("line %03d", 2*rows+1) {
		t.Errorf("top row after scrolling back down = %q, want the live screen", got)
	}
	if bottom := stripSGR(bottomOf(h)); strings.Contains(bottom, "↑") {
		t.Errorf("bottom border = %q, still says scrolled back after returning to live", bottom)
	}
	if h.f.View().Cursor == nil {
		t.Error("no cursor once live again")
	}
}

// TestAScrolledViewerStaysPutUnderNewOutput pins the pager's rule over the
// terminal's: output arriving while somebody is reading history does not
// move what they are reading. The distance to live grows instead, and the
// border says so.
func TestAScrolledViewerStaysPutUnderNewOutput(t *testing.T) {
	h := newFrameHarness(t)
	rows := defaultRows - chromeHeight
	h.scrollOff(t, 1, 3*rows)

	for range 5 {
		h.wheel(t, tea.MouseWheelUp, 5, 5)
	}
	before := h.paneRow(t, 0)

	h.scrollOff(t, 3*rows+1, 3)
	model, _ := h.f.Update(paneMsg{})
	h.f = model.(frame)

	if got := h.paneRow(t, 0); got != before {
		t.Errorf("top row = %q after new output, want %q — the view slid under the reader", got, before)
	}
	if bottom := stripSGR(bottomOf(h)); !strings.Contains(bottom, "↑8") {
		t.Errorf("bottom border = %q, want the distance to live grown by the three new lines", bottom)
	}
}

// TestAKeySnapsAScrolledViewerBackToLive pins that typing is being present:
// the first keystroke returns the view to the live screen and still reaches
// the program, so somebody who scrolled up to check something and then typed
// sees what their typing did.
func TestAKeySnapsAScrolledViewerBackToLive(t *testing.T) {
	h := newFrameHarness(t)
	rows := defaultRows - chromeHeight
	h.scrollOff(t, 1, 3*rows)

	for range 3 {
		h.wheel(t, tea.MouseWheelUp, 5, 5)
	}
	h.press(t, typing('x'))
	h.reached(t, "x")

	if got := h.paneRow(t, 0); got != fmt.Sprintf("line %03d", 2*rows+1) {
		t.Errorf("top row after typing = %q, want the live screen", got)
	}
	if bottom := stripSGR(bottomOf(h)); strings.Contains(bottom, "↑") {
		t.Errorf("bottom border = %q, still says scrolled back after a keystroke", bottom)
	}
}

// TestTheWheelIsArrowKeysOnTheAlternateScreen pins today's behaviour for a
// full-screen program: it has no history to look back at, so a notch is what
// a terminal with alternate scroll sends it — an arrow key per line.
func TestTheWheelIsArrowKeysOnTheAlternateScreen(t *testing.T) {
	h := newFrameHarness(t)
	if _, err := h.s.scan.Write([]byte("\x1b[?1049h")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !h.s.em.IsAltScreen() {
		t.Fatal("the emulator is not on the alternate screen; the mode did not reach it")
	}

	h.wheel(t, tea.MouseWheelUp, 5, 5)
	h.reached(t, "\x1b[A")
	h.wheel(t, tea.MouseWheelDown, 5, 5)
	h.reached(t, "\x1b[B")
	if bottom := stripSGR(bottomOf(h)); strings.Contains(bottom, "↑") {
		t.Errorf("bottom border = %q, says scrolled back on a screen that has no history", bottom)
	}
}

// TestTheWheelReachesAProgramThatAskedForTheMouse pins the first rule: a
// program that turned mouse reporting on gets the wheel as a mouse event, in
// its own coordinates, and gets the frame's scrolling back the moment it turns
// reporting off — every mode, not just the first one it set.
func TestTheWheelReachesAProgramThatAskedForTheMouse(t *testing.T) {
	h := newFrameHarness(t)
	rows := defaultRows - chromeHeight
	h.scrollOff(t, 1, 3*rows)

	if _, err := h.s.scan.Write([]byte("\x1b[?1000h\x1b[?1002h\x1b[?1006h")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !h.s.mouseWanted() {
		t.Fatal("the session did not record the mouse modes; the scanner is not wired")
	}

	// Frame (5,3) is pane (4,2), which SGR reports one-based.
	h.wheel(t, tea.MouseWheelUp, 5, 3)
	h.reached(t, "\x1b[<64;5;3M")
	if got := h.paneRow(t, 0); got != fmt.Sprintf("line %03d", 2*rows+1) {
		t.Errorf("top row = %q, want the live screen — the wheel was the program's", got)
	}

	// Half off is still on.
	if _, err := h.s.scan.Write([]byte("\x1b[?1000l")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !h.s.mouseWanted() {
		t.Fatal("one mode cleared and the session forgot the other")
	}
	if _, err := h.s.scan.Write([]byte("\x1b[?1002l")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if h.s.mouseWanted() {
		t.Fatal("every mode cleared and the session still thinks the program wants the mouse")
	}

	h.wheel(t, tea.MouseWheelUp, 5, 3)
	h.silent(t)
	if got := h.paneRow(t, 0); got != fmt.Sprintf("line %03d", 2*rows) {
		t.Errorf("top row = %q, want history — the wheel is the frame's again", got)
	}
}

// TestEveryFrameAsksForTheMouse pins that a frame declares the mouse to
// whatever it is drawn on — a console's terminal and xterm in a tab alike.
// That is what brings the wheel in for scrollback, and it is what takes the
// host's own drag-select away, which the frame's selection gives back the
// same way in both places.
func TestEveryFrameAsksForTheMouse(t *testing.T) {
	h := newFrameHarness(t)
	if got := h.f.View().MouseMode; got != tea.MouseModeCellMotion {
		t.Errorf("MouseMode = %v, want cell motion — clicks, drags and the wheel", got)
	}
}

// TestTheBoxIsThePane pins where the border goes on a window larger than the
// shared screen: around the screen, centred, not around the window. The
// emulator has settled on the smallest viewer, and a border drawn at this
// viewer's own edges puts blank margin inside it — which reads as the program
// stopping short, when it is another viewer's window being smaller. Outside
// the box is nothing, and the corner chip sits at the corner of the screen it
// describes.
func TestTheBoxIsThePane(t *testing.T) {
	h := newFrameHarness(t)
	if _, err := h.s.em.WriteString("shared screen"); err != nil {
		t.Fatalf("write: %v", err)
	}
	h.f.width, h.f.height = defaultCols+40, defaultRows+16

	lines := strings.Split(h.f.View().Content, "\n")
	if len(lines) != defaultRows+16 {
		t.Fatalf("view has %d rows, want the whole window's %d", len(lines), defaultRows+16)
	}
	w, hgt := h.s.paneSize()
	boxW, boxH := w+chromeWidth, hgt+chromeHeight
	left, top := (h.f.width-boxW)/2, (h.f.height-boxH)/2

	for y := 0; y < top; y++ {
		if strings.TrimSpace(stripSGR(lines[y])) != "" {
			t.Fatalf("row %d = %q, want nothing above the box", y, stripSGR(lines[y]))
		}
	}
	topRow := stripSGR(lines[top])
	if got := uv.NewStyledString(strings.TrimRight(topRow, " ")).UnicodeWidth(); got != left+boxW {
		t.Errorf("top border ends at column %d, want %d — a %d-column box starting at %d", got, left+boxW, boxW, left)
	}
	if !strings.HasPrefix(topRow, strings.Repeat(" ", left)+"╭") {
		t.Errorf("top border = %q, want it starting at column %d", topRow, left)
	}
	bottom := stripSGR(lines[top+boxH-1])
	if !strings.HasSuffix(strings.TrimRight(bottom, " "), "╯") {
		t.Errorf("row %d = %q, want the bottom border there — the box ends where the screen does", top+boxH-1, bottom)
	}
	// The keys, not the counts: the counts carry os.Hostname and give way on
	// a CI runner's sixty-character name, which TestTheLayoutSurvivesALongHostname
	// covers. The keys keep their columns, and start where the box does.
	if !strings.HasPrefix(bottom, strings.Repeat(" ", left)+"╰─ ^K") {
		t.Errorf("bottom border = %q, want the keys at the box's own left edge, column %d", bottom, left)
	}
	for y := top + boxH; y < len(lines); y++ {
		if strings.TrimSpace(stripSGR(lines[y])) != "" {
			t.Errorf("row %d = %q, want nothing below the box", y, stripSGR(lines[y]))
			break
		}
	}
	if !strings.Contains(stripSGR(lines[top+1]), "shared screen") {
		t.Errorf("row %d = %q, want the screen inside the box", top+1, stripSGR(lines[top+1]))
	}
	// The cursor moves with the box: the pane's origin plus where the program
	// left it, which is after what it wrote on the first row.
	if c := h.f.View().Cursor; c == nil || c.X != left+1+len("shared screen") || c.Y != top+1 {
		t.Errorf("cursor = %v, want (%d,%d) — the pane's origin plus the program's position", c, left+1+len("shared screen"), top+1)
	}

	// A window smaller than the screen is still the window: the box cannot
	// be bigger than what it is drawn on.
	h.f.width, h.f.height = defaultCols-10, defaultRows-4
	lines = strings.Split(h.f.View().Content, "\n")
	if len(lines) != defaultRows-4 {
		t.Fatalf("view has %d rows on a small window, want %d", len(lines), defaultRows-4)
	}
	if last := stripSGR(lines[len(lines)-1]); !strings.HasSuffix(strings.TrimRight(last, " "), "╯") {
		t.Errorf("last row = %q, want the bottom border at the window's edge", last)
	}
}

// TestTheAddressIsAQRCodeAway pins the view: ^K q draws the public address as
// a code with the address under it, keys are spent on reading rather than
// reaching the program, the cursor is withheld, and esc is the way back.
func TestTheAddressIsAQRCodeAway(t *testing.T) {
	h := newFrameHarness(t)
	h.window(100, 40) // room for a code: 21 modules plus the quiet zone is 29 columns, 15 rows
	h.s.announce("https://striped-worm.tunneled.pizza/?0")

	h.press(t, commandKey)
	h.press(t, typing('q'))

	pane := stripSGR(h.f.View().Content)
	if !strings.ContainsAny(pane, "█▀▄") {
		t.Errorf("pane = %q, want a code drawn in half blocks", pane)
	}
	if !strings.Contains(pane, "striped-worm.tunneled.pizza/?0") {
		t.Errorf("pane = %q, want the address under the code for whoever would rather type", pane)
	}
	if h.f.View().Cursor != nil {
		t.Error("a cursor is drawn over the code, where the live position means nothing")
	}
	if bottom := stripSGR(bottomOf(h)); !strings.Contains(bottom, "back to the terminal") {
		t.Errorf("bottom border = %q, want it saying how to get back", bottom)
	}

	// Holding a phone up is not typing.
	h.press(t, typing('x'))
	h.silent(t)

	h.press(t, tea.Key{Code: tea.KeyEscape})
	if pane := stripSGR(h.f.View().Content); strings.ContainsAny(pane, "█▀▄") {
		t.Errorf("pane = %q, still shows the code after esc", pane)
	}
	if h.f.View().Cursor == nil {
		t.Error("no cursor once back on the terminal")
	}
}

// TestAPaneTooSmallForACodeSaysSo pins the two ways the view has nothing to
// draw: a pane the code will not fit, which gets the address as text and the
// size a code needs, and a run with no address yet. Neither draws part of a
// code, because part of a code is not a smaller code.
func TestAPaneTooSmallForACodeSaysSo(t *testing.T) {
	h := newFrameHarness(t)
	h.window(60, 10) // 58×8: a code is 29×15 with its quiet zone
	h.s.announce("https://striped-worm.tunneled.pizza/?0")
	h.press(t, commandKey)
	h.press(t, typing('q'))

	pane := stripSGR(h.f.View().Content)
	if strings.ContainsAny(pane, "█▀▄") {
		t.Errorf("pane = %q, want no code on a pane that cannot hold one whole", pane)
	}
	if !strings.Contains(pane, "too small") || !strings.Contains(pane, "striped-worm.tunneled.pizza/?0") {
		t.Errorf("pane = %q, want it saying the pane is too small and giving the address as text", pane)
	}

	h.press(t, tea.Key{Code: tea.KeyEscape})
	h.s.announce("")
	h.press(t, commandKey)
	h.press(t, typing('q'))
	if pane := stripSGR(h.f.View().Content); !strings.Contains(pane, "no address yet") {
		t.Errorf("pane = %q, want it saying there is no address yet", pane)
	}
}

// mouse sends one mouse message to the frame and keeps whatever it became,
// returning the command it produced.
func (h *harness) mouse(t *testing.T, msg tea.Msg) tea.Cmd {
	t.Helper()
	model, cmd := h.f.Update(msg)
	f, ok := model.(frame)
	if !ok {
		t.Fatalf("Update returned %T, want a frame", model)
	}
	h.f = f
	return cmd
}

// TestDraggingSelectsAndCopies pins the frame's own selection on a console:
// press, drag and release over the pane highlights the stretch, copies it to
// the viewer's terminal, says so in the border, and never reaches the
// program. The pane starts one cell in from the box, so pane (0,0) is window
// (1,1) here.
func TestDraggingSelectsAndCopies(t *testing.T) {
	h := newFrameHarness(t)
	if _, err := h.s.em.WriteString("hello world\r\nsecond line"); err != nil {
		t.Fatalf("write: %v", err)
	}
	pane := h.f.pane()
	at := func(x, y int) (int, int) { return pane.Min.X + x, pane.Min.Y + y }

	x, y := at(0, 0)
	h.mouse(t, tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	x, y = at(2, 0)
	h.mouse(t, tea.MouseMotionMsg{X: x, Y: y, Button: tea.MouseLeft})
	if !h.f.selecting {
		t.Fatal("the frame is not selecting after a press and a drag")
	}
	if !reversed(h.f.View().Content) {
		t.Error("nothing is drawn reversed mid-drag, want the stretch highlighted as it grows")
	}

	x, y = at(5, 1)
	cmd := h.mouse(t, tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	if cmd == nil {
		t.Fatal("release produced no command, want the copy")
	}
	if msg := cmd(); !strings.Contains(fmt.Sprintf("%T", msg), "lipboard") {
		t.Errorf("release produced %T, want a clipboard message", msg)
	}
	if got := h.f.sel.text(h.f.composed()); got != "hello world\nsecond" {
		t.Errorf("selected text = %q, want the two rows in stream order", got)
	}
	if bottom := stripSGR(bottomOf(h)); !strings.Contains(bottom, "copied") {
		t.Errorf("bottom border = %q, want it saying the text was copied", bottom)
	}
	h.silent(t) // the program saw none of it

	// Typing spends the selection, and the key still goes where it was going.
	h.press(t, typing('x'))
	h.reached(t, "x")
	if h.f.selected || reversed(h.f.View().Content) {
		t.Error("the selection outlived a keystroke")
	}
}

// reverseSGR matches an SGR that turns reverse video on, alone or among other
// parameters — the renderer folds a cell's attributes into one sequence, so
// a selected cell arrives as \x1b[39;7m as readily as \x1b[7m.
var reverseSGR = regexp.MustCompile(`\x1b\[(?:[0-9]+;)*7(?:;[0-9]+)*m`)

// reversed reports whether rendered content draws anything in reverse video.
func reversed(content string) bool { return reverseSGR.MatchString(content) }

// TestAClickIsStillAClick pins the two ways a press selects nothing: let go
// where it went down, or put down outside the pane. Neither copies, and a
// press outside clears what was selected before.
func TestAClickIsStillAClick(t *testing.T) {
	h := newFrameHarness(t)
	if _, err := h.s.em.WriteString("hello world"); err != nil {
		t.Fatalf("write: %v", err)
	}
	pane := h.f.pane()

	h.mouse(t, tea.MouseClickMsg{X: pane.Min.X + 3, Y: pane.Min.Y, Button: tea.MouseLeft})
	if cmd := h.mouse(t, tea.MouseReleaseMsg{X: pane.Min.X + 3, Y: pane.Min.Y, Button: tea.MouseLeft}); cmd != nil || h.f.selected {
		t.Error("a press released where it began selected something")
	}

	// A real selection, then a press on the border.
	h.mouse(t, tea.MouseClickMsg{X: pane.Min.X, Y: pane.Min.Y, Button: tea.MouseLeft})
	h.mouse(t, tea.MouseReleaseMsg{X: pane.Min.X + 4, Y: pane.Min.Y, Button: tea.MouseLeft})
	if !h.f.selected {
		t.Fatal("a drag did not select")
	}
	if cmd := h.mouse(t, tea.MouseClickMsg{X: 0, Y: 0, Button: tea.MouseLeft}); cmd != nil || h.f.selected || h.f.selecting {
		t.Error("a press on the border kept or started a selection")
	}

	// The right button is nobody's.
	h.mouse(t, tea.MouseClickMsg{X: pane.Min.X, Y: pane.Min.Y, Button: tea.MouseRight})
	if h.f.selecting {
		t.Error("a right press started a selection")
	}
}

// TestAConsoleFrameLingersAfterTheRunEnds pins #146's fix. A frame told to
// linger keeps the last screen up when the run ends, says so in the border,
// withholds the cursor, and leaves on the next key — which is what keeps the
// terminal's answers to the renderer's startup queries from landing on the
// prompt, and what lets the line saying why a program exited be read. A
// frame not told to linger, the browser's, quits at once as before.
func TestAConsoleFrameLingersAfterTheRunEnds(t *testing.T) {
	h := newFrameHarness(t)
	if _, err := h.s.em.WriteString("bash: bash: No such file or directory"); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The browser's frame: gone means quit.
	model, cmd := h.f.Update(goneMsg{})
	if cmd == nil {
		t.Fatal("a tab's frame produced no command on goneMsg, want Quit")
	} else if _, quit := cmd().(tea.QuitMsg); !quit {
		t.Errorf("a tab's frame produced %T on goneMsg, want QuitMsg", cmd())
	}
	h.f = model.(frame)

	// The console's: gone means stay, say so, wait for a key.
	h.f.linger = true
	model, cmd = h.f.Update(goneMsg{})
	h.f = model.(frame)
	if cmd != nil {
		t.Errorf("a lingering frame produced %T on goneMsg, want nothing — it stays up", cmd())
	}
	if !h.f.ended {
		t.Fatal("the frame did not record the run ending")
	}
	if pane := stripSGR(h.f.View().Content); !strings.Contains(pane, "No such file or directory") {
		t.Errorf("pane = %q, want the last screen still shown", pane)
	}
	if bottom := stripSGR(bottomOf(h)); !strings.Contains(bottom, "ended") || !strings.Contains(bottom, "any key") {
		t.Errorf("bottom border = %q, want it saying the run ended and how to leave", bottom)
	}
	if h.f.View().Cursor != nil {
		t.Error("a cursor is drawn on a screen whose program is gone")
	}

	cmd = h.press(t, typing('x'))
	if cmd == nil {
		t.Fatal("a key on a lingering frame produced no command, want Quit")
	} else if _, quit := cmd().(tea.QuitMsg); !quit {
		t.Errorf("a key on a lingering frame produced %T, want QuitMsg", cmd())
	}
	h.silent(t) // the key was the reader leaving, not typing at a dead program
}

// TestTheFrameDrawsEveryRowAfterAResize pins the pane against vt's dirty-row
// optimisation. Emulator.Draw paints only the rows touched since the last
// Draw, and a Resize clears that set — so a program that wrote two lines and
// then fell quiet, as a server does after its startup banner, drew as an
// empty screen with the cursor two rows down from the moment the first viewer
// settled the size. The frame composes a fresh buffer every render, so it
// has to draw every row every time.
func TestTheFrameDrawsEveryRowAfterAResize(t *testing.T) {
	h := newFrameHarness(t)
	if _, err := h.s.em.WriteString("Serving HTTP on :: port 8000 ...\r\nGET / 200\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	h.window(defaultCols+40, defaultRows+10) // the first viewer settles the size

	pane := stripSGR(h.f.View().Content)
	if !strings.Contains(pane, "Serving HTTP on :: port 8000") || !strings.Contains(pane, "GET / 200") {
		t.Errorf("pane = %q, want both lines the program wrote before the resize", pane)
	}
	if c := h.f.View().Cursor; c == nil || c.Y != h.f.pane().Min.Y+2 {
		t.Errorf("cursor = %v, want it two rows down, under the text", c)
	}

	// And through history: a scrolled viewer's live part is drawn the same way.
	h.scrollOff(t, 1, 3*(defaultRows+10))
	h.wheel(t, tea.MouseWheelUp, 5, 5)
	h.window(defaultCols+60, defaultRows+10)
	if pane := stripSGR(h.f.View().Content); !strings.Contains(pane, "line ") {
		t.Errorf("pane = %q, want the screen still drawn while scrolled after a resize", pane)
	}
}

// TestRestartIsOfferedOnlyWhereItWorks pins the hint: r appears in the
// command menu for a program that can be started over, and not for a
// container, where the key would promise something it cannot do.
func TestRestartIsOfferedOnlyWhereItWorks(t *testing.T) {
	h := newFrameHarness(t) // a container: not repeatable
	h.press(t, commandKey)
	if bottom := stripSGR(bottomOf(h)); strings.Contains(bottom, " r ") {
		t.Errorf("bottom border = %q, offers r for a container", bottom)
	}
	h.press(t, tea.Key{Code: tea.KeyEscape})

	h.s.Target = newRerunTarget(true) // a program that can run again and can be ended
	h.press(t, commandKey)
	if bottom := stripSGR(bottomOf(h)); !strings.Contains(bottom, " r ") || !strings.Contains(bottom, "restart") {
		t.Errorf("bottom border = %q, want r restart offered for a program", bottom)
	}
}

// TestBannerSitsAboveTheBox pins the messages of the day: one row per
// message on top of the border, centred, in every view the frame has, with
// the pane shorter by their count. The window here is the box's own size, so
// the bar's rows are the window's first ones; TestBannerRidesOnTheBox covers
// a window with room to spare.
func TestBannerSitsAboveTheBox(t *testing.T) {
	h := newFrameHarness(t)
	h.s.motd = testMotd{"WARNING public", "NOTE 日本語"}
	h.window(defaultCols, defaultRows)

	lines := strings.Split(h.f.View().Content, "\n")
	if got := strings.TrimSpace(stripSGR(lines[0])); got != "WARNING public" {
		t.Errorf("row 0 = %q, want the first message", got)
	}
	if got := strings.TrimSpace(stripSGR(lines[1])); got != "NOTE 日本語" {
		t.Errorf("row 1 = %q, want the second message", got)
	}
	// Centred: as much space before as after, within a cell. The render
	// trims a row's trailing blanks, so what is after is measured against
	// the window rather than read off the row.
	plain := strings.TrimRight(stripSGR(lines[0]), " ")
	left := len(plain) - len(strings.TrimLeft(plain, " "))
	right := defaultCols - uv.NewStyledString(plain).UnicodeWidth()
	if d := left - right; d < -1 || d > 1 {
		t.Errorf("row 0 has %d cells left and %d right, want centred", left, right)
	}
	if !strings.Contains(lines[2], "╭") {
		t.Errorf("row 2 = %q, want the box's top border under the banner", stripSGR(lines[2]))
	}
	if pane := h.f.pane(); pane.Min.Y != 3 {
		t.Errorf("pane starts at row %d, want 3 (two banner rows and the border)", pane.Min.Y)
	}

	for _, view := range []func(){
		func() { h.press(t, commandKey) },
		func() {
			h.press(t, tea.Key{Code: tea.KeyEscape})
			h.press(t, commandKey)
			h.press(t, tea.Key{Code: 'l'})
		},
		// Scrolled back: enough lines to push most into the emulator's
		// history, then one notch up. The banner is the frame's, not the
		// screen's, so looking back through what scrolled off keeps it.
		func() {
			h.press(t, tea.Key{Code: tea.KeyEscape})
			h.scrollOff(t, 1, 3*defaultRows)
			h.wheel(t, tea.MouseWheelUp, 5, 5)
			if bottom := stripSGR(bottomOf(h)); !strings.Contains(bottom, "↑1") {
				t.Fatalf("bottom border = %q, want the view scrolled back one line", bottom)
			}
		},
		// The QR code, ^K q.
		func() {
			h.press(t, commandKey)
			h.press(t, typing('q'))
			if bottom := stripSGR(bottomOf(h)); !strings.Contains(bottom, "back to the terminal") {
				t.Fatalf("bottom border = %q, want the QR view", bottom)
			}
		},
	} {
		view()
		if got := strings.TrimSpace(stripSGR(strings.Split(h.f.View().Content, "\n")[0])); got != "WARNING public" {
			t.Errorf("a frame view lost the banner: row 0 = %q", got)
		}
	}
}

// TestBannerIsABar pins that a banner row fills the box edge to edge in the
// severity's colour, the same bar the panel's own strip draws, rather than an
// island of coloured text with the window's own background showing on either
// side of it. The box fills the harness's window, so its edges are the
// window's.
func TestBannerIsABar(t *testing.T) {
	styled := ansi.Style{}.BackgroundColor(ansi.IndexedColor(214)).ForegroundColor(ansi.IndexedColor(232)).Styled("WARNING public")
	h := newFrameHarness(t)
	h.s.motd = testMotd{styled, "plain, no severity"}

	buf := uv.NewScreenBuffer(h.f.width, h.f.height)
	h.f.drawBanner(buf)

	for _, x := range []int{0, h.f.width - 1} {
		if cell := buf.CellAt(x, 0); cell == nil || cell.Style.Bg != ansi.IndexedColor(214) {
			t.Errorf("row 0 col %d = %+v, want the fill colour 214 all the way to the edge", x, cell)
		}
	}
	// A plain row carries no severity to read a fill colour off of, so
	// nothing past its text is touched: the window's own background still
	// shows at both ends, the way it always has.
	for _, x := range []int{0, h.f.width - 1} {
		if cell := buf.CellAt(x, 1); cell != nil && cell.Style.Bg != nil {
			t.Errorf("plain row col %d background = %v, want none", x, cell.Style.Bg)
		}
	}
}

// TestBannerRidesOnTheBox pins where the bar goes when the box is smaller
// than the window: on the box, as its title bar, box-wide and directly above
// the border — not at the top of the window a screen-height away, and not
// across columns the box does not have.
func TestBannerRidesOnTheBox(t *testing.T) {
	styled := ansi.Style{}.BackgroundColor(ansi.IndexedColor(214)).ForegroundColor(ansi.IndexedColor(232)).Styled("WARNING public")
	h := newFrameHarness(t)
	h.s.motd = testMotd{styled}
	h.window(160, 50)
	// A smaller viewer has negotiated the screen down, so this window holds
	// a box with margin on every side.
	h.s.em.Resize(78, 22)

	box := h.f.box()
	if want := 22 + chromeHeight + 1; box.Dy() != want {
		t.Errorf("box is %d rows, want %d (the screen, its border and the bar)", box.Dy(), want)
	}

	// Replayed into a screen of the window's size, so what is asserted is the
	// cells a viewer's terminal ends up holding rather than the frame's own
	// buffer.
	screen := vt.NewEmulator(h.f.width, h.f.height)
	if _, err := screen.WriteString(strings.ReplaceAll(h.f.View().Content, "\n", "\r\n")); err != nil {
		t.Fatalf("replay the view: %v", err)
	}
	bg := func(x, y int) any {
		if cell := screen.CellAt(x, y); cell != nil && cell.Style.Bg != nil {
			return cell.Style.Bg
		}
		return nil
	}

	for _, x := range []int{box.Min.X, box.Max.X - 1} {
		if got := bg(x, box.Min.Y); got != ansi.IndexedColor(214) {
			t.Errorf("bar at col %d row %d background = %v, want 214 to the box's edge", x, box.Min.Y, got)
		}
	}
	if got := bg(0, box.Min.Y); got != nil {
		t.Errorf("col 0 on the bar's row background = %v, want none outside the box", got)
	}
	if box.Min.Y > 0 {
		if got := bg(box.Min.X, 0); got != nil {
			t.Errorf("window row 0 background = %v, want none: the bar is on the box, not the window", got)
		}
	} else {
		t.Errorf("box starts at row 0, want it centred below the window's top")
	}
	if cell := screen.CellAt(box.Min.X, box.Min.Y+1); cell == nil || cell.Content != "╭" {
		t.Errorf("cell under the bar = %+v, want the border's top-left corner", cell)
	}
	if got, want := h.f.pane().Min.Y, box.Min.Y+2; got != want {
		t.Errorf("pane starts at row %d, want %d (the bar and the border)", got, want)
	}
}

// TestBannerNeverEatsTheWholeWindow pins the short window: the banner rows
// are drawn, the pane keeps its one-row floor, and nothing indexes past the
// buffer.
func TestBannerNeverEatsTheWholeWindow(t *testing.T) {
	h := newFrameHarness(t)
	h.s.motd = testMotd{"a", "b", "c"}
	h.window(40, 4)
	_ = h.f.View() // must not panic
}

// TestAnEmbeddedViewerDrawsNoBar pins a viewer framed by the multiview panel:
// the panel already shows what the provider said, once, above every tile, so
// the frame inside a tile draws no bar of its own. The pane is still the
// session's, shorter by the bar everyone else has, so the box has a spare row
// in the window and the border is where the box starts.
//
// The same session with the flag down is the control: the bar is still there
// for a viewer in its own right. TestBannerSitsAboveTheBox covers that viewer
// whole.
func TestAnEmbeddedViewerDrawsNoBar(t *testing.T) {
	h := newFrameHarness(t)
	h.s.motd = testMotd{"WARNING public"}
	h.f.embedded = true
	h.window(defaultCols, defaultRows)

	lines := strings.Split(h.f.View().Content, "\n")
	if !strings.Contains(lines[0], "╭") {
		t.Errorf("row 0 = %q, want the box's top border", stripSGR(lines[0]))
	}
	for i, line := range lines {
		if strings.Contains(stripSGR(line), "WARNING") {
			t.Errorf("row %d = %q, want no bar in an embedded viewer", i, stripSGR(line))
		}
	}
	if pane := h.f.pane(); pane.Min.Y != 1 {
		t.Errorf("pane starts at row %d, want 1 (the border alone)", pane.Min.Y)
	}

	h.f.embedded = false
	lines = strings.Split(h.f.View().Content, "\n")
	if got := strings.TrimSpace(stripSGR(lines[0])); got != "WARNING public" {
		t.Errorf("row 0 = %q, want the bar back for a viewer in its own right", got)
	}
}

// TestAnEmbeddedBoxIsTheWindow pins the panel case: a viewer framed by the
// panel draws its border at its window's edges whatever size the shared
// screen settled on, so its frame fills its tile the way the panel's own
// frames fill theirs. The screen stays where the border puts it, top left.
func TestAnEmbeddedBoxIsTheWindow(t *testing.T) {
	h := newFrameHarness(t)
	h.f.embedded = true
	h.window(160, 50)
	h.s.em.Resize(78, 22) // the smallest viewer is much smaller than this one

	if box := h.f.box(); box != uv.Rect(0, 0, 160, 50) {
		t.Errorf("box = %v, want the whole 160x50 window", box)
	}
	lines := strings.Split(h.f.View().Content, "\n")
	if !strings.Contains(lines[0], "╭") || !strings.Contains(stripSGR(lines[len(lines)-1]), "╰") {
		t.Errorf("border is not at the window's top and bottom rows")
	}
	if pane := h.f.pane(); pane.Min.X != 1 || pane.Min.Y != 1 {
		t.Errorf("pane starts at %v, want (1,1) inside the border", pane.Min)
	}
}
