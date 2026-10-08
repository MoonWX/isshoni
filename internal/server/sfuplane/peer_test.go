package sfuplane

import (
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"testing"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/sfu"
	"github.com/MoonWX/isshoni/internal/server/signal"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

// The "Calls" table of 01 §15.4: each MediaPeer method is one call of the connection's *sfu.Conn, recorded here by
// a fake Conn that also checks the 5 s context of each call.

func ptr[T any](v T) *T { return &v }

// TestNewPeer: NewPeer is SFU.Join with the connection's ids, role, client kind and decode caps; the user is the
// room's participant, and the peer is the Conn's Signaler.
func TestNewPeer(t *testing.T) {
	tp := newTestPlane(t)
	for _, tc := range []struct {
		name string
		pp   signal.PeerParams
		want sfu.JoinParams
	}{
		{
			"a web sharer",
			signal.PeerParams{
				ConnectionID: "c_k3v9q2m7xw4pa8d1", UserID: "u_alice", RoomID: "lounge", Role: protocol.RoleFull,
				Caps: protocol.Caps{
					Decode: []protocol.CodecKey{"opus", "h264/42e0", "h264/6400"}, Encode: []protocol.CodecKey{"h264/4200"},
					Simulcast: true, DisplayCapture: true,
				},
				Client: protocol.ClientInfo{Kind: protocol.ClientKindWeb, Version: "1.2.3", OS: protocol.ClientOSMacOS, Browser: "chrome"},
			},
			sfu.JoinParams{
				Room: "lounge", Participant: "u_alice", User: "u_alice", Conn: "c_k3v9q2m7xw4pa8d1",
				Role: sfu.RoleFull, Client: sfu.ClientWeb, Decode: sfu.DecodeCaps{H264: []sfu.ProfileKey{"42e0", "6400"}},
			},
		},
		{
			"a phone that only watches, without H.264 yet",
			signal.PeerParams{
				ConnectionID: "c_2", UserID: "u_bob", RoomID: "movies", Role: protocol.RoleViewer,
				Caps:   protocol.Caps{Decode: []protocol.CodecKey{"opus"}},
				Client: protocol.ClientInfo{Kind: protocol.ClientKindMobile},
			},
			sfu.JoinParams{Room: "movies", Participant: "u_bob", User: "u_bob", Conn: "c_2", Role: sfu.RoleViewer, Client: sfu.ClientMobile},
		},
		{
			"the desktop core",
			signal.PeerParams{ConnectionID: "c_3", UserID: "u_bob", RoomID: "movies", Role: protocol.RolePublisher, Client: protocol.ClientInfo{Kind: protocol.ClientKindDesktop}},
			sfu.JoinParams{Room: "movies", Participant: "u_bob", User: "u_bob", Conn: "c_3", Role: sfu.RolePublisher, Client: sfu.ClientDesktop},
		},
		{
			"an agent",
			signal.PeerParams{ConnectionID: "c_4", UserID: "u_bob", RoomID: "movies", Role: protocol.RoleAgent, Client: protocol.ClientInfo{Kind: protocol.ClientKindDesktop}},
			sfu.JoinParams{Room: "movies", Participant: "u_bob", User: "u_bob", Conn: "c_4", Role: sfu.RoleAgent, Client: sfu.ClientDesktop},
		},
		{
			"the load test",
			signal.PeerParams{ConnectionID: "c_5", UserID: "u_load", RoomID: "lounge", Role: protocol.RoleFull, Client: protocol.ClientInfo{Kind: protocol.ClientKindTool}},
			sfu.JoinParams{Room: "lounge", Participant: "u_load", User: "u_load", Conn: "c_5", Role: sfu.RoleFull, Client: sfu.ClientWeb},
		},
	} {
		pr := tp.newPeerWith(tc.pp)
		got := pr.join
		if got.Signaler != sfu.Signaler(pr.peer) {
			t.Errorf("%s: the Conn's Signaler is %v, want the peer", tc.name, got.Signaler)
		}
		got.Signaler = nil
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: Join(%+v)\nwant %+v", tc.name, got, tc.want)
		}
	}
}

// TestNewPeerErrors: NewPeer fails with a plain error, which the hub answers with error{internal}: before Bind,
// without a sink, for a role the SFU doesn't have, and when the SFU refuses the Join.
func TestNewPeerErrors(t *testing.T) {
	pp := signal.PeerParams{ConnectionID: "c_1", UserID: "u_1", RoomID: "lounge", Role: protocol.RoleFull}

	unbound, _ := New(nil)
	if peer, err := unbound.NewPeer(pp, &signaltest.Sink{}); peer != nil || !errors.Is(err, errNotBound) {
		t.Errorf("NewPeer before Bind = %v, %v", peer, err)
	}
	unbound.Bind(nil) // no SFU: still unbound
	if peer, err := unbound.NewPeer(pp, &signaltest.Sink{}); peer != nil || !errors.Is(err, errNotBound) {
		t.Errorf("NewPeer after Bind(nil) = %v, %v", peer, err)
	}

	tp := newTestPlane(t)
	if peer, err := tp.plane.NewPeer(pp, nil); peer != nil || err == nil {
		t.Errorf("NewPeer without a sink = %v, %v", peer, err)
	}
	bad := pp
	bad.Role = "director"
	if peer, err := tp.plane.NewPeer(bad, &signaltest.Sink{}); peer != nil || err == nil {
		t.Errorf("NewPeer with an unknown role = %v, %v", peer, err)
	}
	if len(tp.joins) != 0 {
		t.Errorf("the SFU was asked to join %d times", len(tp.joins))
	}

	tp.joinErr = &sfu.Error{Code: sfu.CodeClosed}
	peer, err := tp.plane.NewPeer(pp, &signaltest.Sink{})
	var se *sfu.Error
	if peer != nil || !errors.As(err, &se) || se.Code != sfu.CodeClosed {
		t.Errorf("NewPeer on a closed SFU = %v, %v", peer, err)
	}
	var pe *protocol.Error
	if errors.As(err, &pe) {
		t.Errorf("NewPeer's error is a wire error (%v): the hub makes it internal itself", pe)
	}
	if n := len(tp.plane.peers); n != 0 {
		t.Errorf("%d peers registered after failed joins", n)
	}
}

func TestCalls(t *testing.T) {
	video, audio := webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio
	const cand = "candidate:842163049 1 udp 1677729535 203.0.113.7 46154 typ srflx raddr 0.0.0.0 rport 0"
	for _, tc := range []struct {
		name string
		do   func(p signal.MediaPeer)
		want []call
	}{
		{
			"CreateShare: the hub's id, the preset, audio and the kind as the source; never the label, ref or replaces",
			func(p signal.MediaPeer) {
				_, _ = p.CreateShare("s_q7m2x9c4v8b1n5k3", protocol.ShareStart{
					Kind: protocol.ShareKindWindow, Label: "my secret label", Preset: protocol.PresetGame, Audio: true,
					Ref: "r1", Replaces: "s_old",
				})
				_, _ = p.CreateShare("s_2", protocol.ShareStart{Kind: protocol.ShareKindScreen, Preset: protocol.PresetAuto})
				_, _ = p.CreateShare("s_3", protocol.ShareStart{Kind: protocol.ShareKindTab, Preset: protocol.PresetMovie, Audio: true})
				_, _ = p.CreateShare("s_4", protocol.ShareStart{Kind: protocol.ShareKindScreen, Preset: protocol.PresetText})
			},
			[]call{
				{"StartShare", []any{sfu.StartShareParams{ID: "s_q7m2x9c4v8b1n5k3", Preset: sfu.PresetGame, Audio: true, Source: sfu.SourceWindow}}},
				{"StartShare", []any{sfu.StartShareParams{ID: "s_2", Preset: sfu.PresetAuto, Source: sfu.SourceScreen}}},
				{"StartShare", []any{sfu.StartShareParams{ID: "s_3", Preset: sfu.PresetMovie, Audio: true, Source: sfu.SourceTab}}},
				{"StartShare", []any{sfu.StartShareParams{ID: "s_4", Preset: sfu.PresetText, Source: sfu.SourceScreen}}},
			},
		},
		{
			"UpdateShare: only a preset reaches the SFU; a label change asks for the current params",
			func(p signal.MediaPeer) {
				_, _ = p.UpdateShare("s_1", protocol.ShareUpdate{ShareID: "s_1", Preset: protocol.PresetText})
				_, _ = p.UpdateShare("s_1", protocol.ShareUpdate{ShareID: "s_1", Label: ptr("new label")})
				_, _ = p.UpdateShare("s_1", protocol.ShareUpdate{ShareID: "s_1", Label: ptr(""), Preset: protocol.PresetMovie})
			},
			[]call{
				{"UpdateShare", []any{sfu.ShareID("s_1"), sfu.ShareUpdate{Preset: ptr(sfu.PresetText)}}},
				{"UpdateShare", []any{sfu.ShareID("s_1"), sfu.ShareUpdate{}}},
				{"UpdateShare", []any{sfu.ShareID("s_1"), sfu.ShareUpdate{Preset: ptr(sfu.PresetMovie)}}},
			},
		},
		{
			"EndShare: StopShare with the hub's reason, the same string",
			func(p signal.MediaPeer) {
				for _, r := range []protocol.EndReason{
					protocol.EndReasonStopped, protocol.EndReasonLeft, protocol.EndReasonDisconnected,
					protocol.EndReasonMediaTimeout, protocol.EndReasonRoomClosed, protocol.EndReasonServerShutdown,
				} {
					p.EndShare("s_1", r)
				}
			},
			[]call{
				{"StopShare", []any{sfu.ShareID("s_1"), sfu.EndReasonStopped}},
				{"StopShare", []any{sfu.ShareID("s_1"), sfu.EndReasonLeft}},
				{"StopShare", []any{sfu.ShareID("s_1"), sfu.EndReasonDisconnected}},
				{"StopShare", []any{sfu.ShareID("s_1"), sfu.EndReasonMediaTimeout}},
				{"StopShare", []any{sfu.ShareID("s_1"), sfu.EndReasonRoomClosed}},
				{"StopShare", []any{sfu.ShareID("s_1"), sfu.EndReasonServerShutdown}},
			},
		},
		{
			"HandleOffer: the pub PC, gen, neg, the SDP as it is and the tracks as bindings",
			func(p signal.MediaPeer) {
				_, _ = p.HandleOffer(protocol.PCOffer{
					PC: protocol.PCKindPub, Gen: 3, Neg: 2, SDP: "v=0\r\nthe offer\r\n",
					Tracks: []protocol.TrackRef{
						{MID: "0", ShareID: "s_1", Kind: protocol.TrackKindVideo},
						{MID: "1", ShareID: "s_1", Kind: protocol.TrackKindAudio},
						{MID: "2", ShareID: "s_2", Kind: protocol.TrackKindVideo},
					},
				})
				_, _ = p.HandleOffer(protocol.PCOffer{PC: protocol.PCKindPub, Gen: 1, Neg: 1, SDP: "v=0\r\n"})
			},
			[]call{
				{"HandleOffer", []any{sfu.PCPub, uint32(3), uint32(2), "v=0\r\nthe offer\r\n", []sfu.TrackBinding{
					{MID: "0", Share: "s_1", Kind: video}, {MID: "1", Share: "s_1", Kind: audio}, {MID: "2", Share: "s_2", Kind: video},
				}}},
				{"HandleOffer", []any{sfu.PCPub, uint32(1), uint32(1), "v=0\r\n", []sfu.TrackBinding{}}},
			},
		},
		{
			"HandleAnswer: the sub PC, gen, neg and the SDP",
			func(p signal.MediaPeer) {
				_ = p.HandleAnswer(protocol.PCAnswer{PC: protocol.PCKindSub, Gen: 2, Neg: 5, SDP: "v=0\r\nthe answer\r\n"})
			},
			[]call{{"HandleAnswer", []any{sfu.PCSub, uint32(2), uint32(5), "v=0\r\nthe answer\r\n"}}},
		},
		{
			"AddICE: the candidate as Pion's init, for either PC; the end-of-candidates marker is ignored",
			func(p signal.MediaPeer) {
				_ = p.AddICE(protocol.PCICE{PC: protocol.PCKindPub, Gen: 1, Candidate: &protocol.ICECandidate{
					Candidate: cand, SDPMid: ptr("0"), SDPMLineIndex: ptr(uint16(0)), UsernameFragment: ptr("EsAw"),
				}})
				_ = p.AddICE(protocol.PCICE{PC: protocol.PCKindSub, Gen: 4, Candidate: &protocol.ICECandidate{Candidate: cand}})
				_ = p.AddICE(protocol.PCICE{PC: protocol.PCKindSub, Gen: 4})                                                      // absent
				_ = p.AddICE(protocol.PCICE{PC: protocol.PCKindSub, Gen: 4, Candidate: &protocol.ICECandidate{}})                 // the browsers' form
				_ = p.AddICE(protocol.PCICE{PC: "data", Gen: 4})                                                                  // nothing to refuse
				_ = p.AddICE(protocol.PCICE{PC: protocol.PCKindPub, Gen: 1, Candidate: &protocol.ICECandidate{SDPMid: ptr("0")}}) // empty
			},
			[]call{
				{"AddICECandidate", []any{sfu.PCPub, uint32(1), webrtc.ICECandidateInit{
					Candidate: cand, SDPMid: ptr("0"), SDPMLineIndex: ptr(uint16(0)), UsernameFragment: ptr("EsAw"),
				}}},
				{"AddICECandidate", []any{sfu.PCSub, uint32(4), webrtc.ICECandidateInit{Candidate: cand}}},
			},
		},
		{
			"Restart: mode ice is RestartICE, mode rebuild is ResetPC, both for the sub PC",
			func(p signal.MediaPeer) {
				_ = p.Restart(protocol.PCRestart{PC: protocol.PCKindSub, Gen: 4, Mode: protocol.RestartModeICE, Reason: protocol.RestartReasonDisconnected})
				_ = p.Restart(protocol.PCRestart{PC: protocol.PCKindSub, Gen: 4, Mode: protocol.RestartModeRebuild, Reason: protocol.RestartReasonFailed})
			},
			[]call{
				{"RestartICE", []any{sfu.PCSub, uint32(4)}},
				{"ResetPC", []any{sfu.PCSub, uint32(4)}},
			},
		},
		{
			"ClosePC",
			func(p signal.MediaPeer) { _ = p.ClosePC(protocol.PCClose{PC: protocol.PCKindPub, Gen: 2}) },
			[]call{{"ClosePC", []any{sfu.PCPub, uint32(2)}}},
		},
		{
			"Subscribe: every want in one call, in order",
			func(p signal.MediaPeer) {
				_, _ = p.Subscribe([]protocol.SubscriptionWant{
					{ShareID: "s_a", Video: protocol.VideoLayerHigh, Audio: protocol.AudioStateOn},
					{ShareID: "s_b", Video: protocol.VideoLayerLow, Audio: protocol.AudioStateOff},
					{ShareID: "s_c", Video: protocol.VideoLayerOff, Audio: protocol.AudioStateOff},
					{ShareID: "s_d", Video: protocol.VideoLayerOff, Audio: protocol.AudioStateOn},
				})
			},
			[]call{{"UpdateSubscriptions", []any{[]sfu.SubscriptionUpdate{
				{Share: "s_a", Video: sfu.QualityHigh, Audio: true},
				{Share: "s_b", Video: sfu.QualityLow},
				{Share: "s_c", Video: sfu.QualityOff},
				{Share: "s_d", Video: sfu.QualityOff, Audio: true},
			}}}},
		},
		{
			"SetCaps: the H.264 profiles without their prefix, nothing else",
			func(p signal.MediaPeer) {
				p.SetCaps(protocol.Caps{Decode: []protocol.CodecKey{"opus", "h264/42e0", "vp9", "h264/6400"}, Encode: []protocol.CodecKey{"h264/4d00"}})
				p.SetCaps(protocol.Caps{Decode: []protocol.CodecKey{"opus"}})
			},
			[]call{
				{"SetDecodeCaps", []any{sfu.DecodeCaps{H264: []sfu.ProfileKey{"42e0", "6400"}}}},
				{"SetDecodeCaps", []any{sfu.DecodeCaps{}}},
			},
		},
		{"Resync", func(p signal.MediaPeer) { p.Resync() }, []call{{"Resync", nil}}},
		{"Stats", func(p signal.MediaPeer) { p.Stats() }, []call{{"Stats", nil}}},
		{"Close: with the reason left", func(p signal.MediaPeer) { p.Close() }, []call{{"Close", []any{sfu.EndReasonLeft}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tp := newTestPlane(t)
			pr := tp.newPeer("c_1", "u_1")
			tc.do(pr)
			if got := pr.conn.take(); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("the Conn was called with\n %+v\nwant\n %+v", got, tc.want)
			}
			if lines := atLeast(tp.logs.take(), slog.LevelInfo); len(lines) != 0 {
				t.Errorf("logged %+v", lines)
			}
		})
	}
}

// TestCallResults: what the SFU returns goes back converted: ShareParams with the "h264/" codec key and the layer
// of each rid, the answer SDP with the offer's pc, gen and neg, and the stats.
func TestCallResults(t *testing.T) {
	tp := newTestPlane(t)
	pr := tp.newPeer("c_1", "u_1")
	pr.conn.shareParams = sfu.ShareParams{
		Profile: sfu.ProfileHigh,
		Encodings: []sfu.EncodingParams{
			{RID: "f", Active: true, MaxBitrate: 6_000_000, MaxFramerate: 60, MaxPixels: 1920 * 1080},
			{RID: "q", Active: true, MaxBitrate: 300_000, MaxFramerate: 15, MaxPixels: 640 * 360},
		},
		AudioBitrate: 256_000,
	}
	want := protocol.ShareParams{
		ShareID: "s_1", Codec: protocol.CodecH264High,
		Encodings: []protocol.Encoding{
			{RID: protocol.RIDHigh, Layer: protocol.VideoLayerHigh, Active: true, MaxBitrate: 6_000_000, MaxFramerate: 60, MaxPixels: 1920 * 1080},
			{RID: protocol.RIDLow, Layer: protocol.VideoLayerLow, Active: true, MaxBitrate: 300_000, MaxFramerate: 15, MaxPixels: 640 * 360},
		},
		AudioBitrate: 256_000,
	}
	got, err := pr.CreateShare("s_1", protocol.ShareStart{Kind: protocol.ShareKindScreen, Preset: protocol.PresetMovie, Audio: true, Ref: "r"})
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("CreateShare = %+v, %v\nwant %+v", got, err, want)
	}
	pr.conn.shareParams.Profile = sfu.ProfileConstrainedBaseline // the room's policy changed meanwhile
	want.Codec = protocol.CodecH264ConstrainedBaseline
	got, err = pr.UpdateShare("s_1", protocol.ShareUpdate{ShareID: "s_1", Preset: protocol.PresetMovie})
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("UpdateShare = %+v, %v\nwant %+v", got, err, want)
	}

	pr.conn.answer = "v=0\r\nthe answer\r\n"
	answer, err := pr.HandleOffer(protocol.PCOffer{PC: protocol.PCKindPub, Gen: 3, Neg: 2, SDP: "v=0\r\nthe offer\r\n"})
	wantAnswer := protocol.PCAnswer{PC: protocol.PCKindPub, Gen: 3, Neg: 2, SDP: "v=0\r\nthe answer\r\n"}
	if err != nil || answer != wantAnswer {
		t.Errorf("HandleOffer = %+v, %v; want %+v", answer, err, wantAnswer)
	}

	pr.conn.stats = sfu.ConnStats{
		DownlinkEstimate: 1_000_000,
		Shares:           []sfu.OwnShareStats{{Share: "s_1", Layers: []sfu.LayerInfo{{RID: "f", Bitrate: 5}}}},
	}
	st := pr.Stats()
	wantStats := protocol.ServerStats{
		DownlinkEstimate: 1_000_000,
		Subs:             []protocol.ServerSubStats{},
		Layers:           []protocol.ServerLayerStats{{ShareID: "s_1", Kind: protocol.TrackKindVideo, RID: "f", Bitrate: 5}},
	}
	if !reflect.DeepEqual(st, wantStats) {
		t.Errorf("Stats = %+v, want %+v", st, wantStats)
	}

	// Every other method succeeds with nil, not with a nil *protocol.Error in an error.
	for name, err := range map[string]error{
		"HandleAnswer": pr.HandleAnswer(protocol.PCAnswer{PC: protocol.PCKindSub, Gen: 1, Neg: 1, SDP: "v=0\r\n"}),
		"AddICE":       pr.AddICE(protocol.PCICE{PC: protocol.PCKindSub, Gen: 1, Candidate: &protocol.ICECandidate{Candidate: "candidate:1"}}),
		"Restart":      pr.Restart(protocol.PCRestart{PC: protocol.PCKindSub, Gen: 1, Mode: protocol.RestartModeICE}),
		"ClosePC":      pr.ClosePC(protocol.PCClose{PC: protocol.PCKindPub, Gen: 1}),
	} {
		if err != nil {
			t.Errorf("%s = %v (%T), want nil", name, err, err)
		}
	}
	if lines := atLeast(tp.logs.take(), slog.LevelInfo); len(lines) != 0 {
		t.Errorf("logged %+v", lines)
	}
}

// TestShareParamsOddities: a profile or a rid that has no wire form is a bug in the SFU; the params still go out,
// without it, and the log says so.
func TestShareParamsOddities(t *testing.T) {
	tp := newTestPlane(t)
	pr := tp.newPeer("c_1", "u_1")
	pr.conn.shareParams = sfu.ShareParams{
		Profile:   "not-a-profile",
		Encodings: []sfu.EncodingParams{{RID: "f", Active: true}, {RID: "x", Active: true}},
	}
	got, err := pr.CreateShare("s_1", protocol.ShareStart{Kind: protocol.ShareKindScreen, Preset: protocol.PresetAuto})
	want := protocol.ShareParams{ShareID: "s_1", Encodings: []protocol.Encoding{{RID: "f", Layer: protocol.VideoLayerHigh, Active: true}}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("CreateShare = %+v, %v; want %+v", got, err, want)
	}
	if lines := atLeast(tp.logs.take(), slog.LevelError); len(lines) != 2 {
		t.Errorf("logged %+v, want one error for the profile and one for the rid", lines)
	}
}

// TestSubscribe: an item whose share is gone is returned as ignored; any other item error fails the whole request
// with its mapped code; a Conn-level error does too.
func TestSubscribe(t *testing.T) {
	wants := []protocol.SubscriptionWant{
		{ShareID: "s_a", Video: protocol.VideoLayerHigh, Audio: protocol.AudioStateOn},
		{ShareID: "s_b", Video: protocol.VideoLayerLow, Audio: protocol.AudioStateOff},
		{ShareID: "s_c", Video: protocol.VideoLayerLow, Audio: protocol.AudioStateOff},
		{ShareID: "s_d", Video: protocol.VideoLayerLow, Audio: protocol.AudioStateOff},
	}
	gone := &sfu.Error{Code: sfu.CodeShareNotFound}
	for _, tc := range []struct {
		name     string
		itemErrs []error
		connErr  error
		ignored  []string
		code     protocol.ErrorCode // "" = success
		params   map[string]any
	}{
		{name: "all applied"},
		{name: "two shares just ended", itemErrs: []error{nil, gone, nil, gone}, ignored: []string{"s_b", "s_d"}},
		{name: "every share ended", itemErrs: []error{gone, gone, gone, gone}, ignored: []string{"s_a", "s_b", "s_c", "s_d"}},
		{
			name:     "the guard on subscriptions per connection",
			itemErrs: []error{nil, gone, &sfu.Error{Code: sfu.CodeTooManySubscriptions}},
			code:     protocol.ErrorCodeBadRequest, params: map[string]any{"field": "subs", "reason": "too_many"},
		},
		{
			name:     "the first failing item decides",
			itemErrs: []error{&sfu.Error{Code: sfu.CodeRoleForbidden}, &sfu.Error{Code: sfu.CodeTooManySubscriptions}},
			code:     protocol.ErrorCodeForbidden,
		},
		{name: "a viewer-only call from a publisher", connErr: &sfu.Error{Code: sfu.CodeRoleForbidden}, code: protocol.ErrorCodeForbidden},
		{
			name:    "more than 64 items",
			connErr: &sfu.Error{Code: sfu.CodeTooManySubscriptions},
			code:    protocol.ErrorCodeBadRequest, params: map[string]any{"field": "subs", "reason": "too_many"},
		},
		{name: "the Conn is closed", connErr: &sfu.Error{Code: sfu.CodeClosed}, code: protocol.ErrorCodeNotInRoom},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tp := newTestPlane(t)
			pr := tp.newPeer("c_1", "u_1")
			pr.conn.itemErrs = tc.itemErrs
			if tc.connErr != nil {
				pr.conn.fail("UpdateSubscriptions", tc.connErr)
			}
			ignored, err := pr.Subscribe(wants)
			if tc.code == "" {
				if err != nil || !slices.Equal(ignored, tc.ignored) {
					t.Errorf("Subscribe = %v, %v; want %v ignored", ignored, err, tc.ignored)
				}
				return
			}
			var pe *protocol.Error
			if !errors.As(err, &pe) || ignored != nil {
				t.Fatalf("Subscribe = %v, %v; want a wire error", ignored, err)
			}
			if pe.Code != tc.code || pe.Scope != protocol.ErrorScopeRequest || !reflect.DeepEqual(pe.Params, tc.params) {
				t.Errorf("Subscribe failed with %+v, want %s with params %v and scope request", *pe, tc.code, tc.params)
			}
		})
	}
}

// TestValuesWithoutACounterpart: the hub validates every payload first, so a value that the SFU has no name for
// can't arrive; if one does, the call is refused as bad_request with the field and the SFU is not called.
func TestValuesWithoutACounterpart(t *testing.T) {
	request, pc := protocol.ErrorScopeRequest, protocol.ErrorScopePC
	for _, tc := range []struct {
		name  string
		do    func(p signal.MediaPeer) error
		field string
		want  protocol.Error // scope and subject
	}{
		{"CreateShare preset", func(p signal.MediaPeer) error {
			_, err := p.CreateShare("s_1", protocol.ShareStart{Kind: protocol.ShareKindScreen, Preset: "cinema"})
			return err
		}, "preset", protocol.Error{Scope: request}},
		{"CreateShare without a preset", func(p signal.MediaPeer) error {
			_, err := p.CreateShare("s_1", protocol.ShareStart{Kind: protocol.ShareKindScreen})
			return err
		}, "preset", protocol.Error{Scope: request}},
		{"CreateShare kind", func(p signal.MediaPeer) error {
			_, err := p.CreateShare("s_1", protocol.ShareStart{Kind: "camera", Preset: protocol.PresetAuto})
			return err
		}, "kind", protocol.Error{Scope: request}},
		{"UpdateShare preset", func(p signal.MediaPeer) error {
			_, err := p.UpdateShare("s_1", protocol.ShareUpdate{ShareID: "s_1", Preset: "cinema"})
			return err
		}, "preset", protocol.Error{Scope: request}},
		{"Subscribe layer", func(p signal.MediaPeer) error {
			_, err := p.Subscribe([]protocol.SubscriptionWant{{ShareID: "s_1", Video: "mid", Audio: protocol.AudioStateOn}})
			return err
		}, "subs", protocol.Error{Scope: request}},
		{"HandleOffer pc", func(p signal.MediaPeer) error {
			_, err := p.HandleOffer(protocol.PCOffer{PC: "data", Gen: 2, Neg: 3, SDP: "v=0"})
			return err
		}, "pc", protocol.Error{Scope: pc, PC: "data", Gen: 2, Neg: 3}},
		{"HandleOffer track kind", func(p signal.MediaPeer) error {
			_, err := p.HandleOffer(protocol.PCOffer{PC: protocol.PCKindPub, Gen: 2, Neg: 3, SDP: "v=0",
				Tracks: []protocol.TrackRef{{MID: "0", ShareID: "s_1", Kind: "data"}}})
			return err
		}, "tracks", protocol.Error{Scope: pc, PC: protocol.PCKindPub, Gen: 2, Neg: 3}},
		{"HandleAnswer pc", func(p signal.MediaPeer) error {
			return p.HandleAnswer(protocol.PCAnswer{PC: "", Gen: 2, Neg: 3, SDP: "v=0"})
		}, "pc", protocol.Error{Scope: pc, Gen: 2, Neg: 3}},
		{"AddICE pc", func(p signal.MediaPeer) error {
			return p.AddICE(protocol.PCICE{PC: "data", Gen: 2, Candidate: &protocol.ICECandidate{Candidate: "candidate:1"}})
		}, "pc", protocol.Error{Scope: pc, PC: "data", Gen: 2}},
		{"Restart pc", func(p signal.MediaPeer) error {
			return p.Restart(protocol.PCRestart{PC: "data", Gen: 2, Mode: protocol.RestartModeICE})
		}, "pc", protocol.Error{Scope: pc, PC: "data", Gen: 2}},
		{"Restart mode", func(p signal.MediaPeer) error {
			return p.Restart(protocol.PCRestart{PC: protocol.PCKindSub, Gen: 2, Mode: "warp"})
		}, "mode", protocol.Error{Scope: pc, PC: protocol.PCKindSub, Gen: 2}},
		{"ClosePC pc", func(p signal.MediaPeer) error {
			return p.ClosePC(protocol.PCClose{PC: "data", Gen: 2})
		}, "pc", protocol.Error{Scope: pc, PC: "data", Gen: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tp := newTestPlane(t)
			pr := tp.newPeer("c_1", "u_1")
			err := tc.do(pr)
			var pe *protocol.Error
			if !errors.As(err, &pe) {
				t.Fatalf("error = %v, want a wire error", err)
			}
			want := tc.want
			want.Code = protocol.ErrorCodeBadRequest
			want.Params = map[string]any{"field": tc.field, "reason": protocol.FieldInvalid}
			if !reflect.DeepEqual(*pe, want) {
				t.Errorf("error = %+v\nwant    %+v", *pe, want)
			}
			if calls := pr.conn.take(); len(calls) != 0 {
				t.Errorf("the SFU was called: %+v", calls)
			}
		})
	}
}

// TestClose: Close closes the Conn and takes the peer out of the Plane's table, once; a peer that the connection's
// next room has already replaced stays.
func TestClose(t *testing.T) {
	tp := newTestPlane(t)
	first := tp.newPeer("c_1", "u_1")
	if tp.plane.peers["c_1"] != first.peer {
		t.Fatal("the peer is not registered")
	}
	first.Close()
	first.Close() // the SFU's Close is idempotent, and so is the peer's
	if n := len(tp.plane.peers); n != 0 {
		t.Errorf("%d peers after Close", n)
	}
	if calls := first.conn.take(); len(calls) != 2 || calls[0].Method != "Close" || calls[1].Method != "Close" {
		t.Errorf("calls = %+v", calls)
	}

	// The connection joins another room: the hub closes the old peer, then makes a new one. Whatever the order in
	// which the old one finishes, the new one keeps its place.
	old := tp.newPeer("c_1", "u_1")
	next := tp.newPeer("c_1", "u_1")
	old.Close()
	if tp.plane.peers["c_1"] != next.peer {
		t.Error("closing the old peer removed the connection's new one")
	}
	next.Close()
	if n := len(tp.plane.peers); n != 0 {
		t.Errorf("%d peers after both closed", n)
	}
}
