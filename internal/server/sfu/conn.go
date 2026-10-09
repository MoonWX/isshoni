package sfu

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
)

// Conn is one signaling connection in a room (02 §5.1): at most one publish and one subscribe PeerConnection, and
// its subscriptions. It outlives a WebSocket: a drop is invisible here, and signal calls Resync on resume and Close
// after its grace period (02 §5.3).
//
// One actor goroutine per Conn does every signaling operation and every Pion PC signaling call (02 §5.4). It reads
// two queues: the bounded command queue, for the methods signal calls (they wait for the actor and return its
// result), and the unbounded internal event queue, for Pion callbacks, timers and work from other Conns (they never
// wait). Everything below "actor state" belongs to the actor alone and needs no lock.
type Conn struct {
	sfu    *SFU
	room   *Room
	part   *Participant
	id     ConnID
	user   UserID
	role   Role
	client ClientKind
	decode DecodeCaps // as joined; README S69 makes it follow SetDecodeCaps
	sig    Signaler
	log    *slog.Logger

	ctx    context.Context // ends at Close: bounds the actor's own waits (gathering)
	cancel context.CancelFunc
	closed atomic.Bool
	stop   chan struct{} // closed by Close: the actor stops
	done   chan struct{} // closed by the actor once its PCs are closed and their readers have ended
	cmds   chan *command
	events eventQueue

	// pubUp and pubDownAt are what the actor has seen of the Conn's pub PC, for the goroutines that decide the media
	// state of its shares (02 §5.3): the layers' read loops and the ticker. pubUp is true while the current pub PC is
	// connected. pubDownAt is the monoNow at which it last stopped being so, 0 before any pub PC was connected. Only
	// the actor writes them (markPubDown, onPCState).
	pubUp     atomic.Bool
	pubDownAt atomic.Int64

	// actor state
	pub    *pubPC // nil until the first pub offer, and after the pub PC closed
	pubGen uint32 // the highest pub gen accepted; 0 before the first
	sub    *subPC // nil until the first subscription
	subGen uint32 // the gen of the newest sub PC; 0 before the first
	subs   map[ShareID]*Subscription
	// cands are the remote candidates of the Conn's current PC of each kind, by PCKind: what counts toward that PC's
	// 64 (02 §12). Nothing but that PC's own gen changes them.
	cands [PCSub + 1]remoteCandidates
	// pubAhead holds the candidates the client trickled for a pub gen above pubGen, whose offer hasn't been accepted
	// yet: the client owns the pub gen, so its candidates can overtake their offer. They are only kept here, apart
	// from the current pub PC's candidates, and become the new PC's when that gen's offer is accepted.
	pubAhead remoteCandidates
	pcs      [PCSub + 1]pcWindow // the PeerConnections the client made the Conn create, by PCKind (02 §12)
	// writers are the writer goroutines of the Conn's DownTracks. Each ends when its subscription goes
	// (DownTrack.stop), and at the latest when the Conn closes (stop); teardown waits for all of them.
	writers sync.WaitGroup
}

// newConn builds a Conn; SFU.Join registers it and starts its actor.
func newConn(s *SFU, room *Room, part *Participant, p JoinParams) *Conn {
	// The Conn is its own top-level operation: it lives until Close, whatever the Join caller's context does.
	ctx, cancel := context.WithCancel(context.Background())
	return &Conn{
		sfu: s, room: room, part: part,
		id: p.Conn, user: p.User, role: p.Role, client: p.Client,
		decode: DecodeCaps{H264: append([]ProfileKey(nil), p.Decode.H264...)},
		sig:    p.Signaler,
		log:    s.log.With("conn_id", string(p.Conn), "room_id", string(p.Room), "user_id", string(p.User)),
		ctx:    ctx, cancel: cancel,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		cmds:   make(chan *command, commandQueueLen),
		events: eventQueue{wake: make(chan struct{}, 1)},
		subs:   map[ShareID]*Subscription{},
	}
}

// ID returns the id signal gave the Conn in JoinParams.
func (c *Conn) ID() ConnID { return c.id }

// Done is closed when the Conn has closed its PeerConnections, some time after Close.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Close takes the Conn out of its room and ends the shares it still publishes with r (the hub has normally ended
// them with its own reason already, so r reaches only logs). It doesn't wait for the PeerConnections: the actor
// closes them and then closes Done. Close is idempotent and safe from any goroutine.
func (c *Conn) Close(r EndReason) {
	if !c.closed.CompareAndSwap(false, true) {
		return
	}
	c.log.Info("connection left", "reason", string(r))
	c.sfu.removeConn(c, r)
	c.cancel()
	close(c.stop)
}

// ---- the actor ----

// command is one signal call waiting for the actor.
type command struct {
	ctx   context.Context
	fn    func(ctx context.Context) error
	res   chan error // capacity 1: the actor never blocks on it
	state atomic.Int32
}

// A command is claimed once: by the actor when it starts it, or by a caller whose context ended first.
const (
	cmdQueued int32 = iota
	cmdRunning
	cmdCanceled
)

// do runs fn on the actor and returns its result (02 §5.4). It waits for room in the command queue for at most 5 s,
// or until ctx ends: after that the call is sfu.busy (retryable) and nothing ran. A call whose context ends while it
// is still queued is sfu.busy too; once the actor has started fn, do waits for the result, so a call either ran
// completely or not at all. After Close every call is sfu.closed.
func (c *Conn) do(ctx context.Context, fn func(ctx context.Context) error) error {
	if c.closed.Load() {
		return errClosed("the connection")
	}
	cmd := &command{ctx: ctx, fn: fn, res: make(chan error, 1)}
	select {
	case c.cmds <- cmd:
	default:
		wait := time.NewTimer(c.sfu.commandWait)
		defer wait.Stop()
		select {
		case c.cmds <- cmd:
		case <-c.stop:
			return errClosed("the connection")
		case <-ctx.Done():
			return errBusy()
		case <-wait.C:
			return errBusy()
		}
	}
	select {
	case err := <-cmd.res:
		return err
	case <-c.done:
	case <-ctx.Done():
		if cmd.state.CompareAndSwap(cmdQueued, cmdCanceled) {
			return errBusy()
		}
		select { // the actor is running it: its Pion calls are short, and gathering is capped at 2 s
		case err := <-cmd.res:
			return err
		case <-c.done:
		}
	}
	// The actor has stopped. It may have finished this command just before.
	select {
	case err := <-cmd.res:
		return err
	default:
		return errClosed("the connection")
	}
}

// eventQueue is the actor's unbounded internal queue (02 §5.4). post never blocks, so a Pion callback, a timer or
// another Conn's actor can always hand work over.
type eventQueue struct {
	mu     sync.Mutex
	items  []func()
	closed bool
	wake   chan struct{} // capacity 1
}

// post queues fn for the actor; false once the queue is closed (the Conn is closing: fn is dropped).
func (q *eventQueue) post(fn func()) bool {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return false
	}
	q.items = append(q.items, fn)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return true
}

func (q *eventQueue) take() []func() {
	q.mu.Lock()
	defer q.mu.Unlock()
	items := q.items
	q.items = nil
	return items
}

func (q *eventQueue) close() {
	q.mu.Lock()
	q.closed, q.items = true, nil
	q.mu.Unlock()
}

// post hands fn to the Conn's actor without waiting. Pion callbacks, timers and other Conns use it, never the
// bounded command queue, so no actor ever waits on another (02 §5.4). Work posted to a closed Conn is dropped.
func (c *Conn) post(fn func()) { c.events.post(fn) }

// run is the actor: it serves the two queues until Close, then tears the Conn down.
func (c *Conn) run() {
	defer c.sfu.actorDone(c)
	defer close(c.done)
	for {
		select {
		case <-c.stop:
			c.teardown()
			return
		default:
		}
		select {
		case <-c.stop:
			c.teardown()
			return
		case <-c.events.wake:
			for _, fn := range c.events.take() {
				fn()
			}
		case cmd := <-c.cmds:
			if cmd.state.CompareAndSwap(cmdQueued, cmdRunning) {
				cmd.res <- cmd.fn(cmd.ctx)
			}
		}
	}
}

// teardown runs on the actor after Close: it drops the queued events, takes the Conn's DownTracks off their shares,
// which ends their writers, stops the timers of both PCs and closes the PCs, concurrently, waiting for the pub PC's
// track readers and the sub PC's RTCP readers. When it returns the Conn has no goroutine left but the actor.
// Commands still queued get sfu.closed from do.
func (c *Conn) teardown() {
	c.events.close()
	for id, sub := range c.subs {
		sub.detach()
		delete(c.subs, id)
	}
	c.markPubDown()
	var wg sync.WaitGroup
	if p := c.pub; p != nil {
		c.pub = nil
		p.stopTimers()
		wg.Go(func() { p.close(c.log) })
	}
	if s := c.sub; s != nil {
		c.sub = nil
		s.markClosed()
		wg.Go(func() { s.close(c.log) })
	}
	wg.Wait()
	// The writers end with their subscriptions, and at the latest with the Conn (c.stop). A write never blocks
	// (02 §5.4), so none of them is far from seeing it.
	c.writers.Wait()
}

// actorTimer is a timer whose work runs on a Conn's actor: the handshake timeout and the grace of a PC, the re-send
// of a sub offer. Like everything of the actor's it has no lock. Its time.Timer only posts to the actor's event
// queue; what the actor then finds stopped or set again doesn't run, so stopping one is final even when the timer
// has already fired.
type actorTimer struct {
	t   *time.Timer
	seq uint64 // counts the times the timer was set or stopped: a post from an earlier one finds another number
}

// after runs fn on the actor in d, in place of whatever t was waiting to run. A Conn that closes meanwhile drops it.
func (c *Conn) after(t *actorTimer, d time.Duration, fn func()) {
	t.stop()
	seq := t.seq
	t.t = time.AfterFunc(d, func() {
		c.post(func() {
			if t.seq != seq {
				return
			}
			t.stop()
			fn()
		})
	})
}

// stop keeps the timer's work from running, if it hasn't yet.
func (t *actorTimer) stop() {
	t.seq++
	if t.t != nil {
		t.t.Stop()
		t.t = nil
	}
}

// pending reports whether the timer's work is still to run.
func (t *actorTimer) pending() bool { return t.t != nil }

// ---- PeerConnection state, shared by both kinds ----

// pcState is what the actor keeps about one PeerConnection of either kind besides its negotiation (02 §5.3).
type pcState struct {
	// lastState is the last connection state onPCState handled, so that each state is reported once.
	lastState webrtc.PeerConnectionState
	// wasConnected: the PC has been connected at least once, so its handshake is done.
	wasConnected bool
	// handshake closes a new PC that isn't connected within 10 s (startHandshake). grace closes one that has stayed
	// failed for 30 s (startGrace).
	handshake, grace actorTimer
}

func (st *pcState) stopTimers() {
	st.handshake.stop()
	st.grace.stop()
}

// currentPC returns the actor's state of pc if pc is the Conn's current PeerConnection of a kind: nil for one that a
// higher gen replaced, and for a pub PC that has closed and gone (02 §5.3).
func (c *Conn) currentPC(kind PCKind, pc *webrtc.PeerConnection) *pcState {
	switch {
	case kind == PCPub && c.pub != nil && c.pub.pc == pc:
		return &c.pub.pcState
	case kind == PCSub && c.sub != nil && c.sub.pc == pc:
		return &c.sub.pcState
	}
	return nil
}

// onPCState handles a connection state change of pc. Its Pion callback posts it without the state: Pion runs every
// callback in a goroutine of its own, so two changes close together can reach the queue in either order. The state
// is read here instead, on the actor, and each one is handled once: what the Conn reports last is always the PC's
// real state, and a state that came and went between two posts is skipped. Only the current PC of a kind counts: one
// already replaced by a higher gen, or a pub PC that has closed and gone, sends nothing more (02 §5.3).
//
// What follows from each state is 02 §5.3's "PeerConnection state": connected ends the handshake and opens the
// DTLS-ready gate, with a keyframe request for what the PC carries; failed starts the 30 s grace, and a pub PC's
// shares are stalled; closed, when it is not the SFU's own doing, is Pion closing the PC because the client closed
// its side. The SFU never restarts ICE or rebuilds a PC because of a state: the client drives every recovery
// (01 §10.4).
func (c *Conn) onPCState(kind PCKind, gen uint32, pc *webrtc.PeerConnection) {
	st := c.currentPC(kind, pc)
	if st == nil {
		return
	}
	state := pc.ConnectionState()
	if state == st.lastState {
		return
	}
	st.lastState = state
	c.log.Info("PeerConnection state", "pc", kind.String(), "gen", gen, "state", state.String())
	if kind == PCSub {
		// The DTLS-ready gate (02 §9.3, S4 finding 1): Pion drops RTP written before DTLS is up, and RTP written to a
		// PC that has failed, so the DownTracks of this PC forward only while it can send.
		c.sub.follow(state)
	} else if state != webrtc.PeerConnectionStateConnected {
		c.markPubDown()
	}
	switch state {
	case webrtc.PeerConnectionStateConnected:
		st.wasConnected = true
		st.stopTimers()
		if kind == PCSub {
			// ICE has connected since the last restart's offer: the next request for a restart starts a new one.
			c.sub.restartAt = 0
			// The gate is open: each video DownTrack asks its publisher for a keyframe to start with, or to go on with
			// after what its viewer missed.
			for _, sub := range c.subs {
				sub.video.requestKeyframe()
			}
		} else {
			// Connected, for the first time or again after a drop or an ICE restart: what the viewers missed meanwhile
			// is gone, so every video layer of this PC gets a keyframe request (02 §9.7), and the first keyframe makes
			// a stalled share live again. On the first connect no track has arrived yet.
			c.pubUp.Store(true)
			now := monoNow()
			for _, t := range c.pub.tracks {
				if l := t.layer.Load(); l != nil {
					l.requestKeyframe(now)
				}
			}
		}
	case webrtc.PeerConnectionStateDisconnected:
		// Nothing: ICE often recovers by itself, and after 3 s the client restarts it (01 §10.4). A share whose pub PC
		// stays away for 2 s is stalled by the ticker (SFU.everySecond).
	case webrtc.PeerConnectionStateFailed:
		// A new PC whose ICE failed has had its handshake; the grace takes over from the handshake timeout.
		st.handshake.stop()
		if kind == PCSub {
			c.sub.restartAt = 0 // ICE has failed since the last restart's offer: a new restart may follow
		} else {
			c.stallShares("the pub PC failed")
		}
		c.startGrace(kind, gen, pc, st)
	case webrtc.PeerConnectionStateClosed:
		// A current PC that closes without a word from the SFU was closed by Pion, because the client closed its side
		// (a DTLS close_notify), or, for a sub PC, by subFatal after a fatal error. (A PC that the SFU replaces, or
		// closes because signal said so or a timer ran out, is no longer current when its state arrives, or has it as
		// its last state already.) A closed pub PC is gone: its tracks have ended, so its live shares are stalled,
		// and only an offer with a higher gen brings a new one. A closed sub PC stays, without further offers, until
		// the Conn needs one again and builds its successor (02 §5.3, the closed row).
		if kind == PCPub {
			c.dropPub()
		} else {
			c.sub.markClosed()
		}
	default:
		// New and connecting are not reported. A PC that is connecting again has had its ICE restarted: the grace of
		// its failed state is over, and if the restart fails, a new one starts.
		st.grace.stop()
		return
	}
	c.sig.SendEvent(PCStateEvent{PC: kind, Gen: gen, State: state.String()})
}

// markPubDown notes that the Conn has no connected pub PC from now on, if it had one: the ticker makes its live
// shares stalled after 2 s of that (02 §5.3).
func (c *Conn) markPubDown() {
	if c.pubUp.Load() {
		c.pubDownAt.Store(max(monoNow(), 1))
		c.pubUp.Store(false)
	}
}

// pubAway returns how long the Conn has had no connected pub PC, as its actor has seen it: 0 while the pub PC is
// connected, and before any was. The ticker asks it for the stalled check.
func (c *Conn) pubAway(now int64) time.Duration {
	if c.pubUp.Load() {
		return 0
	}
	if at := c.pubDownAt.Load(); at != 0 {
		return time.Duration(now - at)
	}
	return 0
}

// stallShares makes the live shares the Conn publishes stalled at once (02 §5.3): its pub PC has failed.
func (c *Conn) stallShares(cause string) {
	c.room.mu.Lock()
	var shares []*Share
	for _, sh := range c.part.shares {
		if sh.conn == c {
			shares = append(shares, sh)
		}
	}
	c.room.mu.Unlock()
	sortShares(shares)
	for _, sh := range shares {
		sh.stall(cause)
	}
}

// dropPub closes the Conn's pub PC, if it has one, and leaves the Conn without: the tracks have ended, so the shares
// they fed lose their layers and the live ones are stalled (Share.detach). The gen stays, so an offer for it is
// sfu.bad_pc from now on and only a higher gen brings a new pub PC. Nothing more is reported about the closed PC.
func (c *Conn) dropPub() {
	p := c.pub
	if p == nil {
		return
	}
	c.pub = nil
	c.cands[PCPub] = remoteCandidates{gen: c.pubGen} // the PC's candidates go with it
	c.markPubDown()
	p.close(c.log)
}

// closeSub closes the Conn's sub PC by the SFU's own decision (a timer ran out, or signal said so) and leaves it in
// the closed state of 02 §5.3, without the closed event that a PC closed by its client gets: whoever calls it
// reports what there is to report.
func (c *Conn) closeSub(s *subPC) {
	s.lastState = webrtc.PeerConnectionStateClosed
	s.close(c.log)
}

// startHandshake gives a new PeerConnection 10 s to get connected (02 §5.3, §12), from the moment its client can
// start: the first answer on either kind. An ICE restart starts no such timer.
func (c *Conn) startHandshake(kind PCKind, gen uint32, pc *webrtc.PeerConnection, st *pcState) {
	if st.wasConnected {
		return
	}
	c.after(&st.handshake, c.sfu.timing.handshake, func() { c.handshakeExpired(kind, gen, pc) })
}

// handshakeExpired closes a new PeerConnection that isn't connected 10 s after its first answer, and reports it as
// failed with PCReasonHandshakeTimeout: for a pub PC the client is then asked for a new one (01 §10.4); a sub PC is
// in the closed state, and its successor follows the client's own rebuild request.
func (c *Conn) handshakeExpired(kind PCKind, gen uint32, pc *webrtc.PeerConnection) {
	st := c.currentPC(kind, pc)
	if st == nil || st.wasConnected {
		return
	}
	if pc.ConnectionState() == webrtc.PeerConnectionStateConnected {
		c.onPCState(kind, gen, pc) // connected just now: its callback is still on its way
		return
	}
	c.log.Info("PeerConnection closed: not connected in time", "pc", kind.String(), "gen", gen,
		"timeout", c.sfu.timing.handshake)
	c.sfu.handshakeTimeouts[kind].Add(1)
	if kind == PCPub {
		c.dropPub()
	} else {
		c.closeSub(c.sub)
	}
	c.sig.SendEvent(PCStateEvent{
		PC: kind, Gen: gen, State: webrtc.PeerConnectionStateFailed.String(), Reason: PCReasonHandshakeTimeout,
	})
}

// startGrace gives a failed PeerConnection 30 s for an ICE restart or a rebuild (02 §5.3, §12).
func (c *Conn) startGrace(kind PCKind, gen uint32, pc *webrtc.PeerConnection, st *pcState) {
	c.after(&st.grace, c.sfu.timing.grace, func() { c.graceExpired(kind, gen, pc) })
}

// graceExpired closes a PeerConnection that is still failed 30 s after it failed, and reports it as closed with
// PCReasonGraceExpired. The shares of a pub PC stay stalled: the hub's own 30 s timeout ends them (02 §12).
func (c *Conn) graceExpired(kind PCKind, gen uint32, pc *webrtc.PeerConnection) {
	if c.currentPC(kind, pc) == nil {
		return
	}
	if pc.ConnectionState() != webrtc.PeerConnectionStateFailed {
		c.onPCState(kind, gen, pc) // an ICE restart got in just now: its callback is still on its way
		return
	}
	c.log.Info("PeerConnection closed: failed for too long", "pc", kind.String(), "gen", gen, "grace", c.sfu.timing.grace)
	if kind == PCPub {
		c.dropPub()
	} else {
		c.closeSub(c.sub)
	}
	c.sig.SendEvent(PCStateEvent{
		PC: kind, Gen: gen, State: webrtc.PeerConnectionStateClosed.String(), Reason: PCReasonGraceExpired,
	})
}

// ---- the PeerConnections a client makes the Conn create (02 §12) ----

// pcWindow holds the times of the last maxPCCreations PeerConnections of one kind that a client made a Conn create:
// a ring, the oldest at i once it is full.
type pcWindow struct {
	at [maxPCCreations]int64 // monoNow of each
	n  int                   // how many are held
	i  int                   // the oldest, once n is maxPCCreations
}

// admit counts one more creation at now if fewer than maxPCCreations fall within the window before it. Otherwise it
// counts nothing, and wait is the time until the oldest of them leaves the window.
func (w *pcWindow) admit(now int64, window time.Duration) (wait time.Duration, ok bool) {
	if w.n < len(w.at) {
		w.at[w.n] = now
		w.n++
		return 0, true
	}
	if age := time.Duration(now - w.at[w.i]); age < window {
		return window - age, false
	}
	w.at[w.i] = now
	w.i = (w.i + 1) % len(w.at)
	return 0, true
}

// admitPC counts a PeerConnection of a kind that the client is making the Conn create, or returns
// sfu.pc_rate_limited when it is one too many within a minute.
func (c *Conn) admitPC(kind PCKind) error {
	if wait, ok := c.pcs[kind].admit(monoNow(), c.sfu.timing.pcWindow); !ok {
		c.log.Debug("PeerConnection not created: too many within the window", "pc", kind.String(), "retry_after", wait)
		return errPCRateLimited(kind, wait)
	}
	return nil
}

// ---- remote ICE candidates (01 §9 rule 5, 02 §7.3, §12) ----

// remoteCandidates are the client's candidates for the PC of one kind and gen. The first 64 that pass the
// remote-candidate filter are admitted, whether they are trickled or come in an SDP, and whether Pion has them
// already or they are still buffered; the others are dropped. What counts as one candidate is what Pion keeps as one
// (candidateKey): Pion has no cap of its own, so every candidate that goes to it is counted here. A candidate that
// was admitted before costs nothing when it comes again, so the candidates a client repeats in every re-offer, or
// after an ICE restart, don't use the 64 up. Trickled candidates are buffered until that gen's PC has its remote
// description.
type remoteCandidates struct {
	gen     uint32
	seen    map[candidateKey]struct{}
	pending []webrtc.ICECandidateInit
}

// count returns how many candidates have been admitted.
func (r *remoteCandidates) count() int { return len(r.seen) }

// admit reports whether the candidate with key k may go to Pion: one of the first 64, or one admitted before.
func (r *remoteCandidates) admit(k candidateKey) bool {
	ok, _ := r.admitNew(k)
	return ok
}

// admitNew is admit that also says whether the candidate is new, which costs one of the 64.
func (r *remoteCandidates) admitNew(k candidateKey) (ok, fresh bool) {
	if _, seen := r.seen[k]; seen {
		return true, false
	}
	if len(r.seen) >= maxRemoteCandidates {
		return false, false
	}
	if r.seen == nil {
		r.seen = map[candidateKey]struct{}{}
	}
	r.seen[k] = struct{}{}
	return true, true
}

// forGen returns a copy of r to judge the candidates of a remote description of a gen with: r itself when it is of
// that gen, with the candidates that arrived ahead of the description still buffered, else an empty one. The caller
// makes it the Conn's (commitCandidates) once Pion has taken the description, so a description that is refused
// admits nothing.
func (r *remoteCandidates) forGen(gen uint32) remoteCandidates {
	if r.gen != gen {
		return remoteCandidates{gen: gen}
	}
	return remoteCandidates{gen: gen, seen: maps.Clone(r.seen), pending: slices.Clone(r.pending)}
}

// AddICECandidate adds a candidate the client trickled for the PC of a kind and gen. Candidates the remote-candidate
// filter drops (02 §7.3), those of an older gen, the end-of-candidates marker and everything past the first 64
// candidates per PC and gen are ignored without an error. A candidate that arrives before its PC's remote
// description is buffered.
func (c *Conn) AddICECandidate(ctx context.Context, pc PCKind, gen uint32, cand webrtc.ICECandidateInit) error {
	return c.do(ctx, func(context.Context) error { return c.addICECandidate(pc, gen, cand) })
}

func (c *Conn) addICECandidate(kind PCKind, gen uint32, cand webrtc.ICECandidateInit) error {
	if kind != PCPub && kind != PCSub {
		return newError(CodeBadPC, "unknown PC kind")
	}
	if strings.TrimPrefix(cand.Candidate, "candidate:") == "" {
		return nil // end-of-candidates: the SFU needs no marker
	}
	key, reason := c.sfu.apis.filter.judgeTrickled(cand)
	if reason != keepCandidate {
		c.log.Debug("remote candidate dropped", "pc", kind.String(), "reason", string(reason))
		return nil
	}
	var (
		buf    *remoteCandidates
		target *webrtc.PeerConnection // nil: the candidate waits for its gen's offer
	)
	switch kind {
	case PCPub:
		switch {
		case gen == 0 || gen < c.pubGen:
			return nil // an older gen
		case gen == c.pubGen:
			if c.pub == nil {
				return nil // its PC has closed
			}
			buf, target = &c.cands[PCPub], c.pub.pc
		default:
			// The client owns the pub gen, so a candidate may name a gen whose offer hasn't arrived yet. It waits in
			// a buffer of its own: whatever gens a client names, the candidates of the pub PC it has stay that PC's,
			// and count toward its 64. The buffer is the newest such gen's; older ones are over.
			if gen < c.pubAhead.gen {
				return nil
			}
			if gen > c.pubAhead.gen {
				c.pubAhead = remoteCandidates{gen: gen}
			}
			buf = &c.pubAhead
		}
	case PCSub:
		if c.sub == nil || gen != c.sub.gen || c.sub.closed {
			return nil
		}
		buf, target = &c.cands[PCSub], c.sub.pc
	}
	ok, fresh := buf.admitNew(key)
	if !ok {
		c.log.Debug("remote candidate dropped", "pc", kind.String(), "reason", "limit")
		return nil
	}
	if target == nil || target.RemoteDescription() == nil {
		if fresh { // one that was admitted before is waiting already
			buf.pending = append(buf.pending, cand)
		}
		return nil
	}
	// Also one admitted before: after an ICE restart Pion has forgotten the candidates it had.
	c.applyCandidate(kind, target, cand)
	return nil
}

// commitCandidates makes budget, the candidates admitted with a remote description that Pion has just taken, the
// candidates of pc, the Conn's current PC of the kind, and gives Pion the trickled ones that waited for the
// description. A pub PC of a new gen takes over what was trickled ahead of its offer: that buffer, and one of a gen
// the client has passed by, are done with.
func (c *Conn) commitCandidates(kind PCKind, budget remoteCandidates, pc *webrtc.PeerConnection) {
	pending := budget.pending
	budget.pending = nil
	c.cands[kind] = budget
	if kind == PCPub && c.pubAhead.gen <= budget.gen {
		c.pubAhead = remoteCandidates{}
	}
	for _, cand := range pending {
		c.applyCandidate(kind, pc, cand)
	}
}

// applyCandidate gives Pion one filtered candidate. A candidate Pion refuses (it names an m-section the description
// doesn't have, say) is dropped like a filtered one: the client's other candidates and peer-reflexive ones still
// connect the PC.
func (c *Conn) applyCandidate(kind PCKind, pc *webrtc.PeerConnection, cand webrtc.ICECandidateInit) {
	if err := pc.AddICECandidate(cand); err != nil {
		c.log.Debug("remote candidate dropped", "pc", kind.String(), "reason", "refused", "err", err)
	}
}

// ---- pc.restart, pc.close and the resume (02 §5.3, §6.5) ----

// RestartICE restarts ICE on the sub PC, because the client asked (01's pc.restart{sub, ice}): the next sub offer,
// with the same gen and the next neg, carries new ICE credentials and candidates, and follows the answer to an offer
// that is still outstanding. While a restart is under way it returns nil and offers nothing new (02 §5.3): one that
// waits for its offer, one whose offer has no answer yet, and one whose offer went out less than 5 s ago with ICE
// neither connected nor failed since. A sub PC that has closed is rebuilt instead, as ResetPC would, and counts as a
// PC the client caused (sfu.pc_rate_limited after 10 in a minute). A gen lower than the current one is ignored.
// It is sfu.bad_pc for the pub PC, whose ICE the client restarts with an offer of its own, and for a sub PC the
// Conn doesn't have.
func (c *Conn) RestartICE(ctx context.Context, pc PCKind, gen uint32) error {
	return c.do(ctx, func(context.Context) error { return c.restartICE(pc, gen) })
}

func (c *Conn) restartICE(kind PCKind, gen uint32) error {
	s, err := c.subPCOf("RestartICE", kind, gen)
	if s == nil {
		return err
	}
	if s.closed {
		return c.rebuildClosedSub("an ICE restart was asked of a closed PC")
	}
	c.restartSubICE(s, "the client asked")
	return nil
}

// ResetPC closes the sub PC and builds a new one with gen + 1 and every subscription, because the client asked
// (01's pc.restart{sub, rebuild}): the new PC's first offer goes out at once. It is the same from the closed state.
// A Conn without subscriptions gets no new PC: its sub PC is closed, and the next subscription builds the successor.
// It returns sfu.pc_rate_limited, with the old PC untouched, for more than 10 sub PCs a client caused within a
// minute. A gen lower than the current one is ignored. It is sfu.bad_pc for the pub PC, which the client rebuilds
// with an offer of a new gen, and for a sub PC the Conn doesn't have.
func (c *Conn) ResetPC(ctx context.Context, pc PCKind, gen uint32) error {
	return c.do(ctx, func(context.Context) error { return c.resetPC(pc, gen) })
}

func (c *Conn) resetPC(kind PCKind, gen uint32) error {
	s, err := c.subPCOf("ResetPC", kind, gen)
	if s == nil {
		return err
	}
	if len(c.subs) == 0 {
		if !s.closed {
			c.log.Info("sub PC closed: reset without subscriptions", "gen", s.gen)
			c.closeSub(s)
		}
		return nil
	}
	if err := c.admitPC(PCSub); err != nil {
		return err
	}
	return c.rebuildSub("the client asked")
}

// subPCOf returns the sub PC that a RestartICE or ResetPC with these arguments is about. It is nil with sfu.bad_pc
// for another kind or a PC the Conn doesn't have, and nil without an error for a gen below the current one, which
// is ignored (02 §5.3).
func (c *Conn) subPCOf(call string, kind PCKind, gen uint32) (*subPC, error) {
	switch s := c.sub; {
	case kind != PCSub:
		return nil, newError(CodeBadPC, call+" is only valid for the sub PC")
	case s == nil || gen > s.gen:
		return nil, newError(CodeBadPC, "there is no such sub PC")
	case gen < s.gen:
		c.log.Debug(call+" ignored: an older gen", "gen", gen, "current", s.gen)
		return nil, nil
	default:
		return s, nil
	}
}

// ClosePC closes a PC that the client closed on purpose (01's pc.close). For the pub PC the hub has ended the Conn's
// shares first; the Conn has no pub PC afterwards, and its gen is over: an offer for it is sfu.bad_pc from now on,
// and the next gen brings a new PC. The client may name a gen the SFU never had (an offer that was refused, say):
// the pub PC the Conn has is then an older one, which the client has left behind, and goes all the same. The sub PC
// goes to the closed state of 02 §5.3 and is rebuilt when the Conn needs one again. A PC that is closed already
// (Pion closes it when the client's DTLS close_notify arrives, often before pc.close does) and a gen lower than
// the current one are ignored. Nothing is reported about a PC closed here. It is sfu.bad_pc for an unknown kind and
// for a sub gen the Conn doesn't have.
func (c *Conn) ClosePC(ctx context.Context, pc PCKind, gen uint32) error {
	return c.do(ctx, func(context.Context) error { return c.closePC(pc, gen) })
}

func (c *Conn) closePC(kind PCKind, gen uint32) error {
	switch kind {
	case PCPub:
		if gen < c.pubGen {
			c.log.Debug("ClosePC ignored: an older gen", "pc", kind.String(), "gen", gen, "current", c.pubGen)
			return nil
		}
		if c.pub != nil {
			c.log.Info("pub PC closed by the client", "gen", c.pub.gen)
			c.dropPub() // with its candidates
		}
		// The candidates that waited for an offer of a gen that is now closed wait for nothing.
		if c.pubAhead.gen <= gen {
			c.pubAhead = remoteCandidates{}
		}
		return nil
	case PCSub:
		s := c.sub
		switch {
		case s == nil || gen > s.gen:
			return newError(CodeBadPC, "there is no such sub PC")
		case gen < s.gen:
			c.log.Debug("ClosePC ignored: an older gen", "pc", kind.String(), "gen", gen, "current", s.gen)
		case !s.closed:
			c.log.Info("sub PC closed by the client", "gen", s.gen)
			c.closeSub(s)
		}
		return nil
	}
	return newError(CodeBadPC, "unknown PC kind")
}

// Resync is called when the Conn's WebSocket has resumed (02 §6.5, 01 §10.5): signal may have dropped what the SFU
// sent meanwhile, so the SFU sends what matters again. It never blocks: the actor does the work when it gets to it.
//
//   - A sub offer that is still outstanding goes out again, with the same gen and neg.
//   - A sub PC that isn't connected gets an ICE restart, unless one is under way (RestartICE); one that has closed
//     is rebuilt, if the Conn has subscriptions. A rebuild that is one PC too many within a minute (02 §12) is
//     reported as an ErrorEvent about the sub PC, with sfu.pc_rate_limited and the time to wait: Resync has no
//     error to return, and the client has to ask again.
//   - Every subscription's current SubscriptionStateEvent.
//   - One CodecPolicyEvent per share the Conn publishes, with the room's codec policy: a client re-offers only when
//     that differs from the profile it last applied (05).
//
// The last QualityHintEvent per share follows with README S88, which sends the first ones.
func (c *Conn) Resync() {
	c.post(c.resync)
}

func (c *Conn) resync() {
	c.log.Debug("resync")
	switch s := c.sub; {
	case s == nil:
	case s.closed:
		c.rebuildClosedSubForEvent("the connection resumed")
	default:
		if s.offering {
			c.sendSubOffer(s)
		}
		if s.pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
			c.restartSubICE(s, "the connection resumed")
		}
	}
	for _, id := range slices.Sorted(maps.Keys(c.subs)) {
		c.resendSubscription(c.subs[id])
	}
	if !c.role.canPublish() {
		return
	}
	policy := c.room.codecPolicy()
	for _, info := range c.room.shareInfos() {
		if info.Conn == c.id {
			c.sig.SendEvent(CodecPolicyEvent{Share: info.ID, Profile: policy})
		}
	}
}

// ---- methods of later slices (README §4 "Interfaces first") ----

// SetDecodeCaps replaces the viewer's decode capabilities (01's caps.update): the room's codec policy follows, and a
// viewer that gains H.264 gets a new sub PC with video (02 §8.3, §8.5). README S69 implements it; until then it
// returns an error that wraps ErrNotImplemented and the caps stay as joined.
func (c *Conn) SetDecodeCaps(ctx context.Context, caps DecodeCaps) error {
	return c.do(ctx, func(context.Context) error { return errNotImplemented("Conn.SetDecodeCaps", "S69") })
}
