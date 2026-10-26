package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// The RFC 8628 device authorization flow on the server (M2 02 §5; README WA30): an app asks for a code
// (StartDeviceFlow), a logged-in friend looks it up and approves or denies it in the browser (LookupUserCode,
// DecideUserCode), and the app's polls of the device_code grant (DeviceToken) collect the answer: a new device with
// its first access and refresh token pair. The in-app password fallback (devicepassword.go) shares the grant's
// success steps (linkDevice). The refresh_token grant is refresh.go, bearer authentication bearer.go, and the device
// calls of the Devices page are in selfservice.go (README WA39).

// Audit actions written here (M2 02 §8). device.revoked {via: "cap"} is auditDeviceRevoked (selfservice.go).
const (
	auditDeviceApproved = "device.approved"
	auditDeviceDenied   = "device.denied"
	auditDeviceLinked   = "device.linked"
)

// scopeDeviceLookup is the scope of the auth.throttled row of the device-lookup bucket (M2 02 §5.5): actor the user,
// no key.
const scopeDeviceLookup = "device_lookup"

// The via of the device audit rows (M2 02 §8). linkedViaDeviceFlow and linkedViaPassword are also the values of
// devices.linked_via; the revokedVia values are device.revoked's.
const (
	linkedViaDeviceFlow  = "device_flow"
	linkedViaPassword    = "password"
	revokedViaCap        = "cap"
	revokedViaDevicePage = "devices_page" // the owner removed it on the Devices page (DELETE /me/devices/{id})
	revokedViaApp        = "app"          // the app signed itself out (POST /device/revoke, or its own ID there)
)

// device_codes.status values (03 §5).
const (
	codePending  = "pending"
	codeApproved = "approved"
	codeDenied   = "denied"
	codeConsumed = "consumed"
)

// device_tokens.kind values (03 §5).
const (
	tokenKindAccess  = "access"
	tokenKindRefresh = "refresh"
)

// Sizes, shapes and lifetimes of M2 02 §3 and §15.
const (
	deviceCodeBytes    = 32 // 43 base64url characters
	deviceTokenBytes   = 32 // the random part of an access or refresh token, 43 characters
	accessTokenPrefix  = "isa_"
	refreshTokenPrefix = "isr_"
	tokenTypeBearer    = "Bearer"

	deviceCodeTTL   = 600 * time.Second
	accessTokenTTL  = 900 * time.Second
	refreshTokenTTL = 90 * 24 * time.Hour

	devicePollInterval    = 5 * time.Second  // the interval of a new code
	devicePollStep        = 5 * time.Second  // slow_down adds this
	devicePollMaxInterval = 60 * time.Second // and stops here
	devicePollTolerance   = time.Second      // network jitter between two polls sent exactly interval apart

	// maxDevicesPerUser is the cap of 03 §7.12 and M2 02 §6.7: a new link first trims the user's devices to one
	// fewer, so the 21st evicts the least recently seen.
	maxDevicesPerUser = 20

	// userCodeDraws is how many user codes a start tries: the first draw and, on collisions with an existing row's
	// user_code_hash, 3 more (M2 02 §3: "retries with a new code, at most 3 times, then internal").
	userCodeDraws = 4

	// pendingCapRetryAfter is the Retry-After of the pending-code cap's 503 server_busy (M2 02 §5.2 step 4).
	pendingCapRetryAfter = 60

	appVersionMaxBytes = 32
)

// JSON field names of the device requests, the keys of their validation_failed answers (M2 02 §5.2, §5.3, §5.4).
const (
	fieldClientKind = "clientKind"
	fieldOS         = "os"
	fieldAppVersion = "appVersion"
	fieldUserCode   = "userCode"
	fieldGrantType  = "grantType"
)

// ---- errors ----

func errInvalidGrant() error      { return api.NewError(api.CodeInvalidGrant) }
func errDeviceCodeInvalid() error { return api.NewError(api.CodeDeviceCodeInvalid) }

// errSessionOnly is 403 forbidden {credential: session}: the answer of the UserSession routes (M2 02 §6.4) to a
// bearer. A device token never approves a code (M2 02 decision 2).
func errSessionOnly() error {
	return &api.Error{Code: api.CodeForbidden,
		Params: map[string]any{api.ParamCredential: string(api.CredentialKindSession)}}
}

// sessionCaller is the principal rule of the approval calls, LookupUserCode and DecideUserCode: only a web session
// approves. httpapi's UserSession access answers a bearer before the call; the service refuses it again (403
// forbidden {credential: session}), and a principal without a user or a session is unauthenticated.
func sessionCaller(p Principal) error {
	if p.Method == MethodBearer {
		return errSessionOnly()
	}
	if p.Method != MethodSession || p.UserID == "" || p.SessionID == "" {
		return errUnauthenticated()
	}
	return nil
}

// ---- codes and tokens ----

// newUserCode draws a normalized user code (M2 02 §3): rejection sampling over random bytes, where a byte b ≥ 240 is
// dropped and any other is the symbol alphabet[b % 20], until there are 8.
func newUserCode() string {
	var out [api.UserCodeLength]byte
	var buf [2 * api.UserCodeLength]byte
	n := 0
	for n < len(out) {
		_, _ = rand.Read(buf[:]) // never fails since Go 1.24
		for _, b := range buf {
			if b >= 240 {
				continue
			}
			out[n] = api.UserCodeAlphabet[int(b)%len(api.UserCodeAlphabet)]
			if n++; n == len(out) {
				break
			}
		}
	}
	return string(out[:])
}

// newDeviceTokenPair draws a new access token ("isa_" + 43) and refresh token ("isr_" + 43) (M2 02 §3). Callers draw
// the pair before their Write, so no crypto/rand runs inside one.
func newDeviceTokenPair() (access, refresh string) {
	return accessTokenPrefix + newToken(deviceTokenBytes), refreshTokenPrefix + newToken(deviceTokenBytes)
}

// deviceHash is the stored form of a device code, a normalized user code, an access or refresh token (the whole
// string, prefix included) and an exchange id: HMAC-SHA-256 with the session key (M2 02 §3), so a changed
// session key invalidates exactly these and the sessions (03 §4.6). The shapes of the four kinds differ, and each
// lives in its own table or kind, so one key serves them all.
func (k Keys) deviceHash(s string) []byte { return keyedHash(k.Session, s) }

// checkUserCode is the first step of a lookup and a decide (M2 02 §5.3): the normalized user code, or 422
// validation_failed {userCode: required} for a code with nothing but spaces, tabs and dashes and {userCode: invalid}
// for any other code NormalizeUserCode refuses. It costs no token.
func checkUserCode(in string) (string, error) {
	code, ok := api.NormalizeUserCode(in)
	switch {
	case ok:
		return code, nil
	case strings.Trim(in, " \t-") == "":
		return "", validationFailed(map[string]string{fieldUserCode: FieldRequired})
	}
	return "", validationFailed(map[string]string{fieldUserCode: FieldInvalid})
}

// ---- the app's own fields (M2 02 §5.2 step 3, §14 step 2) ----

// deviceFields are what an app says about itself when it asks to be linked.
type deviceFields struct {
	ClientKind, OS, Name, AppVersion string // Name normalized (NormalizeDeviceName)
}

// checkDeviceFields validates an app's four fields and adds a field code for each one that fails to fields: clientKind
// desktop or mobile (01's ClientKind; the Linux agent sends desktop), os one of the devices.os CHECK, deviceName by
// NormalizeDeviceName, appVersion by checkAppVersion. Every field is checked, so one answer names them all.
func checkDeviceFields(clientKind, os, name, appVersion string, fields map[string]string) deviceFields {
	out := deviceFields{ClientKind: clientKind, OS: os, AppVersion: appVersion}
	switch clientKind {
	case "desktop", "mobile":
	default:
		fields[fieldClientKind] = FieldInvalid
	}
	switch os {
	case "windows", "macos", "linux", "ios", "android":
	default:
		fields[fieldOS] = FieldInvalid
	}
	n, err := NormalizeDeviceName(name)
	var fe *FieldError
	switch {
	case errors.As(err, &fe):
		fields[fieldDeviceName] = fe.Code
	case err != nil:
		fields[fieldDeviceName] = FieldInvalid
	}
	out.Name = n
	if code := checkAppVersion(appVersion); code != "" {
		fields[fieldAppVersion] = code
	}
	return out
}

// checkAppVersion is the rule of an app version (M2 02 §5.2): 1–32 bytes of [0-9A-Za-z.+-], not necessarily SemVer
// (dev builds are 0.0.0-dev+<commit>). It returns the field code of a failure, "" for a valid version.
func checkAppVersion(v string) string {
	switch {
	case v == "":
		return FieldRequired
	case len(v) > appVersionMaxBytes:
		return FieldTooLong
	}
	for i := range len(v) {
		c := v[i]
		if (c < '0' || c > '9') && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && c != '.' && c != '+' && c != '-' {
			return FieldInvalid
		}
	}
	return ""
}

// ---- throttles (M2 02 §5.5) ----

// takeIPBucket takes a token of a per-IP bucket for the request's key. The first refusal of an episode writes an
// auth.throttled {scope: ip, key} row (the key is the address or the prefix the bucket is for); every refusal logs
// the warn line of logBlocked and is 429 rate_limited with retryAfter. It runs before any other work of its call.
func (s *Service) takeIPBucket(ctx context.Context, m ReqMeta, l *limiter[netip.Prefix], key netip.Prefix,
	keyText, bucket string,
) error {
	v := l.take(key)
	if v.OK {
		return nil
	}
	if v.First {
		s.auditAlone(ctx, store.AuditEntry{At: s.now(), Action: auditThrottled, Outcome: outcomeDenied,
			Actor: anonymous(m), Detail: map[string]any{"scope": scopeIP, "key": keyText}})
	}
	s.logBlocked(ctx, m, bucket)
	return rateLimited(v)
}

// takeDeviceCodeIP is step 2 of a start: device-code-ip by the client's IPKey, then, for an IPv6 client only,
// device-code-ip48 by its /48 (IPKey48). Both count every request; a refusal of the second keeps the first's token,
// as auth-ip keeps its token when a later bucket refuses.
func (s *Service) takeDeviceCodeIP(ctx context.Context, m ReqMeta) error {
	if err := s.takeIPBucket(ctx, m, s.throttle.deviceCodeIP, IPKey(m.IP), ipKeyText(m.IP), "device-code-ip"); err != nil {
		return err
	}
	if p, ok := IPKey48(m.IP); ok {
		return s.takeIPBucket(ctx, m, s.throttle.deviceCodeIP48, p, p.String(), "device-code-ip48")
	}
	return nil
}

// takeDeviceTokenIP is device-token-ip, the first step of every POST /device/token.
func (s *Service) takeDeviceTokenIP(ctx context.Context, m ReqMeta) error {
	return s.takeIPBucket(ctx, m, s.throttle.deviceTokenIP, IPKey(m.IP), ipKeyText(m.IP), "device-token-ip")
}

// lookupAttempt is the place one lookup or decide holds in the device-lookup bucket while its code is read. The
// bucket counts wrong codes, but the token is taken before the read, like the per-username buckets of a login
// (03 §7.3), so guesses sent in parallel all see the bucket as it is. A wrong code (404 device_code_invalid) keeps
// the token; every other end gives it back: a found code, a decision made (also an idempotent repeat), and the ends
// that say nothing about the code (the caller's session is gone, a store or context error).
type lookupAttempt struct {
	l    *limiter[store.UserID]
	user store.UserID
}

// end settles the attempt by the call's error: the token stays only for device_code_invalid (a wrong code keeps
// it); every other end gives it back.
func (a *lookupAttempt) end(err error) {
	if !api.IsCode(err, api.CodeDeviceCodeInvalid) {
		a.l.refund(a.user)
	}
}

// takeDeviceLookup reserves the caller's device-lookup token. The first refusal of an episode writes auth.throttled
// {scope: device_lookup} with the user as the actor and no key; every refusal logs logBlocked's warn line and is 429
// rate_limited with retryAfter, before any read.
func (s *Service) takeDeviceLookup(ctx context.Context, p Principal, m ReqMeta) (*lookupAttempt, error) {
	v := s.throttle.deviceLookup.take(p.UserID)
	if v.OK {
		return &lookupAttempt{l: s.throttle.deviceLookup, user: p.UserID}, nil
	}
	if v.First {
		s.auditAlone(ctx, store.AuditEntry{At: s.now(), Action: auditThrottled, Outcome: outcomeDenied,
			Actor: ActorOf(p, m.IP), Detail: map[string]any{"scope": scopeDeviceLookup}})
	}
	s.logBlocked(ctx, m, "device-lookup")
	return nil, rateLimited(v)
}

// ---- start: POST /api/v1/device/code (M2 02 §5.2) ----

// StartDeviceFlow starts a link (POST /api/v1/device/code, #44). In order, every step before the next:
//
//  1. auth-ip → 429 rate_limited;
//  2. device-code-ip, then device-code-ip48 for an IPv6 client → 429 rate_limited;
//  3. the app's fields (checkDeviceFields) → 422 validation_failed {clientKind|os|deviceName|appVersion};
//  4. one Write: the server-wide cap of 5,000 pending, unexpired codes → 503 server_busy with Retry-After: 60, then
//     the code: request_ip the client's, expires_at now + 600 s, interval 5 s. A user code that collides with an
//     existing row's is drawn again, up to 3 times, then internal.
//
// The URIs are built from Origins.Primary. No audit row: the request is anonymous and frequent (a throttled address
// writes auth.throttled).
func (s *Service) StartDeviceFlow(ctx context.Context, in api.DeviceCodeRequest, m ReqMeta) (api.DeviceCodeResponse, error) {
	if err := s.takeAuthIP(ctx, m); err != nil {
		return api.DeviceCodeResponse{}, err
	}
	if err := s.takeDeviceCodeIP(ctx, m); err != nil {
		return api.DeviceCodeResponse{}, err
	}
	fields := map[string]string{}
	dev := checkDeviceFields(in.ClientKind, in.OS, in.DeviceName, in.AppVersion, fields)
	if len(fields) > 0 {
		return api.DeviceCodeResponse{}, validationFailed(fields)
	}

	deviceCode := newToken(deviceCodeBytes)
	draws := make([]string, userCodeDraws)
	for i := range draws {
		draws[i] = s.userCode()
	}
	now := s.now()
	row := store.DeviceCode{
		DeviceCodeHash: s.keys.deviceHash(deviceCode),
		ClientKind:     dev.ClientKind,
		DeviceName:     dev.Name,
		OS:             dev.OS,
		AppVersion:     dev.AppVersion,
		RequestIP:      ipString(m.IP),
		CreatedAt:      now,
		ExpiresAt:      now.Add(deviceCodeTTL),
		Interval:       devicePollInterval,
		Status:         codePending,
	}
	userCode := ""
	err := s.db.Write(ctx, func(q *store.Q) error {
		n, err := q.CountPendingDeviceCodes(now)
		if err != nil {
			return err
		}
		if n >= maxPendingDeviceCodes {
			return &api.Error{Code: api.CodeServerBusy, RetryAfter: pendingCapRetryAfter}
		}
		for _, code := range draws {
			row.UserCodeHash = s.keys.deviceHash(code)
			err := q.CreateDeviceCode(row)
			var ce *store.ConflictError
			if errors.As(err, &ce) && ce.Column == "user_code_hash" {
				continue // the statement failed alone; the transaction goes on with the next draw
			}
			if err != nil {
				return err
			}
			userCode = code
			return nil
		}
		return fmt.Errorf("auth: %d user codes in a row collided with existing codes", len(draws))
	})
	if err != nil {
		return api.DeviceCodeResponse{}, serviceErr("start device flow", err)
	}
	shown := api.FormatUserCode(userCode)
	return api.DeviceCodeResponse{
		DeviceCode:              deviceCode,
		UserCode:                shown,
		VerificationURI:         s.origins.Primary + "/link",
		VerificationURIComplete: s.origins.Primary + "/link?code=" + shown,
		ExpiresIn:               int(deviceCodeTTL / time.Second),
		Interval:                int(devicePollInterval / time.Second),
	}, nil
}

// ---- approval: POST /api/v1/device/lookup and /device/decide (M2 02 §5.3) ----

// codeLive reports whether a code can still be decided: pending and unexpired at now (expires_at > now).
func codeLive(dc store.DeviceCode, now time.Time) bool {
	return dc.Status == codePending && now.Before(dc.ExpiresAt)
}

// networkMatch compares the address an app asked for its code from with the approving browser's (M2 02 §5.3), by
// IPKey (IPv4 /32, IPv6 /64; an IPv4-mapped IPv6 address counts as IPv4): same when the keys are equal, different
// when both are of one family and differ, unknown when the families differ or either address is unknown.
func networkMatch(requestIP string, browser netip.Addr) api.NetworkMatch {
	app, err := netip.ParseAddr(requestIP)
	if err != nil || !browser.IsValid() {
		return api.NetworkMatchUnknown
	}
	a, b := IPKey(app), IPKey(browser)
	switch {
	case a.Addr().Is4() != b.Addr().Is4():
		return api.NetworkMatchUnknown
	case a == b:
		return api.NetworkMatchSame
	}
	return api.NetworkMatchDifferent
}

// LookupUserCode shows the approval page what it is about to approve (POST /api/v1/device/lookup, #46; cookie
// sessions only). In order:
//
//  1. the caller: a web session (sessionCaller: a bearer is 403 forbidden {credential: session});
//  2. the user code's shape (checkUserCode) → 422 validation_failed {userCode}, costing no token;
//  3. device-lookup takes the caller's token before the read → 429 rate_limited;
//  4. the code: unknown, not pending or expired (expires_at ≤ now) → 404 device_code_invalid, and the token is kept
//     (a wrong code); found → the token goes back.
//
// Network compares the code's request IP with m.IP (networkMatch). Nothing is written.
func (s *Service) LookupUserCode(ctx context.Context, p Principal, userCode string, m ReqMeta) (api.DeviceLookup, error) {
	if err := sessionCaller(p); err != nil {
		return api.DeviceLookup{}, err
	}
	code, err := checkUserCode(userCode)
	if err != nil {
		return api.DeviceLookup{}, err
	}
	attempt, err := s.takeDeviceLookup(ctx, p, m)
	if err != nil {
		return api.DeviceLookup{}, err
	}
	out, err := s.lookupUserCode(ctx, code, m)
	attempt.end(err)
	return out, err
}

func (s *Service) lookupUserCode(ctx context.Context, code string, m ReqMeta) (api.DeviceLookup, error) {
	now := s.now()
	var dc store.DeviceCode
	err := s.db.Read(ctx, func(q *store.Q) error {
		var err error
		dc, err = q.DeviceCodeByUserCode(s.keys.deviceHash(code))
		return err
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		return api.DeviceLookup{}, errDeviceCodeInvalid()
	case err != nil:
		return api.DeviceLookup{}, internalErr("look up user code", err)
	case !codeLive(dc, now):
		return api.DeviceLookup{}, errDeviceCodeInvalid()
	}
	return api.DeviceLookup{
		UserCode:    api.FormatUserCode(code),
		ClientKind:  dc.ClientKind,
		OS:          dc.OS,
		DeviceName:  dc.DeviceName,
		AppVersion:  dc.AppVersion,
		RequestIP:   dc.RequestIP,
		Network:     networkMatch(dc.RequestIP, m.IP),
		RequestedAt: dc.CreatedAt,
		ExpiresAt:   dc.ExpiresAt,
	}, nil
}

// sameDecision reports whether a code's stored status is the decision want ("approved" or "denied") of the same
// approver again: a consumed code was approved, and its app has collected the answer since.
func sameDecision(status, want string) bool {
	return status == want || (want == codeApproved && status == codeConsumed)
}

// DecideUserCode approves or denies a code (POST /api/v1/device/decide, #47; cookie sessions only). In order: the
// caller and the user code as in LookupUserCode (steps 1 and 2), then deviceName when present (NormalizeDeviceName →
// 422 validation_failed {deviceName}), then the device-lookup token (step 3), then one Write:
//
//   - the caller again (loadSelf: an active user whose session still exists, else unauthenticated);
//   - DecideDeviceCode: only a pending, unexpired code changes, and a deviceName replaces the app's name. Of two users
//     deciding one code at once the first Write wins;
//   - 0 rows: a code this user already decided the same way (approve again after the app collected it included) is
//     answered 204 again with no second audit row (a double click, a retried request); anything else is 404
//     device_code_invalid and keeps the token;
//   - the audit row device.approved or device.denied (M2 02 §8): actor the approver with the request's IP, target the
//     device by its name (it has no id yet), detail {clientKind, os}.
//
// The token goes back on 204, also for the repeat. Approval never creates the device: it appears at the app's next
// poll (DeviceToken), so an approval nobody collects leaves only the code row, which expires.
func (s *Service) DecideUserCode(ctx context.Context, p Principal, in api.DeviceDecideRequest, m ReqMeta) error {
	if err := sessionCaller(p); err != nil {
		return err
	}
	code, err := checkUserCode(in.UserCode)
	if err != nil {
		return err
	}
	name := ""
	if in.DeviceName != nil {
		if name, err = NormalizeDeviceName(*in.DeviceName); err != nil {
			var fe *FieldError
			if errors.As(err, &fe) {
				return validationFailed(map[string]string{fieldDeviceName: fe.Code})
			}
			return internalErr("decide user code", err)
		}
	}
	attempt, err := s.takeDeviceLookup(ctx, p, m)
	if err != nil {
		return err
	}
	err = s.decideUserCode(ctx, p, code, in.Approve, name, m)
	attempt.end(err)
	return err
}

func (s *Service) decideUserCode(ctx context.Context, p Principal, code string, approve bool, name string,
	m ReqMeta,
) error {
	status, action := codeDenied, auditDeviceDenied
	if approve {
		status, action = codeApproved, auditDeviceApproved
	}
	h := s.keys.deviceHash(code)
	now := s.now()
	err := s.db.Write(ctx, func(q *store.Q) error {
		me, err := loadSelf(q, p, now)
		if err != nil {
			return err
		}
		err = q.DecideDeviceCode(h, status, me.user.ID, name, now)
		if errors.Is(err, store.ErrNotFound) {
			dc, err := q.DeviceCodeByUserCode(h)
			switch {
			case err == nil && dc.UserID == me.user.ID && sameDecision(dc.Status, status):
				return nil // the repeat of a decision made: 204 again, nothing written
			case err != nil && !errors.Is(err, store.ErrNotFound):
				return err
			}
			return errDeviceCodeInvalid()
		}
		if err != nil {
			return err
		}
		dc, err := q.DeviceCodeByUserCode(h)
		if err != nil {
			return err
		}
		return q.AppendAudit(store.AuditEntry{At: now, Action: action, Actor: userActor(me.user, m),
			TargetKind: targetDevice, TargetName: dc.DeviceName,
			Detail: map[string]any{"clientKind": dc.ClientKind, "os": dc.OS}})
	})
	if err != nil {
		return serviceErr("decide user code", err)
	}
	return nil
}

// ---- polling: POST /api/v1/device/token (M2 02 §5.4, §6.2) ----

// DeviceToken serves both grants of POST /api/v1/device/token (#45). First device-token-ip (→ 429 rate_limited),
// then the grant type (anything but device_code and refresh_token → 422 validation_failed {grantType: invalid}).
// The device_code grant is deviceCodeGrant, the refresh_token grant (M2 02 §6.2) refreshTokenGrant (refresh.go),
// whose answer to a reused token is a *RefreshReusedError{UserID, DeviceID} that unwraps to *api.Error invalid_grant
// {reason: reused}. The caller notifies {UserID}: devices when errors.As finds it (M2 02 §6.2, §8), then writes the
// error as usual; neither ID reaches the client.
func (s *Service) DeviceToken(ctx context.Context, in api.DeviceTokenRequest, m ReqMeta) (api.DeviceTokenResponse, error) {
	if err := s.takeDeviceTokenIP(ctx, m); err != nil {
		return api.DeviceTokenResponse{}, err
	}
	switch in.GrantType {
	case api.GrantTypeDeviceCode:
		return s.deviceCodeGrant(ctx, in.DeviceCode, m)
	case api.GrantTypeRefreshToken:
		return s.refreshTokenGrant(ctx, in, m)
	}
	return api.DeviceTokenResponse{}, validationFailed(map[string]string{fieldGrantType: FieldInvalid})
}

// deviceCodeGrant is one poll of an app (M2 02 §5.4). A device code of the wrong shape (§3) is 400 invalid_grant
// before any hashing or DB access. Then one Write decides the answer and commits, so that the poll's bookkeeping
// (last_poll_at, a raised interval) survives an error answer: the Write returns nil and the decided error is returned
// after the commit. The first row that matches the code at the poll decides:
//
//	unknown hash (never issued, pruned)                 400 invalid_grant          —
//	consumed                                            400 invalid_grant          —
//	now ≥ expires_at                                    400 expired_token          —
//	polled before, now − last_poll_at < interval − 1 s  400 slow_down {interval}   interval + 5 s (≤ 60 s), last_poll_at
//	pending                                             400 authorization_pending  last_poll_at
//	denied                                              400 access_denied          last_poll_at
//	approved, the approver no longer active             400 access_denied          last_poll_at
//	approved                                            200 the token pair         linkDevice, consumed
//
// Success (linkDevice) evicts beyond the cap, creates the device with linked_via device_flow and the name, kind, OS
// and version of the code, its first pair and the device.linked row with the approver as the actor and this poll's
// IP; ConsumeDeviceCode then makes sure a code yields one pair. After the commit, deviceLinked logs the link and
// closes the evicted devices' connections; httpapi notifies {UserID}: devices.
func (s *Service) deviceCodeGrant(ctx context.Context, deviceCode string, m ReqMeta) (api.DeviceTokenResponse, error) {
	if !tokenWellFormed(deviceCode, deviceCodeBytes) {
		return api.DeviceTokenResponse{}, errInvalidGrant()
	}
	h := s.keys.deviceHash(deviceCode)
	access, refresh := newDeviceTokenPair()
	now := s.now()
	var answer error
	var linked linkedDevice
	err := s.db.Write(ctx, func(q *store.Q) error {
		ok, decided, err := pollDeviceCode(q, h, now)
		answer = decided
		if decided != nil || err != nil {
			return err
		}
		dc := ok.code
		linked, err = s.linkDevice(q, ok.approver, store.Device{Name: dc.DeviceName, ClientKind: dc.ClientKind,
			OS: dc.OS, AppVersion: dc.AppVersion, LinkedVia: linkedViaDeviceFlow}, access, refresh, m, now)
		if err != nil {
			return err
		}
		return q.ConsumeDeviceCode(h, now)
	})
	if err != nil {
		return api.DeviceTokenResponse{}, serviceErr("device token", err)
	}
	if answer != nil {
		return api.DeviceTokenResponse{}, answer
	}
	s.deviceLinked(ctx, linked)
	return linked.response(access, refresh), nil
}

// approvedCode is the success row of deviceCodeGrant's table: an approved code and its approver, who is active.
type approvedCode struct {
	code     store.DeviceCode
	approver store.User
}

// pollDeviceCode applies the rows of deviceCodeGrant's table inside its Write. It returns the decided error answer
// with its bookkeeping written, or, for the success row, the approved code with its approver and a nil answer. err
// is a store error only.
func pollDeviceCode(q *store.Q, h []byte, now time.Time) (ok approvedCode, answer, err error) {
	dc, err := q.DeviceCodeByHash(h)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return approvedCode{}, errInvalidGrant(), nil
	case err != nil:
		return approvedCode{}, nil, err
	case dc.Status == codeConsumed:
		return approvedCode{}, errInvalidGrant(), nil
	case !now.Before(dc.ExpiresAt):
		return approvedCode{}, api.NewError(api.CodeExpiredToken), nil
	case !dc.LastPollAt.IsZero() && now.Sub(dc.LastPollAt) < dc.Interval-devicePollTolerance:
		next := min(dc.Interval+devicePollStep, devicePollMaxInterval)
		answer := &api.Error{Code: api.CodeSlowDown, Params: map[string]any{api.ParamInterval: int(next / time.Second)}}
		return approvedCode{}, answer, q.PollDeviceCode(h, now, next)
	}
	switch dc.Status {
	case codePending:
		return approvedCode{}, api.NewError(api.CodeAuthorizationPending), q.PollDeviceCode(h, now, dc.Interval)
	case codeDenied:
		return approvedCode{}, api.NewError(api.CodeAccessDenied), q.PollDeviceCode(h, now, dc.Interval)
	case codeApproved:
	default:
		return approvedCode{}, nil, fmt.Errorf("auth: device code with unknown status %q", dc.Status)
	}
	approver, err := q.UserByID(dc.UserID)
	switch {
	case errors.Is(err, store.ErrNotFound) || (err == nil && approver.Status != store.StatusActive):
		// The approver is gone or no longer active. Disabling or deleting an account also deletes the codes it
		// decided (03 §7.7), which makes the poll's answer invalid_grant; this row covers a status that changed while
		// the code stayed.
		return approvedCode{}, api.NewError(api.CodeAccessDenied), q.PollDeviceCode(h, now, dc.Interval)
	case err != nil:
		return approvedCode{}, nil, err
	}
	return approvedCode{code: dc, approver: approver}, nil, nil
}

// ---- the success steps of a link (M2 02 §5.4, §14 step 6) ----

// linkedDevice is what the success Write of a link made: the account and the device, the devices the cap evicted,
// and how the device was linked.
type linkedDevice struct {
	user    store.User
	device  store.Device
	evicted []store.Device
	via     string
}

// linkDevice runs a link's success steps inside the caller's Write, for the device_code grant (the approver's
// account) and the password fallback (the account itself):
//
//   - TrimDevices(user, 19), the cap of 20 (M2 02 §6.7), with one device.revoked {via: "cap"} row per evicted device
//     (actor system, no IP);
//   - CreateDevice: the name, kind, OS, version and linked_via of d, created and last seen now, last_ip this request's
//     IP;
//   - the first access (900 s) and refresh (90 days) tokens, issued_by NULL;
//   - device.linked {clientKind, os, via} with the user as the actor and this request's IP (M2 02 §8).
//
// access and refresh are the tokens the caller drew before its Write (newDeviceTokenPair).
func (s *Service) linkDevice(q *store.Q, user store.User, d store.Device, access, refresh string, m ReqMeta,
	now time.Time,
) (linkedDevice, error) {
	evicted, err := q.TrimDevices(user.ID, maxDevicesPerUser-1)
	if err != nil {
		return linkedDevice{}, err
	}
	for _, ev := range evicted {
		err := q.AppendAudit(store.AuditEntry{At: now, Action: auditDeviceRevoked, Actor: store.SystemActor,
			TargetKind: targetDevice, TargetID: string(ev.ID), TargetName: ev.Name,
			Detail: map[string]any{"via": revokedViaCap}})
		if err != nil {
			return linkedDevice{}, err
		}
	}
	d.UserID, d.CreatedAt, d.LastSeenAt, d.LastIP = user.ID, now, now, ipString(m.IP)
	if err := q.CreateDevice(&d); err != nil {
		return linkedDevice{}, err
	}
	for _, t := range []store.DeviceToken{
		{TokenHash: s.keys.deviceHash(access), DeviceID: d.ID, Kind: tokenKindAccess, CreatedAt: now,
			ExpiresAt: now.Add(accessTokenTTL)},
		{TokenHash: s.keys.deviceHash(refresh), DeviceID: d.ID, Kind: tokenKindRefresh, CreatedAt: now,
			ExpiresAt: now.Add(refreshTokenTTL)},
	} {
		if err := q.InsertDeviceToken(t); err != nil {
			return linkedDevice{}, err
		}
	}
	err = q.AppendAudit(store.AuditEntry{At: now, Action: auditDeviceLinked, Actor: userActor(user, m),
		TargetKind: targetDevice, TargetID: string(d.ID), TargetName: d.Name,
		Detail: map[string]any{"clientKind": d.ClientKind, "os": d.OS, "via": d.LinkedVia}})
	if err != nil {
		return linkedDevice{}, err
	}
	return linkedDevice{user: user, device: d, evicted: evicted, via: d.LinkedVia}, nil
}

// deviceLinked runs the steps after a link's commit: the info line "device linked" (user_id, device_id, via; never
// the name, M2 02 §8), then the evicted devices leave the bearer cache and their connections are closed with
// ReasonDeviceRevoked (devicesRevoked; 03 §7.7's order: the commit first).
func (s *Service) deviceLinked(ctx context.Context, ld linkedDevice) {
	s.log.LogAttrs(ctx, slog.LevelInfo, "device linked", slog.String("user_id", string(ld.user.ID)),
		slog.String("device_id", string(ld.device.ID)), slog.String("via", ld.via))
	for _, ev := range ld.evicted {
		s.devicesRevoked(ld.user.ID, ev.ID)
	}
}

// response is the 200 answer of a link (M2 02 §5.4): the same shape for both grants and the password fallback.
func (ld linkedDevice) response(access, refresh string) api.DeviceTokenResponse {
	return api.DeviceTokenResponse{
		AccessToken:      access,
		TokenType:        tokenTypeBearer,
		ExpiresIn:        int(accessTokenTTL / time.Second),
		RefreshToken:     refresh,
		RefreshExpiresIn: int(refreshTokenTTL / time.Second),
		Device:           api.DeviceRef{ID: string(ld.device.ID), Name: ld.device.Name},
		User:             api.User{ID: string(ld.user.ID), Username: ld.user.Username, Role: api.Role(ld.user.Role)},
	}
}
