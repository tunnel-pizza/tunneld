package attach

import (
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
)

// screenOf is a buffer with lines written into it, the way the pane is
// composed before a selection reads it.
func screenOf(lines ...string) uv.ScreenBuffer {
	w := 0
	for _, l := range lines {
		w = max(w, uv.NewStyledString(l).UnicodeWidth())
	}
	buf := uv.NewScreenBuffer(w+2, len(lines))
	for y, l := range lines {
		uv.NewStyledString(l).Draw(buf, uv.Rect(0, y, w+2, 1))
	}
	return buf
}

// TestSelectionReadsInStreamOrder pins the shape a terminal selection has:
// the first row from the anchor, whole rows between, the last row to the
// head — whichever way the drag went — with each row's trailing blanks
// dropped.
func TestSelectionReadsInStreamOrder(t *testing.T) {
	buf := screenOf("hello world", "second line", "third")

	for _, tc := range []struct {
		name string
		sel  selection
		want string
	}{
		{"within a row", selection{uv.Pos(0, 0), uv.Pos(4, 0)}, "hello"},
		{"across rows", selection{uv.Pos(6, 0), uv.Pos(5, 1)}, "world\nsecond"},
		{"dragged upward", selection{uv.Pos(5, 1), uv.Pos(6, 0)}, "world\nsecond"},
		{"whole middle row", selection{uv.Pos(10, 0), uv.Pos(0, 2)}, "d\nsecond line\nt"},
		{"past the text", selection{uv.Pos(0, 2), uv.Pos(12, 2)}, "third"},
	} {
		if got := tc.sel.text(buf); got != tc.want {
			t.Errorf("%s: text = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestSelectionSkipsAWideCharactersTail pins that a two-column character is
// copied once: its second cell is the emulator's placeholder, not a space.
func TestSelectionSkipsAWideCharactersTail(t *testing.T) {
	buf := screenOf("日本 x")
	if got := (selection{uv.Pos(0, 0), uv.Pos(5, 0)}).text(buf); got != "日本 x" {
		t.Errorf("text = %q, want the wide characters once each", got)
	}
}

// TestSelectionHighlightTogglesReverse pins the drawing: selected cells get
// reverse, a cell already reversed loses it, and nothing outside changes.
func TestSelectionHighlightTogglesReverse(t *testing.T) {
	buf := screenOf("abcd")
	rev := *buf.CellAt(1, 0)
	rev.Style.Attrs |= uv.AttrReverse
	buf.SetCell(1, 0, &rev)

	(selection{uv.Pos(0, 0), uv.Pos(1, 0)}).highlight(buf)

	if got := buf.CellAt(0, 0).Style.Attrs & uv.AttrReverse; got == 0 {
		t.Error("cell 0 is selected and not reversed")
	}
	if got := buf.CellAt(1, 0).Style.Attrs & uv.AttrReverse; got != 0 {
		t.Error("cell 1 was reversed already and stayed reversed, want it turned back so the selection shows")
	}
	if got := buf.CellAt(2, 0).Style.Attrs & uv.AttrReverse; got != 0 {
		t.Error("cell 2 is outside the selection and got reversed")
	}
	if !strings.Contains(buf.Render(), "\x1b[7m") {
		t.Errorf("rendered = %q, want a reverse SGR in it", buf.Render())
	}
}
