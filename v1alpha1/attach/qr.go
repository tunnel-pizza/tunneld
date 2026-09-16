package attach

import (
	"strings"

	"rsc.io/qr"
)

// quiet is the light margin around a code, in modules. The standard asks for
// four on every side, and a phone camera held at a terminal is not the ideal
// reader the standard had in mind, so it gets all four.
const quiet = 4

// qrLines renders text as a QR code in half-block cells, two modules to a row.
//
// Light modules are drawn and dark ones are left to the background, so the
// frame's usual white-on-black is the code the right way round rather than
// inverted — most cameras read an inverted code, and none has to. Each cell
// carries the module above and the module below: full block for two light,
// upper or lower half for one, a space for none.
//
// Half blocks rather than one cell per module, because a code is square in
// modules and a terminal cell is half as wide as it is tall: drawn a cell per
// module it is twice as tall as it is wide, and a camera has to be told which
// way to squint. Two modules per cell is very nearly square.
func qrLines(text string) ([]string, error) {
	code, err := qr.Encode(text, qr.L)
	if err != nil {
		return nil, err
	}
	n := code.Size + 2*quiet
	light := func(x, y int) bool {
		if y >= n {
			return false // below the grid, on an odd last pair: background
		}
		x, y = x-quiet, y-quiet
		if x < 0 || y < 0 || x >= code.Size || y >= code.Size {
			return true // the quiet zone
		}
		return !code.Black(x, y)
	}

	lines := make([]string, 0, (n+1)/2)
	for y := 0; y < n; y += 2 {
		var b strings.Builder
		for x := range n {
			switch top, bottom := light(x, y), light(x, y+1); {
			case top && bottom:
				b.WriteRune('█')
			case top:
				b.WriteRune('▀')
			case bottom:
				b.WriteRune('▄')
			default:
				b.WriteRune(' ')
			}
		}
		lines = append(lines, b.String())
	}
	return lines, nil
}
