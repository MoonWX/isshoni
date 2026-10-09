package sfuplane

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/sfu"
	"github.com/MoonWX/isshoni/internal/server/signal"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

// TestWithHub puts the three parts together the way the wiring will (04 §6.6): a real hub whose MediaPlane is the
// Plane, bound to a real SFU. One client shares and one watches, each over a WebSocket (on an in-memory network) and
// a Pion PeerConnection on loopback. It is the adapter's own check that both sides take what it gives them: the
// hub's replies and room.state carry the SFU's ShareParams and media facts, the SFU's sub offer reaches the viewer,
// and an SFU error ends up as the wire error of 01 §15.4, acted on by the hub. The media and recovery cases of
// 01 §19 are internal/server/itest's (README S59, S74).
//
// It also runs the SFU's callbacks against the hub's real MediaSink: the SFU reports a share's state with that
// share's report lock held, so a sink that called back into the SFU would stop this test.
func TestWithHub(t *testing.T) {
	rp := newRealPlane(t)
	const origin = "https://watch.example.com"
	auth := signaltest.NewAuth()
	cfg := signal.DefaultConfig()
	cfg.PublicOrigin, cfg.ServerVersion, cfg.ResumeKey = origin, "0.1.0", bytes.Repeat([]byte{7}, 32)
	hub, err := signal.New(cfg, signal.Deps{
		Auth:     auth,
		Rooms:    signaltest.NewRooms(),
		Media:    rp.plane,
		Policy:   (&signaltest.Policy{}).Get,
		ClientIP: signaltest.ClientIP,
		Log:      slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("signal.New: %v", err)
	}
	srv := signaltest.StartServer(hub)
	// The server's shutdown order: the hub first, then (newRealPlane's cleanups) the SFU and its Transport.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := hub.Shutdown(ctx, protocol.ShutdownReasonStop); err != nil {
			t.Errorf("Hub.Shutdown: %v", err)
		}
		srv.Close()
	})

	connect := func(user string, role protocol.Role) *wsClient {
		t.Helper()
		auth.AddSession("cookie-"+user, signal.Identity{UserID: user, Name: user, SessionID: "session-" + user})
		h := signaltest.CookieHeader("cookie-" + user)
		h.Set("Origin", origin)
		c, err := signaltest.Dial(testCtx(t), srv.Net.HTTPClient(), srv.URL("/ws"), signaltest.DialOptions{Header: h})
		if err != nil {
			t.Fatalf("dial as %s: %v", user, err)
		}
		t.Cleanup(c.Close)
		hello := signaltest.DefaultHello()
		hello.Role = role
		welcome, err := c.Hello(testCtx(t), hello)
		if err != nil {
			t.Fatalf("hello as %s: %v", user, err)
		}
		w := &wsClient{t: t, c: c, name: user}
		reply[protocol.RoomJoinResult](w, protocol.MessageTypeRoomJoin, protocol.RoomJoin{RoomID: welcome.DefaultRoomID})
		return w
	}
	alice := connect("alice", protocol.RoleFull)
	bob := connect("bob", protocol.RoleViewer)

	// share.start: the reply is the SFU's ShareParams under the hub's share id.
	params := reply[protocol.ShareParams](alice, protocol.MessageTypeShareStart, protocol.ShareStart{
		Kind: protocol.ShareKindScreen, Preset: protocol.PresetAuto, Audio: true, Ref: "r1",
	})
	shareID := params.ShareID
	if info, ok := rp.sfu.Share(sfu.ShareID(shareID)); !ok || shareID == "" || params.Codec != protocol.CodecH264High ||
		len(params.Encodings) != 2 || params.AudioBitrate <= 0 || info.User != "alice" {
		t.Fatalf("share.start = %+v; the SFU's share is %+v (%v)", params, info, ok)
	}

	// pc.offer on pub: the SFU's answer comes back as pc.answer.
	publisher := newPublisher(t)
	offer := publisherOffer(t, publisher, shareID)
	alice.send(protocol.MessageTypePCOffer, offer)
	answer := decode[protocol.PCAnswer](alice, alice.wait("the pub answer", ofType(protocol.MessageTypePCAnswer)))
	if answer.PC != protocol.PCKindPub || answer.Gen != 1 || answer.Neg != 1 {
		t.Fatalf("pc.answer = %+v", answer)
	}
	if err := publisher.SetAnswer(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP.Reveal()}); err != nil {
		t.Fatalf("the publisher refused the answer: %v", err)
	}

	// subscribe.update: the SFU's sub offer reaches the viewer with its tracks, and takes the viewer's answer.
	subscribed := reply[protocol.SubscribeResult](bob, protocol.MessageTypeSubscribeUpdate, protocol.SubscribeUpdate{
		Subs: []protocol.SubscriptionWant{{ShareID: shareID, Video: protocol.VideoLayerHigh, Audio: protocol.AudioStateOn}},
	})
	if len(subscribed.Ignored) != 0 {
		t.Errorf("subscribe.update ignored %v", subscribed.Ignored)
	}
	subOffer := decode[protocol.PCOffer](bob, bob.wait("the sub offer", ofType(protocol.MessageTypePCOffer)))
	if subOffer.PC != protocol.PCKindSub || len(subOffer.Tracks) != 2 || subOffer.Tracks[0].ShareID != shareID {
		t.Fatalf("the sub offer = %+v, want the share's two tracks", subOffer)
	}
	viewer := newViewer(t)
	bob.send(protocol.MessageTypePCAnswer, viewerAnswer(t, viewer, subOffer))

	// Media: the SFU's ShareUpdated becomes the hub's live share, with the layers, the codec and the audio it reports.
	if err := publisher.Start(testCtx(t)); err != nil {
		t.Fatal(err)
	}
	bothLayers := []protocol.VideoLayer{protocol.VideoLayerHigh, protocol.VideoLayerLow}
	live := func(env protocol.Envelope) bool {
		if env.Type != protocol.MessageTypeRoomState {
			return false
		}
		st := decode[protocol.RoomState](bob, env)
		return len(st.Shares) == 1 && st.Shares[0].ID == shareID && st.Shares[0].Status == protocol.ShareStatusLive &&
			slices.Equal(st.Shares[0].Layers, bothLayers) && st.Shares[0].Codec == protocol.CodecH264High && st.Shares[0].Audio
	}
	bob.wait("room.state with the share live, both layers, its codec and audio", live)
	waitMedia(t, viewer)

	// An SFU error on a pc.* notification: sfu.bad_sdp is sdp_invalid with scope pc and the offer's counters.
	garbage := offer
	garbage.Neg, garbage.SDP = 2, "this is not SDP"
	alice.send(protocol.MessageTypePCOffer, garbage)
	sdpErr := decode[protocol.Error](alice, alice.wait("the error for the bad offer", ofType(protocol.MessageTypeError)))
	want := protocol.NewError(protocol.ErrorCodeSDPInvalid, protocol.ErrorScopePC)
	want.PC, want.Gen, want.Neg = protocol.PCKindPub, 1, 2
	if sdpErr.Code != want.Code || sdpErr.Scope != want.Scope || sdpErr.PC != want.PC || sdpErr.Gen != want.Gen ||
		sdpErr.Neg != want.Neg || sdpErr.Retryable != want.Retryable {
		t.Errorf("the bad offer's error = %+v, want %+v", sdpErr, want)
	}

	// The one error that names a share: an offer whose video has no H.264 (sfu.no_h264) is codec_not_supported with
	// scope share, and the hub ends that share.
	noH264 := offer
	noH264.Neg = 2
	noH264.SDP = protocol.SDP(strings.ReplaceAll(offer.SDP.Reveal(), "H264/90000", "VP8/90000"))
	if noH264.SDP == offer.SDP {
		t.Fatal("the publisher's offer has no H.264 to take out")
	}
	alice.send(protocol.MessageTypePCOffer, noH264)
	codecErr := decode[protocol.Error](alice, alice.wait("the error for the offer without H.264", ofType(protocol.MessageTypeError)))
	if codecErr.Code != protocol.ErrorCodeCodecNotSupported || codecErr.Scope != protocol.ErrorScopeShare ||
		codecErr.ShareID != shareID || codecErr.PC != "" {
		t.Errorf("the error for the offer without H.264 = %+v, want codec_not_supported for share %s", codecErr, shareID)
	}
	bob.wait("room.state without the share", func(env protocol.Envelope) bool {
		return env.Type == protocol.MessageTypeRoomState && len(decode[protocol.RoomState](bob, env).Shares) == 0
	})
	eventually(t, func() bool { _, ok := rp.sfu.Share(sfu.ShareID(shareID)); return !ok },
		func() string { return "the share is still in the SFU after the hub ended it" })

	// Nothing on the way was logged as anyone's bug.
	if lines := atLeast(rp.logs.take(), slog.LevelWarn); len(lines) != 0 {
		t.Errorf("logged %+v", lines)
	}
}

// wsClient is one signaling client of TestWithHub: a raw protocol client that keeps what it reads while it waits
// for something else.
type wsClient struct {
	t     *testing.T
	c     *signaltest.Client
	name  string
	inbox []protocol.Envelope
}

func (w *wsClient) send(typ protocol.MessageType, data any) {
	w.t.Helper()
	if err := w.c.Send(typ, "", data); err != nil {
		w.t.Fatalf("%s: send %s: %v", w.name, typ, err)
	}
}

// wait returns the first message that match accepts: one read earlier, or the next one to arrive.
func (w *wsClient) wait(what string, match func(protocol.Envelope) bool) protocol.Envelope {
	w.t.Helper()
	for i, env := range w.inbox {
		if match(env) {
			w.inbox = slices.Delete(w.inbox, i, i+1)
			return env
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for {
		env, err := w.c.Recv(ctx)
		if err != nil {
			var seen []string
			for _, e := range w.inbox {
				seen = append(seen, string(e.Type)+" "+string(e.Data))
			}
			w.t.Fatalf("%s: waiting for %s: %v\nreceived meanwhile:\n%s", w.name, what, err, strings.Join(seen, "\n"))
		}
		if match(env) {
			return env
		}
		w.inbox = append(w.inbox, env)
	}
}

func ofType(typ protocol.MessageType) func(protocol.Envelope) bool {
	return func(env protocol.Envelope) bool { return env.Type == typ }
}

// decode reads a message's payload.
func decode[T any](w *wsClient, env protocol.Envelope) T {
	w.t.Helper()
	v, err := protocol.Decode[T](env)
	if err != nil {
		w.t.Fatalf("%s: decode %s %s: %v", w.name, env.Type, env.Data, err)
	}
	return v
}

// reply sends a request and returns the payload of its ok.
func reply[T any](w *wsClient, typ protocol.MessageType, data any) T {
	w.t.Helper()
	id := w.c.NextID()
	if err := w.c.Send(typ, id, data); err != nil {
		w.t.Fatalf("%s: send %s: %v", w.name, typ, err)
	}
	env := w.wait("the reply to "+string(typ), func(env protocol.Envelope) bool { return env.Re == id })
	if env.Type != protocol.MessageTypeOK {
		w.t.Fatalf("%s: %s was answered with %s %s", w.name, typ, env.Type, env.Data)
	}
	return decode[T](w, env)
}
