package core

import (
	"log/slog"

	"github.com/MoonWX/isshoni/internal/protocol"
)

// The same-user relay (docs/m2/01-client-core.md §11.11): the Go connection serves no relay request kind in M2
// (D16). playback.state → EventPlaybackState is README WA55's; until then every kind is ignored (rule 3), logged
// with its kind only, never its payload.
func (c *Client) onRelay(r protocol.AgentRecv) {
	c.log.Debug("relay message ignored", slog.String("kind", r.Kind))
}
