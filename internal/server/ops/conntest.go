package ops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// The connection test's REST side (04 §7.7): POST /api/v1/conntest, the server half of the setup wizard's step 2
// and of "Test my connection" (05 §14.2). A browser posts an offer with one data channel for one ICE transport; the
// SFU answers it with a probe PeerConnection that has only that transport's candidates (02 §7.6, reached through
// Prober), and the browser checks whether ICE connects and measures the round trip on the echo channel. Any
// signed-in user may run it; the server object of the answer lets the SPA show admins the fix text for the
// server's provider, NAT and container.
//
// ConnTest.Probe is the typed function; the wiring wraps it into the route (04 §2, §6.6):
//
//	ct := ops.NewConnTest(ops.ConnTestOptions{
//		Prober:     prober{sfu: media}, // ops.Prober over 02's SFU.Probe, with its errors mapped
//		Ports:      ops.ConnTestPorts{UDP: 7882, TCP443: 443, TCP7882: 7882}, // 0: no listener
//		PublicIPv4: public.V4, NAT: public.NAT, Provider: provider, Container: container,
//	})
//	api.Handle("POST /api/v1/conntest", httpapi.User, …) // ct.Probe(r.Context(), principal.UserID, request)

// The connection test's limits and timing (04 §7.7, §7.8).
const (
	// connTestPerWindow requests of one user within connTestWindow are let through to the SFU; the next one is 429
	// rate_limited until the oldest of them leaves the window. A browser's test sends one request per transport, so
	// a user can run four full tests a minute. Requests answered before the limit (a transport without a listener,
	// a malformed one) don't count.
	connTestPerWindow = 12
	connTestWindow    = time.Minute
	// connTestExpiresInS is the answer's expiresInS: a probe PC lives at most 20 s (02 §7.6). The SFU closes it
	// sooner when its data channel closes or ICE fails.
	connTestExpiresInS = 20
)

// ConnTestPorts are the ports of the three ICE transports as the server bound them (04 §7.3), each 0 when that
// transport has no listener: UDP with listen.ice_udp = "", TCP443 in tls.mode = "off" (the operator's proxy owns
// port 443 then, 04 §8.5), TCP7882 with listen.ice_tcp = "". A transport with port 0 is answered with 409
// transport_disabled before the SFU is asked.
type ConnTestPorts struct {
	UDP     int // listen.ice_udp
	TCP443  int // listen.https: ICE-TCP through the 443 multiplexer (04 §7.2)
	TCP7882 int // listen.ice_tcp
}

// of returns the port of transport t, and 0 for a transport without a listener or one that isn't a transport.
func (p ConnTestPorts) of(t api.Transport) int {
	switch t {
	case api.TransportUDP:
		return p.UDP
	case api.TransportTCP443:
		return p.TCP443
	case api.TransportTCP7882:
		return p.TCP7882
	}
	return 0
}

// ConnTestOptions configures NewConnTest. The fields but Prober, Now and Logger describe the server as it started;
// they are what every answer's server object says (api.ConnTestServerInfo).
type ConnTestOptions struct {
	// Prober starts the probe PeerConnections: the wiring's adapter over 02's SFU.Probe. nil (a server without an
	// SFU, which has no API either) answers 503 not_ready.
	Prober Prober
	// Ports are the transports that have a listener, with their ports: the answer's udpPort and tcpPorts.
	Ports ConnTestPorts
	// PublicIPv4 is the public IPv4 address found at startup (netx.PublicAddrs.V4; 04 §7.4), the one the probe
	// answers' IPv4 candidates name. The zero Addr, or one that isn't IPv4, is publicIp "": the server doesn't know
	// its public IPv4 address (dev, or CI without STUN), and the SPA tells the admin to set public_ip (05 §14.2).
	PublicIPv4 netip.Addr
	// Provider is the hosting provider by the machine's DMI data (04 §13.3); "" means unknown.
	Provider api.CloudProvider
	// Container is the container runtime the server runs in (04 §5.1), as doctor's env.container; "" means none.
	Container api.ContainerKind
	// NAT is the classification of the public IPv4 path (04 §7.4); "" means unknown.
	NAT api.NATKind
	// Now is the clock of the per-user limit. nil means time.Now.
	Now func() time.Time
	// Logger is the server's logger; ConnTest adds component=ops. nil means slog.Default().
	Logger *slog.Logger
}

// ConnTest is the connection-test endpoint (04 §7.7). NewConnTest builds it; Probe may be called from any goroutine.
// It starts nothing of its own: the probe PCs are the SFU's, and the SFU closes them.
type ConnTest struct {
	prober Prober
	ports  ConnTestPorts
	server api.ConnTestServerInfo // the server object of every answer; TCPPorts is shared, so never changed
	limit  *connTestLimiter
	log    *slog.Logger
}

// NewConnTest builds the endpoint from what the server knows about itself.
func NewConnTest(o ConnTestOptions) *ConnTest {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	return &ConnTest{
		prober: o.Prober,
		ports:  o.Ports,
		server: o.serverInfo(),
		limit:  newConnTestLimiter(now),
		log:    log.With(slog.String("component", "ops")),
	}
}

// serverInfo is the server object of the answers (04 §7.7): publicIp only for a known IPv4 address, the provider,
// the container and the NAT kind with their defaults, and the ports of the transports that have a listener:
// udpPort 0 when UDP is off, and tcpPorts in the order 443, 7882 (05 shows the first one that isn't 443 in its fix
// text). tcpPorts is [] rather than null when both TCP transports are off.
func (o ConnTestOptions) serverInfo() api.ConnTestServerInfo {
	info := api.ConnTestServerInfo{
		Provider:  o.Provider,
		Container: o.Container,
		UDPPort:   max(o.Ports.UDP, 0),
		TCPPorts:  []int{},
		NAT:       o.NAT,
	}
	if ip := o.PublicIPv4.Unmap(); ip.Is4() {
		info.PublicIP = ip.String()
	}
	if info.Provider == "" {
		info.Provider = api.CloudProviderUnknown
	}
	if info.Container == "" {
		info.Container = api.ContainerKindNone
	}
	if info.NAT == "" {
		info.NAT = api.NATKindUnknown
	}
	for _, p := range []int{o.Ports.TCP443, o.Ports.TCP7882} {
		if p > 0 && !slices.Contains(info.TCPPorts, p) {
			info.TCPPorts = append(info.TCPPorts, p)
		}
	}
	return info
}

// Probe is the typed function of POST /api/v1/conntest (04 §7.7) for the signed-in user userID: it starts the
// probe PC of req.Transport for req.Offer and returns its answer, which the browser applies as its remote
// description. The probe PC outlives the request: it closes after 20 s, when its data channel closes, or when the
// same user starts another probe of the same transport, which replaces it (02 §7.6).
//
// The checks, in order, each an *api.Error for httpapi.WriteError:
//   - the transport: a missing one is 422 validation_failed with fields.transport "required", one that is not
//     udp, tcp443 or tcp7882 the same with "invalid". The SFU is never asked about a transport it doesn't know;
//   - a transport without a listener (ConnTestPorts) is 409 transport_disabled with params.transport;
//   - the user's limit: 12 requests per minute, then 429 rate_limited with retryAfter (whole seconds, at least 1)
//     until the oldest of them is a minute old. Only requests that pass the two checks above count;
//   - the SFU: without one, 503 not_ready. The Prober's own *api.Error values are passed on as they are: the
//     wiring maps 02's errors to 400 bad_sdp (the offer is not exactly one data channel, or too large), 409
//     transport_disabled, 429 rate_limited (20 probes run on the server) and 503 not_ready (the SFU is closed).
//     Any other error, a failure of Pion's among them, is returned wrapped: WriteError answers 500 internal and
//     logs it.
//
// userID is the principal's id; "" is a wiring bug and an error. The offer is never logged (it holds the browser's
// addresses); api.ConnTestRequest keeps it out of fmt and slog.
func (c *ConnTest) Probe(ctx context.Context, userID string, req api.ConnTestRequest) (api.ConnTestResponse, error) {
	if userID == "" {
		return api.ConnTestResponse{}, errors.New("ops: conntest: no user")
	}
	switch req.Transport {
	case "":
		return api.ConnTestResponse{}, &api.Error{
			Code: api.CodeValidationFailed, Fields: map[string]string{"transport": api.FieldRequired},
		}
	case api.TransportUDP, api.TransportTCP443, api.TransportTCP7882:
	default:
		return api.ConnTestResponse{}, &api.Error{
			Code: api.CodeValidationFailed, Fields: map[string]string{"transport": api.FieldInvalid},
		}
	}
	if c.ports.of(req.Transport) <= 0 {
		return api.ConnTestResponse{}, TransportDisabled(req.Transport)
	}
	if wait := c.limit.take(userID); wait > 0 {
		c.log.Debug("connection test rate-limited", slog.String("user_id", userID),
			slog.String("transport", string(req.Transport)))
		return api.ConnTestResponse{}, &api.Error{Code: api.CodeRateLimited, RetryAfter: retryAfterSeconds(wait)}
	}
	if c.prober == nil {
		return api.ConnTestResponse{}, api.NewError(api.CodeNotReady)
	}
	answer, err := c.prober.Probe(ctx, userID, string(req.Transport), req.Offer)
	if err != nil {
		var ae *api.Error
		if errors.As(err, &ae) {
			return api.ConnTestResponse{}, err
		}
		return api.ConnTestResponse{}, fmt.Errorf("ops: conntest %s: %w", req.Transport, err)
	}
	if answer == "" {
		return api.ConnTestResponse{}, fmt.Errorf("ops: conntest %s: the SFU answered with an empty SDP", req.Transport)
	}
	c.log.Debug("connection test probe started", slog.String("user_id", userID),
		slog.String("transport", string(req.Transport)))
	return api.ConnTestResponse{Answer: answer, ExpiresInS: connTestExpiresInS, Server: c.server}, nil
}

// TransportDisabled is the answer for a connection test of transport t, which has no listener on this server:
// 409 transport_disabled with params.transport (04 §7.7). ConnTest returns it on its own; the wiring's Prober
// returns it for 02's sfu.transport_disabled, so both say the same.
func TransportDisabled(t api.Transport) *api.Error {
	return &api.Error{Code: api.CodeTransportDisabled, Params: map[string]any{api.ParamTransport: t}}
}

// retryAfterSeconds is a wait as the retryAfter of a 429: whole seconds, rounded up, at least 1.
func retryAfterSeconds(wait time.Duration) int {
	return max(int(math.Ceil(wait.Seconds())), 1)
}

// connTestLimiter is the per-user limit of the connection test (04 §7.7): at most connTestPerWindow requests within
// any connTestWindow. Each user's entry holds the times of their requests in the window, oldest first.
//
// Memory only, like every throttle: a restart forgets it. The map holds only the users who ran a test within the
// last two windows; older entries mean the same as none and are swept once per window, so it can't outgrow the
// signed-in users who tested in two minutes.
type connTestLimiter struct {
	now func() time.Time

	mu    sync.Mutex
	users map[string][]time.Time
	swept time.Time // when the stale entries were last removed
}

func newConnTestLimiter(now func() time.Time) *connTestLimiter {
	return &connTestLimiter{now: now, users: map[string][]time.Time{}}
}

// inWindow reports whether a request made at t still counts at now. A clock that was set back makes it count no
// more: an early test is harmless, a user locked out for the size of the step is not.
func inWindow(t, now time.Time) bool {
	d := now.Sub(t)
	return d >= 0 && d < connTestWindow
}

// take counts a request of user and returns 0, or, when the user has used up the window, how long until the
// oldest request leaves it (> 0) without counting this one.
func (l *connTestLimiter) take(user string) time.Duration {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if !inWindow(l.swept, now) {
		for u, times := range l.users {
			if len(times) == 0 || !inWindow(times[len(times)-1], now) {
				delete(l.users, u)
			}
		}
		l.swept = now
	}
	times := l.users[user]
	kept := times[:0]
	for _, t := range times {
		if inWindow(t, now) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= connTestPerWindow {
		l.users[user] = kept
		return kept[0].Add(connTestWindow).Sub(now)
	}
	l.users[user] = append(kept, now)
	return 0
}
