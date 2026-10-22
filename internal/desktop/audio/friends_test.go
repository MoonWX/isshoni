package audio

import (
	"encoding/json"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
)

// okReport is a report of a normal PC: probe ok, engine include-set.
func okReport(apps ...media.AudioApp) media.AudioReport {
	return media.AudioReport{V: 1, Mode: media.AudioExcludeApps, Engine: "include-set", State: "ok",
		Probe: media.AudioProbeResult{Result: "ok", OSBuild: 26100}, Apps: apps}
}

// TestBannersAndTips is 05 §14.7's banner and tip table, case by case.
func TestBannersAndTips(t *testing.T) {
	mic := app("kook.exe", "KOOK", verdictKeptOut, reasonMic)
	mic.UsesMic = true
	discord := media.AudioApp{ID: "discord", Name: "Discord", Exe: "discord.exe", Verdict: verdictKeptOut,
		Reason: "builtin"}
	failed := app("game.exe", "Game", verdictNotCaptured, "capture_failed")
	failed.Playing = true
	quietFailed := app("quiet.exe", "Quiet", verdictNotCaptured, "capture_failed")
	with := func(edit func(*media.AudioReport), apps ...media.AudioApp) media.AudioReport {
		r := okReport(apps...)
		if edit != nil {
			edit(&r)
		}
		return r
	}
	live := func(in fhInput) fhInput { in.live, in.withAudio, in.soundOn = true, true, true; return in }
	type banner struct {
		code, level string
		params      map[string]any
	}
	tests := []struct {
		name    string
		in      fhInput
		banners []banner
		tips    bool
	}{
		{name: "nothing yet", in: fhInput{}},
		{name: "normal", in: fhInput{report: okReport(), haveReport: true}},
		{name: "probe failed", in: fhInput{report: with(func(r *media.AudioReport) {
			r.Probe.Result, r.Engine, r.State = "failed", "endpoint", "degraded"
		}), haveReport: true}, banners: []banner{{code: BannerExclusionUnavailable, level: LevelError}}},
		{name: "unsupported at 19041", in: fhInput{report: with(func(r *media.AudioReport) {
			r.Probe = media.AudioProbeResult{Result: "unsupported", OSBuild: 19041}
		}), haveReport: true}, banners: []banner{{code: BannerExclusionUnavailable, level: LevelError}}},
		{name: "unsupported below 19041", in: fhInput{report: with(func(r *media.AudioReport) {
			r.Probe = media.AudioProbeResult{Result: "unsupported", OSBuild: 18363}
			r.Engine = "endpoint"
		}), haveReport: true}, banners: []banner{{code: BannerWindowsTooOld, level: LevelError}}},
		{name: "endpoint whatever the probe", in: fhInput{report: with(func(r *media.AudioReport) {
			r.Engine = "endpoint"
		}), haveReport: true}, banners: []banner{{code: BannerExclusionUnavailable, level: LevelError}}},
		{name: "no device", in: fhInput{report: with(func(r *media.AudioReport) {
			r.State, r.Engine, r.Probe.Result = "no-device", "none", "skipped"
		}), haveReport: true}, banners: []banner{{code: BannerNoOutputDevice, level: LevelWarning}}},
		{name: "rebuilding", in: fhInput{report: with(func(r *media.AudioReport) { r.Rebuilding = true }),
			haveReport: true}, banners: []banner{{code: BannerReconnectingSound, level: LevelInfo}}},
		{name: "sound off", in: fhInput{report: okReport(), haveReport: true, live: true, withAudio: true},
			banners: []banner{{code: BannerSoundOff, level: LevelInfo, params: map[string]any{"cause": "off"}}}},
		{name: "a share without sound", in: fhInput{report: okReport(), haveReport: true, live: true, soundOn: true},
			banners: []banner{{code: BannerSoundOff, level: LevelInfo, params: map[string]any{"cause": "off"}}}},
		{name: "sound off beats paused", in: fhInput{report: okReport(), haveReport: true, live: true, withAudio: true,
			paused: true}, banners: []banner{{code: BannerSoundOff, level: LevelInfo, params: map[string]any{"cause": "off"}}}},
		{name: "SetPaused", in: live(fhInput{report: okReport(), haveReport: true, paused: true}),
			banners: []banner{{code: BannerSoundOff, level: LevelInfo, params: map[string]any{"cause": "paused"}}}},
		{name: "the report's paused", in: live(fhInput{report: with(func(r *media.AudioReport) { r.Paused = true }),
			haveReport: true}),
			banners: []banner{{code: BannerSoundOff, level: LevelInfo, params: map[string]any{"cause": "paused"}}}},
		{name: "the report's muted", in: live(fhInput{report: with(func(r *media.AudioReport) { r.Muted = true }),
			haveReport: true}),
			banners: []banner{{code: BannerSoundOff, level: LevelInfo, params: map[string]any{"cause": "paused"}}}},
		{name: "SetPaused outside a share", in: fhInput{report: okReport(), haveReport: true, paused: true}},
		{name: "live with sound", in: live(fhInput{report: okReport(), haveReport: true})},
		{name: "capture trouble", in: fhInput{report: okReport(quietFailed, failed), haveReport: true},
			banners: []banner{{code: BannerCaptureTrouble, level: LevelWarning, params: map[string]any{"app": "Game"}}}},
		{name: "capture trouble, silent", in: fhInput{report: okReport(quietFailed), haveReport: true}},
		{name: "viewer muted", in: live(fhInput{report: with(func(r *media.AudioReport) {
			r.Engine, r.Probe.Result = "endpoint", "failed"
		}), haveReport: true}), banners: []banner{{code: BannerExclusionUnavailable, level: LevelError},
			{code: BannerViewerMuted, level: LevelInfo}}},
		{name: "viewer unknown browser", in: fhInput{report: okReport(), haveReport: true, unknownViewer: true},
			banners: []banner{{code: BannerViewerUnknownBrowser, level: LevelWarning}}},
		{name: "everything at once", in: fhInput{report: with(func(r *media.AudioReport) {
			r.Probe.Result, r.Engine, r.State, r.Rebuilding = "failed", "endpoint", "no-device", true
		}, failed), haveReport: true, live: true, unknownViewer: true}, banners: []banner{
			{code: BannerExclusionUnavailable, level: LevelError}, {code: BannerNoOutputDevice, level: LevelWarning},
			{code: BannerReconnectingSound, level: LevelInfo},
			{code: BannerSoundOff, level: LevelInfo, params: map[string]any{"cause": "off"}},
			{code: BannerCaptureTrouble, level: LevelWarning, params: map[string]any{"app": "Game"}},
			{code: BannerViewerMuted, level: LevelInfo}, {code: BannerViewerUnknownBrowser, level: LevelWarning}}},

		// The ducking tip.
		{name: "ducking, a mic user", in: fhInput{report: okReport(mic), haveReport: true, ducking: duckingReduces},
			tips: true},
		{name: "ducking, a voice app", in: fhInput{report: okReport(discord), haveReport: true,
			ducking: duckingReduces}, tips: true},
		{name: "ducking, neither", in: fhInput{report: okReport(app("spotify.exe", "Spotify", verdictHeard, "")),
			haveReport: true, ducking: duckingReduces}},
		{name: "ducking off: Do nothing", in: fhInput{report: okReport(mic), haveReport: true,
			ducking: duckingDoNothing}},
		{name: "ducking unknown (not Windows)", in: fhInput{report: okReport(mic), haveReport: true}},
		{name: "ducking tip dismissed", in: fhInput{report: okReport(mic), haveReport: true, ducking: duckingReduces,
			settings: Settings{DismissedTips: []string{TipDucking}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fh := buildFriendsHear(tc.in)
			var got []banner
			for _, b := range fh.Banners {
				got = append(got, banner{b.Code, b.Level, b.Params})
			}
			if !reflect.DeepEqual(got, tc.banners) {
				t.Errorf("banners %+v, want %+v", got, tc.banners)
			}
			if wantTips := tc.tips; (len(fh.Tips) == 1 && fh.Tips[0].Code == TipDucking) != wantTips ||
				len(fh.Tips) > 1 {
				t.Errorf("tips %+v, want ducking %v", fh.Tips, wantTips)
			}
		})
	}
}

// TestFriendsHearSorting: every group lists the shared app first, then the playing apps, then by name ignoring case,
// then by id; the reasons, warnings, controls and decisions come from the report and the settings.
func TestFriendsHearSorting(t *testing.T) {
	row := func(id, name string, playing, shared bool, verdict string) media.AudioApp {
		return media.AudioApp{ID: id, Name: name, Exe: strings.TrimPrefix(id, "exe:"), Verdict: verdict,
			Playing: playing, Shared: shared}
	}
	apps := []media.AudioApp{
		row("exe:b.exe", "beta", false, false, verdictHeard),
		row("exe:a.exe", "Alpha", false, false, verdictHeard),
		row("exe:z.exe", "zulu", true, false, verdictHeard),
		row("exe:c.exe", "Charlie", true, false, verdictHeard),
		row("exe:g.exe", "Game", false, true, verdictHeard),
		row("exe:a2.exe", "alpha", false, false, verdictHeard),
		row("exe:a3.exe", "alpha", false, false, verdictHeard),
		row("exe:k2.exe", "Kept B", false, false, verdictKeptOut),
		row("exe:k1.exe", "Kept A", true, false, verdictKeptOut),
		row("exe:p.exe", "Pending", true, false, "pending"),
		row("exe:n2.exe", "nc", false, false, verdictNotCaptured),
		row("exe:n1.exe", "NC", false, true, verdictNotCaptured),
	}
	fh := buildFriendsHear(fhInput{report: okReport(apps...), haveReport: true, settings: DefaultSettings()})
	ids := func(rows []AppRow) []string {
		var out []string
		for _, r := range rows {
			out = append(out, r.ID)
		}
		return out
	}
	if got, want := ids(fh.Heard), []string{"exe:g.exe", "exe:c.exe", "exe:z.exe", "exe:a.exe", "exe:a2.exe",
		"exe:a3.exe", "exe:b.exe"}; !slices.Equal(got, want) {
		t.Errorf("heard %v, want %v", got, want)
	}
	if got, want := ids(fh.KeptOut), []string{"exe:k1.exe", "exe:k2.exe"}; !slices.Equal(got, want) {
		t.Errorf("kept out %v, want %v", got, want)
	}
	if got, want := ids(fh.NotCaptured), []string{"exe:n1.exe", "exe:n2.exe"}; !slices.Equal(got, want) {
		t.Errorf("not captured %v, want %v", got, want)
	}
	if _, g := rowByID(fh, "exe:p.exe"); g != "" {
		t.Errorf("a pending row is listed in %s", g)
	}
}

// TestFriendsHearRows: a row's reason (05 §12.1's spelled with dashes), detail, control, decision and warning.
func TestFriendsHearRows(t *testing.T) {
	st := DefaultSettings().withDecision("exe:kook.exe", DecisionKeepOut).withDecision("exe:chrome.exe",
		DecisionInclude).withDecision("exe:obs64.exe", DecisionInclude)
	tree := app("launcherx.exe", "LauncherX", verdictNotCaptured, "tree")
	tree.Detail = "Discord"
	reuse := app("game.exe", "Game", verdictNotCaptured, "pid_reuse")
	reuse.Detail = "Discord"
	other := app("other.exe", "Other", verdictNotCaptured, "limit")
	other.Detail = "ignored"
	shared := app("valorant.exe", "VALORANT", verdictHeard, "")
	shared.Warnings, shared.Shared, shared.UsesMic = []string{"multi_process_session", "shared_app_uses_mic"}, true, true
	sharedOut := app("kook.exe", "KOOK", verdictKeptOut, "user")
	sharedOut.Warnings = []string{"shared_app_kept_out"}
	included := app("chrome.exe", "Google Chrome", verdictHeard, "")
	included.Warnings, included.Browser = []string{"included_uses_mic"}, true
	discord := media.AudioApp{ID: "discord", Name: "Discord", Exe: "discord.exe", Verdict: verdictHeard}
	self := media.AudioApp{ID: "isshoni", Name: "isshoni", Exe: "msedgewebview2.exe", Verdict: verdictKeptOut,
		Reason: "isshoni"}
	ptb := app("discordptb.exe", "Discord PTB", verdictKeptOut, "builtin")
	odd := app("odd.exe", "Odd", verdictKeptOut, "something-new")
	failed := app("cf.exe", "CF", verdictNotCaptured, "capture_failed")
	// The DLL's rows (no warnings): a mic user included by a saved decision gets included-uses-mic from the service.
	nativeIncluded := app("obs64.exe", "OBS", verdictHeard, "")
	nativeIncluded.UsesMic = true
	fh := buildFriendsHear(fhInput{report: okReport(tree, reuse, other, shared, sharedOut, included, discord, self,
		ptb, odd, failed, nativeIncluded), haveReport: true, settings: st})
	for _, tc := range []struct {
		id                      string
		reason, detail, control string
		decision                Decision
		warning                 string
		group                   string
	}{
		{"exe:launcherx.exe", "tree", "Discord", ControlToggle, DecisionAuto, "", "notCaptured"},
		{"exe:game.exe", "pid-reuse", "Discord", ControlToggle, DecisionAuto, "", "notCaptured"},
		{"exe:other.exe", "limit", "", ControlToggle, DecisionAuto, "", "notCaptured"},
		{"exe:cf.exe", "capture-failed", "", ControlToggle, DecisionAuto, "", "notCaptured"},
		{"exe:valorant.exe", "", "", ControlToggle, DecisionAuto, WarningSharedUsesMic, "heard"},
		{"exe:kook.exe", "user", "", ControlToggle, DecisionKeepOut, WarningSharedKeptOut, "keptOut"},
		{"exe:chrome.exe", "", "", ControlToggle, DecisionInclude, WarningIncludedUsesMic, "heard"},
		{"discord", "", "", ControlLocked, DecisionAuto, "", "heard"}, // the endpoint engine hears it
		{"isshoni", "isshoni", "", ControlLocked, DecisionAuto, "", "keptOut"},
		{"exe:discordptb.exe", "builtin", "", ControlLocked, DecisionAuto, "", "keptOut"},
		{"exe:odd.exe", "", "", ControlToggle, DecisionAuto, "", "keptOut"},
		{"exe:obs64.exe", "", "", ControlToggle, DecisionInclude, WarningIncludedUsesMic, "heard"},
	} {
		r, g := rowByID(fh, tc.id)
		if g != tc.group || r.Reason != tc.reason || r.Detail != tc.detail || r.Control != tc.control ||
			r.Decision != tc.decision || r.Warning != tc.warning {
			t.Errorf("%s: %s %+v, want %+v", tc.id, g, r, tc)
		}
	}

	// "Keep out apps that use my microphone" off (05 §14.6): a mic user the DLL reports heard without warnings gets
	// included-uses-mic; one that doesn't use the mic, the shared app and a locked app don't, nor any row of the
	// endpoint engine (which hears everything).
	micUser := app("mictool.exe", "Mic tool", verdictHeard, "")
	micUser.UsesMic = true
	quiet := app("spotify.exe", "Spotify", verdictHeard, "")
	sharedMic := app("game.exe", "Game", verdictHeard, "")
	sharedMic.UsesMic, sharedMic.Shared, sharedMic.Warnings = true, true, []string{"shared_app_uses_mic"}
	lockedMic := media.AudioApp{ID: "discord", Name: "Discord", Exe: "discord.exe", Verdict: verdictHeard,
		UsesMic: true}
	off := DefaultSettings()
	off.KeepOutMicUsers = false
	for _, engine := range []string{"include-set", "exclude-self", "endpoint"} {
		r := okReport(micUser, quiet, sharedMic, lockedMic)
		r.Engine = engine
		fh := buildFriendsHear(fhInput{report: r, haveReport: true, settings: off})
		want := map[string]string{"exe:mictool.exe": WarningIncludedUsesMic, "exe:spotify.exe": "",
			"exe:game.exe": WarningSharedUsesMic, "discord": ""}
		if engine == "endpoint" {
			want["exe:mictool.exe"] = ""
		}
		for id, w := range want {
			if row, g := rowByID(fh, id); g != "heard" || row.Warning != w {
				t.Errorf("flag off, %s: %s %s %+v, want warning %q", engine, id, g, row, w)
			}
		}
	}
	// The flag on and no decision: a heard mic user (one the DLL hasn't classified as such) gets no warning.
	if row, _ := rowByID(buildFriendsHear(fhInput{report: okReport(micUser), haveReport: true,
		settings: DefaultSettings()}), "exe:mictool.exe"); row.Warning != "" {
		t.Errorf("flag on: %+v", row)
	}
}

// TestFriendsHearJSON: the shape the SPA reads (05 §14.5): camelCase, lists never null, optional fields left out.
func TestFriendsHearJSON(t *testing.T) {
	fh := buildFriendsHear(fhInput{settings: DefaultSettings()})
	b, err := json.Marshal(fh)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "null") {
		t.Errorf("a null: %s", b)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"seq", "live", "mode", "engine", "muted", "paused", "keepOutMicUsers", "heard",
		"keptOut", "notCaptured", "prompts", "banners", "tips", "voiceApps", "alert"} {
		if _, ok := m[k]; !ok {
			t.Errorf("no %q in %s", k, b)
		}
	}
	if fh.Engine != "none" || fh.Mode != ModeExcludeApps || !fh.KeepOutMicUsers || len(fh.VoiceApps) != 7 {
		t.Errorf("before any report: %+v", fh)
	}
	rb, _ := json.Marshal(AppRow{ID: "exe:a.exe", Name: "A", Control: ControlToggle, Decision: DecisionAuto})
	if want := `{"id":"exe:a.exe","name":"A","reason":"","playing":false,"usesMic":false,"browser":false,` +
		`"shared":false,"control":"toggle","decision":"auto"}`; string(rb) != want {
		t.Errorf("row %s, want %s", rb, want)
	}
	lb, _ := json.Marshal(levelsOf(media.AudioLevels{MixPeakDB: -12.345678, MixRMSDB: float32(math.Inf(-1)),
		Apps: []media.AudioLevel{{AppID: "exe:a.exe", PeakDB: -20, Captured: true}, {AppID: "discord", PeakDB: -24},
			{AppID: "exe:a.exe", PeakDB: -1}}}, 7))
	if want := `{"seq":7,"mixPeakDb":-12.35,"mixRmsDb":-120,"apps":{"discord":{"peakDb":-24,"source":"activity"},` +
		`"exe:a.exe":{"peakDb":-20,"source":"captured"}}}`; string(lb) != want {
		t.Errorf("levels %s, want %s", lb, want)
	}
}

// TestFriendsHearAlert: alert.voiceAppsHeard only while live with an error-level banner, alert.questions the pending
// prompts listed.
func TestFriendsHearAlert(t *testing.T) {
	failed := okReport()
	failed.Probe.Result, failed.Engine = "failed", "endpoint"
	prompts := []Prompt{{ID: "p1"}, {ID: "p2"}, {ID: "p3"}, {ID: "p4"}}
	for _, tc := range []struct {
		name string
		in   fhInput
		want Alert
	}{
		{"idle", fhInput{report: okReport(), haveReport: true}, Alert{}},
		{"error banner, not live", fhInput{report: failed, haveReport: true}, Alert{}},
		{"error banner, live", fhInput{report: failed, haveReport: true, live: true, withAudio: true, soundOn: true},
			Alert{VoiceAppsHeard: true}},
		{"warning banner, live", fhInput{report: okReport(), haveReport: true, live: true, unknownViewer: true},
			Alert{}},
		{"prompts", fhInput{report: okReport(), haveReport: true, prompts: prompts[:2]}, Alert{Questions: 2}},
		{"more prompts than listed", fhInput{report: okReport(), haveReport: true, prompts: prompts},
			Alert{Questions: 3}},
	} {
		if got := buildFriendsHear(tc.in).Alert; got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// TestAudioAlertDegradeLive: with the fake's degrade-live scenario, process loopback fails 5 s into the share, the
// error banner exclusion-unavailable appears, and audio.alert fires once; FriendsHear.alert says friends hear voice
// apps. Never outside a live share.
func TestAudioAlertDegradeLive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		eng := newFake("degrade-live")
		defer func() { _ = eng.Close() }()
		s := New(eng, newStore(DefaultSettings()), testSelf, nil)
		rec := record(s)
		stop := runService(t, s)
		defer stop()
		_ = s.Watch(true)
		settle(10 * time.Second)
		if n := len(named(rec.take(), EventAlert)); n != 0 || len(s.FriendsHear().Banners) != 0 {
			t.Fatalf("%d alerts before the share, banners %+v", n, s.FriendsHear().Banners)
		}
		s.OnShareStarted(0, true)
		sess := startShare(t, eng, s)
		defer func() { _ = sess.Stop() }()
		settle(4 * time.Second)
		if n := len(named(rec.take(), EventAlert)); n != 0 {
			t.Fatalf("%d alerts before the failure", n)
		}
		settle(2 * time.Second)
		fh := s.FriendsHear()
		alerts := named(rec.take(), EventAlert)
		if len(alerts) != 1 || alerts[0].Payload.(Banner).Code != BannerExclusionUnavailable ||
			alerts[0].Payload.(Banner).Level != LevelError || !fh.Alert.VoiceAppsHeard || fh.Engine != "endpoint" {
			t.Fatalf("alerts %+v, FriendsHear %+v", alerts, fh)
		}
		if _, ok := banner(fh, BannerViewerMuted); !ok {
			t.Errorf("no viewer-muted banner: %+v", fh.Banners)
		}
		if r, g := rowByID(fh, "discord"); g != "heard" || r.Control != ControlLocked {
			t.Errorf("Discord %s %+v", g, r)
		}
		settle(30 * time.Second)
		if n := len(named(rec.take(), EventAlert)); n != 0 {
			t.Errorf("%d more alerts while the banner stays", n)
		}
		_ = sess.Stop()
		s.OnShareStopped()
		settle(time.Second)
		if fh := s.FriendsHear(); fh.Alert.VoiceAppsHeard || fh.Live {
			t.Errorf("after the share: %+v", fh.Alert)
		}
		if n := len(named(rec.take(), EventAlert)); n != 0 {
			t.Errorf("%d alerts after the share", n)
		}
		stop()
	})
}

// TestAudioAlertRepeat: an error banner present when the share starts alerts at once; one that goes and comes back
// within the share alerts again only 60 s after the last alert; a new share alerts again; nothing outside a share.
func TestAudioAlertRepeat(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctl := newStub()
		setProbe := func(result string) {
			ctl.set(func(r *media.AudioReport) {
				r.Probe.Result = result
				r.Engine = "include-set"
				if result == "failed" {
					r.Engine = "endpoint"
				}
			})
		}
		setProbe("failed")
		s := New(ctl, newStore(DefaultSettings()), testSelf, nil)
		rec := record(s)
		stop := runService(t, s)
		defer stop()
		_ = s.Watch(true)
		settle(time.Second)
		if _, ok := banner(s.FriendsHear(), BannerExclusionUnavailable); !ok || len(named(rec.take(), EventAlert)) != 0 {
			t.Fatal("the sheet's banner, or an alert outside the share")
		}
		alerts := func() int { return len(named(rec.take(), EventAlert)) }
		s.OnShareStarted(0, true)
		if n := alerts(); n != 1 {
			t.Fatalf("%d alerts as the share starts with the banner", n)
		}
		setProbe("ok") // gone …
		settle(time.Second)
		setProbe("failed") // … and back within 60 s
		settle(time.Second)
		if n := alerts(); n != 0 {
			t.Errorf("%d alerts for a banner back within 60 s", n)
		}
		setProbe("ok")
		settle(60 * time.Second)
		setProbe("failed") // back after 60 s
		settle(time.Second)
		if n := alerts(); n != 1 {
			t.Errorf("%d alerts for a banner back after 60 s", n)
		}
		s.OnShareStopped()
		s.OnShareStarted(0, true) // a new share
		if n := alerts(); n != 1 {
			t.Errorf("%d alerts in a new share", n)
		}
		s.OnShareStopped()
		stop()
	})
}
