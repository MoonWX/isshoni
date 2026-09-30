package signal

import (
	"cmp"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol"
)

// room is one room while anyone is in it (01 §4.1): its participants, their connections and the room's shares. The
// hub creates it with its first participant and forgets it with its last one, or at CloseRoom.
//
// Every change of what room.state shows takes a new rev and goes out as a full snapshot, coalesced (01 §8.5): when
// nothing was broadcast in the last Config.StateCoalesce, the snapshot goes out at once, otherwise at the last
// broadcast + StateCoalesce, carrying every change since. A broadcast is encoded once, so every connection in the
// room gets the same bytes. Discrete changes also go out at once as room.events (01 §8.6).
//
// Lock order: hub (Hub.mu) → room (mu) → connection. Messages are never sent under mu: a change collects them in an
// outbox, which posts them to the connections' actors once the locks are released.
type room struct {
	h  *Hub
	id string

	mu sync.Mutex
	// name is the name the room's latest room.join read with GetRoom, for Snapshot only (room.state has no name). It
	// can be stale after a rename (03 §8), which has no hook. Everything else reads the name with GetRoom when it
	// needs one: PushShareStarted.RoomName (README S40) never comes from here ("the hub never caches room names").
	name         string
	participants map[string]*participant // by user id
	shares       map[string]*share       // by share id; share.start adds them (README S40)
	rev          uint64                  // the rev of the current state (Hub.nextRev)
	closed       bool                    // the hub has let go of the room: emptied, or CloseRoom; nothing more is sent
	sentAt       time.Time               // when the last broadcast went out; zero before the first
	sentRev      uint64                  // the rev of the last broadcast
	timer        *time.Timer             // the scheduled broadcast; nil when none is scheduled
}

// participant merges all connections of one user in one room (01 §4.1).
type participant struct {
	userID   string
	name     string // the user's name when the participant was created; then UpdateUser and Revalidate
	joinedAt time.Time
	members  []*member // the user's connections in the room, in the order they joined
}

// member is one connection in a room: what room.state shows about it, and its desired subscriptions.
type member struct {
	c *conn
	// detached: the connection's socket is gone and it waits for a resume within grace (README S28). room.state
	// shows it as reconnecting.
	detached bool
	// subs are the connection's desired subscriptions by share id (subscribe.update, README S40). Watchers are
	// computed from them (01 §4.1). A subscription goes when its share ends or the connection leaves the room.
	subs map[string]protocol.SubscriptionWant
}

// share is one share in a room (01 §4.4). info holds what room.state shows, except Watchers, which each snapshot
// computes from the members' desired subscriptions. share.start and the share lifecycle come with README S40.
type share struct {
	info protocol.ShareInfo
}

func newRoom(h *Hub, id string) *room {
	return &room{h: h, id: id, participants: make(map[string]*participant), shares: make(map[string]*share)}
}

// nextRev returns the next room.state rev. The counter is the hub's, so a room's rev only grows, also across a room
// that empties and fills again ("+1 per change in this server process", 01 §8.5).
func (h *Hub) nextRev() uint64 { return h.revs.Add(1) }

// outbox collects the messages of a room change, to post to the connections' actors once the locks are released.
type outbox []posting

type posting struct {
	c *conn
	f func()
}

func (o *outbox) post(c *conn, f func()) { *o = append(*o, posting{c: c, f: f}) }

// send posts the collected messages. Posting never blocks (01 §15.2).
func (o outbox) send() {
	for _, p := range o {
		p.c.post(p.f)
	}
}

// membersLocked calls f for every connection in the room.
func (r *room) membersLocked(f func(*member)) {
	for _, p := range r.participants {
		for _, m := range p.members {
			f(m)
		}
	}
}

// memberLocked returns connection c's membership, or nil.
func (r *room) memberLocked(c *conn) *member {
	if p := r.participants[c.userID]; p != nil {
		for _, m := range p.members {
			if m.c == c {
				return m
			}
		}
	}
	return nil
}

// sortedParticipantsLocked returns the participants in room.state order: by joinedAt, then userId (01 §8.5).
func (r *room) sortedParticipantsLocked() []*participant {
	ps := make([]*participant, 0, len(r.participants))
	for _, p := range r.participants {
		ps = append(ps, p)
	}
	slices.SortFunc(ps, func(a, b *participant) int {
		return cmp.Or(a.joinedAt.Compare(b.joinedAt), cmp.Compare(a.userID, b.userID))
	})
	return ps
}

// sortedSharesLocked returns the shares in room.state order: by startedAt, then id (01 §8.5).
func (r *room) sortedSharesLocked() []*share {
	ss := make([]*share, 0, len(r.shares))
	for _, s := range r.shares {
		ss = append(ss, s)
	}
	slices.SortFunc(ss, func(a, b *share) int {
		return cmp.Or(a.info.StartedAt.Compare(b.info.StartedAt), cmp.Compare(a.info.ID, b.info.ID))
	})
	return ss
}

// connectionInfo is what everyone in the room sees about the member's connection (01 §8.5).
func (m *member) connectionInfo() protocol.ConnectionInfo {
	status := protocol.ConnectionStatusOnline
	if m.detached {
		status = protocol.ConnectionStatusReconnecting
	}
	return protocol.ConnectionInfo{ID: m.c.id, Kind: m.c.client.Kind, Role: m.c.role, Status: status}
}

// status is present while at least one of the participant's connections is attached, else reconnecting (01 §4.3).
func (p *participant) status() protocol.ParticipantStatus {
	for _, m := range p.members {
		if !m.detached {
			return protocol.ParticipantStatusPresent
		}
	}
	return protocol.ParticipantStatusReconnecting
}

// layerRank orders video layers for merging: high > low > off (01 §4.1).
func layerRank(l protocol.VideoLayer) int {
	switch l {
	case protocol.VideoLayerHigh:
		return 2
	case protocol.VideoLayerLow:
		return 1
	}
	return 0
}

// watchers returns share s's watchers (01 §4.1): the participants other than its owner with at least one connection
// whose desired subscription has video other than off or audio on. Per participant, video is the maximum over its
// connections and audio is on if any connection has it on. They come in the order of parts.
func watchers(s *share, parts []*participant) []protocol.Watcher {
	out := []protocol.Watcher{}
	for _, p := range parts {
		if p.userID == s.info.UserID {
			continue // the owner is never a watcher of its own share
		}
		video, audio := protocol.VideoLayerOff, protocol.AudioStateOff
		for _, m := range p.members {
			w, ok := m.subs[s.info.ID]
			if !ok {
				continue
			}
			if layerRank(w.Video) > layerRank(video) {
				video = w.Video
			}
			if w.Audio == protocol.AudioStateOn {
				audio = protocol.AudioStateOn
			}
		}
		if video != protocol.VideoLayerOff || audio == protocol.AudioStateOn {
			out = append(out, protocol.Watcher{UserID: p.userID, Video: video, Audio: audio})
		}
	}
	return out
}

// stateLocked returns the room's snapshot (01 §8.5). The slices are its own.
func (r *room) stateLocked() protocol.RoomState {
	parts := r.sortedParticipantsLocked()
	st := protocol.RoomState{
		RoomID:       r.id,
		Rev:          r.rev,
		Participants: make([]protocol.ParticipantInfo, 0, len(parts)),
		Shares:       make([]protocol.ShareInfo, 0, len(r.shares)),
	}
	for _, p := range parts {
		pi := protocol.ParticipantInfo{
			UserID:      p.userID,
			Name:        p.name,
			Status:      p.status(),
			JoinedAt:    p.joinedAt,
			Connections: make([]protocol.ConnectionInfo, 0, len(p.members)),
		}
		for _, m := range p.members {
			pi.Connections = append(pi.Connections, m.connectionInfo())
		}
		st.Participants = append(st.Participants, pi)
	}
	for _, s := range r.sortedSharesLocked() {
		st.Shares = append(st.Shares, s.shareInfo(parts))
	}
	return st
}

// shareInfo returns what room.state shows about the share, with its watchers among parts (sorted participants). Its
// slices are its own and never nil (Snapshot); slices.Clone would keep a nil Layers nil.
func (s *share) shareInfo(parts []*participant) protocol.ShareInfo {
	si := s.info
	si.Layers = append([]protocol.VideoLayer{}, si.Layers...)
	si.Watchers = watchers(s, parts)
	return si
}

// encodeStateLocked encodes the room's snapshot as a room.state message.
func (r *room) encodeStateLocked() ([]byte, bool) {
	b, err := protocol.Marshal(protocol.MessageTypeRoomState, "", "", r.stateLocked())
	if err != nil {
		r.h.log.Error("encode room.state", slog.String("room_id", r.id), slog.Any("err", err))
		return nil, false
	}
	return b, true
}

// changedLocked records a change of what room.state shows: a new rev, and a broadcast to every connection in the
// room at once, or at the last broadcast + StateCoalesce when that is still to come (01 §8.5). A scheduled broadcast
// carries every change until it goes out. A closed room sends nothing more.
//
// The scheduled broadcast counts in the hub's WaitGroup until it has run or been stopped. changedLocked runs for a
// change by a connection in the room (on its actor, or a hub method acting for it), so the WaitGroup is above zero.
func (r *room) changedLocked(o *outbox) {
	if r.closed {
		return
	}
	r.rev = r.h.nextRev()
	if r.timer != nil {
		return // the scheduled broadcast carries this change
	}
	now := time.Now()
	if !r.sentAt.IsZero() {
		if wait := r.sentAt.Add(r.h.cfg.StateCoalesce).Sub(now); wait > 0 {
			r.h.wg.Add(1)
			r.timer = time.AfterFunc(wait, r.flushScheduled)
			return
		}
	}
	r.broadcastLocked(o, now)
}

// broadcastLocked encodes the room's snapshot once and queues it for every connection in the room.
func (r *room) broadcastLocked(o *outbox, now time.Time) {
	b, ok := r.encodeStateLocked()
	if !ok {
		return
	}
	r.sentAt, r.sentRev = now, r.rev
	rev := r.rev
	r.membersLocked(func(m *member) {
		c := m.c
		o.post(c, func() { c.sendRoomState(r, rev, b) })
	})
}

// flushScheduled is the scheduled broadcast (changedLocked). It runs on the timer's goroutine.
func (r *room) flushScheduled() {
	defer r.h.wg.Done()
	var o outbox
	r.mu.Lock()
	r.timer = nil
	if !r.closed && r.rev != r.sentRev {
		r.broadcastLocked(&o, time.Now())
	}
	r.mu.Unlock()
	o.send()
}

// stopTimerLocked cancels a scheduled broadcast. When the timer's function has already started, it finds the room
// closed and ends by itself.
func (r *room) stopTimerLocked() {
	if r.timer != nil && r.timer.Stop() {
		r.h.wg.Done()
	}
	r.timer = nil
}

// eventLocked queues room.event ev for every connection in the room except skip (nil: none). Events go to the
// actor's own other connections too; clients suppress toasts for their own user (01 §8.6).
func (r *room) eventLocked(o *outbox, ev protocol.RoomEvent, skip *conn) {
	if r.closed {
		return
	}
	b, err := protocol.Marshal(protocol.MessageTypeRoomEvent, "", "", ev)
	if err != nil {
		r.h.log.Error("encode room.event", slog.String("room_id", r.id), slog.Any("err", err))
		return
	}
	r.membersLocked(func(m *member) {
		if c := m.c; c != skip {
			o.post(c, func() { c.sendRoomEvent(r, b) })
		}
	})
}

// endShareLocked removes share s from the room with reason: the subscriptions to it go, and every connection in the
// room gets room.event share.stopped (01 §8.6). The caller ends it at the publishing connection's MediaPeer and
// records the change (changedLocked).
func (r *room) endShareLocked(o *outbox, s *share, reason protocol.EndReason, now time.Time) {
	delete(r.shares, s.info.ID)
	r.membersLocked(func(m *member) { delete(m.subs, s.info.ID) })
	name := ""
	if p := r.participants[s.info.UserID]; p != nil {
		name = p.name
	}
	r.eventLocked(o, protocol.RoomEvent{
		Kind:    protocol.RoomEventKindShareStopped,
		RoomID:  r.id,
		UserID:  s.info.UserID,
		Name:    name,
		ShareID: s.info.ID,
		Reason:  reason,
		At:      now,
	}, nil)
}

// setName records the room's name that a room.join just read with GetRoom, for Snapshot.
func (r *room) setName(name string) {
	r.mu.Lock()
	r.name = name
	r.mu.Unlock()
}

// rename changes a participant's name (UpdateUser, Revalidate), which room.state shows.
func (r *room) rename(userID, name string) {
	var o outbox
	r.mu.Lock()
	if p := r.participants[userID]; p != nil && p.name != name {
		p.name = name
		r.changedLocked(&o)
	}
	r.mu.Unlock()
	o.send()
}

// participantLeftReason is the participant.left reason of a connection that leaves with the share end reason:
// server_shutdown is for shares only (01 §8.6), and at shutdown nobody is left to see the event.
func participantLeftReason(r protocol.EndReason) protocol.EndReason {
	if r == protocol.EndReasonServerShutdown {
		return protocol.EndReasonDisconnected
	}
	return r
}

// attach adds connection c to room info.ID for a room.join (01 §8.4): the in-memory room is created with its first
// participant, and the participant with the user's first connection there, which the room's other connections learn
// from room.event participant.joined. It fails with room_not_found when CloseRoom closed the room (also when the
// join's GetRoom came before CloseRoom), and with room_full when a new participant would pass limit (0 = none).
// The check and the attach are one step under the locks.
func (h *Hub) attach(c *conn, info protocol.RoomInfo, limit int) (*room, *protocol.Error) {
	id := c.identity()
	var o outbox
	h.mu.Lock()
	if _, closed := h.closedRooms[info.ID]; closed {
		h.mu.Unlock()
		e := protocol.NewError(protocol.ErrorCodeRoomNotFound, protocol.ErrorScopeRequest)
		return nil, &e
	}
	r := h.rooms[info.ID]
	if r == nil {
		r = newRoom(h, info.ID)
		h.rooms[info.ID] = r
	}
	r.mu.Lock()
	p := r.participants[id.UserID]
	if p == nil && limit > 0 && len(r.participants) >= limit {
		r.mu.Unlock()
		h.mu.Unlock()
		e := protocol.NewError(protocol.ErrorCodeRoomFull, protocol.ErrorScopeRequest)
		e.Params = map[string]any{"limit": limit}
		return nil, &e
	}
	now := time.Now()
	r.name = info.Name
	if p == nil {
		p = &participant{userID: id.UserID, name: id.Name, joinedAt: now}
		r.participants[id.UserID] = p
		h.participants++
		r.eventLocked(&o, protocol.RoomEvent{
			Kind:   protocol.RoomEventKindParticipantJoined,
			RoomID: r.id,
			UserID: p.userID,
			Name:   p.name,
			At:     now,
		}, c)
	}
	p.members = append(p.members, &member{c: c, subs: make(map[string]protocol.SubscriptionWant)})
	c.roomID = r.id
	r.changedLocked(&o)
	h.metrics.roomCounts(len(h.rooms), h.participants)
	r.mu.Unlock()
	h.mu.Unlock()
	o.send()
	return r, nil
}

// detach takes connection c out of room r (01 §8.4, §4.2): its shares there end with reason, its subscriptions go,
// and it leaves its participant, which leaves the room with its last connection (room.event participant.left). The
// room goes with its last participant. It returns the ids of the ended shares, which the caller ends at the
// MediaPeer before closing it (no lock is held while calling a MediaPeer). In a room that CloseRoom closed, nothing
// is sent.
func (h *Hub) detach(c *conn, r *room, reason protocol.EndReason) []string {
	now := time.Now()
	var o outbox
	var ended []string
	h.mu.Lock()
	if c.roomID == r.id {
		c.roomID = ""
	}
	r.mu.Lock()
	for _, s := range r.sortedSharesLocked() {
		if s.info.ConnectionID == c.id {
			r.endShareLocked(&o, s, reason, now)
			ended = append(ended, s.info.ID)
		}
	}
	if p := r.participants[c.userID]; p != nil {
		p.members = slices.DeleteFunc(p.members, func(m *member) bool { return m.c == c })
		if len(p.members) == 0 {
			delete(r.participants, c.userID)
			h.participants--
			r.eventLocked(&o, protocol.RoomEvent{
				Kind:   protocol.RoomEventKindParticipantLeft,
				RoomID: r.id,
				UserID: p.userID,
				Name:   p.name,
				Reason: participantLeftReason(reason),
				At:     now,
			}, nil)
		}
	}
	if len(r.participants) == 0 {
		// Every share belongs to a connection in the room, so the room is empty.
		r.closed = true
		r.stopTimerLocked()
		if h.rooms[r.id] == r {
			delete(h.rooms, r.id)
		}
	} else {
		r.changedLocked(&o)
	}
	h.metrics.roomCounts(len(h.rooms), h.participants)
	r.mu.Unlock()
	h.mu.Unlock()
	o.send()
	return ended
}

// liveLocked returns the room for the live snapshot (Hub.Snapshot).
func (r *room) liveLocked() LiveRoom {
	parts := r.sortedParticipantsLocked()
	lr := LiveRoom{
		ID:           r.id,
		Name:         r.name,
		Participants: make([]LiveParticipant, 0, len(parts)),
		Shares:       make([]LiveShare, 0, len(r.shares)),
	}
	for _, p := range parts {
		lp := LiveParticipant{UserID: p.userID, Name: p.name, Connections: make([]LiveConnection, 0, len(p.members))}
		for _, m := range p.members {
			c := m.c
			lp.Connections = append(lp.Connections, LiveConnection{
				ID:        c.id,
				Kind:      c.client.Kind,
				Role:      c.role,
				OS:        c.client.OS,
				Browser:   c.client.Browser,
				Version:   c.client.Version,
				Status:    m.connectionInfo().Status,
				Since:     c.since,
				LastStats: c.lastStats.Load(),
			})
		}
		lr.Participants = append(lr.Participants, lp)
	}
	for _, s := range r.sortedSharesLocked() {
		// Layers and EgressBitrate come from the publishing connection's MediaPeer.Stats, which only its actor may
		// call; the stats forwarding of README S40 keeps them for the snapshot.
		lr.Shares = append(lr.Shares, LiveShare{Info: s.shareInfo(parts), Layers: []protocol.ServerLayerStats{}})
	}
	return lr
}

// peerSink is the MediaSink of one MediaPeer (01 §15.2). Its methods never block: they post the event to the
// connection's actor, which drops the events of a peer that is no longer the connection's (after a room.leave or a
// join elsewhere). The server→client notifications go out as they are; README S40 adds the share lifecycle
// (ShareMedia) and ends a share on codec_not_supported.
//
// A notification is encoded before it is posted, on the caller's goroutine, so the actor never reads the slices,
// maps or pointers of the caller's value (PCOffer.Tracks, QualityHint.Encodings, PCICE.Candidate, Error.Params),
// which the SFU may reuse once the call returns. Whatever the actor keeps of an event it handles itself
// (ShareMediaEvent.Layers, README S40) must be copied the same way.
type peerSink struct {
	c   *conn
	seq uint64 // the connection's peerSeq when the peer was created
}

var _ MediaSink = (*peerSink)(nil)

// Offer implements MediaSink: a sub PC offer goes out as pc.offer.
func (s *peerSink) Offer(o protocol.PCOffer) { s.forward(protocol.MessageTypePCOffer, o) }

// ICE implements MediaSink: pc.ice.
func (s *peerSink) ICE(c protocol.PCICE) { s.forward(protocol.MessageTypePCICE, c) }

// RestartRequest implements MediaSink: pc.restart, asking the client to restart or rebuild its pub PC.
func (s *peerSink) RestartRequest(r protocol.PCRestart) { s.forward(protocol.MessageTypePCRestart, r) }

// SubscriptionStatus implements MediaSink: subscribe.status.
func (s *peerSink) SubscriptionStatus(st []protocol.SubscriptionStatus) {
	s.forward(protocol.MessageTypeSubscribeStatus, protocol.SubscribeStatus{Subs: st})
}

// QualityHint implements MediaSink: quality.hint.
func (s *peerSink) QualityHint(h protocol.QualityHint) { s.forward(protocol.MessageTypeQualityHint, h) }

// ShareMedia implements MediaSink. The share lifecycle it drives comes with README S40.
func (s *peerSink) ShareMedia(shareID string, ev ShareMediaEvent) {
	c := s.c
	c.post(func() {
		if c.currentPeer(s.seq) {
			c.h.log.Warn("share media event not handled", slog.String("conn_id", c.id),
				slog.String("share_id", shareID), slog.String("kind", ev.Kind.String()), slog.Any("err", errNotImplemented))
		}
	})
}

// Error implements MediaSink: an error with scope pc or share goes out as a notification.
func (s *peerSink) Error(e protocol.Error) {
	c, code := s.c, e.Code
	s.post(protocol.MessageTypeError, e, func() { c.h.metrics.errorSent(code) })
}

// forward sends a notification of type t with payload data.
func (s *peerSink) forward(t protocol.MessageType, data any) { s.post(t, data, nil) }

// post encodes a notification at once and posts it to the actor, which sends it while the peer is the connection's
// current one, then calls sent (nil: nothing) when the message was queued.
func (s *peerSink) post(t protocol.MessageType, data any, sent func()) {
	c := s.c
	b, err := protocol.Marshal(t, "", "", data)
	if err != nil {
		c.h.log.Error("encode message", slog.String("conn_id", c.id), slog.String("type", string(t)),
			slog.Any("err", err))
		return
	}
	c.post(func() {
		if c.currentPeer(s.seq) && c.sendEncoded(t, b) && sent != nil {
			sent()
		}
	})
}
