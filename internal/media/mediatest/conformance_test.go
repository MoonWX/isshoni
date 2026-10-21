package mediatest

import (
	"bytes"
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestHasStartCode(t *testing.T) {
	for _, tc := range []struct {
		b    []byte
		want bool
	}{
		{[]byte{0, 0, 1, 0x65}, true},
		{[]byte{0, 0, 0, 1, 0x67}, true},
		{[]byte{0, 0, 2, 0x65}, false},
		{[]byte{0, 1, 0x65}, false},
		{[]byte{0x65}, false},
		{nil, false},
	} {
		if got := hasStartCode(tc.b); got != tc.want {
			t.Errorf("hasStartCode(%x) = %v", tc.b, got)
		}
	}
}

func TestNALUnits(t *testing.T) {
	au := []byte{
		0, 0, 0, 1, 0x67, 0x64, 0x00, 0x28, // SPS, 4-byte start code
		0, 0, 1, 0x68, 0xee, 0x3c, 0x80, // PPS, 3-byte start code
		0, 0, 0, 1, 0x65, 0x88, 0x00, 0x00, 0x03, 0x01, // IDR with an emulation prevention byte
	}
	got := nalUnits(au)
	want := [][]byte{{0x67, 0x64, 0x00, 0x28}, {0x68, 0xee, 0x3c, 0x80}, {0x65, 0x88, 0x00, 0x00, 0x03, 0x01}}
	if len(got) != len(want) {
		t.Fatalf("nalUnits = %x, want %x", got, want)
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("NAL unit %d = %x, want %x", i, got[i], want[i])
		}
	}
	if n := nalUnits([]byte{0x65, 0x88}); n != nil {
		t.Errorf("nalUnits without a start code = %x", n)
	}
}

func TestSPSProfile(t *testing.T) {
	for _, tc := range []struct {
		name        string
		au          []byte
		idc, constr byte
		ok          bool
	}{
		{"high", []byte{0, 0, 0, 1, 0x67, 0x64, 0x00, 0x2a, 0xac, 0, 0, 0, 1, 0x65, 0x88}, 100, 0x00, true},
		{"cb", []byte{0, 0, 1, 0x67, 0x42, 0xe0, 0x1f, 0x8c, 0, 0, 1, 0x68, 0xce}, 66, 0xe0, true},
		// An SPS whose RBSP starts 00 00 03 xx (not a real profile, but the unescaping must apply).
		{"escaped", []byte{0, 0, 1, 0x67, 0x00, 0x00, 0x03, 0x01, 0x1f}, 0, 0, true},
		{"no SPS", []byte{0, 0, 1, 0x65, 0x88, 0x84}, 0, 0, false},
		{"short SPS", []byte{0, 0, 1, 0x67, 0x42}, 0, 0, false},
	} {
		idc, constr, ok := spsProfile(tc.au)
		if idc != tc.idc || constr != tc.constr || ok != tc.ok {
			t.Errorf("%s: spsProfile = %d, %#02x, %v, want %d, %#02x, %v", tc.name, idc, constr, ok, tc.idc, tc.constr,
				tc.ok)
		}
	}
}

func TestUnescape(t *testing.T) {
	in := []byte{0x00, 0x00, 0x03, 0x01, 0x00, 0x00, 0x03, 0x00, 0x00, 0x03}
	want := []byte{0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00}
	if got := unescape(in); !bytes.Equal(got, want) {
		t.Errorf("unescape(%x) = %x, want %x", in, got, want)
	}
}

func TestLayers(t *testing.T) {
	l := layers()
	if len(l) != 2 || l[0].RID != "f" || l[0].MaxPixels != 1920*1080 || l[0].MaxFPS != 60 || l[0].Bitrate != 4_000_000 ||
		l[1].RID != "q" || l[1].MaxPixels != 640*360 || l[1].MaxFPS != 15 || l[1].Bitrate != 300_000 {
		t.Errorf("layers() = %+v, want 01 §6.4's 1080p60 at 4 Mbit/s and 360p15 at 300 kbit/s", l)
	}
}
