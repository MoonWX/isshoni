package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/MoonWX/isshoni/internal/desktop/audio"
	"github.com/MoonWX/isshoni/internal/media"
)

// These tests cover the mic cases of 05 §18.5 (9 steam-game, 12 browser-mic, 13 mic-user, 14 shared-app-mic) on
// synthetic recordings, and the capture's handling of microphone sessions, row warnings and prompts. They run on
// every OS.

// rowTrack is a role's track with the same session verdict and app row in every sample; capture marks a role that
// has only a microphone session.
func rowTrack(dur float64, verdict, reason string, capture bool, row roleSampleState) *roleTrack {
	tr := &roleTrack{}
	for t := 0.0; t <= dur+1e-9; t += 0.25 {
		s := row
		s.t, s.active, s.capture, s.verdict, s.reason, s.row = t, true, capture, verdict, reason, true
		tr.samples = append(tr.samples, s)
	}
	return tr
}

// promptsEvery is a pending prompt of kind for appID in every sample of [from, to].
func promptsEvery(dur, from, to float64, kind, appID string) []promptSample {
	var out []promptSample
	for t := 0.0; t <= dur+1e-9; t += 0.25 {
		ps := promptSample{t: t}
		if t >= from && t <= to {
			ps.prompts = []promptState{{kind: kind, appID: appID}}
		}
		out = append(out, ps)
	}
	return out
}

func TestPromptKindsMatchTheService(t *testing.T) {
	if promptMic != audio.PromptMic || promptBrowserMic != audio.PromptBrowserMic {
		t.Fatalf("prompt kinds %q/%q, the service's %q/%q", promptMic, promptBrowserMic, audio.PromptMic,
			audio.PromptBrowserMic)
	}
}

// TestSteamGame is case 9: the game in Steam's folder is heard (its row heard) while Steam, which only holds the
// mic, is kept out with reason mic, read through its microphone session.
func TestSteamGame(t *testing.T) {
	cs := mustCase(t, "steam-game")
	cfg := gateConfig{FastPath: true}
	build := func(steamRow string) recording {
		return baseRec(fadedTone(freqGame, dbToAmp(-18), 8, 0.02), 8, map[string]*roleTrack{
			"game":  rowTrack(8, "heard", "", false, roleSampleState{rowVerdict: "heard"}),
			"steam": rowTrack(8, "kept_out", "mic", true, roleSampleState{rowVerdict: steamRow, rowReason: "mic"}),
		}, "include-set")
	}
	if cr := evaluate(cs, build("kept_out"), cfg); cr.Result != resultPass {
		t.Fatalf("want PASS, got %s: %v", cr.Result, cr.Problems)
	}
	if cr := evaluate(cs, build("heard"), cfg); cr.Result != resultFail || !strings.Contains(cr.Problems[0], "row") {
		t.Fatalf("Steam's row heard: want FAIL naming the row, got %s %v", cr.Result, cr.Problems)
	}
	never := build("kept_out")
	never.roles["steam"].neverActive = true
	if cr := evaluate(cs, never, cfg); cr.Result != resultInconclusive {
		t.Fatalf("Steam never held the mic: want INCONCLUSIVE, got %s", cr.Result)
	}
	noMic := build("kept_out")
	noMic.micFail = true
	if cr := evaluate(cs, noMic, cfg); cr.Result != resultSkip || cr.Problems[0] != micSkip || !cr.Required {
		t.Fatalf("Steam couldn't open the mic: want a required SKIP, got %+v", cr)
	}
	if got := exitCode([]caseReport{evaluate(cs, noMic, cfg)}); got != exitSkipped {
		t.Fatalf("a mic case SKIPPED: exit %d, want %d", got, exitSkipped)
	}
}

// recForBrowserMic is case 12: the game heard; Discord Web (440) and the other tab (797) kept out as mic, Chrome's
// row a browser kept out as mic; a browser-mic prompt for Chrome from 2 s.
func recForBrowserMic(withPrompt, browserRow bool) recording {
	dur := 8.0
	row := roleSampleState{rowVerdict: "kept_out", rowReason: "mic", rowBrowser: browserRow}
	rec := baseRec(fadedTone(freqGame, dbToAmp(-18), dur, 0.02), dur, map[string]*roleTrack{
		"game":       rowTrack(dur, "heard", "", false, roleSampleState{rowVerdict: "heard"}),
		"discordWeb": rowTrack(dur, "kept_out", "mic", false, row),
		"otherTab":   rowTrack(dur, "kept_out", "mic", false, row),
	}, "include-set")
	rec.roleApp = map[string]string{"game": "exe:game.exe", "discordWeb": "exe:chrome.exe",
		"otherTab": "exe:chrome.exe"}
	if withPrompt {
		rec.promptAt = promptsEvery(dur, 2, dur, promptBrowserMic, "exe:chrome.exe")
	}
	return rec
}

func TestBrowserMic(t *testing.T) {
	cs := mustCase(t, "browser-mic")
	cfg := gateConfig{FastPath: true}
	if cr := evaluate(cs, recForBrowserMic(true, true), cfg); cr.Result != resultPass {
		t.Fatalf("want PASS, got %s: %v", cr.Result, cr.Problems)
	}
	if cr := evaluate(cs, recForBrowserMic(false, true), cfg); cr.Result != resultFail ||
		!strings.Contains(strings.Join(cr.Problems, "|"), `"browser-mic" prompt`) {
		t.Fatalf("no prompt: want FAIL naming the prompt, got %s %v", cr.Result, cr.Problems)
	}
	if cr := evaluate(cs, recForBrowserMic(true, false), cfg); cr.Result != resultFail {
		t.Fatalf("a row that is no browser's: want FAIL, got %s", cr.Result)
	}
	// A prompt of the other kind, or for another app, doesn't count.
	wrong := recForBrowserMic(false, true)
	wrong.promptAt = promptsEvery(8, 2, 8, promptMic, "exe:chrome.exe")
	if cr := evaluate(cs, wrong, cfg); cr.Result != resultFail {
		t.Fatalf("a mic prompt instead of browser-mic: want FAIL, got %s", cr.Result)
	}
	wrong.promptAt = promptsEvery(8, 2, 8, promptBrowserMic, "exe:msedge.exe")
	if cr := evaluate(cs, wrong, cfg); cr.Result != resultFail {
		t.Fatalf("a prompt for another browser: want FAIL, got %s", cr.Result)
	}
}

// recForMicUser is case 13: KOOK holds the mic and plays 440, kept out (mic) with a pending mic prompt until the
// decision; the recorder's stamp after Decide is at 5.05 s and 440 fades in at 5.1 s.
func recForMicUser(withPrompt, withStamp bool) recording {
	dur := 8.0
	n := int(dur * sampleRate)
	voice := make([]float64, n)
	copyInto(voice, fadedTone(freqVoice, dbToAmp(-18), dur-5.1, 0.005), int(5.1*sampleRate))
	kook := &roleTrack{}
	for t := 0.0; t <= dur+1e-9; t += 0.25 {
		v, reason := "kept_out", "mic"
		if t >= 5.25 {
			v, reason = "heard", ""
		}
		kook.samples = append(kook.samples, roleSampleState{t: t, active: true, verdict: v, reason: reason})
	}
	rec := baseRec(mixTones(fadedTone(freqGame, dbToAmp(-18), dur, 0.02)[:n], voice), dur, map[string]*roleTrack{
		"game": trackActive("heard", "", dur), "kook": kook,
	}, "include-set")
	rec.roleApp = map[string]string{"game": "exe:game.exe", "kook": "exe:kook.exe"}
	if withPrompt {
		rec.promptAt = promptsEvery(dur, 1.0, 5.0, promptMic, "exe:kook.exe")
	}
	if withStamp {
		rec.setStamp, rec.setStampOK = 5.05, true
	}
	return rec
}

// TestMicUser is case 13: 440 absent until 5.0 s and present from 5.6 s, its onset measured from the stamp that
// follows the decision (≤ 500 ms), the mic prompt existed. A missing prompt is a FAIL even though the decision (and
// so T_set) never came; a missing T_set with the prompt there is INCONCLUSIVE; no microphone is a SKIP.
func TestMicUser(t *testing.T) {
	cs := mustCase(t, "mic-user")
	cfg := gateConfig{FastPath: true}
	cr := evaluate(cs, recForMicUser(true, true), cfg)
	if cr.Result != resultPass {
		t.Fatalf("want PASS, got %s: %v", cr.Result, cr.Problems)
	}
	if v := cr.Measured[measureOnset].ValueMs; v < 20 || v > 100 {
		t.Fatalf("onset %d ms after T_set, want about 50", v)
	}
	if cr := evaluate(cs, recForMicUser(false, false), cfg); cr.Result != resultFail ||
		!strings.Contains(cr.Problems[0], "prompt") {
		t.Fatalf("no prompt, no decision: want FAIL naming the prompt, got %s %v", cr.Result, cr.Problems)
	}
	if cr := evaluate(cs, recForMicUser(true, false), cfg); cr.Result != resultInconclusive {
		t.Fatalf("a prompt but no T_set: want INCONCLUSIVE, got %s %v", cr.Result, cr.Problems)
	}
	// A prompt that only appeared after the decision's time doesn't count.
	late := recForMicUser(false, true)
	late.promptAt = promptsEvery(8, 5.5, 8, promptMic, "exe:kook.exe")
	if cr := evaluate(cs, late, cfg); cr.Result != resultFail {
		t.Fatalf("a prompt only after 5 s: want FAIL, got %s", cr.Result)
	}
	// KOOK heard before the decision is a FAIL of the absent window's control.
	leak := recForMicUser(true, true)
	leak.roles["kook"].samples[12].verdict = "heard" // t = 3.0 s
	if cr := evaluate(cs, leak, cfg); cr.Result != resultFail {
		t.Fatalf("KOOK heard at 3 s: want FAIL, got %s", cr.Result)
	}
	noMic := recForMicUser(true, true)
	noMic.micFail = true
	if cr := evaluate(cs, noMic, cfg); cr.Result != resultSkip {
		t.Fatalf("no mic: want SKIP, got %s", cr.Result)
	}
}

// TestSharedAppMic is case 14: the shared game holds the mic and is heard, its row carrying shared_app_uses_mic.
func TestSharedAppMic(t *testing.T) {
	cs := mustCase(t, "shared-app-mic")
	cfg := gateConfig{FastPath: true}
	build := func(warnings []string) recording {
		return baseRec(fadedTone(freqGame, dbToAmp(-18), 8, 0.02), 8, map[string]*roleTrack{
			"game": rowTrack(8, "heard", "", false, roleSampleState{rowVerdict: "heard", rowWarnings: warnings}),
		}, "include-set")
	}
	if cr := evaluate(cs, build([]string{"multi_process_session", "shared_app_uses_mic"}), cfg); cr.Result != resultPass {
		t.Fatalf("want PASS, got %s: %v", cr.Result, cr.Problems)
	}
	if cr := evaluate(cs, build(nil), cfg); cr.Result != resultFail ||
		!strings.Contains(cr.Problems[0], "shared_app_uses_mic") {
		t.Fatalf("no warning: want FAIL naming it, got %s %v", cr.Result, cr.Problems)
	}
	if cs.sharedRole != "game" {
		t.Fatalf("case 14 shares %q, want the game", cs.sharedRole)
	}
}

// TestCaptureMicSessions: a role with only a microphone session is sampled through it (flagged capture, with its
// app row); a role's render session beats its microphone session; the row's warnings and browser flag, the prompts
// and the names the report shows are kept.
func TestCaptureMicSessions(t *testing.T) {
	c := newTestCapture()
	r := media.AudioReport{
		QPCNanos: engineAt(2),
		Apps: []media.AudioApp{
			{ID: "exe:steam.exe", Name: "Steam", Exe: "steam.exe", Verdict: "kept_out", Reason: "mic", UsesMic: true},
			{ID: "exe:game.exe", Name: "game.exe", Exe: "game.exe", Verdict: "heard",
				Warnings: []string{"shared_app_uses_mic"}},
			{ID: "exe:chrome.exe", Name: "Google Chrome", Exe: "chrome.exe", Verdict: "kept_out", Reason: "mic",
				Browser: true},
		},
		Sessions: []media.AudioSession{
			{PID: 10, Exe: "steam.exe", AppID: "exe:steam.exe", Flow: "capture", State: "active",
				Verdict: "kept_out", Reason: "mic"},
			{PID: 11, Exe: "game.exe", AppID: "exe:game.exe", Flow: "capture", State: "active", Verdict: "heard"},
			{PID: 11, Exe: "game.exe", AppID: "exe:game.exe", Flow: "render", State: "inactive", Verdict: "heard"},
			{PID: 12, Exe: "chrome.exe", AppID: "exe:chrome.exe", Flow: "render", State: "active",
				Verdict: "kept_out", Reason: "mic"},
			{PID: 99, Exe: "voicemeeter.exe", AppID: "exe:voicemeeter.exe", Flow: "render", State: "active",
				Verdict: "heard"},
		},
	}
	fh := fhState{prompts: []promptState{{kind: promptBrowserMic, appID: "exe:chrome.exe"}}}
	c.sample(r, fh, nil, map[uint32]string{10: "steam", 11: "game", 12: "discordWeb"})

	steam := c.roles["steam"].samples[0]
	if !steam.capture || !steam.active || steam.rowVerdict != "kept_out" || steam.rowReason != "mic" {
		t.Fatalf("Steam's sample %+v, want its mic session and row", steam)
	}
	game := c.roles["game"].samples[0]
	if game.capture || game.active || !slices.Contains(game.rowWarnings, "shared_app_uses_mic") {
		t.Fatalf("the game's sample %+v, want its (inactive) render session and the row's warning", game)
	}
	if web := c.roles["discordWeb"].samples[0]; !web.rowBrowser {
		t.Fatalf("Chrome's sample %+v, want a browser row", web)
	}
	if c.roleApp["steam"] != "exe:steam.exe" {
		t.Fatalf("Steam's app id %q from its mic session", c.roleApp["steam"])
	}
	if len(c.promptAt) != 1 || c.promptAt[0].prompts[0] != fh.prompts[0] {
		t.Fatalf("prompts %+v", c.promptAt)
	}
	names := c.seenNames()
	for _, want := range []string{"Google Chrome", "Steam", "voicemeeter.exe"} {
		if !slices.Contains(names, want) {
			t.Fatalf("seen names %v lack %q", names, want)
		}
	}
	// A microphone session says nothing about playback: the controls skip it.
	rec := recording{roles: map[string]*roleTrack{"steam": c.roles["steam"]}, recordDur: 8}
	if p := rec.controlProblem(checkSpec{role: "steam", expect: expectPresent}, 0, 8); p != "" {
		t.Fatalf("a capture sample tripped a playback control: %s", p)
	}
}
