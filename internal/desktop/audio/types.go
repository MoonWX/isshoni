package audio

// This file holds the service's data types (05 §14.2, §14.5). The JSON shapes are what the SPA reads through 03's
// bridge (camelCase); 03 generates their TypeScript from these types.

// Decision is a user's decision about an app.
type Decision string

// The decisions. DecisionAuto forgets a saved decision (SetAppDecision only; Decide accepts keep-out and include).
const (
	DecisionKeepOut Decision = "keep-out"
	DecisionInclude Decision = "include"
	DecisionAuto    Decision = "auto"
)

// SelfInfo is what the service needs to know about the running app (03 §6.1).
type SelfInfo struct {
	PID uint32 // os.Getpid(); 0 adds no instance rule
	// WebViewDataDir is the WebView2 user data folder the host sets (WebView2 adds \EBWebView below it, 05 §5.3);
	// "" adds no data-folder rule. A path: never logged.
	WebViewDataDir string
	// Exe is the binary that SoundCheck starts with --sound-check-helper (03: os.Executable()); "" = no helper (the
	// fake engine, e2ehost, the audiotest). A path: never logged.
	Exe string
}

// AppRule is one remembered decision (03's Settings → Audio).
type AppRule struct {
	ID       string   `json:"id"`       // "exe:kook.exe"
	Name     string   `json:"name"`     // the display name; the exe name when the display name is unknown
	Decision Decision `json:"decision"` // "keep-out" | "include"
}

// Event is one service event, which 03 forwards to the SPA as "isshoni:" + Name (03 §6.3).
type Event struct {
	Name    string // EventReport | EventLevels | EventPrompt | EventAlert
	Payload any    // FriendsHear | Levels | Prompt | Banner
}

// The event names (05 §14.4).
const (
	// EventReport carries FriendsHear: emitted when it changes, and at most 4 times a second for engine reports.
	EventReport = "audio.report"
	// EventLevels carries Levels at 10 Hz while a level meter is on screen (WatchLevels).
	EventLevels = "audio.levels"
	// EventPrompt carries a new Prompt, so 03 can show a toast while the window is hidden. It comes with the
	// audio.report that lists the prompt.
	EventPrompt = "audio.prompt"
	// EventAlert carries an error-level Banner that appeared during a live share: 03 turns it into a toast whatever
	// the window's state (05 §14.7, "Outside the window").
	EventAlert = "audio.alert"
)

// FriendsHear is "What friends hear": the share sheet's data (05 §14.5).
type FriendsHear struct {
	Seq             uint64 `json:"seq"`             // goes up whenever anything else in it changes
	Live            bool   `json:"live"`            // a share is running
	Mode            string `json:"mode"`            // ModeExcludeApps; ModeOff while the live share has no sound
	Engine          string `json:"engine"`          // "include-set" | "exclude-self" | "endpoint" | "none"
	Muted           bool   `json:"muted"`           // the report's muted (01's pause mute)
	Paused          bool   `json:"paused"`          // SetPaused, or the report's paused
	KeepOutMicUsers bool   `json:"keepOutMicUsers"` // the setting
	// Heard, KeptOut and NotCaptured are the report's rows by verdict, each sorted: the shared app first, then the
	// playing ones, then by name.
	Heard       []AppRow       `json:"heard"`
	KeptOut     []AppRow       `json:"keptOut"`
	NotCaptured []AppRow       `json:"notCaptured"`
	Prompts     []Prompt       `json:"prompts"`   // pending mic prompts, oldest first, at most 3
	Banners     []Banner       `json:"banners"`   // degraded modes: always shown, not dismissable
	Tips        []Tip          `json:"tips"`      // dismissable (DismissTip)
	VoiceApps   []VoiceAppInfo `json:"voiceApps"` // the built-in voice apps: "Friends won't hear: Discord, …"
	Alert       Alert          `json:"alert"`     // what the tray and the pill show outside the window
}

// The values of FriendsHear.Mode.
const (
	ModeExcludeApps = "exclude-apps"
	ModeOff         = "off"
)

// Alert is what 03's tray and pill show outside the window (05 §14.7).
type Alert struct {
	VoiceAppsHeard bool `json:"voiceAppsHeard"` // live, and an error-level banner shows
	Questions      int  `json:"questions"`      // len(FriendsHear.Prompts)
}

// AppRow is one app of FriendsHear. Its name is local only: never logged, never sent anywhere.
type AppRow struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Reason is, kept out: "builtin", "user", "mic", "viewer", "isshoni"; not captured: "launcher", "tree",
	// "pid-reuse", "limit", "capture-failed"; heard: "".
	Reason   string   `json:"reason"`
	Detail   string   `json:"detail,omitempty"` // "tree", "pid-reuse": the kept-out app's name
	Playing  bool     `json:"playing"`
	UsesMic  bool     `json:"usesMic"`
	Browser  bool     `json:"browser"`
	Shared   bool     `json:"shared"`
	Control  string   `json:"control"`           // ControlLocked (built-in voice apps, isshoni) | ControlToggle
	Decision Decision `json:"decision"`          // the saved decision: "auto" | "keep-out" | "include"
	Warning  string   `json:"warning,omitempty"` // WarningSharedUsesMic | WarningSharedKeptOut | WarningIncludedUsesMic
}

// The values of AppRow.Control and AppRow.Warning.
const (
	ControlLocked = "locked"
	ControlToggle = "toggle"

	WarningSharedUsesMic   = "shared-uses-mic"
	WarningSharedKeptOut   = "shared-kept-out"
	WarningIncludedUsesMic = "included-uses-mic"
)

// VoiceAppInfo is one built-in voice app of FriendsHear.VoiceApps.
type VoiceAppInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Running bool   `json:"running"`
}

// Prompt is one pending mic prompt (05 §14.6): "{{app}} is using your microphone. Keep it out of your stream?"
type Prompt struct {
	ID    string `json:"id"`    // "p1", "p2", … for this run
	AppID string `json:"appId"` // "exe:kook.exe"
	Name  string `json:"name"`
	Kind  string `json:"kind"`  // PromptMic | PromptBrowserMic
	Since int64  `json:"since"` // when it appeared, in ms since the Unix epoch
}

// The values of Prompt.Kind.
const (
	PromptMic        = "mic"
	PromptBrowserMic = "browser-mic"
)

// Banner is one degraded mode (05 §14.7): always shown, never dismissable.
type Banner struct {
	Code   string         `json:"code"`
	Level  string         `json:"level"`            // LevelError | LevelWarning | LevelInfo
	Params map[string]any `json:"params,omitempty"` // sound-off: cause ("off" | "paused"); capture-trouble: app
}

// The banner codes and levels (05 §14.7).
const (
	BannerExclusionUnavailable = "exclusion-unavailable" // error
	BannerWindowsTooOld        = "windows-too-old"       // error
	BannerNoOutputDevice       = "no-output-device"      // warning
	BannerReconnectingSound    = "reconnecting-sound"    // info
	BannerSoundOff             = "sound-off"             // info; params.cause "off" or "paused"
	BannerCaptureTrouble       = "capture-trouble"       // warning; params.app
	BannerViewerMuted          = "viewer-muted"          // info
	BannerViewerUnknownBrowser = "viewer-unknown-browser"

	LevelError   = "error"
	LevelWarning = "warning"
	LevelInfo    = "info"
)

// Tip is one dismissable tip (05 §14.7).
type Tip struct {
	Code string `json:"code"` // TipDucking
}

// TipDucking is the ducking tip: Windows turns other sounds down during calls, and friends hear that too.
const TipDucking = "ducking"

// Levels are the meters (05 §14.5), at 10 Hz while WatchLevels counts a meter on screen.
type Levels struct {
	Seq       uint64              `json:"seq"` // goes up with every sample
	MixPeakDB float64             `json:"mixPeakDb"`
	MixRMSDB  float64             `json:"mixRmsDb"`
	Apps      map[string]AppLevel `json:"apps"` // by app id
}

// AppLevel is one app's level in Levels.
type AppLevel struct {
	PeakDB float64 `json:"peakDb"`
	Source string  `json:"source"` // "captured" (post-fade: what friends hear) | "activity" (the session's meter)
}
