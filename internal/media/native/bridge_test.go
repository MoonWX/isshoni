package native

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/MoonWX/isshoni/internal/media"
)

// bridge_test drives the session half of the binding through fakeCaller (04 §18.3, README WA23): media.Config →
// im_start_config for every row of 04 §12.2, the status and token mapping of a failed start, the one-session rule,
// EventLayer synthesis, the event table, ENDED, the packet copy and its limits, PCMTap, Stop and its races, the
// controls, Reconfigure's order, Stats, the probe, sources and picks, and Close stopping the sessions. The DLL's own
// behavior is session_fake_test's (native/windows/tests) and the Windows DLL tests' (session_windows_test.go).

// sessionFake scripts the session exports of a fakeCaller, reading and writing the binding's memory as the DLL does:
// im_start records the config it was given (deep, during the call) and answers with info; im_next_packet hands out
// the scripted results in order (with nothing scripted it waits its timeout and answers IM_TIMEOUT), the payload in
// memory that it overwrites at the next call, so a binding that kept a slice into it would see the damage; the
// controls are recorded by fakeCaller.
type sessionFake struct {
	f *fakeCaller

	mu       sync.Mutex
	startSt  int32
	startErr string
	info     imSessionInfo // what im_start and im_get_session_info answer
	handle   uint64
	starts   []startRecord
	script   []nextItem
	cur      []byte // the payload handed out last
	stopSt   int32
	stats    string
	caps     imCaps
	sources  string
	pick     imSourceInfo
	pickSt   int32
	pickGate chan struct{} // closed: the pick returns
	canceled bool
	nextHook func() // runs inside im_next_packet, before it answers
	pulls    int
}

// nextItem is one scripted im_next_packet result. ts is relative to the session's epoch.
type nextItem struct {
	st      int32
	lastErr string
	kind    uint32
	layer   uint32
	flags   uint32
	ts      int64
	size    int // > len(data): a size field larger than the memory behind it (never read when over the limit)
	data    []byte
	event   int32
	status  int32
}

type startRecord struct {
	cfg       imStartConfig // a copy; its pointers are the binding's and valid only during the call
	source    string
	hasSource bool
	ex        *exRecord
}

type exRecord struct {
	flags, sharedPID uint32
	rules            []ruleRecord
}

type ruleRecord struct {
	kind, source uint32
	value        string
	appID, name  *string
}

func newSessionFake(t *testing.T) *sessionFake {
	f := newFakeCaller(t)
	s := &sessionFake{f: f, handle: 0x0000000300000007, pickGate: make(chan struct{})}
	close(s.pickGate)
	s.info = twoLayerInfo(f.qpcNS())
	f.set(procStart, s.start)
	f.set(procGetSessionInfo, func(args []uintptr) (uintptr, string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if args[0] != uintptr(s.handle) {
			return fakeResult(imErrInvalidArg), "im_get_session_info: no session"
		}
		writeStruct(args[1], &s.info)
		return fakeResult(imOK), ""
	})
	f.set(procNextPacket, s.next)
	f.set(procStop, func([]uintptr) (uintptr, string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return fakeResult(s.stopSt), "im_stop: refused"
	})
	for _, p := range []proc{procRequestKeyframe, procSetBitrate, procSetLayerActive, procUpdateLayer, procSetProfile,
		procSetContentHint, procSetPaused, procSetAudioBitrate} {
		f.set(p, func([]uintptr) (uintptr, string) { return fakeResult(imOK), "" })
	}
	f.set(procGetStats, func(args []uintptr) (uintptr, string) {
		s.mu.Lock()
		doc := s.stats
		s.mu.Unlock()
		return fakeSnprintf(args[1], args[2], doc), ""
	})
	f.set(procProbe, func(args []uintptr) (uintptr, string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		writeStruct(args[1], &s.caps)
		return fakeResult(imOK), ""
	})
	f.set(procListSources, func(args []uintptr) (uintptr, string) {
		s.mu.Lock()
		doc := s.sources
		s.mu.Unlock()
		return fakeSnprintf(args[1], args[2], doc), ""
	})
	f.set(procPickSource, func(args []uintptr) (uintptr, string) {
		s.mu.Lock()
		gate := s.pickGate
		s.mu.Unlock()
		<-gate
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.canceled {
			return fakeResult(imErrCancelled), "im_pick_source: cancelled"
		}
		if s.pickSt != imOK {
			return fakeResult(s.pickSt), "capture_denied: the privacy setting"
		}
		writeStruct(args[1], &s.pick)
		return fakeResult(imOK), ""
	})
	f.set(procCancelPick, func([]uintptr) (uintptr, string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		select {
		case <-s.pickGate:
		default:
			s.canceled = true
			close(s.pickGate)
		}
		return fakeResult(imOK), ""
	})
	return s
}

// sessionCalls is the number of DLL calls so far other than the log drain's, which runs on its own goroutine (an
// error kicks it at any time).
func sessionCalls(f *fakeCaller) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.proc != procReadLogs {
			n++
		}
	}
	return n
}

// writeStruct copies v to the binding's struct at a, up to the caller's struct_size (04 §5.1).
func writeStruct[T any](a uintptr, v *T) {
	dst := fakePtr[T](a)
	*dst = *v
}

// twoLayerInfo is a session of f 1920×1080@60 and q 640×360@15 on IM_ENC_FAKE, High, with audio.
func twoLayerInfo(epoch int64) imSessionInfo {
	info := imSessionInfo{structSize: uint32(unsafe.Sizeof(imSessionInfo{})), flags: imSessNoOSIndicator | imSessAudio,
		capturePath: imCaptureTest, layerCount: 2, epochNS: epoch, audioState: imAudioStateOK}
	info.layers[0] = imLayerInfo{encoder: imEncFake, width: 1920, height: 1080, fps: 60, bitrateBPS: 4e6,
		profile: imProfileHigh, flags: imEncfAvailable | imEncfLiveBitrate}
	copy(info.layers[0].rid[:], "f")
	info.layers[1] = imLayerInfo{encoder: imEncFake, width: 640, height: 360, fps: 15, bitrateBPS: 3e5,
		profile: imProfileHigh, flags: imEncfAvailable | imEncfLiveBitrate}
	copy(info.layers[1].rid[:], "q")
	return info
}

func (s *sessionFake) start(args []uintptr) (uintptr, string) {
	c := fakePtr[imStartConfig](args[0])
	rec := startRecord{cfg: *c}
	if c.sourceID != nil {
		rec.hasSource = true
		rec.source = fakeCString(uintptr(unsafe.Pointer(c.sourceID)))
	}
	if c.exclusions != nil {
		ex := &exRecord{flags: c.exclusions.flags, sharedPID: c.exclusions.sharedPID}
		if c.exclusions.ruleCount > 0 {
			rules := unsafe.Slice(c.exclusions.rules, c.exclusions.ruleCount)
			for _, r := range rules {
				rr := ruleRecord{kind: r.kind, source: r.source, value: fakeCString(uintptr(unsafe.Pointer(r.value)))}
				if r.appID != nil {
					v := fakeCString(uintptr(unsafe.Pointer(r.appID)))
					rr.appID = &v
				}
				if r.name != nil {
					v := fakeCString(uintptr(unsafe.Pointer(r.name)))
					rr.name = &v
				}
				ex.rules = append(ex.rules, rr)
			}
		}
		rec.ex = ex
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.starts = append(s.starts, rec)
	if s.startSt != imOK {
		return fakeResult(s.startSt), s.startErr
	}
	*fakePtr[uint64](args[1]) = s.handle
	writeStruct(args[2], &s.info)
	return fakeResult(imOK), ""
}

func (s *sessionFake) next(args []uintptr) (uintptr, string) {
	s.mu.Lock()
	for i := range s.cur {
		s.cur[i] = 0xEE // the slot handed out last is gone: a binding that kept it sees garbage
	}
	s.cur = nil
	s.pulls++
	hook := s.nextHook
	if args[0] != uintptr(s.handle) {
		s.mu.Unlock()
		return fakeResult(imErrInvalidArg), "im_next_packet: no session"
	}
	if len(s.script) == 0 {
		s.mu.Unlock()
		if hook != nil {
			hook()
		}
		time.Sleep(time.Duration(min(args[1], 50)) * time.Millisecond)
		return fakeResult(imTimeout), ""
	}
	it := s.script[0]
	s.script = s.script[1:]
	epoch := s.info.epochNS
	if it.st == imOK {
		p := fakePtr[imPacket](args[2])
		p.kind, p.layer, p.flags, p.captureTSNS = it.kind, it.layer, it.flags, epoch+it.ts
		p.event, p.status = it.event, it.status
		p.size = uint32(max(it.size, len(it.data)))
		if len(it.data) > 0 {
			s.cur = slices.Clone(it.data)
			p.data = uintptr(unsafe.Pointer(&s.cur[0]))
		}
		switch it.kind {
		case imPacketAudio:
			p.durationUS = 20000
		case imPacketPCM:
			p.durationUS = 10000
		}
	}
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	return fakeResult(it.st), it.lastErr
}

func (s *sessionFake) push(items ...nextItem) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range items {
		if it.st == 0 && it.kind == 0 && it.lastErr == "" {
			it.st = imTimeout
		}
		s.script = append(s.script, it)
	}
}

func (s *sessionFake) lastStart(t *testing.T) startRecord {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.starts) == 0 {
		t.Fatal("no im_start call")
	}
	return s.starts[len(s.starts)-1]
}

func video(layer uint32, flags uint32, ts int64, data ...byte) nextItem {
	if len(data) == 0 {
		data = []byte{0, 0, 0, 1, 0x65, 0x88}
	}
	return nextItem{st: imOK, kind: imPacketVideo, layer: layer, flags: flags, ts: ts, data: data}
}

func eventItem(ev int32, status int32, layer uint32, json string) nextItem {
	return nextItem{st: imOK, kind: imPacketEvent, layer: layer, event: ev, status: status, ts: 1, data: []byte(json)}
}

// baseConfig is a share's config: two layers and the default audio.
func baseConfig() media.Config {
	return media.Config{
		Source: "pick:7", Profile: "6400", Preset: "auto", Borderless: true,
		Layers: []media.LayerConfig{
			{RID: "f", MaxPixels: 1920 * 1080, MaxFPS: 60, Bitrate: 4_000_000},
			{RID: "q", MaxPixels: 640 * 360, MaxFPS: 15, Bitrate: 300_000},
		},
		Audio: &media.AudioConfig{},
	}
}

// startFake opens an engine on sf's caller and starts cfg; the session is stopped at the end of the test.
func startFake(t *testing.T, sf *sessionFake, cfg media.Config) (*Engine, *session) {
	t.Helper()
	e := openFake(t, sf.f, Options{})
	ms, err := e.Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	s := ms.(*session)
	t.Cleanup(func() { _ = s.Stop() })
	return e, s
}

// nextN returns the next n results of Next (each within 2 s).
func nextN(t *testing.T, s *session, n int) []media.Packet {
	t.Helper()
	var out []media.Packet
	for range n {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		p, err := s.Next(ctx)
		cancel()
		if err != nil {
			t.Fatalf("Next after %d packets: %v", len(out), err)
		}
		out = append(out, p)
	}
	return out
}

func skipStartEvents(t *testing.T, s *session) {
	t.Helper()
	for i, p := range nextN(t, s, 2) {
		if p.Kind != media.KindEvent || p.Event.Code != media.EventLayer {
			t.Fatalf("packet %d after Start: %+v, want an EventLayer", i, p)
		}
	}
}

// ---------------------------------------------------------------------------------------------- the config

func TestStartConfigMapping(t *testing.T) {
	sf := newSessionFake(t)
	e := openFake(t, sf.f, Options{})
	app, name := "voice:discord", "Discord"
	cfg := media.Config{
		Source: "pick:7", Listed: true, Encoder: "nvenc", Profile: "640c", Preset: "movie",
		Layers: []media.LayerConfig{
			{RID: "f", MaxPixels: 1920 * 1080, MaxFPS: 60, Bitrate: 4_000_000},
			{RID: "q", MaxPixels: 640 * 360, MaxFPS: 15, Bitrate: 300_000, Inactive: true},
		},
		Audio: &media.AudioConfig{
			BitrateBPS: 256000, TapPCM: true, TestFlags: imAudiofTestFailProbe,
			Exclusions: media.Exclusions{KeepOutMicUsers: true, FastPath: true, SharedPID: 42, Rules: []media.Rule{
				{Kind: media.RuleAppExclude, Source: media.SourceBuiltin, Value: "discord.exe", AppID: app, Name: name},
				{Kind: media.RuleInstance, Source: media.SourceSelf, Value: "1234"},
			}},
		},
	}
	s, err := e.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := sf.lastStart(t)
	c := r.cfg
	if c.structSize != uint32(unsafe.Sizeof(imStartConfig{})) || !r.hasSource || r.source != "pick:7" {
		t.Errorf("struct_size %d, source %q (%v)", c.structSize, r.source, r.hasSource)
	}
	if c.flags != imStartAllowListedSource|imStartKeepBorder {
		t.Errorf("flags %#x: ALLOW_LISTED and KEEP_BORDER, never HIDE_CURSOR", c.flags)
	}
	if c.profile != imProfileHigh || c.contentHint != imContentMotion || c.encoder != imEncNVENC || c.layerCount != 2 {
		t.Errorf("profile %#x, hint %d, encoder %d, layers %d", c.profile, c.contentHint, c.encoder, c.layerCount)
	}
	f, q := c.layers[0], c.layers[1]
	if cBytes(f.rid[:]) != "f" || f.maxPixels != 1920*1080 || f.fps != 60 || f.bitrateBPS != 4_000_000 ||
		f.gopMS != 0 || f.flags != 0 || f.width != 0 || f.height != 0 {
		t.Errorf("layer f %+v", f)
	}
	if cBytes(q.rid[:]) != "q" || q.maxPixels != 640*360 || q.fps != 15 || q.bitrateBPS != 300_000 ||
		q.flags != imLayerInactive {
		t.Errorf("layer q %+v", q)
	}
	if c.audio.mode != imAudioModeExcludeApps || c.audio.bitrateBPS != 256000 || c.audio.frameMS != 20 ||
		c.audio.flags != imAudiofTapPCM|imAudiofTestFailProbe {
		t.Errorf("audio %+v", c.audio)
	}
	if r.ex == nil || r.ex.flags != imExclfKeepOutMicUsers|imExclfFastPath || r.ex.sharedPID != 42 ||
		len(r.ex.rules) != 2 {
		t.Fatalf("exclusions %+v", r.ex)
	}
	r0, r1 := r.ex.rules[0], r.ex.rules[1]
	if r0.kind != imRuleAppExclude || r0.source != imRuleSrcBuiltin || r0.value != "discord.exe" || r0.appID == nil ||
		*r0.appID != app || r0.name == nil || *r0.name != name {
		t.Errorf("rule 0 %+v", r0)
	}
	if r1.kind != imRuleInstance || r1.source != imRuleSrcSelf || r1.value != "1234" || r1.appID != nil ||
		r1.name != nil {
		t.Errorf("rule 1 %+v: empty AppID and Name are NULL", r1)
	}
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}

	// The other rows: Borderless, the profile keys, the presets, the encoders, no audio, the tone, audio only.
	for _, tc := range []struct {
		name  string
		edit  func(*media.Config)
		check func(t *testing.T, r startRecord)
	}{
		{"borderless, CB, text, auto encoder, no audio", func(c *media.Config) {
			c.Borderless, c.Profile, c.Preset, c.Encoder, c.Audio = true, "42e0", "text", "", nil
		}, func(t *testing.T, r startRecord) {
			if r.cfg.flags != 0 || r.cfg.profile != imProfileCB || r.cfg.contentHint != imContentDetail ||
				r.cfg.encoder != imEncAuto || r.cfg.audio.mode != imAudioModeOff || r.ex != nil {
				t.Errorf("%+v", r.cfg)
			}
		}},
		{"6400, game, fake", func(c *media.Config) { c.Profile, c.Preset, c.Encoder = "6400", "game", "fake" },
			func(t *testing.T, r startRecord) {
				if r.cfg.profile != imProfileHigh || r.cfg.contentHint != imContentMotion || r.cfg.encoder != imEncFake {
					t.Errorf("%+v", r.cfg)
				}
			}},
		{"empty preset is auto", func(c *media.Config) { c.Preset = "" }, func(t *testing.T, r startRecord) {
			if r.cfg.contentHint != imContentAuto {
				t.Errorf("hint %d", r.cfg.contentHint)
			}
		}},
		{"the test tone takes no exclusions", func(c *media.Config) {
			c.Audio = &media.AudioConfig{Mode: media.AudioTestTone, Exclusions: media.Exclusions{FastPath: true}}
		}, func(t *testing.T, r startRecord) {
			if r.cfg.audio.mode != imAudioModeTestTone || r.ex != nil || r.cfg.audio.bitrateBPS != 0 {
				t.Errorf("audio %+v, exclusions %+v", r.cfg.audio, r.ex)
			}
		}},
		{"audio only", func(c *media.Config) {
			c.Source, c.Layers, c.Profile = "", nil, ""
			c.Audio = &media.AudioConfig{Mode: media.AudioTestTone, TapPCM: true}
		}, func(t *testing.T, r startRecord) {
			if r.hasSource || r.cfg.layerCount != 0 || r.cfg.audio.mode != imAudioModeTestTone {
				t.Errorf("%+v", r.cfg)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := baseConfig()
			tc.edit(&c)
			s, err := e.Start(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, sf.lastStart(t))
			if err := s.Stop(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStartRefusesWithoutACall(t *testing.T) {
	sf := newSessionFake(t)
	e := openFake(t, sf.f, Options{})
	rules := make([]media.Rule, imMaxRules+1)
	for i := range rules {
		rules[i] = media.Rule{Value: "a.exe"}
	}
	for name, edit := range map[string]func(*media.Config){
		"audio everything":     func(c *media.Config) { c.Audio.Mode = media.AudioEverything },
		"audio off":            func(c *media.Config) { c.Audio.Mode = media.AudioOff },
		"unknown audio mode":   func(c *media.Config) { c.Audio.Mode = "loud" },
		"audio bitrate low":    func(c *media.Config) { c.Audio.BitrateBPS = 31999 },
		"audio bitrate high":   func(c *media.Config) { c.Audio.BitrateBPS = 510001 },
		"unknown test flag":    func(c *media.Config) { c.Audio.TestFlags = 1 },
		"too many rules":       func(c *media.Config) { c.Audio.Exclusions.Rules = rules },
		"rule with a NUL":      func(c *media.Config) { c.Audio.Exclusions.Rules = []media.Rule{{Value: "a\x00b"}} },
		"unknown encoder":      func(c *media.Config) { c.Encoder = "x264" },
		"unknown profile":      func(c *media.Config) { c.Profile = "4d00" },
		"no profile":           func(c *media.Config) { c.Profile = "" },
		"unknown preset":       func(c *media.Config) { c.Preset = "fast" },
		"four layers":          func(c *media.Config) { c.Layers = append(c.Layers, c.Layers[0], c.Layers[1]) },
		"empty rid":            func(c *media.Config) { c.Layers[0].RID = "" },
		"long rid":             func(c *media.Config) { c.Layers[0].RID = "ffff" },
		"repeated rid":         func(c *media.Config) { c.Layers[1].RID = "f" },
		"fps 0":                func(c *media.Config) { c.Layers[0].MaxFPS = 0 },
		"fps 241":              func(c *media.Config) { c.Layers[1].MaxFPS = 241 },
		"negative pixels":      func(c *media.Config) { c.Layers[0].MaxPixels = -1 },
		"f bitrate low":        func(c *media.Config) { c.Layers[0].Bitrate = 99_999 },
		"q bitrate high":       func(c *media.Config) { c.Layers[1].Bitrate = 2_000_001 },
		"no source with video": func(c *media.Config) { c.Source = "" },
		"no source, no audio":  func(c *media.Config) { c.Source, c.Layers, c.Audio = "", nil, nil },
		"a source, no layers":  func(c *media.Config) { c.Layers = nil },
		"source with a NUL":    func(c *media.Config) { c.Source = "pick:\x007" },
		"source too long":      func(c *media.Config) { c.Source = "pick:" + strings.Repeat("7", 60) },
	} {
		t.Run(name, func(t *testing.T) {
			c := baseConfig()
			edit(&c)
			calls := sf.f.count(procStart)
			_, err := e.Start(context.Background(), c)
			wantError(t, err, media.CodeInvalid, "", "start")
			if sf.f.count(procStart) != calls {
				t.Error("im_start was called")
			}
		})
	}
	_, err := e.StartExtra(context.Background(), baseConfig())
	wantError(t, err, media.CodeInvalid, "", "start_extra")
}

func TestStartStatusErrors(t *testing.T) {
	for _, tc := range []struct {
		st       int32
		lastErr  string
		code     media.ErrorCode
		reason   string
		sentinel error
	}{
		{imErrDependency, "missing: avcodec-62.dll is not in its folder", media.CodeDependency, tokenMissing,
			media.ErrDependency},
		{imErrDependency, "blocked_by_policy: avcodec-62.dll: LoadLibraryExW failed (0x4EC)", media.CodeDependency,
			tokenBlockedByPolicy, media.ErrDependency},
		{imErrNoEncoder, "layer_failed: im_start: no encoder opened for layer q", media.CodeNoEncoder,
			tokenLayerFailed, media.ErrNoEncoder},
		{imErrNoEncoder, "forced_failed: im_start: no encoder", media.CodeNoEncoder, tokenForcedFailed,
			media.ErrNoEncoder},
		{imErrSourceGone, "pick_expired: im_start: the pick token was used", media.CodeSourceGone, tokenPickExpired,
			media.ErrSourceGone},
		{imErrLimit, "session_limit: im_start: the engine's 4 sessions are in use", media.CodeBusy, tokenSessionLimit,
			media.ErrBusy},
		{imErrPermissionDenied, "capture_denied: the privacy setting", media.CodePermissionDenied,
			tokenCaptureDenied, media.ErrPermissionDenied},
		{imErrUnsupportedOS, "os_too_old: build 19044", media.CodeUnsupportedOS, tokenOSTooOld,
			media.ErrUnsupportedOS},
		{imErrABI, "im_start: cfg->struct_size 200 is below v1.0", media.CodeDependency, tokenABI,
			media.ErrDependency},
		{imErrTimeout, "engine_timeout: im_start did not finish within 4000 ms", media.CodeTimeout,
			tokenEngineTimeout, context.DeadlineExceeded},
		{imErrInvalidArg, "im_start: a listed source needs IM_START_ALLOW_LISTED_SOURCE", media.CodeInvalid, "",
			media.ErrInvalid},
		{imErrNotImplemented, "im_start: monitor capture is not implemented yet", media.CodeInternal,
			reasonNotImplemented, nil},
		{imErrInternal, "im_start: unexpected exception: bad_alloc", media.CodeInternal, "", nil},
	} {
		t.Run(statusName(tc.st)+"/"+tc.reason, func(t *testing.T) {
			sf := newSessionFake(t)
			sf.startSt, sf.startErr = tc.st, tc.lastErr
			e := openFake(t, sf.f, Options{})
			_, err := e.Start(context.Background(), baseConfig())
			detail := wantError(t, err, tc.code, tc.reason, "start")
			if !strings.HasPrefix(detail, "im_start: ") {
				t.Errorf("detail %q names the export", detail)
			}
			if tc.sentinel != nil && !errors.Is(err, tc.sentinel) {
				t.Errorf("errors.Is(%v, %v) is false", err, tc.sentinel)
			}
			if sf.f.count(procStop) != 0 {
				t.Error("im_stop after a failed start")
			}
		})
	}
}

func TestStartOneSessionAtATime(t *testing.T) {
	sf := newSessionFake(t)
	e := openFake(t, sf.f, Options{})
	ctx := context.Background()
	s1, err := e.Start(ctx, baseConfig())
	if err != nil {
		t.Fatal(err)
	}
	calls := sf.f.count(procStart)
	_, err = e.Start(ctx, baseConfig())
	wantError(t, err, media.CodeBusy, "", "start")
	if !errors.Is(err, media.ErrBusy) || sf.f.count(procStart) != calls {
		t.Errorf("a second Start: %v, %d im_start calls", err, sf.f.count(procStart)-calls)
	}
	noAudio := baseConfig()
	noAudio.Audio = nil
	x1, err := e.StartExtra(ctx, noAudio)
	if err != nil {
		t.Fatalf("StartExtra while Start's session runs: %v", err)
	}
	x2, err := e.StartExtra(ctx, noAudio)
	if err != nil {
		t.Fatalf("a second StartExtra: %v", err)
	}
	if err := s1.Stop(); err != nil {
		t.Fatal(err)
	}
	s2, err := e.Start(ctx, baseConfig())
	if err != nil {
		t.Fatalf("Start after Stop: %v", err)
	}
	for _, s := range []media.Session{x1, x2, s2} {
		if err := s.Stop(); err != nil {
			t.Fatal(err)
		}
	}
	if n := sf.f.count(procStop); n != 4 {
		t.Errorf("%d im_stop calls for 4 sessions", n)
	}
}

func TestStartContext(t *testing.T) {
	sf := newSessionFake(t)
	e := openFake(t, sf.f, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Start(ctx, baseConfig()); !errors.Is(err, context.Canceled) {
		t.Errorf("Start with a done ctx: %v", err)
	}
	if sf.f.count(procStart) != 0 {
		t.Error("im_start was called")
	}
}

// ---------------------------------------------------------------------------------------------- Next and events

func TestEventLayerSynthesis(t *testing.T) {
	sf := newSessionFake(t)
	_, s := startFake(t, sf, baseConfig())
	got := nextN(t, s, 2)
	want := []media.Event{
		{Code: media.EventLayer, Layer: "f", Width: 1920, Height: 1080, FPS: 60, Encoder: "fake", Profile: "6400",
			LiveBitrate: true, NoOSIndicator: true},
		{Code: media.EventLayer, Layer: "q", Width: 640, Height: 360, FPS: 15, Encoder: "fake", Profile: "6400",
			LiveBitrate: true, NoOSIndicator: true},
	}
	for i, p := range got {
		if p.Kind != media.KindEvent || p.Data != nil || *p.Event != want[i] {
			t.Errorf("start event %d: %+v %+v, want %+v", i, p, p.Event, want[i])
		}
	}
	if n := sf.f.count(procNextPacket); n != 0 {
		t.Errorf("the start's EventLayers took %d im_next_packet calls", n)
	}

	// The layers' first packets are Reinit keyframes: no second EventLayer while nothing changed.
	sf.push(video(0, imPktKeyframe|imPktReinit, 10), video(1, imPktKeyframe|imPktReinit, 10))
	for i, p := range nextN(t, s, 2) {
		if p.Kind != media.KindVideo || !p.Reinit || !p.Keyframe || p.Layer != []string{"f", "q"}[i] {
			t.Errorf("first packet %d: %+v", i, p)
		}
	}

	// A later re-init: the EventLayer from a fresh session info, then the held packet.
	sf.mu.Lock()
	sf.info.layers[0].profile = imProfileCB
	sf.info.layers[0].encoder = imEncMFHW
	sf.info.layers[0].flags = imEncfAvailable | imEncfHardware
	sf.mu.Unlock()
	infos := sf.f.count(procGetSessionInfo)
	sf.push(video(0, imPktKeyframe|imPktReinit, 20))
	got = nextN(t, s, 2)
	if got[0].Kind != media.KindEvent || *got[0].Event != (media.Event{Code: media.EventLayer, Layer: "f", Width: 1920,
		Height: 1080, FPS: 60, Encoder: "mf-hw", HW: true, Profile: "42e0", NoOSIndicator: true}) {
		t.Errorf("before the re-init: %+v %+v", got[0], got[0].Event)
	}
	if got[1].Kind != media.KindVideo || !got[1].Reinit || got[1].CaptureNS != 20 {
		t.Errorf("the held packet: %+v", got[1])
	}
	if n := sf.f.count(procGetSessionInfo) - infos; n != 1 {
		t.Errorf("%d im_get_session_info calls for one re-init", n)
	}
	// A plain keyframe needs no EventLayer.
	sf.push(video(0, imPktKeyframe, 30))
	if p := nextN(t, s, 1)[0]; p.Kind != media.KindVideo || p.Reinit || !p.Keyframe {
		t.Errorf("%+v", p)
	}
}

func TestEventTable(t *testing.T) {
	sf := newSessionFake(t)
	_, s := startFake(t, sf, baseConfig())
	skipStartEvents(t, s)
	sf.push(
		eventItem(imEventSourceResized, imOK, imNoLayer, `{"width":1280,"height":720}`),
		eventItem(imEventLimitChanged, imOK, 0, `{"layer":"f","limit":"gpu","fps":31.5}`),
		eventItem(imEventLimitChanged, imOK, 0, `{"layer":"f","limit":"none","fps":59.9}`),
		eventItem(imEventSourceMinimized, imOK, imNoLayer, `{}`),
		eventItem(imEventSourceRestored, imOK, imNoLayer, `{}`),
		eventItem(imEventRingOverflow, imOK, 0, `{"stream":"f","dropped":12}`), // logged, not passed
		eventItem(imEventCaptureInterrupted, imOK, imNoLayer, `{"cause":"secure_desktop"}`),
		eventItem(imEventCaptureResumed, imOK, imNoLayer, `{"cause":"secure_desktop"}`),
		eventItem(40, imOK, imNoLayer, `{}`),  // the audio range: dropped
		eventItem(200, imOK, imNoLayer, `{}`), // unknown: skipped
		eventItem(imEventDeviceReset, imOK, imNoLayer, `{"reason":"0x887A0006","attempt":1}`),
		eventItem(imEventEncoderChanged, imErrEncoderLost, 0,
			`{"layer":"f","from":"nvenc","to":"mf-hw","codec":"h264_mf","reason":"open_failed"}`),
		video(0, imPktKeyframe|imPktReinit, 5),
	)
	sf.mu.Lock()
	sf.info.layers[0].encoder = imEncMFHW
	sf.mu.Unlock()
	want := []media.Event{
		{Code: media.EventSourceResized, Width: 1280, Height: 720},
		{Code: media.EventLimit, Layer: "f", Limit: "gpu"},
		{Code: media.EventLimit, Layer: "f", Limit: "none"},
		{Code: media.EventSourceMinimized},
		{Code: media.EventSourceRestored},
		{Code: media.EventCaptureInterrupted},
		{Code: media.EventCaptureResumed},
		{Code: media.EventDeviceReset},
		{Code: media.EventEncoderChanged, Layer: "f", Encoder: "mf-hw"},
		{Code: media.EventLayer, Layer: "f", Width: 1920, Height: 1080, FPS: 60, Encoder: "mf-hw", Profile: "6400",
			LiveBitrate: true, NoOSIndicator: true},
	}
	got := nextN(t, s, len(want)+1)
	for i, w := range want {
		if got[i].Kind != media.KindEvent || *got[i].Event != w {
			t.Errorf("event %d: %+v %+v, want %+v", i, got[i], got[i].Event, w)
		}
	}
	if p := got[len(want)]; p.Kind != media.KindVideo || !p.Reinit {
		t.Errorf("after the new encoder's EventLayer: %+v", p)
	}
}

func TestEndedEvent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		item   nextItem
		code   media.ErrorCode
		reason string
	}{
		{"encoder lost", eventItem(imEventEnded, imErrEncoderLost, imNoLayer,
			`{"reason":"encoder_lost","detail":"device_removed: the encoder's device was removed or reset"}`),
			media.CodeEncoderLost, tokenDeviceRemoved},
		{"source gone", eventItem(imEventEnded, imErrSourceGone, imNoLayer, `{"reason":"source_gone","detail":""}`),
			media.CodeSourceGone, ""},
		{"stopped", eventItem(imEventEnded, imOK, imNoLayer, `{"reason":"stopped"}`), media.CodeStopped, ""},
		{"IM_ENDED without the event", nextItem{st: imEnded}, media.CodeInternal, ""},
		{"a fatal status", nextItem{st: imErrBadState, lastErr: "not_initialized: im_next_packet: call im_init first"},
			media.CodeInternal, tokenNotInitialized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sf := newSessionFake(t)
			e, s := startFake(t, sf, baseConfig())
			skipStartEvents(t, s)
			sf.push(tc.item)
			_, err := s.Next(context.Background())
			wantError(t, err, tc.code, tc.reason, "next")
			if n := sf.f.count(procStop); n != 1 {
				t.Errorf("%d im_stop calls after the end, want exactly 1", n)
			}
			calls := sessionCalls(sf.f)
			if _, err := s.Next(context.Background()); !errors.Is(err, media.ErrStopped) {
				t.Errorf("Next after the end: %v", err)
			}
			if err := s.Stop(); err != nil {
				t.Errorf("Stop after the end: %v", err)
			}
			if sessionCalls(sf.f) != calls {
				t.Errorf("%d DLL calls after the end", sessionCalls(sf.f)-calls)
			}
			if s2, err := e.Start(context.Background(), baseConfig()); err != nil {
				t.Errorf("Start after the session's end: %v", err)
			} else {
				_ = s2.Stop()
			}
		})
	}
}

func TestPacketCopyAndLimits(t *testing.T) {
	sf := newSessionFake(t)
	_, s := startFake(t, sf, baseConfig())
	skipStartEvents(t, s)
	au := []byte{0, 0, 0, 1, 0x41, 1, 2, 3}
	opus := []byte{0xFC, 0xFF, 0xFE}
	sf.push(
		nextItem{st: imOK, kind: imPacketVideo, layer: 0, ts: 1, size: maxVideoBytes + 1}, // oversized
		nextItem{st: imOK, kind: 7, layer: 0, ts: 1, data: au},                            // unknown kind
		video(2, imPktKeyframe, 1, au...),                                                 // no layer 2
		video(1, 0, -5, au...),                                                            // before the epoch
		nextItem{st: imOK, kind: imPacketAudio, layer: imNoLayer, ts: 1, size: maxAudioBytes + 1},
		nextItem{st: imOK, kind: imPacketAudio, layer: imNoLayer, ts: -1, data: opus},
		nextItem{st: imOK, kind: imPacketPCM, layer: imNoLayer, ts: 1, data: make([]byte, 100)},
		nextItem{st: imOK, kind: imPacketEvent, event: imEventSourceMinimized, ts: 1, size: maxEventBytes + 1},
		nextItem{st: imOK, kind: imPacketDebug, ts: 1, data: au},
		video(0, imPktDiscontinuity|imPktRefresh, 100, au...),
		nextItem{st: imOK, kind: imPacketAudio, layer: imNoLayer, flags: imPktReinit | imPktDiscontinuity, ts: 120,
			data: opus},
	)
	got := nextN(t, s, 2)
	v, a := got[0], got[1]
	if v.Kind != media.KindVideo || v.Layer != "f" || v.Keyframe || v.Reinit || v.CaptureNS != 100 ||
		string(v.Data) != string(au) {
		t.Errorf("video %+v", v)
	}
	if a.Kind != media.KindAudio || a.Layer != "" || !a.Reinit || a.Keyframe || a.CaptureNS != 120 ||
		string(a.Data) != string(opus) {
		t.Errorf("audio %+v", a)
	}
	// Fresh memory: the fake overwrote its slot at the next call.
	sf.push(video(0, 0, 130, au...))
	nextN(t, s, 1)
	if string(v.Data) != string(au) || string(a.Data) != string(opus) {
		t.Error("Data aliases the engine's memory")
	}
	// The dropped video packets of a known layer asked for a keyframe: the oversized one and the early one.
	var kf []uintptr
	for _, args := range sf.f.callsOf(procRequestKeyframe) {
		kf = append(kf, args[1])
	}
	if !slices.Equal(kf, []uintptr{0, 1}) {
		t.Errorf("keyframe requests for layers %v, want [0 1]", kf)
	}
}

func TestPCMTap(t *testing.T) {
	sf := newSessionFake(t)
	cfg := baseConfig()
	cfg.Audio = &media.AudioConfig{Mode: media.AudioTestTone, TapPCM: true}
	_, s := startFake(t, sf, cfg)
	tap := s.PCMTap()
	if tap == nil {
		t.Fatal("no PCMTap with TapPCM")
	}
	skipStartEvents(t, s)
	chunk := make([]byte, pcmChunkBytes)
	for i := range 960 {
		binary.LittleEndian.PutUint32(chunk[4*i:], math.Float32bits(float32(i)/1000))
	}
	var items []nextItem
	for i := range pcmTapCapacity + 5 {
		items = append(items, nextItem{st: imOK, kind: imPacketPCM, layer: imNoLayer, ts: int64(10_000_000 * i),
			data: chunk})
	}
	items = append(items, video(0, imPktKeyframe, 5))
	sf.push(items...)
	p := nextN(t, s, 1)[0]
	if p.Kind != media.KindVideo {
		t.Fatalf("Next returned %+v: PCM never comes out of Next", p)
	}
	if len(tap) != pcmTapCapacity {
		t.Errorf("tap holds %d chunks, want %d (the rest dropped)", len(tap), pcmTapCapacity)
	}
	c := <-tap
	if c.CaptureNS != 0 || len(c.Frames) != 960 || c.Frames[500] != float32(500)/1000 {
		t.Errorf("chunk %d frames, CaptureNS %d, frame 500 = %v", len(c.Frames), c.CaptureNS, c.Frames[500])
	}
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
	for range tap { // drained, then closed
	}
	other := baseConfig()
	if _, s2 := startFake(t, newSessionFake(t), other); s2.PCMTap() != nil {
		t.Error("PCMTap without TapPCM")
	}
}

func TestStopUnblocksNext(t *testing.T) {
	sf := newSessionFake(t)
	_, s := startFake(t, sf, baseConfig())
	skipStartEvents(t, s)
	type res struct {
		err error
		at  time.Time
	}
	done := make(chan res, 1)
	go func() {
		_, err := s.Next(context.Background())
		done <- res{err, time.Now()}
	}()
	time.Sleep(120 * time.Millisecond) // blocked in its pulls
	stopped := time.Now()
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if !errors.Is(r.err, media.ErrStopped) {
		t.Errorf("a blocked Next after Stop: %v", r.err)
	}
	// Within one 50 ms pull (04 §12.3), plus scheduling slack for a loaded runner under -race; 01's bound is 200 ms.
	if d := r.at.Sub(stopped); d > 150*time.Millisecond {
		t.Errorf("Next returned %v after Stop, want within one 50 ms pull", d)
	}
	if err := s.Stop(); err != nil {
		t.Errorf("a second Stop: %v", err)
	}
	calls := sessionCalls(sf.f)
	if _, err := s.Next(context.Background()); !errors.Is(err, media.ErrStopped) {
		t.Errorf("Next after Stop: %v", err)
	}
	if err := s.SetPaused(true); !errors.Is(err, media.ErrStopped) {
		t.Errorf("SetPaused after Stop: %v", err)
	}
	if sessionCalls(sf.f) != calls {
		t.Errorf("%d DLL calls after Stop", sessionCalls(sf.f)-calls)
	}
	if n := sf.f.count(procStop); n != 1 {
		t.Errorf("%d im_stop calls", n)
	}
}

func TestStopRaceAndFailure(t *testing.T) {
	// IM_ERR_INVALID_ARG from im_next_packet while a Stop is under way is ErrStopped.
	sf := newSessionFake(t)
	_, s := startFake(t, sf, baseConfig())
	skipStartEvents(t, s)
	sf.mu.Lock()
	sf.nextHook = func() { s.stopping.Store(true) } // Stop's first step, while the call runs
	sf.mu.Unlock()
	sf.push(nextItem{st: imErrInvalidArg, lastErr: "im_next_packet: no session 0x0000000300000007"})
	if _, err := s.Next(context.Background()); !errors.Is(err, media.ErrStopped) {
		t.Errorf("INVALID_ARG during Stop: %v", err)
	}
	// im_stop's failure is Stop's error, once.
	sf2 := newSessionFake(t)
	sf2.stopSt = imErrTimeout
	_, s2 := startFake(t, sf2, baseConfig())
	err := s2.Stop()
	wantError(t, err, media.CodeTimeout, "", "stop")
	if err := s2.Stop(); err != nil {
		t.Errorf("a second Stop: %v", err)
	}
}

func TestNextContext(t *testing.T) {
	sf := newSessionFake(t)
	_, s := startFake(t, sf, baseConfig())
	skipStartEvents(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(80*time.Millisecond, cancel)
	start := time.Now()
	_, err := s.Next(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Next: %v", err)
	}
	if d := time.Since(start); d > 80*time.Millisecond+100*time.Millisecond {
		t.Errorf("Next returned %v after ctx's end, want within one pull", d-80*time.Millisecond)
	}
	// The session goes on.
	sf.push(video(0, 0, 9))
	if p := nextN(t, s, 1)[0]; p.Kind != media.KindVideo {
		t.Errorf("%+v", p)
	}
}

func TestSessionNowAfterStart(t *testing.T) {
	sf := newSessionFake(t)
	_, s := startFake(t, sf, baseConfig())
	before := sf.f.qpcNS() - sf.info.epochNS
	now := s.Now()
	if now < before || now > sf.f.qpcNS()-sf.info.epochNS {
		t.Errorf("Now() = %d, not qpcNS - epoch_ns", now)
	}
}

// ---------------------------------------------------------------------------------------------- controls

func TestControls(t *testing.T) {
	sf := newSessionFake(t)
	_, s := startFake(t, sf, baseConfig())
	h := uintptr(sf.handle)
	s.RequestKeyframe("q")
	s.RequestKeyframe("x") // unknown: ignored, no call
	if err := s.SetBitrate("f", 1_000_000); err != nil {
		t.Fatal(err)
	}
	if err := s.SetBitrate(media.LayerAudio, 256_000); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLayerActive("q", false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAudioBitrate(128_000); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		p    proc
		args []uintptr
	}{
		{procRequestKeyframe, []uintptr{h, 1}},
		{procSetBitrate, []uintptr{h, 0, 1_000_000}},
		{procSetAudioBitrate, []uintptr{h, 256_000}},
		{procSetLayerActive, []uintptr{h, 1, 0}},
		{procSetPaused, []uintptr{h, 1}},
		{procSetAudioBitrate, []uintptr{h, 128_000}},
	} {
		if !slices.ContainsFunc(sf.f.callsOf(c.p), func(a []uintptr) bool { return slices.Equal(a, c.args) }) {
			t.Errorf("no %s%v in %v", c.p, c.args, sf.f.callsOf(c.p))
		}
	}
	if n := len(sf.f.callsOf(procRequestKeyframe)); n != 1 {
		t.Errorf("%d keyframe requests", n)
	}
	calls := sessionCalls(sf.f)
	for name, err := range map[string]error{
		"bitrate 0 (a layer pauses with SetLayerActive)": s.SetBitrate("f", 0),
		"f below its range":       s.SetBitrate("f", 99_999),
		"q above its range":       s.SetBitrate("q", 2_000_001),
		"unknown layer":           s.SetBitrate("h", 1_000_000),
		"audio below its range":   s.SetBitrate(media.LayerAudio, 31_999),
		"unknown layer to pause":  s.SetLayerActive("x", true),
		"audio bitrate too large": s.SetAudioBitrate(510_001),
	} {
		var me *media.Error
		if !errors.As(err, &me) || me.Code != media.CodeInvalid {
			t.Errorf("%s: %v", name, err)
		}
	}
	if sessionCalls(sf.f) != calls {
		t.Errorf("refused controls made %d calls", sessionCalls(sf.f)-calls)
	}
	// A failure is mapped.
	sf.f.set(procSetPaused, fakeFails(imErrInvalidArg, "im_set_paused: no session"))
	wantError(t, s.SetPaused(false), media.CodeInvalid, "", "set_paused")

	// Without audio, the audio bitrate is invalid without a call.
	cfg := baseConfig()
	cfg.Audio = nil
	sf2 := newSessionFake(t)
	_, s2 := startFake(t, sf2, cfg)
	wantError(t, s2.SetBitrate(media.LayerAudio, 128_000), media.CodeInvalid, "", "set_bitrate")
	if sf2.f.count(procSetAudioBitrate) != 0 {
		t.Error("im_set_audio_bitrate without audio")
	}
}

func TestReconfigureOrder(t *testing.T) {
	sf := newSessionFake(t)
	_, s := startFake(t, sf, baseConfig())
	var mu sync.Mutex
	var seq []string
	var updated imLayerConfig
	record := func(name string) fakeProc {
		return func(args []uintptr) (uintptr, string) {
			mu.Lock()
			defer mu.Unlock()
			seq = append(seq, fmt.Sprintf("%s%v", name, args[1:]))
			return fakeResult(imOK), ""
		}
	}
	sf.f.set(procSetProfile, record("profile"))
	sf.f.set(procSetContentHint, record("hint"))
	sf.f.set(procSetBitrate, record("bitrate"))
	sf.f.set(procUpdateLayer, func(args []uintptr) (uintptr, string) {
		mu.Lock()
		defer mu.Unlock()
		updated = *fakePtr[imLayerConfig](args[2])
		seq = append(seq, fmt.Sprintf("update[%d]", args[1]))
		return fakeResult(imOK), ""
	})
	err := s.Reconfigure(media.Reconfig{Profile: "42e0", Preset: "text", Layers: []media.LayerConfig{
		{RID: "q", MaxFPS: 10, Bitrate: 200_000}, {RID: "f", Bitrate: 2_000_000}, {RID: "q"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{fmt.Sprintf("profile[%d]", imProfileCB), fmt.Sprintf("hint[%d]", imContentDetail), "update[1]",
		"bitrate[0 2000000]"}
	if !slices.Equal(seq, want) {
		t.Errorf("calls %v, want %v", seq, want)
	}
	if cBytes(updated.rid[:]) != "q" || updated.fps != 10 || updated.maxPixels != 640*360 ||
		updated.bitrateBPS != 200_000 || updated.gopMS != 0 {
		t.Errorf("im_update_layer got %+v: q's config with fps and bitrate replaced", updated)
	}

	// The first error ends it: an invalid field before any call, a failing call before the next one.
	seq = nil
	wantError(t, s.Reconfigure(media.Reconfig{Profile: "4d00", Preset: "text"}), media.CodeInvalid, "",
		"reconfigure")
	wantError(t, s.Reconfigure(media.Reconfig{Layers: []media.LayerConfig{{RID: "h", MaxFPS: 30}}}),
		media.CodeInvalid, "", "reconfigure")
	wantError(t, s.Reconfigure(media.Reconfig{Layers: []media.LayerConfig{{RID: "q", Bitrate: 5}}}),
		media.CodeInvalid, "", "reconfigure")
	if len(seq) != 0 {
		t.Errorf("refused reconfigures made calls %v", seq)
	}
	sf.f.set(procSetProfile, fakeFails(imErrInternal, "im_set_profile: queue"))
	wantError(t, s.Reconfigure(media.Reconfig{Profile: "6400", Preset: "game"}), media.CodeInternal, "",
		"reconfigure")
	if len(seq) != 0 {
		t.Errorf("calls after a failed im_set_profile: %v", seq)
	}
	// A later update starts from the updated config.
	if err := s.Reconfigure(media.Reconfig{Layers: []media.LayerConfig{{RID: "q", MaxPixels: 320 * 180}}}); err != nil {
		t.Fatal(err)
	}
	if updated.fps != 10 || updated.maxPixels != 320*180 || updated.bitrateBPS != 200_000 {
		t.Errorf("second update %+v", updated)
	}
}

const statsJSON = `{"layers":[{"rid":"f","encoder":"nvenc","codec":"h264_nvenc","hw":true,"width":1920,` +
	`"height":1080,"active":true,"fpsIn":60.0,"fpsOut":59.8,"bitrateBps":7900000,"targetBps":8000000,"keyframes":3,` +
	`"refreshes":0,"reinits":0,"drops":{"pacer":12,"ring":1,"stale":2},"encodeMs":{"p50":3.1,"p95":4.8,"p99":6.0},` +
	`"latencyMs":{"p50":9.0,"p95":14.0},"limit":"none"},{"rid":"q","encoder":"nvenc","codec":"h264_nvenc",` +
	`"hw":true,"width":640,"height":360,"active":true,"fpsIn":60.0,"fpsOut":15.0,"bitrateBps":290000,` +
	`"targetBps":300000,"keyframes":5,"refreshes":0,"reinits":0,"drops":{"pacer":0,"ring":0,"stale":0},` +
	`"encodeMs":{"p50":0.9,"p95":1.4,"p99":2.0},"latencyMs":{"p50":6.0,"p95":9.0},"limit":"cpu"}],` +
	`"capture":{"path":"wgc","sourceWidth":2560,"sourceHeight":1440,"frames":3600,"interrupted":false},` +
	`"audio":{"state":"ok","packets":3000,"drops":0,"bitrateBps":128000},"paused":false%s}`

func TestStats(t *testing.T) {
	sf := newSessionFake(t)
	sf.stats = fmt.Sprintf(statsJSON, "")
	_, s := startFake(t, sf, baseConfig())
	st, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	want := media.EngineStats{Layers: []media.EngineLayerStats{
		{RID: "f", Encoder: "nvenc", HW: true, FPSIn: 60, FPSOut: 59.8, BitrateBps: 7900000, TargetBps: 8000000,
			Limit: "none", EncodeP95: 4800 * time.Microsecond, LatencyP50: 9 * time.Millisecond,
			LatencyP95: 14 * time.Millisecond, Drops: 15},
		{RID: "q", Encoder: "nvenc", HW: true, FPSIn: 60, FPSOut: 15, BitrateBps: 290000, TargetBps: 300000,
			Limit: "cpu", EncodeP95: 1400 * time.Microsecond, LatencyP50: 6 * time.Millisecond,
			LatencyP95: 9 * time.Millisecond},
	}}
	if fmt.Sprint(st) != fmt.Sprint(want) {
		t.Errorf("Stats\n %+v\nwant\n %+v", st, want)
	}
	// The snprintf retry: a document over the first 64 KiB buffer.
	sf.mu.Lock()
	sf.stats = fmt.Sprintf(statsJSON, `,"pad":"`+strings.Repeat("x", jsonFirstCap)+`"`)
	sf.mu.Unlock()
	calls := sf.f.count(procGetStats)
	if _, err := s.Stats(); err != nil {
		t.Fatal(err)
	}
	if n := sf.f.count(procGetStats) - calls; n != 2 {
		t.Errorf("%d im_get_stats calls for a 64 KiB+ document, want 2", n)
	}
	sf.f.set(procGetStats, fakeFails(imErrInvalidArg, "im_get_stats: no session"))
	_, err = s.Stats()
	wantError(t, err, media.CodeInvalid, "", "stats")
}

// ---------------------------------------------------------------------------------------------- probe, sources, picks

func TestProbe(t *testing.T) {
	sf := newSessionFake(t)
	c := &sf.caps
	c.structSize = uint32(unsafe.Sizeof(imCaps{}))
	c.osBuild, c.osUBR = 26200, 6584
	c.osFlags = imOSSupported | imOSWGCPicker | imOSWGCBorderless | imOSWGCMinInterval | imOSExcludeFromCapture
	c.audioFlags = imAudioProcessLoopback | imAudioSessionEvents
	c.encoderCount = 3
	c.encoders[0] = imEncoderInfo{kind: imEncNVENC, adapterIndex: 0, flags: imEncfAvailable | imEncfHardware |
		imEncfLiveBitrate | imEncfZeroCopy | imEncfHigh | imEncfCB, status: imOK}
	c.encoders[1] = imEncoderInfo{kind: imEncMFSW, adapterIndex: 0xFFFFFFFF, status: imErrDependency}
	copy(c.encoders[1].reason[:], "no_media_foundation")
	c.encoders[2] = imEncoderInfo{kind: 50, status: imOK, flags: imEncfAvailable} // a later DLL's kind: left out
	e := openFake(t, sf.f, Options{})
	ctx := context.Background()
	p, err := e.Probe(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	want := media.Probe{
		OS:      media.OSInfo{Name: "windows", Version: "10.0.26200.6584", Build: 26200, Supported: true},
		Capture: media.CaptureCaps{Picker: true, Borderless: true, ExcludeFromCapture: true, MaxFPS: 240},
		Audio:   media.AudioCapsFromFlags(imAudioProcessLoopback | imAudioSessionEvents),
		Encoders: []media.EncoderInfo{
			{Name: "nvenc", Adapter: 0, HW: true, Tested: true, LiveBitrate: true, Profiles: []string{"6400", "42e0"},
				ZeroCopy: true},
			{Name: "mf-sw", Adapter: -1, Status: media.CodeDependency, Reason: "no_media_foundation"},
		},
	}
	if fmt.Sprintf("%+v", p) != fmt.Sprintf("%+v", want) {
		t.Errorf("Probe\n %+v\nwant\n %+v", p, want)
	}
	if args := sf.f.callsOf(procProbe); len(args) != 1 || args[0][0] != imProbeFull {
		t.Errorf("im_probe calls %v, want one IM_PROBE_FULL", args)
	}
	p.Encoders[0].Profiles[0] = "x" // the cache is the binding's
	if p2, _ := e.Probe(ctx, false); p2.Encoders[0].Profiles[0] != "6400" || sf.f.count(procProbe) != 1 {
		t.Error("the second Probe isn't the cached copy")
	}
	sf.caps.osFlags &^= imOSWGCMinInterval
	if p3, _ := e.Probe(ctx, true); p3.Capture.MaxFPS != 60 || sf.f.count(procProbe) != 2 {
		t.Errorf("refresh: MaxFPS %d, %d calls", p3.Capture.MaxFPS, sf.f.count(procProbe))
	}
	sf.f.set(procProbe, fakeFails(imErrTimeout, "engine_timeout: im_probe did not finish within 3000 ms"))
	_, err = e.Probe(ctx, true)
	wantError(t, err, media.CodeTimeout, tokenEngineTimeout, "probe")
}

func TestSources(t *testing.T) {
	sf := newSessionFake(t)
	sf.sources = `{"monitors":[{"id":"mon:\\\\.\\DISPLAY1","name":"DELL U2723QE","primary":true,"x":0,"y":0,` +
		`"width":3840,"height":2160,"dpi":144,"refreshHz":60,"rotation":0,"hdr":false,"adapter":0}],"windows":[]}`
	e := openFake(t, sf.f, Options{})
	got, err := e.Sources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []media.Source{{ID: `mon:\\.\DISPLAY1`, Kind: media.SourceScreen, Name: "DELL U2723QE", Width: 3840,
		Height: 2160, MonitorID: `mon:\\.\DISPLAY1`}}
	if !slices.Equal(got, want) {
		t.Errorf("Sources %+v", got)
	}
	if d, ok := e.SourceDetail(`mon:\\.\DISPLAY1`); !ok || d.MonitorID != `mon:\\.\DISPLAY1` {
		t.Errorf("SourceDetail %+v %v", d, ok)
	}
	if _, ok := e.SourceDetail("mon:other"); ok {
		t.Error("SourceDetail of an unknown id")
	}
	sf.f.set(procListSources, fakeFails(imErrNotImplemented, "im_list_sources: not implemented yet (README WA60)"))
	_, err = e.Sources(context.Background())
	wantError(t, err, media.CodeInternal, reasonNotImplemented, "sources")
}

func sourceInfo(id, name string, kind, path, flags, pid uint32) imSourceInfo {
	i := imSourceInfo{structSize: uint32(unsafe.Sizeof(imSourceInfo{})), kind: kind, width: 1280, height: 720,
		capturePath: path, flags: flags, pid: pid, window: 0x1234}
	copy(i.id[:], id)
	copy(i.name[:], name)
	return i
}

func TestPick(t *testing.T) {
	sf := newSessionFake(t)
	sf.pick = sourceInfo("pick:3", "Notes", imSourceWindow, imCaptureWGC, imSrcfOSBorder, 4321)
	e := openFake(t, sf.f, Options{})
	ctx := context.Background()
	src, err := e.Pick(ctx, media.PickRequest{OwnerWindow: 0xABC, Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if want := (media.Source{ID: "pick:3", Kind: media.SourceWindow, Name: "Notes", Width: 1280, Height: 720,
		PID: 4321, Exe: exeOfPID(4321), Capture: "wgc", OSBorder: true}); src != want {
		t.Errorf("Pick %+v, want %+v", src, want)
	}
	params := sf.f.callsOf(procPickSource)
	if len(params) != 1 {
		t.Fatalf("im_pick_source calls %d", len(params))
	}
	if d, ok := e.SourceDetail("pick:3"); !ok || d != (SourceDetail{CapturePath: "wgc", OSBorder: true,
		Window: 0x1234}) {
		t.Errorf("SourceDetail %+v %v", d, ok)
	}
	e.Release("pick:3")
	if _, ok := e.SourceDetail("pick:3"); ok {
		t.Error("SourceDetail after Release")
	}

	// A self pick is released and is invalid/self_window.
	sf.pick = sourceInfo("pick:4", "isshoni", imSourceWindow, imCaptureWGC, imSrcfSelf, 77)
	_, err = e.Pick(ctx, media.PickRequest{})
	wantError(t, err, media.CodeInvalid, tokenSelfWindow, "pick")
	sf.f.mu.Lock()
	released := slices.Clone(sf.f.released)
	sf.f.mu.Unlock()
	if !slices.Equal(released, []string{"pick:3", "pick:4"}) {
		t.Errorf("released %v", released)
	}
	if _, ok := e.SourceDetail("pick:4"); ok {
		t.Error("SourceDetail of a self pick")
	}

	// A monitor pick on Windows 10: DD, mapped.
	sf.pick = sourceInfo("pick:5", "DELL", imSourceMonitor, imCaptureDD, imSrcfMapped, 0)
	copy(sf.pick.monitorID[:], `mon:\\.\DISPLAY2`)
	if src, err := e.Pick(ctx, media.PickRequest{}); err != nil || src.Kind != media.SourceScreen ||
		src.Capture != "dd" || src.MonitorID != `mon:\\.\DISPLAY2` || src.Exe != "" {
		t.Errorf("monitor pick %+v, %v", src, err)
	}
	if d, _ := e.SourceDetail("pick:5"); !d.Mapped || d.CapturePath != "dd" {
		t.Errorf("SourceDetail %+v", d)
	}

	// The OS refuses.
	sf.pickSt = imErrPermissionDenied
	_, err = e.Pick(ctx, media.PickRequest{})
	wantError(t, err, media.CodePermissionDenied, tokenCaptureDenied, "pick")
	sf.pickSt = imOK

	// ctx ends a blocked pick through im_cancel_pick.
	sf.mu.Lock()
	sf.pickGate = make(chan struct{})
	sf.canceled = false
	sf.mu.Unlock()
	cctx, cancel := context.WithCancel(ctx)
	time.AfterFunc(50*time.Millisecond, cancel)
	cancels := sf.f.count(procCancelPick)
	_, err = e.Pick(cctx, media.PickRequest{})
	wantError(t, err, media.CodeCancelled, "", "pick")
	if sf.f.count(procCancelPick) != cancels+1 {
		t.Error("ctx's end did not call im_cancel_pick")
	}
	_, err = e.Pick(ctx, media.PickRequest{Timeout: maxPickTimeout + time.Second})
	wantError(t, err, media.CodeInvalid, "", "pick")
}

func TestCloseStopsSessions(t *testing.T) {
	sf := newSessionFake(t)
	e, err := openEngine(sf.f, Options{Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	ms, err := e.Start(context.Background(), baseConfig())
	if err != nil {
		t.Fatal(err)
	}
	s := ms.(*session)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	seq := sf.f.sequence()
	stop, shut := slices.Index(seq, procStop), slices.Index(seq, procShutdown)
	if stop < 0 || shut < 0 || stop > shut {
		t.Errorf("im_stop at %d, im_shutdown at %d: the sessions stop first", stop, shut)
	}
	if _, err := s.Next(context.Background()); !errors.Is(err, media.ErrStopped) {
		t.Errorf("Next after Close: %v", err)
	}
	if err := s.Stop(); err != nil {
		t.Errorf("Stop after Close: %v", err)
	}
	if n := sf.f.count(procStop); n != 1 {
		t.Errorf("%d im_stop calls", n)
	}
	if _, err := e.Start(context.Background(), baseConfig()); err == nil {
		t.Error("Start after Close")
	}
}
