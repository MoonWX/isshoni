package auth

import (
	"context"
	"net/netip"
	"time"
)

// This file declares the parts of 03 §7.13's Go API that do not depend on the store or the api package. The
// Service itself, Options, New, Principal, ActorOf, ConnSelector, ConnCloser, LoginResult, RegisterResult,
// UserChange and every Service method come with the store (README S30).

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

// Method is how a Principal authenticated.
type Method uint8

const (
	MethodSession Method = 1 // web session cookie
	MethodBearer  Method = 2 // later (M2): device access token
)

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

// SetupInput is the body of POST /api/v1/auth/setup/complete.
type SetupInput struct{ Token, Username, Password, ServerName string }

// RegisterInput is the body of POST /api/v1/auth/register.
type RegisterInput struct{ InviteToken, Username, Password string }

// InviteInfo answers POST /api/v1/auth/invite/check.
type InviteInfo struct {
	ServerName, InvitedBy string
	ExpiresAt             time.Time
	UsesLeft              int
}

// InviteInput creates an invite.
type InviteInput struct {
	Note           string
	ExpiresInHours int // 0 = setting default
	MaxUses        int // 0 = setting default
}
