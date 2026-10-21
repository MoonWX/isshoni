package main

import (
	"errors"
	"image"
	"image/draw"
	"math"
	"math/bits"
	"slices"
	"sort"
)

// The strip's geometry in the clock page's device pixels. web/clock.html draws with the same numbers
// (TestClockPageGeometry compares them).
const (
	cellPx     = 48                  // a cell is 48 × 48 px
	stripCells = 18                  // a guard, the 16 data cells, a guard
	dataCells  = 16                  // the Gray code's bits, the most significant first; black = 1
	marginPx   = 24                  // the mid-grey frame around the strip
	stripW     = stripCells * cellPx // 864
	clockX     = marginPx            // the strip's top-left corner on the page, so on the screen when it is full screen
	clockY     = marginPx
)

// The copies that measure looks for, as scales of the clock's strip (01 §12.2).
const (
	minCopyScale = 0.25
	maxCopyScale = 1.0
)

// find reports strips a little outside the copies' range, so that a viewer that is too small can be named in the
// error, and so that a measured scale of 0.249 still counts as 0.25.
const (
	findMinScale = 0.2
	findMaxScale = 1.05
)

// Frame is one screenshot: W × H pixels of 8-bit blue, green, red and an unused byte (GDI's 32-bit DIB order),
// rows Stride bytes apart, the top row first.
type Frame struct {
	W, H, Stride int
	Pix          []byte
}

// newFrame returns a black frame.
func newFrame(w, h int) *Frame {
	return &Frame{W: w, H: h, Stride: 4 * w, Pix: make([]byte, 4*w*h)}
}

// frameFromImage converts any image into a Frame (the tests' PNG fixtures and JPEG round trips).
func frameFromImage(img image.Image) *Frame {
	b := img.Bounds()
	rgba := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(rgba, rgba.Bounds(), img, b.Min, draw.Src)
	f := newFrame(b.Dx(), b.Dy())
	for i := 0; i < len(f.Pix); i += 4 {
		f.Pix[i], f.Pix[i+1], f.Pix[i+2], f.Pix[i+3] = rgba.Pix[i+2], rgba.Pix[i+1], rgba.Pix[i], 0xff
	}
	return f
}

// bounds is the whole frame.
func (f *Frame) bounds() image.Rectangle { return image.Rect(0, 0, f.W, f.H) }

// luma is a pixel's brightness, 0–255 (BT.601 weights).
func luma(b, g, r byte) int { return (77*int(r) + 150*int(g) + 29*int(b) + 128) >> 8 }

// spread is how far a pixel is from grey: its largest channel minus its smallest.
func spread(b, g, r byte) int {
	return max(int(b), int(g), int(r)) - min(int(b), int(g), int(r))
}

// Pixel classes. The strip has only black, white and its frame's mid-grey, all neutral, so a pixel with colour is
// none of them. The content-cut images never use the mid-grey (the text page is black on white, the noise is
// saturated colour), so a grey run next to a black run is rare outside a strip.
type class uint8

const (
	clsOther class = iota // colour, or a level between black, grey and white (an edge)
	clsBlack
	clsGrey
	clsWhite
)

const (
	neutralMax = 48  // the largest spread of a neutral pixel
	blackMax   = 72  // black: neutral and luma ≤ blackMax
	greyMin    = 88  // grey: neutral and greyMin ≤ luma ≤ greyMax
	greyMax    = 168 //
	whiteMin   = 184 // white: neutral and luma ≥ whiteMin
)

// classify returns the class of a pixel with luma y and spread sp.
func classify(y, sp int) class {
	if sp > neutralMax {
		return clsOther
	}
	switch {
	case y <= blackMax:
		return clsBlack
	case y >= whiteMin:
		return clsWhite
	case y >= greyMin && y <= greyMax:
		return clsGrey
	}
	return clsOther
}

// strip is a copy of the clock's strip in a screenshot, in pixel coordinates: X0 is the left edge of the left
// guard, X1 the right edge of the right guard, Y0 and Y1 the top and bottom edges. Pixel (x, y) covers
// [x, x+1) × [y, y+1).
type strip struct {
	X0, X1, Y0, Y1 float64
}

// scale is the strip's size relative to the clock's own strip.
func (s strip) scale() float64 { return (s.X1 - s.X0) / stripW }

// cell is the width of one cell in pixels.
func (s strip) cell() float64 { return (s.X1 - s.X0) / stripCells }

// valid reports whether the strip has finite coordinates and a size of at least a pixel per cell.
func (s strip) valid() bool {
	for _, v := range []float64{s.X0, s.X1, s.Y0, s.Y1} {
		if math.IsNaN(v) || math.Abs(v) > 1<<20 {
			return false
		}
	}
	return s.cell() >= 1 && s.Y1-s.Y0 >= 1
}

// rect is the strip with its grey frame, rounded outwards.
func (s strip) rect() image.Rectangle {
	m := s.cell() / 2
	return image.Rect(int(math.Floor(s.X0-m)), int(math.Floor(s.Y0-m)), int(math.Ceil(s.X1+m)), int(math.Ceil(s.Y1+m)))
}

// clockStrip is the clock's own strip at its known position: the page draws it at (clockX, clockY), which is on
// the screen there when the page is full screen on the primary monitor.
func clockStrip() strip {
	return strip{X0: clockX, X1: clockX + stripW, Y0: clockY, Y1: clockY + cellPx}
}

// reading is a decoded strip.
type reading struct {
	Value     uint16  // the clock's milliseconds, mod 65536
	Ambiguous int     // cells between black and white (a transition between two frames), resolved by resolve
	Contrast  float64 // the guards' white minus their black, in luma
}

// decode failures. Each says why a place in a screenshot is not a readable strip.
var (
	errOutside   = errors.New("the strip is not inside the screenshot")
	errContrast  = errors.New("the guards have too little contrast")
	errGuards    = errors.New("the guards are not black and white")
	errFrame     = errors.New("the grey frame around the strip is missing")
	errAmbiguous = errors.New("too many cells are between black and white")
)

// Decoding thresholds, as fractions of the guards' contrast unless they say otherwise.
const (
	minContrast   = 64   // luma; black and white are about 255 apart, even after video compression
	guardMismatch = 0.3  // the two guards' blacks (and whites) differ by at most this
	guardNoise    = 0.2  // the standard deviation inside a guard half
	frameOffGrey  = 0.2  // the frame's mean is within this of the middle between black and white
	frameNoise    = 0.15 // the standard deviation inside a frame sample
	ambiguousBand = 0.25 // a cell within this of the middle is ambiguous
	maxAmbiguous  = 6    // more ambiguous cells than this is not a transition (a 50 ms step changes at most 5 bits)
)

// decode reads the strip at s. It checks that the place looks like a strip (black and white guards at both ends,
// the mid-grey frame on all four sides) before it reads the 16 cells, so content that happens to be there is not
// read as a value.
func decode(f *Frame, s strip) (reading, error) {
	c := s.cell()
	if !s.valid() || !s.rect().In(f.bounds()) {
		return reading{}, errOutside
	}
	cy := (s.Y0 + s.Y1) / 2
	ry := (s.Y1 - s.Y0) / 4
	half := func(x float64) boxStats { // the middle quarter of a guard half, centred on x
		return f.box(x-c/8, cy-ry, x+c/8, cy+ry)
	}
	bL, wL := half(s.X0+c/4), half(s.X0+3*c/4)
	wR, bR := half(s.X1-3*c/4), half(s.X1-c/4)
	black, white := (bL.mean+bR.mean)/2, (wL.mean+wR.mean)/2
	contrast := white - black
	if contrast < minContrast {
		return reading{}, errContrast
	}
	if math.Abs(bL.mean-bR.mean) > guardMismatch*contrast || math.Abs(wL.mean-wR.mean) > guardMismatch*contrast {
		return reading{}, errGuards
	}
	for _, g := range []boxStats{bL, wL, wR, bR} {
		if g.std > guardNoise*contrast || g.spread > neutralMax {
			return reading{}, errGuards
		}
	}
	mid := (black + white) / 2
	// The frame is half a cell wide; each sample is the middle half of it, away from both of its edges.
	frame := []boxStats{
		f.box(s.X0-3*c/8, cy-ry, s.X0-c/8, cy+ry),       // left
		f.box(s.X1+c/8, cy-ry, s.X1+3*c/8, cy+ry),       // right
		f.box(s.X0+c, s.Y0-3*c/8, s.X1-c, s.Y0-c/8),     // above the data cells
		f.box(s.X0+c, s.Y1+c/8, s.X1-c, s.Y1+3*c/8),     // below them
		f.box(s.X0-3*c/8, s.Y0-3*c/8, s.X0+c, s.Y0-c/8), // above the left guard and the corner
		f.box(s.X1-c, s.Y1+c/8, s.X1+3*c/8, s.Y1+3*c/8), // below the right guard and the corner
	}
	for _, g := range frame {
		if math.Abs(g.mean-mid) > frameOffGrey*contrast || g.std > frameNoise*contrast || g.spread > neutralMax {
			return reading{}, errFrame
		}
	}
	var gray, amb uint16
	for i := range dataCells {
		x := s.X0 + float64(i+1)*c
		m := f.box(x+c/4, cy-ry, x+3*c/4, cy+ry).mean
		bit := uint16(1) << (dataCells - 1 - i)
		if m < mid {
			gray |= bit
		}
		if math.Abs(m-mid) < ambiguousBand*contrast {
			amb |= bit
		}
	}
	n := bits.OnesCount16(amb)
	if n > maxAmbiguous {
		return reading{}, errAmbiguous
	}
	return reading{Value: resolve(gray, amb), Ambiguous: n, Contrast: contrast}, nil
}

// grayCode is v's binary-reflected Gray code, which the page draws.
func grayCode(v uint16) uint16 { return v ^ v>>1 }

// fromGray inverts grayCode.
func fromGray(g uint16) uint16 {
	v := g
	v ^= v >> 1
	v ^= v >> 2
	v ^= v >> 4
	v ^= v >> 8
	return v
}

// resolve decodes a strip's Gray code g whose cells in amb were between black and white. The page changes the
// strip once per display frame, and the value moves by a frame's milliseconds (16 or 17 at 60 Hz), which changes
// a few bits. A screenshot that catches a blend of two frames sees exactly those cells half way, so the old and the
// new value are two complementary ways of resolving amb. Of all the complementary pairs, the one whose values are
// closest together is taken, and its midpoint is the reading: for a step of d ms it is within d/2 of [old, new]
// (TestResolveBound checks every value), where reading the half-way cells as either level would be off by up to
// 2d. Without ambiguous cells, this is fromGray(g).
func resolve(g, amb uint16) uint16 {
	if amb == 0 {
		return fromGray(g)
	}
	var flips []uint16
	for m := amb; m != 0; m &= m - 1 {
		flips = append(flips, m&-m)
	}
	n := 1 << len(flips)
	vals := make([]int, n)
	for k := range n {
		c := g
		for j, b := range flips {
			if k&(1<<j) != 0 {
				c ^= b
			}
		}
		vals[k] = int(fromGray(c))
	}
	bestLow, bestDiff := 0, math.MaxInt
	for k := range n / 2 { // each pair once: k and its complement
		x, y := vals[k], vals[(n-1)^k]
		d := wrapDiff(y - x)
		low := x
		if d < 0 {
			low, d = y, -d
		}
		if d < bestDiff {
			bestLow, bestDiff = low, d
		}
	}
	return uint16((bestLow + bestDiff/2) & 0xffff)
}

// wrapDiff maps a difference of two 16-bit clock values into [-32768, 32767].
func wrapDiff(d int) int {
	d &= 0xffff
	if d >= 1<<15 {
		d -= 1 << 16
	}
	return d
}

// boxStats are the statistics of a box of pixels.
type boxStats struct {
	mean, std float64 // luma
	spread    float64 // the mean spread (colour)
}

// box returns the statistics of the pixels whose centres lie in [x0, x1) × [y0, y1); a box narrower than a pixel
// takes the pixel at its centre. The caller has checked that the box is inside the frame (strip.rect).
func (f *Frame) box(x0, y0, x1, y1 float64) boxStats {
	px0, px1 := span(x0, x1, f.W)
	py0, py1 := span(y0, y1, f.H)
	var sum, sq, sp float64
	n := 0
	for y := py0; y < py1; y++ {
		row := f.Pix[y*f.Stride:]
		for x := px0; x < px1; x++ {
			b, g, r := row[4*x], row[4*x+1], row[4*x+2]
			l := float64(luma(b, g, r))
			sum += l
			sq += l * l
			sp += float64(spread(b, g, r))
			n++
		}
	}
	if n == 0 {
		return boxStats{}
	}
	mean := sum / float64(n)
	return boxStats{mean: mean, std: math.Sqrt(max(0, sq/float64(n)-mean*mean)), spread: sp / float64(n)}
}

// span is the pixels whose centres lie in [a, b), at least the one at the middle, clipped to [0, size).
func span(a, b float64, size int) (int, int) {
	lo, hi := int(math.Ceil(a-0.5)), int(math.Ceil(b-0.5))
	if hi <= lo {
		lo = int(math.Floor((a + b) / 2))
		hi = lo + 1
	}
	return max(lo, 0), min(hi, size)
}

// found is a strip that find located and could read.
type found struct {
	strip
	reading
}

// find returns every readable strip inside r at scales findMinScale–findMaxScale, the largest first.
//
// It scans each row of r for the strip's two guards. A left guard is a grey run, a black run and a white run; a
// right guard is a white run, a black run and a grey run; in both the black run is half a cell wide, so it gives
// the strip's scale before anything is matched. A left and a right guard 36 half cells apart on one row are a hit;
// hits on adjacent rows with the same ends make a strip about a cell high. The ends are interpolated to a fraction
// of a pixel, so the cells' positions are exact at any scale, and each strip must then pass decode.
func find(f *Frame, r image.Rectangle) []found {
	r = r.Intersect(f.bounds())
	if r.Empty() {
		return nil
	}
	var (
		open, done []*cluster
		runs       []seg
		lefts      []guard
		rights     []guard
	)
	for y := r.Min.Y; y < r.Max.Y; y++ {
		runs = f.rowRuns(y, r.Min.X, r.Max.X, runs[:0])
		lefts, rights = f.guards(y, runs, lefts[:0], rights[:0])
		for _, h := range pairGuards(y, lefts, rights) {
			joined := false
			for _, c := range open {
				if c.accepts(h) {
					c.add(h)
					joined = true
					break
				}
			}
			if !joined {
				open = append(open, &cluster{hits: []hit{h}, last: y})
			}
		}
		kept := open[:0]
		for _, c := range open {
			if y-c.last > c.maxGap() {
				done = append(done, c)
			} else {
				kept = append(kept, c)
			}
		}
		open = kept
	}
	done = append(done, open...)
	var out []found
	for _, c := range done {
		s, ok := c.strip(f)
		if !ok {
			continue
		}
		rd, err := decode(f, s)
		if err != nil {
			continue
		}
		dup := false
		for i, o := range out {
			if o.overlaps(s) {
				dup = true
				if rd.Ambiguous < o.Ambiguous {
					out[i] = found{s, rd}
				}
				break
			}
		}
		if !dup {
			out = append(out, found{s, rd})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].scale() > out[j].scale() })
	return out
}

// overlaps reports whether two strips are the same copy found twice.
func (s strip) overlaps(o strip) bool {
	c := s.cell()
	return math.Abs(s.X0-o.X0) < c && math.Abs(s.Y0-o.Y0) < c && math.Abs(s.scale()/o.scale()-1) < 0.1
}

// seg is a run of pixels of one class on a row: [x0, x1), with its luma sum.
type seg struct {
	c      class
	x0, x1 int
	sum    int
}

func (r seg) len() int      { return r.x1 - r.x0 }
func (r seg) mean() float64 { return float64(r.sum) / float64(r.len()) }
func (f *Frame) lumaAt(x, y int) float64 {
	i := y*f.Stride + 4*x
	return float64(luma(f.Pix[i], f.Pix[i+1], f.Pix[i+2]))
}

// rowRuns splits row y between x0 and x1 into runs of one class.
func (f *Frame) rowRuns(y, x0, x1 int, runs []seg) []seg {
	row := f.Pix[y*f.Stride:]
	for x := x0; x < x1; x++ {
		b, g, r := row[4*x], row[4*x+1], row[4*x+2]
		l := luma(b, g, r)
		c := classify(l, spread(b, g, r))
		if n := len(runs); n > 0 && runs[n-1].c == c {
			runs[n-1].x1 = x + 1
			runs[n-1].sum += l
			continue
		}
		runs = append(runs, seg{c: c, x0: x, x1: x + 1, sum: l})
	}
	return runs
}

// guard is one end of a strip on one row: x is its outer edge (where the grey frame meets the black half), half
// the width of its black half.
type guard struct {
	x, half float64
}

// Run lengths of the guard patterns, in pixels. At the smallest scale find reports, a half cell is 4.8 px and the
// frame is as wide; video compression and scaling blur each edge by a pixel or two, and a soft edge (a blurred
// video, scaled again) by a few.
const (
	minRun = 3 // the black and the grey run
	maxGap = 4 // pixels of transition runs allowed between two runs (an edge)
)

// step is the index of the run next to runs[i] in direction dir (±1), passing over the transition runs between
// them, at most maxGap pixels in all: clsOther runs, and with greyEdge also grey runs. An edge between black and
// white passes through every level, so a soft one is several short runs (other, grey, other); an edge between black
// and the grey frame is at most one clsOther run. -1 when there is none.
func step(runs []seg, i, dir int, greyEdge bool) int {
	j, gap := i+dir, 0
	for j >= 0 && j < len(runs) && (runs[j].c == clsOther || greyEdge && runs[j].c == clsGrey) &&
		gap+runs[j].len() <= maxGap {
		gap += runs[j].len()
		j += dir
	}
	if j < 0 || j >= len(runs) {
		return -1
	}
	return j
}

// guards finds the left guards (grey, black, white) and right guards (white, black, grey) on row y.
func (f *Frame) guards(y int, runs []seg, lefts, rights []guard) ([]guard, []guard) {
	for i, k := range runs {
		if k.c != clsBlack || k.len() < minRun {
			continue
		}
		frameOK := func(j int) bool { // the grey frame: at least minRun and half the black half
			return j >= 0 && runs[j].c == clsGrey && runs[j].len() >= minRun && 2*runs[j].len() >= k.len()
		}
		whiteOK := func(j int) bool { return j >= 0 && runs[j].c == clsWhite }
		if p, q := step(runs, i, -1, false), step(runs, i, 1, true); frameOK(p) && whiteOK(q) {
			g, w := runs[p], runs[q]
			outer := f.crossing(y, g.x1-1, k.x0, (g.mean()+k.mean())/2)
			inner := f.crossing(y, k.x1-1, w.x0, (k.mean()+w.mean())/2)
			if inner > outer {
				lefts = append(lefts, guard{x: outer, half: inner - outer})
			}
		}
		if p, q := step(runs, i, -1, true), step(runs, i, 1, false); whiteOK(p) && frameOK(q) {
			w, g := runs[p], runs[q]
			inner := f.crossing(y, w.x1-1, k.x0, (w.mean()+k.mean())/2)
			outer := f.crossing(y, k.x1-1, g.x0, (k.mean()+g.mean())/2)
			if outer > inner {
				rights = append(rights, guard{x: outer, half: outer - inner})
			}
		}
	}
	return lefts, rights
}

// crossing is where the luma along row y crosses level between pixel a and pixel b (a < b), interpolated between
// pixel centres.
func (f *Frame) crossing(y, a, b int, level float64) float64 {
	for p := a; p < b; p++ {
		ya, yb := f.lumaAt(p, y), f.lumaAt(p+1, y)
		if (ya-level)*(yb-level) <= 0 && ya != yb {
			return float64(p) + 0.5 + (ya-level)/(ya-yb)
		}
	}
	return float64(a+b+1) / 2
}

// hit is a row on which a left and a right guard pair up.
type hit struct {
	y      int
	x0, x1 float64
}

// pairGuards pairs each left guard with the right guard 36 half cells away, as its black half predicts.
func pairGuards(y int, lefts, rights []guard) []hit {
	var hits []hit
	for _, l := range lefts {
		best, bestErr := -1, math.Inf(1)
		for j, r := range rights {
			w := r.x - l.x
			if w <= 0 || l.half/r.half < 0.6 || l.half/r.half > 1/0.6 {
				continue
			}
			ratio := w / (2 * stripCells * (l.half + r.half) / 2)
			scale := w / stripW
			if ratio < 0.75 || ratio > 1.33 || scale < findMinScale || scale > findMaxScale {
				continue
			}
			if e := math.Abs(ratio - 1); e < bestErr {
				best, bestErr = j, e
			}
		}
		if best >= 0 {
			hits = append(hits, hit{y: y, x0: l.x, x1: rights[best].x})
		}
	}
	return hits
}

// cluster is the hits of one strip on adjacent rows.
type cluster struct {
	hits []hit
	last int
}

// maxGap is how many rows a strip's hits may skip (a row spoilt by a compression artefact, or a transition).
func (c *cluster) maxGap() int {
	l := c.hits[len(c.hits)-1]
	return max(2, int(0.25*(l.x1-l.x0)/stripCells))
}

func (c *cluster) accepts(h hit) bool {
	l := c.hits[len(c.hits)-1]
	tol := max(2, 0.015*(l.x1-l.x0))
	return h.y-c.last <= c.maxGap() && h.y != l.y && math.Abs(h.x0-l.x0) <= tol && math.Abs(h.x1-l.x1) <= tol
}

func (c *cluster) add(h hit) {
	c.hits = append(c.hits, h)
	c.last = h.y
}

// strip turns the cluster into a strip when it is about a cell high: the median ends, and the top and bottom edges
// of the guards' black halves (or, when those can't be found, a cell's height centred on the rows that matched).
func (c *cluster) strip(f *Frame) (strip, bool) {
	x0s := make([]float64, len(c.hits))
	x1s := make([]float64, len(c.hits))
	for i, h := range c.hits {
		x0s[i], x1s[i] = h.x0, h.x1
	}
	slices.Sort(x0s)
	slices.Sort(x1s)
	x0, x1 := x0s[len(x0s)/2], x1s[len(x1s)/2]
	cell := (x1 - x0) / stripCells
	first, last := c.hits[0].y, c.hits[len(c.hits)-1].y
	rows := float64(last - first + 1)
	if float64(len(c.hits)) < max(2, 0.4*cell) || rows > 1.5*cell+2 {
		return strip{}, false
	}
	cy := float64(first+last+1) / 2
	s := strip{X0: x0, X1: x1, Y0: cy - cell/2, Y1: cy + cell/2}
	if y0, y1, ok := f.edgesY(s, (first+last)/2); ok {
		s.Y0, s.Y1 = y0, y1
	}
	return s, true
}

// edgesY finds the strip's top and bottom edges: where the luma down the middle of each guard's black half, from
// row cy, crosses half way to the grey frame. ok is false unless both guards give a height of about a cell.
func (f *Frame) edgesY(s strip, cy int) (y0, y1 float64, ok bool) {
	c := s.cell()
	lim := int(c) + 2
	n := 0
	for _, xf := range []float64{s.X0 + c/4, s.X1 - c/4} {
		x := int(xf)
		if x < 0 || x >= f.W || cy < 1 || cy >= f.H-1 {
			return 0, 0, false
		}
		level := (f.lumaAt(x, cy) + 128) / 2
		top, bottom := math.NaN(), math.NaN()
		for y := cy - 1; y >= 0 && cy-y <= lim; y-- {
			if ya, yb := f.lumaAt(x, y), f.lumaAt(x, y+1); ya >= level && yb < level {
				top = float64(y) + 0.5 + (ya-level)/(ya-yb)
				break
			}
		}
		for y := cy + 1; y < f.H && y-cy <= lim; y++ {
			if ya, yb := f.lumaAt(x, y-1), f.lumaAt(x, y); yb >= level && ya < level {
				bottom = float64(y) - 0.5 + (level-ya)/(yb-ya)
				break
			}
		}
		if math.IsNaN(top) || math.IsNaN(bottom) || math.Abs((bottom-top)/c-1) > 0.25 {
			return 0, 0, false
		}
		y0, y1, n = y0+top, y1+bottom, n+1
	}
	return y0 / float64(n), y1 / float64(n), true
}
