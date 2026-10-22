package native

import (
	"context"
	"encoding/json"
	"math"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/MoonWX/isshoni/internal/media"
)

// This file is 05's (docs/m2/05-windows-audio.md §13.2, README WA35): media.AudioController on *Engine over the
// caller, the single mode mapping (05 §13.1) that both im_start's config (bridge.go's buildStart) and SetAudioMode go
// through, buildExclusions, level and report decoding, the audio status → error table (IM_ERR_BAD_STATE from an audio
// call is CodeAudioNotRunning) and the AudioTestHooks wrappers of the im_test_* exports. It has no build tag: all of
// it runs on the Mac and CI ubuntu against fakecaller_test.go's fake (audio_test.go). The C mirrors of the audio
// structs (imRule, imExclusions, imAudioLevel, imAudioLevels) are in abi.go, beside the other mirrors that
// layout_test.go checks (04 §12.1).
//
// Serialization (05 §13.2): the watcher's control calls (WatchAudio, UnwatchAudio, SetExclusions, SetAudioMode,
// SetAudioMuted) run one at a time under engineAudio.ctl, never under a session's pump; AudioReport (whose one-shot
// snapshot may take up to 3 s), AudioLevels (a lock-free read in the DLL), SoundCheck (up to timeout + 500 ms) and
// the test exports run outside it, which the DLL allows (05 §4.3: every audio export is thread-safe).
//
// Privacy (05 §16, README §3 item 18): nothing here logs a rule, an app, an exe, a pid or a level, and no error's
// Detail quotes a rule's value, app id or name; errors name the rule by its index.

// engineAudio is the Engine's audio state.
type engineAudio struct {
	ctl sync.Mutex // orders the watcher's control calls (05 §13.2)
}

// The buffers of the audio JSON exports (05 §13.2, 01 §7.3's helper): the report starts at 64 KiB like every JSON
// export and may grow to 4 MiB (the DLL caps it at 256 KiB by construction, 05 §12.1, §17).
const reportMaxCap = 4 << 20

// The limits of the test exports' arguments that Go checks before a call: im_audio_sound_check's timeout (05 §4.1:
// timeout_ms 1000..10000) and the PCM buffer of im_test_opus_decode (120 ms of stereo at 48 kHz: the longest Opus
// packet, 05 §13.2).
const (
	minSoundCheckTimeout = time.Second
	maxSoundCheckTimeout = 10 * time.Second
	opusMaxFrames        = 5760
)

// ------------------------------------------------------------------------------------------------- mode mapping

// audioModeUse is where a mode is mapped: media.Config.Audio.Mode at im_start, or SetAudioMode on a live share.
type audioModeUse uint8

const (
	modeAtStart audioModeUse = iota // im_audio_config.mode (04 §12.2's media.Config → im_start_config)
	modeLive                        // im_audio_set_mode
)

// audioModes is 05 §13.1's single mode mapping: each media.AudioMode, its IM_AUDIO_MODE_* value and where it is
// accepted. "" is AudioExcludeApps at start only; AudioTestTone exists only at start; AudioOff only live ("Share
// sound" off: a share without audio has Config.Audio nil). AudioEverything is reserved and refused by every layer in
// M2 (docs/m2/README.md §3 item 15: the DLL answers IM_ERR_INVALID_ARG too), so Go never passes it; anything else is
// no mode. A refused mode is CodeInvalid without a DLL call.
var audioModes = [...]struct {
	mode        media.AudioMode
	value       uint32
	start, live bool
	why         string // why it is refused where it isn't accepted
}{
	{"", imAudioModeExcludeApps, true, false, "an empty mode is a start's default only"},
	{media.AudioExcludeApps, imAudioModeExcludeApps, true, true, ""},
	{media.AudioTestTone, imAudioModeTestTone, true, false, "the test tone is a start mode only"},
	{media.AudioOff, imAudioModeOff, false, true,
		"off is a mid-share SetAudioMode value; a session without audio has Config.Audio nil"},
	{media.AudioEverything, imAudioModeEverything, false, false,
		"everything is reserved and refused in M2 (docs/m2/README.md §3 item 15)"},
}

// audioModeValue maps m for use (audioModes) or returns CodeInvalid with what as the field's name.
func audioModeValue(op, what string, m media.AudioMode, use audioModeUse) (uint32, error) {
	for _, am := range audioModes {
		if am.mode != m {
			continue
		}
		if (use == modeAtStart && am.start) || (use == modeLive && am.live) {
			return am.value, nil
		}
		return 0, codeError(media.CodeInvalid, op, "", "%s %q: %s", what, m, am.why)
	}
	return 0, codeError(media.CodeInvalid, op, "", "%s %q is not an audio mode", what, m)
}

// ------------------------------------------------------------------------------------------------- exclusions

// cExclusions is an im_exclusions in Go memory with its rule array and every string it points to (05 §13.2): the
// DLL copies all of it during the call (05 §4.3), so it is pinned for that one call only (pin).
type cExclusions struct {
	ex    imExclusions
	rules []imRule
	strs  []*byte
}

// buildExclusions is media.Exclusions → im_exclusions: the rules in order (an empty AppID or Name is NULL),
// KeepOutMicUsers → IM_EXCLF_KEEP_OUT_MIC_USERS, FastPath → IM_EXCLF_FAST_PATH, SharedPID. Go refuses, without a call,
// what a C string can't carry or the header's sizes exclude: more than IM_MAX_RULES rules, a NUL byte in a value, app
// id or name, a value of IM_MAX_RULE_VALUE bytes or more. The DLL checks the rest (05 §4.3). The Detail names the rule
// by its index, never its contents (rules are private, 05 §16).
func buildExclusions(op string, x media.Exclusions) (*cExclusions, error) {
	if len(x.Rules) > imMaxRules {
		return nil, codeError(media.CodeInvalid, op, "", "Exclusions has %d rules: at most %d", len(x.Rules),
			imMaxRules)
	}
	c := &cExclusions{ex: imExclusions{structSize: uint32(unsafe.Sizeof(imExclusions{})), sharedPID: x.SharedPID}}
	if x.KeepOutMicUsers {
		c.ex.flags |= imExclfKeepOutMicUsers
	}
	if x.FastPath {
		c.ex.flags |= imExclfFastPath
	}
	if len(x.Rules) == 0 {
		return c, nil
	}
	c.rules = make([]imRule, len(x.Rules))
	str := func(i int, field, s string) (*byte, error) {
		if strings.IndexByte(s, 0) >= 0 {
			return nil, codeError(media.CodeInvalid, op, "", "Exclusions.Rules[%d].%s holds a NUL byte", i, field)
		}
		b := make([]byte, len(s)+1)
		copy(b, s)
		c.strs = append(c.strs, &b[0])
		return &b[0], nil
	}
	for i, r := range x.Rules {
		if len(r.Value) >= imMaxRuleValue {
			return nil, codeError(media.CodeInvalid, op, "", "Exclusions.Rules[%d].Value is %d bytes: at most %d", i,
				len(r.Value), imMaxRuleValue-1)
		}
		v, err := str(i, "Value", r.Value)
		if err != nil {
			return nil, err
		}
		rule := imRule{kind: uint32(r.Kind), source: uint32(r.Source), value: v}
		if r.AppID != "" {
			if rule.appID, err = str(i, "AppID", r.AppID); err != nil {
				return nil, err
			}
		}
		if r.Name != "" {
			if rule.name, err = str(i, "Name", r.Name); err != nil {
				return nil, err
			}
		}
		c.rules[i] = rule
	}
	c.ex.rules = &c.rules[0]
	c.ex.ruleCount = uint32(len(c.rules))
	return c, nil
}

// pin pins the struct, the rule array and every string for one DLL call and returns the struct's address.
func (c *cExclusions) pin(pn *runtime.Pinner) uintptr {
	for _, p := range c.strs {
		pn.Pin(p)
	}
	if len(c.rules) > 0 {
		pn.Pin(&c.rules[0])
	}
	return addrOf(pn, &c.ex)
}

// buildAudioConfig is media.AudioConfig → im_audio_config for im_start (04 §12.2's table): the mode through the
// single mapping, bitrate_bps (0 = the engine's 128 000, else 32 000..510 000), frame_ms 20, the flags (TapPCM →
// IM_AUDIOF_TAP_PCM, TestFlags passed through: only IM_AUDIOF_TEST_FAIL_PROBE), and for EXCLUDE_APPS the exclusions
// (nil for the test tone, which captures nothing). Everything is checked without a call.
func buildAudioConfig(op string, ac *media.AudioConfig) (imAudioConfig, *cExclusions, error) {
	var out imAudioConfig
	mode, err := audioModeValue(op, "Config.Audio.Mode", ac.Mode, modeAtStart)
	if err != nil {
		return out, nil, err
	}
	if ac.BitrateBPS != 0 && (ac.BitrateBPS < minAudioBitrateBPS || ac.BitrateBPS > maxAudioBitrateBPS) {
		return out, nil, codeError(media.CodeInvalid, op, "", "Config.Audio.BitrateBPS %d is outside %d..%d",
			ac.BitrateBPS, minAudioBitrateBPS, maxAudioBitrateBPS)
	}
	if ac.TestFlags&^imAudiofTestFailProbe != 0 {
		return out, nil, codeError(media.CodeInvalid, op, "",
			"Config.Audio.TestFlags %#x: only IM_AUDIOF_TEST_FAIL_PROBE", ac.TestFlags)
	}
	out.mode = mode
	out.bitrateBPS = uint32(ac.BitrateBPS)
	out.frameMS = 20
	out.flags = ac.TestFlags
	if ac.TapPCM {
		out.flags |= imAudiofTapPCM
	}
	if mode != imAudioModeExcludeApps {
		return out, nil, nil
	}
	ex, err := buildExclusions(op, ac.Exclusions)
	if err != nil {
		return out, nil, err
	}
	return out, ex, nil
}

// ------------------------------------------------------------------------------------------------- errors

// audioStatusError is 05 §13.1's status → error table for an audio export: IM_ERR_BAD_STATE (neither watching nor
// live, no running audio session, or audio off after IM_INIT_NO_AUDIO) is CodeAudioNotRunning, so errors.Is(err,
// media.ErrAudioNotRunning) holds; every other status maps as from any export (statusError: IM_ERR_INVALID_ARG →
// CodeInvalid with im_last_error in the Detail, IM_ERR_TIMEOUT → CodeTimeout, IM_ERR_ABI → CodeDependency with Reason
// abi, …).
func audioStatusError(op, export string, st int32, lastErr string) *media.Error {
	err := statusError(op, export, st, lastErr)
	if st == imErrBadState {
		err.Code = media.CodeAudioNotRunning
	}
	return err
}

// ------------------------------------------------------------------------------------- media.AudioController

// WatchAudio starts the watcher without capture (im_audio_watch: the share sheet before going live) with ex's rules,
// which replace the previous ones. Idempotent.
func (e *Engine) WatchAudio(ctx context.Context, ex media.Exclusions) error {
	return e.exclusionsCall(ctx, "audio_watch", procAudioWatch, ex)
}

// UnwatchAudio stops the watcher (im_audio_unwatch); it keeps running while a share with audio runs (05 §4.1).
func (e *Engine) UnwatchAudio(ctx context.Context) error {
	return e.audioControl(ctx, "audio_unwatch", procAudioUnwatch)
}

// SetExclusions replaces the rules, flags and shared pid of the watcher and of a running share (im_set_exclusions,
// applied within 50 ms); without either, the DLL keeps them for the next WatchAudio or Start (05 §4.3).
func (e *Engine) SetExclusions(ctx context.Context, ex media.Exclusions) error {
	return e.exclusionsCall(ctx, "set_exclusions", procSetExclusions, ex)
}

// SetAudioMode switches a live share's sound off or on (im_audio_set_mode): AudioOff and AudioExcludeApps only; any
// other mode, AudioEverything and AudioTestTone included, is CodeInvalid without a call (README §3 item 15), also
// when no session runs. Without a running audio session it is media.ErrAudioNotRunning.
func (e *Engine) SetAudioMode(ctx context.Context, m media.AudioMode) error {
	const op = "audio_set_mode"
	v, err := audioModeValue(op, "mode", m, modeLive)
	if err != nil {
		return err
	}
	return e.audioControl(ctx, op, procAudioSetMode, uintptr(v))
}

// SetAudioMuted is the pause mute 01's core sets (im_audio_set_muted), never "Share sound" off (05 §4.3). Without a
// running audio session it is media.ErrAudioNotRunning.
func (e *Engine) SetAudioMuted(ctx context.Context, muted bool) error {
	return e.audioControl(ctx, "audio_set_muted", procAudioSetMuted, boolArg(muted))
}

// AudioReport is "What friends hear" (im_list_audio_apps, 05 §12.1), parsed by media.ParseAudioReport. While the
// watcher runs it is the latest report; otherwise the DLL takes a one-shot snapshot (≤ 500 ms; state "stopped"), and
// IM_ERR_TIMEOUT after 3 s is CodeTimeout, never retried. A report the binding can't read is ParseAudioReport's error.
func (e *Engine) AudioReport(ctx context.Context) (media.AudioReport, error) {
	const op = "list_audio_apps"
	if err := ctx.Err(); err != nil {
		return media.AudioReport{}, err
	}
	if err := e.begin(op); err != nil {
		return media.AudioReport{}, err
	}
	doc, err := callJSONWith(e.c, op, procListAudioApps, reportMaxCap, audioStatusError, 0)
	e.end()
	if err != nil {
		return media.AudioReport{}, e.failed(err)
	}
	return media.ParseAudioReport(doc)
}

// AudioApps returns the apps of "What friends hear" (AudioReport().Apps), for callers that need only the rows.
func (e *Engine) AudioApps(ctx context.Context) ([]media.AudioApp, error) {
	r, err := e.AudioReport(ctx)
	if err != nil {
		return nil, err
	}
	return r.Apps, nil
}

// AudioLevels is im_audio_levels_get (05 §12.2): the mix's level and up to 64 apps', most recently active first.
// Neither watching nor live, it is media.ErrAudioNotRunning.
func (e *Engine) AudioLevels(ctx context.Context) (media.AudioLevels, error) {
	const op = "audio_levels_get"
	if err := ctx.Err(); err != nil {
		return media.AudioLevels{}, err
	}
	if err := e.begin(op); err != nil {
		return media.AudioLevels{}, err
	}
	lv := &imAudioLevels{structSize: uint32(unsafe.Sizeof(imAudioLevels{}))}
	var pn runtime.Pinner
	r, lastErr := e.c.call(procAudioLevelsGet, addrOf(&pn, lv))
	pn.Unpin()
	e.end()
	if st := int32(r); st != imOK {
		return media.AudioLevels{}, e.failed(audioStatusError(op, procAudioLevelsGet.String(), st, lastErr))
	}
	return decodeLevels(lv), nil
}

// decodeLevels is im_audio_levels → media.AudioLevels: count is capped at IM_MAX_LEVEL_APPS, an app's source 0 is
// Captured (what friends hear), 1 its session's activity meter.
func decodeLevels(lv *imAudioLevels) media.AudioLevels {
	out := media.AudioLevels{ReportSeq: lv.reportSeq, QPCNanos: lv.qpcNS, MixPeakDB: lv.mixPeakDB, MixRMSDB: lv.mixRMSDB}
	n := min(int(lv.count), imMaxLevelApps)
	out.Apps = make([]media.AudioLevel, 0, n)
	for i := range n {
		a := &lv.apps[i]
		out.Apps = append(out.Apps, media.AudioLevel{AppID: cBytes(a.appID[:]), PeakDB: a.peakDB, RMSDB: a.rmsDB,
			Captured: a.source == 0})
	}
	return out
}

// SoundCheck is the first-run sound check (im_audio_sound_check, 05 §11.3): it captures the chime of helperPID's
// process tree (0: the DLL plays it itself). timeout is 1–10 s, else CodeInvalid without a call; the DLL waits up to
// timeout + 500 ms and then answers IM_ERR_TIMEOUT, which is CodeTimeout, never retried.
func (e *Engine) SoundCheck(ctx context.Context, helperPID uint32,
	timeout time.Duration) (media.SoundCheckResult, error) {
	const op = "audio_sound_check"
	if timeout < minSoundCheckTimeout || timeout > maxSoundCheckTimeout {
		return media.SoundCheckResult{}, codeError(media.CodeInvalid, op, "", "timeout %v is outside 1s..10s", timeout)
	}
	if err := ctx.Err(); err != nil {
		return media.SoundCheckResult{}, err
	}
	if err := e.begin(op); err != nil {
		return media.SoundCheckResult{}, err
	}
	doc, err := callJSONWith(e.c, op, procAudioSoundCheck, jsonMaxCap, audioStatusError, uintptr(helperPID),
		uintptr(timeout.Milliseconds()))
	e.end()
	if err != nil {
		return media.SoundCheckResult{}, e.failed(err)
	}
	var res media.SoundCheckResult
	if err := json.Unmarshal(doc, &res); err != nil {
		return media.SoundCheckResult{}, codeError(media.CodeInternal, op, "", "im_audio_sound_check: %v", err)
	}
	return res, nil
}

// exclusionsCall is im_audio_watch or im_set_exclusions with x, built (and checked) before any call.
func (e *Engine) exclusionsCall(ctx context.Context, op string, p proc, x media.Exclusions) error {
	cx, err := buildExclusions(op, x)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	e.audio.ctl.Lock()
	defer e.audio.ctl.Unlock()
	if err := e.begin(op); err != nil {
		return err
	}
	defer e.end()
	var pn runtime.Pinner
	r, lastErr := e.c.call(p, cx.pin(&pn))
	pn.Unpin()
	runtime.KeepAlive(cx)
	if st := int32(r); st != imOK {
		return e.failed(audioStatusError(op, p.String(), st, lastErr))
	}
	return nil
}

// audioControl runs one of the watcher's control exports with integer arguments under engineAudio.ctl.
func (e *Engine) audioControl(ctx context.Context, op string, p proc, args ...uintptr) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.audio.ctl.Lock()
	defer e.audio.ctl.Unlock()
	if err := e.begin(op); err != nil {
		return err
	}
	defer e.end()
	r, lastErr := e.c.call(p, args...)
	if st := int32(r); st != imOK {
		return e.failed(audioStatusError(op, p.String(), st, lastErr))
	}
	return nil
}

// ------------------------------------------------------------------------------------------- the test exports

// AudioTest is the shipped im_test_* exports of an engine (05 §13.2), for cmd/isshoni-audiotest's driver only: they
// render or decode test signals and capture nothing. Each call may block for as long as it renders, outside the
// watcher's control lock.
type AudioTest struct {
	e *Engine
}

// AudioTestHooks returns the im_test_* exports of the engine native.Open returned; for any other media.Engine an
// error wrapping media.ErrUnsupportedOS (05 §13.2).
func AudioTestHooks(e media.Engine) (*AudioTest, error) {
	ne, ok := e.(*Engine)
	if !ok || ne == nil {
		return nil, &media.Error{Code: media.CodeUnsupportedOS, Op: "audio_test_hooks",
			Detail: "not an engine from native.Open"}
	}
	return &AudioTest{e: ne}, nil
}

// RenderTone is im_test_render_tone: d of a hz sine at amplitude amp (linear, 0..1) on the render endpoint
// endpointID (an IMMDevice id; "" = the default render endpoint, eConsole). It returns when the tone has played.
func (t *AudioTest) RenderTone(endpointID string, hz, amp float32, d time.Duration) error {
	const op = "test_render_tone"
	ms, err := msArg(op, "d", d)
	if err != nil {
		return err
	}
	ep, err := endpointArg(op, endpointID)
	if err != nil {
		return err
	}
	if err := t.e.begin(op); err != nil {
		return err
	}
	defer t.e.end()
	var pn runtime.Pinner
	r, lastErr := t.e.c.call(procTestRenderTone, addrOf(&pn, ep), floatArg(hz), floatArg(amp), uintptr(ms))
	pn.Unpin()
	if st := int32(r); st != imOK {
		return t.e.failed(audioStatusError(op, procTestRenderTone.String(), st, lastErr))
	}
	return nil
}

// RenderBursts is im_test_render_bursts: n bursts of burst length of a hz sine at amp, one every period, on
// endpointID ("" = the default render endpoint). It returns each burst's predicted onset on the engine clock (QPC
// ns, im_clock's domain, from the render client's IAudioClock, 05 §13.2) once the last burst has played.
func (t *AudioTest) RenderBursts(endpointID string, hz, amp float32, burst, period time.Duration,
	n int) (onsetEngineNS []int64, err error) {
	const op = "test_render_bursts"
	burstMS, err := msArg(op, "burst", burst)
	if err != nil {
		return nil, err
	}
	periodMS, err := msArg(op, "period", period)
	if err != nil {
		return nil, err
	}
	if n < 1 || n > math.MaxInt32 {
		return nil, codeError(media.CodeInvalid, op, "", "n %d: at least 1", n)
	}
	ep, err := endpointArg(op, endpointID)
	if err != nil {
		return nil, err
	}
	if err := t.e.begin(op); err != nil {
		return nil, err
	}
	defer t.e.end()
	onsets := make([]int64, n)
	var pn runtime.Pinner
	r, lastErr := t.e.c.call(procTestRenderBursts, addrOf(&pn, ep), floatArg(hz), floatArg(amp), uintptr(burstMS),
		uintptr(periodMS), uintptr(n), addrOf(&pn, &onsets[0]), uintptr(n))
	pn.Unpin()
	got := int32(r)
	switch {
	case got < 0:
		return nil, t.e.failed(audioStatusError(op, procTestRenderBursts.String(), got, lastErr))
	case int(got) > n:
		return nil, codeError(media.CodeInternal, op, "", "im_test_render_bursts wrote %d onsets into %d", got, n)
	}
	return onsets[:got], nil
}

// OpusDecode is im_test_opus_decode: one Opus packet decoded with the DLL's libopus into pcm, interleaved stereo
// float32, whose length must hold the longest packet (2 × 5760 values). It returns the frames per channel.
func (t *AudioTest) OpusDecode(pkt []byte, pcm []float32) (frames int, err error) {
	const op = "test_opus_decode"
	if len(pkt) == 0 || len(pkt) > math.MaxInt32 {
		return 0, codeError(media.CodeInvalid, op, "", "a packet of %d bytes", len(pkt))
	}
	if len(pcm) < 2*opusMaxFrames {
		return 0, codeError(media.CodeInvalid, op, "", "pcm holds %d values: at least %d (120 ms of stereo)", len(pcm),
			2*opusMaxFrames)
	}
	maxFrames := min(len(pcm)/2, math.MaxInt32)
	if err := t.e.begin(op); err != nil {
		return 0, err
	}
	defer t.e.end()
	var pn runtime.Pinner
	r, lastErr := t.e.c.call(procTestOpusDecode, addrOf(&pn, &pkt[0]), uintptr(len(pkt)), addrOf(&pn, &pcm[0]),
		uintptr(maxFrames))
	pn.Unpin()
	got := int32(r)
	switch {
	case got < 0:
		return 0, t.e.failed(audioStatusError(op, procTestOpusDecode.String(), got, lastErr))
	case int(got) > maxFrames:
		return 0, codeError(media.CodeInternal, op, "", "im_test_opus_decode decoded %d frames into %d", got,
			maxFrames)
	}
	return int(got), nil
}

// EngineClockNS is im_clock().mono_ns, the engine clock that RenderBursts' onsets use.
func (t *AudioTest) EngineClockNS() (int64, error) {
	s, err := t.e.Clock()
	return s.MonoNS, err
}

// msArg is a duration argument in whole milliseconds: at least 1 ms, at most an int32's.
func msArg(op, what string, d time.Duration) (int32, error) {
	ms := d.Milliseconds()
	if ms < 1 || ms > math.MaxInt32 {
		return 0, codeError(media.CodeInvalid, op, "", "%s %v: at least 1ms", what, d)
	}
	return int32(ms), nil
}

// endpointArg is an endpoint id as a C string: "" is NULL, the default render endpoint.
func endpointArg(op, id string) (*byte, error) {
	if id == "" {
		return nil, nil
	}
	return cString(op, id)
}

// floatArg passes a C float: its bits in the low half of the register. SyscallN on windows/amd64 copies the first
// four integer arguments into XMM0–XMM3 too (the x64 convention passes them there for floats), and a later one is a
// stack slot whose low four bytes the callee reads.
func floatArg(f float32) uintptr { return uintptr(math.Float32bits(f)) }
