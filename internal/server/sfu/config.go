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
	maxRemoteCandidates     = 64  // per PC and gen, trickled or in an SDP: the first 64 addresses are kept
	commandQueueLen         = 64  // signal calls waiting for a Conn's actor
	// maxPCCreations is how many PeerConnections of one kind a client may make a Conn create within pcRateWindow:
	// pub offers of a new gen, and every sub PC that isn't a codec rebuild of the SFU's own (README S69). One more is
	// sfu.pc_rate_limited. The hub's own limits come first (01 §13: 6 new pub gens and 12 pc.restart a minute).
	maxPCCreations = 10
)

// Timing (02 §5.3–5.4, §6.1).
const (
	commandWait      = 5 * time.Second       // a full command queue for this long is sfu.busy
	busyRetryAfter   = time.Second           // Error.RetryAfter of sfu.busy
	subOfferDebounce = 50 * time.Millisecond // changes within it share one sub offer (01 §9 rule 3)
	closeTimeout     = time.Second           // SFU.Close gives the Conns this long to close their PCs
)

// PeerConnection recovery (02 §5.3, §12). The SFU keeps each one in a field that tests shorten (timings); none
// changes once the SFU has a Conn.
const (
	// handshakeTimeout is how long a new PeerConnection has to get connected (ICE and DTLS), from the moment the
	// client can start: the pub PC's first answer, the first answer to a sub PC's offer. One that misses it is closed
	// and reported failed with ReasonHandshakeTimeout. An ICE restart has no such timer: the client rebuilds after
	// 15 s (01 §10.4).
	handshakeTimeout = 10 * time.Second
	// pcGrace is how long a failed PeerConnection is kept for an ICE restart or a rebuild before it is closed
	// (ReasonGraceExpired).
	pcGrace = 30 * time.Second
	// offerResendInterval: a sub offer without an answer is sent again this often, with the same gen and neg.
	offerResendInterval = 15 * time.Second
	// iceRestartSpacing: a sub ICE restart whose offer went out less than this long ago, with ICE neither connected
	// nor failed since, is still under way, and another request for one starts none (02 §5.3, 01 §10.4).
	iceRestartSpacing = 5 * time.Second
	// pcRateWindow is the window of maxPCCreations.
	pcRateWindow = time.Minute
	// stalledAfter: a live share whose pub PC hasn't been connected for this long is stalled (02 §5.3, 01 §13).
	stalledAfter = 2 * time.Second
)

// timings are the durations above as one SFU uses them.
type timings struct {
	handshake, grace, offerResend, iceRestart, pcWindow, stalled time.Duration
}

func defaultTimings() timings {
	return timings{
		handshake: handshakeTimeout, grace: pcGrace, offerResend: offerResendInterval, iceRestart: iceRestartSpacing,
		pcWindow: pcRateWindow, stalled: stalledAfter,
	}
}

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
	// subEventInterval is the least time between two SubscriptionStateEvents about one subscription that the media
	// path caused: a publisher decides how often what its viewers get changes, and must not decide how many events
	// they are sent (02 §6.2). What the client itself asked for is reported at once.
	subEventInterval = 250 * time.Millisecond
	// A layer's rates are 2 s moving averages, and a layer without a packet for 2 s isn't active (02 §9.1).
	rateWindow  = 2 * time.Second
	activeAfter = 2 * time.Second
)
