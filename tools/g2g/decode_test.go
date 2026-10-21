package main

import (
	"errors"
	"fmt"
	"image"
	"math"
	"math/bits"
	"math/rand/v2"
	"testing"
)

func TestGrayCodeRoundTrip(t *testing.T) {
	for v := range 1 << 16 {
		g := grayCode(uint16(v))
		if got := fromGray(g); got != uint16(v) {
			t.Fatalf("fromGray(grayCode(%d)) = %d", v, got)
		}
		if v > 0 && bits.OnesCount16(g^grayCode(uint16(v-1))) != 1 {
			t.Fatalf("the codes of %d and %d differ in more than one bit", v-1, v)
		}
	}
}

// TestResolveBound: a screenshot that catches two frames half way sees the bits that changed as ambiguous; the
// reading is then within half a step of [old, new], for every value and frame step of a 60 Hz display (and a
// dropped frame).
func TestResolveBound(t *testing.T) {
	for _, step := range []int{1, 16, 17, 33, 34} {
		worst := 0
		for a := range 1 << 16 {
			b := (a + step) & 0xffff
			ga, gb := grayCode(uint16(a)), grayCode(uint16(b))
			for _, g := range []uint16{ga, gb} { // the half-way cells read as either level
				got := int(resolve(g, ga^gb))
				off := wrapDiff(got - a)
				e := 0
				if off < 0 {
					e = -off
				} else if off > step {
					e = off - step
				}
				worst = max(worst, e)
			}
		}
		if worst > step/2 {
			t.Errorf("step %d ms: a reading is %d ms outside [old, new], more than half a step", step, worst)
		}
	}
	if got := resolve(grayCode(12345), 0); got != 12345 {
		t.Errorf("resolve without ambiguous cells = %d, want 12345", got)
	}
}

// screen draws a test screenshot: a background, the clock's panel at the top-left with value clockV, and the
// screen's copy at scale s with its top-left at (ox, oy) holding copyV, the copy after a JPEG round trip of
// quality q (0: none), as the video codec leaves it. The copy is scaled by area averaging (scaleInto), or with
// bilinear by scaleBilinear; pre and post blur it (box filters of that many taps, 0: none) before and after the
// scaling, for the soft edges of a video at a modest bitrate.
type screen struct {
	w, h          int
	bg            string // "noise", "text" or "dark"
	clockV, copyV uint16
	s, ox, oy     float64
	q             int
	seed          uint64
	bilinear      bool
	pre, post     int
}

func (sc screen) render(t testing.TB) *Frame {
	t.Helper()
	paint := func(v uint16) *Frame {
		f := newFrame(sc.w, sc.h)
		switch sc.bg {
		case "noise":
			fillNoise(f, sc.seed, 32)
		case "text":
			fillText(f, sc.seed)
		default:
			f.fill(f.bounds(), 32, 33, 36)
		}
		paintPanel(f, 0, 0, v)
		return f
	}
	f := paint(sc.clockV)
	if sc.s > 0 {
		inner := paint(sc.copyV) // what the viewer shows: the same screen, earlier
		if sc.q > 0 {
			inner = jpegRoundTrip(t, inner, sc.q)
		}
		if sc.pre > 0 {
			boxBlur(inner, inner.bounds(), sc.pre)
		}
		if sc.bilinear {
			scaleBilinear(f, inner, sc.s, sc.ox, sc.oy)
		} else {
			scaleInto(f, inner, sc.s, sc.ox, sc.oy)
		}
		if sc.post > 0 {
			boxBlur(f, image.Rect(int(sc.ox), int(sc.oy), int(math.Ceil(sc.ox+float64(sc.w)*sc.s)),
				int(math.Ceil(sc.oy+float64(sc.h)*sc.s))), sc.post)
		}
	}
	return f // the screenshot itself is lossless
}

// checkScreen asserts that find sees the clock at its known position with clockV and the copy at scale s with
// copyV, and nothing else.
func checkScreen(t *testing.T, f *Frame, clockV, copyV uint16, s float64) {
	t.Helper()
	all := find(f, f.bounds())
	var clock, copies []found
	for _, c := range all {
		if c.overlaps(clockStrip()) { // by place, not by scale: a copy can be at scale 0.97–1.0 too
			clock = append(clock, c)
		} else {
			copies = append(copies, c)
		}
	}
	if len(clock) != 1 || dist(clock[0].strip, clockStrip()) > 1 || math.Abs(clock[0].scale()-1) > 0.005 ||
		clock[0].Value != clockV {
		t.Fatalf("clock: found %+v, want one strip at (24, 24) reading %d", clock, clockV)
	}
	if s == 0 {
		if len(copies) != 0 {
			t.Fatalf("found copies %+v, want none", copies)
		}
		return
	}
	if len(copies) != 1 {
		t.Fatalf("found copies %+v, want one at scale %.3f reading %d", copies, s, copyV)
	}
	c := copies[0]
	if math.Abs(c.scale()-s) > 0.005 || c.Value != copyV {
		t.Fatalf("copy: found scale %.4f reading %d (ambiguous %d), want scale %.3f reading %d", c.scale(), c.Value,
			c.Ambiguous, s, copyV)
	}
}

// TestFindAndDecodeScales: copies at every scale of the calibration's range (0.25–1.0 in steps of 0.05, and
// scales between the steps), at fractional positions, over both content-cut images and a dark viewer, after JPEG
// compression, decode exactly.
func TestFindAndDecodeScales(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 7)) //nolint:gosec // G404: the same cases in every run, not a secret
	var scales []float64
	for s := 0.25; s < 0.96; s += 0.05 {
		scales = append(scales, s)
	}
	scales = append(scales, 0.27, 0.333, 0.43, 0.5, 0.618, 0.71, 0.875)
	for i, s := range scales {
		for _, bg := range []string{"noise", "text", "dark"} {
			sc := screen{
				w: 1000, h: 300 + int(math.Ceil(panelH*s)), bg: bg,
				clockV: v16(rng.IntN(1 << 16)), copyV: v16(rng.IntN(1 << 16)),
				s: s, ox: 20 + rng.Float64()*20, oy: 170 + rng.Float64()*20, q: 60, seed: uint64(i),
			}
			t.Run(fmt.Sprintf("%.3f/%s", s, bg), func(t *testing.T) {
				checkScreen(t, sc.render(t), sc.clockV, sc.copyV, s)
			})
		}
	}
}

// TestFindSoftEdges: copies whose edges are several pixels wide, as Chrome's video scaling and H.264 at a modest
// bitrate leave them, are found and decode exactly at any scale and sub-pixel position: a box blur of 3 or 5 taps
// after an area-averaging scaler, a bilinear scaler without and with a 3-tap blur, and a 5-tap blur at the screen's
// resolution before either scaler. An edge between black and white then passes through every class (black, other,
// grey, other, white), and which scales show it depends on the sub-pixel phase, so the scales are 0.02 apart over
// the whole range at random positions.
func TestFindSoftEdges(t *testing.T) {
	rng := rand.New(rand.NewPCG(11, 3)) //nolint:gosec // G404: the same cases in every run, not a secret
	bgs := []string{"noise", "text", "dark"}
	for _, v := range []struct {
		name      string
		bilinear  bool
		pre, post int
	}{
		{"area-blur3", false, 0, 3},
		{"area-blur5", false, 0, 5},
		{"bilinear", true, 0, 0},
		{"bilinear-blur3", true, 0, 3},
		{"preblur5-area", false, 5, 0},
		{"preblur5-bilinear", true, 5, 0},
	} {
		for k := range 38 { // 0.25–0.99
			s := 0.25 + 0.02*float64(k)
			bg := bgs[k%len(bgs)]
			sc := screen{
				w: 1000, h: 300 + int(math.Ceil(panelH*s)), bg: bg,
				clockV: v16(rng.IntN(1 << 16)), copyV: v16(rng.IntN(1 << 16)),
				s: s, ox: 20 + rng.Float64()*20, oy: 170 + rng.Float64()*20, q: 60, seed: uint64(k),
				bilinear: v.bilinear, pre: v.pre, post: v.post,
			}
			t.Run(fmt.Sprintf("%s/%.2f/%s", v.name, s, bg), func(t *testing.T) {
				t.Parallel()
				checkScreen(t, sc.render(t), sc.clockV, sc.copyV, s)
			})
		}
	}
}

// TestFindNoFalseStrips: the content-cut images, plain, scaled and blurred, hold no strip.
func TestFindNoFalseStrips(t *testing.T) {
	for _, bg := range []string{"noise", "text"} {
		f := newFrame(1200, 700)
		if bg == "noise" {
			fillNoise(f, 3, 32)
		} else {
			fillText(f, 3)
		}
		small := newFrame(f.W, f.H)
		small.fill(small.bounds(), 128, 128, 128)
		for _, s := range []float64{0.25, 0.37, 0.5, 0.75} {
			scaleInto(small, f, s, 10, 10)
		}
		bilinear := newFrame(f.W, f.H)
		bilinear.fill(bilinear.bounds(), 128, 128, 128)
		for _, s := range []float64{0.29, 0.45, 0.62, 0.9} {
			scaleBilinear(bilinear, f, s, 10.4, 10.7)
		}
		frames := map[string]*Frame{bg: jpegRoundTrip(t, f, 70), bg + "-scaled": jpegRoundTrip(t, small, 70)}
		for _, taps := range []int{3, 5} {
			for name, src := range map[string]*Frame{bg: f, bg + "-scaled": small, bg + "-bilinear": bilinear} {
				b := jpegRoundTrip(t, src, 70)
				boxBlur(b, b.bounds(), taps)
				frames[fmt.Sprintf("%s-blur%d", name, taps)] = b
			}
		}
		for name, fr := range frames {
			if got := find(fr, fr.bounds()); len(got) != 0 {
				t.Errorf("%s: found %+v, want no strip", name, got)
			}
		}
	}
}

func TestDecodeRejects(t *testing.T) {
	f := screen{w: 1000, h: 300, bg: "noise", clockV: 4242, seed: 1}.render(t)
	ok := clockStrip()
	if r, err := decode(f, ok); err != nil || r.Value != 4242 || r.Ambiguous != 0 {
		t.Fatalf("decode(clock) = %+v, %v; want 4242", r, err)
	}
	shift := func(dx, dy float64) strip { return strip{ok.X0 + dx, ok.X1 + dx, ok.Y0 + dy, ok.Y1 + dy} }
	for name, tc := range map[string]struct {
		s    strip
		want error
	}{
		"outside":        {strip{900, 1900, 0, 50}, errOutside},
		"nan":            {strip{math.NaN(), 10, 0, 1}, errOutside},
		"tiny":           {strip{100, 110, 100, 100.5}, errOutside},
		"one cell right": {shift(cellPx, 0), nil},
		"noise":          {strip{100, 964, 200, 248}, nil},
		"half a cell":    {shift(0, cellPx/2), nil},
	} {
		_, err := decode(f, tc.s)
		if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
			t.Errorf("%s: decode error %v, want %v", name, err, tc.want)
		}
	}
	// No contrast: a uniform grey area.
	g := newFrame(1000, 200)
	g.fill(g.bounds(), 128, 128, 128)
	if _, err := decode(g, ok); !errors.Is(err, errContrast) {
		t.Errorf("grey: decode error %v, want %v", err, errContrast)
	}
	// The guards without the grey frame around them.
	h := newFrame(1000, 200)
	h.fill(h.bounds(), 255, 255, 255)
	paintPanel(h, 0, 0, 1)
	h.fill(image.Rect(0, 0, 1000, clockY-2), 255, 0, 0)
	if _, err := decode(h, ok); !errors.Is(err, errFrame) {
		t.Errorf("no frame above: decode error %v, want %v", err, errFrame)
	}
}

// TestDecodeTransition: a strip blended half way between two frames reads within half a frame of them.
func TestDecodeTransition(t *testing.T) {
	for _, a := range []uint16{15, 1023, 32767, 40000, 65530} {
		b := a + 17
		fa := screen{w: 1000, h: 200, bg: "dark", clockV: a}.render(t)
		fb := screen{w: 1000, h: 200, bg: "dark", clockV: b}.render(t)
		r, err := decode(blendFrames(fa, fb, 0.5), clockStrip())
		if err != nil {
			t.Fatalf("%d→%d: %v", a, b, err)
		}
		if off := wrapDiff(int(r.Value) - int(a)); off < -8 || off > 17+8 {
			t.Errorf("%d→%d: read %d, more than half a frame outside", a, b, r.Value)
		}
	}
}

// FuzzFindDecode: no input makes find or decode panic or read outside the frame.
func FuzzFindDecode(f *testing.F) {
	f.Add(uint8(60), uint8(30), []byte{0, 255, 128, 128, 128, 0, 0}, 1.0, 2.0, 30.0, 5.0)
	f.Add(uint8(255), uint8(10), []byte{128, 128, 128, 0, 0, 0, 255, 255, 255}, 0.0, 0.0, 255.0, 9.0)
	f.Fuzz(func(t *testing.T, w, h uint8, pix []byte, x0, y0, x1, y1 float64) {
		if len(pix) == 0 {
			return
		}
		fr := newFrame(int(w)+1, int(h)+1)
		for i := range fr.Pix {
			fr.Pix[i] = pix[i%len(pix)]
		}
		for _, s := range find(fr, fr.bounds()) {
			if _, err := decode(fr, s.strip); err != nil {
				t.Fatalf("find returned %+v, which does not decode: %v", s, err)
			}
		}
		_, _ = decode(fr, strip{x0, x1, y0, y1})
		_ = find(fr, image.Rect(int(x0), int(y0), int(x1), int(y1)))
	})
}
