package httpapi

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/auth"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// The endpoint table of M2 02 §7.1 (README WA47) through the real chain with a real auth.Service: each device route's
// success and its endpoint-specific errors, with the notifications of §8 and the credential each route takes.

// userCodeShape is a formatted user code (M2 02 §3).
var userCodeShape = regexp.MustCompile(`^[BCDFGHJKLMNPQRSTVWXZ]{4}-[BCDFGHJKLMNPQRSTVWXZ]{4}$`)

// testExchangeID is an exchangeId of the right shape: 22 base64url characters of 16 bytes (M2 02 §3).
const testExchangeID = "q9V3xT0bLm2Rk7aZ4cWfYg"

// newMember invites a member as the admin behind c and joins it from ip.
func (f *authFixture) newMember(c *http.Cookie, name, ip string) (api.User, *http.Cookie) {
	f.t.Helper()
	_, token := f.createInvite(c, `{}`)
	return f.join(token, name, ip)
}

// otherUserCode returns a well-formed user code that is not code.
func otherUserCode(code string) string {
	if code != "BCDF-GHJK" {
		return "BCDF-GHJK"
	}
	return "LMNP-QRST"
}

// wantNotes checks the Notify calls since the last check.
func (f *authFixture) wantNotes(what string, want ...string) {
	f.t.Helper()
	if got := f.signal.takeNotes(); !slices.Equal(got, want) {
		f.t.Errorf("%s: notified %q, want %q", what, got, want)
	}
}

// devicesNote is the {UserID}: devices notification of a user.
func devicesNote(userID string) string { return "user " + userID + " [devices]" }

// TestDeviceFlowOverREST is the device flow of M2 02 §5 over REST: #44 start, #45 polls (pending, slow_down, the
// pair, then a spent code; denied, expired, unknown and malformed codes; an unknown grant type), #46 lookup and #47
// decide (idempotent, a new name, their 422 and 404 answers, a bearer refused), and who is told what (§8).
func TestDeviceFlowOverREST(t *testing.T) {
	f := newAuthFixture(t)
	admin, cookie := f.setupAdmin("Alex")
	f.signal.takeNotes()
	start := f.clk.now()

	// #44: the app starts a link.
	code := f.startedLink("DESKTOP-7F3K2LQ", addrA)
	if len(code.DeviceCode) != 43 || !userCodeShape.MatchString(code.UserCode) ||
		code.VerificationURI != testOrigin+"/link" || code.VerificationURIComplete != testOrigin+"/link?code="+code.UserCode ||
		code.ExpiresIn != 600 || code.Interval != 5 {
		t.Fatalf("POST /device/code = user code %q, URIs %q and %q, expiresIn %d, interval %d, device code of %d "+
			"characters", code.UserCode, code.VerificationURI, code.VerificationURIComplete, code.ExpiresIn, code.Interval,
			len(code.DeviceCode))
	}

	// #45 before the decision: authorization_pending; a second poll at once: slow_down with the new interval.
	wantError(t, f.pollCode(code.DeviceCode, addrA), http.StatusBadRequest, api.CodeAuthorizationPending)
	if e := wantError(t, f.pollCode(code.DeviceCode, addrA), http.StatusBadRequest, api.CodeSlowDown); e.Params[api.ParamInterval] != float64(10) {
		t.Errorf("slow_down params %v, want {interval: 10}", e.Params)
	}

	// #46: the friend's browser looks the code up, typed in lower case.
	lk := decodeBody[api.DeviceLookup](t, f.lookupCode(cookie, strings.ToLower(code.UserCode)), http.StatusOK)
	if lk.UserCode != code.UserCode || lk.ClientKind != "desktop" || lk.OS != "windows" || lk.DeviceName != "DESKTOP-7F3K2LQ" ||
		lk.AppVersion != appVersion || lk.RequestIP != addrA || lk.Network != api.NetworkMatchSame ||
		!lk.RequestedAt.Equal(start) || !lk.ExpiresAt.Equal(start.Add(600*time.Second)) {
		t.Errorf("POST /device/lookup = %+v", lk)
	}
	for in, field := range map[string]string{"": api.FieldRequired, "WDJB-MJHA": api.FieldInvalid, "WDJB-MJH": api.FieldInvalid} {
		e := wantError(t, f.lookupCode(cookie, in), http.StatusUnprocessableEntity, api.CodeValidationFailed)
		if len(e.Fields) != 1 || e.Fields["userCode"] != field {
			t.Errorf("lookup of %q: fields %v, want {userCode: %s}", in, e.Fields, field)
		}
	}
	wantError(t, f.lookupCode(cookie, otherUserCode(code.UserCode)), http.StatusNotFound, api.CodeDeviceCodeInvalid)

	// #47: the friend approves under another name; the same decision again is 204 too, the other one 404; a name that
	// is too long is 422 before anything happens.
	long := strings.Repeat("x", 65)
	e := wantError(t, f.call(http.MethodPost, "/api/v1/device/decide", jsonBody(t, api.DeviceDecideRequest{
		UserCode: code.UserCode, Approve: true, DeviceName: &long}), with(cookie)),
		http.StatusUnprocessableEntity, api.CodeValidationFailed)
	if len(e.Fields) != 1 || e.Fields["deviceName"] != api.FieldTooLong {
		t.Errorf("decide with a long name: fields %v", e.Fields)
	}
	name := "Gaming PC"
	wantNoContent(t, f.call(http.MethodPost, "/api/v1/device/decide", jsonBody(t, api.DeviceDecideRequest{
		UserCode: code.UserCode, Approve: true, DeviceName: &name}), with(cookie)))
	wantNoContent(t, f.decideCode(cookie, code.UserCode, true))
	wantError(t, f.decideCode(cookie, code.UserCode, false), http.StatusNotFound, api.CodeDeviceCodeInvalid)
	wantError(t, f.decideCode(cookie, otherUserCode(code.UserCode), true), http.StatusNotFound, api.CodeDeviceCodeInvalid)
	f.wantNotes("approve") // approve and deny notify nobody (M2 02 §8)

	// #45 once approved, after the interval: the pair, from the app's new address; the owner's SPAs hear of the device.
	f.clk.advance(10 * time.Second)
	res := decodeBody[api.DeviceTokenResponse](t, f.pollCode(code.DeviceCode, addrB), http.StatusOK)
	if res.TokenType != "Bearer" || res.ExpiresIn != 900 || res.RefreshExpiresIn != 7776000 ||
		!accessTokenShape.MatchString(res.AccessToken) || !refreshTokenShape.MatchString(res.RefreshToken) ||
		res.Device.Name != name || res.Device.ID == "" ||
		res.User != (api.User{ID: admin.ID, Username: "Alex", Role: api.RoleAdmin}) {
		t.Errorf("the pair: type %q, expiresIn %d, refreshExpiresIn %d, device %+v, user %+v", res.TokenType, res.ExpiresIn,
			res.RefreshExpiresIn, res.Device, res.User)
	}
	f.wantNotes("link", devicesNote(admin.ID))
	wantError(t, f.pollCode(code.DeviceCode, addrB), http.StatusBadRequest, api.CodeInvalidGrant) // spent
	if got := f.deviceList(cookie); len(got) != 1 || got[0].ID != res.Device.ID || got[0].LastIP != addrB {
		t.Errorf("devices after the link: %+v", got)
	}
	me := decodeBody[api.Me](t, f.call(http.MethodGet, "/api/v1/me", "", withBearer(res.AccessToken)), http.StatusOK)
	if me.Device == nil || me.Device.ID != res.Device.ID {
		t.Errorf("GET /me with the new token: %+v", me)
	}

	// A denied code: access_denied. An expired one: expired_token, and the page no longer finds it. An unknown and a
	// malformed device code: invalid_grant. An unknown grant type: 422.
	denied := f.startedLink("PC", addrC)
	wantNoContent(t, f.decideCode(cookie, denied.UserCode, false))
	wantError(t, f.pollCode(denied.DeviceCode, addrC), http.StatusBadRequest, api.CodeAccessDenied)
	expired := f.startedLink("PC", addrC)
	f.clk.advance(600 * time.Second)
	wantError(t, f.pollCode(expired.DeviceCode, addrC), http.StatusBadRequest, api.CodeExpiredToken)
	wantError(t, f.lookupCode(cookie, expired.UserCode), http.StatusNotFound, api.CodeDeviceCodeInvalid)
	wantError(t, f.pollCode(strings.Repeat("A", 43), addrC), http.StatusBadRequest, api.CodeInvalidGrant)
	wantError(t, f.pollCode("not-a-device-code", addrC), http.StatusBadRequest, api.CodeInvalidGrant)
	e = wantError(t, f.call(http.MethodPost, "/api/v1/device/token", `{"grantType":"password"}`, goClient),
		http.StatusUnprocessableEntity, api.CodeValidationFailed)
	if len(e.Fields) != 1 || e.Fields["grantType"] != api.FieldInvalid {
		t.Errorf("an unknown grant type: fields %v", e.Fields)
	}
	f.wantNotes("refusals")

	// The approval routes take a cookie alone (M2 02 decision 2): a valid bearer, the admin's own, is 403
	// {credential: session} and a bad one 401. The Public routes ignore the header: DesktopPlatform sends it on
	// every call while it has a token.
	fresh := f.startedLink("PC", addrC)
	lookup := jsonBody(t, api.DeviceLookupRequest{UserCode: fresh.UserCode})
	wantForbiddenFor(t, f.call(http.MethodPost, "/api/v1/device/lookup", lookup, withBearer(res.AccessToken)),
		api.CredentialKindSession)
	wantForbiddenFor(t, f.call(http.MethodPost, "/api/v1/device/decide", lookup, with(cookie), withBearer(res.AccessToken)),
		api.CredentialKindSession)
	wantInvalidToken(t, f.call(http.MethodPost, "/api/v1/device/lookup", lookup, withBearer(unknownAccessToken)))
	wantError(t, f.call(http.MethodPost, "/api/v1/device/lookup", lookup), http.StatusUnauthorized, api.CodeUnauthenticated)
	decodeBody[api.DeviceCodeResponse](t, f.call(http.MethodPost, "/api/v1/device/code", jsonBody(t, appRequest("PC")),
		goClient, from(addrC), withBearer(unknownAccessToken)), http.StatusOK)
	wantError(t, f.call(http.MethodPost, "/api/v1/device/token", jsonBody(t, api.DeviceTokenRequest{
		GrantType: api.GrantTypeDeviceCode, DeviceCode: fresh.DeviceCode}), goClient, withBearer(unknownAccessToken)),
		http.StatusBadRequest, api.CodeAuthorizationPending)
	// The code was never decided by the bearer requests.
	decodeBody[api.DeviceLookup](t, f.lookupCode(cookie, fresh.UserCode), http.StatusOK)
}

// TestDeviceCodeLimits: #44's 422 field codes, its device-code-ip bucket (429 with Retry-After; the 422 answers count
// too) and the server-wide cap of 5,000 pending codes (503 server_busy with Retry-After: 60).
func TestDeviceCodeLimits(t *testing.T) {
	f := newAuthFixture(t)
	bad := `{"clientKind":"tv","deviceName":"","os":"amiga","appVersion":"1 0"}`
	e := wantError(t, f.call(http.MethodPost, "/api/v1/device/code", bad, goClient, from(addrA)),
		http.StatusUnprocessableEntity, api.CodeValidationFailed)
	if want := map[string]string{"clientKind": api.FieldInvalid, "deviceName": api.FieldRequired, "os": api.FieldInvalid,
		"appVersion": api.FieldInvalid}; fmt.Sprint(e.Fields) != fmt.Sprint(want) {
		t.Errorf("fields %v, want %v", e.Fields, want)
	}
	for range 9 {
		decodeBody[api.DeviceCodeResponse](t, f.startLink("PC", addrA), http.StatusOK)
	}
	rec := f.startLink("PC", addrA)
	e = wantError(t, rec, http.StatusTooManyRequests, api.CodeRateLimited)
	if e.RetryAfter <= 0 || rec.Header().Get("Retry-After") != strconv.Itoa(e.RetryAfter) {
		t.Errorf("429: retryAfter %d, Retry-After %q", e.RetryAfter, rec.Header().Get("Retry-After"))
	}
	decodeBody[api.DeviceCodeResponse](t, f.startLink("PC", addrB), http.StatusOK) // another address

	// The cap: 5,000 pending, unexpired codes, 10 of them from above.
	now := f.clk.now().UnixMilli()
	if _, err := f.rawDB().ExecContext(context.Background(), `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1
		FROM n WHERE i < 4990) INSERT INTO device_codes (device_code_hash, user_code_hash, client_kind, device_name, os,
		app_version, request_ip, created_at, expires_at) SELECT randomblob(32), randomblob(32), 'desktop', 'PC',
		'windows', '0.2.0', '192.0.2.1', ?, ? FROM n`, now, now+600_000); err != nil {
		t.Fatal(err)
	}
	rec = f.startLink("PC", addrC)
	e = wantError(t, rec, http.StatusServiceUnavailable, api.CodeServerBusy)
	if e.RetryAfter != 60 || rec.Header().Get("Retry-After") != "60" {
		t.Errorf("503: retryAfter %d, Retry-After %q", e.RetryAfter, rec.Header().Get("Retry-After"))
	}
	// Codes that expire make room again.
	f.clk.advance(600 * time.Second)
	decodeBody[api.DeviceCodeResponse](t, f.startLink("PC", addrC), http.StatusOK)
}

// TestDeviceRefreshOverREST is #45's refresh_token grant (M2 02 §6.2): a rotation notifies nobody, a malformed
// exchangeId or appVersion is 422, a malformed token 400, and a reused token revokes the device, closes its
// connections, raises the alert and tells the owner's SPAs, while the client gets invalid_grant {reason: reused}
// alone; then the device-token-ip bucket.
func TestDeviceRefreshOverREST(t *testing.T) {
	f := newAuthFixture(t)
	admin, cookie := f.setupAdmin("Alex")
	app := f.linkApp(cookie, "PC", addrA)
	f.signal.takeNotes()
	f.conns.take()
	f.alerts.take()

	next := decodeBody[api.DeviceTokenResponse](t, f.refresh(app.RefreshToken, testExchangeID, addrA), http.StatusOK)
	if next.AccessToken == app.AccessToken || next.RefreshToken == app.RefreshToken || next.Device != app.Device ||
		next.User != app.User || next.TokenType != "Bearer" || next.ExpiresIn != 900 {
		t.Errorf("a rotation answered device %+v, user %+v", next.Device, next.User)
	}
	f.wantNotes("rotation")
	decodeBody[api.Me](t, f.call(http.MethodGet, "/api/v1/me", "", withBearer(next.AccessToken)), http.StatusOK)

	e := wantError(t, f.refresh(next.RefreshToken, "not-an-exchange-id", addrA), http.StatusUnprocessableEntity,
		api.CodeValidationFailed)
	if len(e.Fields) != 1 || e.Fields["exchangeId"] != api.FieldInvalid {
		t.Errorf("a bad exchangeId: fields %v", e.Fields)
	}
	e = wantError(t, f.call(http.MethodPost, "/api/v1/device/token", jsonBody(t, api.DeviceTokenRequest{
		GrantType: api.GrantTypeRefreshToken, RefreshToken: next.RefreshToken, AppVersion: "0.2.0 beta"}), goClient),
		http.StatusUnprocessableEntity, api.CodeValidationFailed)
	if len(e.Fields) != 1 || e.Fields["appVersion"] != api.FieldInvalid {
		t.Errorf("a bad appVersion: fields %v", e.Fields)
	}
	if e = wantError(t, f.refresh("isr_short", "", addrA), http.StatusBadRequest, api.CodeInvalidGrant); len(e.Params) != 0 {
		t.Errorf("a malformed token: params %v", e.Params)
	}

	// The app rotates once more; then the first token comes back, whose child was used: reuse.
	last := decodeBody[api.DeviceTokenResponse](t, f.refresh(next.RefreshToken, "", addrA), http.StatusOK)
	f.wantNotes("second rotation")
	e = wantError(t, f.refresh(app.RefreshToken, "", addrB), http.StatusBadRequest, api.CodeInvalidGrant)
	if len(e.Params) != 1 || e.Params[api.ParamReason] != api.InvalidGrantReused {
		t.Errorf("reuse: params %v, want {reason: reused}", e.Params)
	}
	f.wantNotes("reuse", devicesNote(admin.ID))
	if got := f.conns.take(); len(got) != 1 || got[0].sel != (auth.ConnSelector{UserID: store.UserID(admin.ID),
		DeviceID: store.DeviceID(app.Device.ID)}) || got[0].reason != auth.ReasonDeviceRevoked {
		t.Errorf("reuse closed %+v", got)
	}
	if got := f.alerts.take(); len(got) != 1 || got[0].Kind != auth.AlertRefreshTokenReused || got[0].Target != "Alex" {
		t.Errorf("reuse alerts %+v", got)
	}
	wantInvalidToken(t, f.call(http.MethodGet, "/api/v1/me", "", withBearer(last.AccessToken)))
	if e = wantError(t, f.refresh(last.RefreshToken, "", addrA), http.StatusBadRequest, api.CodeInvalidGrant); len(e.Params) != 0 {
		t.Errorf("a token of a revoked device: params %v, want none (unknown)", e.Params)
	}
	if got := f.deviceList(cookie); len(got) != 0 {
		t.Errorf("devices after the reuse: %+v", got)
	}
	f.wantNotes("after the reuse")

	// device-token-ip: 60 requests per address, both grants and every answer, then 429.
	for range 60 {
		wantError(t, f.refresh("isr_short", "", addrC), http.StatusBadRequest, api.CodeInvalidGrant)
	}
	rec := f.pollCode(strings.Repeat("A", 43), addrC)
	if e := wantError(t, rec, http.StatusTooManyRequests, api.CodeRateLimited); rec.Header().Get("Retry-After") != strconv.Itoa(e.RetryAfter) {
		t.Errorf("429: Retry-After %q, retryAfter %d", rec.Header().Get("Retry-After"), e.RetryAfter)
	}
}

// TestDevicePasswordOverREST is #48 (M2 02 §14): a correct password links a device without a session cookie and tells
// the SPAs; the answers of #2 otherwise (401, 403 pending and disabled, 422 for every field, 429).
func TestDevicePasswordOverREST(t *testing.T) {
	f := newAuthFixture(t)
	admin, cookie := f.setupAdmin("Alex")
	sam, _ := f.newMember(cookie, "Sam", addrB)
	f.signal.takeNotes()

	rec := f.passwordLink("Alex", testPassword, "Alex-PC", addrA)
	res := decodeBody[api.DeviceTokenResponse](t, rec, http.StatusOK)
	if res.User.ID != admin.ID || res.Device.Name != "Alex-PC" || !accessTokenShape.MatchString(res.AccessToken) ||
		!refreshTokenShape.MatchString(res.RefreshToken) || len(rec.Header().Values("Set-Cookie")) != 0 {
		t.Errorf("POST /device/password = device %+v, user %+v, Set-Cookie %q", res.Device, res.User,
			rec.Header().Values("Set-Cookie"))
	}
	f.wantNotes("password link", devicesNote(admin.ID))
	if rows := f.auditOf("device.linked"); len(rows) != 1 || fmt.Sprint(rows[0].Detail) != "map[clientKind:desktop os:windows via:password]" {
		t.Errorf("device.linked rows %+v", rows)
	}
	if got := decodeBody[api.DevicesResponse](t, f.call(http.MethodGet, "/api/v1/me/devices", "",
		withBearer(res.AccessToken)), http.StatusOK).Devices; len(got) != 1 || !got[0].Current {
		t.Errorf("GET /me/devices with the new token: %+v", got)
	}

	wantError(t, f.passwordLink("Alex", "not the password", "PC", addrA), http.StatusUnauthorized, api.CodeInvalidCredentials)
	wantError(t, f.passwordLink("Nobody", testPassword, "PC", addrA), http.StatusUnauthorized, api.CodeInvalidCredentials)
	e := wantError(t, f.call(http.MethodPost, "/api/v1/device/password", `{}`, goClient, from(addrA)),
		http.StatusUnprocessableEntity, api.CodeValidationFailed)
	if got := fmt.Sprint(slices.Sorted(maps.Keys(e.Fields))); got != "[appVersion clientKind deviceName os password username]" {
		t.Errorf("an empty request: fields %v", e.Fields)
	}
	disabled := store.StatusDisabled
	if _, err := f.svc.UpdateUser(context.Background(), store.CLIActor, store.UserID(sam.ID),
		auth.UserChange{Status: &disabled}); err != nil {
		t.Fatal(err)
	}
	wantError(t, f.passwordLink("Sam", testPassword, "PC", addrB), http.StatusForbidden, api.CodeAccountDisabled)
	f.setMode(cookie, "approval")
	f.signup("Kim", addrC)
	wantError(t, f.passwordLink("Kim", testPassword, "PC", addrC), http.StatusForbidden, api.CodeAccountPending)
	for _, note := range f.signal.takeNotes() {
		if strings.Contains(note, "devices") {
			t.Errorf("a refused password link notified %q", note)
		}
	}

	// auth-ip: 20 per address, as for /auth/login.
	other := "192.0.2.200"
	for range 20 {
		wantError(t, f.call(http.MethodPost, "/api/v1/device/password", `{}`, goClient, from(other)),
			http.StatusUnprocessableEntity, api.CodeValidationFailed)
	}
	wantError(t, f.passwordLink("Alex", testPassword, "PC", other), http.StatusTooManyRequests, api.CodeRateLimited)
	if got := f.deviceList(cookie); len(got) != 1 {
		t.Errorf("refused password links made devices: %+v", got)
	}
}

// TestDevicePasswordHashBudget: #48 shares login's hash budget, so an empty budget is 503 server_busy.
func TestDevicePasswordHashBudget(t *testing.T) {
	f := newAuthFixture(t, func(o *auth.Options) { o.Hashes = auth.HashBudget{Burst: 1, PerSecond: 1} })
	f.setupAdmin("Alex") // the budget's one hash (the clock stands still)
	rec := f.passwordLink("Alex", testPassword, "PC", addrA)
	if e := wantError(t, rec, http.StatusServiceUnavailable, api.CodeServerBusy); e.RetryAfter != 1 ||
		rec.Header().Get("Retry-After") != "1" {
		t.Errorf("503: retryAfter %d, Retry-After %q", e.RetryAfter, rec.Header().Get("Retry-After"))
	}
	f.clk.advance(time.Second)
	decodeBody[api.DeviceTokenResponse](t, f.passwordLink("Alex", testPassword, "PC", addrA), http.StatusOK)
}

// TestDeviceRevokeOverREST is #49 (M2 02 §7.2): the app's own sign-out takes a bearer alone; afterwards the device,
// its connections and its tokens are gone and the owner's SPAs know.
func TestDeviceRevokeOverREST(t *testing.T) {
	f := newAuthFixture(t)
	admin, cookie := f.setupAdmin("Alex")
	app := f.linkApp(cookie, "PC", addrA)
	f.signal.takeNotes()
	f.conns.take()

	wantForbiddenFor(t, f.call(http.MethodPost, "/api/v1/device/revoke", `{}`, with(cookie)), api.CredentialKindDevice)
	wantError(t, f.call(http.MethodPost, "/api/v1/device/revoke", `{}`), http.StatusUnauthorized, api.CodeUnauthenticated)
	wantInvalidToken(t, f.call(http.MethodPost, "/api/v1/device/revoke", `{}`, withBearer(unknownAccessToken)))
	wantError(t, f.call(http.MethodPost, "/api/v1/device/revoke", `{}`, withBearer(app.AccessToken),
		header("Content-Type", "text/plain")), http.StatusUnsupportedMediaType, api.CodeUnsupportedMediaType)
	if got := f.deviceList(cookie); len(got) != 1 || len(f.conns.take()) != 0 {
		t.Fatalf("refused sign-outs changed something: devices %+v", got)
	}
	f.wantNotes("refused")

	wantNoContent(t, f.call(http.MethodPost, "/api/v1/device/revoke", `{}`, withBearer(app.AccessToken), goClient))
	if got := f.conns.take(); len(got) != 1 || got[0].sel != (auth.ConnSelector{UserID: store.UserID(admin.ID),
		DeviceID: store.DeviceID(app.Device.ID)}) || got[0].reason != auth.ReasonDeviceRevoked {
		t.Errorf("the sign-out closed %+v", got)
	}
	f.wantNotes("sign-out", devicesNote(admin.ID))
	if rows := f.auditOf("device.revoked"); len(rows) != 1 || fmt.Sprint(rows[0].Detail) != "map[via:app]" {
		t.Errorf("device.revoked rows %+v", rows)
	}
	wantInvalidToken(t, f.call(http.MethodGet, "/api/v1/me", "", withBearer(app.AccessToken)))
	wantError(t, f.refresh(app.RefreshToken, "", addrA), http.StatusBadRequest, api.CodeInvalidGrant)
	if got := f.deviceList(cookie); len(got) != 0 {
		t.Errorf("devices after the sign-out: %+v", got)
	}
	f.wantSignedIn(cookie) // the browser stays
}

// TestRenameDeviceOverREST is #52 (M2 02 §7.2): the browser and the app rename the user's devices; current marks the
// app's own; the same name writes nothing; another user's device is 404 and a bad name 422 {name}.
func TestRenameDeviceOverREST(t *testing.T) {
	f := newAuthFixture(t)
	admin, cookie := f.setupAdmin("Alex")
	app := f.linkApp(cookie, "PC", addrA)
	laptop := f.linkApp(cookie, "Laptop", addrB)
	_, samCookie := f.newMember(cookie, "Sam", addrC)
	samApp := f.linkApp(samCookie, "Sam-PC", addrC)
	f.signal.takeNotes()
	renamed := func(id, name string, opts ...reqOpt) api.DeviceInfo {
		t.Helper()
		rec := f.call(http.MethodPatch, "/api/v1/me/devices/"+id, jsonBody(t, api.RenameDeviceRequest{Name: name}), opts...)
		return decodeBody[api.DeviceResponse](t, rec, http.StatusOK).Device
	}

	got := renamed(app.Device.ID, "Gaming PC", with(cookie))
	if got.ID != app.Device.ID || got.Name != "Gaming PC" || got.Current || got.ClientKind != "desktop" || got.OS != "windows" ||
		got.AppVersion != appVersion || got.CreatedAt.IsZero() || got.LastIP != addrA {
		t.Errorf("the browser's rename answered %+v", got)
	}
	f.wantNotes("rename", devicesNote(admin.ID))
	if rows := f.auditOf("device.renamed"); len(rows) != 1 || fmt.Sprint(rows[0].Detail) != "map[from:PC to:Gaming PC]" {
		t.Errorf("device.renamed rows %+v", rows)
	}
	if got := renamed(app.Device.ID, " Main PC ", withBearer(app.AccessToken)); got.Name != "Main PC" || !got.Current {
		t.Errorf("the app's own rename answered %+v", got)
	}
	if got := renamed(laptop.Device.ID, "Old laptop", withBearer(app.AccessToken)); got.Name != "Old laptop" || got.Current {
		t.Errorf("the app's rename of another device answered %+v", got)
	}
	// The bearer's principal carries the new name at once: auth forgets the renamed device.
	if me := decodeBody[api.Me](t, f.call(http.MethodGet, "/api/v1/me", "", withBearer(app.AccessToken)), http.StatusOK); me.Device == nil || me.Device.Name != "Main PC" {
		t.Errorf("GET /me after the rename: %+v", me.Device)
	}
	rows := len(f.auditOf("device.renamed"))
	renamed(app.Device.ID, "Main PC", with(cookie))
	if n := len(f.auditOf("device.renamed")); n != rows {
		t.Errorf("an unchanged name wrote %d device.renamed rows", n-rows)
	}
	f.signal.takeNotes()

	for _, rq := range []struct {
		name, id string
		cred     reqOpt
	}{
		{"another user's device", samApp.Device.ID, with(cookie)},
		{"another user's device, from a device", samApp.Device.ID, withBearer(app.AccessToken)},
		{"an unknown device", "nope", with(cookie)},
	} {
		rec := f.call(http.MethodPatch, "/api/v1/me/devices/"+rq.id, `{"name":"Mine"}`, rq.cred)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", rq.name, rec.Code)
		}
		wantError(t, rec, http.StatusNotFound, api.CodeNotFound)
	}
	for in, code := range map[string]string{`{"name":""}`: api.FieldRequired, `{"name":"   "}`: api.FieldRequired,
		`{"name":"` + strings.Repeat("x", 65) + `"}`: api.FieldTooLong, `{}`: api.FieldRequired} {
		e := wantError(t, f.call(http.MethodPatch, "/api/v1/me/devices/"+app.Device.ID, in, with(cookie)),
			http.StatusUnprocessableEntity, api.CodeValidationFailed)
		if len(e.Fields) != 1 || e.Fields["name"] != code {
			t.Errorf("rename to %s: fields %v, want {name: %s}", in, e.Fields, code)
		}
	}
	wantInvalidToken(t, f.call(http.MethodPatch, "/api/v1/me/devices/"+app.Device.ID, `{"name":"X"}`,
		withBearer(unknownAccessToken)))
	f.wantNotes("refused renames")
	if got := decodeBody[api.DevicesResponse](t, f.call(http.MethodGet, "/api/v1/me/devices", "", with(samCookie)),
		http.StatusOK).Devices; len(got) != 1 || got[0].Name != "Sam-PC" {
		t.Errorf("Sam's devices after the refused renames: %+v", got)
	}
}

// TestMeForDevice: GET /me, /me/devices and /me/sessions for a bearer (M2 02 §6.4, §7.2), DELETE /me/devices/{id}
// from a device (another one, then its own) and POST /auth/logout-everywhere from a device.
func TestMeForDevice(t *testing.T) {
	f := newAuthFixture(t)
	admin, cookie := f.setupAdmin("Alex")
	app := f.linkApp(cookie, "PC", addrA)
	laptop := f.linkApp(cookie, "Laptop", addrB)

	rec := f.call(http.MethodGet, "/api/v1/me", "", withBearer(app.AccessToken))
	me := decodeBody[api.Me](t, rec, http.StatusOK)
	if me.Session != nil || me.Device == nil || me.Device.ID != app.Device.ID || me.Device.Name != "PC" ||
		!me.Device.Current || me.Device.ClientKind != "desktop" || me.Device.OS != "windows" ||
		me.Device.AppVersion != appVersion || me.Device.CreatedAt.IsZero() || me.User.ID != admin.ID ||
		!me.Permissions.Admin || !me.Permissions.CreateInvites || me.Badges == nil {
		t.Errorf("GET /me with a bearer = %+v (device %+v)", me, me.Device)
	}
	if got := fmt.Sprint(jsonKeys(t, rec.Body.Bytes(), "device")); got != "[appVersion clientKind createdAt current id name os]" {
		t.Errorf("GET /me's device has the keys %s", got)
	}
	if got := fmt.Sprint(jsonKeys(t, rec.Body.Bytes())); got != "[badges device permissions user]" {
		t.Errorf("GET /me with a bearer has the keys %s", got)
	}
	if sc := rec.Header().Values("Set-Cookie"); len(sc) != 0 {
		t.Errorf("GET /me with a bearer set %q", sc)
	}
	if me := decodeBody[api.Me](t, f.call(http.MethodGet, "/api/v1/me", "", with(cookie)), http.StatusOK); me.Device != nil || me.Session == nil {
		t.Errorf("GET /me with the cookie = %+v", me)
	}

	current := func(devices []api.DeviceInfo) []string {
		var out []string
		for _, d := range devices {
			if d.Current {
				out = append(out, d.ID)
			}
		}
		return out
	}
	byApp := decodeBody[api.DevicesResponse](t, f.call(http.MethodGet, "/api/v1/me/devices", "",
		withBearer(app.AccessToken)), http.StatusOK).Devices
	if len(byApp) != 2 || !slices.Equal(current(byApp), []string{app.Device.ID}) {
		t.Errorf("GET /me/devices with the app's bearer: %+v", byApp)
	}
	if byCookie := f.deviceList(cookie); len(byCookie) != 2 || len(current(byCookie)) != 0 {
		t.Errorf("GET /me/devices with the cookie: %+v", byCookie)
	}
	sessions := decodeBody[api.SessionsResponse](t, f.call(http.MethodGet, "/api/v1/me/sessions", "",
		withBearer(app.AccessToken)), http.StatusOK).Sessions
	if len(sessions) != 1 || sessions[0].Current || sessions[0].ID != string(f.sessionID(cookie)) {
		t.Errorf("GET /me/sessions with a bearer: %+v", sessions)
	}

	// The app removes another device of its user (as on the Devices page), then itself (its sign-out).
	f.signal.takeNotes()
	wantNoContent(t, f.call(http.MethodDelete, "/api/v1/me/devices/"+laptop.Device.ID, "", withBearer(app.AccessToken)))
	wantNoContent(t, f.call(http.MethodDelete, "/api/v1/me/devices/"+app.Device.ID, "", withBearer(app.AccessToken)))
	var vias []string
	for _, row := range f.auditOf("device.revoked") {
		vias = append(vias, fmt.Sprint(row.Detail))
	}
	if !slices.Equal(vias, []string{"map[via:devices_page]", "map[via:app]"}) {
		t.Errorf("device.revoked rows: %q", vias)
	}
	f.wantNotes("removals", devicesNote(admin.ID), devicesNote(admin.ID))
	wantInvalidToken(t, f.call(http.MethodGet, "/api/v1/me", "", withBearer(app.AccessToken)))

	// Log out everywhere from a device: everything goes, the app and the browser too, and no cookie is cleared.
	third := f.linkApp(cookie, "Third", addrC)
	rec = f.call(http.MethodPost, "/api/v1/auth/logout-everywhere", `{}`, withBearer(third.AccessToken))
	wantNoContent(t, rec)
	if sc := rec.Header().Values("Set-Cookie"); len(sc) != 0 {
		t.Errorf("logout-everywhere from a device set %q", sc)
	}
	f.wantUnauthenticated(cookie)
	wantInvalidToken(t, f.call(http.MethodGet, "/api/v1/me", "", withBearer(third.AccessToken)))
}

// TestDeviceCallerGone: a request the chain let in on a bearer whose device has gone since (auth's cache may be 30 s
// old) is 401, and changes nothing: the lists and the changes check the device again.
func TestDeviceCallerGone(t *testing.T) {
	f := newAuthFixture(t)
	_, cookie := f.setupAdmin("Alex")
	app := f.linkApp(cookie, "PC", addrA)
	other := f.linkApp(cookie, "Laptop", addrB)
	// The token is in auth's cache now; the row goes behind its back, as a commit that raced the request would.
	decodeBody[api.Me](t, f.call(http.MethodGet, "/api/v1/me", "", withBearer(app.AccessToken)), http.StatusOK)
	if _, err := f.rawDB().ExecContext(context.Background(), `DELETE FROM devices WHERE id = ?`, app.Device.ID); err != nil {
		t.Fatal(err)
	}
	for _, rq := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/me", ``},
		{http.MethodGet, "/api/v1/me/devices", ``},
		{http.MethodGet, "/api/v1/me/sessions", ``},
		{http.MethodPut, "/api/v1/push/preferences", `{"shareStarted":"off","adminAlerts":false}`},
		{http.MethodPatch, "/api/v1/me/devices/" + other.Device.ID, `{"name":"Mine"}`},
		{http.MethodDelete, "/api/v1/me/devices/" + other.Device.ID, ``},
		{http.MethodPost, "/api/v1/device/revoke", `{}`},
	} {
		rec := f.call(rq.method, rq.path, rq.body, withBearer(app.AccessToken))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s from a device that went: status %d, want 401 (body %q)", rq.method, rq.path, rec.Code, rec.Body)
		}
	}
	if got := f.deviceList(cookie); len(got) != 1 || got[0].Name != "Laptop" {
		t.Errorf("devices: %+v", got)
	}
	prefs := decodeBody[api.PushPreferences](t, f.call(http.MethodGet, "/api/v1/push/preferences", "", with(cookie)), http.StatusOK)
	if prefs.ShareStarted != api.ShareStartedPrefAll {
		t.Errorf("preferences changed: %+v", prefs)
	}
}
