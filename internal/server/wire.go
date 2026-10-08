package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/auth"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/httpapi"
	"github.com/MoonWX/isshoni/internal/server/ops"
	"github.com/MoonWX/isshoni/internal/server/signal"
	"github.com/MoonWX/isshoni/internal/server/store"
	"github.com/MoonWX/isshoni/internal/version"
)

// This file is the wiring of 04 §6.6: it builds 03's store, account service and REST API, 01's hub and the admin
// socket's server on top of what Start has opened, and it holds every adapter between them. Each adapter is a few
// lines over a small interface of what it calls, so it has a table test with a fake behind it (wire_test.go).
//
// What the later slices of the M1 plan add here:
//   - README S59: the SFU on netx's Transport (transportOptions, public.go) with 01's sfuplane in place of noMedia,
//     the readiness check "media", SettingsCache.OnChange → SFU.SetLimits, and the SFU and the Transport in the
//     shutdown's step 4;
//   - README S71: push.New before auth.New, the push service as signal.Deps.Push, auth.Options.Alerts and
//     httpapi.Deps.Push;
//   - README S80 and S85: ops.Metrics as the router's Observer and the hub's Registerer, and the REST routes of ops
//     through API.Handle.

// The adapters and what they adapt: each consumer interface of 04 §6.6 on the left, and on the right the part of
// 01's hub and 03's service that an adapter calls.
var (
	_ signal.Authenticator = (*authenticator)(nil)
	_ signal.RoomDirectory = roomDirectory{}
	_ signal.MediaPlane    = noMedia{}
	_ auth.ConnCloser      = (*connCloser)(nil)
	_ httpapi.Signal       = signalAdapter{}
	_ httpapi.InfoSource   = buildInfo{}
	_ ops.AdminAccounts    = (*adminAccounts)(nil)

	_ sessionService = (*auth.Service)(nil)
	_ accountService = (*auth.Service)(nil)
	_ hubCloser      = (*signal.Hub)(nil)
	_ hubControl     = (*signal.Hub)(nil)
)

// ---- step 4: the store and the policy pins ----

// openStore is step 4 of the startup sequence (04 §6.1): it opens 03's store, which brings the database to this
// binary's schema after a backup of its own, and pins the policy settings that the config sets (04 §4.6).
//
// A store.Open error that wraps store.ErrNeedsOperator (a newer schema, a failed migration, a corrupt file, …) is a
// refusal: NeedsOperator reports it and cmd/isshoni prints 03's message and exits 78. For a newer schema with a
// pre-migration backup the message ends with the restore command for this environment. A value that the settings
// refuse is a *config.ValidationError naming the key, like every other config error.
func (s *Server) openStore(ctx context.Context, paths config.Paths) error {
	db, err := store.Open(ctx, store.Options{
		Path:       paths.DB,
		BackupDir:  paths.Backups,
		AppVersion: version.Version(),
		Clock:      s.deps.Now,
		Logger:     s.log,
	})
	if err != nil {
		return storeOpenError(err, s.inContainer())
	}
	s.store = db
	return pinPolicy(&s.cfg, db.Settings())
}

// storeOpenError completes a store.Open error for the operator: 03's message for a newer schema names the backup
// from before the upgrade, and 04 adds the command that restores it (04 §6.1 step 4, §6.3). Every other error is
// returned as it is.
func storeOpenError(err error, container bool) error {
	var tooNew *store.SchemaTooNewError
	if errors.As(err, &tooNew) && tooNew.Backup != "" {
		return fmt.Errorf("%w\n  fix: %s", err, restoreCommand(tooNew.Backup, container))
	}
	return err
}

// restoreCommand is the command that restores a database file while the server is stopped (04 §6.3, §12.6): the
// systemd form, or the Docker form in a container, where the stopped service's container is gone and a one-off
// container does the restore.
func restoreCommand(backup string, container bool) string {
	arg := shellQuote(backup)
	if container {
		return "docker compose stop && docker compose run --rm isshoni admin restore --offline " + arg +
			" && docker compose up -d"
	}
	return "sudo -u isshoni isshoni admin restore --offline " + arg + " && sudo systemctl start isshoni"
}

// shellQuote returns s as one word of a shell command: as it is when it has only plain characters, in single quotes
// otherwise.
func shellQuote(s string) string {
	plain := func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./=:,@%+", r)
	}
	if s != "" && strings.IndexFunc(s, func(r rune) bool { return !plain(r) }) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// policyKeys are the policy keys of the config registry (04 §4.3) with their value in a Config. A key of them that
// the operator set (flag, env or file) pins its setting, the registry's Key.Setting; a test checks the list against
// the registry, so a new policy key can't be forgotten here.
var policyKeys = []struct {
	path  string
	value func(*config.Config) any
}{
	{"registration.mode", func(c *config.Config) any { return c.Registration.Mode }},
	{"clients.min_version", func(c *config.Config) any { return c.Clients.MinVersion }},
	{"limits.max_participants_per_room", func(c *config.Config) any { return c.Limits.MaxParticipantsPerRoom }},
	{"limits.max_shares_per_room", func(c *config.Config) any { return c.Limits.MaxSharesPerRoom }},
	{"limits.max_bitrate_kbps", func(c *config.Config) any { return c.Limits.MaxBitrateKbps }},
	{"limits.transfer_alert_gb", func(c *config.Config) any { return c.Limits.TransferAlertGB }},
	{"updates.release_check", func(c *config.Config) any { return c.Updates.ReleaseCheck }},
}

// pinPolicy pins every policy key that the config sets (cfg.IsSet: by flag, env or file) into 03's settings
// (04 §4.6): the admin UI then shows the field read-only, and PATCH /admin/settings answers 409 setting_locked. A key
// that is not set stays the admin's to change. The settings are the only source of validation for these values: a
// value that Pin refuses comes back as a *config.ValidationError with one Problem per key (its source, and 03's
// field code in the message), on which serve exits 78.
func pinPolicy(cfg *config.Config, settings *store.SettingsCache) error {
	var problems []config.Problem
	for _, pk := range policyKeys {
		if !cfg.IsSet(pk.path) {
			continue
		}
		key, ok := config.Lookup(pk.path)
		if !ok || !key.Policy || key.Setting == "" {
			return fmt.Errorf("server: %s is not a policy key of the config registry", pk.path)
		}
		value := pk.value(cfg)
		err := settings.Pin(key.Setting, value)
		if err == nil {
			continue
		}
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Code != api.CodeValidationFailed {
			return fmt.Errorf("server: pin %s: %w", pk.path, err)
		}
		code := ae.Fields[key.Setting]
		if code == "" {
			code = api.FieldInvalid
		}
		problems = append(problems, config.Problem{
			Key:      pk.path,
			Value:    tomlValue(value),
			Source:   cfg.Source(pk.path),
			Severity: config.SeverityError,
			Message:  fmt.Sprintf("is not a value the %s setting takes (%s)", key.Setting, code),
			Fix:      key.Help,
		})
	}
	if len(problems) > 0 {
		return &config.ValidationError{Problems: problems}
	}
	return nil
}

// tomlValue writes a policy value (a string, an integer or a boolean) as TOML, for a config.Problem.
func tomlValue(v any) string {
	if s, ok := v.(string); ok {
		return strconv.Quote(s)
	}
	return fmt.Sprint(v)
}

// ---- step 8: the components ----

// How long the calls may take that the server makes on behalf of a caller without a context of its own: the
// readiness check "db" and the router's look at whether setup is still open.
const (
	dbPingTimeout     = time.Second
	dbPingEvery       = time.Second
	setupCheckTimeout = 2 * time.Second
)

// wire is step 8 of the startup sequence (04 §6.1) for 01's and 03's parts: the account service, whose key
// fingerprint check purges what a rotated key protected before any listener serves (03 §4.6), then the hub and the
// REST API with the adapters of 04 §6.6 between them, the readiness checks "db" and "signal" (04 §6.2), and the
// admin socket's server. It opens and starts nothing; Start serves what it builds.
//
// A server without a site (ip mode before a public address is known, 04 §7.4) has no accounts, no API and no hub:
// each needs the public origin, and the router answers 421 to every request but the health endpoints anyway. Its
// admin socket still answers health, ready and status, so the operator sees the failing check "public_ip".
func (s *Server) wire(ctx context.Context) error {
	// The wall clock, not Deps.Now: how old an answer is has nothing to do with what time the accounts think it is.
	s.health.AddCheck("db", newDBCheck(s.run, s.store.Ping, time.Now).ready)

	var accounts ops.AdminAccounts
	if s.site.Origin != "" {
		if err := s.wireAccounts(ctx); err != nil {
			return err
		}
		accounts = &adminAccounts{auth: s.accounts, users: userLister(s.store)}
	}
	if s.deps.LogLevel != nil {
		s.logLevel = ops.NewLogLevel(s.deps.LogLevel, s.log)
	}
	s.admin = ops.NewAdminServer(ops.AdminOptions{
		Health:   s.health,
		Status:   s.statusSource(),
		Accounts: accounts,
		LogLevel: s.logLevel,
		Logger:   s.log,
	})
	return nil
}

// wireAccounts builds the account service, the hub and the REST API for the site.
func (s *Server) wireAccounts(ctx context.Context) error {
	origin := s.site.Origin

	// auth comes before the hub (04 §6.1 step 8), and each needs the other: the hub authenticates through auth, and
	// auth closes the hub's connections on a revocation. The closer is bound once the hub exists.
	closer := &connCloser{}
	accounts, err := auth.New(ctx, s.store, auth.Options{
		Keys: auth.Keys{Session: s.secrets.Key(config.KeySession), Invite: s.secrets.Key(config.KeyInvite)},
		// One public origin (04 §4.4): it builds the setup, invite and reset links and is the only origin the REST
		// CSRF check trusts. The hub's Origin allowlist below is the same one.
		Origins:  func() auth.Origins { return auth.Origins{Primary: origin, Public: []string{origin}} },
		ClientIP: httpapi.ClientIP,
		Conns:    closer,
		// Alerts is the push service (README S71); until then admin alerts are only logged.
		Argon:  s.deps.Argon,
		Clock:  s.deps.Now,
		Logger: s.log,
	})
	if err != nil {
		return fmt.Errorf("server: %w", err)
	}

	hubCfg := signal.DefaultConfig()
	hubCfg.PublicOrigin = origin
	hubCfg.AllowedOrigins = s.site.ExtraOrigins // none in M1
	hubCfg.ServerVersion = version.Version()
	hubCfg.ResumeKey = s.secrets.Key(config.KeyResume) // resume tokens die with a rotated key (04 §5.2)
	hubCfg.Limits.PreAuthPerIPPerMinute = s.cfg.Limits.WSHandshakesPerIPPerMinute
	settings := s.store.Settings()
	hub, err := signal.New(hubCfg, signal.Deps{
		Auth:     &authenticator{sessions: accounts, user: userReader(s.store)},
		Rooms:    roomDirectory{db: s.store},
		Media:    s.mediaPlane(),
		Policy:   func() signal.Policy { return policyOf(settings.Get()) },
		ClientIP: httpapi.ClientIP,
		Log:      s.log,
		// Push is the push service (README S71) and Metrics is ops.Metrics.Registerer() (README S85).
	})
	if err != nil {
		return fmt.Errorf("server: %w", err)
	}
	closer.bind(hub)

	s.accounts, s.hub = accounts, hub
	s.api = httpapi.New(httpapi.Deps{
		DB:     s.store,
		Auth:   accounts,
		Signal: signalAdapter{hub: hub},
		// Push stays nil until the push wiring (README S71): the push endpoints answer push_unavailable and GET /info
		// has no push object, as with push.enabled = false.
		Info:     buildInfo{},
		Site:     s.site,
		ClientIP: httpapi.ClientIP,
		Clock:    s.deps.Now,
		Logger:   s.log,
	})
	s.health.AddCheck("signal", func() (bool, string) {
		if hub.Ready() {
			return true, ""
		}
		return false, "signaling is shutting down"
	})
	return nil
}

// mediaPlane returns the hub's MediaPlane. README S59 builds the SFU here (sfu.New on the Transport, with
// sfuplane.New's RoomEvents, then Plane.Bind) and returns the plane; until then the server has no media.
func (s *Server) mediaPlane() signal.MediaPlane { return noMedia{} }

// spaStatus is the router's SPAStatus hook (03 §12.6, 04 §9.5): the web app's /setup answers 404 once setup is
// done, that is once an admin account exists (auth.SetupAvailable). Every other path is 200.
//
// The hook has no context, so the look at the database is bounded by setupCheckTimeout; when it fails the page is
// served, and its own API calls then say what is wrong. An admin account never goes away again while the server
// runs (the last-admin rule, 03 §7.11), so the answer "done" is kept and costs no read from then on.
func (s *Server) spaStatus(path string) int {
	if strings.TrimSuffix(path, "/") != "/setup" || s.accounts == nil {
		return http.StatusOK
	}
	if s.setupDone.Load() {
		return http.StatusNotFound
	}
	ctx, cancel := context.WithTimeout(s.run, setupCheckTimeout)
	defer cancel()
	available, err := s.accounts.SetupAvailable(ctx)
	switch {
	case err != nil:
		s.log.Debug("could not tell whether setup is still open; serving /setup", logx.Err(err))
		return http.StatusOK
	case available:
		return http.StatusOK
	}
	s.setupDone.Store(true)
	return http.StatusNotFound
}

// setupHint logs how to finish setup while no admin account exists (04 §6.1 step 10, 03 §7.8). The setup token
// itself is never logged: `isshoni setup-url` prints it.
func (s *Server) setupHint(ctx context.Context) {
	if s.accounts == nil {
		return
	}
	available, err := s.accounts.SetupAvailable(ctx)
	if err != nil || !available {
		return
	}
	s.log.Info("Finish setup: run `sudo isshoni setup-url` (Docker: `docker compose exec isshoni isshoni setup-url`)")
}

// dbCheck is the readiness check "db" (04 §6.2): store.DB.Ping, a SELECT 1 on the writer and on a reader. /readyz is
// public and a ping takes the store's one writer connection, so an answer is kept for dbPingEvery: however many
// requests ask, the database sees one ping per interval, and callers that arrive during a ping wait for its answer.
// The interval counts from the answer, not from the question: a ping that took its whole timeout would otherwise
// leave an answer that is stale already, and every caller that waited for it would send a ping of its own, one after
// the other.
type dbCheck struct {
	ctx  context.Context // the server's life: a ping never outlives it
	ping func(context.Context) error
	now  func() time.Time

	mu     sync.Mutex
	at     time.Time // when the last ping's answer came; zero before the first
	ok     bool
	detail string
}

func newDBCheck(ctx context.Context, ping func(context.Context) error, now func() time.Time) *dbCheck {
	return &dbCheck{ctx: ctx, ping: ping, now: now}
}

// ready is the check function for ops.Health.AddCheck.
func (c *dbCheck) ready() (ok bool, detail string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now := c.now(); !c.at.IsZero() && now.Sub(c.at) < dbPingEvery && !now.Before(c.at) {
		return c.ok, c.detail
	}
	ctx, cancel := context.WithTimeout(c.ctx, dbPingTimeout)
	defer cancel()
	c.ok, c.detail = true, ""
	if err := c.ping(ctx); err != nil {
		c.ok, c.detail = false, "the database does not answer: "+err.Error()
	}
	c.at = c.now()
	return c.ok, c.detail
}

// ---- signal.Authenticator over 03's auth.Service ----

// sessionService is the part of *auth.Service behind the hub's Authenticator (03 §7.6).
type sessionService interface {
	AuthenticateCookie(r *http.Request) (auth.Principal, error)
	Touch(ctx context.Context, p auth.Principal, ip netip.Addr) error
}

// authenticator implements signal.Authenticator (04 §6.6): the session cookie at the upgrade, auth.Touch and a
// re-read of the user at connect and every five minutes. Only "the credential is gone" becomes signal.ErrInvalid
// (the hub then closes with session_revoked); every other error is passed on as it is, which the hub takes as
// transient: 503 at the upgrade, and at a revalidation it keeps the connection and asks again later.
type authenticator struct {
	sessions sessionService
	user     func(ctx context.Context, id store.UserID) (store.User, error)
}

// AuthenticateRequest reads the session cookie and nothing else: an Authorization header counts for nothing on /ws
// (03 §7.6), and an upgrade never rotates the session token.
func (a *authenticator) AuthenticateRequest(r *http.Request) (signal.Identity, error) {
	p, err := a.sessions.AuthenticateCookie(r)
	switch {
	case err == nil:
		return identityOf(p), nil
	case errors.Is(err, auth.ErrNoCookie):
		return signal.Identity{}, signal.ErrNoCredentials
	case api.IsCode(err, api.CodeUnauthenticated):
		return signal.Identity{}, signal.ErrInvalid
	default:
		return signal.Identity{}, err
	}
}

// AuthenticateBearer always answers signal.ErrInvalid in M1: device tokens come with the native apps (M2,
// auth.AuthenticateBearerToken).
func (a *authenticator) AuthenticateBearer(context.Context, protocol.Secret) (signal.Identity, error) {
	return signal.Identity{}, signal.ErrInvalid
}

// Revalidate checks that the session behind id is still there and marks it as seen (auth.Touch), then reads the user
// again: a rename or a role change made on the CLI reaches the open connections this way (03 §7.7).
func (a *authenticator) Revalidate(ctx context.Context, id signal.Identity, ip netip.Addr) (signal.Identity, error) {
	p := auth.Principal{
		UserID:    store.UserID(id.UserID),
		Method:    auth.MethodSession,
		SessionID: store.SessionID(id.SessionID),
	}
	if id.SessionID == "" { // a device (M2): auth.Touch has no devices yet and says unauthenticated
		p.Method, p.DeviceID = auth.MethodBearer, store.DeviceID(id.DeviceID)
	}
	if err := a.sessions.Touch(ctx, p, ip); err != nil {
		if api.IsCode(err, api.CodeUnauthenticated) {
			return signal.Identity{}, signal.ErrInvalid
		}
		return signal.Identity{}, err
	}
	u, err := a.user(ctx, p.UserID)
	switch {
	case errors.Is(err, store.ErrNotFound): // deleted since the Touch
		return signal.Identity{}, signal.ErrInvalid
	case err != nil:
		return signal.Identity{}, fmt.Errorf("server: revalidate: read the user: %w", err)
	case u.Status != store.StatusActive:
		return signal.Identity{}, signal.ErrInvalid
	}
	id.Name, id.Admin = u.Username, u.Role == store.RoleAdmin
	return id, nil
}

// identityOf is the hub's Identity of an auth.Principal (04 §6.6).
func identityOf(p auth.Principal) signal.Identity {
	return signal.Identity{
		UserID:    string(p.UserID),
		Name:      p.Username,
		Admin:     p.Role == store.RoleAdmin,
		SessionID: string(p.SessionID),
		DeviceID:  string(p.DeviceID),
	}
}

// userReader reads one user from the store.
func userReader(db *store.DB) func(context.Context, store.UserID) (store.User, error) {
	return func(ctx context.Context, id store.UserID) (store.User, error) {
		var u store.User
		err := db.Read(ctx, func(q *store.Q) error {
			var err error
			u, err = q.UserByID(id)
			return err
		})
		return u, err
	}
}

// ---- signal.RoomDirectory over 03's store ----

// roomDirectory implements signal.RoomDirectory (04 §6.6): rooms are 03's rows, read at every use, so a rename
// needs no hook (03 §8).
type roomDirectory struct{ db *store.DB }

// GetRoom returns the room's id and name, or signal.ErrNotFound.
func (d roomDirectory) GetRoom(ctx context.Context, roomID string) (protocol.RoomInfo, error) {
	var room store.Room
	err := d.db.Read(ctx, func(q *store.Q) error {
		var err error
		room, err = q.RoomByID(store.RoomID(roomID))
		return err
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		return protocol.RoomInfo{}, signal.ErrNotFound
	case err != nil:
		return protocol.RoomInfo{}, fmt.Errorf("server: read room: %w", err)
	}
	return protocol.RoomInfo{ID: string(room.ID), Name: room.Name}, nil
}

// DefaultRoomID is store.DefaultRoomID: Lounge, which every database has and which can't be deleted (03 §8).
func (roomDirectory) DefaultRoomID(context.Context) (string, error) {
	return string(store.DefaultRoomID), nil
}

// CanJoin lets everyone in: M1 has no room locks.
func (roomDirectory) CanJoin(context.Context, signal.Identity, string) error { return nil }

// policyOf is the hub's Policy of 03's settings (04 §6.6). The hub reads it on every use, so a change in the admin
// UI applies to the next join, share or hello.
func policyOf(s store.Settings) signal.Policy {
	return signal.Policy{
		MinClientVersion:    s.MinClientVersion,
		MaxRoomParticipants: s.MaxParticipantsPerRoom,
		MaxRoomShares:       s.MaxSharesPerRoom,
		MaxVideoBitrate:     int64(s.MaxShareBitrateKbps) * 1000, // kbit/s → bit/s
	}
}

// ---- auth.ConnCloser over 01's hub ----

// hubCloser is the part of *signal.Hub behind auth's ConnCloser.
type hubCloser interface {
	CloseConnections(sel signal.ConnSelector, code protocol.ErrorCode) int
}

// connCloser implements auth.ConnCloser (03 §7.7, 01 §15.4): a revocation closes the matching WebSockets at once.
// The reason account_disabled goes on the wire as account_disabled (close 4403), every other reason as
// session_revoked (4401).
//
// auth is built before the hub, so the hub is bound afterwards, before the server serves; until then (auth's own
// startup purge has no connection to close) it closes nothing.
type connCloser struct {
	hub hubCloser
}

func (c *connCloser) bind(hub hubCloser) { c.hub = hub }

// CloseConnections implements auth.ConnCloser.
func (c *connCloser) CloseConnections(sel auth.ConnSelector, reason string) int {
	if c.hub == nil {
		return 0
	}
	return c.hub.CloseConnections(signal.ConnSelector{
		UserID:          string(sel.UserID),
		SessionID:       string(sel.SessionID),
		DeviceID:        string(sel.DeviceID),
		ExceptSessionID: string(sel.ExceptSessionID),
	}, revocationCode(reason))
}

// revocationCode maps 03's revocation reason to 01's error code (01 §15.4).
func revocationCode(reason string) protocol.ErrorCode {
	if reason == auth.ReasonAccountDisabled {
		return protocol.ErrorCodeAccountDisabled
	}
	return protocol.ErrorCodeSessionRevoked
}

// ---- httpapi.Signal over 01's hub ----

// hubControl is the part of *signal.Hub behind httpapi's Signal.
type hubControl interface {
	Snapshot() signal.LiveSnapshot
	CloseRoom(roomID string)
	UpdateUser(userID, name string, admin bool)
	Notify(t signal.Target, topics ...protocol.Topic)
}

// signalAdapter implements httpapi.Signal (03 §12.5, 04 §6.6): the live counts of the room and user lists, and the
// hooks 03's REST handlers call after a commit.
type signalAdapter struct{ hub hubControl }

// RoomPresence counts the participants and shares of every room that has someone in it.
func (a signalAdapter) RoomPresence() map[store.RoomID]httpapi.RoomPresence {
	snap := a.hub.Snapshot()
	out := make(map[store.RoomID]httpapi.RoomPresence, len(snap.Rooms))
	for _, r := range snap.Rooms {
		out[store.RoomID(r.ID)] = httpapi.RoomPresence{Participants: len(r.Participants), Shares: len(r.Shares)}
	}
	return out
}

// OnlineUserIDs returns the users with a connection in a room, a reconnecting one included: the hub's snapshot is
// made of rooms, and a web client is in one for as long as its tab is open.
func (a signalAdapter) OnlineUserIDs() map[store.UserID]struct{} {
	out := map[store.UserID]struct{}{}
	for _, r := range a.hub.Snapshot().Rooms {
		for _, p := range r.Participants {
			if len(p.Connections) > 0 {
				out[store.UserID(p.UserID)] = struct{}{}
			}
		}
	}
	return out
}

// RoomDeleted closes the room in the hub: its connections get room_closed and rejoin the default room.
func (a signalAdapter) RoomDeleted(id store.RoomID) { a.hub.CloseRoom(string(id)) }

// UserChanged refreshes the user's name and role on its open connections.
func (a signalAdapter) UserChanged(id store.UserID, username string, admin bool) {
	a.hub.UpdateUser(string(id), username, admin)
}

// Notify sends invalidate{topics} to the connections t selects.
func (a signalAdapter) Notify(t httpapi.NotifyTarget, topics ...protocol.Topic) {
	a.hub.Notify(signal.Target{UserID: string(t.UserID), Admins: t.Admins, All: t.All}, topics...)
}

// ---- httpapi.InfoSource ----

// buildInfo implements httpapi.InfoSource (04 §6.6): this binary's version and 01's protocol range, for GET /info.
type buildInfo struct{}

func (buildInfo) ServerVersion() string { return version.Version() }

func (buildInfo) Protocol() (current, minimum int) { return protocol.Version, protocol.MinVersion }

// ---- ops.AdminAccounts over 03's auth.Service and store ----

// accountService is the part of *auth.Service behind the admin socket (03 §12.6).
type accountService interface {
	SetupAvailable(ctx context.Context) (bool, error)
	IssueSetupToken(ctx context.Context, a store.Actor) (auth.Link, error)
	UserByUsername(ctx context.Context, username string) (store.User, error)
	IssuePasswordReset(ctx context.Context, a store.Actor, id store.UserID, actorPassword string) (auth.Link, error)
	UpdateUser(ctx context.Context, a store.Actor, id store.UserID, ch auth.UserChange) (store.User, error)
	CreateInvite(ctx context.Context, a store.Actor, in auth.InviteInput) (store.Invite, auth.Link, error)
}

// adminAccounts implements ops.AdminAccounts (04 §6.6, §12.5): the operator's CLI acts through 03's service as
// store.CLIActor, so every call follows the same rules as the admin pages (the last-admin rule, the invite limits)
// and is audited as "cli". Errors are 03's *api.Error values, passed on unchanged.
type adminAccounts struct {
	auth  accountService
	users func(ctx context.Context) ([]store.UserRow, error)
}

func (a *adminAccounts) SetupAvailable(ctx context.Context) (bool, error) {
	return a.auth.SetupAvailable(ctx)
}

func (a *adminAccounts) IssueSetupLink(ctx context.Context) (ops.IssuedLink, error) {
	link, err := a.auth.IssueSetupToken(ctx, store.CLIActor)
	return issuedLink(link, err)
}

func (a *adminAccounts) Users(ctx context.Context) ([]ops.AdminUser, error) {
	rows, err := a.users(ctx)
	if err != nil {
		return nil, fmt.Errorf("server: list users: %w", err)
	}
	out := make([]ops.AdminUser, 0, len(rows))
	for _, u := range rows {
		out = append(out, ops.AdminUser{
			ID:         string(u.ID),
			Username:   u.Username,
			Role:       api.Role(u.Role),
			Status:     api.UserStatus(u.Status),
			CreatedAt:  api.WireTime(u.CreatedAt),
			LastSeenAt: api.WireTime(u.LastSeenAt), // zero for a user who never signed in: left out of the JSON
		})
	}
	return out, nil
}

func (a *adminAccounts) IssueResetLink(ctx context.Context, username string) (ops.IssuedLink, error) {
	u, err := a.auth.UserByUsername(ctx, username)
	if err != nil {
		return ops.IssuedLink{}, err
	}
	// No password: the CLI is the operator, whom the socket's peer check has let in (04 §12.1).
	link, err := a.auth.IssuePasswordReset(ctx, store.CLIActor, u.ID, "")
	return issuedLink(link, err)
}

func (a *adminAccounts) SetRole(ctx context.Context, username string, role api.Role) error {
	r := store.Role(role)
	return a.update(ctx, username, auth.UserChange{Role: &r})
}

func (a *adminAccounts) SetDisabled(ctx context.Context, username string, disabled bool) error {
	status := store.StatusActive
	if disabled {
		status = store.StatusDisabled
	}
	return a.update(ctx, username, auth.UserChange{Status: &status})
}

// update applies a change to the user with this name. A role change reaches the user's open connections at the
// hub's next Revalidate (03 §7.7); disabling closes them through auth's ConnCloser.
func (a *adminAccounts) update(ctx context.Context, username string, ch auth.UserChange) error {
	u, err := a.auth.UserByUsername(ctx, username)
	if err != nil {
		return err
	}
	_, err = a.auth.UpdateUser(ctx, store.CLIActor, u.ID, ch)
	return err
}

// CreateInvite passes 0 for a value the operator left out, so 03's invite settings apply (03 §9).
func (a *adminAccounts) CreateInvite(ctx context.Context, maxUses, ttlHours int) (ops.IssuedLink, error) {
	_, link, err := a.auth.CreateInvite(ctx, store.CLIActor, auth.InviteInput{MaxUses: maxUses, ExpiresInHours: ttlHours})
	return issuedLink(link, err)
}

// issuedLink turns auth's link into the socket's: the URL carries its token in the fragment, so it travels as a
// secret from here on.
func issuedLink(link auth.Link, err error) (ops.IssuedLink, error) {
	if err != nil {
		return ops.IssuedLink{}, err
	}
	return ops.IssuedLink{URL: logx.Secret(link.URL), ExpiresAt: link.ExpiresAt}, nil
}

// userLister lists every account from the store, as the store orders them.
func userLister(db *store.DB) func(context.Context) ([]store.UserRow, error) {
	return func(ctx context.Context) ([]store.UserRow, error) {
		var rows []store.UserRow
		err := db.Read(ctx, func(q *store.Q) error {
			var err error
			rows, err = q.ListUsers("")
			return err
		})
		return rows, err
	}
}

// ---- the admin socket's GET /v1/status ----

// statusSource returns the function behind the admin socket's GET /v1/status (04 §12.2, `isshoni admin status`):
// what the running process knows about itself. The site, the listeners and the public addresses are fixed once
// Start has bound everything, so the function keeps its own copies; the certificate, the live counts and the uptime
// are read at each call. The media parts (the advertised addresses, the socket buffers) come with the SFU (README
// S59), transfer and the release check with the ops data (README S85).
func (s *Server) statusSource() func(context.Context) (api.ServerStatus, error) {
	started := s.now()
	base := api.ServerStatus{
		Version:       version.Version(),
		StartedAt:     started,
		Origin:        s.site.Origin,
		NAT:           s.public.NAT,
		Advertised:    []api.AdvertisedAddr{},
		Listeners:     s.listeners(),
		SchemaVersion: s.store.SchemaVersion(),
	}
	if s.public.V4.IsValid() {
		base.PublicIPv4, base.PublicIPv4Method = s.public.V4.String(), string(s.public.V4Method)
	}
	if s.public.V6.IsValid() {
		base.PublicIPv6, base.PublicIPv6Method = s.public.V6.String(), string(s.public.V6Method)
	}
	if s.public.LocalV4.IsValid() {
		base.LocalIPv4 = s.public.LocalV4.String()
	}
	tls, hub, now := s.tls, s.hub, s.now
	return func(context.Context) (api.ServerStatus, error) {
		st := base
		st.UptimeS = int64(now().Sub(started) / time.Second)
		cert := tls.Status()
		st.TLS = api.TLSInfo{
			Mode:          api.TLSMode(cert.Mode),
			Names:         cert.Names,
			Ready:         cert.Ready,
			Issuer:        cert.Issuer,
			NotBefore:     cert.NotBefore,
			NotAfter:      cert.NotAfter,
			NextRenewal:   cert.NextRenewal,
			LastErrorCode: cert.LastErrorCode,
			LastErrorAt:   cert.LastErrorAt,
		}
		if hub != nil {
			for _, r := range hub.Snapshot().Rooms {
				st.Rooms++
				st.Participants += len(r.Participants)
				st.Shares += len(r.Shares)
			}
		}
		return st, nil
	}
}

// listeners lists what the server has bound, by config key (api.ListenerInfo).
func (s *Server) listeners() []api.ListenerInfo {
	var out []api.ListenerInfo
	if s.addrs.HTTPS != nil {
		out = append(out, api.ListenerInfo{Key: "listen.https", Network: "tcp", Addr: s.addrs.HTTPS.String()})
	}
	if s.addrs.HTTP != nil {
		out = append(out, api.ListenerInfo{Key: "listen.http", Network: "tcp", Addr: s.addrs.HTTP.String()})
	}
	if s.adminLn != nil {
		out = append(out, api.ListenerInfo{Key: "listen.admin_socket", Network: "unix", Addr: s.cfg.Listen.AdminSocket})
	}
	return out
}

// ---- the media plane before the SFU ----

// noMedia is the hub's MediaPlane until the SFU runs in the server (README S59; 04 slice W1 wires the hub with a
// media plane that is not the real one). Signaling is complete with it: connections join rooms, see each other and
// resume. What needs media is refused: share.start and the pc.* messages get error{feature_disabled}, and a
// subscription finds no share. It keeps no state, so nothing grows however long the server runs.
type noMedia struct{}

// NewPeer implements signal.MediaPlane.
func (noMedia) NewPeer(signal.PeerParams, signal.MediaSink) (signal.MediaPeer, error) {
	return noMediaPeer{}, nil
}

// noMediaPeer is the media side of a connection on a server without media.
type noMediaPeer struct{}

// errNoMedia is the answer to a request that needs the SFU.
func errNoMedia() error {
	e := protocol.NewError(protocol.ErrorCodeFeatureDisabled, protocol.ErrorScopeRequest)
	return &e
}

// errNoMediaPC is the answer to a pc.* message: scope pc with the message's pc, gen and neg (01 §15.4).
func errNoMediaPC(pc protocol.PCKind, gen, neg uint32) error {
	e := protocol.NewError(protocol.ErrorCodeFeatureDisabled, protocol.ErrorScopePC)
	e.PC, e.Gen, e.Neg = pc, gen, neg
	return &e
}

func (noMediaPeer) CreateShare(string, protocol.ShareStart) (protocol.ShareParams, error) {
	return protocol.ShareParams{}, errNoMedia()
}

func (noMediaPeer) UpdateShare(string, protocol.ShareUpdate) (protocol.ShareParams, error) {
	return protocol.ShareParams{}, errNoMedia()
}

func (noMediaPeer) EndShare(string, protocol.EndReason) {}

func (noMediaPeer) HandleOffer(o protocol.PCOffer) (protocol.PCAnswer, error) {
	return protocol.PCAnswer{}, errNoMediaPC(o.PC, o.Gen, o.Neg)
}

func (noMediaPeer) HandleAnswer(a protocol.PCAnswer) error { return errNoMediaPC(a.PC, a.Gen, a.Neg) }

func (noMediaPeer) AddICE(c protocol.PCICE) error { return errNoMediaPC(c.PC, c.Gen, 0) }

func (noMediaPeer) Restart(r protocol.PCRestart) error { return errNoMediaPC(r.PC, r.Gen, 0) }

func (noMediaPeer) ClosePC(protocol.PCClose) error { return nil }

// Subscribe ignores every share it is asked for: without media no share exists.
func (noMediaPeer) Subscribe(wants []protocol.SubscriptionWant) ([]string, error) {
	ignored := make([]string, 0, len(wants))
	for _, w := range wants {
		ignored = append(ignored, w.ShareID)
	}
	return ignored, nil
}

func (noMediaPeer) SetCaps(protocol.Caps) {}

func (noMediaPeer) Resync() {}

func (noMediaPeer) Stats() protocol.ServerStats {
	return protocol.ServerStats{Subs: []protocol.ServerSubStats{}, Layers: []protocol.ServerLayerStats{}}
}

func (noMediaPeer) Close() {}
