package audio

import (
	"cmp"
	"slices"
	"strings"

	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/audiorules"
)

// This file builds FriendsHear (05 §14.5) from the engine's report and the service's state, with the banner and tip
// rules of 05 §14.7. It is pure: the Service calls it under its lock and compares the result with the last one.

// The report's values this file reads (05 §12.1).
const (
	verdictHeard       = "heard"
	verdictKeptOut     = "kept_out"
	verdictNotCaptured = "not_captured"

	engineEndpoint = "endpoint"
	engineNone     = "none"

	stateNoDevice = "no-device"

	probeFailed      = "failed"
	probeUnsupported = "unsupported"

	reasonMic           = "mic"
	reasonViewer        = "viewer"
	reasonCaptureFailed = "capture-failed"
)

// minExclusionBuild is the first Windows build with process loopback (2004, 19041): an unsupported probe below it is
// "Update Windows" (windows-too-old), at or above it a PC where voice apps can't be kept out (05 §14.7).
const minExclusionBuild = 19041

// maxPrompts is how many prompts FriendsHear lists (05 §14.5).
const maxPrompts = 3

// fhInput is what FriendsHear depends on.
type fhInput struct {
	report     media.AudioReport
	haveReport bool
	live       bool // between OnShareStarted and OnShareStopped
	withAudio  bool // the live share has an audio track
	soundOn    bool // Share sound
	paused     bool // SetPaused
	settings   Settings
	prompts    []Prompt // pending, oldest first
	// unknownViewer: a viewer tab on Windows plays with family safari, other or an unknown value (05 §5.9 step 3).
	unknownViewer bool
	ducking       duckingPref
}

// buildFriendsHear builds FriendsHear without its Seq.
func buildFriendsHear(in fhInput) FriendsHear {
	r := in.report
	fh := FriendsHear{
		Live: in.live, Mode: ModeExcludeApps, Engine: engineNone, KeepOutMicUsers: in.settings.KeepOutMicUsers,
		Heard: []AppRow{}, KeptOut: []AppRow{}, NotCaptured: []AppRow{}, Prompts: []Prompt{}, Banners: []Banner{},
		Tips: []Tip{}, VoiceApps: []VoiceAppInfo{},
	}
	if soundOff(in) {
		fh.Mode = ModeOff
	}
	if in.haveReport {
		if r.Engine != "" {
			fh.Engine = r.Engine
		}
		fh.Muted = r.Muted
		fh.Paused = r.Paused
		for _, a := range r.Apps {
			row := appRow(a, in.settings, r.Engine)
			switch a.Verdict {
			case verdictHeard:
				fh.Heard = append(fh.Heard, row)
			case verdictKeptOut:
				fh.KeptOut = append(fh.KeptOut, row)
			case verdictNotCaptured:
				fh.NotCaptured = append(fh.NotCaptured, row)
			} // "pending": seen < 20 ms ago, not classified yet; it shows with the next report
		}
	}
	fh.Paused = fh.Paused || (in.live && in.paused)
	for _, rows := range [][]AppRow{fh.Heard, fh.KeptOut, fh.NotCaptured} {
		slices.SortStableFunc(rows, compareRows)
	}
	fh.Prompts = append(fh.Prompts, in.prompts[:min(len(in.prompts), maxPrompts)]...)
	running := false
	for _, va := range audiorules.VoiceApps() {
		v := VoiceAppInfo{ID: va.ID, Name: va.Name}
		if in.haveReport {
			v.Running = slices.ContainsFunc(r.Apps, func(a media.AudioApp) bool {
				return a.ID == va.ID || va.Contains(rulesOS, a.Exe)
			})
		}
		running = running || v.Running
		fh.VoiceApps = append(fh.VoiceApps, v)
	}
	fh.Banners = banners(in, fh.NotCaptured)
	if tipDucking(in, running) {
		fh.Tips = append(fh.Tips, Tip{Code: TipDucking})
	}
	fh.Alert = Alert{Questions: len(fh.Prompts)}
	fh.Alert.VoiceAppsHeard = in.live && slices.ContainsFunc(fh.Banners, isErrorBanner)
	return fh
}

// soundOff reports whether the live share has no sound: started without, or Share sound off.
func soundOff(in fhInput) bool { return in.live && (!in.withAudio || !in.soundOn) }

// appRow is the FriendsHear row of a report row from a report with engine.
func appRow(a media.AudioApp, st Settings, engine string) AppRow {
	row := AppRow{
		ID: a.ID, Name: a.Name, Reason: reasonOf(a.Reason), Playing: a.Playing, UsesMic: a.UsesMic,
		Browser: a.Browser, Shared: a.Shared, Control: ControlToggle, Decision: st.decision(a.ID),
		Warning: warningOf(a.Warnings),
	}
	if row.Reason == "tree" || row.Reason == "pid-reuse" {
		row.Detail = a.Detail
	}
	if lockedApp(a.ID) || (a.Exe != "" && lockedApp(exePrefix+a.Exe)) || row.Reason == "builtin" ||
		row.Reason == "isshoni" {
		row.Control, row.Decision = ControlLocked, DecisionAuto
	}
	// 05 §14.6: a heard mic user carries included-uses-mic when the user included it, and also when it is heard
	// because "Keep out apps that use my microphone" is off. The DLL sets included_uses_mic only for a matched allow
	// rule, so the service works it out itself. Not for the shared app (shared-uses-mic), nor on the endpoint engine,
	// which hears every app whatever the rules.
	if row.Warning == "" && a.Verdict == verdictHeard && a.UsesMic && !a.Shared && engine != engineEndpoint &&
		row.Control == ControlToggle && (row.Decision == DecisionInclude || !st.KeepOutMicUsers) {
		row.Warning = WarningIncludedUsesMic
	}
	return row
}

// reasonOf maps a report reason (05 §12.1) to FriendsHear's (05 §14.5); an unknown one is "".
func reasonOf(r string) string {
	switch r {
	case "builtin", "user", reasonMic, reasonViewer, "isshoni", "launcher", "tree", "limit":
		return r
	case "pid_reuse":
		return "pid-reuse"
	case "capture_failed":
		return reasonCaptureFailed
	}
	return ""
}

// warningOf picks a row's warning from the report's (05 §12.1); multi_process_session is not shown.
func warningOf(ws []string) string {
	for _, w := range []struct{ report, row string }{
		{"shared_app_uses_mic", WarningSharedUsesMic},
		{"shared_app_kept_out", WarningSharedKeptOut},
		{"included_uses_mic", WarningIncludedUsesMic},
	} {
		if slices.Contains(ws, w.report) {
			return w.row
		}
	}
	return ""
}

// compareRows orders a FriendsHear group: the shared app first, then the playing apps, then by name (ignoring case),
// then by id.
func compareRows(a, b AppRow) int {
	if a.Shared != b.Shared {
		if a.Shared {
			return -1
		}
		return 1
	}
	if a.Playing != b.Playing {
		if a.Playing {
			return -1
		}
		return 1
	}
	return cmp.Or(strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), strings.Compare(a.Name, b.Name),
		strings.Compare(a.ID, b.ID))
}

// banners applies 05 §14.7's banner rules, in the table's order.
func banners(in fhInput, notCaptured []AppRow) []Banner {
	r := in.report
	var bs []Banner
	add := func(code, level string, params map[string]any) {
		bs = append(bs, Banner{Code: code, Level: level, Params: params})
	}
	if in.haveReport {
		switch {
		case r.Probe.Result == probeFailed || (r.Probe.Result == probeUnsupported && r.Probe.OSBuild >= minExclusionBuild):
			add(BannerExclusionUnavailable, LevelError, nil)
		case r.Probe.Result == probeUnsupported:
			add(BannerWindowsTooOld, LevelError, nil)
		case r.Engine == engineEndpoint:
			// Defensive: the endpoint engine hears everything, voice apps included, whatever the probe says (05 §15,
			// e.g. the Win10 build gate). Degraded modes are always shown.
			add(BannerExclusionUnavailable, LevelError, nil)
		}
		if r.State == stateNoDevice {
			add(BannerNoOutputDevice, LevelWarning, nil)
		}
		if r.Rebuilding {
			add(BannerReconnectingSound, LevelInfo, nil)
		}
	}
	switch {
	case soundOff(in):
		add(BannerSoundOff, LevelInfo, map[string]any{"cause": "off"})
	case in.live && (in.paused || (in.haveReport && (r.Paused || r.Muted))):
		add(BannerSoundOff, LevelInfo, map[string]any{"cause": "paused"})
	}
	if i := slices.IndexFunc(notCaptured, func(a AppRow) bool { return a.Reason == reasonCaptureFailed && a.Playing }); i >= 0 {
		add(BannerCaptureTrouble, LevelWarning, map[string]any{"app": notCaptured[i].Name})
	}
	if in.live && in.haveReport && r.Engine == engineEndpoint {
		add(BannerViewerMuted, LevelInfo, nil)
	}
	if in.unknownViewer {
		add(BannerViewerUnknownBrowser, LevelWarning, nil)
	}
	if bs == nil {
		bs = []Banner{}
	}
	return bs
}

// isErrorBanner reports whether b is error-level (exclusion-unavailable, windows-too-old).
func isErrorBanner(b Banner) bool { return b.Level == LevelError }

// tipDucking is 05 §14.7's tip rule: Windows ducks other sounds (the preference is not "Do nothing", or absent), a mic
// user or a voice app runs, and the user hasn't dismissed the tip.
func tipDucking(in fhInput, voiceAppRunning bool) bool {
	if in.ducking != duckingReduces || !in.haveReport || slices.Contains(in.settings.DismissedTips, TipDucking) {
		return false
	}
	return voiceAppRunning || slices.ContainsFunc(in.report.Apps, func(a media.AudioApp) bool { return a.UsesMic })
}
