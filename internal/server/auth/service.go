package auth

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// This file declares the whole Go API of 03 §7.13 (README S24, "interfaces first"): the Service with every method,
// Options, New, Principal, ActorOf, ConnSelector, ConnCloser, LoginResult, RegisterResult, UserChange and the
// smaller types. The bodies come later: sessions, login and setup with README S30, then invites and registration,
// self-service, admin users and the janitor with the later auth slices (03 §18 slices 6–13). Until then every method
// with an error result returns notImplemented(method): an error that names the method and wraps
// *api.Error{internal}, so an HTTP handler that calls it answers 500 internal (api has no not_implemented code).
// The pure functions (Principal.IsAdmin, ActorOf, IPKey, NormalizeUsername, CheckPassword, DescribeUserAgent) work.
//
// All service errors are *api.Error (03 §12.2) carrying a stable code, so httpapi maps them to statuses in one
// table. The one exception is ErrNoCookie, which only the /ws adapter sees (03 §7.6); AuthenticateCookie returns
// *api.Error{unauthenticated} for an invalid, expired or non-active session and passes any other error through.

// notImplemented is the error of a Service method whose slice has not landed yet.
func notImplemented(method string) error {
	return fmt.Errorf("auth: %s not implemented: %w", method, api.NewError(api.CodeInternal))
}

// ---- construction ----

// Origins are the server's public origins (04's Site.Origin). They are fixed for the process lifetime: config changes
// need a restart (04 §4).
type Origins struct {
	// Primary builds the setup, invite, reset and (M2) link URLs, e.g. "https://watch.example.com".
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
	// restart, 04 §4); 01's WebSocket allowlist (Config.PublicOrigin) uses the same Site.Origin.
	Origins func() Origins
	// ClientIP is 04's trusted-proxy-aware httpapi.ClientIP.
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
	// Logger is the server logger; nil means slog.Default().
	Logger *slog.Logger
}

// Service is 03's account service: sessions, setup, login, registration, invites, self-service, admin actions and the
// janitor (03 §7). Build it with New. It is safe for concurrent use.
type Service struct {
	// The state (store, keys, hasher, throttles, CSRF guard, session cache) comes with README S30.
}

// New builds the service (03 §7.13): it validates the keys, checks their fingerprints and purges the rows a rotated
// key invalidated (§4.6: the deleted rows, the secrets.rotated audit row and the secrets_rotated alert), and computes
// the dummy hash. It starts no goroutine; RunJanitor is the service's only long-running call.
func New(ctx context.Context, db *store.DB, o Options) (*Service, error) {
	return nil, notImplemented("New")
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
	a := store.Actor{Kind: store.ActorUser, UserID: p.UserID, Name: p.Username}
	if ip.IsValid() {
		a.IP = ip.Unmap().WithZone("").String()
	}
	return a
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

// ReqMeta is what the service needs to know about the HTTP request behind a call.
type ReqMeta struct {
	IP        netip.Addr // from 04's ClientIP (trusted-proxy aware)
	UserAgent string     // only turned into a session name (DescribeUserAgent)
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

// Authenticate returns the caller of a REST request: the session cookie in M1 (bearer tokens in M2). A missing,
// invalid or expired credential, or a user who is not active, is *api.Error{unauthenticated}; in M1 so is any
// Authorization header (03 §7.5).
func (s *Service) Authenticate(r *http.Request) (Principal, error) {
	return Principal{}, notImplemented("Authenticate")
}

// AuthenticateCookie is Authenticate for /ws only (03 §7.6): the session cookie, never rotated, ignoring
// Authorization. A request without the cookie returns ErrNoCookie; an invalid, expired or non-active session returns
// *api.Error{unauthenticated}; any other error is passed through.
func (s *Service) AuthenticateCookie(r *http.Request) (Principal, error) {
	return Principal{}, notImplemented("AuthenticateCookie")
}

// Touch is the hub's Revalidate (03 §7.4, §7.6): it checks on every call that the session (or, in M2, the device) and
// the user are still valid, and records the use at most once per 5 min, best effort. Only *api.Error{unauthenticated}
// means the credential is gone.
func (s *Service) Touch(ctx context.Context, p Principal, ip netip.Addr) error {
	return notImplemented("Touch")
}

// MaybeRotate is the REST chain's rotation step (03 §7.4): once a session's token is 24 h old, it issues a new token
// and sets the cookie. Not implemented yet: it rotates nothing.
func (s *Service) MaybeRotate(w http.ResponseWriter, p Principal) {}

// SetSessionCookie sets the session cookie (03 §7.4): __Host-isshoni_session for an https origin, isshoni_session
// without Secure for a plain-http dev origin; Max-Age is the time left until idleExpires. Not implemented yet: it
// sets nothing.
func (s *Service) SetSessionCookie(w http.ResponseWriter, token string, idleExpires time.Time) {}

// ClearSessionCookie expires the session cookie. Not implemented yet: it sets nothing.
func (s *Service) ClearSessionCookie(w http.ResponseWriter) {}

// CSRF wraps next in the REST cross-origin and Content-Type check (03 §7.5), a CSRFGuard built from
// Options.Origins().Public. Not implemented yet: the returned handler fails closed and answers every request with
// 500 {"error":{"code":"internal"}}, without calling next.
func (s *Service) CSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeErrorEnvelope(w, http.StatusInternalServerError, api.CodeInternal)
	})
}

// ---- setup (03 §7.8) ----

// SetupAvailable is true while no admin row exists, whatever its status.
func (s *Service) SetupAvailable(ctx context.Context) (bool, error) {
	return false, notImplemented("SetupAvailable")
}

// IssueSetupToken replaces every setup token with a new one valid for 24 h and returns its /setup#token link; with an
// admin present it returns *api.Error{setup_unavailable}. 04's `isshoni setup-url` calls it with store.CLIActor.
func (s *Service) IssueSetupToken(ctx context.Context, a store.Actor) (Link, error) {
	return Link{}, notImplemented("IssueSetupToken")
}

// CheckSetupToken answers POST /api/v1/auth/setup/check: nil for a live token.
func (s *Service) CheckSetupToken(ctx context.Context, token string, m ReqMeta) error {
	return notImplemented("CheckSetupToken")
}

// CompleteSetup creates the first admin and its session (POST /api/v1/auth/setup/complete).
func (s *Service) CompleteSetup(ctx context.Context, in SetupInput, m ReqMeta) (LoginResult, error) {
	return LoginResult{}, notImplemented("CompleteSetup")
}

// SetupInput is the body of POST /api/v1/auth/setup/complete.
type SetupInput struct{ Token, Username, Password, ServerName string }

// ---- login, registration (03 §7.4, §7.9) ----

// Login checks a username and password and creates a session.
func (s *Service) Login(ctx context.Context, username, password string, m ReqMeta) (LoginResult, error) {
	return LoginResult{}, notImplemented("Login")
}

// Logout deletes the principal's session (or, in M2, device) and closes its WebSockets with ReasonLoggedOut.
func (s *Service) Logout(ctx context.Context, p Principal, m ReqMeta) error {
	return notImplemented("Logout")
}

// LogoutEverywhere deletes every session and device of the principal's user.
func (s *Service) LogoutEverywhere(ctx context.Context, p Principal, m ReqMeta) error {
	return notImplemented("LogoutEverywhere")
}

// CheckInvite answers POST /api/v1/auth/invite/check.
func (s *Service) CheckInvite(ctx context.Context, token string, m ReqMeta) (InviteInfo, error) {
	return InviteInfo{}, notImplemented("CheckInvite")
}

// Register creates an account in the current registration mode: active with a session, or pending approval.
func (s *Service) Register(ctx context.Context, in RegisterInput, m ReqMeta) (RegisterResult, error) {
	return RegisterResult{}, notImplemented("Register")
}

// RegisterInput is the body of POST /api/v1/auth/register.
type RegisterInput struct{ InviteToken, Username, Password string }

// RegisterResult is the outcome of Register: an active account with its session, or a pending sign-up.
type RegisterResult struct {
	Pending bool
	Login   *LoginResult // nil when Pending
}

// InviteInfo answers POST /api/v1/auth/invite/check.
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

// CreateInvite creates an invite and returns it with its /invite#token link.
func (s *Service) CreateInvite(ctx context.Context, a store.Actor, in InviteInput) (store.Invite, Link, error) {
	return store.Invite{}, Link{}, notImplemented("CreateInvite")
}

// RevokeInvite revokes an invite.
func (s *Service) RevokeInvite(ctx context.Context, a store.Actor, id store.InviteID) error {
	return notImplemented("RevokeInvite")
}

// InviteInput creates an invite.
type InviteInput struct {
	Note           string
	ExpiresInHours int // 0 = setting default
	MaxUses        int // 0 = setting default
}

// ---- admin (03 §7.9–7.11; also used by the admin socket with store.CLIActor) ----

// Approve activates a pending sign-up.
func (s *Service) Approve(ctx context.Context, a store.Actor, id store.UserID) (store.User, error) {
	return store.User{}, notImplemented("Approve")
}

// Reject deletes a pending sign-up.
func (s *Service) Reject(ctx context.Context, a store.Actor, id store.UserID) error {
	return notImplemented("Reject")
}

// RejectAll deletes every pending sign-up and returns how many (03 §7.9); one audit row carries the count.
func (s *Service) RejectAll(ctx context.Context, a store.Actor) (int, error) {
	return 0, notImplemented("RejectAll")
}

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
// 03 §7.13 also lists StartDeviceFlow, DeviceToken, LookupUserCode and PasswordDeviceLogin. Their parameter and
// result types (api.DeviceCodeRequest, DeviceCodeResponse, DeviceTokenRequest, DeviceTokenResponse, DeviceLookup,
// DevicePasswordRequest) are M2 DTOs that internal/protocol/api does not declare yet (03 §12.5), so the methods come
// with them.

// AuthenticateBearerToken authenticates a device access token from 01's hello.auth. Later (M2).
func (s *Service) AuthenticateBearerToken(ctx context.Context, token string) (Principal, error) {
	return Principal{}, notImplemented("AuthenticateBearerToken")
}

// DecideUserCode approves or denies a device-flow user code. Later (M2).
func (s *Service) DecideUserCode(ctx context.Context, p Principal, userCode string, approve bool, m ReqMeta) error {
	return notImplemented("DecideUserCode")
}
