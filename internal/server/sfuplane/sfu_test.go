package sfuplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/client/publish"
	"github.com/MoonWX/isshoni/internal/media/fake"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/netx"
	"github.com/MoonWX/isshoni/internal/server/sfu"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
	"github.com/MoonWX/isshoni/internal/server/signal"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

// The tests in this file run the adapter on a real *sfu.SFU, built and bound the way the wiring does it (04 §6.6),
// with a Pion publisher and a Pion viewer on loopback. They check what the tables with the fake Conn can't: that
// the real API takes what the adapter passes, and that the real errors and events come out as 01 §15.4 says.
//
// They assert only what holds for every slice of the SFU: a method that a later slice implements (README S57, S69)
// may return an error today and nil tomorrow, and the SFU may send more events than it does now.

// loopbackOnly is a netx.InterfaceLister with only 127.0.0.1, so the Transport doesn't depend on the host.
type loopbackOnly struct{}

func (loopbackOnly) Interfaces() ([]netx.Interface, error) {
	return []netx.Interface{{
		Name:  "lo",
		Flags: net.FlagUp | net.FlagLoopback,
		Addrs: []netx.InterfaceAddr{{Addr: netip.MustParseAddr("127.0.0.1")}},
	}}, nil
}

func (loopbackOnly) RouteSource(context.Context, netip.Addr) (netip.Addr, error) {
	return netip.Addr{}, errors.New("no route on the loopback-only host")
}

// realPlane is a Plane bound to a real SFU on a loopback Transport.
type realPlane struct {
	plane *Plane
	sfu   *sfu.SFU
	logs  *logRecorder
}

// newRealPlane builds the three in the wiring's order: the Plane and its RoomEvents, the SFU with those events,
// then Bind. Everything closes with the test, the SFU before its Transport.
func newRealPlane(t *testing.T) *realPlane {
	t.Helper()
	tr, err := netx.NewTransport(context.Background(), netx.TransportOptions{
		UDPAddr: "127.0.0.1:0", IncludeLoopback: true, Interfaces: loopbackOnly{},
	})
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	t.Cleanup(func() {
		if err := tr.Close(); err != nil {
			t.Errorf("close the transport: %v", err)
		}
	})
	rp := &realPlane{logs: newLogRecorder()}
	var events sfu.RoomEvents
	rp.plane, events = New(slog.New(rp.logs))
	rp.sfu, err = sfu.New(sfu.Config{Transport: tr}, sfu.Deps{Events: events})
	if err != nil {
		t.Fatalf("sfu.New: %v", err)
	}
	t.Cleanup(func() {
		if err := rp.sfu.Close(); err != nil {
			t.Errorf("SFU.Close: %v", err)
		}
	})
	rp.plane.Bind(rp.sfu)
	return rp
}

// join makes the peer of a connection in the lounge, with a recording sink.
func (rp *realPlane) join(t *testing.T, connID, userID string, role protocol.Role) (signal.MediaPeer, *signaltest.Sink) {
	t.Helper()
	sink := &signaltest.Sink{}
	p, err := rp.plane.NewPeer(signal.PeerParams{
		ConnectionID: connID, UserID: userID, RoomID: "lounge", Role: role,
		Caps:   protocol.Caps{Decode: []protocol.CodecKey{protocol.CodecOpus, protocol.CodecH264ConstrainedBaseline, protocol.CodecH264High}},
		Client: protocol.ClientInfo{Kind: protocol.ClientKindTool, Version: "0.0.0"},
	}, sink)
	if err != nil {
		t.Fatalf("NewPeer(%s): %v", connID, err)
	}
	return p, sink
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func eventually(t *testing.T, cond func() bool, what func() string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// newPublisher returns a Pion publisher of small fake media, like the web sharer: two simulcast video layers (f and
// q, in H.264 High) and audio. It closes with the test.
func newPublisher(t *testing.T) *publish.Publisher {
	t.Helper()
	src, err := fake.New(fake.Config{
		Layers: []fake.VideoLayer{
			{RID: "f", Width: 640, Height: 360, FPS: 30, Bitrate: 400_000},
			{RID: "q", Width: 320, Height: 180, FPS: 15, Bitrate: 100_000},
		},
		GOP: time.Second, Audio: true, Seed: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	pub, err := publish.New(publish.Options{Source: src, Settings: sfutest.LoopbackSettings(), Audio: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pub.Close(); err != nil {
			t.Errorf("Publisher.Close: %v", err)
		}
	})
	return pub
}

// publisherOffer returns the publisher's first pub offer as the wire carries it: gen 1, neg 1, and its video and its
// audio m-section bound to the share.
func publisherOffer(t *testing.T, pub *publish.Publisher, shareID string) protocol.PCOffer {
	t.Helper()
	offer, err := pub.Offer(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	var tracks []protocol.TrackRef
	for _, tr := range pub.Tracks() { // one per simulcast encoding: the video's m-section comes twice
		kind := protocol.TrackKindVideo
		if tr.Kind == webrtc.RTPCodecTypeAudio {
			kind = protocol.TrackKindAudio
		}
		if ref := (protocol.TrackRef{MID: tr.MID, ShareID: shareID, Kind: kind}); !slices.Contains(tracks, ref) {
			tracks = append(tracks, ref)
		}
	}
	return protocol.PCOffer{PC: protocol.PCKindPub, Gen: 1, Neg: 1, SDP: protocol.SDP(offer.SDP), Tracks: tracks}
}

// newViewer returns a Pion subscriber like a browser, which records what it receives. It closes with the test.
func newViewer(t *testing.T) *sfutest.Viewer {
	t.Helper()
	v, err := sfutest.NewViewer(sfutest.ViewerOptions{Settings: sfutest.LoopbackSettings()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := v.Close(); err != nil {
			t.Errorf("Viewer.Close: %v", err)
		}
	})
	return v
}

// viewerAnswer has the viewer answer a sub offer and returns the answer as the wire carries it.
func viewerAnswer(t *testing.T, v *sfutest.Viewer, offer protocol.PCOffer) protocol.PCAnswer {
	t.Helper()
	answer, err := v.Answer(testCtx(t), webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer.SDP.Reveal()})
	if err != nil {
		t.Fatalf("the viewer can't answer the sub offer: %v", err)
	}
	return protocol.PCAnswer{PC: offer.PC, Gen: offer.Gen, Neg: offer.Neg, SDP: protocol.SDP(answer.SDP)}
}

// waitMedia waits until the viewer has received video and audio.
func waitMedia(t *testing.T, v *sfutest.Viewer) {
	t.Helper()
	ctx := testCtx(t)
	for _, kind := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio} {
		rec, err := v.WaitRecorder(ctx, func(r *sfutest.Recorder) bool { return r.Kind() == kind })
		if err != nil {
			t.Fatalf("the viewer got no %s track: %v", kind, err)
		}
		if err := rec.Wait(ctx, func(r *sfutest.Recorder) bool { return r.Stats().Packets >= 20 }); err != nil {
			t.Fatalf("the viewer got no %s: %v", kind, err)
		}
	}
}

// sinkEvents returns the arguments of a sink's recorded calls of one method.
func sinkEvents[T any](sink *signaltest.Sink, method string) []T {
	var out []T
	for _, ev := range sink.Events() {
		if ev.Method == method {
			out = append(out, ev.Arg.(T))
		}
	}
	return out
}

// wantWire fails unless err is the *protocol.Error that a MediaPeer returns, with the code and scope given.
func wantWire(t *testing.T, what string, err error, code protocol.ErrorCode, scope protocol.ErrorScope) protocol.Error {
	t.Helper()
	var pe *protocol.Error
	if !errors.As(err, &pe) {
		t.Fatalf("%s: error = %v (%T), want %s", what, err, err, code)
	}
	if pe.Code != code || pe.Scope != scope || pe.Retryable != protocol.NewError(code, scope).Retryable {
		t.Errorf("%s: error = %+v, want %s with scope %s", what, *pe, code, scope)
	}
	return *pe
}

// TestRealSFU drives one publisher and one viewer through the adapter on a real SFU: join, share, publish,
// subscribe, media, stats, the errors a client can cause, and the end.
func TestRealSFU(t *testing.T) {
	rp := newRealPlane(t)
	ctx := testCtx(t)
	const shareID = "s_q7m2x9c4v8b1n5k3"
	request, pcScope := protocol.ErrorScopeRequest, protocol.ErrorScopePC

	// ---- NewPeer: SFU.Join ----
	pub, pubSink := rp.join(t, "c_pub", "u_alice", protocol.RoleFull)
	view, viewSink := rp.join(t, "c_view", "u_bob", protocol.RoleViewer)
	snap := rp.sfu.Snapshot()
	wantConns := []sfu.ConnSummary{{Conn: "c_pub", User: "u_alice"}, {Conn: "c_view", User: "u_bob"}}
	if len(snap.Rooms) != 1 || snap.Rooms[0].Room != "lounge" || !slices.Equal(snap.Rooms[0].Conns, wantConns) {
		t.Fatalf("the SFU's rooms after two joins = %+v", snap.Rooms)
	}
	if m := rp.sfu.Metrics(); m.Conns[string(protocol.RoleFull)] != 1 || m.Conns[string(protocol.RoleViewer)] != 1 {
		t.Errorf("the SFU counts its Conns by role as %v, want one full and one viewer", m.Conns)
	}
	// The same connection can't join twice: the SFU refuses, and NewPeer's plain error becomes the hub's internal.
	if again, err := rp.plane.NewPeer(signal.PeerParams{
		ConnectionID: "c_pub", UserID: "u_alice", RoomID: "lounge", Role: protocol.RoleFull,
	}, &signaltest.Sink{}); again != nil || !errors.Is(err, &sfu.Error{Code: sfu.CodeInternal}) {
		t.Errorf("a second peer for a joined connection = %v, %v; want the SFU's contract error", again, err)
	}
	if rp.plane.peers["c_pub"] != pub.(*peer) {
		t.Error("the refused join replaced the connection's peer")
	}

	// ---- CreateShare, UpdateShare: Conn.StartShare, Conn.UpdateShare ----
	params, err := pub.CreateShare(shareID, protocol.ShareStart{
		Kind: protocol.ShareKindWindow, Label: "never reaches the SFU", Preset: protocol.PresetMovie, Audio: true, Ref: "r1",
	})
	if err != nil {
		t.Fatalf("CreateShare: %v", err)
	}
	info, ok := rp.sfu.Share(shareID)
	if !ok || info.Room != "lounge" || info.Conn != "c_pub" || info.User != "u_alice" || info.Participant != "u_alice" ||
		info.Source != sfu.SourceWindow || info.Preset != sfu.PresetMovie || info.State != sfu.SharePending {
		t.Errorf("the SFU's share = %+v, %v", info, ok)
	}
	// The reply is the SFU's ShareParams, converted: read them again from the Conn itself and compare.
	checkParams := func(what string, got protocol.ShareParams) {
		t.Helper()
		raw, err := pub.(*peer).conn.UpdateShare(ctx, shareID, sfu.ShareUpdate{})
		if err != nil {
			t.Fatalf("%s: reading the SFU's params: %v", what, err)
		}
		if got.ShareID != shareID || got.Codec != protocol.CodecKey("h264/"+string(raw.Profile)) || !got.Codec.IsH264() ||
			got.AudioBitrate != raw.AudioBitrate || got.AudioBitrate <= 0 || len(got.Encodings) != len(raw.Encodings) {
			t.Fatalf("%s: params = %+v, the SFU's are %+v", what, got, raw)
		}
		for i, e := range got.Encodings {
			r := raw.Encodings[i]
			if e.RID != r.RID || e.Active != r.Active || e.MaxBitrate != int64(r.MaxBitrate) || e.MaxBitrate <= 0 ||
				e.MaxFramerate != r.MaxFramerate || e.MaxPixels != r.MaxPixels {
				t.Errorf("%s: encoding %d = %+v, the SFU's is %+v", what, i, e, r)
			}
		}
		// Wire order: high first (f, then q), each with the layer of its rid (01 §8.7).
		if len(got.Encodings) != 2 || got.Encodings[0].RID != protocol.RIDHigh || got.Encodings[0].Layer != protocol.VideoLayerHigh ||
			got.Encodings[1].RID != protocol.RIDLow || got.Encodings[1].Layer != protocol.VideoLayerLow {
			t.Errorf("%s: encodings = %+v, want f/high then q/low", what, got.Encodings)
		}
	}
	checkParams("CreateShare", params)
	if params.Codec != protocol.CodecH264High {
		t.Errorf("codec = %q, want high: everyone in the room decodes it", params.Codec)
	}
	movieAudio := params.AudioBitrate

	updated, err := pub.UpdateShare(shareID, protocol.ShareUpdate{ShareID: shareID, Preset: protocol.PresetText})
	if err != nil {
		t.Fatalf("UpdateShare: %v", err)
	}
	checkParams("UpdateShare", updated)
	if info, _ := rp.sfu.Share(shareID); info.Preset != sfu.PresetText {
		t.Errorf("the SFU's preset after share.update = %v, want text", info.Preset)
	}
	if reflect.DeepEqual(updated, params) || updated.AudioBitrate >= movieAudio {
		t.Errorf("the text preset's params %+v don't differ from the movie preset's %+v as 02 §8.6 says", updated, params)
	}
	// A label change never reaches the SFU: the preset stays, and the reply is the share's current params.
	relabeled, err := pub.UpdateShare(shareID, protocol.ShareUpdate{ShareID: shareID, Label: ptr("new label")})
	if err != nil || !reflect.DeepEqual(relabeled, updated) {
		t.Errorf("UpdateShare with a label only = %+v, %v; want %+v", relabeled, err, updated)
	}
	if info, _ := rp.sfu.Share(shareID); info.Preset != sfu.PresetText {
		t.Errorf("the SFU's preset after a label change = %v, want text", info.Preset)
	}

	_, err = pub.UpdateShare("s_nope", protocol.ShareUpdate{ShareID: "s_nope", Preset: protocol.PresetGame})
	wantWire(t, "share.update of an unknown share", err, protocol.ErrorCodeShareNotFound, request)
	_, err = view.UpdateShare(shareID, protocol.ShareUpdate{ShareID: shareID, Preset: protocol.PresetGame})
	wantWire(t, "share.update of another connection's share", err, protocol.ErrorCodeForbidden, request)
	_, err = view.CreateShare("s_viewer", protocol.ShareStart{Kind: protocol.ShareKindScreen, Preset: protocol.PresetAuto, Ref: "r"})
	wantWire(t, "share.start from a viewer", err, protocol.ErrorCodeForbidden, request)

	// ---- HandleOffer: Conn.HandleOffer ----
	publisher := newPublisher(t)
	pubOffer := publisherOffer(t, publisher, shareID)

	_, err = view.HandleOffer(pubOffer)
	if e := wantWire(t, "a pub offer from a viewer", err, protocol.ErrorCodeForbidden, pcScope); e.PC != protocol.PCKindPub || e.Gen != 1 || e.Neg != 1 {
		t.Errorf("the viewer's offer error names %+v, want pub, gen 1, neg 1", e)
	}
	garbage := pubOffer
	garbage.SDP = "this is not SDP"
	_, err = pub.HandleOffer(garbage)
	wantWire(t, "a pub offer that doesn't parse", err, protocol.ErrorCodeSDPInvalid, pcScope)

	answer, err := pub.HandleOffer(pubOffer)
	if err != nil {
		t.Fatalf("HandleOffer: %v", err)
	}
	if answer.PC != protocol.PCKindPub || answer.Gen != 1 || answer.Neg != 1 || !strings.Contains(answer.SDP.Reveal(), "a=candidate:") {
		t.Fatalf("answer = %+v, want pub, gen 1, neg 1 and the server's candidates in the SDP", answer)
	}
	if err := publisher.SetAnswer(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP.Reveal()}); err != nil {
		t.Fatalf("the publisher refused the answer: %v", err)
	}
	// The same neg again (a resume replays it): the stored answer. A counter below the current one: stale.
	if again, err := pub.HandleOffer(pubOffer); err != nil || again != answer {
		t.Errorf("the repeated offer's answer differs: %v", err)
	}
	stale := pubOffer
	stale.Neg = 0
	_, err = pub.HandleOffer(stale)
	if e := wantWire(t, "a stale pub offer", err, protocol.ErrorCodeStaleNegotiation, pcScope); e.PC != protocol.PCKindPub || e.Gen != 1 || e.Neg != 0 {
		t.Errorf("the stale offer's error names %+v, want pub, gen 1, neg 0", e)
	}

	// ---- Subscribe, the sub offer, HandleAnswer, AddICE ----
	ignored, err := view.Subscribe([]protocol.SubscriptionWant{
		{ShareID: "s_gone", Video: protocol.VideoLayerLow, Audio: protocol.AudioStateOff},
		{ShareID: shareID, Video: protocol.VideoLayerHigh, Audio: protocol.AudioStateOn},
	})
	if err != nil || !slices.Equal(ignored, []string{"s_gone"}) {
		t.Fatalf("Subscribe = %v, %v; want the unknown share ignored", ignored, err)
	}
	// The SFU's guard on one call: 64 items (the hub's validation refuses a 65th first).
	tooMany := make([]protocol.SubscriptionWant, 65)
	for i := range tooMany {
		tooMany[i] = protocol.SubscriptionWant{
			ShareID: fmt.Sprintf("s_many%d", i), Video: protocol.VideoLayerLow, Audio: protocol.AudioStateOff,
		}
	}
	_, err = view.Subscribe(tooMany)
	if e := wantWire(t, "65 wants in one call", err, protocol.ErrorCodeBadRequest, request); !reflect.DeepEqual(e.Params, map[string]any{"field": "subs", "reason": "too_many"}) {
		t.Errorf("params = %v, want the subs field with too_many", e.Params)
	}

	var subOffer protocol.PCOffer
	eventually(t, func() bool {
		offers := sinkEvents[protocol.PCOffer](viewSink, "Offer")
		if len(offers) == 0 {
			return false
		}
		subOffer = offers[0]
		return true
	}, func() string { return fmt.Sprintf("no sub offer; the viewer's sink has %+v", viewSink.Events()) })
	if subOffer.PC != protocol.PCKindSub || subOffer.Gen != 1 || subOffer.Neg != 1 || len(subOffer.Tracks) != 2 {
		t.Fatalf("sub offer = %+v, want sub, gen 1, neg 1 and the share's two tracks", subOffer)
	}
	kinds := map[protocol.TrackKind]string{}
	for _, tr := range subOffer.Tracks {
		if tr.ShareID != shareID || tr.MID == "" {
			t.Errorf("sub offer track %+v, want a mid of share %s", tr, shareID)
		}
		kinds[tr.Kind] = tr.MID
	}
	if len(kinds) != 2 || kinds[protocol.TrackKindVideo] == "" || kinds[protocol.TrackKindAudio] == "" {
		t.Errorf("sub offer tracks = %+v, want one video and one audio m-section", subOffer.Tracks)
	}

	viewer := newViewer(t)
	wireAnswer := viewerAnswer(t, viewer, subOffer)
	// An answer to another offer than the outstanding one is dropped without a word; an answer for the pub PC is not
	// an answer at all.
	staleAnswer := wireAnswer
	staleAnswer.Neg = 9
	if err := view.HandleAnswer(staleAnswer); err != nil {
		t.Errorf("a stale answer = %v, want it dropped silently", err)
	}
	wrongPC := wireAnswer
	wrongPC.PC = protocol.PCKindPub
	wantWire(t, "an answer for the pub PC", view.HandleAnswer(wrongPC), protocol.ErrorCodeBadRequest, pcScope)
	if err := view.HandleAnswer(wireAnswer); err != nil {
		t.Fatalf("HandleAnswer: %v", err)
	}
	// Trickled candidates and the end-of-candidates marker are taken for either PC. (The address is TEST-NET-1's:
	// no check to it reaches anything.)
	for _, c := range []protocol.PCICE{
		{PC: protocol.PCKindSub, Gen: 1, Candidate: &protocol.ICECandidate{
			Candidate: "candidate:1 1 udp 2130706431 192.0.2.1 50000 typ host", SDPMid: ptr(kinds[protocol.TrackKindVideo]), SDPMLineIndex: ptr(uint16(0)),
		}},
		{PC: protocol.PCKindSub, Gen: 1, Candidate: &protocol.ICECandidate{}},
		{PC: protocol.PCKindSub, Gen: 1},
		{PC: protocol.PCKindPub, Gen: 1, Candidate: &protocol.ICECandidate{Candidate: "candidate:2 1 udp 2130706431 192.0.2.1 50001 typ host"}},
	} {
		if err := view.AddICE(c); err != nil {
			t.Errorf("AddICE(%+v) = %v", c, err)
		}
	}

	// ---- media: RoomEvents.ShareUpdated → ShareMedia on the publishing connection's sink ----
	if facts := sinkEvents[signaltest.ShareMediaCall](pubSink, "ShareMedia"); len(facts) != 0 {
		t.Errorf("share facts before any media: %+v", facts)
	}
	if err := publisher.Start(ctx); err != nil {
		t.Fatal(err)
	}
	var facts []signaltest.ShareMediaCall
	bothLayers := []protocol.VideoLayer{protocol.VideoLayerHigh, protocol.VideoLayerLow}
	eventually(t, func() bool {
		facts = sinkEvents[signaltest.ShareMediaCall](pubSink, "ShareMedia")
		if len(facts) == 0 {
			return false
		}
		last := facts[len(facts)-1].Event
		return slices.Equal(last.Layers, bothLayers) && last.Audio && last.Codec == protocol.CodecH264High
	}, func() string { return fmt.Sprintf("the publisher's share facts are %+v", facts) })
	for i, f := range facts {
		want := signal.ShareMediaChanged
		if i == 0 {
			want = signal.ShareMediaLive // the first keyframe; everything after it is a change while live
		}
		if f.ShareID != shareID || f.Event.Kind != want || len(f.Event.Layers) == 0 || f.Event.Codec != protocol.CodecH264High {
			t.Errorf("share fact %d = %+v, want %v of %s with its layers and codec", i, f, want, shareID)
		}
	}
	if stray := sinkEvents[signaltest.ShareMediaCall](viewSink, "ShareMedia"); len(stray) != 0 {
		t.Errorf("the viewer's sink got share facts: %+v", stray)
	}

	// The viewer gets what it subscribed to.
	waitMedia(t, viewer)

	// ---- Stats: Conn.Stats ----
	var st protocol.ServerStats
	eventually(t, func() bool {
		st = pub.Stats()
		return len(st.Layers) >= 2
	}, func() string { return fmt.Sprintf("the publisher's stats are %+v", st) })
	var rids []string
	for _, l := range st.Layers {
		if l.ShareID != shareID || !l.Kind.Valid() {
			t.Errorf("stats layer %+v, want one of share %s", l, shareID)
		}
		if l.Kind == protocol.TrackKindVideo {
			rids = append(rids, l.RID)
		}
	}
	if !slices.Equal(rids, []string{protocol.RIDHigh, protocol.RIDLow}) || st.Subs == nil {
		t.Errorf("the publisher's stats = %+v, want its f and q layers", st)
	}
	if vs := view.Stats(); vs.Subs == nil || vs.Layers == nil || len(vs.Layers) != 0 {
		t.Errorf("the viewer's stats = %#v, want no published layers and lists that are never nil", vs)
	}

	// ---- the calls of later slices: whatever the SFU says today, the answer is nil or a wire error ----
	pub.Resync()
	view.Resync()
	view.SetCaps(protocol.Caps{Decode: []protocol.CodecKey{protocol.CodecOpus, protocol.CodecH264High}})
	// (Gen 0 is below every PC's gen, so once these methods exist they still change nothing here.)
	for what, err := range map[string]error{
		"pc.restart{ice}":     view.Restart(protocol.PCRestart{PC: protocol.PCKindSub, Gen: 0, Mode: protocol.RestartModeICE, Reason: protocol.RestartReasonDisconnected}),
		"pc.restart{rebuild}": view.Restart(protocol.PCRestart{PC: protocol.PCKindSub, Gen: 0, Mode: protocol.RestartModeRebuild, Reason: protocol.RestartReasonFailed}),
		"pc.close":            pub.ClosePC(protocol.PCClose{PC: protocol.PCKindPub, Gen: 0}),
	} {
		var pe *protocol.Error
		if err != nil && (!errors.As(err, &pe) || pe.Scope != pcScope || !pe.Code.Valid()) {
			t.Errorf("%s = %v (%T), want nil or a wire error with scope pc", what, err, err)
		}
	}

	// ---- the SFU's own share guard: share_limit with its limit ----
	for i := 1; i < sharesPerParticipant; i++ {
		if _, err := pub.CreateShare(fmt.Sprintf("s_extra%d", i), protocol.ShareStart{Kind: protocol.ShareKindTab, Preset: protocol.PresetAuto, Ref: "r"}); err != nil {
			t.Fatalf("share %d of %d: %v", i+1, sharesPerParticipant, err)
		}
	}
	_, err = pub.CreateShare("s_one_too_many", protocol.ShareStart{Kind: protocol.ShareKindTab, Preset: protocol.PresetAuto, Ref: "r"})
	if e := wantWire(t, "one share past the SFU's guard", err, protocol.ErrorCodeShareLimit, request); !reflect.DeepEqual(e.Params, map[string]any{"limit": sharesPerParticipant, "per": "user"}) {
		t.Errorf("share_limit params = %v, want the SFU's limit of %d per user", e.Params, sharesPerParticipant)
	}

	// Nothing so far is the adapter's or the SFU's bug, so nothing was logged as one. (A method of a later slice is
	// logged as an internal error until its slice lands: those lines name their call.)
	for _, l := range atLeast(rp.logs.take(), slog.LevelWarn) {
		switch l.Attrs["call"] {
		case "RestartICE", "ResetPC", "ClosePC", "SetDecodeCaps":
		default:
			t.Errorf("logged %+v", l)
		}
	}

	// ---- EndShare: Conn.StopShare ----
	pub.EndShare(shareID, protocol.EndReasonStopped)
	if _, ok := rp.sfu.Share(shareID); ok {
		t.Error("the share is still in the SFU after EndShare")
	}
	pub.EndShare(shareID, protocol.EndReasonStopped) // again: nothing left to end, and nothing to say
	if lines := atLeast(rp.logs.take(), slog.LevelInfo); len(lines) != 0 {
		t.Errorf("ending a share, twice, logged %+v", lines)
	}
	// The hub always ends a share at the connection that publishes it. Anything else is its bug, and logged as one.
	view.EndShare("s_extra1", protocol.EndReasonStopped)
	if _, ok := rp.sfu.Share("s_extra1"); !ok {
		t.Error("a connection ended another connection's share")
	}
	if lines := atLeast(rp.logs.take(), slog.LevelInfo); len(lines) != 1 || lines[0].Level != slog.LevelError || lines[0].Attrs["call"] != "StopShare" {
		t.Errorf("ending another connection's share logged %+v, want one error", lines)
	}
	if ignored, err := view.Subscribe([]protocol.SubscriptionWant{{ShareID: shareID, Video: protocol.VideoLayerLow, Audio: protocol.AudioStateOff}}); err != nil || !slices.Equal(ignored, []string{shareID}) {
		t.Errorf("Subscribe to the ended share = %v, %v; want it ignored", ignored, err)
	}
	rp.plane.mu.Lock()
	kept := len(pub.(*peer).media)
	rp.plane.mu.Unlock()
	if kept != 0 {
		t.Errorf("%d share states kept after the only reported share ended", kept)
	}

	// No error reached a client on the way, and no sfu.* code is in anything a sink got.
	for name, sink := range map[string]*signaltest.Sink{"publisher": pubSink, "viewer": viewSink} {
		if errs := sinkEvents[protocol.Error](sink, "Error"); len(errs) != 0 {
			t.Errorf("the %s's sink got errors: %+v", name, errs)
		}
	}

	// ---- Close: Conn.Close ----
	view.Close()
	pub.Close()
	if rooms := rp.sfu.Snapshot().Rooms; len(rooms) != 0 {
		t.Errorf("the SFU's rooms after both peers closed = %+v", rooms)
	}
	if n := len(rp.plane.peers); n != 0 {
		t.Errorf("%d peers left in the Plane", n)
	}
	// A closed Conn: the requests are answered not_in_room, the notifications with nothing.
	_, err = pub.CreateShare("s_late", protocol.ShareStart{Kind: protocol.ShareKindScreen, Preset: protocol.PresetAuto, Ref: "r"})
	wantWire(t, "share.start after Close", err, protocol.ErrorCodeNotInRoom, request)
	_, err = view.Subscribe([]protocol.SubscriptionWant{{ShareID: "s_extra1", Video: protocol.VideoLayerLow, Audio: protocol.AudioStateOff}})
	wantWire(t, "subscribe.update after Close", err, protocol.ErrorCodeNotInRoom, request)
	_, err = pub.HandleOffer(pubOffer)
	wantWire(t, "a pub offer after Close", err, protocol.ErrorCodeNotInRoom, pcScope)
	if err := view.HandleAnswer(wireAnswer); err != nil {
		t.Errorf("an answer after Close = %v, want nothing", err)
	}
	pub.EndShare("s_extra1", protocol.EndReasonLeft)
	pub.Close() // idempotent

	// The same connection joins again (the hub does this for a room.join elsewhere) and gets a new peer.
	back, _ := rp.join(t, "c_pub", "u_alice", protocol.RoleFull)
	if _, err := back.CreateShare("s_again", protocol.ShareStart{Kind: protocol.ShareKindScreen, Preset: protocol.PresetAuto, Ref: "r"}); err != nil {
		t.Errorf("CreateShare after rejoining: %v", err)
	}

	if lines := atLeast(rp.logs.take(), slog.LevelWarn); len(lines) != 0 {
		t.Errorf("closing and rejoining logged %+v", lines)
	}
}

// TestRealSFUClosed: once the SFU has closed (the server is stopping), a share's end is reported by the SFU and
// ignored here, peers answer as closed, and no new peer can join.
func TestRealSFUClosed(t *testing.T) {
	rp := newRealPlane(t)
	pub, sink := rp.join(t, "c_pub", "u_alice", protocol.RoleFull)
	if _, err := pub.CreateShare("s_1", protocol.ShareStart{Kind: protocol.ShareKindScreen, Preset: protocol.PresetAuto, Ref: "r"}); err != nil {
		t.Fatal(err)
	}
	if err := rp.sfu.Close(); err != nil {
		t.Fatal(err)
	}
	if events := sink.Events(); len(events) != 0 {
		t.Errorf("the sink got %+v: the hub ends its shares itself", events)
	}
	_, err := pub.UpdateShare("s_1", protocol.ShareUpdate{ShareID: "s_1", Preset: protocol.PresetGame})
	wantWire(t, "share.update on a closed SFU", err, protocol.ErrorCodeNotInRoom, protocol.ErrorScopeRequest)
	pub.EndShare("s_1", protocol.EndReasonServerShutdown)
	pub.Close()

	late, err := rp.plane.NewPeer(signal.PeerParams{
		ConnectionID: "c_late", UserID: "u_bob", RoomID: "lounge", Role: protocol.RoleViewer,
	}, &signaltest.Sink{})
	if late != nil || !errors.Is(err, &sfu.Error{Code: sfu.CodeClosed}) {
		t.Errorf("NewPeer on a closed SFU = %v, %v", late, err)
	}
	if n := len(rp.plane.peers); n != 0 {
		t.Errorf("%d peers left in the Plane", n)
	}
	if lines := atLeast(rp.logs.take(), slog.LevelWarn); len(lines) != 0 {
		t.Errorf("logged %+v", lines)
	}
}

// TestRealSFUReasons: every subscription reason that the real SFU counts (the label values of
// isshoni_sfu_downgrades_total, 02 §13) has a wire reason.
func TestRealSFUReasons(t *testing.T) {
	rp := newRealPlane(t)
	reasons := rp.sfu.Metrics().Downgrades
	if len(reasons) == 0 {
		t.Fatal("the SFU's metrics list no downgrade reasons")
	}
	for r := range reasons {
		ev := sfu.SubscriptionStateEvent{Requested: sfu.QualityHigh, Forwarded: sfu.QualityOff, Reason: sfu.SubReason(r)}
		if wire, known := statusReason(ev); !known || !wire.Valid() {
			t.Errorf("the SFU's reason %q maps to %q (known: %v)", r, wire, known)
		}
	}
}
