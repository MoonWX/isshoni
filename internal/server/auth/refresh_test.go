package auth

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// The refresh_token grant at the service (M2 02 §16.1's tokens row, the refresh half; README WA39): rotation, reuse
// revocation with its audit row, alert, cache and ConnCloser steps, every grace case of §6.2 (the same exchangeId
// any time, no id once within 120 s, another id), the superseded child that a grace re-issue keeps as a spent row
// and that trips reuse detection when it is presented later, and FuzzDeviceToken.

// ---- helpers ----

// newExchangeID draws an exchangeId as the client does: 16 random bytes, 22 characters.
func newExchangeID() string { return newToken(exchangeIDBytes) }

// refreshFrom is one refresh_token grant from ip.
func (e *svcEnv) refreshFrom(token, exchangeID, ip string) (api.DeviceTokenResponse, error) {
	return e.svc.DeviceToken(context.Background(), api.DeviceTokenRequest{GrantType: api.GrantTypeRefreshToken,
		RefreshToken: token, ExchangeID: exchangeID}, meta(ip))
}

// mustRefresh is refreshFrom that fails the test on error.
func (e *svcEnv) mustRefresh(token, exchangeID, ip string) api.DeviceTokenResponse {
	e.t.Helper()
	res, err := e.refreshFrom(token, exchangeID, ip)
	if err != nil {
		e.t.Fatalf("refresh from %s: %v", ip, err)
	}
	return res
}

// refreshRow reads the row of an unexpired refresh token, spent or not; ok is false when there is none.
func (e *svcEnv) refreshRow(tok string) (store.DeviceToken, bool) {
	e.t.Helper()
	var out store.DeviceToken
	found := false
	e.read(func(q *store.Q) error {
		t, _, _, err := q.DeviceTokenByHash(e.svc.keys.deviceHash(tok), tokenKindRefresh, e.clk.now())
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		out, found = t, err == nil
		return err
	})
	return out, found
}

// accessRow reads the row of an unexpired access token; ok is false when there is none.
func (e *svcEnv) accessRow(tok string) (store.DeviceToken, bool) {
	e.t.Helper()
	var out store.DeviceToken
	found := false
	e.read(func(q *store.Q) error {
		t, _, _, err := q.DeviceTokenByHash(e.svc.keys.deviceHash(tok), tokenKindAccess, e.clk.now())
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		out, found = t, err == nil
		return err
	})
	return out, found
}

// at moves the test clock to t0 + d.
func (e *svcEnv) at(t0 time.Time, d time.Duration) {
	e.t.Helper()
	if now := e.clk.now(); t0.Add(d).Before(now) {
		e.t.Fatalf("the clock is at %v, past t0 + %v", now.Sub(t0), d)
	}
	e.advance(t0.Add(d).Sub(e.clk.now()))
}

// wantPair fails the test unless res is a well-formed 200 answer of the grant for the app.
func wantPair(t *testing.T, res api.DeviceTokenResponse, app linked) {
	t.Helper()
	if !accessTokenWellFormed(res.AccessToken) || !refreshTokenWellFormed(res.RefreshToken) ||
		res.TokenType != "Bearer" || res.ExpiresIn != 900 || res.RefreshExpiresIn != 7776000 {
		t.Errorf("answer = %+v", res)
	}
	wantUser := api.User{ID: string(app.user.ID), Username: app.user.Username, Role: api.Role(app.user.Role)}
	if res.Device != (api.DeviceRef{ID: string(app.device), Name: app.name}) || res.User != wantUser {
		t.Errorf("answer device %+v, user %+v", res.Device, res.User)
	}
}

// wantReused checks a reuse (M2 02 §6.2, §8) presented from ip: invalid_grant {reason: reused} as a
// *RefreshReusedError naming the user and the device; the device gone with every token and its push subscriptions;
// one device.refresh_reused row (actor system with the presenter's IP, target the device, detail {}); the
// refresh_token_reused alert (actor system, target the username); one ConnCloser call for the device; the warn line
// with user_id, device_id and remote_ip, and no secret or device name in the log.
func (e *svcEnv) wantReused(err error, app linked, ip string, secrets ...string) {
	e.t.Helper()
	t := e.t
	ae := wantCode(t, err, api.CodeInvalidGrant)
	if !reflect.DeepEqual(ae.Params, map[string]any{"reason": "reused"}) {
		t.Errorf("invalid_grant params = %v, want {reason: reused}", ae.Params)
	}
	var re *RefreshReusedError
	if !errors.As(err, &re) || re.UserID != app.user.ID || re.DeviceID != app.device {
		t.Errorf("the error %v is not a RefreshReusedError for %s/%s", err, app.user.ID, app.device)
	}
	if e.deviceRow(app.device) != nil {
		t.Error("the device is still there")
	}
	for what, n := range map[string]int{
		"device tokens":      e.rawInt("SELECT count(*) FROM device_tokens WHERE device_id = ?", string(app.device)),
		"push subscriptions": e.rawInt("SELECT count(*) FROM push_subscriptions WHERE device_id = ?", string(app.device)),
	} {
		if n != 0 {
			t.Errorf("%d %s of the revoked device are left", n, what)
		}
	}
	rows := e.audit("device.refresh_reused")
	if len(rows) != 1 {
		t.Fatalf("%d device.refresh_reused rows, want 1", len(rows))
	}
	r := rows[0]
	if r.Actor != (store.Actor{Kind: store.ActorSystem, Name: "system", IP: ip}) || r.TargetKind != "device" ||
		r.TargetID != string(app.device) || r.TargetName != app.name || r.Outcome != "ok" || len(r.Detail) != 0 ||
		!r.At.Equal(e.clk.now()) {
		t.Errorf("device.refresh_reused row = %+v", r)
	}
	wantAlerts := []AdminAlert{{Kind: AlertRefreshTokenReused, Actor: "system", Target: app.user.Username, At: e.clk.now()}}
	if alerts := e.alerts.take(); !reflect.DeepEqual(alerts, wantAlerts) {
		t.Errorf("alerts = %+v, want %+v", alerts, wantAlerts)
	}
	wantCalls := []closedConn{{ConnSelector{UserID: app.user.ID, DeviceID: app.device}, ReasonDeviceRevoked}}
	if calls := e.conns.take(); !reflect.DeepEqual(calls, wantCalls) {
		t.Errorf("ConnCloser calls = %+v, want %+v", calls, wantCalls)
	}
	logs := e.logs.String()
	line := `level=WARN msg="refresh token reused, device revoked" component=auth user_id=` + string(app.user.ID) +
		" device_id=" + string(app.device) + " remote_ip=" + ip
	if !strings.Contains(logs, line) || !strings.Contains(logs, `msg="admin alert" component=auth kind=refresh_token_reused`) {
		t.Errorf("no warn line %q and admin alert line in the log:\n%s", line, logs)
	}
	for _, s := range append(secrets, app.access, app.refresh, app.name, app.user.Username) {
		if s != "" && strings.Contains(logs, s) {
			t.Errorf("the log names %q:\n%s", s, logs)
		}
	}
}

// ---- rotation ----

// TestRefreshRotates: a refresh spends the presented token (spent_at, replaced_by the new refresh token,
// exchange_hash the HMAC of the exchangeId), issues a pair with issued_by = the presented token, records the
// device's use with the request's IP and app version, and writes no audit row.
func TestRefreshRotates(t *testing.T) {
	e, _, app := newLinkEnv(t)
	e.advance(10 * time.Minute)
	now := e.clk.now()
	x := newExchangeID()
	res, err := e.svc.DeviceToken(context.Background(), api.DeviceTokenRequest{GrantType: api.GrantTypeRefreshToken,
		RefreshToken: app.refresh, ExchangeID: x, AppVersion: "0.2.1"}, meta(ipC))
	if err != nil {
		t.Fatal(err)
	}
	wantPair(t, res, app)
	if res.AccessToken == app.access || res.RefreshToken == app.refresh {
		t.Fatal("the refresh answered the old tokens")
	}
	p, ok := e.refreshRow(app.refresh)
	if !ok || !p.SpentAt.Equal(now) || !bytes.Equal(p.ReplacedBy, e.svc.keys.deviceHash(res.RefreshToken)) ||
		!bytes.Equal(p.ExchangeHash, e.svc.keys.deviceHash(x)) || p.IssuedBy != nil {
		t.Errorf("the presented token's row = %+v (found %v)", p, ok)
	}
	c, ok := e.refreshRow(res.RefreshToken)
	if !ok || !c.SpentAt.IsZero() || !bytes.Equal(c.IssuedBy, p.TokenHash) || !c.CreatedAt.Equal(now) ||
		!c.ExpiresAt.Equal(now.Add(refreshTokenTTL)) || c.DeviceID != app.device || c.ExchangeHash != nil {
		t.Errorf("the new refresh token's row = %+v (found %v)", c, ok)
	}
	a, ok := e.accessRow(res.AccessToken)
	if !ok || !bytes.Equal(a.IssuedBy, p.TokenHash) || !a.CreatedAt.Equal(now) || !a.ExpiresAt.Equal(now.Add(accessTokenTTL)) {
		t.Errorf("the new access token's row = %+v (found %v)", a, ok)
	}
	if d := e.deviceRow(app.device); d == nil || d.AppVersion != "0.2.1" || !d.LastSeenAt.Equal(now) || d.LastIP != ipC {
		t.Errorf("the device after the refresh = %+v", d)
	}
	// Both access tokens work: the old one has 5 minutes left.
	for _, tok := range []string{app.access, res.AccessToken} {
		if p, err := e.svc.AuthenticateBearerToken(context.Background(), tok); err != nil || p.DeviceID != app.device {
			t.Errorf("an access token after the refresh: %+v, %v", p, err)
		}
	}
	if n := e.rawInt("SELECT count(*) FROM audit_log WHERE action LIKE 'device.%' AND action NOT IN ('device.approved', 'device.linked')"); n != 0 {
		t.Errorf("%d device audit rows for a refresh, want none", n)
	}
	if calls, alerts := e.conns.take(), e.alerts.take(); len(calls) != 0 || len(alerts) != 0 {
		t.Errorf("a refresh closed %v and raised %v", calls, alerts)
	}

	// Without an appVersion the stored one stays; without an exchangeId exchange_hash is NULL.
	e.advance(time.Minute)
	res2 := e.mustRefresh(res.RefreshToken, "", ipA)
	if d := e.deviceRow(app.device); d.AppVersion != "0.2.1" || d.LastIP != ipA {
		t.Errorf("after a refresh without appVersion: %+v", d)
	}
	if c, _ := e.refreshRow(res.RefreshToken); c.ExchangeHash != nil || !bytes.Equal(c.ReplacedBy,
		e.svc.keys.deviceHash(res2.RefreshToken)) {
		t.Errorf("a token spent without an exchangeId: %+v", c)
	}
	logs := e.logs.String()
	for _, s := range []string{app.refresh, res.RefreshToken, res.AccessToken, x, app.name} {
		if strings.Contains(logs, s) {
			t.Errorf("the log names %q:\n%s", s, logs)
		}
	}
}

// TestRefreshTrimsAccessTokens: a device keeps its 5 newest access tokens (M2 02 §6.7); the sixth refresh deletes
// the oldest, the link's, and the refresh drops the device from the bearer cache, so the deleted token is refused at
// once.
func TestRefreshTrimsAccessTokens(t *testing.T) {
	e, _, app := newLinkEnv(t)
	ctx := context.Background()
	if _, err := e.svc.AuthenticateBearerToken(ctx, app.access); err != nil {
		t.Fatal(err)
	}
	tokens := []string{app.access}
	refresh := app.refresh
	for range 5 {
		e.advance(time.Second)
		res := e.mustRefresh(refresh, newExchangeID(), ipA)
		tokens, refresh = append(tokens, res.AccessToken), res.RefreshToken
	}
	if n := e.rawInt("SELECT count(*) FROM device_tokens WHERE device_id = ? AND kind = 'access'", string(app.device)); n != 5 {
		t.Fatalf("%d access tokens after 5 refreshes, want 5", n)
	}
	if _, err := e.svc.AuthenticateBearerToken(ctx, app.access); !api.IsCode(err, api.CodeInvalidToken) {
		t.Errorf("the link's access token after it was trimmed: %v, want invalid_token", err)
	}
	for _, tok := range tokens[1:] {
		if _, err := e.svc.AuthenticateBearerToken(ctx, tok); err != nil {
			t.Errorf("a kept access token: %v", err)
		}
	}
	// Refresh tokens are never trimmed: the spent chain stays for reuse detection.
	if n := e.rawInt("SELECT count(*) FROM device_tokens WHERE device_id = ? AND kind = 'refresh'", string(app.device)); n != 6 {
		t.Errorf("%d refresh tokens, want 6 (5 spent and the live one)", n)
	}
}

// TestRefreshValidation: the optional fields are checked first (422, one answer for both), then the refresh token's
// shape (400 invalid_grant without params, before any DB access); an unknown or expired token, or one of a user who
// is not active, is invalid_grant without params. None of them changes a row.
func TestRefreshValidation(t *testing.T) {
	e, _, app := newLinkEnv(t)
	x := newExchangeID()
	nonCanonical := x[:21] + "B" // 16 bytes leave 4 bits in the 22nd character, which must be zero
	if x[21] == 'B' {
		nonCanonical = x[:21] + "C"
	}
	before := e.rowCounts()
	for name, tc := range map[string]struct {
		in     api.DeviceTokenRequest
		fields map[string]string
	}{
		"exchangeId of 21":       {api.DeviceTokenRequest{ExchangeID: x[:21]}, map[string]string{"exchangeId": "invalid"}},
		"exchangeId of 23":       {api.DeviceTokenRequest{ExchangeID: x + "A"}, map[string]string{"exchangeId": "invalid"}},
		"exchangeId padded":      {api.DeviceTokenRequest{ExchangeID: x[:20] + "=="}, map[string]string{"exchangeId": "invalid"}},
		"exchangeId base64":      {api.DeviceTokenRequest{ExchangeID: strings.Repeat("/", 22)}, map[string]string{"exchangeId": "invalid"}},
		"exchangeId noncanon":    {api.DeviceTokenRequest{ExchangeID: nonCanonical}, map[string]string{"exchangeId": "invalid"}},
		"exchangeId of 32 bytes": {api.DeviceTokenRequest{ExchangeID: newToken(32)}, map[string]string{"exchangeId": "invalid"}},
		"appVersion with space":  {api.DeviceTokenRequest{AppVersion: "0.2 1"}, map[string]string{"appVersion": "invalid"}},
		"appVersion of 33":       {api.DeviceTokenRequest{AppVersion: strings.Repeat("1", 33)}, map[string]string{"appVersion": "too_long"}},
		"both, and a bad token": {api.DeviceTokenRequest{ExchangeID: "x", AppVersion: "ü", RefreshToken: "nope"},
			map[string]string{"exchangeId": "invalid", "appVersion": "invalid"}},
	} {
		tc.in.GrantType = api.GrantTypeRefreshToken
		if tc.in.RefreshToken == "" {
			tc.in.RefreshToken = app.refresh
		}
		_, err := e.svc.DeviceToken(context.Background(), tc.in, meta(ipA))
		if ae := wantCode(t, err, api.CodeValidationFailed); !reflect.DeepEqual(ae.Fields, tc.fields) {
			t.Errorf("%s: fields %v, want %v", name, ae.Fields, tc.fields)
		}
	}
	tok := strings.TrimPrefix(app.refresh, refreshTokenPrefix)
	malformed := map[string]string{
		"empty":            "",
		"an access token":  app.access,
		"no prefix":        tok,
		"42 characters":    refreshTokenPrefix + tok[:42],
		"44 characters":    refreshTokenPrefix + tok + "A",
		"padding":          refreshTokenPrefix + tok[:42] + "=",
		"upper-case":       "ISR_" + tok,
		"a device code":    newToken(deviceCodeBytes),
		"a trailing space": app.refresh + " ",
	}
	wantPlain := func(name string, err error) {
		t.Helper()
		if ae := wantCode(t, err, api.CodeInvalidGrant); ae.Params != nil {
			t.Errorf("%s: invalid_grant params %v, want none", name, ae.Params)
		}
	}
	for name, bad := range malformed {
		_, err := e.refreshFrom(bad, x, ipA)
		wantPlain(name, err)
	}
	_, err := e.refreshFrom(refreshTokenPrefix+newToken(deviceTokenBytes), x, ipA)
	wantPlain("an unknown token", err)
	if after := e.rowCounts(); fmt.Sprint(after) != fmt.Sprint(before) || e.deviceRow(app.device) == nil {
		t.Fatalf("refused refreshes changed rows: %v → %v", before, after)
	}
	if p, _ := e.refreshRow(app.refresh); !p.SpentAt.IsZero() {
		t.Fatalf("a refused refresh spent the token: %+v", p)
	}

	// A user who is not active (no admin path leaves devices behind, so the status is set directly).
	e.write(func(q *store.Q) error { return q.SetStatus(app.user.ID, store.StatusDisabled, e.clk.now()) })
	_, err = e.refreshFrom(app.refresh, x, ipA)
	wantPlain("a disabled user's token", err)
	e.write(func(q *store.Q) error { return q.SetStatus(app.user.ID, store.StatusActive, e.clk.now()) })

	// 90 days after its issue the token is unknown.
	e.advance(refreshTokenTTL)
	_, err = e.refreshFrom(app.refresh, x, ipA)
	wantPlain("an expired token", err)
	if n := e.auditCount("device.refresh_reused"); n != 0 {
		t.Errorf("%d reuse rows, want none", n)
	}

	// No DB access for any of the shape checks.
	if err := e.db.Close(); err != nil {
		t.Fatal(err)
	}
	for name, bad := range malformed {
		_, err := e.refreshFrom(bad, x, ipA)
		wantPlain(name+" on a closed store", err)
	}
	_, err = e.refreshFrom(app.refresh, "bad", ipA)
	wantCode(t, err, api.CodeValidationFailed)
	_, err = e.refreshFrom(app.refresh, x, ipA)
	wantCode(t, err, api.CodeInternal)
}

// ---- reuse ----

// TestRefreshReuse: a spent token whose child was used revokes the device when it comes back, with or without its
// own exchangeId: the device and every token go, device.refresh_reused is written, the alert raised, and only after
// the commit the cache forgets the device and ConnCloser closes its connections. The chain the other holder had is
// dead with the device.
func TestRefreshReuse(t *testing.T) {
	for _, withOwnID := range []bool{false, true} {
		t.Run(fmt.Sprintf("own exchangeId %v", withOwnID), func(t *testing.T) {
			e, _, app := newLinkEnv(t)
			ctx := context.Background()
			e.write(func(q *store.Q) error {
				sub := store.PushSubscription{UserID: app.user.ID, DeviceID: app.device,
					Endpoint: "https://push.example/device", P256dh: "p", Auth: "a", Name: "isshoni for Windows"}
				_, err := q.UpsertPushSubscription(&sub)
				return err
			})
			x0 := newExchangeID()
			r1 := e.mustRefresh(app.refresh, x0, ipA) // P0 → P1
			e.advance(5 * time.Minute)
			r2 := e.mustRefresh(r1.RefreshToken, newExchangeID(), ipA) // P1 → P2: P0's child was used
			e.advance(5 * time.Minute)
			if _, err := e.svc.AuthenticateBearerToken(ctx, r2.AccessToken); err != nil { // cached now
				t.Fatal(err)
			}

			var early []string
			e.conns.during = func(ConnSelector, string) {
				if e.deviceRow(app.device) != nil {
					early = append(early, "the device row was there")
				}
				if _, _, ok := e.svc.bearers.byDevice(app.device, e.clk.now()); ok {
					early = append(early, "the bearer cache held the device")
				}
				e.alerts.mu.Lock()
				raised := len(e.alerts.alerts)
				e.alerts.mu.Unlock()
				if raised != 0 {
					early = append(early, "the alert came first")
				}
			}
			replayID := ""
			if withOwnID {
				replayID = x0
			}
			_, err := e.refreshFrom(app.refresh, replayID, ipC)
			e.conns.during = nil
			e.wantReused(err, app, ipC, r1.RefreshToken, r2.RefreshToken, r2.AccessToken, x0)
			if len(early) != 0 {
				t.Errorf("when ConnCloser ran: %v", early)
			}
			// The other holder's chain died with the device: its tokens are unknown now (no second reuse).
			_, err = e.refreshFrom(r2.RefreshToken, "", ipA)
			if ae := wantCode(t, err, api.CodeInvalidGrant); ae.Params != nil {
				t.Errorf("the live chain's token after the reuse: %+v", ae)
			}
			_, err = e.svc.AuthenticateBearerToken(ctx, r2.AccessToken)
			wantCode(t, err, api.CodeInvalidToken)
			if n := e.auditCount("device.refresh_reused"); n != 1 {
				t.Errorf("%d reuse rows, want 1", n)
			}
		})
	}
}

// ---- grace (M2 02 §6.2) ----

// TestRefreshGrace walks the grace cases of §6.2's table. Each case links an app, spends its first refresh token P
// at t0 (with an exchangeId X, or without one), and replays P at the given times: "same" sends X again, "none" no
// exchangeId, "other" a new one. A replay that takes a grace path answers a new pair, deletes the access token of
// the child it replaces and keeps that child as a spent row (replaced_by the new refresh token, exchange_hash NULL);
// any other replay is a reuse.
func TestRefreshGrace(t *testing.T) {
	type replay struct {
		at       time.Duration // after the spend
		exchange string        // same, none or other
		pair     bool          // a grace path, else a reuse
	}
	for _, tc := range []struct {
		name    string
		spendID bool // the spend sent X
		replays []replay
	}{
		{name: "the same exchangeId after 10 min, any number of times", spendID: true,
			replays: []replay{{10 * time.Minute, "same", true}, {20 * time.Minute, "same", true}, {89 * 24 * time.Hour, "same", true}}},
		{name: "no exchangeId at 100 s, once", spendID: true,
			replays: []replay{{100 * time.Second, "none", true}, {110 * time.Second, "none", false}}},
		{name: "no exchangeId at exactly 120 s", spendID: true, replays: []replay{{120 * time.Second, "none", true}}},
		{name: "no exchangeId at 121 s", spendID: true, replays: []replay{{121 * time.Second, "none", false}}},
		{name: "another exchangeId after 10 min", spendID: true, replays: []replay{{10 * time.Minute, "other", false}}},
		{name: "another exchangeId at 60 s takes the restart path", spendID: true,
			replays: []replay{{60 * time.Second, "other", true}, {70 * time.Second, "other", false}}},
		{name: "the restart path, then the same exchangeId", spendID: true,
			replays: []replay{{100 * time.Second, "none", true}, {10 * time.Minute, "same", true}}},
		{name: "no exchangeId never matches a NULL exchange_hash", spendID: false,
			replays: []replay{{10 * time.Minute, "none", false}}},
		{name: "an exchangeId never matches a NULL exchange_hash", spendID: false,
			replays: []replay{{10 * time.Minute, "other", false}}},
		{name: "a spend without exchangeId still has the restart path", spendID: false,
			replays: []replay{{30 * time.Second, "none", true}, {31 * time.Second, "none", false}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _, app := newLinkEnv(t)
			e.advance(time.Minute)
			t0 := e.clk.now()
			x := ""
			if tc.spendID {
				x = newExchangeID()
			}
			child := e.mustRefresh(app.refresh, x, ipA)
			p, _ := e.refreshRow(app.refresh)
			for i, r := range tc.replays {
				e.at(t0, r.at)
				id := map[string]string{"same": x, "none": "", "other": newExchangeID()}[r.exchange]
				res, err := e.refreshFrom(app.refresh, id, ipC)
				if !r.pair {
					e.wantReused(err, app, ipC, child.RefreshToken, child.AccessToken)
					if i != len(tc.replays)-1 {
						t.Fatal("a case goes on after a reuse")
					}
					return
				}
				if err != nil {
					t.Fatalf("replay %d at +%v (%s): %v, want a new pair", i, r.at, r.exchange, err)
				}
				wantPair(t, res, app)
				now := e.clk.now()
				// The child it replaces: a spent row now, superseded by the new refresh token; its access token gone.
				old, ok := e.refreshRow(child.RefreshToken)
				if !ok || !old.SpentAt.Equal(now) || !bytes.Equal(old.ReplacedBy, e.svc.keys.deviceHash(res.RefreshToken)) ||
					old.ExchangeHash != nil {
					t.Errorf("replay %d: the replaced child's row = %+v (found %v)", i, old, ok)
				}
				if _, ok := e.accessRow(child.AccessToken); ok {
					t.Errorf("replay %d: the replaced child's access token is still there", i)
				}
				if _, err := e.svc.AuthenticateBearerToken(context.Background(), child.AccessToken); !api.IsCode(err, api.CodeInvalidToken) {
					t.Errorf("replay %d: the replaced child's access token: %v, want invalid_token", i, err)
				}
				// P: still spent at t0 with its exchange_hash, now pointing at the new pair, which it issued.
				cur, _ := e.refreshRow(app.refresh)
				if !cur.SpentAt.Equal(p.SpentAt) || !bytes.Equal(cur.ExchangeHash, p.ExchangeHash) ||
					!bytes.Equal(cur.ReplacedBy, e.svc.keys.deviceHash(res.RefreshToken)) {
					t.Errorf("replay %d: P = %+v", i, cur)
				}
				n, _ := e.refreshRow(res.RefreshToken)
				a, _ := e.accessRow(res.AccessToken)
				if !bytes.Equal(n.IssuedBy, p.TokenHash) || !bytes.Equal(a.IssuedBy, p.TokenHash) || !n.CreatedAt.Equal(now) ||
					!n.SpentAt.IsZero() {
					t.Errorf("replay %d: the new pair's rows: refresh %+v, access %+v", i, n, a)
				}
				if _, err := e.svc.AuthenticateBearerToken(context.Background(), res.AccessToken); err != nil {
					t.Errorf("replay %d: the new access token: %v", i, err)
				}
				if e.deviceRow(app.device) == nil || e.auditCount("device.refresh_reused") != 0 || len(e.alerts.take()) != 0 {
					t.Fatalf("replay %d revoked the device", i)
				}
				child = res
			}
			// The last pair the grace issued is a working chain.
			e.advance(time.Second)
			e.mustRefresh(child.RefreshToken, newExchangeID(), ipC)
		})
	}
}

// TestRefreshGraceThenSupersededChild: the child that a grace re-issue replaced is kept as a spent row, so whoever
// presents it later trips reuse detection (M2 02 §6.2 "Why a grace path keeps C"): after a replay of P without an
// exchangeId at +100 s, after a retry with P's own exchangeId, and after the mirror case (another exchangeId within
// the window, the other holder rotating first), C presented at +101 s (inside the 120 s window) and at +15 min is
// 400 invalid_grant {reason: reused}, the device and every token gone, device.refresh_reused and the alert. The
// pair the grace issued dies with the device.
func TestRefreshGraceThenSupersededChild(t *testing.T) {
	for _, first := range []struct {
		name     string
		at       time.Duration
		exchange string
	}{
		{"no exchangeId at +100 s", 100 * time.Second, "none"},
		{"the same exchangeId at +100 s", 100 * time.Second, "same"},
		{"another exchangeId at +50 s", 50 * time.Second, "other"},
	} {
		for _, presentAt := range []time.Duration{101 * time.Second, 15 * time.Minute} {
			t.Run(fmt.Sprintf("%s, C at +%v", first.name, presentAt), func(t *testing.T) {
				e, _, app := newLinkEnv(t)
				e.advance(time.Minute)
				t0 := e.clk.now()
				x := newExchangeID()
				c := e.mustRefresh(app.refresh, x, ipA) // P → C
				e.at(t0, first.at)
				id := map[string]string{"same": x, "none": "", "other": newExchangeID()}[first.exchange]
				n := e.mustRefresh(app.refresh, id, ipB) // the grace: P → N, C superseded
				if row, ok := e.refreshRow(c.RefreshToken); !ok || row.SpentAt.IsZero() || row.ExchangeHash != nil {
					t.Fatalf("C after the grace = %+v (found %v), want a spent row", row, ok)
				}
				e.at(t0, presentAt)
				_, err := e.refreshFrom(c.RefreshToken, newExchangeID(), ipC)
				e.wantReused(err, app, ipC, c.RefreshToken, n.RefreshToken, n.AccessToken, x)
				_, err = e.svc.AuthenticateBearerToken(context.Background(), n.AccessToken)
				wantCode(t, err, api.CodeInvalidToken)
				_, err = e.refreshFrom(n.RefreshToken, "", ipB)
				if ae := wantCode(t, err, api.CodeInvalidGrant); ae.Params != nil {
					t.Errorf("the grace's pair after the reuse: %+v", ae)
				}
			})
		}
	}
}

// TestRefreshParallelRetries: retries of one refresh sent at once (the same exchangeId) all get a pair and never
// revoke the device; exactly one refresh token of the device is left unspent, the last one issued.
func TestRefreshParallelRetries(t *testing.T) {
	e, _, app := newLinkEnv(t)
	x := newExchangeID()
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() { _, errs[i] = e.refreshFrom(app.refresh, x, v4(i+1)) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("retry %d: %v", i, err)
		}
	}
	live := e.rawInt(`SELECT count(*) FROM device_tokens WHERE device_id = ? AND kind = 'refresh' AND spent_at IS NULL`,
		string(app.device))
	if live != 1 || e.deviceRow(app.device) == nil || e.auditCount("device.refresh_reused") != 0 {
		t.Errorf("after %d parallel retries: %d live refresh tokens, device %v; want 1 and the device", n, live,
			e.deviceRow(app.device))
	}
}

// TestRefreshParallelRestarts: replays without an exchangeId sent at once, all inside the spend's millisecond:
// exactly one takes the restart path (it is open once, even then), the next one is a reuse that revokes the device,
// and every later one finds an unknown token.
func TestRefreshParallelRestarts(t *testing.T) {
	e, _, app := newLinkEnv(t)
	const n = 6
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() { _, errs[i] = e.refreshFrom(app.refresh, "", v4(i+1)) })
	}
	wg.Wait()
	var pairs, reused, unknown int
	for _, err := range errs {
		var re *RefreshReusedError
		switch {
		case err == nil:
			pairs++
		case errors.As(err, &re):
			reused++
		case api.IsCode(err, api.CodeInvalidGrant):
			unknown++
		default:
			t.Errorf("unexpected answer %v", err)
		}
	}
	if pairs != 2 || reused != 1 || unknown != n-3 {
		t.Errorf("%d pairs, %d reuses, %d unknown; want the spend and one restart, one reuse, %d unknown", pairs, reused,
			unknown, n-3)
	}
}

// ---- fuzzing ----

// FuzzDeviceToken (M2 02 §16.1): random bodies of POST /api/v1/device/token, decoded as httpapi decodes them, never
// panic and never answer 5xx. In a refresh token or exchangeId, "<live>" stands for a live refresh token of a
// device made for the input, "<spent>" for its spent parent and "<spentx>" for the exchangeId that spent it, so that
// the fuzzer reaches the rotation, the grace paths and reuse.
func FuzzDeviceToken(f *testing.F) {
	for _, seed := range []string{
		`{"grantType":"refresh_token","refreshToken":"<live>","exchangeId":"q9V3xT0bLm2Rk7aZ4cWfYg","appVersion":"0.2.1"}`,
		`{"grantType":"refresh_token","refreshToken":"<spent>"}`,
		`{"grantType":"refresh_token","refreshToken":"<spent>","exchangeId":"<spentx>"}`,
		`{"grantType":"refresh_token","refreshToken":"<spent>","exchangeId":"q9V3xT0bLm2Rk7aZ4cWfYg"}`,
		`{"grantType":"refresh_token","refreshToken":"<live>","exchangeId":"short"}`,
		`{"grantType":"refresh_token","refreshToken":"isr_","appVersion":"0.0.0-dev+abc"}`,
		`{"grantType":"device_code","deviceCode":"EXAMPLEdeviceCODE0123456789abcdefghijklmnopq"}`,
		`{"grantType":"password"}`,
		`{}`,
	} {
		f.Add([]byte(seed))
	}
	e := newSvcEnv(f, func(o *Options) { o.Logger = slog.New(slog.DiscardHandler) })
	alex := e.addUser("Alex")
	var inputs uint32
	f.Fuzz(func(t *testing.T, body []byte) {
		var in api.DeviceTokenRequest
		if json.Unmarshal(body, &in) != nil {
			return
		}
		if strings.Contains(in.RefreshToken+in.ExchangeID, "<") {
			live, spent, spentX := e.fuzzChain(t, alex.ID)
			r := strings.NewReplacer("<live>", live, "<spentx>", spentX, "<spent>", spent)
			in.RefreshToken, in.ExchangeID = r.Replace(in.RefreshToken), r.Replace(in.ExchangeID)
		}
		// Each input from its own /64 of 2001:db8::/32, so that device-token-ip never throttles the fuzzer.
		inputs++
		a := [16]byte{0x20, 0x01, 0x0d, 0xb8, 15: 1}
		binary.BigEndian.PutUint32(a[4:8], inputs)
		ip := netip.AddrFrom16(a)
		_, err := e.svc.DeviceToken(context.Background(), in, ReqMeta{IP: ip})
		var ae *api.Error
		if err != nil && (!errors.As(err, &ae) || api.StatusOf(ae.Code) >= 500) {
			t.Fatalf("DeviceToken(%q) = %v, want no 5xx", body, err)
		}
		e.conns.take()
		e.alerts.take()
	})
}

// fuzzChain makes a device of user with a refresh chain: spent, exchanged with spentX for live, which is unspent.
func (e *svcEnv) fuzzChain(t *testing.T, user store.UserID) (live, spent, spentX string) {
	t.Helper()
	_, live = newDeviceTokenPair()
	_, spent = newDeviceTokenPair()
	spentX = newExchangeID()
	now := e.clk.now()
	k := e.svc.keys
	err := e.db.Write(context.Background(), func(q *store.Q) error {
		d := store.Device{UserID: user, Name: "Fuzz PC", ClientKind: "desktop", OS: "windows", AppVersion: "0.2.0",
			LinkedVia: "device_flow", CreatedAt: now}
		if err := q.CreateDevice(&d); err != nil {
			return err
		}
		for _, tok := range []store.DeviceToken{
			{TokenHash: k.deviceHash(spent), DeviceID: d.ID, Kind: tokenKindRefresh, CreatedAt: now,
				ExpiresAt: now.Add(refreshTokenTTL), SpentAt: now, ReplacedBy: k.deviceHash(live),
				ExchangeHash: k.deviceHash(spentX)},
			{TokenHash: k.deviceHash(live), DeviceID: d.ID, Kind: tokenKindRefresh, CreatedAt: now,
				ExpiresAt: now.Add(refreshTokenTTL), IssuedBy: k.deviceHash(spent)},
		} {
			if err := q.InsertDeviceToken(tok); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return live, spent, spentX
}
