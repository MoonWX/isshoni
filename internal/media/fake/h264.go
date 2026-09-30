package fake

import (
	"fmt"
	"strconv"
)

// H.264 NAL unit headers of the synthetic stream (nal_ref_idc and nal_unit_type, H.264 7.3.1).
const (
	nalSPS      = 0x67 // nal_ref_idc 3, type 7
	nalPPS      = 0x68 // nal_ref_idc 3, type 8
	nalIDR      = 0x65 // nal_ref_idc 3, type 5
	nalNonIDR   = 0x41 // nal_ref_idc 2, type 1
	rbspStopBit = 0x80 // rbsp_stop_one_bit and alignment zeros
)

var startCode = [4]byte{0, 0, 0, 1}

// profile is the part of an SPS a profile key sets.
type profile struct {
	idc         uint8 // profile_idc
	constraints uint8 // constraint_set0..5 flags and the reserved bits
}

// parseProfile reads a profile key: 4 lowercase hex digits, profile_idc 66 (Baseline), 77 (Main) or 100 (High).
func parseProfile(key string) (profile, error) {
	v, err := strconv.ParseUint(key, 16, 16)
	if err != nil || len(key) != 4 || key != fmt.Sprintf("%04x", v) {
		return profile{}, fmt.Errorf("%w: Profile %q is not 4 lowercase hex digits", ErrInvalidConfig, key)
	}
	p := profile{idc: uint8(v >> 8), constraints: uint8(v)}
	switch p.idc {
	case 66, 77, 100:
		return p, nil
	}
	return profile{}, fmt.Errorf("%w: Profile %q: profile_idc %d is not 66, 77 or 100", ErrInvalidConfig, key, p.idc)
}

// levels is H.264 Table A-1 without level 1b: level_idc, MaxMBPS (macroblocks per second) and MaxFS (macroblocks).
var levels = [...]struct {
	idc            uint8
	maxMBPS, maxFS int
}{
	{10, 1485, 99}, {11, 3000, 396}, {12, 6000, 396}, {13, 11880, 396},
	{20, 11880, 396}, {21, 19800, 792}, {22, 20250, 1620},
	{30, 40500, 1620}, {31, 108000, 3600}, {32, 216000, 5120},
	{40, 245760, 8192}, {41, 245760, 8192}, {42, 522240, 8704},
	{50, 589824, 22080}, {51, 983040, 36864}, {52, 2073600, 36864},
	{60, 4177920, 139264}, {61, 8355840, 139264}, {62, 16711680, 139264},
}

// levelFor returns the lowest level_idc whose frame size, macroblock rate and dimension limits (A.3.1: each side at
// most sqrt(8·MaxFS) macroblocks) fit a layer.
func levelFor(widthMBs, heightMBs, fps int) (uint8, bool) {
	fs := widthMBs * heightMBs
	for _, l := range levels {
		if fs <= l.maxFS && fs*fps <= l.maxMBPS && widthMBs*widthMBs <= 8*l.maxFS && heightMBs*heightMBs <= 8*l.maxFS {
			return l.idc, true
		}
	}
	return 0, false
}

// buildSPS returns the SPS NAL unit (header, then RBSP with emulation prevention) of a layer: seq_parameter_set_id 0,
// 4:2:0 8-bit, pic_order_cnt_type 2, one reference frame, frame macroblocks only, the size padded to whole
// macroblocks and cropped back, no VUI (H.264 7.3.2.1.1).
func buildSPS(p profile, l VideoLayer) ([]byte, error) {
	wMBs, hMBs := (l.Width+15)/16, (l.Height+15)/16
	level, ok := levelFor(wMBs, hMBs, l.FPS)
	if !ok {
		return nil, fmt.Errorf("%w: layer %q: %dx%d@%d is above H.264 level 6.2", ErrInvalidConfig, l.RID,
			l.Width, l.Height, l.FPS)
	}
	var w bitWriter
	w.u(8, uint64(p.idc))
	w.u(8, uint64(p.constraints))
	w.u(8, uint64(level))
	w.ue(0) // seq_parameter_set_id
	if p.idc == 100 {
		w.ue(1)   // chroma_format_idc: 4:2:0
		w.ue(0)   // bit_depth_luma_minus8
		w.ue(0)   // bit_depth_chroma_minus8
		w.u(1, 0) // qpprime_y_zero_transform_bypass_flag
		w.u(1, 0) // seq_scaling_matrix_present_flag
	}
	w.ue(4)   // log2_max_frame_num_minus4: frame_num has 8 bits
	w.ue(2)   // pic_order_cnt_type: output order is decoding order (no B-frames)
	w.ue(1)   // max_num_ref_frames
	w.u(1, 0) // gaps_in_frame_num_value_allowed_flag
	w.ue(uint64(wMBs - 1))
	w.ue(uint64(hMBs - 1))
	w.u(1, 1) // frame_mbs_only_flag
	w.u(1, 1) // direct_8x8_inference_flag
	// 4:2:0 crop units are 2 samples.
	cropR, cropB := (wMBs*16-l.Width)/2, (hMBs*16-l.Height)/2
	if cropR > 0 || cropB > 0 {
		w.u(1, 1) // frame_cropping_flag
		w.ue(0)   // frame_crop_left_offset
		w.ue(uint64(cropR))
		w.ue(0) // frame_crop_top_offset
		w.ue(uint64(cropB))
	} else {
		w.u(1, 0)
	}
	w.u(1, 0) // vui_parameters_present_flag
	return appendEscaped([]byte{nalSPS}, w.trailing()), nil
}

// buildPPS returns the PPS NAL unit: pic_parameter_set_id 0 on SPS 0, CAVLC, one slice group, no weighted
// prediction, QP 26, deblocking control present (H.264 7.3.2.2).
func buildPPS() []byte {
	var w bitWriter
	w.ue(0)   // pic_parameter_set_id
	w.ue(0)   // seq_parameter_set_id
	w.u(1, 0) // entropy_coding_mode_flag: CAVLC
	w.u(1, 0) // bottom_field_pic_order_in_frame_present_flag
	w.ue(0)   // num_slice_groups_minus1
	w.ue(0)   // num_ref_idx_l0_default_active_minus1
	w.ue(0)   // num_ref_idx_l1_default_active_minus1
	w.u(1, 0) // weighted_pred_flag
	w.u(2, 0) // weighted_bipred_idc
	w.se(0)   // pic_init_qp_minus26
	w.se(0)   // pic_init_qs_minus26
	w.se(0)   // chroma_qp_index_offset
	w.u(1, 1) // deblocking_filter_control_present_flag
	w.u(1, 0) // constrained_intra_pred_flag
	w.u(1, 0) // redundant_pic_cnt_present_flag
	return appendEscaped([]byte{nalPPS}, w.trailing())
}

// syntheticAU builds an Annex B access unit of about size bytes (02 §15.1): [SPS, PPS,] one slice NAL unit whose RBSP
// is the Marker, seeded filler and the stop bit. The size is exact unless emulation prevention adds a byte (about
// once per 16 MB of filler) or size is below the minimum for the NAL units.
func (st *stream) syntheticAU(key bool, m Marker, size int) []byte {
	au := make([]byte, 0, size+8)
	hdr := byte(nalNonIDR)
	if key {
		hdr = nalIDR
		au = append(au, startCode[:]...)
		au = append(au, st.sps...)
		au = append(au, startCode[:]...)
		au = append(au, st.pps...)
	}
	au = append(au, startCode[:]...)
	au = append(au, hdr)
	rbsp := appendMarker(st.scratch[:0], m)
	rbsp = appendFiller(rbsp, max(size-len(au)-MarkerSize-1, 0), st.rng)
	rbsp = append(rbsp, rbspStopBit)
	st.scratch = rbsp
	return appendEscaped(au, rbsp)
}

// appendEscaped appends rbsp to b with emulation prevention (H.264 7.4.1): 0x03 goes after every two zero bytes that
// a byte ≤ 0x03 follows. The zero count starts fresh, so b should end with a NAL header byte.
func appendEscaped(b, rbsp []byte) []byte {
	zeros := 0
	for _, c := range rbsp {
		if zeros >= 2 && c <= 3 {
			b = append(b, 3)
			zeros = 0
		}
		b = append(b, c)
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return b
}

// bitWriter writes an RBSP MSB first.
type bitWriter struct {
	b    []byte
	nbit int // bits used in the last byte, 0–7
}

// u writes the n low bits of v, n ≤ 64 (u(n), H.264 7.2).
func (w *bitWriter) u(n int, v uint64) {
	for i := n - 1; i >= 0; i-- {
		if w.nbit == 0 {
			w.b = append(w.b, 0)
		}
		if v>>uint(i)&1 == 1 {
			w.b[len(w.b)-1] |= 0x80 >> uint(w.nbit)
		}
		w.nbit = (w.nbit + 1) % 8
	}
}

// ue writes an unsigned Exp-Golomb code (ue(v), H.264 9.1), v < 2^32 − 1.
func (w *bitWriter) ue(v uint64) {
	x := v + 1
	n := 0
	for x>>uint(n) > 1 {
		n++
	}
	w.u(n, 0)
	w.u(n+1, x)
}

// se writes a signed Exp-Golomb code (se(v), H.264 9.1.1).
func (w *bitWriter) se(v int64) {
	if v > 0 {
		w.ue(uint64(2*v - 1))
	} else {
		w.ue(uint64(-2 * v))
	}
}

// trailing writes rbsp_trailing_bits and returns the RBSP.
func (w *bitWriter) trailing() []byte {
	w.u(1, 1)
	if w.nbit != 0 {
		w.u(8-w.nbit, 0)
	}
	return w.b
}
