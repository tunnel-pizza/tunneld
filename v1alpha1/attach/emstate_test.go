package attach

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

// TestReadEmState pins vt's layout: a vt upgrade that moves the fields this
// reads fails here rather than leaving snapshots without them.
func TestReadEmState(t *testing.T) {
	em := vt.NewEmulator(20, 5)
	if _, err := em.Write([]byte("\x1b[31m\x1b]8;id=a;https://example.com\x1b\\\x1b[2;4r\x1b[?7l\x1b=")); err != nil {
		t.Fatal(err)
	}
	st, ok := readEmState(em)
	if !ok {
		t.Fatal("vt's layout changed: readEmState found none of its fields")
	}
	if st.pen.Fg == nil {
		t.Error("pen has no foreground; want red")
	}
	if st.link.URL != "https://example.com" || st.link.Params != "id=a" {
		t.Errorf("link %+v", st.link)
	}
	if st.scroll.Min.Y != 1 || st.scroll.Max.Y != 4 {
		t.Errorf("scroll region %v, want rows 1 to 4", st.scroll)
	}
	if !st.modes[ansi.ModeAutoWrap].IsReset() || !st.modes[ansi.ModeNumericKeypad].IsSet() {
		t.Errorf("modes: autowrap %v, keypad %v", st.modes[ansi.ModeAutoWrap], st.modes[ansi.ModeNumericKeypad])
	}
}
