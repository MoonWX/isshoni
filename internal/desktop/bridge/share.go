package bridge

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/client/deeplink"
	"github.com/MoonWX/isshoni/internal/desktop/app"
	"github.com/MoonWX/isshoni/internal/desktop/bridge/codes"
	"github.com/MoonWX/isshoni/internal/desktop/share"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/protocol"
)

// Share is the bound Share service (03 §6.2, §10): the native share of this app, through the app's share
// controller (internal/desktop/share) and C2. Capabilities answers from the engine's start-time probe. The
// controller's events go out as share.state (ShareView, at most 10 a second), share.stats and share.notice.
type Share struct {
	core Core
	ctl  app.ShareController
	log  *slog.Logger

	mu          sync.Mutex
	serverStats int // Share.WatchServerStats' count; the controller gets count > 0 (01's WatchServerStats)
}

// ShareCaps is what this PC can share (Share.Capabilities, 03 §6.2): canShare with reason (a media code such as
// unsupported_os or no_encoder) when it can't, the usable encoders' kind tokens in 04's order, borderless (the
// capture border can be turned off), the OS build and the audio backend (process-loopback, endpoint or none).
type ShareCaps struct {
	CanShare     bool     `json:"canShare"`
	Reason       string   `json:"reason,omitempty"`
	Encoders     []string `json:"encoders"`
	Borderless   bool     `json:"borderless"`
	OSBuild      int      `json:"osBuild"`
	AudioBackend string   `json:"audioBackend"`
}

// Picked is a source picked in the OS picker and held by Go until Start or Release, at most 300 s (03 §10.1): kind
// window or screen, label (the picker's name, shown on this PC only and never sent), border (Windows draws its
// yellow capture border) and the one-time sheets to show first, in order (first-share, dnd).
type Picked struct {
	PickID  string   `json:"pickId"`
	Kind    string   `json:"kind"`
	Label   string   `json:"label"`
	Border  bool     `json:"border"`
	Confirm []string `json:"confirm"`
}

// StartRequest is Share.Start's argument: the pick, the room the SPA is in, the preset (auto, game, movie, text) and
// whether to share sound.
type StartRequest struct {
	PickID    string `json:"pickId"`
	RoomID    string `json:"roomId"`
	Preset    string `json:"preset"`
	WithAudio bool   `json:"withAudio"`
}

// ShareView is the native share as the SPA sees it (03 §6.3): Share.Current() and the share.state event. state is
// picking, starting, live, paused, reconnecting, unreachable, stopping, ended or failed; endReason user, hotkey,
// tray, source-gone, elsewhere, server, server-unreachable, unlinked, system or error; liveSince is RFC 3339.
type ShareView struct {
	State        string          `json:"state"`
	ShareID      string          `json:"shareId"`
	ConnectionID string          `json:"connectionId"`
	RoomID       string          `json:"roomId"`
	Kind         string          `json:"kind"`
	Label        string          `json:"label"`
	Preset       string          `json:"preset"`
	WithAudio    bool            `json:"withAudio"`
	AudioOn      bool            `json:"audioOn"`
	Border       bool            `json:"border"`
	Pill         bool            `json:"pill"`
	Minimized    bool            `json:"minimized"`
	Watching     int             `json:"watching"`
	LiveSince    *string         `json:"liveSince"`
	Hint         *ShareHint      `json:"hint"`
	Encoder      string          `json:"encoder"`
	HW           bool            `json:"hw"`
	Layers       []ShareLayer    `json:"layers"`
	Server       map[string]any  `json:"server"` // 01's server stats while WatchServerStats is on, else null
	EndReason    *string         `json:"endReason"`
	Error        *ShareViewError `json:"error"`
}

// ShareHint is M1's hint shape: {kind: upload-limited, approxHeight} or {kind: cpu-limited}.
type ShareHint struct {
	Kind         string `json:"kind"`
	ApproxHeight int    `json:"approxHeight,omitempty"`
}

// ShareLayer is one simulcast layer in the Details disclosure (03 §10.3).
type ShareLayer struct {
	RID             string       `json:"rid"`
	Width           int          `json:"width"`
	Height          int          `json:"height"`
	FPS             int          `json:"fps"`
	TargetKbps      int          `json:"targetKbps"`
	SentKbps        int          `json:"sentKbps"`
	CaptureToSendMs LatencyRange `json:"captureToSendMs"`
}

// LatencyRange is a median and 95th percentile in milliseconds.
type LatencyRange struct {
	P50 int `json:"p50"`
	P95 int `json:"p95"`
}

// ShareViewError is a failed share's error, 03 §6.4's {code, params}.
type ShareViewError struct {
	Code   string         `json:"code"`
	Params map[string]any `json:"params,omitempty"`
}

// ShareStatsSample is M1's stats shape (05 §8) for Share.Stats() and the share.stats event; the TypeScript wrapper
// adds `at` on receipt. limit is none, bandwidth or cpu.
type ShareStatsSample struct {
	Layers    []ShareStatsLayer `json:"layers"`
	AudioKbps int               `json:"audioKbps"`
}

// ShareStatsLayer is one layer of a ShareStatsSample.
type ShareStatsLayer struct {
	RID     string `json:"rid"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	FPS     int    `json:"fps"`
	Kbps    int    `json:"kbps"`
	Encoder string `json:"encoder"`
	HW      bool   `json:"hw"`
	Limit   string `json:"limit"`
}

// Capabilities answers from the engine's start-time probe, waiting for it at most 2 s (03 §6.1). It never fails:
// without an engine, an unsupported OS or no usable encoder, canShare is false with the media code as reason.
func (s *Share) Capabilities(ctx context.Context) ShareCaps {
	p, err := s.core.Probe(ctx)
	caps := ShareCaps{Encoders: []string{}, AudioBackend: "none"}
	if err != nil {
		caps.Reason = string(media.CodeInternal)
		var me *media.Error
		if errors.As(err, &me) && me.Code != "" {
			caps.Reason = string(me.Code)
		}
		return caps
	}
	caps.Borderless = p.Capture.Borderless
	caps.OSBuild = p.OS.Build
	if p.Audio.Backend != "" {
		caps.AudioBackend = p.Audio.Backend
	}
	// The probe can list a kind once per adapter (EncoderInfo.Adapter): each usable kind once, in 04's order.
	seen := map[string]bool{}
	for _, e := range p.Encoders {
		if e.Tested && e.Status == "" && !seen[e.Name] {
			seen[e.Name] = true
			caps.Encoders = append(caps.Encoders, e.Name)
		}
	}
	switch {
	case !p.OS.Supported:
		caps.Reason = string(media.CodeUnsupportedOS)
	case len(caps.Encoders) == 0:
		caps.Reason = string(media.CodeNoEncoder)
	default:
		caps.CanShare = true
	}
	return caps
}

// Pick opens the OS picker owned by the main window (03 §10.1); null when cancelled (or timed out: the picker's
// own 120 s, so no method bound applies). already_sharing, engine.<code> (engine.invalid with params.reason
// self_window for an isshoni window: nothing is held, and the SPA opens the picker again, 03 §10.5).
func (s *Share) Pick(ctx context.Context) (*Picked, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	p, err := s.ctl.Pick(ctx)
	if err != nil {
		return nil, mapError(err, s.log)
	}
	if p == nil {
		return nil, nil
	}
	confirm := make([]string, 0, len(p.Confirm))
	for _, c := range p.Confirm {
		confirm = append(confirm, string(c))
	}
	return &Picked{PickID: p.PickID, Kind: string(p.Kind), Label: p.Label, Border: p.Border, Confirm: confirm}, nil
}

// Release drops a held pick: the user backed out before Start. Another id is a no-op.
func (s *Share) Release(pickID string) { s.ctl.Release(pickID) }

// Start starts a share of a held pick in roomId (03 §10.1), at most 20 s: C2 online within 10 s
// (publisher_offline), then 01's StartShare. It resolves at starting, and live comes as an event. invalid_argument
// (params.field pickId, roomId or preset), pick_expired, already_sharing, not_linked, publisher_offline,
// incompatible_server, engine.<code>, wire codes (share_limit, room_full, room_not_found, forbidden, …).
func (s *Share) Start(ctx context.Context, req StartRequest) (ShareView, error) {
	switch {
	case !validPickID(req.PickID):
		return ShareView{}, invalidArgument("pickId")
	case !deeplink.ValidRoomID(req.RoomID):
		return ShareView{}, invalidArgument("roomId")
	case req.Preset != "" && !protocol.Preset(req.Preset).Valid():
		return ShareView{}, invalidArgument("preset")
	}
	ctx, cancel := withTimeout(ctx, startTimeout)
	defer cancel()
	v, err := s.ctl.Start(ctx, share.StartRequest{PickID: req.PickID, RoomID: req.RoomID,
		Preset: protocol.Preset(req.Preset), WithAudio: req.WithAudio})
	if err != nil {
		return ShareView{}, mapError(err, s.log)
	}
	return s.view(v), nil
}

// validPickID reports a pickId's shape: 16 lower-case hex characters (the controller's).
func validPickID(id string) bool {
	if len(id) != 16 {
		return false
	}
	for i := range len(id) {
		if c := id[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Current returns the share this app publishes, as the share.state event shows it: the share being started or
// running, the picker's view while it is open, the last share's final view (ended, failed) until the next pick;
// null before any.
func (s *Share) Current() *ShareView {
	v := s.ctl.Current()
	if v == nil {
		return nil
	}
	sv := s.view(*v)
	return &sv
}

// Stop stops the share (endReason user); it never fails and is idempotent. The server's replies are awaited at
// most 10 s; the local part always runs.
func (s *Share) Stop(ctx context.Context) error {
	ctx, cancel := withTimeout(ctx, ioTimeout)
	defer cancel()
	s.ctl.Stop(ctx, share.CauseUser)
	return nil
}

// StopOther sends share.stop on C2 for another connection's share of this user (01's Client.StopOther), at most
// 10 s: invalid_argument (roomId, shareId), publisher_offline, not_linked, busy (C2 shares in another room), wire
// codes.
func (s *Share) StopOther(ctx context.Context, roomID, shareID string) error {
	switch {
	case !deeplink.ValidRoomID(roomID):
		return invalidArgument("roomId")
	case !deeplink.ValidRoomID(shareID): // share ids follow the same id rule (01 §4.1)
		return invalidArgument("shareId")
	}
	ctx, cancel := withTimeout(ctx, ioTimeout)
	defer cancel()
	return mapError(s.ctl.StopOther(ctx, roomID, shareID), s.log)
}

// SetPaused pauses or resumes the share (03 §11.3): feature_disabled (a server without share.pause), not_sharing,
// publisher_offline, wire codes.
func (s *Share) SetPaused(ctx context.Context, paused bool) error {
	ctx, cancel := withTimeout(ctx, ioTimeout)
	defer cancel()
	return mapError(s.ctl.SetPaused(ctx, paused), s.log)
}

// SetPreset changes the share's preset (01's Share.SetPreset: C2 sends share.update): invalid_argument (preset),
// not_sharing, publisher_offline, wire codes.
func (s *Share) SetPreset(ctx context.Context, preset string) error {
	if preset == "" || !protocol.Preset(preset).Valid() {
		return invalidArgument("preset")
	}
	ctx, cancel := withTimeout(ctx, ioTimeout)
	defer cancel()
	return mapError(s.ctl.SetPreset(ctx, protocol.Preset(preset)), s.log)
}

// SetAudioEnabled is the share's Share sound switch (05's SetShareSound; no wire message): not_sharing. A share
// started without sound has no switch, and nothing happens.
func (s *Share) SetAudioEnabled(on bool) error { return mapError(s.ctl.SetAudioEnabled(on), s.log) }

// Stats returns the share's latest stats sample, null unless a share is live or paused.
func (s *Share) Stats() *ShareStatsSample {
	st := s.ctl.Stats()
	if st == nil {
		return nil
	}
	out := statsSample(st)
	return &out
}

// WatchServerStats counts the open Details disclosures (03 §10.3): while the count is above 0 the controller asks
// 01's core for the server's stats. Unbalanced offs never take it below 0.
func (s *Share) WatchServerStats(on bool) {
	s.mu.Lock()
	before := s.serverStats
	s.serverStats = count(s.serverStats, on)
	after := s.serverStats
	s.mu.Unlock()
	if (before > 0) != (after > 0) {
		s.ctl.WatchServerStats(after > 0)
	}
}

// noShares stands in for a share controller when the app has none (a Core that isn't *app.App: tests and tools):
// no share can exist, and the calls that would make one are not_implemented.
type noShares struct{}

var _ app.ShareController = noShares{}

func (noShares) Pick(context.Context) (*share.Picked, error) {
	return nil, notImplemented("Share.Pick")
}
func (noShares) Release(string) {}
func (noShares) Start(context.Context, share.StartRequest) (share.View, error) {
	return share.View{}, notImplemented("Share.Start")
}
func (noShares) Current() *share.View                  { return nil }
func (noShares) Stop(context.Context, share.Cause)     {}
func (noShares) Stats() *share.Stats                   { return nil }
func (noShares) WatchServerStats(bool)                 {}
func (noShares) Subscribe(func(share.Event)) func()    { return func() {} }
func (noShares) SetAudioEnabled(bool) error            { return notImplemented("Share.SetAudioEnabled") }
func (noShares) SetPaused(context.Context, bool) error { return notImplemented("Share.SetPaused") }
func (noShares) SetPreset(context.Context, protocol.Preset) error {
	return notImplemented("Share.SetPreset")
}
func (noShares) StopOther(context.Context, string, string) error {
	return notImplemented("Share.StopOther")
}

// count is a reference count's next value: +1 for on, −1 for off, never below 0.
func count(n int, on bool) int {
	if on {
		return n + 1
	}
	return max(n-1, 0)
}

// forward sends one of the controller's events to the page (03 §6.3).
func (s *Share) forward(ev share.Event, emit func(string, any)) {
	switch ev.Kind {
	case share.EventState:
		if ev.View == nil {
			emit(EventShareState, nil)
			return
		}
		emit(EventShareState, s.view(*ev.View))
	case share.EventStats:
		if ev.Stats != nil {
			emit(EventShareStats, statsSample(ev.Stats))
		}
	case share.EventNotice:
		if ev.Notice != nil {
			n := ShareNotice{Code: ev.Notice.Code}
			if len(ev.Notice.Params) > 0 {
				n.Params = make(map[string]any, len(ev.Notice.Params))
				for k, v := range ev.Notice.Params {
					n.Params[k] = v
				}
			}
			emit(EventShareNotice, n)
		}
	}
}

// view is the controller's View as the ShareView JSON (03 §6.3): kbit/s from bit/s, milliseconds, liveSince in RFC
// 3339 UTC with milliseconds, null for the absent hint, server stats, end reason and error, and a failed share's
// error as 03 §6.4's {code, params}.
func (s *Share) view(v share.View) ShareView {
	out := ShareView{
		State: string(v.State), ShareID: v.ShareID, ConnectionID: v.ConnectionID, RoomID: v.RoomID,
		Kind: string(v.Kind), Label: v.Label, Preset: string(v.Preset), WithAudio: v.WithAudio, AudioOn: v.AudioOn,
		Border: v.Border, Pill: v.Pill, Minimized: v.Minimized, Watching: v.Watching, Encoder: v.Encoder, HW: v.HW,
		Layers: make([]ShareLayer, 0, len(v.Layers)),
	}
	if !v.LiveSince.IsZero() {
		t := v.LiveSince.UTC().Format("2006-01-02T15:04:05.000Z07:00")
		out.LiveSince = &t
	}
	if v.Hint.Kind != share.HintNone {
		out.Hint = &ShareHint{Kind: string(v.Hint.Kind), ApproxHeight: v.Hint.ApproxHeight}
	}
	for _, l := range v.Layers {
		out.Layers = append(out.Layers, ShareLayer{RID: l.RID, Width: l.Width, Height: l.Height, FPS: fps(l.FPS),
			TargetKbps: l.TargetBps / 1000, SentKbps: l.SentBps / 1000,
			CaptureToSendMs: LatencyRange{P50: millis(l.CaptureToSendP50), P95: millis(l.CaptureToSendP95)}})
	}
	if v.Server != nil {
		out.Server = map[string]any{"layers": v.Server}
	}
	if v.EndReason != "" {
		r := string(v.EndReason)
		out.EndReason = &r
	}
	if v.Err != nil {
		var be *Error
		// The controller logged the failure once; a view is rendered at every share.state, so no line here.
		if !errors.As(mapError(v.Err, quietLog), &be) {
			be = newError(codes.CodeInternal, nil)
		}
		out.Error = &ShareViewError{Code: string(be.Code), Params: be.Params}
	}
	return out
}

// quietLog drops mapError's line for an unknown error.
var quietLog = slog.New(slog.DiscardHandler)

// statsSample is the controller's Stats as the ShareStatsSample JSON.
func statsSample(st *share.Stats) ShareStatsSample {
	out := ShareStatsSample{Layers: make([]ShareStatsLayer, 0, len(st.Layers)), AudioKbps: st.AudioSentBps / 1000}
	for _, l := range st.Layers {
		out.Layers = append(out.Layers, ShareStatsLayer{RID: l.RID, Width: l.Width, Height: l.Height, FPS: fps(l.FPS),
			Kbps: l.SentBps / 1000, Encoder: l.Encoder, HW: l.HW, Limit: string(l.Limit)})
	}
	return out
}

// fps rounds a frame rate.
func fps(f float64) int {
	if f <= 0 || math.IsNaN(f) || f > math.MaxInt32 {
		return 0
	}
	return int(math.Round(f))
}

// millis is a duration in whole milliseconds.
func millis(d time.Duration) int { return int(d.Round(time.Millisecond) / time.Millisecond) }
