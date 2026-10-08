package signal

import (
	"sync"

	"github.com/MoonWX/isshoni/internal/protocol"
)

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

// Counts returns the hub's bookkeeping: open connections (detached ones too), live sockets, pre-auth sockets, and
// the per-user slots of userID.
func Counts(h *Hub, userID string) (conns, sockets, preAuth, slots int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.conns), len(h.sockets), h.preAuth, h.userSlots[userID]
}

// Stall holds the actor of connection connID until release is called, as a dependency call that takes long would.
// release is nil when there is no such connection.
func Stall(h *Hub, connID string) (release func()) {
	h.mu.Lock()
	c := h.conns[connID]
	h.mu.Unlock()
	if c == nil {
		return nil
	}
	gate := make(chan struct{})
	if !c.postWait(func() { <-gate }) {
		return nil
	}
	return sync.OnceFunc(func() { close(gate) })
}

// OverCap returns the number of userID's cookie sockets that wait at the per-user cap for their hello.
func OverCap(h *Hub, userID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.overCap[userID]
}

// AddShare puts a share into a room that has participants and records the change, as share.start does: the hub's
// side only, in the given status, without a MediaPeer call, a limit check or a timeout. Tests of rooms and resume
// use it to set a scene; the share tests go through share.start. It reports whether the room exists.
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
	h.metrics.shareStatus("", info.Status)
	r.changedLocked(&o)
	r.mu.Unlock()
	o.send()
	return true
}

// SetWants merges desired subscriptions into a connection's and records the change, as subscribe.update does: the
// hub's side only, without a MediaPeer call. It reports whether the connection is in a room.
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
