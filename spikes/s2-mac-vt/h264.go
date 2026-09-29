package main

import (
	"errors"
	"fmt"
	"sort"
)

// Minimal H.264 bitstream parsing: Annex B splitting, SPS (incl. VUI) and the first
// fields of slice headers. Enough to validate what VideoToolbox emits.

const (
	nalSlice = 1
	nalIDR   = 5
	nalSEI   = 6
	nalSPS   = 7
	nalPPS   = 8
	nalAUD   = 9
)

var errShort = errors.New("h264: bitstream too short")

// SplitAnnexB returns the NAL units in b (without start codes). It accepts 3- and
// 4-byte start codes; a zero byte before a 3-byte start code belongs to the start code.
func SplitAnnexB(b []byte) [][]byte {
	var nals [][]byte
	start := -1
	i := 0
	for i+3 <= len(b) {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			if start >= 0 {
				nals = appendNAL(nals, b[start:i])
			}
			i += 3
			start = i
			continue
		}
		i++
	}
	if start >= 0 {
		nals = appendNAL(nals, b[start:])
	}
	return nals
}

func appendNAL(nals [][]byte, n []byte) [][]byte {
	for len(n) > 0 && n[len(n)-1] == 0 {
		n = n[:len(n)-1]
	}
	if len(n) == 0 {
		return nals
	}
	return append(nals, n)
}

// Unescape removes emulation-prevention bytes (00 00 03 -> 00 00) from a NAL payload.
func Unescape(b []byte) []byte {
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

type bitReader struct {
	b   []byte
	pos int // bit position
}

func (r *bitReader) u(n int) (uint32, error) {
	var v uint32
	for range n {
		if r.pos >= len(r.b)*8 {
			return 0, errShort
		}
		bit := (r.b[r.pos/8] >> (7 - uint(r.pos%8))) & 1
		v = v<<1 | uint32(bit)
		r.pos++
	}
	return v, nil
}

func (r *bitReader) flag() (bool, error) {
	v, err := r.u(1)
	return v == 1, err
}

func (r *bitReader) ue() (uint32, error) {
	zeros := 0
	for {
		b, err := r.u(1)
		if err != nil {
			return 0, err
		}
		if b == 1 {
			break
		}
		zeros++
		if zeros > 31 {
			return 0, errors.New("h264: bad exp-golomb code")
		}
	}
	if zeros == 0 {
		return 0, nil
	}
	rest, err := r.u(zeros)
	if err != nil {
		return 0, err
	}
	return (1<<uint(zeros) - 1) + rest, nil
}

func (r *bitReader) se() (int32, error) {
	v, err := r.ue()
	if err != nil {
		return 0, err
	}
	if v%2 == 1 {
		return int32((v + 1) / 2), nil
	}
	return -int32(v / 2), nil
}

// SPS holds the fields of a sequence parameter set that matter for this spike.
type SPS struct {
	ProfileIDC         uint8  `json:"profile_idc"`
	ConstraintFlags    uint8  `json:"constraint_flags"` // constraint_set0..5 + 2 reserved bits, as in profile-level-id
	LevelIDC           uint8  `json:"level_idc"`
	ProfileLevelID     string `json:"profile_level_id"` // e.g. "640c33", as WebRTC SDP would say
	ChromaFormatIDC    uint32 `json:"chroma_format_idc"`
	BitDepthLuma       uint32 `json:"bit_depth_luma"`
	ScalingMatrix      bool   `json:"scaling_matrix"`
	Log2MaxFrameNum    uint32 `json:"log2_max_frame_num"`
	PicOrderCntType    uint32 `json:"pic_order_cnt_type"`
	MaxNumRefFrames    uint32 `json:"max_num_ref_frames"`
	FrameMbsOnly       bool   `json:"frame_mbs_only"`
	Width              int    `json:"width"`
	Height             int    `json:"height"`
	CodedWidth         int    `json:"coded_width"`
	CodedHeight        int    `json:"coded_height"`
	VUI                bool   `json:"vui"`
	VideoFullRange     bool   `json:"video_full_range"`
	ColourPrimaries    int    `json:"colour_primaries"` // -1 if absent
	TransferChar       int    `json:"transfer_characteristics"`
	MatrixCoeffs       int    `json:"matrix_coefficients"`
	TimingInfo         bool   `json:"timing_info"`
	NumUnitsInTick     uint32 `json:"num_units_in_tick,omitempty"`
	TimeScale          uint32 `json:"time_scale,omitempty"`
	HRD                bool   `json:"hrd"`
	BitstreamRestrict  bool   `json:"bitstream_restriction"`
	MaxNumReorder      int    `json:"max_num_reorder_frames"` // -1 if absent
	MaxDecFrameBuffers int    `json:"max_dec_frame_buffering"`
}

// ProfileName names profile_idc (+ constraint flags) the way WebRTC does.
func (s SPS) ProfileName() string {
	set1 := s.ConstraintFlags&0x40 != 0
	set3 := s.ConstraintFlags&0x10 != 0
	set4 := s.ConstraintFlags&0x08 != 0
	set5 := s.ConstraintFlags&0x04 != 0
	switch s.ProfileIDC {
	case 66:
		if set1 {
			return "Constrained Baseline"
		}
		return "Baseline"
	case 77:
		return "Main"
	case 100:
		if set4 && set5 {
			return "Constrained High"
		}
		if set4 {
			return "Progressive High"
		}
		return "High"
	default:
		_ = set3
		return fmt.Sprintf("profile %d", s.ProfileIDC)
	}
}

func isHighFamily(p uint8) bool {
	switch p {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		return true
	}
	return false
}

// ParseSPS parses a SPS NAL unit (including its one-byte NAL header).
func ParseSPS(nal []byte) (SPS, error) {
	var s SPS
	if len(nal) < 4 || nal[0]&0x1f != nalSPS {
		return s, errors.New("h264: not a SPS")
	}
	r := &bitReader{b: Unescape(nal[1:])}
	var err error
	must := func(v uint32, e error) uint32 {
		if err == nil && e != nil {
			err = e
		}
		return v
	}
	mustS := func(v int32, e error) int32 {
		if err == nil && e != nil {
			err = e
		}
		return v
	}
	mustF := func(v bool, e error) bool {
		if err == nil && e != nil {
			err = e
		}
		return v
	}
	s.ProfileIDC = uint8(must(r.u(8)))
	s.ConstraintFlags = uint8(must(r.u(8)))
	s.LevelIDC = uint8(must(r.u(8)))
	s.ProfileLevelID = fmt.Sprintf("%02x%02x%02x", s.ProfileIDC, s.ConstraintFlags, s.LevelIDC)
	must(r.ue()) // seq_parameter_set_id
	s.ChromaFormatIDC = 1
	s.BitDepthLuma = 8
	if isHighFamily(s.ProfileIDC) {
		s.ChromaFormatIDC = must(r.ue())
		if s.ChromaFormatIDC == 3 {
			must(r.u(1)) // separate_colour_plane_flag
		}
		s.BitDepthLuma = 8 + must(r.ue())
		must(r.ue()) // bit_depth_chroma_minus8
		must(r.u(1)) // qpprime_y_zero_transform_bypass_flag
		s.ScalingMatrix = mustF(r.flag())
		if s.ScalingMatrix {
			n := 8
			if s.ChromaFormatIDC == 3 {
				n = 12
			}
			for i := 0; i < n && err == nil; i++ {
				if mustF(r.flag()) {
					size := 16
					if i >= 6 {
						size = 64
					}
					last, next := int32(8), int32(8)
					for j := 0; j < size && err == nil; j++ {
						if next != 0 {
							delta := mustS(r.se())
							next = (last + delta + 256) % 256
						}
						if next != 0 {
							last = next
						}
					}
				}
			}
		}
	}
	s.Log2MaxFrameNum = must(r.ue()) + 4
	s.PicOrderCntType = must(r.ue())
	switch s.PicOrderCntType {
	case 0:
		must(r.ue()) // log2_max_pic_order_cnt_lsb_minus4
	case 1:
		must(r.u(1))
		mustS(r.se())
		mustS(r.se())
		n := must(r.ue())
		for i := uint32(0); i < n && err == nil; i++ {
			mustS(r.se())
		}
	}
	s.MaxNumRefFrames = must(r.ue())
	must(r.u(1)) // gaps_in_frame_num_value_allowed_flag
	wMbs := must(r.ue()) + 1
	hMapUnits := must(r.ue()) + 1
	s.FrameMbsOnly = mustF(r.flag())
	if !s.FrameMbsOnly {
		must(r.u(1)) // mb_adaptive_frame_field_flag
	}
	must(r.u(1)) // direct_8x8_inference_flag
	frameMbsFactor := 2
	if s.FrameMbsOnly {
		frameMbsFactor = 1
	}
	s.CodedWidth = int(wMbs) * 16
	s.CodedHeight = frameMbsFactor * int(hMapUnits) * 16
	s.Width, s.Height = s.CodedWidth, s.CodedHeight
	if mustF(r.flag()) { // frame_cropping_flag
		l, rt, t, b := must(r.ue()), must(r.ue()), must(r.ue()), must(r.ue())
		cropX, cropY := 1, frameMbsFactor
		switch s.ChromaFormatIDC {
		case 1:
			cropX, cropY = 2, 2*frameMbsFactor
		case 2:
			cropX, cropY = 2, frameMbsFactor
		}
		s.Width -= cropX * int(l+rt)
		s.Height -= cropY * int(t+b)
	}
	s.ColourPrimaries, s.TransferChar, s.MatrixCoeffs = -1, -1, -1
	s.MaxNumReorder, s.MaxDecFrameBuffers = -1, -1
	s.VUI = mustF(r.flag())
	if s.VUI && err == nil {
		err = parseVUI(r, &s)
	}
	if err != nil {
		return s, fmt.Errorf("h264: SPS: %w", err)
	}
	return s, nil
}

func parseVUI(r *bitReader, s *SPS) error {
	var err error
	must := func(v uint32, e error) uint32 {
		if err == nil && e != nil {
			err = e
		}
		return v
	}
	mustF := func(v bool, e error) bool {
		if err == nil && e != nil {
			err = e
		}
		return v
	}
	if mustF(r.flag()) { // aspect_ratio_info_present_flag
		if must(r.u(8)) == 255 {
			must(r.u(16))
			must(r.u(16))
		}
	}
	if mustF(r.flag()) { // overscan_info_present_flag
		must(r.u(1))
	}
	if mustF(r.flag()) { // video_signal_type_present_flag
		must(r.u(3))
		s.VideoFullRange = mustF(r.flag())
		if mustF(r.flag()) {
			s.ColourPrimaries = int(must(r.u(8)))
			s.TransferChar = int(must(r.u(8)))
			s.MatrixCoeffs = int(must(r.u(8)))
		}
	}
	if mustF(r.flag()) { // chroma_loc_info_present_flag
		must(r.ue())
		must(r.ue())
	}
	s.TimingInfo = mustF(r.flag())
	if s.TimingInfo {
		s.NumUnitsInTick = must(r.u(32))
		s.TimeScale = must(r.u(32))
		must(r.u(1))
	}
	nalHRD := mustF(r.flag())
	if nalHRD && err == nil {
		err = skipHRD(r)
	}
	vclHRD := mustF(r.flag())
	if vclHRD && err == nil {
		err = skipHRD(r)
	}
	s.HRD = nalHRD || vclHRD
	if s.HRD {
		must(r.u(1)) // low_delay_hrd_flag
	}
	must(r.u(1)) // pic_struct_present_flag
	s.BitstreamRestrict = mustF(r.flag())
	if s.BitstreamRestrict {
		must(r.u(1))
		must(r.ue())
		must(r.ue())
		must(r.ue())
		must(r.ue())
		s.MaxNumReorder = int(must(r.ue()))
		s.MaxDecFrameBuffers = int(must(r.ue()))
	}
	return err
}

func skipHRD(r *bitReader) error {
	cnt, err := r.ue()
	if err != nil {
		return err
	}
	if _, err := r.u(8); err != nil { // bit_rate_scale, cpb_size_scale
		return err
	}
	for i := uint32(0); i <= cnt; i++ {
		if _, err := r.ue(); err != nil {
			return err
		}
		if _, err := r.ue(); err != nil {
			return err
		}
		if _, err := r.u(1); err != nil {
			return err
		}
	}
	_, err = r.u(20)
	return err
}

// SliceHeader holds the first fields of a slice header.
type SliceHeader struct {
	FirstMB   uint32
	SliceType uint32 // 0..9; %5: 0 P, 1 B, 2 I, 3 SP, 4 SI
	PPSID     uint32
}

func ParseSliceHeader(nal []byte) (SliceHeader, error) {
	var h SliceHeader
	if len(nal) < 2 {
		return h, errShort
	}
	t := nal[0] & 0x1f
	if t != nalSlice && t != nalIDR {
		return h, errors.New("h264: not a slice")
	}
	// Only a few bytes are needed; unescape a bounded prefix.
	end := min(len(nal), 16)
	r := &bitReader{b: Unescape(nal[1:end])}
	var err error
	if h.FirstMB, err = r.ue(); err != nil {
		return h, err
	}
	if h.SliceType, err = r.ue(); err != nil {
		return h, err
	}
	if h.SliceType > 9 {
		return h, fmt.Errorf("h264: bad slice_type %d", h.SliceType)
	}
	if h.PPSID, err = r.ue(); err != nil {
		return h, err
	}
	return h, nil
}

func SliceTypeName(t uint32) string {
	return [...]string{"P", "B", "I", "SP", "SI"}[t%5]
}

// AURef locates one access unit (one encoded frame) in an Annex B stream.
type AURef struct {
	Off   int64
	Len   int32
	Frame int32
	Key   bool // VT said sync sample
}

// StreamReport summarises what is in an encoded stream.
type StreamReport struct {
	AccessUnits      int            `json:"access_units"`
	Bytes            int64          `json:"bytes"`
	NALTypes         map[string]int `json:"nal_types"`
	SliceTypes       map[string]int `json:"slice_types"`
	BSlices          int            `json:"b_slices"`
	IDRFrames        int            `json:"idr_frames"`
	NonRefFrames     int            `json:"nonref_frames"` // nal_ref_idc == 0 (temporal-layer style)
	SlicesPerFrame   map[string]int `json:"slices_per_frame"`
	SPS              *SPS           `json:"sps,omitempty"`
	SPSVariants      []string       `json:"sps_variants,omitempty"`
	IDRWithoutParams int            `json:"idr_without_sps_pps"`
	KeyMismatch      int            `json:"key_flag_mismatch"` // VT sync flag vs IDR NAL
	ParseErrors      int            `json:"parse_errors"`
	Errors           []string       `json:"errors,omitempty"`
	OK               bool           `json:"ok"` // High profile, no B slices, params before every IDR, no errors
}

var nalTypeNames = map[uint8]string{
	1: "1 slice", 5: "5 IDR", 6: "6 SEI", 7: "7 SPS", 8: "8 PPS", 9: "9 AUD", 12: "12 filler",
}

func (r *StreamReport) errf(format string, a ...any) {
	r.ParseErrors++
	if len(r.Errors) < 5 {
		r.Errors = append(r.Errors, fmt.Sprintf(format, a...))
	}
}

// AnalyzeStream parses every access unit and checks the properties the plan relies on.
func AnalyzeStream(stream []byte, aus []AURef, wantProfile uint8) StreamReport {
	rep := StreamReport{
		NALTypes:       map[string]int{},
		SliceTypes:     map[string]int{},
		SlicesPerFrame: map[string]int{},
	}
	spsSeen := map[string]bool{}
	for _, au := range aus {
		if au.Off < 0 || au.Len <= 0 || au.Off+int64(au.Len) > int64(len(stream)) {
			continue
		}
		rep.AccessUnits++
		rep.Bytes += int64(au.Len)
		nals := SplitAnnexB(stream[au.Off : au.Off+int64(au.Len)])
		hasSPS, hasPPS, idr := false, false, false
		slices, refSlices := 0, 0
		for _, n := range nals {
			t := n[0] & 0x1f
			name, ok := nalTypeNames[t]
			if !ok {
				name = fmt.Sprintf("%d other", t)
			}
			rep.NALTypes[name]++
			switch t {
			case nalSPS:
				hasSPS = true
				key := fmt.Sprintf("%x", n)
				if !spsSeen[key] {
					spsSeen[key] = true
					sps, err := ParseSPS(n)
					if err != nil {
						rep.errf("frame %d: %v", au.Frame, err)
						continue
					}
					if rep.SPS == nil {
						rep.SPS = &sps
					}
					rep.SPSVariants = append(rep.SPSVariants, fmt.Sprintf("%s %dx%d poc=%d refs=%d",
						sps.ProfileLevelID, sps.Width, sps.Height, sps.PicOrderCntType, sps.MaxNumRefFrames))
				}
			case nalPPS:
				hasPPS = true
			case nalSlice, nalIDR:
				if t == nalIDR {
					idr = true
				}
				slices++
				if n[0]&0x60 != 0 {
					refSlices++
				}
				sh, err := ParseSliceHeader(n)
				if err != nil {
					rep.errf("frame %d: %v", au.Frame, err)
					continue
				}
				st := SliceTypeName(sh.SliceType)
				rep.SliceTypes[st]++
				if st == "B" {
					rep.BSlices++
				}
			}
		}
		rep.SlicesPerFrame[fmt.Sprint(slices)]++
		if slices > 0 && refSlices == 0 {
			rep.NonRefFrames++
		}
		if idr {
			rep.IDRFrames++
			if !hasSPS || !hasPPS {
				rep.IDRWithoutParams++
			}
		}
		if idr != au.Key {
			rep.KeyMismatch++
		}
	}
	sort.Strings(rep.SPSVariants)
	rep.OK = rep.AccessUnits > 0 && rep.SPS != nil && rep.SPS.ProfileIDC == wantProfile &&
		rep.BSlices == 0 && rep.IDRWithoutParams == 0 && rep.ParseErrors == 0 && rep.KeyMismatch == 0
	return rep
}
