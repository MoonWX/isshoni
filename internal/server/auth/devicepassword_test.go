package auth

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// The in-app password fallback (M2 02 §14, §16.1's password-link cases; README WA30): login's order and buckets,
// checked with the counting hasher of svcEnv, and the device_code grant's success steps with linked_via password.

// passwordRequest is a POST /device/password body with valid device fields.
func passwordRequest(username, password string) api.DevicePasswordRequest {
	return api.DevicePasswordRequest{Username: username, Password: password, ClientKind: "desktop",
		DeviceName: "DESKTOP-7F3K2LQ", OS: "windows", AppVersion: "0.2.0"}
}

// passwordLink calls PasswordDeviceLogin from ip.
func (e *svcEnv) passwordLink(username, password, ip string) (api.DeviceTokenResponse, error) {
	return e.svc.PasswordDeviceLogin(context.Background(), passwordRequest(username, password), meta(ip))
}

func TestPasswordDeviceLogin(t *testing.T) {
	e := newSvcEnv(t)
	alex := e.addUser("Alex")
	// Two wrong tries from the same address first: they hold tokens in both per-username buckets.
	for range 2 {
		_, err := e.passwordLink("Alex", "not the password", ipC)
		wantCode(t, err, api.CodeInvalidCredentials)
	}
	if held := e.heldTokens(); held != [2]int{1, 1} {
		t.Fatalf("held tokens %v after two wrong tries, want [1 1]", held)
	}
	e.hashes.Store(0)
	now := e.clk.now()
	req := passwordRequest("alex", testPassword) // the username's case does not matter, as at login
	req.DeviceName, req.ClientKind, req.OS, req.AppVersion = " Gaming  PC ", "mobile", "android", "0.2.0-rc.1"
	res, err := e.svc.PasswordDeviceLogin(context.Background(), req, meta(ipC))
	if err != nil {
		t.Fatal(err)
	}
	if h := e.hashes.Load(); h != 1 {
		t.Errorf("%d hashes, want 1", h)
	}
	if !strings.HasPrefix(res.AccessToken, "isa_") || !strings.HasPrefix(res.RefreshToken, "isr_") ||
		res.TokenType != "Bearer" || res.ExpiresIn != 900 || res.RefreshExpiresIn != 7776000 ||
		res.User != (api.User{ID: string(alex.ID), Username: "Alex", Role: api.RoleUser}) ||
		res.Device.Name != "Gaming PC" {
		t.Errorf("answer = %+v", res)
	}
	devs := e.devices(alex.ID)
	want := store.Device{ID: store.DeviceID(res.Device.ID), UserID: alex.ID, Name: "Gaming PC", ClientKind: "mobile",
		OS: "android", AppVersion: "0.2.0-rc.1", LinkedVia: "password", CreatedAt: now, LastSeenAt: now, LastIP: ipC}
	if len(devs) != 1 || !reflect.DeepEqual(devs[0], want) {
		t.Fatalf("devices = %+v\nwant [%+v]", devs, want)
	}
	if n := e.rawCount("device_tokens"); n != 2 {
		t.Errorf("%d device tokens, want the pair", n)
	}
	// No web session, no auth.login row, and the last-login time stays.
	if n := len(e.sessions(alex.ID)); n != 0 {
		t.Errorf("%d sessions after a password link, want none", n)
	}
	if n := e.auditCount("auth.login"); n != 0 {
		t.Errorf("%d auth.login rows, want none", n)
	}
	if u := e.userRow(alex.ID); !u.LastLoginAt.IsZero() {
		t.Errorf("last login %v after a password link, want none", u.LastLoginAt)
	}
	rows := e.audit("device.linked")
	if len(rows) != 1 {
		t.Fatalf("%d device.linked rows, want 1", len(rows))
	}
	r := rows[0]
	if r.Actor != (store.Actor{Kind: store.ActorUser, UserID: alex.ID, Name: "Alex", IP: ipC}) ||
		r.TargetKind != "device" || r.TargetID != res.Device.ID || r.TargetName != "Gaming PC" {
		t.Errorf("device.linked row = %+v", r)
	}
	wantDetail(t, "device.linked", r.Detail, map[string]any{"clientKind": "mobile", "os": "android",
		"via": "password"})
	if !strings.Contains(e.logs.String(), "via=password") || strings.Contains(e.logs.String(), "Gaming PC") ||
		strings.Contains(e.logs.String(), testPassword) {
		t.Errorf("log:\n%s", e.logs.String())
	}
	// The attempt ended like a login: this address's auth-user-ip bucket is full again, and auth-user is as it was
	// before the attempt (the two failures stay).
	if held := e.heldTokens(); held != [2]int{0, 1} {
		t.Errorf("held tokens %v after a password link, want [0 1]", held)
	}
}

// TestPasswordDeviceLoginValidation: every field is checked before the answer, before any bucket of a username and
// any hash.
func TestPasswordDeviceLoginValidation(t *testing.T) {
	e := newSvcEnv(t)
	e.addUser("Alex")
	mod := func(f func(*api.DevicePasswordRequest)) api.DevicePasswordRequest {
		in := passwordRequest("Alex", testPassword)
		f(&in)
		return in
	}
	for i, tc := range []struct {
		in   api.DevicePasswordRequest
		want map[string]string
	}{
		{mod(func(r *api.DevicePasswordRequest) { r.ClientKind = "laptop" }), map[string]string{"clientKind": "invalid"}},
		{mod(func(r *api.DevicePasswordRequest) { r.OS = "beos" }), map[string]string{"os": "invalid"}},
		{mod(func(r *api.DevicePasswordRequest) { r.DeviceName = "" }), map[string]string{"deviceName": "required"}},
		{mod(func(r *api.DevicePasswordRequest) { r.DeviceName = strings.Repeat("x", 65) }),
			map[string]string{"deviceName": "too_long"}},
		{mod(func(r *api.DevicePasswordRequest) { r.DeviceName = "a\x00b" }), map[string]string{"deviceName": "invalid"}},
		{mod(func(r *api.DevicePasswordRequest) { r.AppVersion = "" }), map[string]string{"appVersion": "required"}},
		{mod(func(r *api.DevicePasswordRequest) { r.AppVersion = strings.Repeat("1", 33) }),
			map[string]string{"appVersion": "too_long"}},
		{mod(func(r *api.DevicePasswordRequest) { r.AppVersion = "0.2.0 beta" }),
			map[string]string{"appVersion": "invalid"}},
		{mod(func(r *api.DevicePasswordRequest) { r.Username = " " }), map[string]string{"username": "required"}},
		{mod(func(r *api.DevicePasswordRequest) { r.Password = "" }), map[string]string{"password": "required"}},
		{mod(func(r *api.DevicePasswordRequest) { r.Password = strings.Repeat("p", 1025) }),
			map[string]string{"password": "too_long"}},
		{api.DevicePasswordRequest{}, map[string]string{"username": "required", "password": "required",
			"clientKind": "invalid", "os": "invalid", "deviceName": "required", "appVersion": "required"}},
	} {
		_, err := e.svc.PasswordDeviceLogin(context.Background(), tc.in, meta(v4(i+1)))
		ae := wantCode(t, err, api.CodeValidationFailed)
		if !reflect.DeepEqual(ae.Fields, tc.want) {
			t.Errorf("%+v: fields %v, want %v", tc.in, ae.Fields, tc.want)
		}
	}
	if h := e.hashes.Load(); h != 0 {
		t.Errorf("%d hashes for invalid requests, want none", h)
	}
	if held := e.heldTokens(); held != [2]int{0, 0} {
		t.Errorf("held tokens %v after invalid requests, want none", held)
	}
	if n := e.rawCount("devices"); n != 0 {
		t.Errorf("%d devices", n)
	}
}

// TestPasswordDeviceLoginOrder: login's buckets in login's order, each refusal without a hash (M2 02 §14).
func TestPasswordDeviceLoginOrder(t *testing.T) {
	e := newSvcEnv(t)
	e.addUser("Alex")
	sam := e.addUser("Sam")

	// 1. auth-ip comes first, before the validation, and is shared with login.
	for range 20 {
		wantCode(t, e.tryLogin("", "", ipB), api.CodeValidationFailed)
	}
	_, err := e.passwordLink("Alex", testPassword, ipB)
	wantCode(t, err, api.CodeRateLimited)
	_, err = e.svc.PasswordDeviceLogin(context.Background(), api.DevicePasswordRequest{}, meta(ipB))
	wantCode(t, err, api.CodeRateLimited)
	if h := e.hashes.Load(); h != 0 {
		t.Errorf("%d hashes after auth-ip refusals, want none", h)
	}

	// 2. auth-user-ip: five wrong passwords from one address, at /auth/login and here alike, block that address,
	// even for the right password, without a hash.
	for i := range 5 {
		var err error
		if i%2 == 0 {
			err = e.tryLogin("Alex", "not the password", ipA)
		} else {
			_, err = e.passwordLink("Alex", "not the password", ipA)
		}
		wantCode(t, err, api.CodeInvalidCredentials)
	}
	if h := e.hashes.Load(); h != 5 {
		t.Errorf("%d hashes for five wrong passwords, want 5", h)
	}
	_, err = e.passwordLink("Alex", testPassword, ipA)
	wantCode(t, err, api.CodeRateLimited)
	wantCode(t, e.tryLogin("Alex", testPassword, ipA), api.CodeRateLimited)
	if h := e.hashes.Load(); h != 5 {
		t.Errorf("%d hashes after the block, want still 5", h)
	}
	if n := e.auditCount("auth.login_failed"); n != 5 {
		t.Errorf("%d auth.login_failed rows, want 5", n)
	}
	// Another address still links.
	if _, err := e.passwordLink("Alex", testPassword, ipC); err != nil {
		t.Errorf("from another address: %v", err)
	}

	// 3. auth-user (Sam's own): 30 wrong passwords from 30 addresses empty it; then only the user's known IPs get
	// through.
	e.hashes.Store(0)
	for i := range 30 {
		_, err := e.passwordLink("Sam", "not the password", v4(i+1))
		wantCode(t, err, api.CodeInvalidCredentials)
	}
	_, err = e.passwordLink("Sam", testPassword, v4(100))
	wantCode(t, err, api.CodeRateLimited)
	if h := e.hashes.Load(); h != 30 {
		t.Errorf("%d hashes, want 30 (the refusal hashes nothing)", h)
	}
	e.write(func(q *store.Q) error { // a live session from v4(101) makes it a known IP
		return q.CreateSession(&store.Session{UserID: sam.ID, TokenHash: keyedHash([]byte("s"), "known"),
			Name: "Chrome on Windows", CreatedAt: e.clk.now(), LastSeenAt: e.clk.now(), LastIP: v4(101),
			IdleExpiresAt: e.clk.now().Add(sessionIdleTTL), ExpiresAt: e.clk.now().Add(sessionMaxTTL)})
	})
	if _, err := e.passwordLink("Sam", testPassword, v4(101)); err != nil {
		t.Errorf("from a known IP while auth-user is empty: %v", err)
	}
}

// TestPasswordDeviceLoginHashBudget: auth-hash comes last, after the username buckets: an empty budget is 503
// server_busy without a hash, and the attempt's tokens go back.
func TestPasswordDeviceLoginHashBudget(t *testing.T) {
	e := newSvcEnv(t, func(o *Options) { o.Hashes = HashBudget{Burst: 1, PerSecond: 1} })
	e.addUser("Alex")
	_, err := e.passwordLink("Alex", "not the password", ipA)
	wantCode(t, err, api.CodeInvalidCredentials)
	held := e.heldTokens()
	_, err = e.passwordLink("Alex", testPassword, ipA)
	if ae := wantCode(t, err, api.CodeServerBusy); ae.RetryAfter != 1 {
		t.Errorf("retryAfter %d, want 1", ae.RetryAfter)
	}
	if h := e.hashes.Load(); h != 1 {
		t.Errorf("%d hashes, want 1 (the refused request hashes nothing)", h)
	}
	if got := e.heldTokens(); got != held {
		t.Errorf("held tokens %v after a hash-budget refusal, want %v as before", got, held)
	}
}

// TestPasswordDeviceLoginRefusals: an unknown user hashes the dummy; a wrong password is 401 with auth.login_failed;
// a pending or disabled account is told so only after a correct password.
func TestPasswordDeviceLoginRefusals(t *testing.T) {
	e := newSvcEnv(t)
	alex := e.addUser("Alex")
	pending := e.addUser("Pat", func(u *store.User) { u.Status = store.StatusPending; u.CreatedVia = "signup" })
	disabled := e.addUser("Dee", func(u *store.User) { u.Status = store.StatusDisabled })
	noHash := e.addUser("Reset", func(u *store.User) { u.PasswordHash = "" })

	for _, tc := range []struct {
		user, password, ip string
		code, reason       string
		target             store.UserID
	}{
		{"Nobody", testPassword, v4(1), api.CodeInvalidCredentials, "unknown_user", ""},
		{"Alex", "not the password", v4(2), api.CodeInvalidCredentials, "wrong_password", alex.ID},
		{"Reset", testPassword, v4(3), api.CodeInvalidCredentials, "wrong_password", noHash.ID},
		{"Pat", "not the password", v4(4), api.CodeInvalidCredentials, "wrong_password", pending.ID},
		{"Pat", testPassword, v4(5), api.CodeAccountPending, "account_pending", pending.ID},
		{"Dee", "not the password", v4(6), api.CodeInvalidCredentials, "wrong_password", disabled.ID},
		{"Dee", testPassword, v4(7), api.CodeAccountDisabled, "account_disabled", disabled.ID},
	} {
		e.hashes.Store(0)
		before := len(e.audit("auth.login_failed"))
		_, err := e.passwordLink(tc.user, tc.password, tc.ip)
		wantCode(t, err, tc.code)
		if h := e.hashes.Load(); h != 1 {
			t.Errorf("%s/%s: %d hashes, want exactly 1", tc.user, tc.code, h)
		}
		rows := e.audit("auth.login_failed")
		if len(rows) != before+1 {
			t.Fatalf("%s/%s: %d new auth.login_failed rows, want 1", tc.user, tc.code, len(rows)-before)
		}
		r := rows[len(rows)-1]
		if r.Actor != (store.Actor{Kind: store.ActorAnonymous, IP: tc.ip}) || r.TargetID != string(tc.target) ||
			r.Detail["reason"] != tc.reason || r.Outcome != "denied" {
			t.Errorf("%s/%s: auth.login_failed row = %+v", tc.user, tc.code, r)
		}
	}
	if n := e.rawCount("devices"); n != 0 {
		t.Errorf("%d devices after refusals", n)
	}
	if n := e.auditCount("device.linked"); n != 0 {
		t.Errorf("%d device.linked rows after refusals", n)
	}
}

// TestPasswordDeviceLoginRechecksInWrite: the account is judged again in the Write: a password changed between the
// hash and the Write is 401, an account disabled meanwhile 403, and nothing is linked.
func TestPasswordDeviceLoginRechecksInWrite(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(q *store.Q, u store.UserID, now time.Time) error
		code   string
	}{
		{"password changed", func(q *store.Q, u store.UserID, now time.Time) error {
			return q.SetPasswordHash(u, katForeignPHC, now)
		}, api.CodeInvalidCredentials},
		{"account disabled", func(q *store.Q, u store.UserID, now time.Time) error {
			return q.SetStatus(u, store.StatusDisabled, now)
		}, api.CodeAccountDisabled},
		{"account deleted", func(q *store.Q, u store.UserID, _ time.Time) error {
			return q.DeleteUser(u)
		}, api.CodeInvalidCredentials},
	} {
		e := newSvcEnv(t)
		alex := e.addUser("Alex")
		e.changeDuringCheck(func(q *store.Q, now time.Time) error { return tc.change(q, alex.ID, now) })
		_, err := e.passwordLink("Alex", testPassword, ipA)
		if !api.IsCode(err, tc.code) {
			t.Errorf("%s: %v, want %s", tc.name, err, tc.code)
		}
		if n := e.rawCount("devices"); n != 0 {
			t.Errorf("%s: %d devices", tc.name, n)
		}
		if n := e.auditCount("device.linked"); n != 0 {
			t.Errorf("%s: %d device.linked rows", tc.name, n)
		}
	}
}

// TestPasswordDeviceLoginRehash: login's rehash rule applies: a hash with other argon2 parameters is replaced in
// the link's Write.
func TestPasswordDeviceLoginRehash(t *testing.T) {
	e := newSvcEnv(t)
	other := testArgon
	other.Time = 2
	h, err := newHasher(other, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	old := h.compute([]byte(testPassword))
	alex := e.addUser("Alex", func(u *store.User) { u.PasswordHash = old })
	e.hashes.Store(0)
	if _, err := e.passwordLink("Alex", testPassword, ipA); err != nil {
		t.Fatal(err)
	}
	if n := e.hashes.Load(); n != 2 {
		t.Errorf("%d hashes, want 2 (the check and the rehash)", n)
	}
	u := e.userRow(alex.ID)
	ph, err := parsePHC(u.PasswordHash)
	if err != nil || u.PasswordHash == old || ph.Params != testArgon {
		t.Errorf("stored hash %q (%v) after the link, want one with the current parameters", u.PasswordHash, err)
	}
	// The new hash works for a login.
	if err := e.tryLogin("Alex", testPassword, ipB); err != nil {
		t.Errorf("login with the rehashed password: %v", err)
	}
}

// TestPasswordDeviceLoginNoDeviceBuckets: the fallback takes none of the device flow's buckets (M2 02 §5.5).
func TestPasswordDeviceLoginNoDeviceBuckets(t *testing.T) {
	e := newSvcEnv(t)
	e.addUser("Alex")
	for i := range 12 { // more than device-code-ip's 10, from one address
		if _, err := e.passwordLink("Alex", testPassword, "2001:db8:aa:1::1"); err != nil {
			t.Fatalf("link %d: %v", i+1, err)
		}
	}
	for name, n := range map[string]int{
		"device-code-ip":   e.svc.throttle.deviceCodeIP.size(),
		"device-code-ip48": e.svc.throttle.deviceCodeIP48.size(),
		"device-token-ip":  e.svc.throttle.deviceTokenIP.size(),
		"device-lookup":    e.svc.throttle.deviceLookup.size(),
	} {
		if n != 0 {
			t.Errorf("%s holds %d keys after password links, want 0", name, n)
		}
	}
	if n := len(e.devices(e.userByName("Alex").ID)); n != 12 {
		t.Errorf("%d devices, want 12", n)
	}
}
