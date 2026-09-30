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
