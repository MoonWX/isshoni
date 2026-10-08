package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/netx"
)

// Transfer accounting (04 §11.3). netx counts every byte at the sockets: the UDP media sockets, the ICE-TCP
// connections and the HTTP(S) connections, with SRTP, RTCP, STUN and TLS overhead, because that is what a VPS
// provider bills. Transfer moves those counts into 03's table transfer_months, one row per calendar month in UTC,
// and alerts the admins when a month's egress crosses 80 % and 100 % of the setting transferAlertGb.
//
// The wiring builds it in step 8 of 04 §6.1 and runs it for the server's life:
//
//	t := ops.NewTransfer(ops.TransferOptions{Counter: counter, Store: …, Meta: …, Policy: …, Alerter: …})
//	go func() { _ = t.Run(ctx) }() // ctx ends in step 6 of the shutdown (04 §6.4), after the sockets have closed:
//	                               // Run then stores what is left and returns, and only then the store closes

// TransferStore is 03's table transfer_months as Transfer uses it (03 §6). The wiring implements it over
// (*store.Q).AddTransfer and TransferMonth, each call one short transaction.
type TransferStore interface {
	// AddTransfer adds byte counts to a UTC month ("2026-09"), creating the month's row.
	AddTransfer(ctx context.Context, month string, egress, ingress int64, now time.Time) error
	// TransferMonth returns the byte counts of a month, zero when it has no row.
	TransferMonth(ctx context.Context, month string) (egress, ingress int64, err error)
}

// TransferOptions configures NewTransfer. Counter and Store are required.
type TransferOptions struct {
	// Counter is the server's byte counter, the one the listeners and the ICE transports count on.
	Counter *netx.TransferCounter
	// Store holds the monthly totals.
	Store TransferStore
	// Meta remembers which threshold was alerted this month (MetaTransferAlertSent), so a restart does not alert
	// again. nil keeps it in memory only.
	Meta MetaStore
	// Policy gives the alert limit, read on every tick. nil means no limit: no alerts.
	Policy Policy
	// Alerter pushes a threshold alert to the admins. nil only logs it; the dashboard shows it either way.
	Alerter AdminAlerter
	// Now is the clock that decides the month. nil means time.Now.
	Now func() time.Time
	// Logger is the server's logger; Transfer adds component=ops. nil means slog.Default().
	Logger *slog.Logger
}

const (
	// transferFlushEvery is how often the counted bytes go to the store. A crash loses at most this much.
	transferFlushEvery = 60 * time.Second
	// transferSampleEvery and transferRateWindow give the current rate: the counter is sampled every second, and
	// the rate is the average over the last five.
	transferSampleEvery = time.Second
	transferRateWindow  = 5
	// transferStoreTimeout bounds the store calls of one flush; transferFinalTimeout those of the flush at
	// shutdown, whose step has 2 s in all (04 §6.4 step 6).
	transferStoreTimeout = 5 * time.Second
	transferFinalTimeout = 2 * time.Second
	// bytesPerGB: the alert limit counts in GB of 10⁹ bytes, as providers do (04 §11.3).
	bytesPerGB = 1_000_000_000
	// projectionAfter is how much of a month must have passed before a projection means anything.
	projectionAfter = 3 * 24 * time.Hour
	// monthLayout is the key of a month, "2026-09".
	monthLayout = "2006-01"
)

// The alert thresholds, in percent of the limit.
const (
	transferWarnPercent = 80
	transferFullPercent = 100
)

// byteCount is a pair of byte totals.
type byteCount struct{ egress, ingress int64 }

func (b byteCount) add(o byteCount) byteCount {
	return byteCount{satAdd(b.egress, o.egress), satAdd(b.ingress, o.ingress)}
}

// since returns b − o for two readings of the counter, o the earlier one. The counter only grows, so a negative
// difference cannot happen; it is clamped to zero all the same.
func (b byteCount) since(o byteCount) byteCount {
	return byteCount{max(b.egress-o.egress, 0), max(b.ingress-o.ingress, 0)}
}

func (b byteCount) isZero() bool { return b.egress == 0 && b.ingress == 0 }

// satAdd adds two non-negative counts and stops at the largest int64 where the sum would overflow.
func satAdd(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// clampInt64 converts a counter value to the int64 the store and the API use (9.2 EB is out of reach).
func clampInt64(u uint64) int64 {
	if u > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(u)
}

// rateSample is one reading of the counter for the current rate.
type rateSample struct {
	at    time.Time
	total byteCount
}

// Transfer is the server's transfer accounting. NewTransfer builds it, Run drives it until the server stops; Info
// and Alerts may be called at any time and from any goroutine.
type Transfer struct {
	counter *netx.TransferCounter
	store   TransferStore
	meta    MetaStore
	policy  Policy
	alerter AdminAlerter
	now     func() time.Time
	log     *slog.Logger

	running atomic.Bool

	// flushMu makes flushes take turns: the ticks of Run, Flush, and the last flush of a shutdown. It is held
	// across the store calls; mu never is.
	flushMu sync.Mutex

	mu      sync.Mutex
	month   string    // the month that stored belongs to; "" before the first flush
	loaded  bool      // stored was read from the store for month
	stored  byteCount // month's totals in the store, as far as this process knows them
	flushed byteCount // the counter's reading at the last flush that reached the store
	// The alert state: the highest threshold alerted in sentMonth. sentKnown is false until the meta key was read.
	sentKnown bool
	sentMonth string
	sentLevel int
	// The current rate, from the samples of the last transferRateWindow seconds (oldest first).
	samples               []rateSample
	egressBps, ingressBps int64
}

// NewTransfer builds the accounting. It starts nothing and reads nothing. A nil Counter or Store is a wiring bug
// and panics.
func NewTransfer(o TransferOptions) *Transfer {
	if o.Counter == nil {
		panic("ops: NewTransfer: nil Counter")
	}
	if o.Store == nil {
		panic("ops: NewTransfer: nil Store")
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Transfer{
		counter: o.Counter,
		store:   o.Store,
		meta:    o.Meta,
		policy:  o.Policy,
		alerter: o.Alerter,
		now:     now,
		log:     log.With(slog.String("component", "ops")),
	}
}

// Run does the accounting until ctx ends: it reads the month's totals from the store, then stores the newly
// counted bytes every 60 s and at the first instant of each month (UTC), so that a month's row holds the
// bytes counted before the month ended, and checks the alert thresholds after every flush. When ctx ends it
// stores what is left, for at most 2 s (04 §6.4 step 6), and returns nil. A store that fails is logged and tried
// again at the next flush; the bytes stay counted until they are stored. Run can be called once.
func (t *Transfer) Run(ctx context.Context) error {
	if !t.running.CompareAndSwap(false, true) {
		return errors.New("ops: Transfer.Run called more than once")
	}
	t.tick(ctx, "") // the month's totals and the alert state, so Info is right from the start

	sample := time.NewTicker(transferSampleEvery)
	defer sample.Stop()
	next, closing := nextFlush(t.now())
	timer := time.NewTimer(next.Sub(t.now()))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			t.finalFlush(ctx)
			return nil
		case <-sample.C:
			t.sample(t.now())
		case <-timer.C:
			t.tick(ctx, closing)
			next, closing = nextFlush(t.now())
			timer.Reset(next.Sub(t.now()))
		}
	}
}

// nextFlush returns when the next flush is due after one at now: in transferFlushEvery, or at the start of the
// next month if that comes first. For that flush, closing names the month that ends there: everything counted
// until then belongs to it. Otherwise closing is "".
func nextFlush(now time.Time) (at time.Time, closing string) {
	now = now.UTC()
	regular := now.Add(transferFlushEvery)
	monthEnd := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	if monthEnd.After(regular) {
		return regular, ""
	}
	return monthEnd, monthKey(now)
}

// monthKey is the month of an instant in UTC, "2026-09": the key of transfer_months.
func monthKey(t time.Time) string { return t.UTC().Format(monthLayout) }

// tick is one round of Run: flush, then the alert check. closing is nextFlush's.
func (t *Transfer) tick(ctx context.Context, closing string) {
	if ctx.Err() != nil {
		return // the server is stopping: Run's last flush follows
	}
	now := t.now()
	if err := t.flush(ctx, now, closing, transferStoreTimeout); err != nil {
		t.log.LogAttrs(ctx, slog.LevelWarn, "transfer accounting: the store failed; the bytes stay counted and "+
			"the next flush tries again", logx.Err(err))
	}
	t.checkAlerts(ctx, now)
}

// finalFlush stores what is left when the server stops. ctx has ended, so it runs on a context of its own.
func (t *Transfer) finalFlush(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	if err := t.flush(ctx, t.now(), "", transferFinalTimeout); err != nil {
		t.log.LogAttrs(ctx, slog.LevelWarn, "transfer accounting: the last flush failed; the bytes counted since "+
			"the flush before are missing from this month's total", logx.Err(err))
	}
}

// Flush stores the bytes counted since the last flush now, without waiting for the next one. Run does this by
// itself; Flush is for a caller that needs the store up to date at a known moment, such as a backup.
func (t *Transfer) Flush(ctx context.Context) error {
	return t.flush(ctx, t.now(), "", transferStoreTimeout)
}

// flush adds the bytes counted since the last flush to a month's row, and makes sure the totals in memory are
// those of the month of now. The bytes go to the month closing when the flush is the one at a month's end, and to
// the month of now otherwise.
func (t *Transfer) flush(ctx context.Context, now time.Time, closing string, timeout time.Duration) error {
	t.flushMu.Lock()
	defer t.flushMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	current := monthKey(now)
	target := closing
	if target == "" {
		target = current
	}

	reading := t.read()
	t.mu.Lock()
	delta := reading.since(t.flushed)
	t.mu.Unlock()

	var errs []error
	if !delta.isZero() {
		if err := t.store.AddTransfer(ctx, target, delta.egress, delta.ingress, now); err != nil {
			errs = append(errs, fmt.Errorf("ops: adding transfer to %s: %w", target, err))
		} else {
			t.mu.Lock()
			t.flushed = reading
			if t.loaded && t.month == target {
				t.stored = t.stored.add(delta)
			}
			t.mu.Unlock()
		}
	}

	// The first flush, a new month, or a clock that was set to another month: read that month's row.
	t.mu.Lock()
	reload := !t.loaded || t.month != current
	t.mu.Unlock()
	if reload {
		egress, ingress, err := t.store.TransferMonth(ctx, current)
		t.mu.Lock()
		t.month, t.loaded, t.stored = current, err == nil, byteCount{}
		if err == nil {
			t.stored = byteCount{max(egress, 0), max(ingress, 0)}
		}
		t.mu.Unlock()
		if err != nil {
			errs = append(errs, fmt.Errorf("ops: reading the transfer of %s: %w", current, err))
		}
	}
	return errors.Join(errs...)
}

// read returns the counter's totals over all paths.
func (t *Transfer) read() byteCount {
	var b byteCount
	for _, p := range t.counter.Totals() {
		b = b.add(byteCount{clampInt64(p.Egress), clampInt64(p.Ingress)})
	}
	return b
}

// sample takes one reading for the current rate.
func (t *Transfer) sample(now time.Time) {
	reading := t.read()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.samples = append(t.samples, rateSample{at: now, total: reading})
	if len(t.samples) > transferRateWindow+1 {
		t.samples = t.samples[1:]
	}
	first, last := t.samples[0], t.samples[len(t.samples)-1]
	seconds := last.at.Sub(first.at).Seconds()
	if seconds <= 0 {
		return // the first sample, or a clock that does not move
	}
	delta := last.total.since(first.total)
	t.egressBps = int64(float64(delta.egress) * 8 / seconds)
	t.ingressBps = int64(float64(delta.ingress) * 8 / seconds)
}

// alertLimit is the setting transferAlertGb, 0 when there is no limit.
func (t *Transfer) alertLimit() int {
	if t.policy == nil {
		return 0
	}
	return max(t.policy.TransferAlertGB(), 0)
}

// Info returns the transfer of the current month for the dashboard, `isshoni admin status` and doctor
// (api.TransferInfo): the month's totals including the bytes not flushed yet, the alert limit, the projection for
// the whole month and the current rate. The totals of the month before the first flush read the store are the
// bytes counted since the server started; the rate is zero until Run has sampled for a second.
func (t *Transfer) Info() api.TransferInfo {
	now := t.now()
	reading := t.read()

	t.mu.Lock()
	month := t.month
	total := t.stored.add(reading.since(t.flushed))
	egressBps, ingressBps := t.egressBps, t.ingressBps
	t.mu.Unlock()
	if month == "" {
		month = monthKey(now)
	}

	info := api.TransferInfo{
		Month:        month,
		EgressBytes:  total.egress,
		IngressBytes: total.ingress,
		AlertGB:      t.alertLimit(),
		EgressBps:    egressBps,
		IngressBps:   ingressBps,
	}
	if month == monthKey(now) {
		info.ProjectedEgressBytes = projectMonth(total.egress, now)
	}
	return info
}

// projectMonth projects a month-to-date count to the whole month: mtd × days in the month / days elapsed
// (04 §11.3), with the days elapsed as a fraction, so the projection does not jump at midnight. It is 0, "no
// projection", during the first three days: a few hours of traffic say little about a month.
func projectMonth(mtd int64, now time.Time) int64 {
	now = now.UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	elapsed := now.Sub(start)
	if elapsed < projectionAfter || mtd <= 0 {
		return 0
	}
	whole := start.AddDate(0, 1, 0).Sub(start)
	projected := float64(mtd) * (float64(whole) / float64(elapsed))
	if projected >= math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(projected)
}

// thresholdCrossed returns the highest alert threshold that egress has reached for a limit in GB: 0, 80 or 100.
func thresholdCrossed(egress int64, limitGB int) int {
	if limitGB <= 0 {
		return 0
	}
	limit := int64(math.MaxInt64)
	if int64(limitGB) <= math.MaxInt64/bytesPerGB {
		limit = int64(limitGB) * bytesPerGB
	}
	switch {
	case egress >= limit:
		return transferFullPercent
	case egress >= limit/100*transferWarnPercent: // a limit is a multiple of 10⁹, so this is exact
		return transferWarnPercent
	default:
		return 0
	}
}

// Alerts returns the dashboard alert of the current month (04 §11.4): transfer.100 once the month's egress has
// reached the limit, transfer.80 from 80 % of it, none below that or without a limit. It follows the current
// numbers and the current limit, so it goes away when an admin raises the limit; the push message, in contrast,
// is sent once per threshold and month.
func (t *Transfer) Alerts() []api.Alert {
	info := t.Info()
	params := map[string]any{"month": info.Month, "alertGb": info.AlertGB}
	switch thresholdCrossed(info.EgressBytes, info.AlertGB) {
	case transferFullPercent:
		return []api.Alert{{Code: api.AlertCodeTransfer100, Severity: api.AlertSeverityError, Params: params}}
	case transferWarnPercent:
		return []api.Alert{{Code: api.AlertCodeTransfer80, Severity: api.AlertSeverityWarn, Params: params}}
	default:
		return nil
	}
}

// checkAlerts sends the admin alert transfer_threshold when the month's egress has crossed a threshold that was
// not alerted this month yet: each threshold once per month (04 §11.3). A month that jumps past both gets the 100 %
// alert only. The threshold is remembered before the alert goes out, so a store that fails delays an alert but
// never repeats one.
func (t *Transfer) checkAlerts(ctx context.Context, now time.Time) {
	limit := t.alertLimit()
	if limit == 0 {
		return
	}
	// While the month's row can't be read, Info has only the bytes counted since the server started: an alert can
	// then come late, but never without cause.
	info := t.Info()
	level := thresholdCrossed(info.EgressBytes, limit)
	if level == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, transferStoreTimeout)
	defer cancel()
	if !t.loadSent(ctx) {
		return
	}
	t.mu.Lock()
	due := t.sentMonth != info.Month || t.sentLevel < level
	t.mu.Unlock()
	if !due {
		return
	}
	if t.meta != nil {
		if err := t.meta.SetMeta(ctx, MetaTransferAlertSent, encodeAlertSent(info.Month, level)); err != nil {
			t.log.LogAttrs(ctx, slog.LevelWarn, "transfer alert: could not remember the threshold; the alert waits "+
				"for the next flush", logx.Err(err))
			return
		}
	}
	t.mu.Lock()
	t.sentMonth, t.sentLevel = info.Month, level
	t.mu.Unlock()

	t.log.LogAttrs(ctx, slog.LevelWarn, fmt.Sprintf("this month's outgoing transfer has reached %d %% of the transfer alert", level),
		slog.String("month", info.Month), slog.Int64("egress_bytes", info.EgressBytes), slog.Int("alert_gb", limit))
	if t.alerter != nil {
		t.alerter.AdminAlert(ctx, AdminAlert{
			Kind:   api.AdminAlertKindTransferThreshold,
			Actor:  AlertActorSystem,
			Target: strconv.Itoa(level),
			At:     now,
		})
	}
}

// loadSent reads the remembered threshold from the meta table, once. It reports whether the alert state is known:
// while the read fails no alert goes out, because it might be a repeat.
func (t *Transfer) loadSent(ctx context.Context) bool {
	t.mu.Lock()
	known := t.sentKnown
	t.mu.Unlock()
	if known {
		return true
	}
	var month string
	var level int
	if t.meta != nil {
		value, err := t.meta.Meta(ctx, MetaTransferAlertSent)
		if err != nil {
			t.log.LogAttrs(ctx, slog.LevelWarn, "transfer alert: could not read which threshold was alerted; "+
				"trying again at the next flush", logx.Err(err))
			return false
		}
		month, level = decodeAlertSent(value)
	}
	t.mu.Lock()
	t.sentKnown, t.sentMonth, t.sentLevel = true, month, level
	t.mu.Unlock()
	return true
}

// encodeAlertSent is the value of MetaTransferAlertSent: the JSON string "2026-09:80".
func encodeAlertSent(month string, level int) string {
	b, _ := json.Marshal(month + ":" + strconv.Itoa(level)) // a string always encodes
	return string(b)
}

// decodeAlertSent reads MetaTransferAlertSent. Anything it can't read, and the empty value of a key that does not
// exist, counts as "nothing alerted".
func decodeAlertSent(value string) (month string, level int) {
	var s string
	if err := json.Unmarshal([]byte(value), &s); err != nil {
		return "", 0
	}
	month, percent, ok := strings.Cut(s, ":")
	if !ok {
		return "", 0
	}
	if _, err := time.Parse(monthLayout, month); err != nil {
		return "", 0
	}
	level, err := strconv.Atoi(percent)
	if err != nil || (level != transferWarnPercent && level != transferFullPercent) {
		return "", 0
	}
	return month, level
}
