package signal

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/MoonWX/isshoni/internal/protocol"
)

// Config is the hub's configuration. The wiring (04 §6.6) fills it from 04's config and secrets; every duration
// and limit must be positive. Start from DefaultConfig.
type Config struct {
	PublicOrigin      string               // 04's Site.Origin, e.g. "https://watch.example.com"
	AllowedOrigins    []string             // extra exact origins: none in M1 (no config key); M2 adds Wails origins in code
	ServerVersion     string               // internal/version.Version() (04)
	ResumeKey         []byte               // 32 bytes, secrets.json keys.resume (04 config.KeyResume)
	ICEServers        []protocol.ICEServer // M1: nil (sent as [])
	Grace             time.Duration        // 30s
	HelloTimeout      time.Duration        // 10s
	IdleTimeout       time.Duration        // 45s
	PingInterval      time.Duration        // 15s (advertised; also the hub's WebSocket ping-frame interval, §3.4)
	StateCoalesce     time.Duration        // 200ms
	ShareStartTimeout time.Duration        // 30s
	StalledTimeout    time.Duration        // 30s
	RevalidateEvery   time.Duration        // 5m (03 Touch; also refreshes the session's last-seen time)
	Limits            Limits
}

// Limits are guards (constants), not admin policy. Only PreAuthPerIPPerMinute has a config key
// (04: limits.ws_handshakes_per_ip_per_minute).
type Limits struct {
	MaxSharesPerUser      int // 4
	MaxConnectionsPerUser int // 16
	PreAuthPerIPPerMinute int // 20
	MaxPreAuthConns       int // 500
	MessagesPerSecond     int // 20
	MessageBurst          int // 100
	BytesPerSecond        int // 512 << 10
	BytesBurst            int // 1 << 20
	SendQueueMessages     int // 512
	SendQueueBytes        int // 8 << 20
}

// DefaultConfig returns the values in the comments above; tests shorten the durations. PublicOrigin, ServerVersion
// and ResumeKey have no default.
func DefaultConfig() Config {
	return Config{
		Grace:             30 * time.Second,
		HelloTimeout:      10 * time.Second,
		IdleTimeout:       45 * time.Second,
		PingInterval:      15 * time.Second,
		StateCoalesce:     200 * time.Millisecond,
		ShareStartTimeout: 30 * time.Second,
		StalledTimeout:    30 * time.Second,
		RevalidateEvery:   5 * time.Minute,
		Limits: Limits{
			MaxSharesPerUser:      4,
			MaxConnectionsPerUser: 16,
			PreAuthPerIPPerMinute: 20,
			MaxPreAuthConns:       500,
			MessagesPerSecond:     20,
			MessageBurst:          100,
			BytesPerSecond:        512 << 10,
			BytesBurst:            1 << 20,
			SendQueueMessages:     512,
			SendQueueBytes:        8 << 20,
		},
	}
}

// resumeKeyLen is the length of Config.ResumeKey (04 config.KeyResume).
const resumeKeyLen = 32

func (c *Config) validate() error {
	var errs []error
	if len(c.ResumeKey) != resumeKeyLen {
		errs = append(errs, fmt.Errorf("ResumeKey must be %d bytes, got %d", resumeKeyLen, len(c.ResumeKey)))
	}
	for _, d := range []struct {
		name string
		v    time.Duration
	}{
		{"Grace", c.Grace}, {"HelloTimeout", c.HelloTimeout}, {"IdleTimeout", c.IdleTimeout},
		{"PingInterval", c.PingInterval}, {"StateCoalesce", c.StateCoalesce},
		{"ShareStartTimeout", c.ShareStartTimeout}, {"StalledTimeout", c.StalledTimeout},
		{"RevalidateEvery", c.RevalidateEvery},
	} {
		if d.v <= 0 {
			errs = append(errs, fmt.Errorf("%s must be positive, got %v", d.name, d.v))
		}
	}
	l := c.Limits
	for _, n := range []struct {
		name string
		v    int
	}{
		{"MaxSharesPerUser", l.MaxSharesPerUser}, {"MaxConnectionsPerUser", l.MaxConnectionsPerUser},
		{"PreAuthPerIPPerMinute", l.PreAuthPerIPPerMinute}, {"MaxPreAuthConns", l.MaxPreAuthConns},
		{"MessagesPerSecond", l.MessagesPerSecond}, {"MessageBurst", l.MessageBurst},
		{"BytesPerSecond", l.BytesPerSecond}, {"BytesBurst", l.BytesBurst},
		{"SendQueueMessages", l.SendQueueMessages}, {"SendQueueBytes", l.SendQueueBytes},
	} {
		if n.v <= 0 {
			errs = append(errs, fmt.Errorf("limit %s must be positive, got %d", n.name, n.v))
		}
	}
	if l.BytesBurst < protocol.MaxSDPBytes {
		errs = append(errs, fmt.Errorf("limit BytesBurst must be at least %d (the largest message), got %d",
			protocol.MaxSDPBytes, l.BytesBurst))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("signal: config: %w", err)
	}
	return nil
}

// Deps are the hub's dependencies (01 §15.2). Auth, Rooms, Media and ClientIP are required.
type Deps struct {
	Auth     Authenticator
	Rooms    RoomDirectory
	Media    MediaPlane
	Policy   func() Policy                  // admin soft limits and client floor, read on every use (wiring: 03 settings); nil: no policy
	Push     PushNotifier                   // optional (nil: no Web Push)
	ClientIP func(*http.Request) netip.Addr // 04 httpapi.ClientIP: honors trusted proxies
	Log      *slog.Logger                   // nil: discard
	Metrics  prometheus.Registerer          // optional (nil: no metrics); 04's ops.Metrics.Registerer()
}

// Policy is admin policy from 03's SettingsCache (pinnable by 04's config keys). Changes apply to the next
// room.join / share.start / hello; nothing already running is torn down.
type Policy struct {
	MinClientVersion    string // "" = none (setting minClientVersion)
	MaxRoomParticipants int    // 0 = none (setting maxParticipantsPerRoom)
	MaxRoomShares       int    // 0 = none (setting maxSharesPerRoom)
	MaxVideoBitrate     int64  // bit/s per share, 0 = preset default (setting maxShareBitrateKbps × 1000)
}

// ConnSelector selects connections by user, session or device (same fields as 03's auth.ConnSelector).
// UserID is required: an empty UserID matches nothing (ERROR log, returns 0). The other fields narrow within that user.
type ConnSelector struct {
	UserID          string
	SessionID       string // "" = any
	DeviceID        string // "" = any
	ExceptSessionID string // keep this one (password change; "sign out other browsers" names each session it ends)
}

func (s ConnSelector) matches(id *Identity) bool {
	return id.UserID == s.UserID &&
		(s.SessionID == "" || id.SessionID == s.SessionID) &&
		(s.DeviceID == "" || id.DeviceID == s.DeviceID) &&
		(s.ExceptSessionID == "" || id.SessionID != s.ExceptSessionID)
}

// Target selects connections for Notify. A connection matches when any field that is set matches it.
type Target struct {
	UserID string // "" = not by user
	RoomID string // "" = not by room
	Admins bool   // all connections of admin users
	All    bool   // every connection
}

// Hub is the signaling server (01): it upgrades /ws requests, runs the handshake and one actor per connection, and
// holds the connections, rooms and shares. New starts nothing; ServeHTTP starts the goroutines of each socket, and
// Shutdown stops them all.
//
// Lock order: hub (mu) → room → connection. No lock is held while calling a dependency, writing to a socket, or
// sending on a channel that can block.
type Hub struct {
	cfg      Config
	deps     Deps
	log      *slog.Logger
	origins  origins
	metrics  *metrics           // nil without Deps.Metrics
	features []protocol.Feature // features this server has enabled (welcome.features is the intersection with hello)

	// Step 4 of the upgrade: pre-auth upgrades per client IP key.
	preAuthIP *keyedLimiter
	// Security-event log lines (01 §17), each kind with its own budget of one line per client IP key per minute:
	// originLog for Origin 403s (step 2), throttleLog for pre-auth 429s (step 4) and, under allClientsKey, the
	// server-wide pre-auth limit.
	originLog   *keyedLimiter
	throttleLog *keyedLimiter

	// ctx is the hub's lifetime, for dependency calls and socket I/O after the upgrade. It is canceled when Shutdown
	// has finished or gives up.
	ctx    context.Context
	cancel context.CancelFunc

	// wg counts every goroutine the hub owns: each ServeHTTP call past the first check (it runs the socket's
	// reader), and each writer, pinger, connection actor, periodic Revalidate call and scheduled room.state
	// broadcast. Shutdown waits for it.
	wg sync.WaitGroup

	// down is closed when Shutdown begins. Every connection's actor then shuts its connection down: an attached one
	// through its socket, a detached one at once.
	down chan struct{}

	revs atomic.Uint64 // the last room.state rev (nextRev)

	mu             sync.Mutex
	closing        bool
	shutdownReason protocol.ShutdownReason
	stopped        chan struct{}        // closed once wg reaches zero after Shutdown began
	sockets        map[*socket]struct{} // every accepted socket until its reader has ended
	conns          map[string]*conn     // connections by id, ready or detached, until they close
	userSlots      map[string]int       // per user: connections (detached ones too) plus handshaking sockets (01 §3.1 step 5)
	overCap        map[string]int       // per user: cookie sockets that wait at the cap for a hello that resumes (ServeHTTP)
	preAuth        int                  // concurrent pre-auth sockets (01 §3.1 step 4)
	rooms          map[string]*room     // rooms with at least one participant, by id
	participants   int                  // participants over all rooms, CloseRoom's included until they are detached
	closedRooms    map[string]struct{}  // rooms closed by CloseRoom, for this process's lifetime
}

// serverFeatures are the features this server enables (01 §6.2): agent.relay, the same-user relay (relay.go).
var serverFeatures = []protocol.Feature{protocol.FeatureAgentRelay}

// New returns a hub. It validates cfg and deps, builds the metrics (registering them with deps.Metrics), and starts
// nothing.
func New(cfg Config, deps Deps) (*Hub, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	var missing []string
	if deps.Auth == nil {
		missing = append(missing, "Auth")
	}
	if deps.Rooms == nil {
		missing = append(missing, "Rooms")
	}
	if deps.Media == nil {
		missing = append(missing, "Media")
	}
	if deps.ClientIP == nil {
		missing = append(missing, "ClientIP")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("signal: deps: missing %s", strings.Join(missing, ", "))
	}
	o, err := newOrigins(cfg.PublicOrigin, cfg.AllowedOrigins)
	if err != nil {
		return nil, err
	}
	m, err := newMetrics(deps.Metrics)
	if err != nil {
		return nil, err
	}
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	cfg.ResumeKey = append([]byte(nil), cfg.ResumeKey...)
	cfg.AllowedOrigins = append([]string(nil), cfg.AllowedOrigins...)
	cfg.ICEServers = append([]protocol.ICEServer(nil), cfg.ICEServers...)
	ctx, cancel := context.WithCancel(context.Background())
	h := &Hub{
		cfg:         cfg,
		deps:        deps,
		log:         log.With(slog.String("component", "signal")),
		origins:     o,
		metrics:     m,
		features:    serverFeatures,
		preAuthIP:   newKeyedLimiter(perMinute(cfg.Limits.PreAuthPerIPPerMinute), maxLimiterKeys),
		originLog:   newKeyedLimiter(perMinute(1), maxLimiterKeys),
		throttleLog: newKeyedLimiter(perMinute(1), maxLimiterKeys),
		ctx:         ctx,
		cancel:      cancel,
		down:        make(chan struct{}),
		sockets:     make(map[*socket]struct{}),
		conns:       make(map[string]*conn),
		userSlots:   make(map[string]int),
		overCap:     make(map[string]int),
		rooms:       make(map[string]*room),
		closedRooms: make(map[string]struct{}),
	}
	return h, nil
}

// policy returns the current admin policy.
func (h *Hub) policy() Policy {
	if h.deps.Policy == nil {
		return Policy{}
	}
	return h.deps.Policy()
}

// Ready reports whether the hub accepts connections: true until Shutdown begins. 04 includes it in /readyz.
func (h *Hub) Ready() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.closing
}

// Shutdown sends server.shutdown{reason, reconnectInMs} and error{server_shutdown, scope connection, retryable}
// to every connection, closes the sockets with 1012 and returns when they are closed or ctx ends (§11.6).
// 04 maps its own reasons: stop → stop; restart and restore → restart.
//
// New upgrades get 503 from the moment Shutdown begins, and nothing resumes any more. A detached connection has no
// socket to tell: it closes at once, so Shutdown never waits for a grace. When ctx ends first, Shutdown closes the
// remaining sockets without a close handshake, waits for the hub's goroutines to end and returns ctx's error. Later
// calls wait the same way and change nothing.
func (h *Hub) Shutdown(ctx context.Context, reason protocol.ShutdownReason) error {
	if !reason.Valid() {
		h.log.Error("unknown shutdown reason, using restart", slog.String("reason", string(reason)))
		reason = protocol.ShutdownReasonRestart
	}
	h.mu.Lock()
	first := !h.closing
	conns := 0
	var socks []*socket
	if first {
		h.closing = true
		h.shutdownReason = reason
		h.stopped = make(chan struct{})
		conns = len(h.conns)
		close(h.down) // every connection's actor shuts its connection down (conn.run)
		for s := range h.sockets {
			if s.conn.Load() == nil {
				socks = append(socks, s)
			}
		}
	}
	stopped := h.stopped
	h.mu.Unlock()

	if first {
		h.log.Info("shutting down", slog.String("reason", string(reason)), slog.Int("connections", conns))
		for _, s := range socks {
			s.shutdown(reason)
		}
		// Owned by the first Shutdown call; it ends as soon as the hub's goroutines have, which a forced close
		// (below) bounds.
		go func() {
			h.wg.Wait()
			close(stopped)
		}()
	}

	select {
	case <-stopped:
		h.cancel()
		return nil
	case <-ctx.Done():
		h.forceClose()
		<-stopped
		return fmt.Errorf("signal: shutdown: %w", ctx.Err())
	}
}

// shutdownReasonNow returns the reason of the Shutdown that has begun.
func (h *Hub) shutdownReasonNow() protocol.ShutdownReason {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.shutdownReason
}

// forceClose cancels the hub's context and closes every socket's network connection, which ends every read,
// write and close handshake at once.
func (h *Hub) forceClose() {
	h.cancel()
	h.mu.Lock()
	socks := make([]*socket, 0, len(h.sockets))
	for s := range h.sockets {
		socks = append(socks, s)
	}
	h.mu.Unlock()
	for _, s := range socks {
		s.forceClose()
	}
}

// Notify sends invalidate{topics} to every connection that t selects (01 §8.12). It never blocks. Topics of the
// admin.* family reach only connections of admin users, whatever t says.
//
// Called through the wiring adapters (04 §6.6) by 03's REST handlers and admin actions.
func (h *Hub) Notify(t Target, topics ...protocol.Topic) {
	if len(topics) == 0 {
		return
	}
	var public []protocol.Topic
	for _, tp := range topics {
		if !isAdminTopic(tp) {
			public = append(public, tp)
		}
	}
	all, err := protocol.Marshal(protocol.MessageTypeInvalidate, "", "", protocol.Invalidate{Topics: topics})
	if err != nil {
		h.log.Error("encode invalidate", slog.Any("err", err))
		return
	}
	var nonAdmin []byte
	if len(public) > 0 {
		if nonAdmin, err = protocol.Marshal(protocol.MessageTypeInvalidate, "", "",
			protocol.Invalidate{Topics: public}); err != nil {
			h.log.Error("encode invalidate", slog.Any("err", err))
			return
		}
	}

	h.mu.Lock()
	var targets []*conn
	for _, c := range h.conns {
		id := c.identity()
		if t.All || (t.UserID != "" && id.UserID == t.UserID) || (t.Admins && id.Admin) ||
			(t.RoomID != "" && c.roomID == t.RoomID) {
			targets = append(targets, c)
		}
	}
	h.mu.Unlock()

	for _, c := range targets {
		msg := all
		if !c.identity().Admin {
			if nonAdmin == nil {
				continue
			}
			msg = nonAdmin
		}
		c.post(func() { c.sendEncoded(protocol.MessageTypeInvalidate, msg) })
	}
}

func isAdminTopic(t protocol.Topic) bool { return strings.HasPrefix(string(t), "admin.") }

// CloseConnections closes every matching connection with error{code, scope session} and 4401 (4403 for
// account_disabled). code is ErrorCodeSessionRevoked or ErrorCodeAccountDisabled. Returns the number closed.
// The wiring implements 03's auth.ConnCloser with it (reason → code table in §15.4).
//
// Sockets of that user that were authenticated by cookie and are still in the handshake are closed the same way.
// Closing is asynchronous: the sockets close right after the call returns. A revocation skips grace (01 §4.2): each
// connection ends with its socket, and a detached one, which has no socket to tell, ends at once (it is counted
// too). Their shares end with left. A revocation is recorded on the connection before its actor is told, so it holds
// even when the actor's inbox is full.
func (h *Hub) CloseConnections(sel ConnSelector, code protocol.ErrorCode) int {
	if sel.UserID == "" {
		h.log.Error("CloseConnections without a user id: nothing closed")
		return 0
	}
	if code != protocol.ErrorCodeSessionRevoked && code != protocol.ErrorCodeAccountDisabled {
		h.log.Error("CloseConnections with an unexpected code, using session_revoked", slog.String("code", string(code)))
		code = protocol.ErrorCodeSessionRevoked
	}
	h.mu.Lock()
	var conns []*conn
	for _, c := range h.conns {
		if id := c.identity(); sel.matches(&id) {
			conns = append(conns, c)
		}
	}
	var socks []*socket
	for s := range h.sockets {
		if s.conn.Load() == nil && s.cookie && sel.matches(&s.ident) {
			socks = append(socks, s)
		}
	}
	h.mu.Unlock()

	e := protocol.NewError(code, protocol.ErrorScopeSession)
	for _, c := range conns {
		c.revoked.Store(&e) // first: a full inbox can drop the post, but never the revocation
		c.post(c.applyRevocation)
	}
	for _, s := range socks {
		s.fail(e, "")
	}
	n := len(conns) + len(socks)
	h.log.Info("revocation", slog.String("user_id", sel.UserID), slog.String("code", string(code)),
		slog.Int("closed", n))
	return n
}

// UpdateUser applies a rename or role change to the user's open connections: their Identity (admin topics of Notify
// follow the flag) and the participant's name and admin flag in the room.state of every room they are in. Each
// connection's actor applies it; the snapshots with the new name or role go out coalesced like any change.
func (h *Hub) UpdateUser(userID, name string, admin bool) {
	h.mu.Lock()
	var conns []*conn
	for _, c := range h.conns {
		if c.userID == userID {
			conns = append(conns, c)
		}
	}
	h.mu.Unlock()
	for _, c := range conns {
		c.post(func() { c.setUser(name, admin) })
	}
}

// CloseRoom → room_closed (scope room); clients rejoin defaultRoomId. It also records roomID as closed for the
// process lifetime; a room.join that passed GetRoom before the delete gets room_not_found.
//
// The room's connections leave it (their shares end with room_closed and their MediaPeers close) and get
// error{room_closed, scope room, roomId}; connections in other rooms get nothing. Nothing more is sent about the room:
// no room.state and no room.event. Each connection's actor does its part right after the call returns.
func (h *Hub) CloseRoom(roomID string) {
	h.mu.Lock()
	h.closedRooms[roomID] = struct{}{}
	r := h.rooms[roomID]
	var conns []*conn
	if r != nil {
		delete(h.rooms, roomID)
		r.mu.Lock()
		r.closed = true
		r.stopTimerLocked()
		r.membersLocked(func(m *member) { conns = append(conns, m.c) })
		r.mu.Unlock()
		h.metrics.roomCounts(len(h.rooms), h.participants)
	}
	h.mu.Unlock()
	for _, c := range conns {
		c.post(func() { c.roomClosed(r) })
	}
	h.log.Info("room closed", slog.String("room_id", roomID), slog.Int("connections", len(conns)))
}

// Snapshot returns the live state for presence counts (03) and the admin dashboard (04): every room with at least
// one participant, sorted by id, with participants and shares in room.state order. Slices are never nil. A room's
// Name is the one its latest room.join read with GetRoom, so it lags a rename (03 §8) until the next join: 04's
// dashboard adapter prefers the store's current name. LiveShare.Layers and EgressBitrate come from the last
// MediaPeer.Stats of the share's publishing connection and of its subscribers, which their actors read every 2 s
// (stats.go): they are at most that old, and empty and 0 before the first read.
func (h *Hub) Snapshot() LiveSnapshot {
	h.mu.Lock()
	rooms := make([]*room, 0, len(h.rooms))
	for _, r := range h.rooms {
		rooms = append(rooms, r)
	}
	h.mu.Unlock()
	slices.SortFunc(rooms, func(a, b *room) int { return cmp.Compare(a.id, b.id) })
	snap := LiveSnapshot{Rooms: make([]LiveRoom, 0, len(rooms))}
	for _, r := range rooms {
		r.mu.Lock()
		if !r.closed {
			snap.Rooms = append(snap.Rooms, r.liveLocked())
		}
		r.mu.Unlock()
	}
	return snap
}

// later: func (h *Hub) Kick(roomID, userID string) error  // → kicked (scope room) + rejoin block
