package native

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"image"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
)

// lumaHeader is the engine's half of the luma tap (04 §5.8).
const lumaHeader = "../../../native/windows/src/session/luma_sample.h"

// TestLumaConstantsMatchTheEngine: luma.go's constants are luma_sample.h's, and the start flag is no bit the public
// header defines for im_start_config.flags.
func TestLumaConstantsMatchTheEngine(t *testing.T) {
	re := regexp.MustCompile(`(?m)^inline constexpr uint32_t (k\w+) = ([0-9A-Fa-fx']+)u?;`)
	got := map[string]int64{}
	for _, m := range re.FindAllStringSubmatch(readSource(t, lumaHeader), -1) {
		v, err := strconv.ParseInt(strings.ReplaceAll(m[2], "'", ""), 0, 64)
		if err != nil {
			t.Fatalf("%s: %v", m[1], err)
		}
		got[m[1]] = v
	}
	for name, want := range map[string]int64{
		"kStartFlag": startTestLumaTap, "kMagic": lumaMagic, "kVersion": lumaVersion,
		"kPayloadBytes": lumaPayloadBytes, "kMeanScale": lumaMeanScale,
	} {
		if v, ok := got[name]; !ok || v != want {
			t.Errorf("luma_sample.h's %s = %d (found %t), luma.go's %d", name, v, ok, want)
		}
	}
	for name, v := range abiConstants {
		if strings.HasPrefix(name, "IM_START_") && v&startTestLumaTap != 0 {
			t.Errorf("%s shares a bit with IM_START_TEST_LUMA_TAP", name)
		}
	}
}

// lumaGoldenHex is luma_sample_test.cpp's kGoldenHex: capture_ts_ns 0x0102030405060708, mean 32768, source
// 1920×1080, region (832, 412) 256×256, missed 3.
const lumaGoldenHex = "4c554d41" + "01000000" + "0807060504030201" + "00800000" + "80070000" + "38040000" +
	"40030000" + "9c010000" + "00010000" + "00010000" + "03000000"

func TestLumaPayloadGolden(t *testing.T) {
	b, err := hex.DecodeString(lumaGoldenHex)
	if err != nil {
		t.Fatal(err)
	}
	s, ts, ok := decodeLuma(b)
	if !ok {
		t.Fatal("the golden payload doesn't decode")
	}
	want := LumaSample{Mean: 32768.0 / 65535, Source: image.Pt(1920, 1080), Region: image.Rect(832, 412, 1088, 668),
		Missed: 3}
	if s != want || ts != 0x0102030405060708 {
		t.Errorf("decoded %+v at %#x, want %+v at 0x0102030405060708", s, ts, want)
	}
	if !strings.Contains(readSource(t, "../../../native/windows/tests/luma_sample_test.cpp"), "luma_test.go") {
		t.Error("luma_sample_test.cpp no longer names this test's twin")
	}
	for name, mutate := range map[string]func([]byte) []byte{
		"short":   func(b []byte) []byte { return b[:47] },
		"long":    func(b []byte) []byte { return append(b, 0) },
		"magic":   func(b []byte) []byte { b[0] = 'X'; return b },
		"version": func(b []byte) []byte { b[4] = 2; return b },
		"mean":    func(b []byte) []byte { binary.LittleEndian.PutUint32(b[16:], 65536); return b },
		"empty":   func([]byte) []byte { return nil },
	} {
		c := append([]byte(nil), b...)
		if _, _, ok := decodeLuma(mutate(c)); ok {
			t.Errorf("%s: decoded", name)
		}
	}
}

// lumaPayload is a reading of mean (0..65535) at the engine-clock time ts, over a 256×256 box of a 640×360 frame.
func lumaPayload(ts int64, mean uint32, missed uint32) []byte {
	b := make([]byte, lumaPayloadBytes)
	le := binary.LittleEndian
	le.PutUint32(b[0:], lumaMagic)
	le.PutUint32(b[4:], lumaVersion)
	le.PutUint64(b[8:], uint64(ts))
	le.PutUint32(b[16:], mean)
	le.PutUint32(b[20:], 640)
	le.PutUint32(b[24:], 360)
	le.PutUint32(b[28:], 192)
	le.PutUint32(b[32:], 52)
	le.PutUint32(b[36:], 256)
	le.PutUint32(b[40:], 256)
	le.PutUint32(b[44:], missed)
	return b
}

func TestLumaTapOf(t *testing.T) {
	if LumaTapOf() != nil || LumaTapOf(WithGOP("f", 2e9)) != nil {
		t.Error("no WithLumaTap: want nil")
	}
	var n int
	fn := func(LumaSample) { n++ }
	LumaTapOf(WithLumaTap(func(LumaSample) {}), nil, WithLumaTap(fn))(LumaSample{})
	if n != 1 {
		t.Error("the last WithLumaTap wins")
	}
	if LumaTapOf(WithLumaTap(fn), WithLumaTap(nil)) != nil {
		t.Error("WithLumaTap(nil) turns the tap off")
	}
}

// TestStartWithLumaTap: WithLumaTap sets IM_START_TEST_LUMA_TAP and Next hands the readings to its function, in order,
// on the session clock, and never returns them; malformed readings are dropped. Without it the flag stays clear and
// debug packets are dropped silently.
func TestStartWithLumaTap(t *testing.T) {
	sf := newSessionFake(t)
	e := openFake(t, sf.f, Options{})
	var mu sync.Mutex
	var got []LumaSample
	ms, err := e.StartWith(context.Background(), baseConfig(), WithLumaTap(func(s LumaSample) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, s)
	}))
	if err != nil {
		t.Fatal(err)
	}
	s := ms.(*session)
	t.Cleanup(func() { _ = s.Stop() })
	if f := sf.lastStart(t).cfg.flags; f&startTestLumaTap == 0 || f&imStartKeepBorder != 0 {
		t.Errorf("im_start flags %#x: want IM_START_TEST_LUMA_TAP (and the borderless default)", f)
	}
	skipStartEvents(t, s)
	epoch := sf.info.epochNS
	debug := func(ts int64, data []byte) nextItem {
		return nextItem{st: imOK, kind: imPacketDebug, layer: imNoLayer, ts: ts, data: data}
	}
	const ms5, ms21 = 5_000_000, 21_000_000
	short := debug(10_000_000, lumaPayload(epoch+10_000_000, 3000, 0)[:40])
	otherTime := debug(15_000_000, lumaPayload(epoch+16_000_000, 3000, 0))
	beforeEpoch := debug(-1, lumaPayload(epoch-1, 3000, 0))
	oversized := nextItem{st: imOK, kind: imPacketDebug, layer: imNoLayer, ts: 22_000_000, size: maxVideoBytes + 1}
	sf.push(
		debug(ms5, lumaPayload(epoch+ms5, 3000, 0)),
		short, otherTime, beforeEpoch,
		debug(ms21, lumaPayload(epoch+ms21, 65535, 2)), // white, two frames missed
		oversized,
		video(0, imPktKeyframe|imPktReinit, 30_000_000),
	)
	p := nextN(t, s, 1)[0]
	if p.Kind != media.KindVideo || p.CaptureNS != 30_000_000 {
		t.Fatalf("Next returned %+v, want the video packet after the readings", p)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []LumaSample{
		{CaptureNS: ms5, Mean: 3000.0 / 65535, Source: image.Pt(640, 360), Region: image.Rect(192, 52, 448, 308)},
		{CaptureNS: ms21, Mean: 1, Source: image.Pt(640, 360), Region: image.Rect(192, 52, 448, 308), Missed: 2},
	}
	if len(got) != len(want) {
		t.Fatalf("readings %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("reading %d: %+v, want %+v", i, got[i], want[i])
		}
	}
	if math.Abs(got[0].Mean-0.04578) > 1e-4 {
		t.Errorf("mean %v", got[0].Mean)
	}
	if n := sf.f.count(procRequestKeyframe); n != 0 {
		t.Errorf("%d keyframe requests for dropped debug packets", n)
	}

	// Without the option: no flag, and debug packets never reach anyone.
	sf2 := newSessionFake(t)
	_, s2 := startFake(t, sf2, baseConfig())
	if f := sf2.lastStart(t).cfg.flags; f&startTestLumaTap != 0 {
		t.Errorf("Start set IM_START_TEST_LUMA_TAP (%#x)", f)
	}
	skipStartEvents(t, s2)
	sf2.push(debug(ms5, lumaPayload(sf2.info.epochNS+ms5, 3000, 0)), video(0, imPktKeyframe|imPktReinit, ms21))
	if p := nextN(t, s2, 1)[0]; p.Kind != media.KindVideo {
		t.Errorf("without the tap: %+v, want the video packet after the dropped reading", p)
	}
}

// TestLumaTapNeedsTestHooks: a DLL without test hooks (every shipped one) has no tap, so the start is
// media.CodeDependency without im_start; StartExtra too.
func TestLumaTapNeedsTestHooks(t *testing.T) {
	sf := newSessionFake(t)
	sf.f.setBuildJSON(strings.Replace(fakeBuildJSON, `"testHooks":true`, `"testHooks":false`, 1))
	e := openFake(t, sf.f, Options{})
	ctx := context.Background()
	tap := WithLumaTap(func(LumaSample) {})
	_, err := e.StartWith(ctx, baseConfig(), tap)
	if d := wantError(t, err, media.CodeDependency, "", "start"); !strings.Contains(d, "IM_TEST_HOOKS") {
		t.Errorf("detail %q doesn't name the test hooks", d)
	}
	noAudio := baseConfig()
	noAudio.Audio = nil
	_, err = e.StartExtra(ctx, noAudio, tap)
	wantError(t, err, media.CodeDependency, "", "start_extra")
	if n := sf.f.count(procStart); n != 0 {
		t.Errorf("%d im_start calls", n)
	}
	// An invalid config is still CodeInvalid first, and a nil function asks for nothing.
	bad := baseConfig()
	bad.Profile = "x"
	_, err = e.StartWith(ctx, bad, tap)
	wantError(t, err, media.CodeInvalid, "", "start")
	s, err := e.StartWith(ctx, baseConfig(), WithLumaTap(nil))
	if err != nil {
		t.Fatal(err)
	}
	if f := sf.lastStart(t).cfg.flags; f&startTestLumaTap != 0 {
		t.Errorf("WithLumaTap(nil) set the flag (%#x)", f)
	}
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
}

// TestDLLLumaTap: the win-x64-ci DLL's tap through the binding (04 §5.8, §18.2's "Go on Windows"): test:pattern with
// IM_ENC_FAKE, whose session the tap puts on the GPU path (WARP on CI), hands readings of the centered 256-pixel box of
// its 1920×1080 frames to the function, in capture order and on the session clock, while Next returns only media.
func TestDLLLumaTap(t *testing.T) {
	e := sessionEngine(t)
	var mu sync.Mutex
	var got []LumaSample
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ms, err := e.StartWith(ctx, nativeConfig(), WithLumaTap(func(s LumaSample) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, s)
	}))
	if err != nil {
		t.Fatalf("StartWith: %v", err)
	}
	s := ms.(*session)
	t.Cleanup(func() { _ = s.Stop() })
	run, cancelRun := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelRun()
	video := 0
	for {
		p, err := s.Next(run)
		if err != nil {
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Next: %v", err)
			}
			break
		}
		if p.Kind == media.KindVideo {
			video++
		}
	}
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	// 2 s of a 60 Hz pattern: WARP may draw 1080p slower than that on a CI runner, never this slowly.
	if len(got) < 10 || video < 10 {
		t.Fatalf("%d readings and %d video packets in 2 s", len(got), video)
	}
	for i, r := range got {
		if r.Source != image.Pt(1920, 1080) || r.Region != image.Rect(832, 412, 1088, 668) || r.Mean < 0 ||
			r.Mean > 1 || r.CaptureNS < 0 || (i > 0 && r.CaptureNS <= got[i-1].CaptureNS) {
			t.Fatalf("reading %d: %+v (the previous at %d)", i, r, got[max(i-1, 0)].CaptureNS)
		}
	}
}
