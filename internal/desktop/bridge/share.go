package bridge

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/MoonWX/isshoni/internal/media"
)

// Share is the bound Share service (03 §6.2, §10): the native share of this app, through the share controller and
// C2. The controller is README WA58's: until it lands no share can exist, so Current and Stats are null and Stop and
// Release do nothing, while Pick, Start and the calls on a live share are not_implemented. Capabilities already
// answers from the engine's start-time probe.
type Share struct {
	core Core
	log  *slog.Logger

	mu          sync.Mutex
	serverStats int // Share.WatchServerStats' count; the controller passes count > 0 to 01's WatchServerStats
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

// Picked is a source picked in the OS picker and held by Go until Start or Release (03 §10.1): kind window or
// monitor, label (the picker's name, shown on this PC only and never sent), border (Windows draws its yellow
// capture border) and the one-time sheets to show first, in order (first-share, dnd).
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
	for _, e := range p.Encoders {
		if e.Tested && e.Status == "" {
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

// Pick opens the OS picker owned by the main window (03 §10.1); null when cancelled. already_sharing,
// engine.<code>. README WA58.
func (s *Share) Pick(ctx context.Context) (*Picked, error) { return nil, notImplemented("Share.Pick") }

// Release drops a held pick. No pick can be held before README WA58, so it does nothing.
func (s *Share) Release(pickID string) {}

// Start starts a share of a held pick in roomId (03 §10.1); it resolves at starting, and live comes as an event.
// pick_expired, already_sharing, not_linked, publisher_offline, engine.<code>, wire codes. README WA58.
func (s *Share) Start(ctx context.Context, req StartRequest) (ShareView, error) {
	return ShareView{}, notImplemented("Share.Start")
}

// Current returns the share this app publishes, null when none (always before README WA58).
func (s *Share) Current() *ShareView { return nil }

// Stop stops the share; it never fails and is idempotent.
func (s *Share) Stop(ctx context.Context) error { return nil }

// StopOther sends share.stop on C2 for another connection's share of this user (01's Client.StopOther). README WA58.
func (s *Share) StopOther(ctx context.Context, roomID, shareID string) error {
	return notImplemented("Share.StopOther")
}

// SetPaused pauses or resumes the share (03 §11.3): feature_disabled, not_sharing. README WA58.
func (s *Share) SetPaused(ctx context.Context, paused bool) error {
	return notImplemented("Share.SetPaused")
}

// SetPreset changes the share's preset (01's Share.SetPreset): wire codes, not_sharing. README WA58.
func (s *Share) SetPreset(ctx context.Context, preset string) error {
	return notImplemented("Share.SetPreset")
}

// SetAudioEnabled is the share's Share sound switch (05's SetShareSound; no wire message): not_sharing. README WA58.
func (s *Share) SetAudioEnabled(on bool) error { return notImplemented("Share.SetAudioEnabled") }

// Stats returns the share's latest stats sample, null when no share is live (always before README WA58).
func (s *Share) Stats() *ShareStatsSample { return nil }

// WatchServerStats counts the open Details disclosures (03 §10.3): while the count is above 0 the controller asks
// 01's core for the server's stats. Unbalanced offs never take it below 0.
func (s *Share) WatchServerStats(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.serverStats = count(s.serverStats, on)
}

// count is a reference count's next value: +1 for on, −1 for off, never below 0.
func count(n int, on bool) int {
	if on {
		return n + 1
	}
	return max(n-1, 0)
}
