package attach

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"
	"k8s.io/cri-streaming/pkg/streaming/remotecommand"
)

// size is the shorthand a table row reads better with.
func size(w, h uint16) remotecommand.TerminalSize {
	return remotecommand.TerminalSize{Width: w, Height: h}
}

// TestPaneLinesReproduceTheScreen is the whole of why the session keeps an
// emulator, pinned without a network, a container or a browser.
//
// A viewer that arrives late used to be handed the bytes the target had
// written and left to replay them into an empty terminal. That reproduces the
// characters, and it can even look right, but it does not reproduce the state
// the app is drawing against — above all where the cursor is. The app's next
// redraw is measured from where it believes the cursor to be, so it lands at
// the wrong origin and paints over what is already there.
//
// A frame draws the emulator instead of replaying it, so the property worth
// pinning is that what it draws is the screen: a terminal fed only paneLines
// is indistinguishable, cell for cell, from the one that watched the whole
// stream, and paneCursor says where the app believes it is. A render that
// drops an attribute or loses the cursor fails here rather than in somebody's
// tab.
func TestPaneLinesReproduceTheScreen(t *testing.T) {
	const cols, rows = 40, 10

	for _, tc := range []struct {
		name   string
		stream string
	}{
		{"plain text", "hello\r\nworld"},
		{"styled text", "\x1b[1;31mred bold\x1b[0m and plain"},
		{"the cursor left somewhere else", "abc\r\ndef\x1b[H"},
		{"a screen cleared and redrawn", "junk everywhere\x1b[2J\x1b[Hfresh"},
		{"more lines than fit", strings.Repeat("a line of output\r\n", rows*3)},
		{"an app on the alternate screen", "\x1b[?1049h\x1b[Hfull screen app\x1b[5;5H"},
		{"colours and attributes together", "\x1b[4;32mgreen underline\x1b[0m\r\n\x1b[7mreverse\x1b[0m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// The terminal that watched it happen.
			live := vt.NewSafeEmulator(cols, rows)
			if _, err := live.WriteString(tc.stream); err != nil {
				t.Fatalf("write to the live emulator: %v", err)
			}

			// A terminal that saw only what a frame draws, the way a viewer
			// joining late does. The alternate screen is entered first
			// because the frame is on one and the cells are compared
			// against the buffer the app is actually using.
			restored := vt.NewSafeEmulator(cols, rows)
			if _, err := restored.WriteString("\x1b[?1049h\x1b[H"); err != nil {
				t.Fatalf("enter the alternate screen: %v", err)
			}
			// Joined rather than terminated: a newline after the last line
			// scrolls the screen a row, which is the whole screen wrong by
			// one and exactly what a frame must not do.
			s := &session{em: live}
			drawn := strings.Join(s.paneLines(rows), "\x1b[0m\r\n")
			if _, err := restored.WriteString(drawn); err != nil {
				t.Fatalf("write the pane: %v", err)
			}

			for y := range rows {
				for x := range cols {
					want, got := live.CellAt(x, y), restored.CellAt(x, y)
					if want == nil || got == nil {
						if want != got {
							t.Fatalf("cell (%d,%d): live %v, restored %v", x, y, want, got)
						}
						continue
					}
					if !want.Equal(got) {
						t.Fatalf("cell (%d,%d) = %q %v, want %q %v",
							x, y, got.Content, got.Style, want.Content, want.Style)
					}
				}
			}
			// The cursor is placed by the frame rather than by the bytes, so
			// what is pinned here is that the session still knows where it
			// belongs: a frame reads this and hands it to the renderer.
			if want, got := live.CursorPosition(), s.paneCursor(); want != got {
				t.Errorf("paneCursor at %v, want %v — the next redraw would land at the wrong origin", got, want)
			}
		})
	}
}

// TestNegotiateTakesTheSmallestWindow pins the rule the pty is sized by.
//
// One pty, any number of windows. Anything wider or taller than the smallest
// of them is drawn wrapped or clipped in that window, which looks exactly like
// the corruption this package exists to avoid; a window larger than the pty
// merely has unused margin. So the smallest wins, and it is recomputed when a
// viewer leaves, because the one that left may have been the smallest.
//
// What comes back is the pane and not the window: the frame keeps chromeHeight
// rows and chromeWidth columns for its border, and a container told it had the
// whole window would draw its last row and column underneath one.
func TestNegotiateTakesTheSmallestWindow(t *testing.T) {
	s := &session{
		em:      vt.NewSafeEmulator(defaultCols, defaultRows),
		viewers: map[*viewer]struct{}{},
		size:    size(defaultCols, defaultRows),
	}

	wide := &viewer{wake: make(chan struct{}, 1), size: size(200, 60)}
	narrow := &viewer{wake: make(chan struct{}, 1), size: size(80, 24)}

	s.viewers[wide] = struct{}{}
	if got, want := s.negotiate(), size(200-chromeWidth, 60-chromeHeight); got != want {
		t.Errorf("one viewer settled on %v, want its own window less the chrome %v", got, want)
	}

	s.viewers[narrow] = struct{}{}
	if got, want := s.negotiate(), size(80-chromeWidth, 24-chromeHeight); got != want {
		t.Errorf("two viewers settled on %v, want the smaller %v", got, want)
	}
	if w, h := s.em.Width(), s.em.Height(); w != 80-chromeWidth || h != 24-chromeHeight {
		t.Errorf("emulator is %dx%d, want it resized with the pty to %dx%d",
			w, h, 80-chromeWidth, 24-chromeHeight)
	}

	delete(s.viewers, narrow)
	if got, want := s.negotiate(), size(200-chromeWidth, 60-chromeHeight); got != want {
		t.Errorf("after the smaller left, settled on %v, want %v", got, want)
	}

	// A viewer that has not said how big it is yet must not drag the pty to
	// nothing; it is ignored until it does.
	silent := &viewer{wake: make(chan struct{}, 1)}
	s.viewers[silent] = struct{}{}
	if got := s.negotiate(); got != (size(0, 0)) {
		t.Errorf("a viewer with no size changed the pty to %v, want no change", got)
	}
}

// TestClipboardReplyReachesTheContainerOnlyWhenAsked pins the guard: a reply to
// an outstanding OSC 52 query is written to the container's stdin, and an
// unsolicited one — the danger of forwarding the query at all — is dropped.
func TestClipboardReplyReachesTheContainer(t *testing.T) {
	target := newFakeTarget("api", true, true)
	s := serveFake(t, target)
	sess := s.session

	// No query outstanding: a reply is dropped.
	sess.clipboard('c', "unsolicited")
	select {
	case got := <-target.seenIn:
		t.Fatalf("stdin got %q with no query outstanding, want nothing", got)
	case <-time.After(200 * time.Millisecond):
	}

	// A query marks one outstanding; the reply is written, base64-encoded on
	// the wire.
	sess.said(Sequence{Kind: OSC, Cmd: 52, Data: []byte("c;?"), Raw: []byte("\x1b]52;c;?\a")})
	sess.clipboard('c', "hi")
	select {
	case got := <-target.seenIn:
		if got != "\x1b]52;c;aGk=\a" {
			t.Errorf("stdin = %q, want the base64 reply", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no reply reached stdin after a query")
	}

	// The query is spent: a second reply is dropped.
	sess.clipboard('c', "again")
	select {
	case got := <-target.seenIn:
		t.Fatalf("stdin got %q after the query was answered, want nothing", got)
	case <-time.After(200 * time.Millisecond):
	}

	// A reply after the grace is dropped even with a query outstanding.
	sess.clipboardGrace = time.Nanosecond
	sess.said(Sequence{Kind: OSC, Cmd: 52, Data: []byte("c;?"), Raw: []byte("\x1b]52;c;?\a")})
	time.Sleep(time.Millisecond)
	sess.clipboard('c', "late")
	select {
	case got := <-target.seenIn:
		t.Fatalf("stdin got %q after the grace, want nothing", got)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestTitleFollowsTheShell pins that the frame can say what the container is
// doing, which the container is the only one who knows.
//
// A prompt framework — Oh My Zsh, and most others — sets the terminal's titles
// from preexec and resets them from precmd, so the stream carries the running
// command while one runs and the prompt's own idea of itself when none does.
// It is the same thing a terminal emulator reads to name its tab. It arrives
// as an ordinary escape in the container's output, so the emulator was already
// parsing it and dropping it on the floor.
func TestTitleFollowsTheShell(t *testing.T) {
	target := newFakeTarget("api", true, true)
	// Exactly what a zsh with Oh My Zsh writes when `sleep 2` is run: the
	// window title, then the tab title.
	// Exactly what a zsh with Oh My Zsh writes when `sleep 2` is run: the
	// window title carrying the whole command line, then the tab title
	// carrying its name. Both are sent, and the frame wants the second — so
	// the window title being the wrong one is half of what this pins.
	target.out = "\x1b]2;sleep 2\a\x1b]1;sleep\a"
	s := serveFake(t, target)

	// Both are kept, and they are not the same thing: the title is the whole
	// command line, the subtitle its name.
	deadline := time.Now().Add(5 * time.Second)
	for {
		title, subtitle := s.session.titles()
		if title == "sleep 2" && subtitle == "sleep" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("title = %q, subtitle = %q, want %q and %q", title, subtitle, "sleep 2", "sleep")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestATitleArrivesWhole pins the fix for #93/#66: a title whose bytes include
// 0x9C — ✳ is E2 9C B3 — is delivered whole, and nothing of it lands on the
// screen. The old parser cut it at 0x9C, so the callback saw \xe2 and the rest
// was printed into the container's own screen.
func TestATitleArrivesWhole(t *testing.T) {
	target := newFakeTarget("api", true, true)
	// A drawn row, then the title an app sets over the top of it.
	target.out = "row one\r\n\x1b]0;✳ Claude Code\a"
	s := serveFake(t, target)

	deadline := time.Now().Add(5 * time.Second)
	for {
		title, subtitle := s.session.titles()
		if title == "✳ Claude Code" && subtitle == "✳ Claude Code" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("title = %q, subtitle = %q, want both %q", title, subtitle, "✳ Claude Code")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// #66: the second row is not " Claude Code". The screen carries the drawn
	// row and nothing the title left behind.
	lines := s.session.paneLines(2)
	if len(lines) > 1 && strings.Contains(lines[1], "Claude Code") {
		t.Errorf("row 1 = %q, want the title's residue absent from the screen", lines[1])
	}
}

// TestTitleIsEmptyUntilTheShellSays pins the other half. Most shells set no
// title at all, and a frame must have nothing to show rather than something
// invented.
func TestTitleIsEmptyUntilTheShellSays(t *testing.T) {
	target := newFakeTarget("api", true, true)
	target.out = "a shell that says nothing about itself\r\n"
	s := serveFake(t, target)

	// Long enough for the output above to have been through the emulator.
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(s.session.em.Render(), "says nothing") {
		if time.Now().After(deadline) {
			t.Fatal("the output never reached the screen")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if title, subtitle := s.session.titles(); title != "" || subtitle != "" {
		t.Errorf("titles = %q / %q, want nothing said", title, subtitle)
	}
}
