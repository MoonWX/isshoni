package fake

import (
	"bytes"
	"fmt"
	"math"
	"testing"
	"testing/synctest"
	"time"

	"github.com/pion/rtp/codecs"
)

// decodableCheck checks one layer's decodable access units, in order, as a decoder and as the SFU see them.
type decodableCheck struct {
	t     *testing.T
	l     VideoLayer
	dec   *testDecoder
	pay   codecs.H264Payloader
	sps   []byte // the layer's SPS NAL unit, from its first keyframe
	next  uint32 // the next frame index
	idrs  int
	pcmP  int // I_PCM macroblocks in P frames
	delta int // P frames
	// semantic is false for pictures too small for the scene's parts to stand apart.
	semantic bool
}

func newDecodableCheck(t *testing.T, l VideoLayer) *decodableCheck {
	return &decodableCheck{t: t, l: l, dec: newTestDecoder(), semantic: l.Width >= 208 && l.Height >= 96}
}

// check checks one access unit of the layer.
func (c *decodableCheck) check(p Packet) {
	t := c.t
	t.Helper()
	m := Marker{RID: c.l.RID, Keyframe: p.Keyframe, Flash: p.Flash, Frame: c.next, CaptureNS: p.CaptureNS}
	c.next++
	nals := splitAnnexB(t, p.Data)
	if p.Keyframe {
		if len(nals) != 3 || nals[0][0] != nalSPS || nals[1][0] != nalPPS || nals[2][0] != nalIDR {
			t.Fatalf("%s/%d: keyframe NAL units %v", c.l.RID, m.Frame, nalTypes(nals))
		}
		c.checkSPS(nals[0])
		c.sps = nals[0]
	} else if len(nals) != 1 || nals[0][0] != nalNonIDR {
		t.Fatalf("%s/%d: delta frame NAL units %v", c.l.RID, m.Frame, nalTypes(nals))
	}

	sl, pic, err := c.dec.decode(p.Data)
	if err != nil {
		t.Fatalf("%s/%d (keyframe %v): %v", c.l.RID, m.Frame, p.Keyframe, err)
	}
	total := pic.sps.wMBs * pic.sps.hMBs
	switch {
	case sl.idr != p.Keyframe:
		t.Fatalf("%s/%d: IDR %v, keyframe %v", c.l.RID, m.Frame, sl.idr, p.Keyframe)
	case sl.idr && (sl.pcm != total || sl.skip != 0):
		t.Fatalf("%s/%d: IDR with %d I_PCM and %d skipped of %d macroblocks", c.l.RID, m.Frame, sl.pcm, sl.skip, total)
	case !sl.idr && (sl.pcm+sl.skip != total || sl.pcm < 1 || sl.pcmAddrs[0] != 0):
		t.Fatalf("%s/%d: P frame with %d I_PCM and %d skipped of %d macroblocks, the first I_PCM at %v", c.l.RID,
			m.Frame, sl.pcm, sl.skip, total, sl.pcmAddrs)
	}
	if sl.idr {
		c.idrs++
	} else {
		c.pcmP += sl.pcm
		c.delta++
	}
	if w, h := pic.sps.size(); w != c.l.Width || h != c.l.Height {
		t.Fatalf("%s: decoded size %dx%d", c.l.RID, w, h)
	}

	// The decoded picture is the scene, sample for sample.
	sc := sceneOf(c.l, m)
	var want [pcmBytes]byte
	for addr := range total {
		mbx, mby := addr%pic.sps.wMBs, addr/pic.sps.wMBs
		sc.render(mbx, mby, &want)
		var got [pcmBytes]byte
		for i := range mbLuma {
			got[i] = pic.lumaAt(mbx*16+i%16, mby*16+i/16)
		}
		for i := range mbChroma {
			got[mbLuma+i], got[mbLuma+mbChroma+i] = pic.chromaAt(mbx*16+i%8*2, mby*16+i/8*2)
		}
		if got != want {
			t.Fatalf("%s/%d: macroblock (%d, %d) decodes differently from the scene", c.l.RID, m.Frame, mbx, mby)
		}
	}
	c.checkPicture(pic, m, sc)
	c.checkRTP(p, m)
}

// checkSPS parses a keyframe's SPS with the SFU's parser and with the test's reader. Its level is the lowest that
// fits the layer, but at least decodableMinLevel.
func (c *decodableCheck) checkSPS(nal []byte) {
	t := c.t
	t.Helper()
	info, err := parseSPS(nal)
	if err != nil {
		t.Fatalf("%s: the SFU's parseSPS: %v", c.l.RID, err)
	}
	level, _ := levelFor((c.l.Width+15)/16, (c.l.Height+15)/16, c.l.FPS, 0)
	level = max(level, decodableMinLevel)
	if want := fmt.Sprintf("%s%02x", DecodableProfile, level); info.profileLevelID() != want ||
		info.width != c.l.Width || info.height != c.l.Height {
		t.Fatalf("%s: the SFU reads %s %dx%d, want %s %dx%d", c.l.RID, info.profileLevelID(), info.width,
			info.height, want, c.l.Width, c.l.Height)
	}
	if s := readSPS(t, nal); s.profileKey() != DecodableProfile || s.width != c.l.Width || s.height != c.l.Height {
		t.Fatalf("%s: SPS %+v", c.l.RID, s)
	}
	if c.sps != nil && !bytes.Equal(c.sps, nal) {
		t.Fatalf("%s: the SPS changed", c.l.RID)
	}
}

// checkPicture reads the scene's parts from the decoded samples: the Marker in macroblock 0, the frame counter,
// the flash square and the box.
func (c *decodableCheck) checkPicture(pic *decPicture, m Marker, sc scene) {
	t := c.t
	t.Helper()
	var raw [MarkerSize]byte
	for i := range 2 * MarkerSize {
		s := pic.lumaAt(i%16, i/16)
		if s&0xf0 != pcmMarkerTag {
			t.Fatalf("%s/%d: marker sample %d is %#x", c.l.RID, m.Frame, i, s)
		}
		raw[i/2] = raw[i/2]<<4 | s&0x0f
	}
	if got, ok := decodeMarker(raw[:]); !ok || got != m {
		t.Fatalf("%s/%d: the picture's marker %+v %v, want %+v", c.l.RID, m.Frame, got, ok, m)
	}
	if !c.semantic {
		return
	}
	for bit := range counterBits {
		x := counterX + (counterBits-1-bit)*counterBitW + counterBitW/2
		want := byte(16)
		if m.Frame>>bit&1 == 1 {
			want = 235
		}
		if got := pic.lumaAt(x, counterH/2); got != want {
			t.Fatalf("%s/%d: counter bit %d is %d, want %d", c.l.RID, m.Frame, bit, got, want)
		}
	}
	y := pic.lumaAt(c.l.Width-flashSize/2, flashSize/2)
	if (y == 235) != m.Flash {
		t.Fatalf("%s/%d: flash square luma %d, flash %v", c.l.RID, m.Frame, y, m.Flash)
	}
	bx, by := boxAt(c.l.Width, c.l.Height, m.CaptureNS)
	if bx != sc.boxX || by != sc.boxY || bx < 0 || by < flashSize || bx+boxSize > c.l.Width || by+boxSize > c.l.Height {
		t.Fatalf("%s/%d: box at (%d, %d) in %dx%d", c.l.RID, m.Frame, bx, by, c.l.Width, c.l.Height)
	}
	cb, cr := pic.chromaAt(bx+boxSize/2, by+boxSize/2)
	if got := (yuv{pic.lumaAt(bx+boxSize/2, by+boxSize/2), cb, cr}); got != colourBox {
		t.Fatalf("%s/%d: box centre %v, want %v", c.l.RID, m.Frame, got, colourBox)
	}
}

// checkRTP packetizes the access unit like publish.Publisher (pion's H264Payloader, MTU 1200) and reads it like
// the SFU (keyframe start) and like sfutest's Recorder (the Marker in the slice's first packet).
func (c *decodableCheck) checkRTP(p Packet, m Marker) {
	t := c.t
	t.Helper()
	payloads := c.pay.Payload(1200, p.Data)
	if len(payloads) == 0 {
		t.Fatalf("%s/%d: no RTP payloads", c.l.RID, m.Frame)
	}
	marks := 0
	for i, pl := range payloads {
		if key := isKeyframeStart(pl); key != (i == 0 && p.Keyframe) {
			t.Fatalf("%s/%d: packet %d: the SFU's isKeyframeStart = %v", c.l.RID, m.Frame, i, key)
		}
		var body []byte
		switch pl[0] & 0x1f {
		case 1, 5:
			body = pl[1:]
		case 28:
			if pl[1]&0x80 != 0 {
				body = pl[2:]
			}
		}
		if body == nil {
			continue
		}
		got, ok := ParseVideoMarker(body)
		if !ok || got != m {
			t.Fatalf("%s/%d: packet %d: marker %+v %v, want %+v", c.l.RID, m.Frame, i, got, ok, m)
		}
		marks++
	}
	if marks != 1 {
		t.Fatalf("%s/%d: %d packets with a marker", c.l.RID, m.Frame, marks)
	}
}

// sceneOf returns the scene a layer shows for a frame.
func sceneOf(l VideoLayer, m Marker) scene {
	sc := scene{w: l.Width, h: l.Height, cw: (l.Width + 15) / 16 * 16, ch: (l.Height + 15) / 16 * 16, frame: m.Frame,
		flash: m.Flash}
	sc.boxX, sc.boxY = boxAt(l.Width, l.Height, m.CaptureNS)
	appendMarker(sc.marker[:0], m)
	return sc
}

// TestDecodableStream decodes every frame of several layers over 3.5 s, with 1 s GOPs, a keyframe request and
// flashes: SPS/PPS and slice headers parse with the SFU's parser and the test decoder, every macroblock is I_PCM or
// P_Skip with a zero motion vector, the I_PCM samples are byte-aligned, the macroblock count is the picture's, and
// the decoded picture is the scene, whose marker, counter, flash square and box read back from the samples.
func TestDecodableStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		layers := []VideoLayer{
			{RID: "f", Width: 640, Height: 360, FPS: 30},
			{RID: "q", Width: 320, Height: 180, FPS: 15},
			{RID: "h", Width: 208 + 2, Height: 96 + 6, FPS: 10}, // cropped on the right and at the bottom
			{RID: "t", Width: 48, Height: 34, FPS: 5},           // smaller than the scene's parts
		}
		s, err := New(Config{Mode: Decodable, Layers: layers, GOP: time.Second, Audio: true, Seed: 3})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		checks := map[string]*decodableCheck{}
		for _, l := range layers {
			checks[l.RID] = newDecodableCheck(t, l)
		}
		requested, forced := false, uint32(0)
		flashes := map[string]int{}
		for {
			p, err := s.Next(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if p.CaptureNS >= int64(3500*time.Millisecond) {
				break
			}
			if p.Kind == Audio {
				continue
			}
			c := checks[p.Layer]
			if requested && p.Layer == "f" && forced == 0 {
				forced = c.next
				if !p.Keyframe {
					t.Fatalf("f/%d after RequestKeyframe is not a keyframe", c.next)
				}
			}
			c.check(p)
			if p.Flash {
				flashes[p.Layer]++
			}
			if !requested && p.Layer == "f" && p.CaptureNS >= int64(1500*time.Millisecond) {
				s.RequestKeyframe("f")
				requested = true
			}
		}
		if forced == 0 {
			t.Fatal("no keyframe request")
		}
		// Keyframes at 0, 1, 2 and 3 s; f's forced one restarts its GOP, so f has them at 0, 1, 1.53 and 2.53 s.
		for _, l := range layers {
			c := checks[l.RID]
			frames := (7*l.FPS + 1) / 2 // capture times below 3.5 s
			t.Logf("%s %dx%d@%d: %d frames, %d IDR, %.1f I_PCM macroblocks per P frame", l.RID, l.Width, l.Height,
				l.FPS, c.next, c.idrs, float64(c.pcmP)/float64(c.delta))
			if int(c.next) != frames || c.idrs != 4 || flashes[l.RID] != 4 {
				t.Errorf("%s: %d frames, %d IDR, %d flashes; want %d, 4, 4", l.RID, c.next, c.idrs, flashes[l.RID],
					frames)
			}
		}
	})
}

// TestDecodableLarge decodes a 1280×720 layer: 3600 macroblocks per IDR frame.
func TestDecodableLarge(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := VideoLayer{RID: "f", Width: 1280, Height: 720, FPS: 30}
		s, err := New(Config{Mode: Decodable, Layers: []VideoLayer{l}})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		c := newDecodableCheck(t, l)
		for _, p := range collect(t, s, 300*time.Millisecond) {
			c.check(p)
		}
		if c.idrs != 1 || c.next != 9 {
			t.Errorf("%d IDR, %d frames", c.idrs, c.next)
		}
	})
}

// TestDecodableFrameNumWrap: frame_num wraps modulo 256 within a long GOP, and idr_pic_id differs between
// back-to-back IDR frames.
func TestDecodableFrameNumWrap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := VideoLayer{RID: "q", Width: 32, Height: 32, FPS: 100}
		s, err := New(Config{Mode: Decodable, Layers: []VideoLayer{l}, GOP: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		c := newDecodableCheck(t, l)
		for i := range 300 {
			if i == 280 || i == 281 {
				s.RequestKeyframe("q")
			}
			p, err := s.Next(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			c.check(p) // the test decoder checks frame_num and idr_pic_id
		}
		if c.idrs != 3 {
			t.Errorf("%d IDR frames, want 3", c.idrs)
		}
	})
}

// TestDecodableRate measures the default layers over three 3 s GOPs against DefaultDecodableLayers' Bitrate.
func TestDecodableRate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, err := New(Config{Mode: Decodable})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		bytes := map[string]int{}
		frames := map[string]int{}
		for _, p := range collect(t, s, 9*time.Second) {
			bytes[p.Layer] += len(p.Data)
			frames[p.Layer]++
		}
		for _, l := range DefaultDecodableLayers() {
			bps := float64(bytes[l.RID]) * 8 / 9
			t.Logf("%s %dx%d@%d: %.0f bps, %d frames (Bitrate %d)", l.RID, l.Width, l.Height, l.FPS, bps,
				frames[l.RID], l.Bitrate)
			if math.Abs(bps/float64(l.Bitrate)-1) > 0.1 || frames[l.RID] != 9*l.FPS {
				t.Errorf("%s: %.0f bps, want %d ± 10%%", l.RID, bps, l.Bitrate)
			}
		}
	})
}

// TestDecodableDeterministic: the decodable video depends on nothing but the config (not the seed).
func TestDecodableDeterministic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := func(seed uint64) []Packet {
			s, err := New(Config{Mode: Decodable, Seed: seed, GOP: 500 * time.Millisecond, Audio: true})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			return collect(t, s, time.Second)
		}
		a, b := run(1), run(2)
		if len(a) != len(b) {
			t.Fatalf("%d vs %d packets", len(a), len(b))
		}
		for i := range a {
			if !bytes.Equal(a[i].Data, b[i].Data) {
				t.Fatalf("packet %d differs", i)
			}
		}
	})
}

// TestDecodableMarkerParse: ParseVideoMarker reads a decodable slice's Marker from its first bytes (as in the first
// FU-A packet) and rejects damaged slices.
func TestDecodableMarkerParse(t *testing.T) {
	d := newDecodable(VideoLayer{RID: "f", Width: 64, Height: 64, FPS: 30})
	for i, key := range []bool{true, false, false, true} {
		m := Marker{RID: "f", Keyframe: key, Flash: i == 2, Frame: uint32(0x01000000 + i), CaptureNS: int64(i) << 33}
		au := d.accessUnit(key, m, m.CaptureNS, nil, nil)
		body := au[bytes.LastIndex(au, startCode[:])+len(startCode)+1:]
		for _, n := range []int{len(body), markerPrefix, 50} {
			if got, ok := ParseVideoMarker(body[:n]); !ok || got != m {
				t.Fatalf("frame %d, %d bytes: %+v %v, want %+v", i, n, got, ok, m)
			}
		}
		if _, ok := ParseVideoMarker(body[:20]); ok {
			t.Errorf("frame %d: a marker from 20 bytes", i)
		}
		for _, at := range []int{0, 12, 30, 39} { // the slice type, marker samples
			bad := bytes.Clone(body[:markerPrefix])
			bad[at] ^= 0x10
			if got, ok := ParseVideoMarker(bad); ok && got == m {
				t.Errorf("frame %d: byte %d flipped, still parsed", i, at)
			}
		}
	}
}
