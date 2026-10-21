package audiorules

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The OS values of OSNames.For: GOOS values, plus M1's protocol.ClientOS "macos".
var allOS = []string{"windows", "darwin", "macos", "linux"}

// list is one named list of names, for the checks that run over every list.
type list struct {
	what  string // "voice app discord", "launchers", …
	names OSNames
}

func allLists() []list {
	var out []list
	for _, a := range VoiceApps() {
		out = append(out, list{"voice app " + a.ID, a.OSNames})
	}
	out = append(out, list{"launchers", Launchers()}, list{"shared runtimes", SharedRuntimes()})
	for _, b := range Browsers() {
		out = append(out, list{"browser family " + b.Family, b.OSNames})
	}
	return out
}

// The data must match 05 §5.4 exactly: these tables are a second copy of the spec, so a change to the built-in data
// is always a deliberate change to both (doc.go, "Changing the data").
func TestDataMatchesSpec(t *testing.T) {
	wantVoice := []VoiceApp{
		{ID: "discord", Name: "Discord", OSNames: OSNames{Windows: []string{
			"discord.exe", "discordptb.exe", "discordcanary.exe", "discorddevelopment.exe"}}},
		{ID: "vesktop", Name: "Vesktop", OSNames: OSNames{Windows: []string{"vesktop.exe", "equibop.exe"}}},
		{ID: "teamspeak", Name: "TeamSpeak", OSNames: OSNames{Windows: []string{
			"ts3client_win64.exe", "ts3client_win32.exe", "teamspeak.exe"}}},
		{ID: "mumble", Name: "Mumble", OSNames: OSNames{Windows: []string{"mumble.exe"}}},
		{ID: "zoom", Name: "Zoom", OSNames: OSNames{Windows: []string{"zoom.exe"}}},
		{ID: "teams", Name: "Microsoft Teams", OSNames: OSNames{Windows: []string{"ms-teams.exe", "teams.exe"}}},
		{ID: "slack", Name: "Slack", OSNames: OSNames{Windows: []string{"slack.exe"}}},
	}
	if got := VoiceApps(); !reflect.DeepEqual(got, wantVoice) {
		t.Errorf("VoiceApps() = %+v\nwant (05 §5.4) %+v", got, wantVoice)
	}

	wantLaunchers := OSNames{Windows: []string{
		"explorer.exe", "steam.exe", "epicgameslauncher.exe", "battle.net.exe", "battle.net launcher.exe",
		"galaxyclient.exe", "eadesktop.exe", "ealauncher.exe", "upc.exe", "riotclientservices.exe", "heroic.exe",
		"playnite.desktopapp.exe", "playnite.fullscreenapp.exe", "xboxpcapp.exe", "gamelaunchhelper.exe",
		"overwolf.exe", "itch.exe", "services.exe", "svchost.exe", "wininit.exe", "winlogon.exe", "userinit.exe",
		"sihost.exe", "runtimebroker.exe", "dllhost.exe", "taskhostw.exe", "applicationframehost.exe", "cmd.exe",
		"powershell.exe", "pwsh.exe", "windowsterminal.exe", "openconsole.exe", "conhost.exe", "wsl.exe", "bash.exe",
	}}
	if got := Launchers(); !reflect.DeepEqual(got, wantLaunchers) {
		t.Errorf("Launchers() = %+v\nwant (05 §5.4) %+v", got, wantLaunchers)
	}

	wantRuntimes := OSNames{Windows: []string{"msedgewebview2.exe"}}
	if got := SharedRuntimes(); !reflect.DeepEqual(got, wantRuntimes) {
		t.Errorf("SharedRuntimes() = %+v, want (05 §5.4) %+v", got, wantRuntimes)
	}
	// Browsers: TestBrowserFamiliesComplete.
}

// Acceptance: the browser families are complete. The families are exactly the three of the viewer rule (05 §5.9
// step 3), each with every browser of 05 §5.4's table.
func TestBrowserFamiliesComplete(t *testing.T) {
	want := []Browser{
		{Family: FamilyChrome, OSNames: OSNames{Windows: []string{
			"chrome.exe", "chromium.exe", "brave.exe", "vivaldi.exe", "opera.exe", "arc.exe", "thorium.exe"}}},
		{Family: FamilyEdge, OSNames: OSNames{Windows: []string{"msedge.exe"}}},
		{Family: FamilyFirefox, OSNames: OSNames{Windows: []string{
			"firefox.exe", "librewolf.exe", "waterfox.exe", "floorp.exe", "zen.exe"}}},
	}
	if got := Browsers(); !reflect.DeepEqual(got, want) {
		t.Errorf("Browsers() = %+v\nwant (05 §5.4) %+v", got, want)
	}

	// The family names are M1's client.browser values (internal/protocol ClientInfo.Browser:
	// chrome|edge|firefox|safari|other; this package imports nothing of this repository, so they are spelled out).
	if FamilyChrome != "chrome" || FamilyEdge != "edge" || FamilyFirefox != "firefox" {
		t.Errorf("families = %q, %q, %q; want M1's client.browser values chrome, edge, firefox",
			FamilyChrome, FamilyEdge, FamilyFirefox)
	}
	for _, family := range []string{FamilyChrome, FamilyEdge, FamilyFirefox} {
		b, ok := BrowserByFamily(family)
		switch {
		case !ok:
			t.Errorf("BrowserByFamily(%q): no family", family)
		case len(b.Windows) == 0:
			t.Errorf("family %q has no Windows browser", family)
		}
	}
	// The rest of M1's values, and unknown ones, add no rule (05 §5.9 step 3: a banner instead).
	for _, family := range []string{"safari", "other", "", "Chrome", "opera"} {
		if b, ok := BrowserByFamily(family); ok {
			t.Errorf("BrowserByFamily(%q) = %+v, want none", family, b)
		}
	}
}

// Acceptance: names are lower case. Windows matches exe names case-insensitively, the engine on its own lower-case
// copy (05 §5.1); ASCII keeps every comparison (Go's, the engine's ordinal one) agreeing on what "lower case" means.
func TestNamesLowerCase(t *testing.T) {
	for _, l := range allLists() {
		for _, os := range allOS {
			for _, name := range l.names.For(os) {
				if !isLowerASCII(name) || strings.TrimSpace(name) != name {
					t.Errorf("%s (%s): name %q is not trimmed lower-case ASCII", l.what, os, name)
				}
			}
		}
	}
	for _, a := range VoiceApps() {
		if !isLowerASCII(a.ID) || a.ID == "" {
			t.Errorf("voice app id %q is not lower-case ASCII", a.ID)
		}
	}
	for _, b := range Browsers() {
		if !isLowerASCII(b.Family) || b.Family == "" {
			t.Errorf("browser family %q is not lower-case ASCII", b.Family)
		}
	}
}

func isLowerASCII(s string) bool {
	for _, r := range s {
		if r < 0x20 || r > 0x7e || ('A' <= r && r <= 'Z') {
			return false
		}
	}
	return true
}

// Every Windows name is an exe file name that the service's app ids accept after "exe:" (05 §14.2,
// ErrInvalidAppID: 5–255 bytes, ending in ".exe", without \ / : * ? " < > | or control characters), so a decision
// for any of them can be written and the service can lock the exe: ids of the voice apps.
func TestWindowsNamesAreExeFileNames(t *testing.T) {
	for _, l := range allLists() {
		for _, name := range l.names.Windows {
			if len(name) < 5 || len(name) > 255 || !strings.HasSuffix(name, ".exe") ||
				strings.ContainsAny(name, `\/:*?"<>|`) || !isLowerASCII(name) {
				t.Errorf("%s: %q is not an exe file name of 5–255 bytes", l.what, name)
			}
		}
	}
}

// Acceptance: names are unique across lists. A name in two lists would get two contradicting rules (a browser that
// is also a launcher would end every app walk at the browser), and a duplicate inside one list hides a typo.
func TestNamesUniqueAcrossLists(t *testing.T) {
	for _, os := range allOS {
		seen := map[string]string{} // name -> list
		for _, l := range allLists() {
			for _, name := range l.names.For(os) {
				if prev, dup := seen[name]; dup {
					t.Errorf("%s: %q is in %s and in %s", os, name, prev, l.what)
					continue
				}
				seen[name] = l.what
			}
		}
	}
}

// Voice app ids are unique and never collide with the ids the service builds itself: "isshoni" (the self rule,
// 05 §14.3) and "exe:<name>" (other apps, 05 §5.1). Browser families are unique.
func TestIDsUnique(t *testing.T) {
	ids := map[string]bool{}
	for _, a := range VoiceApps() {
		switch {
		case ids[a.ID]:
			t.Errorf("voice app id %q appears twice", a.ID)
		case a.ID == "isshoni" || strings.HasPrefix(a.ID, "exe:"):
			t.Errorf("voice app id %q collides with an id the service builds itself", a.ID)
		case a.Name == "":
			t.Errorf("voice app %q has no name", a.ID)
		case len(a.Windows) == 0:
			t.Errorf("voice app %q has no Windows exe", a.ID)
		}
		ids[a.ID] = true
	}
	families := map[string]bool{}
	for _, b := range Browsers() {
		if families[b.Family] {
			t.Errorf("browser family %q appears twice", b.Family)
		}
		families[b.Family] = true
	}
}

// Acceptance: msedgewebview2.exe is not a browser. It is the shared runtime: a browser rule on it would treat every
// WebView2 app (isshoni's own window, new Teams) as one browser app.
func TestWebView2IsNotABrowser(t *testing.T) {
	const webview2 = "msedgewebview2.exe"
	if !SharedRuntimes().Contains("windows", webview2) {
		t.Errorf("%s is not a shared runtime", webview2)
	}
	if b, ok := BrowserByProcess("windows", webview2); ok {
		t.Errorf("%s is in browser family %q", webview2, b.Family)
	}
	if Launchers().Contains("windows", webview2) {
		t.Errorf("%s is a launcher", webview2)
	}
	if a, ok := VoiceAppByProcess("windows", webview2); ok {
		t.Errorf("%s is listed for voice app %q", webview2, a.ID)
	}
}

// No browser is a launcher (05 §19.1): a browser opened from a launcher stays its own app, and a browser must stay
// an include target.
func TestNoBrowserIsALauncher(t *testing.T) {
	for _, os := range allOS {
		for _, b := range Browsers() {
			for _, name := range b.For(os) {
				if Launchers().Contains(os, name) {
					t.Errorf("%s: browser %q (family %s) is a launcher", os, name, b.Family)
				}
			}
		}
	}
}

// isshoni's own exe is in no list: the service adds the self rules itself (05 §14.3 step 1), and a launcher or
// browser rule on it would change how its own tree is walked.
func TestSelfIsInNoList(t *testing.T) {
	for _, l := range allLists() {
		if l.names.Contains("windows", "isshoni.exe") {
			t.Errorf("isshoni.exe is in %s", l.what)
		}
	}
}

// M2 ships Windows data only; the macOS (M3) and Linux (M4) fields exist and stay empty until those milestones.
func TestOtherOSesEmptyInM2(t *testing.T) {
	for _, l := range allLists() {
		if len(l.names.MacOS) != 0 || len(l.names.Linux) != 0 {
			t.Errorf("%s: MacOS %q, Linux %q; want both empty in M2", l.what, l.names.MacOS, l.names.Linux)
		}
	}
}

// The built-in rules fit the engine's rule limit (media.Exclusions.Rules <= 512, 05 §13.1) together with the most
// the service adds: the self rules (05 §14.3 step 1), 256 saved decisions (05 §14.1 appRules), and a viewer rule
// for every browser exe (05 §5.9).
func TestRulesFitTheLimit(t *testing.T) {
	const (
		maxRules     = 512
		selfRules    = 3 // instance, isshoni.exe, WebView2 data folder
		maxDecisions = 256
	)
	builtin, browserExes := 0, 0
	for _, l := range allLists() {
		builtin += len(l.names.Windows)
	}
	for _, b := range Browsers() {
		browserExes += len(b.Windows)
	}
	if total := selfRules + builtin + maxDecisions + browserExes; total > maxRules {
		t.Errorf("worst-case rule count %d (built-in %d, viewer %d) > %d", total, builtin, browserExes, maxRules)
	}
}

func TestOSNamesFor(t *testing.T) {
	n := OSNames{Windows: []string{"w.exe"}, MacOS: []string{"m"}, Linux: []string{"l"}}
	for _, tc := range []struct {
		os   string
		want []string
	}{
		{"windows", []string{"w.exe"}},
		{"darwin", []string{"m"}},
		{"macos", []string{"m"}},
		{"linux", []string{"l"}},
		{"ios", nil},
		{"android", nil},
		{"Windows", nil},
		{"", nil},
	} {
		if got := n.For(tc.os); !slices.Equal(got, tc.want) {
			t.Errorf("For(%q) = %q, want %q", tc.os, got, tc.want)
		}
		if got, want := n.Contains(tc.os, "w.exe"), tc.os == "windows"; got != want {
			t.Errorf("Contains(%q, w.exe) = %v, want %v", tc.os, got, want)
		}
	}
}

func TestLookups(t *testing.T) {
	if a, ok := VoiceAppByID("discord"); !ok || a.Name != "Discord" || !a.Contains("windows", "discordcanary.exe") {
		t.Errorf("VoiceAppByID(discord) = %+v, %v", a, ok)
	}
	if a, ok := VoiceAppByID("teams"); !ok || a.Name != "Microsoft Teams" {
		t.Errorf("VoiceAppByID(teams) = %+v, %v", a, ok)
	}
	for _, id := range []string{"exe:discord.exe", "Discord", "isshoni", "kook", ""} {
		if a, ok := VoiceAppByID(id); ok {
			t.Errorf("VoiceAppByID(%q) = %+v, want none", id, a)
		}
	}

	for _, tc := range []struct{ os, name, want string }{
		{"windows", "discordptb.exe", "discord"},
		{"windows", "equibop.exe", "vesktop"},
		{"windows", "ts3client_win32.exe", "teamspeak"},
		{"windows", "teamspeak.exe", "teamspeak"},
		{"windows", "ms-teams.exe", "teams"},
		{"windows", "teams.exe", "teams"},
		{"windows", "Discord.exe", ""}, // exact names: callers lower-case first (05 §5.1)
		{"windows", "discord", ""},
		{"windows", "update.exe", ""}, // Squirrel's updater is not Discord
		{"windows", "kook.exe", ""},   // other voice apps are caught by mic detection (05 §5.4)
		{"darwin", "discord.exe", ""}, // no macOS data in M2
		{"linux", "discord.exe", ""},
	} {
		a, ok := VoiceAppByProcess(tc.os, tc.name)
		if ok != (tc.want != "") || a.ID != tc.want {
			t.Errorf("VoiceAppByProcess(%q, %q) = %q, %v; want %q", tc.os, tc.name, a.ID, ok, tc.want)
		}
	}

	for _, tc := range []struct{ os, name, want string }{
		{"windows", "chrome.exe", FamilyChrome},
		{"windows", "brave.exe", FamilyChrome},
		{"windows", "msedge.exe", FamilyEdge},
		{"windows", "zen.exe", FamilyFirefox},
		{"windows", "msedgewebview2.exe", ""},
		{"windows", "Chrome.exe", ""},
		{"windows", "iexplore.exe", ""},
		{"macos", "firefox.exe", ""},
	} {
		b, ok := BrowserByProcess(tc.os, tc.name)
		if ok != (tc.want != "") || b.Family != tc.want {
			t.Errorf("BrowserByProcess(%q, %q) = %q, %v; want %q", tc.os, tc.name, b.Family, ok, tc.want)
		}
	}

	if !Launchers().Contains("windows", "steam.exe") || !Launchers().Contains("windows", "battle.net launcher.exe") {
		t.Error("Launchers() misses steam.exe or battle.net launcher.exe")
	}
	if Launchers().Contains("windows", "discord.exe") || Launchers().Contains("darwin", "steam.exe") {
		t.Error("Launchers() contains discord.exe on Windows or steam.exe on macOS")
	}
}

// The functions return copies: a caller that changes a result changes neither the built-in data nor what the next
// caller gets (the built-in voice apps can't be turned off).
func TestResultsAreCopies(t *testing.T) {
	apps := VoiceApps()
	apps[0].ID = "changed"
	apps[0].Windows[0] = "changed.exe"
	apps[0].MacOS = append(apps[0].MacOS, "changed")
	ln := Launchers()
	ln.Windows[0] = "changed.exe"
	r := SharedRuntimes()
	r.Windows[0] = "changed.exe"
	bs := Browsers()
	bs[0].Family = "changed"
	bs[0].Windows[0] = "changed.exe"
	a, _ := VoiceAppByID("zoom")
	a.Windows[0] = "changed.exe"
	b, _ := BrowserByFamily(FamilyEdge)
	b.Windows[0] = "changed.exe"

	for _, l := range allLists() {
		for _, os := range allOS {
			if slices.Contains(l.names.For(os), "changed.exe") || slices.Contains(l.names.For(os), "changed") {
				t.Errorf("%s (%s) was changed through a returned value", l.what, os)
			}
		}
	}
	if got := VoiceApps()[0]; got.ID != "discord" || got.Windows[0] != "discord.exe" || len(got.MacOS) != 0 {
		t.Errorf("VoiceApps()[0] = %+v after changing a copy", got)
	}
	if got := Browsers()[0]; got.Family != FamilyChrome || got.Windows[0] != "chrome.exe" {
		t.Errorf("Browsers()[0] = %+v after changing a copy", got)
	}
}
