// Package qrterm draws QR codes with Unicode half blocks so they can be scanned from a terminal.
package qrterm

import (
	"fmt"
	"strings"

	"rsc.io/qr"
)

const (
	// quietZone is the light border scanners need around the code, in modules.
	quietZone = 2
	// rowsPerLine is how many module rows each half-block character covers.
	rowsPerLine = 2
)

// Lines renders text as a QR code, two modules per character row. Light modules are drawn as
// blocks, which suits the common dark terminal background.
func Lines(text string) ([]string, error) {
	code, err := qr.Encode(text, qr.L)
	if err != nil {
		return nil, fmt.Errorf("encode qr code: %w", err)
	}

	light := func(x, y int) bool {
		x, y = x-quietZone, y-quietZone

		return x < 0 || y < 0 || x >= code.Size || y >= code.Size || !code.Black(x, y)
	}

	size := code.Size + quietZone + quietZone
	lines := make([]string, 0, (size+rowsPerLine-1)/rowsPerLine)

	for y := 0; y < size; y += rowsPerLine {
		var b strings.Builder

		for x := range size {
			top, bottom := light(x, y), y+1 < size && light(x, y+1)

			switch {
			case top && bottom:
				b.WriteString("█")
			case top:
				b.WriteString("▀")
			case bottom:
				b.WriteString("▄")
			default:
				b.WriteByte(' ')
			}
		}

		lines = append(lines, b.String())
	}

	return lines, nil
}
