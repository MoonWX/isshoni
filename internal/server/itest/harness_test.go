package itest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/client/publish"
	"github.com/MoonWX/isshoni/internal/client/signal"
	"github.com/MoonWX/isshoni/internal/media/fake"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/ops"
	"github.com/MoonWX/isshoni/internal/server/servertest"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
	"github.com/MoonWX/isshoni/internal/version"
)

// The harness: one server per test, accounts on it, and clients made of the Go signaling client, the Go publisher
// and sfutest's Viewer (doc.go).

const (
	adminName = "Alex"
	password  = "correct horse battery"
	// waitTimeout bounds every wait of a test. What a test waits for takes milliseconds on loopback; the bound only
	// keeps a broken server from hanging the run, on a slow CI machine under the race detector too.
	waitTimeout = 30 * time.Second
)

// world is one test's server with its accounts.
type world struct {
	t       *testing.T
	srv     *servertest.Server
	invite  string // the token of an invite for every member of the test
	members int
}

// newWorld starts a server and creates the admin the way an operator does: a setup link from the admin socket,
// then the setup form. It also takes an invite from the admin socket, which the test's members register with.
func newWorld(t *testing.T, opts servertest.Options) *world {
	t.Helper()
	if opts.Deps.SPA == nil {
		opts.Deps.SPA = testSPA()
	}
	w := &world{t: t, srv: servertest.Start(t, opts)}
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	admin := ops.DialAdmin(w.srv.AdminSocket)
	link, err := admin.SetupURL(ctx, 0)
	if err != nil {
		t.Fatalf("setup-url: %v", err)
	}
	token, ok := strings.CutPrefix(link.URL, w.srv.URL+"/setup#")
	if !ok || token == "" {
		t.Fatalf("setup link %q, want %s/setup#<token>", link.URL, w.srv.URL)
	}
	w.post(w.srv.Client, "", "/api/v1/auth/setup/complete",
		api.SetupCompleteRequest{Token: token, Username: adminName, Password: password}, http.StatusCreated)
	invite, err := admin.CreateInvite(ctx, 100, time.Hour)
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	if w.invite, ok = strings.CutPrefix(invite.URL, w.srv.URL+"/invite#"); !ok || w.invite == "" {
		t.Fatalf("invite link %q, want %s/invite#<token>", invite.URL, w.srv.URL)
	}
	return w
}

// testSPA is the web app of the harness's servers: one page, built for this binary's version. Without it a server
// serves the web app embedded in the test binary, which is whatever the working tree's web/dist holds: nothing on a
// fresh clone, and after `task build` or `task e2e` a build stamped with another version than a test binary has,
// which the server says with a warning at startup. No test here reads a page, and the ones that read the server's
// log expect no warning of the server's own.
func testSPA() fstest.MapFS {
	return fstest.MapFS{
		"index.html":   &fstest.MapFile{Data: []byte("<!doctype html><title>isshoni</title><div id=root></div>\n")},
		"version.json": &fstest.MapFile{Data: []byte(`{"version":"` + version.Version() + `"}`)},
	}
}

// post sends a JSON request as the web app does, with the site's Origin, and fails the test unless the answer has
// the wanted status. forwardedFor, when set, is the client address that the test passes on as the operator's
// proxy in front of an off-mode server would (04 §8.5). A 429 is retried after its Retry-After: the accounts of a
// large test come faster than the server lets strangers sign up.
func (w *world) post(hc *http.Client, forwardedFor, path string, body any, want int) {
	w.t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		w.t.Fatal(err)
	}
	deadline := time.Now().Add(waitTimeout)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.srv.URL+path, bytes.NewReader(payload))
		if err != nil {
			cancel()
			w.t.Fatal(err)
		}
		req.Header.Set("Origin", w.srv.URL)
		req.Header.Set("Content-Type", "application/json")
		if forwardedFor != "" {
			req.Header.Set("X-Forwarded-For", forwardedFor)
		}
		res, err := hc.Do(req)
		if err != nil {
			cancel()
			w.t.Fatalf("POST %s: %v", path, err)
		}
		answer, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))
		_ = res.Body.Close()
		cancel()
		if res.StatusCode == want {
			return
		}
		wait, _ := strconv.Atoi(res.Header.Get("Retry-After"))
		if res.StatusCode != http.StatusTooManyRequests || wait <= 0 || time.Now().Add(time.Duration(wait)*time.Second).After(deadline) {
			w.t.Fatalf("POST %s = %d %s, want %d", path, res.StatusCode, answer, want)
		}
		time.Sleep(time.Duration(wait) * time.Second)
	}
}

// member is one account with its browser: an HTTP client whose jar holds the session cookie.
type member struct {
	name string
	http *http.Client
}

// member registers an account with the test's invite. Each gets a cookie jar of its own on the harness's
// transport, and, where the server takes a proxy's word for it (off mode), a client address of its own, so that
// the sign-ups of one test don't count as one stranger's.
func (w *world) member(name string) *member {
	w.t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		w.t.Fatal(err)
	}
	m := &member{name: name, http: &http.Client{Jar: jar, Transport: w.srv.Client.Transport}}
	w.members++
	forwardedFor := ""
	if w.srv.Roots == nil { // off mode: the test plays the operator's proxy
		forwardedFor = fmt.Sprintf("198.51.100.%d", w.members)
	}
	w.post(m.http, forwardedFor, "/api/v1/auth/register",
		api.RegisterRequest{InviteToken: w.invite, Username: name, Password: password}, http.StatusCreated)
	return m
}

// caps are the capabilities of the Go clients: what publish.Publisher sends and sfutest.Viewer takes.
var caps = protocol.Caps{
	Decode:         []protocol.CodecKey{protocol.CodecH264ConstrainedBaseline, protocol.CodecH264High, protocol.CodecOpus},
	Encode:         []protocol.CodecKey{protocol.CodecH264ConstrainedBaseline, protocol.CodecH264High, protocol.CodecOpus},
	Simulcast:      true,
	DisplayCapture: true,
}

// client is one connection of a member in the default room: the Go signaling client, with a goroutine that
// receives its notifications for as long as it lives, and the PeerConnections the test gives it.
type client struct {
	t    *testing.T
	name string
	sig  *signal.Client
	// UserID and ConnectionID are the welcome's.
	userID, connectionID string

	pumped sync.WaitGroup // the goroutine that receives sig.Events

	mu      sync.Mutex // guards the fields below
	room    protocol.RoomState
	events  []protocol.RoomEvent
	status  map[string][]protocol.SubscriptionStatus // by share, in the order they came
	answers []protocol.PCAnswer                      // to this client's pub offers
	offers  []protocol.PCOffer                       // the server's sub offers
	errs    []protocol.Error                         // error notifications
	goodbye []protocol.ServerShutdown                // server.shutdown notices
	failed  []error                                  // what the pump ran into
	viewer  *sfutest.Viewer                          // answers the sub offers; nil until watch
}

// connect opens a signaling connection for m with the given role and joins the default room. The client is closed
// when the test ends, before the server stops.
func (w *world) connect(m *member, role protocol.Role) *client {
	w.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	sig, err := signal.Dial(ctx, signal.Options{
		URL:        w.srv.WSURL,
		HTTPClient: m.http, // its jar has the session cookie, and its transport reaches the test server
		Client:     protocol.ClientInfo{Kind: protocol.ClientKindTool, Version: "0.1.0", OS: protocol.ClientOSLinux},
		Role:       role,
		Caps:       caps,
	})
	if err != nil {
		w.t.Fatalf("%s: dial %s: %v", m.name, w.srv.WSURL, err)
	}
	welcome := sig.Welcome()
	c := &client{
		t: w.t, name: m.name, sig: sig, userID: welcome.User.ID, connectionID: welcome.ConnectionID,
		status: map[string][]protocol.SubscriptionStatus{},
	}
	// A signaling client whose Events nobody receives stops reading its socket (01 §15.3): the pump runs until Close
	// has closed the channel.
	c.pumped.Go(c.pump)
	w.t.Cleanup(c.close)
	if welcome.Resumed || welcome.User.Name != m.name {
		w.t.Fatalf("%s: welcome = %+v", m.name, welcome)
	}
	var joined protocol.RoomJoinResult
	c.request(protocol.MessageTypeRoomJoin, protocol.RoomJoin{RoomID: welcome.DefaultRoomID}, &joined)
	if joined.Room.ID != welcome.DefaultRoomID {
		w.t.Fatalf("%s: joined %+v, want the default room %s", m.name, joined.Room, welcome.DefaultRoomID)
	}
	return c
}

// close ends the connection with a normal close, so the server ends it at once, and waits for the pump.
func (c *client) close() {
	if err := c.sig.Close(); err != nil {
		c.t.Logf("%s: closing the signaling client: %v", c.name, err)
	}
	c.pumped.Wait()
}

// pump receives the client's notifications in order until the client has stopped. It keeps what the tests look at
// and answers the server's sub offers with the client's Viewer, as a browser's sub PeerConnection would (01 §9).
func (c *client) pump() {
	for env := range c.sig.Events() {
		if err := c.handle(env); err != nil {
			c.mu.Lock()
			c.failed = append(c.failed, fmt.Errorf("%s: %w", env.Type, err))
			c.mu.Unlock()
		}
	}
}

func (c *client) handle(env protocol.Envelope) error {
	switch env.Type {
	case protocol.MessageTypeRoomState:
		st, err := protocol.Decode[protocol.RoomState](env)
		if err != nil {
			return err
		}
		c.mu.Lock()
		c.room = st
		c.mu.Unlock()
	case protocol.MessageTypeRoomEvent:
		ev, err := protocol.Decode[protocol.RoomEvent](env)
		if err != nil {
			return err
		}
		c.mu.Lock()
		c.events = append(c.events, ev)
		c.mu.Unlock()
	case protocol.MessageTypeSubscribeStatus:
		st, err := protocol.Decode[protocol.SubscribeStatus](env)
		if err != nil {
			return err
		}
		c.mu.Lock()
		for _, s := range st.Subs {
			c.status[s.ShareID] = append(c.status[s.ShareID], s)
		}
		c.mu.Unlock()
	case protocol.MessageTypePCAnswer:
		a, err := protocol.Decode[protocol.PCAnswer](env)
		if err != nil {
			return err
		}
		c.mu.Lock()
		c.answers = append(c.answers, a)
		c.mu.Unlock()
	case protocol.MessageTypePCOffer:
		o, err := protocol.Decode[protocol.PCOffer](env)
		if err != nil {
			return err
		}
		return c.answerSubOffer(o)
	case protocol.MessageTypeError:
		e, err := protocol.Decode[protocol.Error](env)
		if err != nil {
			return err
		}
		c.mu.Lock()
		c.errs = append(c.errs, e)
		c.mu.Unlock()
	case protocol.MessageTypeServerShutdown:
		bye, err := protocol.Decode[protocol.ServerShutdown](env)
		if err != nil {
			return err
		}
		c.mu.Lock()
		c.goodbye = append(c.goodbye, bye)
		c.mu.Unlock()
	}
	return nil
}

// answerSubOffer applies a sub offer on the Viewer and sends its answer with the offer's gen and neg. The Viewer's
// answer carries its candidates, so the client sends no pc.ice.
func (c *client) answerSubOffer(o protocol.PCOffer) error {
	c.mu.Lock()
	c.offers = append(c.offers, o)
	viewer := c.viewer
	c.mu.Unlock()
	if o.PC != protocol.PCKindSub {
		return fmt.Errorf("an offer for the %s PC: the server offers on sub only", o.PC)
	}
	if viewer == nil {
		return errors.New("a sub offer, and the client has no Viewer")
	}
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	answer, err := viewer.Answer(ctx, webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: o.SDP.Reveal()})
	if err != nil {
		return err
	}
	return c.sig.Send(protocol.MessageTypePCAnswer, protocol.PCAnswer{PC: o.PC, Gen: o.Gen, Neg: o.Neg, SDP: protocol.SDP(answer.SDP)})
}

// request sends a request and decodes its reply into result; any error fails the test.
func (c *client) request(typ protocol.MessageType, data, result any) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	if err := c.sig.Request(ctx, typ, data, result); err != nil {
		c.t.Fatalf("%s: %s: %v", c.name, typ, err)
	}
}

// watch gives the client its sub PeerConnection: a Viewer with the given Pion settings, which answers the
// server's sub offers from now on. keep bounds the packets each of its Recorders keeps (0: the Viewer's default).
func (c *client) watch(se webrtc.SettingEngine, keep int) *sfutest.Viewer {
	c.t.Helper()
	v, err := sfutest.NewViewer(sfutest.ViewerOptions{Settings: se, KeepPackets: keep})
	if err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() {
		if err := v.Close(); err != nil {
			c.t.Errorf("%s: Viewer.Close: %v", c.name, err)
		}
	})
	c.mu.Lock()
	c.viewer = v
	c.mu.Unlock()
	return v
}

// subscribe sends the client's subscriptions in one subscribe.update, which the SFU applies in one step. A share
// the server no longer knows fails the test.
func (c *client) subscribe(wants ...protocol.SubscriptionWant) {
	c.t.Helper()
	var res protocol.SubscribeResult
	c.request(protocol.MessageTypeSubscribeUpdate, protocol.SubscribeUpdate{Subs: wants}, &res)
	if len(res.Ignored) != 0 {
		c.t.Fatalf("%s: subscribe.update ignored %v", c.name, res.Ignored)
	}
}

// want is one subscription: the full layer with audio for the share in focus, the preview without audio for a
// thumbnail, as the web client asks.
func want(shareID string, focused bool) protocol.SubscriptionWant {
	if focused {
		return protocol.SubscriptionWant{ShareID: shareID, Video: protocol.VideoLayerHigh, Audio: protocol.AudioStateOn}
	}
	return protocol.SubscriptionWant{ShareID: shareID, Video: protocol.VideoLayerLow, Audio: protocol.AudioStateOff}
}

// roomState returns the newest room.state.
func (c *client) roomState() protocol.RoomState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.room
}

// shareIn returns a share of the newest room.state.
func (c *client) shareIn(shareID string) (protocol.ShareInfo, bool) {
	for _, s := range c.roomState().Shares {
		if s.ID == shareID {
			return s, true
		}
	}
	return protocol.ShareInfo{}, false
}

// lastStatus returns the newest subscribe.status entry about a share.
func (c *client) lastStatus(shareID string) (protocol.SubscriptionStatus, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.status[shareID]
	if len(st) == 0 {
		return protocol.SubscriptionStatus{}, false
	}
	return st[len(st)-1], true
}

// statuses returns every subscribe.status entry about a share, in order.
func (c *client) statuses(shareID string) []protocol.SubscriptionStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.status[shareID])
}

// subOffers returns the server's sub offers so far.
func (c *client) subOffers() []protocol.PCOffer {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.offers)
}

// roomEvents returns the room.event notifications so far.
func (c *client) roomEvents() []protocol.RoomEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.events)
}

// waitStatus waits until the newest subscribe.status about a share says that the client gets what it asked for
// with w: that video layer, that audio, and no reason why not.
func (c *client) waitStatus(w protocol.SubscriptionWant) {
	c.t.Helper()
	eventually(c.t, func() bool {
		st, ok := c.lastStatus(w.ShareID)
		return ok && st.Video == w.Video && st.RequestedVideo == w.Video && st.Audio == w.Audio && st.Reason == ""
	}, func() string {
		return fmt.Sprintf("%s: the statuses of %s are %+v, want the last to be video %s, audio %s", c.name, w.ShareID,
			c.statuses(w.ShareID), w.Video, w.Audio)
	})
}

// check fails the test when the client was sent an error notification, or its pump could not handle a message:
// nothing in these tests is supposed to go wrong on the way.
func (c *client) check() {
	c.t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.errs {
		c.t.Errorf("%s was sent error %s (scope %s, share %q, pc %q)", c.name, e.Code, e.Scope, e.ShareID, e.PC)
	}
	for _, err := range c.failed {
		c.t.Errorf("%s: %v", c.name, err)
	}
}

// ---- publishing ----

// share is one share of a client: the hub's id and parameters, and the Publisher that sends its media.
type share struct {
	id     string
	params protocol.ShareParams
	pub    *publish.Publisher
}

// fakeLayers are small fake video layers, light enough for a 10×10 test on any CI machine: the full layer "f" and
// the preview "q".
func fakeLayers() []fake.VideoLayer {
	return []fake.VideoLayer{
		{RID: "f", Width: 640, Height: 360, FPS: 30, Bitrate: 400_000},
		{RID: "q", Width: 320, Height: 180, FPS: 15, Bitrate: 100_000},
	}
}

// publish starts a share and publishes fake media on it, the way a browser does (01 §11.2): share.start, a pub
// offer whose tracks name the share, the server's answer, then media. The video is simulcast f and q with audio;
// its GOP is far longer than a test, so every keyframe after a layer's first is one that somebody asked for. se
// are the Pion settings of the pub PeerConnection.
func (c *client) publish(seed uint64, se webrtc.SettingEngine) *share {
	c.t.Helper()
	sh := &share{}
	c.request(protocol.MessageTypeShareStart, protocol.ShareStart{
		Kind: protocol.ShareKindScreen, Preset: protocol.PresetAuto, Audio: true, Ref: "r" + strconv.FormatUint(seed, 10),
	}, &sh.params)
	sh.id = sh.params.ShareID
	if sh.id == "" || sh.params.Codec != protocol.CodecH264High || len(sh.params.Encodings) != 2 || sh.params.AudioBitrate <= 0 {
		c.t.Fatalf("%s: share.start = %+v, want an id, High profile, two encodings and an audio bitrate", c.name, sh.params)
	}

	src, err := fake.New(fake.Config{Layers: fakeLayers(), GOP: time.Minute, Audio: true, Seed: seed})
	if err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() { _ = src.Close() })
	sh.pub, err = publish.New(publish.Options{Source: src, Settings: se, Audio: true})
	if err != nil {
		c.t.Fatal(err)
	}
	// The Publisher sends until the test ends: its context has no deadline of its own.
	ctx, cancel := context.WithCancel(context.Background())
	c.t.Cleanup(func() {
		cancel()
		if err := sh.pub.Close(); err != nil {
			c.t.Errorf("%s: Publisher.Close: %v", c.name, err)
		}
	})

	offerCtx, cancelOffer := context.WithTimeout(ctx, waitTimeout)
	defer cancelOffer()
	offer, err := sh.pub.Offer(offerCtx)
	if err != nil {
		c.t.Fatalf("%s: pub offer: %v", c.name, err)
	}
	// One TrackRef per m-section: the simulcast encodings share the video's mid.
	var tracks []protocol.TrackRef
	for _, tr := range sh.pub.Tracks() {
		ref := protocol.TrackRef{MID: tr.MID, ShareID: sh.id, Kind: protocol.TrackKindVideo}
		if tr.Kind == webrtc.RTPCodecTypeAudio {
			ref.Kind = protocol.TrackKindAudio
		}
		if !slices.Contains(tracks, ref) {
			tracks = append(tracks, ref)
		}
	}
	if err := c.sig.Send(protocol.MessageTypePCOffer, protocol.PCOffer{
		PC: protocol.PCKindPub, Gen: 1, Neg: 1, SDP: protocol.SDP(offer.SDP), Tracks: tracks,
	}); err != nil {
		c.t.Fatalf("%s: send the pub offer: %v", c.name, err)
	}
	var answer protocol.PCAnswer
	eventually(c.t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, a := range c.answers {
			if a.PC == protocol.PCKindPub && a.Gen == 1 && a.Neg == 1 {
				answer = a
				return true
			}
		}
		return false
	}, func() string {
		c.mu.Lock()
		defer c.mu.Unlock()
		return fmt.Sprintf("%s: no answer to the pub offer; errors so far: %+v", c.name, c.errs)
	})
	if err := sh.pub.SetAnswer(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP.Reveal()}); err != nil {
		c.t.Fatalf("%s: the Publisher refused the answer: %v", c.name, err)
	}
	if err := sh.pub.Start(ctx); err != nil {
		c.t.Fatal(err)
	}
	return sh
}

// waitLive waits until the client's room.state shows the share live with both layers, its codec and its audio:
// the SFU has both simulcast layers and has seen the first keyframe.
func (c *client) waitLive(shareID string) protocol.ShareInfo {
	c.t.Helper()
	both := []protocol.VideoLayer{protocol.VideoLayerHigh, protocol.VideoLayerLow}
	var info protocol.ShareInfo
	eventually(c.t, func() bool {
		var ok bool
		info, ok = c.shareIn(shareID)
		return ok && info.Status == protocol.ShareStatusLive && slices.Equal(info.Layers, both) &&
			info.Codec == protocol.CodecH264High && info.Audio
	}, func() string {
		return fmt.Sprintf("%s: share %s in room.state is %+v, want it live", c.name, shareID, info)
	})
	return info
}

// layerStats returns a Publisher's counters of one video layer.
func layerStats(t *testing.T, pub *publish.Publisher, layer string) publish.TrackStats {
	t.Helper()
	for _, ts := range pub.Stats().Tracks {
		if ts.Kind == webrtc.RTPCodecTypeVideo && ts.Layer == layer {
			return ts
		}
	}
	t.Fatalf("the publisher has no layer %q", layer)
	return publish.TrackStats{}
}

// ---- what a viewer got ----

// eventually polls cond until it holds; what says what is wrong when it never does.
func eventually(t *testing.T, cond func() bool, what func() string) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	t.Cleanup(cancel)
	return ctx
}

// track waits for the Viewer's track of a kind in a share's stream: the SFU sets the msid stream id to the share
// id (01 §9 rule 4). A Viewer learns of a track with its first packet.
func track(t *testing.T, v *sfutest.Viewer, shareID string, kind webrtc.RTPCodecType) *sfutest.Recorder {
	t.Helper()
	r, err := v.WaitRecorder(waitCtx(t), func(r *sfutest.Recorder) bool { return r.Kind() == kind && r.StreamID() == shareID })
	if err != nil {
		t.Fatalf("the viewer got no %s of %s: %v", kind, shareID, err)
	}
	return r
}

// hasTrack reports whether the Viewer ever got a packet of a kind in a share's stream.
func hasTrack(v *sfutest.Viewer, shareID string, kind webrtc.RTPCodecType) bool {
	return slices.ContainsFunc(v.Recorders(), func(r *sfutest.Recorder) bool {
		return r.Kind() == kind && r.StreamID() == shareID
	})
}

// layerFrames counts the whole frames of a layer among pkts.
func layerFrames(pkts []sfutest.Packet, rid string) int {
	n := 0
	for _, f := range sfutest.Frames(sfutest.WithoutPartialTail(pkts)) {
		if f.HasMark && f.Mark.RID == rid {
			n++
		}
	}
	return n
}

// waitFrames waits until the Recorder has n whole frames of a layer after its first skip packets.
func waitFrames(t *testing.T, r *sfutest.Recorder, skip int, rid string, n int) {
	t.Helper()
	err := r.Wait(waitCtx(t), func(r *sfutest.Recorder) bool {
		pkts := r.Packets()
		return len(pkts) >= skip && layerFrames(pkts[skip:], rid) >= n
	})
	if err != nil {
		t.Fatalf("%d frames of layer %s: %v (the viewer has %d packets, layers %q)", n, rid, err, len(r.Packets()), layerRuns(r.Packets()))
	}
}

// waitPackets waits until the Recorder has received n more packets than it had.
func waitPackets(t *testing.T, r *sfutest.Recorder, n int) {
	t.Helper()
	from := r.Stats().Packets
	if err := r.Wait(waitCtx(t), func(r *sfutest.Recorder) bool { return r.Stats().Packets >= from+n }); err != nil {
		t.Fatalf("%d more packets of the %s of %s: %v", n, r.Kind(), r.StreamID(), err)
	}
}

// checkVideo checks the video a viewer got, up to its last whole frame: one continuous stream (no sequence number
// missing or twice, timestamps that never go back) that starts on an SPS, as does every change of layer in it, of
// whole frames with consecutive frame indices within each layer. The one frame that may be short is the old
// layer's last before a switch (sfutest.WithoutCutFrames). It returns the packets it checked.
func checkVideo(t *testing.T, what string, pkts []sfutest.Packet) []sfutest.Packet {
	t.Helper()
	pkts = sfutest.WithoutPartialTail(pkts)
	for _, check := range []func([]sfutest.Packet) error{sfutest.CheckContinuous, sfutest.CheckStartsOnSPS} {
		if err := check(pkts); err != nil {
			t.Errorf("%s: %v", what, err)
		}
	}
	if err := sfutest.CheckVideoMarkers(sfutest.WithoutCutFrames(pkts)); err != nil {
		t.Errorf("%s: %v", what, err)
	}
	return pkts
}

// layerRuns returns the layers of the frames in order, each run of one layer once: "f q f".
func layerRuns(pkts []sfutest.Packet) string {
	var runs []string
	for _, f := range sfutest.Frames(pkts) {
		if f.HasMark && (len(runs) == 0 || runs[len(runs)-1] != f.Mark.RID) {
			runs = append(runs, f.Mark.RID)
		}
	}
	return strings.Join(runs, " ")
}

// ticks returns how far the RTP timestamp to is ahead of from, in clock ticks; negative when it is behind. RTP
// timestamps start at a random value and wrap at 2^32.
func ticks(from, to uint32) int64 {
	d := int64(to - from)
	if d >= 1<<31 {
		d -= 1 << 32
	}
	return d
}

// checkSwitchTimestamps checks that the viewer's timeline stays the capture timeline across layer switches:
// between the last frame of one layer and the first of the next, the RTP timestamp advances by their capture time
// difference (02 §9.4). The tolerance is what the SFU's wall-clock rule may add on a busy machine; a wrong offset
// is off by a random 32-bit number.
func checkSwitchTimestamps(t *testing.T, pkts []sfutest.Packet) {
	t.Helper()
	const tolerance = 50 * time.Millisecond
	frames := sfutest.Frames(pkts)
	for i := 1; i < len(frames); i++ {
		a, b := frames[i-1], frames[i]
		if !a.HasMark || !b.HasMark || a.Mark.RID == b.Mark.RID {
			continue
		}
		got := time.Duration(ticks(a.TS, b.TS)) * time.Second / 90000
		wantStep := time.Duration(b.Mark.CaptureNS - a.Mark.CaptureNS)
		if d := got - wantStep; d < -tolerance || d > tolerance {
			t.Errorf("switch %s → %s at seq %d: the timestamp advances by %v, the capture time by %v", a.Mark.RID,
				b.Mark.RID, b.FirstSeq, got, wantStep)
		}
	}
}

// audioRun is one uninterrupted stretch of a fake audio stream as a viewer got it: packets with consecutive
// indices. Between two runs the SFU forwarded nothing of the stream.
type audioRun struct {
	first, last time.Time // the arrival of its first and its last packet
	packets     int
}

// audioRuns splits a viewer's audio packets into runs, and checks what holds across a pause: every packet has its
// marker, the sequence numbers go on without a gap, the packet indices only go forward, and the timestamp advances
// by exactly the media time that was left out (20 ms, 960 ticks, per packet), so a resumed stream is still on its
// publisher's timeline (02 §9.4).
func audioRuns(t *testing.T, what string, pkts []sfutest.Packet) []audioRun {
	t.Helper()
	var runs []audioRun
	for i, p := range pkts {
		if !p.HasMark {
			t.Errorf("%s: audio seq %d has no marker", what, p.Seq)
			return runs
		}
		if i == 0 {
			runs = append(runs, audioRun{first: p.Arrival, last: p.Arrival, packets: 1})
			continue
		}
		prev := pkts[i-1]
		step := int64(p.Mark.Frame) - int64(prev.Mark.Frame)
		if step < 1 || ticks(prev.TS, p.TS) != step*960 || p.Seq != prev.Seq+1 {
			t.Errorf("%s: audio seq %d → %d: packet %d → %d, timestamp +%d; want the next seq and 960 ticks per packet",
				what, prev.Seq, p.Seq, prev.Mark.Frame, p.Mark.Frame, ticks(prev.TS, p.TS))
			return runs
		}
		if step > 1 {
			runs = append(runs, audioRun{first: p.Arrival, last: p.Arrival, packets: 1})
			continue
		}
		run := &runs[len(runs)-1]
		run.last = p.Arrival
		run.packets++
	}
	return runs
}

// selectedPair returns the candidate pair that a client's PeerConnection selected. Its senders or receivers share
// one transport (BUNDLE).
func selectedPair(t *testing.T, what string, pc *webrtc.PeerConnection) *webrtc.ICECandidatePair {
	t.Helper()
	for _, tr := range pc.GetTransceivers() {
		var dtls *webrtc.DTLSTransport
		if s := tr.Sender(); s != nil {
			dtls = s.Transport()
		}
		if r := tr.Receiver(); dtls == nil && r != nil {
			dtls = r.Transport()
		}
		if dtls == nil || dtls.ICETransport() == nil {
			continue
		}
		pair, err := dtls.ICETransport().GetSelectedCandidatePair()
		if err != nil || pair == nil || pair.Local == nil || pair.Remote == nil {
			t.Fatalf("%s: no selected candidate pair: %+v, %v", what, pair, err)
		}
		return pair
	}
	t.Fatalf("%s: the PeerConnection has no transport", what)
	return nil
}
