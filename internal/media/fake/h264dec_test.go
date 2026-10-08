package fake

import (
	"bytes"
	"errors"
	"fmt"
)

// testDecoder decodes the decodable fake video like a decoder must (H.264 7.3, 7.4, 8.2.1, 8.3.5, 8.4.1): it reads
// every syntax element of the SPS, PPS, slice header and slice data with its own bit reader, following the spec's
// syntax tables rather than the encoder's code, and rebuilds the pictures. It supports the subset the fake emits
// (Baseline, frame pictures, CAVLC, one slice per picture, one reference frame, I_PCM and P_Skip macroblocks) and
// fails on anything else, which is how it checks that the stream stays inside that subset.
type testDecoder struct {
	sps       map[uint32]*decSPS
	pps       map[uint32]*decPPS
	ref       *decPicture // the one reference frame
	refNum    int         // its frame_num
	lastIDRID int         // idr_pic_id of the previous access unit when it was an IDR, else −1
}

func newTestDecoder() *testDecoder {
	return &testDecoder{sps: map[uint32]*decSPS{}, pps: map[uint32]*decPPS{}, lastIDRID: -1}
}

type decSPS struct {
	profileIDC, constraints, level uint8
	log2MaxFrameNum                int
	pocType                        uint32
	log2MaxPOCLsb                  int
	maxRefFrames                   uint32
	gaps                           bool
	wMBs, hMBs                     int
	cropL, cropR, cropT, cropB     int
}

// size returns the display size after cropping (4:2:0: crop units of 2 samples).
func (s *decSPS) size() (w, h int) {
	return s.wMBs*16 - 2*(s.cropL+s.cropR), s.hMBs*16 - 2*(s.cropT+s.cropB)
}

type decPPS struct {
	spsID                 uint32
	bottomFieldPOC        bool
	numRefIdxL0           uint32 // num_ref_idx_l0_default_active_minus1 + 1
	weightedPred          bool
	qp                    int32 // pic_init_qp
	deblockingControl     bool
	redundantPicCntExists bool
}

// decSlice is what the decoder learned about an access unit's slice.
type decSlice struct {
	idr       bool
	sliceType uint32
	frameNum  int
	idrPicID  int
	pcm, skip int
	pcmAddrs  []int
}

// decPicture is a decoded frame at the coded size, 4:2:0.
type decPicture struct {
	sps       *decSPS
	y, cb, cr []byte
}

func newPicture(s *decSPS) *decPicture {
	n := s.wMBs * s.hMBs * mbLuma
	return &decPicture{sps: s, y: make([]byte, n), cb: make([]byte, n/4), cr: make([]byte, n/4)}
}

// lumaAt and chromaAt read the picture (x, y in luma samples).
func (p *decPicture) lumaAt(x, y int) byte { return p.y[y*p.sps.wMBs*16+x] }
func (p *decPicture) chromaAt(x, y int) (cb, cr byte) {
	i := y/2*p.sps.wMBs*8 + x/2
	return p.cb[i], p.cr[i]
}

// decode decodes one Annex B access unit: parameter sets, then exactly one slice.
func (d *testDecoder) decode(au []byte) (decSlice, *decPicture, error) {
	if !bytes.HasPrefix(au, startCode[:]) {
		return decSlice{}, nil, errors.New("no start code")
	}
	nals := bytes.Split(au[len(startCode):], startCode[:])
	for i, nal := range nals {
		if len(nal) < 2 || nal[0]&0x80 != 0 {
			return decSlice{}, nil, fmt.Errorf("NAL unit %d: bad header", i)
		}
		refIDC, typ := int(nal[0]>>5&3), nal[0]&0x1f
		rbsp := testUnescape(nal[1:])
		var err error
		switch typ {
		case 7:
			err = d.parseSPS(rbsp)
		case 8:
			err = d.parsePPS(rbsp)
		case 1, 5:
			if i != len(nals)-1 {
				return decSlice{}, nil, errors.New("a slice is not the last NAL unit")
			}
			return d.decodeSlice(rbsp, typ == 5, refIDC)
		default:
			err = fmt.Errorf("unexpected NAL unit type %d", typ)
		}
		if err != nil {
			return decSlice{}, nil, fmt.Errorf("NAL unit %d (type %d): %w", i, typ, err)
		}
	}
	return decSlice{}, nil, errors.New("no slice")
}

// testUnescape removes emulation-prevention bytes and checks that none is missing.
func testUnescape(b []byte) []byte {
	out := make([]byte, 0, len(b))
	zeros := 0
	for _, c := range b {
		if zeros >= 2 && c == 3 {
			zeros = 0
			continue
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

// parseSPS reads seq_parameter_set_rbsp() (7.3.2.1.1) for profile_idc 66 (Baseline).
func (d *testDecoder) parseSPS(rbsp []byte) error {
	r := newDecBits(rbsp)
	s := &decSPS{profileIDC: uint8(r.u(8)), constraints: uint8(r.u(8)), level: uint8(r.u(8))}
	if s.profileIDC != 66 || s.constraints&0x40 == 0 || s.constraints&0x03 != 0 {
		return fmt.Errorf("profile %d, constraints %#x: not Constrained Baseline", s.profileIDC, s.constraints)
	}
	id := r.ue()
	s.log2MaxFrameNum = int(r.ue()) + 4
	s.pocType = r.ue()
	switch s.pocType {
	case 0:
		s.log2MaxPOCLsb = int(r.ue()) + 4
	case 2:
	default:
		return fmt.Errorf("pic_order_cnt_type %d not supported", s.pocType)
	}
	s.maxRefFrames = r.ue()
	s.gaps = r.flag()
	s.wMBs, s.hMBs = int(r.ue())+1, int(r.ue())+1
	if !r.flag() {
		return errors.New("frame_mbs_only_flag 0 (fields) not supported")
	}
	r.flag() // direct_8x8_inference_flag
	if r.flag() {
		s.cropL, s.cropR, s.cropT, s.cropB = int(r.ue()), int(r.ue()), int(r.ue()), int(r.ue())
	}
	if r.flag() {
		return errors.New("VUI not supported")
	}
	if err := r.trailing(); err != nil {
		return err
	}
	if s.log2MaxFrameNum > 16 || s.maxRefFrames != 1 || id > 31 {
		return fmt.Errorf("log2_max_frame_num %d, max_num_ref_frames %d, id %d", s.log2MaxFrameNum,
			s.maxRefFrames, id)
	}
	d.sps[id] = s
	return nil
}

// parsePPS reads pic_parameter_set_rbsp() (7.3.2.2) without the High profile extension.
func (d *testDecoder) parsePPS(rbsp []byte) error {
	r := newDecBits(rbsp)
	id := r.ue()
	p := &decPPS{spsID: r.ue()}
	if r.flag() {
		return errors.New("CABAC not supported")
	}
	p.bottomFieldPOC = r.flag()
	if r.ue() != 0 {
		return errors.New("slice groups not supported")
	}
	p.numRefIdxL0 = r.ue() + 1
	r.ue() // num_ref_idx_l1_default_active_minus1
	p.weightedPred = r.flag()
	if r.u(2) != 0 {
		return errors.New("weighted bi-prediction in a Baseline PPS")
	}
	p.qp = 26 + r.se()
	r.se() // pic_init_qs_minus26
	r.se() // chroma_qp_index_offset
	p.deblockingControl = r.flag()
	r.flag() // constrained_intra_pred_flag
	p.redundantPicCntExists = r.flag()
	if err := r.trailing(); err != nil {
		return err
	}
	if _, ok := d.sps[p.spsID]; !ok {
		return fmt.Errorf("PPS on unknown SPS %d", p.spsID)
	}
	d.pps[id] = p
	return nil
}

// mbInfo is what motion vector prediction needs to know about a decoded macroblock.
type mbInfo struct {
	intra  bool
	mv     [2]int
	refIdx int
}

// decodeSlice reads slice_layer_without_partitioning_rbsp() (7.3.2.8): the slice header (7.3.3) and the slice data
// (7.3.4, CAVLC), and rebuilds the picture.
func (d *testDecoder) decodeSlice(rbsp []byte, idr bool, refIDC int) (decSlice, *decPicture, error) {
	fail := func(format string, a ...any) (decSlice, *decPicture, error) {
		return decSlice{}, nil, fmt.Errorf("slice: %s", fmt.Sprintf(format, a...))
	}
	r := newDecBits(rbsp)
	sl := decSlice{idr: idr, idrPicID: -1}
	if r.ue() != 0 {
		return fail("first_mb_in_slice is not 0: one slice per picture")
	}
	sl.sliceType = r.ue()
	isI, isP := sl.sliceType%5 == 2, sl.sliceType%5 == 0
	if !isI && !isP || idr && !isI {
		return fail("slice_type %d (IDR %v)", sl.sliceType, idr)
	}
	pps, ok := d.pps[r.ue()]
	if !ok {
		return fail("unknown PPS")
	}
	sps := d.sps[pps.spsID]
	sl.frameNum = int(r.u(sps.log2MaxFrameNum))
	if idr {
		sl.idrPicID = int(r.ue())
	}
	if sps.pocType == 0 {
		r.u(sps.log2MaxPOCLsb) // pic_order_cnt_lsb
		if pps.bottomFieldPOC {
			r.se() // delta_pic_order_cnt_bottom
		}
	}
	if pps.redundantPicCntExists {
		r.ue() // redundant_pic_cnt
	}
	numRefIdx := pps.numRefIdxL0
	if isP {
		if r.flag() { // num_ref_idx_active_override_flag
			numRefIdx = r.ue() + 1
		}
		if r.flag() { // ref_pic_list_modification_flag_l0
			return fail("reference list modification not supported")
		}
		if pps.weightedPred {
			return fail("weighted prediction not supported")
		}
		if numRefIdx != 1 {
			return fail("%d active references", numRefIdx)
		}
	}
	if refIDC == 0 {
		return fail("a non-reference picture: the next P frame would have no reference")
	}
	if idr {
		if r.u(2) != 0 { // no_output_of_prior_pics_flag, long_term_reference_flag
			return fail("IDR marking flags set")
		}
	} else if r.flag() { // adaptive_ref_pic_marking_mode_flag
		return fail("adaptive reference marking not supported")
	}
	qp := pps.qp + r.se()
	if qp < 0 || qp > 51 {
		return fail("QP %d", qp)
	}
	if !pps.deblockingControl || r.ue() != 1 {
		return fail("deblocking is on (disable_deblocking_filter_idc is not 1)")
	}
	if r.err != nil {
		return fail("header: %v", r.err)
	}

	// Frame numbering (7.4.3, gaps not allowed) and IDR ids.
	maxFrameNum := 1 << sps.log2MaxFrameNum
	switch {
	case idr && sl.frameNum != 0:
		return fail("IDR frame_num %d", sl.frameNum)
	case idr && sl.idrPicID == d.lastIDRID:
		return fail("two consecutive IDR pictures with idr_pic_id %d", sl.idrPicID)
	case !idr && d.ref == nil:
		return fail("a P slice before any IDR")
	case !idr && sl.frameNum != (d.refNum+1)%maxFrameNum:
		return fail("frame_num %d after %d", sl.frameNum, d.refNum)
	case !idr && d.ref.sps != sps:
		return fail("a P slice on another SPS than its reference")
	}

	pic := newPicture(sps)
	total := sps.wMBs * sps.hMBs
	info := make([]mbInfo, total)
	addr := 0
	more := true
	for more {
		if isP {
			run := int(r.ue()) // mb_skip_run
			if addr+run > total {
				return fail("mb_skip_run %d at macroblock %d runs past the picture", run, addr)
			}
			for range run {
				mv := d.pSkipMV(info, sps, addr)
				if mv != [2]int{} {
					return fail("P_Skip macroblock %d predicts motion vector %v, not zero", addr, mv)
				}
				info[addr] = mbInfo{mv: mv, refIdx: 0}
				pic.copyMB(d.ref, addr)
				sl.skip++
				addr++
			}
			if run > 0 {
				more = r.moreData()
			}
		}
		if !more {
			break
		}
		if addr >= total {
			return fail("more macroblocks than the picture's %d", total)
		}
		mbType, want := r.ue(), uint32(mbTypeIPCMInI)
		if isP {
			want = mbTypeIPCMInP
		}
		if mbType != want {
			return fail("macroblock %d: mb_type %d, only I_PCM (%d) is supported", addr, mbType, want)
		}
		for !r.aligned() {
			if r.u(1) != 0 {
				return fail("macroblock %d: pcm_alignment_zero_bit is 1", addr)
			}
		}
		var pcm [pcmBytes]byte
		for i := range pcm {
			pcm[i] = byte(r.u(8))
			if pcm[i] < sampleMin || pcm[i] > sampleMax {
				return fail("macroblock %d: sample %d is %d, outside 16–235", addr, i, pcm[i])
			}
		}
		pic.putPCM(addr, &pcm)
		info[addr] = mbInfo{intra: true, refIdx: -1}
		sl.pcm++
		sl.pcmAddrs = append(sl.pcmAddrs, addr)
		addr++
		more = r.moreData()
	}
	if err := r.trailing(); err != nil {
		return fail("%v", err)
	}
	if addr != total {
		return fail("%d macroblocks, the picture has %d", addr, total)
	}
	// Sliding-window marking with one reference frame: this picture replaces the last.
	d.ref, d.refNum = pic, sl.frameNum
	d.lastIDRID = -1
	if idr {
		d.lastIDRID = sl.idrPicID
	}
	return sl, pic, nil
}

// pSkipMV derives a P_Skip macroblock's motion vector (8.4.1.1) from its decoded neighbours A (left), B (above),
// C (above right) and D (above left), with the 16×16 median prediction of 8.4.1.3.
func (d *testDecoder) pSkipMV(info []mbInfo, sps *decSPS, addr int) [2]int {
	x, y := addr%sps.wMBs, addr/sps.wMBs
	// neighbour returns the macroblock at (x+dx, y+dy): available only inside the picture and before addr.
	neighbour := func(dx, dy int) (mbInfo, bool) {
		nx, ny := x+dx, y+dy
		if nx < 0 || nx >= sps.wMBs || ny < 0 {
			return mbInfo{}, false
		}
		n := ny*sps.wMBs + nx
		if n >= addr {
			return mbInfo{}, false
		}
		return info[n], true
	}
	a, okA := neighbour(-1, 0)
	b, okB := neighbour(0, -1)
	if !okA || !okB || a.refIdx == 0 && a.mv == [2]int{} || b.refIdx == 0 && b.mv == [2]int{} {
		return [2]int{}
	}
	c, okC := neighbour(1, -1)
	if !okC {
		c, okC = neighbour(-1, -1)
	}
	// 8.4.1.3.2: an unavailable or intra neighbour has mv 0 and refIdx −1.
	norm := func(n mbInfo, ok bool) mbInfo {
		if !ok || n.intra {
			return mbInfo{refIdx: -1}
		}
		return n
	}
	a, b, c = norm(a, true), norm(b, true), norm(c, okC)
	matches := 0
	var only [2]int
	for _, n := range []mbInfo{a, b, c} {
		if n.refIdx == 0 {
			matches++
			only = n.mv
		}
	}
	if matches == 1 {
		return only
	}
	median := func(p, q, r int) int { return max(min(p, q), min(max(p, q), r)) }
	return [2]int{median(a.mv[0], b.mv[0], c.mv[0]), median(a.mv[1], b.mv[1], c.mv[1])}
}

// putPCM places an I_PCM macroblock's samples (8.3.5).
func (p *decPicture) putPCM(addr int, pcm *[pcmBytes]byte) {
	w := p.sps.wMBs
	xP, yP := addr%w*16, addr/w*16
	for i := range mbLuma {
		p.y[(yP+i/16)*w*16+xP+i%16] = pcm[i]
	}
	for i := range mbChroma {
		j := (yP/2+i/8)*w*8 + xP/2 + i%8
		p.cb[j] = pcm[mbLuma+i]
		p.cr[j] = pcm[mbLuma+mbChroma+i]
	}
}

// copyMB copies a macroblock from the reference picture: inter prediction with a zero motion vector.
func (p *decPicture) copyMB(ref *decPicture, addr int) {
	w := p.sps.wMBs
	xP, yP := addr%w*16, addr/w*16
	for j := range 16 {
		o := (yP+j)*w*16 + xP
		copy(p.y[o:o+16], ref.y[o:o+16])
	}
	for j := range 8 {
		o := (yP/2+j)*w*8 + xP/2
		copy(p.cb[o:o+8], ref.cb[o:o+8])
		copy(p.cr[o:o+8], ref.cr[o:o+8])
	}
}

// decBits reads an RBSP MSB first; the first error sticks.
type decBits struct {
	b    []byte
	pos  int
	last int // the position of the last 1 bit: rbsp_stop_one_bit
	err  error
}

func newDecBits(b []byte) *decBits {
	r := &decBits{b: b, last: -1}
	for i := len(b)*8 - 1; i >= 0; i-- {
		if b[i/8]>>(7-i%8)&1 == 1 {
			r.last = i
			break
		}
	}
	return r
}

func (r *decBits) u(n int) uint32 {
	var v uint32
	for range n {
		if r.pos >= len(r.b)*8 {
			if r.err == nil {
				r.err = errors.New("RBSP too short")
			}
			return 0
		}
		v = v<<1 | uint32(r.b[r.pos/8]>>(7-r.pos%8)&1)
		r.pos++
	}
	return v
}

func (r *decBits) flag() bool { return r.u(1) == 1 }

func (r *decBits) ue() uint32 {
	zeros := 0
	for r.u(1) == 0 && r.err == nil {
		zeros++
		if zeros > 31 {
			r.err = errors.New("Exp-Golomb code too long")
			return 0
		}
	}
	return 1<<zeros - 1 + r.u(zeros)
}

func (r *decBits) se() int32 {
	v := r.ue()
	if v%2 == 1 {
		return int32(v/2) + 1
	}
	return -int32(v / 2)
}

func (r *decBits) aligned() bool { return r.pos%8 == 0 }

// moreData is more_rbsp_data() (7.2): whether anything comes before rbsp_stop_one_bit.
func (r *decBits) moreData() bool { return r.pos < r.last }

// trailing reads rbsp_trailing_bits() and checks that the RBSP ends there.
func (r *decBits) trailing() error {
	if r.err != nil {
		return r.err
	}
	if r.pos != r.last {
		return fmt.Errorf("rbsp_stop_one_bit expected at bit %d, found at %d", r.pos, r.last)
	}
	r.pos++
	if rest := len(r.b)*8 - r.pos; rest > 7 { // only alignment zeros may follow, within the last byte
		return fmt.Errorf("%d bits after rbsp_stop_one_bit", rest)
	}
	return nil
}
