package share

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/MoonWX/isshoni/internal/client/core"
	"github.com/MoonWX/isshoni/internal/desktop/settings"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/protocol"
)

// The controller's bounds (03 §6.2, §6.3, §10.1, §20).
const (
	// PickHold is how long a pick is held without Start: 300 s from the pick, 04's pick-token life (04 §7.2), so a
	// friend who reads the first-share sheet or follows the DND tip into Windows Settings comes back to a pick that
	// still works (03 §10.1, §20).
	PickHold = 300 * time.Second
	// ReadyTimeout is how long Start (and StopOther) wait for C2 to be online; then publisher_offline (03 §9).
	ReadyTimeout = 10 * time.Second
	// StartShareTimeout bounds 01's StartShare after the wait for C2, so both fit Share.Start's 20 s (03 §6.2).
	StartShareTimeout = 10 * time.Second
	// StateEvery is the shortest gap between two share.state events: at most 10 a second (03 §6.3).
	StateEvery = 100 * time.Millisecond
	// StatsEvery is the share.stats period while a share is live (03 §6.3).
	StatsEvery = 2 * time.Second
	// ReconnectingAfter is how long 01's ShareState must stay reconnecting before the view shows it: 01 reports
	// reconnecting for every signaling drop and pub PC recovery, also the short ones a resume or an ICE restart cures
	// unseen (group 4's WA38 hand-over), and the panel, tray and pill shouldn't flash "Reconnecting…" for those.
	ReconnectingAfter = 2 * time.Second
	// CloseTimeout bounds Close's stop of a share that is still running.
	CloseTimeout = 3 * time.Second
)

// The controller's errors. internal/desktop/bridge maps each to its 03 §6.4 code (named here); errors from 01's core
// that the controller passes on are *protocol.Error (wire codes), *media.Error (engine.<code>) and 02's
// deviceauth.ErrNotLinked or ErrIncompatible from C2's session (not_linked, incompatible_server).
var (
	// ErrAlreadySharing: a share is starting or running (one at a time, 01 D12): already_sharing.
	ErrAlreadySharing = errors.New("share: already sharing")
	// ErrNotSharing: the call needs a running share: not_sharing.
	ErrNotSharing = errors.New("share: not sharing")
	// ErrPickExpired: the pickId isn't held (released, replaced by a new pick, or older than PickHold):
	// pick_expired, with params.reason pick_expired like the engine's own (03 §10.5).
	ErrPickExpired = errors.New("share: the pick expired")
	// ErrPublisherOffline: C2 wasn't online within ReadyTimeout: publisher_offline (03 §9).
	ErrPublisherOffline = errors.New("share: the publisher connection isn't ready")
	// ErrFeatureDisabled: the server lacks share.pause (03 §6.2 Share.SetPaused): feature_disabled.
	ErrFeatureDisabled = errors.New("share: the server has no share.pause")
	// ErrBusy: StopOther while C2 is in another room (01's Client.StopOther): busy.
	ErrBusy = errors.New("share: the publisher connection is in another room")
	// ErrInvalid: an argument the bridge should have refused (an unknown preset): invalid_argument.
	ErrInvalid = errors.New("share: invalid argument")
	// ErrClosed: the controller was closed (the app is quitting): internal.
	ErrClosed = errors.New("share: closed")
)

// State is the share's state as the SPA shows it (03 §6.3 ShareView.state). picking and stopping are the
// controller's own; the others map 01's ShareState and Paused (mapping.go).
type State string

// The states.
const (
	StatePicking      State = "picking"
	StateStarting     State = "starting"
	StateLive         State = "live"
	StatePaused       State = "paused"
	StateReconnecting State = "reconnecting"
	StateUnreachable  State = "unreachable"
	StateStopping     State = "stopping"
	StateEnded        State = "ended"
	StateFailed       State = "failed"
)

// EndReason says why a share ended (03 §6.3 ShareView.endReason, §10.1's table).
type EndReason string

// The end reasons.
const (
	EndUser              EndReason = "user"
	EndHotkey            EndReason = "hotkey"
	EndTray              EndReason = "tray" // also the pill's Stop
	EndSourceGone        EndReason = "source-gone"
	EndElsewhere         EndReason = "elsewhere" // stopped from another connection of the user
	EndServer            EndReason = "server"
	EndServerUnreachable EndReason = "server-unreachable"
	EndUnlinked          EndReason = "unlinked"
	EndSystem            EndReason = "system" // sleep, end of session
	EndError             EndReason = "error"
)

// Cause is who asked the controller to stop (Stop): it becomes the end reason of a share that 01 ends with reason
// user (03 §10.1's first row).
type Cause string

// The causes.
const (
	CauseUser   Cause = "user"   // the SPA (its Stop button, a room switch, leaving the room), Quit
	CauseHotkey Cause = "hotkey" // the Stop hotkey
	CauseTray   Cause = "tray"   // the tray menu or the pill's Stop
	CauseSystem Cause = "system" // sleep, end of session, the lock pause's 30 min bound
)

// HintKind is the share's hint (M1's ShareHint kinds, 03 §6.3).
type HintKind string

// The hints.
const (
	HintNone          HintKind = ""
	HintUploadLimited HintKind = "upload-limited"
	HintCPULimited    HintKind = "cpu-limited"
)

// Hint is the panel's upload or CPU hint: upload-limited with the height the upload allows (01's Upload), or
// cpu-limited (01's Overloaded).
type Hint struct {
	Kind         HintKind
	ApproxHeight int // upload-limited only: 360, 480, 540, 720, 900 or 1080
}

// Layer is one video layer of the Details disclosure (03 §10.3), from 01's LayerSnapshot.
type Layer struct {
	RID              string
	Width, Height    int
	FPS              float64
	TargetBps        int
	SentBps          int
	CaptureToSendP50 time.Duration
	CaptureToSendP95 time.Duration
}

// View is the share as the SPA sees it: 01's ShareSnapshot plus the controller's own fields (03 §6.3; the bridge
// turns it into its ShareView JSON). Label is the picker's name of the source: it stays on this PC (the SPA and the
// tray show it; 01's ShareRequest.Label is always "") and is never logged.
type View struct {
	State        State
	ShareID      string // 01's current share id ("" while starting); it changes when C2 publishes again (replaces)
	ConnectionID string // C2's connection id
	RoomID       string
	Kind         protocol.ShareKind // screen or window
	Label        string
	Preset       protocol.Preset
	WithAudio    bool // the share was started with sound
	AudioOn      bool // Share sound is on (WithAudio and not switched off)
	Border       bool // Windows draws its capture border: Source.OSBorder, then !NoOSIndicator once live
	Pill         bool // the Sharing pill is due: an active share without the OS border
	Minimized    bool // the shared window is minimized (04's SOURCE_MINIMIZED)
	Watching     int  // len(ShareSnapshot.Watchers)
	LiveSince    time.Time
	Hint         Hint
	Encoder      string // f's encoder ("nvenc", …)
	HW           bool
	Layers       []Layer
	Server       []protocol.ServerLayerStats // the server's view while WatchServerStats is on, else nil
	EndReason    EndReason                   // "" until the share ended
	// Err is a failed share's error (State failed): a *media.Error (engine.<code>), a *protocol.Error (a wire code)
	// or one of this package's errors. The bridge maps it to 03 §6.4's {code, params}.
	Err error
}

// Sheet is a one-time sheet the SPA shows before Start (03 §10.2), in Picked.Confirm's order.
type Sheet string

// The sheets.
const (
	SheetFirstShare Sheet = "first-share" // once per install (settings tips.firstShareShown)
	SheetDND        Sheet = "dnd"         // the first share of kind screen (tips.dndShown)
)

// Picked is a held pick (Pick's result): the controller's pickId (16 hex), the source's kind, its name for the local
// UI, whether Windows draws its capture border, and the sheets due.
type Picked struct {
	PickID  string
	Kind    protocol.ShareKind
	Label   string
	Border  bool
	Confirm []Sheet
}

// StartRequest is Start's argument: the held pick, the room the SPA is in, the preset ("" is auto) and whether to
// share sound.
type StartRequest struct {
	PickID    string
	RoomID    string
	Preset    protocol.Preset
	WithAudio bool
}

// Limit is a ShareStatsSample layer's limit (05 §8's shape, 03 §6.3).
type Limit string

// The limits.
const (
	LimitNone      Limit = "none"
	LimitBandwidth Limit = "bandwidth" // 01's Upload.Limited
	LimitCPU       Limit = "cpu"       // 01's Overloaded
)

// StatsLayer is one layer of a Stats sample.
type StatsLayer struct {
	RID           string
	Width, Height int
	FPS           float64
	SentBps       int
	Encoder       string
	HW            bool
	Limit         Limit
}

// Stats is the share.stats sample (M1's shape, 05 §8), from 01's snapshot.
type Stats struct {
	Layers       []StatsLayer
	AudioSentBps int
}

// Notice is a transient notice for the share.notice event: 01's EventNotice (encoder_fallback {layer, encoder}).
type Notice struct {
	Code   string
	Params map[string]string
}

// EventKind is the kind of an Event.
type EventKind uint8

// The events.
const (
	EventState  EventKind = iota + 1 // View changed (coalesced to StateEvery); View is nil when there is no share
	EventStats                       // a Stats sample, every StatsEvery while live
	EventNotice                      // a Notice
)

// Event is one change for the subscribers (the bridge's share.state, share.stats and share.notice events; later the
// tray and the pill). Exactly the field of its Kind is set, except that EventState's View is nil when no share
// exists; each Event has its own copies.
type Event struct {
	Kind   EventKind
	View   *View
	Stats  *Stats
	Notice *Notice
}

// Picker is what the controller uses of the media engine (01's media.Engine implements it): the OS picker owned by
// the main window, and Release for a pick nobody will start.
type Picker interface {
	Pick(ctx context.Context, req media.PickRequest) (media.Source, error)
	Release(id string)
}

var _ Picker = media.Engine(nil)

// Publisher is C2, the app's publisher connection (internal/desktop/session through the app).
type Publisher interface {
	// Ready returns C2's core client once it is online, and the origin of the server it is connected to. It waits
	// until ctx ends (then ctx's error); it fails at once with 02's deviceauth.ErrNotLinked when the active server
	// isn't linked, and with deviceauth.ErrIncompatible while it is incompatible.
	Ready(ctx context.Context) (Client, string, error)
}

// Client is C2's core client as the controller uses it; FromCore adapts 01's *core.Client.
type Client interface {
	Status() core.Status
	Subscribe() (<-chan core.Event, func())
	StartShare(ctx context.Context, r core.ShareRequest) (Share, error)
	StopOther(ctx context.Context, roomID, shareID string) error
	WatchServerStats(on bool)
}

// Share is one share of 01's core; *core.Share implements it.
type Share interface {
	Snapshot() core.ShareSnapshot
	SetPreset(ctx context.Context, p protocol.Preset) error
	SetPaused(ctx context.Context, paused bool) error
	Stop(ctx context.Context) error
	Done() <-chan struct{}
}

var _ Share = (*core.Share)(nil)

// CoreClient is the part of 01's *core.Client that FromCore adapts.
type CoreClient interface {
	Status() core.Status
	Subscribe() (<-chan core.Event, func())
	StartShare(ctx context.Context, r core.ShareRequest) (*core.Share, error)
	StopOther(ctx context.Context, roomID, shareID string) error
	WatchServerStats(on bool)
}

var _ CoreClient = (*core.Client)(nil)

// FromCore adapts a core client (01's *core.Client) to Client.
func FromCore(c CoreClient) Client { return coreClient{c} }

type coreClient struct{ CoreClient }

func (c coreClient) StartShare(ctx context.Context, r core.ShareRequest) (Share, error) {
	s, err := c.CoreClient.StartShare(ctx, r)
	if err != nil {
		return nil, err // no typed nil inside the interface
	}
	return s, nil
}

// AudioPolicy is what the controller calls of 05's *audio.Service (05 §14.2): the share's life, the pause's extra
// mute and the Share sound switch. 05's SetSharedPID at pick time is README WA77's hand-off.
type AudioPolicy interface {
	// OnShareStarted is called before StartShare; sharedPID is the shared window's process, 0 for a monitor.
	OnShareStarted(sharedPID uint32, withAudio bool)
	// OnShareStopped is called after every share end, and after a StartShare that failed.
	OnShareStopped()
	// SetPaused follows a pause or resume once 01's Share.SetPaused succeeded (05's extra mute, not the mechanism).
	SetPaused(paused bool)
	// SetShareSound is the share's Share sound switch: local only, silent Opus frames while off.
	SetShareSound(on bool) error
}

// Window is what the controller needs of the main window (app.Shell): shown and focused before the picker, which
// it owns.
type Window interface {
	ShowWindow(focus bool)
	MainWindowHandle() uintptr
}

// Settings is settings.json (*settings.Store): the one-time sheets' tips and serverPrefs[origin].preset.
type Settings interface {
	Get() settings.Settings
	Update(fn func(*settings.Settings)) error
}

var _ Settings = (*settings.Store)(nil)

// Options configure a Controller. Every field may be nil or zero: without Engine, Pick answers unsupported_os;
// without Publisher, Start answers publisher_offline; without Audio, Window or Settings, those calls are skipped (no
// sheet is due without Settings).
type Options struct {
	Engine    Picker
	Publisher Publisher
	Audio     AudioPolicy
	Window    Window
	Settings  Settings
	// PickHold is how long a pick is held; 0 means PickHold.
	PickHold time.Duration
	// Log gets the controller's lines (component=share); nil means slog.Default().
	Log *slog.Logger
}
