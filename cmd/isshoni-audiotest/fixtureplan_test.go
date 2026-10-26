package main

import (
	"slices"
	"strings"
	"testing"
)

// TestFixturePlansValid walks every case's fixture plan (05 §18.3) and checks it is well formed: every step has a
// known verb and a role, a tone names its frequencies, a step with a child is a spawn, and a reparented step has an
// exe. Cases 20 and 27's extra fixtures (bursts, reuse-pid) are the driver's own.
func TestFixturePlansValid(t *testing.T) {
	for _, cs := range cases() {
		if len(cs.fixtures) == 0 && cs.special != "timestamps" {
			t.Errorf("case %q has no fixtures", cs.name)
		}
		roles := map[string]bool{}
		for i := range cs.fixtures {
			walkFixture(t, cs.name, &cs.fixtures[i], roles)
		}
		if strings.Contains(cs.name, "viewer") && cs.viewer == nil {
			t.Errorf("case %q is a viewer case but makes no SetViewerBrowser call", cs.name)
		}
	}
}

func walkFixture(t *testing.T, caseName string, s *fixtureStep, roles map[string]bool) {
	t.Helper()
	if s.role == "" || s.role == selfRole {
		t.Errorf("%s: a fixture has role %q", caseName, s.role)
	}
	if roles[s.role] {
		t.Errorf("%s: role %q is used twice (pids map to one role)", caseName, s.role)
	}
	roles[s.role] = true
	switch s.verb {
	case verbTone:
		if len(s.freqs) == 0 {
			t.Errorf("%s: tone fixture %q has no frequency", caseName, s.role)
		}
		if s.child != nil {
			t.Errorf("%s: tone fixture %q has a child (use spawn -tone)", caseName, s.role)
		}
	case verbSpawn:
		if s.child == nil {
			t.Errorf("%s: spawn fixture %q has no child", caseName, s.role)
		}
	case verbIdle:
		if len(s.freqs) > 0 || s.child != nil {
			t.Errorf("%s: idle fixture %q plays or spawns", caseName, s.role)
		}
	default:
		t.Errorf("%s: fixture %q has verb %q, which a plan doesn't start", caseName, s.role, s.verb)
	}
	for _, f := range s.freqs {
		if f < 20 || f > 20000 {
			t.Errorf("%s: fixture %q frequency %g Hz out of range", caseName, s.role, f)
		}
	}
	if s.exe == "" {
		t.Errorf("%s: fixture %q needs an exe path", caseName, s.role)
	}
	if s.amp < 0 || s.dur < 0 || s.at < 0 || s.orphanAfter < 0 {
		t.Errorf("%s: fixture %q has a negative amp or time", caseName, s.role)
	}
	if s.child != nil {
		if s.child.underExplorer {
			t.Errorf("%s: child %q can't be reparented (it is its spawn's child)", caseName, s.child.role)
		}
		walkFixture(t, caseName, s.child, roles)
	}
}

// TestCheckedRolesExist: every role a case checks (a tone check or a row check) is a fixture of that case (or the
// harness itself, when it plays), so no check waits for a fixture that never starts (always INCONCLUSIVE).
func TestCheckedRolesExist(t *testing.T) {
	for _, cs := range cases() {
		have := fixtureRoles(cs)
		for _, role := range checkedRoles(cs) {
			if !slices.Contains(have, role) {
				t.Errorf("case %q checks role %q, which isn't in its fixture tree", cs.name, role)
			}
		}
		for _, ms := range cs.measures {
			if ms.role != "" && !slices.Contains(have, ms.role) {
				t.Errorf("case %q measures role %q, which isn't in its fixture tree", cs.name, ms.role)
			}
		}
		if cs.selfTone > 0 && !slices.Contains(checkedRoles(cs), selfRole) {
			t.Errorf("case %q plays %g Hz itself but checks no %q control", cs.name, cs.selfTone, selfRole)
		}
	}
}

// TestFixtureArgs pins the command lines of the tree cases (05 §18.3, §18.5): a spawn carries its child's exe and
// arguments recursively, a late child's -at is relative to its spawn (t = 3 s after the 1 s settle is -at 4), a tone
// step with a child is `spawn -tone`, the orphan parent exits after -orphan-after, and a tone with dur 0 plays one
// second past the end of the recording.
func TestFixtureArgs(t *testing.T) {
	const w = `C:\work`
	tests := []struct {
		num  int
		dur  float64
		want []string
	}{
		{6, 8, []string{
			`fixture tone -role game -f 1000 -d 10`,
			`fixture spawn -role discord -d 10 C:\work\Discord\app-1.0.9999\voicehelper.exe ` +
				`fixture spawn -role voicehelper -d 10 C:\work\Discord\app-1.0.9999\audiohelper.exe ` +
				`fixture tone -role audiohelper -f 440 -d 10`,
			`fixture spawn -role teams -d 10 C:\work\WebView2Runtime\msedgewebview2.exe ` +
				`fixture tone -role teamsRuntime -f 571 -d 10`,
		}},
		{7, 11, []string{
			`fixture tone -role game -f 1000 -d 13`,
			`fixture spawn -role discord -at 4 -d 13 C:\work\Discord\app-1.0.9999\audiohelper.exe ` +
				`fixture tone -role helper -f 440 -d 9`,
		}},
		{11, 8, []string{
			`fixture tone -role game -f 1000 -d 10`,
			`fixture spawn -role edge -d 10 C:\work\Microsoft\Edge\Application\msedge.exe ` +
				`fixture spawn -role isshoniTab -tone 440 -d 10 C:\work\Microsoft\Edge\Application\msedge.exe ` +
				`fixture tone -role otherTab -f 797 -d 10`,
		}},
		{17, 8, []string{
			`fixture tone -role game -f 1000 -d 10`,
			`fixture spawn -role launcherx -tone 683 -d 10 C:\work\Discord\app-1.0.9999\discord.exe ` +
				`fixture tone -role discord -f 440 -d 10`,
		}},
		{18, 8, []string{
			`fixture spawn -role discord -d 4 -orphan-after 4 C:\work\Discord\app-1.0.9999\voicehelper.exe ` +
				`fixture tone -role voicehelper -f 440 -d 10`,
		}},
		{25, 11, []string{
			`fixture idle -role discord -d 13`,
			`fixture spawn -role game -at 4 -tone 1000 -d 13 C:\work\Games\game.exe ` +
				`fixture tone -role gameChild -f 683 -d 9`,
		}},
	}
	for _, tc := range tests {
		cs, ok := caseByNum(tc.num)
		if !ok {
			t.Fatalf("no case %d", tc.num)
		}
		if got := caseDuration(cs); got != tc.dur {
			t.Errorf("case %d records %v s, want %v", tc.num, got, tc.dur)
		}
		var got []string
		for i := range cs.fixtures {
			got = append(got, strings.Join(fixtureArgs(&cs.fixtures[i], w, tc.dur), " "))
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("case %d argv:\n got  %q\n want %q", tc.num, got, tc.want)
		}
	}
}

// TestPlanSpans: the planned start and lifetime the "exited early" control compares against.
func TestPlanSpans(t *testing.T) {
	cs, _ := caseByNum(18)
	spans := planSpans(cs, 8)
	if s := spans["discord"]; s.start != -1 || s.life != 4 {
		t.Errorf("the orphan parent's span %+v, want start −1, life 4 (it exits 3 s into the recording)", s)
	}
	if s := spans["voicehelper"]; s.start != -1 || s.life != 10 {
		t.Errorf("the helper's span %+v, want start −1, life 10", s)
	}
	cs, _ = caseByNum(8)
	if s := planSpans(cs, 8)["game2"]; s.start != 3 || s.life != 3 {
		t.Errorf("case 8's late game span %+v, want start 3, life 3", s)
	}
}

// TestCaseFreqs: the silence pre-check listens for every frequency a case plays, its own tone included.
func TestCaseFreqs(t *testing.T) {
	cs, _ := caseByNum(10)
	if got := caseFreqs(cs); !slices.Equal(got, []float64{freqBrowserTab, freqSelf, freqGame}) {
		t.Errorf("case 10 freqs %v", got)
	}
	cs, _ = caseByNum(20)
	if got := caseFreqs(cs); !slices.Equal(got, []float64{freqBursts}) {
		t.Errorf("case 20 freqs %v", got)
	}
}

// TestCopyList: every fixture exe of a tree is copied once, before any recording (05 §18.3).
func TestCopyList(t *testing.T) {
	cs, _ := caseByNum(11)
	if got := copyList(cs); !slices.Equal(got, []string{exeGame, exeEdge}) {
		t.Errorf("case 11 copies %q", got)
	}
	cs, _ = caseByNum(6)
	want := []string{exeGame, exeDiscord, exeVoiceHelp, exeAudioHelp, exeTeams, exeWebView2}
	if got := copyList(cs); !slices.Equal(got, want) {
		t.Errorf("case 6 copies %q, want %q", got, want)
	}
}
