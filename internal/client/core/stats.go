package core

import (
	"github.com/MoonWX/isshoni/internal/protocol"
)

// Stats (docs/m2/01-client-core.md §11.9): the engine's Stats poll, the stats messages to the server and the
// server's view of the share are README WA55's.

// WatchServerStats turns stats.watch on or off (the app's debug overlay); the server's view of this connection's
// layers then arrives in ShareSnapshot.Server. Not implemented yet (README WA55): the wish is kept, and nothing is
// sent.
func (c *Client) WatchServerStats(on bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.watchStats = on
}

// onServerStats takes the server's stats notification (stats.watch). Not implemented yet (README WA55).
func (c *Client) onServerStats(protocol.ServerStats) {}
