package signal

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/MoonWX/isshoni/internal/protocol"
)

// depTimeout bounds each dependency call the hub makes after the upgrade (authentication, revalidation, the room
// directory).
const depTimeout = 10 * time.Second

// originKind classifies a request's Origin header (01 §3.1 step 2).
type originKind uint8

const (
	originAbsent  originKind = iota + 1 // no Origin header: a non-browser client
	originPublic                        // Config.PublicOrigin
	originAllowed                       // an entry of Config.AllowedOrigins: accepted only for bearer connections
	originBad                           // anything else, including several Origin headers
)

// origins is the exact Origin allowlist, normalized.
type origins struct {
	public  string
	allowed map[string]struct{}
}

func newOrigins(public string, allowed []string) (origins, error) {
	p, err := normalizeOrigin(public, true)
	if err != nil {
		return origins{}, fmt.Errorf("signal: config: PublicOrigin: %w", err)
	}
	o := origins{public: p, allowed: make(map[string]struct{}, len(allowed))}
	for _, a := range allowed {
		n, err := normalizeOrigin(a, false)
		if err != nil {
			return origins{}, fmt.Errorf("signal: config: AllowedOrigins: %w", err)
		}
		o.allowed[n] = struct{}{}
	}
	return o, nil
}

// classify compares the Origin header values with the allowlist: exactly equal after normalization (scheme and host
// case-insensitive, default ports removed).
func (o origins) classify(values []string) originKind {
	switch len(values) {
	case 0:
		return originAbsent
	case 1:
	default:
		return originBad
	}
	n, err := normalizeOrigin(values[0], false)
	switch {
	case err != nil:
		return originBad
	case n == o.public:
		return originPublic
	}
	if _, ok := o.allowed[n]; ok {
		return originAllowed
	}
	return originBad
}

// normalizeOrigin returns o the way a browser sends it in the Origin header: lower-case scheme and host, no default
// port (80 for http, 443 for https), canonical IPv6. An origin with a path, query, fragment or user info, an empty
// or non-ASCII host, or a bad port is an error, and so is the opaque origin "null". httpOnly limits the scheme to
// http and https; otherwise any scheme is allowed (the M2 desktop app's webview origins).
func normalizeOrigin(o string, httpOnly bool) (string, error) {
	bad := func(why string) (string, error) { return "", fmt.Errorf("origin %q: %s", o, why) }
	u, err := url.Parse(o)
	if err != nil {
		return bad("not a URL")
	}
	scheme := strings.ToLower(u.Scheme)
	switch {
	case scheme == "":
		return bad("no scheme")
	case httpOnly && scheme != "http" && scheme != "https":
		return bad("the scheme must be http or https")
	case u.Opaque != "" || u.User != nil || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || u.Host == "":
		return bad(`want "scheme://host[:port]"`)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return bad("empty host")
	}
	if a, err := netip.ParseAddr(host); err == nil {
		if a.Zone() != "" {
			return bad("an IPv6 zone is not allowed")
		}
		host = a.String()
	} else {
		for i := range len(host) {
			if !isHostChar(host[i]) {
				return bad("the host must be an IP address or an ASCII name")
			}
		}
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return bad("bad port")
		}
		port = strconv.Itoa(n)
		if (scheme == "https" && n == 443) || (scheme == "http" && n == 80) {
			port = ""
		}
	}
	switch {
	case port != "":
		return scheme + "://" + net.JoinHostPort(host, port), nil
	case strings.Contains(host, ":"):
		return scheme + "://[" + host + "]", nil
	default:
		return scheme + "://" + host, nil
	}
}

// isHostChar reports whether c may appear in a lower-cased host name: [a-z0-9.-_] (punycode for other scripts).
func isHostChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '-', c == '_':
		return true
	}
	return false
}

// maxLoggedOrigin bounds the Origin value in a security log line.
const maxLoggedOrigin = 128

// ServeHTTP upgrades GET /ws (mounted by 04). It runs the checks of 01 §3.1 in order, accepts the WebSocket, and
// then serves the socket until it closes: the hello handshake, then one message after another for the connection's
// actor.
//
//  1. The hub is not shutting down, else 503 with Retry-After: 2.
//  2. The Origin header is absent (a non-browser client) or exactly Config.PublicOrigin or an entry of
//     Config.AllowedOrigins, else 403. AllowedOrigins are accepted only for bearer connections: with a valid cookie
//     they get 403 too.
//  3. Authenticator.AuthenticateRequest: success makes the socket cookie-authenticated; ErrNoCredentials and
//     ErrInvalid make it pre-auth; any other error gets 503 with Retry-After: 2.
//  4. Pre-auth only: at most Limits.PreAuthPerIPPerMinute upgrades per client IP key per minute (429 with
//     Retry-After) and Limits.MaxPreAuthConns pre-auth sockets at once (503 with Retry-After: 2).
//  5. Cookie only: the user has fewer than Limits.MaxConnectionsPerUser connections and handshaking sockets, else
//     the socket is accepted and gets error{too_many_connections, scope connection} and 4429, unless its hello
//     resumes one of the user's connections. A resume needs no slot (the connection has its own), and the resume
//     token only arrives with hello, so a socket at the cap waits for its hello (handshake): the error is then the
//     reply to a hello that resumes nothing. At most MaxConnectionsPerUser sockets per user wait like this; beyond
//     that the error comes at once.
//  6. websocket.Accept without compression or subprotocols, and without coder/websocket's own Origin check (step 2
//     did the exact one; the library's compares with r.Host, which breaks behind proxies that rewrite Host).
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Step 1.
	if !h.Ready() {
		refuse(w, http.StatusServiceUnavailable, 2)
		return
	}
	ip := h.deps.ClientIP(r)
	key := ipKey(ip)

	// Step 2.
	origin := h.origins.classify(r.Header.Values("Origin"))
	if origin == originBad {
		h.securityEvent(h.originLog, key, ip, "websocket origin refused",
			slog.String("origin", truncate(r.Header.Get("Origin"), maxLoggedOrigin)))
		refuse(w, http.StatusForbidden, 0)
		return
	}

	// Step 3.
	id, err := h.deps.Auth.AuthenticateRequest(r)
	cookie := err == nil
	if err != nil && !errors.Is(err, ErrNoCredentials) && !errors.Is(err, ErrInvalid) {
		h.log.Warn("authenticate websocket upgrade", slog.Any("err", err))
		refuse(w, http.StatusServiceUnavailable, 2)
		return
	}
	if cookie && origin == originAllowed {
		h.securityEvent(h.originLog, key, ip, "websocket origin refused for a cookie connection")
		refuse(w, http.StatusForbidden, 0)
		return
	}
	if !cookie {
		id = Identity{}
	}

	// Steps 4 and 5, then register the socket.
	s := newSocket(h, ip, cookie, id)
	h.mu.Lock()
	switch {
	case h.closing:
		h.mu.Unlock()
		refuse(w, http.StatusServiceUnavailable, 2)
		return
	case !cookie && h.preAuth >= h.cfg.Limits.MaxPreAuthConns:
		h.mu.Unlock()
		if ok, _ := h.throttleLog.allow(allClientsKey, time.Now()); ok { // one line per minute server-wide
			h.log.Warn("pre-auth websocket limit reached", slog.Int("limit", h.cfg.Limits.MaxPreAuthConns))
		}
		refuse(w, http.StatusServiceUnavailable, 2)
		return
	case !cookie:
		if ok, wait := h.preAuthIP.allow(key, time.Now()); !ok {
			h.mu.Unlock()
			h.securityEvent(h.throttleLog, key, ip, "websocket pre-auth upgrades throttled")
			refuse(w, http.StatusTooManyRequests, retryAfterSeconds(wait))
			return
		}
		h.preAuth++
		s.preAuth = true
	case h.userSlots[id.UserID] < h.cfg.Limits.MaxConnectionsPerUser:
		h.userSlots[id.UserID]++
		s.slotUser = id.UserID
	case h.overCap[id.UserID] < h.cfg.Limits.MaxConnectionsPerUser:
		// At the cap. The user's connections count, detached ones too, and so does the socket a client opens to
		// resume one: the cap must not turn that socket away before its hello shows the resume token.
		h.overCap[id.UserID]++
		s.overUser = id.UserID
	default:
		s.tooMany = true
	}
	h.sockets[s] = struct{}{}
	h.wg.Add(1)
	h.mu.Unlock()
	defer h.wg.Done()
	defer h.dropSocket(s)

	// Step 6. The tap keeps the hijacked connection; a writer that can't hijack goes to Accept as is (it answers 501).
	tap := &hijackTap{ResponseWriter: w}
	aw := w
	if canHijack(w) {
		aw = tap
	}
	ws, err := websocket.Accept(aw, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // step 2 did the exact Origin check
		CompressionMode:    websocket.CompressionDisabled,
		OnPingReceived: func(context.Context, []byte) bool {
			s.touch()
			return true
		},
		OnPongReceived: func(context.Context, []byte) { s.touch() },
	})
	if err != nil { // Accept has answered the request
		h.log.Debug("websocket accept", slog.Any("err", err))
		return
	}
	s.serve(ws, tap.conn)
}

// refuse answers an upgrade that the hub turns down. retryAfter is in seconds; 0 sends no Retry-After.
func refuse(w http.ResponseWriter, status, retryAfter int) {
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	http.Error(w, http.StatusText(status), status)
}

// securityEvent logs a refused upgrade at WARN with the client IP (01 §17). budget is the limiter of the event's
// kind (Hub.originLog or Hub.throttleLog): each kind is logged at most once per client IP key per minute, so a
// flood cannot flood the log, and one kind never hides the other.
func (h *Hub) securityEvent(budget *keyedLimiter, key netip.Prefix, ip netip.Addr, msg string, attrs ...any) {
	if ok, _ := budget.allow(key, time.Now()); !ok {
		return
	}
	h.log.Warn(msg, append([]any{slog.String("remote_ip", ip.String())}, attrs...)...)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// allClientsKey is the security-log key of events about all clients at once; ipKey never returns a /128.
var allClientsKey = netip.PrefixFrom(netip.IPv6Unspecified(), 128)

// canHijack reports whether w, or a ResponseWriter it wraps (Unwrap, as http.ResponseController follows), is an
// http.Hijacker.
func canHijack(w http.ResponseWriter) bool {
	for {
		switch t := w.(type) {
		case http.Hijacker:
			return true
		case interface{ Unwrap() http.ResponseWriter }:
			w = t.Unwrap()
		default:
			return false
		}
	}
}

// hijackTap passes Hijack through to the ResponseWriter and keeps the hijacked connection, so a forced shutdown can
// close it directly.
type hijackTap struct {
	http.ResponseWriter
	conn net.Conn
}

func (t *hijackTap) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, rw, err := http.NewResponseController(t.ResponseWriter).Hijack()
	t.conn = c
	return c, rw, err
}

// Unwrap lets http.ResponseController reach the wrapped ResponseWriter.
func (t *hijackTap) Unwrap() http.ResponseWriter { return t.ResponseWriter }

// dropSocket releases what a socket holds in the hub once its reader has ended: its place in the socket set, its
// pre-auth count, and its per-user slot or its place at the cap (unless a connection took the slot over).
func (h *Hub) dropSocket(s *socket) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.sockets, s)
	if s.preAuth {
		h.preAuth--
		s.preAuth = false
	}
	h.releaseSocketLocked(s)
}

// releaseSocketLocked gives back what socket s holds for a connection of its own: its per-user slot, or its place
// among the sockets that wait at the user's cap.
func (h *Hub) releaseSocketLocked(s *socket) {
	if s.slotUser != "" {
		h.releaseSlotLocked(s.slotUser)
		s.slotUser = ""
	}
	if s.overUser != "" {
		if n := h.overCap[s.overUser] - 1; n > 0 {
			h.overCap[s.overUser] = n
		} else {
			delete(h.overCap, s.overUser)
		}
		s.overUser = ""
	}
}

func (h *Hub) releaseSlotLocked(userID string) {
	if n := h.userSlots[userID] - 1; n > 0 {
		h.userSlots[userID] = n
	} else {
		delete(h.userSlots, userID)
	}
}

// authenticated marks a pre-auth socket as authenticated by bearer at hello: it stops counting against the pre-auth
// limit.
func (h *Hub) authenticated(s *socket) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s.preAuth {
		h.preAuth--
		s.preAuth = false
	}
}

// takeSlot gives socket s the per-user slot of the connection its hello is about to open, unless it has had one
// since the upgrade. It fails when the user has Limits.MaxConnectionsPerUser connections and handshaking sockets
// already: a bearer socket, which is not checked at the upgrade, or a cookie socket that has waited at the cap
// (ServeHTTP step 5).
func (h *Hub) takeSlot(s *socket, userID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s.slotUser != "" {
		return true
	}
	if h.userSlots[userID] >= h.cfg.Limits.MaxConnectionsPerUser {
		return false
	}
	h.releaseSocketLocked(s) // its place at the cap
	h.userSlots[userID]++
	s.slotUser = userID
	return true
}

// handshake handles the first message of a socket (01 §8.2 steps 1–7). It returns the socket's connection, whose
// actor sends welcome: a new one, or the one that the hello's resume token resumed. It returns nil when the
// handshake failed and the socket is closing.
func (s *socket) handshake(b []byte) *conn {
	h := s.h
	env, err := protocol.ParseEnvelope(b)
	switch {
	case err != nil:
		s.fail(protocol.NewError(protocol.ErrorCodeBadMessage, protocol.ErrorScopeConnection), "")
		return nil
	case env.Type != protocol.MessageTypeHello:
		s.fail(protocol.NewError(protocol.ErrorCodeHelloRequired, protocol.ErrorScopeConnection), "")
		return nil
	case env.ID == "": // a request needs an id
		s.fail(protocol.NewError(protocol.ErrorCodeBadMessage, protocol.ErrorScopeConnection), "")
		return nil
	}
	h.metrics.message(env.Type, dirIn)

	// Step 2: validate.
	hello, err := protocol.Decode[protocol.Hello](env)
	if err != nil {
		var fe *protocol.FieldError
		if errors.As(err, &fe) {
			s.fail(fe.BadRequest(protocol.ErrorScopeConnection), env.ID)
		} else {
			s.failInternal(err, env.ID)
		}
		return nil
	}

	// Step 3: authenticate (01 §3.2).
	ctx, cancel := context.WithTimeout(h.ctx, depTimeout)
	defer cancel()
	id := s.ident
	switch {
	case s.cookie && hello.Auth != nil:
		fe := protocol.FieldError{Field: "auth", Reason: protocol.FieldInvalid}
		s.fail(fe.BadRequest(protocol.ErrorScopeConnection), env.ID)
		return nil
	case s.cookie:
	case hello.Auth != nil:
		bid, err := h.deps.Auth.AuthenticateBearer(ctx, hello.Auth.Token)
		switch {
		case errors.Is(err, ErrInvalid) || errors.Is(err, ErrNoCredentials):
			s.fail(protocol.NewError(protocol.ErrorCodeUnauthenticated, protocol.ErrorScopeSession), env.ID)
			return nil
		case err != nil:
			s.failInternal(fmt.Errorf("authenticate bearer: %w", err), env.ID)
			return nil
		}
		h.authenticated(s)
		id = bid
	default:
		s.fail(protocol.NewError(protocol.ErrorCodeUnauthenticated, protocol.ErrorScopeSession), env.ID)
		return nil
	}
	// The per-user cap (01 §3.1 step 5; §8.2 step 3 for bearer): a new connection needs a slot. A cookie socket has
	// had one since the upgrade, unless the user was at the cap then. A hello with a resume token goes on without
	// one: the connection it resumes has its own, so the token is checked first (step 5), and the cap after it when
	// the token resumes nothing.
	if hello.ResumeToken == "" && !h.takeSlot(s, id.UserID) {
		s.fail(protocol.NewError(protocol.ErrorCodeTooManyConnections, protocol.ErrorScopeConnection), env.ID)
		return nil
	}
	// Revalidate at connect: marks the session as seen and picks up a rename or role change (01 §3.2).
	switch rid, err := h.deps.Auth.Revalidate(ctx, id, s.ip); {
	case errors.Is(err, ErrInvalid):
		s.fail(protocol.NewError(protocol.ErrorCodeSessionRevoked, protocol.ErrorScopeSession), env.ID)
		return nil
	case err != nil:
		h.log.Warn("revalidate at connect", slog.String("user_id", id.UserID), slog.Any("err", err))
	default:
		id.Name, id.Admin = rid.Name, rid.Admin
	}

	// Step 4: version and client floor (01 §6.1).
	chosen, ok := protocol.Negotiate(hello.Protocol, hello.MinProtocol)
	if !ok {
		e := protocol.NewError(protocol.ErrorCodeProtocolUnsupported, protocol.ErrorScopeConnection)
		e.Params = map[string]any{
			"serverMin":     protocol.MinVersion,
			"serverMax":     protocol.Version,
			"serverVersion": h.cfg.ServerVersion,
		}
		s.fail(e, env.ID)
		return nil
	}
	pol := h.policy()
	if checksClientVersion(hello.Client.Kind) && pol.MinClientVersion != "" {
		below, ok := versionBelow(hello.Client.Version, pol.MinClientVersion)
		if !ok {
			h.log.Warn("unparseable minClientVersion policy, not enforced",
				slog.String("min_client_version", pol.MinClientVersion))
		} else if below {
			s.fail(protocol.NewError(protocol.ErrorCodeClientOutdated, protocol.ErrorScopeConnection), env.ID)
			return nil
		}
	}
	defaultRoom, err := h.deps.Rooms.DefaultRoomID(ctx)
	if err != nil {
		s.failInternal(fmt.Errorf("default room: %w", err), env.ID)
		return nil
	}

	// Step 5: a resume token resumes its connection (01 §10.3): the connection's actor takes the socket and sends
	// welcome{resumed: true}, then room.state and Resync (step 7). A token that resumes nothing is not an error: the
	// hello opens a new connection (welcome.resumed false), if the user has a slot for one.
	if hello.ResumeToken != "" {
		c := h.resume(hello.ResumeToken, resumeRequest{
			sock: s, re: env.ID, id: id, role: hello.Role, version: chosen, caps: hello.Caps,
			defaultRoom: defaultRoom, pol: pol,
		})
		h.metrics.resumeResult(c != nil)
		if c != nil {
			// Step 6 for a resumed connection. Only this goroutine reads from the socket, so the limit is up before
			// the first read after welcome.
			s.ws.SetReadLimit(protocol.MaxSDPBytes)
			s.startPinger()
			return c
		}
		if !h.takeSlot(s, id.UserID) {
			s.fail(protocol.NewError(protocol.ErrorCodeTooManyConnections, protocol.ErrorScopeConnection), env.ID)
			return nil
		}
	}

	// Step 6: raise the read limit; the actor sends welcome as its first act.
	c := newConn(h, s, id, &hello, chosen)
	if reason, ok := h.addConn(c, s); !ok {
		s.shutdown(reason)
		return nil
	}
	s.ws.SetReadLimit(protocol.MaxSDPBytes)
	s.startPinger()
	go c.run(func() { c.attach(s, env.ID, defaultRoom, pol, false) })
	return c
}

// checksClientVersion reports whether the client floor (Policy.MinClientVersion) applies to a client kind: the
// native and tool clients. Web clients are the server's own SPA build, and unknown kinds are treated like web.
func checksClientVersion(k protocol.ClientKind) bool {
	switch k {
	case protocol.ClientKindDesktop, protocol.ClientKindMobile, protocol.ClientKindTool:
		return true
	}
	return false
}

// addConn registers a new connection and moves the socket's per-user slot to it. It fails while the hub shuts down
// and returns the shutdown reason.
func (h *Hub) addConn(c *conn, s *socket) (protocol.ShutdownReason, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		return h.shutdownReason, false
	}
	h.conns[c.id] = c
	c.slotUser, s.slotUser = s.slotUser, ""
	s.conn.Store(c)
	h.wg.Add(1) // the actor
	h.metrics.connOpened(c.client.Kind, c.role)
	return "", true
}

// removeConn unregisters a closed connection and releases its per-user slot, which it has held since it opened,
// through every socket and every grace.
func (h *Hub) removeConn(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.conns, c.id)
	if c.slotUser != "" {
		h.releaseSlotLocked(c.slotUser)
		c.slotUser = ""
	}
}
