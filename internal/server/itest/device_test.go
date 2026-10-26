package itest

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server"
	"github.com/MoonWX/isshoni/internal/server/servertest"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

// The device-auth integration cases of docs/m2/02-device-auth.md §16.2 on servertest with the real wiring (README
// WA56): cases 1–3 and 5 with raw HTTP in place of deviceauth.Manager (WA64 brings the manager and cases 4 and 6–8),
// and cases 9–12, raw HTTP only. TestDeviceOrigins and TestDeviceSockets add what README WA56 asks of the real
// wiring: the desktop origins on REST and on the hub, and the pre-auth refund. Every test checks that it leaves no
// goroutine behind (case 8's goleak, over the whole file); TestMain checks the package once more.

// revokeWithin is how soon a device's sockets close after its revocation committed (README WA56).
const revokeWithin = 100 * time.Millisecond

// TestDeviceLink is cases 1–3: the device flow with raw HTTP and the server's clock hook, the device's two signaling
// connections as one participant with its name on user.connections, and their close when the device is removed on
// the Devices page.
func TestDeviceLink(t *testing.T) {
	noLeaks(t)
	clock := &testClock{}
	w := newWorld(t, servertest.Options{Deps: server.Deps{Now: clock.now}, Config: withMetrics})
	alice := w.member("alice")
	cookie := sessionCookie(t, w.srv, alice.http.Jar)
	const home = "203.0.113.20" // the app and the browser share one address: network "same"

	// Case 1. The app starts the flow and polls: pending.
	var code api.DeviceCodeResponse
	rest(t, w.srv, restCall{method: http.MethodPost, path: "/api/v1/device/code", body: appCodeRequest("DESKTOP-TEST"),
		from: home}).decodeAs(t, "the app's start", http.StatusOK, &code)
	if code.Interval != 5 || code.ExpiresIn != 600 || !strings.HasPrefix(code.VerificationURIComplete, w.srv.URL+"/link?code=") {
		t.Fatalf("the start answered interval %d, expiresIn %d, a complete URI on %q", code.Interval, code.ExpiresIn,
			strings.SplitN(code.VerificationURIComplete, "?", 2)[0])
	}
	poll := func() restAnswer {
		return rest(t, w.srv, restCall{method: http.MethodPost, path: "/api/v1/device/token", from: home,
			body: api.DeviceTokenRequest{GrantType: api.GrantTypeDeviceCode, DeviceCode: code.DeviceCode}})
	}
	if e := poll().apiError(t, "the first poll", http.StatusBadRequest); e.Code != api.CodeAuthorizationPending {
		t.Fatalf("the first poll = %s, want authorization_pending", e.Code)
	}
	// The browser plays its part: lookup, then decide with a new name.
	var lookup api.DeviceLookup
	rest(t, w.srv, restCall{method: http.MethodPost, path: "/api/v1/device/lookup", cookie: cookie, from: home,
		body: api.DeviceLookupRequest{UserCode: code.UserCode}}).decodeAs(t, "the lookup", http.StatusOK, &lookup)
	if lookup.Network != api.NetworkMatchSame || lookup.UserCode != code.UserCode || lookup.DeviceName != "DESKTOP-TEST" ||
		lookup.ClientKind != "desktop" || lookup.OS != "windows" || lookup.AppVersion != deviceAppVersion || lookup.RequestIP != home {
		t.Fatalf("the lookup = network %q, kind %q, os %q, version %q, request IP %q; want same, desktop, windows, %s, %s",
			lookup.Network, lookup.ClientKind, lookup.OS, lookup.AppVersion, lookup.RequestIP, deviceAppVersion, home)
	}
	name := "Test PC"
	rest(t, w.srv, restCall{method: http.MethodPost, path: "/api/v1/device/decide", cookie: cookie, from: home,
		body: api.DeviceDecideRequest{UserCode: code.UserCode, Approve: true, DeviceName: &name}}).
		expect(t, "the approval", http.StatusNoContent)
	// The next poll, one interval later on the server's clock, gets the pair.
	clock.advance(time.Duration(code.Interval) * time.Second)
	var pair api.DeviceTokenResponse
	poll().decodeAs(t, "the poll after the approval", http.StatusOK, &pair)
	checkPair(t, "the poll after the approval", pair)
	if pair.Device.Name != name || pair.User.Username != "alice" {
		t.Fatalf("the pair is for device %q of %q, want %q of alice", pair.Device.Name, pair.User.Username, name)
	}
	if e := poll().apiError(t, "a poll after the pair", http.StatusBadRequest); e.Code != api.CodeInvalidGrant {
		t.Errorf("a poll of the consumed code = %s, want invalid_grant", e.Code)
	}
	list := devices(t, w.srv, cookie)
	if len(list) != 1 || list[0].ID != pair.Device.ID || list[0].Name != name || list[0].ClientKind != "desktop" ||
		list[0].OS != "windows" || list[0].AppVersion != deviceAppVersion || list[0].Current {
		t.Fatalf("GET /me/devices = %+v, want the one device %s named %q, not current for a cookie", list, pair.Device.ID, name)
	}
	access := protocol.Secret(pair.AccessToken)

	// Case 2. The app's Go connection (publisher, no Origin) and its SPA (viewer, the webview's origin), both on the
	// device's token, join the Lounge: one participant with two connections, and user.connections names the device
	// on both (01 C08, WA27, has merged).
	app := connectDevice(t, w.srv, access, "alice's app", deviceDial{role: protocol.RolePublisher}, "lounge")
	spa := connectDevice(t, w.srv, access, "alice's app's SPA",
		deviceDial{role: protocol.RoleViewer, origin: protocol.DesktopAssetOriginWindows}, "lounge")
	for _, d := range []*deviceConn{app, spa} {
		if u := d.sig.Welcome().User; u.ID != pair.User.ID || u.Name != "alice" || u.Admin {
			t.Errorf("%s: welcome user %+v, want alice", d.name, u)
		}
	}
	both := []string{app.connID(), spa.connID()}
	slices.Sort(both)
	for _, d := range []*deviceConn{app, spa} {
		eventually(t, func() bool {
			st := d.roomState()
			return len(st.Participants) == 1 && st.Participants[0].UserID == pair.User.ID &&
				slices.Equal(connIDs(st.Participants[0]), both)
		}, func() string {
			return fmt.Sprintf("%s: room.state participants %+v, want alice once with connections %q", d.name,
				d.roomState().Participants, both)
		})
	}
	wantConns := map[string]protocol.Role{app.connID(): protocol.RolePublisher, spa.connID(): protocol.RoleViewer}
	for _, d := range []*deviceConn{app, spa} {
		eventually(t, func() bool {
			list := d.userConns()
			if len(list) != 2 {
				return false
			}
			for _, c := range list {
				if wantConns[c.ID] != c.Role || c.Kind != protocol.ClientKindDesktop || c.DeviceName != name ||
					c.OS != protocol.ClientOSWindows || c.Version != deviceAppVersion {
					return false
				}
			}
			return true
		}, func() string {
			return fmt.Sprintf("%s: user.connections %+v, want both desktop connections named %q", d.name, d.userConns(), name)
		})
	}

	// Case 3. The browser removes the device on the Devices page: both sockets get error{session_revoked} and close
	// with 4401 within 100 ms of the answer; the refresh token is gone (the app would sign out: signed_out), and so
	// is the access token.
	closes := func() float64 { return scrape(t, w)[`isshoni_ws_close_total{code="4401"}`] }
	before := closes()
	rest(t, w.srv, restCall{method: http.MethodDelete, path: "/api/v1/me/devices/" + pair.Device.ID, cookie: cookie}).
		expect(t, "removing the device", http.StatusNoContent)
	answered := time.Now()
	for _, d := range []*deviceConn{app, spa} {
		sc, at := d.waitStopped()
		if sc.Err == nil || sc.Err.Code != protocol.ErrorCodeSessionRevoked || sc.Err.Scope != protocol.ErrorScopeSession {
			t.Errorf("%s stopped with %+v, want error session_revoked in scope session", d.name, sc.Err)
		}
		if took := at.Sub(answered); took > revokeWithin {
			t.Errorf("%s stopped %v after the removal was answered, want at most %v", d.name, took, revokeWithin)
		}
	}
	eventually(t, func() bool { return closes() == before+2 },
		func() string {
			return fmt.Sprintf("isshoni_ws_close_total{code=4401} = %v, want %v", closes(), before+2)
		})
	e := refreshDevice(t, w.srv, protocol.Secret(pair.RefreshToken), newExchangeID(t)).
		apiError(t, "the app's refresh after the removal", http.StatusBadRequest)
	if e.Code != api.CodeInvalidGrant || e.Params[api.ParamReason] != nil {
		t.Errorf("the refresh after the removal = %s %v, want invalid_grant without a reason (an unknown token)", e.Code, e.Params)
	}
	rest(t, w.srv, restCall{method: http.MethodGet, path: "/api/v1/me", bearer: access}).
		wantInvalidToken(t, "GET /me with the removed device's token")
	if _, err := dialDevice(t, w.srv, access, "alice's app, again", deviceDial{role: protocol.RolePublisher}); !isProtocolError(err, protocol.ErrorCodeUnauthenticated) {
		t.Errorf("a hello with the removed device's token: %v, want unauthenticated", err)
	}
	if list := devices(t, w.srv, cookie); len(list) != 0 {
		t.Errorf("GET /me/devices after the removal = %+v, want none", list)
	}
}

// TestDeviceClientOutdated is case 5: an admin sets minClientVersion above the app's version, and the app's next dial
// gets client_outdated, its SPA's too (client.kind is desktop for both, M2 02 §6.3 item 5); a newer app passes, and
// the web client is never checked.
func TestDeviceClientOutdated(t *testing.T) {
	noLeaks(t)
	w := newWorld(t, servertest.Options{})
	alice := w.member("alice")
	aliceCookie := sessionCookie(t, w.srv, alice.http.Jar)
	dev := linkDevice(t, w.srv, aliceCookie, "Test PC")
	connectDevice(t, w.srv, dev.Access, "alice's app before the floor", deviceDial{role: protocol.RolePublisher}, "")

	admin := sessionCookie(t, w.srv, w.srv.Client.Jar)
	var settings api.SettingsResponse
	rest(t, w.srv, restCall{method: http.MethodPatch, path: "/api/v1/admin/settings", cookie: admin,
		body: map[string]string{"minClientVersion": "0.3.0"}}).decodeAs(t, "the admin's new floor", http.StatusOK, &settings)
	if settings.Settings.MinClientVersion != "0.3.0" {
		t.Fatalf("the settings say minClientVersion %q, want 0.3.0", settings.Settings.MinClientVersion)
	}

	for _, o := range []deviceDial{
		{role: protocol.RolePublisher},
		{role: protocol.RoleViewer, origin: protocol.DesktopAssetOriginWindows},
	} {
		_, err := dialDevice(t, w.srv, dev.Access, "alice's app below the floor", o)
		if !isProtocolError(err, protocol.ErrorCodeClientOutdated) {
			t.Errorf("a %s hello of version %s below the floor: %v, want client_outdated", o.role, deviceAppVersion, err)
		}
	}
	newer := connectDevice(t, w.srv, dev.Access, "alice's updated app",
		deviceDial{role: protocol.RolePublisher, version: "0.3.0"}, "")
	if got := newer.sig.Welcome().MinClientVersion; got != "0.3.0" {
		t.Errorf("the updated app's welcome has minClientVersion %q, want 0.3.0", got)
	}
	// The web client is the server's own build and is never checked (01 §6.1): alice's browser tab still connects.
	tab, err := signaltest.Dial(waitCtx(t), &http.Client{Transport: w.srv.Client.Transport}, w.srv.WSURL,
		signaltest.DialOptions{Header: http.Header{"Origin": {w.srv.URL}, "Cookie": {aliceCookie}}})
	if err != nil {
		t.Fatalf("alice's browser tab: %v", err)
	}
	t.Cleanup(tab.Close)
	if _, err := tab.Hello(waitCtx(t), signaltest.DefaultHello()); err != nil {
		t.Errorf("a web hello of version 0.1.0 below the floor: %v, want a welcome", err)
	}
}

// TestDeviceGraceThenSupersededChild is case 9: a refresh P → C with an exchangeId; at +100 s on the server's clock
// P is replayed without one, which takes the restart grace path and supersedes C; when C is then presented, the
// device is revoked as for any reuse: invalid_grant {reason: reused}, the device gone, device.refresh_reused in the
// audit log, the refresh_token_reused admin alert, and every token of the device refused.
func TestDeviceGraceThenSupersededChild(t *testing.T) {
	noLeaks(t)
	clock := &testClock{}
	w := newWorld(t, servertest.Options{Deps: server.Deps{Now: clock.now}})
	alice := w.member("alice")
	cookie := sessionCookie(t, w.srv, alice.http.Jar)
	dev := linkDevice(t, w.srv, cookie, "Test PC")

	var c api.DeviceTokenResponse
	refreshDevice(t, w.srv, dev.Refresh, newExchangeID(t)).decodeAs(t, "the refresh P → C", http.StatusOK, &c)
	checkPair(t, "the refresh P → C", c)

	clock.advance(100 * time.Second)
	var g api.DeviceTokenResponse
	refreshDevice(t, w.srv, dev.Refresh, "").decodeAs(t, "P replayed at +100 s", http.StatusOK, &g)
	checkPair(t, "P replayed at +100 s", g)
	if g.RefreshToken == c.RefreshToken || g.AccessToken == c.AccessToken || g.Device.ID != dev.DeviceID {
		t.Fatalf("the grace path answered C's pair again or another device (%q, want %q)", g.Device.ID, dev.DeviceID)
	}

	e := refreshDevice(t, w.srv, protocol.Secret(c.RefreshToken), "").apiError(t, "C presented", http.StatusBadRequest)
	if e.Code != api.CodeInvalidGrant || e.Params[api.ParamReason] != api.InvalidGrantReused {
		t.Fatalf("C presented = %s %v, want invalid_grant {reason: reused}", e.Code, e.Params)
	}
	if list := devices(t, w.srv, cookie); len(list) != 0 {
		t.Errorf("GET /me/devices after the reuse = %+v, want none", list)
	}
	var page api.AuditPage
	rest(t, w.srv, restCall{method: http.MethodGet, path: "/api/v1/admin/audit?action=device.refresh_reused",
		cookie: sessionCookie(t, w.srv, w.srv.Client.Jar)}).decodeAs(t, "the audit log", http.StatusOK, &page)
	if len(page.Entries) != 1 || page.Entries[0].Target == nil || page.Entries[0].Target.ID != dev.DeviceID {
		t.Errorf("the audit log has %d device.refresh_reused rows (%+v), want one for device %s", len(page.Entries),
			page.Entries, dev.DeviceID)
	}
	eventually(t, func() bool { return adminAlerts(t, w.srv.Logs(), "refresh_token_reused") == 1 },
		func() string {
			return fmt.Sprintf("the server's log has %d admin alert lines with kind refresh_token_reused, want 1",
				adminAlerts(t, w.srv.Logs(), "refresh_token_reused"))
		})
	for what, tok := range map[string]string{"the grace pair's": g.AccessToken, "C's": c.AccessToken,
		"the first": dev.Access.Reveal()} {
		rest(t, w.srv, restCall{method: http.MethodGet, path: "/api/v1/me", bearer: protocol.Secret(tok)}).
			wantInvalidToken(t, "GET /me with "+what+" access token")
	}
	if e := refreshDevice(t, w.srv, protocol.Secret(g.RefreshToken), "").apiError(t, "the grace pair's refresh token",
		http.StatusBadRequest); e.Code != api.CodeInvalidGrant {
		t.Errorf("the grace pair's refresh token = %s, want invalid_grant", e.Code)
	}
}

// adminAlerts counts the server's "admin alert" WARN lines of one kind (M1's alert helper, auth/alerts.go); the
// harness logs JSON.
func adminAlerts(t *testing.T, logs, kind string) int {
	t.Helper()
	n := 0
	for line := range strings.SplitSeq(logs, "\n") {
		if !strings.Contains(line, `"admin alert"`) {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("a log line that is no JSON: %v", err)
		}
		if rec["msg"] == "admin alert" && rec["level"] == "WARN" && rec["kind"] == kind {
			n++
		}
	}
	return n
}

// adminRoutes are every /api/v1/admin/* route of the real wiring: 03's #29–#42 (httpapi) and M1 04's dashboard
// (#43), doctor and bandwidth estimate (wire.go's wireOpsRoutes).
var adminRoutes = []struct{ method, path string }{
	{http.MethodGet, "/api/v1/admin/users"},                   // #29
	{http.MethodPatch, "/api/v1/admin/users/x"},               // #30
	{http.MethodDelete, "/api/v1/admin/users/x"},              // #31
	{http.MethodPost, "/api/v1/admin/users/x/password-reset"}, // #32
	{http.MethodPost, "/api/v1/admin/users/x/sign-out"},       // #33
	{http.MethodGet, "/api/v1/admin/approvals"},               // #34
	{http.MethodPost, "/api/v1/admin/approvals/x/approve"},    // #35
	{http.MethodPost, "/api/v1/admin/approvals/x/reject"},     // #36
	{http.MethodPost, "/api/v1/admin/rooms"},                  // #37
	{http.MethodPatch, "/api/v1/admin/rooms/x"},               // #38
	{http.MethodDelete, "/api/v1/admin/rooms/x"},              // #39
	{http.MethodGet, "/api/v1/admin/settings"},                // #40
	{http.MethodPatch, "/api/v1/admin/settings"},              // #41
	{http.MethodGet, "/api/v1/admin/audit"},                   // #42
	{http.MethodGet, "/api/v1/admin/dashboard"},               // #43
	{http.MethodGet, "/api/v1/admin/doctor"},                  // M1 04 §13.1
	{http.MethodPost, "/api/v1/admin/doctor"},                 // M1 04 §13.1
	{http.MethodGet, "/api/v1/admin/bandwidth"},               // M1 04 §11.6
}

// TestDeviceAdminBearer is case 10: the admin links a device; its bearer gets 403 forbidden {credential: session}
// on every admin route, and the settings stay as they were, while it may create an invite and its hello gets a
// welcome with user.admin, as a cookie's would (M2 02 §6.4).
func TestDeviceAdminBearer(t *testing.T) {
	noLeaks(t)
	w := newWorld(t, servertest.Options{})
	cookie := sessionCookie(t, w.srv, w.srv.Client.Jar) // the admin's browser from the setup
	dev := linkDevice(t, w.srv, cookie, "Admin PC")

	settings := func() api.Settings {
		var res api.SettingsResponse
		rest(t, w.srv, restCall{method: http.MethodGet, path: "/api/v1/admin/settings", cookie: cookie}).
			decodeAs(t, "the settings for the admin's cookie", http.StatusOK, &res)
		return res.Settings
	}
	before := settings()
	rest(t, w.srv, restCall{method: http.MethodPatch, path: "/api/v1/admin/settings", bearer: dev.Access,
		body: map[string]string{"serverName": "Taken over"}}).
		wantForbiddenFor(t, "PATCH /admin/settings with the admin's bearer", api.CredentialKindSession)
	if after := settings(); after != before {
		t.Errorf("the settings changed under a refused bearer: %+v, want %+v", after, before)
	}
	for _, r := range adminRoutes {
		call := restCall{method: r.method, path: r.path, bearer: dev.Access}
		if r.method != http.MethodGet && r.method != http.MethodDelete {
			call.body = map[string]any{}
		}
		rest(t, w.srv, call).wantForbiddenFor(t, "the admin's bearer", api.CredentialKindSession)
	}
	// A bad bearer is still 401: the token is checked before the route's credential.
	rest(t, w.srv, restCall{method: http.MethodPatch, path: "/api/v1/admin/settings",
		bearer: protocol.Secret("isa_" + strings.Repeat("A", 43)), body: map[string]any{}}).
		wantInvalidToken(t, "PATCH /admin/settings with an unknown bearer")

	var invite api.CreateInviteResponse
	rest(t, w.srv, restCall{method: http.MethodPost, path: "/api/v1/invites", bearer: dev.Access,
		body: api.CreateInviteRequest{}}).decodeAs(t, "POST /invites with the admin's bearer", http.StatusCreated, &invite)
	if invite.URL == "" {
		t.Error("the invite has no link")
	}

	d := connectDevice(t, w.srv, dev.Access, "the admin's app", deviceDial{role: protocol.RoleViewer}, "")
	if u := d.sig.Welcome().User; u.Name != adminName || !u.Admin {
		t.Errorf("the admin's bearer hello: welcome user %+v, want %s with admin: true", u, adminName)
	}
}

// TestDeviceCode48Bucket is case 11: POST /device/code from 60 different /64s inside one IPv6 /48 (through the
// harness's trusted proxy): the first 30 get 200, the rest 429 rate_limited; another /48 and an IPv4 client still
// get 200 (M2 02 §5.5).
func TestDeviceCode48Bucket(t *testing.T) {
	noLeaks(t)
	w := newWorld(t, servertest.Options{})
	start := func(from string) restAnswer {
		return rest(t, w.srv, restCall{method: http.MethodPost, path: "/api/v1/device/code",
			body: appCodeRequest("DESKTOP-TEST"), from: from})
	}
	for i := range 60 {
		from := fmt.Sprintf("2001:db8:aa:%x::1", i) // the /64 2001:db8:aa:i::/64 of 2001:db8:aa::/48
		res := start(from)
		if i < 30 {
			res.expect(t, "a start from "+from, http.StatusOK)
			continue
		}
		e := res.apiError(t, "a start from "+from, http.StatusTooManyRequests)
		if e.Code != api.CodeRateLimited || e.RetryAfter <= 0 || res.header.Get("Retry-After") == "" {
			t.Fatalf("a start from %s = %s, retryAfter %d, Retry-After %q; want rate_limited with a wait", from, e.Code,
				e.RetryAfter, res.header.Get("Retry-After"))
		}
	}
	start("2001:db8:bb::1").expect(t, "a start from another /48", http.StatusOK)
	start("203.0.113.9").expect(t, "a start over IPv4", http.StatusOK)
}

// TestDeviceLookupNetwork is case 12: the approval page's network comparison (M2 02 §5.3): a code started over IPv6
// and looked up over IPv4 is unknown, one address is same, two IPv4 addresses are different.
func TestDeviceLookupNetwork(t *testing.T) {
	noLeaks(t)
	w := newWorld(t, servertest.Options{})
	alice := w.member("alice")
	cookie := sessionCookie(t, w.srv, alice.http.Jar)
	for _, tc := range []struct {
		app, browser string
		want         api.NetworkMatch
	}{
		{"2001:db8:cc::7", "203.0.113.7", api.NetworkMatchUnknown},
		{"203.0.113.7", "203.0.113.7", api.NetworkMatchSame},
		{"203.0.113.7", "198.51.100.9", api.NetworkMatchDifferent},
	} {
		var code api.DeviceCodeResponse
		rest(t, w.srv, restCall{method: http.MethodPost, path: "/api/v1/device/code", body: appCodeRequest("DESKTOP-TEST"),
			from: tc.app}).decodeAs(t, "a start from "+tc.app, http.StatusOK, &code)
		var lookup api.DeviceLookup
		rest(t, w.srv, restCall{method: http.MethodPost, path: "/api/v1/device/lookup", cookie: cookie, from: tc.browser,
			body: api.DeviceLookupRequest{UserCode: code.UserCode}}).decodeAs(t, "a lookup from "+tc.browser, http.StatusOK, &lookup)
		if lookup.Network != tc.want || lookup.RequestIP != tc.app {
			t.Errorf("started from %s, looked up from %s: network %q, request IP %q; want %q and %s", tc.app, tc.browser,
				lookup.Network, lookup.RequestIP, tc.want, tc.app)
		}
	}
}

// TestDeviceOrigins: the wiring gives the site the desktop origins (M2 02 §6.3 item 1), and the REST API answers
// CORS for each of them (§6.4): the preflight of the app's SPA, and a bearer request's answer, never with
// Allow-Credentials. The hub's half is TestDeviceSockets.
func TestDeviceOrigins(t *testing.T) {
	noLeaks(t)
	w := newWorld(t, servertest.Options{})
	if got := w.srv.Srv.Site().ExtraOrigins; !slices.Equal(got, protocol.DesktopOrigins()) {
		t.Fatalf("the site's ExtraOrigins = %q, want protocol.DesktopOrigins() %q", got, protocol.DesktopOrigins())
	}
	dev := linkDevice(t, w.srv, sessionCookie(t, w.srv, w.srv.Client.Jar), "Admin PC")
	hc := &http.Client{Transport: w.srv.Client.Transport} // no cookie jar: the webview sends none
	// send returns the answer's status and headers; the bodies don't matter here.
	send := func(req *http.Request) (int, http.Header) {
		t.Helper()
		res, err := hc.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
		}
		defer func() { _ = res.Body.Close() }()
		return res.StatusCode, res.Header
	}
	for _, origin := range protocol.DesktopOrigins() {
		pre, err := http.NewRequestWithContext(waitCtx(t), http.MethodOptions, w.srv.URL+"/api/v1/me", nil)
		if err != nil {
			t.Fatal(err)
		}
		pre.Header.Set("Origin", origin)
		pre.Header.Set("Access-Control-Request-Method", http.MethodGet)
		pre.Header.Set("Access-Control-Request-Headers", "authorization")
		status, h := send(pre)
		if status != http.StatusNoContent || h.Get("Access-Control-Allow-Origin") != origin ||
			h.Get("Access-Control-Max-Age") != "600" || h.Get("Access-Control-Allow-Credentials") != "" {
			t.Errorf("a preflight from %s = %d with %v, want 204 allowing the origin for 600 s, without credentials",
				origin, status, h)
		}

		get, err := http.NewRequestWithContext(waitCtx(t), http.MethodGet, w.srv.URL+"/api/v1/me", nil)
		if err != nil {
			t.Fatal(err)
		}
		get.Header.Set("Origin", origin)
		get.Header.Set("Authorization", "Bearer "+dev.Access.Reveal())
		status, h = send(get)
		if status != http.StatusOK || h.Get("Access-Control-Allow-Origin") != origin ||
			h.Get("Access-Control-Allow-Credentials") != "" {
			t.Errorf("a bearer GET /me from %s = %d with Allow-Origin %q, Allow-Credentials %q; want 200 for the origin",
				origin, status, h.Get("Access-Control-Allow-Origin"), h.Get("Access-Control-Allow-Credentials"))
		}
	}
}

// TestDeviceSockets is the hub in the real wiring (README WA56, M2 02 §6.3): a socket from http://wails.localhost
// with hello.auth is accepted and with a cookie gets 403; 21 bearer sockets of two users from one address, one after
// another, all pass (the pre-auth refund), while 21 sockets whose hello fails get 429 at the 21st upgrade.
func TestDeviceSockets(t *testing.T) {
	noLeaks(t)
	w := newWorld(t, servertest.Options{})
	alice, bob := w.member("alice"), w.member("bob")
	aliceCookie := sessionCookie(t, w.srv, alice.http.Jar)
	tokens := []protocol.Secret{
		linkDevice(t, w.srv, aliceCookie, "Alice's PC").Access,
		linkDevice(t, w.srv, sessionCookie(t, w.srv, bob.http.Jar), "Bob's PC").Access,
	}
	if protocol.DesktopOrigins()[0] != "http://wails.localhost" {
		t.Fatalf("DesktopOrigins()[0] = %q, want http://wails.localhost", protocol.DesktopOrigins()[0])
	}
	hello := func(token protocol.Secret) protocol.Hello {
		h := signaltest.DefaultHello()
		h.Client = protocol.ClientInfo{Kind: protocol.ClientKindDesktop, Version: deviceAppVersion, OS: protocol.ClientOSWindows}
		h.Auth = &protocol.HelloAuth{Scheme: protocol.AuthSchemeBearer, Token: token}
		return h
	}
	dial := func(h http.Header) (*signaltest.Client, error) {
		c, err := signaltest.Dial(waitCtx(t), &http.Client{Transport: w.srv.Client.Transport}, w.srv.WSURL,
			signaltest.DialOptions{Header: h})
		if err == nil {
			t.Cleanup(c.Close)
		}
		return c, err
	}

	// The Windows app's SPA origin: accepted with hello.auth, 403 with alice's cookie.
	c, err := dial(http.Header{"Origin": {protocol.DesktopAssetOriginWindows}})
	if err != nil {
		t.Fatalf("an upgrade from %s: %v", protocol.DesktopAssetOriginWindows, err)
	}
	if wel, err := c.Hello(waitCtx(t), hello(tokens[0])); err != nil || wel.User.Name != "alice" {
		t.Fatalf("a bearer hello from %s: %+v, %v; want alice's welcome", protocol.DesktopAssetOriginWindows, wel.User, err)
	}
	c.Close()
	_, err = dial(http.Header{"Origin": {protocol.DesktopAssetOriginWindows}, "Cookie": {aliceCookie}})
	var re *signaltest.RefusedError
	if !errors.As(err, &re) || re.Status != http.StatusForbidden {
		t.Fatalf("an upgrade from %s with a cookie: %v, want 403", protocol.DesktopAssetOriginWindows, err)
	}

	// 21 bearer sockets of two users from one address, opened one after another (the apps of a household after a
	// server restart): each hello gives its pre-auth token back, so none is throttled. 11 and 10 connections stay
	// below the per-user cap of 16.
	const household = "203.0.113.50"
	for i := range 21 {
		h := http.Header{"X-Forwarded-For": {household}}
		if i%4 >= 2 {
			h.Set("Origin", protocol.DesktopAssetOriginWindows)
		}
		c, err := dial(h)
		if err != nil {
			t.Fatalf("bearer socket %d from one address: %v", i+1, err)
		}
		if _, err := c.Hello(waitCtx(t), hello(tokens[i%2])); err != nil {
			t.Fatalf("bearer socket %d from one address: hello: %v", i+1, err)
		}
	}

	// 20 sockets from another address whose hello fails keep their tokens: the 21st upgrade gets 429.
	const stranger = "198.51.100.200"
	for i := range 20 {
		c, err := dial(http.Header{"X-Forwarded-For": {stranger}, "Origin": {protocol.DesktopAssetOriginWindows}})
		if err != nil {
			t.Fatalf("pre-auth socket %d: %v", i+1, err)
		}
		_, err = c.Hello(waitCtx(t), hello(protocol.Secret("isa_"+strings.Repeat("A", 43))))
		if !isProtocolError(err, protocol.ErrorCodeUnauthenticated) {
			t.Fatalf("pre-auth socket %d with an unknown token: %v, want unauthenticated", i+1, err)
		}
	}
	_, err = dial(http.Header{"X-Forwarded-For": {stranger}})
	if !errors.As(err, &re) || re.Status != http.StatusTooManyRequests {
		t.Fatalf("the 21st upgrade after 20 failed hellos: %v, want 429", err)
	}
}

// isProtocolError reports whether err is, or wraps, a *protocol.Error with the code.
func isProtocolError(err error, code protocol.ErrorCode) bool {
	var pe *protocol.Error
	return errors.As(err, &pe) && pe.Code == code
}
