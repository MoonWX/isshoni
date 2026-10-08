package httpapi

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// Tests of the push subscription and preference endpoints with a fake Push (README S53; 03 §12.4.6, §15 "Push").

// pushEndpoint is the n-th subscription endpoint of a push service.
func pushEndpoint(n int) string {
	return fmt.Sprintf("https://push.example.net/send/dx1%04d", n)
}

// b64 is base64url without padding, the encoding of a browser's PushSubscription.toJSON().
func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// newPushKeys returns the keys of a browser's subscription: a P-256 public key as an uncompressed point (65 bytes)
// and a 16-byte auth secret.
func newPushKeys(t *testing.T) api.PushKeys {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, pushAuthBytes)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	return api.PushKeys{P256dh: b64(priv.PublicKey().Bytes()), Auth: b64(secret)}
}

// subscribe sends POST /api/v1/push/subscriptions.
func (f *authFixture) subscribe(c *http.Cookie, endpoint string, keys api.PushKeys, opts ...reqOpt) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.call(http.MethodPost, "/api/v1/push/subscriptions",
		jsonBody(f.t, api.PushSubscribeRequest{Endpoint: endpoint, Keys: keys}), append([]reqOpt{with(c)}, opts...)...)
}

// subscribed subscribes with new keys and returns the subscription's ID; the answer must be 201.
func (f *authFixture) subscribed(c *http.Cookie, endpoint string) string {
	f.t.Helper()
	return decodeBody[api.PushSubscribeResponse](f.t, f.subscribe(c, endpoint, newPushKeys(f.t)), http.StatusCreated).ID
}

// unsubscribe sends POST /api/v1/push/unsubscribe.
func (f *authFixture) unsubscribe(c *http.Cookie, endpoint string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.call(http.MethodPost, "/api/v1/push/unsubscribe", jsonBody(f.t, api.PushUnsubscribeRequest{Endpoint: endpoint}), with(c))
}

// pushTest sends POST /api/v1/push/test, as the SPA does: with the body {}.
func (f *authFixture) pushTest(c *http.Cookie) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.call(http.MethodPost, "/api/v1/push/test", `{}`, with(c))
}

// putPrefs sends PUT /api/v1/push/preferences with a raw JSON body.
func (f *authFixture) putPrefs(c *http.Cookie, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.call(http.MethodPut, "/api/v1/push/preferences", body, with(c))
}

// subs lists the stored subscriptions that the filter selects, as 04's sender reads them.
func (f *authFixture) subs(filter store.PushFilter) []store.PushSubscription {
	f.t.Helper()
	var out []store.PushSubscription
	f.read(func(q *store.Q) error {
		var err error
		out, err = q.ListPushSubscriptions(filter)
		return err
	})
	return out
}

// subsOf lists a user's stored subscriptions, oldest first.
func (f *authFixture) subsOf(userID string) []store.PushSubscription {
	f.t.Helper()
	return f.subs(store.PushFilter{UserIDs: []store.UserID{store.UserID(userID)}})
}

// endpointsOf returns the endpoints of subscriptions, in their order.
func endpointsOf(subs []store.PushSubscription) []string {
	out := make([]string, 0, len(subs))
	for _, s := range subs {
		out = append(out, s.Endpoint)
	}
	return out
}

// sessionID returns the ID of the session behind a cookie.
func (f *authFixture) sessionID(c *http.Cookie) store.SessionID {
	f.t.Helper()
	me := decodeBody[api.Me](f.t, f.call(http.MethodGet, "/api/v1/me", "", with(c)), http.StatusOK)
	if me.Session == nil {
		f.t.Fatal("GET /me has no session")
	}
	return store.SessionID(me.Session.ID)
}

// wantPushRejected checks a 422 push_endpoint_rejected with one reason, in the golden shape of 03 §12.2.
func wantPushRejected(t *testing.T, rec *httptest.ResponseRecorder, reason api.PushRejectReason) {
	t.Helper()
	wantError(t, rec, http.StatusUnprocessableEntity, api.CodePushEndpointRejected)
	if got, want := rec.Body.String(), `{"error":{"code":"push_endpoint_rejected","params":{"reason":"`+string(reason)+`"}}}`+"\n"; got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
}

// TestPushSubscribeOverREST is 03 §15's push case: an endpoint over 2048 bytes → 422 too_long; bad keys → 422
// bad_keys; a fake ValidateEndpoint rejection → 422 with its reason; a valid subscription → 201, and the same
// endpoint again → 200; the 11th subscription evicts the oldest; logout deletes the session's subscriptions.
func TestPushSubscribeOverREST(t *testing.T) {
	f := newAuthFixture(t)
	admin, cookie, sam, samCookie := f.adminAndMember()
	count := f.counter()
	keys := newPushKeys(t)
	point, _ := base64.RawURLEncoding.DecodeString(keys.P256dh)
	secret, _ := base64.RawURLEncoding.DecodeString(keys.Auth)

	// 1. The endpoint is at most 2048 bytes, else too_long, whatever else is wrong with the body.
	long := "https://push.example.net/" + strings.Repeat("x", pushEndpointMaxBytes-len("https://push.example.net/")+1)
	wantPushRejected(t, f.subscribe(cookie, long, keys), api.PushRejectReasonTooLong)
	wantPushRejected(t, f.subscribe(cookie, long, api.PushKeys{}), api.PushRejectReasonTooLong)

	// 2. p256dh decodes to 65 bytes starting with 0x04 and auth to 16 bytes, else bad_keys.
	compressed := append([]byte{0x02}, point[1:]...)
	for name, bad := range map[string]api.PushKeys{
		"no keys":                    {},
		"no p256dh":                  {Auth: keys.Auth},
		"no auth":                    {P256dh: keys.P256dh},
		"p256dh of 64 bytes":         {P256dh: b64(point[:64]), Auth: keys.Auth},
		"p256dh of 66 bytes":         {P256dh: b64(append(bytes.Clone(point), 0)), Auth: keys.Auth},
		"p256dh not uncompressed":    {P256dh: b64(compressed), Auth: keys.Auth},
		"p256dh compressed 33 bytes": {P256dh: b64(compressed[:33]), Auth: keys.Auth},
		"auth of 15 bytes":           {P256dh: keys.P256dh, Auth: b64(secret[:15])},
		"auth of 17 bytes":           {P256dh: keys.P256dh, Auth: b64(append(bytes.Clone(secret), 0))},
		"p256dh not base64":          {P256dh: strings.Repeat("!", 87), Auth: keys.Auth},
		"auth not base64":            {P256dh: keys.P256dh, Auth: "not base64 at all!!!!!"},
		"standard base64":            {P256dh: base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{0x04, 0xfb, 0xff}, 22)[:65]), Auth: keys.Auth},
		"wrong padding":              {P256dh: keys.P256dh + "==", Auth: keys.Auth},
		"auth in hex":                {P256dh: keys.P256dh, Auth: fmt.Sprintf("%x", secret)},
	} {
		rec := f.subscribe(cookie, pushEndpoint(0), bad)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: status %d (%s), want 422", name, rec.Code, rec.Body)
		}
		wantPushRejected(t, rec, api.PushRejectReasonBadKeys)
	}
	// The body's shape is checked first: 04's rules never saw an endpoint so far.
	if got := f.push.takeValidated(); len(got) != 0 {
		t.Errorf("ValidateEndpoint was called for a body of the wrong shape: %q", got)
	}

	// 3. Then Push.ValidateEndpoint: its reason is the answer. An empty endpoint is 04's to refuse too.
	f.push.validate = func(endpoint string) error {
		reason := api.PushRejectReasonPrivateAddress
		if endpoint == "" {
			reason = api.PushRejectReasonNotHTTPS
		}
		return &api.Error{Code: api.CodePushEndpointRejected, Params: map[string]any{api.ParamReason: string(reason)}}
	}
	wantPushRejected(t, f.subscribe(cookie, "https://localhost/push", keys), api.PushRejectReasonPrivateAddress)
	wantPushRejected(t, f.subscribe(cookie, "", keys), api.PushRejectReasonNotHTTPS)
	if got := f.push.takeValidated(); fmt.Sprint(got) != "[https://localhost/push ]" {
		t.Errorf("ValidateEndpoint saw %q", got)
	}
	// An error of another kind (its resolver, a cancelled request) is 500 internal, never a subscription.
	f.push.validate = func(string) error { return errors.New("resolver exploded") }
	wantError(t, f.subscribe(cookie, pushEndpoint(0), keys), http.StatusInternalServerError, api.CodeInternal)
	f.push.validate = nil
	f.push.takeValidated()
	if n := count("SELECT count(*) FROM push_subscriptions"); n != 0 {
		t.Fatalf("%d subscriptions after refused requests, want none", n)
	}

	// A valid subscription → 201 {id}. The row is bound to this session and named after the browser.
	rec := f.subscribe(cookie, pushEndpoint(0), keys)
	id := decodeBody[api.PushSubscribeResponse](t, rec, http.StatusCreated).ID
	if got, want := rec.Body.String(), `{"id":"`+id+`"}`+"\n"; got != want || len(id) != 12 {
		t.Errorf("POST /push/subscriptions body = %s, want %s with a 12-character ID", got, want)
	}
	session := f.sessionID(cookie)
	rows := f.subsOf(admin.ID)
	if len(rows) != 1 || !rows[0].CreatedAt.Equal(f.clk.now()) || !rows[0].LastSuccessAt.IsZero() {
		t.Fatalf("stored subscriptions = %+v, want one, created now and never used", rows)
	}
	rows[0].CreatedAt = time.Time{}
	if rows[0] != (store.PushSubscription{ID: store.PushSubID(id), UserID: store.UserID(admin.ID), SessionID: session,
		Endpoint: pushEndpoint(0), P256dh: keys.P256dh, Auth: keys.Auth, Name: "Chrome on Windows"}) {
		t.Errorf("stored subscription = %+v", rows[0])
	}
	// An endpoint of exactly 2048 bytes fits.
	fits := long[:pushEndpointMaxBytes]
	decodeBody[api.PushSubscribeResponse](t, f.subscribe(cookie, fits, keys), http.StatusCreated)
	if got := f.push.takeValidated(); len(got) != 2 || got[0] != pushEndpoint(0) || got[1] != fits {
		t.Errorf("ValidateEndpoint saw %d endpoints, want the two that were stored", len(got))
	}
	if rec := f.unsubscribe(cookie, fits); rec.Code != http.StatusNoContent {
		t.Fatalf("unsubscribe = %d %s", rec.Code, rec.Body)
	}

	// The same endpoint again → 200 with the same ID: the SPA posts it on every app start. New keys replace the
	// old ones; padded keys are fine and are stored without the padding.
	f.clk.advance(time.Hour)
	keys2 := newPushKeys(t)
	padded := api.PushKeys{P256dh: keys2.P256dh + "=", Auth: keys2.Auth + "=="}
	rec = f.subscribe(cookie, pushEndpoint(0), padded)
	if got := decodeBody[api.PushSubscribeResponse](t, rec, http.StatusOK).ID; got != id {
		t.Errorf("the same endpoint again: id %q, want %q", got, id)
	}
	if rows = f.subsOf(admin.ID); len(rows) != 1 || rows[0].P256dh != keys2.P256dh || rows[0].Auth != keys2.Auth ||
		!rows[0].CreatedAt.Equal(f.clk.now()) || rows[0].SessionID != session {
		t.Errorf("subscription after a second post = %+v", rows)
	}

	// After a new login the same browser posts again: the row moves to the new session, so it lives as long as it.
	loginB := f.login("Alex", testPassword, addrB)
	cookieB := f.sessionCookie(loginB, thirtyDays)
	sessionB := f.sessionID(cookieB)
	if got := decodeBody[api.PushSubscribeResponse](t, f.subscribe(cookieB, pushEndpoint(0), keys2), http.StatusOK).ID; got != id {
		t.Errorf("the same endpoint from a new session: id %q, want %q", got, id)
	}
	if rows = f.subsOf(admin.ID); len(rows) != 1 || rows[0].SessionID != sessionB || sessionB == session {
		t.Errorf("subscription after a post from a new session = %+v, want it bound to %s", rows, sessionB)
	}
	// Another account in the same browser: the endpoint is the browser's, so the row goes with it.
	if got := decodeBody[api.PushSubscribeResponse](t, f.subscribe(samCookie, pushEndpoint(0), keys2), http.StatusOK).ID; got != id {
		t.Errorf("the same endpoint from another account: id %q, want %q", got, id)
	}
	if len(f.subsOf(admin.ID)) != 0 || len(f.subsOf(sam.ID)) != 1 {
		t.Errorf("after another account posted the endpoint: Alex has %d, Sam %d; want 0 and 1", len(f.subsOf(admin.ID)), len(f.subsOf(sam.ID)))
	}

	// Each user keeps at most 10: the 11th evicts the one that was posted longest ago.
	for i := 1; i <= maxPushSubscriptions; i++ {
		f.clk.advance(time.Minute)
		f.subscribed(cookie, pushEndpoint(i))
	}
	if rows = f.subsOf(admin.ID); len(rows) != maxPushSubscriptions || rows[0].Endpoint != pushEndpoint(1) {
		t.Fatalf("Alex has %d subscriptions, the oldest %q", len(rows), rows[0].Endpoint)
	}
	// Endpoint 1 is posted again (an app start): it is the newest now, and endpoint 2 the oldest.
	f.clk.advance(time.Minute)
	decodeBody[api.PushSubscribeResponse](t, f.subscribe(cookie, pushEndpoint(1), newPushKeys(t)), http.StatusOK)
	f.clk.advance(time.Minute)
	f.subscribed(cookieB, pushEndpoint(11))
	got := endpointsOf(f.subsOf(admin.ID))
	want := []string{pushEndpoint(3), pushEndpoint(4), pushEndpoint(5), pushEndpoint(6), pushEndpoint(7), pushEndpoint(8),
		pushEndpoint(9), pushEndpoint(10), pushEndpoint(1), pushEndpoint(11)}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("after the 11th subscription Alex has %q, want %q", got, want)
	}
	// The cap is per user: Sam's subscription is untouched.
	if rows = f.subsOf(sam.ID); len(rows) != 1 || rows[0].Endpoint != pushEndpoint(0) {
		t.Errorf("Sam's subscriptions = %+v", rows)
	}

	// Logout deletes the session's subscriptions, and only those (03 §7.7).
	if rec := f.call(http.MethodPost, "/api/v1/auth/logout", `{}`, with(cookieB)); rec.Code != http.StatusNoContent {
		t.Fatalf("logout = %d %s", rec.Code, rec.Body)
	}
	if got := endpointsOf(f.subsOf(admin.ID)); len(got) != 9 || strings.Contains(fmt.Sprint(got), pushEndpoint(11)) {
		t.Errorf("after the logout of session B Alex has %q, want the 9 of the first session", got)
	}
	if n := count("SELECT count(*) FROM push_subscriptions WHERE session_id = ?", string(sessionB)); n != 0 {
		t.Errorf("%d subscriptions of the session that logged out", n)
	}
	// Nothing about push is audited or announced (03 §10, §12.5), and no endpoint is in the audit log.
	if n := count("SELECT count(*) FROM audit_log WHERE instr(detail, 'push.example.net') > 0 OR instr(action, 'push') > 0"); n != 0 {
		t.Errorf("%d audit rows about push", n)
	}
}

// TestPushUnsubscribeOverREST: POST /push/unsubscribe deletes the caller's subscription by its endpoint, and is
// idempotent (03 §12.3 #27).
func TestPushUnsubscribeOverREST(t *testing.T) {
	f := newAuthFixture(t)
	admin, cookie, sam, samCookie := f.adminAndMember()
	f.subscribed(cookie, pushEndpoint(1))
	f.subscribed(cookie, pushEndpoint(2))
	f.subscribed(samCookie, pushEndpoint(3))

	// Somebody else's endpoint: 204, and their subscription stays.
	if rec := f.unsubscribe(samCookie, pushEndpoint(1)); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("unsubscribe = %d %q, want 204 without a body", rec.Code, rec.Body)
	}
	if got := endpointsOf(f.subsOf(admin.ID)); len(got) != 2 {
		t.Fatalf("Alex's subscriptions after Sam unsubscribed one of them = %q", got)
	}
	// One's own: gone. Again: still 204.
	for range 2 {
		if rec := f.unsubscribe(cookie, pushEndpoint(1)); rec.Code != http.StatusNoContent {
			t.Fatalf("unsubscribe = %d %s", rec.Code, rec.Body)
		}
		if got := endpointsOf(f.subsOf(admin.ID)); fmt.Sprint(got) != fmt.Sprint([]string{pushEndpoint(2)}) {
			t.Fatalf("Alex's subscriptions = %q, want only the second", got)
		}
	}
	// Nothing to look for is 204 too: an unknown endpoint, none, and one longer than any stored endpoint.
	for _, body := range []string{`{"endpoint":"https://push.example.net/unknown"}`, `{}`, `{"endpoint":""}`,
		`{"endpoint":"https://push.example.net/` + strings.Repeat("x", pushEndpointMaxBytes) + `"}`} {
		if rec := f.call(http.MethodPost, "/api/v1/push/unsubscribe", body, with(cookie)); rec.Code != http.StatusNoContent {
			t.Errorf("unsubscribe %.60s = %d %s, want 204", body, rec.Code, rec.Body)
		}
	}
	wantError(t, f.call(http.MethodPost, "/api/v1/push/unsubscribe", `{"endpoint":7}`, with(cookie)), http.StatusBadRequest, api.CodeBadRequest)
	wantError(t, f.call(http.MethodPost, "/api/v1/push/unsubscribe", ``, with(cookie)), http.StatusBadRequest, api.CodeBadRequest)
	if len(f.subsOf(admin.ID)) != 1 || len(f.subsOf(sam.ID)) != 1 {
		t.Errorf("subscriptions at the end: Alex %d, Sam %d; want 1 and 1", len(f.subsOf(admin.ID)), len(f.subsOf(sam.ID)))
	}
}

// TestPushTestOverREST: POST /push/test sends to this session's subscriptions through Push.SendTest, at most once
// per 10 s per user (03 §12.3 #28, §12.4.6, §7.3 push-test).
func TestPushTestOverREST(t *testing.T) {
	f := newAuthFixture(t)
	admin, cookie, _, samCookie := f.adminAndMember()

	// No subscription in this session → 404 not_found. That costs no token and sends nothing.
	for range 3 {
		rec := f.pushTest(cookie)
		wantError(t, rec, http.StatusNotFound, api.CodeNotFound)
		if got, want := rec.Body.String(), `{"error":{"code":"not_found"}}`+"\n"; got != want {
			t.Errorf("body = %s, want %s", got, want)
		}
	}

	// Two subscriptions in this session, one in another session of the same user, one of another user.
	idA1 := f.subscribed(cookie, pushEndpoint(1))
	f.clk.advance(time.Second)
	idA2 := f.subscribed(cookie, pushEndpoint(2))
	cookieB := f.sessionCookie(f.login("Alex", testPassword, addrB), thirtyDays)
	f.subscribed(cookieB, pushEndpoint(3))
	f.subscribed(samCookie, pushEndpoint(4))
	if got := f.push.takeTests(); len(got) != 0 {
		t.Fatalf("SendTest was called %d times before any test could be sent", len(got))
	}

	// 202 with no body; the push service gets this session's subscriptions, with what it needs to send.
	rec := f.pushTest(cookie)
	if rec.Code != http.StatusAccepted || rec.Body.Len() != 0 || rec.Header().Get("Content-Type") != "" {
		t.Fatalf("POST /push/test = %d %q (Content-Type %q), want 202 without a body", rec.Code, rec.Body, rec.Header().Get("Content-Type"))
	}
	tests := f.push.takeTests()
	if len(tests) != 1 || len(tests[0]) != 2 || tests[0][0].ID != store.PushSubID(idA1) || tests[0][1].ID != store.PushSubID(idA2) ||
		tests[0][0].Endpoint != pushEndpoint(1) || tests[0][0].UserID != store.UserID(admin.ID) || tests[0][0].SessionID != f.sessionID(cookie) ||
		len(tests[0][0].P256dh) != 87 || len(tests[0][0].Auth) != 22 {
		t.Errorf("SendTest got %+v, want the two subscriptions of this session", tests)
	}

	// A second test within 10 s → 429 with Retry-After, from any session of that user. Nothing is sent.
	for _, c := range []*http.Cookie{cookie, cookieB} {
		rec = f.pushTest(c)
		e := wantError(t, rec, http.StatusTooManyRequests, api.CodeRateLimited)
		if e.RetryAfter != 10 || rec.Header().Get("Retry-After") != "10" {
			t.Errorf("rate-limited test: retryAfter %d, Retry-After %q; want 10", e.RetryAfter, rec.Header().Get("Retry-After"))
		}
		if got, want := rec.Body.String(), `{"error":{"code":"rate_limited","retryAfter":10}}`+"\n"; got != want {
			t.Errorf("body = %s, want %s", got, want)
		}
	}
	// The bucket is per user: Sam's test goes out, to Sam's browser.
	if rec = f.pushTest(samCookie); rec.Code != http.StatusAccepted {
		t.Fatalf("Sam's test = %d %s", rec.Code, rec.Body)
	}
	if tests = f.push.takeTests(); len(tests) != 1 || len(tests[0]) != 1 || tests[0][0].Endpoint != pushEndpoint(4) {
		t.Errorf("SendTest got %+v, want only Sam's subscription", tests)
	}
	// 9.5 s later there is still no token; the wait is rounded up to whole seconds. At 10 s there is one.
	f.clk.advance(9500 * time.Millisecond)
	if e := wantError(t, f.pushTest(cookie), http.StatusTooManyRequests, api.CodeRateLimited); e.RetryAfter != 1 {
		t.Errorf("retryAfter half a second before the token = %d, want 1", e.RetryAfter)
	}
	f.clk.advance(500 * time.Millisecond)
	if rec = f.pushTest(cookieB); rec.Code != http.StatusAccepted {
		t.Fatalf("a test 10 s after the last = %d %s", rec.Code, rec.Body)
	}
	if tests = f.push.takeTests(); len(tests) != 1 || len(tests[0]) != 1 || tests[0][0].Endpoint != pushEndpoint(3) {
		t.Errorf("SendTest got %+v, want only the subscription of session B", tests)
	}
	wantError(t, f.pushTest(cookie), http.StatusTooManyRequests, api.CodeRateLimited)

	// SendTest only queues. Its errors go out as they are: 04's full queue is 503 server_busy with Retry-After.
	// That test sent nothing, so it does not count against the next one.
	f.clk.advance(pushTestEvery)
	f.push.sendErr = &api.Error{Code: api.CodeServerBusy, RetryAfter: 5}
	rec = f.pushTest(cookie)
	if e := wantError(t, rec, http.StatusServiceUnavailable, api.CodeServerBusy); e.RetryAfter != 5 || rec.Header().Get("Retry-After") != "5" {
		t.Errorf("server_busy: retryAfter %d, Retry-After %q", e.RetryAfter, rec.Header().Get("Retry-After"))
	}
	f.push.sendErr = &api.Error{Code: api.CodeServerShutdown, RetryAfter: 5}
	wantError(t, f.pushTest(cookie), http.StatusServiceUnavailable, api.CodeServerShutdown)
	f.push.sendErr = errors.New("queue exploded")
	wantError(t, f.pushTest(cookie), http.StatusInternalServerError, api.CodeInternal)
	f.push.sendErr = nil
	if rec = f.pushTest(cookie); rec.Code != http.StatusAccepted {
		t.Fatalf("a test right after three that failed to queue = %d %s, want 202", rec.Code, rec.Body)
	}
	if tests = f.push.takeTests(); len(tests) != 4 {
		t.Errorf("SendTest was called %d times, want 4", len(tests))
	}
	// The body is not read: none at all is fine too.
	f.clk.advance(pushTestEvery)
	if rec = f.call(http.MethodPost, "/api/v1/push/test", "", with(cookie)); rec.Code != http.StatusAccepted {
		t.Errorf("a test without a body = %d %s, want 202", rec.Code, rec.Body)
	}

	// After the logout of the session its subscriptions are gone: the user's other session has nothing to test.
	f.clk.advance(pushTestEvery)
	if rec := f.call(http.MethodPost, "/api/v1/auth/logout", `{}`, with(cookieB)); rec.Code != http.StatusNoContent {
		t.Fatalf("logout = %d %s", rec.Code, rec.Body)
	}
	cookieC := f.sessionCookie(f.login("Alex", testPassword, addrC), thirtyDays)
	wantError(t, f.pushTest(cookieC), http.StatusNotFound, api.CodeNotFound)
}

// TestPushPreferencesOverREST: GET and PUT /push/preferences round-trip, and what is stored is what 04's sender
// filters its recipients by: ListPushSubscriptions with PushFilter.Pref (03 §12.4.6, §18 slice 12).
func TestPushPreferencesOverREST(t *testing.T) {
	f := newAuthFixture(t)
	admin, cookie, sam, samCookie := f.adminAndMember()
	_, token := f.createInvite(cookie, `{}`)
	kim, kimCookie := f.join(token, "Kim", clientIP(2))
	count := f.counter()
	const (
		defaults = `{"shareStarted":"all","adminAlerts":true}` + "\n"
		off      = `{"shareStarted":"off","adminAlerts":false}` + "\n"
	)
	get := func(c *http.Cookie) string {
		t.Helper()
		rec := f.call(http.MethodGet, "/api/v1/push/preferences", "", with(c))
		decodeBody[api.PushPreferences](t, rec, http.StatusOK)
		return rec.Body.String()
	}

	// A user who never chose has the defaults (03 §14), and no row.
	if got := get(cookie); got != defaults {
		t.Errorf("GET /push/preferences of a new user = %s, want %s", got, defaults)
	}
	if n := count("SELECT count(*) FROM push_preferences"); n != 0 {
		t.Errorf("%d preference rows after a GET, want none", n)
	}

	// PUT with the same shape → 200 with what is stored; GET returns it. The choices are per user.
	rec := f.putPrefs(cookie, `{"shareStarted":"off","adminAlerts":false}`)
	if got := decodeBody[api.PushPreferences](t, rec, http.StatusOK); got != (api.PushPreferences{ShareStarted: api.ShareStartedPrefOff}) || rec.Body.String() != off {
		t.Errorf("PUT /push/preferences = %s, want %s", rec.Body, off)
	}
	if got := get(cookie); got != off {
		t.Errorf("GET after the PUT = %s, want %s", got, off)
	}
	if got := get(samCookie); got != defaults {
		t.Errorf("Sam's preferences after Alex changed theirs = %s, want %s", got, defaults)
	}
	// Each combination round-trips, the defaults too. Unknown fields are ignored (03 §12.1).
	for _, body := range []string{
		`{"shareStarted":"all","adminAlerts":false}`,
		`{"shareStarted":"off","adminAlerts":true}`,
		`{"shareStarted":"all","adminAlerts":true}`,
	} {
		rec := f.putPrefs(cookie, strings.TrimSuffix(body, "}")+`,"later":"field"}`)
		if rec.Code != http.StatusOK || rec.Body.String() != body+"\n" {
			t.Errorf("PUT %s = %d %s", body, rec.Code, rec.Body)
		}
		if got := get(cookie); got != body+"\n" {
			t.Errorf("GET after PUT %s = %s", body, got)
		}
	}
	if n := count("SELECT count(*) FROM push_preferences WHERE user_id = ?", admin.ID); n != 1 {
		t.Errorf("%d preference rows of Alex, want 1", n)
	}

	// A PUT replaces both choices: a value that is not one, or a field left out, is 422 with a code per field,
	// and nothing changes.
	f.putPrefs(cookie, `{"shareStarted":"off","adminAlerts":false}`)
	for body, fields := range map[string]string{
		`{"shareStarted":"sometimes","adminAlerts":true}`: "map[shareStarted:invalid]",
		`{"shareStarted":"ALL","adminAlerts":true}`:       "map[shareStarted:invalid]",
		`{"shareStarted":"","adminAlerts":true}`:          "map[shareStarted:invalid]",
		`{"adminAlerts":true}`:                            "map[shareStarted:required]",
		`{"shareStarted":"all"}`:                          "map[adminAlerts:required]",
		`{"shareStarted":null,"adminAlerts":null}`:        "map[adminAlerts:required shareStarted:required]",
		`{}`:                                "map[adminAlerts:required shareStarted:required]",
		`{"shareStarted":"nope","other":1}`: "map[adminAlerts:required shareStarted:invalid]",
	} {
		e := wantError(t, f.putPrefs(cookie, body), http.StatusUnprocessableEntity, api.CodeValidationFailed)
		if fmt.Sprint(e.Fields) != fields {
			t.Errorf("PUT %s: fields = %v, want %s", body, e.Fields, fields)
		}
	}
	for _, body := range []string{`{"shareStarted":"all","adminAlerts":"yes"}`, `{"shareStarted":1,"adminAlerts":true}`, `[]`, `null x`, ``} {
		wantError(t, f.putPrefs(cookie, body), http.StatusBadRequest, api.CodeBadRequest)
	}
	if got := get(cookie); got != off {
		t.Errorf("preferences after refused PUTs = %s, want %s", got, off)
	}
	f.putPrefs(cookie, `{"shareStarted":"all","adminAlerts":true}`)

	// The sender's filter. Everyone has a subscription; Alex is the only admin.
	f.subscribed(cookie, pushEndpoint(1))
	f.subscribed(samCookie, pushEndpoint(2))
	f.subscribed(kimCookie, pushEndpoint(3))
	shareStarted := func() string {
		t.Helper()
		return fmt.Sprint(usersOf(f.subs(store.PushFilter{Pref: store.PrefShareStarted}), admin, sam, kim))
	}
	adminAlerts := func() string {
		t.Helper()
		return fmt.Sprint(usersOf(f.subs(store.PushFilter{Pref: store.PrefAdminAlerts, AdminsOnly: true}), admin, sam, kim))
	}
	if got := shareStarted(); got != "[Alex Kim Sam]" {
		t.Errorf("share_started recipients with the defaults = %s, want everyone", got)
	}
	if got := adminAlerts(); got != "[Alex]" {
		t.Errorf("admin_alerts recipients with the defaults = %s, want the admin", got)
	}
	// Sam turns the share notifications off, and admin alerts too (which matter for admins only).
	if rec := f.putPrefs(samCookie, `{"shareStarted":"off","adminAlerts":false}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body)
	}
	if got := shareStarted(); got != "[Alex Kim]" {
		t.Errorf("share_started recipients after Sam turned it off = %s", got)
	}
	if got := adminAlerts(); got != "[Alex]" {
		t.Errorf("admin_alerts recipients after a member turned them off = %s", got)
	}
	// The admin turns the alerts off and keeps the share notifications.
	f.putPrefs(cookie, `{"shareStarted":"all","adminAlerts":false}`)
	if a, s := adminAlerts(), shareStarted(); a != "[]" || s != "[Alex Kim]" {
		t.Errorf("after the admin turned the alerts off: admin_alerts %s, share_started %s", a, s)
	}
	// And the other way round.
	f.putPrefs(cookie, `{"shareStarted":"off","adminAlerts":true}`)
	if a, s := adminAlerts(), shareStarted(); a != "[Alex]" || s != "[Kim]" {
		t.Errorf("after the admin swapped the two: admin_alerts %s, share_started %s", a, s)
	}
	// Sam turns the share notifications on again. Without a filter the choices play no part.
	f.putPrefs(samCookie, `{"shareStarted":"all","adminAlerts":false}`)
	if got := shareStarted(); got != "[Kim Sam]" {
		t.Errorf("share_started recipients after Sam turned it on again = %s", got)
	}
	if got := fmt.Sprint(usersOf(f.subs(store.PushFilter{}), admin, sam, kim)); got != "[Alex Kim Sam]" {
		t.Errorf("recipients without a preference filter = %s, want everyone", got)
	}
	// Nothing about the preferences is audited or announced (03 §10, §12.5).
	if n := count("SELECT count(*) FROM audit_log WHERE instr(action, 'push') > 0"); n != 0 {
		t.Errorf("%d audit rows about push", n)
	}
}

// usersOf returns the sorted usernames of the users that subscriptions belong to.
func usersOf(subs []store.PushSubscription, users ...api.User) []string {
	names := map[string]string{}
	for _, u := range users {
		names[u.ID] = u.Username
	}
	seen := map[string]bool{}
	for _, s := range subs {
		seen[names[string(s.UserID)]] = true
	}
	return keysOf(seen)
}

// TestPushUnavailable: with push off (Deps.Push is nil, or it has no VAPID key: the rule by which GET /info omits
// push) subscribe and test answer 503 push_unavailable. Unsubscribe and the preferences still work.
func TestPushUnavailable(t *testing.T) {
	for name, p := range map[string]Push{"nil": nil, "no key": &fakePush{}} {
		t.Run(name, func(t *testing.T) {
			f := newAuthFixtureDeps(t, func(d *Deps) { d.Push = p })
			admin, cookie := f.setupAdmin("Alex")
			// A subscription from the time push was on.
			keys, session := newPushKeys(t), f.sessionID(cookie)
			f.write(func(q *store.Q) error {
				_, err := q.UpsertPushSubscription(&store.PushSubscription{UserID: store.UserID(admin.ID), SessionID: session,
					Endpoint: pushEndpoint(1), P256dh: keys.P256dh, Auth: keys.Auth, Name: "Chrome on Windows"})
				return err
			})
			for _, rec := range []*httptest.ResponseRecorder{
				f.subscribe(cookie, pushEndpoint(2), newPushKeys(t)),
				f.subscribe(cookie, strings.Repeat("x", pushEndpointMaxBytes+1), api.PushKeys{}),
				f.call(http.MethodPost, "/api/v1/push/subscriptions", `not json`, with(cookie)),
				f.pushTest(cookie),
			} {
				wantError(t, rec, http.StatusServiceUnavailable, api.CodePushUnavailable)
				if got, want := rec.Body.String(), `{"error":{"code":"push_unavailable"}}`+"\n"; got != want {
					t.Errorf("body = %s, want %s", got, want)
				}
			}
			if info := decodeBody[api.Info](t, f.call(http.MethodGet, "/api/v1/info", ""), http.StatusOK); info.Push != nil {
				t.Errorf("GET /info has push %+v while the push endpoints answer push_unavailable", info.Push)
			}
			if got := endpointsOf(f.subsOf(admin.ID)); len(got) != 1 {
				t.Fatalf("subscriptions = %q, want the one from before", got)
			}
			// The caller's own data stays reachable.
			if rec := f.putPrefs(cookie, `{"shareStarted":"off","adminAlerts":true}`); rec.Code != http.StatusOK {
				t.Errorf("PUT /push/preferences with push off = %d %s", rec.Code, rec.Body)
			}
			prefs := decodeBody[api.PushPreferences](t, f.call(http.MethodGet, "/api/v1/push/preferences", "", with(cookie)), http.StatusOK)
			if prefs != (api.PushPreferences{ShareStarted: api.ShareStartedPrefOff, AdminAlerts: true}) {
				t.Errorf("GET /push/preferences with push off = %+v", prefs)
			}
			if rec := f.unsubscribe(cookie, pushEndpoint(1)); rec.Code != http.StatusNoContent || len(f.subsOf(admin.ID)) != 0 {
				t.Errorf("unsubscribe with push off = %d %s, %d subscriptions left", rec.Code, rec.Body, len(f.subsOf(admin.ID)))
			}
		})
	}
}

// TestPushRoutes: the push routes sit behind the /api/v1 chain like every other, as User routes.
func TestPushRoutes(t *testing.T) {
	f := newAuthFixture(t)
	_, cookie, _, member := f.adminAndMember()
	keys := newPushKeys(t)
	subscribe := jsonBody(t, api.PushSubscribeRequest{Endpoint: pushEndpoint(1), Keys: keys})
	f.subscribed(member, pushEndpoint(9))

	unsafe := []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/push/subscriptions", subscribe},
		{http.MethodPost, "/api/v1/push/unsubscribe", `{"endpoint":"` + pushEndpoint(9) + `"}`},
		{http.MethodPost, "/api/v1/push/test", `{}`},
		{http.MethodPut, "/api/v1/push/preferences", `{"shareStarted":"off","adminAlerts":false}`},
	}
	for _, u := range unsafe {
		// Not signed in: 401.
		wantError(t, f.call(u.method, u.path, u.body), http.StatusUnauthorized, api.CodeUnauthenticated)
		// A cross-site request with the member's cookie changes nothing: 403 csrf_failed. Without a JSON
		// Content-Type: 415. No bearer tokens in M1.
		rec := f.call(u.method, u.path, u.body, with(member), header("Sec-Fetch-Site", "cross-site"), header("Origin", "https://evil.example"))
		wantError(t, rec, http.StatusForbidden, api.CodeCSRFFailed)
		wantError(t, f.call(u.method, u.path, u.body, with(member), header("Content-Type", "text/plain")), http.StatusUnsupportedMediaType, api.CodeUnsupportedMediaType)
		wantError(t, f.call(u.method, u.path, u.body, with(member), header("Content-Type", "")), http.StatusUnsupportedMediaType, api.CodeUnsupportedMediaType)
		rec = f.call(u.method, u.path, u.body, with(member), header("Authorization", "Bearer isa_"+strings.Repeat("A", 43)))
		wantError(t, rec, http.StatusUnauthorized, api.CodeUnauthenticated)
	}
	wantError(t, f.call(http.MethodGet, "/api/v1/push/preferences", ""), http.StatusUnauthorized, api.CodeUnauthenticated)
	prefs := decodeBody[api.PushPreferences](t, f.call(http.MethodGet, "/api/v1/push/preferences", "", with(member)), http.StatusOK)
	if got := f.subs(store.PushFilter{}); len(got) != 1 || got[0].Endpoint != pushEndpoint(9) || prefs != (api.PushPreferences{ShareStarted: api.ShareStartedPrefAll, AdminAlerts: true}) ||
		len(f.push.takeTests()) != 0 {
		t.Errorf("refused requests changed something: subscriptions %q, preferences %+v", endpointsOf(got), prefs)
	}

	// Members and admins alike use them: these are User routes.
	for _, c := range []*http.Cookie{member, cookie} {
		if rec := f.putPrefs(c, `{"shareStarted":"off","adminAlerts":false}`); rec.Code != http.StatusOK {
			t.Errorf("PUT /push/preferences = %d %s", rec.Code, rec.Body)
		}
	}

	// Other methods: JSON 405 with Allow. Unknown paths: JSON 404.
	for path, allow := range map[string]string{
		"/api/v1/push/subscriptions": "POST",
		"/api/v1/push/unsubscribe":   "POST",
		"/api/v1/push/test":          "POST",
		"/api/v1/push/preferences":   "GET, HEAD, PUT",
	} {
		rec := f.call(http.MethodDelete, path, ``, with(member))
		wantError(t, rec, http.StatusMethodNotAllowed, api.CodeMethodNotAllowed)
		if got := rec.Header().Get("Allow"); got != allow {
			t.Errorf("DELETE %s: Allow %q, want %q", path, got, allow)
		}
	}
	for _, path := range []string{"/api/v1/push", "/api/v1/push/", "/api/v1/push/subscriptions/abc", "/api/v1/push/preferences/x"} {
		wantError(t, f.call(http.MethodGet, path, "", with(member)), http.StatusNotFound, api.CodeNotFound)
	}
	// The body limit is the 64 KiB of every route outside /auth.
	big := strings.Repeat("x", otherBodyLimit)
	wantError(t, f.call(http.MethodPost, "/api/v1/push/subscriptions", `{"endpoint":"`+big+`"}`, with(member)), http.StatusRequestEntityTooLarge, api.CodePayloadTooLarge)
	wantError(t, f.call(http.MethodPost, "/api/v1/push/unsubscribe", `{"endpoint":"`+big+`"}`, with(member)), http.StatusRequestEntityTooLarge, api.CodePayloadTooLarge)
	wantError(t, f.call(http.MethodPut, "/api/v1/push/preferences", `{"shareStarted":"`+big+`"}`, with(member)), http.StatusRequestEntityTooLarge, api.CodePayloadTooLarge)
	wantPushRejected(t, f.subscribe(member, big[:authBodyLimit+1], keys), api.PushRejectReasonTooLong)
}

// TestPushCallerGone: the session is checked again in the transaction of a change (callerLive). The fake auth
// service lets a principal in whose session the store does not have: one that was revoked after the chain's
// (cached) authentication. It gets 401, not a failed foreign key, and nothing is stored.
func TestPushCallerGone(t *testing.T) {
	push := &fakePush{key: testVAPIDKey}
	f := newAPIFixture(t, func(d *Deps) { d.Push = push })
	gone := withCookie("user", sameOriginJSON...)
	body := jsonBody(t, api.PushSubscribeRequest{Endpoint: pushEndpoint(1), Keys: newPushKeys(t)})
	wantError(t, f.do(http.MethodPost, "/api/v1/push/subscriptions", strings.NewReader(body), gone...), http.StatusUnauthorized, api.CodeUnauthenticated)
	wantError(t, f.do(http.MethodPut, "/api/v1/push/preferences", strings.NewReader(`{"shareStarted":"off","adminAlerts":false}`), gone...),
		http.StatusUnauthorized, api.CodeUnauthenticated)
	// Reading and deleting need no such check: there is nothing of that caller.
	rec := f.do(http.MethodGet, "/api/v1/push/preferences", nil, withCookie("user", "Sec-Fetch-Site", "same-origin")...)
	if rec.Code != http.StatusOK || rec.Body.String() != `{"shareStarted":"all","adminAlerts":true}`+"\n" {
		t.Errorf("GET /push/preferences = %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(http.MethodPost, "/api/v1/push/unsubscribe", strings.NewReader(`{"endpoint":"`+pushEndpoint(1)+`"}`), gone...); rec.Code != http.StatusNoContent {
		t.Errorf("unsubscribe = %d %s", rec.Code, rec.Body)
	}
	wantError(t, f.do(http.MethodPost, "/api/v1/push/test", strings.NewReader(`{}`), gone...), http.StatusNotFound, api.CodeNotFound)
	if len(push.takeTests()) != 0 {
		t.Error("SendTest was called for a caller without a subscription")
	}
}

// TestPushKey is the key rule of 03 §12.4.6: base64url of exactly n bytes, with or without padding, stored without.
func TestPushKey(t *testing.T) {
	sixteen := bytes.Repeat([]byte{0xfb, 0xef}, 8) // encodes with '-' and '_' in base64url, '+' and '/' in base64
	raw := b64(sixteen)
	if !strings.ContainsAny(raw, "-_") {
		t.Fatalf("the fixture %q has no base64url-only character", raw)
	}
	for _, c := range []struct {
		name, in string
		n        int
		ok       bool
	}{
		{"unpadded", raw, 16, true},
		{"padded", base64.URLEncoding.EncodeToString(sixteen), 16, true},
		{"one = too few", raw + "=", 16, false},
		{"too many =", raw + "===", 16, false},
		{"standard alphabet", base64.RawStdEncoding.EncodeToString(sixteen), 16, false},
		{"standard alphabet, padded", base64.StdEncoding.EncodeToString(sixteen), 16, false},
		{"wrong length", raw, 15, false},
		{"one byte short", b64(sixteen[:15]), 16, false},
		{"one byte long", b64(append(bytes.Clone(sixteen), 1)), 16, false},
		{"empty", "", 16, false},
		{"spaces around", " " + raw + " ", 16, false},
		{"a line break inside", raw[:10] + "\n" + raw[10:], 16, false}, // encoding/base64 skips these on its own
		{"a line break at the end", raw + "\r\n", 16, false},
		{"not base64", strings.Repeat("!", 22), 16, false},
	} {
		canonical, decoded, ok := pushKey(c.in, c.n)
		if ok != c.ok {
			t.Errorf("%s: pushKey(%q, %d) ok = %v, want %v", c.name, c.in, c.n, ok, c.ok)
			continue
		}
		if ok && (canonical != raw || !bytes.Equal(decoded, sixteen)) {
			t.Errorf("%s: pushKey(%q) = %q, % x; want %q", c.name, c.in, canonical, decoded, raw)
		}
		if !ok && (canonical != "" || decoded != nil) {
			t.Errorf("%s: pushKey(%q) = %q, % x for a refused key", c.name, c.in, canonical, decoded)
		}
	}
	// Spare bits in the last character are dropped: what is stored is the canonical spelling.
	loose := raw[:len(raw)-1] + "x" // 22 characters carry 132 bits; the last 4 are spare
	if canonical, _, ok := pushKey(loose, 16); ok && canonical == loose {
		t.Errorf("pushKey(%q) kept a non-canonical spelling", loose)
	}
}

// TestPushTestLimiter is the push-test bucket of 03 §7.3: per user, a burst of 1 and one token every 10 s.
func TestPushTestLimiter(t *testing.T) {
	clk := newManualClock()
	l := newPushTestLimiter(clk.now)
	const alex, sam = store.UserID("a1a1a1a1a1a1"), store.UserID("u1u1u1u1u1u1")

	at, wait := l.take(alex)
	if wait != 0 || !at.Equal(clk.now()) {
		t.Fatalf("first take = %v, %v; want the token", at, wait)
	}
	if _, wait := l.take(alex); wait != pushTestEvery {
		t.Errorf("second take at once: wait %v, want %v", wait, pushTestEvery)
	}
	if _, wait := l.take(sam); wait != 0 {
		t.Errorf("another user's take: wait %v, want the token", wait)
	}
	clk.advance(pushTestEvery - time.Millisecond)
	if _, wait := l.take(alex); wait != time.Millisecond {
		t.Errorf("take 1 ms before the token is back: wait %v", wait)
	}
	clk.advance(time.Millisecond)
	at, wait = l.take(alex)
	if wait != 0 {
		t.Fatalf("take after 10 s: wait %v, want the token", wait)
	}
	// A refund gives exactly that token back; a stale one does nothing.
	l.refund(alex, at.Add(-time.Second))
	if _, wait := l.take(alex); wait == 0 {
		t.Fatal("a refund for another take gave the token back")
	}
	l.refund(alex, at)
	if _, wait := l.take(alex); wait != 0 {
		t.Errorf("take after a refund: wait %v, want the token", wait)
	}
	l.refund(sam, at) // Sam's token was taken at another time
	l.refund("nobody000000", at)

	// The map keeps only the users of the last windows: old entries are swept by a later take.
	for i := range 1000 {
		l.take(store.UserID(fmt.Sprintf("user%08d", i)))
	}
	clk.advance(pushTestEvery)
	if _, wait := l.take(sam); wait != 0 {
		t.Errorf("Sam's take a window later: wait %v", wait)
	}
	l.mu.Lock()
	n := len(l.taken)
	l.mu.Unlock()
	if n != 1 {
		t.Errorf("%d entries after a sweep, want only the one just taken", n)
	}
	// A clock that is set back does not lock anyone out.
	clk.advance(-time.Hour)
	if _, wait := l.take(sam); wait != 0 {
		t.Errorf("take after the clock went back an hour: wait %v, want the token", wait)
	}

	for wait, want := range map[time.Duration]int{
		time.Nanosecond: 1, time.Millisecond: 1, time.Second: 1, time.Second + 1: 2, 9500 * time.Millisecond: 10, 10 * time.Second: 10,
	} {
		if got := retryAfterSeconds(wait); got != want {
			t.Errorf("retryAfterSeconds(%v) = %d, want %d", wait, got, want)
		}
	}
}

// FuzzPushKey: a key is accepted only as base64url of exactly n bytes, and what is stored for it is the one
// canonical spelling of those bytes, which is accepted again as itself.
func FuzzPushKey(f *testing.F) {
	sixteen := bytes.Repeat([]byte{0xfb, 0xef}, 8)
	for _, s := range []string{b64(sixteen), base64.URLEncoding.EncodeToString(sixteen), base64.StdEncoding.EncodeToString(sixteen),
		b64(sixteen) + "=", b64(sixteen)[:21] + "x", "", "=", "====", " ", "a\nb", "tBHItJI5svbpez7KI4CCXg",
		b64(append([]byte{pushPointPrefix}, bytes.Repeat([]byte{7}, 64)...))} {
		f.Add(s, pushAuthBytes)
		f.Add(s, pushP256dhBytes)
	}
	f.Fuzz(func(t *testing.T, in string, n int) {
		canonical, raw, ok := pushKey(in, n)
		if !ok {
			if canonical != "" || raw != nil {
				t.Fatalf("pushKey(%q, %d) = %q, % x for a refused key", in, n, canonical, raw)
			}
			return
		}
		if len(raw) != n || canonical != b64(raw) || strings.ContainsAny(canonical, "=+/\r\n ") {
			t.Fatalf("pushKey(%q, %d) = %q, % x: want %d bytes and their base64url without padding", in, n, canonical, raw, n)
		}
		if strings.ContainsAny(in, "+/\r\n ") || (strings.HasSuffix(in, "=") && len(in)%4 != 0) {
			t.Fatalf("pushKey(%q, %d) accepted a character outside base64url, or a padding that is not whole", in, n)
		}
		again, raw2, ok := pushKey(canonical, n)
		if !ok || again != canonical || !bytes.Equal(raw2, raw) {
			t.Fatalf("pushKey(%q, %d) = %q, but that gives %q, % x, %v", in, n, canonical, again, raw2, ok)
		}
	})
}
