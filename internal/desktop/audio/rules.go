package audio

import (
	"maps"
	"slices"
	"strconv"

	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/audiorules"
)

// ruleInput is everything the rule set depends on (05 §14.3).
type ruleInput struct {
	self      SelfInfo
	settings  Settings // normalized
	sharedPID uint32
	// viewerExes are the exes kept out for the user's isshoni viewer (05 §5.9), per-run overrides already removed.
	viewerExes []string
}

// buildExclusions is the rules builder of 05 §14.3. The rows, in order:
//  1. Self: the instance rule on SelfInfo.PID, isshoni.exe, and the WebView2 data folder, all as app "isshoni".
//  2. Data: a stop rule per launcher, the shared runtime msedgewebview2.exe, a browser rule per browser exe.
//  3. Built-ins: an exclude rule per voice-app exe, with the app's id and name.
//  4. User: keep-out → an exclude rule, include → an allow rule (source USER), by app id.
//  5. Viewer: an exclude rule (source VIEWER) per viewer exe. A saved include for the same exe stays a user allow
//     rule, which the viewer rule beats (05 §5.3's row 4 above row 5): only the per-run override removes a viewer
//     rule.
//  6. The flags from the settings, and SharedPID.
//
// It never holds more than 512 rules: 3 self rules, 35 + 1 + 13 data rules and 14 built-in ones (05 §5.4), at most
// 256 decisions (05 §14.1) and 13 viewer rules.
func buildExclusions(in ruleInput) media.Exclusions {
	ex := media.Exclusions{
		KeepOutMicUsers: in.settings.KeepOutMicUsers,
		FastPath:        in.settings.FastPath,
		SharedPID:       in.sharedPID,
	}
	add := func(kind media.RuleKind, src media.RuleSource, value, appID, name string) {
		ex.Rules = append(ex.Rules, media.Rule{Kind: kind, Source: src, Value: value, AppID: appID, Name: name})
	}

	// 1. Self.
	if in.self.PID != 0 {
		add(media.RuleInstance, media.SourceSelf, strconv.FormatUint(uint64(in.self.PID), 10), selfAppID, selfName)
	}
	add(media.RuleAppExclude, media.SourceSelf, selfExe, selfAppID, selfName)
	if in.self.WebViewDataDir != "" {
		add(media.RuleWebViewDataDir, media.SourceSelf, in.self.WebViewDataDir, selfAppID, selfName)
	}

	// 2. Data.
	for _, exe := range audiorules.Launchers().For(rulesOS) {
		add(media.RuleStop, media.SourceData, exe, "", "")
	}
	for _, exe := range audiorules.SharedRuntimes().For(rulesOS) {
		add(media.RuleSharedRuntime, media.SourceData, exe, "", "")
	}
	for _, b := range audiorules.Browsers() {
		for _, exe := range b.For(rulesOS) {
			add(media.RuleBrowser, media.SourceData, exe, "", "")
		}
	}

	// 3. Built-ins.
	for _, va := range audiorules.VoiceApps() {
		for _, exe := range va.For(rulesOS) {
			add(media.RuleAppExclude, media.SourceBuiltin, exe, va.ID, va.Name)
		}
	}

	// 4. User decisions, by app id (the settings are normalized: valid, unlocked exe ids only).
	for _, id := range slices.Sorted(maps.Keys(in.settings.AppRules)) {
		exe, ok := exeOf(id)
		if !ok || !validAppID(id) || lockedApp(id) {
			continue
		}
		switch in.settings.AppRules[id] {
		case DecisionKeepOut:
			add(media.RuleAppExclude, media.SourceUser, exe, "", "")
		case DecisionInclude:
			add(media.RuleAppAllow, media.SourceUser, exe, "", "")
		}
	}

	// 5. Viewer.
	for _, exe := range in.viewerExes {
		add(media.RuleAppExclude, media.SourceViewer, exe, "", "")
	}
	return ex
}

// sameExclusions reports whether x and y are the same rule set.
func sameExclusions(x, y media.Exclusions) bool {
	return x.KeepOutMicUsers == y.KeepOutMicUsers && x.FastPath == y.FastPath && x.SharedPID == y.SharedPID &&
		slices.Equal(x.Rules, y.Rules)
}

// cloneExclusions returns a copy of ex that shares no slice with it.
func cloneExclusions(ex media.Exclusions) media.Exclusions {
	ex.Rules = slices.Clone(ex.Rules)
	return ex
}
