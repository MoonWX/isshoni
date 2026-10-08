package main

import (
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

// qrQuietZone is the light border around the code, in modules (04 §3.3). The standard asks for 4; 2 keeps the code
// small enough for an 80-column terminal and phone cameras read it fine.
const qrQuietZone = 2

// qrText draws text as a QR code for a terminal (04 §3.3): half-block characters, so one line of text holds two
// rows of modules, with a quiet zone of qrQuietZone modules. Light modules are drawn (█) and dark ones left blank,
// as go-qrcode's ToSmallString and qrencode do: on the usual dark terminal the picture has the right polarity,
// and on a light one it is the negative, which phones read as well.
//
// The lines carry no indentation and each ends with a newline.
func qrText(text string) (string, error) {
	q, err := qrcode.New(text, qrcode.Medium)
	if err != nil {
		return "", err
	}
	q.DisableBorder = true // the library's border is 4 modules; halfBlocks adds ours
	return halfBlocks(q.Bitmap(), qrQuietZone), nil
}

// halfBlocks draws a module bitmap (true = dark) with quiet light modules around it, two rows per line. It is
// go-qrcode's ToSmallString with a border of our size: a QR code has an odd number of rows, so with an even border
// the last line holds one row, the border's last, as upper half blocks.
func halfBlocks(bits [][]bool, quiet int) string {
	size := len(bits) + 2*quiet
	dark := func(x, y int) bool {
		x, y = x-quiet, y-quiet
		return y >= 0 && y < len(bits) && x >= 0 && x < len(bits[y]) && bits[y][x]
	}
	var b strings.Builder
	for y := 0; y < size; y += 2 {
		for x := range size {
			top, bottom := dark(x, y), dark(x, y+1)
			switch {
			case y+1 == size: // the single last row: nothing is below it
				if top {
					b.WriteByte(' ')
				} else {
					b.WriteString("▀")
				}
			case top && bottom:
				b.WriteByte(' ')
			case top:
				b.WriteString("▄")
			case bottom:
				b.WriteString("▀")
			default:
				b.WriteString("█")
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}
