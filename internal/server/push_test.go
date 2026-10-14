package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/client/publish"
	"github.com/MoonWX/isshoni/internal/media/fake"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/push"
	"github.com/MoonWX/isshoni/internal/server/servertest"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

// Web Push in a running server (04 §6.6, §14; README S71): the push service behind the hub's PushNotifier, auth's
// AdminAlerter and the REST API's Push, on 03's tables, with servertest's PushService standing in for the browsers'
// push services. The push service itself (encryption, VAPID, the SSRF guard, dedup and limits) is tested in
// internal/server/push.

// pushDeps are wiredDeps with a fake push service as the server's sender and resolver, which a test reads.
func pushDeps() (server.Deps, *servertest.PushService) {
	pushes := servertest.NewPushService()
	deps := wiredDeps()
	deps.PushSender, deps.Resolver = pushes, pushes
	return deps, pushes
}

// subscribe subscribes the browser c to push as the web app does, with the fake service's endpoint for name, and
// returns the subscription's id.
func subscribe(t *testing.T, srv *servertest.Server, c *http.Client, pushes *servertest.PushService, name string) string {
	t.Helper()
	res := callAs(t, c, srv, http.MethodPost, "/api/v1/push/subscriptions", pushes.Subscription(name))
	var sub api.PushSubscribeResponse
	if res.status != http.StatusCreated || json.Unmarshal([]byte(res.body), &sub) != nil || sub.ID == "" {
		t.Fatalf("subscribing %s = %d %s, want 201 with an id", name, res.status, res.body)
	}
	return sub.ID
}

// userID is the id of the account with this username.
func userID(t *testing.T, srv *servertest.Server, username string) string {
	t.Helper()
	rows := query(t, srv, true, `SELECT id FROM users WHERE username = ?`, username)
	if len(rows) != 1 {
		t.Fatalf("users named %s: %v", username, rows)
	}
	return rows[0][0]
}

// dialWSAs is dialWS for another browser than srv.Client.
func dialWSAs(t *testing.T, srv *servertest.Server, c *http.Client) *signaltest.Client {
	t.Helper()
	own := srv.Client
	srv.Client = c
	defer func() { srv.Client = own }()
	return dialWS(t, srv)
}

// waitPushes waits until the fake push service has at least n messages, and returns them.
func waitPushes(t *testing.T, pushes *servertest.PushService, n int) []servertest.PushMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	got, err := pushes.Wait(ctx, func(m []servertest.PushMessage) bool { return len(m) >= n })
	if err != nil {
		t.Fatalf("the push service got %d messages, want %d: %v", len(got), n, err)
	}
	return got
}

// goLive starts a share on c, which is in a room, and publishes fake media on it with the Go publisher until the
// test ends, the way a browser does (01 §11.2): share.start, a pub offer whose tracks name the share, the server's
// answer, then media. It returns the share's id once c's room.state shows the share live.
func goLive(t *testing.T, c *signaltest.Client) string {
	t.Helper()
	shareID := startShare(t, c, "r1").ShareID
	src, err := fake.New(fake.Config{
		Layers: []fake.VideoLayer{
			{RID: "f", Width: 640, Height: 360, FPS: 30, Bitrate: 400_000},
			{RID: "q", Width: 320, Height: 180, FPS: 15, Bitrate: 100_000},
		},
		GOP:   time.Minute,
		Audio: true,
		Seed:  71,
	})
	if err != nil {
		t.Fatal(err)
	}
	pub, err := publish.New(publish.Options{Source: src, Settings: sfutest.LoopbackSettings(), Audio: true})
	if err != nil {
		_ = src.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background()) // the publisher sends until the test ends
	t.Cleanup(func() {
		cancel()
		if err := pub.Close(); err != nil {
			t.Errorf("Publisher.Close: %v", err)
		}
		_ = src.Close()
	})

	offerCtx, cancelOffer := context.WithTimeout(ctx, 10*time.Second)
	defer cancelOffer()
	offer, err := pub.Offer(offerCtx)
	if err != nil {
		t.Fatalf("pub offer: %v", err)
	}
	var tracks []protocol.TrackRef // one per m-section: the simulcast encodings share the video's mid
	for _, tr := range pub.Tracks() {
		ref := protocol.TrackRef{MID: tr.MID, ShareID: shareID, Kind: protocol.TrackKindVideo}
		if tr.Kind == webrtc.RTPCodecTypeAudio {
			ref.Kind = protocol.TrackKindAudio
		}
		if !slices.Contains(tracks, ref) {
			tracks = append(tracks, ref)
		}
	}
	if err := c.Send(protocol.MessageTypePCOffer, "", protocol.PCOffer{
		PC: protocol.PCKindPub, Gen: 1, Neg: 1, SDP: protocol.SDP(offer.SDP), Tracks: tracks,
	}); err != nil {
		t.Fatalf("send the pub offer: %v", err)
	}
	env := nextOf(t, c, protocol.MessageTypePCAnswer, protocol.MessageTypeError)
	answer, err := protocol.Decode[protocol.PCAnswer](env)
	if env.Type != protocol.MessageTypePCAnswer || err != nil || answer.PC != protocol.PCKindPub {
		t.Fatalf("the pub offer was answered with %s %s", env.Type, env.Data)
	}
	if err := pub.SetAnswer(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP.Reveal()}); err != nil {
		t.Fatalf("the publisher refused the answer: %v", err)
	}
	if err := pub.Start(ctx); err != nil {
		t.Fatal(err)
	}
	for {
		state, err := protocol.Decode[protocol.RoomState](nextOf(t, c, protocol.MessageTypeRoomState))
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range state.Shares {
			if s.ID == shareID && s.Status == protocol.ShareStatusLive {
				return shareID
			}
		}
	}
}

// TestShareStartedPush is the acceptance test of the push wiring (README S71; 04 §14.3): a share that goes live
// reaches the push service for the users who are not in the room, and only for them. Not for the sharer, not for a
// user who is in the room and sees the share there, not for one who switched the notification off; and the one
// who gets it gets it on each of their browsers.
func TestShareStartedPush(t *testing.T) {
	deps, pushes := pushDeps()
	srv := servertest.Start(t, servertest.Options{Deps: deps})
	setUp(t, srv)
	kim, sam, pat := register(t, srv, "Kim"), register(t, srv, "Sam"), register(t, srv, "Pat")
	subscribe(t, srv, srv.Client, pushes, "alex-laptop")
	subscribe(t, srv, kim, pushes, "kim-laptop")
	samLaptop := subscribe(t, srv, sam, pushes, "sam-laptop")
	samPhone := subscribe(t, srv, sam, pushes, "sam-phone")
	subscribe(t, srv, pat, pushes, "pat-phone")
	wantStatus(t, "Pat's preferences", callAs(t, pat, srv, http.MethodPut, "/api/v1/push/preferences",
		api.PushPreferences{ShareStarted: api.ShareStartedPrefOff, AdminAlerts: true}), http.StatusOK, "")

	sharer := dialWS(t, srv) // Alex's
	joinLounge(t, sharer, signaltest.DefaultHello())
	watcher := dialWSAs(t, srv, kim)
	joinLounge(t, watcher, signaltest.DefaultHello())
	shareID := goLive(t, sharer)
	waitPushes(t, pushes, 1)
	// The shutdown drains the push queue (04 §6.4 step 6): what the share made the server send is all in by then.
	srv.Stop(t)

	msgs := pushes.Messages()
	got := map[string]servertest.PushMessage{}
	for _, m := range msgs {
		got[m.Endpoint] = m
	}
	if len(msgs) != 2 || len(got) != 2 {
		t.Fatalf("the push service got %d messages, to %v; want one to each of Sam's two browsers", len(msgs), keys(got))
	}
	alex, samID := userID(t, srv, adminName), userID(t, srv, "Sam")
	for name, subID := range map[string]string{"sam-laptop": samLaptop, "sam-phone": samPhone} {
		m, ok := got[pushes.Endpoint(name)]
		if !ok {
			t.Errorf("nothing reached %s; the messages went to %v", name, keys(got))
			continue
		}
		if m.SubID != subID || m.UserID != samID {
			t.Errorf("%s: the message was for subscription %s of %s, want %s of Sam (%s)", name, m.SubID, m.UserID, subID, samID)
		}
		want := api.PushPayload{
			V:       api.PushPayloadVersion,
			Type:    api.PushTypeShareStarted,
			TS:      m.Payload.TS,
			Tag:     "share:lounge:" + alex,
			URL:     "/r/lounge?focus=" + shareID,
			Room:    &api.NameRef{ID: "lounge", Name: "Lounge"},
			User:    &api.NameRef{ID: alex, Name: adminName},
			ShareID: shareID,
		}
		if gotJSON, wantJSON := mustJSON(t, m.Payload), mustJSON(t, want); gotJSON != wantJSON {
			t.Errorf("%s: payload\n got %s\nwant %s", name, gotJSON, wantJSON)
		}
		if age := time.Since(time.UnixMilli(m.Payload.TS)); age < 0 || age > time.Minute {
			t.Errorf("%s: the payload's ts is %v old", name, age)
		}
		if m.Options.TTL != 600*time.Second || m.Options.Urgency != push.UrgencyHigh || m.Options.Topic == "" {
			t.Errorf("%s: options %+v, want TTL 600 s, urgency high and a topic per room and sharer", name, m.Options)
		}
	}
	if got[pushes.Endpoint("sam-laptop")].Options.Topic != got[pushes.Endpoint("sam-phone")].Options.Topic {
		t.Error("the two browsers of one user got different topics for one sharer in one room")
	}
	// The push service answered 201: each subscription has its success recorded in 03's table.
	rows := query(t, srv, true, `SELECT id, failures, last_success_at IS NOT NULL FROM push_subscriptions
		WHERE user_id = ? ORDER BY id`, samID)
	if len(rows) != 2 {
		t.Errorf("Sam's subscriptions after the delivery: %v", rows)
	}
	for _, row := range rows {
		if row[1] != "0" || row[2] != "1" {
			t.Errorf("subscription %s after the delivery: failures %s, last success recorded %s", row[0], row[1], row[2])
		}
	}
}

// TestSignupPendingPush: an admin alert of 03 reaches the admins through the push service (04 §14.3), and only
// them: a sign-up in the approval mode raises signup_pending, which a member does not get.
func TestSignupPendingPush(t *testing.T) {
	deps, pushes := pushDeps()
	srv := servertest.Start(t, servertest.Options{Deps: deps, Flags: []string{"--registration.mode=approval"}})
	setUp(t, srv)
	kim := register(t, srv, "Kim") // with an invite: active at once, also in the approval mode
	alexSub := subscribe(t, srv, srv.Client, pushes, "alex-laptop")
	subscribe(t, srv, kim, pushes, "kim-laptop")

	stranger := browser(t, srv)
	wantStatus(t, "a sign-up without an invite", callAs(t, stranger, srv, http.MethodPost, "/api/v1/auth/register",
		api.RegisterRequest{Username: "Sam", Password: "a long enough password"}), http.StatusAccepted, "")
	waitPushes(t, pushes, 1)
	srv.Stop(t)

	msgs := pushes.Messages()
	if len(msgs) != 1 || msgs[0].Endpoint != pushes.Endpoint("alex-laptop") || msgs[0].SubID != alexSub {
		t.Fatalf("the push service got %+v, want one message for the admin's browser", msgs)
	}
	m := msgs[0]
	want := api.PushPayload{
		V:      api.PushPayloadVersion,
		Type:   api.PushTypeAdminAlert,
		TS:     m.Payload.TS,
		Tag:    "admin:signup_pending",
		URL:    "/admin/approvals",
		Kind:   api.AdminAlertKindSignupPending,
		Actor:  "system",
		Target: "Sam",
	}
	if gotJSON, wantJSON := mustJSON(t, m.Payload), mustJSON(t, want); gotJSON != wantJSON {
		t.Errorf("payload\n got %s\nwant %s", gotJSON, wantJSON)
	}
	if m.Options != (push.SendOptions{TTL: 24 * time.Hour, Urgency: push.UrgencyNormal}) {
		t.Errorf("options %+v, want TTL 86400 s, urgency normal and no topic", m.Options)
	}
}

// TestPushEndpoints: the REST API's push endpoints reach the push service (04 §14.2): GET /info publishes the
// VAPID key of secrets.json, a subscription's endpoint passes the push service's checks, the test notification
// goes to this browser alone, and a subscription that the push service says is gone is deleted from 03's table.
func TestPushEndpoints(t *testing.T) {
	deps, pushes := pushDeps()
	srv := servertest.Start(t, servertest.Options{Deps: deps})
	setUp(t, srv)
	if key := infoVAPID(t, srv); key == "" || key != secretIDs(t, srv)["vapid"] {
		t.Errorf("GET /info publishes the VAPID key %q, secrets.json has %q", key, secretIDs(t, srv)["vapid"])
	}
	if res := call(t, srv, http.MethodGet, "/api/v1/info", nil); !strings.Contains(res.body, `"push"`) {
		t.Errorf("GET /info = %s, want push among its features", res.body)
	}

	// The endpoint rules are the push service's: a name that is the machine itself is refused without a lookup, and
	// one that resolves only to public addresses is taken.
	bad := pushes.Subscription("x")
	bad.Endpoint = "https://localhost/send/x"
	res := call(t, srv, http.MethodPost, "/api/v1/push/subscriptions", bad)
	if res.status != http.StatusUnprocessableEntity || errorCode(res.body) != api.CodePushEndpointRejected ||
		!strings.Contains(res.body, `"reason":"private_address"`) {
		t.Errorf("subscribing with a localhost endpoint = %d %s, want 422 push_endpoint_rejected private_address", res.status, res.body)
	}
	laptop := subscribe(t, srv, srv.Client, pushes, "alex-laptop")
	other := browser(t, srv) // the admin on a second browser: its subscription is not "this browser"'s
	wantStatus(t, "login on a second browser", callAs(t, other, srv, http.MethodPost, "/api/v1/auth/login",
		api.LoginRequest{Username: adminName, Password: adminPassword}), http.StatusOK, "")
	phone := subscribe(t, srv, other, pushes, "alex-phone")

	wantStatus(t, "POST /push/test", call(t, srv, http.MethodPost, "/api/v1/push/test", struct{}{}), http.StatusAccepted, "")
	m := waitPushes(t, pushes, 1)[0]
	if m.Endpoint != pushes.Endpoint("alex-laptop") || m.SubID != laptop || m.Payload.Type != api.PushTypeTest ||
		m.Payload.URL != "/account/notifications" || m.Options.TTL != time.Minute || m.Options.Urgency != push.UrgencyHigh {
		t.Errorf("the test notification: %+v", m)
	}

	// A push service that says the subscription is gone (410) has it deleted (04 §14.5), through 03's table. The
	// test endpoint takes one test per user in 10 s (03 §7.3), so this one is another user's.
	kim := register(t, srv, "Kim")
	subscribe(t, srv, kim, pushes, "kim-phone")
	pushes.SetStatus(pushes.Endpoint("kim-phone"), http.StatusGone)
	wantStatus(t, "Kim's POST /push/test", callAs(t, kim, srv, http.MethodPost, "/api/v1/push/test", struct{}{}),
		http.StatusAccepted, "")
	if got := waitPushes(t, pushes, 2)[1]; got.Endpoint != pushes.Endpoint("kim-phone") {
		t.Fatalf("Kim's test notification went to %s", got.Endpoint)
	}
	eventually(t, func() bool {
		rows := query(t, srv, true, `SELECT id FROM push_subscriptions ORDER BY id`)
		want := [][]string{{laptop}, {phone}}
		slices.SortFunc(want, func(a, b []string) int { return strings.Compare(a[0], b[0]) })
		return slices.EqualFunc(rows, want, slices.Equal)
	}, "Kim's gone subscription is deleted, and the admin's two stay")
	if n := len(pushes.Messages()); n != 2 {
		t.Errorf("the push service got %d messages, want the two tests", n)
	}
}

// TestPushDisabled: with push.enabled = false there is no push service (04 §6.6): the REST API answers
// push_unavailable to a subscription and a test, GET /info has no push object, admin alerts are only logged, a share
// that goes live notifies nobody, and the shutdown has no push step. What a browser keeps of its own, unsubscribing
// and the preferences, still works.
func TestPushDisabled(t *testing.T) {
	deps, pushes := pushDeps()
	srv := servertest.Start(t, servertest.Options{Deps: deps, Flags: []string{"--push.enabled=false"}})
	setUp(t, srv)

	if key := infoVAPID(t, srv); key != "" {
		t.Errorf("GET /info publishes the VAPID key %q with push off", key)
	}
	if res := call(t, srv, http.MethodGet, "/api/v1/info", nil); strings.Contains(res.body, `"push"`) {
		t.Errorf("GET /info = %s, want no push object and no push feature", res.body)
	}
	wantStatus(t, "subscribing with push off", call(t, srv, http.MethodPost, "/api/v1/push/subscriptions",
		pushes.Subscription("alex-laptop")), http.StatusServiceUnavailable, api.CodePushUnavailable)
	wantStatus(t, "a test notification with push off", call(t, srv, http.MethodPost, "/api/v1/push/test", struct{}{}),
		http.StatusServiceUnavailable, api.CodePushUnavailable)
	wantStatus(t, "unsubscribing with push off", call(t, srv, http.MethodPost, "/api/v1/push/unsubscribe",
		api.PushUnsubscribeRequest{Endpoint: pushes.Endpoint("alex-laptop")}), http.StatusNoContent, "")
	wantStatus(t, "the preferences with push off", call(t, srv, http.MethodPut, "/api/v1/push/preferences",
		api.PushPreferences{ShareStarted: api.ShareStartedPrefAll, AdminAlerts: true}), http.StatusOK, "")

	// An admin alert is a log line and nothing else; so is a share that goes live, to a server without push.
	query(t, srv, false, `INSERT INTO push_subscriptions (id, user_id, session_id, endpoint, p256dh, auth_secret, name, created_at)
		SELECT 'pushpushpush', user_id, id, ?, 'p256dh', 'auth', 'Chrome on macOS', 0 FROM sessions`, pushes.Endpoint("alex-laptop"))
	wantStatus(t, "a change of the registration mode", call(t, srv, http.MethodPatch, "/api/v1/admin/settings",
		map[string]any{"registrationMode": "approval"}), http.StatusOK, "")
	c := dialWS(t, srv)
	joinLounge(t, c, signaltest.DefaultHello())
	goLive(t, c)

	var mu sync.Mutex
	var steps []string
	srv.Srv.SetShutdownStepHook(func(step string) {
		mu.Lock()
		defer mu.Unlock()
		steps = append(steps, step)
	})
	srv.Stop(t)
	if msgs := pushes.Messages(); len(msgs) != 0 {
		t.Errorf("the push service got %d messages with push off", len(msgs))
	}
	mu.Lock()
	defer mu.Unlock()
	if slices.Contains(steps, "push") || !slices.Contains(steps, "admin socket") {
		t.Errorf("the shutdown's steps with push off: %v", steps)
	}
	alerted := false
	for line := range strings.Lines(srv.Logs()) {
		alerted = alerted || strings.Contains(line, `"msg":"admin alert"`) && strings.Contains(line, `"kind":"registration_mode_changed"`)
		if strings.Contains(line, `"component":"push"`) {
			t.Errorf("a log line of a push service, with push off: %s", line)
		}
	}
	if !alerted {
		t.Error("the admin alert is not in the log")
	}
}

// TestPushSubjectOnlyAPrefix: a push.subject that is only "mailto:" or "https:" passes config's own check, and the
// push service would refuse it at every start (04 §14.1). The server refuses it as the config error it is
// (NeedsOperator: exit 78). With push off nothing reads the subject, and the server starts.
func TestPushSubjectOnlyAPrefix(t *testing.T) {
	for _, subject := range []string{"mailto:", "https:"} {
		t.Run(subject, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "data")
			_, err := servertest.Try(t, servertest.Options{DataDir: dir, Deps: wiredDeps(), Flags: []string{"--push.subject=" + subject}})
			var ve *config.ValidationError
			if !server.NeedsOperator(err) || !errors.As(err, &ve) || len(ve.Problems) != 1 ||
				ve.Problems[0].Key != "push.subject" || ve.Problems[0].Source.Kind != config.SourceFlag {
				t.Fatalf("Start with push.subject = %q: %v, want a config error for that key, from its flag", subject, err)
			}
			if msg := err.Error(); !strings.Contains(msg, `push.subject = "`+subject+`"`) || !strings.Contains(msg, "fix:") {
				t.Errorf("the error says %q", msg)
			}
			servertest.Start(t, servertest.Options{DataDir: dir, Deps: wiredDeps(),
				Flags: []string{"--push.subject=" + subject, "--push.enabled=false"}})
		})
	}
	// A subject with a contact is taken.
	servertest.Start(t, servertest.Options{Deps: wiredDeps(), Flags: []string{"--push.subject=mailto:ops@example.com"}})
}

// mustJSON is v as JSON, for a comparison that shows the difference.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// keys returns a map's keys, sorted.
func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// eventually polls cond for up to 10 s and fails the test with what when it never holds.
func eventually(t *testing.T, cond func() bool, what string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting: %s", what)
		}
	}
}
