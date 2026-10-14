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
// proxy in front of an off-mode server would (04 §8.5). A 429, and a 503 with a Retry-After (the server-wide
// auth-hash budget of 03 §7.3, a burst of 20), is retried after its Retry-After: the accounts of a large test come
// faster than the server lets strangers sign up, and on a fast machine faster than the budget refills.
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
		retry := res.StatusCode == http.StatusTooManyRequests || res.StatusCode == http.StatusServiceUnavailable
		if !retry || wait <= 0 || time.Now().Add(time.Duration(wait)*time.Second).After(deadline) {
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

// client is one connection of a member in the default room: the Go signaling client, with the goroutines that
// receive its notifications and its state changes for as long as it lives, and the PeerConnections the test gives
// it. Like the web client, it recovers by itself (client_test.go): it resumes, answers every sub offer as 01 §9
// says, restarts and rebuilds its PeerConnections (01 §10.4), and after a welcome that does not resume it joins
// its room again, publishes its share again with replaces and moves its subscriptions to the new shares
// (01 §10.5, §10.6). It publishes at most one share, on a pub PeerConnection of its own.
type client struct {
	t     *testing.T
	name  string
	sig   *signal.Client
	sock  *sockets // the TCP connections of the client's WebSocket upgrades
	quit  chan struct{}
	work  chan func()    // the client's own actions, run one after the other by its worker (client_test.go)
	alive sync.WaitGroup // the goroutines that receive sig.Events and sig.States, and the worker
	// userID is the welcome's; the user is the same on every connection.
	userID string

	// subMu makes the sub PeerConnection's owner one at a time, as a browser's page is: answering an offer, the sub
	// PC's recovery (subDriver) and the discarding of a new connection's old PeerConnections.
	subMu sync.Mutex

	mu           sync.Mutex // guards the fields below
	connectionID string     // the last welcome's
	roomID       string     // the room the client is in: the one it joins again after a welcome that did not resume
	ready        bool       // signaling is ready (01 §10.1): the client acts on its PeerConnections only then
	epoch        int        // the welcomes that did not resume since the first: a new connection after a restart
	room         protocol.RoomState
	rooms        []protocol.RoomState // every room.state, in order
	events       []protocol.RoomEvent
	status       map[string][]protocol.SubscriptionStatus // by share, in the order they came
	answers      []protocol.PCAnswer                      // to this client's pub offers
	offers       []protocol.PCOffer                       // the server's sub offers
	hints        []protocol.QualityHint                   // quality.hint notifications
	asked        []protocol.PCRestart                     // the server's pc.restart requests
	sent         []protocol.PCRestart                     // the client's pc.restart requests
	pubOffers    []protocol.PCOffer                       // the client's pub offers
	errs         []protocol.Error                         // error notifications
	goodbye      []protocol.ServerShutdown                // server.shutdown notices
	states       []signal.StateChange                     // the signaling client's states after its first welcome
	failed       []error                                  // what the pump, the worker and the drivers ran into
	viewer       *sfutest.Viewer                          // answers the sub offers; nil until watch
	sub          subPC                                    // the sub PeerConnection as the client knows it
	wants        []protocol.SubscriptionWant              // the desired subscriptions, one per share, merged (01 §8.9)
	owners       map[string]string                        // the user of every share the client has seen in room.state
	shares       []*share                                 // what the client publishes
}

// subPC is what a client knows of its sub PeerConnection (01 §9, §10.4): the gen and neg of the last offer it
// answered, that offer's ICE user name and its answer, and the timers of its recovery.
type subPC struct {
	gen, neg uint32
	ufrag    string
	answer   string
	// down is when the PC's ICE was first seen disconnected, zero while it isn't; restartAt is when an offer with a
	// new ICE user name came (an ICE restart, whoever asked for it), zero once ICE is connected again; askedAt and
	// rebuiltAt are the client's last pc.restart requests for an ICE restart and a rebuild.
	down, restartAt, askedAt, rebuiltAt time.Time
}

// connOptions configure a connection beyond its member and role.
type connOptions struct {
	// caps are the hello's capabilities; nil means the harness's caps.
	caps *protocol.Caps
}

// connect opens a signaling connection for m with the given role and joins the default room. The client is closed
// when the test ends, before the server stops.
func (w *world) connect(m *member, role protocol.Role) *client {
	w.t.Helper()
	return w.connectWith(m, role, connOptions{})
}

// connectWith is connect with options.
func (w *world) connectWith(m *member, role protocol.Role, o connOptions) *client {
	w.t.Helper()
	hello := caps
	if o.caps != nil {
		hello = *o.caps
	}
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	sock := &sockets{}
	sig, err := signal.Dial(ctx, signal.Options{
		URL: w.srv.WSURL,
		// Its jar has the member's session cookie, and its transport reaches the test server. The sockets it dials
		// are recorded, so a test can cut one.
		HTTPClient: &http.Client{Jar: m.http.Jar, Transport: sock.transport(w.t, w.srv.Client.Transport)},
		Client:     protocol.ClientInfo{Kind: protocol.ClientKindTool, Version: "0.1.0", OS: protocol.ClientOSLinux},
		Role:       role,
		Caps:       hello,
	})
	if err != nil {
		w.t.Fatalf("%s: dial %s: %v", m.name, w.srv.WSURL, err)
	}
	welcome := sig.Welcome()
	c := &client{
		t: w.t, name: m.name, sig: sig, sock: sock, quit: make(chan struct{}), work: make(chan func(), 64),
		userID: welcome.User.ID, connectionID: welcome.ConnectionID, ready: true,
		status: map[string][]protocol.SubscriptionStatus{}, owners: map[string]string{},
	}
	// A signaling client whose Events nobody receives stops reading its socket (01 §15.3): the pump runs until Close
	// has closed the channel. The state watcher and the worker are the client's recovery (client_test.go).
	c.alive.Go(c.pump)
	c.alive.Go(c.watchStates)
	c.alive.Go(c.worker)
	w.t.Cleanup(c.close)
	if welcome.Resumed || welcome.User.Name != m.name {
		w.t.Fatalf("%s: welcome = %+v", m.name, welcome)
	}
	var joined protocol.RoomJoinResult
	c.request(protocol.MessageTypeRoomJoin, protocol.RoomJoin{RoomID: welcome.DefaultRoomID}, &joined)
	if joined.Room.ID != welcome.DefaultRoomID {
		w.t.Fatalf("%s: joined %+v, want the default room %s", m.name, joined.Room, welcome.DefaultRoomID)
	}
	c.mu.Lock()
	c.roomID = joined.Room.ID
	c.mu.Unlock()
	return c
}

// close ends the connection with a normal close, so the server ends it at once, and waits for the client's
// goroutines.
func (c *client) close() {
	if err := c.sig.Close(); err != nil {
		c.t.Logf("%s: closing the signaling client: %v", c.name, err)
	}
	close(c.quit)
	c.alive.Wait()
}

// connID returns the connection id of the last welcome: a new one after a welcome that did not resume.
func (c *client) connID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connectionID
}

// pump receives the client's notifications in order until the client has stopped. It keeps what the tests look at
// and answers the server's sub offers with the client's Viewer, as a browser's sub PeerConnection would (01 §9).
// What takes a request of its own (moving a subscription to a share that replaces another) or a negotiation (a
// codec hint, a request to rebuild the pub PC) goes to the worker or to the share's driver: a pump that waited for
// a reply would hold up the notifications behind it.
func (c *client) pump() {
	for env := range c.sig.Events() {
		if err := c.handle(env); err != nil {
			c.fail(fmt.Errorf("%s: %w", env.Type, err))
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
		c.roomStateArrived(st)
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
	case protocol.MessageTypePCRestart:
		r, err := protocol.Decode[protocol.PCRestart](env)
		if err != nil {
			return err
		}
		c.mu.Lock()
		c.asked = append(c.asked, r)
		c.mu.Unlock()
		if r.PC != protocol.PCKindPub || r.Mode != protocol.RestartModeRebuild {
			return fmt.Errorf("the server asked for %+v: it asks only to rebuild the pub PC (01 §10.4)", r)
		}
		for _, sh := range c.published() {
			sh.post(func() { sh.rebuildAsked(r.Gen) })
		}
	case protocol.MessageTypeQualityHint:
		h, err := protocol.Decode[protocol.QualityHint](env)
		if err != nil {
			return err
		}
		c.mu.Lock()
		c.hints = append(c.hints, h)
		c.mu.Unlock()
		for _, sh := range c.published() {
			sh.post(func() { sh.hinted(h) })
		}
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
		// The server is going away and will close the PeerConnections too: like the web client, this one ignores what
		// happens to them until it is ready again (01 §11.6).
		c.mu.Lock()
		c.goodbye = append(c.goodbye, bye)
		c.ready = false
		c.mu.Unlock()
	}
	return nil
}

// answerSubOffer answers a sub offer on the Viewer as 01 §9 says, and sends the answer with the offer's gen and neg.
// An offer of a new gen gets a new PeerConnection (Viewer.Rebuild); the repeat of the offer last answered (a resume
// sends it again) gets the answer that was sent; an older one is dropped. An offer of the same gen with a new ICE
// user name is an ICE restart, whoever asked for it: it stops the 3 s timer of the sub PC's recovery and starts its
// 15 s one (01 §10.4). The Viewer's answer carries its candidates, so the client sends no pc.ice.
func (c *client) answerSubOffer(o protocol.PCOffer) error {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	c.mu.Lock()
	c.offers = append(c.offers, o)
	viewer, cur := c.viewer, c.sub
	c.mu.Unlock()
	if o.PC != protocol.PCKindSub {
		return fmt.Errorf("an offer for the %s PC: the server offers on sub only", o.PC)
	}
	if viewer == nil {
		return errors.New("a sub offer, and the client has no Viewer")
	}
	switch {
	case o.Gen < cur.gen, o.Gen == cur.gen && o.Neg < cur.neg:
		return nil // stale (01 §9 rules 2 and 3)
	case o.Gen == cur.gen && o.Neg == cur.neg:
		return c.send(protocol.MessageTypePCAnswer, protocol.PCAnswer{PC: o.PC, Gen: o.Gen, Neg: o.Neg, SDP: protocol.SDP(cur.answer)})
	case o.Gen > cur.gen && cur.gen != 0:
		if err := viewer.Rebuild(); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	answer, err := viewer.Answer(ctx, webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: o.SDP.Reveal()})
	if err != nil {
		return err
	}
	ufrag := iceUfrag(o.SDP.Reveal())
	c.mu.Lock()
	next := subPC{gen: o.Gen, neg: o.Neg, ufrag: ufrag, answer: answer.SDP, askedAt: cur.askedAt, rebuiltAt: cur.rebuiltAt}
	if o.Gen == cur.gen {
		next.down, next.restartAt = cur.down, cur.restartAt
		if ufrag != cur.ufrag {
			next.down, next.restartAt = time.Time{}, time.Now()
		}
	}
	c.sub = next
	c.mu.Unlock()
	return c.send(protocol.MessageTypePCAnswer, protocol.PCAnswer{PC: o.PC, Gen: o.Gen, Neg: o.Neg, SDP: protocol.SDP(answer.SDP)})
}

// iceUfrag returns the first ICE user name of a description, "" when it has none.
func iceUfrag(sdp string) string {
	for line := range strings.SplitSeq(sdp, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "a=ice-ufrag:"); ok {
			return v
		}
	}
	return ""
}

// send sends a notification and keeps the pc.restart requests and the pub offers among them.
func (c *client) send(typ protocol.MessageType, data any) error {
	if err := c.sig.Send(typ, data); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch m := data.(type) {
	case protocol.PCRestart:
		c.sent = append(c.sent, m)
	case protocol.PCOffer:
		c.pubOffers = append(c.pubOffers, m)
	}
	return nil
}

// request sends a request and decodes its reply into result; any error fails the test. It is for the test's own
// goroutine; the client's own actions use do.
func (c *client) request(typ protocol.MessageType, data, result any) {
	c.t.Helper()
	if err := c.do(typ, data, result); err != nil {
		c.t.Fatalf("%s: %v", c.name, err)
	}
}

// do sends a request and decodes its reply into result.
func (c *client) do(typ protocol.MessageType, data, result any) error {
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	if err := c.sig.Request(ctx, typ, data, result); err != nil {
		return fmt.Errorf("%s: %w", typ, err)
	}
	return nil
}

// fail keeps what went wrong on one of the client's own goroutines, for check.
func (c *client) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failed = append(c.failed, fmt.Errorf("%s: %w", c.name, err))
}

// viewerOptions configure a client's Viewer beyond its Pion settings.
type viewerOptions struct {
	// keep bounds the packets each of its Recorders keeps (0: the Viewer's default).
	keep int
	// noH264 makes the Viewer a fresh Firefox, without an H.264 decoder (sfutest.ViewerOptions.NoH264).
	noH264 bool
}

// watch gives the client its sub PeerConnection: a Viewer with the given Pion settings, which answers the
// server's sub offers from now on. keep bounds the packets each of its Recorders keeps (0: the Viewer's default).
func (c *client) watch(se webrtc.SettingEngine, keep int) *sfutest.Viewer {
	c.t.Helper()
	return c.watchWith(se, viewerOptions{keep: keep})
}

// watchWith is watch with options. The sub PC's recovery (subDriver) runs until the Viewer closes.
func (c *client) watchWith(se webrtc.SettingEngine, o viewerOptions) *sfutest.Viewer {
	c.t.Helper()
	v, err := sfutest.NewViewer(sfutest.ViewerOptions{Settings: se, KeepPackets: o.keep, NoH264: o.noH264})
	if err != nil {
		c.t.Fatal(err)
	}
	stop, stopped := make(chan struct{}), make(chan struct{})
	c.t.Cleanup(func() {
		close(stop)
		<-stopped
		if err := v.Close(); err != nil {
			c.t.Errorf("%s: Viewer.Close: %v", c.name, err)
		}
	})
	c.mu.Lock()
	c.viewer = v
	c.mu.Unlock()
	go func() {
		defer close(stopped)
		c.subDriver(stop)
	}()
	return v
}

// subscribe sends the client's subscriptions in one subscribe.update, which the SFU applies in one step, and merges
// them into the client's desired set (01 §8.9), which it sends again after a resume. A share the server no longer
// knows fails the test.
func (c *client) subscribe(wants ...protocol.SubscriptionWant) {
	c.t.Helper()
	c.mu.Lock()
	c.wants = mergeWants(c.wants, wants...)
	c.mu.Unlock()
	var res protocol.SubscribeResult
	c.request(protocol.MessageTypeSubscribeUpdate, protocol.SubscribeUpdate{Subs: wants}, &res)
	if len(res.Ignored) != 0 {
		c.t.Fatalf("%s: subscribe.update ignored %v", c.name, res.Ignored)
	}
}

// mergeWants merges subscriptions into a desired set, one entry per share, as the server merges a
// subscribe.update into a connection's wants.
func mergeWants(set []protocol.SubscriptionWant, wants ...protocol.SubscriptionWant) []protocol.SubscriptionWant {
	set = slices.Clone(set)
	for _, w := range wants {
		if i := slices.IndexFunc(set, func(s protocol.SubscriptionWant) bool { return s.ShareID == w.ShareID }); i >= 0 {
			set[i] = w
		} else {
			set = append(set, w)
		}
	}
	return set
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

// roomStates returns every room.state so far, in order.
func (c *client) roomStates() []protocol.RoomState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.rooms)
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

// check fails the test when the client was sent an error notification, or one of its goroutines ran into
// something: nothing in these tests is supposed to go wrong on the way.
func (c *client) check() {
	c.t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.errs {
		c.t.Errorf("%s was sent error %s (scope %s, share %q, pc %q)", c.name, e.Code, e.Scope, e.ShareID, e.PC)
	}
	for _, err := range c.failed {
		c.t.Errorf("%v", err)
	}
}

// ---- publishing ----

// fakeLayers are small fake video layers, light enough for a 10×10 test on any CI machine: the full layer "f" and
// the preview "q".
func fakeLayers() []fake.VideoLayer {
	return []fake.VideoLayer{
		{RID: "f", Width: 640, Height: 360, FPS: 30, Bitrate: 400_000},
		{RID: "q", Width: 320, Height: 180, FPS: 15, Bitrate: 100_000},
	}
}

// fakeSource makes the fake media of a share: f and q with audio, in an H.264 profile ("" is fake's default, High),
// with a GOP far longer than a test, so that every keyframe after a layer's first is one that somebody asked for.
func fakeSource(seed uint64, profile string) (*fake.Source, error) {
	return fake.New(fake.Config{Profile: profile, Layers: fakeLayers(), GOP: time.Minute, Audio: true, Seed: seed})
}

// shareOptions configure a share beyond its seed and the Pion settings of its publisher.
type shareOptions struct {
	// profileSource makes the media a sfutest.ProfileSource that starts in High and can switch to Constrained
	// Baseline, and the Publisher offers both, High first, as Chrome does: a sharer whose room's codec policy can
	// change (02 §8.3).
	profileSource bool
}

// publish starts a share and publishes fake media on it, the way a browser does (01 §11.2): share.start, a pub
// offer whose tracks name the share, the server's answer, then media. The video is simulcast f and q with audio.
// se are the Pion settings of the pub PeerConnection. The share's driver (client_test.go) recovers its pub PC
// until the test ends.
func (c *client) publish(seed uint64, se webrtc.SettingEngine) *share {
	c.t.Helper()
	return c.publishWith(seed, se, shareOptions{})
}

// publishWith is publish with options.
func (c *client) publishWith(seed uint64, se webrtc.SettingEngine, o shareOptions) *share {
	c.t.Helper()
	if len(c.published()) != 0 {
		c.t.Fatalf("%s publishes a share already: a client of this harness has one pub PC, for one share", c.name)
	}
	sh := &share{
		c: c, seed: seed, settings: se, quit: make(chan struct{}), cmds: make(chan func(), 64),
		start: protocol.ShareStart{Kind: protocol.ShareKindScreen, Preset: protocol.PresetAuto, Audio: true},
	}
	params, err := sh.startShare("")
	if err != nil {
		c.t.Fatalf("%s: %v", c.name, err)
	}
	if params.ShareID == "" || params.Codec != protocol.CodecH264High || len(params.Encodings) != 2 || params.AudioBitrate <= 0 {
		c.t.Fatalf("%s: share.start = %+v, want an id, High profile, two encodings and an audio bitrate", c.name, params)
	}
	sh.id, sh.applied = params.ShareID, params.Codec
	sh.ids = []string{params.ShareID}

	if o.profileSource {
		sh.profiles = []string{"42e0"}
		sh.src, err = sfutest.NewProfileSource(publish.DefaultProfile, func(p string) (*fake.Source, error) {
			return fakeSource(seed, p)
		})
	} else {
		sh.src, err = fakeSource(seed, "")
	}
	if err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() { _ = sh.src.Close() })
	// The Publishers send until the test ends: their context has no deadline of its own.
	sh.ctx, sh.cancel = context.WithCancel(context.Background())
	pub, err := sh.newPublisher()
	if err != nil {
		c.t.Fatal(err)
	}
	sh.pub, sh.gen, sh.neg = pub, 1, 1
	c.t.Cleanup(sh.stop)
	if err := sh.negotiate(pub, 1, 1, false); err != nil {
		c.t.Fatalf("%s: %v", c.name, err)
	}
	if err := pub.Start(sh.ctx); err != nil {
		c.t.Fatal(err)
	}
	c.mu.Lock()
	c.shares = append(c.shares, sh)
	c.mu.Unlock()
	sh.driven.Go(sh.drive)
	return sh
}

// published returns the shares the client publishes.
func (c *client) published() []*share {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.shares)
}

// waitLive waits until the client's room.state shows the share live with both layers, its codec and its audio:
// the SFU has both simulcast layers and has seen the first keyframe.
func (c *client) waitLive(shareID string) protocol.ShareInfo {
	c.t.Helper()
	return c.waitLiveIn(shareID, protocol.CodecH264High)
}

// waitLiveIn is waitLive for a share in another H.264 profile.
func (c *client) waitLiveIn(shareID string, codec protocol.CodecKey) protocol.ShareInfo {
	c.t.Helper()
	both := []protocol.VideoLayer{protocol.VideoLayerHigh, protocol.VideoLayerLow}
	var info protocol.ShareInfo
	eventually(c.t, func() bool {
		var ok bool
		info, ok = c.shareIn(shareID)
		return ok && info.Status == protocol.ShareStatusLive && slices.Equal(info.Layers, both) &&
			info.Codec == codec && info.Audio
	}, func() string {
		return fmt.Sprintf("%s: share %s in room.state is %+v, want it live in %s", c.name, shareID, info, codec)
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
//
// The new layer's first frame can have been captured before the old layer's last one (README S74: TestLayers once
// failed with "the capture time by -100ms" on a loaded machine). The SFU switches when the new layer's keyframe is
// read, and each layer has a read loop of its own: when the new layer's loop is late, the old layer's frames
// captured after the keyframe are forwarded first. A timestamp never goes back, so the SFU puts such a keyframe one
// tick after the last frame (02 §9.4, the lastTS + 1 rule, for a step of up to a second back), and the step to
// expect is then that tick. The test assumed that the keyframe always comes after the frames captured before it.
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
		capture := time.Duration(b.Mark.CaptureNS - a.Mark.CaptureNS)
		wantStep := capture
		if capture < 0 && capture > -time.Second {
			wantStep = 0
		}
		if d := got - wantStep; d < -tolerance || d > tolerance {
			t.Errorf("switch %s → %s at seq %d: the timestamp advances by %v, the capture time by %v", a.Mark.RID,
				b.Mark.RID, b.FirstSeq, got, capture)
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
