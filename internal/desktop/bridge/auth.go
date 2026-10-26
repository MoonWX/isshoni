package bridge

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/MoonWX/isshoni/internal/client/compat"
	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/desktop/bridge/codes"
	"github.com/MoonWX/isshoni/internal/desktop/servers"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// Auth is the bound Auth service (03 §6.2, 02 §11.7): the server list, the probe, linking, and the access token of
// the active server. Every token-scoped method names its origin and is answered only for the active server's own
// origin (03 §5.8): AccessToken returns null for any other, SessionEnded and HelloRefused reject with not_found, and
// none of them touches a manager's token for it.
//
// Adding, switching and removing servers and Sign out change the active server and reload the window, which is the
// app's (README WA71): they are not_implemented until then.
type Auth struct {
	core    Core
	servers ServerList
	probe   ProbeFunc
	log     *slog.Logger
}

// AuthState is the active server's link state (02 §11.7): Auth.State() and the auth.state event. status is unlinked,
// linking, linked, offline or incompatible; notice is "", denied, code_expired, signed_out, refresh_reused,
// account_disabled or save_failed.
type AuthState struct {
	Origin     string     `json:"origin"`
	ServerName string     `json:"serverName"`
	Status     string     `json:"status"`
	Link       *LinkCode  `json:"link"`   // status linking
	User       *User      `json:"user"`   // linked, offline
	Device     *Device    `json:"device"` // linked, offline
	Notice     string     `json:"notice"`
	Compat     CompatInfo `json:"compat"`
}

// LinkCode is the code a linking app shows; expiresAt in ms since the epoch.
type LinkCode struct {
	UserCode                string `json:"userCode"`
	VerificationURI         string `json:"verificationUri"`
	VerificationURIComplete string `json:"verificationUriComplete"`
	ExpiresAt               int64  `json:"expiresAt"`
}

// User is the linked account: api.User {id, username, role}.
type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

// Device is this PC as the server knows it: api.DeviceRef {id, name}.
type Device struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// CompatInfo is 02 §13.2's compat.Check: verdict ok, update_app or update_server; reason "", protocol_too_new,
// protocol_too_old, no_device_flow or min_client_version.
type CompatInfo struct {
	Verdict          string `json:"verdict"`
	Reason           string `json:"reason"`
	ServerVersion    string `json:"serverVersion"`
	MinClientVersion string `json:"minClientVersion"`
}

// ServerInfo is one entry of Auth.Servers(): 02's ServerEntry plus the derived local id (03 §8.1), the active mark
// and linked (= user != null). addedAt is RFC 3339.
type ServerInfo struct {
	Origin  string  `json:"origin"`
	Name    string  `json:"name"`
	AddedAt string  `json:"addedAt"`
	User    *User   `json:"user"`
	Device  *Device `json:"device"`
	ID      string  `json:"id"`
	Active  bool    `json:"active"`
	Linked  bool    `json:"linked"`
}

// ServerProbe is what Auth.Probe found: the server's own origin (which Add stores), name and version.
type ServerProbe = servers.ServerProbe

// AccessToken is Auth.AccessToken's answer: the active server's access token (isa_…) with at least 120 s left, and
// its expiry in ms since the epoch. It lives in the page's memory only (03 §5.8).
type AccessToken struct {
	Token     protocol.Secret `json:"token"`
	ExpiresAt int64           `json:"expiresAt"`
}

// HelloRefusal is C1's refused hello as Auth.HelloRefused takes it: code client_outdated or protocol_unsupported,
// with the error's params.
type HelloRefusal struct {
	Code   string         `json:"code"`
	Params map[string]any `json:"params,omitempty"`
}

// UnlinkResult is Auth.Unlink's answer: confirmed false when the server wasn't reached (02 §11.7).
type UnlinkResult struct {
	Confirmed bool `json:"confirmed"`
}

// The limits of Auth's string arguments (02 §14's fields are far smaller; these only refuse junk before a request).
const (
	maxAddress  = 2048
	maxOrigin   = 512
	maxUsername = 256
	maxPassword = 1024
)

// State returns the active server's link state; with no server, an empty unlinked state.
func (s *Auth) State() AuthState {
	m := s.servers.Active()
	if m == nil {
		return authState(deviceauth.State{Status: deviceauth.StatusUnlinked})
	}
	return authState(m.State())
}

// Servers lists the servers of servers.json in order.
func (s *Auth) Servers() []ServerInfo {
	active := ""
	if m := s.servers.Active(); m != nil {
		active = m.Origin()
	}
	list := s.servers.List()
	out := make([]ServerInfo, 0, len(list))
	for _, e := range list {
		info := ServerInfo{
			Origin: e.Origin,
			Name:   e.Name,
			User:   userOf(e.User),
			Device: deviceOf(e.Device),
			ID:     servers.LocalID(e.Origin),
			Active: e.Origin == active,
			Linked: e.User != nil,
		}
		if !e.AddedAt.IsZero() {
			info.AddedAt = e.AddedAt.UTC().Format(time.RFC3339)
		}
		out = append(out, info)
	}
	return out
}

// Probe asks the server at address for its /info (03 §8.1) and never adds it: invalid_address, server_unreachable,
// tls_untrusted, not_isshoni, server_not_set_up, incompatible_server (params: the compat check).
func (s *Auth) Probe(ctx context.Context, address string) (ServerProbe, error) {
	if address == "" || len(address) > maxAddress {
		return ServerProbe{}, newError(codes.CodeInvalidAddress, nil)
	}
	ctx, cancel := withTimeout(ctx, probeTimeout)
	defer cancel()
	p, err := s.probe(ctx, address)
	if err != nil {
		return ServerProbe{}, s.authError(err, nil)
	}
	return p, nil
}

// AddServer adds the server and makes it active, then reloads the window at /desktop/setup?link=1 (03 §8.2):
// README WA71.
func (s *Auth) AddServer(ctx context.Context, origin string) error {
	return notImplemented("Auth.AddServer")
}

// SwitchServer makes a listed server active and reloads the window (03 §8.1): README WA71.
func (s *Auth) SwitchServer(origin string) error {
	return notImplemented("Auth.SwitchServer")
}

// RemoveServer unlinks a server and drops it from the list (02's Servers.Remove): README WA71.
func (s *Auth) RemoveServer(ctx context.Context, origin string) error {
	return notImplemented("Auth.RemoveServer")
}

// Link starts a device-flow link on the active server (02 §11.3): it returns once the code is shown, and progress
// arrives as auth.state. server_unreachable, incompatible_server, rate_limited; not_found without a server.
func (s *Auth) Link(ctx context.Context) error {
	m := s.servers.Active()
	if m == nil {
		return newError(codes.CodeNotFound, nil)
	}
	ctx, cancel := withTimeout(ctx, linkTimeout)
	defer cancel()
	return s.authError(m.Link(ctx), m)
}

// CancelLink ends a running device-flow link on the active server; without one it does nothing.
func (s *Auth) CancelLink() {
	if m := s.servers.Active(); m != nil {
		m.CancelLink()
	}
}

// LinkWithPassword links the active server with a username and password (02 §14, 03 §8.2's form) and returns the
// new state. The password crosses the binding once, as a protocol.Secret, and is never stored, retried or logged.
// Errors: invalid_argument, the server's own codes passed through (invalid_credentials, account_pending,
// account_disabled, rate_limited with params.retryAfter, server_busy, validation_failed with params.fields),
// server_unreachable, incompatible_server; not_found without a server.
func (s *Auth) LinkWithPassword(ctx context.Context, username string, password protocol.Secret) (AuthState, error) {
	switch {
	case len(username) > maxUsername || !cleanText(username):
		return AuthState{}, invalidArgument("username")
	case len(password) > maxPassword || !cleanText(password.Reveal()):
		return AuthState{}, invalidArgument("password")
	}
	m := s.servers.Active()
	if m == nil {
		return AuthState{}, newError(codes.CodeNotFound, nil)
	}
	ctx, cancel := withTimeout(ctx, ioTimeout)
	defer cancel()
	if err := m.LinkWithPassword(ctx, username, password); err != nil {
		return AuthState{}, s.authError(err, m)
	}
	return authState(m.State()), nil
}

// Unlink signs this PC out of the active server (03 §8.3), stopping a live share on it first: README WA71.
func (s *Auth) Unlink(ctx context.Context) (UnlinkResult, error) {
	return UnlinkResult{}, notImplemented("Auth.Unlink")
}

// Recheck re-runs the active server's compatibility check (the version screen's Try again, 02 §13.1).
func (s *Auth) Recheck(ctx context.Context) (AuthState, error) {
	m := s.servers.Active()
	if m == nil {
		return AuthState{}, newError(codes.CodeNotFound, nil)
	}
	ctx, cancel := withTimeout(ctx, ioTimeout)
	defer cancel()
	st, err := m.Recheck(ctx)
	if err != nil {
		return AuthState{}, s.authError(err, m)
	}
	return authState(st), nil
}

// AccessToken returns the active server's access token, refreshing first when needed, but only when origin is that
// server's own origin (03 §5.8): for any other origin, with no active server, or when it isn't linked, null. The
// active manager is read once, so the token is always origin's own even when the active server changes during the
// call. force (after the server refused the page's token) is 02's forced refresh, at most one per 10 s.
// server_unreachable (02's ErrOffline), incompatible_server.
func (s *Auth) AccessToken(ctx context.Context, origin string, force bool) (*AccessToken, error) {
	m := s.servers.Active()
	if m == nil || origin == "" || m.Origin() != origin {
		s.log.Debug("desktop: token request for a server that isn't active")
		return nil, nil
	}
	ctx, cancel := withTimeout(ctx, accessTokenTimeout)
	defer cancel()
	tok, exp, err := m.AccessTokenWithExpiry(ctx, force)
	switch {
	case errors.Is(err, deviceauth.ErrNotLinked):
		return nil, nil
	case err != nil:
		return nil, s.authError(err, m)
	}
	return &AccessToken{Token: tok, ExpiresAt: exp.UnixMilli()}, nil
}

// SessionEnded tells the active server's manager that C1 was stopped with code (unauthenticated, session_revoked or
// account_disabled; 03 §7.3, §8.3) and returns the resulting state: the SPA redials C1 when it is linked and shows
// relink otherwise. invalid_argument for another code; not_found when origin isn't the active server's.
func (s *Auth) SessionEnded(ctx context.Context, origin, code string) (AuthState, error) {
	switch protocol.ErrorCode(code) {
	case protocol.ErrorCodeUnauthenticated, protocol.ErrorCodeSessionRevoked, protocol.ErrorCodeAccountDisabled:
	default:
		return AuthState{}, invalidArgument("code")
	}
	m := s.activeFor(origin)
	if m == nil {
		return AuthState{}, newError(codes.CodeNotFound, nil)
	}
	ctx, cancel := withTimeout(ctx, ioTimeout)
	defer cancel()
	return authState(m.SessionEnded(ctx, protocol.ErrorCode(code))), nil
}

// HelloRefused tells the active server's manager that C1's hello was refused with client_outdated or
// protocol_unsupported (02 §13.2) and returns the state the version screen follows (03 §16.3). invalid_argument for
// another code; not_found when origin isn't the active server's.
func (s *Auth) HelloRefused(origin string, refusal HelloRefusal) (AuthState, error) {
	switch protocol.ErrorCode(refusal.Code) {
	case protocol.ErrorCodeClientOutdated, protocol.ErrorCodeProtocolUnsupported:
	default:
		return AuthState{}, invalidArgument("code")
	}
	m := s.activeFor(origin)
	if m == nil {
		return AuthState{}, newError(codes.CodeNotFound, nil)
	}
	m.HelloRefused(&protocol.Error{Code: protocol.ErrorCode(refusal.Code), Scope: protocol.ErrorScopeConnection,
		Params: refusal.Params})
	return authState(m.State()), nil
}

// activeFor returns the active manager when origin is its origin, else nil with the debug line of 03 §5.8.
func (s *Auth) activeFor(origin string) Manager {
	m := s.servers.Active()
	if m == nil || origin == "" || len(origin) > maxOrigin || m.Origin() != origin {
		s.log.Debug("desktop: a session call for a server that isn't active")
		return nil
	}
	return m
}

// authError maps an error of 02's manager or the probe; incompatible_server gets the manager's compat check as its
// params when m is known.
func (s *Auth) authError(err error, m Manager) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, deviceauth.ErrIncompatible) && m != nil {
		return newError(codes.CodeIncompatibleServer, compatParams(m.State().Compat))
	}
	return mapError(err, s.log)
}

// userOf maps api.User to the binding's User.
func userOf(u *api.User) *User {
	if u == nil {
		return nil
	}
	return &User{ID: u.ID, Username: u.Username, Role: string(u.Role)}
}

// deviceOf maps api.DeviceRef to the binding's Device.
func deviceOf(d *api.DeviceRef) *Device {
	if d == nil {
		return nil
	}
	return &Device{ID: d.ID, Name: d.Name}
}

// cleanText reports text without NUL or other C0 control characters (a password may hold any other character).
func cleanText(v string) bool {
	for i := range len(v) {
		if v[i] < 0x20 && v[i] != '\t' {
			return false
		}
	}
	return true
}

// authState maps 02's State to the binding's JSON (02 §11.7). A check that was never made shows verdict ok.
func authState(st deviceauth.State) AuthState {
	out := AuthState{
		Origin:     st.Origin,
		ServerName: st.ServerName,
		Status:     string(st.Status),
		User:       userOf(st.User),
		Device:     deviceOf(st.Device),
		Notice:     string(st.Notice),
		Compat: CompatInfo{
			Verdict:          string(st.Compat.Verdict),
			Reason:           string(st.Compat.Reason),
			ServerVersion:    st.Compat.ServerVersion,
			MinClientVersion: st.Compat.MinClientVersion,
		},
	}
	if out.Compat.Verdict == "" {
		out.Compat.Verdict = string(compat.OK)
	}
	if st.Link != nil {
		out.Link = &LinkCode{
			UserCode:                st.Link.UserCode,
			VerificationURI:         st.Link.VerificationURI,
			VerificationURIComplete: st.Link.VerificationURIComplete,
			ExpiresAt:               st.Link.ExpiresAt.UnixMilli(),
		}
	}
	return out
}
