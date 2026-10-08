package sfu

import (
	"cmp"
	"log/slog"
	"slices"
	"sync"
	"time"
)

// SFU is the media server of one isshoni process (02 §5.1): rooms with their participants, Conns and shares, on the
// publish and subscribe webrtc.APIs that New builds on 04's netx.Transport. Signal drives it through 01's sfuplane:
// Join returns a Conn, and every later call goes to that Conn or names a room or a share.
type SFU struct {
	log    *slog.Logger
	events RoomEvents
	apis   *apis
	// pauseUnwatched is Config.PauseUnwatchedLayers, for the uplink policy (README S88).
	pauseUnwatched bool
	// commandWait is how long a signal call waits for room in a Conn's command queue: commandWait (5 s), shorter in
	// tests. It is set before the first Join and never changes.
	commandWait time.Duration

	// The ticker (02 §5.4): one goroutine, started by the first Join and stopped by Close. kick wakes it at once for a
	// share's state change, tickStop ends it and tickDone says that it has ended.
	kick     chan struct{} // capacity 1
	tickStop chan struct{}
	tickDone chan struct{}

	mu      sync.Mutex // guards the fields below; lock order: SFU.mu → Room.mu → Share.mu → DownTrack.mu (doc.go)
	closed  bool
	ticking bool // the ticker goroutine was started
	limits  Limits
	rooms   map[RoomID]*Room
	conns   map[ConnID]*Conn   // joined and not closed
	shares  map[ShareID]*Share // every share that hasn't ended
	// actors are the Conns whose actor goroutine still runs: the joined ones, and closed ones until their PCs are
	// closed. Close waits for them.
	actors map[*Conn]struct{}
}

// New checks cfg and builds the SFU's APIs on cfg.Transport, whose sockets 04 has already bound. It starts no
// goroutine: the first Join starts the ticker, and each Join its Conn's actor.
func New(cfg Config, deps Deps) (*SFU, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	log := deps.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	log = log.With("component", "sfu")
	a, err := newAPIs(cfg.Transport, log)
	if err != nil {
		return nil, err
	}
	events := deps.Events
	if events == nil {
		events = noRoomEvents{}
	}
	return &SFU{
		log:            log,
		events:         events,
		apis:           a,
		pauseUnwatched: cfg.PauseUnwatchedLayers,
		commandWait:    commandWait,
		kick:           make(chan struct{}, 1),
		tickStop:       make(chan struct{}),
		tickDone:       make(chan struct{}),
		limits:         cfg.Limits,
		rooms:          map[RoomID]*Room{},
		conns:          map[ConnID]*Conn{},
		shares:         map[ShareID]*Share{},
		actors:         map[*Conn]struct{}{},
	}, nil
}

// Ready returns nil once the APIs are built, which is as soon as New has returned, and sfu.closed after Close. 04's
// /readyz asks it.
func (s *SFU) Ready() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errClosed("the SFU")
	}
	return nil
}

// Close ends every share with server_shutdown, closes every Conn, their PeerConnections concurrently, and stops the
// ticker. It returns within 1 s: Conns still closing their PCs then are abandoned (they finish on their own) and
// counted in a warning. It is safe to call twice and never closes the Transport, which is 04's.
func (s *SFU) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	ticking := s.ticking
	shares := make([]*Share, 0, len(s.shares))
	for _, sh := range s.shares {
		shares = append(shares, sh)
	}
	conns := make([]*Conn, 0, len(s.conns))
	for _, c := range s.conns {
		conns = append(conns, c)
	}
	actors := make([]*Conn, 0, len(s.actors))
	for c := range s.actors {
		actors = append(actors, c)
	}
	s.mu.Unlock()

	sortShares(shares)
	for _, sh := range shares {
		s.endShare(sh, EndReasonServerShutdown)
	}
	for _, c := range conns {
		c.Close(EndReasonServerShutdown)
	}
	close(s.tickStop)

	deadline := time.NewTimer(closeTimeout)
	defer deadline.Stop()
	if ticking {
		select {
		case <-s.tickDone:
		case <-deadline.C:
			// Only a RoomEvents call that blocks, against its contract, can keep the ticker this long.
			s.log.Warn("the ticker was still reporting when the SFU closed", "timeout", closeTimeout)
			return nil
		}
	}
	for i, c := range actors {
		select {
		case <-c.done:
		case <-deadline.C:
			left := 0
			for _, rest := range actors[i:] {
				select {
				case <-rest.done:
				default:
					left++
				}
			}
			if left > 0 {
				s.log.Warn("connections were still closing their PeerConnections when the SFU closed",
					"count", left, "timeout", closeTimeout)
			}
			return nil
		}
	}
	return nil
}

// ---- the ticker (02 §5.4) ----

// runTicker is the SFU's ticker goroutine. Every 250 ms, and at once when a share changes state (kick), it makes the
// ShareUpdated calls that are due; once a second it turns the layers' counters into rates and sizes their packet
// caches. Later slices add the rest of 02 §5.4's ticker work (README S57, S69, S84, S88).
func (s *SFU) runTicker() {
	defer close(s.tickDone)
	tick := time.NewTicker(tickInterval)
	defer tick.Stop()
	for n := 1; ; {
		select {
		case <-s.tickStop:
			return
		case <-s.kick:
		case <-tick.C:
			if n%statsEvery == 0 {
				s.everySecond(monoNow())
			}
			n++
		}
		s.reportShares(monoNow())
	}
}

// liveShares returns the shares that haven't ended.
func (s *SFU) liveShares() []*Share {
	s.mu.Lock()
	defer s.mu.Unlock()
	shares := make([]*Share, 0, len(s.shares))
	for _, sh := range s.shares {
		shares = append(shares, sh)
	}
	return shares
}

// reportShares makes the ShareUpdated calls that are due (Share.takeUpdates). Only the ticker goroutine calls it, so
// one share's reports keep their order, and Share.notify keeps them ahead of its ShareEnded.
func (s *SFU) reportShares(now int64) {
	for _, sh := range s.liveShares() {
		sh.notify.Lock()
		for _, info := range sh.takeUpdates(now) {
			s.events.ShareUpdated(info.Room, info)
		}
		sh.notify.Unlock()
	}
}

// everySecond is the ticker's once-a-second work on the media path: each layer's rates and cache size (02 §9.1).
func (s *SFU) everySecond(now int64) {
	for _, sh := range s.liveShares() {
		for _, l := range sh.attached() {
			l.tick(now)
		}
	}
}

// shareLive runs when a share's first keyframe has made it live (the layer's read loop calls it): the ticker reports
// the state change at once.
func (s *SFU) shareLive(sh *Share) {
	s.log.Info("share live", "share_id", string(sh.id), "room_id", string(sh.room.id), "conn_id", string(sh.conn.id))
	select {
	case s.kick <- struct{}{}:
	default: // a kick is already waiting, and the ticker looks at every share when it takes it
	}
}

// errCaller is the error of a call that breaks the API's contract (an empty id, a nil Signaler, a share id used
// twice): a bug in the caller, so sfu.internal and not retryable.
func errCaller(msg string) *Error { return &Error{Code: CodeInternal, msg: msg} }

// Join adds a connection to a room, creating the room and the participant as needed, and starts the Conn's actor.
// The ids are the hub's; a Conn id can be joined once at a time, and all Conns of a participant belong to one user.
func (s *SFU) Join(p JoinParams) (*Conn, error) {
	switch {
	case p.Room == "" || p.Participant == "" || p.User == "" || p.Conn == "":
		return nil, errCaller("Join: an id is empty")
	case !p.Role.valid():
		return nil, errCaller("Join: unknown role")
	case !p.Client.valid():
		return nil, errCaller("Join: unknown client kind")
	case p.Signaler == nil:
		return nil, errCaller("Join: no Signaler")
	}
	c, err := s.addConn(p)
	if err != nil {
		return nil, err
	}
	c.log.Info("connection joined", "role", c.role.String(), "client", c.client.String())
	return c, nil
}

// addConn registers a new Conn in its room and starts its actor.
func (s *SFU) addConn(p JoinParams) (*Conn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errClosed("the SFU")
	}
	if _, dup := s.conns[p.Conn]; dup {
		return nil, errCaller("Join: the connection id is already joined")
	}
	room := s.rooms[p.Room]
	if room == nil {
		room = newRoom(p.Room)
	}
	room.mu.Lock()
	part := room.participants[p.Participant]
	if part != nil && part.user != p.User {
		room.mu.Unlock()
		return nil, errCaller("Join: the participant belongs to another user")
	}
	if part == nil {
		part = newParticipant(room, p.Participant, p.User)
		room.participants[part.id] = part
	}
	c := newConn(s, room, part, p)
	room.conns[c.id] = c
	part.conns[c.id] = c
	room.mu.Unlock()
	s.rooms[room.id] = room
	s.conns[c.id] = c
	s.actors[c] = struct{}{}
	go c.run()
	if !s.ticking {
		s.ticking = true
		go s.runTicker()
	}
	return c, nil
}

// removeConn takes a closing Conn out of its room, removes the participant and the room when they are empty, and
// ends the shares the Conn publishes with r.
func (s *SFU) removeConn(c *Conn, r EndReason) {
	s.mu.Lock()
	if s.conns[c.id] == c {
		delete(s.conns, c.id)
	}
	room := c.room
	room.mu.Lock()
	delete(room.conns, c.id)
	delete(c.part.conns, c.id)
	var shares []*Share
	for _, sh := range c.part.shares {
		if sh.conn == c {
			shares = append(shares, sh)
		}
	}
	if len(c.part.conns) == 0 {
		delete(room.participants, c.part.id)
	}
	empty := len(room.conns) == 0
	room.mu.Unlock()
	if empty && s.rooms[room.id] == room {
		delete(s.rooms, room.id)
	}
	s.mu.Unlock()

	sortShares(shares)
	for _, sh := range shares {
		s.endShare(sh, r)
	}
}

// actorDone forgets a Conn whose actor has stopped.
func (s *SFU) actorDone(c *Conn) {
	s.mu.Lock()
	delete(s.actors, c)
	s.mu.Unlock()
}

// addShare creates a pending share for c (02 §5.3). It refuses a share once c is closed, so no share can outlive its
// source Conn.
func (s *SFU) addShare(c *Conn, p StartShareParams) (*Share, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conns[c.id] != c {
		return nil, errClosed("the connection")
	}
	if _, dup := s.shares[p.ID]; dup {
		return nil, errCaller("StartShare: the share id exists")
	}
	c.room.mu.Lock()
	defer c.room.mu.Unlock()
	if len(c.part.shares) >= maxSharesPerParticipant {
		return nil, newError(CodeTooManyShares, "the participant already has 4 shares")
	}
	sh := &Share{
		id: p.ID, room: c.room, part: c.part, conn: c, source: p.Source, startedAt: time.Now(),
		preset: p.Preset, state: SharePending,
	}
	sh.awaitsKeyframe.Store(true)
	s.shares[sh.id] = sh
	c.room.shares[sh.id] = sh
	c.part.shares[sh.id] = sh
	return sh, nil
}

// lookupShare returns a share that hasn't ended, or nil.
func (s *SFU) lookupShare(id ShareID) *Share {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shares[id]
}

// endShare removes a share and tells everyone (02 §5.3): RoomEvents.ShareEnded with the caller's reason, the
// publishing Conn (its tracks stop feeding the share) and every subscriber (its subscription and DownTracks go, and
// its sub PC renegotiates). The Conns hear it on their internal event queues, so endShare never waits for an actor
// and returns once the share is gone and the posts are queued (02 §5.4). It returns false when the share had already
// ended. ShareEnded fires once per share and is the last thing reported about it: a state change that the ticker
// hadn't reported yet is reported here first, and nothing follows.
func (s *SFU) endShare(sh *Share, r EndReason) bool {
	s.mu.Lock()
	if s.shares[sh.id] != sh {
		s.mu.Unlock()
		return false
	}
	delete(s.shares, sh.id)
	sh.room.mu.Lock()
	delete(sh.room.shares, sh.id)
	delete(sh.part.shares, sh.id)
	sh.room.mu.Unlock()
	s.mu.Unlock()

	sh.notify.Lock()
	info, unreported, subscribers := sh.end()
	s.log.Info("share ended", "share_id", string(sh.id), "room_id", string(info.Room), "conn_id", string(info.Conn),
		"reason", string(r))
	for _, u := range unreported {
		s.events.ShareUpdated(u.Room, u)
	}
	s.events.ShareEnded(info.Room, info, r)
	sh.notify.Unlock()
	sh.conn.post(func() { sh.conn.detachShare(sh) })
	for _, sub := range subscribers {
		sub.post(func() { sub.subscribedShareEnded(sh) })
	}
	return true
}

func sortShares(shares []*Share) {
	slices.SortFunc(shares, func(a, b *Share) int {
		if c := a.startedAt.Compare(b.startedAt); c != 0 {
			return c
		}
		return cmp.Compare(a.id, b.id)
	})
}

// Shares returns a snapshot of a room's shares, sorted by StartedAt. An unknown room has none.
func (s *SFU) Shares(room RoomID) []ShareInfo {
	s.mu.Lock()
	r := s.rooms[room]
	s.mu.Unlock()
	if r == nil {
		return nil
	}
	return r.shareInfos()
}

// Share returns one share; false when it doesn't exist or has ended.
func (s *SFU) Share(id ShareID) (ShareInfo, bool) {
	sh := s.lookupShare(id)
	if sh == nil {
		return ShareInfo{}, false
	}
	return sh.info()
}

// CodecPolicy returns a room's codec policy (02 §8.3): ProfileHigh ("high") or ProfileConstrainedBaseline ("cb").
// It is ProfileHigh, the default, for every room until README S69 adds the policy.
func (s *SFU) CodecPolicy(room RoomID) ProfileKey {
	s.mu.Lock()
	r := s.rooms[room]
	s.mu.Unlock()
	if r == nil {
		return ProfileHigh
	}
	return r.codecPolicy()
}

// StopShare ends any share with the hub's reason; the hub normally uses Conn.StopShare on the publishing Conn. It
// returns sfu.share_not_found for an unknown or ended share.
func (s *SFU) StopShare(id ShareID, r EndReason) error {
	sh := s.lookupShare(id)
	if sh == nil || !s.endShare(sh, r) {
		return newError(CodeShareNotFound, "no such share")
	}
	return nil
}

// CloseRoom ends every share of a room with r and closes its Conns. The hub ends shares and closes its peers itself
// when an admin deletes a room (01 §15.2); this is the direct way for callers without a hub.
func (s *SFU) CloseRoom(room RoomID, r EndReason) {
	s.mu.Lock()
	rm := s.rooms[room]
	s.mu.Unlock()
	if rm == nil {
		return
	}
	rm.mu.Lock()
	shares := make([]*Share, 0, len(rm.shares))
	for _, sh := range rm.shares {
		shares = append(shares, sh)
	}
	conns := make([]*Conn, 0, len(rm.conns))
	for _, c := range rm.conns {
		conns = append(conns, c)
	}
	rm.mu.Unlock()
	sortShares(shares)
	for _, sh := range shares {
		s.endShare(sh, r)
	}
	for _, c := range conns {
		c.Close(r)
	}
}

// SetLimits replaces the admin's soft limits; the wiring calls it when 03's settings change. New ShareParams follow
// the new cap at once. Telling running shares (REMB on the pub PC and a quality hint, 02 §11) is README S88's. An
// invalid value is logged and ignored.
func (s *SFU) SetLimits(l Limits) {
	if err := l.validate(); err != nil {
		s.log.Warn("limits ignored", "err", err)
		return
	}
	s.mu.Lock()
	s.limits = l
	s.mu.Unlock()
}

// currentLimits returns the admin's soft limits.
func (s *SFU) currentLimits() Limits {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.limits
}
