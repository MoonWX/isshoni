package auth

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// The device flow at the service (M2 02 §16.1's device-flow row; README WA30), under the manual clock of svcEnv: the
// start, the approval, the poll state machine, the four buckets and the pending-code cap, the device cap, and the
// audit rows device.approved, device.denied and device.linked. The password fallback is devicepassword_test.go.

// ---- helpers ----

// deviceRequest is a valid POST /device/code body (M2 02 §5.2's example).
func deviceRequest() api.DeviceCodeRequest {
	return api.DeviceCodeRequest{ClientKind: "desktop", DeviceName: "DESKTOP-7F3K2LQ", OS: "windows", AppVersion: "0.2.0"}
}

// startFlow starts a link from ip and fails the test on error.
func (e *svcEnv) startFlow(ip string) api.DeviceCodeResponse {
	e.t.Helper()
	res, err := e.svc.StartDeviceFlow(context.Background(), deviceRequest(), meta(ip))
	if err != nil {
		e.t.Fatalf("StartDeviceFlow from %s: %v", ip, err)
	}
	return res
}

// tryStart starts a link from ip and returns only the error.
func (e *svcEnv) tryStart(in api.DeviceCodeRequest, ip string) error {
	_, err := e.svc.StartDeviceFlow(context.Background(), in, meta(ip))
	return err
}

// poll is one poll of the device_code grant from ip.
func (e *svcEnv) poll(deviceCode, ip string) (api.DeviceTokenResponse, error) {
	return e.svc.DeviceToken(context.Background(),
		api.DeviceTokenRequest{GrantType: api.GrantTypeDeviceCode, DeviceCode: deviceCode}, meta(ip))
}

// decide approves or denies a user code as the session of res, from ip. It is safe to call from any goroutine.
func (e *svcEnv) decide(res LoginResult, userCode string, approve bool, ip string) error {
	return e.svc.DecideUserCode(context.Background(), principalOf(res.Session, res.User),
		api.DeviceDecideRequest{UserCode: userCode, Approve: approve}, meta(ip))
}

// lookup looks a user code up as the session of res, from ip.
func (e *svcEnv) lookup(res LoginResult, userCode, ip string) (api.DeviceLookup, error) {
	return e.svc.LookupUserCode(context.Background(), principalOf(res.Session, res.User), userCode, meta(ip))
}

// link runs a whole device flow for the user of res: the app starts from appIP, the user approves from ipB, and
// the app's poll from appIP gets the pair.
func (e *svcEnv) link(res LoginResult, appIP string) api.DeviceTokenResponse {
	e.t.Helper()
	flow := e.startFlow(appIP)
	if err := e.decide(res, flow.UserCode, true, ipB); err != nil {
		e.t.Fatalf("approve: %v", err)
	}
	pair, err := e.poll(flow.DeviceCode, appIP)
	if err != nil {
		e.t.Fatalf("poll after the approval: %v", err)
	}
	return pair
}

// codeRow reads the row of a device code.
func (e *svcEnv) codeRow(deviceCode string) store.DeviceCode {
	e.t.Helper()
	var dc store.DeviceCode
	e.read(func(q *store.Q) error {
		var err error
		dc, err = q.DeviceCodeByHash(e.svc.keys.deviceHash(deviceCode))
		return err
	})
	return dc
}

// devices returns a user's devices, the most recently seen first.
func (e *svcEnv) devices(u store.UserID) []store.Device {
	e.t.Helper()
	var out []store.Device
	e.read(func(q *store.Q) error {
		var err error
		out, err = q.ListDevices(u)
		return err
	})
	return out
}

// normalized is the normalized form of a user code as the server shows it.
func normalized(t *testing.T, userCode string) string {
	t.Helper()
	code, ok := api.NormalizeUserCode(userCode)
	if !ok {
		t.Fatalf("user code %q does not normalize", userCode)
	}
	return code
}

// otherUserCode is a well-formed user code that is not code.
func otherUserCode(code string) string {
	if normalizedEq(code, "BBBB-BBBB") {
		return "CCCC-CCCC"
	}
	return "BBBB-BBBB"
}

func normalizedEq(a, b string) bool {
	x, _ := api.NormalizeUserCode(a)
	y, _ := api.NormalizeUserCode(b)
	return x == y
}

// wantDetail fails the test unless an audit row's detail is want (strings only, as the rows of this file have).
func wantDetail(t *testing.T, what string, got, want map[string]any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s detail = %v, want %v", what, got, want)
	}
}

// lookupKeys is the number of users the device-lookup bucket holds tokens of: 0 when every token went back.
func (e *svcEnv) lookupKeys() int { return e.svc.throttle.deviceLookup.size() }

// v4 returns the n-th of the addresses 198.51.100.101 and up (RFC 5737; clear of ipB), for tests that need many
// clients. n is 1–150.
func v4(n int) string { return fmt.Sprintf("198.51.100.%d", 100+n) }

// ---- start (M2 02 §5.2) ----

var userCodeShape = regexp.MustCompile(`^[BCDFGHJKLMNPQRSTVWXZ]{4}-[BCDFGHJKLMNPQRSTVWXZ]{4}$`)

func TestStartDeviceFlow(t *testing.T) {
	e := newSvcEnv(t)
	now := e.clk.now()
	in := deviceRequest()
	in.DeviceName = "  DESKTOP   7F3K2LQ "
	res, err := e.svc.StartDeviceFlow(context.Background(), in, meta("::ffff:"+ipA))
	if err != nil {
		t.Fatal(err)
	}
	if !tokenWellFormed(res.DeviceCode, deviceCodeBytes) {
		t.Errorf("deviceCode %q is not 43 base64url characters of 32 bytes", res.DeviceCode)
	}
	if !userCodeShape.MatchString(res.UserCode) {
		t.Errorf("userCode %q is not XXXX-XXXX of the alphabet", res.UserCode)
	}
	if res.VerificationURI != testOrigin+"/link" || res.VerificationURIComplete != testOrigin+"/link?code="+res.UserCode {
		t.Errorf("URIs = %q, %q", res.VerificationURI, res.VerificationURIComplete)
	}
	if res.ExpiresIn != 600 || res.Interval != 5 {
		t.Errorf("expiresIn %d, interval %d; want 600 and 5", res.ExpiresIn, res.Interval)
	}

	row := e.codeRow(res.DeviceCode)
	want := store.DeviceCode{
		DeviceCodeHash: e.svc.keys.deviceHash(res.DeviceCode),
		UserCodeHash:   e.svc.keys.deviceHash(normalized(t, res.UserCode)),
		ClientKind:     "desktop", DeviceName: "DESKTOP 7F3K2LQ", OS: "windows", AppVersion: "0.2.0",
		RequestIP: ipA, CreatedAt: now, ExpiresAt: now.Add(600 * time.Second), Interval: 5 * time.Second,
		Status: "pending",
	}
	if !reflect.DeepEqual(row, want) {
		t.Errorf("device code row = %+v\nwant %+v", row, want)
	}
	if n := e.rawCount("audit_log"); n != 0 {
		t.Errorf("%d audit rows after a start, want none", n)
	}
	if other := e.startFlow(ipA); other.DeviceCode == res.DeviceCode || other.UserCode == res.UserCode {
		t.Errorf("two starts gave the same codes")
	}
}

func TestStartDeviceFlowValidation(t *testing.T) {
	e := newSvcEnv(t)
	mod := func(f func(*api.DeviceCodeRequest)) api.DeviceCodeRequest {
		in := deviceRequest()
		f(&in)
		return in
	}
	for i, tc := range []struct {
		in   api.DeviceCodeRequest
		want map[string]string // nil = accepted
	}{
		{mod(func(r *api.DeviceCodeRequest) { r.ClientKind = "mobile"; r.OS = "android" }), nil},
		{mod(func(r *api.DeviceCodeRequest) { r.OS = "linux" }), nil},
		{mod(func(r *api.DeviceCodeRequest) { r.AppVersion = "0.0.0-dev+3f9c2a1" }), nil},
		{mod(func(r *api.DeviceCodeRequest) { r.ClientKind = "" }), map[string]string{"clientKind": "invalid"}},
		{mod(func(r *api.DeviceCodeRequest) { r.ClientKind = "web" }), map[string]string{"clientKind": "invalid"}},
		{mod(func(r *api.DeviceCodeRequest) { r.OS = "Windows" }), map[string]string{"os": "invalid"}},
		{mod(func(r *api.DeviceCodeRequest) { r.OS = "" }), map[string]string{"os": "invalid"}},
		{mod(func(r *api.DeviceCodeRequest) { r.DeviceName = " " }), map[string]string{"deviceName": "required"}},
		{mod(func(r *api.DeviceCodeRequest) { r.DeviceName = strings.Repeat("x", 65) }),
			map[string]string{"deviceName": "too_long"}},
		{mod(func(r *api.DeviceCodeRequest) { r.DeviceName = "a\nb" }), map[string]string{"deviceName": "invalid"}},
		{mod(func(r *api.DeviceCodeRequest) { r.AppVersion = "" }), map[string]string{"appVersion": "required"}},
		{mod(func(r *api.DeviceCodeRequest) { r.AppVersion = strings.Repeat("1", 33) }),
			map[string]string{"appVersion": "too_long"}},
		{mod(func(r *api.DeviceCodeRequest) { r.AppVersion = "0.2.0 beta" }), map[string]string{"appVersion": "invalid"}},
		{api.DeviceCodeRequest{}, map[string]string{"clientKind": "invalid", "os": "invalid", "deviceName": "required",
			"appVersion": "required"}},
	} {
		err := e.tryStart(tc.in, v4(i+1))
		if tc.want == nil {
			if err != nil {
				t.Errorf("%+v: %v", tc.in, err)
			}
			continue
		}
		ae := wantCode(t, err, api.CodeValidationFailed)
		if !reflect.DeepEqual(ae.Fields, tc.want) {
			t.Errorf("%+v: fields %v, want %v", tc.in, ae.Fields, tc.want)
		}
	}
	if n := e.rawCount("device_codes"); n != 3 {
		t.Errorf("%d device codes, want the 3 accepted ones", n)
	}
}

// TestNewUserCode: user codes are 8 letters of the alphabet, and every letter is drawn.
func TestNewUserCode(t *testing.T) {
	seen := map[rune]int{}
	for range 2000 {
		c := newUserCode()
		if n, ok := api.NormalizeUserCode(c); !ok || n != c {
			t.Fatalf("newUserCode() = %q, not a normalized user code", c)
		}
		for _, r := range c {
			seen[r]++
		}
	}
	if len(seen) != len(api.UserCodeAlphabet) {
		t.Errorf("%d distinct letters in 16,000 draws, want all %d", len(seen), len(api.UserCodeAlphabet))
	}
}

// TestUserCodeCollisions: a user code that collides with an existing row's is drawn again, up to 3 times, then the
// start is internal and writes nothing (M2 02 §3).
func TestUserCodeCollisions(t *testing.T) {
	e := newSvcEnv(t)
	taken := normalized(t, e.startFlow(ipA).UserCode)
	free := "CCCCCCCC"
	if taken == free {
		free = "DDDDDDDD"
	}
	var draws []string
	e.svc.userCode = func() string {
		d := draws[0]
		draws = draws[1:]
		return d
	}

	draws = []string{taken, taken, taken, free}
	res := e.startFlow(ipA)
	if res.UserCode != api.FormatUserCode(free) || len(draws) != 0 {
		t.Errorf("after 3 collisions: user code %q (%d draws left), want %s", res.UserCode, len(draws), free)
	}

	draws = []string{taken, taken, taken, taken}
	err := e.tryStart(deviceRequest(), ipA)
	wantCode(t, err, api.CodeInternal)
	if n := e.rawCount("device_codes"); n != 2 {
		t.Errorf("%d device codes after 4 collisions, want 2", n)
	}
}

// ---- the poll state machine (M2 02 §5.4) ----

// newFlowEnv is a service with the user Alex logged in from ipB.
func newFlowEnv(t *testing.T) (*svcEnv, LoginResult) {
	e := newSvcEnv(t)
	e.addUser("Alex")
	return e, e.login("Alex", ipB)
}

// wantSlowDown fails the test unless err is slow_down with params.interval, and the row has that interval and was
// polled now.
func (e *svcEnv) wantSlowDown(err error, deviceCode string, interval int) {
	e.t.Helper()
	ae := wantCode(e.t, err, api.CodeSlowDown)
	if ae.Params[api.ParamInterval] != interval {
		e.t.Errorf("slow_down params = %v, want interval %d", ae.Params, interval)
	}
	row := e.codeRow(deviceCode)
	if row.Interval != time.Duration(interval)*time.Second || !row.LastPollAt.Equal(e.clk.now()) {
		e.t.Errorf("after slow_down: interval %v, last poll %v; want %ds at %v", row.Interval, row.LastPollAt,
			interval, e.clk.now())
	}
}

func TestDevicePollPendingAndSlowDown(t *testing.T) {
	e, _ := newFlowEnv(t)
	flow := e.startFlow(ipA)

	_, err := e.poll(flow.DeviceCode, ipA)
	wantCode(t, err, api.CodeAuthorizationPending)
	if row := e.codeRow(flow.DeviceCode); !row.LastPollAt.Equal(e.clk.now()) || row.Interval != 5*time.Second {
		t.Errorf("after the first poll: last poll %v, interval %v", row.LastPollAt, row.Interval)
	}

	// 3.9 s after a poll with interval 5 (less than 5 − 1): slow_down, interval 10.
	e.advance(3900 * time.Millisecond)
	_, err = e.poll(flow.DeviceCode, ipA)
	e.wantSlowDown(err, flow.DeviceCode, 10)

	// With interval 10, 8.9 s is early again and 9.0 s is not.
	e.advance(8900 * time.Millisecond)
	_, err = e.poll(flow.DeviceCode, ipA)
	e.wantSlowDown(err, flow.DeviceCode, 15)
	e.advance(14 * time.Second)
	_, err = e.poll(flow.DeviceCode, ipA)
	wantCode(t, err, api.CodeAuthorizationPending)
	if row := e.codeRow(flow.DeviceCode); row.Interval != 15*time.Second || !row.LastPollAt.Equal(e.clk.now()) {
		t.Errorf("after a poll on time: interval %v, last poll %v", row.Interval, row.LastPollAt)
	}

	// 4.0 s after a poll with interval 5 is on time (the 1 s tolerance).
	other := e.startFlow(ipA)
	_, err = e.poll(other.DeviceCode, ipA)
	wantCode(t, err, api.CodeAuthorizationPending)
	e.advance(4 * time.Second)
	_, err = e.poll(other.DeviceCode, ipA)
	wantCode(t, err, api.CodeAuthorizationPending)
	if row := e.codeRow(other.DeviceCode); row.Interval != 5*time.Second {
		t.Errorf("interval %v after a poll at 4.0 s, want 5s", row.Interval)
	}
}

func TestDevicePollIntervalCap(t *testing.T) {
	e, _ := newFlowEnv(t)
	flow := e.startFlow(ipA)
	_, err := e.poll(flow.DeviceCode, ipA)
	wantCode(t, err, api.CodeAuthorizationPending)
	for _, want := range []int{10, 15, 20, 25, 30, 35, 40, 45, 50, 55, 60, 60, 60} {
		e.advance(time.Second)
		_, err := e.poll(flow.DeviceCode, ipA)
		e.wantSlowDown(err, flow.DeviceCode, want)
	}
	// On time for 60 s (59 s with the tolerance) it is pending again.
	e.advance(59 * time.Second)
	_, err = e.poll(flow.DeviceCode, ipA)
	wantCode(t, err, api.CodeAuthorizationPending)
}

func TestDevicePollDenied(t *testing.T) {
	e, alex := newFlowEnv(t)
	flow := e.startFlow(ipA)
	if err := e.decide(alex, flow.UserCode, false, ipB); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		_, err := e.poll(flow.DeviceCode, ipA)
		wantCode(t, err, api.CodeAccessDenied)
		if row := e.codeRow(flow.DeviceCode); !row.LastPollAt.Equal(e.clk.now()) || row.Status != "denied" {
			t.Errorf("after an access_denied poll: %+v", row)
		}
		e.advance(5 * time.Second)
	}
	if n := e.rawCount("devices"); n != 0 {
		t.Errorf("%d devices after a denial", n)
	}
}

func TestDevicePollApprovedOnce(t *testing.T) {
	e, alex := newFlowEnv(t)
	flow := e.startFlow(ipA)
	if err := e.decide(alex, flow.UserCode, true, ipB); err != nil {
		t.Fatal(err)
	}
	if _, err := e.poll(flow.DeviceCode, ipA); err != nil {
		t.Fatalf("the poll after the approval: %v", err)
	}
	polled := e.clk.now()
	e.advance(5 * time.Second)
	_, err := e.poll(flow.DeviceCode, ipA)
	wantCode(t, err, api.CodeInvalidGrant)
	row := e.codeRow(flow.DeviceCode)
	if row.Status != "consumed" || !row.LastPollAt.Equal(polled) {
		t.Errorf("a consumed code's poll changed the row: %+v", row)
	}
	if n := e.rawCount("devices"); n != 1 {
		t.Errorf("%d devices, want 1", n)
	}
}

func TestDevicePollExpiry(t *testing.T) {
	e, alex := newFlowEnv(t)
	flow := e.startFlow(ipA)
	e.advance(599 * time.Second)
	_, err := e.poll(flow.DeviceCode, ipA)
	wantCode(t, err, api.CodeAuthorizationPending)
	// At expires_at the code is expired; expiry comes before slow_down (the last poll was 1 s ago).
	e.advance(time.Second)
	_, err = e.poll(flow.DeviceCode, ipA)
	wantCode(t, err, api.CodeExpiredToken)

	// An approved code expires the same way, and no device appears.
	approved := e.startFlow(ipA)
	if err := e.decide(alex, approved.UserCode, true, ipB); err != nil {
		t.Fatal(err)
	}
	e.advance(600 * time.Second)
	_, err = e.poll(approved.DeviceCode, ipA)
	wantCode(t, err, api.CodeExpiredToken)
	if n := e.rawCount("devices"); n != 0 {
		t.Errorf("%d devices from an expired approval", n)
	}

	// The janitor prunes a code an hour after its expiry; its device code is then unknown.
	e.advance(time.Hour + time.Millisecond)
	if _, err := e.db.Prune(context.Background(), e.clk.now()); err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{flow.DeviceCode, approved.DeviceCode} {
		_, err = e.poll(code, ipA)
		wantCode(t, err, api.CodeInvalidGrant)
	}
}

func TestDevicePollApproverNoLongerActive(t *testing.T) {
	e, _ := newFlowEnv(t)
	sam := e.addUser("Sam")
	samRes := e.login("Sam", ipC)

	// A status change that kept the code: access_denied, the poll recorded, no device.
	flow := e.startFlow(ipA)
	if err := e.decide(samRes, flow.UserCode, true, ipC); err != nil {
		t.Fatal(err)
	}
	e.write(func(q *store.Q) error { return q.SetStatus(sam.ID, store.StatusDisabled, e.clk.now()) })
	_, err := e.poll(flow.DeviceCode, ipA)
	wantCode(t, err, api.CodeAccessDenied)
	if row := e.codeRow(flow.DeviceCode); !row.LastPollAt.Equal(e.clk.now()) || row.Status != "approved" {
		t.Errorf("after the poll: %+v", row)
	}
	if n := e.rawCount("devices"); n != 0 {
		t.Errorf("%d devices for a disabled approver", n)
	}

	// The admin's disable (03 §7.7) deletes the codes the user decided: the code is unknown.
	e.write(func(q *store.Q) error { return q.SetStatus(sam.ID, store.StatusActive, e.clk.now()) })
	samRes = e.login("Sam", ipC)
	flow = e.startFlow(ipA)
	if err := e.decide(samRes, flow.UserCode, true, ipC); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.UpdateUser(context.Background(), store.CLIActor, sam.ID,
		UserChange{Status: ptr(store.StatusDisabled)}); err != nil {
		t.Fatal(err)
	}
	_, err = e.poll(flow.DeviceCode, ipA)
	wantCode(t, err, api.CodeInvalidGrant)
}

// TestDevicePollMalformedCode: a device code of the wrong shape is invalid_grant before any hashing or DB access
// (here the store is closed), and an unknown grant type is 422.
func TestDevicePollMalformedCode(t *testing.T) {
	e := newSvcEnv(t)
	good := newToken(deviceCodeBytes)
	_, err := e.poll(good, ipA)
	wantCode(t, err, api.CodeInvalidGrant) // never issued
	for _, gt := range []api.GrantType{"", "password", "authorization_code", "DEVICE_CODE"} {
		_, err := e.svc.DeviceToken(context.Background(), api.DeviceTokenRequest{GrantType: gt, DeviceCode: good},
			meta(ipA))
		if ae := wantCode(t, err, api.CodeValidationFailed); !reflect.DeepEqual(ae.Fields,
			map[string]string{"grantType": "invalid"}) {
			t.Errorf("grant type %q: fields %v", gt, ae.Fields)
		}
	}

	if err := e.db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"", "short", good + "A", good[:42], good[:42] + "!", strings.Repeat("_", 42) + "B",
		"isa_" + good, " " + good[1:]} {
		_, err := e.poll(code, ipA)
		wantCode(t, err, api.CodeInvalidGrant)
	}
	_, err = e.poll(good, ipA)
	wantCode(t, err, api.CodeInternal) // the shape was right, so the closed store answered
}

// TestDevicePollSuccess: the 200 answer, the device and its tokens, the consumed code, device.linked and the log.
func TestDevicePollSuccess(t *testing.T) {
	e := newSvcEnv(t)
	alexUser := e.addUser("Alex")
	alex := e.login("Alex", ipB)
	flow := e.startFlow(ipA)
	e.advance(time.Minute)
	if err := e.decide(alex, flow.UserCode, true, ipB); err != nil {
		t.Fatal(err)
	}
	e.advance(time.Minute)
	now := e.clk.now()
	res, err := e.poll(flow.DeviceCode, ipC)
	if err != nil {
		t.Fatal(err)
	}

	access, okA := strings.CutPrefix(res.AccessToken, "isa_")
	refresh, okR := strings.CutPrefix(res.RefreshToken, "isr_")
	if !okA || !okR || !tokenWellFormed(access, 32) || !tokenWellFormed(refresh, 32) {
		t.Errorf("tokens %q, %q are not isa_/isr_ + 43", res.AccessToken, res.RefreshToken)
	}
	if res.TokenType != "Bearer" || res.ExpiresIn != 900 || res.RefreshExpiresIn != 7776000 {
		t.Errorf("answer = %+v", res)
	}
	wantUser := api.User{ID: string(alexUser.ID), Username: "Alex", Role: api.RoleUser}
	if res.User != wantUser || res.Device.Name != "DESKTOP-7F3K2LQ" || res.Device.ID == "" {
		t.Errorf("answer device %+v, user %+v", res.Device, res.User)
	}

	devs := e.devices(alexUser.ID)
	want := store.Device{ID: store.DeviceID(res.Device.ID), UserID: alexUser.ID, Name: "DESKTOP-7F3K2LQ",
		ClientKind: "desktop", OS: "windows", AppVersion: "0.2.0", LinkedVia: "device_flow", CreatedAt: now,
		LastSeenAt: now, LastIP: ipC}
	if len(devs) != 1 || !reflect.DeepEqual(devs[0], want) {
		t.Fatalf("devices = %+v\nwant [%+v]", devs, want)
	}
	for _, tc := range []struct {
		tok, kind string
		ttl       time.Duration
	}{{res.AccessToken, "access", 900 * time.Second}, {res.RefreshToken, "refresh", 90 * 24 * time.Hour}} {
		e.read(func(q *store.Q) error {
			tok, dev, user, err := q.DeviceTokenByHash(e.svc.keys.deviceHash(tc.tok), tc.kind, now)
			if err != nil {
				return err
			}
			if dev.ID != want.ID || user.ID != alexUser.ID || !tok.CreatedAt.Equal(now) ||
				!tok.ExpiresAt.Equal(now.Add(tc.ttl)) || tok.IssuedBy != nil || !tok.SpentAt.IsZero() {
				t.Errorf("%s token row = %+v", tc.kind, tok)
			}
			return nil
		})
	}
	if row := e.codeRow(flow.DeviceCode); row.Status != "consumed" || !row.LastPollAt.Equal(now) {
		t.Errorf("code after the success = %+v", row)
	}

	rows := e.audit("device.linked")
	if len(rows) != 1 {
		t.Fatalf("%d device.linked rows, want 1", len(rows))
	}
	r := rows[0]
	if r.Actor != (store.Actor{Kind: store.ActorUser, UserID: alexUser.ID, Name: "Alex", IP: ipC}) ||
		r.TargetKind != "device" || r.TargetID != res.Device.ID || r.TargetName != "DESKTOP-7F3K2LQ" ||
		!r.At.Equal(now) || r.Outcome != "ok" {
		t.Errorf("device.linked row = %+v", r)
	}
	wantDetail(t, "device.linked", r.Detail, map[string]any{"clientKind": "desktop", "os": "windows",
		"via": "device_flow"})
	if n := e.auditCount("device.revoked"); n != 0 {
		t.Errorf("%d device.revoked rows for a user without devices", n)
	}
	if calls := e.conns.take(); len(calls) != 0 {
		t.Errorf("ConnCloser calls %v, want none", calls)
	}

	logs := e.logs.String()
	line := `msg="device linked" component=auth user_id=` + string(alexUser.ID) + " device_id=" + res.Device.ID +
		" via=device_flow"
	if !strings.Contains(logs, line) {
		t.Errorf("no %q line in the log:\n%s", line, logs)
	}
	for _, secret := range []string{"DESKTOP-7F3K2LQ", res.AccessToken, access, res.RefreshToken, refresh,
		flow.DeviceCode, flow.UserCode, normalized(t, flow.UserCode)} {
		if strings.Contains(logs, secret) {
			t.Errorf("the log names %q:\n%s", secret, logs)
		}
	}
}

// ---- approval (M2 02 §5.3) ----

func TestLookupUserCode(t *testing.T) {
	e, alex := newFlowEnv(t)
	created := e.clk.now()
	flow := e.startFlow(ipA)
	e.advance(time.Minute)

	got, err := e.lookup(alex, strings.ToLower(strings.ReplaceAll(flow.UserCode, "-", " ")), ipA)
	if err != nil {
		t.Fatal(err)
	}
	want := api.DeviceLookup{UserCode: flow.UserCode, ClientKind: "desktop", OS: "windows",
		DeviceName: "DESKTOP-7F3K2LQ", AppVersion: "0.2.0", RequestIP: ipA, Network: api.NetworkMatchSame,
		RequestedAt: created, ExpiresAt: created.Add(600 * time.Second)}
	if got != want {
		t.Errorf("lookup = %+v\nwant %+v", got, want)
	}

	// The network comparison, through the service (the table is TestNetworkMatch).
	for _, tc := range []struct {
		app, browser string
		want         api.NetworkMatch
	}{
		{ipA, ipB, api.NetworkMatchDifferent},
		{"2001:db8:cc::7", ipA, api.NetworkMatchUnknown},
		{ipA, "2001:db8:cc::7", api.NetworkMatchUnknown},
		{"2001:db8:cc:1::7", "2001:db8:cc:1::99", api.NetworkMatchSame},
		{ipA, "::ffff:" + ipA, api.NetworkMatchSame},
	} {
		code := e.startFlow(tc.app).UserCode
		got, err := e.lookup(alex, code, tc.browser)
		if err != nil || got.Network != tc.want {
			t.Errorf("started from %s, looked up from %s: %v, %v; want %s", tc.app, tc.browser, got.Network, err,
				tc.want)
		}
	}

	// A code that can no longer be decided is device_code_invalid: unknown, decided, expired.
	_, err = e.lookup(alex, otherUserCode(flow.UserCode), ipA)
	wantCode(t, err, api.CodeDeviceCodeInvalid)
	decided := e.startFlow(ipB)
	if err := e.decide(alex, decided.UserCode, false, ipB); err != nil {
		t.Fatal(err)
	}
	_, err = e.lookup(alex, decided.UserCode, ipB)
	wantCode(t, err, api.CodeDeviceCodeInvalid)
	e.advance(600 * time.Second)
	_, err = e.lookup(alex, flow.UserCode, ipA)
	wantCode(t, err, api.CodeDeviceCodeInvalid)
}

func TestApprovalCallers(t *testing.T) {
	e, alex := newFlowEnv(t)
	flow := e.startFlow(ipA)
	ctx := context.Background()
	bearer := Principal{UserID: alex.User.ID, Username: "Alex", Role: store.RoleUser, Method: MethodBearer,
		DeviceID: "d1d1d1d1d1d1", ClientKind: "desktop"}
	_, err := e.svc.LookupUserCode(ctx, bearer, flow.UserCode, meta(ipA))
	ae := wantCode(t, err, api.CodeForbidden)
	err = e.svc.DecideUserCode(ctx, bearer, api.DeviceDecideRequest{UserCode: flow.UserCode, Approve: true}, meta(ipA))
	be := wantCode(t, err, api.CodeForbidden)
	for _, got := range []api.Error{ae, be} {
		if !reflect.DeepEqual(got.Params, map[string]any{"credential": "session"}) {
			t.Errorf("a bearer's forbidden params = %v, want {credential: session}", got.Params)
		}
	}
	_, err = e.svc.LookupUserCode(ctx, Principal{}, flow.UserCode, meta(ipA))
	wantCode(t, err, api.CodeUnauthenticated)
	err = e.svc.DecideUserCode(ctx, Principal{Method: MethodSession, UserID: alex.User.ID},
		api.DeviceDecideRequest{UserCode: flow.UserCode}, meta(ipA))
	wantCode(t, err, api.CodeUnauthenticated)

	// A session that is gone changes nothing, and its device-lookup token goes back.
	if err := e.svc.Logout(ctx, principalOf(alex.Session, alex.User), meta(ipB)); err != nil {
		t.Fatal(err)
	}
	wantCode(t, e.decide(alex, flow.UserCode, true, ipB), api.CodeUnauthenticated)
	if n := e.lookupKeys(); n != 0 {
		t.Errorf("device-lookup holds %d keys after an unauthenticated decide, want 0", n)
	}
	if row := e.codeRow(flow.DeviceCode); row.Status != "pending" {
		t.Errorf("the code is %s after a decide without a session", row.Status)
	}
}

func TestDecideUserCode(t *testing.T) {
	e, alex := newFlowEnv(t)
	alexUser := alex.User
	e.addUser("Sam")
	sam := e.login("Sam", ipC)
	flow := e.startFlow(ipA)
	e.advance(time.Minute)
	now := e.clk.now()

	// The user code as typed: lower case, a space for the dash.
	if err := e.decide(alex, strings.ToLower(strings.ReplaceAll(flow.UserCode, "-", " ")), true, ipB); err != nil {
		t.Fatal(err)
	}
	row := e.codeRow(flow.DeviceCode)
	if row.Status != "approved" || row.UserID != alexUser.ID || !row.DecidedAt.Equal(now) ||
		row.DeviceName != "DESKTOP-7F3K2LQ" {
		t.Errorf("code after the approval = %+v", row)
	}
	rows := e.audit("device.approved")
	if len(rows) != 1 {
		t.Fatalf("%d device.approved rows, want 1", len(rows))
	}
	r := rows[0]
	if r.Actor != (store.Actor{Kind: store.ActorUser, UserID: alexUser.ID, Name: "Alex", IP: ipB}) ||
		r.TargetKind != "device" || r.TargetID != "" || r.TargetName != "DESKTOP-7F3K2LQ" || !r.At.Equal(now) ||
		r.Outcome != "ok" {
		t.Errorf("device.approved row = %+v", r)
	}
	wantDetail(t, "device.approved", r.Detail, map[string]any{"clientKind": "desktop", "os": "windows"})

	// Idempotent for the same user and decision: 204 again, no second row, the token back.
	e.advance(time.Second)
	if err := e.decide(alex, flow.UserCode, true, ipB); err != nil {
		t.Errorf("approving again: %v", err)
	}
	if n := e.auditCount("device.approved"); n != 1 {
		t.Errorf("%d device.approved rows after a repeat, want 1", n)
	}
	// Another decision, or another user, is 404.
	wantCode(t, e.decide(alex, flow.UserCode, false, ipB), api.CodeDeviceCodeInvalid)
	wantCode(t, e.decide(sam, flow.UserCode, true, ipC), api.CodeDeviceCodeInvalid)
	wantCode(t, e.decide(sam, flow.UserCode, false, ipC), api.CodeDeviceCodeInvalid)
	if row := e.codeRow(flow.DeviceCode); row.Status != "approved" || row.UserID != alexUser.ID {
		t.Errorf("the code changed: %+v", row)
	}
	// After the app collected it, approving again is still the same decision.
	if _, err := e.poll(flow.DeviceCode, ipA); err != nil {
		t.Fatal(err)
	}
	if err := e.decide(alex, flow.UserCode, true, ipB); err != nil {
		t.Errorf("approving a consumed code again: %v", err)
	}

	// A denial, with its row; denying again is 204.
	denied := e.startFlow(ipA)
	if err := e.decide(sam, denied.UserCode, false, ipC); err != nil {
		t.Fatal(err)
	}
	if err := e.decide(sam, denied.UserCode, false, ipC); err != nil {
		t.Errorf("denying again: %v", err)
	}
	wantCode(t, e.decide(sam, denied.UserCode, true, ipC), api.CodeDeviceCodeInvalid)
	rows = e.audit("device.denied")
	if len(rows) != 1 || rows[0].Actor.UserID != sam.User.ID || rows[0].Actor.IP != ipC ||
		rows[0].TargetName != "DESKTOP-7F3K2LQ" || rows[0].TargetID != "" {
		t.Fatalf("device.denied rows = %+v", rows)
	}
	wantDetail(t, "device.denied", rows[0].Detail, map[string]any{"clientKind": "desktop", "os": "windows"})

	// An unknown or expired code is 404.
	wantCode(t, e.decide(alex, otherUserCode(flow.UserCode), true, ipB), api.CodeDeviceCodeInvalid)
	late := e.startFlow(ipA)
	e.advance(600 * time.Second)
	wantCode(t, e.decide(alex, late.UserCode, true, ipB), api.CodeDeviceCodeInvalid)
	if n := e.auditCount("device.approved") + e.auditCount("device.denied"); n != 2 {
		t.Errorf("%d decision rows, want 2", n)
	}
}

func TestDecideRenamesDevice(t *testing.T) {
	e, alex := newFlowEnv(t)
	ctx := context.Background()
	p := principalOf(alex.Session, alex.User)
	flow := e.startFlow(ipA)

	// The shapes come before the bucket and cost no token.
	for _, tc := range []struct {
		in   api.DeviceDecideRequest
		want map[string]string
	}{
		{api.DeviceDecideRequest{UserCode: ""}, map[string]string{"userCode": "required"}},
		{api.DeviceDecideRequest{UserCode: " - "}, map[string]string{"userCode": "required"}},
		{api.DeviceDecideRequest{UserCode: "WDJB-MJHA"}, map[string]string{"userCode": "invalid"}},
		{api.DeviceDecideRequest{UserCode: "WDJB-MJH"}, map[string]string{"userCode": "invalid"}},
		{api.DeviceDecideRequest{UserCode: flow.UserCode, DeviceName: ptr("")}, map[string]string{"deviceName": "required"}},
		{api.DeviceDecideRequest{UserCode: flow.UserCode, DeviceName: ptr(strings.Repeat("x", 65))},
			map[string]string{"deviceName": "too_long"}},
		{api.DeviceDecideRequest{UserCode: flow.UserCode, DeviceName: ptr("a\tb")},
			map[string]string{"deviceName": "invalid"}},
	} {
		ae := wantCode(t, e.svc.DecideUserCode(ctx, p, tc.in, meta(ipB)), api.CodeValidationFailed)
		if !reflect.DeepEqual(ae.Fields, tc.want) {
			t.Errorf("%+v: fields %v, want %v", tc.in, ae.Fields, tc.want)
		}
	}
	for _, code := range []string{"", "WDJB-MJHA"} {
		_, err := e.lookup(alex, code, ipB)
		wantCode(t, err, api.CodeValidationFailed)
	}
	if n := e.lookupKeys(); n != 0 {
		t.Errorf("device-lookup holds %d keys after shape errors, want 0", n)
	}

	err := e.svc.DecideUserCode(ctx, p, api.DeviceDecideRequest{UserCode: flow.UserCode, Approve: true,
		DeviceName: ptr("  Gaming   PC ")}, meta(ipB))
	if err != nil {
		t.Fatal(err)
	}
	if row := e.codeRow(flow.DeviceCode); row.DeviceName != "Gaming PC" {
		t.Errorf("device name %q after a rename at decide, want Gaming PC", row.DeviceName)
	}
	if rows := e.audit("device.approved"); len(rows) != 1 || rows[0].TargetName != "Gaming PC" {
		t.Errorf("device.approved rows = %+v", rows)
	}
	res, err := e.poll(flow.DeviceCode, ipA)
	if err != nil || res.Device.Name != "Gaming PC" {
		t.Errorf("poll: %+v, %v; want the device Gaming PC", res.Device, err)
	}
	if rows := e.audit("device.linked"); len(rows) != 1 || rows[0].TargetName != "Gaming PC" {
		t.Errorf("device.linked rows = %+v", rows)
	}
}

// TestDecideInParallel: of two users deciding one code at once, exactly one wins (204) and the other gets 404; one
// user deciding twice at once gets 204 twice and one row.
func TestDecideInParallel(t *testing.T) {
	e, alex := newFlowEnv(t)
	e.addUser("Sam")
	sam := e.login("Sam", ipC)
	const rounds = 20
	for i := range rounds {
		e.advance(time.Minute) // each loser keeps a device-lookup token; this refills it
		flow := e.startFlow(v4(i + 1))
		var errs [2]error
		var wg sync.WaitGroup
		start := make(chan struct{})
		for j, res := range []LoginResult{alex, sam} {
			wg.Go(func() {
				<-start
				errs[j] = e.decide(res, flow.UserCode, j == 0, ipB) // Alex approves, Sam denies
			})
		}
		close(start)
		wg.Wait()
		winner := -1
		for j, err := range errs {
			switch {
			case err == nil && winner < 0:
				winner = j
			case api.IsCode(err, api.CodeDeviceCodeInvalid):
			default:
				t.Fatalf("round %d: errors %v; want one nil and one device_code_invalid", i, errs)
			}
		}
		if winner < 0 {
			t.Fatalf("round %d: errors %v; nobody won", i, errs)
		}
		row := e.codeRow(flow.DeviceCode)
		wantStatus, wantUser := "approved", alex.User.ID
		if winner == 1 {
			wantStatus, wantUser = "denied", sam.User.ID
		}
		if row.Status != wantStatus || row.UserID != wantUser {
			t.Fatalf("round %d: the winner was %d, but the code is %+v", i, winner, row)
		}
	}
	if n := e.auditCount("device.approved") + e.auditCount("device.denied"); n != rounds {
		t.Errorf("%d decision rows after %d rounds, want one each", n, rounds)
	}

	flow := e.startFlow(v4(100))
	var errs [2]error
	var wg sync.WaitGroup
	for j := range errs {
		wg.Go(func() { errs[j] = e.decide(alex, flow.UserCode, true, ipB) })
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Errorf("one user approving twice at once: %v", errs)
	}
	if n := e.auditCount("device.approved") + e.auditCount("device.denied"); n != rounds+1 {
		t.Errorf("%d decision rows, want %d", n, rounds+1)
	}
}

// ---- the buckets (M2 02 §5.5) ----

// throttledRows returns the auth.throttled rows.
func (e *svcEnv) throttledRows() []store.AuditEntry {
	e.t.Helper()
	return e.audit("auth.throttled")
}

func TestDeviceCodeIPBucket(t *testing.T) {
	e := newSvcEnv(t)
	for range 10 {
		e.startFlow(ipA)
	}
	err := e.tryStart(deviceRequest(), ipA)
	if ae := wantCode(t, err, api.CodeRateLimited); ae.RetryAfter != 360 {
		t.Errorf("retryAfter %d, want 360 (one token per 6 min)", ae.RetryAfter)
	}
	wantCode(t, e.tryStart(deviceRequest(), ipA), api.CodeRateLimited)
	rows := e.throttledRows()
	if len(rows) != 1 {
		t.Fatalf("%d auth.throttled rows for one episode, want 1", len(rows))
	}
	if r := rows[0]; r.Actor != (store.Actor{Kind: store.ActorAnonymous, IP: ipA}) || r.Outcome != "denied" ||
		r.TargetKind != "" {
		t.Errorf("auth.throttled row = %+v", r)
	}
	wantDetail(t, "auth.throttled", rows[0].Detail, map[string]any{"scope": "ip", "key": ipA})
	if !strings.Contains(e.logs.String(), "bucket=device-code-ip remote_ip="+ipA) {
		t.Errorf("no warn line for the block:\n%s", e.logs.String())
	}
	// Another address is not affected; an IPv6 client's key is its /64.
	e.startFlow(ipB)
	for i := range 10 {
		e.startFlow(fmt.Sprintf("2001:db8:1:2::%x", i+1))
	}
	wantCode(t, e.tryStart(deviceRequest(), "2001:db8:1:2::ff"), api.CodeRateLimited)
	if rows := e.throttledRows(); len(rows) != 2 || rows[1].Detail["key"] != "2001:db8:1:2::/64" {
		t.Errorf("auth.throttled rows = %+v", rows)
	}

	// A new token after 6 minutes; the next refusal is a new episode.
	e.advance(6 * time.Minute)
	e.startFlow(ipA)
	wantCode(t, e.tryStart(deviceRequest(), ipA), api.CodeRateLimited)
	if n := len(e.throttledRows()); n != 3 {
		t.Errorf("%d auth.throttled rows after a second episode, want 3", n)
	}

	// A refusal needs no read: with the store closed it is still 429.
	if err := e.db.Close(); err != nil {
		t.Fatal(err)
	}
	wantCode(t, e.tryStart(deviceRequest(), ipA), api.CodeRateLimited)
}

func TestDeviceCodeBucketOrder(t *testing.T) {
	e := newSvcEnv(t)

	// auth-ip comes first: an address that spent it on logins is refused before device-code-ip takes a token.
	for range 20 {
		wantCode(t, e.tryLogin("", "", ipB), api.CodeValidationFailed)
	}
	wantCode(t, e.tryStart(deviceRequest(), ipB), api.CodeRateLimited)
	if n := e.svc.throttle.deviceCodeIP.size(); n != 0 {
		t.Errorf("device-code-ip holds %d keys after an auth-ip refusal, want 0", n)
	}

	// device-code-ip comes before the validation, and every request takes its token.
	bad := deviceRequest()
	bad.OS = "beos"
	for range 10 {
		wantCode(t, e.tryStart(bad, ipC), api.CodeValidationFailed)
	}
	wantCode(t, e.tryStart(deviceRequest(), ipC), api.CodeRateLimited)
	wantCode(t, e.tryStart(bad, ipC), api.CodeRateLimited)

	// device-code-ip comes before device-code-ip48: a /64's refused requests take no /48 token.
	for range 10 {
		e.startFlow("2001:db8:aa:1::1")
	}
	for range 5 {
		wantCode(t, e.tryStart(deviceRequest(), "2001:db8:aa:1::1"), api.CodeRateLimited)
	}
	for i := range 20 {
		e.startFlow(fmt.Sprintf("2001:db8:aa:%x::1", i+2))
	}
	wantCode(t, e.tryStart(deviceRequest(), "2001:db8:aa:ff::1"), api.CodeRateLimited)
	if n := e.rawCount("device_codes"); n != 30 {
		t.Errorf("%d device codes, want 30", n)
	}
}

func TestDeviceCodeIP48Bucket(t *testing.T) {
	e := newSvcEnv(t)
	// IPv4 clients, also IPv4-mapped ones, never take a /48 token.
	e.startFlow("203.0.113.9")
	e.startFlow("::ffff:203.0.113.10")
	if n := e.svc.throttle.deviceCodeIP48.size(); n != 0 {
		t.Errorf("device-code-ip48 holds %d keys after IPv4 requests, want 0", n)
	}

	// 31 different /64s of one /48: the 31st is refused.
	for i := 1; i <= 31; i++ {
		ip := fmt.Sprintf("2001:db8:aa:%x::1", i)
		err := e.tryStart(deviceRequest(), ip)
		if i <= 30 && err != nil {
			t.Fatalf("request %d from %s: %v", i, ip, err)
		}
		if i == 31 {
			if ae := wantCode(t, err, api.CodeRateLimited); ae.RetryAfter != 120 {
				t.Errorf("retryAfter %d, want 120 (one token per 2 min)", ae.RetryAfter)
			}
		}
	}
	rows := e.throttledRows()
	if len(rows) != 1 || rows[0].Actor != (store.Actor{Kind: store.ActorAnonymous, IP: "2001:db8:aa:1f::1"}) {
		t.Fatalf("auth.throttled rows = %+v", rows)
	}
	wantDetail(t, "auth.throttled", rows[0].Detail, map[string]any{"scope": "ip", "key": "2001:db8:aa::/48"})
	if !strings.Contains(e.logs.String(), "bucket=device-code-ip48") {
		t.Errorf("no warn line for the /48 block:\n%s", e.logs.String())
	}

	// Another /48 and an IPv4 client still get codes.
	e.startFlow("2001:db8:bb::1")
	e.startFlow("203.0.113.11")
	// One more token after 2 minutes.
	e.advance(2 * time.Minute)
	e.startFlow("2001:db8:aa:100::1")
	wantCode(t, e.tryStart(deviceRequest(), "2001:db8:aa:101::1"), api.CodeRateLimited)
}

func TestPendingCodeCap(t *testing.T) {
	e := newSvcEnv(t)
	now := e.clk.now()
	e.write(func(q *store.Q) error {
		add := func(i int, status string, expires time.Time) error {
			return q.CreateDeviceCode(store.DeviceCode{
				DeviceCodeHash: keyedHash([]byte("device"), fmt.Sprint(i)),
				UserCodeHash:   keyedHash([]byte("user"), fmt.Sprint(i)),
				ClientKind:     "desktop", DeviceName: "PC", OS: "windows", AppVersion: "0.2.0", RequestIP: ipC,
				CreatedAt: now, ExpiresAt: expires, Status: status,
			})
		}
		for i := range maxPendingDeviceCodes - 1 {
			if err := add(i, "pending", now.Add(5*time.Minute)); err != nil {
				return err
			}
		}
		// Neither of these counts: an expired pending code (expires_at = now) and a decided one.
		if err := add(-1, "pending", now); err != nil {
			return err
		}
		return add(-2, "denied", now.Add(5*time.Minute))
	})

	e.startFlow(ipA) // the 5,000th pending code
	err := e.tryStart(deviceRequest(), ipB)
	if ae := wantCode(t, err, api.CodeServerBusy); ae.RetryAfter != 60 {
		t.Errorf("retryAfter %d, want 60", ae.RetryAfter)
	}
	e.read(func(q *store.Q) error {
		n, err := q.CountPendingDeviceCodes(e.clk.now())
		if n != maxPendingDeviceCodes {
			t.Errorf("%d pending codes, want %d", n, maxPendingDeviceCodes)
		}
		return err
	})
	// The cap is the last step: an invalid request is 422.
	bad := deviceRequest()
	bad.ClientKind = "toaster"
	wantCode(t, e.tryStart(bad, ipC), api.CodeValidationFailed)

	// Codes expire, and there is room again.
	e.advance(5 * time.Minute)
	e.startFlow(ipB)
}

func TestDeviceTokenIPBucket(t *testing.T) {
	e := newSvcEnv(t)
	const bad = "not-a-device-code"
	for range 60 {
		_, err := e.poll(bad, ipA)
		wantCode(t, err, api.CodeInvalidGrant)
	}
	_, err := e.poll(bad, ipA)
	if ae := wantCode(t, err, api.CodeRateLimited); ae.RetryAfter != 1 {
		t.Errorf("retryAfter %d, want 1", ae.RetryAfter)
	}
	// The bucket comes before the grant type, and both grants take from it.
	_, err = e.svc.DeviceToken(context.Background(), api.DeviceTokenRequest{GrantType: "nope"}, meta(ipA))
	wantCode(t, err, api.CodeRateLimited)
	_, err = e.svc.DeviceToken(context.Background(), api.DeviceTokenRequest{GrantType: api.GrantTypeRefreshToken},
		meta(ipA))
	wantCode(t, err, api.CodeRateLimited)
	rows := e.throttledRows()
	if len(rows) != 1 || rows[0].Actor != (store.Actor{Kind: store.ActorAnonymous, IP: ipA}) {
		t.Fatalf("auth.throttled rows = %+v", rows)
	}
	wantDetail(t, "auth.throttled", rows[0].Detail, map[string]any{"scope": "ip", "key": ipA})

	// Another address is not affected; a second later there is one more token.
	_, err = e.poll(bad, ipB)
	wantCode(t, err, api.CodeInvalidGrant)
	e.advance(time.Second)
	_, err = e.poll(bad, ipA)
	wantCode(t, err, api.CodeInvalidGrant)

	// A refusal needs no read.
	if err := e.db.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = e.poll(newToken(deviceCodeBytes), ipA)
	wantCode(t, err, api.CodeRateLimited)
}

func TestDeviceLookupBucket(t *testing.T) {
	e, alex := newFlowEnv(t)
	alexUser := alex.User
	e.addUser("Sam")
	sam := e.login("Sam", ipC)
	flow := e.startFlow(ipA)
	wrong := otherUserCode(flow.UserCode)

	// A found code gives its token back: lookups of it never run out.
	for range 15 {
		if _, err := e.lookup(alex, flow.UserCode, ipB); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.lookupKeys(); n != 0 {
		t.Errorf("device-lookup holds %d keys after found codes, want 0", n)
	}
	// Wrong codes keep theirs: 10, then even the right code is refused, before any read.
	for range 10 {
		_, err := e.lookup(alex, wrong, ipB)
		wantCode(t, err, api.CodeDeviceCodeInvalid)
	}
	_, err := e.lookup(alex, flow.UserCode, ipB)
	if ae := wantCode(t, err, api.CodeRateLimited); ae.RetryAfter != 60 {
		t.Errorf("retryAfter %d, want 60", ae.RetryAfter)
	}
	wantCode(t, e.decide(alex, flow.UserCode, true, ipB), api.CodeRateLimited)
	_, err = e.lookup(alex, wrong, ipB)
	wantCode(t, err, api.CodeRateLimited)
	rows := e.throttledRows()
	if len(rows) != 1 {
		t.Fatalf("%d auth.throttled rows for one episode, want 1", len(rows))
	}
	if r := rows[0]; r.Actor != (store.Actor{Kind: store.ActorUser, UserID: alexUser.ID, Name: "Alex", IP: ipB}) ||
		r.Outcome != "denied" {
		t.Errorf("auth.throttled row = %+v", r)
	}
	wantDetail(t, "auth.throttled", rows[0].Detail, map[string]any{"scope": "device_lookup"})
	if !strings.Contains(e.logs.String(), "bucket=device-lookup") {
		t.Errorf("no warn line for the block:\n%s", e.logs.String())
	}
	// The shape check comes first and costs nothing; Sam's bucket is his own.
	_, err = e.lookup(alex, "nope", ipB)
	wantCode(t, err, api.CodeValidationFailed)
	if _, err := e.lookup(sam, flow.UserCode, ipC); err != nil {
		t.Errorf("Sam's lookup while Alex is throttled: %v", err)
	}

	// One token a minute later. A found code keeps it available; a wrong decide spends it.
	e.advance(time.Minute)
	for range 3 {
		if _, err := e.lookup(alex, flow.UserCode, ipB); err != nil {
			t.Fatal(err)
		}
	}
	wantCode(t, e.decide(alex, wrong, true, ipB), api.CodeDeviceCodeInvalid)
	wantCode(t, e.decide(alex, flow.UserCode, true, ipB), api.CodeRateLimited)
	// A decision and its repeats give the token back.
	e.advance(time.Minute)
	for range 3 {
		if err := e.decide(alex, flow.UserCode, true, ipB); err != nil {
			t.Fatal(err)
		}
	}
	// The code is approved now, so looking it up is a wrong code, which keeps the last token.
	_, err = e.lookup(alex, flow.UserCode, ipB)
	wantCode(t, err, api.CodeDeviceCodeInvalid)

	// A refusal needs no read.
	if err := e.db.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = e.lookup(alex, flow.UserCode, ipB)
	wantCode(t, err, api.CodeRateLimited)
}

// ---- the device cap (M2 02 §6.7) ----

// addDevices gives a user n devices, the i-th last seen (n − i) minutes ago, except that the device named
// "PC <oldest>" was last seen 3 hours ago (though it is not the oldest created). It returns the devices.
func (e *svcEnv) addDevices(u store.UserID, n, oldest int) []store.Device {
	e.t.Helper()
	now := e.clk.now()
	out := make([]store.Device, n)
	e.write(func(q *store.Q) error {
		for i := range n {
			d := store.Device{UserID: u, Name: fmt.Sprintf("PC %d", i), ClientKind: "desktop", OS: "windows",
				AppVersion: "0.1.0", LinkedVia: "device_flow", CreatedAt: now.Add(-time.Duration(48-i) * time.Hour),
				LastSeenAt: now.Add(-time.Duration(n-i) * time.Minute), LastIP: ipC}
			if i == oldest {
				d.LastSeenAt = now.Add(-3 * time.Hour)
			}
			if err := q.CreateDevice(&d); err != nil {
				return err
			}
			out[i] = d
		}
		return nil
	})
	return out
}

// TestDeviceCap: the 21st device evicts the least recently seen one, with a device.revoked {via: "cap"} row and a
// ConnCloser call after the commit, for both ways of linking. With 19 devices nothing goes.
func TestDeviceCap(t *testing.T) {
	for _, via := range []string{"device_flow", "password"} {
		e, alex := newFlowEnv(t)
		alexUser := alex.User
		evicted := e.addDevices(alexUser.ID, 20, 7)[7]
		e.conns.during = func(sel ConnSelector, _ string) {
			for _, d := range e.devices(alexUser.ID) {
				if d.ID == sel.DeviceID {
					t.Errorf("%s: ConnCloser ran while the evicted device was still there", via)
				}
			}
		}
		e.advance(time.Minute)
		var res api.DeviceTokenResponse
		if via == "device_flow" {
			res = e.link(alex, ipA)
		} else {
			var err error
			res, err = e.svc.PasswordDeviceLogin(context.Background(), passwordRequest("Alex", testPassword), meta(ipA))
			if err != nil {
				t.Fatal(err)
			}
		}

		devs := e.devices(alexUser.ID)
		if len(devs) != 20 || devs[0].ID != store.DeviceID(res.Device.ID) {
			t.Fatalf("%s: %d devices (newest %s), want 20 with the new one first", via, len(devs), devs[0].ID)
		}
		for _, d := range devs {
			if d.ID == evicted.ID {
				t.Errorf("%s: the least recently seen device is still there", via)
			}
		}
		calls := e.conns.take()
		want := []closedConn{{ConnSelector{UserID: alexUser.ID, DeviceID: evicted.ID}, ReasonDeviceRevoked}}
		if !reflect.DeepEqual(calls, want) {
			t.Errorf("%s: ConnCloser calls %+v, want %+v", via, calls, want)
		}
		rows := e.audit("device.revoked")
		if len(rows) != 1 {
			t.Fatalf("%s: %d device.revoked rows, want 1", via, len(rows))
		}
		r := rows[0]
		if r.Actor != store.SystemActor || r.TargetKind != "device" || r.TargetID != string(evicted.ID) ||
			r.TargetName != "PC 7" || r.Outcome != "ok" || !r.At.Equal(e.clk.now()) {
			t.Errorf("%s: device.revoked row = %+v", via, r)
		}
		wantDetail(t, via+" device.revoked", r.Detail, map[string]any{"via": "cap"})
		linked := e.audit("device.linked")
		if len(linked) != 1 || linked[0].ID < r.ID || linked[0].Detail["via"] != via {
			t.Errorf("%s: device.linked rows = %+v (the cap row is %d)", via, linked, r.ID)
		}
	}

	e, alex := newFlowEnv(t)
	e.addDevices(alex.User.ID, 19, 0)
	e.link(alex, ipA)
	if n := len(e.devices(alex.User.ID)); n != 20 {
		t.Errorf("%d devices after the 20th link, want 20", n)
	}
	if calls := e.conns.take(); len(calls) != 0 {
		t.Errorf("ConnCloser calls %v for the 20th device, want none", calls)
	}
	if n := e.auditCount("device.revoked"); n != 0 {
		t.Errorf("%d device.revoked rows for the 20th device, want none", n)
	}
}
