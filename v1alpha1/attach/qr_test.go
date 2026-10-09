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
// right cell has drawn the code. One row per level a caller draws at: the
// frame's, and the one a run's --qr prints.
func TestQRLinesAreTheCode(t *testing.T) {
	const text = "https://striped-worm.tunneled.pizza/?0"
	for _, tc := range []struct {
		name  string
		level qr.Level
	}{
		{"the frame's", qr.L},
		{"a run's --qr", qr.M},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, err := qr.Encode(text, tc.level)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			lines, err := QRLines(text, tc.level)
			if err != nil {
				t.Fatalf("QRLines: %v", err)
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
		})
	}
}

// TestQRSVGIsACode pins the SVG: one rect per dark module on a light field,
// square, with the quiet zone.
func TestQRSVGIsACode(t *testing.T) {
	svg, err := QRSVG("https://x.tunneled.test/")
	if err != nil {
		t.Fatal(err)
	}
	s := string(svg)
	if !strings.HasPrefix(s, "<svg") || !strings.Contains(s, "<rect") || !strings.Contains(s, `viewBox="0 0 `) {
		t.Errorf("not an SVG of rects: %q", s[:min(120, len(s))])
	}
	if _, err := QRSVG(strings.Repeat("x", 5000)); err == nil {
		t.Error("a text too long to encode gave no error")
	}
}
