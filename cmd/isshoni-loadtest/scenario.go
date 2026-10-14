package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/logging"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/protocol"
)

// scenario is one of 02 §15.2's scenarios: its size, its times, and the limits of its checks.
type scenario struct {
	name                    string
	publishers, subscribers int
	duration, warmup        time.Duration
	decodable               bool
	// layout is how subscribers watch: layoutFocus (focus, smoke) or layoutAllHigh.
	layout layout
	limits limits
}

// layout is how the subscribers watch the shares.
type layout uint8

const (
	// layoutFocus: subscriber i treats publisher i as itself and doesn't watch that share; it focuses the next share
	// (the full layer and its sound) and shows the others as thumbnails (the preview, no sound). Every -focus-rotate,
	// at an offset of its own, it moves its focus to the next share.
	layoutFocus layout = iota
	// layoutAllHigh starts as layoutFocus without the rotation; after the warm-up, every 30 s 10 more thumbnails
	// become full layers, until the loss or the server's CPU is too high (the knee) or none is left.
	layoutAllHigh
)

// limits are the pass criteria of a scenario. A zero limit is not checked: the value is only reported.
type limits struct {
	cpuAvgPct, cpuSamplePct float64 // process CPU ÷ (wall × NumCPU): on average, and in any sample interval
	rssPerDownTrack         float64 // bytes: (RSS − baseline RSS) ÷ DownTracks
	lossFinalPct            float64 // unrecovered loss after NACK/RTX, of the expected packets
	pliPerSecond            float64 // upstream PLIs per layer, on average
	pliPer10s               int     // upstream PLIs per layer in any 10 s window
	egressLow, egressHigh   float64 // media.egressBps ÷ the expected egress
	switchP95               time.Duration
	avP99                   time.Duration // |flash − beep| at the viewers
}

// The stop condition of all-high (02 §15.2): a step whose loss (what NACK/RTX did not recover) or average server CPU
// is above these is the knee, and ends the run.
const (
	kneeLossPct = 2.0
	kneeCPUPct  = 90.0
)

// switchTimeout bounds the wait for the first packet of a new layer after a subscribe.update.
const switchTimeout = 5 * time.Second

// scenarios are 02 §15.2's three. focus has the plan's pass criteria. smoke runs in CI (as a Go test against an
// in-process server, and in web/e2e/decodable.spec.ts against a server process next to the browser), so CPU and
// memory are only reported there: the clients share the machine, or the process, with the server.
var scenarios = map[string]scenario{
	"focus": {
		name: "focus", publishers: 10, subscribers: 10, duration: 10 * time.Minute, warmup: time.Minute,
		layout: layoutFocus,
		limits: limits{
			cpuAvgPct: 50, cpuSamplePct: 65, rssPerDownTrack: 1e6, lossFinalPct: 0.5, pliPerSecond: 0.2, pliPer10s: 10,
			egressLow: 0.85, egressHigh: 1.15, switchP95: time.Second, avP99: 5 * time.Millisecond,
		},
	},
	"all-high": {
		name: "all-high", publishers: 10, subscribers: 10, duration: 10 * time.Minute, warmup: time.Minute,
		layout: layoutAllHigh, // report only: no limits
	},
	"smoke": {
		name: "smoke", publishers: 2, subscribers: 2, duration: 30 * time.Second, warmup: 5 * time.Second,
		decodable: true, layout: layoutFocus,
		limits: limits{
			lossFinalPct: 2, pliPerSecond: 1, pliPer10s: 20, egressLow: 0.5, egressHigh: 2,
			switchP95: 3 * time.Second, avP99: 20 * time.Millisecond,
		},
	},
}

// loadTest is one run.
type loadTest struct {
	cfg      *config
	log      *slog.Logger
	pionLog  logging.LoggerFactory
	origin   string // the server's origin, e.g. https://watch.example.com
	wsURL    string
	loopback bool // the server is on this machine: the clients use loopback ICE candidates
	runID    string

	transport *http.Transport // shared by every REST client
	admin     *restClient
	adminID   string
	pace      pacer

	inviteID    string
	inviteToken logx.Secret
	accounts    []*account
	pubs, subs  []*client

	stopMedia   context.CancelFunc // ends the Publishers' sending (teardown)
	meter       *meter
	resumed     atomic.Int64
	mu          sync.Mutex // guards problems
	problems    []string
	problemSeen map[string]bool
}

// maxProblems bounds the problems a report lists; the log has the rest.
const maxProblems = 50

// newLoadTest prepares a run; it does nothing on the network yet.
func newLoadTest(cfg *config, log *slog.Logger) *loadTest {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 16
	transport.ForceAttemptHTTP2 = true
	origin := cfg.server.String()
	wsScheme := "ws"
	if cfg.server.Scheme == "https" {
		wsScheme = "wss"
	}
	lt := &loadTest{
		cfg: cfg, log: log, pionLog: logx.NewPionLoggerFactory(pionLogger(log, cfg.verbose)),
		origin: origin, wsURL: wsScheme + "://" + cfg.server.Host + "/ws", loopback: isLoopback(cfg.server.Hostname()),
		runID: newRunID(), transport: transport, pace: pacer{every: cfg.loginInterval}, problemSeen: map[string]bool{},
	}
	lt.admin = newRESTClient(origin, transport)
	lt.stopMedia = func() {}
	lt.meter = newMeter(lt)
	return lt
}

// pionLogger is the logger of the clients' Pion stacks: errors only, unless -v. Forty clients that connect at once
// log many harmless warnings (an ICE agent that pings before it has a pair, a stream closed under a read at the
// teardown).
func pionLogger(log *slog.Logger, verbose bool) *slog.Logger {
	floor := slog.LevelError
	if verbose {
		floor = slog.LevelDebug
	}
	return slog.New(levelFloor{Handler: log.Handler(), floor: floor}).With("component", "pion")
}

// levelFloor is a handler that drops the records below floor.
type levelFloor struct {
	slog.Handler
	floor slog.Level
}

func (h levelFloor) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= h.floor && h.Handler.Enabled(ctx, l)
}

func (h levelFloor) WithAttrs(attrs []slog.Attr) slog.Handler {
	return levelFloor{Handler: h.Handler.WithAttrs(attrs), floor: h.floor}
}

func (h levelFloor) WithGroup(name string) slog.Handler {
	return levelFloor{Handler: h.Handler.WithGroup(name), floor: h.floor}
}

// isLoopback reports whether a host name or address is this machine's loopback.
func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// problem records something that went wrong during the run, once per text: it fails the run.
func (lt *loadTest) problem(text string) {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	if lt.problemSeen[text] {
		return
	}
	lt.problemSeen[text] = true
	lt.log.Warn("problem", "what", text)
	if len(lt.problems) < maxProblems {
		lt.problems = append(lt.problems, text)
	} else if len(lt.problems) == maxProblems {
		lt.problems = append(lt.problems, "… more problems: see the log")
	}
}

// run runs the scenario: setup, warm-up, measurement and teardown. The report is never nil. The error is a
// *setupError, errInterrupted, or a failure that ended the run early (also listed in the report's failures).
func (lt *loadTest) run(ctx context.Context) (*report, error) {
	rep := newReport(lt.cfg)
	err := lt.runScenario(ctx, rep)
	lt.teardown(rep)
	switch {
	case ctx.Err() != nil:
		err = errInterrupted
		rep.Failures = append(rep.Failures, "interrupted")
	case err != nil:
		var se *setupError
		if errors.As(err, &se) {
			rep.SetupError = se.err.Error()
		}
		rep.Failures = append(rep.Failures, err.Error())
	}
	lt.mu.Lock()
	rep.Failures = append(rep.Failures, lt.problems...)
	lt.mu.Unlock()
	rep.Resumes = int(lt.resumed.Load())
	rep.Failures = slices.DeleteFunc(rep.Failures, func(s string) bool { return s == "" })
	rep.Pass = err == nil && len(rep.Failures) == 0
	if rep.Failures == nil {
		rep.Failures = []string{}
	}
	return rep, err
}

// runScenario is the run up to its teardown.
func (lt *loadTest) runScenario(ctx context.Context, rep *report) error {
	cfg := lt.cfg
	if err := lt.login(ctx); err != nil {
		return err
	}
	// The baseline: the server before any client of the run joined (02 §15.2).
	base, err := lt.dashboard(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return setupErrorf("reading the admin dashboard: %w", err)
	}
	lt.meter.setBaseline(base)
	if err := lt.createInvite(ctx, cfg.publishers+cfg.subscribers); err != nil {
		return err
	}
	for n := 1; n <= cfg.publishers+cfg.subscribers; n++ {
		a, err := lt.register(ctx, n)
		if err != nil {
			return err
		}
		lt.accounts = append(lt.accounts, a)
	}
	rep.Users.Created = len(lt.accounts)
	lt.log.Info("accounts registered", "count", len(lt.accounts), "run", lt.runID)

	if err := lt.connectAll(ctx); err != nil {
		return err
	}
	lt.log.Info("clients connected", "publishers", len(lt.pubs), "subscribers", len(lt.subs))
	// The Publishers send until the teardown, or the run's end.
	media, stopMedia := context.WithCancel(ctx)
	lt.stopMedia = stopMedia
	if err := parallel(ctx, len(lt.pubs), func(i int) error {
		return lt.pubs[i].publish(ctx, media, uint64(i)+1) //nolint:gosec // G115: i is an index, never negative
	}); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &setupError{err: err}
	}
	if err := lt.waitSharesLive(ctx); err != nil {
		return err
	}
	lt.log.Info("shares live", "count", len(lt.pubs))
	if err := lt.subscribeAll(ctx); err != nil {
		return err
	}
	if len(lt.subs) > 0 {
		lt.log.Info("subscriptions delivering", "subscribers", len(lt.subs))
	}
	return lt.measure(ctx, rep)
}

// connectAll opens every client's connection: publishers with role full, subscribers with role viewer.
func (lt *loadTest) connectAll(ctx context.Context) error {
	cfg := lt.cfg
	clients := make([]*client, len(lt.accounts))
	err := parallel(ctx, len(lt.accounts), func(i int) error {
		role, index := protocol.RoleFull, i
		if i >= cfg.publishers {
			role, index = protocol.RoleViewer, i-cfg.publishers
		}
		c, err := lt.connect(ctx, lt.accounts[i], role, index)
		clients[i] = c
		return err
	})
	for i, c := range clients {
		if c == nil {
			continue
		}
		if i < cfg.publishers {
			lt.pubs = append(lt.pubs, c)
		} else {
			lt.subs = append(lt.subs, c)
		}
	}
	return err
}

// waitSharesLive waits until every publisher's room.state shows its share live with both layers and audio.
func (lt *loadTest) waitSharesLive(ctx context.Context) error {
	deadline := time.Now().Add(lt.cfg.setupTimeout)
	for {
		var waiting []string
		for _, c := range lt.pubs {
			if info, ok := c.shareLive(); !ok {
				waiting = append(waiting, fmt.Sprintf("%s (%s, layers %v)", c.acct.name, orNone(string(info.Status)), info.Layers))
			}
		}
		if len(waiting) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("shares not live within %v: %s", lt.cfg.setupTimeout, strings.Join(waiting, ", "))
		}
		if err := sleep(ctx, 50*time.Millisecond); err != nil {
			return err
		}
	}
}

func orNone(s string) string {
	if s == "" {
		return "not in room.state"
	}
	return s
}

// self returns the publisher a subscriber treats as itself (02 §15.2), whose share it doesn't watch: publisher
// i mod P. With one publisher there is none: its share is the only one to watch.
func (lt *loadTest) self(sub int) int {
	if len(lt.pubs) < 2 {
		return -1
	}
	return sub % len(lt.pubs)
}

// nextShare returns the publisher after p that a subscriber watches.
func (lt *loadTest) nextShare(sub, p int) int {
	n := len(lt.pubs)
	for k := 1; k <= n; k++ {
		if q := (p + k) % n; q != lt.self(sub) {
			return q
		}
	}
	return p
}

// shareID returns the share of publisher p.
func (lt *loadTest) shareID(p int) string {
	return lt.pubs[p].share.id
}

// focusWant and thumbWant are the two kinds of subscription of the focus layout, as the web client asks.
func focusWant(shareID string) protocol.SubscriptionWant {
	return protocol.SubscriptionWant{ShareID: shareID, Video: protocol.VideoLayerHigh, Audio: protocol.AudioStateOn}
}

func thumbWant(shareID string) protocol.SubscriptionWant {
	return protocol.SubscriptionWant{ShareID: shareID, Video: protocol.VideoLayerLow, Audio: protocol.AudioStateOff}
}

// subscribeAll gives every subscriber the focus layout's subscriptions in one subscribe.update each, and waits until
// every subscription delivers what it asked for.
func (lt *loadTest) subscribeAll(ctx context.Context) error {
	err := parallel(ctx, len(lt.subs), func(j int) error {
		c := lt.subs[j]
		focus := lt.nextShare(j, lt.self(j))
		c.mu.Lock()
		c.focus = focus
		c.mu.Unlock()
		var wants []protocol.SubscriptionWant
		for p := range lt.pubs {
			switch p {
			case lt.self(j):
			case focus:
				wants = append(wants, focusWant(lt.shareID(p)))
			default:
				wants = append(wants, thumbWant(lt.shareID(p)))
			}
		}
		if err := c.subscribe(ctx, wants...); err != nil {
			return fmt.Errorf("%s: %w", c.acct.name, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	deadline := time.Now().Add(lt.cfg.setupTimeout)
	for {
		var waiting []string
		for _, c := range lt.subs {
			waiting = append(waiting, lt.undelivered(c)...)
		}
		if len(waiting) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			if len(waiting) > 10 {
				waiting = append(waiting[:10], fmt.Sprintf("and %d more", len(waiting)-10))
			}
			return fmt.Errorf("subscriptions not delivering within %v: %s", lt.cfg.setupTimeout, strings.Join(waiting, "; "))
		}
		if err := sleep(ctx, 50*time.Millisecond); err != nil {
			return err
		}
	}
}

// undelivered lists a subscriber's subscriptions that have not delivered yet: video of the wanted layer, and the
// audio of the focused share.
func (lt *loadTest) undelivered(c *client) []string {
	var out []string
	c.mu.Lock()
	wants := make([]protocol.SubscriptionWant, 0, len(c.wants))
	for _, w := range c.wants {
		wants = append(wants, w)
	}
	c.mu.Unlock()
	for _, w := range wants {
		rid := lt.rid(w.Video)
		v := c.recorder(w.ShareID, videoKind)
		if v == nil || !hasLayer(v.Packets(), rid) {
			out = append(out, fmt.Sprintf("%s: %s video of %s", c.acct.name, w.Video, w.ShareID))
		}
		if w.Audio == protocol.AudioStateOn && c.recorder(w.ShareID, audioKind) == nil {
			out = append(out, fmt.Sprintf("%s: audio of %s", c.acct.name, w.ShareID))
		}
	}
	return out
}

// rid returns the simulcast layer of a wanted video layer: f for high, q for low.
func (lt *loadTest) rid(l protocol.VideoLayer) string {
	if l == protocol.VideoLayerLow {
		return lt.cfg.layers[1].RID
	}
	return lt.cfg.layers[0].RID
}

// measure runs the measured part: the warm-up, then the measurement until -duration (or, in all-high, the knee),
// with the samplers and the scenario's driver; then it fills the report.
func (lt *loadTest) measure(ctx context.Context, rep *report) error {
	cfg := lt.cfg
	m := lt.meter
	start := time.Now()
	m.start(start.Add(cfg.warmup))
	rep.StartedAt = start.UTC()
	lt.log.Info("running", "scenario", cfg.scenario.name, "warmup", cfg.warmup.String(), "duration", cfg.duration.String())

	run, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { m.sampleDashboard(run) })
	wg.Go(func() { m.samplePublishers(run) })
	done := make(chan struct{}) // closed when all-high has reached its knee or has nothing left to raise
	switch cfg.scenario.layout {
	case layoutFocus:
		for j, c := range lt.subs {
			wg.Go(func() { lt.rotate(run, j, c, start) })
		}
	case layoutAllHigh:
		wg.Go(func() {
			defer close(done)
			lt.stepUp(run, rep)
		})
	}

	err := sleep(ctx, time.Until(start.Add(cfg.warmup)))
	if err == nil {
		m.markWarm(run)
		lt.log.Info("warm-up over: measuring")
		t := time.NewTimer(time.Until(start.Add(cfg.duration)))
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-t.C:
		case <-done:
		}
		t.Stop()
	}
	stop()
	wg.Wait()
	if err != nil {
		return err
	}
	m.finish(ctx)
	m.report(rep)
	return nil
}

// rotate is a subscriber's focus rotation (layoutFocus): every -focus-rotate, from an offset of its own, it moves its
// focus to the next share, and measures both switches. With fewer than two shares to watch there is nothing to move.
func (lt *loadTest) rotate(ctx context.Context, j int, c *client, start time.Time) {
	first := lt.nextShare(j, lt.self(j))
	if lt.nextShare(j, first) == first {
		return
	}
	every := lt.cfg.focusRotate
	next := start.Add(rand.N(every)) //nolint:gosec // G404: a random offset, not a secret
	for {
		if sleep(ctx, time.Until(next)) != nil {
			return
		}
		lt.rotateOnce(ctx, j, c)
		next = next.Add(every)
		for !next.After(time.Now()) { // a rotation that took longer than -focus-rotate
			next = next.Add(every)
		}
	}
}

// rotateOnce moves a subscriber's focus to the next share in one subscribe.update: the old focus becomes a thumbnail
// and the next share the focus. It measures the switch of both to their new layer.
func (lt *loadTest) rotateOnce(ctx context.Context, j int, c *client) {
	c.mu.Lock()
	old := c.focus
	c.mu.Unlock()
	next := lt.nextShare(j, old)
	oldID, nextID := lt.shareID(old), lt.shareID(next)
	lt.meter.collectAV(c, oldID) // what the old focus had, before its sound goes
	sentAt := time.Now()
	if err := c.subscribe(ctx, thumbWant(oldID), focusWant(nextID)); err != nil {
		if ctx.Err() == nil {
			c.problem("moving the focus: %v", err)
		}
		return
	}
	c.mu.Lock()
	c.focus = next
	c.mu.Unlock()
	lt.log.Debug("focus moved", "client", c.acct.name, "from", oldID, "to", nextID)
	deadline := sentAt.Add(min(switchTimeout, lt.cfg.focusRotate))
	var wg sync.WaitGroup
	wg.Go(func() { lt.meter.measureSwitch(ctx, c, nextID, lt.rid(protocol.VideoLayerHigh), sentAt, deadline) })
	wg.Go(func() { lt.meter.measureSwitch(ctx, c, oldID, lt.rid(protocol.VideoLayerLow), sentAt, deadline) })
	wg.Wait()
}

// stepUp is all-high's driver: after the warm-up, every 30 s it records the step that ends (02 §15.2: egress, CPU
// and loss at each step), stops at the knee, and else raises the next 10 thumbnails to the full layer.
func (lt *loadTest) stepUp(ctx context.Context, rep *report) {
	m := lt.meter
	if sleep(ctx, time.Until(m.warmEnd)) != nil {
		return
	}
	type thumb struct{ sub, pub int }
	var low []thumb
	for j, c := range lt.subs {
		c.mu.Lock()
		focus := c.focus
		c.mu.Unlock()
		for p := range lt.pubs {
			if p != lt.self(j) && p != focus {
				low = append(low, thumb{j, p})
			}
		}
	}
	rand.Shuffle(len(low), func(a, b int) { low[a], low[b] = low[b], low[a] }) //nolint:gosec // G404: an order, not a secret
	from := time.Now()
	marks := m.recorderStats()
	for {
		if sleep(ctx, time.Until(from.Add(lt.cfg.stepEvery))) != nil {
			return
		}
		to := time.Now()
		st := m.step(from, to, marks)
		rep.Steps = append(rep.Steps, st)
		lt.log.Info("all-high step", "high", st.High, "egress_mbps", st.EgressMbps, "cpu_pct", st.CPUAvg, "loss_pct", st.LossFinal)
		if st.LossFinal > kneeLossPct || st.CPUAvg > kneeCPUPct {
			knee := st
			rep.Knee = &knee
			return
		}
		if len(low) == 0 {
			return
		}
		raise := low[:min(lt.cfg.stepSize, len(low))]
		low = low[len(raise):]
		bySub := map[int][]protocol.SubscriptionWant{}
		for _, t := range raise {
			bySub[t.sub] = append(bySub[t.sub],
				protocol.SubscriptionWant{ShareID: lt.shareID(t.pub), Video: protocol.VideoLayerHigh, Audio: protocol.AudioStateOff})
		}
		for j, wants := range bySub {
			if err := lt.subs[j].subscribe(ctx, wants...); err != nil && ctx.Err() == nil {
				lt.subs[j].problem("raising thumbnails to the full layer: %v", err)
			}
		}
		from, marks = time.Now(), m.recorderStats()
	}
}

// teardownTimeout bounds the teardown, which runs after an interrupt too.
const teardownTimeout = 60 * time.Second

// teardown closes every client, deletes the run's accounts (unless -keep-users), revokes the invite and logs the
// admin out. What fails here is a failure of the run: accounts may be left over.
func (lt *loadTest) teardown(rep *report) {
	ctx, cancel := context.WithTimeout(context.Background(), teardownTimeout)
	defer cancel()
	defer lt.transport.CloseIdleConnections()
	lt.stopMedia()
	clients := append(slices.Clone(lt.pubs), lt.subs...)
	_ = parallel(ctx, len(clients), func(i int) error {
		clients[i].close()
		return nil
	})
	if len(clients) > 0 {
		lt.log.Info("clients closed", "count", len(clients))
	}
	rep.Users.Kept = lt.cfg.keepUsers
	if !lt.cfg.keepUsers {
		for _, a := range lt.accounts {
			if err := lt.deleteUser(ctx, a); err != nil {
				rep.Failures = append(rep.Failures, fmt.Sprintf("teardown: deleting %s: %v", a.name, err))
				continue
			}
			rep.Users.Deleted++
		}
		if len(lt.accounts) > 0 {
			lt.log.Info("accounts deleted", "count", rep.Users.Deleted, "of", len(lt.accounts))
		}
	}
	if lt.inviteID != "" {
		if err := lt.revokeInvite(ctx); err != nil {
			rep.Failures = append(rep.Failures, fmt.Sprintf("teardown: revoking the invite: %v", err))
		}
	}
	if lt.adminID != "" {
		if err := lt.logout(ctx); err != nil {
			lt.log.Warn("logging the admin out", "err", err)
		}
	}
}

// parallelLimit bounds the clients that connect, publish or close at once.
const parallelLimit = 16

// parallel runs fn(0) … fn(n−1), at most parallelLimit at a time, and returns the first error. Once one has failed,
// or ctx has ended, the calls that have not started are skipped.
func parallel(ctx context.Context, n int, fn func(i int) error) error {
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
	)
	slots := make(chan struct{}, parallelLimit)
	for i := range n {
		mu.Lock()
		failed := first != nil
		mu.Unlock()
		if failed || ctx.Err() != nil {
			break
		}
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			if err := fn(i); err != nil {
				mu.Lock()
				if first == nil {
					first = err
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if first == nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return first
}
