package ops

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/ops/doctor"
)

// Doctor inside the running server (04 §13.1): the checks of ops/doctor run in the server's own process, where they
// see its certificate, its sockets and its /proc. Three callers share one Doctor:
//
//   - the server itself, 20 s after it started and every 24 h (Run). The report is kept for the dashboard, in
//     memory and in 03's meta table, so a restarted server shows the last one right away;
//   - the admin page, on demand (Check, POST /api/v1/admin/doctor), at most once per 10 s, and Last for the report
//     that is there (GET);
//   - `isshoni doctor`, through the admin socket's POST /v1/doctor (RunFor), which may ask for some of the checks
//     and for the bandwidth estimate of another session.
//
// One run at a time, whoever asks: the page gets 429 doctor_busy while one is under way, the other two wait their
// turn. A run sends at most one request to the outside, the clock check's GET of the ACME directory (04 §16), so
// the spacing also bounds what a page that keeps asking can make the server send.
//
// The typed functions here return DTOs and *api.Error; the wiring wraps them into the REST routes (04 §2, §6.6):
//
//	doc := ops.NewDoctor(ops.DoctorOptions{Meta: meta, Logger: log, Env: func(ctx context.Context) doctor.Env {
//		status, _ := statusOf(ctx) // what GET /v1/status answers
//		return doctor.Env{
//			Config: cfg, Live: &status, UID: os.Getuid(), Host: deps.Host, Resolver: deps.Resolver, Now: deps.Now,
//			DB: doctor.DBFiles{LatestSchemaVersion: store.LatestSchemaVersion},
//		}
//	}})
//	admin := ops.NewAdminServer(ops.AdminOptions{…, Doctor: doc}) // POST /v1/doctor
//	go func() { _ = doc.Run(ctx) }()                               // 20 s after the start, then every 24 h
//	api.Handle("GET /api/v1/admin/doctor", httpapi.Admin, …)       // doc.Last(r.Context())
//	api.Handle("POST /api/v1/admin/doctor", httpapi.Admin, …)      // doc.Check(r.Context())
//	api.Handle("GET /api/v1/admin/bandwidth", httpapi.Admin, …)    // ops.BandwidthFromQuery(r.URL.Query())
//
// and the dashboard takes doc.Summary() and doc.Alerts().

const (
	// doctorFirstRun is how long after its start the server examines itself: the listeners are up and the first
	// certificate order has had time to succeed or to fail with a cause.
	doctorFirstRun = 20 * time.Second
	doctorEvery    = 24 * time.Hour
	// doctorMinGap is the least time between the end of one run and the start of a run on demand.
	doctorMinGap = 10 * time.Second
	// doctorRunTimeout bounds one run. Every probe has a timeout of its own; this one is for the sum.
	doctorRunTimeout = 2 * time.Minute
	// doctorStoreTimeout bounds one read or write of the meta key.
	doctorStoreTimeout = 5 * time.Second
	// doctorStateVersion is the version of the JSON in MetaDoctorLast.
	doctorStateVersion = 1
)

// DoctorOptions configures NewDoctor. Only Env is required.
type DoctorOptions struct {
	// Env returns what one run sees (doctor.Env): the wiring fills in the config, the server's status as the admin
	// socket's GET /v1/status reports it (Env.Live, which makes it a run inside the server), 03's
	// LatestSchemaVersion, the uid and the server's test seams. It is called once per run. Required.
	Env func(ctx context.Context) doctor.Env
	// Meta keeps the last report across restarts (MetaDoctorLast). nil keeps it in memory only.
	Meta MetaStore
	// Now is the clock for the spacing of runs. nil means time.Now.
	Now func() time.Time
	// Logger is the server's logger; Doctor adds component=ops. nil means slog.Default().
	Logger *slog.Logger
}

// doctorState is the value of MetaDoctorLast.
type doctorState struct {
	V      int              `json:"v"`
	Report api.DoctorReport `json:"report"`
}

// Doctor runs the server's diagnostics. NewDoctor builds it, Run drives the periodic runs until the server stops;
// the other methods may be called at any time and from any goroutine.
type Doctor struct {
	env  func(ctx context.Context) doctor.Env
	meta MetaStore
	now  func() time.Time
	log  *slog.Logger

	started atomic.Bool
	// turn holds the one run that is under way: a run puts a token in and takes it out when it is done.
	turn chan struct{}

	mu      sync.Mutex
	last    *api.DoctorReport // the last full report, of this process or loaded from the meta table
	lastEnd time.Time         // when the last run of this process ended
}

// NewDoctor builds the Doctor. It starts nothing and checks nothing. A nil Env is a wiring bug and panics.
func NewDoctor(o DoctorOptions) *Doctor {
	if o.Env == nil {
		panic("ops: NewDoctor: nil Env")
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Doctor{
		env:  o.Env,
		meta: o.Meta,
		now:  now,
		log:  log.With(slog.String("component", "ops")),
		turn: make(chan struct{}, 1),
	}
}

// Run loads the last report from the meta table, so the dashboard has it right after a restart, then runs doctor
// 20 s after the start and every 24 h from then on, until ctx ends; it then returns nil. Run can be called once.
func (d *Doctor) Run(ctx context.Context) error {
	if !d.started.CompareAndSwap(false, true) {
		return errors.New("ops: Doctor.Run called more than once")
	}
	d.load(ctx)

	timer := time.NewTimer(doctorFirstRun)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			if err := d.acquire(ctx); err != nil {
				return nil // the server stops
			}
			d.run(ctx, AdminDoctorRequest{}, true)
			d.release()
			timer.Reset(doctorEvery)
		}
	}
}

// acquire waits for this caller's turn.
func (d *Doctor) acquire(ctx context.Context) error {
	select {
	case d.turn <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *Doctor) release() { <-d.turn }

// run runs the checks once; the caller holds the turn. A full run with the default session (keep) becomes the
// last report: it is what the dashboard summarizes, and a run of three checks must not replace it.
func (d *Doctor) run(ctx context.Context, req AdminDoctorRequest, keep bool) api.DoctorReport {
	rctx, cancel := context.WithTimeout(ctx, doctorRunTimeout)
	defer cancel()
	var in api.BandwidthInput
	if req.Bandwidth != nil {
		in = *req.Bandwidth
	}
	rep := doctor.Run(rctx, d.env(rctx), in, req.Only)

	d.mu.Lock()
	d.lastEnd = d.now()
	if keep && ctx.Err() == nil {
		kept := rep
		d.last = &kept
	}
	d.mu.Unlock()
	if keep && ctx.Err() == nil {
		d.save(ctx, rep)
		d.announce(ctx, rep)
	}
	return rep
}

// Check runs every check now and returns the report: the typed function of POST /api/v1/admin/doctor. While
// another run is under way, and for 10 s after the last one, it returns 429 doctor_busy with RetryAfter instead:
// the page shows the report it has and tries again.
func (d *Doctor) Check(ctx context.Context) (api.DoctorReport, error) {
	select {
	case d.turn <- struct{}{}:
	default:
		return api.DoctorReport{}, doctorBusy(doctorMinGap)
	}
	defer d.release()
	d.mu.Lock()
	wait := time.Duration(0)
	if !d.lastEnd.IsZero() {
		wait = doctorMinGap - d.now().Sub(d.lastEnd)
	}
	d.mu.Unlock()
	if wait > 0 {
		return api.DoctorReport{}, doctorBusy(wait)
	}
	rep := d.run(ctx, AdminDoctorRequest{}, true)
	if err := ctx.Err(); err != nil {
		return api.DoctorReport{}, err // the client went away; a cut-off report is nobody's
	}
	return rep, nil
}

// doctorBusy is 429 doctor_busy with the seconds to wait, rounded up.
func doctorBusy(wait time.Duration) error {
	return &api.Error{Code: api.CodeDoctorBusy, RetryAfter: max(int(math.Ceil(wait.Seconds())), 1)}
}

// Last returns the last full report: the typed function of GET /api/v1/admin/doctor. Before the first run of a
// server that has never run doctor (the first 20 s of a new installation) there is none: 404 not_found.
func (d *Doctor) Last(context.Context) (api.DoctorReport, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.last == nil {
		return api.DoctorReport{}, api.NewError(api.CodeNotFound)
	}
	return *d.last, nil
}

// RunFor runs doctor for the admin socket's POST /v1/doctor (04 §12.2): the checks named in req, or all of them,
// with the bandwidth estimate of req's session. It waits for its turn instead of answering "busy": the operator at
// a terminal has no page that retries. The report says "cli-with-server". The error is ctx's when the caller goes
// away first.
func (d *Doctor) RunFor(ctx context.Context, req AdminDoctorRequest) (api.DoctorReport, error) {
	if err := d.acquire(ctx); err != nil {
		return api.DoctorReport{}, err
	}
	defer d.release()
	// A run of every check with the default session is the same one the server makes for itself, so it is kept.
	rep := d.run(ctx, req, len(req.Only) == 0 && req.Bandwidth == nil)
	if err := ctx.Err(); err != nil {
		return api.DoctorReport{}, err
	}
	rep.Mode = api.DoctorModeCLIWithServer
	return rep, nil
}

// Summary returns the counts of the last full report for the dashboard (api.OpsDashboard.Doctor), or nil before
// the first one.
func (d *Doctor) Summary() *api.DoctorSummary {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.last == nil {
		return nil
	}
	return &api.DoctorSummary{RanAt: d.last.RanAt, OK: d.last.Summary.OK, Warn: d.last.Summary.Warn, Fail: d.last.Summary.Fail}
}

// Alerts returns the dashboard alert doctor.fail while the last full report has failures (04 §11.4). Params
// carries their number and the ids of the failing checks.
func (d *Doctor) Alerts() []api.Alert {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.last == nil || d.last.Summary.Fail == 0 {
		return nil
	}
	return []api.Alert{{
		Code:     api.AlertCodeDoctorFail,
		Severity: api.AlertSeverityError,
		Params:   map[string]any{"fail": d.last.Summary.Fail, "checks": checkIDs(*d.last, api.DoctorStatusFail)},
	}}
}

// checkIDs returns the ids of the checks of rep with this status.
func checkIDs(rep api.DoctorReport, status api.DoctorStatus) []string {
	ids := []string{}
	for _, c := range rep.Checks {
		if c.Status == status {
			ids = append(ids, c.ID)
		}
	}
	return ids
}

// announce writes one log line per kept run: the counts, and at warn level the checks that failed, so that an
// operator who reads the journal and never opens the dashboard sees them too.
func (d *Doctor) announce(ctx context.Context, rep api.DoctorReport) {
	attrs := []slog.Attr{
		slog.Int("ok", rep.Summary.OK), slog.Int("warn", rep.Summary.Warn), slog.Int("fail", rep.Summary.Fail),
	}
	if rep.Summary.Fail == 0 {
		d.log.LogAttrs(ctx, slog.LevelInfo, "doctor ran", attrs...)
		return
	}
	attrs = append(attrs, slog.Any("failed", checkIDs(rep, api.DoctorStatusFail)))
	d.log.LogAttrs(ctx, slog.LevelWarn, "doctor found problems; `isshoni doctor` shows them with their fixes", attrs...)
}

// load reads the last report from the meta table. A value that can't be used is as good as none: the first run
// replaces it.
func (d *Doctor) load(ctx context.Context) {
	if d.meta == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, doctorStoreTimeout)
	defer cancel()
	value, err := d.meta.Meta(ctx, MetaDoctorLast)
	if err != nil {
		if ctx.Err() == nil {
			d.log.LogAttrs(ctx, slog.LevelWarn, "reading the last doctor report failed", logx.Err(err))
		}
		return
	}
	var state doctorState
	if value == "" || json.Unmarshal([]byte(value), &state) != nil || state.V != doctorStateVersion ||
		state.Report.Schema != api.DoctorReportSchema || state.Report.RanAt.IsZero() {
		return
	}
	d.mu.Lock()
	if d.last == nil { // a run that was faster keeps its report
		d.last = &state.Report
	}
	d.mu.Unlock()
}

// save writes the report to the meta table. A failed write costs only the report after the next restart.
func (d *Doctor) save(ctx context.Context, rep api.DoctorReport) {
	if d.meta == nil {
		return
	}
	data, err := json.Marshal(doctorState{V: doctorStateVersion, Report: rep})
	if err != nil {
		d.log.LogAttrs(ctx, slog.LevelWarn, "encoding the doctor report failed", logx.Err(err))
		return
	}
	// The write must not die with a caller that hung up after its run was done.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), doctorStoreTimeout)
	defer cancel()
	if err := d.meta.SetMeta(sctx, MetaDoctorLast, string(data)); err != nil {
		d.log.LogAttrs(ctx, slog.LevelWarn, "keeping the doctor report failed", logx.Err(err))
	}
}

// Bandwidth is the calculator of 04 §13.4 as a typed function: the estimate for a session, or 400 bad_request
// with params.field naming the value it does not take (doctor.BandwidthInputProblem has the ranges). The CLI's
// bandwidth flags, the doctor report and the admin page all end in doctor.Bandwidth, so there is one
// implementation.
func Bandwidth(in api.BandwidthInput) (api.BandwidthEstimate, error) {
	if field, _, ok := doctor.BandwidthInputProblem(in); !ok {
		return api.BandwidthEstimate{}, badParam(field)
	}
	return doctor.Bandwidth(in), nil
}

// BandwidthFromQuery is the typed function of GET /api/v1/admin/bandwidth (04 §13.4):
//
//	?people=5&sharing=2&thumbnails=8&quality=1080p60&preset=auto&hours=2
//
// A parameter that is left out has the value of doctor.DefaultBandwidthInput; one that is not a number, or out of
// range, is 400 bad_request with params.field. Other parameters are ignored, like unknown fields everywhere in the
// API (03 §12.1).
func BandwidthFromQuery(q url.Values) (api.BandwidthEstimate, error) {
	in := doctor.DefaultBandwidthInput()
	ints := []struct {
		name string
		dst  *int
	}{{"people", &in.People}, {"sharing", &in.Sharing}, {"thumbnails", &in.Thumbnails}}
	for _, f := range ints {
		if !q.Has(f.name) {
			continue
		}
		n, err := strconv.Atoi(q.Get(f.name))
		if err != nil {
			return api.BandwidthEstimate{}, badParam(f.name)
		}
		*f.dst = n
	}
	if q.Has("hours") {
		h, err := strconv.ParseFloat(q.Get("hours"), 64)
		if err != nil {
			return api.BandwidthEstimate{}, badParam("hours")
		}
		in.Hours = h
	}
	if q.Has("quality") {
		in.Quality = api.BandwidthQuality(q.Get("quality"))
	}
	if q.Has("preset") {
		in.Preset = api.BandwidthPreset(q.Get("preset"))
	}
	return Bandwidth(in)
}

// badParam is 400 bad_request for a query parameter or a request field, named in params.field.
func badParam(field string) error {
	return &api.Error{Code: api.CodeBadRequest, Params: map[string]any{api.ParamField: field}}
}
