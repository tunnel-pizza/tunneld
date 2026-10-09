package attach

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"
)

// TestSnapshotRoundTrips pins the snapshot against a fresh emulator: fed the
// bytes, it holds the same screen, history, cursor and alternate screen as
// the session's. Every kind of thing a program draws is in the corpus.
func TestSnapshotRoundTrips(t *testing.T) {
	var long strings.Builder
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&long, "line %02d\r\n", i)
	}
	for _, tc := range []struct {
		name, script string
		after        string // written to both once the snapshot is in
	}{
		{"plain text", "hello\r\nworld", ""},
		{"sixteen colours and attributes", "\x1b[31;1mred bold\x1b[m \x1b[4;3munder italic\x1b[m \x1b[7mrev\x1b[m \x1b[9mstrike\x1b[m \x1b[2mfaint\x1b[m", ""},
		{"256 and true colours", "\x1b[38;5;208morange\x1b[48;2;1;2;3m on blue\x1b[m", ""},
		{"wide characters", "日本語 x", ""},
		{"a hyperlink", "\x1b]8;;https://example.com\x1b\\here\x1b]8;;\x1b\\ there", ""},
		{"history past the screen", long.String(), ""},
		{"the alternate screen", "main text\r\n\x1b[?1049h\x1b[2;3Halt text", ""},
		{"the main screen behind the alternate one", "main text\r\nmore\x1b[?1049h\x1b[2;3Halt text", "\x1b[?1049l"},
		{"a moved cursor", "abc\x1b[3;5H", ""},
		{"a hyperlink with an id", "\x1b]8;id=x1;https://example.com\x1b\\here\x1b]8;;\x1b\\", ""},
		{"a background colour on rows that scroll", strings.Repeat("\x1b[41mX\x1b[m\r\n", 8) + "end", ""},
		{"a pen left set", "\x1b[31;42mred", "more"},
		{"a link left open", "\x1b]8;id=y;https://example.com\x1b\\open", "more"},
		{"a scroll region", "\x1b[2;4r\x1b[4;1H", "a\r\nb\r\nc\r\nd"},
		{"origin mode", "\x1b[2;4r\x1b[?6h\x1b[2;3H", "x"},
		{"autowrap off", "\x1b[?7l", strings.Repeat("w", 25)},
		{"a wrapped line", strings.Repeat("a", 20) + "bb\r\n" + long.String(), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newFrameHarness(t)
			s := h.s
			s.screen.Lock()
			s.em.Resize(20, 5)
			s.screen.Unlock()
			if _, err := s.scan.Write([]byte(tc.script)); err != nil {
				t.Fatal(err)
			}
			snap := s.snapshot()
			if !bytes.HasPrefix(snap, []byte("\x1bc")) {
				t.Fatalf("a snapshot starts with a reset; got %q", snap[:min(8, len(snap))])
			}
			fresh := vt.NewSafeEmulator(20, 5)
			if _, err := fresh.Write(snap); err != nil {
				t.Fatal(err)
			}
			if _, err := s.scan.Write([]byte(tc.after)); err != nil {
				t.Fatal(err)
			}
			if _, err := fresh.Write([]byte(tc.after)); err != nil {
				t.Fatal(err)
			}
			if fresh.IsAltScreen() != s.em.IsAltScreen() {
				t.Errorf("alt screen %v, want %v", fresh.IsAltScreen(), s.em.IsAltScreen())
			}
			if got, want := fresh.ScrollbackLen(), s.em.ScrollbackLen(); got != want {
				t.Errorf("history %d lines, want %d", got, want)
			}
			for y := range s.em.ScrollbackLen() {
				for x := range 20 {
					if !sameCell(fresh.ScrollbackCellAt(x, y), s.em.ScrollbackCellAt(x, y)) {
						t.Errorf("history cell (%d,%d) = %v, want %v", x, y, fresh.ScrollbackCellAt(x, y), s.em.ScrollbackCellAt(x, y))
					}
				}
			}
			for y := range 5 {
				for x := range 20 {
					if !sameCell(fresh.CellAt(x, y), s.em.CellAt(x, y)) {
						t.Errorf("cell (%d,%d) = %v, want %v", x, y, fresh.CellAt(x, y), s.em.CellAt(x, y))
					}
				}
			}
			if got, want := fresh.CursorPosition(), s.em.CursorPosition(); got != want {
				t.Errorf("cursor %v, want %v", got, want)
			}
		})
	}
}

// TestSnapshotCarriesTheModes pins what a fresh emulator cannot be asked
// about: the modes the program set, its hidden cursor, and its title go in
// the bytes.
func TestSnapshotCarriesTheModes(t *testing.T) {
	h := newFrameHarness(t)
	s := h.s
	if _, err := s.scan.Write([]byte("\x1b]2;my title\x07\x1b[?1000h\x1b[?1006h\x1b[?2004h\x1b[?25l\x1b=")); err != nil {
		t.Fatal(err)
	}
	snap := string(s.snapshot())
	for _, want := range []string{"\x1b[?1000h", "\x1b[?1006h", "\x1b[?2004h", "\x1b[?25l", "\x1b]2;my title\x07", "\x1b="} {
		if !strings.Contains(snap, want) {
			t.Errorf("snapshot lacks %q", want)
		}
	}
	if _, err := s.scan.Write([]byte("\x1b[?1000l\x1b[?25h")); err != nil {
		t.Fatal(err)
	}
	snap = string(s.snapshot())
	if strings.Contains(snap, "\x1b[?1000h") || strings.Contains(snap, "\x1b[?25l") {
		t.Error("a cleared mode, or a shown cursor, is still in the snapshot")
	}
}

// sameCell compares what a viewer would see: content, width, style and link.
func sameCell(a, b *uv.Cell) bool {
	blank := func(c *uv.Cell) bool {
		return c == nil || ((c.Content == "" || c.Content == " ") && c.Style.IsZero() && c.Link.URL == "")
	}
	if blank(a) && blank(b) {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.Content == b.Content && a.Width == b.Width && a.Style.Equal(&b.Style) && a.Link == b.Link
}

// TestSnapshotJoinsWrappedRows pins that a line the emulator wrapped comes
// back as one line a tab's xterm wrapped too, and that a full row followed
// by an empty one keeps the empty one.
func TestSnapshotJoinsWrappedRows(t *testing.T) {
	h := newFrameHarness(t)
	s := h.s
	s.screen.Lock()
	s.em.Resize(20, 5)
	s.screen.Unlock()
	full := strings.Repeat("a", 20)
	if _, err := s.scan.Write([]byte(full + "bb\r\n" + full + "\r\n\r\nc")); err != nil {
		t.Fatal(err)
	}
	snap := string(s.snapshot())
	if !strings.Contains(snap, full+"bb") {
		t.Errorf("a wrapped line is split in the snapshot: %q", snap)
	}
	if !strings.Contains(snap, full+"\r\n\r\nc") {
		t.Errorf("a full row before an empty one lost the empty one: %q", snap)
	}
}

// TestSnapshotPlacesTheCursorLast pins the order a terminal needs: setting
// origin mode or a scroll region homes the cursor, so the cursor goes after
// both, relative to the region when origin mode is on.
func TestSnapshotPlacesTheCursorLast(t *testing.T) {
	h := newFrameHarness(t)
	s := h.s
	if _, err := s.scan.Write([]byte("\x1b[2;4r\x1b[?6h\x1b[2;3H")); err != nil {
		t.Fatal(err)
	}
	snap := string(s.snapshot())
	mode, region, cup := strings.Index(snap, "\x1b[?6h"), strings.Index(snap, "\x1b[2;4r"), strings.LastIndex(snap, "\x1b[2;3H")
	if mode < 0 || region < 0 || cup < 0 || cup < mode || cup < region {
		t.Errorf("want ?6h, the region, then the cursor at 2;3 in the region; got %q", snap)
	}
}
