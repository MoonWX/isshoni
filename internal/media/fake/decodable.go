package fake

import (
	"bytes"
	"time"
)

// Decodable H.264 (02 §15.1) is a tiny pure-Go encoder written from the H.264 spec (ITU-T H.264 7.3, 8.3.5, 8.4.1):
// Constrained Baseline, CAVLC, pic_order_cnt_type 2, one slice per frame, deblocking off, the size padded to whole
// macroblocks and cropped back in the SPS. It never approximates: an I_PCM macroblock carries its samples verbatim,
// and a P_Skip macroblock copies the reference picture, because every motion vector predicts to zero. The only
// inter macroblocks are P_Skip and the only intra ones I_PCM (mv 0, refIdx −1 for prediction), so every motion
// vector predictor is the median of zeros (8.4.1.1, 8.4.1.3). The decoder's picture therefore always equals the
// encoder's.
//
//   - IDR: every macroblock is I_PCM (mb_type 25), samples within 16–235.
//   - P: P_Skip everywhere except the macroblocks whose content changed, which are I_PCM (mb_type 30).
//
// The picture (at display size w×h): a checkerboard background with a colour gradient; a 32×32 amber box that
// moves across it on a triangle wave (4 s per round trip horizontally, 3 s vertically), a function of the capture
// time, so every layer shows it at the same place relative to its size; the frame index's low 16 bits as 8×16
// blocks (white 1, black 0, most significant first) from x = 16 in the top macroblock row; a white 64×64 flash
// square in the top-right corner on flash frames; and the marker macroblock at the top left.
//
// The marker macroblock (macroblock 0) is I_PCM in every frame. Its first 36 luma samples carry the frame's Marker,
// one nibble each (0x40 | nibble, high nibble first), so the Marker is at the same place as in a synthetic slice:
// within the first bytes of the slice, where ParseVideoMarker reads it from the first RTP packet of a frame.
// Everything else in it is dark grey (0x40), with neutral chroma.
//
// An IDR frame of the default f layer (640×360) is about 355 kB; a delta frame codes about 15 macroblocks
// (about 6 kB).

// Syntax values of the decodable slices.
const (
	sliceTypeP      = 5 // P, and every slice of the picture is P (H.264 Table 7-6)
	sliceTypeI      = 7 // I, and every slice of the picture is I
	mbTypeIPCMInI   = 25
	mbTypeIPCMInP   = 30 // 5 + 25: the intra types follow the five P types (Table 7-13)
	log2MaxFrameNum = 8  // buildSPS writes log2_max_frame_num_minus4 = 4
	mbSize          = 16
	mbLuma          = mbSize * mbSize
	mbChroma        = mbLuma / 4          // per chroma component, 4:2:0
	pcmBytes        = mbLuma + 2*mbChroma // 384: pcm_sample_luma, then Cb, then Cr, raster order each
	pcmMarkerTag    = 0x40                // marker samples are 0x40 | nibble
)

// Scene geometry, in luma samples.
const (
	boxSize       = 32
	flashSize     = 64
	counterBits   = 16
	counterX      = mbSize // right of the marker macroblock
	counterBitW   = 8
	counterH      = mbSize
	boxPeriodX    = 4 * time.Second
	boxPeriodY    = 3 * time.Second
	sampleMin     = 16
	sampleMax     = 235
	chromaNeutral = 128
)

// The approximate average bitrates of DefaultDecodableLayers, with 3 s GOPs and a flash every second
// (TestDecodableRate keeps them honest).
const (
	decodableRateF = 2_300_000
	decodableRateQ = 900_000
)

// decodableMinLevel is the lowest level_idc of a Decodable SPS: level 3.0. Frame size and macroblock rate alone put
// the default q layer (320×180@15) at level 1.2, whose MaxBR (384 kbit/s, 460.8 kbit/s for the NAL HRD) its I_PCM
// IDR frames exceed twice over. Level 3.0 (MaxBR 10 Mbit/s, MaxCPB 10 Mbit) holds both default layers' rates and a
// 640×360 I_PCM IDR frame (about 2.9 Mbit), and stays within the 42e01f the SFU offers (02 §8.1). The I_PCM IDR
// frames still break the level's MinCR, by design.
const decodableMinLevel = 30

// yuv is one colour, BT.601 limited range.
type yuv struct{ y, cb, cr byte }

var (
	colourWhite  = yuv{235, chromaNeutral, chromaNeutral}
	colourBlack  = yuv{16, chromaNeutral, chromaNeutral}
	colourBox    = yuv{170, 39, 175} // amber, RGB (255, 176, 0)
	colourMarker = yuv{pcmMarkerTag, chromaNeutral, chromaNeutral}
)

// scene is the picture of one frame.
type scene struct {
	w, h       int // display size
	cw, ch     int // coded size (whole macroblocks)
	frame      uint32
	flash      bool
	boxX, boxY int
	marker     [MarkerSize]byte
}

// boxAt returns the box's top-left corner at capture time t: a triangle wave over the picture, below the flash
// square's rows when the picture is tall enough.
func boxAt(w, h int, t int64) (x, y int) {
	yMin := min(flashSize, max(h-boxSize, 0))
	return triangle(t, boxPeriodX, max(w-boxSize, 0)), yMin + triangle(t, boxPeriodY, max(h-boxSize-yMin, 0))
}

// triangle maps t ≥ 0 onto 0…span and back once per period.
func triangle(t int64, period time.Duration, span int) int {
	p := int64(period)
	ph := t % p
	if ph > p/2 {
		ph = p - ph
	}
	return int(int64(span) * 2 * ph / p)
}

// at returns the colour at luma position (x, y) outside the marker macroblock.
func (sc *scene) at(x, y int) yuv {
	if y < counterH && x >= counterX && x < counterX+counterBits*counterBitW {
		if bit := counterBits - 1 - (x-counterX)/counterBitW; sc.frame>>uint(bit)&1 == 1 {
			return colourWhite
		}
		return colourBlack
	}
	if sc.flash && y < flashSize && x >= sc.w-flashSize && x < sc.w {
		return colourWhite
	}
	if x >= sc.boxX && x < sc.boxX+boxSize && y >= sc.boxY && y < sc.boxY+boxSize {
		return colourBox
	}
	c := yuv{56, byte(96 + 64*x/sc.cw), byte(96 + 64*y/sc.ch)}
	if (x/32+y/32)%2 == 1 {
		c.y = 80
	}
	return c
}

// render writes macroblock (mbx, mby) in I_PCM sample order to out.
func (sc *scene) render(mbx, mby int, out *[pcmBytes]byte) {
	if mbx == 0 && mby == 0 {
		for i := range mbLuma {
			out[i] = colourMarker.y
		}
		for i, c := range sc.marker {
			out[2*i] |= c >> 4
			out[2*i+1] |= c & 0x0f
		}
		for i := mbLuma; i < pcmBytes; i++ {
			out[i] = chromaNeutral
		}
		return
	}
	x0, y0 := mbx*mbSize, mby*mbSize
	for j := range mbSize {
		for i := range mbSize {
			out[j*mbSize+i] = clampSample(sc.at(x0+i, y0+j).y)
		}
	}
	// 4:2:0: each chroma sample takes the colour at its co-sited luma sample (top left of its 2×2).
	for j := range mbSize / 2 {
		for i := range mbSize / 2 {
			c := sc.at(x0+2*i, y0+2*j)
			out[mbLuma+j*mbSize/2+i] = clampSample(c.cb)
			out[mbLuma+mbChroma+j*mbSize/2+i] = clampSample(c.cr)
		}
	}
}

// clampSample keeps a sample within 16–235 (02 §15.1).
func clampSample(v byte) byte { return min(max(v, sampleMin), sampleMax) }

// decodable is one layer's encoder. Its reference picture is what a decoder holds after the last frame.
type decodable struct {
	w, h       int // display size
	wMBs, hMBs int
	ref        []byte // pcmBytes per macroblock, in macroblock address order
	frameNum   uint8
	idrs       uint16 // IDR frames so far: idr_pic_id
	boxX, boxY int    // the last frame's box
	coded      []bool // scratch: the macroblocks of this frame that are I_PCM
	mb         [pcmBytes]byte
	rbsp       bitWriter
}

func newDecodable(l VideoLayer) *decodable {
	wMBs, hMBs := (l.Width+mbSize-1)/mbSize, (l.Height+mbSize-1)/mbSize
	return &decodable{
		w: l.Width, h: l.Height, wMBs: wMBs, hMBs: hMBs,
		ref: make([]byte, wMBs*hMBs*pcmBytes), coded: make([]bool, wMBs*hMBs),
	}
}

// accessUnit encodes the next frame: [sps, pps,] one slice NAL unit, Annex B.
func (d *decodable) accessUnit(key bool, m Marker, capture int64, sps, pps []byte) []byte {
	sc := scene{w: d.w, h: d.h, cw: d.wMBs * mbSize, ch: d.hMBs * mbSize, frame: m.Frame, flash: m.Flash}
	sc.boxX, sc.boxY = boxAt(d.w, d.h, capture)
	appendMarker(sc.marker[:0], m)
	d.rbsp.b, d.rbsp.nbit = d.rbsp.b[:0], 0
	if key {
		d.frameNum = 0
		d.writeIDR(&sc)
		d.idrs++
	} else {
		d.frameNum++ // every frame is a reference frame: frame_num counts them modulo 2^8
		d.writeP(&sc)
	}
	d.boxX, d.boxY = sc.boxX, sc.boxY
	rbsp := d.rbsp.trailing()

	au := make([]byte, 0, len(sps)+len(pps)+len(rbsp)+len(rbsp)/64+16)
	hdr := byte(nalNonIDR)
	if key {
		hdr = nalIDR
		au = append(au, startCode[:]...)
		au = append(au, sps...)
		au = append(au, startCode[:]...)
		au = append(au, pps...)
	}
	au = append(au, startCode[:]...)
	au = append(au, hdr)
	return appendEscaped(au, rbsp)
}

// writeIDR writes an I slice of I_PCM macroblocks and makes the scene the reference picture.
func (d *decodable) writeIDR(sc *scene) {
	d.sliceHeader(true)
	for addr := range d.wMBs * d.hMBs {
		ref := (*[pcmBytes]byte)(d.ref[addr*pcmBytes:])
		sc.render(addr%d.wMBs, addr/d.wMBs, ref)
		d.rbsp.ue(mbTypeIPCMInI)
		d.rbsp.align()
		d.rbsp.bytes(ref[:])
	}
}

// writeP writes a P slice: the macroblocks that differ from the reference picture as I_PCM (the marker macroblock
// always), the others P_Skip.
func (d *decodable) writeP(sc *scene) {
	clear(d.coded)
	d.mark(0, 0, mbSize, mbSize)
	d.mark(counterX, 0, counterX+counterBits*counterBitW, counterH)
	d.mark(d.w-flashSize, 0, d.w, flashSize)
	d.mark(d.boxX, d.boxY, d.boxX+boxSize, d.boxY+boxSize)
	d.mark(sc.boxX, sc.boxY, sc.boxX+boxSize, sc.boxY+boxSize)
	for addr, c := range d.coded {
		if !c {
			continue
		}
		sc.render(addr%d.wMBs, addr/d.wMBs, &d.mb)
		ref := d.ref[addr*pcmBytes : (addr+1)*pcmBytes]
		if addr != 0 && bytes.Equal(d.mb[:], ref) {
			d.coded[addr] = false
			continue
		}
		copy(ref, d.mb[:])
	}

	d.sliceHeader(false)
	skip := 0
	for addr, c := range d.coded {
		if !c {
			skip++
			continue
		}
		d.rbsp.ue(uint64(skip)) // mb_skip_run
		skip = 0
		d.rbsp.ue(mbTypeIPCMInP)
		d.rbsp.align()
		d.rbsp.bytes(d.ref[addr*pcmBytes : (addr+1)*pcmBytes])
	}
	if skip > 0 {
		d.rbsp.ue(uint64(skip)) // the trailing run; then more_rbsp_data() is false
	}
}

// mark marks the macroblocks that the luma rectangle [x0, x1) × [y0, y1) touches, clipped to the picture.
func (d *decodable) mark(x0, y0, x1, y1 int) {
	x0, y0 = max(x0, 0)/mbSize, max(y0, 0)/mbSize
	x1, y1 = min((x1+mbSize-1)/mbSize, d.wMBs), min((y1+mbSize-1)/mbSize, d.hMBs)
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			d.coded[y*d.wMBs+x] = true
		}
	}
}

// sliceHeader writes the slice header (H.264 7.3.3) of an IDR (I) or a reference P slice, for buildSPS's SPS and
// buildPPS's PPS: frame_num of log2MaxFrameNum bits, no POC fields (type 2), one reference picture, sliding-window
// marking, CAVLC, QP 26 (unused by I_PCM and P_Skip), deblocking off.
func (d *decodable) sliceHeader(idr bool) {
	w := &d.rbsp
	w.ue(0) // first_mb_in_slice
	if idr {
		w.ue(sliceTypeI)
	} else {
		w.ue(sliceTypeP)
	}
	w.ue(0) // pic_parameter_set_id
	w.u(log2MaxFrameNum, uint64(d.frameNum))
	if idr {
		w.ue(uint64(d.idrs)) // idr_pic_id: consecutive IDR pictures differ
	} else {
		w.u(1, 0) // num_ref_idx_active_override_flag
		w.u(1, 0) // ref_pic_list_modification_flag_l0
	}
	// dec_ref_pic_marking(): nal_ref_idc is not 0.
	if idr {
		w.u(1, 0) // no_output_of_prior_pics_flag
		w.u(1, 0) // long_term_reference_flag
	} else {
		w.u(1, 0) // adaptive_ref_pic_marking_mode_flag: sliding window
	}
	w.se(0) // slice_qp_delta
	w.ue(1) // disable_deblocking_filter_idc: deblocking off
}

// parseDecodableMarker reads the Marker of a decodable slice from the start of its RBSP: the slice header that
// sliceHeader writes, then macroblock 0, which is I_PCM with the Marker in its first luma samples.
func parseDecodableMarker(rbsp []byte) (Marker, bool) {
	r := rbspReader{b: rbsp}
	if r.ue() != 0 { // first_mb_in_slice
		return Marker{}, false
	}
	idr := false
	switch r.ue() {
	case sliceTypeI:
		idr = true
	case sliceTypeP:
	default:
		return Marker{}, false
	}
	if r.ue() != 0 { // pic_parameter_set_id
		return Marker{}, false
	}
	r.u(log2MaxFrameNum)
	var flags uint32 // the header's flags, all 0
	wantType := uint32(mbTypeIPCMInP)
	if idr {
		r.ue() // idr_pic_id
		flags = r.u(2)
		wantType = mbTypeIPCMInI
	} else {
		flags = r.u(3)
	}
	if flags != 0 || r.se() != 0 || r.ue() != 1 { // slice_qp_delta, disable_deblocking_filter_idc
		return Marker{}, false
	}
	if !idr && r.ue() != 0 { // mb_skip_run: macroblock 0 is coded
		return Marker{}, false
	}
	if r.ue() != wantType {
		return Marker{}, false
	}
	for r.pos%8 != 0 {
		if r.u(1) != 0 { // pcm_alignment_zero_bit
			return Marker{}, false
		}
	}
	var raw [MarkerSize]byte
	for i := range 2 * MarkerSize {
		s := r.u(8)
		if s&0xf0 != pcmMarkerTag {
			return Marker{}, false
		}
		raw[i/2] |= byte(s&0x0f) << (4 * (1 - i%2))
	}
	if r.bad {
		return Marker{}, false
	}
	return decodeMarker(raw[:])
}

// rbspReader reads an RBSP MSB first. After it runs out, reads return 0 and bad is set.
type rbspReader struct {
	b   []byte
	pos int // in bits
	bad bool
}

// u reads n ≤ 32 bits.
func (r *rbspReader) u(n int) uint32 {
	var v uint32
	for range n {
		if r.pos >= len(r.b)*8 {
			r.bad = true
			return 0
		}
		v = v<<1 | uint32(r.b[r.pos/8]>>(7-r.pos%8)&1)
		r.pos++
	}
	return v
}

// ue reads an Exp-Golomb code of at most 31 leading zeros.
func (r *rbspReader) ue() uint32 {
	zeros := 0
	for r.u(1) == 0 {
		if r.bad || zeros == 31 {
			r.bad = true
			return 0
		}
		zeros++
	}
	return 1<<zeros - 1 + r.u(zeros)
}

// se reads a signed Exp-Golomb code.
func (r *rbspReader) se() int32 {
	v := r.ue()
	if v%2 == 1 {
		return int32(v/2) + 1
	}
	return -int32(v / 2)
}
