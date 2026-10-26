package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// These are the report redaction tests of WA62b's acceptance (05 §18.7): no names (the user, host and domain names,
// and the app and exe names collected at runtime), paths relative to the work folder (other absolute paths become
// placeholders), and "other" for non-fixture apps. They run on every OS.

// The made-up identifiers of these tests: a user, a host, a domain, a profile folder and a work folder below it.
const (
	testUser    = "Ada Example"
	testLogin   = "adaex"
	testHost    = "ADA-DESKTOP-7Q"
	testDomain  = "EXAMPLECORP"
	testHome    = `C:\Users\Ada Example`
	testWork    = testHome + `\AppData\Local\Temp\isshoni-audiotest-2741`
	testHarness = `D:\tools\isshoni-audiotest`
)

func testRedactor() *redactor {
	return newRedactor(
		redactInfo{workDirs: []string{testWork}, harnessDir: testHarness,
			names: []string{"Spotify", "spotify.exe", "Elden Ring", "eldenring.exe", "Discord", "discord.exe", "x"}},
		localIDs{names: []string{testUser, testLogin, testHost, testDomain, testDomain + `\` + testLogin},
			homes: []string{testHome, "/Users/adaex"}})
}

func TestRedactText(t *testing.T) {
	rd := testRedactor()
	tests := []struct{ in, want string }{
		// Paths below the work folder become relative to it, whatever the case or the separators.
		{`copy ` + testWork + `\Discord\app-1.0.9999\discord.exe: denied`,
			`copy Discord\app-1.0.9999\discord.exe: denied`},
		{`open c:/users/ada example/appdata/local/temp/isshoni-audiotest-2741/Games/game.exe`, `open Games/game.exe`},
		{`the work folder ` + testWork + ` is gone`, `the work folder . is gone`},
		// A sibling whose name starts with the work folder's is not below it: a path in the profile's temp folder.
		{testWork + `2\x.txt`, `<home>\AppData\Local\Temp`},
		{testWork + `2\game.exe crashed`, `<home>\AppData\Local\Temp\game.exe crashed`},
		// The harness's folder and the profile folder become placeholders; other absolute paths too, keeping a
		// plain file name of the harness's kinds and nothing of the folders in between.
		{`load ` + testHarness + `\isshoni-media.dll: not found`, `load <harness>\isshoni-media.dll: not found`},
		{`C:\Users\Ada Example\Downloads\notes.txt`, `<home>`},
		{`open C:\Users\Ada Example\Documents\Medical Records\scan.pdf: denied`, `open <home>: denied`},
		{`C:\Users\Ada Example\Downloads\isshoni-media.dll was blocked`, `<home>\isshoni-media.dll was blocked`},
		{`open E:\Private Stuff\isshoni-media.dll, then`, `open <path>\isshoni-media.dll, then`},
		{`read \\fileserver\share\secret.doc`, `read <path>`},
		// A folder with spaces is part of the path, in a profile the redactor doesn't know too, however many spaces
		// it has; a path runs to the first character no Windows path holds (Go's PathError puts ": " after it).
		{`open C:\Users\Bob Jones\AppData\x.dat: The system cannot find`, `open <path>: The system cannot find`},
		{`E:\Data\My Private Folder\x.bin`, `<path>`},
		{`(see E:\Data\My Private Folder\x.bin)`, `(see <path>)`},
		// A collected name inside a path doesn't end it: the path goes first, the names after.
		{`open E:\Apps\Spotify\Private Project Folder\cache.bin: denied`, `open <path>: denied`},
		{`E:\Data\` + testLogin + `\Secret Folder\x.json`, `<path>\x.json`},
		{`\\` + testHost + `\share\Tax Returns\x.dll`, `<path>\x.dll`},
		{`E:\Games\Elden Ring\eldenring.exe crashed`, `<path>\<name> crashed`},
		// Two paths in one line are redacted each on its own.
		{`copy C:\Users\Bob\a b\game.exe to D:\x y\z.dll: denied`,
			`copy <path>\game.exe to <path>\z.dll: denied`},
		{`stat /home/someone/build/isshoni-audiotest: no`, `stat <path>/isshoni-audiotest: no`},
		{`stat /home/someone/My Private Folder/x.bin`, `stat <path>`},
		{`stat /Users/adaex/x.json`, `stat <home>/x.json`},
		{`stat /Users/adaex/Medical Records/scan.pdf`, `stat <home>`},
		// Names: the local user, login, host and domain, and the app and exe names seen in the reports.
		{`user ` + testDomain + `\` + testLogin + ` on ` + testHost, `user <name> on <name>`},
		{`Spotify and ELDEN RING were heard; eldenring.exe kept out`,
			`<name> and <name> were heard; <name> kept out`},
		{`Ada Example's ADA-DESKTOP-7Q`, `<name>'s <name>`},
		// A name only counts as a whole word, a one-letter name never, and the harness's own vocabulary (fixture
		// exes and roles, tokens) is kept even when a real app has the same name.
		{`Spotifyish and xylophone`, `Spotifyish and xylophone`},
		{`discord.exe kept out (fixture:discord), reason builtin`, `discord.exe kept out (fixture:discord), reason builtin`},
		{`median abs timestamp error 3.1 ms > 15; 50 ± 1 packets/s`, `median abs timestamp error 3.1 ms > 15; 50 ± 1 packets/s`},
	}
	for _, tc := range tests {
		got := rd.text(tc.in)
		if got != tc.want {
			t.Errorf("text(%q)\n got  %q\n want %q", tc.in, got, tc.want)
		}
		// What merge's check for an unredacted report looks for is never left in a redacted text.
		if absPathStart.MatchString(got) {
			t.Errorf("text(%q) = %q still holds an absolute path", tc.in, got)
		}
	}
}

// TestRedactReport redacts a whole report full of names and paths and checks that its JSON names none of them, that
// non-fixture apps are "other", and that what the report is for (fixture ids, results, levels, codes) survives.
func TestRedactReport(t *testing.T) {
	r := report{
		V: 1, Harness: "v0.2.0-14-g1a2b3c4", DLL: dllInfo{Version: "0.2.0", SHA256: strings.Repeat("ab", 32)},
		OS: osInfo{Product: "win11", Build: 26200, UBR: 6899, Edition: "Professional"},
		Machine: machineInfo{CPU: "AuthenticAMD", RenderEndpoints: []endpointInfo{
			{FormFactor: "Headphones", Bus: "usb"}, {FormFactor: "Ada's Speakers", Bus: "VendorBus"}}},
		Gate: true, GateCfg: gateConfig{FastPath: true},
		Cases: []caseReport{
			{Name: "builtin-voice", Result: resultFail, Engine: []string{"include-set"},
				Problems: []string{
					"a fixture was \"kept_out\" at 2.25s in a present window",
					"the case could not run: open " + testWork + `\Games\game.exe: Access is denied (` + testUser + `)`,
					"another app was heard: Spotify on " + testHost,
				},
				Events: []reportEvent{
					{TMs: 1210, Code: "stream_running", App: "fixture:game"},
					{TMs: 1500, Code: "session_created", App: "exe:spotify.exe"},
					{TMs: 1600, Code: "session_created", App: "discord"},
					{TMs: 1700, Code: "fastpath_exit", App: ""},
					{TMs: 1800, Code: "Weird Code", App: "fixture:" + testUser},
					{TMs: 1900, Code: "session_created", App: "fixture:gameChild"},
				},
				Checks: []checkReport{{Freq: 440, From: 1.5, Expect: "absent", LevelDB: -141.7, Pass: true}}},
			{Name: "pid-reuse-tree", Result: resultInfo, Info: map[string]any{
				"matched": true, "attempts": 12, "gameReason": "pid_reuse",
				"note": "reused under " + testWork + `\Games\game.exe by ` + testLogin,
				"list": []any{"Elden Ring", 3.5, map[string]any{"who": testDomain}},
			}},
		},
		Manual: []manualEntry{{Label: "R1", Pass: true}, {Label: "R7 " + testUser, Pass: false}},
	}
	testRedactor().report(&r)
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	for _, leak := range []string{testUser, testLogin, testHost, testDomain, "Spotify", "spotify", "Elden Ring",
		"AppData", `C:\\`, "Ada", "VendorBus"} {
		if strings.Contains(js, leak) {
			t.Errorf("the redacted report still names %q:\n%s", leak, js)
		}
	}
	for _, keep := range []string{`"harness":"v0.2.0-14-g1a2b3c4"`, `"edition":"Professional"`,
		`"cpu":"AuthenticAMD"`, `{"formFactor":"Headphones","bus":"usb"}`, `{"formFactor":"unknown","bus":"other"}`,
		`"app":"fixture:game"`, `"code":"fastpath_exit","app":""`, `"gameReason":"pid_reuse"`, `"attempts":12`,
		`Games\\game.exe: Access is denied (\u003cname\u003e)`, `"label":"R1"`, `"label":"unlabelled"`,
		`a fixture was \"kept_out\" at 2.25s in a present window`} {
		if !strings.Contains(js, keep) {
			t.Errorf("the redacted report lost %q:\n%s", keep, js)
		}
	}
	ev := r.Cases[0].Events
	if ev[1].App != otherRedacted || ev[2].App != otherRedacted || ev[4].App != otherRedacted ||
		ev[4].Code != otherRedacted {
		t.Errorf("non-fixture apps and odd codes must be %q: %+v", otherRedacted, ev)
	}
	if ev[5].App != "fixture:gameChild" {
		t.Errorf("a fixture role in camel case must stay: %+v", ev[5])
	}
	// Every role of the case table is a role the redactor keeps.
	for _, cs := range cases() {
		for _, role := range fixtureRoles(cs) {
			if app := fixturePrefix + role; redactEventApp(app) != app {
				t.Errorf("case %q's role %q would be redacted", cs.name, role)
			}
		}
	}
	if p := unredactedProblem(r); p != "" {
		t.Errorf("a redacted report reads as unredacted: %s", p)
	}
}

// TestUnredactedProblem: a report written with -no-redact is detected, so merge refuses it: its events still name a
// non-fixture app, or (a quiet room: fixture events only) its problems or INFO values still hold an absolute path.
// The redaction's placeholders and paths relative to the work folder are no absolute paths.
func TestUnredactedProblem(t *testing.T) {
	r := report{Cases: []caseReport{{Name: "builtin-voice", Events: []reportEvent{
		{Code: "session_created", App: "fixture:voice"}, {Code: "x", App: otherRedacted}, {Code: "y"}},
		Problems: []string{`the case could not run: copy <harness>\isshoni-audiotest.exe to Games\game.exe: denied`,
			`open <path>\isshoni-media.dll, <home>/x.json, <home>\AppData\Local\Temp and <name>: no`},
		Info: map[string]any{"note": "under Discord/app-1.0/discord.exe", "list": []any{"<path>", 3.5}}}}}
	if p := unredactedProblem(r); p != "" {
		t.Fatalf("a redacted report: %s", p)
	}
	unredacted := []func(r *report){
		func(r *report) {
			r.Cases[0].Events = append(r.Cases[0].Events, reportEvent{Code: "session_created", App: "exe:spotify.exe"})
		},
		func(r *report) {
			r.Cases[0].Problems = append(r.Cases[0].Problems, `open C:\Users\X\Documents\x.bin: denied`)
		},
		func(r *report) { r.Cases[0].Problems = []string{`read \\host\share\x.dll`} },
		func(r *report) { r.Cases[0].Problems = []string{`stat /home/someone/x.json: no`} },
		func(r *report) { r.Cases[0].Info = map[string]any{"note": `E:\Data\x.json`} },
		func(r *report) {
			r.Cases[0].Info = map[string]any{"list": []any{1.0, map[string]any{"where": `d:/x/y.wav`}}}
		},
	}
	for i, edit := range unredacted {
		u := report{Cases: []caseReport{{Name: "builtin-voice", Problems: slices.Clone(r.Cases[0].Problems),
			Events: slices.Clone(r.Cases[0].Events)}}}
		edit(&u)
		if p := unredactedProblem(u); !strings.Contains(p, "-no-redact") {
			t.Errorf("unredacted report %d: %q", i, p)
		}
	}
}

// TestCurrentLocalIDs: the machine's own identifiers are read (at least a home folder or a name on any CI runner).
func TestCurrentLocalIDs(t *testing.T) {
	ids := currentLocalIDs()
	if len(ids.names) == 0 && len(ids.homes) == 0 {
		t.Fatal("no local user, host or home identifiers were found")
	}
}
