package attach

import (
	"bytes"
	"fmt"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// snapshot is the terminal as it stands, as bytes that leave a fresh xterm in
// the same state: a reset, the title, the main screen's history and rows,
// the alternate screen if the program is on it, every private mode the
// program has set, the scroll region, the cursor, and the pen. Taken under
// the screen lock, so it is consistent with the stream up to the chunk
// before it.
//
// vt keeps no wrap flag, so a row that fills its last column and is followed
// by one with content is taken to have wrapped: it is written without CR LF
// and the tab's xterm wraps it too. A line the program ended exactly at the
// margin is joined to the next the same way.
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
	kept := s.em.ScrollbackLen()
	rows := make([]func(x int) *uv.Cell, 0, kept+h)
	for y := range kept {
		rows = append(rows, func(x int) *uv.Cell { return s.em.ScrollbackCellAt(x, y) })
	}
	for y := range h {
		rows = append(rows, func(x int) *uv.Cell { return main.CellAt(x, y) })
	}
	for i, at := range rows {
		st.row(&b, w, at)
		if i == len(rows)-1 {
			break
		}
		if filled(w, at) && !empty(w, rows[i+1]) {
			continue
		}
		st.reset(&b)
		b.WriteString("\r\n")
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

	for _, m := range s.modesSet() {
		if m == 1049 {
			continue // set above, with its screen
		}
		fmt.Fprintf(&b, "\x1b[?%dh", m)
	}
	em, _ := readEmState(s.em.Emulator)
	if em.modes[ansi.ModeAutoWrap].IsReset() {
		b.WriteString(ansi.ResetModeAutoWrap)
	}
	if em.modes[ansi.ModeNumericKeypad].IsSet() {
		b.WriteString(ansi.KeypadApplicationMode)
	}
	region := em.scroll
	if region.Empty() {
		region = uv.Rect(0, 0, w, h)
	}
	if region.Min.Y != 0 || region.Max.Y != h {
		b.WriteString(ansi.SetTopBottomMargins(region.Min.Y+1, region.Max.Y))
	}
	if em.modes[ansi.ModeLeftRightMargin].IsSet() && (region.Min.X != 0 || region.Max.X != w) {
		b.WriteString(ansi.SetLeftRightMargins(region.Min.X+1, region.Max.X))
	}
	pos := s.em.CursorPosition()
	if em.modes[ansi.ModeOrigin].IsSet() {
		pos = pos.Sub(region.Min)
	}
	b.WriteString(ansi.CursorPosition(pos.X+1, pos.Y+1))
	st.set(&b, em.pen, em.link)
	if s.cursorHidden() {
		b.WriteString(ansi.ResetModeTextCursorEnable)
	}
	return b.Bytes()
}

// filled reports whether a row's last column has something in it.
func filled(w int, at func(x int) *uv.Cell) bool {
	c := at(w - 1)
	if wideTail(c) && w > 1 {
		c = at(w - 2)
	}
	return !blankCell(c)
}

// empty reports whether a row draws nothing.
func empty(w int, at func(x int) *uv.Cell) bool {
	for x := range w {
		if !blankCell(at(x)) {
			return false
		}
	}
	return true
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
			b.WriteString(ansi.SetHyperlink(link.URL, link.Params))
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
