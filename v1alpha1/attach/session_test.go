package attach

import (
	"strings"
	"testing"

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
			live.SetScrollbackSize(scrollbackLines)
			if _, err := live.WriteString(tc.stream); err != nil {
				t.Fatalf("write to the live emulator: %v", err)
			}

			// A terminal that saw only what a frame draws, the way a viewer
			// joining late does. The alternate screen is entered first
			// because the frame is on one and the cells are compared
			// against the buffer the app is actually using.
			restored := vt.NewSafeEmulator(cols, rows)
			restored.SetScrollbackSize(scrollbackLines)
			if _, err := restored.WriteString("\x1b[?1049h\x1b[H"); err != nil {
				t.Fatalf("enter the alternate screen: %v", err)
			}
			// Joined rather than terminated: a newline after the last line
			// scrolls the screen a row, which is the whole screen wrong by
			// one and exactly what a frame must not do.
			s := &session{em: live}
			drawn := strings.Join(s.paneLines(0, rows), "\x1b[0m\r\n")
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
