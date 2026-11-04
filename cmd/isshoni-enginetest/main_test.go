package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"slices"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/MoonWX/isshoni/internal/media"
)

func runArgs(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestUsage(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
	}{
		{nil, exitUsage},
		{[]string{"nope"}, exitUsage},
		{[]string{"-x"}, exitUsage},
		{[]string{"-h"}, exitPass},
		{[]string{"probe", "extra"}, exitUsage},
		{[]string{"probe", "-bogus"}, exitUsage},
		{[]string{"capture", "-keyframes", "500ms", "-bitrate-steps", "8M,2M"}, exitUsage},
		{[]string{"capture", "-bitrate-steps", "8X"}, exitUsage},
		{[]string{"capture", "-profile", "main"}, exitUsage},
		{[]string{"capture", "-audio", "everything"}, exitUsage},
		{[]string{"capture", "-encoder", "x264"}, exitUsage},
		{[]string{"capture", "-seconds", "0"}, exitUsage},
		{[]string{"bench", "-dump-aus", "3"}, exitUsage}, // bench writes no files
	} {
		if code, _, stderr := runArgs(t, tc.args...); code != tc.code {
			t.Errorf("%q: exit %d, want %d\n%s", tc.args, code, tc.code, stderr)
		}
	}
	_, _, stderr := runArgs(t)
	for _, name := range []string{"probe", "capture", "bench"} {
		if !strings.Contains(stderr, name) {
			t.Errorf("the usage text lists no %s:\n%s", name, stderr)
		}
	}
}

// Without an engine DLL in the folder (or off Windows) the engine commands are environment failures (exit 2).
func TestNoEngineIsEnvironment(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{"-dll", dir, "probe", "-out", t.TempDir()},
		{"-dll", dir, "capture", "-out", t.TempDir(), "-seconds", "1"},
		{"-dll", dir, "bench", "-seconds", "1"},
	} {
		if code, _, stderr := runArgs(t, args...); code != exitEnv {
			t.Errorf("%q: exit %d, want %d\n%s", args, code, exitEnv, stderr)
		}
	}
}

func TestEnvCode(t *testing.T) {
	for code, want := range map[media.ErrorCode]int{
		media.CodeNoEncoder: exitEnv, media.CodeUnsupportedOS: exitEnv, media.CodeDependency: exitEnv,
		media.CodeInternal: exitFail, media.CodeInvalid: exitFail,
	} {
		if got := envCode(&media.Error{Code: code}); got != want {
			t.Errorf("%s: %d, want %d", code, got, want)
		}
	}
}

func TestParseBitrate(t *testing.T) {
	for in, want := range map[string]int{"8M": 8_000_000, "2.5M": 2_500_000, "500k": 500_000, "800000": 800_000} {
		if got, err := parseBitrate(in); err != nil || got != want {
			t.Errorf("%q: %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "x", "0", "51M", "99k", "-1M"} {
		if _, err := parseBitrate(in); err == nil {
			t.Errorf("%q: no error", in)
		}
	}
	if s, err := parseSteps("8M,2M,8M"); err != nil || len(s) != 3 || s[1] != 2_000_000 {
		t.Errorf("parseSteps: %v, %v", s, err)
	}
}

// The im_caps layout of 04 §5.4 (native/common/tests/testdata/abi_layout.json), on every GOARCH.
func TestCapsLayout(t *testing.T) {
	var c rawCaps
	var a rawAdapter
	var e rawEncoder
	for name, got := range map[string][2]uintptr{
		"im_adapter_info":         {unsafe.Sizeof(a), 160},
		"im_adapter_info.name":    {unsafe.Offsetof(a.Name), 24},
		"im_encoder_info":         {unsafe.Sizeof(e), 104},
		"im_encoder_info.codec":   {unsafe.Offsetof(e.Codec), 40},
		"im_encoder_info.reason":  {unsafe.Offsetof(e.Reason), 72},
		"im_caps":                 {unsafe.Sizeof(c), 1568},
		"im_caps.adapters":        {unsafe.Offsetof(c.Adapters), 24},
		"im_caps.encoder_count":   {unsafe.Offsetof(c.EncoderCount), 664},
		"im_caps.encoders":        {unsafe.Offsetof(c.Encoders), 672},
		"im_caps.probe_ms":        {unsafe.Offsetof(c.ProbeMS), 1504},
		"im_encoder_info.test_us": {unsafe.Offsetof(e.TestEncodeUS), 24},
	} {
		if got[0] != got[1] {
			t.Errorf("%s: %d, want %d", name, got[0], got[1])
		}
	}
}

func TestDecodeAndCheckProbe(t *testing.T) {
	c := &rawCaps{OSBuild: 26100, OSUBR: 4061, OSFlags: 0x281, AudioFlags: 0x7, AdapterCount: 1, EncoderCount: 3,
		ProbeMS: 900}
	copy(c.Adapters[0].Name[:], "Some GPU")
	c.Adapters[0].VendorID = 0x10DE
	c.Adapters[0].DriverVersion = 32<<48 | 15<<16 | 6094
	c.Adapters[0].Flags = imAdapterHasOutputs
	c.Encoders[0] = rawEncoder{Kind: 1, AdapterIndex: 0, Flags: imEncfAvailable | imEncfHardware | imEncfHigh}
	c.Encoders[1] = rawEncoder{Kind: 5, AdapterIndex: 0xFFFFFFFF, Status: -12}
	copy(c.Encoders[1].Reason[:], "no_media_foundation")
	c.Encoders[2] = rawEncoder{Kind: 6, AdapterIndex: 0xFFFFFFFF, Status: -12}
	copy(c.Encoders[2].Reason[:], "disabled")
	d := decodeCaps(c, true)
	d.WallMS = 950
	if fails := checkProbe(d); len(fails) != 0 {
		t.Errorf("a good probe failed: %v", fails)
	}
	if d.Adapters[0].Name != "Some GPU" || d.Adapters[0].Driver != "32.0.15.6094" || d.Encoders[0].Name != "nvenc" ||
		d.Encoders[1].Adapter != -1 || d.Encoders[1].Reason != "no_media_foundation" {
		t.Errorf("decoded %+v", d)
	}
	if strings.Join(d.OSFlags, ",") != "supported,desktopDuplication,mediaFoundation" {
		t.Errorf("os flags %v", d.OSFlags)
	}
	d.WallMS = 2100
	if fails := checkProbe(d); len(fails) != 1 || !strings.Contains(fails[0], "2100 ms") {
		t.Errorf("a slow probe: %v", fails)
	}
	d.WallMS = 100
	d.Encoders[2].Reason = ""
	if fails := checkProbe(d); len(fails) != 1 || !strings.Contains(fails[0], "without a reason") {
		t.Errorf("a status without its token: %v", fails)
	}
	d.Encoders[2].Reason = "disabled"
	d.Encoders[0].Available = false
	if fails := checkProbe(d); len(fails) != 1 || !strings.Contains(fails[0], "untested") {
		t.Errorf("an untested OK encoder in a full probe: %v", fails)
	}
	d.Full = false
	if fails := checkProbe(d); len(fails) != 0 {
		t.Errorf("a quick probe's untested encoder is fine: %v", fails)
	}
}

func TestAnnexB(t *testing.T) {
	au := []byte{0, 0, 0, 1, 0x67, 100, 0, 40, 0, 0, 1, 0x68, 0xCE, 0, 0, 0, 1, 0x65, 0x88, 0, 0, 1, 0x65, 0x84, 0}
	types := nalTypes(au)
	if len(types) != 4 || types[0] != nalSPS || types[1] != nalPPS || types[2] != nalIDR || types[3] != nalIDR {
		t.Fatalf("types %v", types)
	}
	if !isKeyframeAU(types) {
		t.Error("SPS, PPS, IDR, IDR is a keyframe")
	}
	if idc, flags, ok := spsProfile(au); !ok || idc != 100 || flags != 0 {
		t.Errorf("spsProfile: %d %x %t", idc, flags, ok)
	}
	for _, bad := range [][]uint8{{nalPPS, nalSPS, nalIDR}, {nalSPS, nalPPS}, {nalSPS, nalPPS, nalIDR, 6},
		{nalSlice}} {
		if isKeyframeAU(bad) {
			t.Errorf("%v is no keyframe", bad)
		}
	}
	if len(nalUnits(nil)) != 0 || len(nalUnits([]byte{1, 2, 3})) != 0 {
		t.Error("no start code, no unit")
	}
}

func TestOggCRC(t *testing.T) {
	// CRC-32 with polynomial 0x04C11DB7, no reflection, init 0, no final XOR: CRC-32/CKSUM's check value without its
	// final XOR (0x765E7680 ^ 0xFFFFFFFF).
	if got := oggCRC([]byte("123456789")); got != 0x89A1897F {
		t.Errorf("oggCRC = %#x", got)
	}
}

func TestOggOpus(t *testing.T) {
	var buf bytes.Buffer
	w := newOggOpus(&buf, 2)
	for i := range 120 {
		if err := w.WritePacket(bytes.Repeat([]byte{byte(i)}, 300+i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	var pages, packets int
	var lastGranule int64
	var granules []int64
	for seq := uint32(0); len(b) > 0; seq++ {
		if len(b) < 27 || string(b[:4]) != "OggS" {
			t.Fatalf("page %d: no capture pattern", seq)
		}
		nseg := int(b[26])
		size := 27 + nseg
		for _, l := range b[27 : 27+nseg] {
			size += int(l)
			if l < 255 {
				packets++
			}
		}
		page := append([]byte(nil), b[:size]...)
		crc := binary.LittleEndian.Uint32(page[22:])
		binary.LittleEndian.PutUint32(page[22:], 0)
		if oggCRC(page) != crc {
			t.Errorf("page %d: CRC mismatch", seq)
		}
		if got := binary.LittleEndian.Uint32(page[18:]); got != seq {
			t.Errorf("page %d: sequence %d", seq, got)
		}
		switch {
		case seq == 0 && (page[5] != oggHeaderTypeBOS || string(page[28:36]) != "OpusHead"):
			t.Errorf("page 0: %x %q", page[5], page[28:36])
		case seq == 1 && string(page[28:36]) != "OpusTags":
			t.Errorf("page 1: %q", page[28:36])
		}
		lastGranule = int64(binary.LittleEndian.Uint64(page[6:])) //nolint:gosec // G115: small granules
		granules = append(granules, lastGranule)
		b = b[size:]
		pages++
		if b2 := len(b); b2 == 0 && page[5] != oggHeaderTypeEOS {
			t.Error("the last page has no EOS flag")
		}
	}
	if packets != 2+120 {
		t.Errorf("%d packets in %d pages, want 122", packets, pages)
	}
	// RFC 7845 §4: the granule counts decoded samples including the pre-skip; the EOS page's is 960 × packets.
	if want := int64(120 * oggPacketSamples); lastGranule != want {
		t.Errorf("EOS granule %d, want %d (960 × 120 packets)", lastGranule, want)
	}
	if want := []int64{0, 0, 50 * oggPacketSamples, 100 * oggPacketSamples, 120 * oggPacketSamples}; !slices.Equal(
		granules, want) {
		t.Errorf("granules %v, want %v", granules, want)
	}
}

func TestPercentile(t *testing.T) {
	var d []time.Duration
	for i := 1; i <= 100; i++ {
		d = append(d, time.Duration(i)*time.Millisecond)
	}
	if percentile(d, 0.5) != 50*time.Millisecond || percentile(d, 0.99) != 99*time.Millisecond ||
		percentile(nil, 0.5) != 0 {
		t.Errorf("percentiles %v %v", percentile(d, 0.5), percentile(d, 0.99))
	}
}
