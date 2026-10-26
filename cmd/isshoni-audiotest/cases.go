package main

// This file is the audiotest case table (docs/m2/05-windows-audio.md §18.5): the OS-neutral checks and the Windows
// fixture setup of every case, 1–27. WA62a wrote the cases that need no microphone (1–8, 10, 11, 15–21 and 24–27);
// WA62b added the mic cases 9 and 12–14, the multi-endpoint case 22 and the soak case 23 (`run -long` only). M3 and
// M4 add their own fixtures later.
//
// A case is pure data: its checks drive the portable verdict (verdict.go) on a recording, and its fixture plan
// drives the Windows driver (driver_windows.go, through fixtureplan.go). The frequencies are 05 §18.3's (all
// −18 dBFS, ≥ 90 Hz apart), except isshoni's own tone (freqSelf).

// The fixture frequencies (05 §18.3).
//
// freqSelf is not 05 §18.3's 907 Hz: a −18 dBFS 1000 Hz tone with the mixer's 5 ms onset or fade reads up to about
// −44 dBFS at 907 Hz in a 50 ms window, over the absent limit of −50 dBFS (WA62a's measurement; 05 §18.6's "at most
// about −64 dBFS" does not hold). The integrator decided for group 6 to move isshoni's tone at least 200 Hz from the
// engine's 1000 Hz test tone and from every other fixture tone. 1250 Hz, the suggested value, lies 70 Hz from case
// 27's 1320 Hz chime partial, so the tone is 1700 Hz: 300 Hz from the bursts' 2000 Hz, 380 Hz from 1320 Hz, and
// clear of the second harmonics of 683 and 1000 Hz, the tones it shares cases 4, 10 and 26 with. TestSelfToneSpacing
// pins the rule, and TestFadedToneNeverTripsSelf the analyzer's expectation.
const (
	freqGame       = 1000 // the game friends should hear
	freqLateGame   = 683  // game2, the late child of case 25, and case 22's tone on the second endpoint
	freqVoice      = 440  // the voice app, and the browser voice tab
	freqVoice2     = 571  // the second voice app
	freqBrowserTab = 797  // the browser's "other tab" (and the harness's own tone in case 10)
	freqSelf       = 1700 // isshoni's own playback (and the late isshoni child of case 26); see above
	freqChimeLow   = 880  // case 27's sound-check chime partials (§11.3)
	freqChimeHigh  = 1320
	freqBursts     = 2000 // case 20's bursts (im_test_render_bursts)
)

// selfSpacingHz is the least distance between isshoni's own tone and any other fixture tone (the group 6 decision).
const selfSpacingHz = 200

// soakAMHz is the slow amplitude modulation of the soak's music-like fixture (05 §18.5 case 23): one period per
// 10 s loop of its WAV, whose integer-cycle carriers make the loop seamless.
const soakAMHz = 0.1

// soakSec is the soak case's recording length (05 §18.5: 30 min).
const soakSec = 1800.0

// The soak's budgets (05 §18.5 case 23): the counters that must stay zero, and the resampler's ppm ceiling.
const soakMaxResamplePpm = 1000

// requirement is how a case contributes to the gate exit code (05 §18.5, §18.10).
type requirement int

const (
	reqYes       requirement = iota // required
	reqFastPath                     // required only when the gate ships audio.fastPath on (cases 4, 5, 26)
	reqMic                          // required, but SKIPs without a microphone with desktop-app access (9, 12–14)
	reqInfo                         // informational: never changes the exit code (22, 27)
	reqTierMinor                    // a minor-release tier case (23, the soak): never changes the exit code
)

// The check expectations (05 §18.6).
const (
	expectPresent = "present"
	expectAbsent  = "absent"
	expectInfo    = "info"
)

// The positive-control verdicts a check asserts about its fixture role (05 §18.6, §12.1). controlNotKeptOut is a
// present check's control (§18.6: "must not be kept_out in any sample inside that check's window"); controlNone
// asserts nothing about the verdict (the endpoint engine keeps the policy's verdict while it hears every app, so
// cases 1 and 19 check the sound and the engine, not the row).
const (
	controlHeard       = "heard"
	controlKeptOut     = "kept_out"
	controlNotCaptured = "not_captured"
	controlNotKeptOut  = "not_kept_out"
	controlNone        = "none"
)

// selfRole is the role of the harness's own process: it plays a tone itself in cases 4 and 10 (05 §18.5), and its
// rows are isshoni's.
const selfRole = "self"

// The FriendsHear prompt kinds a case checks (05 §14.6; internal/desktop/audio's PromptMic and PromptBrowserMic,
// pinned equal by TestPromptKindsMatchTheService).
const (
	promptMic        = "mic"
	promptBrowserMic = "browser-mic"
)

// eventTSet names the recorder's SetExclusions stamp as a window bound or a measurement event (cases 13 and 15's
// T_set: the first stamp after the case's Decide or SetAppDecision).
const eventTSet = "T_set"

// sessionPrefix marks a bound or measurement event as a role's session_created engine event (cases 8, 25).
const sessionPrefix = "session:"

// bound is a check-window bound or a measurement event (05 §18.6). An absolute bound has event "" and a value in
// abs. An event bound names a role (its firstRenderNs), "session:<role>" (its session_created), or eventTSet (the
// recorder's SetExclusions stamp), plus an offset in seconds. A zero to-bound (event "", abs 0) means the end of the
// recording.
type bound struct {
	event  string
	offset float64
	abs    float64
}

// at is an absolute bound in seconds.
func at(s float64) bound { return bound{abs: s} }

// end is the to-bound meaning the end of the recording.
func end() bound { return bound{} }

// afterTSet is a bound at the recorder's SetExclusions stamp plus offset seconds.
func afterTSet(offset float64) bound { return bound{event: eventTSet, offset: offset} }

// checkSpec is one frequency check of a case (05 §18.5, §18.6). role is the fixture role that plays freq (selfRole
// for the harness itself). ctl overrides the control derived from expect (present → not kept out, absent → kept
// out, info → none); reason is the expected session reason for that control.
type checkSpec struct {
	freq   float64
	from   bound
	to     bound
	expect string
	role   string
	ctl    string
	reason string
}

// rowCheck is an app-row or session expectation for a fixture role (05 §18.5: "Discord row kept_out/builtin", "the
// child's session note = covered", "warning shared_app_uses_mic"): every report sample at or after from (zero =
// 1.5 s) in which the role's session is active must show it. verdict, reason, detail, warning and browser are the
// app row's (looked up by the session's app id); note is the render session's. A role whose only session is a
// microphone session (Steam in case 9) is checked through that session's app row. An empty field asserts nothing.
type rowCheck struct {
	role    string
	verdict string
	reason  string
	detail  string
	warning string // one of the row's warnings (05 §12.1's apps[].warnings)
	browser bool   // the row is a browser's
	note    string
	from    bound
}

// promptCheck is a "the mic prompt existed" expectation (05 §18.5 cases 12 and 13): some report sample in
// [from, to] (zero values: [1.5 s, end]) shows a pending FriendsHear prompt of kind for the role's app.
type promptCheck struct {
	role string
	kind string // audio.PromptMic | audio.PromptBrowserMic
	from bound
	to   bound
}

// decideCall is a mid-run Service.Decide a case makes (05 §18.5 case 13): at t seconds the harness answers the
// pending prompt of role's app with decision; the recorder's next SetExclusions stamp is the case's T_set.
type decideCall struct {
	at       float64
	role     string
	decision string // "keep-out" | "include"
}

// measureSpec is one event-relative value a case records and asserts (05 §17, §18.6, §18.7).
type measureSpec struct {
	key    string  // measureCut | measureOnset | measureSessionLag | measureFastpathExit
	freq   float64 // cut/onset only
	event  bound   // the event a cut or onset is measured from
	role   string  // session lag and fast-path exit: the fixture whose session_created and firstRenderNs count
	budget int     // ms
}

// fixtureStep is one fixture the Windows driver starts for a case (05 §18.3). It is declarative data; fixtureplan.go
// turns it into a `fixture` command line. role is the fixture's redacted id ("fixture:<role>"). Steps nest through
// child: a step with a child is a spawn, which can play a tone itself (`spawn -tone`).
type fixtureStep struct {
	role          string
	exe           string    // relative path under the work folder, e.g. `Discord\app-1.0.9999\discord.exe`
	verb          string    // verbTone | verbIdle | verbSpawn
	freqs         []float64 // the tone (a spawn's -tone)
	amp           float64   // 0 = the default −18 dBFS
	am            float64   // > 0: a tone looped with this slow amplitude modulation (Hz), the soak's music
	at            float64   // start time relative to the recording start; ≤ 0 = with its parent (top level: at the settle)
	dur           float64   // how long it plays or lives (s); 0 = until the case ends
	mic           bool
	orphanAfter   float64 // > 0: a spawn exits this long after it started (orphan case)
	child         *fixtureStep
	underExplorer bool // top-level fixtures are reparented under explorer.exe; a harness-own child is not
}

// The fixture verbs (05 §18.3).
const (
	verbTone         = "tone"
	verbIdle         = "idle"
	verbSpawn        = "spawn"
	verbReusePID     = "reuse-pid"
	verbBursts       = "bursts"
	verbEndpointTone = "endpoint-tone"
)

// caseSpec is one audiotest case (05 §18.5).
type caseSpec struct {
	num  int
	name string
	desc string
	req  requirement

	checks       []checkSpec
	rowChecks    []rowCheck
	promptChecks []promptCheck
	measures     []measureSpec

	// engineThroughout, if set, must be the report's engine in every sample inside [expectFrom, end]
	// ("include-set", "exclude-self", "endpoint"); "" = any. 05 §18.6 limits the expectations to the window: right
	// after Start the engine can still be "none", or (with the fail-probe flag) the include set until the probe
	// fails.
	engineThroughout string
	// bannerThroughout, if set, must be present in every sample's banners inside [expectFrom, end] (case 19).
	bannerThroughout string
	// expectFrom is where the engine and banner expectations start (zero = 1.5 s, the default check window).
	expectFrom bound
	// counterZero are report counters that must stay 0 (case 5's fastpathLateCuts; the soak's underruns, trims and
	// mixerResyncs).
	counterZero []string
	// counterMax are report counters with a ceiling (the soak's maxResamplePpm ≤ 1000).
	counterMax map[string]int

	// clickFrom, clickTo bound the click window; the zero value is the default [1.5 s, end]. noClick disables the
	// click check (05 §18.5: "unless marked").
	clickFrom bound
	clickTo   bound
	noClick   bool

	// forceFastPath makes the engine run with Exclusions.FastPath on regardless of the shipped default (cases 4, 5,
	// 26 test the fast path even when the release ships it off, §18.10). Such a case is INCONCLUSIVE when a
	// non-fixture process is kept out (05 §18.4).
	forceFastPath bool
	// failProbe makes the engine behave as if the startup probe failed (IM_AUDIOF_TEST_FAIL_PROBE: cases 1, 19).
	failProbe bool
	// seedInclude seeds the settings store with these exe app ids as "include" before the Service starts (case 24).
	seedInclude []string
	// viewer, if set, is a SetViewerBrowser call the case makes (case 11, 24): {connID, family, os}.
	viewer *viewerCall
	// selfTone, if set, is a frequency the harness plays itself through PlaySoundW for the whole case, from the
	// settle on (cases 4 and 10): isshoni's own playback, role selfRole.
	selfTone float64
	// sharedRole, if set, is the fixture whose pid the case passes to Service.OnShareStarted as the shared app
	// (case 14), instead of 0 (a monitor).
	sharedRole string
	// decide, if set, is a Service.Decide the case makes mid-run (case 13).
	decide *decideCall

	// special, if set, routes the case to the driver's own handler instead of the tone verdict: "timestamps"
	// (case 20), "opus" (case 21), "multi-endpoint" (case 22), "soak" (case 23), "pid-reuse" (case 27).
	special string
	// long marks a case that runs only with `run -long` (the soak, 05 §18.2).
	long bool
	// minEndpoints is how many active render endpoints the case needs; with fewer it SKIPs (case 22: 2).
	minEndpoints int

	// fixtures is the Windows fixture plan (fixtureplan.go, driver_windows.go).
	fixtures []fixtureStep
	// driverRoles are the roles of fixtures the driver starts itself, outside the plan (case 20's bursts, case 22's
	// endpoint tone), so checks may name them.
	driverRoles []string
}

// needsMic reports whether cs needs a microphone with desktop-app access (05 §18.4): a mic case, or a case with a
// fixture that holds the mic (the soak's idle voice app). Without one the case SKIPs.
func (cs caseSpec) needsMic() bool {
	if cs.req == reqMic {
		return true
	}
	var walk func(s *fixtureStep) bool
	walk = func(s *fixtureStep) bool {
		return s.mic || (s.child != nil && walk(s.child))
	}
	for i := range cs.fixtures {
		if walk(&cs.fixtures[i]) {
			return true
		}
	}
	return false
}

// viewerCall is a Service.SetViewerBrowser call a case makes (05 §5.9; cases 11, 24).
type viewerCall struct {
	connID, family, os string
}

// present and absent build the common checks over the default window [1.5 s, end].
func present(freq float64, role string) checkSpec {
	return checkSpec{freq: freq, from: at(1.5), to: end(), expect: expectPresent, role: role, ctl: controlNotKeptOut}
}

func absent(freq float64, role string) checkSpec {
	return checkSpec{freq: freq, from: at(1.5), to: end(), expect: expectAbsent, role: role, ctl: controlKeptOut}
}

// selfAbsent is an absent check for a frequency the harness itself plays (cases 4 and 10): its session must be
// active and kept out with reason isshoni.
func selfAbsent(freq float64) checkSpec {
	return checkSpec{freq: freq, from: at(1.5), to: end(), expect: expectAbsent, role: selfRole, ctl: controlKeptOut,
		reason: "isshoni"}
}

// The fixture paths of 05 §18.3's work-folder layout.
const (
	exeGame      = `Games\game.exe`
	exeGame2     = `Games2\game2.exe`
	exeDiscord   = `Discord\app-1.0.9999\discord.exe`
	exeVoiceHelp = `Discord\app-1.0.9999\voicehelper.exe`
	exeAudioHelp = `Discord\app-1.0.9999\audiohelper.exe`
	exeTeamSpeak = `TeamSpeak\teamspeak.exe`
	exeTeams     = `Teams\ms-teams.exe`
	exeWebView2  = `WebView2Runtime\msedgewebview2.exe`
	exeEdge      = `Microsoft\Edge\Application\msedge.exe`
	exeFakeVoice = `FakeVoice\fakevoice.exe`
	exeLauncherX = `LauncherX\launcherx.exe`
	exeBrowser   = `Browser\browser.exe`
	exeSelfWV    = `SelfWebView\msedgewebview2.exe`
	exeBursts    = `Bursts\bursts.exe`
	exeSteam     = `Steam\steam.exe`
	exeSteamGame = `Steam\steamapps\common\TestGame\game.exe`
	exeChrome    = `Google\Chrome\Application\chrome.exe`
	exeKook      = `KOOK\kook.exe`
)

// cases returns the case table, in case-number order (05 §18.5).
func cases() []caseSpec {
	return []caseSpec{
		{
			num: 1, name: "baseline-endpoint", req: reqYes, failProbe: true,
			desc:             "endpoint engine (fail-probe); game and a voice app both heard: the audio setup works",
			engineThroughout: "endpoint",
			checks: []checkSpec{
				present(freqGame, "game"),
				// The endpoint engine hears Discord while the planner keeps the policy's verdict (kept_out/builtin):
				// the sound and engine "endpoint" are what this case proves, not the row.
				{freq: freqVoice, from: at(1.5), to: end(), expect: expectPresent, role: "voice", ctl: controlNone},
			},
			fixtures: []fixtureStep{
				explorerTone("game", exeGame, freqGame),
				explorerTone("voice", exeDiscord, freqVoice),
			},
		},
		{
			num: 2, name: "builtin-voice", req: reqYes,
			desc: "a built-in voice app is kept out; the game is heard",
			checks: []checkSpec{
				present(freqGame, "game"),
				{freq: freqVoice, from: at(1.5), to: end(), expect: expectAbsent, role: "voice",
					ctl: controlKeptOut, reason: "builtin"},
			},
			rowChecks: []rowCheck{{role: "voice", verdict: controlKeptOut, reason: "builtin"}},
			fixtures: []fixtureStep{
				explorerTone("game", exeGame, freqGame),
				explorerTone("voice", exeDiscord, freqVoice),
			},
		},
		{
			num: 3, name: "two-voice-apps", req: reqYes, engineThroughout: "include-set",
			desc: "two voice apps kept out at once, the game heard",
			checks: []checkSpec{
				present(freqGame, "game"), absent(freqVoice, "voice"), absent(freqVoice2, "voice2"),
			},
			fixtures: []fixtureStep{
				explorerTone("game", exeGame, freqGame),
				explorerTone("voice", exeDiscord, freqVoice),
				explorerTone("voice2", exeTeamSpeak, freqVoice2),
			},
		},
		{
			num: 4, name: "fast-path", req: reqFastPath, forceFastPath: true, engineThroughout: "exclude-self",
			selfTone: freqSelf,
			desc:     "the fast path: one EXCLUDE stream on isshoni's tree; every other app heard from its first sample",
			checks: []checkSpec{
				present(freqGame, "game"), present(freqLateGame, "game2"), selfAbsent(freqSelf),
			},
			fixtures: []fixtureStep{
				explorerTone("game", exeGame, freqGame),
				explorerTone("game2", exeGame2, freqLateGame),
			},
		},
		{
			num: 5, name: "fast-path-late-voice", req: reqFastPath, forceFastPath: true,
			desc: "a voice app that starts mid-share never leaks under the fast path; the session lag is measured",
			checks: []checkSpec{
				present(freqGame, "game"),
				absent(freqVoice, "voice"),
			},
			measures: []measureSpec{
				{key: measureFastpathExit, role: "voice", budget: budgetFastpathExitMs},
				{key: measureSessionLag, role: "voice", budget: budgetSessionLagMs},
			},
			counterZero: []string{"fastpathLateCuts"},
			fixtures: []fixtureStep{
				explorerTone("game", exeGame, freqGame),
				explorerTone("game2", exeGame2, freqLateGame), // "as 4 without" isshoni's own tone
				{role: "voice", exe: exeDiscord, verb: verbTone, freqs: []float64{freqVoice}, at: 3,
					underExplorer: true},
			},
		},
		{
			num: 6, name: "helper-tree", req: reqYes,
			desc: "a grandchild helper and a shared-runtime helper of voice apps are both kept out",
			checks: []checkSpec{
				present(freqGame, "game"), absent(freqVoice, "audiohelper"), absent(freqVoice2, "teamsRuntime"),
			},
			fixtures: []fixtureStep{
				explorerTone("game", exeGame, freqGame),
				{role: "discord", exe: exeDiscord, verb: verbSpawn, underExplorer: true,
					child: &fixtureStep{role: "voicehelper", exe: exeVoiceHelp, verb: verbSpawn,
						child: &fixtureStep{role: "audiohelper", exe: exeAudioHelp, verb: verbTone,
							freqs: []float64{freqVoice}}}},
				{role: "teams", exe: exeTeams, verb: verbSpawn, underExplorer: true,
					child: &fixtureStep{role: "teamsRuntime", exe: exeWebView2, verb: verbTone,
						freqs: []float64{freqVoice2}}},
			},
		},
		{
			num: 7, name: "late-helper", req: reqYes,
			desc: "a helper that a running voice app spawns mid-share is kept out from its first sample",
			checks: []checkSpec{
				present(freqGame, "game"),
				absent(freqVoice, "helper"),
			},
			fixtures: []fixtureStep{
				explorerTone("game", exeGame, freqGame),
				// discord.exe runs from the start and spawns its helper at t = 3 s (`spawn -at 4`, 05 §18.5).
				{role: "discord", exe: exeDiscord, verb: verbSpawn, underExplorer: true,
					child: &fixtureStep{role: "helper", exe: exeAudioHelp, verb: verbTone,
						freqs: []float64{freqVoice}, at: 3}},
			},
		},
		{
			num: 8, name: "late-app", req: reqYes,
			desc: "an app that starts mid-share is heard within 500 ms of its session; the onset has no click",
			checks: []checkSpec{
				{freq: freqLateGame, from: at(3.5), to: at(5.8), expect: expectPresent, role: "game2",
					ctl: controlNotKeptOut},
			},
			measures: []measureSpec{
				{key: measureOnset, freq: freqLateGame, event: bound{event: sessionPrefix + "game2"},
					budget: budgetOnsetMs},
			},
			fixtures: []fixtureStep{
				{role: "discord", exe: exeDiscord, verb: verbIdle, underExplorer: true},
				{role: "game2", exe: exeGame2, verb: verbTone, freqs: []float64{freqLateGame}, at: 3, dur: 3,
					underExplorer: true},
			},
		},
		{
			num: 9, name: "steam-game", req: reqMic,
			desc: "a game under Steam's folder is heard while Steam, holding the mic, is kept out (the launcher stop)",
			checks: []checkSpec{
				present(freqGame, "game"),
			},
			// Steam has only a microphone session: its row is read through that session's app id.
			rowChecks: []rowCheck{
				{role: "steam", verdict: controlKeptOut, reason: "mic"},
				{role: "game", verdict: controlHeard},
			},
			fixtures: []fixtureStep{
				// `Steam\steam.exe` (`spawn -mic`) → `steamapps\common\TestGame\game.exe` playing 1000 (05 §18.5).
				{role: "steam", exe: exeSteam, verb: verbSpawn, mic: true, underExplorer: true,
					child: &fixtureStep{role: "game", exe: exeSteamGame, verb: verbTone, freqs: []float64{freqGame}}},
			},
		},
		{
			num: 10, name: "isshoni-self", req: reqYes, selfTone: freqBrowserTab,
			desc: "isshoni's own playback and its WebView2 child are never captured",
			checks: []checkSpec{
				present(freqGame, "game"),
				selfAbsent(freqBrowserTab),
				{freq: freqSelf, from: at(1.5), to: end(), expect: expectAbsent, role: "selfWebview",
					ctl: controlKeptOut, reason: "isshoni"},
			},
			rowChecks: []rowCheck{
				{role: selfRole, verdict: controlKeptOut, reason: "isshoni"},
				{role: "selfWebview", verdict: controlKeptOut, reason: "isshoni"},
			},
			fixtures: []fixtureStep{
				explorerTone("game", exeGame, freqGame),
				// The self WebView is the harness's own child (not reparented), with a child that plays isshoni's tone.
				{role: "selfWebviewParent", exe: exeSelfWV, verb: verbSpawn,
					child: &fixtureStep{role: "selfWebview", exe: exeSelfWV, verb: verbTone,
						freqs: []float64{freqSelf}}},
			},
		},
		viewerCase(11, "viewer-browser", "a browser that plays the user's isshoni viewer is kept out whole, reason viewer",
			nil),
		{
			num: 12, name: "browser-mic", req: reqMic,
			desc: "a browser whose tab holds the mic is kept out whole (reason mic) and asked about (browser-mic)",
			checks: []checkSpec{
				present(freqGame, "game"),
				{freq: freqVoice, from: at(1.5), to: end(), expect: expectAbsent, role: "discordWeb",
					ctl: controlKeptOut, reason: "mic"},
				{freq: freqBrowserTab, from: at(1.5), to: end(), expect: expectAbsent, role: "otherTab",
					ctl: controlKeptOut, reason: "mic"},
			},
			rowChecks:    []rowCheck{{role: "discordWeb", verdict: controlKeptOut, reason: "mic", browser: true}},
			promptChecks: []promptCheck{{role: "discordWeb", kind: promptBrowserMic}},
			fixtures: []fixtureStep{
				explorerTone("game", exeGame, freqGame),
				// chrome.exe (spawn) → chrome.exe (`spawn -tone 440 -mic`, Discord Web) → chrome.exe playing 797.
				{role: "chrome", exe: exeChrome, verb: verbSpawn, underExplorer: true,
					child: &fixtureStep{role: "discordWeb", exe: exeChrome, verb: verbSpawn,
						freqs: []float64{freqVoice}, mic: true,
						child: &fixtureStep{role: "otherTab", exe: exeChrome, verb: verbTone,
							freqs: []float64{freqBrowserTab}}}},
			},
		},
		{
			num: 13, name: "mic-user", req: reqMic,
			desc: "an unknown app that holds the mic is kept out and asked about; answering include fades it in",
			checks: []checkSpec{
				present(freqGame, "game"),
				{freq: freqVoice, from: at(1.5), to: at(5.0), expect: expectAbsent, role: "kook",
					ctl: controlKeptOut, reason: "mic"},
				{freq: freqVoice, from: at(5.6), to: end(), expect: expectPresent, role: "kook",
					ctl: controlNotKeptOut},
			},
			measures: []measureSpec{
				{key: measureOnset, freq: freqVoice, event: afterTSet(0), budget: budgetOnsetMs},
			},
			promptChecks: []promptCheck{{role: "kook", kind: promptMic, to: at(5.0)}},
			decide:       &decideCall{at: 5, role: "kook", decision: "include"},
			fixtures: []fixtureStep{
				explorerTone("game", exeGame, freqGame),
				{role: "kook", exe: exeKook, verb: verbTone, freqs: []float64{freqVoice}, mic: true,
					underExplorer: true},
			},
		},
		{
			num: 14, name: "shared-app-mic", req: reqMic, sharedRole: "game",
			desc: "the shared app is heard although it holds the mic, with the shared_app_uses_mic warning",
			checks: []checkSpec{
				present(freqGame, "game"),
			},
			rowChecks: []rowCheck{{role: "game", verdict: controlHeard, warning: "shared_app_uses_mic"}},
			fixtures: []fixtureStep{
				{role: "game", exe: exeGame, verb: verbTone, freqs: []float64{freqGame}, mic: true,
					underExplorer: true},
			},
		},
		{
			num: 15, name: "runtime-rule", req: reqYes,
			desc: "adding a keep-out rule mid-share fades the app out within 100 ms, with no click",
			checks: []checkSpec{
				present(freqGame, "game"),
				{freq: freqVoice, from: at(1.5), to: at(4.0), expect: expectPresent, role: "fakevoice",
					ctl: controlNotKeptOut},
				{freq: freqVoice, from: afterTSet(0.1), to: end(), expect: expectAbsent, role: "fakevoice",
					ctl: controlKeptOut},
			},
			measures: []measureSpec{
				{key: measureCut, freq: freqVoice, event: afterTSet(0), budget: budgetCutMs},
			},
			fixtures: []fixtureStep{
				explorerTone("game", exeGame, freqGame),
				explorerTone("fakevoice", exeFakeVoice, freqVoice),
			},
		},
		{
			num: 16, name: "app-boundary", req: reqYes,
			desc: "an app a voice app launched (another folder) is still heard",
			checks: []checkSpec{
				present(freqGame, "browser"),
			},
			fixtures: []fixtureStep{
				{role: "discord", exe: exeDiscord, verb: verbSpawn, underExplorer: true,
					child: &fixtureStep{role: "browser", exe: exeBrowser, verb: verbTone,
						freqs: []float64{freqGame}}},
			},
		},
		{
			num: 17, name: "tree-contains-excluded", req: reqYes,
			desc: "a launcher whose tree holds a voice app is not captured (tree), and the voice app is kept out",
			checks: []checkSpec{
				present(freqGame, "game"),
				absent(freqVoice, "discord"),
				// 683's level is recorded only (INFO: not heard by design, §6.2); the row check below is required.
				{freq: freqLateGame, from: at(1.5), to: end(), expect: expectInfo, role: "launcherx", ctl: controlNone},
			},
			rowChecks: []rowCheck{{role: "launcherx", verdict: controlNotCaptured, reason: "tree", detail: "Discord"}},
			fixtures: []fixtureStep{
				explorerTone("game", exeGame, freqGame),
				// LauncherX plays 683 itself (`spawn -tone 683`) and starts discord.exe, which plays 440.
				{role: "launcherx", exe: exeLauncherX, verb: verbSpawn, freqs: []float64{freqLateGame},
					underExplorer: true,
					child: &fixtureStep{role: "discord", exe: exeDiscord, verb: verbTone,
						freqs: []float64{freqVoice}}},
			},
		},
		{
			num: 18, name: "orphan", req: reqYes,
			desc: "a helper whose parent exits mid-share stays kept out (lineage cache)",
			checks: []checkSpec{
				absent(freqVoice, "voicehelper"),
			},
			fixtures: []fixtureStep{
				{role: "discord", exe: exeDiscord, verb: verbSpawn, orphanAfter: 4, underExplorer: true,
					child: &fixtureStep{role: "voicehelper", exe: exeVoiceHelp, verb: verbTone,
						freqs: []float64{freqVoice}}},
			},
		},
		{
			num: 19, name: "probe-fallback", req: reqYes, failProbe: true,
			desc:             "a failed probe falls back to endpoint loopback with a banner, and really captures",
			engineThroughout: "endpoint", bannerThroughout: "exclusion-unavailable",
			checks: []checkSpec{
				present(freqGame, "game"),
				// As case 1: the endpoint engine hears Discord; its row keeps the policy's verdict.
				{freq: freqVoice, from: at(1.5), to: end(), expect: expectPresent, role: "discord", ctl: controlNone},
			},
			fixtures: []fixtureStep{
				explorerTone("game", exeGame, freqGame),
				explorerTone("discord", exeDiscord, freqVoice),
			},
		},
		{
			num: 20, name: "timestamps", req: reqYes, special: "timestamps", noClick: true,
			desc:        "PCM-tap onset times match im_test_render_bursts' predicted device-position times",
			driverRoles: []string{"bursts"},
		},
		{
			num: 21, name: "opus", req: reqYes, special: "opus", noClick: true,
			desc: "the Opus stage encodes the mix: 1000 Hz present, 440 Hz absent, the bitrate change visible",
			checks: []checkSpec{
				present(freqGame, "game"), absent(freqVoice, "discord"),
			},
			fixtures: []fixtureStep{
				explorerTone("game", exeGame, freqGame),
				explorerTone("discord", exeDiscord, freqVoice),
			},
		},
		{
			num: 22, name: "multi-endpoint", req: reqInfo, special: "multi-endpoint", noClick: true, minEndpoints: 2,
			desc: "whether a tone on a second render endpoint is captured (INFO; SKIP with one endpoint)",
			checks: []checkSpec{
				present(freqGame, "game"),
				{freq: freqLateGame, from: at(1.5), to: end(), expect: expectInfo, role: "game2", ctl: controlNone},
			},
			fixtures: []fixtureStep{
				explorerTone("game", exeGame, freqGame), // on the default endpoint
			},
			// game2 is `fixture endpoint-tone` on another active render endpoint, started by the driver (it needs
			// the endpoint's id and the DLL's folder).
			driverRoles: []string{"game2"},
		},
		{
			num: 23, name: "soak", req: reqTierMinor, special: "soak", long: true,
			desc: "30 min of a music-like tone with an idle voice app holding the mic: no underruns, trims, " +
				"resyncs or clicks",
			checks: []checkSpec{
				present(freqGame, "music"), present(freqLateGame, "music"),
				// 05 §18.5: "440 never present"; checked with the absent rule, which is stricter.
				{freq: freqVoice, from: at(1.5), to: end(), expect: expectAbsent, ctl: controlNone},
			},
			counterZero: []string{"underruns", "trims", "mixerResyncs"},
			counterMax:  map[string]int{"maxResamplePpm": soakMaxResamplePpm},
			fixtures: []fixtureStep{
				// The music: 1000 + 683 Hz with a slow AM, a seamless 10 s loop.
				{role: "music", exe: exeGame, verb: verbTone, freqs: []float64{freqGame, freqLateGame}, am: soakAMHz,
					underExplorer: true},
				// Discord idle with the mic: it forces the include set and holds a capture session all along.
				{role: "discord", exe: exeDiscord, verb: verbIdle, mic: true, underExplorer: true},
			},
		},
		viewerCase(24, "viewer-browser-included",
			"a saved Include for the viewer's browser is still kept out (the viewer rule beats it)",
			[]string{"exe:msedge.exe"}),
		{
			num: 25, name: "late-child-heard", req: reqYes,
			desc: "a late child of a heard app, in the same folder, is covered by the app's stream and heard",
			checks: []checkSpec{
				present(freqGame, "game"),
				{freq: freqLateGame, from: at(3.6), to: end(), expect: expectPresent, role: "gameChild",
					ctl: controlNotKeptOut},
			},
			rowChecks: []rowCheck{
				{role: "gameChild", note: "covered", from: at(3.6)},
				{role: "game", verdict: controlHeard},
			},
			measures: []measureSpec{
				{key: measureOnset, freq: freqLateGame, event: bound{event: sessionPrefix + "gameChild"},
					budget: budgetOnsetMs},
			},
			fixtures: []fixtureStep{
				// An idle voice app forces the include set: with the fast path on and nothing kept out but
				// isshoni, the engine would be exclude-self, which has no include stream to cover the child.
				{role: "discord", exe: exeDiscord, verb: verbIdle, underExplorer: true},
				// `Games\game.exe spawn -tone 1000 -at 4 Games\game.exe tone -f 683` (05 §18.5).
				{role: "game", exe: exeGame, verb: verbSpawn, freqs: []float64{freqGame}, underExplorer: true,
					child: &fixtureStep{role: "gameChild", exe: exeGame, verb: verbTone,
						freqs: []float64{freqLateGame}, at: 3}},
			},
		},
		{
			num: 26, name: "fast-path-late-child", req: reqFastPath, forceFastPath: true,
			desc: "a late isshoni descendant stays inside the EXCLUDE stream under the fast path",
			checks: []checkSpec{
				present(freqGame, "game"), present(freqLateGame, "game2"),
				{freq: freqSelf, from: at(1.5), to: end(), expect: expectAbsent, role: "selfWebview",
					ctl: controlKeptOut, reason: "isshoni"},
			},
			fixtures: []fixtureStep{
				explorerTone("game", exeGame, freqGame),
				explorerTone("game2", exeGame2, freqLateGame),
				// The harness's own child (not reparented), started at t = 3 s.
				{role: "selfWebview", exe: exeSelfWV, verb: verbTone, freqs: []float64{freqSelf}, at: 3},
			},
		},
		{
			num: 27, name: "pid-reuse-tree", req: reqInfo, special: "pid-reuse", noClick: true,
			desc: "whether Windows follows a reused PID into a heard app's tree (decides §5.2's loose union)",
			fixtures: []fixtureStep{
				explorerTone("game", exeGame, freqGame),
				// A (launcherx) exits at 2 s; B (a built-in voice app) plays the chime partials of §11.3.
				{role: "launcherx", exe: exeLauncherX, verb: verbSpawn, orphanAfter: 2, underExplorer: true,
					child: &fixtureStep{role: "reuseVoice", exe: exeDiscord, verb: verbTone,
						freqs: []float64{freqChimeLow, freqChimeHigh}}},
			},
		},
	}
}

// viewerCase is cases 11 and 24 (05 §18.5): `msedge.exe` (spawn) → msedge.exe (`spawn -tone 440`, the "isshoni tab")
// → msedge.exe playing 797 (the "other tab"), and the game; the whole browser is kept out with reason viewer.
// seedInclude is case 24's saved Include.
func viewerCase(num int, name, desc string, seedInclude []string) caseSpec {
	return caseSpec{
		num: num, name: name, req: reqYes, desc: desc, seedInclude: seedInclude,
		viewer: &viewerCall{connID: "c_test1", family: "edge", os: "windows"},
		checks: []checkSpec{
			present(freqGame, "game"),
			{freq: freqVoice, from: at(1.5), to: end(), expect: expectAbsent, role: "isshoniTab",
				ctl: controlKeptOut, reason: "viewer"},
			{freq: freqBrowserTab, from: at(1.5), to: end(), expect: expectAbsent, role: "otherTab",
				ctl: controlKeptOut, reason: "viewer"},
		},
		fixtures: []fixtureStep{
			explorerTone("game", exeGame, freqGame),
			{role: "edge", exe: exeEdge, verb: verbSpawn, underExplorer: true,
				child: &fixtureStep{role: "isshoniTab", exe: exeEdge, verb: verbSpawn, freqs: []float64{freqVoice},
					child: &fixtureStep{role: "otherTab", exe: exeEdge, verb: verbTone,
						freqs: []float64{freqBrowserTab}}}},
		},
	}
}

// explorerTone is a top-level fixture (reparented under explorer.exe) that plays one tone for the whole case.
func explorerTone(role, exe string, freq float64) fixtureStep {
	return fixtureStep{role: role, exe: exe, verb: verbTone, freqs: []float64{freq}, underExplorer: true}
}

// caseByName returns the case with the given name.
func caseByName(name string) (caseSpec, bool) {
	for _, c := range cases() {
		if c.name == name {
			return c, true
		}
	}
	return caseSpec{}, false
}

// caseByNum returns the case with the given number.
func caseByNum(num int) (caseSpec, bool) {
	for _, c := range cases() {
		if c.num == num {
			return c, true
		}
	}
	return caseSpec{}, false
}
