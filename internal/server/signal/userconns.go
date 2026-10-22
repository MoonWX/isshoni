package signal

import (
	"cmp"
	"log/slog"
	"slices"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol"
)

// The user's own connections (M2 01 §10.4, feature user.connections).
//
// A user's connections learn about each other from user.connections: the full list of the user's connections, the
// recipient included, each with its kind, role, status, OS, client version, room and, for a bearer connection, the
// device's name (protocol.OwnConnection). It is how a web tab learns that the user's desktop app is online, and which
// connection id to address with a relayed playback.state (M2 01 §11.11). Only the user's own connections ever see it:
// OS, version and device name stay private (01 §17).
//
// The list goes to every connection of the user that has the feature active and is online:
//
//   - after the connection's welcome, the first one and every resumed one;
//   - whenever one of the user's connections opens (its first welcome), closes, detaches, re-attaches (resumes) or
//     changes room (room.join, room.leave, CloseRoom);
//   - after a Revalidate that changed a connection's DeviceName. A device rename therefore reaches the list up to
//     Config.RevalidateEvery late (5 min).
//
// Like room.state (room.go), every change takes a new rev and goes out as the full list, coalesced per user: when
// nothing was sent to the user in the last userConnsCoalesce, the list goes out at once, otherwise at the last one +
// userConnsCoalesce, carrying every change since. It is encoded once, sorted by connection id, and each recipient's
// actor sends it unless it has sent a newer rev already. A connection is listed from its first welcome until it
// closes, detached ones with status reconnecting; a connection that is still in its handshake is not listed yet.
//
// Nothing is computed for a user none of whose connections has the feature. The device name is user content and is
// never logged.

// userConnsCoalesce is the least time between two user.connections of one user (M2 01 §10.4, §13: at most one per
// 200 ms per user).
const userConnsCoalesce = 200 * time.Millisecond

// userConns is the user.connections state of one user, guarded by the hub lock. The hub creates it with the user's
// first welcomed connection and forgets it with the user's last one.
type userConns struct {
	userID string
	conns  map[*conn]struct{} // the user's listed connections: welcomed, not closed yet
	sentAt time.Time          // when the last list went out; zero before the first
	timer  *time.Timer        // the scheduled list; nil when none is scheduled
}

// hasFeature reports whether feature f is active on the connection (01 §6.2). A connection's features never change,
// so any goroutine may call it.
func (c *conn) hasFeature(f protocol.Feature) bool { return slices.Contains(c.features, f) }

// listConn lists connection c, which has just sent its first welcome, in its user's user.connections: the opening of
// a connection is a change (M2 01 §10.4), and it gets the list after its welcome, like the user's other connections
// with the feature. It runs on c's actor, after attach.
func (h *Hub) listConn(c *conn) {
	var o outbox
	h.mu.Lock()
	u := h.userConns[c.userID]
	if u == nil {
		u = &userConns{userID: c.userID, conns: make(map[*conn]struct{})}
		h.userConns[c.userID] = u
	}
	u.conns[c] = struct{}{}
	c.listed = true
	h.userConnsChangedLocked(&o, c)
	h.mu.Unlock()
	o.send()
}

// unlistConnLocked takes connection c, which has closed, out of its user's user.connections: a change for the user's
// other connections. With the user's last connection the user's state goes, and a scheduled list with it. The hub
// lock is held.
func (h *Hub) unlistConnLocked(o *outbox, c *conn) {
	if !c.listed {
		return
	}
	c.listed = false
	u := h.userConns[c.userID]
	if u == nil {
		return
	}
	delete(u.conns, c)
	if len(u.conns) == 0 {
		h.stopUserConnsTimerLocked(u)
		delete(h.userConns, c.userID)
		return
	}
	h.scheduleUserConnsLocked(o, u)
}

// relistConn records that connection c has re-attached (resumed): a change for the user's connections, c included,
// which gets the list after its resumed welcome. A list encoded while c was detached shows it reconnecting and may
// still be queued in c's inbox, behind the resume: c's floor rises to the hub's last rev, so it skips those, and the
// next list, sent now or the scheduled one, is newer. It runs on c's actor, after attach (connsRev is the actor's).
func (h *Hub) relistConn(c *conn) {
	var o outbox
	h.mu.Lock()
	c.connsRev = max(c.connsRev, h.connsRev)
	h.userConnsChangedLocked(&o, c)
	h.mu.Unlock()
	o.send()
}

// userConnsChanged records a change of what user.connections shows about connection c (M2 01 §10.4): it detached,
// or its device name changed.
func (h *Hub) userConnsChanged(c *conn) {
	var o outbox
	h.mu.Lock()
	h.userConnsChangedLocked(&o, c)
	h.mu.Unlock()
	o.send()
}

// userConnsChangedLocked records a change of what user.connections shows about connection c. Nothing happens for a
// connection that is not listed: before its first welcome, which lists it, or after it closed. The hub lock is held.
func (h *Hub) userConnsChangedLocked(o *outbox, c *conn) {
	if !c.listed {
		return
	}
	if u := h.userConns[c.userID]; u != nil {
		h.scheduleUserConnsLocked(o, u)
	}
}

// scheduleUserConnsLocked sends user u's list at once, or at the last list + userConnsCoalesce when that is still to
// come. A scheduled list carries every change until it goes out.
//
// The scheduled list counts in the hub's WaitGroup until it has run or been stopped. Every change comes from a
// connection's actor, which is counted, so the WaitGroup is above zero here.
func (h *Hub) scheduleUserConnsLocked(o *outbox, u *userConns) {
	if u.timer != nil {
		return // the scheduled list carries this change
	}
	now := time.Now()
	if !u.sentAt.IsZero() {
		if wait := u.sentAt.Add(userConnsCoalesce).Sub(now); wait > 0 {
			h.wg.Add(1)
			u.timer = time.AfterFunc(wait, func() { h.flushUserConns(u) })
			return
		}
	}
	h.broadcastUserConnsLocked(o, u, now)
}

// flushUserConns is a scheduled list (scheduleUserConnsLocked). It runs on the timer's goroutine. A user whose last
// connection has closed meanwhile gets nothing.
func (h *Hub) flushUserConns(u *userConns) {
	defer h.wg.Done()
	var o outbox
	h.mu.Lock()
	u.timer = nil
	if h.userConns[u.userID] == u {
		h.broadcastUserConnsLocked(&o, u, time.Now())
	}
	h.mu.Unlock()
	o.send()
}

// stopUserConnsTimerLocked cancels a scheduled list. When the timer's function has already started, it finds the
// user's state gone and ends by itself.
func (h *Hub) stopUserConnsTimerLocked(u *userConns) {
	if u.timer != nil && u.timer.Stop() {
		h.wg.Done()
	}
	u.timer = nil
}

// broadcastUserConnsLocked encodes user u's list once and queues it for each of the user's listed connections that
// has the feature. Each one's actor sends it while the connection is attached (conn.sendUserConns). With no such
// connection nothing is encoded and nothing counts as sent.
func (h *Hub) broadcastUserConnsLocked(o *outbox, u *userConns, now time.Time) {
	var to []*conn
	list := make([]protocol.OwnConnection, 0, len(u.conns))
	for c := range u.conns {
		list = append(list, c.ownConnectionLocked())
		if c.hasFeature(protocol.FeatureUserConnections) {
			to = append(to, c)
		}
	}
	if len(to) == 0 {
		return
	}
	slices.SortFunc(list, func(a, b protocol.OwnConnection) int { return cmp.Compare(a.ID, b.ID) })
	b, err := protocol.Marshal(protocol.MessageTypeUserConnections, "", "", protocol.UserConnections{Connections: list})
	if err != nil {
		h.log.Error("encode user.connections", slog.String("user_id", u.userID), slog.Any("err", err))
		return
	}
	h.connsRev++
	rev := h.connsRev
	u.sentAt = now
	for _, c := range to {
		o.post(c, func() { c.sendUserConns(rev, b) })
	}
	h.log.Debug("user connections", slog.String("user_id", u.userID), slog.Int("connections", len(list)),
		slog.Int("recipients", len(to)))
}

// ownConnectionLocked is what user.connections shows about the connection (M2 01 §10.4): reconnecting while it is
// detached, and its room. The hub lock is held (roomID); the rest is fixed or read atomically.
func (c *conn) ownConnectionLocked() protocol.OwnConnection {
	status := protocol.ConnectionStatusOnline
	if c.sockP.Load() == nil {
		status = protocol.ConnectionStatusReconnecting
	}
	return protocol.OwnConnection{
		ConnectionInfo: protocol.ConnectionInfo{ID: c.id, Kind: c.client.Kind, Role: c.role, Status: status},
		OS:             c.client.OS,
		Version:        c.client.Version,
		RoomID:         c.roomID,
		DeviceName:     c.identity().DeviceName,
	}
}

// sendUserConns sends a user.connections list with rev. Nothing is sent when the connection has had that rev or a
// newer one already (two lists may reach the actor out of order), or while it is detached. A list encoded before the
// connection re-attached is not sent after it either, even when it reaches the actor after the resume: relistConn
// raised the floor to its rev at least, and the list that the re-attach brings is newer.
func (c *conn) sendUserConns(rev uint64, msg []byte) {
	if rev <= c.connsRev {
		return
	}
	c.connsRev = rev
	c.sendEncoded(protocol.MessageTypeUserConnections, msg)
}

// setDeviceName records the device name that a Revalidate or a resume's handshake reported for the connection's
// bearer identity (M2 02 §6.3). It reports whether the name changed, which the caller makes a user.connections change.
func (c *conn) setDeviceName(name string) bool {
	id := c.identity()
	if id.DeviceName == name {
		return false
	}
	id.DeviceName = name
	c.ident.Store(&id)
	return true
}
