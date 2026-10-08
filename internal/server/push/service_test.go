package push

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/config"
)

// The spec's example event (04 §14.3).
func exampleShare() ShareStarted {
	return ShareStarted{
		RoomID: "lounge", RoomName: "Lounge", ShareID: "s_q7m2x9c4v8b1n5k3",
		UserID: "k3m9p2qxw7ht", UserName: "Alex",
		At: time.UnixMilli(1790712000000),
	}
}

// share is a share.started event of a sharer in a room.
func share(roomID, userID string) ShareStarted {
	return ShareStarted{RoomID: roomID, RoomName: "Room " + roomID, ShareID: "s_" + roomID + userID, UserID: userID, UserName: "name-" + userID}
}

func TestShareStartedRecipientsAndPayload(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// The store answers with more than it should: the sharer's own subscription and a participant's.
		f := newFixture(t, nil,
			sub("b1", "bob"), sub("b2", "bob"), sub("c1", "carol"),
			sub("a1", "k3m9p2qxw7ht"), sub("d1", "dave"))
		f.start(t)

		ev := exampleShare()
		ev.PresentUserIDs = []string{"dave", "k3m9p2qxw7ht", "dave", ""}
		f.ShareStarted(ev)
		synctest.Wait()

		// The filter the store got: the sharer and the participants excluded (each once), and the preference.
		got := f.store.snapshot()
		wantFilter := RecipientFilter{ExcludeUserIDs: []string{"k3m9p2qxw7ht", "dave"}, Pref: "share_started"}
		if len(got.filters) != 1 || !reflect.DeepEqual(got.filters[0], wantFilter) {
			t.Errorf("filters = %+v, want [%+v]", got.filters, wantFilter)
		}

		const wantPayload = `{"v":1,"type":"share.started","ts":1790712000000,"tag":"share:lounge:k3m9p2qxw7ht",` +
			`"url":"/r/lounge?focus=s_q7m2x9c4v8b1n5k3","room":{"id":"lounge","name":"Lounge"},` +
			`"user":{"id":"k3m9p2qxw7ht","name":"Alex"},"shareId":"s_q7m2x9c4v8b1n5k3"}`
		wantOpts := SendOptions{TTL: 600 * time.Second, Urgency: "high", Topic: shareTopic("lounge", "k3m9p2qxw7ht")}
		var ids []string
		for _, c := range f.sender.sent() {
			ids = append(ids, c.Sub.ID)
			if c.Payload != wantPayload {
				t.Errorf("payload\n got %s\nwant %s", c.Payload, wantPayload)
			}
			if c.Opts != wantOpts {
				t.Errorf("options %+v, want %+v", c.Opts, wantOpts)
			}
		}
		slices.Sort(ids)
		if !slices.Equal(ids, []string{"b1", "b2", "c1"}) {
			t.Errorf("sent to %v, want b1 b2 c1 (never the sharer or a participant)", ids)
		}
		if n := len(got.results); n != 3 || f.sent(resultOK) != 3 {
			t.Errorf("%d results recorded, ok = %d; want 3 and 3", n, f.sent(resultOK))
		}
		for _, r := range got.results {
			if !r.OK || !r.At.Equal(time.Now()) {
				t.Errorf("recorded %+v, want a success at %v", r, time.Now())
			}
		}
	})
}

func TestShareStartedWithoutTimeUsesNow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, nil, sub("b1", "bob"))
		f.start(t)
		f.ShareStarted(share("lounge", "alex"))
		synctest.Wait()
		var p api.PushPayload
		if err := json.Unmarshal([]byte(f.sender.sent()[0].Payload), &p); err != nil {
			t.Fatal(err)
		}
		if p.TS != time.Now().UnixMilli() {
			t.Errorf("ts = %d, want now (%d)", p.TS, time.Now().UnixMilli())
		}
	})
}

// One payload per alert kind of 03 §7.11 and 04's transfer_threshold, to the admins who want alerts.
func TestAdminAlertPayloads(t *testing.T) {
	tests := []struct {
		kind, actor, target string
		wantURL             string
	}{
		{"signup_pending", "system", "sam_k", "/admin/approvals"},
		{"admin_granted", "alex", "sam_k", "/admin/users"},
		{"admin_revoked", "alex", "sam_k", "/admin/users"},
		{"admin_password_reset", "cli", "alex", "/admin/users"},
		{"registration_mode_changed", "alex", "", "/admin/settings"},
		{"secrets_rotated", "cli", "", "/admin/audit"},
		{"transfer_threshold", "system", "80", "/admin"},
		{"refresh_token_reused", "system", "sam_k", "/admin"},
		{"a_kind_of_a_later_version", "system", "", "/admin"},
	}
	for _, tc := range tests {
		t.Run(tc.kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFixture(t, nil, sub("a1", "admin1"), sub("a2", "admin2"))
				f.start(t)
				f.AdminAlert(AdminAlert{Kind: tc.kind, Actor: tc.actor, Target: tc.target, At: time.UnixMilli(1790712000000)})
				synctest.Wait()

				got := f.store.snapshot()
				wantFilter := RecipientFilter{AdminsOnly: true, Pref: "admin_alerts"}
				if len(got.filters) != 1 || !reflect.DeepEqual(got.filters[0], wantFilter) {
					t.Errorf("filters = %+v, want [%+v]", got.filters, wantFilter)
				}
				want := fmt.Sprintf(`{"v":1,"type":"admin.alert","ts":1790712000000,"tag":"admin:%s","url":%q,"kind":%q,"actor":%q`,
					tc.kind, tc.wantURL, tc.kind, tc.actor)
				if tc.target != "" {
					want += fmt.Sprintf(`,"target":%q`, tc.target)
				}
				want += "}"
				calls := f.sender.sent()
				if len(calls) != 2 {
					t.Fatalf("%d sends, want 2", len(calls))
				}
				for _, c := range calls {
					if c.Payload != want {
						t.Errorf("payload\n got %s\nwant %s", c.Payload, want)
					}
					if c.Opts != (SendOptions{TTL: 86400 * time.Second, Urgency: "normal"}) {
						t.Errorf("options %+v, want TTL 86400 s, urgency normal, no topic", c.Opts)
					}
				}
			})
		})
	}
	// The spec's example, byte for byte.
	p, err := alertPayload(AdminAlert{Kind: "signup_pending", Actor: "system", Target: "sam_k"}, time.UnixMilli(1790712000000))
	const example = `{"v":1,"type":"admin.alert","ts":1790712000000,"tag":"admin:signup_pending","url":"/admin/approvals",` +
		`"kind":"signup_pending","actor":"system","target":"sam_k"}`
	if err != nil || string(p) != example {
		t.Errorf("alertPayload = %s, %v", p, err)
	}
}

func TestSendTest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, nil)
		stop := f.start(t)
		mine := []Subscription{sub("m1", "me"), sub("m2", "me")}
		// More tests than the recipient's bucket holds: 03 limits the endpoint, the bucket does not apply.
		for range recipientBurst + 2 {
			if err := f.SendTest(context.Background(), mine); err != nil {
				t.Fatalf("SendTest: %v", err)
			}
		}
		synctest.Wait()
		calls := f.sender.sent()
		if len(calls) != 2*(recipientBurst+2) {
			t.Fatalf("%d sends, want %d", len(calls), 2*(recipientBurst+2))
		}
		want := fmt.Sprintf(`{"v":1,"type":"push.test","ts":%d,"tag":"test","url":"/account/notifications"}`, time.Now().UnixMilli())
		for _, c := range calls {
			if c.Payload != want {
				t.Errorf("payload\n got %s\nwant %s", c.Payload, want)
			}
			if c.Opts != (SendOptions{TTL: 60 * time.Second, Urgency: "high"}) {
				t.Errorf("options %+v, want TTL 60 s, urgency high, no topic", c.Opts)
			}
		}
		if got := f.store.snapshot(); len(got.filters) != 0 {
			t.Errorf("SendTest read recipients from the store: %+v", got.filters)
		}

		if err := f.SendTest(context.Background(), nil); err != nil {
			t.Errorf("SendTest with no subscriptions = %v, want nil", err)
		}
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		if err := f.SendTest(cancelled, mine); !errors.Is(err, context.Canceled) {
			t.Errorf("SendTest with a cancelled context = %v", err)
		}

		stop()
		err := f.SendTest(context.Background(), mine)
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Code != api.CodeServerShutdown || ae.RetryAfter != 5 {
			t.Errorf("SendTest after shutdown = %v, want server_shutdown with retryAfter 5", err)
		}
	})
}

func TestSendTestQueueFull(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.QueueLen = 2 }) // not running: nothing leaves the queue
	if err := f.SendTest(context.Background(), []Subscription{sub("m1", "me"), sub("m2", "me"), sub("m3", "me")}); err != nil {
		t.Fatalf("SendTest = %v, want nil when some of the messages were queued", err)
	}
	if f.sent(resultDropped) != 1 {
		t.Errorf("dropped = %d, want 1", f.sent(resultDropped))
	}
	err := f.SendTest(context.Background(), []Subscription{sub("m1", "me")})
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != api.CodeServerBusy || ae.RetryAfter <= 0 {
		t.Fatalf("SendTest on a full queue = %v, want server_busy with retryAfter", err)
	}
	if f.sent(resultDropped) != 2 {
		t.Errorf("dropped = %d, want 2", f.sent(resultDropped))
	}
}

// The result table of 04 §14.5.
func TestResultTable(t *testing.T) {
	type want struct {
		recorded *bool // RecordResult's ok; nil = not called
		deleted  bool
		counted  result
	}
	yes, no := true, false
	ok := want{recorded: &yes, counted: resultOK}
	gone := want{deleted: true, counted: resultGone}
	rejected := want{deleted: true, counted: resultRejected}
	refused := want{recorded: &no, counted: resultError}
	tests := []struct {
		name string
		res  SendResult
		err  error
		want want
	}{
		{"200", SendResult{Status: 200}, nil, ok},
		{"201", SendResult{Status: 201}, nil, ok},
		{"202", SendResult{Status: 202}, nil, ok},
		{"204", SendResult{Status: 204}, nil, ok},
		{"404", SendResult{Status: 404}, nil, gone},
		{"410", SendResult{Status: 410}, nil, gone},
		{"401", SendResult{Status: 401}, nil, rejected},
		{"403", SendResult{Status: 403}, nil, rejected},
		{"400", SendResult{Status: 400}, nil, refused},
		{"413", SendResult{Status: 413}, nil, refused},
		{"307", SendResult{Status: 307}, nil, refused},
		{"405", SendResult{Status: 405}, nil, refused},
		{"400 with Retry-After", SendResult{Status: 400, RetryAfter: time.Second}, nil, refused},
		{"undeliverable", SendResult{}, fmt.Errorf("%w: bad keys", ErrUndeliverable), refused},
		{"blocked address", SendResult{}, fmt.Errorf("dial: %w: 10.0.0.1", ErrBlockedAddress), refused},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFixture(t, nil, sub("b1", "bob"))
				f.sender.respond = func(int, sendCall) (SendResult, error) { return tc.res, tc.err }
				f.start(t)
				f.ShareStarted(share("lounge", "alex"))
				time.Sleep(10 * time.Minute) // long past every retry: none of these rows is retried
				synctest.Wait()

				if n := len(f.sender.sent()); n != 1 {
					t.Errorf("%d attempts, want 1", n)
				}
				got := f.store.snapshot()
				switch {
				case tc.want.recorded == nil && len(got.results) != 0:
					t.Errorf("recorded %+v, want nothing", got.results)
				case tc.want.recorded != nil && (len(got.results) != 1 || got.results[0].ID != "b1" || got.results[0].OK != *tc.want.recorded):
					t.Errorf("recorded %+v, want one result with ok=%v for b1", got.results, *tc.want.recorded)
				}
				if tc.want.deleted != slices.Equal(got.deleted, []string{"b1"}) || (!tc.want.deleted && len(got.deleted) != 0) {
					t.Errorf("deleted %v, want deleted=%v", got.deleted, tc.want.deleted)
				}
				for r := range numResults {
					wantN := uint64(0)
					if r == tc.want.counted {
						wantN = 1
					}
					if f.sent(r) != wantN {
						t.Errorf("counter %s = %d, want %d", resultNames[r], f.sent(r), wantN)
					}
				}
			})
		})
	}
}

// attempt is when a fakeSender call happened, relative to the start of the test, and the TTL it asked for.
type attempt struct {
	After time.Duration
	TTL   time.Duration
}

func attempts(start time.Time, calls []sendCall) []attempt {
	out := make([]attempt, len(calls))
	for i, c := range calls {
		out[i] = attempt{After: c.At.Sub(start), TTL: c.Opts.TTL}
	}
	return out
}

// 429, 5xx and network errors: retried after 5 s and 30 s, or after Retry-After when it is at most 60 s, while
// the message's TTL lasts; then one failure is recorded.
func TestRetries(t *testing.T) {
	sec := time.Second
	netErr := errors.New("push: sending to push.example.com: connection refused")
	tests := []struct {
		name     string
		test     bool // a push.test (TTL 60 s) in place of a share.started (TTL 600 s)
		answers  []SendResult
		err      error // returned in place of the answers
		want     []attempt
		recorded bool // RecordResult's ok after the last attempt
	}{
		{name: "503 three times", answers: []SendResult{{Status: 503}, {Status: 503}, {Status: 503}},
			want: []attempt{{0, 600 * sec}, {5 * sec, 595 * sec}, {35 * sec, 565 * sec}}, recorded: false},
		{name: "500 then 201", answers: []SendResult{{Status: 500}, {Status: 201}},
			want: []attempt{{0, 600 * sec}, {5 * sec, 595 * sec}}, recorded: true},
		{name: "502, 504, then 201", answers: []SendResult{{Status: 502}, {Status: 504}, {Status: 201}},
			want: []attempt{{0, 600 * sec}, {5 * sec, 595 * sec}, {35 * sec, 565 * sec}}, recorded: true},
		{name: "429 with Retry-After 7 then 201", answers: []SendResult{{Status: 429, RetryAfter: 7 * sec}, {Status: 201}},
			want: []attempt{{0, 600 * sec}, {7 * sec, 593 * sec}}, recorded: true},
		{name: "429 without Retry-After", answers: []SendResult{{Status: 429}, {Status: 429}, {Status: 429}},
			want: []attempt{{0, 600 * sec}, {5 * sec, 595 * sec}, {35 * sec, 565 * sec}}, recorded: false},
		{name: "Retry-After of 60 s is waited for", answers: []SendResult{{Status: 503, RetryAfter: 60 * sec}, {Status: 201}},
			want: []attempt{{0, 600 * sec}, {60 * sec, 540 * sec}}, recorded: true},
		{name: "Retry-After over 60 s gives up", answers: []SendResult{{Status: 429, RetryAfter: 61 * sec}},
			want: []attempt{{0, 600 * sec}}, recorded: false},
		{name: "408 is retried", answers: []SendResult{{Status: 408}, {Status: 201}},
			want: []attempt{{0, 600 * sec}, {5 * sec, 595 * sec}}, recorded: true},
		{name: "network error three times", err: netErr,
			want: []attempt{{0, 600 * sec}, {5 * sec, 595 * sec}, {35 * sec, 565 * sec}}, recorded: false},
		{name: "410 after a retry deletes", answers: []SendResult{{Status: 503}, {Status: 410}},
			want: []attempt{{0, 600 * sec}, {5 * sec, 595 * sec}}},
		{name: "test: both retries fit in 60 s", test: true, answers: []SendResult{{Status: 503}, {Status: 503}, {Status: 503}},
			want: []attempt{{0, 60 * sec}, {5 * sec, 55 * sec}, {35 * sec, 25 * sec}}, recorded: false},
		{name: "test: a retry past the TTL is not made", test: true,
			answers: []SendResult{{Status: 503, RetryAfter: 50 * sec}, {Status: 503, RetryAfter: 50 * sec}},
			want:    []attempt{{0, 60 * sec}, {50 * sec, 10 * sec}}, recorded: false},
		{name: "test: a wait as long as the TTL is not made", test: true,
			answers: []SendResult{{Status: 429, RetryAfter: 60 * sec}},
			want:    []attempt{{0, 60 * sec}}, recorded: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFixture(t, nil, sub("b1", "bob"))
				f.sender.respond = func(n int, _ sendCall) (SendResult, error) {
					if tc.err != nil {
						return SendResult{}, tc.err
					}
					if n >= len(tc.answers) {
						t.Errorf("attempt %d: the answers ran out", n+1)
						return SendResult{Status: 201}, nil
					}
					return tc.answers[n], nil
				}
				f.start(t)
				start := time.Now()
				if tc.test {
					if err := f.SendTest(context.Background(), f.store.subs); err != nil {
						t.Fatal(err)
					}
				} else {
					f.ShareStarted(share("lounge", "alex"))
				}
				time.Sleep(15 * time.Minute)
				synctest.Wait()

				if got := attempts(start, f.sender.sent()); !slices.Equal(got, tc.want) {
					t.Errorf("attempts %v, want %v", got, tc.want)
				}
				got := f.store.snapshot()
				last := SendResult{}
				if tc.err == nil {
					last = tc.answers[len(tc.want)-1]
				}
				if last.Status == 410 {
					if !slices.Equal(got.deleted, []string{"b1"}) || len(got.results) != 0 || f.sent(resultGone) != 1 {
						t.Errorf("deleted %v, recorded %+v; want b1 deleted and nothing recorded", got.deleted, got.results)
					}
					return
				}
				// Exactly one result for the message, whatever the number of attempts.
				if len(got.results) != 1 || got.results[0].OK != tc.recorded {
					t.Fatalf("recorded %+v, want one result with ok=%v", got.results, tc.recorded)
				}
				if wantAt := start.Add(tc.want[len(tc.want)-1].After); !got.results[0].At.Equal(wantAt) {
					t.Errorf("recorded at %v, want at the last attempt (%v)", got.results[0].At, wantAt)
				}
				wantOK, wantErr := uint64(0), uint64(1)
				if tc.recorded {
					wantOK, wantErr = 1, 0
				}
				if f.sent(resultOK) != wantOK || f.sent(resultError) != wantErr || f.sent(resultDropped) != 0 {
					t.Errorf("counters ok=%d error=%d dropped=%d, want ok=%d error=%d dropped=0",
						f.sent(resultOK), f.sent(resultError), f.sent(resultDropped), wantOK, wantErr)
				}
			})
		})
	}
}

// A message that waits for its retry does not hold a worker: the next message goes out at once.
func TestRetryDoesNotBlockTheWorker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, func(o *Options) { o.Workers = 1 }, sub("b1", "bob"))
		f.sender.respond = func(_ int, c sendCall) (SendResult, error) {
			if c.Sub.ID == "b1" {
				return SendResult{Status: 503}, nil
			}
			return SendResult{Status: 201}, nil
		}
		f.start(t)
		start := time.Now()
		f.ShareStarted(share("lounge", "alex"))
		synctest.Wait()
		if err := f.SendTest(context.Background(), []Subscription{sub("m1", "me")}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		calls := f.sender.sent()
		if len(calls) != 2 || calls[1].Sub.ID != "m1" || !calls[1].At.Equal(start) {
			t.Errorf("calls %+v, want b1 and then m1 without waiting", attempts(start, calls))
		}
	})
}

// share.started goes out at most once per (room, sharer) in 10 minutes (04 §14.4).
func TestShareStartedDedup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, nil)
		// A new recipient for every event, so that the recipients' buckets play no part here.
		n := 0
		f.store.recipients = func(RecipientFilter) []Subscription {
			n++
			return []Subscription{sub(fmt.Sprint("s", n), fmt.Sprint("user", n))}
		}
		f.start(t)
		sends := func() int { synctest.Wait(); return len(f.sender.sent()) }

		f.ShareStarted(share("lounge", "alex"))
		if got := sends(); got != 1 {
			t.Fatalf("first share: %d sends, want 1", got)
		}
		// The sharer's second connection, a restart of the share, a new share id: all the same (room, sharer).
		again := share("lounge", "alex")
		again.ShareID = "s_another"
		f.ShareStarted(again)
		f.ShareStarted(share("lounge", "alex"))
		if got := sends(); got != 1 {
			t.Errorf("repeats at once: %d sends, want 1", got)
		}
		if got := len(f.store.snapshot().filters); got != 1 {
			t.Errorf("the store was asked %d times, want 1: a suppressed event needs no recipients", got)
		}
		time.Sleep(10*time.Minute - time.Nanosecond)
		f.ShareStarted(share("lounge", "alex"))
		if got := sends(); got != 1 {
			t.Errorf("repeat just inside the window: %d sends, want 1", got)
		}
		// Another room, another sharer: their own windows.
		f.ShareStarted(share("movies", "alex"))
		f.ShareStarted(share("lounge", "carol"))
		if got := sends(); got != 3 {
			t.Errorf("another room and another sharer: %d sends, want 3", got)
		}
		time.Sleep(time.Nanosecond) // 10 minutes after the first
		f.ShareStarted(share("lounge", "alex"))
		if got := sends(); got != 4 {
			t.Errorf("after the window: %d sends, want 4", got)
		}
		f.ShareStarted(share("lounge", "alex"))
		if got := sends(); got != 4 {
			t.Errorf("the window starts again: %d sends, want 4", got)
		}
	})
}

// An event whose recipients could not be read does not start the window: the next one is sent.
func TestShareStartedDedupAfterStoreError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, nil, sub("b1", "bob"))
		f.store.errs["Recipients"] = errors.New("database is locked")
		f.start(t)
		f.ShareStarted(share("lounge", "alex"))
		synctest.Wait()
		if n := len(f.sender.sent()); n != 0 {
			t.Fatalf("%d sends with a failing store", n)
		}
		f.store.mu.Lock()
		delete(f.store.errs, "Recipients")
		f.store.mu.Unlock()
		f.ShareStarted(share("lounge", "alex"))
		synctest.Wait()
		if n := len(f.sender.sent()); n != 1 {
			t.Errorf("%d sends after the store recovered, want 1", n)
		}
		if !strings.Contains(f.logs.String(), "reading the recipients failed") {
			t.Errorf("no warning in the log:\n%s", f.logs.String())
		}
	})
}

// Per recipient: burst 3, then 1 per 10 minutes; the excess is dropped and counted (04 §14.4).
func TestRecipientRateLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// bob has two browsers: an event costs him one token and reaches both.
		f := newFixture(t, nil, sub("b1", "bob"), sub("b2", "bob"), sub("c1", "carol"))
		f.start(t)
		sharer := 0
		event := func() { // a different sharer each time, so the dedup window plays no part
			sharer++
			f.ShareStarted(share("lounge", fmt.Sprint("sharer", sharer)))
			synctest.Wait()
		}
		check := func(when string, wantSends int, wantDropped uint64) {
			t.Helper()
			if got := len(f.sender.sent()); got != wantSends || f.sent(resultDropped) != wantDropped {
				t.Errorf("%s: %d sends, %d dropped; want %d and %d", when, got, f.sent(resultDropped), wantSends, wantDropped)
			}
		}
		for range 3 {
			event()
		}
		check("burst of 3", 9, 0)
		event()
		check("4th at once", 9, 3)
		f.AdminAlert(AdminAlert{Kind: "signup_pending", Actor: "system"}) // the same buckets for every type
		synctest.Wait()
		check("an admin alert on empty buckets", 9, 6)

		time.Sleep(10*time.Minute - time.Second)
		event()
		check("just before the refill", 9, 9)
		time.Sleep(time.Second)
		event()
		check("after 10 minutes: one token", 12, 9)
		event()
		check("and only one", 12, 12)

		time.Sleep(time.Hour) // never more than the burst, however long the pause
		for range 4 {
			event()
		}
		check("burst after a long pause", 21, 15)
		if !strings.Contains(f.logs.String(), "reason=recipient_rate_limited") {
			t.Errorf("the drops are not in the log:\n%s", f.logs.String())
		}
	})
}

// The triggers never block: a full queue drops the event and counts it.
func TestQueueFullDropsEvents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, func(o *Options) { o.QueueLen = 2 }, sub("b1", "bob"))
		// Not running yet: the events wait in the queue.
		f.ShareStarted(share("lounge", "u1"))
		f.AdminAlert(AdminAlert{Kind: "signup_pending", Actor: "system"})
		f.ShareStarted(share("lounge", "u2"))
		f.AdminAlert(AdminAlert{Kind: "admin_granted", Actor: "alex", Target: "sam"})
		if f.sent(resultDropped) != 2 {
			t.Errorf("dropped = %d, want 2", f.sent(resultDropped))
		}
		if !strings.Contains(f.logs.String(), "reason=queue_full") {
			t.Errorf("the drop is not in the log:\n%s", f.logs.String())
		}
		f.start(t)
		synctest.Wait()
		if n := len(f.sender.sent()); n != 2 {
			t.Errorf("%d sends once running, want the 2 queued events", n)
		}
	})
}

// A full job queue drops what does not fit, and the rest still goes out.
func TestQueueFullDropsJobs(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.QueueLen = 2 })
	subs := []Subscription{sub("s1", "u1"), sub("s2", "u2"), sub("s3", "u3"), sub("s4", "u4"), sub("s5", "u5")}
	f.fanOut(f.log, api.PushTypeAdminAlert, subs, []byte("{}"), SendOptions{TTL: time.Minute}, time.Now())
	if len(f.jobs) != 2 || f.sent(resultDropped) != 3 {
		t.Errorf("%d queued, %d dropped; want 2 and 3", len(f.jobs), f.sent(resultDropped))
	}
}

func TestDefaults(t *testing.T) {
	f := newFixture(t, nil)
	if f.workers != 4 || cap(f.jobs) != 1024 || cap(f.events) != 1024 {
		t.Errorf("workers %d, queues %d and %d; want 4, 1024, 1024", f.workers, cap(f.jobs), cap(f.events))
	}
	if f.VAPIDPublicKey() != f.keys.Public {
		t.Errorf("VAPIDPublicKey = %q, want %q", f.VAPIDPublicKey(), f.keys.Public)
	}
	if drainTimeout != 2*time.Second || sendTimeout != 10*time.Second || maxResponseBody != 4<<10 {
		t.Error("the limits of 04 §14.5 changed")
	}
}

// Shutdown (04 §6.4 step 6): what is queued is still sent, for at most 2 s.
func TestShutdownDrainsTheQueue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, nil, sub("b1", "bob"))
		// Queued before Run, and Run's context is already over: only the drain can send them.
		f.ShareStarted(share("lounge", "alex"))
		if err := f.SendTest(context.Background(), []Subscription{sub("m1", "me"), sub("m2", "me")}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		start := time.Now()
		if err := f.Run(ctx); err != nil {
			t.Fatalf("Run = %v", err)
		}
		if time.Since(start) != 0 {
			t.Errorf("Run took %v with an answering push service", time.Since(start))
		}
		if n := len(f.sender.sent()); n != 3 {
			t.Errorf("%d sends during the drain, want 3", n)
		}
		if got := f.store.snapshot(); len(got.results) != 3 {
			t.Errorf("%d results recorded, want 3", len(got.results))
		}

		// After the shutdown the triggers do nothing.
		f.ShareStarted(share("movies", "alex"))
		f.AdminAlert(AdminAlert{Kind: "signup_pending"})
		if len(f.events) != 0 || f.sent(resultDropped) != 0 {
			t.Errorf("a trigger after shutdown was queued or counted: %d queued, %d dropped", len(f.events), f.sent(resultDropped))
		}
		if err := f.Run(context.Background()); err == nil {
			t.Error("a second Run succeeded")
		}
	})
}

func TestShutdownGivesUpAfterTwoSeconds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, func(o *Options) { o.Workers = 1 })
		f.sender.hang = true // a push service that never answers
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- f.Run(ctx) }()
		if err := f.SendTest(context.Background(), []Subscription{sub("m1", "me"), sub("m2", "me")}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait() // m1 is being sent, m2 waits for the only worker
		start := time.Now()
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("Run = %v", err)
		}
		if d := time.Since(start); d != drainTimeout {
			t.Errorf("Run returned %v after its context ended, want %v", d, drainTimeout)
		}
		// Neither message is the subscription's failure: nothing is recorded, both count as dropped.
		if got := f.store.snapshot(); len(got.results) != 0 || len(got.deleted) != 0 {
			t.Errorf("recorded %+v, deleted %v; want nothing", got.results, got.deleted)
		}
		if f.sent(resultDropped) != 2 || f.sent(resultError) != 0 {
			t.Errorf("dropped=%d error=%d, want 2 and 0", f.sent(resultDropped), f.sent(resultError))
		}
	})
}

func TestShutdownDropsPendingRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, nil, sub("b1", "bob"))
		f.sender.respond = status(503)
		stop := f.start(t)
		f.ShareStarted(share("lounge", "alex"))
		time.Sleep(time.Second) // the retry waits for t = 5 s
		start := time.Now()
		stop()
		if time.Since(start) != 0 {
			t.Errorf("Run waited %v for a retry", time.Since(start))
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if n := len(f.sender.sent()); n != 1 {
			t.Errorf("%d attempts, want 1: the retry must not run after shutdown", n)
		}
		if got := f.store.snapshot(); len(got.results) != 0 {
			t.Errorf("recorded %+v, want nothing for a retry given up at shutdown", got.results)
		}
		if f.sent(resultDropped) != 1 || f.sent(resultError) != 0 {
			t.Errorf("dropped=%d error=%d, want 1 and 0", f.sent(resultDropped), f.sent(resultError))
		}
		f.mu.Lock()
		pending := len(f.retries)
		f.mu.Unlock()
		if pending != 0 {
			t.Errorf("%d retry timers left", pending)
		}
	})
}

// The daily job (04 §14.6): at the start and every 24 hours, 20 failures and no success for 30 days.
func TestDailyPrune(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, nil)
		f.store.pruneN = 3
		start := time.Now()
		f.start(t)
		synctest.Wait()
		time.Sleep(48 * time.Hour)
		synctest.Wait()
		var want []pruneCall
		for _, after := range []time.Duration{0, 24 * time.Hour, 48 * time.Hour} {
			want = append(want, pruneCall{MinFailures: 20, NoSuccessSince: start.Add(after - 30*24*time.Hour)})
		}
		if got := f.store.snapshot().prunes; !slices.Equal(got, want) {
			t.Errorf("prunes %v, want %v", got, want)
		}
		if !strings.Contains(f.logs.String(), "count=3") {
			t.Errorf("the pruned count is not in the log:\n%s", f.logs.String())
		}
	})
}

func TestDailyPruneError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, nil)
		f.store.errs["Prune"] = errors.New("database is locked")
		f.start(t)
		synctest.Wait()
		if !strings.Contains(f.logs.String(), "pruning push subscriptions failed") {
			t.Errorf("no warning in the log:\n%s", f.logs.String())
		}
	})
}

// The VAPID fingerprint check (04 §5.2): a changed key deletes every subscription before New returns.
func TestVAPIDFingerprint(t *testing.T) {
	st := newFakeStore()
	st.deleteAllN = 7
	keys := newVAPID(t)
	logs := &lockedBuffer{}
	newService := func(k config.VAPIDKeys) (*Service, error) {
		return New(context.Background(), Options{
			Enabled: true, VAPID: func() config.VAPIDKeys { return k }, Subject: testSubject, Store: st,
			Sender: &fakeSender{}, Logger: slog.New(slog.NewTextHandler(logs, nil)),
		})
	}

	// First start: nothing stored. The fingerprint is stored; nothing is deleted.
	if _, err := newService(keys); err != nil {
		t.Fatal(err)
	}
	fp := st.meta["vapid_key_fp"]
	if len(fp) != 16 || strings.Trim(fp, "0123456789abcdef") != "" {
		t.Errorf("fingerprint %q, want 16 hex digits", fp)
	}
	if strings.Contains(keys.Public, fp) || strings.Contains(keys.Private.Reveal(), fp) {
		t.Error("the fingerprint is part of the key")
	}
	if got := st.snapshot(); got.purges != 0 || !slices.Equal(got.calls, []string{"Meta", "SetMeta"}) {
		t.Errorf("first start: calls %v, %d purges; want Meta, SetMeta and no purge", got.calls, got.purges)
	}

	// Same key: nothing is written.
	st.calls = nil
	if _, err := newService(keys); err != nil {
		t.Fatal(err)
	}
	if got := st.snapshot(); !slices.Equal(got.calls, []string{"Meta"}) {
		t.Errorf("same key: calls %v, want only Meta", got.calls)
	}

	// Rotated: every subscription is deleted, then the new fingerprint is stored, all before New returns.
	st.calls = nil
	rotated := newVAPID(t)
	s, err := newService(rotated)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.snapshot(); got.purges != 1 || !slices.Equal(got.calls, []string{"Meta", "DeleteAll", "SetMeta"}) {
		t.Errorf("rotated: calls %v, %d purges; want Meta, DeleteAll, SetMeta", got.calls, got.purges)
	}
	if st.meta["vapid_key_fp"] == fp || st.meta["vapid_key_fp"] == "" {
		t.Errorf("rotated: stored fingerprint %q, was %q", st.meta["vapid_key_fp"], fp)
	}
	if s.VAPIDPublicKey() != rotated.Public {
		t.Error("the Service does not publish the rotated key")
	}
	if !strings.Contains(logs.String(), "count=7") {
		t.Errorf("the purge is not in the log:\n%s", logs.String())
	}

	// A padded key is the same key.
	st.calls = nil
	padded := rotated
	padded.Public += "="
	if s, err := newService(padded); err != nil || s.VAPIDPublicKey() != rotated.Public {
		t.Errorf("padded key: %v", err)
	}
	if got := st.snapshot(); !slices.Equal(got.calls, []string{"Meta"}) {
		t.Errorf("padded key: calls %v, want only Meta", got.calls)
	}
}

func TestVAPIDFingerprintStoreErrors(t *testing.T) {
	boom := errors.New("database is locked")
	for _, method := range []string{"Meta", "DeleteAll", "SetMeta"} {
		t.Run(method, func(t *testing.T) {
			st := newFakeStore()
			st.meta["vapid_key_fp"] = "0123456789abcdef" // the previous key's
			st.errs[method] = boom
			keys := newVAPID(t)
			s, err := New(context.Background(), Options{
				Enabled: true, VAPID: func() config.VAPIDKeys { return keys }, Subject: testSubject, Store: st,
			})
			if s != nil || !errors.Is(err, boom) {
				t.Fatalf("New = %v, %v; want the store's error", s, err)
			}
			// The old fingerprint stays, so the next start runs the purge again.
			if st.meta["vapid_key_fp"] != "0123456789abcdef" {
				t.Errorf("the fingerprint changed to %q although New failed", st.meta["vapid_key_fp"])
			}
		})
	}
}

func TestNewValidatesOptions(t *testing.T) {
	keys, other := newVAPID(t), newVAPID(t)
	valid := func() Options {
		return Options{
			Enabled: true, VAPID: func() config.VAPIDKeys { return keys }, Subject: testSubject,
			Store: newFakeStore(), Sender: &fakeSender{},
		}
	}
	if _, err := New(context.Background(), valid()); err != nil {
		t.Fatalf("New with valid options: %v", err)
	}

	disabled := valid()
	disabled.Enabled = false
	if s, err := New(context.Background(), disabled); s != nil || !errors.Is(err, ErrDisabled) {
		t.Errorf("New with Enabled false = %v, %v; want ErrDisabled", s, err)
	}
	if st := disabled.Store.(*fakeStore); len(st.calls) != 0 {
		t.Errorf("a disabled service touched the store: %v", st.calls)
	}

	tests := map[string]func(*Options){
		"no store": func(o *Options) { o.Store = nil },
		"no VAPID": func(o *Options) { o.VAPID = nil },
		"empty keys": func(o *Options) {
			o.VAPID = func() config.VAPIDKeys { return config.VAPIDKeys{} }
		},
		"public key of another pair": func(o *Options) {
			o.VAPID = func() config.VAPIDKeys { return config.VAPIDKeys{Public: other.Public, Private: keys.Private} }
		},
		"public key not base64url": func(o *Options) {
			o.VAPID = func() config.VAPIDKeys { return config.VAPIDKeys{Public: "!!!", Private: keys.Private} }
		},
		"private key too short": func(o *Options) {
			o.VAPID = func() config.VAPIDKeys { return config.VAPIDKeys{Public: keys.Public, Private: "AAAA"} }
		},
		"private key not base64url": func(o *Options) {
			o.VAPID = func() config.VAPIDKeys { return config.VAPIDKeys{Public: keys.Public, Private: "!!!"} }
		},
		"no subject":               func(o *Options) { o.Subject = "" },
		"subject without a scheme": func(o *Options) { o.Subject = "you@example.com" },
		"http subject":             func(o *Options) { o.Subject = "http://localhost:8080" },
		"empty mailto":             func(o *Options) { o.Subject = "mailto:" },
		"empty https":              func(o *Options) { o.Subject = "https:" },
		"ftp subject":              func(o *Options) { o.Subject = "ftp://example.com" },
	}
	for name, mod := range tests {
		o := valid()
		mod(&o)
		s, err := New(context.Background(), o)
		if s != nil || err == nil {
			t.Errorf("%s: New = %v, %v; want an error", name, s, err)
			continue
		}
		if strings.Contains(err.Error(), keys.Private.Reveal()) {
			t.Errorf("%s: the error contains the private key", name)
		}
		if st, ok := o.Store.(*fakeStore); ok && len(st.calls) != 0 {
			t.Errorf("%s: the store was touched before the options were checked: %v", name, st.calls)
		}
	}

	for _, subject := range []string{"mailto:you@example.com", "https://watch.example.com", "https://203.0.113.7"} {
		o := valid()
		o.Subject = subject
		if _, err := New(context.Background(), o); err != nil {
			t.Errorf("subject %q: %v", subject, err)
		}
	}
}

func TestDeriveSubject(t *testing.T) {
	tests := []struct{ pushSubject, acmeEmail, origin, want string }{
		{"mailto:ops@example.com", "you@example.com", "https://watch.example.com", "mailto:ops@example.com"},
		{"https://example.com/contact", "you@example.com", "https://watch.example.com", "https://example.com/contact"},
		{"", "you@example.com", "https://watch.example.com", "mailto:you@example.com"},
		{"", "", "https://watch.example.com", "https://watch.example.com"},
		{"", "", "https://203.0.113.7", "https://203.0.113.7"}, // ip mode: the form the iPhone check covers
		{"", "", "https://[2001:db8::1]:8443", "https://[2001:db8::1]:8443"},
		{"", "", "http://localhost:8080", "https://localhost:8080"}, // a development server
	}
	for _, tc := range tests {
		got := DeriveSubject(tc.pushSubject, tc.acmeEmail, tc.origin)
		if got != tc.want {
			t.Errorf("DeriveSubject(%q, %q, %q) = %q, want %q", tc.pushSubject, tc.acmeEmail, tc.origin, got, tc.want)
		}
		if !validSubject(got) {
			t.Errorf("DeriveSubject(%q, %q, %q) = %q, which New refuses", tc.pushSubject, tc.acmeEmail, tc.origin, got)
		}
	}
}

// Payloads stay under 1 KB, whatever the names (04 §14.3).
func TestPayloadsStayUnder1KB(t *testing.T) {
	longest := strings.Repeat("𠮷", 500) // 4 bytes each in UTF-8
	ev := ShareStarted{
		RoomID: "k3m9p2qxw7ht", RoomName: longest, ShareID: "s_q7m2x9c4v8b1n5k3",
		UserID: "q7m2x9c4v8b1", UserName: longest,
	}
	p, err := sharePayload(ev, time.UnixMilli(1790712000000))
	if err != nil || len(p) >= 1024 {
		t.Fatalf("share.started with the longest names: %d bytes, %v", len(p), err)
	}
	var got api.PushPayload
	if err := json.Unmarshal(p, &got); err != nil {
		t.Fatal(err)
	}
	if want := strings.Repeat("𠮷", maxNameRunes); got.Room.Name != want || got.User.Name != want {
		t.Errorf("names are not cut to %d characters: %d and %d bytes", maxNameRunes, len(got.Room.Name), len(got.User.Name))
	}

	p, err = alertPayload(AdminAlert{Kind: "admin_password_reset", Actor: longest, Target: longest}, time.Now())
	if err != nil || len(p) >= 1024 {
		t.Errorf("admin.alert with the longest names: %d bytes, %v", len(p), err)
	}
	if p, err = testPayload(time.Now()); err != nil || len(p) >= 1024 {
		t.Errorf("push.test: %d bytes, %v", len(p), err)
	}

	// 32-character usernames (03) and an ordinary room name are sent whole.
	name := strings.Repeat("太", 32)
	p, err = sharePayload(ShareStarted{RoomID: "lounge", RoomName: "Lounge", ShareID: "s_1", UserID: "u1", UserName: name}, time.Now())
	if err != nil || !strings.Contains(string(p), name) {
		t.Errorf("a 32-character username was cut: %s, %v", p, err)
	}
	// Ids are escaped in the link.
	p, _ = sharePayload(ShareStarted{RoomID: "a/b?c", ShareID: "s&x=1#y", UserID: "u"}, time.Now())
	_ = json.Unmarshal(p, &got)
	if got.URL != "/r/a%2Fb%3Fc?focus=s%26x%3D1%23y" {
		t.Errorf("url = %q", got.URL)
	}
	// What does not fit is an error, never a truncated JSON.
	if _, err := sharePayload(ShareStarted{RoomID: strings.Repeat("r", 2000)}, time.Now()); err == nil {
		t.Error("an oversized payload was built")
	}
}

// An event whose payload can't be built is logged and sends nothing.
func TestOversizedPayloadIsNotSent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, nil, sub("b1", "bob"))
		f.start(t)
		f.ShareStarted(ShareStarted{RoomID: strings.Repeat("r", 2000), UserID: "alex"})
		f.AdminAlert(AdminAlert{Kind: strings.Repeat("k", 2000)})
		synctest.Wait()
		if n := len(f.sender.sent()); n != 0 {
			t.Errorf("%d sends", n)
		}
		if strings.Count(f.logs.String(), "over the limit of 1024") != 2 {
			t.Errorf("want two warnings in the log:\n%s", f.logs.String())
		}
	})
}

// Logs carry the push service's host, never the endpoint, and no names (README §4 "Logging").
func TestLogsHideEndpointsAndNames(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, nil, sub("ok", "u1"), sub("gone", "u2"), sub("rejected", "u3"), sub("refused", "u4"),
			sub("flaky", "u5"), sub("blocked", "u6"))
		f.sender.respond = func(_ int, c sendCall) (SendResult, error) {
			switch c.Sub.ID {
			case "gone":
				return SendResult{Status: 410}, nil
			case "rejected":
				return SendResult{Status: 403}, nil
			case "refused":
				return SendResult{Status: 400}, nil
			case "flaky":
				return SendResult{Status: 503}, nil
			case "blocked":
				return SendResult{}, fmt.Errorf("push: sending to push.example.com: %w: 10.0.0.1", ErrBlockedAddress)
			}
			return SendResult{Status: 201}, nil
		}
		f.store.errs["RecordResult"] = errors.New("database is locked")
		f.store.errs["Delete"] = errors.New("database is locked")
		f.start(t)
		ev := exampleShare()
		ev.RoomName, ev.UserName = "SecretRoomName", "SecretUserName"
		f.ShareStarted(ev)
		f.AdminAlert(AdminAlert{Kind: "admin_granted", Actor: "SecretActorName", Target: "SecretTargetName"})
		time.Sleep(time.Hour)
		synctest.Wait()

		logs := f.logs.String()
		for _, secret := range []string{"TOKEN-", "/send/", "SecretRoomName", "SecretUserName", "SecretActorName",
			"SecretTargetName", f.keys.Private.Reveal(), "tBHItJI5svbpez7KI4CCXg"} {
			if strings.Contains(logs, secret) {
				t.Errorf("the log contains %q:\n%s", secret, logs)
			}
		}
		for _, want := range []string{"component=push", "push_host=push.example.com", "user_id=u2", "status=410",
			"status=403", "status=400", "status=503", "room_id=lounge", "share_id=s_q7m2x9c4v8b1n5k3",
			"kind=admin_granted", "recording the push result failed", "deleting the push subscription failed",
			"the SSRF guard refused the address"} {
			if !strings.Contains(logs, want) {
				t.Errorf("the log lacks %q:\n%s", want, logs)
			}
		}
	})
}

// isshoni_push_sent_total{result} (04 §11.2).
func TestMetrics(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reg := prometheus.NewRegistry()
		f := newFixture(t, func(o *Options) { o.Metrics, o.QueueLen = reg, 8 },
			sub("ok1", "u1"), sub("ok2", "u2"), sub("gone", "u3"), sub("rejected", "u4"), sub("refused", "u5"))
		f.sender.respond = func(_ int, c sendCall) (SendResult, error) {
			switch c.Sub.ID {
			case "gone":
				return SendResult{Status: 404}, nil
			case "rejected":
				return SendResult{Status: 401}, nil
			case "refused":
				return SendResult{Status: 413}, nil
			}
			return SendResult{Status: 201}, nil
		}
		read := func() map[string]float64 {
			t.Helper()
			families, err := reg.Gather()
			if err != nil {
				t.Fatal(err)
			}
			out := map[string]float64{}
			for _, mf := range families {
				if mf.GetName() != "isshoni_push_sent_total" {
					t.Errorf("unexpected metric %s", mf.GetName())
					continue
				}
				for _, m := range mf.GetMetric() {
					if len(m.GetLabel()) != 1 || m.GetLabel()[0].GetName() != "result" {
						t.Errorf("labels %v, want only result", m.GetLabel())
						continue
					}
					out[m.GetLabel()[0].GetValue()] = m.GetCounter().GetValue()
				}
			}
			return out
		}
		zero := map[string]float64{"ok": 0, "gone": 0, "rejected": 0, "error": 0, "dropped": 0}
		if got := read(); !reflect.DeepEqual(got, zero) {
			t.Errorf("before any send: %v, want every result at 0", got)
		}
		f.start(t)
		f.ShareStarted(share("lounge", "alex"))
		synctest.Wait()
		for range 4 { // the fourth is over the recipients' burst
			f.AdminAlert(AdminAlert{Kind: "signup_pending", Actor: "system"})
			synctest.Wait()
		}
		// share.started: 2 ok, 1 gone, 1 rejected, 1 refused. Then two alerts with a token left for each of the
		// five recipients (the store here keeps returning the deleted rows), and two without.
		want := map[string]float64{"ok": 6, "gone": 3, "rejected": 3, "error": 3, "dropped": 10}
		if got := read(); !reflect.DeepEqual(got, want) {
			t.Errorf("metrics %v, want %v", got, want)
		}

		// One registry, one service.
		if _, err := New(context.Background(), Options{
			Enabled: true, VAPID: func() config.VAPIDKeys { return f.keys }, Subject: testSubject,
			Store: newFakeStore(), Sender: &fakeSender{}, Metrics: reg,
		}); err == nil {
			t.Error("a second Service registered the same metric")
		}
	})
}

// An admin alert whose recipients can't be read is logged and sends nothing.
func TestAdminAlertStoreError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, nil, sub("a1", "admin1"))
		f.store.errs["Recipients"] = errors.New("database is locked")
		f.start(t)
		f.AdminAlert(AdminAlert{Kind: "signup_pending", Actor: "system"})
		synctest.Wait()
		if n := len(f.sender.sent()); n != 0 {
			t.Errorf("%d sends with a failing store", n)
		}
		if !strings.Contains(f.logs.String(), "reading the recipients failed") {
			t.Errorf("no warning in the log:\n%s", f.logs.String())
		}
	})
}

// A message that waited in the queue past its TTL is not sent any more.
func TestExpiredJobIsDropped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, nil)
		if err := f.SendTest(context.Background(), []Subscription{sub("m1", "me")}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(ttlTest) // nothing runs yet: the message's 60 s pass in the queue
		f.start(t)
		synctest.Wait()
		if n := len(f.sender.sent()); n != 0 {
			t.Errorf("%d sends of an expired message", n)
		}
		if f.sent(resultDropped) != 1 || len(f.store.snapshot().results) != 0 {
			t.Errorf("dropped=%d, recorded %+v; want 1 and nothing", f.sent(resultDropped), f.store.snapshot().results)
		}
		if !strings.Contains(f.logs.String(), "reason=expired") {
			t.Errorf("the drop is not in the log:\n%s", f.logs.String())
		}
	})
}

// A retry that finds the queue full is dropped; after shutdown none is scheduled.
func TestRetryQueueFullAndAfterShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, func(o *Options) { o.QueueLen = 1 })
		retry := job{kind: api.PushTypeTest, sub: sub("m1", "me"), deadline: time.Now().Add(time.Hour), attempt: 1}
		if !f.retryLater(retry, 5*time.Second) {
			t.Fatal("retryLater refused before shutdown")
		}
		// The queue fills up while the retry waits.
		if err := f.SendTest(context.Background(), []Subscription{sub("m2", "me")}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if len(f.jobs) != 1 || f.sent(resultDropped) != 1 {
			t.Errorf("%d queued, %d dropped; want the retry dropped", len(f.jobs), f.sent(resultDropped))
		}
		f.mu.Lock()
		pending := len(f.retries)
		f.mu.Unlock()
		if pending != 0 {
			t.Errorf("%d retry timers left", pending)
		}

		f.shutdown()
		if f.retryLater(retry, time.Second) {
			t.Error("a retry was scheduled after shutdown")
		}
		// An event that the dispatcher reaches only after the drain time is counted, not sent.
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		f.expand(cancelled, event{alert: &AdminAlert{Kind: "signup_pending"}})
		if f.sent(resultDropped) != 2 || len(f.store.snapshot().filters) != 0 {
			t.Errorf("dropped=%d, store asked %d times; want 2 and 0", f.sent(resultDropped), len(f.store.snapshot().filters))
		}
	})
}

// The tests' switch (the fake push service is on loopback): ValidateEndpoint lets an IP literal and any port
// through, without asking the resolver. Nothing outside this package can turn it on.
func TestValidateEndpointAllowPrivate(t *testing.T) {
	res := &fakeResolver{}
	f := newFixture(t, func(o *Options) { o.Resolver, o.allowPrivate = res, true })
	for _, endpoint := range []string{"https://127.0.0.1:49152/push/abc", "https://[::1]:8443/x"} {
		if err := f.ValidateEndpoint(context.Background(), endpoint); err != nil {
			t.Errorf("ValidateEndpoint(%q) = %v", endpoint, err)
		}
	}
	if len(res.lookedUp()) != 0 {
		t.Errorf("the resolver was asked for an IP literal: %v", res.lookedUp())
	}
	if err := f.ValidateEndpoint(context.Background(), "http://127.0.0.1:49152/push/abc"); !api.IsCode(err, api.CodePushEndpointRejected) {
		t.Errorf("http passed with allowPrivate: %v", err)
	}
}
