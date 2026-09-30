package signal

import "github.com/MoonWX/isshoni/internal/protocol"

// Test hooks for the external tests (package signal_test).

// LastStats returns the last stats report that the connection kept, or nil.
func LastStats(h *Hub, connID string) *protocol.ClientStats {
	h.mu.Lock()
	c := h.conns[connID]
	h.mu.Unlock()
	if c == nil {
		return nil
	}
	return c.lastStats.Load()
}

// Counts returns the hub's bookkeeping: open connections, live sockets, pre-auth sockets, and the per-user slots
// of userID.
func Counts(h *Hub, userID string) (conns, sockets, preAuth, slots int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.conns), len(h.sockets), h.preAuth, h.userSlots[userID]
}

// AddShare puts a share into a room that has participants and records the change, as share.start will (README S40):
// the hub's side only, without a MediaPeer call. It reports whether the room exists.
func AddShare(h *Hub, roomID string, info protocol.ShareInfo) bool {
	h.mu.Lock()
	r := h.rooms[roomID]
	h.mu.Unlock()
	if r == nil {
		return false
	}
	var o outbox
	r.mu.Lock()
	r.shares[info.ID] = &share{info: info}
	r.changedLocked(&o)
	r.mu.Unlock()
	o.send()
	return true
}

// SetWants merges desired subscriptions into a connection's and records the change, as subscribe.update will
// (README S40). It reports whether the connection is in a room.
func SetWants(h *Hub, connID string, wants ...protocol.SubscriptionWant) bool {
	h.mu.Lock()
	c := h.conns[connID]
	var r *room
	if c != nil {
		r = h.rooms[c.roomID]
	}
	h.mu.Unlock()
	if r == nil {
		return false
	}
	var o outbox
	r.mu.Lock()
	m := r.memberLocked(c)
	if m != nil {
		for _, w := range wants {
			m.subs[w.ShareID] = w
		}
		r.changedLocked(&o)
	}
	r.mu.Unlock()
	o.send()
	return m != nil
}
