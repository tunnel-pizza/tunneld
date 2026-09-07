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

// TestRepaintRestoresTheScreen is the whole of why the session keeps an
// emulator, pinned without a network, a container or a browser.
//
// A viewer that arrives late used to be handed the bytes the target had
// written and left to replay them into an empty terminal. That reproduces the
// characters, and it can even look right, but it does not reproduce the state
// the app is drawing against — above all where the cursor is. The app's next
// redraw is measured from where it believes the cursor to be, so it lands at
// the wrong origin and paints over what is already there.
//
// So the property worth pinning is not "the text comes out". It is that a
// terminal fed only the repaint is indistinguishable from the one that watched
// the whole stream: same cells, same cursor. Feed both, compare, and a repaint
// that drops an attribute or lands the cursor a row out fails here rather than
// in somebody's tab.
func TestRepaintRestoresTheScreen(t *testing.T) {
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

			// A terminal that saw only the repaint, the way a viewer joining
			// late does.
			restored := vt.NewSafeEmulator(cols, rows)
			restored.SetScrollbackSize(scrollbackLines)
			if _, err := restored.Write(repaint(live)); err != nil {
				t.Fatalf("write the repaint: %v", err)
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
			if want, got := live.CursorPosition(), restored.CursorPosition(); want != got {
				t.Errorf("cursor at %v, want %v — the next redraw would land at the wrong origin", got, want)
			}
			if want, got := live.IsAltScreen(), restored.IsAltScreen(); want != got {
				t.Errorf("alternate screen = %v, want %v", got, want)
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
func TestNegotiateTakesTheSmallestWindow(t *testing.T) {
	s := &session{
		em:      vt.NewSafeEmulator(defaultCols, defaultRows),
		viewers: map[*viewer]struct{}{},
		size:    size(defaultCols, defaultRows),
	}

	wide := &viewer{out: make(chan []byte, 1), size: size(200, 60)}
	narrow := &viewer{out: make(chan []byte, 1), size: size(80, 24)}

	s.viewers[wide] = struct{}{}
	if got, want := s.negotiate(), size(200, 60); got != want {
		t.Errorf("one viewer settled on %v, want its own window %v", got, want)
	}

	s.viewers[narrow] = struct{}{}
	if got, want := s.negotiate(), size(80, 24); got != want {
		t.Errorf("two viewers settled on %v, want the smaller %v", got, want)
	}
	if w, h := s.em.Width(), s.em.Height(); w != 80 || h != 24 {
		t.Errorf("emulator is %dx%d, want it resized with the pty to 80x24", w, h)
	}

	delete(s.viewers, narrow)
	if got, want := s.negotiate(), size(200, 60); got != want {
		t.Errorf("after the smaller left, settled on %v, want %v", got, want)
	}

	// A viewer that has not said how big it is yet must not drag the pty to
	// nothing; it is ignored until it does.
	silent := &viewer{out: make(chan []byte, 1)}
	s.viewers[silent] = struct{}{}
	if got := s.negotiate(); got != (size(0, 0)) {
		t.Errorf("a viewer with no size changed the pty to %v, want no change", got)
	}
}
