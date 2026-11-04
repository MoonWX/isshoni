//go:build ignore

// icons.go writes the tray icons of docs/m2/03-desktop-app.md §11.1 into this folder: tray-idle.ico, tray-live.ico
// (red), tray-paused.ico (amber) and tray-warn.ico, each with 16, 20, 24 and 32 px images (100 % to 200 % of the
// small icon size), PNG-compressed inside the ICO as Windows Vista and later read it. shell.go embeds them. Run it
// from the repository root after changing a design:
//
//	go run ./cmd/isshoni-desktop/build/tray/icons.go
//
// The output is deterministic, so a run without a change leaves no diff.
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

var sizes = []int{16, 20, 24, 32}

// glyph draws one icon at size n: the shape's coverage at a point of the unit square ((0,0) top left, (1,1) bottom
// right) and its colour there.
type glyph func(x, y float64) color.NRGBA

var (
	slate = color.NRGBA{0x5B, 0x63, 0x7A, 0xFF}
	red   = color.NRGBA{0xDC, 0x26, 0x26, 0xFF}
	amber = color.NRGBA{0xD9, 0x8E, 0x04, 0xFF}
	warn  = color.NRGBA{0xEA, 0x58, 0x0C, 0xFF}
	white = color.NRGBA{0xFF, 0xFF, 0xFF, 0xFF}
	none  = color.NRGBA{}
)

func disc(x, y, cx, cy, r float64) bool { return math.Hypot(x-cx, y-cy) <= r }

func rect(x, y, x0, y0, x1, y1 float64) bool { return x >= x0 && x <= x1 && y >= y0 && y <= y1 }

var icons = map[string]glyph{
	// Idle: a slate ring around a dot.
	"tray-idle": func(x, y float64) color.NRGBA {
		switch {
		case disc(x, y, .5, .5, .2):
			return slate
		case disc(x, y, .5, .5, .46) && !disc(x, y, .5, .5, .34):
			return slate
		}
		return none
	},
	// Live: a red disc with a white dot (on air).
	"tray-live": func(x, y float64) color.NRGBA {
		switch {
		case disc(x, y, .5, .5, .17):
			return white
		case disc(x, y, .5, .5, .46):
			return red
		}
		return none
	},
	// Paused: an amber disc with two white bars.
	"tray-paused": func(x, y float64) color.NRGBA {
		switch {
		case rect(x, y, .34, .3, .44, .7), rect(x, y, .56, .3, .66, .7):
			return white
		case disc(x, y, .5, .5, .46):
			return amber
		}
		return none
	},
	// Warn: an orange disc with a white exclamation mark.
	"tray-warn": func(x, y float64) color.NRGBA {
		switch {
		case rect(x, y, .44, .22, .56, .58), disc(x, y, .5, .72, .07):
			return white
		case disc(x, y, .5, .5, .46):
			return warn
		}
		return none
	},
}

// render draws g at n×n px with 8×8 supersampling.
func render(g glyph, n int) *image.NRGBA {
	const ss = 8
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	for py := range n {
		for px := range n {
			var r, gr, b, a float64
			for sy := range ss {
				for sx := range ss {
					c := g((float64(px)+(float64(sx)+.5)/ss)/float64(n), (float64(py)+(float64(sy)+.5)/ss)/float64(n))
					fa := float64(c.A) / 255
					r, gr, b, a = r+float64(c.R)*fa, gr+float64(c.G)*fa, b+float64(c.B)*fa, a+fa
				}
			}
			if a == 0 {
				continue
			}
			img.SetNRGBA(px, py, color.NRGBA{uint8(r / a), uint8(gr / a), uint8(b / a), uint8(math.Round(a / ss / ss * 255))})
		}
	}
	return img
}

// ico is an ICO file of PNG images (ICONDIR, ICONDIRENTRY per image, then the images).
func ico(images [][]byte, dims []int) []byte {
	var buf bytes.Buffer
	le := binary.LittleEndian
	_ = binary.Write(&buf, le, [3]uint16{0, 1, uint16(len(images))})
	offset := 6 + 16*len(images)
	for i, data := range images {
		d := uint8(dims[i])
		_ = binary.Write(&buf, le, struct {
			W, H, Colors, Reserved uint8
			Planes, BitCount       uint16
			Size, Offset           uint32
		}{d, d, 0, 0, 1, 32, uint32(len(data)), uint32(offset)})
		offset += len(data)
	}
	for _, data := range images {
		buf.Write(data)
	}
	return buf.Bytes()
}

func main() {
	dir := filepath.Join("cmd", "isshoni-desktop", "build", "tray")
	for name, g := range icons {
		var images [][]byte
		for _, n := range sizes {
			var b bytes.Buffer
			enc := png.Encoder{CompressionLevel: png.BestCompression}
			if err := enc.Encode(&b, render(g, n)); err != nil {
				panic(err)
			}
			images = append(images, b.Bytes())
		}
		path := filepath.Join(dir, name+".ico")
		if err := os.WriteFile(path, ico(images, sizes), 0o644); err != nil {
			panic(err)
		}
		fmt.Println("wrote", path)
	}
}
