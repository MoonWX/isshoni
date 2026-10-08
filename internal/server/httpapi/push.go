package httpapi

import (
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/auth"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// Web Push subscriptions and preferences (03 §12.3 #24, #27, #28, #50, #51; §12.4.6), User routes. This file owns
// the storage side: a browser's subscription is a row bound to the web session that posted it, so it goes with
// that session (logout, revocation, expiry), and the preferences are a row per user. Sending is 04's: its push
// service reaches the handlers as Deps.Push (endpoint validation, the test notification) and reads the same rows
// through the store (ListPushSubscriptions with PushFilter.Pref).
//
// When push is off (Deps.Push is nil, or it has no VAPID key, the same rule by which GET /info omits push), subscribe
// and test answer 503 push_unavailable. Unsubscribing and the preferences keep working: they only touch the caller's
// own stored data.

const (
	// pushEndpointMaxBytes is the longest endpoint URL a subscription may have (03 §12.4.6, §14).
	pushEndpointMaxBytes = 2048
	// maxPushSubscriptions is how many subscriptions a user keeps; the oldest goes (03 §12.4.6, §14).
	maxPushSubscriptions = 10
	// Sizes of a subscription's keys once decoded (RFC 8291): the browser's P-256 public key as an uncompressed
	// point, which starts with 0x04, and its auth secret.
	pushP256dhBytes = 65
	pushAuthBytes   = 16
	pushPointPrefix = 0x04

	// JSON names of the fields of PUT /api/v1/push/preferences, the keys of its validation_failed answer.
	fieldShareStarted = "shareStarted"
	fieldAdminAlerts  = "adminAlerts"
)

// pushRejected is 422 push_endpoint_rejected with params.reason, for the two body-shape rules of the subscribe
// handler; 04's ValidateEndpoint returns the same error for its URL and host rules.
func pushRejected(reason api.PushRejectReason) error {
	return &api.Error{Code: api.CodePushEndpointRejected, Params: map[string]any{api.ParamReason: string(reason)}}
}

// pushService returns 04's push service, or false when push is unavailable: it is off (Deps.Push is nil,
// push.enabled = false) or has no VAPID key. GET /info omits its push object by the same rule (03 §12.4.1), so a
// client that was told push exists is never answered push_unavailable for it, and the other way round.
func (a *API) pushService() (Push, bool) {
	if a.d.Push == nil || a.d.Push.VAPIDPublicKey() == "" {
		return nil, false
	}
	return a.d.Push, true
}

// pushKey decodes one key of a browser's PushSubscription.toJSON(): base64url, without padding as browsers write
// it, or with it. It returns the canonical form, base64url without padding, which is what the row stores and 04's
// sender reads; false when s is not base64url or does not decode to n bytes. Line breaks, which encoding/base64
// skips, are refused like any other character outside the alphabet.
func pushKey(s string, n int) (canonical string, raw []byte, ok bool) {
	if strings.ContainsAny(s, "\r\n") {
		return "", nil, false
	}
	enc := base64.RawURLEncoding
	if strings.HasSuffix(s, "=") {
		enc = base64.URLEncoding
	}
	raw, err := enc.DecodeString(s)
	if err != nil || len(raw) != n {
		return "", nil, false
	}
	return base64.RawURLEncoding.EncodeToString(raw), raw, true
}

// callerLive is the chain's authentication again, inside the transaction of a change: the caller's web session must
// still exist, else *api.Error{unauthenticated}. The chain let the request in on a principal that may be up to 30 s
// old (auth's session cache), and a logout, a revocation or the account's deletion may have removed the session
// since. Every way an account stops being active deletes its sessions (03 §7.7), so the session stands for the
// account too. Reading it in the Write also keeps the foreign keys of the push tables (session_id, user_id) from
// failing as a 500. M2 adds the device of a bearer principal here.
func callerLive(q *store.Q, p auth.Principal) error {
	sessions, err := q.ListSessions(p.UserID)
	if err != nil {
		return err
	}
	for i := range sessions {
		if sessions[i].ID == p.SessionID {
			return nil
		}
	}
	return api.NewError(api.CodeUnauthenticated)
}

// postPushSubscription is POST /api/v1/push/subscriptions (03 §12.3 #24, §12.4.6): the browser's
// PushSubscription.toJSON(), {endpoint, keys: {p256dh, auth}} → 201 {id} for a new subscription, 200 {id} when the
// endpoint was known. The SPA posts it on every app start once permission is granted, which keeps the binding
// current after a new login and makes the call idempotent.
//
// Validation, in this order; each failure is 422 push_endpoint_rejected with that params.reason:
//
//  1. the endpoint is at most 2048 bytes, else too_long;
//  2. p256dh decodes to 65 bytes starting with 0x04 and auth to 16 bytes, both base64url, else bad_keys;
//  3. Push.ValidateEndpoint (04): https, port 443, no userinfo, a DNS name that resolves to public addresses only.
//
// The handler checks the body's shape only; every URL and host rule is 04's, and an empty endpoint is refused there
// (not_https). ValidateEndpoint looks the host up in DNS, so it runs before the Write, never inside it.
//
// Storage: the row is upserted by endpoint. A known endpoint is rebound to the caller and this session, with the
// new keys and a fresh failure count; the name is the browser label of the User-Agent ("Chrome on Windows"). A user
// keeps at most 10 subscriptions: the ones that were posted longest ago go.
func (a *API) postPushSubscription(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteError(w, r, api.NewError(api.CodeUnauthenticated))
		return
	}
	push, ok := a.pushService()
	if !ok {
		WriteError(w, r, api.NewError(api.CodePushUnavailable))
		return
	}
	var req api.PushSubscribeRequest
	if err := DecodeJSON(w, r, &req, otherBodyLimit); err != nil {
		WriteError(w, r, err)
		return
	}
	if len(req.Endpoint) > pushEndpointMaxBytes {
		WriteError(w, r, pushRejected(api.PushRejectReasonTooLong))
		return
	}
	p256dh, point, okPoint := pushKey(req.Keys.P256dh, pushP256dhBytes)
	secret, _, okSecret := pushKey(req.Keys.Auth, pushAuthBytes)
	if !okPoint || !okSecret || point[0] != pushPointPrefix {
		WriteError(w, r, pushRejected(api.PushRejectReasonBadKeys))
		return
	}
	if err := push.ValidateEndpoint(r.Context(), req.Endpoint); err != nil {
		WriteError(w, r, err)
		return
	}
	sub := store.PushSubscription{
		UserID:    p.UserID,
		SessionID: p.SessionID,
		Endpoint:  req.Endpoint,
		P256dh:    p256dh,
		Auth:      secret,
		Name:      auth.DescribeUserAgent(r.UserAgent()),
		CreatedAt: a.d.Clock(),
	}
	created := false
	err := a.d.DB.Write(r.Context(), func(q *store.Q) error {
		if err := callerLive(q, p); err != nil {
			return err
		}
		var err error
		if created, err = q.UpsertPushSubscription(&sub); err != nil {
			return err
		}
		return q.TrimPushSubscriptions(p.UserID, maxPushSubscriptions)
	})
	if err != nil {
		WriteError(w, r, opErr("push subscribe", err))
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	WriteJSON(w, status, api.PushSubscribeResponse{ID: string(sub.ID)})
}

// postPushUnsubscribe is POST /api/v1/push/unsubscribe (03 §12.3 #27, §12.4.6): {endpoint} → 204. It deletes the
// caller's subscription with that endpoint. It is idempotent: an endpoint the caller has no subscription for,
// somebody else's among them, answers 204 too. It works while push is off, so a browser can always take its
// subscription back.
func (a *API) postPushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteError(w, r, api.NewError(api.CodeUnauthenticated))
		return
	}
	var req api.PushUnsubscribeRequest
	if err := DecodeJSON(w, r, &req, otherBodyLimit); err != nil {
		WriteError(w, r, err)
		return
	}
	// No stored endpoint is empty or longer than the subscribe limit: nothing to look for.
	if req.Endpoint != "" && len(req.Endpoint) <= pushEndpointMaxBytes {
		err := a.d.DB.Write(r.Context(), func(q *store.Q) error {
			_, err := q.DeletePushSubscriptionByEndpoint(p.UserID, req.Endpoint)
			return err
		})
		if err != nil {
			WriteError(w, r, opErr("push unsubscribe", err))
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// postPushTest is POST /api/v1/push/test (03 §12.3 #28, §12.4.6): 202 with no body. It sends a push.test
// notification to this session's subscriptions, that is, to this browser, through Push.SendTest, which only queues
// (04 §14.2). The body ({}) is not read. In order:
//
//  1. push off → 503 push_unavailable;
//  2. this session has no subscription → 404 not_found;
//  3. the push-test bucket: one test per 10 s per user → 429 rate_limited with retryAfter and Retry-After. It is the
//     only limit on tests: 04's per-recipient bucket leaves push.test out (04 §14.4);
//  4. SendTest. An error it returns goes out as it is: 503 server_busy when 04's queue took nothing, 503
//     server_shutdown while the server stops. The test then sent nothing, so its token goes back.
func (a *API) postPushTest(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteError(w, r, api.NewError(api.CodeUnauthenticated))
		return
	}
	push, ok := a.pushService()
	if !ok {
		WriteError(w, r, api.NewError(api.CodePushUnavailable))
		return
	}
	var subs []store.PushSubscription
	if p.SessionID != "" { // a caller without a web session has no subscription of "this session"
		err := a.d.DB.Read(r.Context(), func(q *store.Q) error {
			var err error
			subs, err = q.ListPushSubscriptions(store.PushFilter{UserIDs: []store.UserID{p.UserID}, SessionID: p.SessionID})
			return err
		})
		if err != nil {
			WriteError(w, r, opErr("push test", err))
			return
		}
	}
	if len(subs) == 0 {
		WriteError(w, r, api.NewError(api.CodeNotFound))
		return
	}
	taken, wait := a.pushTests.take(p.UserID)
	if wait > 0 {
		WriteError(w, r, &api.Error{Code: api.CodeRateLimited, RetryAfter: retryAfterSeconds(wait)})
		return
	}
	if err := push.SendTest(r.Context(), subs); err != nil {
		a.pushTests.refund(p.UserID, taken)
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// apiPushPreferences mirrors a user's stored preferences into their DTO.
func apiPushPreferences(p store.PushPreferences) api.PushPreferences {
	return api.PushPreferences{ShareStarted: api.ShareStartedPref(p.ShareStarted), AdminAlerts: p.AdminAlerts}
}

// getPushPreferences is GET /api/v1/push/preferences (03 §12.3 #50, §12.4.6): {shareStarted, adminAlerts}, the
// caller's notification choices. A user who never changed them has the defaults, all and true (03 §14). It reads
// only, and works while push is off.
func (a *API) getPushPreferences(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteError(w, r, api.NewError(api.CodeUnauthenticated))
		return
	}
	var prefs store.PushPreferences
	err := a.d.DB.Read(r.Context(), func(q *store.Q) error {
		var err error
		prefs, err = q.PushPreferences(p.UserID)
		return err
	})
	if err != nil {
		WriteError(w, r, opErr("push preferences", err))
		return
	}
	WriteJSON(w, http.StatusOK, apiPushPreferences(prefs))
}

// pushPreferencesBody is api.PushPreferences as a request body. The fields are pointers, so that one left out can
// be told from false and "".
type pushPreferencesBody struct {
	ShareStarted *string `json:"shareStarted"`
	AdminAlerts  *bool   `json:"adminAlerts"`
}

// putPushPreferences is PUT /api/v1/push/preferences (03 §12.3 #51, §12.4.6): the shape GET returns → 200 with the
// stored preferences. A PUT replaces both choices, so both fields are needed: one left out (or null) is 422
// validation_failed with the field code required, rather than a choice the user never made. shareStarted is all or
// off, anything else is invalid. adminAlerts is stored for every user and matters for admins only.
//
// 04's sender reads the choices with the recipients (ListPushSubscriptions with PushFilter.Pref): a share that goes
// live reaches the users whose shareStarted is all, an admin alert the admins whose adminAlerts is on.
func (a *API) putPushPreferences(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteError(w, r, api.NewError(api.CodeUnauthenticated))
		return
	}
	var req pushPreferencesBody
	if err := DecodeJSON(w, r, &req, otherBodyLimit); err != nil {
		WriteError(w, r, err)
		return
	}
	fields := map[string]string{}
	var prefs store.PushPreferences
	switch {
	case req.ShareStarted == nil:
		fields[fieldShareStarted] = api.FieldRequired
	case *req.ShareStarted != string(api.ShareStartedPrefAll) && *req.ShareStarted != string(api.ShareStartedPrefOff):
		fields[fieldShareStarted] = api.FieldInvalid
	default:
		prefs.ShareStarted = *req.ShareStarted
	}
	if req.AdminAlerts == nil {
		fields[fieldAdminAlerts] = api.FieldRequired
	} else {
		prefs.AdminAlerts = *req.AdminAlerts
	}
	if len(fields) > 0 {
		WriteError(w, r, &api.Error{Code: api.CodeValidationFailed, Fields: fields})
		return
	}
	now := a.d.Clock()
	err := a.d.DB.Write(r.Context(), func(q *store.Q) error {
		if err := callerLive(q, p); err != nil {
			return err
		}
		return q.SetPushPreferences(p.UserID, prefs, now)
	})
	if err != nil {
		WriteError(w, r, opErr("set push preferences", err))
		return
	}
	WriteJSON(w, http.StatusOK, apiPushPreferences(prefs))
}

// pushTestEvery is the push-test bucket of 03 §7.3: per user, a burst of 1 and one token every 10 s.
const pushTestEvery = 10 * time.Second

// pushTestLimiter is the push-test bucket: at most one POST /api/v1/push/test per user per 10 s. With a burst of 1,
// a bucket is one timestamp: the user has a token unless one was taken less than 10 s ago.
//
// It lives here and not with auth's throttles (03 §7.3) because the handler is its only user and auth.Service has
// no call for it. Memory only, like every throttle: a restart forgets it. The map holds only the users who tested
// within the last two windows; older entries mean the same as none and are swept.
type pushTestLimiter struct {
	now func() time.Time

	mu    sync.Mutex
	taken map[store.UserID]time.Time // when each user's token was taken
	swept time.Time                  // when the stale entries were last removed
}

func newPushTestLimiter(now func() time.Time) *pushTestLimiter {
	return &pushTestLimiter{now: now, taken: map[store.UserID]time.Time{}}
}

// stale reports whether a token taken at t is back at now. A clock that was set back also counts as back: an early
// test is harmless, a user locked out for the size of the step is not.
func stale(t, now time.Time) bool {
	d := now.Sub(t)
	return d < 0 || d >= pushTestEvery
}

// take takes u's token and returns the time it was taken at, for refund. Without a token it returns how long until
// u has one again (> 0) and takes nothing.
func (l *pushTestLimiter) take(u store.UserID) (at time.Time, wait time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if last, ok := l.taken[u]; ok && !stale(last, now) {
		return time.Time{}, pushTestEvery - now.Sub(last)
	}
	if stale(l.swept, now) {
		for id, t := range l.taken {
			if stale(t, now) {
				delete(l.taken, id)
			}
		}
		l.swept = now
	}
	l.taken[u] = now
	return now, 0
}

// refund gives back the token that take handed out at at, for a test that sent nothing. A token taken since then
// (the first one had come back) is left alone.
func (l *pushTestLimiter) refund(u store.UserID, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t, ok := l.taken[u]; ok && t.Equal(at) {
		delete(l.taken, u)
	}
}

// retryAfterSeconds is a wait as the retryAfter of a rate-limited answer: whole seconds, rounded up, at least 1.
func retryAfterSeconds(wait time.Duration) int {
	return max(1, int((wait+time.Second-1)/time.Second))
}
