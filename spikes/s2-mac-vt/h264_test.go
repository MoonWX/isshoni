package main

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// SPS NAL units captured from this spike's own VideoToolbox output (1920x1080, High,
// AutoLevel). The first one was also decoded by hand; see the comments in the test.
const (
	spsVT1080p       = "2764002aac5680780227e59a80808081"           // com.apple.videotoolbox.videoencoder.ave.avc
	spsVT1080pLowLat = "2764002aac1314501e0089f966a0202020f0804258" // ...h264.rtvc (low-latency RC)
	idrPrefix        = "25b82003bfc35fcd72ffeeb4"
	pPrefix          = "21e104421fd5d70347c54c22"
)

func TestSplitAnnexB(t *testing.T) {
	in := []byte{0, 0, 0, 1, 0x67, 1, 2, 0, 0, 1, 0x68, 3, 0, 0, 0, 1, 0x65, 4, 5, 0}
	got := SplitAnnexB(in)
	want := [][]byte{{0x67, 1, 2}, {0x68, 3}, {0x65, 4, 5}}
	if len(got) != len(want) {
		t.Fatalf("got %d NALs, want %d: %x", len(got), len(want), got)
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("NAL %d = %x, want %x", i, got[i], want[i])
		}
	}
	if n := SplitAnnexB([]byte{1, 2, 3}); len(n) != 0 {
		t.Errorf("no start code: got %x", n)
	}
}

func TestUnescape(t *testing.T) {
	cases := []struct{ in, want string }{
		{"000003010203", "0000010203"},
		{"0000030000030001", "000000000001"},
		{"00000303", "000003"},
		{"0103000003", "01030000"},
		{"aabbcc", "aabbcc"},
	}
	for _, c := range cases {
		if got := Unescape(mustHex(t, c.in)); !bytes.Equal(got, mustHex(t, c.want)) {
			t.Errorf("Unescape(%s) = %x, want %s", c.in, got, c.want)
		}
	}
}

// bitWriter builds test bitstreams.
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

func (w *bitWriter) ue(v uint32) {
	x := v + 1
	n := 0
	for t := x; t > 1; t >>= 1 {
		n++
	}
	w.put(0, n)
	w.put(x, n+1)
}

func (w *bitWriter) se(v int32) {
	if v > 0 {
		w.ue(uint32(2*v - 1))
	} else {
		w.ue(uint32(-2 * v))
	}
}

func TestExpGolomb(t *testing.T) {
	var w bitWriter
	ues := []uint32{0, 1, 2, 3, 7, 8, 119, 67, 65535, 1 << 20}
	ses := []int32{0, 1, -1, 2, -2, 100, -100}
	for _, v := range ues {
		w.ue(v)
	}
	for _, v := range ses {
		w.se(v)
	}
	r := &bitReader{b: w.b}
	for _, want := range ues {
		if got, err := r.ue(); err != nil || got != want {
			t.Fatalf("ue: got %d (%v), want %d", got, err, want)
		}
	}
	for _, want := range ses {
		if got, err := r.se(); err != nil || got != want {
			t.Fatalf("se: got %d (%v), want %d", got, err, want)
		}
	}
	if _, err := (&bitReader{b: []byte{0}}).ue(); err == nil {
		t.Error("ue on all-zero input should fail")
	}
}

func TestParseSPSVideoToolbox(t *testing.T) {
	// Hand decode of spsVT1080p: 64 00 2a = High, no constraint flags, level 4.2;
	// chroma 4:2:0, 8-bit, no scaling lists; log2_max_frame_num 5; POC type 0; 1 ref;
	// 120x68 MBs, frame_mbs_only, crop bottom 4 (x2) -> 1920x1080; VUI: video_format 5,
	// limited range, BT.709 primaries/transfer/matrix; no timing, no HRD, no bitstream
	// restriction.
	s, err := ParseSPS(mustHex(t, spsVT1080p))
	if err != nil {
		t.Fatal(err)
	}
	want := SPS{
		ProfileIDC: 100, ConstraintFlags: 0, LevelIDC: 42, ProfileLevelID: "64002a", ChromaFormatIDC: 1,
		BitDepthLuma: 8, Log2MaxFrameNum: 5, PicOrderCntType: 0, MaxNumRefFrames: 1, FrameMbsOnly: true,
		Width: 1920, Height: 1080, CodedWidth: 1920, CodedHeight: 1088, VUI: true, ColourPrimaries: 1,
		TransferChar: 1, MatrixCoeffs: 1, MaxNumReorder: -1, MaxDecFrameBuffers: -1,
	}
	if s != want {
		t.Errorf("ParseSPS:\n got %+v\nwant %+v", s, want)
	}
	if s.ProfileName() != "High" {
		t.Errorf("ProfileName = %q", s.ProfileName())
	}

	ll, err := ParseSPS(mustHex(t, spsVT1080pLowLat))
	if err != nil {
		t.Fatal(err)
	}
	if ll.ProfileLevelID != "64002a" || ll.Width != 1920 || ll.Height != 1080 || ll.MaxNumRefFrames != 4 ||
		!ll.BitstreamRestrict || ll.MaxNumReorder != 0 || ll.MaxDecFrameBuffers != 4 || ll.PicOrderCntType != 0 {
		t.Errorf("low-latency SPS: %+v", ll)
	}

	if _, err := ParseSPS(mustHex(t, spsVT1080p)[:8]); err == nil {
		t.Error("truncated SPS should fail")
	}
	if _, err := ParseSPS(mustHex(t, "28ee3cb0")); err == nil {
		t.Error("PPS parsed as SPS")
	}
}

func TestParseSPSSynthetic(t *testing.T) {
	// Constrained High 4:2:0 with scaling lists, POC type 2, 1280x720 (crop 0), timing
	// info, NAL HRD and bitstream restriction, plus an emulation-prevention byte.
	var w bitWriter
	w.put(100, 8)
	w.put(0x0c, 8)
	w.put(31, 8)
	w.ue(0) // sps id
	w.ue(1) // chroma 4:2:0
	w.ue(0)
	w.ue(0)
	w.put(0, 1)
	w.put(1, 1) // seq_scaling_matrix_present
	for i := range 8 {
		if i == 0 {
			w.put(1, 1)
			for range 16 {
				w.se(0) // delta 0 keeps nextScale at 8
			}
		} else {
			w.put(0, 1)
		}
	}
	w.ue(0) // log2_max_frame_num_minus4
	w.ue(2) // poc type 2
	w.ue(1) // refs
	w.put(0, 1)
	w.ue(79) // 80 MBs
	w.ue(44) // 45 map units
	w.put(1, 1)
	w.put(1, 1)
	w.put(0, 1) // no crop
	w.put(1, 1) // vui
	w.put(0, 1)
	w.put(0, 1)
	w.put(0, 1)
	w.put(0, 1)
	w.put(1, 1) // timing
	w.put(1, 32)
	w.put(120, 32)
	w.put(1, 1)
	w.put(1, 1) // nal hrd
	w.ue(0)
	w.put(0, 4)
	w.put(0, 4)
	w.ue(1000)
	w.ue(2000)
	w.put(0, 1)
	w.put(23, 5)
	w.put(23, 5)
	w.put(23, 5)
	w.put(24, 5)
	w.put(0, 1) // vcl hrd
	w.put(1, 1) // low_delay
	w.put(0, 1)
	w.put(1, 1) // bitstream restriction
	w.put(1, 1)
	w.ue(2)
	w.ue(1)
	w.ue(16)
	w.ue(16)
	w.ue(0)
	w.ue(1)
	w.put(1, 1) // stop bit
	// escape: insert 03 after any 00 00 followed by a byte <= 3
	var esc []byte
	zeros := 0
	for _, c := range w.b {
		if zeros >= 2 && c <= 3 {
			esc = append(esc, 3)
			zeros = 0
		}
		esc = append(esc, c)
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	nal := append([]byte{0x67}, esc...)
	s, err := ParseSPS(nal)
	if err != nil {
		t.Fatal(err)
	}
	if s.ProfileLevelID != "640c1f" || s.ProfileName() != "Constrained High" || !s.ScalingMatrix ||
		s.PicOrderCntType != 2 || s.Width != 1280 || s.Height != 720 || !s.TimingInfo || s.TimeScale != 120 ||
		!s.HRD || !s.BitstreamRestrict || s.MaxNumReorder != 0 || s.MaxDecFrameBuffers != 1 {
		t.Errorf("synthetic SPS: %+v", s)
	}
}

func TestParseSliceHeader(t *testing.T) {
	idr, err := ParseSliceHeader(mustHex(t, idrPrefix))
	if err != nil || idr.FirstMB != 0 || SliceTypeName(idr.SliceType) != "I" || idr.PPSID != 0 {
		t.Errorf("IDR: %+v %v", idr, err)
	}
	p, err := ParseSliceHeader(mustHex(t, pPrefix))
	if err != nil || SliceTypeName(p.SliceType) != "P" {
		t.Errorf("P: %+v %v", p, err)
	}
	// first_mb 0, slice_type 6 (B, all slices), pps 0: 1 00111 1 1 -> 0x9f 0x80
	b, err := ParseSliceHeader([]byte{0x01, 0x9f, 0x80})
	if err != nil || SliceTypeName(b.SliceType) != "B" || b.SliceType != 6 {
		t.Errorf("B: %+v %v", b, err)
	}
	if _, err := ParseSliceHeader(mustHex(t, spsVT1080p)); err == nil {
		t.Error("SPS parsed as slice")
	}
}

func TestAnalyzeStream(t *testing.T) {
	sc := []byte{0, 0, 0, 1}
	var stream []byte
	var aus []AURef
	add := func(key bool, frame int32, nals ...[]byte) {
		off := int64(len(stream))
		for _, n := range nals {
			stream = append(stream, sc...)
			stream = append(stream, n...)
		}
		aus = append(aus, AURef{Off: off, Len: int32(int64(len(stream)) - off), Frame: frame, Key: key})
	}
	sps, pps := mustHex(t, spsVT1080p), mustHex(t, "28ee3cb0")
	idr, p := mustHex(t, idrPrefix), mustHex(t, pPrefix)
	nonRefP := append([]byte{0x01}, p[1:]...) // nal_ref_idc 0

	add(true, 0, sps, pps, idr)
	add(false, 1, p)
	add(false, 2, nonRefP)
	rep := AnalyzeStream(stream, aus, 100)
	if !rep.OK || rep.AccessUnits != 3 || rep.IDRFrames != 1 || rep.BSlices != 0 || rep.NonRefFrames != 1 ||
		rep.SliceTypes["I"] != 1 || rep.SliceTypes["P"] != 2 || rep.SPS == nil || rep.SPS.Width != 1920 {
		t.Errorf("good stream: %+v", rep)
	}

	add(false, 3, []byte{0x01, 0x9f, 0x80}) // a B slice
	add(true, 4, idr)                       // IDR without SPS/PPS in band
	rep = AnalyzeStream(stream, aus, 100)
	if rep.OK || rep.BSlices != 1 || rep.IDRWithoutParams != 1 {
		t.Errorf("bad stream: %+v", rep)
	}
	if rep := AnalyzeStream(stream, aus[:1], 66); rep.OK {
		t.Error("profile 66 wanted, High accepted")
	}
}
