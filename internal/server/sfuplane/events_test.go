package sfuplane

import (
	"go/ast"
	"log/slog"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/sfu"
	"github.com/MoonWX/isshoni/internal/server/signal"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

// The "Events" table of 01 §15.4: what the SFU sends through a Conn's Signaler and through its RoomEvents becomes
// MediaSink calls, recorded here by signaltest.Sink.

// stripRefs replaces the random ref of each internal error among events with "ref", after checking its form.
func stripRefs(t *testing.T, events []signaltest.Event) []signaltest.Event {
	t.Helper()
	for i, ev := range events {
		if e, ok := ev.Arg.(protocol.Error); ok && e.Code == protocol.ErrorCodeInternal {
			checkRef(t, e)
			e.Params = map[string]any{"ref": "ref"}
			events[i].Arg = e
		}
	}
	return events
}

// sinkReader reads what a sink recorded since the last read.
type sinkReader struct {
	sink *signaltest.Sink
	seen int
}

func (r *sinkReader) next() []signaltest.Event {
	all := r.sink.Events()
	fresh := all[r.seen:]
	r.seen = len(all)
	return fresh
}

func TestSendOffer(t *testing.T) {
	tp := newTestPlane(t)
	pr := tp.newPeer("c_1", "u_1")
	tracks := []sfu.TrackBinding{
		{MID: "0", Share: "s_a", Kind: webrtc.RTPCodecTypeVideo},
		{MID: "1", Share: "s_a", Kind: webrtc.RTPCodecTypeAudio},
		{MID: "4", Share: "s_b", Kind: webrtc.RTPCodecTypeVideo},
	}
	pr.peer.SendOffer(sfu.PCSub, 2, 3, "v=0\r\nthe sub offer\r\n", tracks)
	tracks[0].MID = "reused" // the SFU may reuse its slice once the call has returned
	pr.peer.SendOffer(sfu.PCSub, 3, 1, "v=0\r\nno tracks yet\r\n", nil)
	want := []signaltest.Event{
		{Method: "Offer", Arg: protocol.PCOffer{
			PC: protocol.PCKindSub, Gen: 2, Neg: 3, SDP: "v=0\r\nthe sub offer\r\n",
			Tracks: []protocol.TrackRef{
				{MID: "0", ShareID: "s_a", Kind: protocol.TrackKindVideo},
				{MID: "1", ShareID: "s_a", Kind: protocol.TrackKindAudio},
				{MID: "4", ShareID: "s_b", Kind: protocol.TrackKindVideo},
			},
		}},
		{Method: "Offer", Arg: protocol.PCOffer{
			PC: protocol.PCKindSub, Gen: 3, Neg: 1, SDP: "v=0\r\nno tracks yet\r\n", Tracks: []protocol.TrackRef{},
		}},
	}
	if got := pr.sink.Events(); !reflect.DeepEqual(got, want) {
		t.Errorf("the sink got\n %+v\nwant\n %+v", got, want)
	}
	if logs := atLeast(tp.logs.take(), slog.LevelInfo); len(logs) != 0 {
		t.Errorf("logged %+v", logs)
	}

	// The server offers on sub only (01 §9 rule 1): anything else is a bug in the SFU and goes nowhere.
	pr.peer.SendOffer(sfu.PCPub, 1, 1, "v=0\r\n", nil)
	pr.peer.SendOffer(0, 1, 1, "v=0\r\n", nil)
	if got := pr.sink.Events(); len(got) != 2 {
		t.Errorf("an offer that isn't for the sub PC reached the sink: %+v", got[2:])
	}
	if logs := atLeast(tp.logs.take(), slog.LevelError); len(logs) != 2 {
		t.Errorf("logged %+v, want two errors", logs)
	}

	// A binding that is neither video nor audio has no TrackRef: the offer goes out without it, and the log says so.
	pr.peer.SendOffer(sfu.PCSub, 3, 2, "v=0\r\n", []sfu.TrackBinding{
		{MID: "0", Share: "s_a", Kind: webrtc.RTPCodecTypeVideo}, {MID: "1", Share: "s_a", Kind: webrtc.RTPCodecType(0)},
	})
	got := pr.sink.Events()
	wantRefs := []protocol.TrackRef{{MID: "0", ShareID: "s_a", Kind: protocol.TrackKindVideo}}
	if len(got) != 3 || !slices.Equal(got[2].Arg.(protocol.PCOffer).Tracks, wantRefs) {
		t.Errorf("the sink got %+v, want an offer with the video track only", got[2:])
	}
	if logs := atLeast(tp.logs.take(), slog.LevelError); len(logs) != 1 {
		t.Errorf("logged %+v, want one error", logs)
	}
}

func TestEvents(t *testing.T) {
	high, low, off := protocol.VideoLayerHigh, protocol.VideoLayerLow, protocol.VideoLayerOff
	on, mute := protocol.AudioStateOn, protocol.AudioStateOff
	status := func(share string, video protocol.VideoLayer, audio protocol.AudioState, requested protocol.VideoLayer,
		reason protocol.StatusReason,
	) []signaltest.Event {
		return []signaltest.Event{{Method: "SubscriptionStatus", Arg: []protocol.SubscriptionStatus{{
			ShareID: share, Video: video, Audio: audio, RequestedVideo: requested, Reason: reason,
		}}}}
	}
	restart := []signaltest.Event{{Method: "RestartRequest", Arg: protocol.PCRestart{
		PC: protocol.PCKindPub, Gen: 3, Mode: protocol.RestartModeRebuild, Reason: protocol.RestartReasonFailed,
	}}}
	wireErr := func(code protocol.ErrorCode, scope protocol.ErrorScope, set func(*protocol.Error)) []signaltest.Event {
		e := protocol.NewError(code, scope)
		if set != nil {
			set(&e)
		}
		return []signaltest.Event{{Method: "Error", Arg: e}}
	}
	encodings := []sfu.EncodingParams{
		{RID: "f", Active: false, MaxBitrate: 2_500_000, MaxFramerate: 60, MaxPixels: 2_073_600},
		{RID: "q", Active: true, MaxBitrate: 300_000, MaxFramerate: 15, MaxPixels: 230_400},
	}
	wireEncs := []protocol.Encoding{
		{RID: "f", Layer: high, Active: false, MaxBitrate: 2_500_000, MaxFramerate: 60, MaxPixels: 2_073_600},
		{RID: "q", Layer: low, Active: true, MaxBitrate: 300_000, MaxFramerate: 15, MaxPixels: 230_400},
	}

	tests := []struct {
		name string
		ev   sfu.Event
		want []signaltest.Event // nil: nothing reaches the sink
	}{
		// SubscriptionStateEvent → SubscriptionStatus.
		{"forwarded as asked", sfu.SubscriptionStateEvent{Share: "s_a", Requested: sfu.QualityHigh, Forwarded: sfu.QualityHigh, Audio: true},
			status("s_a", high, on, high, "")},
		{"a thumbnail, muted", sfu.SubscriptionStateEvent{Share: "s_a", Requested: sfu.QualityLow, Forwarded: sfu.QualityLow},
			status("s_a", low, mute, low, "")},
		{"paused", sfu.SubscriptionStateEvent{Share: "s_a", Requested: sfu.QualityOff, Forwarded: sfu.QualityOff},
			status("s_a", off, mute, off, "")},
		{"the sub PC isn't connected yet", sfu.SubscriptionStateEvent{Share: "s_a", Requested: sfu.QualityHigh, Forwarded: sfu.QualityOff},
			status("s_a", off, mute, high, protocol.StatusReasonWaiting)},
		{"waiting for the high layer's keyframe", sfu.SubscriptionStateEvent{Share: "s_a", Requested: sfu.QualityHigh, Forwarded: sfu.QualityLow, Audio: true},
			status("s_a", low, on, high, protocol.StatusReasonWaiting)},
		{"capped by the downlink", sfu.SubscriptionStateEvent{Share: "s_a", Requested: sfu.QualityHigh, Forwarded: sfu.QualityLow, Audio: true, Reason: sfu.SubReasonBandwidth},
			status("s_a", low, on, high, protocol.StatusReasonBandwidth)},
		{"capped by the server", sfu.SubscriptionStateEvent{Share: "s_a", Requested: sfu.QualityHigh, Forwarded: sfu.QualityLow, Reason: sfu.SubReasonServerLimit},
			status("s_a", low, mute, high, protocol.StatusReasonBandwidth)},
		{"codec-blocked: audio only", sfu.SubscriptionStateEvent{Share: "s_a", Requested: sfu.QualityHigh, Forwarded: sfu.QualityOff, Audio: true, Reason: sfu.SubReasonDecoderUnavailable},
			status("s_a", off, on, high, protocol.StatusReasonCodec)},
		{"a profile the viewer can't decode", sfu.SubscriptionStateEvent{Share: "s_a", Requested: sfu.QualityLow, Forwarded: sfu.QualityOff, Reason: sfu.SubReasonCodecMismatch},
			status("s_a", off, mute, low, protocol.StatusReasonCodec)},
		{"the viewer's decoder failed", sfu.SubscriptionStateEvent{Share: "s_a", Requested: sfu.QualityHigh, Forwarded: sfu.QualityOff, Reason: sfu.SubReasonDecoderFailed},
			status("s_a", off, mute, high, protocol.StatusReasonCodec)},
		{"no preview layer for a thumbnail", sfu.SubscriptionStateEvent{Share: "s_a", Requested: sfu.QualityLow, Forwarded: sfu.QualityOff, Reason: sfu.SubReasonNoPreviewLayer},
			status("s_a", off, mute, low, protocol.StatusReasonUnavailable)},
		{"no layer yet", sfu.SubscriptionStateEvent{Share: "s_a", Requested: sfu.QualityHigh, Forwarded: sfu.QualityOff, Audio: true, Reason: sfu.SubReasonNoLayer},
			status("s_a", off, on, high, protocol.StatusReasonUnavailable)},
		{"a quality without a name", sfu.SubscriptionStateEvent{Share: "s_a", Requested: sfu.QualityHigh + 1, Forwarded: sfu.QualityHigh}, nil},

		// CodecPolicyEvent → a hint with the codec only.
		{"the room needs constrained baseline", sfu.CodecPolicyEvent{Share: "s_a", Profile: sfu.ProfileConstrainedBaseline},
			[]signaltest.Event{{Method: "QualityHint", Arg: protocol.QualityHint{ShareID: "s_a", Reason: protocol.HintReasonCodec, Codec: "h264/42e0"}}}},
		{"the room can take high again", sfu.CodecPolicyEvent{Share: "s_b", Profile: sfu.ProfileHigh},
			[]signaltest.Event{{Method: "QualityHint", Arg: protocol.QualityHint{ShareID: "s_b", Reason: protocol.HintReasonCodec, Codec: "h264/6400"}}}},
		{"a codec policy without a profile", sfu.CodecPolicyEvent{Share: "s_a"}, nil},
		{"a codec policy with a malformed profile", sfu.CodecPolicyEvent{Share: "s_a", Profile: "high"}, nil},

		// QualityHintEvent → a hint with the SFU's reason and its full encodings, one to one.
		{"nobody watches high", sfu.QualityHintEvent{Share: "s_a", Reason: "viewers", Encodings: encodings},
			[]signaltest.Event{{Method: "QualityHint", Arg: protocol.QualityHint{ShareID: "s_a", Reason: protocol.HintReasonViewers, Encodings: wireEncs}}}},
		{"the admin's cap", sfu.QualityHintEvent{Share: "s_a", Reason: "admin", MaxBitrate: 2_500_000, Encodings: encodings},
			[]signaltest.Event{{Method: "QualityHint", Arg: protocol.QualityHint{ShareID: "s_a", Reason: protocol.HintReasonAdmin, Encodings: wireEncs, MaxBitrate: 2_500_000}}}},

		// PCStateEvent: only a failed pub PC makes the server act.
		{"the pub PC failed", sfu.PCStateEvent{PC: sfu.PCPub, Gen: 3, State: "failed"}, restart},
		{"a new pub PC missed its handshake", sfu.PCStateEvent{PC: sfu.PCPub, Gen: 3, State: "failed", Reason: "handshake_timeout"}, restart},
		{"the pub PC connected", sfu.PCStateEvent{PC: sfu.PCPub, Gen: 3, State: "connected"}, nil},
		{"the pub PC is disconnected", sfu.PCStateEvent{PC: sfu.PCPub, Gen: 3, State: "disconnected"}, nil},
		{"the pub PC closed", sfu.PCStateEvent{PC: sfu.PCPub, Gen: 3, State: "closed", Reason: "pc_grace_expired"}, nil},
		{"the sub PC failed: the client drives its restarts", sfu.PCStateEvent{PC: sfu.PCSub, Gen: 2, State: "failed"}, nil},
		{"the sub PC connected", sfu.PCStateEvent{PC: sfu.PCSub, Gen: 2, State: "connected"}, nil},
		{"the sub PC is disconnected", sfu.PCStateEvent{PC: sfu.PCSub, Gen: 2, State: "disconnected"}, nil},
		{"the sub PC closed", sfu.PCStateEvent{PC: sfu.PCSub, Gen: 2, State: "closed"}, nil},

		// ErrorEvent → Error, with the event's scope.
		{"a sub offer that couldn't be created", sfu.ErrorEvent{Err: sfuErr(sfu.CodeInternal), Scope: sfu.ScopePCSub},
			wireErr(protocol.ErrorCodeInternal, protocol.ErrorScopePC, func(e *protocol.Error) {
				e.PC, e.Params = protocol.PCKindSub, map[string]any{"ref": "ref"}
			})},
		{"an error about the pub PC", sfu.ErrorEvent{Err: sfuErr(sfu.CodeBadSDP), Scope: sfu.ScopePCPub},
			wireErr(protocol.ErrorCodeSDPInvalid, protocol.ErrorScopePC, func(e *protocol.Error) { e.PC = protocol.PCKindPub })},
		{"too many PCs", sfu.ErrorEvent{Err: sfuErr(sfu.CodePCRateLimited), Scope: sfu.ScopePCSub},
			wireErr(protocol.ErrorCodeRateLimited, protocol.ErrorScopePC, func(e *protocol.Error) {
				e.PC, e.RetryAfterMs = protocol.PCKindSub, 6500
			})},
		{"a share without H.264", sfu.ErrorEvent{Err: &sfu.Error{Code: sfu.CodeNoH264, Share: "s_a"}, Scope: sfu.ScopeShare},
			wireErr(protocol.ErrorCodeCodecNotSupported, protocol.ErrorScopeShare, func(e *protocol.Error) { e.ShareID = "s_a" })},
		{"a share without H.264, reported about its PC", sfu.ErrorEvent{Err: &sfu.Error{Code: sfu.CodeNoH264, Share: "s_a"}, Scope: sfu.ScopePCPub},
			wireErr(protocol.ErrorCodeCodecNotSupported, protocol.ErrorScopeShare, func(e *protocol.Error) { e.ShareID = "s_a" })},
		{"an error about a subscription", sfu.ErrorEvent{Err: &sfu.Error{Code: sfu.CodeShareNotFound, Share: "s_a"}, Scope: sfu.ScopeSubscription},
			wireErr(protocol.ErrorCodeShareNotFound, protocol.ErrorScopeSubscription, func(e *protocol.Error) { e.ShareID = "s_a" })},
		{"an internal error about a share", sfu.ErrorEvent{Err: &sfu.Error{Code: sfu.CodeInternal, Share: "s_a"}, Scope: sfu.ScopeShare},
			wireErr(protocol.ErrorCodeInternal, protocol.ErrorScopeShare, func(e *protocol.Error) {
				e.ShareID, e.Retryable, e.Params = "s_a", false, map[string]any{"ref": "ref"}
			})},
		{"the Conn closed meanwhile", sfu.ErrorEvent{Err: sfuErr(sfu.CodeClosed), Scope: sfu.ScopePCSub}, nil},
		{"a stale answer", sfu.ErrorEvent{Err: sfuErr(sfu.CodeStaleAnswer), Scope: sfu.ScopePCSub}, nil},
		{"an error event without an error", sfu.ErrorEvent{Scope: sfu.ScopePCSub}, nil},
		{"an error event with an unknown scope", sfu.ErrorEvent{Err: sfuErr(sfu.CodeBadSDP), Scope: "room"}, nil},
	}

	// Every event type the SFU declares has rows above.
	covered := map[string]bool{}
	for _, tc := range tests {
		covered[reflect.TypeOf(tc.ev).Name()] = true
	}
	declared := 0
	for _, decl := range sfuFile(t, "events.go").Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "isEvent" || fd.Recv == nil || len(fd.Recv.List) != 1 {
			continue
		}
		id, ok := fd.Recv.List[0].Type.(*ast.Ident)
		if !ok {
			t.Fatalf("the SFU's isEvent has a receiver that is not a plain type: %T", fd.Recv.List[0].Type)
		}
		declared++
		if !covered[id.Name] {
			t.Errorf("the SFU's event %s has no row: handle it in SendEvent and add it here", id.Name)
		}
	}
	if declared != len(covered) || declared == 0 {
		t.Errorf("the SFU declares %d event types, the table covers %d", declared, len(covered))
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tp := newTestPlane(t)
			pr := tp.newPeer("c_1", "u_1")
			pr.peer.SendEvent(tc.ev)
			got := stripRefs(t, pr.sink.Events())
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("the sink got\n %+v\nwant\n %+v", got, tc.want)
			}
			if calls := pr.conn.take(); len(calls) != 0 {
				t.Errorf("an event called the SFU back: %+v", calls)
			}
		})
	}
}

// TestEventOddities: values the SFU should never send still leave the client with something sensible, and a log
// line.
func TestEventOddities(t *testing.T) {
	tp := newTestPlane(t)
	pr := tp.newPeer("c_1", "u_1")

	// A reason this package has no row for: the status goes out as unavailable.
	pr.peer.SendEvent(sfu.SubscriptionStateEvent{Share: "s_a", Requested: sfu.QualityHigh, Forwarded: sfu.QualityLow, Reason: "moon_phase"})
	// A hint with a reason the wire doesn't know and a rid without a layer: the encodings still reach the sharer.
	pr.peer.SendEvent(sfu.QualityHintEvent{Share: "s_a", Reason: "weather", Encodings: []sfu.EncodingParams{{RID: "f", Active: true}, {RID: "x"}}})
	// An event type from nowhere.
	pr.peer.SendEvent(nil)
	want := []signaltest.Event{
		{Method: "SubscriptionStatus", Arg: []protocol.SubscriptionStatus{{
			ShareID: "s_a", Video: protocol.VideoLayerLow, Audio: protocol.AudioStateOff,
			RequestedVideo: protocol.VideoLayerHigh, Reason: protocol.StatusReasonUnavailable,
		}}},
		{Method: "QualityHint", Arg: protocol.QualityHint{
			ShareID: "s_a", Reason: "weather",
			Encodings: []protocol.Encoding{{RID: "f", Layer: protocol.VideoLayerHigh, Active: true}},
		}},
	}
	if got := pr.sink.Events(); !reflect.DeepEqual(got, want) {
		t.Errorf("the sink got\n %+v\nwant\n %+v", got, want)
	}
	if logs := atLeast(tp.logs.take(), slog.LevelWarn); len(logs) != 4 {
		t.Errorf("logged %+v, want a line for the reason, the rid, the hint's reason and the event", logs)
	}
}

// share returns a ShareInfo of connection c_pub as the SFU reports it.
func share(id sfu.ShareID, state sfu.ShareState, profile sfu.ProfileKey, audio bool, layers ...sfu.LayerInfo) sfu.ShareInfo {
	return sfu.ShareInfo{
		ID: id, Room: "lounge", Participant: "u_pub", User: "u_pub", Conn: "c_pub",
		Source: sfu.SourceScreen, State: state, Profile: profile, Layers: layers, Audio: audio,
		Viewers: []sfu.ParticipantID{"u_view"}, StartedAt: time.Now(),
	}
}

// TestShareUpdated is the RoomEvents row: a share's media facts go to the sink of the connection that publishes
// it. pending → live and stalled → live are Live, → stalled is Stalled, and a change while live is Changed.
func TestShareUpdated(t *testing.T) {
	tp := newTestPlane(t)
	pub := tp.newPeer("c_pub", "u_pub")
	other := tp.newPeer("c_other", "u_pub") // the same user's phone
	viewer := tp.newPeer("c_view", "u_view")
	f, q := sfu.LayerInfo{RID: "f", Width: 1920, Height: 1080, Active: true}, sfu.LayerInfo{RID: "q", Width: 640, Height: 360, Active: true}
	paused := sfu.LayerInfo{RID: "f", Width: 1920, Height: 1080} // nobody watches it: paused, still attached
	high, low := protocol.VideoLayerHigh, protocol.VideoLayerLow
	media := func(id string, kind signal.ShareMediaKind, codec protocol.CodecKey, audio bool, layers ...protocol.VideoLayer) signaltest.Event {
		if layers == nil {
			layers = []protocol.VideoLayer{}
		}
		return signaltest.Event{Method: "ShareMedia", Arg: signaltest.ShareMediaCall{
			ShareID: id, Event: signal.ShareMediaEvent{Kind: kind, Layers: layers, Codec: codec, Audio: audio},
		}}
	}

	steps := []struct {
		name string
		info sfu.ShareInfo
		want *signaltest.Event // nil: nothing
	}{
		{"the first keyframe", share("s_1", sfu.ShareLive, "6400", false, f),
			ptr(media("s_1", signal.ShareMediaLive, "h264/6400", false, high))},
		{"the preview layer and the audio attach", share("s_1", sfu.ShareLive, "6400", true, f, q),
			ptr(media("s_1", signal.ShareMediaChanged, "h264/6400", true, high, low))},
		{"a paused layer is still a layer", share("s_1", sfu.ShareLive, "6400", true, paused, q),
			ptr(media("s_1", signal.ShareMediaChanged, "h264/6400", true, high, low))},
		{"the profile changes", share("s_1", sfu.ShareLive, "42e0", true, f, q),
			ptr(media("s_1", signal.ShareMediaChanged, "h264/42e0", true, high, low))},
		{"the pub PC is gone", share("s_1", sfu.ShareStalled, "42e0", false),
			ptr(media("s_1", signal.ShareMediaStalled, "h264/42e0", false))},
		{"still stalled", share("s_1", sfu.ShareStalled, "42e0", false),
			ptr(media("s_1", signal.ShareMediaStalled, "h264/42e0", false))},
		{"a new pub PC's keyframe", share("s_1", sfu.ShareLive, "42e0", true, f),
			ptr(media("s_1", signal.ShareMediaLive, "h264/42e0", true, high))},
		{"its preview layer follows", share("s_1", sfu.ShareLive, "42e0", true, f, q),
			ptr(media("s_1", signal.ShareMediaChanged, "h264/42e0", true, high, low))},
		// Each share has its own state.
		{"a second share, still pending", share("s_2", sfu.SharePending, "", false, f), nil},
		{"the second share goes live, its profile not yet known", share("s_2", sfu.ShareLive, "", true, q),
			ptr(media("s_2", signal.ShareMediaLive, "", true, low))},
		{"the first share changes", share("s_1", sfu.ShareLive, "42e0", false, f, q),
			ptr(media("s_1", signal.ShareMediaChanged, "h264/42e0", false, high, low))},
		{"a state without a name", share("s_1", sfu.ShareStalled+1, "42e0", false, f), nil},
	}
	facts := &sinkReader{sink: pub.sink}
	for _, st := range steps {
		tp.events.ShareUpdated("lounge", st.info)
		got := facts.next()
		switch {
		case st.want == nil && len(got) != 0:
			t.Errorf("%s: the sink got %+v, want nothing", st.name, got)
		case st.want != nil && (len(got) != 1 || !reflect.DeepEqual(got[0], *st.want)):
			t.Errorf("%s: the sink got\n %+v\nwant\n %+v", st.name, got, *st.want)
		}
	}
	for name, pr := range map[string]*testPeer{"the user's other connection": other, "a viewer": viewer} {
		if got := pr.sink.Events(); len(got) != 0 {
			t.Errorf("%s got %+v: a share's facts go to the connection that publishes it", name, got)
		}
	}
	for _, pr := range []*testPeer{pub, other, viewer} {
		if calls := pr.conn.take(); len(calls) != 0 {
			t.Errorf("a room event called the SFU back: %+v", calls)
		}
	}
	if logs := atLeast(tp.logs.take(), slog.LevelInfo); len(logs) != 0 {
		t.Errorf("logged %+v", logs)
	}

	// ShareEnded and CodecPolicyChanged tell the hub nothing: it ends shares itself, and publishers get their codec
	// hints per share. The share's state is forgotten with it.
	tp.events.ShareEnded("lounge", share("s_1", sfu.ShareLive, "42e0", false, f, q), sfu.EndReasonStopped)
	tp.events.CodecPolicyChanged("lounge", sfu.ProfileConstrainedBaseline)
	if got := facts.next(); len(got) != 0 {
		t.Errorf("ShareEnded or CodecPolicyChanged reached the sink: %+v", got)
	}
	if _, kept := pub.peer.media["s_1"]; kept || len(pub.peer.media) != 1 {
		t.Errorf("share states after s_1 ended: %v, want only s_2", pub.peer.media)
	}

	// A malformed profile is the SFU's bug: the fact still goes out, without a codec.
	tp.events.ShareUpdated("lounge", share("s_2", sfu.ShareLive, "high", true, q))
	if got := facts.next(); len(got) != 1 || !reflect.DeepEqual(got[0], media("s_2", signal.ShareMediaChanged, "", true, low)) {
		t.Errorf("the sink got %+v", got)
	}
	if logs := atLeast(tp.logs.take(), slog.LevelError); len(logs) != 1 {
		t.Errorf("logged %+v, want one error", logs)
	}

	// Once the publishing connection's peer has closed, its shares' facts have nowhere to go.
	pub.Close()
	tp.events.ShareUpdated("lounge", share("s_2", sfu.ShareStalled, "", false))
	tp.events.ShareEnded("lounge", share("s_2", sfu.ShareStalled, "", false), sfu.EndReasonLeft)
	unknown := share("s_9", sfu.ShareLive, "6400", true, f)
	unknown.Conn = "c_nobody"
	tp.events.ShareUpdated("lounge", unknown)
	tp.events.ShareEnded("lounge", unknown, sfu.EndReasonStopped)
	if got := facts.next(); len(got) != 0 {
		t.Errorf("a share without a peer reached its old sink: %+v", got)
	}
	for _, pr := range []*testPeer{other, viewer} {
		if got := pr.sink.Events(); len(got) != 0 {
			t.Errorf("a share without a peer reached a sink: %+v", got)
		}
	}
}

// TestShareUpdatedAfterRejoin: a connection that joins another room gets a new peer, and its shares there report to
// the new sink from a clean state.
func TestShareUpdatedAfterRejoin(t *testing.T) {
	tp := newTestPlane(t)
	first := tp.newPeer("c_pub", "u_pub")
	tp.events.ShareUpdated("lounge", share("s_1", sfu.ShareLive, "6400", false))
	first.Close()
	second := tp.newPeer("c_pub", "u_pub")
	tp.events.ShareUpdated("movies", share("s_2", sfu.ShareLive, "6400", false))
	if got := first.sink.Events(); len(got) != 1 {
		t.Errorf("the first peer's sink got %+v, want only its own share", got)
	}
	got := second.sink.Events()
	if len(got) != 1 || got[0].Arg.(signaltest.ShareMediaCall).ShareID != "s_2" ||
		got[0].Arg.(signaltest.ShareMediaCall).Event.Kind != signal.ShareMediaLive {
		t.Errorf("the second peer's sink got %+v, want s_2 live", got)
	}
}

// TestConcurrentEvents: the SFU's ticker, the Conns' actors and the hub's actors all reach the Plane at once. Under
// the race detector this checks the Plane's one lock, and that one share's reports stay in order: they come from one
// goroutine at a time (the SFU's per-share report lock), here one goroutine per share.
func TestConcurrentEvents(t *testing.T) {
	tp := newTestPlane(t)
	const conns, rounds = 8, 200
	var wg sync.WaitGroup
	sinks := make([]*signaltest.Sink, conns)
	for i := range conns {
		connID := "c_" + string(rune('a'+i))
		shareID := sfu.ShareID("s_" + string(rune('a'+i)))
		pr := tp.newPeer(connID, "u_"+string(rune('a'+i)))
		sinks[i] = pr.sink
		// The SFU's side of one share: live, then changes, a stall and a recovery, over and over.
		wg.Go(func() {
			info := share(shareID, sfu.ShareLive, "6400", true, sfu.LayerInfo{RID: "f"})
			info.Conn = sfu.ConnID(connID)
			for range rounds {
				for _, st := range []sfu.ShareState{sfu.ShareLive, sfu.ShareLive, sfu.ShareStalled} {
					info.State = st
					tp.events.ShareUpdated("lounge", info)
				}
			}
			tp.events.ShareEnded("lounge", info, sfu.EndReasonStopped)
		})
		// The Conn's actor.
		wg.Go(func() {
			for n := range rounds {
				pr.peer.SendEvent(sfu.CodecPolicyEvent{Share: shareID, Profile: sfu.ProfileHigh})
				pr.peer.SendOffer(sfu.PCSub, 1, uint32(n+1), "v=0\r\n", nil)
			}
		})
		// The hub's actor for this connection, and others joining and leaving meanwhile.
		wg.Go(func() {
			for range rounds {
				pr.Stats()
				tmp, err := tp.plane.NewPeer(signal.PeerParams{
					ConnectionID: connID + "_tmp", UserID: "u_tmp", RoomID: "lounge", Role: protocol.RoleViewer,
				}, &signaltest.Sink{})
				if err != nil {
					t.Errorf("NewPeer: %v", err)
					return
				}
				tmp.Close()
			}
		})
	}
	wg.Wait()

	wantKinds := make([]signal.ShareMediaKind, 0, 3*rounds)
	for range rounds {
		wantKinds = append(wantKinds, signal.ShareMediaLive, signal.ShareMediaChanged, signal.ShareMediaStalled)
	}
	for i, sink := range sinks {
		var kinds []signal.ShareMediaKind
		for _, ev := range sink.Events() {
			if c, ok := ev.Arg.(signaltest.ShareMediaCall); ok {
				kinds = append(kinds, c.Event.Kind)
			}
		}
		if !slices.Equal(kinds, wantKinds) {
			t.Errorf("connection %d: %d share facts, not live, changed, stalled in turn", i, len(kinds))
		}
	}
	if n := len(tp.plane.peers); n != conns {
		t.Errorf("%d peers left, want %d", n, conns)
	}
}
