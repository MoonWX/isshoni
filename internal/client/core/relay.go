package core

import (
	"log/slog"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/relay"
)

// The same-user relay (docs/m2/01-client-core.md §11.11, M1 01 §8.14): the Go connection asks for agent.relay and
// consumes one kind, playback.state, which the user's web tabs send it so that 05 can keep their browser out of the
// share's audio (05 §5.9). It serves no relay request kind in M2 (D16):
//
//  1. playback.state: decoded strictly by relay.DecodePlaybackState (unknown fields ignored; the server bounds the
//     payload at 16 KiB and delivers only messages of the same user). A payload that doesn't decode, or a browser
//     outside the four families, is ignored. Otherwise EventPlaybackState{From, Playing, Browser, OS} goes to the
//     app, which hands it to 05's Service.SetViewerBrowser(From, Browser, OS, Playing).
//  2. playback.stop is the desktop SPA's and the tabs' (03 §12.2): the core never sends or handles it.
//  3. Every other kind (share.request, share.status, exclusions.*, unknown ones) is ignored.
//
// What is ignored is logged at debug level with its kind only: a relay payload is never logged (§11.12), and neither
// is the decoding error, which could quote it.

// onRelay handles an agent.recv on the connection actor.
func (c *Client) onRelay(r protocol.AgentRecv) {
	if r.Kind != relay.KindPlaybackState {
		c.log.Debug("relay message ignored", slog.String("kind", r.Kind))
		return
	}
	p, err := relay.DecodePlaybackState(r.Payload)
	if err != nil {
		c.log.Debug("relay message ignored: invalid payload", slog.String("kind", r.Kind))
		return
	}
	ps := PlaybackState{From: r.From, Playing: p.Playing, Browser: p.Browser, OS: p.OS}
	c.events.publish(func() Event {
		ev := ps
		return Event{Kind: EventPlaybackState, Playback: &ev}
	})
}
