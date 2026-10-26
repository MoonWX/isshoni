package httpapi

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// Helpers of the device route tests (README WA47): the app's side of M2 02 §5, §6.2 and §14 as raw REST through the
// whole chain, with the real auth.Service behind it.

// appVersion is the version the test app reports.
const appVersion = "0.2.0"

// goClient makes a request look like the desktop app's Go client (M2 02 §11.2): no Origin, no Sec-Fetch-Site and its
// own User-Agent. The CSRF check lets such a request pass, as any non-browser client.
func goClient(r *http.Request) {
	r.Header.Del("Origin")
	r.Header.Del("Sec-Fetch-Site")
	r.Header.Set("User-Agent", "isshoni-desktop/"+appVersion)
}

// withBearer sends a device's access token in the Authorization header.
func withBearer(token string) reqOpt { return header("Authorization", "Bearer "+token) }

// unknownAccessToken has an access token's shape (M2 02 §3), but nobody issued it.
var unknownAccessToken = "isa_" + strings.Repeat("A", 43)

// accessTokenShape and refreshTokenShape are the shapes of M2 02 §3.
var (
	accessTokenShape  = regexp.MustCompile(`^isa_[A-Za-z0-9_-]{43}$`)
	refreshTokenShape = regexp.MustCompile(`^isr_[A-Za-z0-9_-]{43}$`)
)

// appRequest is the start request of the test app, named name.
func appRequest(name string) api.DeviceCodeRequest {
	return api.DeviceCodeRequest{ClientKind: "desktop", DeviceName: name, OS: "windows", AppVersion: appVersion}
}

// startLink is the app's POST /api/v1/device/code from ip.
func (f *authFixture) startLink(name, ip string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.call(http.MethodPost, "/api/v1/device/code", jsonBody(f.t, appRequest(name)), goClient, from(ip))
}

// startedLink starts a link from ip and returns its codes.
func (f *authFixture) startedLink(name, ip string) api.DeviceCodeResponse {
	f.t.Helper()
	return decodeBody[api.DeviceCodeResponse](f.t, f.startLink(name, ip), http.StatusOK)
}

// lookupCode is the approval page's POST /api/v1/device/lookup with the browser's cookie.
func (f *authFixture) lookupCode(c *http.Cookie, userCode string, opts ...reqOpt) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.call(http.MethodPost, "/api/v1/device/lookup", jsonBody(f.t, api.DeviceLookupRequest{UserCode: userCode}),
		append([]reqOpt{with(c)}, opts...)...)
}

// decideCode is the approval page's POST /api/v1/device/decide with the browser's cookie, without a new name.
func (f *authFixture) decideCode(c *http.Cookie, userCode string, approve bool, opts ...reqOpt) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.call(http.MethodPost, "/api/v1/device/decide",
		jsonBody(f.t, api.DeviceDecideRequest{UserCode: userCode, Approve: approve}), append([]reqOpt{with(c)}, opts...)...)
}

// pollCode is the app's poll, POST /api/v1/device/token with the device_code grant, from ip.
func (f *authFixture) pollCode(deviceCode, ip string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.call(http.MethodPost, "/api/v1/device/token", jsonBody(f.t, api.DeviceTokenRequest{
		GrantType: api.GrantTypeDeviceCode, DeviceCode: deviceCode}), goClient, from(ip))
}

// refresh is the app's POST /api/v1/device/token with the refresh_token grant, from ip ("" exchangeID: none).
func (f *authFixture) refresh(refreshToken, exchangeID, ip string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.call(http.MethodPost, "/api/v1/device/token", jsonBody(f.t, api.DeviceTokenRequest{
		GrantType: api.GrantTypeRefreshToken, RefreshToken: refreshToken, ExchangeID: exchangeID,
		AppVersion: appVersion}), goClient, from(ip))
}

// passwordLink is the app's POST /api/v1/device/password from ip.
func (f *authFixture) passwordLink(username, password, name, ip string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.call(http.MethodPost, "/api/v1/device/password", jsonBody(f.t, api.DevicePasswordRequest{
		Username: username, Password: password, ClientKind: "desktop", DeviceName: name, OS: "windows",
		AppVersion: appVersion}), goClient, from(ip))
}

// linkApp links an app named name for the user behind c through the device flow, every request from ip: start,
// approve, poll. It returns the first token pair.
func (f *authFixture) linkApp(c *http.Cookie, name, ip string) api.DeviceTokenResponse {
	f.t.Helper()
	code := f.startedLink(name, ip)
	wantNoContent(f.t, f.decideCode(c, code.UserCode, true, from(ip)))
	res := decodeBody[api.DeviceTokenResponse](f.t, f.pollCode(code.DeviceCode, ip), http.StatusOK)
	if !accessTokenShape.MatchString(res.AccessToken) || !refreshTokenShape.MatchString(res.RefreshToken) {
		f.t.Fatalf("a link answered tokens of the wrong shape (lengths %d and %d)", len(res.AccessToken),
			len(res.RefreshToken))
	}
	return res
}

// wantForbiddenFor checks a 403 forbidden whose params name the only credential the route takes.
func wantForbiddenFor(t *testing.T, rec *httptest.ResponseRecorder, kind api.CredentialKind) {
	t.Helper()
	e := wantError(t, rec, http.StatusForbidden, api.CodeForbidden)
	if len(e.Params) != 1 || e.Params[api.ParamCredential] != string(kind) {
		t.Fatalf("403 forbidden with params %v, want {credential: %s}", e.Params, kind)
	}
}

// wantInvalidToken checks a 401 invalid_token with its WWW-Authenticate header (M2 02 §6.1).
func wantInvalidToken(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	wantError(t, rec, http.StatusUnauthorized, api.CodeInvalidToken)
	if got := rec.Header().Values("WWW-Authenticate"); len(got) != 1 || got[0] != `Bearer error="invalid_token"` {
		t.Fatalf("401 invalid_token with WWW-Authenticate %q", got)
	}
}
