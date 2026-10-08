package sfu_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/server/netx"
	"github.com/MoonWX/isshoni/internal/server/sfu"
)

// The whole signal ↔ sfu interface of 02 §6 (the list in 02 §18), pinned at compile time: every function with the
// signature the spec gives, every type with its fields. 01's sfuplane and 04's wiring are written against exactly
// this, so a change here is a change to the spec first. The methods that later slices fill in are part of it; what
// they return until then is TestNotImplemented's.
var (
	// Construction (04's wiring).
	_ func(sfu.Config, sfu.Deps) (*sfu.SFU, error) = sfu.New
	_ func(*sfu.SFU) error                         = (*sfu.SFU).Close
	_ func(*sfu.SFU) error                         = (*sfu.SFU).Ready

	// Rooms and connections (signal).
	_ func(*sfu.SFU, sfu.JoinParams) (*sfu.Conn, error) = (*sfu.SFU).Join
	_ func(*sfu.SFU, sfu.RoomID) []sfu.ShareInfo        = (*sfu.SFU).Shares
	_ func(*sfu.SFU, sfu.ShareID) (sfu.ShareInfo, bool) = (*sfu.SFU).Share
	_ func(*sfu.SFU, sfu.RoomID) sfu.ProfileKey         = (*sfu.SFU).CodecPolicy
	_ func(*sfu.SFU, sfu.ShareID, sfu.EndReason) error  = (*sfu.SFU).StopShare
	_ func(*sfu.SFU, sfu.RoomID, sfu.EndReason)         = (*sfu.SFU).CloseRoom
	_ func(*sfu.SFU, sfu.Limits)                        = (*sfu.SFU).SetLimits
	_ func(*sfu.SFU) sfu.Snapshot                       = (*sfu.SFU).Snapshot
	_ func(*sfu.SFU) sfu.Metrics                        = (*sfu.SFU).Metrics
	_ func(*sfu.SFU, context.Context, sfu.UserID, sfu.ProbeTransport, string,
	) (string, <-chan sfu.ProbeResult, error) = (*sfu.SFU).Probe

	// Per connection (01's sfuplane).
	_ func(*sfu.Conn) sfu.ConnID = (*sfu.Conn).ID
	_ func(*sfu.Conn, context.Context, sfu.PCKind, uint32, uint32, string, []sfu.TrackBinding,
	) (string, error) = (*sfu.Conn).HandleOffer
	_ func(*sfu.Conn, context.Context, sfu.PCKind, uint32, uint32, string) error              = (*sfu.Conn).HandleAnswer
	_ func(*sfu.Conn, context.Context, sfu.PCKind, uint32, webrtc.ICECandidateInit) error     = (*sfu.Conn).AddICECandidate
	_ func(*sfu.Conn, context.Context, sfu.PCKind, uint32) error                              = (*sfu.Conn).RestartICE
	_ func(*sfu.Conn, context.Context, sfu.PCKind, uint32) error                              = (*sfu.Conn).ResetPC
	_ func(*sfu.Conn, context.Context, sfu.PCKind, uint32) error                              = (*sfu.Conn).ClosePC
	_ func(*sfu.Conn, context.Context, sfu.StartShareParams) (sfu.ShareParams, error)         = (*sfu.Conn).StartShare
	_ func(*sfu.Conn, context.Context, sfu.ShareID, sfu.ShareUpdate) (sfu.ShareParams, error) = (*sfu.Conn).UpdateShare
	_ func(*sfu.Conn, context.Context, sfu.ShareID, sfu.EndReason) error                      = (*sfu.Conn).StopShare
	_ func(*sfu.Conn, context.Context, []sfu.SubscriptionUpdate) ([]error, error)             = (*sfu.Conn).UpdateSubscriptions
	_ func(*sfu.Conn, context.Context, sfu.DecodeCaps) error                                  = (*sfu.Conn).SetDecodeCaps
	_ func(*sfu.Conn)                                                                         = (*sfu.Conn).Resync
	_ func(*sfu.Conn) sfu.ConnStats                                                           = (*sfu.Conn).Stats
	_ func(*sfu.Conn, sfu.EndReason)                                                          = (*sfu.Conn).Close
	_ func(*sfu.Conn) <-chan struct{}                                                         = (*sfu.Conn).Done

	_ error = (*sfu.Error)(nil)
)

// surfaceSignaler and surfaceEvents implement the two interfaces the SFU calls, with the spec's method sets.
type (
	surfaceSignaler struct{}
	surfaceEvents   struct{}
)

func (surfaceSignaler) SendOffer(sfu.PCKind, uint32, uint32, string, []sfu.TrackBinding) {}
func (surfaceSignaler) SendEvent(sfu.Event)                                              {}
func (surfaceEvents) ShareUpdated(sfu.RoomID, sfu.ShareInfo)                             {}
func (surfaceEvents) ShareEnded(sfu.RoomID, sfu.ShareInfo, sfu.EndReason)                {}
func (surfaceEvents) CodecPolicyChanged(sfu.RoomID, sfu.ProfileKey)                      {}

var (
	_ sfu.Signaler   = surfaceSignaler{}
	_ sfu.RoomEvents = surfaceEvents{}
)

// TestAPISurface writes every exported type of 02 §6 with all its fields and every constant, so a renamed or
// retyped field no longer compiles, and checks the few values that are fixed strings.
func TestAPISurface(t *testing.T) {
	preset := sfu.PresetMovie
	video := webrtc.RTPCodecTypeVideo
	binding := sfu.TrackBinding{MID: "0", Share: "s_1", Kind: video}
	encoding := sfu.EncodingParams{RID: "f", Active: true, MaxBitrate: 1, MaxFramerate: 2, MaxPixels: 3}
	layer := sfu.LayerInfo{RID: "f", Width: 1, Height: 2, FPS: 3, Bitrate: 4, LossPct: 5, Active: true}
	info := sfu.ShareInfo{
		ID: "s_1", Room: "r", Participant: "p", User: "u", Conn: "c", Source: sfu.SourceTab, Preset: preset,
		State: sfu.ShareStalled, Profile: sfu.ProfileHigh, Layers: []sfu.LayerInfo{layer}, Audio: true,
		Viewers: []sfu.ParticipantID{"p"}, StartedAt: time.Now(), LiveAt: time.Now(),
	}
	sfuErr := &sfu.Error{Code: sfu.CodeBusy, Retryable: true, Share: "s_1", RetryAfter: time.Second}
	_ = []any{
		sfu.Config{Transport: (*netx.Transport)(nil), PauseUnwatchedLayers: true, Limits: sfu.Limits{MaxShareKbps: 1}},
		sfu.Deps{Events: surfaceEvents{}, Logger: (*slog.Logger)(nil)},
		sfu.JoinParams{
			Room: "r", Participant: "p", User: "u", Conn: "c", Role: sfu.RoleFull, Client: sfu.ClientWeb,
			Decode: sfu.DecodeCaps{H264: []sfu.ProfileKey{sfu.ProfileHigh}}, Signaler: surfaceSignaler{},
		},
		sfu.StartShareParams{ID: "s_1", Preset: preset, Audio: true, Source: sfu.SourceScreen},
		sfu.ShareUpdate{Preset: &preset},
		sfu.ShareParams{Profile: sfu.ProfileHigh, Encodings: []sfu.EncodingParams{encoding}, AudioBitrate: 1},
		sfu.SubscriptionUpdate{Share: "s_1", Video: sfu.QualityHigh, Audio: true},
		binding, info,
		sfu.ProbeResult{Transport: sfu.ProbeUDP, Connected: true, Selected: "udp", RTT: time.Second, Err: "timeout"},
		// Events.
		sfu.SubscriptionStateEvent{
			Share: "s_1", Requested: sfu.QualityHigh, Forwarded: sfu.QualityLow, Audio: true, Reason: sfu.SubReasonBandwidth,
		},
		sfu.CodecPolicyEvent{Share: "s_1", Profile: sfu.ProfileConstrainedBaseline},
		sfu.QualityHintEvent{Share: "s_1", Reason: "admin", MaxBitrate: 1, Encodings: []sfu.EncodingParams{encoding}},
		sfu.PCStateEvent{PC: sfu.PCPub, Gen: 1, State: "failed", Reason: "handshake_timeout"},
		sfu.ErrorEvent{Err: sfuErr, Scope: sfu.ScopePCSub},
		// Observability.
		sfu.ConnStats{
			PCs:    []sfu.PCStats{{Kind: sfu.PCSub, Gen: 1, State: "connected", Transport: "udp", RTT: time.Second}},
			Shares: []sfu.OwnShareStats{{Share: "s_1", State: sfu.ShareLive, Layers: []sfu.LayerInfo{layer}, Audio: true, AudioBitrate: 1, AudioLossPct: 2}},
			Subscriptions: []sfu.SubscriptionStats{{
				Share: "s_1", Requested: sfu.QualityHigh, Forwarded: sfu.QualityLow, Reason: sfu.SubReasonNoLayer, Layer: "q",
				Profile: sfu.ProfileHigh, AudioRequested: true, AudioForwarded: true,
				Video: sfu.DownTrackStats{Bitrate: 1, LossPct: 2, NACKsServed: 3, Drops: 4}, Audio: sfu.DownTrackStats{},
			}},
			DownlinkEstimate: 1,
		},
		sfu.Snapshot{
			At: time.Now(),
			Rooms: []sfu.RoomSnapshot{{
				Room: "r",
				Shares: []sfu.ShareSnapshot{{
					Info: info, IngressBps: 1, EgressBps: 2, Viewers: sfu.ViewerCounts{High: 1, Low: 2, Audio: 3},
				}},
				Conns: []sfu.ConnSummary{{Conn: "c", User: "u", Transport: "tcp443", RTT: time.Second}},
			}},
			Totals: sfu.SnapshotTotals{
				IngressBps: 1, EgressBps: 2, DownTracks: 3, PCsByState: map[string]int{}, PCsByTransport: map[string]int{},
			},
		},
		sfu.Metrics{
			Conns: map[string]int64{}, PeerConnections: map[string]map[string]int64{}, SelectedTransport: map[string]int64{},
			Shares: map[string]int64{}, DownTracks: map[string]map[string]int64{}, IngressBytes: map[string]uint64{},
			IngressPackets: map[string]uint64{}, IngressDuplicates: 1, EgressBytes: map[string]uint64{},
			EgressPackets: map[string]uint64{}, NACKReceived: 1, NACKServed: 1, NACKMissed: 1, RTXSkipped: 1, PLISent: 1,
			PLIThrottled: 1, KeyframesReceived: 1, LayerSwitches: 1, Downgrades: map[string]uint64{},
			QueueDrops: map[string]uint64{}, HandshakeTimeouts: map[string]uint64{}, PacketCacheBytes: 1,
		},
		// Enums.
		[]sfu.Role{sfu.RoleFull, sfu.RoleViewer, sfu.RolePublisher, sfu.RoleAgent},
		[]sfu.ClientKind{sfu.ClientWeb, sfu.ClientDesktop, sfu.ClientMobile},
		[]sfu.PCKind{sfu.PCPub, sfu.PCSub},
		[]sfu.Quality{sfu.QualityOff, sfu.QualityLow, sfu.QualityHigh},
		[]sfu.Preset{sfu.PresetAuto, sfu.PresetGame, sfu.PresetMovie, sfu.PresetText},
		[]sfu.SourceKind{sfu.SourceUnknown, sfu.SourceScreen, sfu.SourceWindow, sfu.SourceTab},
		[]sfu.ShareState{sfu.SharePending, sfu.ShareLive, sfu.ShareStalled},
		[]sfu.ProbeTransport{sfu.ProbeUDP, sfu.ProbeTCP443, sfu.ProbeTCP7882},
		[]string{sfu.ScopePCPub, sfu.ScopePCSub, sfu.ScopeShare, sfu.ScopeSubscription},
	}

	// The strings 01 relies on: the end reasons are its protocol.EndReason values, and the sub reasons map to its four
	// status reasons (01 §15.4).
	for got, want := range map[sfu.EndReason]string{
		sfu.EndReasonStopped: "stopped", sfu.EndReasonLeft: "left", sfu.EndReasonDisconnected: "disconnected",
		sfu.EndReasonMediaTimeout: "media_timeout", sfu.EndReasonKicked: "kicked", sfu.EndReasonRoomClosed: "room_closed",
		sfu.EndReasonServerShutdown: "server_shutdown",
	} {
		if string(got) != want {
			t.Errorf("EndReason %q, want %q", got, want)
		}
	}
	for got, want := range map[sfu.SubReason]string{
		sfu.SubReasonNone: "", sfu.SubReasonBandwidth: "bandwidth", sfu.SubReasonServerLimit: "server_limit",
		sfu.SubReasonCodecMismatch: "codec_mismatch", sfu.SubReasonDecoderUnavailable: "decoder_unavailable",
		sfu.SubReasonDecoderFailed: "decoder_failed", sfu.SubReasonNoPreviewLayer: "no_preview_layer",
		sfu.SubReasonNoLayer: "no_layer",
	} {
		if string(got) != want {
			t.Errorf("SubReason %q, want %q", got, want)
		}
	}
	// Quality is ordered: a smaller value is a cheaper one, so min(requested, cap) works (02 §10.1).
	if sfu.QualityOff >= sfu.QualityLow || sfu.QualityLow >= sfu.QualityHigh {
		t.Error("Quality is not ordered off < low < high")
	}
	// The zero Role, ClientKind and PCKind are not valid values: a value an adapter forgot to map is refused.
	if sfu.Role(0) == sfu.RoleFull || sfu.ClientKind(0) == sfu.ClientWeb || sfu.PCKind(0) == sfu.PCPub {
		t.Error("a zero enum value is a valid one")
	}
}
