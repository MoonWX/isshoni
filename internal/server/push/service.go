package push

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/netx"
)

// ShareStarted is the event behind "Alex started sharing": a share went from starting to live and replaces no
// other share. The wiring converts 01's signal.PushShareStarted.
type ShareStarted struct {
	RoomID, RoomName string
	ShareID          string
	UserID, UserName string   // the sharer; the name is the username
	PresentUserIDs   []string // participants of the room, who already see the share: they get no notification
	At               time.Time
}

// AdminAlert is a security event for the admins (03 §7.11, and 04's transfer_threshold). The wiring converts 03's
// auth.AdminAlert.
type AdminAlert struct {
	Kind, Actor, Target string // Kind is an api.AdminAlertKind; Actor a username, "cli" or "system"
	At                  time.Time
}

// Options configures New. Only Enabled, VAPID, Subject and Store are required.
type Options struct {
	Enabled  bool                    // push.enabled; New refuses to build a disabled service (ErrDisabled)
	VAPID    func() config.VAPIDKeys // SecretStore.VAPID; read once, in New
	Subject  string                  // the VAPID sub claim: a mailto: or https: URL (DeriveSubject)
	Store    Store
	Sender   Sender           // nil: the real one, webpush-go with the guarded HTTP client (04 §14.5)
	Workers  int              // 0: 4
	QueueLen int              // 0: 1024
	Now      func() time.Time // nil: time.Now
	Logger   *slog.Logger     // nil: discard

	// Resolver resolves an endpoint's host in ValidateEndpoint. nil: net.DefaultResolver.
	Resolver netx.Resolver
	// OwnAddrs are the server's public addresses (netx.PublicAddrs.V4 and V6): the guard refuses them, so that an
	// endpoint can't make the server call itself. Invalid (zero) addresses are ignored.
	OwnAddrs []netip.Addr
	// Metrics, when set, gets isshoni_push_sent_total{result} (04 §11.2): ops.Metrics.Registerer().
	Metrics prometheus.Registerer

	// allowPrivate turns off the guard's address, port and IP-literal rules. Tests only (the fake push service
	// listens on a loopback address); this package's tests set it, nothing else can.
	allowPrivate bool
	// rootCAs replaces the system's root certificates in the real sender. Tests only.
	rootCAs *x509.CertPool
}

// ErrDisabled is New's error for Options.Enabled == false: with push.enabled = false the wiring builds no Service,
// passes httpapi a nil Push (the endpoints answer push_unavailable) and auth a nil AdminAlerter (04 §6.6).
var ErrDisabled = errors.New("push: disabled by push.enabled = false")

// Defaults and limits (04 §14.5).
const (
	defaultWorkers  = 4
	defaultQueueLen = 1024
	// drainTimeout is how long Run keeps sending what is queued after its context ended (04 §6.4 step 6).
	drainTimeout = 2 * time.Second
	// dropLogEvery bounds the "dropped" warnings.
	dropLogEvery = time.Minute
)

// retryDelays are the waits before the second and third attempt of a message that got 429, 5xx or no answer.
// A Retry-After of at most maxRetryAfter replaces the wait; nothing is retried past the message's TTL.
var retryDelays = [...]time.Duration{5 * time.Second, 30 * time.Second}

// result is a value of the result label of isshoni_push_sent_total.
type result int

const (
	resultOK       result = iota // the push service accepted the message
	resultGone                   // 404, 410: the subscription is deleted
	resultRejected               // 401, 403: the subscription belongs to another VAPID key; deleted
	resultError                  // refused (400, 413, ...) or still failing after the retries
	resultDropped                // never sent: queue full, recipient over its rate limit, expired, or shutdown
	numResults
)

var resultNames = [numResults]string{"ok", "gone", "rejected", "error", "dropped"}

// Reasons in the "dropped" warning.
const (
	dropQueueFull   = "queue_full"
	dropRateLimited = "recipient_rate_limited"
	dropExpired     = "expired"
	dropShutdown    = "shutdown"
)

// event is a trigger waiting for the dispatcher: exactly one field is set.
type event struct {
	share *ShareStarted
	alert *AdminAlert
}

// job is one message for one subscription.
type job struct {
	kind     api.PushType
	sub      Subscription
	payload  []byte
	opts     SendOptions
	deadline time.Time // when the message's TTL ends: nothing is sent or retried after it
	attempt  int       // 0 for the first try
}

// Service is the Web Push sender. New builds it (and runs the VAPID fingerprint check), Run drives it until its
// context ends. The triggers and SendTest only queue; they may be called before Run, and from any goroutine.
type Service struct {
	log       *slog.Logger
	store     Store
	sender    Sender
	closeSend func() // closes the sender's idle connections when the Service built the sender; else nil
	now       func() time.Time
	resolver  netx.Resolver
	guard     *guard
	publicKey string // base64url without padding
	workers   int

	events chan event
	jobs   chan job
	stop   chan struct{} // closed when Run's context ends: no new work, the queues drain

	running atomic.Bool // Run was called
	closed  atomic.Bool // Run's context ended: triggers are ignored

	// Owned by the dispatcher goroutine.
	dedup   *dedup
	buckets *buckets

	mu      sync.Mutex
	stopped bool                     // set with closed, under mu: no retry is scheduled or queued any more
	retries map[*time.Timer]struct{} // retries that wait for their time

	counts [numResults]atomic.Uint64
	metric *prometheus.CounterVec // nil without Options.Metrics

	dropMu      sync.Mutex
	dropLogged  time.Time // when the last "dropped" warning was logged
	dropPending int       // drops since then
}

// New builds the Service. Before it returns it compares the VAPID key with the fingerprint in 03's meta table and,
// after a rotation, deletes every subscription (04 §5.2): the wiring calls New before any listener serves, so no
// request ever sees a subscription of the old key. New starts no goroutine; Run does.
func New(ctx context.Context, opts Options) (*Service, error) {
	if !opts.Enabled {
		return nil, ErrDisabled
	}
	if opts.Store == nil {
		return nil, errors.New("push: Options.Store is required")
	}
	if opts.VAPID == nil {
		return nil, errors.New("push: Options.VAPID is required")
	}
	keys := opts.VAPID()
	pub, err := checkVAPIDKeys(keys)
	if err != nil {
		return nil, err
	}
	keys.Public = base64.RawURLEncoding.EncodeToString(pub)
	if !validSubject(opts.Subject) {
		return nil, errors.New("push: Options.Subject must be a mailto: or https: URL (see DeriveSubject)")
	}

	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Service{
		log:       log.With("component", "push"),
		store:     opts.Store,
		sender:    opts.Sender,
		now:       opts.Now,
		resolver:  opts.Resolver,
		guard:     newGuard(opts.OwnAddrs, opts.allowPrivate),
		publicKey: keys.Public,
		workers:   opts.Workers,
		stop:      make(chan struct{}),
		dedup:     newDedup(shareDedupWindow),
		buckets:   newBuckets(recipientBurst, recipientRefill),
		retries:   map[*time.Timer]struct{}{},
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.resolver == nil {
		s.resolver = net.DefaultResolver
	}
	if s.workers <= 0 {
		s.workers = defaultWorkers
	}
	queueLen := opts.QueueLen
	if queueLen <= 0 {
		queueLen = defaultQueueLen
	}
	s.events = make(chan event, queueLen)
	s.jobs = make(chan job, queueLen)

	rotated, deleted, err := checkVAPIDFingerprint(ctx, s.store, vapidFingerprint(pub))
	if err != nil {
		return nil, err
	}
	if rotated {
		s.log.Info("the VAPID key changed: deleted every push subscription; each browser subscribes again when the app opens",
			"count", deleted)
	}

	if opts.Metrics != nil {
		s.metric = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "isshoni_push_sent_total", Help: "Web Push messages by result.",
		}, []string{"result"})
		if err := opts.Metrics.Register(s.metric); err != nil {
			return nil, fmt.Errorf("push: register metrics: %w", err)
		}
		for _, name := range resultNames {
			s.metric.WithLabelValues(name) // every result is exported from the start, at 0
		}
	}
	if s.sender == nil {
		wp := newWebPushSender(keys, opts.Subject, s.guard, opts.rootCAs)
		s.sender, s.closeSend = wp, wp.Close
	}
	return s, nil
}

// checkVAPIDKeys decodes the pair and checks that it is one: a 65-byte uncompressed P-256 point that belongs to the
// 32-byte private scalar. It returns the public key's bytes.
func checkVAPIDKeys(k config.VAPIDKeys) ([]byte, error) {
	pub, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(k.Public, "="))
	if err != nil || len(pub) != 65 {
		return nil, errors.New("push: the VAPID public key is not a 65-byte P-256 point in base64url")
	}
	priv, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(k.Private.Reveal(), "="))
	if err != nil {
		return nil, errors.New("push: the VAPID private key is not base64url")
	}
	key, err := ecdh.P256().NewPrivateKey(priv)
	if err != nil {
		return nil, errors.New("push: the VAPID private key is not a P-256 key")
	}
	if !bytes.Equal(key.PublicKey().Bytes(), pub) {
		return nil, errors.New("push: the VAPID public key does not belong to the private key")
	}
	return pub, nil
}

// validSubject reports whether s can be a VAPID sub claim (RFC 8292 §2.1): a mailto: or an https: URL. It is the
// rule that config applies to push.subject (04 §4.5), so a config that validates never fails here.
func validSubject(s string) bool {
	return strings.HasPrefix(s, "mailto:") && len(s) > len("mailto:") ||
		strings.HasPrefix(s, "https:") && len(s) > len("https:")
}

// DeriveSubject returns the VAPID subject for Options.Subject (04 §4.3, §14.1): the config's push.subject when it
// is set, else "mailto:" and tls.acme_email when that is set, else the public origin (config.Site.Origin). The
// claim must be a mailto: or https: URL (Apple rejects tokens with any other sub), so the http:// origin of a
// development server is given as https://: it only names a contact, nothing connects to it.
func DeriveSubject(pushSubject, acmeEmail, origin string) string {
	switch {
	case pushSubject != "":
		return pushSubject
	case acmeEmail != "":
		return "mailto:" + acmeEmail
	case strings.HasPrefix(origin, "http://"):
		return "https://" + strings.TrimPrefix(origin, "http://")
	default:
		return origin
	}
}

// VAPIDPublicKey returns the server's VAPID public key: base64url without padding, the 65-byte uncompressed P-256
// point that a browser passes to pushManager.subscribe as applicationServerKey. 03 publishes it in GET /info.
func (s *Service) VAPIDPublicKey() string { return s.publicKey }

// Run starts the workers, the dispatcher and the daily prune, and blocks until ctx ends. Then it stops taking
// work, keeps sending what is already queued for at most 2 seconds (04 §6.4), drops the rest and the pending
// retries, and returns nil. Run can be called once.
func (s *Service) Run(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return errors.New("push: Run called more than once")
	}
	// The work context outlives ctx by the drain time.
	work, cancelWork := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelWork()

	dispatched := make(chan struct{}) // closed when the dispatcher is done: no more jobs will be queued
	var wg sync.WaitGroup
	wg.Go(func() {
		defer close(dispatched)
		s.dispatch(work)
	})
	for range s.workers {
		wg.Go(func() { s.work(work, dispatched) })
	}
	wg.Go(func() { s.pruneLoop(ctx) })

	<-ctx.Done()
	s.shutdown()
	drain := time.AfterFunc(drainTimeout, cancelWork)
	wg.Wait()
	drain.Stop()
	if s.closeSend != nil {
		s.closeSend()
	}
	return nil
}

// shutdown ends the intake: triggers are ignored from now on, and the retries that wait are given up.
func (s *Service) shutdown() {
	s.closed.Store(true)
	s.mu.Lock()
	s.stopped = true
	waiting := 0
	for t := range s.retries {
		if t.Stop() {
			waiting++
		}
	}
	clear(s.retries)
	s.mu.Unlock()
	if waiting > 0 {
		s.dropped(waiting, dropShutdown)
	}
	close(s.stop)
}

// ShareStarted queues the "started sharing" notification for everyone who wants it and is not in the room. It
// never blocks (the hub calls it from its room actor): when the queue is full the event is dropped and counted.
func (s *Service) ShareStarted(ev ShareStarted) {
	ev.PresentUserIDs = slices.Clone(ev.PresentUserIDs)
	s.trigger(event{share: &ev})
}

// AdminAlert queues an admin alert for the admins who want them. It never blocks.
func (s *Service) AdminAlert(a AdminAlert) { s.trigger(event{alert: &a}) }

func (s *Service) trigger(ev event) {
	if s.closed.Load() {
		return
	}
	select {
	case s.events <- ev:
	default:
		s.dropped(1, dropQueueFull)
	}
}

// SendTest queues the test notification for the given subscriptions (03's POST /api/v1/push/test passes the
// caller's current session's). It does not wait for the delivery. The per-recipient rate limit does not apply:
// 03 limits the endpoint itself, and a test that silently sends nothing would look like broken notifications.
// It returns an *api.Error with server_busy when the queue is full, and server_shutdown once Run's context ended.
func (s *Service) SendTest(ctx context.Context, subs []Subscription) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closed.Load() {
		return &api.Error{Code: api.CodeServerShutdown, RetryAfter: 5}
	}
	if len(subs) == 0 {
		return nil
	}
	now := s.now()
	payload, err := testPayload(now)
	if err != nil {
		return err
	}
	o := SendOptions{TTL: ttlTest, Urgency: UrgencyHigh}
	queued := 0
	for _, sub := range subs {
		if s.enqueue(job{kind: api.PushTypeTest, sub: sub, payload: payload, opts: o, deadline: now.Add(o.TTL)}) {
			queued++
		}
	}
	if queued < len(subs) {
		s.dropped(len(subs)-queued, dropQueueFull)
	}
	if queued == 0 {
		return &api.Error{Code: api.CodeServerBusy, RetryAfter: 5}
	}
	return nil
}

// enqueue queues a job without blocking and reports whether there was room.
func (s *Service) enqueue(j job) bool {
	select {
	case s.jobs <- j:
		return true
	default:
		return false
	}
}

// dispatch turns events into jobs: it is the only goroutine that reads the recipients and that owns the dedup
// window and the buckets. After stop it handles the events that are already queued and returns.
func (s *Service) dispatch(ctx context.Context) {
	for {
		select {
		case ev := <-s.events:
			s.expand(ctx, ev)
		case <-s.stop:
			for {
				select {
				case ev := <-s.events:
					s.expand(ctx, ev)
				default:
					return
				}
			}
		}
	}
}

func (s *Service) expand(ctx context.Context, ev event) {
	if ctx.Err() != nil {
		s.dropped(1, dropShutdown)
		return
	}
	switch {
	case ev.share != nil:
		s.expandShare(ctx, *ev.share)
	case ev.alert != nil:
		s.expandAlert(ctx, *ev.alert)
	}
}

// expandShare applies the rules of share.started (04 §14.3, §14.4): at most once per (room, sharer) in 10 minutes;
// to active users whose shareStarted preference is "all", except the sharer and the room's participants.
func (s *Service) expandShare(ctx context.Context, ev ShareStarted) {
	now := s.now()
	log := s.log.With("type", string(api.PushTypeShareStarted), "room_id", ev.RoomID, "user_id", ev.UserID,
		"share_id", ev.ShareID)
	key := shareDedupKey(ev.RoomID, ev.UserID)
	if s.dedup.seen(key, now) {
		log.Debug("not sent: this sharer's last notification for the room is under 10 minutes old")
		return
	}
	exclude := make([]string, 0, 1+len(ev.PresentUserIDs))
	skip := make(map[string]struct{}, 1+len(ev.PresentUserIDs))
	for _, id := range append([]string{ev.UserID}, ev.PresentUserIDs...) {
		if _, dup := skip[id]; !dup && id != "" {
			skip[id] = struct{}{}
			exclude = append(exclude, id)
		}
	}
	subs, err := s.store.Recipients(ctx, RecipientFilter{ExcludeUserIDs: exclude, Pref: PrefShareStarted})
	if err != nil {
		log.Warn("not sent: reading the recipients failed", logx.Err(err))
		return
	}
	s.dedup.mark(key, now)
	ts := ev.At
	if ts.IsZero() {
		ts = now
	}
	payload, err := sharePayload(ev, ts)
	if err != nil {
		log.Warn("not sent", logx.Err(err))
		return
	}
	// The store applied the filter; never notify the sharer or a participant even if an adapter forgot to.
	subs = slices.DeleteFunc(subs, func(sub Subscription) bool {
		_, excluded := skip[sub.UserID]
		return excluded
	})
	s.fanOut(log, api.PushTypeShareStarted, subs, payload, SendOptions{
		TTL: ttlShareStarted, Urgency: UrgencyHigh, Topic: shareTopic(ev.RoomID, ev.UserID),
	}, now)
}

// expandAlert sends an admin alert to the active admins whose adminAlerts preference is on. 03 coalesces the
// alerts that can repeat (signup_pending), so there is no dedup here, and no Topic: a pending security event is
// not replaced by the next one.
func (s *Service) expandAlert(ctx context.Context, a AdminAlert) {
	now := s.now()
	log := s.log.With("type", string(api.PushTypeAdminAlert), "kind", a.Kind)
	subs, err := s.store.Recipients(ctx, RecipientFilter{AdminsOnly: true, Pref: PrefAdminAlerts})
	if err != nil {
		log.Warn("not sent: reading the recipients failed", logx.Err(err))
		return
	}
	ts := a.At
	if ts.IsZero() {
		ts = now
	}
	payload, err := alertPayload(a, ts)
	if err != nil {
		log.Warn("not sent", logx.Err(err))
		return
	}
	s.fanOut(log, api.PushTypeAdminAlert, subs, payload, SendOptions{TTL: ttlAdminAlert, Urgency: UrgencyNormal}, now)
}

// fanOut queues one job per subscription. Each recipient pays one token of their bucket for the event, whatever
// the number of their subscriptions; a recipient without a token gets nothing.
func (s *Service) fanOut(log *slog.Logger, kind api.PushType, subs []Subscription, payload []byte, o SendOptions, now time.Time) {
	hasToken := map[string]bool{}
	var queued, limited, full int
	for _, sub := range subs {
		ok, asked := hasToken[sub.UserID]
		if !asked {
			ok = s.buckets.take(sub.UserID, now)
			hasToken[sub.UserID] = ok
		}
		switch {
		case !ok:
			limited++
		case s.enqueue(job{kind: kind, sub: sub, payload: payload, opts: o, deadline: now.Add(o.TTL)}):
			queued++
		default:
			full++
		}
	}
	if limited > 0 {
		s.dropped(limited, dropRateLimited)
	}
	if full > 0 {
		s.dropped(full, dropQueueFull)
	}
	log.Debug("queued", "recipients", len(hasToken), "subscriptions", queued)
}

// work is one worker: it sends jobs until the dispatcher is done and the queue is empty.
func (s *Service) work(ctx context.Context, dispatched <-chan struct{}) {
	for {
		select {
		case j := <-s.jobs:
			s.deliver(ctx, j)
		case <-dispatched:
			for {
				select {
				case j := <-s.jobs:
					s.deliver(ctx, j)
				default:
					return
				}
			}
		}
	}
}

// outcome is a row of the result table of 04 §14.5.
type outcome int

const (
	outcomeOK        outcome = iota // 2xx
	outcomeGone                     // 404, 410
	outcomeRejected                 // 401, 403
	outcomeRefused                  // 400, 413 and every other answer that a retry can't change
	outcomeTransient                // 408, 429, 5xx, or no answer
)

func classify(res SendResult, err error) outcome {
	if err != nil {
		if errors.Is(err, ErrUndeliverable) {
			return outcomeRefused
		}
		return outcomeTransient
	}
	switch st := res.Status; {
	case st >= 200 && st < 300:
		return outcomeOK
	case st == http.StatusNotFound || st == http.StatusGone:
		return outcomeGone
	case st == http.StatusUnauthorized || st == http.StatusForbidden:
		return outcomeRejected
	case st == http.StatusRequestTimeout || st == http.StatusTooManyRequests || st >= 500:
		return outcomeTransient
	default:
		return outcomeRefused
	}
}

// deliver sends one job and applies the result table of 04 §14.5.
func (s *Service) deliver(ctx context.Context, j job) {
	if ctx.Err() != nil {
		s.dropped(1, dropShutdown)
		return
	}
	o := j.opts
	left := j.deadline.Sub(s.now())
	if left <= 0 {
		s.dropped(1, dropExpired)
		return
	}
	if j.attempt > 0 {
		// A retry asks the push service to keep the message only for what is left of its time.
		o.TTL = min(o.TTL, (left + time.Second - 1).Truncate(time.Second))
	}
	sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	res, err := s.sender.Send(sendCtx, j.sub, j.payload, o)
	cancel()
	if err != nil && ctx.Err() != nil {
		s.dropped(1, dropShutdown) // the drain time ran out during the send: not the subscription's failure
		return
	}

	now := s.now()
	log := s.log.With("type", string(j.kind), "user_id", j.sub.UserID, "push_host", pushHost(j.sub.Endpoint.Reveal()))
	switch classify(res, err) {
	case outcomeOK:
		s.count(resultOK, 1)
		s.record(ctx, log, j.sub, true, now)
	case outcomeGone:
		s.count(resultGone, 1)
		log.Info("the push service says the subscription is gone: deleted", "status", res.Status)
		s.delete(ctx, log, j.sub)
	case outcomeRejected:
		s.count(resultRejected, 1)
		log.Warn("the push service rejected the VAPID key for this subscription (it belongs to another key): deleted",
			"status", res.Status)
		s.delete(ctx, log, j.sub)
	case outcomeRefused:
		s.count(resultError, 1)
		if err != nil {
			log.Warn("not sent", logx.Err(err))
		} else {
			log.Warn("the push service refused the message", "status", res.Status)
		}
		s.record(ctx, log, j.sub, false, now)
	case outcomeTransient:
		if wait, ok := retryWait(j, res, now); ok {
			next := j
			next.attempt++
			if s.retryLater(next, wait) {
				log.Debug("will retry", append(failure(res, err), "attempt", next.attempt, "wait", wait.String())...)
				return
			}
		}
		s.count(resultError, 1)
		log.Warn("not sent: the push service keeps failing", append(failure(res, err), "attempts", j.attempt+1)...)
		s.record(ctx, log, j.sub, false, now)
	}
}

// failure is the log attribute of a failed attempt: the error when no answer arrived, else the status.
func failure(res SendResult, err error) []any {
	if err != nil {
		return []any{logx.Err(err)}
	}
	return []any{"status", res.Status}
}

// retryWait returns how long to wait before the next attempt of j, or false when there is none: after the third
// attempt, when the push service asks for more than maxRetryAfter, or when the wait would pass the message's TTL.
func retryWait(j job, res SendResult, now time.Time) (time.Duration, bool) {
	if j.attempt >= len(retryDelays) {
		return 0, false
	}
	wait := retryDelays[j.attempt]
	if res.RetryAfter > 0 {
		if res.RetryAfter > maxRetryAfter {
			return 0, false
		}
		wait = res.RetryAfter
	}
	if !now.Add(wait).Before(j.deadline) {
		return 0, false
	}
	return wait, true
}

// retryLater queues j again after wait, and reports false when the Service is shutting down. The retry does not
// occupy a worker while it waits.
func (s *Service) retryLater(j job, wait time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return false
	}
	var t *time.Timer
	t = time.AfterFunc(wait, func() {
		// Queued under mu: shutdown, which takes mu before it lets the queues drain, sees the job or stops it.
		s.mu.Lock()
		delete(s.retries, t)
		queued := !s.stopped && s.enqueue(j)
		reason := dropQueueFull
		if s.stopped {
			reason = dropShutdown
		}
		s.mu.Unlock()
		if !queued {
			s.dropped(1, reason)
		}
	})
	s.retries[t] = struct{}{}
	return true
}

func (s *Service) record(ctx context.Context, log *slog.Logger, sub Subscription, ok bool, at time.Time) {
	if err := s.store.RecordResult(ctx, sub.ID, ok, at); err != nil && ctx.Err() == nil {
		log.Warn("recording the push result failed", logx.Err(err))
	}
}

func (s *Service) delete(ctx context.Context, log *slog.Logger, sub Subscription) {
	if err := s.store.Delete(ctx, sub.ID); err != nil && ctx.Err() == nil {
		log.Warn("deleting the push subscription failed", logx.Err(err))
	}
}

// count adds n messages with result r to the counters.
func (s *Service) count(r result, n int) {
	if n <= 0 {
		return
	}
	s.counts[r].Add(uint64(n))
	if s.metric != nil {
		s.metric.WithLabelValues(resultNames[r]).Add(float64(n))
	}
}

// dropped counts n messages that were never sent, and says so in the log at most once a minute.
func (s *Service) dropped(n int, reason string) {
	s.count(resultDropped, n)
	now := s.now()
	s.dropMu.Lock()
	s.dropPending += n
	if !s.dropLogged.IsZero() && now.Sub(s.dropLogged) < dropLogEvery {
		s.dropMu.Unlock()
		return
	}
	n, s.dropPending = s.dropPending, 0
	s.dropLogged = now
	s.dropMu.Unlock()
	s.log.Warn("dropped push notifications", "count", n, "reason", reason)
}
