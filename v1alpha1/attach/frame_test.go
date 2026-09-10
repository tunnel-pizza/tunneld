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
	uv "github.com/charmbracelet/ultraviolet"
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
	program.scheme = v1.FileScheme
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
	h.f.height = 8
	h.s.announce("https://striped-worm.tunneled.pizza/?0")

	wide, tight := roomFor(h.f)
	h.f.width = wide

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
	h.f.width = tight
	if got := stripSGR(bottomOf(h)); strings.Contains(got, testBanner) {
		t.Errorf("bottom border = %q, want the build dropped before the counts", got)
	} else if !strings.Contains(got, "1 viewer") {
		t.Errorf("bottom border = %q, want the counts kept", got)
	}

	// Then the counts: what to press matters more than how many are watching.
	h.f.width = 24
	defer func() { h.f.width = wide }()
	if got := stripSGR(bottomOf(h)); strings.Contains(got, "viewer") {
		t.Errorf("bottom border = %q, want the counts dropped rather than overlapping the keys", got)
	} else if strings.Contains(got, testBanner) {
		t.Errorf("bottom border = %q, want the build dropped too", got)
	} else if !strings.Contains(got, "^K") {
		t.Errorf("bottom border = %q, want the keys kept", got)
	}
	h.f.width = wide

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
	h.f.width, h.f.height = 60, 6

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
	h.f.width, h.f.height = 80, 6

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
// a program frames itself as file://htop where a container frames itself as
// dockerd://api. The same string pasted back into a command line has to work,
// which is the whole reason it is spelled as an origin at all.
func TestTheFrameNamesTheOriginItServes(t *testing.T) {
	for _, tc := range []struct{ scheme, name, want string }{
		{v1.DockerScheme, "api", "dockerd://api"},
		// A program names itself by the path that will run, which is what the
		// origin carries: "top" says which program only on the machine that
		// resolved it.
		{v1.FileScheme, "/usr/bin/top", "file:///usr/bin/top"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			h := newFrameHarness(t)
			target := newFakeTarget(tc.name, true, true)
			target.scheme = tc.scheme
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

// TestPageTitleNamesTheTerminal pins what the browser tab displaying this
// terminal is called.
//
// It rides on the frame's own window title, which the renderer emits as an OSC
// and the page raises to document.title — the same mechanism the container
// uses to name its terminal, one level out. What the terminal is doing leads,
// because a browser tab loses its end: a row of them all starting dockerd://
// would be a row that says nothing.
func TestPageTitleNamesTheTerminal(t *testing.T) {
	h := newFrameHarness(t)
	title := v1.DockerScheme + "://" + h.s.Name()

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
	h.f.height = 8
	wide, tight := roomFor(h.f)

	h.f.width = wide
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
	h.f.width = tight
	if got := stripSGR(bottomOf(h)); strings.Contains(got, testBanner) {
		t.Errorf("bottom border = %q, want the build dropped before the counts", got)
	} else if !strings.Contains(got, "1 viewer") {
		t.Errorf("bottom border = %q, want the counts kept", got)
	}
}
