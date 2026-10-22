package native

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/MoonWX/isshoni/internal/media"
)

// audio_test drives 05's half of the binding (audio.go, README WA35) through fakeCaller on every OS (05 §19.1 "Go
// binding logic"): the exclusions as the DLL reads them, Go's refusals without a call, the status → error table of
// 05 §13.1, the snprintf retry of the report, the single mode mapping at start and in SetAudioMode, level decoding,
// the sound check, the control lock, and the im_test_* wrappers.

// readExclusions deep-reads an im_exclusions the binding passed, as the DLL copies it during the call (05 §4.3).
func readExclusions(a uintptr) *exRecord {
	if a == 0 {
		return nil
	}
	ex := fakePtr[imExclusions](a)
	rec := &exRecord{flags: ex.flags, sharedPID: ex.sharedPID}
	if ex.structSize != uint32(unsafe.Sizeof(imExclusions{})) {
		rec.flags = 0xDEAD // a wrong struct_size: the tests compare flags
	}
	if ex.ruleCount == 0 {
		return rec
	}
	for _, r := range unsafe.Slice(ex.rules, ex.ruleCount) {
		rr := ruleRecord{kind: r.kind, source: r.source, value: fakeCString(uintptr(unsafe.Pointer(r.value)))}
		if r.appID != nil {
			v := fakeCString(uintptr(unsafe.Pointer(r.appID)))
			rr.appID = &v
		}
		if r.name != nil {
			v := fakeCString(uintptr(unsafe.Pointer(r.name)))
			rr.name = &v
		}
		rec.rules = append(rec.rules, rr)
	}
	return rec
}

// audioFake scripts the audio exports of a fakeCaller: im_audio_watch and im_set_exclusions record their exclusions,
// im_list_audio_apps and im_audio_sound_check answer their documents under the snprintf rule, im_audio_levels_get
// writes levels, the rest answer IM_OK. Each export can be made to fail with st (fail).
type audioFake struct {
	f *fakeCaller

	mu         sync.Mutex
	exclusions map[proc][]*exRecord
	report     string
	soundCheck string
	levels     imAudioLevels
}

func newAudioFake(t *testing.T) *audioFake {
	a := &audioFake{f: newFakeCaller(t), exclusions: map[proc][]*exRecord{}}
	a.report = goldenReport(t, "report-normal.json")
	a.soundCheck = `{"captured":true,"levelDb":-14.3,"ms":1460,"backend":"process-loopback"}`
	for _, p := range []proc{procAudioWatch, procSetExclusions} {
		a.f.set(p, func(args []uintptr) (uintptr, string) {
			rec := readExclusions(args[0])
			a.mu.Lock()
			defer a.mu.Unlock()
			a.exclusions[p] = append(a.exclusions[p], rec)
			return fakeResult(imOK), ""
		})
	}
	for _, p := range []proc{procAudioUnwatch, procAudioSetMode, procAudioSetMuted, procTestRenderTone} {
		a.f.set(p, func([]uintptr) (uintptr, string) { return fakeResult(imOK), "" })
	}
	a.f.set(procListAudioApps, func(args []uintptr) (uintptr, string) {
		a.mu.Lock()
		doc := a.report
		a.mu.Unlock()
		return fakeSnprintf(args[1], args[2], doc), ""
	})
	a.f.set(procAudioSoundCheck, func(args []uintptr) (uintptr, string) {
		a.mu.Lock()
		doc := a.soundCheck
		a.mu.Unlock()
		return fakeSnprintf(args[2], args[3], doc), ""
	})
	a.f.set(procAudioLevelsGet, func(args []uintptr) (uintptr, string) {
		out := fakePtr[imAudioLevels](args[0])
		if out.structSize < uint32(unsafe.Sizeof(imAudioLevels{})) {
			return fakeResult(imErrABI), "abi: im_audio_levels_get: struct_size below v1.0"
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		*out = a.levels
		out.structSize = uint32(unsafe.Sizeof(imAudioLevels{}))
		return fakeResult(imOK), ""
	})
	return a
}

// goldenReport is one of internal/media's golden reports (05 §13.1), which the DLL's report writer produces too.
func goldenReport(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", "audio", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (a *audioFake) recorded(p proc) []*exRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.exclusions[p]
}

// testExclusions is a rule set like the desktop service builds (05 §14.3): isshoni, data, a built-in, a user rule.
func testExclusions() media.Exclusions {
	return media.Exclusions{KeepOutMicUsers: true, FastPath: true, SharedPID: 4242, Rules: []media.Rule{
		{Kind: media.RuleInstance, Source: media.SourceSelf, Value: "1234", AppID: "isshoni"},
		{Kind: media.RuleStop, Source: media.SourceData, Value: "steam.exe"},
		{Kind: media.RuleAppExclude, Source: media.SourceBuiltin, Value: "discord.exe", AppID: "discord", Name: "Discord"},
		{Kind: media.RuleWebViewDataDir, Source: media.SourceSelf, Value: `C:\Users\x\AppData\Local\isshoni\WebView2`},
		{Kind: media.RuleAppAllow, Source: media.SourceUser, Value: "chrome.exe", Name: "Google Chrome"},
	}}
}

// TestAudioExclusionsAsTheDLLReadsThem: WatchAudio and SetExclusions pass im_exclusions with struct_size, the flags,
// shared_pid and every rule in order; an empty AppID or Name is NULL (the DLL then derives them, 05 §4.1).
func TestAudioExclusionsAsTheDLLReadsThem(t *testing.T) {
	a := newAudioFake(t)
	e := openFake(t, a.f, Options{})
	ctx := context.Background()
	x := testExclusions()
	if err := e.WatchAudio(ctx, x); err != nil {
		t.Fatal(err)
	}
	if err := e.SetExclusions(ctx, media.Exclusions{}); err != nil {
		t.Fatal(err)
	}
	w, s := a.recorded(procAudioWatch), a.recorded(procSetExclusions)
	if len(w) != 1 || len(s) != 1 {
		t.Fatalf("%d watches and %d set_exclusions", len(w), len(s))
	}
	if w[0].flags != imExclfKeepOutMicUsers|imExclfFastPath || w[0].sharedPID != 4242 || len(w[0].rules) != 5 {
		t.Fatalf("watch: %+v", w[0])
	}
	for i, r := range x.Rules {
		got := w[0].rules[i]
		if got.kind != uint32(r.Kind) || got.source != uint32(r.Source) || got.value != r.Value {
			t.Errorf("rule %d: %+v, want %+v", i, got, r)
		}
		if (got.appID == nil) != (r.AppID == "") || (got.appID != nil && *got.appID != r.AppID) {
			t.Errorf("rule %d: app id %v, want %q (NULL when empty)", i, got.appID, r.AppID)
		}
		if (got.name == nil) != (r.Name == "") || (got.name != nil && *got.name != r.Name) {
			t.Errorf("rule %d: name %v, want %q (NULL when empty)", i, got.name, r.Name)
		}
	}
	if s[0].flags != 0 || s[0].sharedPID != 0 || len(s[0].rules) != 0 {
		t.Errorf("empty exclusions: %+v", s[0])
	}
	if err := e.UnwatchAudio(ctx); err != nil {
		t.Fatal(err)
	}
	if a.f.count(procAudioUnwatch) != 1 {
		t.Error("UnwatchAudio made no im_audio_unwatch call")
	}
}

// TestAudioExclusionsRefusedWithoutACall: what a C string can't carry and the header's limits are CodeInvalid
// before any call, naming the rule's index and never its contents (05 §13.1, §16).
func TestAudioExclusionsRefusedWithoutACall(t *testing.T) {
	a := newAudioFake(t)
	e := openFake(t, a.f, Options{})
	ctx := context.Background()
	tooMany := make([]media.Rule, imMaxRules+1)
	for i := range tooMany {
		tooMany[i] = media.Rule{Value: "a.exe"}
	}
	secret := "voicechat-secret"
	long := strings.Repeat("x", imMaxRuleValue)
	for name, tc := range map[string]struct {
		rules []media.Rule
		index string
	}{
		"513 rules":           {tooMany, "513 rules"},
		"a NUL in the value":  {[]media.Rule{{Value: "a.exe"}, {Value: secret + "\x00.exe"}}, "Rules[1].Value"},
		"a NUL in the app id": {[]media.Rule{{Value: "a.exe", AppID: secret + "\x00"}}, "Rules[0].AppID"},
		"a NUL in the name":   {[]media.Rule{{Value: "a.exe", Name: secret + "\x00"}}, "Rules[0].Name"},
		"a 520-byte value":    {[]media.Rule{{Value: long}}, "Rules[0].Value"},
	} {
		t.Run(name, func(t *testing.T) {
			calls := a.f.total()
			for op, err := range map[string]error{
				"audio_watch":    e.WatchAudio(ctx, media.Exclusions{Rules: tc.rules}),
				"set_exclusions": e.SetExclusions(ctx, media.Exclusions{Rules: tc.rules}),
			} {
				detail := wantError(t, err, media.CodeInvalid, "", op)
				if !strings.Contains(detail, tc.index) || strings.Contains(detail, secret) {
					t.Errorf("%s: detail %q, want the rule's index and not its contents", op, detail)
				}
			}
			if a.f.total() != calls {
				t.Errorf("%d DLL calls", a.f.total()-calls)
			}
		})
	}
	// The longest value the header allows: 519 bytes and its NUL.
	if err := e.SetExclusions(ctx, media.Exclusions{Rules: []media.Rule{{Value: long[:imMaxRuleValue-1]}}}); err != nil {
		t.Errorf("a 519-byte value: %v", err)
	}
	if r := a.recorded(procSetExclusions); len(r) != 1 || len(r[0].rules[0].value) != imMaxRuleValue-1 {
		t.Errorf("the 519-byte value did not arrive whole")
	}
}

// TestAudioStatusTable is 05 §13.1's status → error table, each status from the calls it names: the code, the Op
// (the export without im_), the Reason, the Detail with im_last_error, errors.Is, and exactly one call (a timeout is
// never retried).
func TestAudioStatusTable(t *testing.T) {
	type call struct {
		op string
		p  proc
		do func(e *Engine) error
	}
	ctx := context.Background()
	calls := []call{
		{"audio_watch", procAudioWatch, func(e *Engine) error { return e.WatchAudio(ctx, testExclusions()) }},
		{"audio_unwatch", procAudioUnwatch, func(e *Engine) error { return e.UnwatchAudio(ctx) }},
		{"set_exclusions", procSetExclusions, func(e *Engine) error { return e.SetExclusions(ctx, testExclusions()) }},
		{"audio_set_mode", procAudioSetMode, func(e *Engine) error { return e.SetAudioMode(ctx, media.AudioOff) }},
		{"audio_set_muted", procAudioSetMuted, func(e *Engine) error { return e.SetAudioMuted(ctx, true) }},
		{"list_audio_apps", procListAudioApps, func(e *Engine) error { return second(e.AudioReport(ctx)) }},
		{"list_audio_apps", procListAudioApps, func(e *Engine) error { return second(e.AudioApps(ctx)) }},
		{"audio_levels_get", procAudioLevelsGet, func(e *Engine) error { return second(e.AudioLevels(ctx)) }},
		{"audio_sound_check", procAudioSoundCheck, func(e *Engine) error {
			return second(e.SoundCheck(ctx, 0, 3*time.Second))
		}},
	}
	for _, row := range []struct {
		name    string
		st      int32
		lastErr string
		code    media.ErrorCode
		reason  string
		is      error
	}{
		{"bad state", imErrBadState, "not_watching: im_audio_levels_get: neither watching nor live",
			media.CodeAudioNotRunning, tokenNotWatching, media.ErrAudioNotRunning},
		{"bad state after IM_INIT_NO_AUDIO", imErrBadState, "audio is off (IM_INIT_NO_AUDIO)",
			media.CodeAudioNotRunning, "", media.ErrAudioNotRunning},
		{"invalid", imErrInvalidArg, "im_set_exclusions: ex->rules[3].app_id has a character outside [a-z0-9:._-]",
			media.CodeInvalid, "", media.ErrInvalid},
		{"timeout", imErrTimeout, "im_list_audio_apps: the audio watcher did not answer within 3 s",
			media.CodeTimeout, "", context.DeadlineExceeded},
		{"abi", imErrABI, "abi: im_exclusions.struct_size 16 is below the v1.0 size 24", media.CodeDependency,
			tokenABI, media.ErrDependency},
		{"internal", imErrInternal, "COM failed", media.CodeInternal, "", nil},
		{"not implemented", imErrNotImplemented, "", media.CodeInternal, reasonNotImplemented, nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			for _, c := range calls {
				a := newAudioFake(t)
				e := openFake(t, a.f, Options{})
				a.f.set(c.p, fakeFails(row.st, row.lastErr))
				err := c.do(e)
				detail := wantError(t, err, row.code, row.reason, c.op)
				if row.is != nil && !errors.Is(err, row.is) {
					t.Errorf("%s: errors.Is(%v, %v) is false", c.op, err, row.is)
				}
				if !strings.HasPrefix(detail, c.p.String()+": ") {
					t.Errorf("%s: detail %q does not name %s", c.op, detail, c.p)
				}
				if row.st == imErrInvalidArg && !strings.Contains(detail, "rules[3].app_id") {
					t.Errorf("%s: detail %q lacks im_last_error", c.op, detail)
				}
				if n := a.f.count(c.p); n != 1 {
					t.Errorf("%s: %d calls of %s, want exactly 1", c.op, n, c.p)
				}
			}
		})
	}
}

// TestAudioModeMappingAtStart is the single mode mapping at im_start (05 §13.1, 04 §12.2): "", exclude-apps and the
// test tone → 1, 1, 100 (the tone with no exclusions); off and everything (README §3 item 15) and an unknown mode are
// CodeInvalid without an im_start call.
func TestAudioModeMappingAtStart(t *testing.T) {
	sf := newSessionFake(t)
	e := openFake(t, sf.f, Options{})
	for _, tc := range []struct {
		mode media.AudioMode
		want uint32
		ex   bool
	}{
		{"", imAudioModeExcludeApps, true},
		{media.AudioExcludeApps, imAudioModeExcludeApps, true},
		{media.AudioTestTone, imAudioModeTestTone, false},
	} {
		cfg := baseConfig()
		cfg.Audio = &media.AudioConfig{Mode: tc.mode, Exclusions: testExclusions()}
		s, err := e.Start(context.Background(), cfg)
		if err != nil {
			t.Fatalf("mode %q: %v", tc.mode, err)
		}
		r := sf.lastStart(t)
		if r.cfg.audio.mode != tc.want || (r.ex != nil) != tc.ex {
			t.Errorf("mode %q: im_audio_config.mode %d (exclusions %v), want %d (%v)", tc.mode, r.cfg.audio.mode,
				r.ex != nil, tc.want, tc.ex)
		}
		if tc.ex && (len(r.ex.rules) != 5 || r.ex.sharedPID != 4242) {
			t.Errorf("mode %q: exclusions %+v", tc.mode, r.ex)
		}
		if err := s.Stop(); err != nil {
			t.Fatal(err)
		}
	}
	for _, mode := range []media.AudioMode{media.AudioOff, media.AudioEverything, "loud", "EXCLUDE-APPS"} {
		calls := sf.f.count(procStart)
		cfg := baseConfig()
		cfg.Audio = &media.AudioConfig{Mode: mode}
		_, err := e.Start(context.Background(), cfg)
		wantError(t, err, media.CodeInvalid, "", "start")
		if !errors.Is(err, media.ErrInvalid) {
			t.Errorf("mode %q: %v is not ErrInvalid", mode, err)
		}
		if sf.f.count(procStart) != calls {
			t.Errorf("mode %q: im_start was called", mode)
		}
	}
}

// TestAudioModeMappingLive: SetAudioMode passes IM_AUDIO_MODE_OFF and _EXCLUDE_APPS; the test tone, everything
// (README §3 item 15), "" and an unknown mode are CodeInvalid without a call, also with no session running.
func TestAudioModeMappingLive(t *testing.T) {
	a := newAudioFake(t)
	e := openFake(t, a.f, Options{})
	ctx := context.Background()
	for _, m := range []media.AudioMode{media.AudioOff, media.AudioExcludeApps} {
		if err := e.SetAudioMode(ctx, m); err != nil {
			t.Fatalf("%q: %v", m, err)
		}
	}
	got := a.f.callsOf(procAudioSetMode)
	if len(got) != 2 || got[0][0] != imAudioModeOff || got[1][0] != imAudioModeExcludeApps {
		t.Errorf("im_audio_set_mode calls %v, want [[0] [1]]", got)
	}
	calls := a.f.total()
	for _, m := range []media.AudioMode{media.AudioTestTone, media.AudioEverything, "", "loud"} {
		err := e.SetAudioMode(ctx, m)
		wantError(t, err, media.CodeInvalid, "", "audio_set_mode")
	}
	if a.f.total() != calls {
		t.Errorf("%d DLL calls for refused modes", a.f.total()-calls)
	}
	// The mute is a 0/1 argument.
	if err := e.SetAudioMuted(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := e.SetAudioMuted(ctx, false); err != nil {
		t.Fatal(err)
	}
	if got := a.f.callsOf(procAudioSetMuted); len(got) != 2 || got[0][0] != 1 || got[1][0] != 0 {
		t.Errorf("im_audio_set_muted calls %v", got)
	}
}

// TestAudioReport: the report goes through ParseAudioReport; one larger than the first 64 KiB buffer is read with
// exactly one retry of n + 1 bytes (the snprintf rule, 01 §7.3's helper); a document that is not a report and one of
// another version are ParseAudioReport's errors.
func TestAudioReport(t *testing.T) {
	a := newAudioFake(t)
	e := openFake(t, a.f, Options{})
	ctx := context.Background()
	r, err := e.AudioReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want, err := media.ParseAudioReport([]byte(a.report))
	if err != nil {
		t.Fatal(err)
	}
	if r.Seq != want.Seq || r.Engine != want.Engine || len(r.Apps) != len(want.Apps) || r.Apps[1].ID != "discord" {
		t.Errorf("report %+v", r)
	}
	apps, err := e.AudioApps(ctx)
	if err != nil || len(apps) != len(want.Apps) || apps[0].ID != want.Apps[0].ID {
		t.Errorf("AudioApps %+v, %v", apps, err)
	}
	if got := a.f.callsOf(procListAudioApps); got[0][0] != 0 || got[0][2] != jsonFirstCap {
		t.Errorf("im_list_audio_apps args %v: flags 0 and a 64 KiB buffer", got[0])
	}

	// A large report (the golden's 128 apps, with long names): one retry with n + 1 bytes.
	many := goldenReport(t, "report-many-apps.json")
	pad := `"name":"` + strings.Repeat("n", 600)
	big := strings.ReplaceAll(many, `"name":"`, pad)
	if len(big) <= jsonFirstCap {
		t.Fatalf("the big report is only %d bytes", len(big))
	}
	a.mu.Lock()
	a.report = big
	a.mu.Unlock()
	before := len(a.f.callsOf(procListAudioApps))
	r, err = e.AudioReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := a.f.callsOf(procListAudioApps)[before:]
	if len(got) != 2 || got[1][2] != uintptr(len(big)+1) || len(r.Apps) != 128 {
		t.Errorf("%d calls (%v) and %d apps for a %d-byte report", len(got), got, len(r.Apps), len(big))
	}

	for doc, code := range map[string]media.ErrorCode{
		`{"v":2,"seq":1}`: media.CodeDependency,
		`not json`:        media.CodeInternal,
	} {
		a.mu.Lock()
		a.report = doc
		a.mu.Unlock()
		_, err := e.AudioReport(ctx)
		reason := ""
		if code == media.CodeDependency {
			reason = tokenABI
		}
		wantError(t, err, code, reason, "parse_audio_report")
	}
}

// TestAudioLevels: im_audio_levels is decoded field by field; count is capped at 64, source 0 is Captured.
func TestAudioLevels(t *testing.T) {
	a := newAudioFake(t)
	e := openFake(t, a.f, Options{})
	lv := imAudioLevels{reportSeq: 118, qpcNS: 81234567890123, mixPeakDB: -12.5, mixRMSDB: -18.25, count: 2}
	copy(lv.apps[0].appID[:], "exe:spotify.exe")
	lv.apps[0].peakDB, lv.apps[0].rmsDB, lv.apps[0].source = -14, -17, 0
	copy(lv.apps[1].appID[:], "discord")
	lv.apps[1].peakDB, lv.apps[1].rmsDB, lv.apps[1].source = -24, -24, 1
	a.mu.Lock()
	a.levels = lv
	a.mu.Unlock()
	got, err := e.AudioLevels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.ReportSeq != 118 || got.QPCNanos != 81234567890123 || got.MixPeakDB != -12.5 || got.MixRMSDB != -18.25 ||
		len(got.Apps) != 2 {
		t.Fatalf("levels %+v", got)
	}
	if s := got.Apps[0]; s.AppID != "exe:spotify.exe" || s.PeakDB != -14 || s.RMSDB != -17 || !s.Captured {
		t.Errorf("app 0 %+v", s)
	}
	if d := got.Apps[1]; d.AppID != "discord" || d.PeakDB != -24 || d.Captured {
		t.Errorf("app 1 %+v: source 1 is the activity meter", d)
	}
	lv.count = 1000
	for i := range lv.apps {
		copy(lv.apps[i].appID[:], "exe:a.exe")
	}
	a.mu.Lock()
	a.levels = lv
	a.mu.Unlock()
	if got, err := e.AudioLevels(context.Background()); err != nil || len(got.Apps) != imMaxLevelApps {
		t.Errorf("count 1000: %d apps, %v", len(got.Apps), err)
	}
}

// TestAudioSoundCheck: the helper pid and timeout_ms reach the DLL; the result decodes; a timeout outside 1–10 s is
// CodeInvalid without a call.
func TestAudioSoundCheck(t *testing.T) {
	a := newAudioFake(t)
	e := openFake(t, a.f, Options{})
	ctx := context.Background()
	res, err := e.SoundCheck(ctx, 777, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Captured || res.LevelDB != -14.3 || res.Millis != 1460 || res.Backend != "process-loopback" {
		t.Errorf("result %+v", res)
	}
	if got := a.f.callsOf(procAudioSoundCheck); got[0][0] != 777 || got[0][1] != 3000 {
		t.Errorf("im_audio_sound_check args %v", got[0])
	}
	a.mu.Lock()
	a.soundCheck = `{"captured":false,"reason":"no_device","backend":"none"}`
	a.mu.Unlock()
	if res, err := e.SoundCheck(ctx, 0, 10*time.Second); err != nil || res.Captured || res.Reason != "no_device" ||
		res.Backend != "none" {
		t.Errorf("no device: %+v, %v", res, err)
	}
	calls := a.f.total()
	for _, d := range []time.Duration{0, 999 * time.Millisecond, 10*time.Second + time.Millisecond, time.Minute} {
		_, err := e.SoundCheck(ctx, 0, d)
		wantError(t, err, media.CodeInvalid, "", "audio_sound_check")
	}
	if a.f.total() != calls {
		t.Errorf("%d DLL calls for refused timeouts", a.f.total()-calls)
	}
	a.mu.Lock()
	a.soundCheck = `{"captured":`
	a.mu.Unlock()
	_, err = e.SoundCheck(ctx, 0, time.Second)
	wantError(t, err, media.CodeInternal, "", "audio_sound_check")
}

// TestAudioCallsWithoutACall: a done context and a closed engine answer without reaching the DLL.
func TestAudioCallsWithoutACall(t *testing.T) {
	a := newAudioFake(t)
	e := openFake(t, a.f, Options{})
	done, cancel := context.WithCancel(context.Background())
	cancel()
	at, err := AudioTestHooks(e)
	if err != nil {
		t.Fatal(err)
	}
	all := func(ctx context.Context) map[string]error {
		return map[string]error{
			"audio_watch":       e.WatchAudio(ctx, testExclusions()),
			"audio_unwatch":     e.UnwatchAudio(ctx),
			"set_exclusions":    e.SetExclusions(ctx, testExclusions()),
			"audio_set_mode":    e.SetAudioMode(ctx, media.AudioOff),
			"audio_set_muted":   e.SetAudioMuted(ctx, true),
			"list_audio_apps":   second(e.AudioReport(ctx)),
			"audio_levels_get":  second(e.AudioLevels(ctx)),
			"audio_sound_check": second(e.SoundCheck(ctx, 0, time.Second)),
		}
	}
	calls := a.f.total()
	for op, err := range all(done) {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s with a done context: %v", op, err)
		}
	}
	if a.f.total() != calls {
		t.Errorf("%d DLL calls with a done context", a.f.total()-calls)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	calls = a.f.total()
	for op, err := range all(context.Background()) {
		wantError(t, err, media.CodeInternal, tokenNotInitialized, op)
	}
	wantError(t, at.RenderTone("", 440, 0.5, time.Second), media.CodeInternal, tokenNotInitialized,
		"test_render_tone")
	if a.f.total() != calls {
		t.Errorf("%d DLL calls after Close", a.f.total()-calls)
	}
}

// TestAudioControlLock: the watcher's control calls run one at a time (05 §13.2), while AudioReport, AudioLevels and
// SoundCheck, which may block for seconds, run outside that lock.
func TestAudioControlLock(t *testing.T) {
	a := newAudioFake(t)
	e := openFake(t, a.f, Options{})
	ctx := context.Background()
	var inside, overlaps atomic.Int32
	control := func([]uintptr) (uintptr, string) {
		if inside.Add(1) > 1 {
			overlaps.Add(1)
		}
		time.Sleep(2 * time.Millisecond)
		inside.Add(-1)
		return fakeResult(imOK), ""
	}
	for _, p := range []proc{procAudioWatch, procAudioUnwatch, procSetExclusions, procAudioSetMode, procAudioSetMuted} {
		a.f.set(p, control)
	}
	release := make(chan struct{})
	blocked := make(chan struct{}, 2)
	a.f.set(procListAudioApps, func(args []uintptr) (uintptr, string) {
		blocked <- struct{}{}
		<-release
		return fakeSnprintf(args[1], args[2], goldenReport(t, "report-normal.json")), ""
	})
	reportDone := make(chan error, 1)
	go func() {
		_, err := e.AudioReport(ctx)
		reportDone <- err
	}()
	<-blocked // the report is in the DLL now
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch i % 5 {
			case 0:
				_ = e.WatchAudio(ctx, testExclusions())
			case 1:
				_ = e.UnwatchAudio(ctx)
			case 2:
				_ = e.SetExclusions(ctx, testExclusions())
			case 3:
				_ = e.SetAudioMode(ctx, media.AudioExcludeApps)
			default:
				_ = e.SetAudioMuted(ctx, i%2 == 0)
			}
		}()
	}
	wg.Wait() // the controls finished while the report still blocks: it holds no lock of theirs
	if _, err := e.AudioLevels(ctx); err != nil {
		t.Error(err)
	}
	close(release)
	if err := <-reportDone; err != nil {
		t.Fatal(err)
	}
	if n := overlaps.Load(); n != 0 {
		t.Errorf("%d control calls overlapped", n)
	}
}

// TestAudioTestHooks: the im_test_* wrappers pass their arguments as the header declares them (floats in the low 32
// bits of their registers, durations in ms, "" as the default endpoint's NULL), return what the DLL wrote, and refuse
// what they can tell is wrong without a call.
func TestAudioTestHooks(t *testing.T) {
	var other media.Engine
	if _, err := AudioTestHooks(other); !errors.Is(err, media.ErrUnsupportedOS) {
		t.Errorf("nil engine: %v", err)
	}
	if _, err := AudioTestHooks((*Engine)(nil)); !errors.Is(err, media.ErrUnsupportedOS) {
		t.Errorf("nil *Engine: %v", err)
	}
	a := newAudioFake(t)
	e := openFake(t, a.f, Options{})
	at, err := AudioTestHooks(e)
	if err != nil {
		t.Fatal(err)
	}
	var endpoint string
	a.f.set(procTestRenderTone, func(args []uintptr) (uintptr, string) {
		endpoint = fakeCString(args[0])
		return fakeResult(imOK), ""
	})
	if err := at.RenderTone("", 997, 0.001, 200*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := at.RenderTone(`{0.0.0.00000000}.{guid}`, 440, 0.5, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	tone := a.f.callsOf(procTestRenderTone)
	if tone[0][0] != 0 || math.Float32frombits(uint32(tone[0][1])) != 997 ||
		math.Float32frombits(uint32(tone[0][2])) != 0.001 || tone[0][3] != 200 {
		t.Errorf("im_test_render_tone args %v", tone[0])
	}
	if tone[1][0] == 0 || endpoint != `{0.0.0.00000000}.{guid}` || tone[1][3] != 2000 {
		t.Errorf("im_test_render_tone with an endpoint: %v, %q", tone[1], endpoint)
	}

	a.f.set(procTestRenderBursts, func(args []uintptr) (uintptr, string) {
		if math.Float32frombits(uint32(args[1])) != 2000 || math.Float32frombits(uint32(args[2])) != 0.25 ||
			args[3] != 5 || args[4] != 1000 || args[5] != 3 || args[7] != 3 {
			return fakeResult(imErrInvalidArg), "unexpected arguments"
		}
		onsets := unsafe.Slice(fakePtr[int64](args[6]), int(args[7]))
		onsets[0], onsets[1] = 1_000_000_000, 2_000_000_000
		return fakeResult(2), ""
	})
	onsets, err := at.RenderBursts("", 2000, 0.25, 5*time.Millisecond, time.Second, 3)
	if err != nil || len(onsets) != 2 || onsets[0] != 1e9 || onsets[1] != 2e9 {
		t.Errorf("RenderBursts %v, %v", onsets, err)
	}
	a.f.set(procTestRenderBursts, func([]uintptr) (uintptr, string) { return fakeResult(4), "" })
	_, err = at.RenderBursts("", 2000, 0.25, 5*time.Millisecond, time.Second, 3)
	wantError(t, err, media.CodeInternal, "", "test_render_bursts")

	pkt := []byte{0xFC, 0xFF, 0xFE}
	pcm := make([]float32, 2*opusMaxFrames)
	a.f.set(procTestOpusDecode, func(args []uintptr) (uintptr, string) {
		if string(fakeBytes(args[0], int(args[1]))) != string(pkt) || args[3] != opusMaxFrames {
			return fakeResult(imErrInvalidArg), "unexpected arguments"
		}
		out := unsafe.Slice(fakePtr[float32](args[2]), 2*int(args[3]))
		out[0], out[1] = 0.5, -0.5
		return fakeResult(960), ""
	})
	frames, err := at.OpusDecode(pkt, pcm)
	if err != nil || frames != 960 || pcm[0] != 0.5 || pcm[1] != -0.5 {
		t.Errorf("OpusDecode %d, %v, pcm %v", frames, err, pcm[:2])
	}

	calls := a.f.total()
	for op, err := range map[string]error{
		"test_render_tone":   at.RenderTone("", 440, 0.5, 0),
		"test_render_bursts": second(at.RenderBursts("", 2000, 0.5, 5*time.Millisecond, time.Second, 0)),
		"test_opus_decode":   second(at.OpusDecode(nil, pcm)),
	} {
		wantError(t, err, media.CodeInvalid, "", op)
	}
	_, err = at.OpusDecode(pkt, pcm[:2*opusMaxFrames-1])
	wantError(t, err, media.CodeInvalid, "", "test_opus_decode")
	_, err = at.RenderBursts("", 2000, 0.5, 0, time.Second, 3)
	wantError(t, err, media.CodeInvalid, "", "test_render_bursts")
	if a.f.total() != calls {
		t.Errorf("%d DLL calls for refused arguments", a.f.total()-calls)
	}

	// A failure of a test export is the audio table's error.
	a.f.set(procTestRenderTone, fakeFails(imErrBadState, "no render endpoint"))
	err = at.RenderTone("", 440, 0.5, time.Second)
	wantError(t, err, media.CodeAudioNotRunning, "", "test_render_tone")
	a.f.set(procTestOpusDecode, fakeFails(imErrNotImplemented, ""))
	_, err = at.OpusDecode(pkt, pcm)
	wantError(t, err, media.CodeInternal, reasonNotImplemented, "test_opus_decode")
}

// TestAudioEngineClock: AudioTest.EngineClockNS is im_clock's mono_ns.
func TestAudioEngineClock(t *testing.T) {
	a := newAudioFake(t)
	e := openFake(t, a.f, Options{})
	at, err := AudioTestHooks(e)
	if err != nil {
		t.Fatal(err)
	}
	before := a.f.qpcNS()
	ns, err := at.EngineClockNS()
	if err != nil || ns < before || ns > a.f.qpcNS() {
		t.Errorf("EngineClockNS %d, %v: not between the fake QPC readings", ns, err)
	}
}
