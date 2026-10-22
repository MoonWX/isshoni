package audio

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/audiorules"
)

// TestRulesBuilder is 05 §14.3's builder for every combination of the settings' flags, decisions (none, keep-out,
// include, locked and invalid ones that are ignored), the SelfInfo parts, SharedPID and viewer rules.
func TestRulesBuilder(t *testing.T) {
	decisions := []map[string]Decision{
		nil,
		{"exe:kook.exe": DecisionKeepOut},
		{"exe:chrome.exe": DecisionInclude, "exe:spotify.exe": DecisionKeepOut},
		{"discord": DecisionInclude, "exe:discordptb.exe": DecisionInclude, "exe:isshoni.exe": DecisionInclude,
			"exe:Bad.exe": DecisionKeepOut, "exe:ok.exe": "maybe"},
	}
	selves := []SelfInfo{testSelf, {}, {PID: 7}}
	for _, mic := range []bool{true, false} {
		for _, fast := range []bool{true, false} {
			for di, rules := range decisions {
				for si, self := range selves {
					for _, pid := range []uint32{0, 9932} {
						for _, viewer := range [][]string{nil, {"chrome.exe", "brave.exe"}} {
							name := fmt.Sprintf("mic=%v/fast=%v/decisions=%d/self=%d/pid=%d/viewer=%d", mic, fast, di,
								si, pid, len(viewer))
							t.Run(name, func(t *testing.T) {
								st := DefaultSettings()
								st.KeepOutMicUsers, st.FastPath = mic, fast
								for id, d := range rules {
									st = st.withDecision(id, d)
								}
								checkRules(t, buildExclusions(ruleInput{self: self, settings: st.normalized(),
									sharedPID: pid, viewerExes: viewer}), self, st.normalized(), pid, viewer)
							})
						}
					}
				}
			}
		}
	}
}

// checkRules checks ex against 05 §14.3's rows for its inputs.
func checkRules(t *testing.T, ex media.Exclusions, self SelfInfo, st Settings, pid uint32, viewer []string) {
	t.Helper()
	if ex.KeepOutMicUsers != st.KeepOutMicUsers || ex.FastPath != st.FastPath || ex.SharedPID != pid {
		t.Errorf("flags %v %v %d", ex.KeepOutMicUsers, ex.FastPath, ex.SharedPID)
	}
	var want []media.Rule
	// 1. Self.
	if self.PID != 0 {
		want = append(want, media.Rule{Kind: media.RuleInstance, Source: media.SourceSelf,
			Value: fmt.Sprint(self.PID), AppID: "isshoni", Name: "isshoni"})
	}
	want = append(want, media.Rule{Kind: media.RuleAppExclude, Source: media.SourceSelf, Value: "isshoni.exe",
		AppID: "isshoni", Name: "isshoni"})
	if self.WebViewDataDir != "" {
		want = append(want, media.Rule{Kind: media.RuleWebViewDataDir, Source: media.SourceSelf,
			Value: self.WebViewDataDir, AppID: "isshoni", Name: "isshoni"})
	}
	// 2. Data.
	for _, exe := range audiorules.Launchers().Windows {
		want = append(want, media.Rule{Kind: media.RuleStop, Source: media.SourceData, Value: exe})
	}
	want = append(want, media.Rule{Kind: media.RuleSharedRuntime, Source: media.SourceData,
		Value: "msedgewebview2.exe"})
	for _, b := range audiorules.Browsers() {
		for _, exe := range b.Windows {
			want = append(want, media.Rule{Kind: media.RuleBrowser, Source: media.SourceData, Value: exe})
		}
	}
	// 3. Built-ins.
	for _, va := range audiorules.VoiceApps() {
		for _, exe := range va.Windows {
			want = append(want, media.Rule{Kind: media.RuleAppExclude, Source: media.SourceBuiltin, Value: exe,
				AppID: va.ID, Name: va.Name})
		}
	}
	// 4. User: only valid, unlocked exe ids; keep-out → exclude, include → allow.
	ids := slices.Sorted(maps.Keys(st.AppRules))
	for _, id := range ids {
		exe, _ := exeOf(id)
		kind := media.RuleAppAllow
		if st.AppRules[id] == DecisionKeepOut {
			kind = media.RuleAppExclude
		}
		want = append(want, media.Rule{Kind: kind, Source: media.SourceUser, Value: exe})
	}
	// 5. Viewer.
	for _, exe := range viewer {
		want = append(want, media.Rule{Kind: media.RuleAppExclude, Source: media.SourceViewer, Value: exe})
	}
	if !slices.Equal(ex.Rules, want) {
		t.Errorf("rules:\n got %v\nwant %v", ex.Rules, want)
	}
	for _, id := range []string{"discord", "exe:discordptb.exe", "exe:isshoni.exe", "exe:Bad.exe", "exe:ok.exe"} {
		if _, ok := st.AppRules[id]; ok {
			t.Errorf("the normalized settings keep %q", id)
		}
	}
	if hasRule(ex, media.RuleAppAllow, media.SourceUser, "discordptb.exe") ||
		hasRule(ex, media.RuleAppAllow, media.SourceUser, "isshoni.exe") {
		t.Error("a locked app got an allow rule")
	}
}

// TestRulesLimit: with 256 decisions and every browser kept out for a viewer the rule set stays within the engine's
// 512 rules (05 §17), and a 257th decision drops the oldest (05 §14.1).
func TestRulesLimit(t *testing.T) {
	st := DefaultSettings()
	for i := range maxAppRules + 1 {
		st = st.withDecision(fmt.Sprintf("exe:app%03d.exe", i), DecisionKeepOut)
	}
	if len(st.AppRules) != maxAppRules || st.decision("exe:app000.exe") != DecisionAuto ||
		st.decision("exe:app256.exe") != DecisionKeepOut {
		t.Fatalf("%d decisions, oldest %q", len(st.AppRules), st.decision("exe:app000.exe"))
	}
	var viewer []string
	for _, b := range audiorules.Browsers() {
		viewer = append(viewer, b.Windows...)
	}
	ex := buildExclusions(ruleInput{self: testSelf, settings: st.normalized(), sharedPID: 1, viewerExes: viewer})
	if len(ex.Rules) > 512 {
		t.Errorf("%d rules", len(ex.Rules))
	}
	// Re-deciding an app makes its decision the newest.
	st = st.withDecision("exe:app001.exe", DecisionInclude).withDecision("exe:new.exe", DecisionKeepOut)
	if st.decision("exe:app001.exe") != DecisionInclude || st.decision("exe:app002.exe") != DecisionAuto {
		t.Errorf("re-decided: %q, next oldest %q", st.decision("exe:app001.exe"), st.decision("exe:app002.exe"))
	}
}

// TestAudioConfigFlushesDebounce: a rule change reaches SetExclusions 50 ms after it (changes within that window
// coalesce), AudioConfig flushes a pending change at once and returns Mode exclude-apps with BitrateBPS 0, and
// Exclusions is AudioConfig's.
func TestAudioConfigFlushesDebounce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctl := newStub()
		s := New(ctl, newStore(DefaultSettings()), testSelf, nil)
		stop := runService(t, s)
		defer stop()
		if err := s.Watch(true); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if w := ctl.ops("watch"); len(w) != 1 || w[0].ex.SharedPID != 0 || !w[0].ex.KeepOutMicUsers || !w[0].ex.FastPath {
			t.Fatalf("watch calls %+v", w)
		}

		// Debounced: three changes within 50 ms, one SetExclusions 50 ms after the first.
		start := time.Now()
		s.SetSharedPID(11)
		settle(20 * time.Millisecond)
		s.SetSharedPID(12)
		settle(20 * time.Millisecond)
		_ = s.SetKeepOutMicUsers(false)
		settle(5 * time.Millisecond)
		if n := len(ctl.ops("exclusions")); n != 0 {
			t.Fatalf("%d SetExclusions before the debounce ended", n)
		}
		settle(10 * time.Millisecond)
		calls := ctl.ops("exclusions")
		if len(calls) != 1 || calls[0].at.Sub(start) != rulesDebounce || calls[0].ex.SharedPID != 12 ||
			calls[0].ex.KeepOutMicUsers {
			t.Fatalf("SetExclusions %+v", calls)
		}
		settle(time.Second)
		if n := len(ctl.ops("exclusions")); n != 1 {
			t.Errorf("%d SetExclusions without a change", n)
		}

		// AudioConfig flushes: the change is in the engine before AudioConfig returns.
		s.SetSharedPID(13)
		cfg := s.AudioConfig()
		calls = ctl.ops("exclusions")
		if len(calls) != 2 || calls[1].ex.SharedPID != 13 || cfg.Mode != media.AudioExcludeApps || cfg.BitrateBPS != 0 ||
			!sameExclusions(cfg.Exclusions, calls[1].ex) || cfg.TapPCM || cfg.TestFlags != 0 {
			t.Fatalf("AudioConfig %+v, calls %+v", cfg, calls)
		}
		settle(time.Second)
		if n := len(ctl.ops("exclusions")); n != 2 {
			t.Errorf("the flushed change was applied again: %d calls", n)
		}
		if ex := s.Exclusions(); !sameExclusions(ex, cfg.Exclusions) || len(ctl.ops("exclusions")) != 2 {
			t.Errorf("Exclusions %+v", ex)
		}
		// AudioConfig's rules are a copy.
		cfg.Exclusions.Rules[0].Value = "x"
		if s.Exclusions().Rules[0].Value == "x" {
			t.Error("AudioConfig shares its rules")
		}
		stop()
	})
}

// TestRulesWithoutWatcher: without a watcher or a share no rule change reaches the engine; Watch(true) brings the
// current rules, and AudioConfig returns them without a call.
func TestRulesWithoutWatcher(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctl := newStub()
		s := New(ctl, newStore(DefaultSettings()), testSelf, nil)
		stop := runService(t, s)
		defer stop()
		s.SetSharedPID(5)
		settle(time.Second)
		if cfg := s.AudioConfig(); cfg.Exclusions.SharedPID != 5 || len(ctl.ops()) != 0 {
			t.Fatalf("AudioConfig %+v, calls %+v", cfg, ctl.ops())
		}
		if err := s.Watch(true); err != nil {
			t.Fatal(err)
		}
		settle(time.Second)
		if w := ctl.ops("watch"); len(w) != 1 || w[0].ex.SharedPID != 5 || len(ctl.ops("exclusions")) != 0 {
			t.Fatalf("calls %+v", ctl.ops())
		}
		stop()
	})
}

// TestRulesLiveWithoutWatcher: a share whose WatchAudio failed still polls and still gets every rule change within
// 50 ms (05 §14.3): im_set_exclusions replaces a running share's rules, or stores them for its start (05 §4.3).
func TestRulesLiveWithoutWatcher(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctl := newStub()
		ctl.set(func(r *media.AudioReport) { r.Sessions = []media.AudioSession{session("chrome.exe", 100, "active")} })
		ctl.fail("watch", errors.New("watch failed"))
		s := New(ctl, newStore(DefaultSettings()), testSelf, nil)
		stop := runService(t, s)
		defer stop()
		s.OnShareStarted(7, true)
		cfg := s.AudioConfig() // the core's start config, right after
		if calls := ctl.ops("exclusions"); len(calls) != 1 || !reflect.DeepEqual(calls[0].ex, cfg.Exclusions) ||
			cfg.Exclusions.SharedPID != 7 {
			t.Fatalf("AudioConfig with SharedPID %d, %d exclusions calls (want 1 with its rules)",
				cfg.Exclusions.SharedPID, len(calls))
		}
		settle(time.Second)
		if n := len(ctl.ops("levels")); n != 4 {
			t.Errorf("%d polls in a live second without the watcher, want 4", n)
		}

		n := len(ctl.ops("exclusions"))
		if err := s.SetAppDecision("exe:kook.exe", DecisionKeepOut); err != nil {
			t.Fatal(err)
		}
		s.SetViewerBrowser("c1", "chrome", "windows", true)
		settle(rulesDebounce)
		calls := ctl.ops("exclusions")[n:]
		if len(calls) != 1 || !hasRule(calls[0].ex, media.RuleAppExclude, media.SourceUser, "kook.exe") ||
			!hasRule(calls[0].ex, media.RuleAppExclude, media.SourceViewer, "chrome.exe") {
			t.Fatalf("%d exclusions calls after the changes, want 1 with both rules", len(calls))
		}
		s.SetSharedPID(9)
		settle(rulesDebounce)
		if ex := ctl.lastEx(t); ex.SharedPID != 9 {
			t.Errorf("SharedPID %d reached the engine", ex.SharedPID)
		}

		// The share ends: nothing watches and nothing is live, so the rules wait for the next WatchAudio.
		s.OnShareStopped()
		n = len(ctl.ops())
		if err := s.SetAppDecision("exe:kook.exe", DecisionInclude); err != nil {
			t.Fatal(err)
		}
		settle(time.Second)
		if calls := ctl.ops()[n:]; len(calls) != 0 {
			t.Errorf("%d engine calls after the share, the first %q", len(calls), calls[0].op)
		}
		stop()
	})
}

// TestSetSharedPID: a pick sets SharedPID, a release or pick expiry (0) clears it, and the shared app (the fake's
// Spotify, pid 4120) leads the heard list.
func TestSetSharedPID(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		eng := newFake("normal")
		defer func() { _ = eng.Close() }()
		s := New(eng, newStore(DefaultSettings()), testSelf, nil)
		stop := runService(t, s)
		defer stop()
		_ = s.Watch(true)
		settle(time.Second)
		s.SetSharedPID(4120) // the pick
		if ex := s.Exclusions(); ex.SharedPID != 4120 {
			t.Fatalf("SharedPID %d", ex.SharedPID)
		}
		settle(time.Second)
		fh := s.FriendsHear()
		if len(fh.Heard) == 0 || fh.Heard[0].ID != "exe:spotify.exe" || !fh.Heard[0].Shared {
			t.Fatalf("heard %+v", fh.Heard)
		}
		s.SetSharedPID(0) // released, or the pick expired
		settle(time.Second)
		if ex := s.Exclusions(); ex.SharedPID != 0 {
			t.Errorf("SharedPID %d after the release", ex.SharedPID)
		}
		if r, _ := rowByID(s.FriendsHear(), "exe:spotify.exe"); r.Shared {
			t.Error("Spotify still shared")
		}
		stop()
	})
}
