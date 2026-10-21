package audiorules

import "slices"

// OSNames is one list of process names per desktop OS.
type OSNames struct {
	Windows []string // exe file names in lower case, e.g. "discord.exe" (05 §5.1)
	MacOS   []string // M3; empty in M2
	Linux   []string // M4; empty in M2
}

// For returns the names for os: a GOOS value ("windows", "darwin", "linux") or M1's protocol.ClientOS ("windows",
// "macos", "linux"). Any other value gets nil.
func (n OSNames) For(os string) []string {
	switch os {
	case "windows":
		return n.Windows
	case "darwin", "macos":
		return n.MacOS
	case "linux":
		return n.Linux
	}
	return nil
}

// Contains reports whether name is in the list for os (see For). Names compare exactly: on Windows, pass the exe
// file name in lower case, the app identity of 05 §5.1.
func (n OSNames) Contains(os, name string) bool {
	return slices.Contains(n.For(os), name)
}

func (n OSNames) clone() OSNames {
	return OSNames{Windows: slices.Clone(n.Windows), MacOS: slices.Clone(n.MacOS), Linux: slices.Clone(n.Linux)}
}

// VoiceApp is one built-in voice app. Every process whose app walk hits one of its names is kept out with source
// BUILTIN and reason "builtin", and the user can't include it (05 §5.3 row 2; the service's ErrLockedApp, 05 §14.2).
type VoiceApp struct {
	ID   string // app id of the report rows and decisions, e.g. "discord": [a-z0-9._-]{1,63} (05 §4.3), never "isshoni"
	Name string // display name, e.g. "Discord": the name the service puts into the app's rules (05 §5.1)
	OSNames
}

// Browser family names: M1's client.browser values for the browsers of the viewer rule (05 §5.9 step 3). M1's other
// values, "safari" and "other", are no family here: the service adds no rule for them and shows a banner instead.
const (
	FamilyChrome  = "chrome"  // Chrome and the other Chromium browsers, except Edge
	FamilyEdge    = "edge"    // Microsoft Edge (Chromium)
	FamilyFirefox = "firefox" // Firefox and its forks
)

// Browser is one browser family and the browsers in it. A browser plays all its tabs from one process tree in its
// own folder, so the whole browser is one app (05 §5.5). When a tab of a family on Windows reports that it plays the
// user's isshoni viewer, the service keeps out every exe of that family whose session is active (05 §5.9).
type Browser struct {
	Family string // FamilyChrome, FamilyEdge or FamilyFirefox
	OSNames
}

// The built-in data, in the order of 05 §5.4's tables. Read it only through the functions below, which copy it.
var (
	voiceApps = []VoiceApp{
		{ID: "discord", Name: "Discord", OSNames: OSNames{Windows: []string{
			"discord.exe", "discordptb.exe", "discordcanary.exe", "discorddevelopment.exe",
		}}},
		{ID: "vesktop", Name: "Vesktop", OSNames: OSNames{Windows: []string{
			"vesktop.exe", "equibop.exe",
		}}},
		{ID: "teamspeak", Name: "TeamSpeak", OSNames: OSNames{Windows: []string{
			"ts3client_win64.exe", "ts3client_win32.exe", // TeamSpeak 3
			"teamspeak.exe", // TeamSpeak 5 and 6
		}}},
		{ID: "mumble", Name: "Mumble", OSNames: OSNames{Windows: []string{
			"mumble.exe",
		}}},
		{ID: "zoom", Name: "Zoom", OSNames: OSNames{Windows: []string{
			"zoom.exe",
		}}},
		{ID: "teams", Name: "Microsoft Teams", OSNames: OSNames{Windows: []string{
			"ms-teams.exe", // new Teams
			"teams.exe",    // classic Teams
		}}},
		{ID: "slack", Name: "Slack", OSNames: OSNames{Windows: []string{
			"slack.exe",
		}}},
	}

	// launchers are the names of S1 plus the plan's: game launchers, the shell and system hosts, and consoles. A
	// launcher's OS tree is everything started from it, so it is never an include target either (05 §5.4).
	launchers = OSNames{Windows: []string{
		// The shell and game launchers.
		"explorer.exe", "steam.exe", "epicgameslauncher.exe", "battle.net.exe", "battle.net launcher.exe",
		"galaxyclient.exe", "eadesktop.exe", "ealauncher.exe", "upc.exe", "riotclientservices.exe", "heroic.exe",
		"playnite.desktopapp.exe", "playnite.fullscreenapp.exe", "xboxpcapp.exe", "gamelaunchhelper.exe",
		"overwolf.exe", "itch.exe",
		// System processes and hosts.
		"services.exe", "svchost.exe", "wininit.exe", "winlogon.exe", "userinit.exe", "sihost.exe",
		"runtimebroker.exe", "dllhost.exe", "taskhostw.exe", "applicationframehost.exe",
		// Shells and consoles.
		"cmd.exe", "powershell.exe", "pwsh.exe", "windowsterminal.exe", "openconsole.exe", "conhost.exe", "wsl.exe",
		"bash.exe",
	}}

	sharedRuntimes = OSNames{Windows: []string{
		"msedgewebview2.exe", // WebView2, which plays the sound of apps like new Teams and of isshoni's own window
	}}

	browsers = []Browser{
		{Family: FamilyChrome, OSNames: OSNames{Windows: []string{
			"chrome.exe", "chromium.exe", "brave.exe", "vivaldi.exe", "opera.exe", "arc.exe", "thorium.exe",
		}}},
		{Family: FamilyEdge, OSNames: OSNames{Windows: []string{
			"msedge.exe",
		}}},
		{Family: FamilyFirefox, OSNames: OSNames{Windows: []string{
			"firefox.exe", "librewolf.exe", "waterfox.exe", "floorp.exe", "zen.exe",
		}}},
	}
)

// VoiceApps returns the built-in voice apps (05 §5.4), always kept out. The result is a fresh copy.
func VoiceApps() []VoiceApp {
	out := make([]VoiceApp, len(voiceApps))
	for i, a := range voiceApps {
		out[i] = VoiceApp{ID: a.ID, Name: a.Name, OSNames: a.clone()}
	}
	return out
}

// Launchers returns the launchers and shells (IM_RULE_STOP): the app walk stops at them, and a process started from
// one is its own app (05 §5.2). The result is a fresh copy.
func Launchers() OSNames { return launchers.clone() }

// SharedRuntimes returns the shared runtimes (IM_RULE_SHARED_RUNTIME): a process of one joins its parent's app even
// from another folder (05 §5.2). The result is a fresh copy.
func SharedRuntimes() OSNames { return sharedRuntimes.clone() }

// Browsers returns the browser families (IM_RULE_BROWSER) with their browsers (05 §5.4). The result is a fresh copy.
func Browsers() []Browser {
	out := make([]Browser, len(browsers))
	for i, b := range browsers {
		out[i] = Browser{Family: b.Family, OSNames: b.clone()}
	}
	return out
}

// VoiceAppByID returns the built-in voice app with app id id ("discord"). An "exe:" id is no voice app's id: look its
// name up with VoiceAppByProcess.
func VoiceAppByID(id string) (VoiceApp, bool) {
	for _, a := range VoiceApps() {
		if a.ID == id {
			return a, true
		}
	}
	return VoiceApp{}, false
}

// VoiceAppByProcess returns the built-in voice app that lists name for os (see OSNames.For and OSNames.Contains):
// "discordptb.exe" on "windows" gives Discord.
func VoiceAppByProcess(os, name string) (VoiceApp, bool) {
	for _, a := range VoiceApps() {
		if a.Contains(os, name) {
			return a, true
		}
	}
	return VoiceApp{}, false
}

// BrowserByFamily returns the browser family named family (FamilyChrome, FamilyEdge or FamilyFirefox). "safari",
// "other" and unknown values have none.
func BrowserByFamily(family string) (Browser, bool) {
	for _, b := range Browsers() {
		if b.Family == family {
			return b, true
		}
	}
	return Browser{}, false
}

// BrowserByProcess returns the browser family that lists name for os (see OSNames.For and OSNames.Contains):
// "brave.exe" on "windows" gives the chrome family. "msedgewebview2.exe" is a shared runtime, not a browser.
func BrowserByProcess(os, name string) (Browser, bool) {
	for _, b := range Browsers() {
		if b.Contains(os, name) {
			return b, true
		}
	}
	return Browser{}, false
}
