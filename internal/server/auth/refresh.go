package auth

import (
	"bytes"
	"context"
	"crypto/hmac"
	"errors"
	"log/slog"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// The refresh_token grant of POST /api/v1/device/token (M2 02 §6.2; README WA39): refresh tokens rotate on every use,
// a spent token that comes back revokes its device (reuse detection, RFC 9700 §4.14.2), and two grace paths keep an
// app signed in whose answer was lost: its own retry of one refresh (the same exchangeId, any time before the token
// expires) and a restart within 120 s of the spend (once). A grace re-issue keeps the child it replaces as a spent
// row, so that whoever presents that child later is caught as reuse.

// auditDeviceRefreshReused is the audit row of a reuse (M2 02 §8): actor system with the presenter's IP, target the
// device, detail {}; a security event.
const auditDeviceRefreshReused = "device.refresh_reused"

// Sizes and limits of M2 02 §3, §6.2, §6.7 and §15.
const (
	exchangeIDBytes          = 16                // an exchangeId: 22 base64url characters
	refreshGraceWindow       = 120 * time.Second // a restarted client's replay without its old exchangeId, once
	maxAccessTokensPerDevice = 5                 // TrimAccessTokens at every insert
)

// fieldExchangeID is the JSON name of the exchange id in a refresh_token grant, the key of its validation_failed
// answer.
const fieldExchangeID = "exchangeId"

// The two grace paths of M2 02 §6.2, for the debug line of a re-issue.
const (
	graceRetry   = "retry"   // the same exchangeId: the client's retry of one refresh
	graceRestart = "restart" // within 120 s of the spend, without that exchangeId: a restarted client
)

// errReused is 400 invalid_grant {reason: reused}: a spent refresh token came back, and its device was revoked.
func errReused() *api.Error {
	return &api.Error{Code: api.CodeInvalidGrant, Params: map[string]any{api.ParamReason: api.InvalidGrantReused}}
}

// RefreshReusedError is DeviceToken's answer to a refresh token that was spent already and took no grace path
// (M2 02 §6.2): the device is gone, device.refresh_reused is written and the admin alert raised. It unwraps to the
// client's answer, *api.Error invalid_grant with params {reason: "reused"}, which httpapi writes as for any service
// error, and it names the user whose device went, so that httpapi can notify {UserID}: devices after the call (M2 02
// §8), as it does after the other device changes. Neither ID is sent to the client.
type RefreshReusedError struct {
	UserID   store.UserID
	DeviceID store.DeviceID
}

// Error says what happened; the client's answer is Unwrap's.
func (e *RefreshReusedError) Error() string {
	return "auth: refresh token reused, device revoked: " + errReused().Error()
}

// Unwrap returns the client's answer: *api.Error invalid_grant {reason: reused}.
func (e *RefreshReusedError) Unwrap() error { return errReused() }

// refreshed is what the Write of a refresh decided.
type refreshed struct {
	user   store.User
	device store.Device
	// answer is the decided error answer (invalid_grant), returned after the commit; nil for a new pair.
	answer error
	// reused is set when the Write revoked the device.
	reused bool
	// grace is graceRetry or graceRestart for a grace re-issue, "" for a plain rotation.
	grace string
}

// refreshTokenGrant is the refresh_token grant (M2 02 §6.2), after device-token-ip and the grant type. In order:
//
//  1. the optional fields: an exchangeId that is not 22 characters of 16 bytes, or an appVersion that breaks
//     checkAppVersion's rule → 422 validation_failed {exchangeId|appVersion}, one answer naming both;
//  2. the refresh token's shape ("isr_" + 43) → 400 invalid_grant, before any hashing or DB access;
//  3. the new pair is drawn (no crypto/rand inside the Write), then one Write applies the table of §6.2 (refresh) and
//     commits, also when its answer is an error: a reuse deletes the device.
//
// After the commit: a new pair makes the bearer cache forget the device (its older access tokens may be gone), and a
// reuse runs refreshReused.
func (s *Service) refreshTokenGrant(ctx context.Context, in api.DeviceTokenRequest, m ReqMeta) (api.DeviceTokenResponse, error) {
	fields := map[string]string{}
	if in.ExchangeID != "" && !tokenWellFormed(in.ExchangeID, exchangeIDBytes) {
		fields[fieldExchangeID] = FieldInvalid
	}
	if in.AppVersion != "" {
		if code := checkAppVersion(in.AppVersion); code != "" {
			fields[fieldAppVersion] = code
		}
	}
	if len(fields) > 0 {
		return api.DeviceTokenResponse{}, validationFailed(fields)
	}
	if !refreshTokenWellFormed(in.RefreshToken) {
		return api.DeviceTokenResponse{}, errInvalidGrant()
	}
	presented := s.keys.deviceHash(in.RefreshToken)
	var exchange []byte
	if in.ExchangeID != "" {
		exchange = s.keys.deviceHash(in.ExchangeID)
	}
	access, refresh := newDeviceTokenPair()
	now := s.now()
	var out refreshed
	err := s.db.Write(ctx, func(q *store.Q) error {
		var err error
		out, err = s.refresh(q, presented, exchange, access, refresh, in.AppVersion, m, now)
		return err
	})
	if err != nil {
		return api.DeviceTokenResponse{}, serviceErr("refresh device token", err)
	}
	switch {
	case out.reused:
		s.refreshReused(ctx, out, m, now)
		return api.DeviceTokenResponse{}, &RefreshReusedError{UserID: out.user.ID, DeviceID: out.device.ID}
	case out.answer != nil:
		return api.DeviceTokenResponse{}, out.answer
	}
	// TrimAccessTokens and a grace path's DeleteAccessTokensIssuedBy may have deleted access tokens the cache knows.
	s.bearers.invalidateDevice(out.device.ID)
	if out.grace != "" {
		s.log.LogAttrs(ctx, slog.LevelDebug, "spent refresh token replayed; a new pair was issued in its grace",
			slog.String("user_id", string(out.user.ID)), slog.String("device_id", string(out.device.ID)),
			slog.String("grace", out.grace))
	}
	return linkedDevice{user: out.user, device: out.device}.response(access, refresh), nil
}

// refresh applies the table of M2 02 §6.2 inside its Write, with P the presented token (its hash presented), T its
// spent_at, C the token its replaced_by names and X the HMAC of the request's exchangeId (nil without one):
//
//	P unknown or expired, or its user not active                 400 invalid_grant
//	P unspent                                                    rotation: SpendRefreshToken(P, new refresh, X)
//	P spent, C is P's own child, X equals P.exchange_hash         grace (retry)
//	P spent, C is P's own child, now − T ≤ 120 s, C.created_at = T grace (restart)
//	any other spent P                                            reuse: DeleteDevice, device.refresh_reused,
//	                                                             400 invalid_grant {reason: reused}
//
// A grace path deletes C's access token (DeleteAccessTokensIssuedBy(P)), supersedes C (a spent row until its own
// expiry, exchange_hash NULL) and points P at the new refresh token (SetReplacedBy). Both a rotation and a grace path
// insert the new pair with issued_by = P, trim the device's access tokens to the newest 5 and record the device's use
// (TouchDevice with this request's IP and, when given, its appVersion). access and refresh are the tokens the caller
// drew before its Write.
func (s *Service) refresh(q *store.Q, presented, exchange []byte, access, refresh, appVersion string, m ReqMeta,
	now time.Time,
) (refreshed, error) {
	p, dev, user, err := q.DeviceTokenByHash(presented, tokenKindRefresh, now)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return refreshed{answer: errInvalidGrant()}, nil
	case err != nil:
		return refreshed{}, err
	case user.Status != store.StatusActive:
		// Disabling or resetting an account deletes its devices (03 §7.7), so this is a race; checked anyway.
		return refreshed{answer: errInvalidGrant()}, nil
	}
	out := refreshed{user: user, device: dev}
	newRefresh := s.keys.deviceHash(refresh)
	if p.SpentAt.IsZero() {
		if err := q.SpendRefreshToken(presented, newRefresh, exchange, now); err != nil {
			return refreshed{}, err
		}
	} else {
		child, grace, err := graceChild(q, p, exchange, now)
		if err != nil {
			return refreshed{}, err
		}
		if grace == "" {
			return s.reuse(q, out, m, now)
		}
		if _, err := q.DeleteAccessTokensIssuedBy(presented); err != nil {
			return refreshed{}, err
		}
		if err := q.SupersedeRefreshToken(child.TokenHash, newRefresh, now); err != nil {
			return refreshed{}, err
		}
		out.grace = grace
	}
	created := now
	if out.grace != "" && now.UnixMilli() <= p.SpentAt.UnixMilli() {
		// A replay in the spend's own millisecond (the store keeps milliseconds): the new pair must not look like the
		// spend's child, or the restart path would stay open for a second replay (graceChild's C.created_at = T).
		created = p.SpentAt.Add(time.Millisecond)
	}
	for _, t := range []store.DeviceToken{
		{TokenHash: s.keys.deviceHash(access), DeviceID: dev.ID, Kind: tokenKindAccess, CreatedAt: created,
			ExpiresAt: now.Add(accessTokenTTL), IssuedBy: presented},
		{TokenHash: newRefresh, DeviceID: dev.ID, Kind: tokenKindRefresh, CreatedAt: created,
			ExpiresAt: now.Add(refreshTokenTTL), IssuedBy: presented},
	} {
		if err := q.InsertDeviceToken(t); err != nil {
			return refreshed{}, err
		}
	}
	if out.grace != "" {
		if err := q.SetReplacedBy(presented, newRefresh); err != nil {
			return refreshed{}, err
		}
	}
	if _, err := q.TrimAccessTokens(dev.ID, maxAccessTokensPerDevice); err != nil {
		return refreshed{}, err
	}
	if err := q.TouchDevice(dev.ID, ipString(m.IP), appVersion, now); err != nil {
		return refreshed{}, err
	}
	return out, nil
}

// graceChild decides whether a replay of the spent refresh token p takes a grace path (M2 02 §6.2), and returns the
// child that path replaces with the path's name; grace is "" for a reuse. "C is P's own child" means that the token
// p's replaced_by names exists unexpired, is unspent, belongs to p's device and has issued_by = p: it came from p's own
// exchange. A child that a grace path superseded is spent, and its successor's issued_by is the grandparent, so a
// superseded row never qualifies as a parent here. exchange equals p's exchange_hash only when both are set: a
// request without an exchangeId never takes the retry path.
//
// The restart path's C.created_at = T holds only until a grace re-issue: the new pair is created at the replay's
// time, not at T, so a second replay without the exchangeId falls through to reuse.
func graceChild(q *store.Q, p store.DeviceToken, exchange []byte, now time.Time) (store.DeviceToken, string, error) {
	if len(p.ReplacedBy) == 0 {
		return store.DeviceToken{}, "", nil
	}
	c, _, _, err := q.DeviceTokenByHash(p.ReplacedBy, tokenKindRefresh, now)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return store.DeviceToken{}, "", nil
	case err != nil:
		return store.DeviceToken{}, "", err
	case !c.SpentAt.IsZero() || c.DeviceID != p.DeviceID || !bytes.Equal(c.IssuedBy, p.TokenHash):
		return store.DeviceToken{}, "", nil
	case len(exchange) > 0 && len(p.ExchangeHash) > 0 && hmac.Equal(exchange, p.ExchangeHash):
		return c, graceRetry, nil
	case now.Sub(p.SpentAt) <= refreshGraceWindow && c.CreatedAt.Equal(p.SpentAt):
		return c, graceRestart, nil
	}
	return store.DeviceToken{}, "", nil
}

// reuse is the last row of refresh's table, inside its Write: the device goes (with every token and its push
// subscriptions, by the cascade) and device.refresh_reused is written, with the system as the actor, the presenter's
// IP and the device as the target. The answer, invalid_grant {reason: reused}, is returned after the commit.
func (s *Service) reuse(q *store.Q, out refreshed, m ReqMeta, now time.Time) (refreshed, error) {
	if _, err := q.DeleteDevice(out.user.ID, out.device.ID); err != nil {
		return refreshed{}, err
	}
	err := q.AppendAudit(store.AuditEntry{At: now, Action: auditDeviceRefreshReused,
		Actor:      store.Actor{Kind: store.ActorSystem, Name: store.SystemActor.Name, IP: ipString(m.IP)},
		TargetKind: targetDevice, TargetID: string(out.device.ID), TargetName: out.device.Name})
	if err != nil {
		return refreshed{}, err
	}
	out.answer, out.reused = errReused(), true
	return out, nil
}

// refreshReused runs the steps after a committed reuse (M2 02 §6.2, §8): the warn line "refresh token reused, device
// revoked" with user_id, device_id and remote_ip (a security event; never a token or the device's name), the bearer
// cache forgets the device and its connections are closed with ReasonDeviceRevoked (devicesRevoked), and the admin
// alert refresh_token_reused is raised (actor system, target the username). httpapi notifies {UserID}: devices
// (RefreshReusedError).
func (s *Service) refreshReused(ctx context.Context, out refreshed, m ReqMeta, now time.Time) {
	s.log.LogAttrs(ctx, slog.LevelWarn, "refresh token reused, device revoked",
		slog.String("user_id", string(out.user.ID)), slog.String("device_id", string(out.device.ID)),
		slog.String("remote_ip", ipString(m.IP)))
	s.devicesRevoked(out.user.ID, out.device.ID)
	// It does not depend on the client staying connected.
	s.alert(context.WithoutCancel(ctx), AdminAlert{Kind: AlertRefreshTokenReused, Actor: store.SystemActor.Name,
		Target: out.user.Username, At: now})
}
