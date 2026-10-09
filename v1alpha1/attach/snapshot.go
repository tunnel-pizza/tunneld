package attach

import (
	"bytes"
	"fmt"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// snapshot is the terminal as it stands, as bytes that leave a fresh xterm in
// the same state: a reset, the title, the main screen's history and rows,
// the alternate screen if the program is on it, the cursor, and every
// private mode the program has set. Taken under the screen lock, so it is
// consistent with the stream up to the chunk before it.
//
// History rows end in CR LF, so a line the emulator wrapped is several rows
// here: vt keeps no wrap flag to join them by.
func (s *session) snapshot() []byte {
	s.screen.RLock()
	defer s.screen.RUnlock()

	var b bytes.Buffer
	b.WriteString("\x1bc")
	if title, _ := s.titles(); title != "" {
		b.WriteString(ansi.SetWindowTitle(title))
	}

	w, h := s.em.Width(), s.em.Height()
	var st styled
	kept := s.em.ScrollbackLen()
	for y := range kept {
		st.row(&b, w, func(x int) *uv.Cell { return s.em.ScrollbackCellAt(x, y) })
		b.WriteString("\r\n")
	}
	// The main screen's rows follow the history as more lines, so what is on
	// screen is the last h lines written and the history is above them. On
	// the alternate screen the main one is what was kept as it went behind,
	// with its cursor placed before ?1049h saves it.
	alt := s.em.IsAltScreen()
	main := uv.NewScreenBuffer(w, h)
	switch {
	case !alt:
		s.drawPaneLocked(main, main.Bounds())
	case s.behind != nil:
		main = *s.behind
	}
	for y := range h {
		st.row(&b, w, func(x int) *uv.Cell { return main.CellAt(x, y) })
		if y < h-1 {
			b.WriteString("\r\n")
		}
	}
	if alt {
		st.reset(&b)
		b.WriteString(ansi.CursorPosition(s.behindAt.X+1, s.behindAt.Y+1))
		b.WriteString(ansi.SetModeAltScreenSaveCursor)
		alt := uv.NewScreenBuffer(w, h)
		s.drawPaneLocked(alt, alt.Bounds())
		for y := range h {
			b.WriteString(ansi.CursorPosition(1, y+1))
			st.row(&b, w, func(x int) *uv.Cell { return alt.CellAt(x, y) })
		}
	}
	st.reset(&b)

	pos := s.em.CursorPosition()
	b.WriteString(ansi.CursorPosition(pos.X+1, pos.Y+1))
	for _, m := range s.modesSet() {
		if m == 1049 {
			continue // set above, with its screen
		}
		fmt.Fprintf(&b, "\x1b[?%dh", m)
	}
	if s.cursorHidden() {
		b.WriteString(ansi.ResetModeTextCursorEnable)
	}
	return b.Bytes()
}

// styled writes cells as text with the SGR and hyperlink transitions between
// them, remembering what the terminal was last told.
type styled struct {
	cur  uv.Style
	link uv.Link
}

// row writes one row of w cells: a wide character once, its tail skipped,
// and trailing blank cells with no style left off.
func (st *styled) row(b *bytes.Buffer, w int, at func(x int) *uv.Cell) {
	last := -1
	for x := 0; x < w; x++ {
		if c := at(x); !wideTail(c) && !blankCell(c) {
			last = x
		}
	}
	for x := 0; x <= last; {
		c := at(x)
		if c == nil {
			st.set(b, uv.Style{}, uv.Link{})
			b.WriteByte(' ')
			x++
			continue
		}
		if wideTail(c) {
			x++
			continue
		}
		st.set(b, c.Style, c.Link)
		if c.Content == "" {
			b.WriteByte(' ')
		} else {
			b.WriteString(c.Content)
		}
		x += max(1, c.Width)
	}
}

func (st *styled) set(b *bytes.Buffer, style uv.Style, link uv.Link) {
	if !style.Equal(&st.cur) {
		b.WriteString(style.Diff(&st.cur))
		st.cur = style
	}
	if link != st.link {
		if link.URL == "" {
			b.WriteString(ansi.ResetHyperlink())
		} else {
			b.WriteString(ansi.SetHyperlink(link.URL))
		}
		st.link = link
	}
}

func (st *styled) reset(b *bytes.Buffer) { st.set(b, uv.Style{}, uv.Link{}) }

// wideTail is the second cell of a wide character, which its head carried.
func wideTail(c *uv.Cell) bool { return c != nil && c.Width == 0 && c.Content == "" }

// blankCell is a cell that draws nothing: no content, no style, no link.
func blankCell(c *uv.Cell) bool {
	return c == nil || ((c.Content == "" || c.Content == " ") && c.Style.IsZero() && c.Link.URL == "")
}
