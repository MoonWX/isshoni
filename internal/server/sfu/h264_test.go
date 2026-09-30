package sfu

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"
)

func mustHex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Real SPS NAL units (with their NAL header byte).
//   - S2-VT: the S2 spike's own VideoToolbox output on macOS (spikes/s2-mac-vt), 1920x1080 High.
//   - Chrome 154 (headless, macOS arm64), WebCodecs VideoEncoder with avc.format "annexb": "prefer-software" is
//     OpenH264 (the encoder Chrome's WebRTC uses without hardware), "prefer-hardware" is VideoToolbox.
var realSPS = []struct {
	name      string
	hex       string
	plid      string
	w, h      int
	nalHeader byte
}{
	{"S2-VT ave 1080p High", "2764002aac5680780227e59a80808081", "64002a", 1920, 1080, 0x27},
	{"S2-VT rtvc 1080p High", "2764002aac1314501e0089f966a0202020f0804258", "64002a", 1920, 1080, 0x27},
	{"Chrome OpenH264 720p CB", "6742c01f8c6805005ba6a021a020f08846a0", "42c01f", 1280, 720, 0x67},
	{"Chrome OpenH264 1080p CB", "6742c0288c680780227e59a80868083c2211a8", "42c028", 1920, 1080, 0x67},
	{"Chrome OpenH264 360p CB", "6742c01e8c680a02ff966a021a020f08846a", "42c01e", 640, 360, 0x67},
	{"Chrome OpenH264 1080p Main", "674d4c288c680780227e59a80868083c2211a8", "4d4c28", 1920, 1080, 0x67},
	{"Chrome OpenH264 1080p CHigh", "67640c28ac18d00f0044fcb35010d01078442350", "640c28", 1920, 1080, 0x67},
	{"Chrome VideoToolbox 720p Baseline", "2742001f898a300a00b74d40434041e10084d0", "42001f", 1280, 720, 0x27},
	{"Chrome VideoToolbox 1080p Baseline", "27420028898a280f0044fcb35010d0107840212c", "420028", 1920, 1080, 0x27},
	{"Chrome VideoToolbox 360p Baseline", "2742001e898a1205017fcb35010d010784021130", "42001e", 640, 360, 0x27},
	{"Chrome VideoToolbox 1080p Main", "274d0028898a280f0044fcb35010d0107840212c", "4d0028", 1920, 1080, 0x27},
	{"Chrome VideoToolbox 1080p High", "27640028ac1314501e0089f966a021a020f0804258", "640028", 1920, 1080, 0x27},
}

const ppsHex = "68ce3c80"

// stapA builds a STAP-A payload from NAL units.
func stapA(nals ...[]byte) []byte {
	out := []byte{0x78} // F=0, NRI=3, type 24
	for _, n := range nals {
		out = append(out, byte(len(n)>>8), byte(len(n))) //nolint:gosec // test NAL units are shorter than 64 KiB
		out = append(out, n...)
	}
	return out
}

func TestKeyframeStart(t *testing.T) {
	sps, pps := mustHex(t, realSPS[0].hex), mustHex(t, ppsHex)
	stap := stapA(sps, pps)
	cases := []struct {
		name    string
		payload []byte
		want    []byte // the SPS spsNAL must return; nil = not a keyframe start
	}{
		// The S4 spike's cases.
		{"sps", []byte{0x67, 0x42}, []byte{0x67, 0x42}},
		{"idr alone", []byte{0x65, 0x88}, nil},
		{"non-idr", []byte{0x41, 0x9a}, nil},
		{"stap-a sps+pps", []byte{0x78, 0x00, 0x02, 0x67, 0x42, 0x00, 0x02, 0x68, 0xce}, []byte{0x67, 0x42}},
		{"stap-a without sps", []byte{0x78, 0x00, 0x02, 0x06, 0x05}, nil},
		{"stap-a truncated", []byte{0x78, 0x00, 0x09, 0x67}, nil},
		{"fu-a idr start", []byte{0x7c, 0x85}, nil},
		{"empty", nil, nil},
		// Real units and malformed STAP-As.
		{"real sps", sps, sps},
		{"real sps, nal_ref_idc 0", append([]byte{0x07}, sps[1:]...), append([]byte{0x07}, sps[1:]...)},
		{"stap-a real sps+pps", stap, sps},
		{"stap-a pps then sps", stapA(pps, sps), sps},
		{"stap-a sei, pps, sps", stapA([]byte{0x06, 0x05, 0x01}, pps, sps), sps},
		{"stap-a header only", []byte{0x78}, nil},
		{"stap-a one size byte", []byte{0x78, 0x00}, nil},
		{"stap-a size, no unit", []byte{0x78, 0x00, 0x01}, nil},
		{"stap-a size 0", []byte{0x78, 0x00, 0x00, 0x67, 0x42}, nil},
		{"stap-a size 0 after pps", append(stapA(pps), 0x00, 0x00, 0x67), nil},
		{"stap-a size one past the end", []byte{0x78, 0x00, 0x03, 0x67, 0x42}, nil},
		{"stap-a size 0xffff", []byte{0x78, 0xff, 0xff, 0x67}, nil},
		{"stap-a sps first, then garbage", append(stapA(sps), 0xff, 0xff, 0x00), sps},
		{"stap-a pps, then truncated sps", append(stapA(pps), 0x00, 0x10, 0x67, 0x64), nil},
		{"stap-a trailing byte", append(stapA(pps), 0x01), nil},
		{"stap-a one-byte sps", stapA([]byte{0x67}), []byte{0x67}},
		{"fu-a carrying an sps", []byte{0x7c, 0x87, 0x64}, nil}, // never used for an SPS in WebRTC
		{"stap-b", []byte{0x79, 0x00, 0x00, 0x00, 0x02, 0x67, 0x42}, nil},
		{"pps", pps, nil},
	}
	for _, c := range cases {
		got := spsNAL(c.payload)
		if !bytes.Equal(got, c.want) || (got == nil) != (c.want == nil) {
			t.Errorf("%s: spsNAL = %x, want %x", c.name, got, c.want)
		}
		if got := isKeyframeStart(c.payload); got != (c.want != nil) {
			t.Errorf("%s: isKeyframeStart = %v", c.name, got)
		}
	}
}

func TestIsKeyframeStartAllocs(t *testing.T) {
	payload := stapA(mustHex(t, ppsHex), mustHex(t, realSPS[0].hex))
	if n := testing.AllocsPerRun(100, func() { _ = isKeyframeStart(payload) }); n != 0 {
		t.Errorf("isKeyframeStart allocates %v times per call", n)
	}
}

func BenchmarkIsKeyframeStart(b *testing.B) {
	payload := stapA(mustHex(b, "06050102"), mustHex(b, ppsHex), mustHex(b, realSPS[0].hex))
	for b.Loop() {
		_ = isKeyframeStart(payload)
	}
}

func TestParseSPSReal(t *testing.T) {
	pps := mustHex(t, ppsHex)
	for _, c := range realSPS {
		nal := mustHex(t, c.hex)
		if nal[0] != c.nalHeader {
			t.Errorf("%s: fixture NAL header %#x", c.name, nal[0])
		}
		s, err := parseSPS(nal)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if s.width != c.w || s.height != c.h || s.profileLevelID() != c.plid {
			t.Errorf("%s: got %dx%d %s, want %dx%d %s", c.name, s.width, s.height, s.profileLevelID(), c.w, c.h, c.plid)
		}
		// The same unit inside a STAP-A, as the SFU finds it in RTP.
		s2, err := parseSPS(spsNAL(stapA(nal, pps)))
		if err != nil || s2 != s {
			t.Errorf("%s in a STAP-A: %+v, %v", c.name, s2, err)
		}
	}
}

func TestParseSPSTruncated(t *testing.T) {
	for _, c := range realSPS {
		nal := mustHex(t, c.hex)
		// Every prefix that ends before the frame cropping fields must fail cleanly; the rest may parse, and if
		// they do, they parse to the same result (the VUI is not read).
		full, _ := parseSPS(nal)
		for n := range len(nal) {
			s, err := parseSPS(nal[:n])
			if err != nil {
				if !errors.Is(err, errSPS) {
					t.Errorf("%s[:%d]: error %v doesn't wrap errSPS", c.name, n, err)
				}
				continue
			}
			if s != full {
				t.Errorf("%s[:%d] parsed to %+v, want %+v", c.name, n, s, full)
			}
		}
	}
}

// bitWriter builds test bitstreams (from the S2-VT spike's tests).
type bitWriter struct {
	b    []byte
	nbit int
}

func (w *bitWriter) put(v uint32, n int) {
	for i := n - 1; i >= 0; i-- {
		if w.nbit%8 == 0 {
			w.b = append(w.b, 0)
		}
		if v>>uint(i)&1 == 1 {
			w.b[len(w.b)-1] |= 0x80 >> uint(w.nbit%8)
		}
		w.nbit++
	}
}

func (w *bitWriter) flag(b bool) {
	if b {
		w.put(1, 1)
	} else {
		w.put(0, 1)
	}
}

func (w *bitWriter) ue(v uint32) {
	x := uint64(v) + 1
	n := 0
	for t := x; t > 1; t >>= 1 {
		n++
	}
	w.put(0, n)
	for i := n; i >= 0; i-- {
		w.put(uint32(x>>uint(i)&1), 1)
	}
}

func (w *bitWriter) se(v int64) {
	if v > 0 {
		w.ue(uint32(2*v - 1)) //nolint:gosec // test values stay within se(v)'s range
	} else {
		w.ue(uint32(-2 * v)) //nolint:gosec // test values stay within se(v)'s range
	}
}

// escape inserts emulation-prevention bytes: 03 after any 00 00 followed by a byte ≤ 3.
func escape(b []byte) []byte {
	var out []byte
	zeros := 0
	for _, c := range b {
		if zeros >= 2 && c <= 3 {
			out = append(out, 3)
			zeros = 0
		}
		out = append(out, c)
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

// spsSpec describes a synthetic SPS. Zero values give a 1280x720 4:2:0 High SPS with POC type 0.
type spsSpec struct {
	profile, constraints, level uint8
	spsID                       uint32
	chroma                      uint32 // chroma_format_idc (High family only); 0 means 1 (4:2:0) unless mono
	mono                        bool   // chroma_format_idc 0
	bitDepthLuma8               uint32 // bit_depth_luma_minus8
	scalingLists                []int  // indexes of the scaling lists present
	log2FrameNum4               uint32
	pocType                     uint32
	log2PocLsb4                 uint32
	pocCycle                    []int32
	widthMBs, heightUnits       uint32 // pic_width_in_mbs_minus1 + 1, pic_height_in_map_units_minus1 + 1
	fields                      bool   // frame_mbs_only_flag = 0
	crop                        []uint32
	rawCycleLen                 *uint32 // overrides len(pocCycle) in the bitstream
}

func (s spsSpec) nal() []byte {
	if s.profile == 0 {
		s.profile = 100
	}
	if s.level == 0 {
		s.level = 31
	}
	if s.widthMBs == 0 {
		s.widthMBs, s.heightUnits = 80, 45
	}
	var w bitWriter
	w.put(uint32(s.profile), 8)
	w.put(uint32(s.constraints), 8)
	w.put(uint32(s.level), 8)
	w.ue(s.spsID)
	if highProfileFamily(s.profile) {
		chroma := s.chroma
		switch {
		case s.mono:
			chroma = 0
		case chroma == 0:
			chroma = 1
		}
		w.ue(chroma)
		if chroma == 3 {
			w.put(1, 1) // separate_colour_plane_flag
		}
		w.ue(s.bitDepthLuma8)
		w.ue(0)
		w.put(0, 1)
		w.flag(len(s.scalingLists) > 0)
		if len(s.scalingLists) > 0 {
			n := 8
			if chroma == 3 {
				n = 12
			}
			for i := range n {
				present := false
				for _, j := range s.scalingLists {
					present = present || j == i
				}
				w.flag(present)
				if present {
					// nextScale goes 8 → 11 → 0; a nextScale of 0 ends the list (it repeats the last scale).
					w.se(3)
					w.se(-11)
				}
			}
		}
	}
	w.ue(s.log2FrameNum4)
	w.ue(s.pocType)
	switch s.pocType {
	case 0:
		w.ue(s.log2PocLsb4)
	case 1:
		w.put(0, 1)
		w.se(-2)
		w.se(0)
		n := uint32(len(s.pocCycle)) //nolint:gosec // short test slices
		if s.rawCycleLen != nil {
			n = *s.rawCycleLen
		}
		w.ue(n)
		for _, o := range s.pocCycle {
			w.se(int64(o))
		}
	}
	w.ue(1) // max_num_ref_frames
	w.put(0, 1)
	w.ue(s.widthMBs - 1)
	w.ue(s.heightUnits - 1)
	w.flag(!s.fields)
	if s.fields {
		w.put(1, 1)
	}
	w.put(1, 1)
	w.flag(len(s.crop) == 4)
	for _, c := range s.crop {
		w.ue(c)
	}
	w.put(0, 1) // vui_parameters_present_flag
	w.put(1, 1) // rbsp stop bit
	return append([]byte{0x67}, escape(w.b)...)
}

func TestParseSPSSynthetic(t *testing.T) {
	cases := []struct {
		name string
		spec spsSpec
		w, h int
		plid string
	}{
		{"defaults", spsSpec{}, 1280, 720, "64001f"},
		{"CHigh with scaling lists, POC type 2", spsSpec{constraints: 0x0c, scalingLists: []int{0, 3, 6, 7}, pocType: 2}, 1280, 720, "640c1f"},
		{"POC type 1 with a cycle", spsSpec{pocType: 1, pocCycle: []int32{1, -1, 2}}, 1280, 720, "64001f"},
		{"POC type 1 with the longest cycle", spsSpec{pocType: 1, pocCycle: make([]int32, 255)}, 1280, 720, "64001f"},
		{"1080p, crop bottom 4", spsSpec{widthMBs: 120, heightUnits: 68, crop: []uint32{0, 0, 0, 4}}, 1920, 1080, "64001f"},
		{"interlaced 1080 (field pairs)", spsSpec{widthMBs: 120, heightUnits: 34, fields: true, crop: []uint32{0, 0, 0, 2}}, 1920, 1080, "64001f"},
		{"4:4:4 crop units are luma samples", spsSpec{profile: 244, chroma: 3, crop: []uint32{0, 4, 0, 0}}, 1276, 720, "f4001f"},
		{"4:2:2 crop units", spsSpec{profile: 122, chroma: 2, crop: []uint32{1, 1, 0, 8}}, 1276, 712, "7a001f"},
		{"monochrome crop units", spsSpec{mono: true, crop: []uint32{0, 0, 0, 8}}, 1280, 712, "64001f"},
		{"Baseline: no chroma fields", spsSpec{profile: 66, constraints: 0xe0, level: 31, pocType: 2}, 1280, 720, "42e01f"},
		{"1-pixel-wide after cropping", spsSpec{widthMBs: 1, heightUnits: 1, crop: []uint32{0, 7, 0, 7}}, 2, 2, "64001f"},
		{"8K at the size limit", spsSpec{widthMBs: 480, heightUnits: 270, level: 62}, 7680, 4320, "64003e"},
		{"largest width", spsSpec{widthMBs: 1055, heightUnits: 132}, 16880, 2112, "64001f"},
		{"sps id 31, large fields", spsSpec{spsID: 31, log2FrameNum4: 12, log2PocLsb4: 12, bitDepthLuma8: 6}, 1280, 720, "64001f"},
	}
	for _, c := range cases {
		s, err := parseSPS(c.spec.nal())
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if s.width != c.w || s.height != c.h || s.profileLevelID() != c.plid {
			t.Errorf("%s: got %dx%d %s, want %dx%d %s", c.name, s.width, s.height, s.profileLevelID(), c.w, c.h, c.plid)
		}
	}
	// The emulation-prevention path: an offset of 2^29 is an Exp-Golomb code with 30 leading zeros, so the NAL unit
	// needs an emulation-prevention byte, which the parser must remove.
	nal := spsSpec{pocType: 1, pocCycle: []int32{1 << 29}}.nal()
	if !bytes.Contains(nal, []byte{0, 0, 3}) {
		t.Fatalf("test SPS %x has no emulation-prevention byte", nal)
	}
	if s, err := parseSPS(nal); err != nil || s.width != 1280 || s.height != 720 {
		t.Errorf("escaped SPS: %+v, %v", s, err)
	}
}

func TestParseSPSErrors(t *testing.T) {
	u32 := func(v uint32) *uint32 { return &v }
	bad := []struct {
		name string
		nal  []byte
	}{
		{"empty", nil},
		{"header only", []byte{0x67}},
		{"too short", []byte{0x67, 0x64, 0x00}},
		{"too short once unescaped", mustHex(t, "67000003")},
		{"a PPS", mustHex(t, "28ee3cb0")},
		{"an IDR slice", mustHex(t, "25b82003bfc35fcd72ffeeb4")},
		{"STAP-A (not unwrapped)", stapA(mustHex(t, realSPS[0].hex))},
		{"sps id 32", spsSpec{spsID: 32}.nal()},
		{"chroma_format_idc 4", spsSpec{chroma: 4}.nal()},
		{"bit depth 15", spsSpec{bitDepthLuma8: 7}.nal()},
		{"log2_max_frame_num 17", spsSpec{log2FrameNum4: 13}.nal()},
		{"POC type 3", spsSpec{pocType: 3}.nal()},
		{"log2_max_poc_lsb 17", spsSpec{log2PocLsb4: 13}.nal()},
		{"POC cycle 256", spsSpec{pocType: 1, pocCycle: make([]int32, 256)}.nal()},
		{"POC cycle length huge", spsSpec{pocType: 1, rawCycleLen: u32(1<<32 - 2)}.nal()},
		{"POC cycle runs out of bits", spsSpec{pocType: 1, rawCycleLen: u32(200)}.nal()},
		{"width 1056 MBs", spsSpec{widthMBs: 1056, heightUnits: 1}.nal()},
		{"height 1056 MBs", spsSpec{widthMBs: 1, heightUnits: 1056}.nal()},
		{"interlaced height 1056 MBs", spsSpec{widthMBs: 1, heightUnits: 528 + 1, fields: true}.nal()},
		{"frame over level 6.2's MaxFS", spsSpec{widthMBs: 1055, heightUnits: 133}.nal()},
		{"huge width", spsSpec{widthMBs: 1<<32 - 1, heightUnits: 1}.nal()},
		{"huge interlaced height (would wrap in 32 bits)", spsSpec{widthMBs: 1, heightUnits: 1 << 31, fields: true}.nal()},
		{"crop removes the width", spsSpec{widthMBs: 1, heightUnits: 1, crop: []uint32{4, 4, 0, 0}}.nal()},
		{"crop removes the height", spsSpec{crop: []uint32{0, 0, 360, 0}}.nal()},
		{"huge crop offsets", spsSpec{crop: []uint32{1<<32 - 2, 1<<32 - 2, 0, 0}}.nal()},
		{"Exp-Golomb longer than 32 bits", []byte{0x67, 0x64, 0x00, 0x1f, 0x00, 0x00, 0x00, 0x00, 0x80}},
		{"all zeros", append([]byte{0x67}, make([]byte, 64)...)},
		{"all ones (runs out in the scaling lists)", append([]byte{0x67, 0x64, 0x00, 0x1f}, bytes.Repeat([]byte{0xff}, 8)...)},
	}
	for _, c := range bad {
		s, err := parseSPS(c.nal)
		if err == nil {
			t.Errorf("%s: parsed to %+v, want an error", c.name, s)
			continue
		}
		if !errors.Is(err, errSPS) {
			t.Errorf("%s: %v doesn't wrap errSPS", c.name, err)
		}
	}
}

func TestUnescapeRBSP(t *testing.T) {
	cases := []struct{ in, want string }{
		{"000003010203", "0000010203"},
		{"0000030000030001", "000000000001"},
		{"00000303", "000003"},
		{"0103000003", "01030000"},
		{"000003", "0000"},
		{"aabbcc", "aabbcc"},
		{"", ""},
	}
	for _, c := range cases {
		if got := unescapeRBSP(mustHex(t, c.in)); !bytes.Equal(got, mustHex(t, c.want)) {
			t.Errorf("unescapeRBSP(%s) = %x, want %s", c.in, got, c.want)
		}
	}
}

func TestExpGolomb(t *testing.T) {
	var w bitWriter
	ues := []uint32{0, 1, 2, 3, 7, 8, 119, 67, 65535, 1 << 20, 1<<32 - 2}
	ses := []int64{0, 1, -1, 2, -2, 100, -100, 1<<31 - 1, -(1<<31 - 1)}
	for _, v := range ues {
		w.ue(v)
	}
	for _, v := range ses {
		w.se(v)
	}
	r := &bitReader{b: w.b}
	for _, want := range ues {
		if got := r.ue(); r.err != nil || got != want {
			t.Fatalf("ue: got %d (%v), want %d", got, r.err, want)
		}
	}
	for _, want := range ses {
		if got := r.se(); r.err != nil || got != want {
			t.Fatalf("se: got %d (%v), want %d", got, r.err, want)
		}
	}
	// 32 leading zeros: longer than any 32-bit code.
	r = &bitReader{b: []byte{0, 0, 0, 0, 0xff}}
	if got := r.ue(); got != 0 || !errors.Is(r.err, errExpGolomb) {
		t.Errorf("33-bit code: %d, %v", got, r.err)
	}
	r = &bitReader{b: []byte{0}}
	if got := r.ue(); got != 0 || !errors.Is(r.err, errBitsShort) {
		t.Errorf("all-zero input: %d, %v", got, r.err)
	}
	// Reads after an error return 0 and keep the first error.
	if r.u(8) != 0 || r.ue() != 0 || r.flag() || !errors.Is(r.err, errBitsShort) {
		t.Error("reads after an error must return 0 and keep the error")
	}
	r = &bitReader{b: []byte{0x00, 0x01}} // 15 zeros, a 1, and no bits left for the suffix
	if r.ue(); !errors.Is(r.err, errBitsShort) {
		t.Errorf("truncated suffix: %v", r.err)
	}
}

func FuzzKeyframeStart(f *testing.F) {
	pps := mustHex(f, ppsHex)
	for _, c := range realSPS {
		sps := mustHex(f, c.hex)
		f.Add(sps)
		f.Add(stapA(sps, pps))
		f.Add(stapA(pps, sps))
	}
	f.Add([]byte{0x78, 0x00, 0x09, 0x67})
	f.Add([]byte{0x78, 0xff, 0xff})
	f.Add([]byte{0x7c, 0x85, 0x00})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, payload []byte) {
		sps := spsNAL(payload)
		if isKeyframeStart(payload) != (sps != nil) {
			t.Fatal("isKeyframeStart disagrees with spsNAL")
		}
		if sps == nil {
			return
		}
		if len(sps) == 0 || sps[0]&nalTypeMask != nalTypeSPS {
			t.Fatalf("spsNAL returned %x", sps)
		}
		// The result aliases payload and lies inside it.
		off := cap(payload) - cap(sps)
		if off < 0 || off+len(sps) > len(payload) || !bytes.Equal(payload[off:off+len(sps)], sps) {
			t.Fatalf("spsNAL result is not inside the payload (offset %d, len %d of %d)", off, len(sps), len(payload))
		}
		_, _ = parseSPS(sps)
	})
}

func FuzzParseSPS(f *testing.F) {
	for _, c := range realSPS {
		f.Add(mustHex(f, c.hex))
	}
	f.Add(spsSpec{scalingLists: []int{0, 7}, pocType: 1, pocCycle: []int32{1, 2}}.nal())
	f.Add(spsSpec{profile: 244, chroma: 3, crop: []uint32{0, 4, 0, 0}}.nal())
	f.Add(spsSpec{widthMBs: 120, heightUnits: 34, fields: true, crop: []uint32{0, 0, 0, 2}}.nal())
	f.Add([]byte{0x67, 0x64, 0x00, 0x1f, 0x00, 0x00, 0x00, 0x00, 0x80})
	f.Fuzz(func(t *testing.T, nal []byte) {
		s, err := parseSPS(nal)
		if err != nil {
			if !errors.Is(err, errSPS) {
				t.Fatalf("error %v doesn't wrap errSPS", err)
			}
			return
		}
		if len(nal) < 4 || nal[0]&nalTypeMask != nalTypeSPS {
			t.Fatalf("parsed a non-SPS %x", nal)
		}
		maxSide := spsMaxDimensionMBs * 16
		if s.width <= 0 || s.height <= 0 || s.width > maxSide || s.height > maxSide {
			t.Fatalf("size %dx%d out of range", s.width, s.height)
		}
		if mbs := ((s.width + 15) / 16) * ((s.height + 15) / 16); mbs > spsMaxFrameMBs {
			t.Fatalf("%d macroblocks", mbs)
		}
	})
}
