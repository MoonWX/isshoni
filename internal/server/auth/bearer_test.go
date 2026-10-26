package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// Device access tokens at the service (M2 02 §16.1's tokens row, the bearer half; README WA39): the header grammar
// and the token's shape refused before any read (a counting accessToken), the 30 s cache and the token's own expiry,
// the bearer principal with the device's current name, Touch for devices with its 5-minute last-seen write, and the
// cache forgetting a device before ConnCloser runs. The refresh_token grant is refresh_test.go, the device calls of
// the Devices page devices_test.go.

// ---- helpers ----

// linked is an app linked through the device flow, with the pair the flow issued.
type linked struct {
	user            store.User
	device          store.DeviceID
	name            string
	access, refresh string
}

// linkApp links an app for the user of res through the whole device flow, the app polling from appIP (svcEnv.link).
func (e *svcEnv) linkApp(res LoginResult, appIP string) linked {
	e.t.Helper()
	pair := e.link(res, appIP)
	return linked{user: res.User, device: store.DeviceID(pair.Device.ID), name: pair.Device.Name,
		access: pair.AccessToken, refresh: pair.RefreshToken}
}

// principal is the device's principal as AuthenticateBearerToken answers it while nothing has changed.
func (l linked) principal() Principal {
	return Principal{UserID: l.user.ID, Username: l.user.Username, Role: l.user.Role, Method: MethodBearer,
		DeviceID: l.device, ClientKind: "desktop", DeviceName: l.name}
}

// newLinkEnv is a service with Alex logged in from ipB and one app of Alex's linked from ipA.
func newLinkEnv(t *testing.T, mods ...func(*Options)) (*svcEnv, LoginResult, linked) {
	t.Helper()
	e := newSvcEnv(t, mods...)
	e.addUser("Alex")
	alex := e.login("Alex", ipB)
	return e, alex, e.linkApp(alex, ipA)
}

// bearerRequest is a GET to the test origin from ip that carries header as its Authorization header and no cookie.
func bearerRequest(header, ip string) *http.Request {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, testOrigin+"/api/v1/me", nil)
	r.RemoteAddr = netip.AddrPortFrom(netip.MustParseAddr(ip), 50000).String()
	r.Header.Set("Authorization", header)
	return r
}

// countAccessReads makes the service count its access-token reads (Service.accessToken) and returns the counter.
func (e *svcEnv) countAccessReads() *atomic.Int32 {
	var n atomic.Int32
	next := e.svc.accessToken
	e.svc.accessToken = func(ctx context.Context, h []byte, now time.Time) (store.DeviceToken, store.Device, store.User,
		error,
	) {
		n.Add(1)
		return next(ctx, h, now)
	}
	return &n
}

// deviceRow reads a device, or nil when it is gone.
func (e *svcEnv) deviceRow(id store.DeviceID) *store.Device {
	e.t.Helper()
	var out *store.Device
	e.read(func(q *store.Q) error {
		d, _, err := q.DeviceByID(id)
		if err == nil {
			out = &d
			return nil
		}
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	})
	return out
}

// ---- the header and the token's shape (M2 02 §3, §6.1) ----

// TestAuthenticateBearer: a request with one bearer access token is the device's principal, whatever the case of
// the scheme, and its cookies count for nothing either way.
func TestAuthenticateBearer(t *testing.T) {
	e, alex, app := newLinkEnv(t)
	want := app.principal()
	for _, scheme := range []string{"Bearer", "bearer", "BEARER", "bEaReR"} {
		p, err := e.svc.Authenticate(bearerRequest(scheme+" "+app.access, ipC))
		if err != nil || p != want {
			t.Errorf("Authenticate(%s …) = %+v, %v; want %+v", scheme, p, err, want)
		}
	}
	p, err := e.svc.AuthenticateBearerToken(context.Background(), app.access)
	if err != nil || p != want {
		t.Errorf("AuthenticateBearerToken = %+v, %v; want %+v", p, err, want)
	}

	// A valid cookie beside a bad bearer: invalid_token. A bad cookie beside a good bearer: the device.
	r := bearerRequest("Bearer isa_"+newToken(deviceTokenBytes), ipC)
	r.Header.Add("Cookie", e.svc.cookie.name+"="+alex.Token)
	_, err = e.svc.Authenticate(r)
	wantCode(t, err, api.CodeInvalidToken)
	r = bearerRequest("Bearer "+app.access, ipC)
	r.Header.Add("Cookie", e.svc.cookie.name+"=garbage")
	if p, err := e.svc.Authenticate(r); err != nil || p != want {
		t.Errorf("a good bearer beside a bad cookie = %+v, %v", p, err)
	}
	// The refresh token is no access token, and the web session's token is no device's.
	for _, tok := range []string{app.refresh, alex.Token, "isa_" + alex.Token} {
		_, err := e.svc.Authenticate(bearerRequest("Bearer "+tok, ipC))
		wantCode(t, err, api.CodeInvalidToken)
		_, err = e.svc.AuthenticateBearerToken(context.Background(), tok)
		wantCode(t, err, api.CodeInvalidToken)
	}
}

// TestBearerShapeWithoutARead: every Authorization header that is not exactly one bearer access token, and every
// token of the wrong shape, is invalid_token before any hashing or DB access: the counting read stays at 0, and so
// it does on a closed store.
func TestBearerShapeWithoutARead(t *testing.T) {
	e, _, app := newLinkEnv(t)
	reads := e.countAccessReads()
	tok := strings.TrimPrefix(app.access, accessTokenPrefix)
	badTokens := map[string]string{
		"empty":                 "",
		"no prefix":             tok,
		"refresh prefix":        refreshTokenPrefix + tok,
		"upper-case prefix":     "ISA_" + tok,
		"42 characters":         accessTokenPrefix + tok[:42],
		"44 characters":         accessTokenPrefix + tok + "A",
		"padding":               accessTokenPrefix + tok[:42] + "=",
		"not base64url":         accessTokenPrefix + strings.Repeat("*", 43),
		"standard base64":       accessTokenPrefix + strings.Repeat("+", 43),
		"non-canonical last":    accessTokenPrefix + tok[:42] + "B", // 43rd character with nonzero padding bits
		"trailing space":        app.access + " ",
		"a second token":        app.access + " " + app.access,
		"an exchange id's size": accessTokenPrefix + newToken(exchangeIDBytes),
	}
	if strings.HasSuffix(tok, "B") {
		badTokens["non-canonical last"] = accessTokenPrefix + tok[:42] + "C"
	}
	for name, bad := range badTokens {
		_, err := e.svc.AuthenticateBearerToken(context.Background(), bad)
		if !api.IsCode(err, api.CodeInvalidToken) {
			t.Errorf("AuthenticateBearerToken with %s: %v, want invalid_token", name, err)
		}
		_, err = e.svc.Authenticate(bearerRequest("Bearer "+bad, ipC))
		if !api.IsCode(err, api.CodeInvalidToken) {
			t.Errorf("Authenticate with the token %s: %v, want invalid_token", name, err)
		}
	}
	headers := map[string][]string{
		"another scheme":            {"Basic " + app.access},
		"Token scheme":              {"Token " + app.access},
		"no space":                  {"Bearer" + app.access},
		"two spaces":                {"Bearer  " + app.access},
		"a tab":                     {"Bearer\t" + app.access},
		"a leading space":           {" Bearer " + app.access},
		"the scheme alone":          {"Bearer"},
		"the scheme and a space":    {"Bearer "},
		"an empty header":           {""},
		"two headers":               {"Bearer " + app.access, "Bearer " + app.access},
		"two headers, one of them":  {"Bearer " + app.access, ""},
		"a comma-separated list":    {"Bearer " + app.access + ", Bearer " + app.access},
		"the token without scheme":  {app.access},
		"the refresh token instead": {"Bearer " + app.refresh},
	}
	for name, vals := range headers {
		r := bearerRequest("", ipC)
		r.Header["Authorization"] = vals
		if _, err := e.svc.Authenticate(r); !api.IsCode(err, api.CodeInvalidToken) {
			t.Errorf("Authorization %s: %v, want invalid_token", name, err)
		}
	}
	if n := reads.Load(); n != 0 {
		t.Errorf("%d access-token reads for malformed credentials, want 0", n)
	}
	if err := e.db.Close(); err != nil {
		t.Fatal(err)
	}
	for name, bad := range badTokens {
		if _, err := e.svc.AuthenticateBearerToken(context.Background(), bad); !api.IsCode(err, api.CodeInvalidToken) {
			t.Errorf("on a closed store, %s: %v, want invalid_token (no DB access)", name, err)
		}
	}
}

// ---- the cache and expiry (M2 02 §6.1) ----

// TestBearerCache: the first use of a token reads it, uses within 30 s don't, and the read after 30 s goes to the
// store again. An unknown token reads every time and is never cached. A store failure is internal, not
// invalid_token.
func TestBearerCache(t *testing.T) {
	e, _, app := newLinkEnv(t)
	reads := e.countAccessReads()
	ctx := context.Background()
	auth := func(when string, wantReads int32) {
		t.Helper()
		if _, err := e.svc.AuthenticateBearerToken(ctx, app.access); err != nil {
			t.Fatalf("%s: %v", when, err)
		}
		if n := reads.Load(); n != wantReads {
			t.Fatalf("%s: %d reads, want %d", when, n, wantReads)
		}
	}
	auth("first use", 1)
	e.advance(bearerCacheTTL - time.Millisecond)
	auth("29.999 s later", 1)
	if _, err := e.svc.Authenticate(bearerRequest("Bearer "+app.access, ipC)); err != nil {
		t.Fatal(err)
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("a REST request inside the 30 s read the token again (%d reads)", n)
	}
	e.advance(time.Millisecond)
	auth("at 30 s", 2)
	auth("right after", 2)

	unknown := accessTokenPrefix + newToken(deviceTokenBytes)
	for i := range 2 {
		_, err := e.svc.AuthenticateBearerToken(ctx, unknown)
		wantCode(t, err, api.CodeInvalidToken)
		if n := reads.Load(); n != int32(3+i) {
			t.Fatalf("unknown token, try %d: %d reads, want %d (never cached)", i, n, 3+i)
		}
	}

	// The cached token keeps working while the store is down; a token the cache does not hold is internal.
	if err := e.db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AuthenticateBearerToken(ctx, app.access); err != nil {
		t.Errorf("a cached token on a closed store: %v", err)
	}
	if _, err := e.svc.AuthenticateBearerToken(ctx, unknown); !api.IsCode(err, api.CodeInternal) {
		t.Errorf("an uncached token on a closed store: %v, want internal", err)
	}
}

// TestBearerExpiry: an access token works until its 900 s are up and not at 900 s. A cached entry never outlives
// the token (min(30 s, expiry − now)), and the cache refuses a token it knows to have expired without a read; once
// the entry is gone, the store refuses it.
func TestBearerExpiry(t *testing.T) {
	e, _, app := newLinkEnv(t)
	reads := e.countAccessReads()
	ctx := context.Background()
	e.advance(accessTokenTTL - 20*time.Second)
	if _, err := e.svc.AuthenticateBearerToken(ctx, app.access); err != nil {
		t.Fatalf("at 880 s: %v", err)
	}
	e.advance(20*time.Second - time.Millisecond)
	if _, err := e.svc.AuthenticateBearerToken(ctx, app.access); err != nil {
		t.Fatalf("at 899.999 s: %v", err)
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("%d reads by 899.999 s, want 1", n)
	}
	e.advance(time.Millisecond)
	_, err := e.svc.AuthenticateBearerToken(ctx, app.access)
	wantCode(t, err, api.CodeInvalidToken)
	_, err = e.svc.Authenticate(bearerRequest("Bearer "+app.access, ipC))
	wantCode(t, err, api.CodeInvalidToken)
	if n := reads.Load(); n != 1 {
		t.Errorf("the expired token was read again (%d reads): its expiry is known", n)
	}
	// After the cache has dropped it, the store says the same.
	e.advance(2 * bearerStateKeep)
	e.svc.bearers.mu.Lock()
	e.svc.bearers.sweep(e.clk.now())
	e.svc.bearers.mu.Unlock()
	_, err = e.svc.AuthenticateBearerToken(ctx, app.access)
	wantCode(t, err, api.CodeInvalidToken)
	if n := reads.Load(); n != 2 {
		t.Errorf("%d reads, want 2 (the swept entry is read again)", n)
	}
	if tokens, devices := e.svc.bearers.size(); tokens != 0 || devices != 0 {
		t.Errorf("the cache holds %d tokens and %d devices of an expired token, want none", tokens, devices)
	}
}

// TestBearerUserStatus: a user who is not active has no valid token. An admin's disable deletes the devices and
// forgets them at once; a status changed behind the service's back (no cache invalidation) shows at the next read,
// within 30 s.
func TestBearerUserStatus(t *testing.T) {
	e, _, app := newLinkEnv(t)
	ctx := context.Background()
	if _, err := e.svc.AuthenticateBearerToken(ctx, app.access); err != nil {
		t.Fatal(err)
	}
	e.write(func(q *store.Q) error { return q.SetStatus(app.user.ID, store.StatusDisabled, e.clk.now()) })
	if _, err := e.svc.AuthenticateBearerToken(ctx, app.access); err != nil {
		t.Fatalf("inside the 30 s: %v (the cache answers)", err)
	}
	e.advance(bearerCacheTTL)
	_, err := e.svc.AuthenticateBearerToken(ctx, app.access)
	wantCode(t, err, api.CodeInvalidToken)
	_, err = e.svc.TouchPrincipal(ctx, app.principal(), netip.MustParseAddr(ipA))
	wantCode(t, err, api.CodeUnauthenticated)

	e.write(func(q *store.Q) error { return q.SetStatus(app.user.ID, store.StatusActive, e.clk.now()) })
	if _, err := e.svc.AuthenticateBearerToken(ctx, app.access); err != nil {
		t.Fatalf("active again: %v", err)
	}
	kim := actorFor(e.addAdmin("Kim"), ipC)
	if _, err := e.svc.UpdateUser(ctx, kim, app.user.ID, UserChange{Status: ptr(store.StatusDisabled)}); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.AuthenticateBearerToken(ctx, app.access)
	wantCode(t, err, api.CodeInvalidToken)
}

// TestBearerPrincipalFollowsTheUser: a rename or a role change of the user reaches the device's next request at
// once, because UpdateUser forgets the user's cached devices too.
func TestBearerPrincipalFollowsTheUser(t *testing.T) {
	e, _, app := newLinkEnv(t)
	ctx := context.Background()
	if _, err := e.svc.AuthenticateBearerToken(ctx, app.access); err != nil {
		t.Fatal(err)
	}
	kim := actorFor(e.addAdmin("Kim"), ipC)
	_, err := e.svc.UpdateUser(ctx, kim, app.user.ID, UserChange{Username: ptr("Alexandra"),
		Role: ptr(store.RoleAdmin), ActorPassword: testPassword})
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.svc.AuthenticateBearerToken(ctx, app.access)
	if err != nil || p.Username != "Alexandra" || !p.IsAdmin() {
		t.Errorf("after the change: %+v, %v; want Alexandra, an admin", p, err)
	}
	p, err = e.svc.TouchPrincipal(ctx, app.principal(), netip.MustParseAddr(ipA))
	if err != nil || p.Username != "Alexandra" || !p.IsAdmin() || p.DeviceName != app.name {
		t.Errorf("TouchPrincipal after the change: %+v, %v", p, err)
	}
}

// ---- use (M2 02 §6.1) ----

// TestBearerRecordsUse: a REST request with a bearer records the device's last_seen_at and last_ip at most once per
// 5 minutes, from the row's last_seen_at on; AuthenticateBearerToken (hello) records nothing, as the hub's Touch
// does it.
func TestBearerRecordsUse(t *testing.T) {
	e, _, app := newLinkEnv(t)
	ctx := context.Background()
	t0 := e.clk.now()
	row := func() store.Device { return *e.deviceRow(app.device) }

	e.advance(deviceTouchEvery - time.Second)
	if _, err := e.svc.Authenticate(bearerRequest("Bearer "+app.access, ipC)); err != nil {
		t.Fatal(err)
	}
	if d := row(); !d.LastSeenAt.Equal(t0) || d.LastIP != ipA {
		t.Fatalf("a use 4:59 after the link was written: %+v", d)
	}
	e.advance(time.Second)
	t1 := e.clk.now()
	if _, err := e.svc.Authenticate(bearerRequest("Bearer "+app.access, ipC)); err != nil {
		t.Fatal(err)
	}
	if d := row(); !d.LastSeenAt.Equal(t1) || d.LastIP != ipC {
		t.Fatalf("after a use at 5:00: %+v; want last seen %v from %s", d, t1, ipC)
	}
	e.advance(deviceTouchEvery - time.Second)
	if _, err := e.svc.Authenticate(bearerRequest("Bearer "+app.access, ipB)); err != nil {
		t.Fatal(err)
	}
	if d := row(); !d.LastSeenAt.Equal(t1) {
		t.Fatalf("a second use within 5 minutes was written: %+v", d)
	}
	e.advance(deviceTouchEvery) // 14:59 after the link: the token is still live
	if _, err := e.svc.AuthenticateBearerToken(ctx, app.access); err != nil {
		t.Fatal(err)
	}
	if d := row(); !d.LastSeenAt.Equal(t1) {
		t.Fatalf("AuthenticateBearerToken recorded a use: %+v", d)
	}
}

// ---- Touch for devices (M2 02 §6.3 item 3) ----

// TestTouchDevice: TouchPrincipal checks a bearer principal's device and user (the cache, or one read), records the
// use at most once per 5 minutes and returns the principal with the device's current name; a device that is gone,
// another user's or unknown is unauthenticated. A session principal comes back unchanged, and Touch (M1's signature)
// answers the same errors.
func TestTouchDevice(t *testing.T) {
	e, alex, app := newLinkEnv(t)
	ctx := context.Background()
	sam := e.addUser("Sam")
	t0 := e.clk.now()

	// The wiring's Revalidate passes what the hub's Identity has: the user, the method and the device.
	hub := Principal{UserID: app.user.ID, Method: MethodBearer, DeviceID: app.device}
	p, err := e.svc.TouchPrincipal(ctx, hub, netip.MustParseAddr(ipC))
	if err != nil || p != app.principal() {
		t.Fatalf("TouchPrincipal = %+v, %v; want %+v", p, err, app.principal())
	}
	if err := e.svc.Touch(ctx, hub, netip.MustParseAddr(ipC)); err != nil {
		t.Fatalf("Touch = %v", err)
	}
	if d := e.deviceRow(app.device); !d.LastSeenAt.Equal(t0) {
		t.Fatalf("a Touch right after the link wrote a use: %+v", d)
	}
	e.advance(deviceTouchEvery)
	if _, err := e.svc.TouchPrincipal(ctx, hub, netip.MustParseAddr("::ffff:"+ipC)); err != nil {
		t.Fatal(err)
	}
	if d := e.deviceRow(app.device); !d.LastSeenAt.Equal(e.clk.now()) || d.LastIP != ipC {
		t.Fatalf("after a Touch at 5:00: %+v", d)
	}

	// A rename reaches the next Touch at once, and AuthenticateBearerToken too.
	if _, err := e.svc.RenameDevice(ctx, principal(alex), app.device, "Gaming PC", meta(ipB)); err != nil {
		t.Fatal(err)
	}
	if p, err := e.svc.TouchPrincipal(ctx, hub, netip.Addr{}); err != nil || p.DeviceName != "Gaming PC" {
		t.Errorf("TouchPrincipal after the rename: %+v, %v", p, err)
	}
	if p, err := e.svc.AuthenticateBearerToken(ctx, app.access); err != nil || p.DeviceName != "Gaming PC" {
		t.Errorf("AuthenticateBearerToken after the rename: %+v, %v", p, err)
	}

	// Principals that are not this device's.
	for name, bad := range map[string]Principal{
		"another user's":  {UserID: sam.ID, Method: MethodBearer, DeviceID: app.device},
		"unknown device":  {UserID: app.user.ID, Method: MethodBearer, DeviceID: "nosuchdevice"},
		"no device ID":    {UserID: app.user.ID, Method: MethodBearer},
		"no user ID":      {Method: MethodBearer, DeviceID: app.device},
		"an unknown kind": {UserID: app.user.ID, Method: 7, DeviceID: app.device},
	} {
		for _, cached := range []bool{true, false} {
			if !cached {
				e.svc.bearers.invalidateDevice(app.device)
			}
			if _, err := e.svc.TouchPrincipal(ctx, bad, netip.MustParseAddr(ipA)); !api.IsCode(err, api.CodeUnauthenticated) {
				t.Errorf("TouchPrincipal with %s (cached %v): %v, want unauthenticated", name, cached, err)
			}
			if err := e.svc.Touch(ctx, bad, netip.MustParseAddr(ipA)); !api.IsCode(err, api.CodeUnauthenticated) {
				t.Errorf("Touch with %s (cached %v): %v, want unauthenticated", name, cached, err)
			}
		}
	}

	// A session principal comes back as it went in.
	sp := principal(alex)
	if p, err := e.svc.TouchPrincipal(ctx, sp, netip.MustParseAddr(ipB)); err != nil || p != sp {
		t.Errorf("TouchPrincipal for a session = %+v, %v; want it unchanged", p, err)
	}

	// The device gone: unauthenticated at once (the revocation forgets the cache).
	if err := e.svc.RevokeDevice(ctx, principal(alex), app.device, meta(ipB)); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.TouchPrincipal(ctx, hub, netip.MustParseAddr(ipA))
	wantCode(t, err, api.CodeUnauthenticated)
}

// TestTouchDeviceWriteFailure: the device's last-seen write is best effort: with the store gone and the device
// cached, Touch still says the device is there, logs the failure once and tries again only 5 minutes later.
func TestTouchDeviceWriteFailure(t *testing.T) {
	e, _, app := newLinkEnv(t)
	ctx := context.Background()
	hub := Principal{UserID: app.user.ID, Method: MethodBearer, DeviceID: app.device}
	e.advance(6 * time.Minute)
	if _, err := e.svc.AuthenticateBearerToken(ctx, app.access); err != nil {
		t.Fatal(err)
	}
	if err := e.db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.TouchPrincipal(ctx, hub, netip.MustParseAddr(ipB)); err != nil {
		t.Fatalf("TouchPrincipal with a failing last-seen write: %v, want nil", err)
	}
	logs := e.logs.String()
	if !strings.Contains(logs, "could not record the device's last use") {
		t.Fatalf("the failed write was not logged:\n%s", logs)
	}
	if _, err := e.svc.TouchPrincipal(ctx, hub, netip.MustParseAddr(ipB)); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(e.logs.String(), "could not record the device's"); n != 1 {
		t.Errorf("the failed write was retried at once (%d log lines, want 1)", n)
	}
	if strings.Contains(e.logs.String(), app.name) || strings.Contains(e.logs.String(), app.access) {
		t.Errorf("the log names the device or its token:\n%s", e.logs.String())
	}
}

// TestMaybeRotateBearer: the daily rotation is for web sessions; a device's principal sets no cookie and writes
// nothing (M2 02 §6.1).
func TestMaybeRotateBearer(t *testing.T) {
	e, _, app := newLinkEnv(t)
	e.advance(48 * time.Hour)
	before := e.rowCounts()
	w := httptest.NewRecorder()
	e.svc.MaybeRotate(w, app.principal())
	if got := w.Header().Values("Set-Cookie"); len(got) != 0 {
		t.Errorf("MaybeRotate for a device set %v", got)
	}
	if after := e.rowCounts(); len(after) != len(before) || after["sessions"] != before["sessions"] {
		t.Errorf("rows %v → %v", before, after)
	}
}

// ---- revocation (M2 02 §6.1, §6.5) ----

// TestBearerForgottenBeforeConnCloser: every path that deletes a device forgets its cached token and rows after the
// commit and before ConnCloser runs, so that the device's next request is refused at once: revoking it (cookie and
// bearer), the app's sign-out, logging out everywhere from the app, a password change, an admin's sign-out, and the
// device cap's eviction. (The refresh-token reuse is refresh_test.go's, the rest of 03 §7.7 TestRevocationMatrix.)
func TestBearerForgottenBeforeConnCloser(t *testing.T) {
	ctx := context.Background()
	for name, revoke := range map[string]func(e *svcEnv, alex LoginResult, app linked){
		"RevokeDevice with the cookie": func(e *svcEnv, alex LoginResult, app linked) {
			if err := e.svc.RevokeDevice(ctx, principal(alex), app.device, meta(ipB)); err != nil {
				t.Fatal(err)
			}
		},
		"RevokeDevice with its own bearer": func(e *svcEnv, _ LoginResult, app linked) {
			if err := e.svc.RevokeDevice(ctx, app.principal(), app.device, meta(ipA)); err != nil {
				t.Fatal(err)
			}
		},
		"RevokeOwnDevice": func(e *svcEnv, _ LoginResult, app linked) {
			if err := e.svc.RevokeOwnDevice(ctx, app.principal(), meta(ipA)); err != nil {
				t.Fatal(err)
			}
		},
		"LogoutEverywhere with the bearer": func(e *svcEnv, _ LoginResult, app linked) {
			if err := e.svc.LogoutEverywhere(ctx, app.principal(), meta(ipA)); err != nil {
				t.Fatal(err)
			}
		},
		"ChangePassword": func(e *svcEnv, alex LoginResult, _ linked) {
			if _, err := e.svc.ChangePassword(ctx, principal(alex), testPassword, newPassword, meta(ipB)); err != nil {
				t.Fatal(err)
			}
		},
		"SignOutUser": func(e *svcEnv, _ LoginResult, app linked) {
			if _, _, err := e.svc.SignOutUser(ctx, actorFor(e.addAdmin("Kim"), ipC), app.user.ID); err != nil {
				t.Fatal(err)
			}
		},
		"the device cap": func(e *svcEnv, alex LoginResult, _ linked) {
			e.advance(time.Hour)
			e.addDevices(alex.User.ID, 19, -1) // all seen in the last 19 minutes: the app is the least recently seen
			e.linkApp(alex, ipC)
		},
	} {
		t.Run(name, func(t *testing.T) {
			e, alex, app := newLinkEnv(t)
			if _, err := e.svc.AuthenticateBearerToken(ctx, app.access); err != nil {
				t.Fatal(err)
			}
			if _, err := e.svc.TouchPrincipal(ctx, app.principal(), netip.Addr{}); err != nil {
				t.Fatal(err)
			}
			e.conns.take()
			reads := e.countAccessReads()
			var during []string
			e.conns.during = func(sel ConnSelector, _ string) {
				if sel.DeviceID != "" && sel.DeviceID != app.device {
					return
				}
				if e.deviceRow(app.device) != nil {
					during = append(during, "the device row was still there")
				}
				if _, _, hit := e.svc.bearers.byToken(e.svc.keys.deviceHash(app.access), e.clk.now()); hit != cacheMiss {
					during = append(during, "the access token was still cached")
				}
				if _, _, ok := e.svc.bearers.byDevice(app.device, e.clk.now()); ok {
					during = append(during, "the device was still cached")
				}
			}
			revoke(e, alex, app)
			e.conns.during = nil
			calls := e.conns.take()
			if len(calls) == 0 {
				t.Fatal("no ConnCloser call")
			}
			if len(during) != 0 {
				t.Errorf("when ConnCloser ran: %v", during)
			}
			_, err := e.svc.AuthenticateBearerToken(ctx, app.access)
			wantCode(t, err, api.CodeInvalidToken)
			if reads.Load() != 1 {
				t.Errorf("%d reads after the revocation, want 1 (the cache was forgotten)", reads.Load())
			}
			_, err = e.svc.TouchPrincipal(ctx, app.principal(), netip.Addr{})
			wantCode(t, err, api.CodeUnauthenticated)
		})
	}
}
