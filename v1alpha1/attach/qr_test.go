package attach

import (
	"strings"
	"testing"
	"unicode/utf8"

	"rsc.io/qr"
)

// TestQRLinesAreTheCode pins the rendering against the matrix it was rendered
// from, module by module: a light module is the half of its cell that is
// drawn, a dark one the half that is not, and the quiet zone is light all the
// way round. There is no decoder here to scan it with, so this is what stands
// in for one — a renderer that maps every module to the right half of the
// right cell has drawn the code.
func TestQRLinesAreTheCode(t *testing.T) {
	const text = "https://striped-worm.tunneled.pizza/?0"
	code, err := qr.Encode(text, qr.L)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	lines, err := qrLines(text)
	if err != nil {
		t.Fatalf("qrLines: %v", err)
	}

	n := code.Size + 2*quiet
	if want := (n + 1) / 2; len(lines) != want {
		t.Fatalf("%d lines, want %d for %d modules two to a row", len(lines), want, n)
	}
	light := func(x, y int) bool {
		if y >= n {
			return false
		}
		x, y = x-quiet, y-quiet
		if x < 0 || y < 0 || x >= code.Size || y >= code.Size {
			return true
		}
		return !code.Black(x, y)
	}
	for row, line := range lines {
		if got := utf8.RuneCountInString(line); got != n {
			t.Fatalf("line %d is %d cells, want %d", row, got, n)
		}
		for x, r := range []rune(line) {
			top, bottom := light(x, 2*row), light(x, 2*row+1)
			var want rune
			switch {
			case top && bottom:
				want = '█'
			case top:
				want = '▀'
			case bottom:
				want = '▄'
			default:
				want = ' '
			}
			if r != want {
				t.Fatalf("cell (%d,%d) = %q, want %q for modules light=%v/%v", x, row, r, want, top, bottom)
			}
		}
	}

	// The quiet zone is drawn, not assumed: the first two rows are all light.
	for _, line := range lines[:quiet/2] {
		if strings.Trim(line, "█") != "" {
			t.Errorf("quiet row %q has a dark module in it", line)
		}
	}
}
