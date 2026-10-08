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

// The media path (02 §5.4, §9).
const (
	// rtpReadBuffer holds one incoming RTP or RTCP packet (Pion's receive MTU is 1460).
	rtpReadBuffer = 1500
	// A DownTrack's queue: packets waiting for its writer. A full queue drops the packet (counted), so a writer that
	// falls behind never holds up the layer's read loop or other viewers (02 §9.3).
	videoQueueLen = 1024
	audioQueueLen = 256
	// pliInterval is the least time between two keyframe requests for one layer (02 §9.7).
	pliInterval = 500 * time.Millisecond
	// A start of a viewer's stream that Pion takes without sending it is repeated with the next keyframe, which the
	// DownTrack has the publisher asked for. After unsentStartLimit such starts in a row it asks only once per
	// unsentStartBackoff until a packet leaves, so a viewer that nothing reaches can't cost the other viewers of the
	// layer two keyframes a second (02 §9.3, §9.7).
	unsentStartLimit   = 2
	unsentStartBackoff = 5 * time.Second
	// tickInterval is the SFU ticker's period, and the least time between two debounced ShareUpdated calls for one
	// share; every statsEvery-th tick does the once-a-second work (02 §5.4).
	tickInterval = 250 * time.Millisecond
	statsEvery   = int(time.Second / tickInterval)
	// A layer's rates are 2 s moving averages, and a layer without a packet for 2 s isn't active (02 §9.1).
	rateWindow  = 2 * time.Second
	activeAfter = 2 * time.Second
)
