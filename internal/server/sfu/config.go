package sfu

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/MoonWX/isshoni/internal/server/netx"
)

// Config is what the wiring (04 §6.6) passes to New. The ICE ports, IP families, loopback candidates and socket
// buffers are 04's keys and reach the SFU only through Transport.
type Config struct {
	// Transport holds the ICE sockets, muxes, filters and rewrite rules (04 §7.3). 04 builds it before the SFU and
	// closes it after SFU.Close; the SFU never binds or closes a socket. Tests inject sockets (sfutest.FaultConn)
	// through netx.TransportOptions.PacketConns.
	Transport *netx.Transport
	// PauseUnwatchedLayers is 04's key sfu.pause_unwatched_layers (02 §11, README S88).
	PauseUnwatchedLayers bool
	Limits               Limits
}

// Limits are the admin's soft limits (02 §12): 0 means unlimited, the default. They are stored by 03 and pushed
// live with SFU.SetLimits. The room share limit and the participant limit are the hub's (01), not here.
type Limits struct {
	// MaxShareKbps caps each share's full layer: ShareParams (02 §8.6) and, with README S88, REMB and the quality
	// hint (02 §11). It is 03's maxShareBitrateKbps.
	MaxShareKbps int
}

func (l Limits) validate() error {
	if l.MaxShareKbps < 0 {
		return fmt.Errorf("sfu: Limits.MaxShareKbps is %d (0 means unlimited)", l.MaxShareKbps)
	}
	return nil
}

func (c Config) validate() error {
	if c.Transport == nil {
		return errors.New("sfu: no ICE transport (Config.Transport is nil)")
	}
	return c.Limits.validate()
}

// Deps are the SFU's collaborators.
type Deps struct {
	Events RoomEvents   // 01's sfuplane; nil drops the events
	Logger *slog.Logger // the SFU adds component=sfu; nil discards the logs
}

// Guards (02 §12). They limit abuse, not people, and are constants on purpose. The SDP guards are in sdpcheck.go and
// the ICE and DTLS timeouts in api.go; doc.go lists the guards that later slices add.
const (
	maxSharesPerParticipant = 4   // sfu.too_many_shares; the hub enforces 4 per user and the room soft limit first
	maxSubscriptions        = 256 // per Conn: sfu.too_many_subscriptions
	maxSubscriptionsPerCall = 64  // items per UpdateSubscriptions: sfu.too_many_subscriptions
	maxRemoteCandidates     = 64  // per PC and gen, buffered or applied: the first 64 are kept
	commandQueueLen         = 64  // signal calls waiting for a Conn's actor
)

// Timing (02 §5.3–5.4, §6.1).
const (
	commandWait      = 5 * time.Second       // a full command queue for this long is sfu.busy
	busyRetryAfter   = time.Second           // Error.RetryAfter of sfu.busy
	subOfferDebounce = 50 * time.Millisecond // changes within it share one sub offer (01 §9 rule 3)
	closeTimeout     = time.Second           // SFU.Close gives the Conns this long to close their PCs
)

// rtpReadBuffer holds one incoming RTP or RTCP packet (Pion's receive MTU is 1460).
const rtpReadBuffer = 1500
