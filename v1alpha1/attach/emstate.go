package attach

import (
	"reflect"
	"unsafe"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

// emState is what vt keeps but does not export: the pen and link the next
// character is drawn with, the scroll region, and the mode table.
type emState struct {
	pen    uv.Style
	link   uv.Link
	scroll uv.Rectangle
	modes  ansi.Modes
}

// readEmState reads emState out of em's unexported fields. ok is false when
// vt's layout is not the one this reads, and the caller then treats the
// terminal as fresh. The caller holds the lock em is written under.
func readEmState(em *vt.Emulator) (st emState, ok bool) {
	defer func() {
		if recover() != nil {
			st, ok = emState{}, false
		}
	}()
	e := reflect.ValueOf(em).Elem()
	scr := e.FieldByName("scr")
	if scr.Kind() != reflect.Pointer || scr.IsNil() {
		return st, false
	}
	scr = scr.Elem()
	cur := scr.FieldByName("cur")
	if !cur.IsValid() {
		return st, false
	}
	var oks [4]bool
	st.pen, oks[0] = peek[uv.Style](cur, "Pen")
	st.link, oks[1] = peek[uv.Link](cur, "Link")
	st.scroll, oks[2] = peek[uv.Rectangle](scr, "scroll")
	st.modes, oks[3] = peek[ansi.Modes](e, "modes")
	return st, oks == [4]bool{true, true, true, true}
}

// peek reads v's field name as a T, exported or not.
func peek[T any](v reflect.Value, name string) (T, bool) {
	var zero T
	f := v.FieldByName(name)
	if !f.IsValid() || f.Type() != reflect.TypeFor[T]() || !f.CanAddr() {
		return zero, false
	}
	return *(*T)(unsafe.Pointer(f.UnsafeAddr())), true
}
