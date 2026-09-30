package fake

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"testing"
)

func newTestRand() *rand.Rand { return newRand(1, 2) }

// splitAnnexB splits an access unit at its 4-byte start codes and checks each NAL unit's escaping.
func splitAnnexB(t *testing.T, au []byte) [][]byte {
	t.Helper()
	if !bytes.HasPrefix(au, startCode[:]) {
		t.Fatalf("access unit starts with %x", au[:min(len(au), 4)])
	}
	var nals [][]byte
	for _, part := range bytes.Split(au[4:], startCode[:]) {
		checkEscaped(t, part)
		nals = append(nals, part)
	}
	return nals
}

// checkEscaped fails when a NAL unit contains 00 00 0x (x ≤ 2), an escape that isn't one, or ends with a zero byte.
func checkEscaped(t *testing.T, nal []byte) {
	t.Helper()
	if len(nal) < 2 || nal[len(nal)-1] == 0 {
		t.Fatalf("NAL unit %x is too short or ends with 00", nal[:min(len(nal), 8)])
	}
	for i := 2; i < len(nal); i++ {
		if nal[i-2] == 0 && nal[i-1] == 0 {
			if nal[i] <= 2 {
				t.Fatalf("start-code emulation at byte %d", i)
			}
			if nal[i] == 3 && (i+1 >= len(nal) || nal[i+1] > 3) {
				t.Fatalf("0x03 at byte %d is not an escape", i)
			}
		}
	}
}

func nalTypes(nals [][]byte) []string {
	var out []string
	for _, n := range nals {
		out = append(out, fmt.Sprintf("%#x", n[0]))
	}
	return out
}

// testSPS is what readSPS reads back.
type testSPS struct {
	profileIDC, constraints, level uint8
	width, height                  int
}

func (s testSPS) profileKey() string { return fmt.Sprintf("%02x%02x", s.profileIDC, s.constraints) }

// readSPS parses the SPS NAL units buildSPS writes (H.264 7.3.2.1.1, the fields this package sets) with a bit
// reader of its own, so the writer isn't checked against itself. S22 adds the check with the SFU's parser.
func readSPS(t *testing.T, nal []byte) testSPS {
	t.Helper()
	if nal[0] != nalSPS {
		t.Fatalf("not an SPS: %#x", nal[0])
	}
	var rbsp []byte
	zeros := 0
	for _, c := range nal[1:] {
		if zeros >= 2 && c == 3 {
			zeros = 0
			continue
		}
		rbsp = append(rbsp, c)
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	r := &testBits{b: rbsp}
	s := testSPS{profileIDC: uint8(r.u(8)), constraints: uint8(r.u(8)), level: uint8(r.u(8))}
	r.want("seq_parameter_set_id", r.ue(), 0)
	if s.profileIDC == 100 {
		r.want("chroma_format_idc", r.ue(), 1)
		r.want("bit_depth_luma_minus8", r.ue(), 0)
		r.want("bit_depth_chroma_minus8", r.ue(), 0)
		r.want("qpprime_y_zero_transform_bypass_flag", r.u(1), 0)
		r.want("seq_scaling_matrix_present_flag", r.u(1), 0)
	}
	r.ue() // log2_max_frame_num_minus4
	r.want("pic_order_cnt_type", r.ue(), 2)
	r.ue() // max_num_ref_frames
	r.u(1) // gaps_in_frame_num_value_allowed_flag
	w := int(r.ue()+1) * 16
	h := int(r.ue()+1) * 16
	r.want("frame_mbs_only_flag", r.u(1), 1)
	r.u(1) // direct_8x8_inference_flag
	if r.u(1) == 1 {
		w -= 2 * int(r.ue()+r.ue())
		h -= 2 * int(r.ue()+r.ue())
	}
	r.want("vui_parameters_present_flag", r.u(1), 0)
	r.want("rbsp_stop_one_bit", r.u(1), 1)
	for r.pos%8 != 0 {
		r.want("alignment bit", r.u(1), 0)
	}
	if r.pos != len(rbsp)*8 {
		t.Fatalf("%d bytes after the SPS", len(rbsp)-r.pos/8)
	}
	if r.err != nil {
		t.Fatal(r.err)
	}
	s.width, s.height = w, h
	return s
}

type testBits struct {
	b   []byte
	pos int
	err error
}

func (r *testBits) u(n int) uint64 {
	var v uint64
	for range n {
		if r.pos >= len(r.b)*8 {
			r.err = fmt.Errorf("SPS too short")
			return 0
		}
		v = v<<1 | uint64(r.b[r.pos/8]>>(7-r.pos%8)&1)
		r.pos++
	}
	return v
}

func (r *testBits) ue() uint64 {
	n := 0
	for r.err == nil && r.u(1) == 0 {
		n++
	}
	return 1<<n - 1 + r.u(n)
}

func (r *testBits) want(name string, got, want uint64) {
	if r.err == nil && got != want {
		r.err = fmt.Errorf("%s = %d, want %d", name, got, want)
	}
}

// TestBitWriter checks the Exp-Golomb codes against H.264 Table 9-2.
func TestBitWriter(t *testing.T) {
	for _, tc := range []struct {
		v    uint64
		bits string
	}{{0, "1"}, {1, "010"}, {2, "011"}, {3, "00100"}, {6, "00111"}, {7, "0001000"}} {
		var w bitWriter
		w.ue(tc.v)
		var got string
		for i := range len(tc.bits) {
			got += fmt.Sprint(w.b[i/8] >> (7 - i%8) & 1)
		}
		if got != tc.bits || w.nbit != len(tc.bits)%8 {
			t.Errorf("ue(%d) = %s, want %s", tc.v, got, tc.bits)
		}
	}
	for _, tc := range []struct {
		v    int64
		want uint64
	}{{0, 0}, {1, 1}, {-1, 2}, {2, 3}, {-2, 4}} {
		var a, b bitWriter
		a.se(tc.v)
		b.ue(tc.want)
		if !bytes.Equal(a.b, b.b) {
			t.Errorf("se(%d) differs from ue(%d)", tc.v, tc.want)
		}
	}
}
