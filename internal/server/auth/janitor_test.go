package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/server/store"
)

// The janitor's schedule (03 §4.7, §15) under the fake clock of a synctest bubble: the store, the service and the
// janitor's ticker all read the bubble's clock, so a test that sleeps an hour moves every one of them by an hour.

// pruneRecorder wraps the service's prune: it records the time of each run and can make a run fail.
type pruneRecorder struct {
	next func(ctx context.Context, now time.Time) (store.PruneStats, error)
	// fail, if set, runs before the real prune with the index of the run (0 = the one at startup); an error it
	// returns is the run's result, and the real prune is skipped.
	fail func(ctx context.Context, run int) error

	mu   sync.Mutex
	runs []time.Time
}

func (r *pruneRecorder) prune(ctx context.Context, now time.Time) (store.PruneStats, error) {
	r.mu.Lock()
	run := len(r.runs)
	r.runs = append(r.runs, now)
	r.mu.Unlock()
	if r.fail != nil {
		if err := r.fail(ctx, run); err != nil {
			return store.PruneStats{}, err
		}
	}
	return r.next(ctx, now)
}

// times returns the times of the runs so far.
func (r *pruneRecorder) times() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.runs...)
}

// newBubbleEnv is newSvcEnv for a synctest bubble: the store and the service use the bubble's clock (time.Now), and
// the service's prune goes through the returned recorder. It must be called inside the bubble. The store is closed
// when the bubble's test ends; a test stops its janitor before that.
func newBubbleEnv(t *testing.T) (*svcEnv, *pruneRecorder) {
	t.Helper()
	e := &svcEnv{t: t, clk: newFakeClock(), dir: t.TempDir(), conns: &fakeConns{}, alerts: &fakeAlerts{},
		logs: &lockedBuffer{}}
	db, err := store.Open(context.Background(), store.Options{
		Path:       dbPath(e.dir),
		AppVersion: "0.1.0",
		Logger:     slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	e.db = db
	e.start(func(o *Options) { o.Clock = time.Now })
	rec := &pruneRecorder{next: e.svc.prune}
	e.svc.prune = rec.prune
	return e, rec
}

// startJanitor runs the service's janitor until the returned stop is called; stop waits for RunJanitor to return
// and fails the test if it does not return before the bubble's time moves. A test that fails first stops it in its
// cleanup, so that the bubble ends with the test's own error.
func startJanitor(t *testing.T, svc *Service) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.RunJanitor(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return func() {
		t.Helper()
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("RunJanitor did not return when its context ended")
		}
	}
}

// wantRuns checks the times of the janitor's runs: t0 plus each offset, exactly.
func wantRuns(t *testing.T, rec *pruneRecorder, t0 time.Time, offsets ...time.Duration) {
	t.Helper()
	var got []time.Duration
	for _, at := range rec.times() {
		got = append(got, at.Sub(t0))
	}
	if fmt.Sprint(got) != fmt.Sprint(offsets) {
		t.Fatalf("the janitor ran at t0 + %v, want t0 + %v", got, offsets)
	}
}

// TestJanitorScheduleUnderFakeClock is 03 §15's prune schedule: the janitor prunes at startup and then every hour,
// at the service's clock. A row exactly at its limit stays (03 §4.7: strictly older is deleted) and goes with the run
// an hour later: a session whose idle expiry is the time of a run, a pending sign-up whose 14 days end then (with
// its user.signup_expired row, actor system), and an audit row whose 30 days end then. No connection is closed and
// no alert is raised (03 §7.7), and each run logs one debug line with its counts.
func TestJanitorScheduleUnderFakeClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, rec := newBubbleEnv(t)
		t0 := time.Now()
		const hour = time.Hour

		sam := e.addUser("Sam", func(u *store.User) { u.CreatedAt = t0.Add(-60 * 24 * hour) })
		var session store.Session
		var pending store.User
		e.write(func(q *store.Q) error {
			session = store.Session{UserID: sam.ID, TokenHash: []byte("token hash of a session"), Name: "Chrome on Windows",
				CreatedAt: t0.Add(-29 * 24 * hour), ExpiresAt: t0.Add(150 * 24 * hour), IdleExpiresAt: t0.Add(hour)}
			if err := q.CreateSession(&session); err != nil {
				return err
			}
			pending = store.User{Username: "Kim", UsernameKey: "kim", Status: store.StatusPending, CreatedVia: "signup",
				CreatedAt: t0.Add(-14*24*hour + hour)}
			if err := q.CreateUser(&pending); err != nil {
				return err
			}
			return q.AppendAudit(store.AuditEntry{At: t0.Add(-30*24*hour + 3*hour), Action: "auth.logout",
				Actor: store.Actor{Kind: store.ActorUser, UserID: sam.ID, Name: "Sam", IP: ipA}})
		})
		state := func() (sessions, pendingUsers, oldAudit int) {
			t.Helper()
			return e.rawCount("sessions"), e.rawInt("SELECT count(*) FROM users WHERE status = 'pending'"),
				e.rawInt("SELECT count(*) FROM audit_log WHERE action = 'auth.logout'")
		}
		wantState := func(when string, sessions, pendingUsers, oldAudit int) {
			t.Helper()
			s, p, a := state()
			if s != sessions || p != pendingUsers || a != oldAudit {
				t.Fatalf("%s: %d sessions, %d pending sign-ups, %d old audit rows; want %d, %d, %d", when, s, p, a,
					sessions, pendingUsers, oldAudit)
			}
		}

		stop := startJanitor(t, e.svc)
		synctest.Wait()
		wantRuns(t, rec, t0, 0) // at startup, at once
		wantState("after the run at startup", 1, 1, 1)

		time.Sleep(hour)
		synctest.Wait()
		wantRuns(t, rec, t0, 0, hour)
		wantState("after the run at +1h, where each row is exactly at its limit", 1, 1, 1)
		if n := e.auditCount("user.signup_expired"); n != 0 {
			t.Fatalf("%d user.signup_expired rows before the sign-up expired", n)
		}

		time.Sleep(hour)
		synctest.Wait()
		wantRuns(t, rec, t0, 0, hour, 2*hour)
		wantState("after the run at +2h", 0, 0, 1)
		expired := e.audit("user.signup_expired")
		if len(expired) != 1 || !expired[0].At.Equal(t0.Add(2*hour)) || expired[0].Actor.Kind != store.ActorSystem ||
			expired[0].Actor.Name != "system" || expired[0].Actor.IP != "" || expired[0].TargetKind != "user" ||
			expired[0].TargetID != string(pending.ID) || expired[0].TargetName != "Kim" {
			t.Fatalf("user.signup_expired rows = %+v; want one at +2h by system for Kim", expired)
		}

		time.Sleep(hour)
		synctest.Wait()
		wantState("after the run at +3h, where the audit row is exactly 30 days old", 0, 0, 1)
		time.Sleep(hour)
		synctest.Wait()
		wantState("after the run at +4h", 0, 0, 0)
		wantRuns(t, rec, t0, 0, hour, 2*hour, 3*hour, 4*hour)

		// Nothing early: the next run is an hour after the last one, not a minute sooner.
		time.Sleep(59 * time.Minute)
		synctest.Wait()
		wantRuns(t, rec, t0, 0, hour, 2*hour, 3*hour, 4*hour)
		time.Sleep(time.Minute)
		synctest.Wait()
		wantRuns(t, rec, t0, 0, hour, 2*hour, 3*hour, 4*hour, 5*hour)

		stop()
		wantRuns(t, rec, t0, 0, hour, 2*hour, 3*hour, 4*hour, 5*hour) // stopping is no run
		time.Sleep(3 * hour)
		synctest.Wait()
		wantRuns(t, rec, t0, 0, hour, 2*hour, 3*hour, 4*hour, 5*hour)

		if calls := e.conns.take(); len(calls) != 0 {
			t.Errorf("the janitor closed connections: %+v (03 §7.7: expiry is not pushed)", calls)
		}
		if alerts := e.alerts.take(); len(alerts) != 0 {
			t.Errorf("the janitor raised alerts: %+v", alerts)
		}
		logs := e.logs.String()
		if n := strings.Count(logs, `msg="janitor: pruned expired rows"`); n != 6 {
			t.Errorf("%d debug lines for 6 runs:\n%s", n, logs)
		}
		if !strings.Contains(logs, `level=DEBUG msg="janitor: pruned expired rows" component=auth sessions=1 tokens=0 `+
			`invites=0 pending=1 device_codes=0 devices=0 device_tokens=0 audit=0`) {
			t.Errorf("no debug line with the counts of the run at +2h:\n%s", logs)
		}
		if strings.Contains(logs, "level=WARN") || strings.Contains(logs, "Kim") {
			t.Errorf("the janitor logged a warning or a username:\n%s", logs)
		}
	})
}

// TestJanitorKeepsGoingAfterAFailedRun: a run that fails is logged at warn with the error, and the janitor runs
// again an hour later as usual.
func TestJanitorKeepsGoingAfterAFailedRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, rec := newBubbleEnv(t)
		t0 := time.Now()
		rec.fail = func(_ context.Context, run int) error {
			if run == 0 {
				return errors.New("store: prune: disk I/O error")
			}
			return nil
		}
		startJanitor(t, e.svc)
		synctest.Wait()
		wantRuns(t, rec, t0, 0)
		logs := e.logs.String()
		if !strings.Contains(logs, `level=WARN msg="janitor: pruning expired rows failed; it runs again in an hour" `+
			`component=auth sessions=0 tokens=0 invites=0 pending=0 device_codes=0 devices=0 device_tokens=0 audit=0 `+
			`err="store: prune: disk I/O error"`) {
			t.Fatalf("no warn line for the failed run:\n%s", logs)
		}
		time.Sleep(time.Hour)
		synctest.Wait()
		wantRuns(t, rec, t0, 0, time.Hour)
		if n := strings.Count(e.logs.String(), `msg="janitor: pruned expired rows"`); n != 1 {
			t.Fatalf("the run after the failure logged %d debug lines, want 1:\n%s", n, e.logs.String())
		}
	})
}

// TestJanitorStopsWithItsContext: a context that has ended runs nothing; one that ends during a run ends the janitor
// after it without a warning (shutdown is no failure); one that ends between runs ends it at once.
func TestJanitorStopsWithItsContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, rec := newBubbleEnv(t)
		t0 := time.Now()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		e.svc.RunJanitor(ctx) // returns at once
		wantRuns(t, rec, t0)

		// The context ends during the run at startup: the store's error is no failure to report.
		ctx, cancel = context.WithCancel(context.Background())
		rec.fail = func(ctx context.Context, run int) error {
			cancel()
			return fmt.Errorf("store: prune: %w", ctx.Err())
		}
		e.svc.RunJanitor(ctx)
		wantRuns(t, rec, t0, 0)
		if logs := e.logs.String(); strings.Contains(logs, "janitor") {
			t.Fatalf("a run that shutdown cut short was logged:\n%s", logs)
		}

		// Between runs: stop returns without the clock moving.
		rec.fail = nil
		stop := startJanitor(t, e.svc)
		synctest.Wait()
		time.Sleep(30 * time.Minute)
		stop()
		if !time.Now().Equal(t0.Add(30 * time.Minute)) {
			t.Fatalf("stopping took until %v", time.Now())
		}
		wantRuns(t, rec, t0, 0, 0)
	})
}

// TestJanitorLogsExpiredDevices: a run's debug line counts the devices the store's devices rule deleted (M2 02 §6.7,
// PruneStats.Devices) next to the other counts, and names none of them: the device.expired rows in the audit log do.
func TestJanitorLogsExpiredDevices(t *testing.T) {
	e := newSvcEnv(t)
	alex := e.addUser("Alex")
	now := e.clk.now()
	var gone, live store.Device
	e.write(func(q *store.Q) error {
		// Linked two hours ago; its only refresh token expired an hour ago: the janitor's rule deletes it.
		gone = store.Device{UserID: alex.ID, Name: "Old Laptop", ClientKind: "desktop", OS: "windows", AppVersion: "0.2.0",
			LinkedVia: "device_flow", CreatedAt: now.Add(-2 * time.Hour), LastIP: ipA}
		if err := q.CreateDevice(&gone); err != nil {
			return err
		}
		if err := q.InsertDeviceToken(store.DeviceToken{TokenHash: []byte("expired refresh"), DeviceID: gone.ID,
			Kind: tokenKindRefresh, CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour)}); err != nil {
			return err
		}
		// One with a live refresh token stays.
		live = store.Device{UserID: alex.ID, Name: "Gaming PC", ClientKind: "desktop", OS: "windows", AppVersion: "0.2.0",
			LinkedVia: "device_flow", CreatedAt: now.Add(-2 * time.Hour), LastIP: ipA}
		if err := q.CreateDevice(&live); err != nil {
			return err
		}
		return q.InsertDeviceToken(store.DeviceToken{TokenHash: []byte("live refresh"), DeviceID: live.ID,
			Kind: tokenKindRefresh, CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(time.Hour)})
	})
	e.svc.janitorRun(context.Background())

	if devs := e.devices(alex.ID); len(devs) != 1 || devs[0].ID != live.ID {
		t.Fatalf("devices after the run = %+v, want only %s", devs, live.ID)
	}
	if rows := e.audit("device.expired"); len(rows) != 1 || rows[0].TargetID != string(gone.ID) {
		t.Fatalf("device.expired rows = %+v, want one for %s", rows, gone.ID)
	}
	logs := e.logs.String()
	if !strings.Contains(logs, `msg="janitor: pruned expired rows" component=auth sessions=0 tokens=0 invites=0 `+
		`pending=0 device_codes=0 devices=1 device_tokens=0 audit=0`) {
		t.Errorf("no debug line counting the expired device:\n%s", logs)
	}
	for _, name := range []string{"Old Laptop", "Gaming PC", string(gone.ID)} {
		if strings.Contains(logs, name) {
			t.Errorf("the janitor's log names %q:\n%s", name, logs)
		}
	}
}
