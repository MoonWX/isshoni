package sfu

import (
	"context"
	"log/slog"
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

	// actor state
	pub    *pubPC // nil until the first pub offer
	pubGen uint32 // the highest pub gen accepted; 0 before the first
	sub    *subPC // nil until the first subscription
	subGen uint32 // the gen of the newest sub PC; 0 before the first
	subs   map[ShareID]*Subscription
	cands  [PCSub + 1]remoteCandidates // by PCKind
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

// teardown runs on the actor after Close: it drops the queued events, takes the Conn's DownTracks off their shares
// and closes both PCs, concurrently, waiting for the pub PC's readers. Commands still queued get sfu.closed from do.
func (c *Conn) teardown() {
	c.events.close()
	for id, sub := range c.subs {
		sub.detach()
		delete(c.subs, id)
	}
	var wg sync.WaitGroup
	if p := c.pub; p != nil {
		c.pub = nil
		wg.Go(func() { p.close(c.log) })
	}
	if s := c.sub; s != nil {
		c.sub = nil
		wg.Go(func() { s.close(c.log) })
	}
	wg.Wait()
}

// ---- PeerConnection state, shared by both kinds ----

// onPCState handles a connection state change of pc, posted by its Pion callback. Only the current PC of a kind
// counts: one already replaced by a higher gen, or closed, sends no event (02 §5.3).
func (c *Conn) onPCState(kind PCKind, gen uint32, pc *webrtc.PeerConnection, state webrtc.PeerConnectionState) {
	switch {
	case kind == PCPub && (c.pub == nil || c.pub.pc != pc):
		return
	case kind == PCSub && (c.sub == nil || c.sub.pc != pc):
		return
	}
	c.log.Info("PeerConnection state", "pc", kind.String(), "gen", gen, "state", state.String())
	switch state {
	case webrtc.PeerConnectionStateConnected:
		if kind == PCSub {
			// The DTLS-ready gate (02 §9.3): Pion drops RTP written before DTLS is up, so DownTracks of this PC forward
			// only from now on. README S41 adds the keyframe requests that go with it.
			c.sub.ready.Store(true)
		}
	case webrtc.PeerConnectionStateDisconnected, webrtc.PeerConnectionStateFailed:
	case webrtc.PeerConnectionStateClosed:
		// The SFU didn't close this PC (it would no longer be the current one): Pion did, because the client closed
		// its side (a DTLS close_notify). A closed pub PC is gone: its tracks have ended, and only an offer with a
		// higher gen brings a new one. A closed sub PC stays, without further offers, until README S57 rebuilds it
		// from closed (02 §5.3); until then new subscriptions on this Conn fail with sfu.internal.
		if kind == PCPub {
			p := c.pub
			c.pub = nil
			p.close(c.log)
		} else {
			c.sub.closed = true
		}
	default:
		return // new and connecting are not reported
	}
	// The handshake timer, the grace period after failed and the rest of 02 §5.3's PC states are README S57's.
	c.sig.SendEvent(PCStateEvent{PC: kind, Gen: gen, State: state.String()})
}

// ---- remote ICE candidates (01 §9 rule 5, 02 §7.3, §12) ----

// remoteCandidates are the client's trickled candidates for one PC kind: those of the newest gen seen, counted
// whether buffered or applied, and buffered until that gen's PC has its remote description.
type remoteCandidates struct {
	gen     uint32
	count   int
	pending []webrtc.ICECandidateInit
}

// AddICECandidate adds a candidate the client trickled for the PC of a kind and gen. Candidates the remote-candidate
// filter drops (02 §7.3), those of an older gen, the end-of-candidates marker and everything past the first 64 per
// PC and gen are ignored without an error. A candidate that arrives before its PC's remote description is buffered.
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
	if reason := c.sfu.apis.filter.trickled(cand); reason != keepCandidate {
		c.log.Debug("remote candidate dropped", "pc", kind.String(), "reason", string(reason))
		return nil
	}
	buf := &c.cands[kind]
	var target *webrtc.PeerConnection
	switch kind {
	case PCPub:
		// The client owns the pub gen, so a candidate may name a gen whose offer hasn't arrived yet: it starts that
		// gen's buffer, and older gens are over.
		if gen == 0 || gen < c.pubGen || gen < buf.gen {
			return nil
		}
		if gen > buf.gen {
			*buf = remoteCandidates{gen: gen}
		}
		if c.pub != nil && c.pub.gen == gen {
			target = c.pub.pc
		}
	case PCSub:
		if c.sub == nil || gen != c.sub.gen {
			return nil
		}
		target = c.sub.pc
	}
	if buf.count >= maxRemoteCandidates {
		c.log.Debug("remote candidate dropped", "pc", kind.String(), "reason", "limit")
		return nil
	}
	buf.count++
	if target == nil || target.RemoteDescription() == nil {
		buf.pending = append(buf.pending, cand)
		return nil
	}
	c.applyCandidate(kind, target, cand)
	return nil
}

// resetCandidates starts the candidate buffer of a new PC, keeping candidates that arrived ahead of its offer.
func (c *Conn) resetCandidates(kind PCKind, gen uint32) {
	if c.cands[kind].gen != gen {
		c.cands[kind] = remoteCandidates{gen: gen}
	}
}

// flushCandidates applies the buffered candidates of a PC that now has its remote description.
func (c *Conn) flushCandidates(kind PCKind, gen uint32, pc *webrtc.PeerConnection) {
	buf := &c.cands[kind]
	if buf.gen != gen {
		return
	}
	pending := buf.pending
	buf.pending = nil
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

// ---- methods of later slices (README §4 "Interfaces first") ----

// RestartICE restarts ICE on the sub PC (for pub the client re-offers); a closed sub PC is rebuilt instead, and it
// returns nil without a new offer while a restart is under way (02 §5.3). A gen lower than the current one is
// ignored. README S57 implements it; until then it returns an error that wraps ErrNotImplemented.
func (c *Conn) RestartICE(ctx context.Context, pc PCKind, gen uint32) error {
	return c.do(ctx, func(context.Context) error { return errNotImplemented("Conn.RestartICE", "S57") })
}

// ResetPC closes the sub PC and builds a new one with gen + 1 and every subscription, because the client asked. A
// gen lower than the current one is ignored. README S57 implements it; until then it returns an error that wraps
// ErrNotImplemented.
func (c *Conn) ResetPC(ctx context.Context, pc PCKind, gen uint32) error {
	return c.do(ctx, func(context.Context) error { return errNotImplemented("Conn.ResetPC", "S57") })
}

// ClosePC closes a PC that the client closed on purpose (01's pc.close). A gen lower than the current one is
// ignored. README S57 implements it; until then it returns an error that wraps ErrNotImplemented.
func (c *Conn) ClosePC(ctx context.Context, pc PCKind, gen uint32) error {
	return c.do(ctx, func(context.Context) error { return errNotImplemented("Conn.ClosePC", "S57") })
}

// SetDecodeCaps replaces the viewer's decode capabilities (01's caps.update): the room's codec policy follows, and a
// viewer that gains H.264 gets a new sub PC with video (02 §8.3, §8.5). README S69 implements it; until then it
// returns an error that wraps ErrNotImplemented and the caps stay as joined.
func (c *Conn) SetDecodeCaps(ctx context.Context, caps DecodeCaps) error {
	return c.do(ctx, func(context.Context) error { return errNotImplemented("Conn.SetDecodeCaps", "S69") })
}

// Resync is called when the Conn's WebSocket has resumed: the SFU sends an outstanding sub offer and the current
// states again and ICE-restarts a sub PC that isn't connected (02 §6.5). README S57 implements it; until then it
// only logs, at debug.
func (c *Conn) Resync() {
	c.log.Debug("Resync is not implemented yet (README S57)")
}
