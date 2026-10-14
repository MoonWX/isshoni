package itest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/client/publish"
	"github.com/MoonWX/isshoni/internal/client/signal"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
)

// The client's own recovery, as the web client does it (01 §10.4–§10.6, 05 §9), for the recovery cases 4–8 of
// 01 §19 (README S74). The harness's clients do all of it in every test; in the tests of cases 1–3 and 9 nothing
// ever needs it.
//
//   - Signaling (watchStates, the worker): after a welcome that resumed, the client sends its full desired
//     subscription set and has its pub PC restart ICE if it isn't connected; after one that did not (the server
//     restarted), it discards its PeerConnections, joins its room again and publishes its share again with
//     replaces, and moves each subscription to the share that replaces the one it had (roomStateArrived).
//   - The sub PC (subDriver): ICE disconnected for 3 s asks for an ICE restart, pc.restart{sub, ice}; ICE failed,
//     or an ICE restart that hasn't connected 15 s after its offer, asks for a new one, pc.restart{sub, rebuild}.
//     A sub PC that the server closed counts as disconnected (watchSub). The server's offers are answered by the
//     pump (answerSubOffer).
//   - The pub PC (the share's driver): ICE disconnected for 3 s restarts ICE with an offer of the same gen; ICE
//     failed, the PC closed under it, an ICE restart that hasn't connected within 15 s, or the server's
//     pc.restart{pub, rebuild} rebuild it: a new PeerConnection and an offer of the next gen with the same share. A
//     codec hint has it offer again with the hinted profile first (01 §8.10).
//
// The limits of 01 §10.4 hold: at most one ICE restart per PC per 5 s and one rebuild per 10 s, and nothing is done
// while signaling isn't ready; the timers run on meanwhile. The drivers look at their PeerConnection every
// pollEvery, which stands in for the browser's state callbacks.

// The timers of 01 §10.4.
const (
	pollEvery        = 50 * time.Millisecond
	disconnectedWait = 3 * time.Second  // ICE disconnected this long: an ICE restart
	restartWait      = 15 * time.Second // an ICE restart not connected this long after its offer: a rebuild
	restartSpacing   = 5 * time.Second  // at most one ICE restart per PC per 5 s
	rebuildSpacing   = 10 * time.Second // at most one rebuild per PC per 10 s
)

// ---- signaling ----

// sockets records the TCP connections of a client's WebSocket upgrades, so that a test can cut the one in use
// (case 4: "close the TCP conn").
type sockets struct {
	mu   sync.Mutex
	last net.Conn
	n    int // connections dialed
}

// transport returns a copy of base, the test server's transport, that records the connections it dials.
func (s *sockets) transport(t *testing.T, base http.RoundTripper) http.RoundTripper {
	t.Helper()
	tr, ok := base.(*http.Transport)
	if !ok {
		t.Fatalf("the test server's transport is a %T, want an *http.Transport", base)
	}
	tr = tr.Clone()
	dial := tr.DialContext
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err == nil {
			s.mu.Lock()
			s.last, s.n = conn, s.n+1
			s.mu.Unlock()
		}
		return conn, err
	}
	return tr
}

// killSocket closes the TCP connection under the client's WebSocket: no close frame, no warning, as when a carrier's
// NAT forgets the connection or a proxy resets it (01 §11.5 A). The client notices at once and resumes.
func (c *client) killSocket() {
	c.t.Helper()
	c.sock.mu.Lock()
	conn := c.sock.last
	c.sock.mu.Unlock()
	if conn == nil {
		c.t.Fatalf("%s has no socket to kill", c.name)
	}
	if err := conn.Close(); err != nil {
		c.t.Fatalf("%s: closing its socket: %v", c.name, err)
	}
}

// dialed returns how many TCP connections the client's WebSocket has dialed: one per connection attempt.
func (c *client) dialed() int {
	c.sock.mu.Lock()
	defer c.sock.mu.Unlock()
	return c.sock.n
}

// watchStates follows the signaling client's states after its first welcome (Dial's): the client is ready, or not,
// and a welcome either resumed the connection or started a new one (01 §10.1, §10.5).
func (c *client) watchStates() {
	welcomed := false
	for sc := range c.sig.States() {
		if !welcomed {
			welcomed = sc.State == signal.StateReady // the changes up to Dial's welcome
			continue
		}
		c.mu.Lock()
		c.states = append(c.states, sc)
		c.mu.Unlock()
		switch {
		case sc.State != signal.StateReady:
			c.setReady(false)
		case sc.Resumed:
			c.setReady(true)
			c.post(c.resumed)
		default:
			c.newConnection()
			c.post(c.rejoin)
		}
	}
}

// setReady records whether signaling is ready.
func (c *client) setReady(ready bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ready = ready
}

// isReady reports whether signaling is ready on the connection of the given epoch.
func (c *client) isReady(epoch int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ready && c.epoch == epoch
}

// stateLog returns the signaling client's states after its first welcome.
func (c *client) stateLog() []signal.StateChange {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.states)
}

// waitWelcomes waits until the client has had n more welcomes after its first one, and returns their resumed flags.
func (c *client) waitWelcomes(n int) []bool {
	c.t.Helper()
	var resumed []bool
	eventually(c.t, func() bool {
		resumed = nil
		for _, sc := range c.stateLog() {
			if sc.State == signal.StateReady {
				resumed = append(resumed, sc.Resumed)
			}
		}
		return len(resumed) >= n
	}, func() string { return fmt.Sprintf("%s: states %+v, want %d more welcomes", c.name, c.stateLog(), n) })
	return resumed
}

// worker runs the client's own actions in order until the client closes: what follows a welcome, and the
// subscription updates that move a subscription to a share that replaces another.
func (c *client) worker() {
	for {
		select {
		case <-c.quit:
			return
		case f := <-c.work:
			f()
		}
	}
}

// post hands an action to the worker.
func (c *client) post(f func()) {
	select {
	case c.work <- f:
	case <-c.quit:
	}
}

// resumed is what follows a welcome that resumed the connection (01 §10.5): the full desired subscription set, and
// an ICE restart of a pub PC that isn't connected (01 §10.4). The server's Resync() sees to the sub PC.
func (c *client) resumed() {
	c.mu.Lock()
	wants := slices.Clone(c.wants)
	c.mu.Unlock()
	if len(wants) > 0 {
		var res protocol.SubscribeResult
		if err := c.do(protocol.MessageTypeSubscribeUpdate, protocol.SubscribeUpdate{Subs: wants}, &res); err != nil {
			c.fail(fmt.Errorf("the desired subscriptions after a resume: %w", err))
		}
	}
	for _, sh := range c.published() {
		sh.post(sh.resumed)
	}
}

// newConnection is the first part of what follows a welcome that did not resume (01 §10.5): the client discards
// its PeerConnections (the Viewer gets a new one; each share's driver discards its pub PC when it publishes again)
// and their gens, and takes the new connection's id.
func (c *client) newConnection() {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	c.mu.Lock()
	viewer := c.viewer
	c.mu.Unlock()
	if viewer != nil {
		if err := viewer.Rebuild(); err != nil {
			c.fail(fmt.Errorf("discarding the sub PC: %w", err))
		}
	}
	welcome := c.sig.Welcome()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epoch++
	c.connectionID = welcome.ConnectionID
	c.sub = subPC{}
	c.ready = true
}

// rejoin is the rest of it: room.join with the room the client was in, then its share published again with
// replaces on a new pub PC of gen 1. Its subscriptions move to the new shares as room.state names them
// (roomStateArrived).
func (c *client) rejoin() {
	c.mu.Lock()
	room, epoch := c.roomID, c.epoch
	c.mu.Unlock()
	var joined protocol.RoomJoinResult
	if err := c.do(protocol.MessageTypeRoomJoin, protocol.RoomJoin{RoomID: room}, &joined); err != nil {
		c.fail(fmt.Errorf("joining the room again: %w", err))
		return
	}
	for _, sh := range c.published() {
		sh.call(func() { sh.republish(epoch) })
	}
}

// roomStateArrived keeps a room.state and moves the client's subscriptions to the shares that replace the ones it
// had (01 §10.6): a share whose replaces names a share the client subscribed to, which it saw with the same user.
// The server can't check the old id after a restart, so the client does.
func (c *client) roomStateArrived(st protocol.RoomState) {
	c.mu.Lock()
	c.room = st
	c.rooms = append(c.rooms, st)
	var moved []protocol.SubscriptionWant
	for _, s := range st.Shares {
		if s.Replaces == "" || c.owners[s.Replaces] != s.UserID {
			continue
		}
		has := func(id string) int {
			return slices.IndexFunc(c.wants, func(w protocol.SubscriptionWant) bool { return w.ShareID == id })
		}
		if i := has(s.Replaces); i >= 0 && has(s.ID) < 0 {
			c.wants[i].ShareID = s.ID
			moved = append(moved, c.wants[i])
		}
	}
	for _, s := range st.Shares {
		c.owners[s.ID] = s.UserID
	}
	c.mu.Unlock()
	if len(moved) > 0 {
		c.post(func() {
			var res protocol.SubscribeResult
			if err := c.do(protocol.MessageTypeSubscribeUpdate, protocol.SubscribeUpdate{Subs: moved}, &res); err != nil {
				c.fail(fmt.Errorf("moving the subscriptions to the shares that replace theirs: %w", err))
			}
		})
	}
}

// desired returns the client's desired subscriptions.
func (c *client) desired() []protocol.SubscriptionWant {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.wants)
}

// lost reports whether err is a request or notification that found signaling down: the client acts again once it
// is ready.
func lost(err error) bool {
	return errors.Is(err, signal.ErrConnectionLost) || errors.Is(err, signal.ErrNotReady)
}

// sentRestarts returns the pc.restart requests the client sent.
func (c *client) sentRestarts() []protocol.PCRestart {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.sent)
}

// sentOffers returns the client's pub offers.
func (c *client) sentOffers() []protocol.PCOffer {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.pubOffers)
}

// askedRestarts returns the server's pc.restart requests.
func (c *client) askedRestarts() []protocol.PCRestart {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.asked)
}

// qualityHints returns the quality.hint notifications.
func (c *client) qualityHints() []protocol.QualityHint {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.hints)
}

// ---- the sub PC ----

// subDriver is the sub PC's recovery (01 §10.4, the "Sub" table), until stop is closed.
func (c *client) subDriver(stop <-chan struct{}) {
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
		}
		c.watchSub()
	}
}

// watchSub looks at the sub PC once, and asks the server for an ICE restart or a new PC when 01 §10.4 says so.
func (c *client) watchSub() {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	c.mu.Lock()
	viewer, sub, ready := c.viewer, c.sub, c.ready
	c.mu.Unlock()
	if viewer == nil || sub.gen == 0 {
		return // nothing offered yet on this connection
	}
	pc := viewer.PC()
	ice, state := pc.ICEConnectionState(), pc.ConnectionState()
	now := time.Now()
	switch {
	case state == webrtc.PeerConnectionStateClosed:
		// Pion closes a PeerConnection whose peer closed its side (a DTLS close_notify, 02 §5.3). The SFU does that
		// when it replaces the sub PC on its own, for a codec rebuild (02 §8.5), and the new PC's offer follows
		// within moments; a browser's PeerConnection doesn't close by itself, its ICE only stops hearing the server.
		// So a sub PC closed under the client counts as disconnected: the client asks for nothing unless the new
		// offer fails to come, and what it asks then (an ICE restart) gets a closed sub PC rebuilt (02 §6.4).
		if sub.down.IsZero() {
			sub.down = now
		}
	case ice == webrtc.ICEConnectionStateConnected || ice == webrtc.ICEConnectionStateCompleted:
		sub.down, sub.restartAt = time.Time{}, time.Time{}
	case ice == webrtc.ICEConnectionStateDisconnected:
		if sub.down.IsZero() {
			sub.down = now
		}
	default:
		sub.down = time.Time{}
	}
	var ask *protocol.PCRestart
	if ready {
		broken := ice == webrtc.ICEConnectionStateFailed ||
			(!sub.restartAt.IsZero() && now.Sub(sub.restartAt) >= restartWait)
		switch {
		case broken && now.Sub(sub.rebuiltAt) >= rebuildSpacing:
			ask = &protocol.PCRestart{
				PC: protocol.PCKindSub, Gen: sub.gen, Mode: protocol.RestartModeRebuild, Reason: protocol.RestartReasonFailed,
			}
			sub.rebuiltAt = now
		case !broken && !sub.down.IsZero() && now.Sub(sub.down) >= disconnectedWait && now.Sub(sub.askedAt) >= restartSpacing:
			ask = &protocol.PCRestart{
				PC: protocol.PCKindSub, Gen: sub.gen, Mode: protocol.RestartModeICE, Reason: protocol.RestartReasonDisconnected,
			}
			sub.askedAt = now
		}
	}
	c.mu.Lock()
	c.sub = sub
	c.mu.Unlock()
	if ask == nil {
		return
	}
	if err := c.send(protocol.MessageTypePCRestart, *ask); err != nil {
		if !lost(err) {
			c.fail(fmt.Errorf("pc.restart %+v: %w", *ask, err))
		}
		c.mu.Lock()
		c.sub.askedAt, c.sub.rebuiltAt = time.Time{}, time.Time{} // not sent: asks again once ready
		c.mu.Unlock()
	}
}

// ---- the pub PC ----

// share is one share of a client: the hub's id and parameters, the Source of its media, and the Publisher that
// sends it on the client's pub PeerConnection. After the first negotiation the share's driver owns that
// PeerConnection and its negotiations, one at a time: its recovery (01 §10.4, the "Pub" table), the codec hints,
// and the share published again after a server restart.
type share struct {
	c        *client
	seed     uint64
	settings webrtc.SettingEngine
	start    protocol.ShareStart // share.start without ref and replaces
	profiles []string            // the H.264 profiles the Publisher offers after High
	src      interface {
		publish.Source
		Close() error
	}
	ctx    context.Context // the Publishers': it ends when the test does
	cancel context.CancelFunc
	cmds   chan func() // run by the driver
	quit   chan struct{}
	driven sync.WaitGroup

	// id is the share's first id, as share.start answered it.
	id string

	mu       sync.Mutex // guards the fields below; after publish only the driver changes them
	pub      *publish.Publisher
	gen, neg uint32
	epoch    int               // the client's connection the pub PC belongs to
	applied  protocol.CodecKey // the H.264 profile last applied: share.start's, then a codec hint's (01 §8.10)
	ids      []string          // the share's ids: a new one after each server restart
	refs     int               // share.start requests made, for a ref of each its own
	did      []string          // what the driver did, in order: "ice restart", "rebuild", "republish", "codec h264/…"

	// The pub PC's recovery timers, the driver's alone: down is when its ICE was first seen disconnected, zero
	// while it isn't; restartAt is when the last ICE restart's offer went out, zero once ICE is connected again;
	// icedAt and rebuiltAt are the client's last ICE restart and rebuild.
	down, restartAt, icedAt, rebuiltAt time.Time
}

// publisher returns the Publisher that sends the share now.
func (sh *share) publisher() *publish.Publisher {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return sh.pub
}

// current returns the share's id now: share.start's first one, or the one of its latest re-publish.
func (sh *share) current() string {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return sh.ids[len(sh.ids)-1]
}

// allIDs returns every id the share has had, the first one first.
func (sh *share) allIDs() []string {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return slices.Clone(sh.ids)
}

// actions returns what the driver did, in order.
func (sh *share) actions() []string {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return slices.Clone(sh.did)
}

func (sh *share) note(what string) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.did = append(sh.did, what)
}

// startShare sends share.start, a new share or the re-publish of one that a server restart ended (replaces).
func (sh *share) startShare(replaces string) (protocol.ShareParams, error) {
	sh.mu.Lock()
	sh.refs++
	req := sh.start
	req.Ref, req.Replaces = "r"+strconv.FormatUint(sh.seed, 10)+"-"+strconv.Itoa(sh.refs), replaces
	sh.mu.Unlock()
	var params protocol.ShareParams
	err := sh.c.do(protocol.MessageTypeShareStart, req, &params)
	return params, err
}

// newPublisher makes a Publisher of the share's Source, with the profile last applied first in its offers.
func (sh *share) newPublisher() (*publish.Publisher, error) {
	pub, err := publish.New(publish.Options{Source: sh.src, Settings: sh.settings, Audio: true, Profiles: sh.profiles})
	if err != nil {
		return nil, err
	}
	sh.mu.Lock()
	applied := sh.applied
	sh.mu.Unlock()
	if profile, _ := strings.CutPrefix(string(applied), "h264/"); profile != "" && profile != publish.DefaultProfile {
		if err := pub.SetProfile(profile); err != nil {
			_ = pub.Close()
			return nil, err
		}
	}
	return pub, nil
}

// tracks are the TrackRefs of a pub offer: one per m-section of the Publisher (its simulcast encodings share the
// video's mid), all of them the share's.
func tracks(pub *publish.Publisher, shareID string) []protocol.TrackRef {
	var refs []protocol.TrackRef
	for _, tr := range pub.Tracks() {
		ref := protocol.TrackRef{MID: tr.MID, ShareID: shareID, Kind: protocol.TrackKindVideo}
		if tr.Kind == webrtc.RTPCodecTypeAudio {
			ref.Kind = protocol.TrackKindAudio
		}
		if !slices.Contains(refs, ref) {
			refs = append(refs, ref)
		}
	}
	return refs
}

// errStopped ends a negotiation whose share has stopped.
var errStopped = errors.New("the share's driver has stopped")

// negotiate runs one pub negotiation (01 §9): the offer of gen and neg with the share's tracks, ICE restart or
// not, the server's answer, applied. An error notification about the offer ends it. An offer that a drop of the
// socket left without an answer is sent again after the resume, with the same neg (01 §10.5); one whose connection
// is gone (a welcome that did not resume) is abandoned.
func (sh *share) negotiate(pub *publish.Publisher, gen, neg uint32, iceRestart bool) error {
	ctx, cancel := context.WithTimeout(sh.ctx, waitTimeout)
	defer cancel()
	var (
		offer webrtc.SessionDescription
		err   error
	)
	if iceRestart {
		offer, err = pub.OfferICERestart(ctx)
	} else {
		offer, err = pub.Offer(ctx)
	}
	if err != nil {
		return fmt.Errorf("pub offer gen %d, neg %d: %w", gen, neg, err)
	}
	c := sh.c
	sh.mu.Lock()
	o := protocol.PCOffer{
		PC: protocol.PCKindPub, Gen: gen, Neg: neg, SDP: protocol.SDP(offer.SDP), Tracks: tracks(pub, sh.ids[len(sh.ids)-1]),
	}
	epoch := sh.epoch
	sh.mu.Unlock()
	c.mu.Lock()
	answers, errs := len(c.answers), len(c.errs)
	c.mu.Unlock()
	sent := false
	var wasReady bool
	for {
		ready := c.isReady(epoch)
		if !sent && ready {
			switch err := c.send(protocol.MessageTypePCOffer, o); {
			case err == nil:
				sent = true
			case !lost(err):
				return fmt.Errorf("send the pub offer gen %d, neg %d: %w", gen, neg, err)
			}
		}
		if sent && !ready && wasReady {
			sent = false // the socket went away under the offer: it goes again after the resume
		}
		wasReady = ready
		var (
			answer *protocol.PCAnswer
			failed *protocol.Error
		)
		c.mu.Lock()
		gone := c.epoch != epoch
		for _, a := range c.answers[answers:] {
			if a.PC == protocol.PCKindPub && a.Gen == gen && a.Neg == neg {
				answer = &a
			}
		}
		for _, e := range c.errs[errs:] {
			if e.Scope == protocol.ErrorScopePC && e.PC == protocol.PCKindPub && e.Gen == gen && e.Neg == neg {
				failed = &e
			}
		}
		c.mu.Unlock()
		switch {
		case answer != nil:
			if err := pub.SetAnswer(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP.Reveal()}); err != nil {
				return fmt.Errorf("the Publisher refused the answer to gen %d, neg %d: %w", gen, neg, err)
			}
			return nil
		case failed != nil:
			return fmt.Errorf("the pub offer gen %d, neg %d: error %s", gen, neg, failed.Code)
		case gone:
			return fmt.Errorf("the pub offer gen %d, neg %d: the connection is gone", gen, neg)
		}
		select {
		case <-sh.quit:
			return errStopped
		case <-ctx.Done():
			return fmt.Errorf("no answer to the pub offer gen %d, neg %d: %w", gen, neg, ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// drive is the share's driver: it runs the commands posted to it and looks at the pub PC every pollEvery, until
// the share stops.
func (sh *share) drive() {
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	for {
		select {
		case <-sh.quit:
			return
		case f := <-sh.cmds:
			f()
		case <-tick.C:
			sh.watch()
		}
	}
}

// post hands a command to the driver.
func (sh *share) post(f func()) {
	select {
	case sh.cmds <- f:
	case <-sh.quit:
	}
}

// call runs a command on the driver and waits for it.
func (sh *share) call(f func()) {
	done := make(chan struct{})
	sh.post(func() {
		defer close(done)
		f()
	})
	select {
	case <-done:
	case <-sh.quit:
	}
}

// stop ends the driver and closes the Publisher (the test's cleanup).
func (sh *share) stop() {
	close(sh.quit)
	sh.driven.Wait()
	sh.cancel()
	if err := sh.publisher().Close(); err != nil {
		sh.c.t.Errorf("%s: Publisher.Close: %v", sh.c.name, err)
	}
}

// watch looks at the pub PC once, and restarts ICE or rebuilds the PC when 01 §10.4 says so.
func (sh *share) watch() {
	sh.mu.Lock()
	pub, epoch := sh.pub, sh.epoch
	sh.mu.Unlock()
	pc := pub.PC()
	ice, state := pc.ICEConnectionState(), pc.ConnectionState()
	now := time.Now()
	switch ice {
	case webrtc.ICEConnectionStateConnected, webrtc.ICEConnectionStateCompleted:
		sh.down, sh.restartAt = time.Time{}, time.Time{}
	case webrtc.ICEConnectionStateDisconnected:
		if sh.down.IsZero() {
			sh.down = now
		}
	default:
		sh.down = time.Time{}
	}
	if !sh.c.isReady(epoch) {
		return // only recorded while signaling isn't ready, or until the share is published on the new connection
	}
	switch {
	case state == webrtc.PeerConnectionStateClosed || ice == webrtc.ICEConnectionStateFailed:
		sh.rebuild("the pub PC is " + state.String())
	case !sh.restartAt.IsZero() && now.Sub(sh.restartAt) >= restartWait:
		sh.rebuild("the ICE restart did not connect")
	case !sh.down.IsZero() && now.Sub(sh.down) >= disconnectedWait:
		sh.restartICE()
	}
}

// resumed runs after a welcome that resumed the connection: a pub PC that isn't connected restarts ICE (01 §10.4).
func (sh *share) resumed() {
	switch sh.publisher().PC().ICEConnectionState() {
	case webrtc.ICEConnectionStateConnected, webrtc.ICEConnectionStateCompleted:
	default:
		sh.restartICE()
	}
}

// restartICE offers again on the same PC with new ICE credentials: the same gen, the next neg.
func (sh *share) restartICE() {
	now := time.Now()
	if now.Sub(sh.icedAt) < restartSpacing {
		return
	}
	sh.mu.Lock()
	sh.neg++
	pub, gen, neg := sh.pub, sh.gen, sh.neg
	sh.mu.Unlock()
	sh.icedAt, sh.restartAt, sh.down = now, now, time.Time{}
	sh.note("ice restart")
	if err := sh.negotiate(pub, gen, neg, true); err != nil && !errors.Is(err, errStopped) {
		sh.c.fail(fmt.Errorf("ICE restart: %w", err))
	}
}

// rebuildAsked handles the server's pc.restart{pub, rebuild} for the PC of gen.
func (sh *share) rebuildAsked(gen uint32) {
	sh.mu.Lock()
	cur, epoch := sh.gen, sh.epoch
	sh.mu.Unlock()
	if gen == cur && sh.c.isReady(epoch) {
		sh.rebuild("the server asked")
	}
}

// rebuild replaces the pub PC: a new Publisher of the same Source (the old one is closed first: a Source has one
// consumer at a time), and an offer of the next gen with the same share in its tracks.
func (sh *share) rebuild(why string) {
	now := time.Now()
	if now.Sub(sh.rebuiltAt) < rebuildSpacing {
		return
	}
	sh.rebuiltAt, sh.restartAt, sh.down = now, time.Time{}, time.Time{}
	old := sh.publisher()
	if err := old.Close(); err != nil {
		sh.c.t.Logf("%s: closing the pub PC to rebuild it: %v", sh.c.name, err)
	}
	pub, err := sh.newPublisher()
	if err != nil {
		sh.c.fail(fmt.Errorf("rebuild: %w", err))
		return
	}
	sh.mu.Lock()
	sh.pub = pub
	sh.gen++
	sh.neg = 1
	gen := sh.gen
	sh.mu.Unlock()
	sh.note("rebuild")
	sh.c.t.Logf("%s rebuilds its pub PC, gen %d: %s", sh.c.name, gen, why)
	if err := sh.negotiate(pub, gen, 1, false); err != nil {
		if !errors.Is(err, errStopped) {
			sh.c.fail(fmt.Errorf("rebuild: %w", err))
		}
		return
	}
	if err := pub.Start(sh.ctx); err != nil {
		sh.c.fail(fmt.Errorf("rebuild: %w", err))
	}
}

// republish publishes the share again on the connection of epoch, which a welcome that did not resume began (the
// server restarted, 01 §10.5, §10.6): the old pub PC is discarded, share.start replaces the share's last id, and a
// new pub PC of gen 1 carries the new share.
func (sh *share) republish(epoch int) {
	if err := sh.publisher().Close(); err != nil {
		sh.c.t.Logf("%s: closing the old pub PC: %v", sh.c.name, err)
	}
	params, err := sh.startShare(sh.current())
	if err != nil {
		sh.c.fail(fmt.Errorf("publishing again: %w", err))
		return
	}
	sh.mu.Lock()
	sh.applied = params.Codec
	sh.mu.Unlock()
	pub, err := sh.newPublisher()
	if err != nil {
		sh.c.fail(fmt.Errorf("publishing again: %w", err))
		return
	}
	sh.mu.Lock()
	sh.pub, sh.gen, sh.neg, sh.epoch = pub, 1, 1, epoch
	sh.ids = append(sh.ids, params.ShareID)
	sh.mu.Unlock()
	sh.down, sh.restartAt, sh.icedAt, sh.rebuiltAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}
	sh.note("republish")
	if err := sh.negotiate(pub, 1, 1, false); err != nil {
		if !errors.Is(err, errStopped) {
			sh.c.fail(fmt.Errorf("publishing again: %w", err))
		}
		return
	}
	if err := pub.Start(sh.ctx); err != nil {
		sh.c.fail(fmt.Errorf("publishing again: %w", err))
	}
}

// hinted handles a quality.hint: one with a codec other than the one last applied has the Publisher put that
// profile first and offer again (same gen, next neg), as a browser sharer does; the answer has it switch (01 §8.10).
// Encodings hints change nothing: the Go Publisher has no rate control in M1.
func (sh *share) hinted(h protocol.QualityHint) {
	sh.mu.Lock()
	pub, gen, id, applied, epoch := sh.pub, sh.gen, sh.ids[len(sh.ids)-1], sh.applied, sh.epoch
	sh.mu.Unlock()
	if h.ShareID != id || h.Codec == "" || h.Codec == applied || !sh.c.isReady(epoch) {
		return
	}
	profile, ok := strings.CutPrefix(string(h.Codec), "h264/")
	if !ok {
		sh.c.fail(fmt.Errorf("a codec hint %q that is no H.264 profile", h.Codec))
		return
	}
	if err := pub.SetProfile(profile); err != nil {
		sh.c.fail(fmt.Errorf("codec hint %s: %w", h.Codec, err))
		return
	}
	sh.mu.Lock()
	sh.applied = h.Codec
	sh.neg++
	neg := sh.neg
	sh.mu.Unlock()
	sh.note("codec " + string(h.Codec))
	if err := sh.negotiate(pub, gen, neg, false); err != nil && !errors.Is(err, errStopped) {
		sh.c.fail(fmt.Errorf("codec hint %s: %w", h.Codec, err))
	}
}

// ---- networks ----

// faultySettings returns loopback settings for a client whose UDP goes through one socket wrapped in a FaultConn,
// so that a test can take its network away and give it back without a word to anybody. Its ICE behaves more like a
// browser's than Pion's defaults do:
//   - it notices a silence after 2 s, checking every 500 ms (Pion: 5 s and 2 s), and fails 25 s later;
//   - it goes on checking a candidate pair until then. Pion gives up on a pair after 7 unanswered binding requests,
//     about 1.5 s while it checks every 200 ms, and never checks it again: an ICE restart made while the network is
//     still gone would not connect when the network is back, and the client would rebuild after 15 s (01 §10.4).
func faultySettings(t *testing.T) (webrtc.SettingEngine, *sfutest.FaultConn) {
	t.Helper()
	var lc net.ListenConfig
	sock, err := lc.ListenPacket(context.Background(), "udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	wire := sfutest.NewFaultConn(sock)
	mux := webrtc.NewICEUDPMux(nil, wire)
	t.Cleanup(func() { _ = mux.Close() }) // after the client's own cleanup, which is registered later and runs first
	se := sfutest.LoopbackSettings()
	se.SetICEUDPMux(mux)
	se.SetICETimeouts(2*time.Second, 25*time.Second, 500*time.Millisecond)
	se.SetICEMaxBindingRequests(25 * 5) // 25 s of checks, every 200 ms
	return se, wire
}

// blackHole takes a client's UDP away (nothing in, nothing out) or gives it back.
func blackHole(wire *sfutest.FaultConn, on bool) {
	if on {
		wire.SetDrop(func(sfutest.Direction, net.Addr, []byte) bool { return true })
	} else {
		wire.SetDrop(nil)
	}
}
