package attach

import (
	"strings"

	uv "github.com/charmbracelet/ultraviolet"
)

// selection is a stretch of the pane a viewer has dragged across, in pane
// coordinates: from anchor, where the button went down, to head, where the
// pointer is or was let go. Stream order, the way a terminal selects — the
// anchor's row from the anchor to its end, every row between whole, the
// head's row from its start to the head — rather than a rectangle.
//
// The frame draws it because the terminal cannot: a terminal that is
// reporting the mouse to an application has handed selection over with it,
// and only the application knows what is under the pointer. The browser tab
// never sees one of these; the page keeps the mouse and selects natively.
type selection struct {
	anchor, head uv.Position
}

// ordered returns the two ends first-to-last in stream order.
func (s selection) ordered() (from, to uv.Position) {
	from, to = s.anchor, s.head
	if to.Y < from.Y || (to.Y == from.Y && to.X < from.X) {
		from, to = to, from
	}
	return from, to
}

// empty reports a selection with nothing in it: a click that did not drag.
func (s selection) empty() bool { return s.anchor == s.head }

// contains reports whether the cell at x, y is inside the selection.
func (s selection) contains(x, y int) bool {
	from, to := s.ordered()
	if y < from.Y || y > to.Y {
		return false
	}
	if y == from.Y && x < from.X {
		return false
	}
	if y == to.Y && x > to.X {
		return false
	}
	return true
}

// highlight marks the selected cells of buf so they read as selected: the
// reverse attribute toggled, so a cell the program already drew reversed
// shows selected by turning back.
func (s selection) highlight(buf uv.ScreenBuffer) {
	from, to := s.ordered()
	w := buf.Bounds().Dx()
	for y := from.Y; y <= to.Y; y++ {
		for x := range w {
			if !s.contains(x, y) {
				continue
			}
			c := buf.CellAt(x, y)
			if c == nil {
				c = &uv.Cell{Content: " "}
			}
			marked := *c
			marked.Style.Attrs ^= uv.AttrReverse
			buf.SetCell(x, y, &marked)
		}
	}
}

// text is what the selection says, as the lines a clipboard wants: each
// row's cells in order, trailing blanks dropped, rows joined by newlines. A
// wide character's second cell is empty and contributes nothing; a cell the
// buffer has nothing for is a blank.
func (s selection) text(buf uv.ScreenBuffer) string {
	from, to := s.ordered()
	w := buf.Bounds().Dx()
	var lines []string
	for y := from.Y; y <= to.Y; y++ {
		var b strings.Builder
		for x := range w {
			if !s.contains(x, y) {
				continue
			}
			c := buf.CellAt(x, y)
			switch {
			case c == nil:
				b.WriteByte(' ')
			case c.Content == "" && c.Width == 0:
				// The tail of a wide character; the head carried it.
			default:
				b.WriteString(c.Content)
			}
		}
		lines = append(lines, strings.TrimRight(b.String(), " "))
	}
	return strings.Join(lines, "\n")
}
