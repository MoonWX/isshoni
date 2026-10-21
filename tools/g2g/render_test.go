package main

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"math/rand/v2"
	"os"
	"testing"
)

// Test helpers that draw what the clock page draws, so the decoder can be tested at any scale and value without
// a browser. The recorded fixtures (fixtures_test.go) are the real page.

const (
	panelW = stripW + 2*marginPx // the grey panel, as web/clock.html draws it
	panelH = cellPx + 2*marginPx + 44
)

func (f *Frame) set(x, y int, r, g, b byte) {
	i := y*f.Stride + 4*x
	f.Pix[i], f.Pix[i+1], f.Pix[i+2], f.Pix[i+3] = b, g, r, 0xff
}

func (f *Frame) fill(rc image.Rectangle, r, g, b byte) {
	rc = rc.Intersect(f.bounds())
	if rc.Empty() {
		return
	}
	first := f.Pix[rc.Min.Y*f.Stride+4*rc.Min.X : rc.Min.Y*f.Stride+4*rc.Max.X]
	for i := 0; i < len(first); i += 4 {
		first[i], first[i+1], first[i+2], first[i+3] = b, g, r, 0xff
	}
	for y := rc.Min.Y + 1; y < rc.Max.Y; y++ {
		copy(f.Pix[y*f.Stride+4*rc.Min.X:], first)
	}
}

// fillAA fills the rectangle [x0, x1) × [y0, y1) in fractional pixels with a grey level, blending the edge pixels
// by how much of them the rectangle covers (an ideal downscaler's result for a sharp-edged rectangle).
func (f *Frame) fillAA(x0, y0, x1, y1 float64, c byte) {
	px0, px1 := max(int(math.Floor(x0)), 0), min(int(math.Ceil(x1)), f.W)
	py0, py1 := max(int(math.Floor(y0)), 0), min(int(math.Ceil(y1)), f.H)
	for y := py0; y < py1; y++ {
		cy := math.Min(y1, float64(y+1)) - math.Max(y0, float64(y))
		for x := px0; x < px1; x++ {
			a := cy * (math.Min(x1, float64(x+1)) - math.Max(x0, float64(x)))
			i := y*f.Stride + 4*x
			for k := range 3 {
				f.Pix[i+k] = byte(math.Round(a*float64(c) + (1-a)*float64(f.Pix[i+k])))
			}
			f.Pix[i+3] = 0xff
		}
	}
}

// paintPanelScaled draws the clock's panel for v at scale s with its top-left corner at (ox, oy), as fillAA
// rectangles: the same picture as paintPanel then scaleInto, much faster (the measurement tests draw many).
func paintPanelScaled(f *Frame, ox, oy, s float64, v uint16) {
	rect := func(x0, y0, x1, y1 float64, c byte) { f.fillAA(ox+s*x0, oy+s*y0, ox+s*x1, oy+s*y1, c) }
	rect(0, 0, panelW, panelH, 128)
	g := grayCode(v)
	cell := func(x0, w float64, black bool) {
		c := byte(255)
		if black {
			c = 0
		}
		rect(x0, marginPx, x0+w, marginPx+cellPx, c)
	}
	const half = cellPx / 2
	cell(marginPx, half, true)
	cell(marginPx+half, half, false)
	for i := range dataCells {
		cell(float64(marginPx+(i+1)*cellPx), cellPx, g&(1<<(dataCells-1-i)) != 0)
	}
	xr := float64(marginPx + (stripCells-1)*cellPx)
	cell(xr, half, false)
	cell(xr+half, half, true)
}

// paintPanel draws the clock's grey panel with the strip for v at (x, y), at scale 1, with a few black bars where
// the page writes its counter.
func paintPanel(f *Frame, x, y int, v uint16) {
	f.fill(image.Rect(x, y, x+panelW, y+panelH), 128, 128, 128)
	g := grayCode(v)
	sx, sy := x+marginPx, y+marginPx
	cell := func(x0, w int, black bool) {
		c := byte(255)
		if black {
			c = 0
		}
		f.fill(image.Rect(x0, sy, x0+w, sy+cellPx), c, c, c)
	}
	cell(sx, cellPx/2, true)
	cell(sx+cellPx/2, cellPx/2, false)
	for i := range dataCells {
		cell(sx+(i+1)*cellPx, cellPx, g&(1<<(dataCells-1-i)) != 0)
	}
	xr := sx + (stripCells-1)*cellPx
	cell(xr, cellPx/2, false)
	cell(xr+cellPx/2, cellPx/2, true)
	for i := range 20 { // the counter's text: black strokes on the grey
		tx := x + marginPx + i*15
		f.fill(image.Rect(tx, y+cellPx+2*marginPx+10, tx+9, y+cellPx+2*marginPx+32), 0, 0, 0)
	}
}

// fillNoise covers f with coarse saturated colour blocks, like the page's noise image.
func fillNoise(f *Frame, seed uint64, block int) {
	rng := rand.New(rand.NewPCG(seed, 1)) //nolint:gosec // G404: a test picture, not a secret
	for by := 0; by < f.H; by += block {
		for bx := 0; bx < f.W; bx += block {
			r, g, b := hsl(rng.Float64()*360, 0.75+rng.Float64()*0.25, 0.35+rng.Float64()*0.25)
			f.fill(image.Rect(bx, by, bx+block, by+block), r, g, b)
		}
	}
}

// fillText covers f with small black marks on white, like the page's text image.
func fillText(f *Frame, seed uint64) {
	rng := rand.New(rand.NewPCG(seed, 2)) //nolint:gosec // G404: a test picture, not a secret
	f.fill(f.bounds(), 255, 255, 255)
	for y := 2; y+9 < f.H; y += 13 {
		for x := 2; x+6 < f.W; x += 7 {
			if rng.IntN(5) == 0 {
				continue
			}
			for range 3 + rng.IntN(5) {
				px, py := x+rng.IntN(6), y+rng.IntN(9)
				f.set(px, py, 0, 0, 0)
			}
		}
	}
}

func hsl(h, s, l float64) (r, g, b byte) {
	c := (1 - math.Abs(2*l-1)) * s
	hp := h / 60
	x := c * (1 - math.Abs(math.Mod(hp, 2)-1))
	var rf, gf, bf float64
	switch {
	case hp < 1:
		rf, gf = c, x
	case hp < 2:
		rf, gf = x, c
	case hp < 3:
		gf, bf = c, x
	case hp < 4:
		gf, bf = x, c
	case hp < 5:
		rf, bf = x, c
	default:
		rf, bf = c, x
	}
	m := l - c/2
	to := func(v float64) byte { return byte(math.Round(255 * (v + m))) }
	return to(rf), to(gf), to(bf)
}

// scaleInto draws src scaled by s with its top-left corner at (ox, oy) into dst, averaging the area each
// destination pixel covers (what a good downscaler does).
func scaleInto(dst, src *Frame, s, ox, oy float64) {
	x0, y0 := int(math.Floor(ox)), int(math.Floor(oy))
	x1, y1 := int(math.Ceil(ox+float64(src.W)*s)), int(math.Ceil(oy+float64(src.H)*s))
	for dy := max(y0, 0); dy < min(y1, dst.H); dy++ {
		sy0, sy1 := (float64(dy)-oy)/s, (float64(dy+1)-oy)/s
		for dx := max(x0, 0); dx < min(x1, dst.W); dx++ {
			sx0, sx1 := (float64(dx)-ox)/s, (float64(dx+1)-ox)/s
			var acc [3]float64
			var wsum float64
			for sy := int(math.Floor(sy0)); float64(sy) < sy1; sy++ {
				if sy < 0 || sy >= src.H {
					continue
				}
				wy := math.Min(sy1, float64(sy+1)) - math.Max(sy0, float64(sy))
				for sx := int(math.Floor(sx0)); float64(sx) < sx1; sx++ {
					if sx < 0 || sx >= src.W {
						continue
					}
					wx := math.Min(sx1, float64(sx+1)) - math.Max(sx0, float64(sx))
					w := wx * wy
					i := sy*src.Stride + 4*sx
					for c := range 3 {
						acc[c] += w * float64(src.Pix[i+c])
					}
					wsum += w
				}
			}
			if wsum < 0.999 { // a partly covered edge pixel: blend over what is there
				i := dy*dst.Stride + 4*dx
				for c := range 3 {
					acc[c] += (1 - wsum) * float64(dst.Pix[i+c])
				}
				wsum = 1
			}
			i := dy*dst.Stride + 4*dx
			for c := range 3 {
				dst.Pix[i+c] = byte(math.Round(acc[c] / wsum))
			}
			dst.Pix[i+3] = 0xff
		}
	}
}

// blendFrames is a·(1−t) + b·t, pixel by pixel.
func blendFrames(a, b *Frame, t float64) *Frame {
	out := newFrame(a.W, a.H)
	for i := range out.Pix {
		out.Pix[i] = byte(math.Round(float64(a.Pix[i])*(1-t) + float64(b.Pix[i])*t))
	}
	return out
}

// jpegRoundTrip compresses f as a JPEG of quality q and decodes it: block artefacts and chroma subsampling, as a
// video codec leaves them.
func jpegRoundTrip(t testing.TB, f *Frame, q int) *Frame {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, f.image(), &jpeg.Options{Quality: q}); err != nil {
		t.Fatal(err)
	}
	img, err := jpeg.Decode(&buf)
	if err != nil {
		t.Fatal(err)
	}
	return frameFromImage(img)
}

// image converts f to an RGBA image.
func (f *Frame) image() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, f.W, f.H))
	for y := range f.H {
		for x := range f.W {
			i := y*f.Stride + 4*x
			img.SetRGBA(x, y, color.RGBA{R: f.Pix[i+2], G: f.Pix[i+1], B: f.Pix[i], A: 0xff})
		}
	}
	return img
}

func readPNG(t testing.TB, path string) *Frame {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: a test fixture
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return frameFromImage(img)
}

func writePNG(t testing.TB, path string, f *Frame) {
	t.Helper()
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, f.image()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}
