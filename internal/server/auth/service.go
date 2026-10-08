package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// This file declares the whole Go API of 03 §7.13 (README S24, "interfaces first"): the Service with every M1 method
// (the four M2 device-flow methods come with their DTOs, see "later (M2)" below), Options, New, Principal, ActorOf,
// ConnSelector, ConnCloser, LoginResult, RegisterResult, UserChange and the smaller types.
//
// README S30 filled in New (this file; the key-rotation purge is in tokens.go), the sessions and the CSRF wrapper
// (session.go), login and logout (login.go) and setup (setup.go). README S42 filled in the invites (invite.go),
// registration and the approval queue (register.go), and added UpdateSettings (settings.go). The methods still
// declared below come with the later auth slices (03 §18 slices 9, 10 and 13: self-service, admin users and password
// resets, the janitor). Until then each one with an error result returns notImplemented(method): an error that names
// the method and wraps *api.Error{internal}, so an HTTP handler that calls it answers 500 internal (api has no
// not_implemented code).
//
// All service errors are *api.Error (03 §12.2) carrying a stable code, so httpapi maps them to statuses in one
// table: an unexpected failure (the store, a cancelled context) wraps both its cause and *api.Error{internal}
// (internalErr). The one exception is ErrNoCookie, which only the /ws adapter sees (03 §7.6); AuthenticateCookie
// returns *api.Error{unauthenticated} for an invalid, expired or non-active session.

// notImplemented is the error of a Service method whose slice has not landed yet.
func notImplemented(method string) error {
	return fmt.Errorf("auth: %s not implemented: %w", method, api.NewError(api.CodeInternal))
}

// internalErr wraps an unexpected failure of op. The result is an *api.Error{internal} for errors.As (03 §7.13) and
// keeps err for errors.Is and for the log line of httpapi.WriteError; the cause never reaches the client.
func internalErr(op string, err error) error {
	return fmt.Errorf("auth: %s: %w (%w)", op, err, api.NewError(api.CodeInternal))
}

// serviceErr returns err as a service error: an *api.Error stays as it is, anything else becomes internalErr.
func serviceErr(op string, err error) error {
	var ae *api.Error
	if errors.As(err, &ae) {
		return err
	}
	return internalErr(op, err)
}

// errUnauthenticated is the answer for a missing, invalid or expired credential and for a user who is not active.
func errUnauthenticated() error { return api.NewError(api.CodeUnauthenticated) }

// ---- construction ----

// Origins are the server's public origins (04's Site.Origin). They are fixed for the process lifetime: config changes
// need a restart (04 §4).
type Origins struct {
	// Primary builds the setup, invite, reset and (M2) link URLs, e.g. "https://watch.example.com". Its scheme also
	// picks the session cookie: __Host-isshoni_session with Secure for https, isshoni_session for plain http (dev).
	Primary string
	// Public are the origins trusted by the REST CSRF check: [Site.Origin] in M1, since 04's Host check admits no
	// other host. Dev needs nothing extra: task dev sets public_url to the Vite origin (06 §7.3). 01's WebSocket
	// allowlist uses the same Site.Origin.
	Public []string
}

// Options configures New. The wiring fills it (04 §6.6); auth never reads config.
type Options struct {
	// Keys are the token-hashing keys from secrets.json (04), 32 bytes each.
	Keys Keys
	// Origins returns 04's Site.Origin as Origins. It is fixed for the process lifetime (config changes need a
	// restart, 04 §4), so New calls it once; 01's WebSocket allowlist (Config.PublicOrigin) uses the same
	// Site.Origin. Required.
	Origins func() Origins
	// ClientIP is 04's trusted-proxy-aware httpapi.ClientIP. nil means the TCP peer of the request.
	ClientIP func(*http.Request) netip.Addr
	// Conns closes WebSocket connections on revocation (03 §7.7): the wiring's adapter over 01's
	// Hub.CloseConnections. nil is a no-op, only in unit tests and offline CLI commands (admin-socket commands run in
	// the server and use the real adapter).
	Conns ConnCloser
	// Alerts delivers admin alerts: 04's push service. nil means alerts are only logged.
	Alerts AdminAlerter
	// Argon are the argon2id parameters; the zero value means DefaultArgon.
	Argon ArgonParams
	// Hashes is the auth-hash bucket (03 §7.3); the zero value means DefaultHashBudget. Only tests change it.
	Hashes HashBudget
	// Clock is the service's clock; nil means time.Now.
	Clock func() time.Time
	// Logger is the server logger; the service adds component=auth. nil means slog.Default().
	Logger *slog.Logger
}

// Service is 03's account service: sessions, setup, login, registration, invites, self-service, admin actions and the
// janitor (03 §7). Build it with New. It is safe for concurrent use.
type Service struct {
	db       *store.DB
	keys     Keys
	origins  Origins // Primary is normalized (normalizeOrigin)
	clientIP func(*http.Request) netip.Addr
	conns    ConnCloser   // nil: no connections to close
	alerts   AdminAlerter // nil: alerts are only logged
	now      func() time.Time
	log      *slog.Logger

	hasher   *hasher
	throttle *throttles
	csrf     *CSRFGuard
	cookie   sessionCookie
	sessions *sessionCache

	// settingsMu serializes UpdateSettings, so that each call sees exactly its own change (settings.go).
	settingsMu sync.Mutex
}

// New builds the service (03 §7.13): it validates the keys and the origins, computes the dummy hash, then checks the
// key fingerprints and purges the rows a rotated key invalidated (§4.6: the purged rows, the secrets.rotated audit
// row and the secrets_rotated alert). It starts no goroutine; RunJanitor is the service's only long-running call.
//
// The wiring calls it once at startup, before serving (04 §6.6). Its errors are plain errors (a bad option, or the
// store), not *api.Error.
func New(ctx context.Context, db *store.DB, o Options) (*Service, error) {
	if db == nil {
		return nil, errors.New("auth: New: the store is nil")
	}
	if err := o.Keys.validate(); err != nil {
		return nil, err
	}
	if o.Origins == nil {
		return nil, errors.New("auth: New: Options.Origins is nil")
	}
	origins := o.Origins()
	primary, err := normalizeOrigin(origins.Primary)
	if err != nil {
		return nil, fmt.Errorf("auth: New: the primary origin is not usable: %w", err)
	}
	csrf, err := NewCSRFGuard(origins.Public)
	if err != nil {
		return nil, err
	}
	clock := o.Clock
	if clock == nil {
		clock = time.Now
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	clientIP := o.ClientIP
	if clientIP == nil {
		clientIP = peerIP
	}
	throttle, err := newThrottles(clock, o.Hashes)
	if err != nil {
		return nil, err
	}
	// The last of the option checks, and the slow one: it computes the dummy hash with the configured parameters.
	h, err := newHasher(o.Argon, 0, nil)
	if err != nil {
		return nil, err
	}
	s := &Service{
		db: db,
		// Copies, so that the caller may wipe its own key slices.
		keys:     Keys{Session: bytes.Clone(o.Keys.Session), Invite: bytes.Clone(o.Keys.Invite)},
		origins:  Origins{Primary: primary, Public: slices.Clone(origins.Public)},
		clientIP: clientIP,
		conns:    o.Conns,
		alerts:   o.Alerts,
		now:      clock,
		log:      log.With(slog.String("component", "auth")),
		hasher:   h,
		throttle: throttle,
		csrf:     csrf,
		cookie:   cookieFor(primary),
		sessions: newSessionCache(),
	}
	rotated, err := s.syncKeyFingerprints(ctx)
	if err != nil {
		return nil, err
	}
	if len(rotated) > 0 {
		// After the commit (03 §7.11). No connection exists yet, so there is none to close (03 §7.7).
		s.log.LogAttrs(ctx, slog.LevelWarn, "token keys changed since the last start; the tokens they protected are gone",
			slog.Any("keys", rotated))
		s.alert(ctx, AdminAlert{Kind: AlertSecretsRotated, Actor: store.SystemActor.Name, At: s.now()})
	}
	return s, nil
}

// peerIP is the ClientIP of a nil Options.ClientIP: the TCP peer, unmapped and without a zone; the zero Addr when
// RemoteAddr is not an IP address.
func peerIP(r *http.Request) netip.Addr {
	if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		return ap.Addr().Unmap().WithZone("")
	}
	return netip.Addr{}
}

// ipString is an IP in the form the store keeps (03 §3.3): netip.Addr.String() with IPv4-mapped addresses unmapped
// and no zone; "" for the zero Addr (an unknown peer).
func ipString(ip netip.Addr) string {
	if !ip.IsValid() {
		return ""
	}
	return ip.Unmap().WithZone("").String()
}

// ---- principals ----

// Method is how a Principal authenticated.
type Method uint8

const (
	MethodSession Method = 1 // web session cookie
	MethodBearer  Method = 2 // later (M2): device access token
)

// Principal is the authenticated caller of a REST request or WebSocket.
type Principal struct {
	UserID     store.UserID
	Username   string
	Role       store.Role
	Method     Method
	SessionID  store.SessionID // Method == MethodSession
	DeviceID   store.DeviceID  // Method == MethodBearer (M2)
	ClientKind string          // "web" for sessions; the device's kind ("desktop"|"mobile", 01 ClientKind) for bearer
}

// IsAdmin reports whether the principal has the admin role.
func (p Principal) IsAdmin() bool { return p.Role == store.RoleAdmin }

// ActorOf returns the audit actor for a principal acting from ip: a user actor with the username snapshot. The IP is
// stored in the form of 03 §3.3 (netip.Addr.String(), IPv4-mapped addresses unmapped, no zone); the zero Addr (an
// unknown peer) is stored as "".
func ActorOf(p Principal, ip netip.Addr) store.Actor {
	return store.Actor{Kind: store.ActorUser, UserID: p.UserID, Name: p.Username, IP: ipString(ip)}
}

// acting is the account behind a store.Actor, as actingAs reads it inside the transaction of the change.
type acting struct {
	// user is the acting account; the zero User for the CLI.
	user store.User
	// admin is true for the CLI and for an active admin account.
	admin bool
}

// id is the acting user's ID, "" for the CLI: the value of the created_by, revoked_by and approved_by columns.
func (a acting) id() store.UserID { return a.user.ID }

// actingAs resolves who a call acts for. The service's methods take a store.Actor, which names an account but not
// its role, and the admin socket calls them with store.CLIActor (03 §12.6), so each one reads the account in its own
// transaction:
//   - the CLI acts as an admin (the admin socket has checked its peer, 04 §12.1);
//   - a user actor is its account as stored now. An account that is gone or not active is *api.Error{forbidden}:
//     it was deleted or disabled after the request was authenticated;
//   - every other actor (system, anonymous) is forbidden.
//
// Reading the row in the transaction also keeps the foreign keys of created_by, revoked_by and approved_by from
// failing for an account that was deleted a moment ago.
func actingAs(q *store.Q, a store.Actor) (acting, error) {
	switch a.Kind {
	case store.ActorCLI:
		return acting{admin: true}, nil
	case store.ActorUser:
		u, err := q.UserByID(a.UserID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return acting{}, api.NewError(api.CodeForbidden)
		case err != nil:
			return acting{}, err
		case u.Status != store.StatusActive:
			return acting{}, api.NewError(api.CodeForbidden)
		}
		return acting{user: u, admin: u.Role == store.RoleAdmin}, nil
	}
	return acting{}, api.NewError(api.CodeForbidden)
}

// actingAdmin is actingAs for the admin-only calls: anyone but the CLI and an active admin is
// *api.Error{forbidden}. The REST routes of these calls are Admin routes already (03 §12.1); this is the same rule
// at the moment of the change.
func actingAdmin(q *store.Q, a store.Actor) (acting, error) {
	who, err := actingAs(q, a)
	if err != nil {
		return acting{}, err
	}
	if !who.admin {
		return acting{}, api.NewError(api.CodeForbidden)
	}
	return who, nil
}

// ---- revocation (03 §7.7) ----

// ConnSelector selects WebSocket connections by user, session or device. The wiring implements ConnCloser over 01's
// Hub.CloseConnections (04 §6.6).
type ConnSelector struct {
	UserID          store.UserID    // required, never empty (01 closes nothing otherwise)
	SessionID       store.SessionID // "" = any
	DeviceID        store.DeviceID  // "" = any
	ExceptSessionID store.SessionID // keep this one (password change, "sign out other browsers")
}

// ConnCloser closes the WebSocket connections a selector matches, with a Reason* code, and returns how many it
// closed.
type ConnCloser interface {
	CloseConnections(sel ConnSelector, reason string) int
}

// Reason codes passed to ConnCloser.CloseConnections (03 §7.7). The wiring maps account_disabled to 01's
// account_disabled and every other reason to session_revoked (01 §15.4).
const (
	ReasonLoggedOut       = "logged_out"
	ReasonSessionRevoked  = "session_revoked"
	ReasonPasswordChanged = "password_changed"
	ReasonPasswordReset   = "password_reset"
	ReasonAccountDisabled = "account_disabled"
	ReasonAccountDeleted  = "account_deleted"
	ReasonDeviceRevoked   = "device_revoked" // later (M2)
)

// ---- admin alerts (03 §7.11) ----

// AdminAlert is a security event sent to every admin whose adminAlerts preference is on (03 §7.11). 04's push
// service delivers it as {type: "admin.alert", kind, actor, target}; 05 renders push.adminAlert.<kind>.
type AdminAlert struct {
	Kind   string // one of the Alert* kinds
	Actor  string // username, "cli" or "system"
	Target string // username, or "" when there is no target
	At     time.Time
}

// AdminAlerter is implemented by the Web Push service (04). auth calls it after the commit.
type AdminAlerter interface {
	AdminAlert(ctx context.Context, a AdminAlert)
}

// Admin alert kinds (03 §7.11). They are wire values (push payloads and i18n keys), never renamed.
const (
	AlertAdminGranted            = "admin_granted"
	AlertAdminRevoked            = "admin_revoked"
	AlertAdminPasswordReset      = "admin_password_reset"
	AlertRegistrationModeChanged = "registration_mode_changed"
	AlertSignupPending           = "signup_pending"
	AlertSecretsRotated          = "secrets_rotated"
	AlertTransferThreshold       = "transfer_threshold" // raised by 04 §11.3; Target "80" or "100"
	AlertRefreshTokenReused      = "refresh_token_reused"
)

// ---- request metadata, links and results ----

// ReqMeta is what the service needs to know about the HTTP request behind a call. RequestMeta builds it from a
// request.
//
// It carries a live credential, so it never prints it (README §4: tokens and cookies are never logged): every fmt
// verb and log/slog show "[redacted]" in place of a session token, and encoding/json leaves the field out.
type ReqMeta struct {
	IP        netip.Addr // from 04's ClientIP (trusted-proxy aware)
	UserAgent string     // only turned into a session name (DescribeUserAgent)
	// SessionToken is the value of the session cookie the request arrived with, "" for none. A login, registration,
	// setup or reset completion deletes that old session in the transaction that creates the new one, and then closes
	// the old session's connections with ReasonSessionRevoked (03 §7.4, §7.7). It is an addition to the two fields of
	// 03 §7.13: those calls take no request, so the cookie has to travel with them.
	SessionToken string `json:"-"`
}

// redactedToken stands for a token wherever a ReqMeta is printed or logged, as in the api DTOs (an empty token
// stays empty).
const redactedToken = "[redacted]"

// redact returns m without its session token.
func (m ReqMeta) redact() ReqMeta {
	if m.SessionToken != "" {
		m.SessionToken = redactedToken
	}
	return m
}

// Format implements fmt.Formatter so that no verb (%v, %+v, %#v, %s …) prints the session token: directly, through
// a pointer, or as a field of another printed value.
func (m ReqMeta) Format(f fmt.State, verb rune) {
	type plain ReqMeta // without methods, so that printing it cannot come back here
	p := plain(m.redact())
	if verb == 'v' && f.Flag('#') {
		// %#v names the type: put ReqMeta's name back in place of the local one.
		_, fields, _ := strings.Cut(fmt.Sprintf("%#v", p), "{")
		_, _ = fmt.Fprintf(f, "%T{%s", m, fields)
		return
	}
	_, _ = fmt.Fprintf(f, fmt.FormatString(f, verb), p)
}

// LogValue implements slog.LogValuer: a ReqMeta logged as an attribute is a group of its fields, with the session
// token redacted. (As a field of a logged struct it goes through Format in the text handler and through
// encoding/json in the JSON handler.)
func (m ReqMeta) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("remote_ip", ipString(m.IP)),
		slog.String("user_agent", m.UserAgent),
		slog.String("session_token", m.redact().SessionToken),
	)
}

// RequestMeta returns the ReqMeta of r: the client IP (Options.ClientIP), the User-Agent header and the value of the
// session cookie, if the request has one.
func (s *Service) RequestMeta(r *http.Request) ReqMeta {
	m := ReqMeta{IP: s.clientIP(r), UserAgent: r.UserAgent()}
	if tok, ok := s.cookie.read(r); ok {
		m.SessionToken = tok
	}
	return m
}

// Link is a one-time URL with a token in its fragment: /setup#t, /invite#t, /reset#t.
type Link struct {
	URL       string
	ExpiresAt time.Time
}

// LoginResult is a new web session: login, registration, setup, reset and password change return one.
type LoginResult struct {
	User    store.User
	Session store.Session
	Token   string // raw cookie value; httpapi sets the cookie with SetSessionCookie
}

// ---- HTTP integration (03 §7.4–7.6) ----
//
// Authenticate, AuthenticateCookie, Touch, MaybeRotate, SetSessionCookie, ClearSessionCookie and CSRF are in
// session.go.

// ---- setup (03 §7.8) ----
//
// SetupAvailable, IssueSetupToken, CheckSetupToken and CompleteSetup are in setup.go.

// SetupInput is the body of POST /api/v1/auth/setup/complete.
type SetupInput struct{ Token, Username, Password, ServerName string }

// ---- login, registration (03 §7.4, §7.9) ----
//
// Login and Logout are in login.go, CheckInvite is in invite.go and Register in register.go.

// LogoutEverywhere deletes every session and device of the principal's user.
func (s *Service) LogoutEverywhere(ctx context.Context, p Principal, m ReqMeta) error {
	return notImplemented("LogoutEverywhere")
}

// RegisterInput is the body of POST /api/v1/auth/register. An empty InviteToken is a public sign-up, which only the
// approval mode takes.
type RegisterInput struct{ InviteToken, Username, Password string }

// RegisterResult is the outcome of Register: an active account with its session, or a pending sign-up.
type RegisterResult struct {
	Pending bool
	Login   *LoginResult // nil when Pending
}

// InviteInfo answers POST /api/v1/auth/invite/check: what the invite page shows before its form. InvitedBy is the
// creator's username, "" for an invite from the CLI or one whose creator was deleted.
type InviteInfo struct {
	ServerName, InvitedBy string
	ExpiresAt             time.Time
	UsesLeft              int
}

// ---- self-service (03 §7.7) ----

// ChangePassword checks the current password, sets the new one, revokes the user's other sessions and devices and
// rotates this session's token.
func (s *Service) ChangePassword(ctx context.Context, p Principal, current, next string, m ReqMeta) (LoginResult, error) {
	return LoginResult{}, notImplemented("ChangePassword")
}

// DeleteSelf deletes the principal's account after checking its password.
func (s *Service) DeleteSelf(ctx context.Context, p Principal, password string, m ReqMeta) error {
	return notImplemented("DeleteSelf")
}

// RevokeSession deletes one of the principal's own sessions.
func (s *Service) RevokeSession(ctx context.Context, p Principal, id store.SessionID, m ReqMeta) error {
	return notImplemented("RevokeSession")
}

// RevokeOtherSessions deletes every session of the principal's user except the current one and returns how many.
func (s *Service) RevokeOtherSessions(ctx context.Context, p Principal, m ReqMeta) (int, error) {
	return 0, notImplemented("RevokeOtherSessions")
}

// RevokeDevice deletes one of the principal's own devices (none exist before M2).
func (s *Service) RevokeDevice(ctx context.Context, p Principal, id store.DeviceID, m ReqMeta) error {
	return notImplemented("RevokeDevice")
}

// ---- invites (03 §7.9) ----
//
// CreateInvite and RevokeInvite are in invite.go.

// InviteInput creates an invite (the body of POST /api/v1/invites, and the flags of `isshoni admin invite create`).
type InviteInput struct {
	Note           string // free text, at most 64 characters
	ExpiresInHours int    // 1–720; 0 = the setting inviteDefaultTtlHours
	MaxUses        int    // 1–1000; 0 = the setting inviteDefaultMaxUses
}

// ---- admin (03 §7.9–7.11; also used by the admin socket with store.CLIActor) ----
//
// Approve, Reject and RejectAll (the approval queue, 03 §7.9) are in register.go. UpdateSettings (settings.go) is
// the settings patch of 03 §9 together with the registration_mode_changed alert of 03 §7.11.

// UpdateUser renames a user or changes its role or status, with the last-admin and self rules.
func (s *Service) UpdateUser(ctx context.Context, a store.Actor, id store.UserID, ch UserChange) (store.User, error) {
	return store.User{}, notImplemented("UpdateUser")
}

// DeleteUser deletes a user, with the last-admin and self rules.
func (s *Service) DeleteUser(ctx context.Context, a store.Actor, id store.UserID) error {
	return notImplemented("DeleteUser")
}

// SignOutUser deletes every session and device of a user and returns how many of each.
func (s *Service) SignOutUser(ctx context.Context, a store.Actor, id store.UserID) (sessions, devices int, err error) {
	return 0, 0, notImplemented("SignOutUser")
}

// IssuePasswordReset returns a /reset#token link for a user (03 §7.10). actorPassword re-authenticates an admin
// actor; the CLI passes "".
func (s *Service) IssuePasswordReset(ctx context.Context, a store.Actor, id store.UserID, actorPassword string) (Link, error) {
	return Link{}, notImplemented("IssuePasswordReset")
}

// CheckPasswordReset answers POST /api/v1/auth/reset/check with the username the link resets.
func (s *Service) CheckPasswordReset(ctx context.Context, token string, m ReqMeta) (username string, err error) {
	return "", notImplemented("CheckPasswordReset")
}

// CompletePasswordReset sets the new password, revokes the user's sessions and devices and logs the user in.
func (s *Service) CompletePasswordReset(ctx context.Context, token, password string, m ReqMeta) (LoginResult, error) {
	return LoginResult{}, notImplemented("CompletePasswordReset")
}

// UserByUsername looks a user up by any spelling of the username (CLI lookups).
func (s *Service) UserByUsername(ctx context.Context, username string) (store.User, error) {
	return store.User{}, notImplemented("UserByUsername")
}

// UserChange is a patch for UpdateUser: nil fields stay as they are.
type UserChange struct {
	Username      *string
	Role          *store.Role
	Status        *store.UserStatus // "active" | "disabled" only
	ActorPassword string            // required when granting admin (not for the CLI)
}

// ---- maintenance (03 §4.7) ----

// RunJanitor prunes expired rows on its schedule and blocks until ctx is done. Not implemented yet: it prunes
// nothing, and only waits for ctx.
func (s *Service) RunJanitor(ctx context.Context) {
	<-ctx.Done()
}

// ---- later (M2) ----
//
// 03 §7.13 also lists StartDeviceFlow, DeviceToken, LookupUserCode and PasswordDeviceLogin. They take and return the
// device-flow DTOs of 03 §12.4.7 (api.DeviceCodeRequest, DeviceCodeResponse, DeviceTokenRequest,
// DeviceTokenResponse, DeviceLookup, DevicePasswordRequest), which internal/protocol/api does not declare yet
// (03 §12.5 lists them as later). The four methods come together with those DTOs in the device-flow slice
// (03 §18 slice 15, M2); no M1 handler, adapter or fake calls them.

// AuthenticateBearerToken authenticates a device access token from 01's hello.auth. Later (M2).
func (s *Service) AuthenticateBearerToken(ctx context.Context, token string) (Principal, error) {
	return Principal{}, notImplemented("AuthenticateBearerToken")
}

// DecideUserCode approves or denies a device-flow user code. Later (M2).
func (s *Service) DecideUserCode(ctx context.Context, p Principal, userCode string, approve bool, m ReqMeta) error {
	return notImplemented("DecideUserCode")
}
