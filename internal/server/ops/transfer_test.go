package ops

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/netx"
)

// fakeTransferStore is 03's transfer_months and meta tables in two maps. It records every AddTransfer call.
type fakeTransferStore struct {
	mu      sync.Mutex
	months  map[string]byteCount
	meta    map[string]string
	adds    []transferAdd
	failAdd error // returned by AddTransfer when set
	// failAddMonth limits failAdd to the calls for this month; "" means all of them.
	failAddMonth string
	// onAdd, when set, runs at the start of every AddTransfer call with its month: a look at the state in the
	// middle of a flush.
	onAdd   func(month string)
	failGet error // returned by TransferMonth when set
	failSet error // returned by SetMeta when set
	failGM  error // returned by Meta when set
	sets    int   // SetMeta calls that succeeded
}

type transferAdd struct {
	month           string
	egress, ingress int64
	at              time.Time
}

func newFakeTransferStore() *fakeTransferStore {
	return &fakeTransferStore{months: map[string]byteCount{}, meta: map[string]string{}}
}

func (s *fakeTransferStore) AddTransfer(_ context.Context, month string, egress, ingress int64, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.onAdd != nil {
		s.onAdd(month)
	}
	if s.failAdd != nil && (s.failAddMonth == "" || s.failAddMonth == month) {
		return s.failAdd
	}
	s.adds = append(s.adds, transferAdd{month, egress, ingress, now})
	s.months[month] = s.months[month].add(byteCount{egress, ingress})
	return nil
}

func (s *fakeTransferStore) TransferMonth(_ context.Context, month string) (int64, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failGet != nil {
		return 0, 0, s.failGet
	}
	return s.months[month].egress, s.months[month].ingress, nil
}

func (s *fakeTransferStore) Meta(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failGM != nil {
		return "", s.failGM
	}
	return s.meta[key], nil
}

func (s *fakeTransferStore) SetMeta(_ context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failSet != nil {
		return s.failSet
	}
	s.meta[key] = value
	s.sets++
	return nil
}

func (s *fakeTransferStore) month(m string) byteCount {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.months[m]
}

func (s *fakeTransferStore) metaValue(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.meta[key]
}

func (s *fakeTransferStore) addCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.adds)
}

func (s *fakeTransferStore) fail(set func(*fakeTransferStore)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	set(s)
}

// fakePolicy is the settings cache: both values can change while the server runs.
type fakePolicy struct {
	alertGB      atomic.Int64
	releaseCheck atomic.Bool
}

func (p *fakePolicy) TransferAlertGB() int { return int(p.alertGB.Load()) }
func (p *fakePolicy) ReleaseCheck() bool   { return p.releaseCheck.Load() }

// fakeAlerter records the admin alerts that would become push messages.
type fakeAlerter struct {
	mu     sync.Mutex
	alerts []AdminAlert
}

func (a *fakeAlerter) AdminAlert(_ context.Context, alert AdminAlert) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.alerts = append(a.alerts, alert)
}

func (a *fakeAlerter) targets() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.alerts))
	for _, alert := range a.alerts {
		out = append(out, alert.Target)
	}
	return out
}

// clockAt returns a clock for a synctest bubble that reads start now and then runs with the bubble's fake time.
// The times it returns are in a zone nine hours ahead of UTC, so a test that passes proves the month is the UTC
// one. Call it inside the bubble.
func clockAt(start time.Time) func() time.Time {
	offset := time.Until(start)
	ahead := time.FixedZone("UTC+9", 9*60*60)
	return func() time.Time { return time.Now().Add(offset).In(ahead) }
}

// transferRig is a Transfer under test with its fakes; run starts Run in the bubble.
type transferRig struct {
	t       *testing.T
	counter *netx.TransferCounter
	store   *fakeTransferStore
	policy  *fakePolicy
	alerter *fakeAlerter
	tr      *Transfer
	cancel  context.CancelFunc
	done    chan error
}

func newTransferRig(t *testing.T, now func() time.Time, store *fakeTransferStore) *transferRig {
	t.Helper()
	r := &transferRig{t: t, counter: new(netx.TransferCounter), store: store, policy: new(fakePolicy), alerter: new(fakeAlerter)}
	log, _ := testLogger()
	r.tr = NewTransfer(TransferOptions{
		Counter: r.counter, Store: store, Meta: store, Policy: r.policy, Alerter: r.alerter, Now: now, Logger: log,
	})
	return r
}

func (r *transferRig) run() {
	ctx, cancel := context.WithCancel(r.t.Context())
	r.cancel = cancel
	r.done = make(chan error, 1)
	go func() { r.done <- r.tr.Run(ctx) }()
	synctest.Wait()
}

// stop ends Run as a shutdown does and waits for the last flush.
func (r *transferRig) stop() {
	r.t.Helper()
	r.cancel()
	if err := <-r.done; err != nil {
		r.t.Errorf("Run returned %v, want nil", err)
	}
}

// egress counts n bytes sent on the UDP media path; ingress n bytes received on the web path.
func (r *transferRig) egress(n int)  { r.counter.Add(netx.PathMediaUDP, true, n) }
func (r *transferRig) ingress(n int) { r.counter.Add(netx.PathWeb, false, n) }

const gb = 1_000_000_000

// TestTransferFlush: the counted bytes reach the store every 60 s and at shutdown, summed over the paths, and Info
// shows them at once (04 §11.3).
func TestTransferFlush(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newFakeTransferStore()
		store.months["2026-09"] = byteCount{egress: 5000, ingress: 700} // what earlier runs of the server stored
		r := newTransferRig(t, clockAt(time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)), store)

		// Before Run nothing was read: Info has what was counted since the start.
		r.egress(100)
		if info := r.tr.Info(); info.Month != "2026-09" || info.EgressBytes != 100 || info.IngressBytes != 0 {
			t.Errorf("Info before Run = %+v, want the 100 bytes counted so far in 2026-09", info)
		}

		r.run()
		// Run reads the month's row first, and stores what was counted before it started.
		if got := store.month("2026-09"); got != (byteCount{5100, 700}) {
			t.Errorf("store after the start = %+v, want {5100 700}", got)
		}
		if info := r.tr.Info(); info.EgressBytes != 5100 || info.IngressBytes != 700 {
			t.Errorf("Info after the start = %+v, want 5100 / 700", info)
		}

		r.counter.Add(netx.PathMediaUDP, true, 1000)
		r.counter.Add(netx.PathMediaTCP, true, 200)
		r.counter.Add(netx.PathWeb, true, 30)
		r.counter.Add(netx.PathMediaUDP, false, 40)
		r.counter.Add(netx.PathWeb, false, 5)
		// Info counts bytes that are not stored yet.
		if info := r.tr.Info(); info.EgressBytes != 6330 || info.IngressBytes != 745 {
			t.Errorf("Info with unflushed bytes = %+v, want 6330 / 745", info)
		}
		time.Sleep(transferFlushEvery - time.Second)
		synctest.Wait()
		if got := store.month("2026-09"); got != (byteCount{5100, 700}) {
			t.Errorf("store after 59 s = %+v, want nothing new yet", got)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if got := store.month("2026-09"); got != (byteCount{6330, 745}) {
			t.Errorf("store after 60 s = %+v, want {6330 745}", got)
		}
		if info := r.tr.Info(); info.EgressBytes != 6330 || info.IngressBytes != 745 {
			t.Errorf("Info after the flush = %+v: a flush must not change the totals", info)
		}

		// A minute without traffic writes nothing.
		adds := store.addCount()
		time.Sleep(transferFlushEvery)
		synctest.Wait()
		if got := store.addCount(); got != adds {
			t.Errorf("%d AddTransfer calls in a minute without traffic", got-adds)
		}

		// Shutdown: what was counted since the last flush is stored before Run returns.
		r.egress(77)
		time.Sleep(10 * time.Second)
		r.stop()
		if got := store.month("2026-09"); got != (byteCount{6407, 745}) {
			t.Errorf("store after shutdown = %+v, want {6407 745}", got)
		}
		store.mu.Lock()
		last := store.adds[len(store.adds)-1]
		store.mu.Unlock()
		if want := time.Date(2026, 9, 14, 12, 2, 10, 0, time.UTC); !last.at.Equal(want) {
			t.Errorf("the last flush is stamped %v, want %v", last.at.UTC(), want)
		}
		if err := r.tr.Run(t.Context()); err == nil {
			t.Error("a second Run succeeded; Run can be called once")
		}
	})
}

// TestTransferMonthRollover: months are calendar months in UTC. Bytes counted before midnight UTC of the 1st are
// stored in the month that ended, the bytes after it in the new month, which starts at zero (04 §11.3, §17).
func TestTransferMonthRollover(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newFakeTransferStore()
		store.months["2026-09"] = byteCount{egress: 1000, ingress: 100}
		// 23:58:30 UTC on September 30. The clock's own zone already shows October 1, 08:58.
		r := newTransferRig(t, clockAt(time.Date(2026, 9, 30, 23, 58, 30, 0, time.UTC)), store)
		r.run()

		r.egress(10)
		time.Sleep(60 * time.Second) // 23:59:30: a regular flush
		synctest.Wait()
		if got := store.month("2026-09"); got != (byteCount{1010, 100}) {
			t.Fatalf("September after the 23:59:30 flush = %+v, want {1010 100}", got)
		}

		// The last 30 seconds of September.
		r.egress(20)
		r.ingress(3)
		time.Sleep(29 * time.Second) // 23:59:59
		synctest.Wait()
		if info := r.tr.Info(); info.Month != "2026-09" || info.EgressBytes != 1030 || info.IngressBytes != 103 {
			t.Errorf("Info at 23:59:59 = %+v, want September with 1030 / 103", info)
		}
		// Somebody looks at the dashboard while the flush that closes September is in the store: September
		// is still the month, with all of its bytes.
		var during []api.TransferInfo
		store.fail(func(s *fakeTransferStore) {
			s.onAdd = func(string) { during = append(during, r.tr.Info()) }
		})
		time.Sleep(time.Second) // 00:00:00 UTC, October 1: the flush that closes September
		synctest.Wait()
		store.fail(func(s *fakeTransferStore) { s.onAdd = nil })
		if len(during) != 1 || during[0].Month != "2026-09" || during[0].EgressBytes != 1030 || during[0].IngressBytes != 103 {
			t.Errorf("Info during the flush at midnight = %+v, want September with 1030 / 103 once", during)
		}
		if got := store.month("2026-09"); got != (byteCount{1030, 103}) {
			t.Errorf("September at midnight = %+v, want {1030 103}: its last seconds belong to it", got)
		}
		if got := store.month("2026-10"); got != (byteCount{}) {
			t.Errorf("October at midnight = %+v, want nothing yet", got)
		}
		if info := r.tr.Info(); info.Month != "2026-10" || info.EgressBytes != 0 || info.IngressBytes != 0 {
			t.Errorf("Info at midnight = %+v, want October at zero", info)
		}

		// October's first bytes.
		r.egress(500)
		r.ingress(50)
		if info := r.tr.Info(); info.Month != "2026-10" || info.EgressBytes != 500 || info.IngressBytes != 50 {
			t.Errorf("Info in October = %+v, want 500 / 50", info)
		}
		time.Sleep(60 * time.Second) // 00:01:00
		synctest.Wait()
		if got := store.month("2026-10"); got != (byteCount{500, 50}) {
			t.Errorf("October after its first flush = %+v, want {500 50}", got)
		}
		if got := store.month("2026-09"); got != (byteCount{1030, 103}) {
			t.Errorf("September changed after it ended: %+v", got)
		}
		r.stop()

		// The flushes were at 23:58:30 (start, nothing to store), 23:59:30, 00:00:00 and 00:01:00.
		store.mu.Lock()
		defer store.mu.Unlock()
		var got []string
		for _, add := range store.adds {
			got = append(got, add.month+"@"+add.at.UTC().Format("15:04:05"))
		}
		want := []string{"2026-09@23:59:30", "2026-09@00:00:00", "2026-10@00:01:00"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("flushes = %v, want %v", got, want)
		}
	})
}

// TestTransferMonthEndStoreFailure: a store that fails across the start of a month moves no bytes into the new
// month. What was counted before midnight is stored for the month that ended once the store works again, and Info
// shows the new month without it in the meantime.
func TestTransferMonthEndStoreFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newFakeTransferStore()
		store.months["2026-09"] = byteCount{egress: 1000, ingress: 100}
		r := newTransferRig(t, clockAt(time.Date(2026, 9, 30, 23, 58, 30, 0, time.UTC)), store)
		r.run()
		october := func(when string, egress, ingress int64) {
			t.Helper()
			if info := r.tr.Info(); info.Month != "2026-10" || info.EgressBytes != egress || info.IngressBytes != ingress {
				t.Errorf("Info %s = %+v, want October with %d / %d", when, info, egress, ingress)
			}
		}

		// The store fails from 23:59 on: the regular flush at 23:59:30 already.
		r.egress(10)
		store.fail(func(s *fakeTransferStore) { s.failAdd = errors.New("database is locked") })
		time.Sleep(60 * time.Second) // 23:59:30
		synctest.Wait()
		r.egress(20)
		r.ingress(3)
		// Info in the middle of every flush from here on: the totals never jump while the bytes change places.
		var during []string
		store.fail(func(s *fakeTransferStore) {
			s.onAdd = func(month string) {
				info := r.tr.Info()
				during = append(during, fmt.Sprintf("%s: %s %d/%d", month, info.Month, info.EgressBytes, info.IngressBytes))
			}
		})
		time.Sleep(30 * time.Second) // 00:00:00 UTC, October 1: the flush that closes September fails too
		synctest.Wait()
		if got := store.addCount(); got != 0 {
			t.Fatalf("%d AddTransfer calls succeeded while the store fails", got)
		}
		october("at midnight, with September's last bytes not stored", 0, 0)

		r.egress(500)
		r.ingress(50)
		october("in October", 500, 50)
		time.Sleep(60 * time.Second) // 00:01:00: still failing
		synctest.Wait()
		october("after a failed flush in October", 500, 50)
		if err := r.tr.Flush(t.Context()); err == nil {
			t.Error("Flush returned nil while the store fails")
		}

		store.fail(func(s *fakeTransferStore) { s.failAdd = nil })
		r.egress(7)
		time.Sleep(60 * time.Second) // 00:02:00
		synctest.Wait()
		if got := store.month("2026-09"); got != (byteCount{1030, 103}) {
			t.Errorf("September once the store works = %+v, want {1030 103}: all that was counted before midnight", got)
		}
		if got := store.month("2026-10"); got != (byteCount{507, 50}) {
			t.Errorf("October once the store works = %+v, want {507 50}: only what was counted after midnight", got)
		}
		october("once the store works", 507, 50)
		r.stop()

		store.mu.Lock()
		defer store.mu.Unlock()
		wantDuring := []string{
			"2026-09: 2026-09 1030/103", // midnight: September is ending, and the store refuses its last bytes
			"2026-09: 2026-10 500/50",   // 00:01:00, and Flush right after it
			"2026-09: 2026-10 500/50",
			"2026-09: 2026-10 507/50", // 00:02:00: September's bytes go through,
			"2026-10: 2026-10 507/50", // then October's
		}
		if !reflect.DeepEqual(during, wantDuring) {
			t.Errorf("Info in the middle of the flushes:\n got %q\nwant %q", during, wantDuring)
		}
		var got []string
		for _, add := range store.adds {
			got = append(got, add.month+"@"+add.at.UTC().Format("15:04:05"))
		}
		if want := []string{"2026-09@00:02:00", "2026-10@00:02:00"}; !reflect.DeepEqual(got, want) {
			t.Errorf("flushes = %v, want %v", got, want)
		}
	})

	// A shutdown before the store has worked again: the last flush stores both months, each with its own bytes.
	synctest.Test(t, func(t *testing.T) {
		store := newFakeTransferStore()
		r := newTransferRig(t, clockAt(time.Date(2026, 12, 31, 23, 59, 30, 0, time.UTC)), store)
		r.run()
		r.egress(40)
		store.fail(func(s *fakeTransferStore) { s.failAdd = errors.New("database is locked") })
		time.Sleep(30 * time.Second) // midnight, January 1
		synctest.Wait()
		r.egress(2)
		time.Sleep(10 * time.Second)
		store.fail(func(s *fakeTransferStore) { s.failAdd = nil })
		r.stop()
		if dec, jan := store.month("2026-12"), store.month("2027-01"); dec.egress != 40 || jan.egress != 2 {
			t.Errorf("after the shutdown: December %+v, January %+v; want 40 and 2 bytes out", dec, jan)
		}
	})
}

// A store that fails for more than a month owes more than one month its last bytes. They are stored in order, and
// a month that was stored is not stored again when the one after it fails.
func TestTransferSeveralMonthEndsPending(t *testing.T) {
	store := newFakeTransferStore()
	counter := new(netx.TransferCounter)
	log, _ := testLogger()
	clock := time.Date(2026, 9, 30, 23, 59, 0, 0, time.UTC)
	tr := NewTransfer(TransferOptions{Counter: counter, Store: store, Now: func() time.Time { return clock }, Logger: log})
	flush := func(at time.Time, closing string) error {
		clock = at
		return tr.flush(t.Context(), at, closing, transferStoreTimeout)
	}
	wantInfo := func(when, month string, egress int64) {
		t.Helper()
		if info := tr.Info(); info.Month != month || info.EgressBytes != egress {
			t.Errorf("Info %s = %+v, want %s with %d bytes out", when, info, month, egress)
		}
	}
	wantStore := func(when string, sep, oct, nov int64) {
		t.Helper()
		if got := [3]int64{store.month("2026-09").egress, store.month("2026-10").egress, store.month("2026-11").egress}; got != [3]int64{sep, oct, nov} {
			t.Errorf("store %s: September, October, November = %v, want [%d %d %d]", when, got, sep, oct, nov)
		}
	}

	if err := flush(clock, ""); err != nil {
		t.Fatal(err)
	}
	counter.Add(netx.PathWeb, true, 100) // September's
	store.fail(func(s *fakeTransferStore) { s.failAdd = errors.New("disk full") })
	if err := flush(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), "2026-09"); err == nil {
		t.Error("the flush at the end of September returned nil while the store fails")
	}
	wantInfo("on October 1", "2026-10", 0)
	counter.Add(netx.PathWeb, true, 200) // October's
	if err := flush(time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC), ""); err == nil {
		t.Error("a flush in October returned nil while the store fails")
	}
	wantInfo("in October", "2026-10", 200)
	if err := flush(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), "2026-10"); err == nil {
		t.Error("the flush at the end of October returned nil while the store fails")
	}
	wantInfo("on November 1", "2026-11", 0)
	counter.Add(netx.PathWeb, true, 400) // November's
	wantInfo("in November", "2026-11", 400)
	wantStore("while it fails", 0, 0, 0)

	// The store takes September's row and refuses October's: September is done, the rest waits.
	store.fail(func(s *fakeTransferStore) { s.failAddMonth = "2026-10" })
	if err := flush(time.Date(2026, 11, 1, 0, 1, 0, 0, time.UTC), ""); err == nil || !strings.Contains(err.Error(), "2026-10") {
		t.Errorf("a flush that could store September only returned %v, want an error about 2026-10", err)
	}
	wantStore("after September went through", 100, 0, 0)
	wantInfo("with October still owed", "2026-11", 400)

	store.fail(func(s *fakeTransferStore) { s.failAdd = nil })
	if err := flush(time.Date(2026, 11, 1, 0, 2, 0, 0, time.UTC), ""); err != nil {
		t.Errorf("the flush once the store works: %v", err)
	}
	wantStore("once the store works", 100, 200, 400)
	wantInfo("once the store works", "2026-11", 400)

	// Nothing is owed any more: the next bytes are November's, once.
	counter.Add(netx.PathWeb, true, 1)
	if err := flush(time.Date(2026, 11, 1, 0, 3, 0, 0, time.UTC), ""); err != nil {
		t.Errorf("a flush after all was stored: %v", err)
	}
	wantStore("after one more flush", 100, 200, 401)
	if got := store.addCount(); got != 4 {
		t.Errorf("%d AddTransfer calls went through, want 4: one for each month and one for the last byte", got)
	}
}

// TestTransferAlertsOnce: the admins are alerted when the month's egress crosses 80 % and 100 % of the setting,
// each threshold once per month, also across a restart; a new month starts again (04 §11.3, §17). A limit that is
// raised has its own thresholds.
func TestTransferAlertsOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newFakeTransferStore()
		now := clockAt(time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC))
		r := newTransferRig(t, now, store)
		r.run()
		minute := func() {
			time.Sleep(transferFlushEvery)
			synctest.Wait()
		}
		wantAlerts := func(when string, want ...string) {
			t.Helper()
			if got := r.alerter.targets(); !reflect.DeepEqual(got, append([]string{}, want...)) {
				t.Errorf("%s: alerts %v, want %v", when, got, want)
			}
		}
		dashboard := func() string {
			var codes []string
			for _, a := range r.tr.Alerts() {
				codes = append(codes, string(a.Code))
			}
			return strings.Join(codes, ",")
		}

		// No limit: no alert, whatever the traffic.
		r.egress(gb / 2)
		minute()
		wantAlerts("without a limit")
		if got := dashboard(); got != "" {
			t.Errorf("dashboard alerts without a limit: %s", got)
		}

		// The setting is read on every tick: 1 GB from now on. 50 % is below both thresholds.
		r.policy.alertGB.Store(1)
		minute()
		wantAlerts("at 50 %")
		if info := r.tr.Info(); info.AlertGB != 1 {
			t.Errorf("Info.AlertGB = %d, want 1", info.AlertGB)
		}

		// One byte below 80 %, then the byte that crosses it.
		r.egress(gb*3/10 - 1)
		minute()
		wantAlerts("one byte below 80 %")
		if got := dashboard(); got != "" {
			t.Errorf("dashboard alerts one byte below 80 %%: %s", got)
		}
		r.egress(1)
		if got := dashboard(); got != "transfer.80" {
			t.Errorf("dashboard alerts at 80 %% = %q, want transfer.80 at once, before the flush", got)
		}
		wantAlerts("at 80 % before the tick")
		minute()
		wantAlerts("at 80 %", "80")
		if got := store.metaValue(MetaTransferAlertSent); got != `"2026-09:80"` {
			t.Errorf("meta %s = %s, want \"2026-09:80\"", MetaTransferAlertSent, got)
		}
		r.alerter.mu.Lock()
		sent := append([]AdminAlert(nil), r.alerter.alerts...)
		r.alerter.mu.Unlock()
		if len(sent) != 1 {
			t.Fatalf("%d alerts at 80 %%, want 1", len(sent))
		}
		wantAt := time.Date(2026, 9, 10, 8, 4, 0, 0, time.UTC) // the flush that saw it
		if a := sent[0]; a.Kind != api.AdminAlertKindTransferThreshold || a.Actor != "system" || !a.At.Equal(wantAt) {
			t.Errorf("alert = %+v, want kind transfer_threshold from system at %v", a, wantAt)
		}

		// More ticks, more traffic below 100 %: no repeat.
		r.egress(gb / 10)
		for range 5 {
			minute()
		}
		wantAlerts("after more ticks at 90 %", "80")

		// A restart in between: the new process knows from the meta table what was sent.
		r.stop()
		r = newTransferRig(t, now, store)
		r.policy.alertGB.Store(1)
		r.run()
		minute()
		wantAlerts("after a restart at 90 %")

		// 100 %.
		r.egress(gb / 10)
		minute()
		wantAlerts("at 100 %", "100")
		if got := store.metaValue(MetaTransferAlertSent); got != `"2026-09:100"` {
			t.Errorf("meta %s = %s, want \"2026-09:100\"", MetaTransferAlertSent, got)
		}
		if got := dashboard(); got != "transfer.100" {
			t.Errorf("dashboard alerts at 100 %% = %q, want transfer.100 alone", got)
		}
		r.egress(gb)
		for range 5 {
			minute()
		}
		wantAlerts("after more ticks at 200 %", "100")

		// An admin raises the limit, as the alert suggests: the dashboard alert goes away at once, and the
		// thresholds of the new limit are alerted when the month gets there, each once.
		r.policy.alertGB.Store(10) // 2 GB of 10: 20 %
		if got := dashboard(); got != "" {
			t.Errorf("dashboard alerts after raising the limit: %s", got)
		}
		minute()
		wantAlerts("after raising the limit", "100")
		if got := store.metaValue(MetaTransferAlertSent); got != `"2026-09:0"` {
			t.Errorf("meta %s = %s after raising the limit, want \"2026-09:0\": nothing is alerted for the new limit", MetaTransferAlertSent, got)
		}
		r.egress(6*gb - 1)
		minute()
		wantAlerts("one byte below 80 % of the raised limit", "100")
		r.egress(1)
		minute()
		wantAlerts("at 80 % of the raised limit", "100", "80")
		if got := store.metaValue(MetaTransferAlertSent); got != `"2026-09:80"` {
			t.Errorf("meta %s = %s, want \"2026-09:80\"", MetaTransferAlertSent, got)
		}
		r.egress(gb)
		for range 5 {
			minute()
		}
		wantAlerts("after more ticks at 90 % of the raised limit", "100", "80")

		// A restart does not bring back what was forgotten, nor forget what was alerted since.
		r.stop()
		r = newTransferRig(t, now, store)
		r.policy.alertGB.Store(10)
		r.run()
		minute()
		wantAlerts("after a restart at 90 % of the raised limit")
		r.egress(gb)
		minute()
		wantAlerts("at 100 % of the raised limit", "100")
		r.egress(gb)
		for range 5 {
			minute()
		}
		wantAlerts("after more ticks at 110 % of the raised limit", "100")

		// Raised again, and this time the month is past 80 % of the new limit when the next flush looks: the
		// admin set that limit a moment ago, knowing the numbers, and is told nothing. 100 % is alerted.
		r.policy.alertGB.Store(12) // 11 GB of 12: 91 %
		minute()
		wantAlerts("at 91 % of a limit raised a moment ago", "100")
		if got := dashboard(); got != "transfer.80" {
			t.Errorf("dashboard alerts at 91 %% of the raised limit = %q, want transfer.80", got)
		}
		if got := store.metaValue(MetaTransferAlertSent); got != `"2026-09:80"` {
			t.Errorf("meta %s = %s, want \"2026-09:80\"", MetaTransferAlertSent, got)
		}
		r.egress(gb)
		for range 3 {
			minute()
		}
		wantAlerts("at 100 % of the limit raised a second time", "100", "100")

		// Lowered below what the month has used: that limit is reached, and 100 % was alerted already.
		r.policy.alertGB.Store(5)
		minute()
		wantAlerts("after lowering the limit", "100", "100")
		// Switched off and on again: the same thresholds, nothing new.
		r.policy.alertGB.Store(0)
		minute()
		r.policy.alertGB.Store(12)
		minute()
		wantAlerts("after switching the alert off and on", "100", "100")
		r.stop()
	})
}

// While the month's row can't be read, the totals are only what was counted since the start and look low. That
// is not a limit that went up: the alerted threshold stays alerted, and nothing goes out twice.
func TestTransferAlertKeptWhileTotalsUnknown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newFakeTransferStore()
		store.months["2026-09"] = byteCount{egress: 2 * gb}
		store.meta[MetaTransferAlertSent] = `"2026-09:100"`
		store.fail(func(s *fakeTransferStore) { s.failGet = errors.New("database is locked") })
		r := newTransferRig(t, clockAt(time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC)), store)
		r.policy.alertGB.Store(1)
		r.run()
		r.egress(1000)
		for range 3 {
			time.Sleep(transferFlushEvery)
			synctest.Wait()
		}
		if info := r.tr.Info(); info.EgressBytes >= gb*8/10 {
			t.Fatalf("Info while the month's row can't be read = %+v: this test needs totals that look low", info)
		}
		if got := store.metaValue(MetaTransferAlertSent); got != `"2026-09:100"` {
			t.Errorf("meta %s = %s while the totals are unknown, want it unchanged", MetaTransferAlertSent, got)
		}

		store.fail(func(s *fakeTransferStore) { s.failGet = nil })
		for range 3 {
			time.Sleep(transferFlushEvery)
			synctest.Wait()
		}
		if info := r.tr.Info(); info.EgressBytes != 2*gb+1000 {
			t.Errorf("Info once the row is read = %+v, want 2 GB and 1000 bytes", info)
		}
		if got := r.alerter.targets(); len(got) != 0 {
			t.Errorf("alerts = %v: 100 %% was alerted before the restart", got)
		}
		if got := store.metaValue(MetaTransferAlertSent); got != `"2026-09:100"` {
			t.Errorf("meta %s = %s, want it unchanged", MetaTransferAlertSent, got)
		}
		r.stop()
	})
}

// A new month alerts again, and a month that jumps past both thresholds between two ticks gets one alert, the
// 100 % one.
func TestTransferAlertsNewMonth(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newFakeTransferStore()
		store.months["2026-09"] = byteCount{egress: 2 * gb}
		store.meta[MetaTransferAlertSent] = `"2026-09:100"`
		r := newTransferRig(t, clockAt(time.Date(2026, 9, 30, 23, 59, 0, 0, time.UTC)), store)
		r.policy.alertGB.Store(1)
		r.run()
		if got := r.alerter.targets(); len(got) != 0 {
			t.Fatalf("alerts at the start of a month that was alerted: %v", got)
		}
		time.Sleep(time.Minute) // midnight
		synctest.Wait()
		if codes := r.tr.Alerts(); codes != nil {
			t.Errorf("dashboard alerts in the first second of October: %+v", codes)
		}

		r.egress(3 * gb) // from 0 % to 300 % within one minute
		time.Sleep(time.Minute)
		synctest.Wait()
		if got := r.alerter.targets(); !reflect.DeepEqual(got, []string{"100"}) {
			t.Errorf("alerts in October = %v, want [100]", got)
		}
		if got := store.metaValue(MetaTransferAlertSent); got != `"2026-10:100"` {
			t.Errorf("meta = %s, want \"2026-10:100\"", got)
		}
		alerts := r.tr.Alerts()
		if len(alerts) != 1 || alerts[0].Code != api.AlertCodeTransfer100 || alerts[0].Severity != api.AlertSeverityError ||
			!reflect.DeepEqual(alerts[0].Params, map[string]any{"month": "2026-10", "alertGb": 1}) {
			t.Errorf("dashboard alerts = %+v", alerts)
		}
		r.stop()
	})
}

// An alert is remembered before it is sent: while the meta table can't be written or read, nothing goes out, and
// the alert follows once it works again. Never twice.
func TestTransferAlertNeedsItsMemory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newFakeTransferStore()
		r := newTransferRig(t, clockAt(time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC)), store)
		r.policy.alertGB.Store(1)
		store.fail(func(s *fakeTransferStore) { s.failGM = errors.New("database is locked") })
		r.run()

		r.egress(gb * 9 / 10)
		time.Sleep(transferFlushEvery)
		synctest.Wait()
		if got := r.alerter.targets(); len(got) != 0 {
			t.Errorf("alerts while the meta table can't be read: %v (it might be a repeat)", got)
		}
		store.fail(func(s *fakeTransferStore) { s.failGM, s.failSet = nil, errors.New("disk full") })
		time.Sleep(transferFlushEvery)
		synctest.Wait()
		if got := r.alerter.targets(); len(got) != 0 {
			t.Errorf("alerts while the meta table can't be written: %v (the next tick would repeat it)", got)
		}
		store.fail(func(s *fakeTransferStore) { s.failSet = nil })
		for range 3 {
			time.Sleep(transferFlushEvery)
			synctest.Wait()
		}
		if got := r.alerter.targets(); !reflect.DeepEqual(got, []string{"80"}) {
			t.Errorf("alerts once the meta table works = %v, want [80]", got)
		}
		r.stop()
	})
}

// A store that fails loses nothing: the bytes stay counted, Info keeps showing them, and the next flush that
// works stores all of them.
func TestTransferStoreFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newFakeTransferStore()
		store.months["2026-09"] = byteCount{egress: 1000}
		store.fail(func(s *fakeTransferStore) {
			s.failAdd, s.failGet = errors.New("database is locked"), errors.New("database is locked")
		})
		log, logs := testLogger()
		counter := new(netx.TransferCounter)
		tr := NewTransfer(TransferOptions{
			Counter: counter, Store: store, Now: clockAt(time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)), Logger: log,
		})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- tr.Run(ctx) }()
		synctest.Wait()

		counter.Add(netx.PathWeb, true, 300)
		time.Sleep(transferFlushEvery)
		synctest.Wait()
		if got := store.addCount(); got != 0 {
			t.Fatalf("%d AddTransfer calls succeeded while the store fails", got)
		}
		// The month's row is unknown, so Info has the bytes counted since the start.
		if info := tr.Info(); info.Month != "2026-09" || info.EgressBytes != 300 {
			t.Errorf("Info while the store fails = %+v, want the 300 bytes counted", info)
		}
		if !strings.Contains(logs.String(), "transfer accounting: the store failed") {
			t.Errorf("no warning about the failing store:\n%s", logs.String())
		}
		if err := tr.Flush(t.Context()); err == nil {
			t.Error("Flush returned nil while the store fails")
		}

		counter.Add(netx.PathWeb, true, 200)
		store.fail(func(s *fakeTransferStore) { s.failAdd, s.failGet = nil, nil })
		time.Sleep(transferFlushEvery)
		synctest.Wait()
		if got := store.month("2026-09"); got.egress != 1500 {
			t.Errorf("store after it recovered = %+v, want 1000 + 300 + 200", got)
		}
		if info := tr.Info(); info.EgressBytes != 1500 {
			t.Errorf("Info after the store recovered = %+v, want 1500", info)
		}

		// Flush stores at once, between two ticks.
		counter.Add(netx.PathWeb, false, 9)
		if err := tr.Flush(t.Context()); err != nil {
			t.Errorf("Flush: %v", err)
		}
		if got := store.month("2026-09"); got != (byteCount{1500, 9}) {
			t.Errorf("store after Flush = %+v, want {1500 9}", got)
		}

		// A failing last flush is logged; Run still returns nil, the shutdown goes on.
		counter.Add(netx.PathWeb, true, 1)
		store.fail(func(s *fakeTransferStore) { s.failAdd = errors.New("database is closed") })
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run returned %v, want nil", err)
		}
		if !strings.Contains(logs.String(), "the last flush failed") {
			t.Errorf("no warning about the failed last flush:\n%s", logs.String())
		}
	})
}

// The current rate is the average over the last five seconds, in bits per second.
func TestTransferRate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newTransferRig(t, nil, newFakeTransferStore())
		r.run()
		if info := r.tr.Info(); info.EgressBps != 0 || info.IngressBps != 0 {
			t.Errorf("rate at the start = %d / %d, want 0", info.EgressBps, info.IngressBps)
		}
		for range 8 {
			r.egress(1_000_000) // 8 Mbit/s out
			r.ingress(125_000)  // 1 Mbit/s in
			time.Sleep(time.Second)
			synctest.Wait()
		}
		if info := r.tr.Info(); info.EgressBps != 8_000_000 || info.IngressBps != 1_000_000 {
			t.Errorf("rate under load = %d / %d bit/s, want 8000000 / 1000000", info.EgressBps, info.IngressBps)
		}
		time.Sleep(2 * time.Second) // two of the last five seconds are silent
		synctest.Wait()
		if info := r.tr.Info(); info.EgressBps != 4_800_000 {
			t.Errorf("rate two seconds after the traffic stopped = %d bit/s, want 4800000", info.EgressBps)
		}
		time.Sleep(4 * time.Second)
		synctest.Wait()
		if info := r.tr.Info(); info.EgressBps != 0 || info.IngressBps != 0 {
			t.Errorf("rate after six silent seconds = %d / %d, want 0", info.EgressBps, info.IngressBps)
		}
		r.stop()
	})
}

// The projection is mtd × days in the month / days elapsed, shown after day 3 (04 §11.3).
func TestProjectMonth(t *testing.T) {
	at := func(s string) time.Time {
		tm, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	for _, c := range []struct {
		name string
		mtd  int64
		now  time.Time
		want int64
	}{
		// 04 §11.4's example: 412 GB by September 29, 20:14 → 428 GB.
		{"the spec's example", 412 * gb, at("2026-09-29T20:14:05Z"), 428_525_167_081},
		{"halfway through a 30-day month", 100 * gb, at("2026-09-16T00:00:00Z"), 200 * gb},
		{"first day", 5 * gb, at("2026-09-01T18:00:00Z"), 0},
		{"one second before the end of day 3", 5 * gb, at("2026-09-03T23:59:59Z"), 0},
		{"the start of day 4", 3 * gb, at("2026-09-04T00:00:00Z"), 30 * gb},
		{"the last instant of a month", 31 * gb, at("2026-10-31T23:59:59.999999999Z"), 31 * gb},
		{"February of a leap year", 14 * gb, at("2028-02-15T00:00:00Z"), 29 * gb},
		{"February otherwise", 14 * gb, at("2027-02-15T00:00:00Z"), 28 * gb},
		{"nothing sent", 0, at("2026-09-16T00:00:00Z"), 0},
		{"the month is the UTC one", 3 * gb, at("2026-09-04T09:00:00+09:00"), 30 * gb},
		{"no overflow", math.MaxInt64, at("2026-09-16T00:00:00Z"), math.MaxInt64},
	} {
		got := projectMonth(c.mtd, c.now)
		// Float arithmetic: a relative error of 1e-9 is a byte per gigabyte.
		if diff := math.Abs(float64(got - c.want)); diff > float64(c.want)*1e-9+1 {
			t.Errorf("%s: projectMonth(%d, %s) = %d, want %d", c.name, c.mtd, c.now.Format(time.RFC3339), got, c.want)
		}
	}

	// Info carries it, and leaves it out during the first three days.
	synctest.Test(t, func(t *testing.T) {
		store := newFakeTransferStore()
		store.months["2026-09"] = byteCount{egress: 100 * gb}
		r := newTransferRig(t, clockAt(time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)), store)
		r.run()
		if info := r.tr.Info(); info.ProjectedEgressBytes != 200*gb {
			t.Errorf("Info.ProjectedEgressBytes on the 16th = %d, want %d", info.ProjectedEgressBytes, int64(200*gb))
		}
		r.stop()

		early := newTransferRig(t, clockAt(time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)), store)
		early.run()
		if info := early.tr.Info(); info.ProjectedEgressBytes != 0 || info.EgressBytes != 100*gb {
			t.Errorf("Info on the 2nd = %+v, want no projection", info)
		}
		early.stop()
	})
}

func TestThresholdCrossed(t *testing.T) {
	for _, c := range []struct {
		egress  int64
		limitGB int
		want    int
	}{
		{0, 0, 0},
		{math.MaxInt64, 0, 0}, // no limit
		{math.MaxInt64, -5, 0},
		{0, 1, 0},
		{799_999_999, 1, 0},
		{800_000_000, 1, 80},
		{999_999_999, 1, 80},
		{1_000_000_000, 1, 100},
		{5_000_000_000, 1, 100},
		{799_999_999_999, 1000, 0},
		{800_000_000_000, 1000, 80},
		{1_000_000_000_000, 1000, 100},
		{math.MaxInt64, math.MaxInt32, 100}, // 2.1 EB still fits
	} {
		if got := thresholdCrossed(c.egress, c.limitGB); got != c.want {
			t.Errorf("thresholdCrossed(%d, %d GB) = %d, want %d", c.egress, c.limitGB, got, c.want)
		}
	}
	// A limit beyond what an int64 of bytes can hold is never reached, and does not overflow.
	if strconv.IntSize == 64 {
		if got := thresholdCrossed(math.MaxInt64-1, math.MaxInt); got != 80 {
			t.Errorf("thresholdCrossed(MaxInt64-1, MaxInt GB) = %d, want 80", got)
		}
		if got := thresholdCrossed(math.MaxInt64/2, math.MaxInt); got != 0 {
			t.Errorf("thresholdCrossed(MaxInt64/2, MaxInt GB) = %d, want 0", got)
		}
	}
}

func TestNextFlush(t *testing.T) {
	at := func(s string) time.Time {
		tm, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	for _, c := range []struct {
		now, want, closing string
	}{
		{"2026-09-14T12:00:00Z", "2026-09-14T12:01:00Z", ""},
		{"2026-09-30T23:58:59Z", "2026-09-30T23:59:59Z", ""},
		{"2026-09-30T23:59:00Z", "2026-10-01T00:00:00Z", "2026-09"}, // exactly a minute before: the month's end
		{"2026-09-30T23:59:30.5Z", "2026-10-01T00:00:00Z", "2026-09"},
		{"2026-10-01T00:00:00Z", "2026-10-01T00:01:00Z", ""},
		{"2026-12-31T23:59:45Z", "2027-01-01T00:00:00Z", "2026-12"},
		{"2028-02-29T23:59:45Z", "2028-03-01T00:00:00Z", "2028-02"},
		// 08:59:45 on October 1 in a zone nine hours ahead is 23:59:45 UTC on September 30.
		{"2026-10-01T08:59:45+09:00", "2026-10-01T00:00:00Z", "2026-09"},
	} {
		got, closing := nextFlush(at(c.now))
		if !got.Equal(at(c.want)) || closing != c.closing {
			t.Errorf("nextFlush(%s) = %s closing %q, want %s closing %q", c.now, got.Format(time.RFC3339Nano), closing, c.want, c.closing)
		}
	}
}

func TestAlertSentCodec(t *testing.T) {
	if got := encodeAlertSent("2026-09", 80); got != `"2026-09:80"` {
		t.Errorf("encodeAlertSent = %s, want the JSON string \"2026-09:80\" (04 §11.3)", got)
	}
	if got := encodeAlertSent("2026-09", 0); got != `"2026-09:0"` {
		t.Errorf("encodeAlertSent = %s, want the JSON string \"2026-09:0\"", got)
	}
	for value, want := range map[string]struct {
		month string
		level int
	}{
		`"2026-09:80"`:   {"2026-09", 80},
		`"2026-12:100"`:  {"2026-12", 100},
		`"2026-09:0"`:    {"2026-09", 0}, // forgotten after the limit was raised
		``:               {},
		`null`:           {},
		`2026-09:80`:     {}, // not JSON
		`"2026-09"`:      {},
		`"2026-09:90"`:   {}, // not a threshold
		`"2026-13:80"`:   {}, // not a month
		`"september:80"`: {},
		`{"month":1}`:    {},
		`"2026-09:80:1"`: {},
	} {
		month, level := decodeAlertSent(value)
		if month != want.month || level != want.level {
			t.Errorf("decodeAlertSent(%s) = %q, %d, want %q, %d", value, month, level, want.month, want.level)
		}
	}
}

func TestNewTransferNeedsCounterAndStore(t *testing.T) {
	for name, o := range map[string]TransferOptions{
		"no counter": {Store: newFakeTransferStore()},
		"no store":   {Counter: new(netx.TransferCounter)},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: NewTransfer did not panic", name)
				}
			}()
			NewTransfer(o)
		}()
	}
}
