package ops

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"runtime"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"golang.org/x/mod/semver"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/netx"
	"github.com/MoonWX/isshoni/internal/version"
)

// The admin dashboard (04 §11.4; README S85): GET /api/v1/admin/dashboard, the one document that 05's admin page
// polls every 2 s while it is visible and 02's load test every 5 s. Dashboard.Snapshot is its typed function; the
// wiring registers it for admins through 03's API.Handle (04 §6.6):
//
//	d := ops.NewDashboard(ops.DashboardOptions{
//		Status: status, Live: live, Accounts: accounts, // the admin socket's GET /v1/status; hub + SFU; 03's API
//		Transfer: transfer, Release: release, Doctor: doc, // the other ops parts
//		PublicAddrs: …, MinClientVersion: …,
//	})
//	api.Handle("GET /api/v1/admin/dashboard", httpapi.Admin, …) // d.Snapshot(r.Context())
//
// Every part comes from a source: the server's status, the live rooms and the media totals, the accounts, the
// transfer, the release check, doctor and the process. A source that fails (an error, a panic, or no answer in
// its time) leaves its section empty (an empty list, zero totals, or an absent object) and adds the alert
// dashboard.source_failed with params.source naming it; the response never fails for it. The only error of
// Snapshot is the caller's context: a client that went away gets no half-built document.

// TransferSource is the transfer part of the dashboard: *Transfer (04 §11.3).
type TransferSource interface {
	Info() api.TransferInfo
	Alerts() []api.Alert
}

// UpdateSource is the release part of the dashboard: *ReleaseCheck (04 §11.5).
type UpdateSource interface {
	Update() *api.UpdateInfo
	Alerts() []api.Alert
}

// DoctorSource is the doctor part of the dashboard: *Doctor (04 §13.1).
type DoctorSource interface {
	Summary() *api.DoctorSummary
	Alerts() []api.Alert
}

var (
	_ TransferSource = (*Transfer)(nil)
	_ UpdateSource   = (*ReleaseCheck)(nil)
	_ DoctorSource   = (*Doctor)(nil)
)

// The sources of the dashboard: the values of the params.source of dashboard.source_failed. They are part of the
// wire contract (05 renders them): never renamed.
const (
	DashboardSourceServer   = "server"   // DashboardOptions.Status: the server object but its process and update
	DashboardSourceProcess  = "process"  // the process's CPU, memory and goroutines
	DashboardSourceRooms    = "rooms"    // LiveSource.Rooms: rooms, and the clients counted from them
	DashboardSourceMedia    = "media"    // LiveSource.Media
	DashboardSourceTransfer = "transfer" // TransferSource
	DashboardSourceUpdate   = "update"   // UpdateSource: server.update and the release alerts
	DashboardSourceDoctor   = "doctor"   // DoctorSource
	DashboardSourceAccounts = "accounts" // AccountsSource
)

const (
	// dashboardCacheFor is how long a snapshot is served again (04 §11.4): however many admin pages poll, the
	// sources are asked at most once a second.
	dashboardCacheFor = time.Second
	// dashboardSourceTimeout bounds each source: the accounts' store read (04 §11.4: 1 s), and the others, which
	// read memory and never wait.
	dashboardSourceTimeout = time.Second
)

// DashboardOptions configures NewDashboard. Only Status is required; a source that is left out has its section
// empty, without an alert: the server has no such part (a server without media has no live rooms).
type DashboardOptions struct {
	// Status is what the admin socket's GET /v1/status answers (04 §12.2): the version, the start, the origin, the
	// certificate, the public addresses, the NAT and the advertised media addresses of the server object. It is
	// asked once per snapshot. Required.
	Status func(ctx context.Context) (api.ServerStatus, error)
	// Live gives the rooms with their participants, connections and shares, and the media totals: the wiring's
	// adapter over 01's Hub.Snapshot and 02's SFU.Snapshot.
	Live LiveSource
	// Accounts gives the accounts part: 03's API.DashboardAccounts, under a timeout of 1 s.
	Accounts AccountsSource
	// Transfer gives the month's transfer and the transfer alerts.
	Transfer TransferSource
	// Release gives server.update and the release alerts.
	Release UpdateSource
	// Doctor gives the summary of the last doctor report and the doctor.fail alert.
	Doctor DoctorSource
	// PublicAddrs returns the public addresses as the server's last periodic look found them (04 §7.4), and false
	// while they are the ones it started with. An address that differs from the status's is the alert
	// public_ip.changed. nil: no such alert.
	PublicAddrs func() (netx.PublicAddrs, bool)
	// MinClientVersion is the setting minClientVersion (03 §9), read at every snapshot: a client below it is
	// outdated. nil or "": no minimum.
	MinClientVersion func() string
	// Version is the running version without a leading v, for the clients' outdated flag. "" means
	// version.Version().
	Version string
	// Process reads the process's numbers. nil means this process: CPU seconds and resident memory from
	// Prometheus's process collector (0 where the platform has none), the CPUs and goroutines from the runtime.
	Process func() api.ProcessInfo
	// Now is the clock of generatedAt and of the cache, and the one the TLS alert compares a certificate's expiry
	// with. nil means time.Now.
	Now func() time.Time
	// Logger is the server's logger; the dashboard adds component=ops. nil means slog.Default().
	Logger *slog.Logger
}

// Dashboard builds the admin dashboard. NewDashboard builds it; Snapshot may be called from any goroutine.
type Dashboard struct {
	status           func(context.Context) (api.ServerStatus, error)
	live             LiveSource
	accounts         AccountsSource
	transfer         TransferSource
	release          UpdateSource
	doctor           DoctorSource
	publicAddrs      func() (netx.PublicAddrs, bool)
	minClientVersion func() string
	version          string
	process          func() api.ProcessInfo
	now              func() time.Time
	log              *slog.Logger

	// mu makes snapshots take turns: a caller that comes while one is being built waits for it and gets it from
	// the cache. It guards the fields below.
	mu       sync.Mutex
	cached   *api.OpsDashboard
	cachedAt time.Time
	failing  map[string]bool // the sources that failed in the last snapshot: their failure was logged once
}

// NewDashboard builds the dashboard. It starts nothing and asks no source. A nil Status is a wiring bug and
// panics.
func NewDashboard(o DashboardOptions) *Dashboard {
	if o.Status == nil {
		panic("ops: NewDashboard: nil Status")
	}
	d := &Dashboard{
		status:           o.Status,
		live:             o.Live,
		accounts:         o.Accounts,
		transfer:         o.Transfer,
		release:          o.Release,
		doctor:           o.Doctor,
		publicAddrs:      o.PublicAddrs,
		minClientVersion: o.MinClientVersion,
		version:          cmp.Or(o.Version, version.Version()),
		process:          o.Process,
		now:              o.Now,
		failing:          map[string]bool{},
	}
	if d.process == nil {
		d.process = newProcessReader().read
	}
	if d.now == nil {
		d.now = time.Now
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	d.log = log.With(slog.String("component", "ops"))
	return d
}

// Snapshot returns the dashboard (04 §11.4): the typed function of GET /api/v1/admin/dashboard. A snapshot is
// served again for 1 s, so the sources are asked at most once a second; callers share it and must not change it.
// A failing source never fails the snapshot: its section is empty and an alert dashboard.source_failed names it.
// The error is ctx's when the caller went away before the snapshot was done.
func (d *Dashboard) Snapshot(ctx context.Context) (api.OpsDashboard, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return api.OpsDashboard{}, err
	}
	now := d.now()
	if d.cached != nil && !now.Before(d.cachedAt) && now.Sub(d.cachedAt) < dashboardCacheFor {
		return *d.cached, nil
	}
	snap := d.build(ctx, now)
	if err := ctx.Err(); err != nil {
		return api.OpsDashboard{}, err // its sources were cut off with it: nobody else gets this one either
	}
	d.cached, d.cachedAt = &snap, now
	return snap, nil
}

// build asks every source once; the caller holds mu. A source's values go into the document only when it did
// not fail: a section is whole or empty.
func (d *Dashboard) build(ctx context.Context, now time.Time) api.OpsDashboard {
	out := api.OpsDashboard{
		GeneratedAt: now.UTC(),
		Server:      api.ServerInfo{Version: d.version},
		Rooms:       []api.RoomLive{},
	}
	var failed []string
	ask := func(source string, fn func(context.Context) error) bool {
		if err := callSource(ctx, source, fn); err != nil {
			failed = append(failed, source)
			d.noteFailure(ctx, source, err)
			return false
		}
		delete(d.failing, source)
		return true
	}

	var (
		status                                     api.ServerStatus
		transferAlerts, updateAlerts, doctorAlerts []api.Alert
	)
	statusOK := ask(DashboardSourceServer, func(ctx context.Context) error {
		var err error
		status, err = d.status(ctx)
		return err
	})
	if statusOK {
		out.Server = api.ServerInfo{
			Version:    status.Version,
			StartedAt:  status.StartedAt,
			Origin:     status.Origin,
			TLS:        status.TLS,
			PublicIPv4: status.PublicIPv4,
			PublicIPv6: status.PublicIPv6,
			NAT:        status.NAT,
			Advertised: status.Advertised,
		}
	}
	var process api.ProcessInfo
	if ask(DashboardSourceProcess, func(context.Context) error { process = d.process(); return nil }) {
		out.Server.Process = process
	}
	if d.release != nil {
		var update *api.UpdateInfo
		var alerts []api.Alert
		if ask(DashboardSourceUpdate, func(context.Context) error {
			update, alerts = d.release.Update(), d.release.Alerts()
			return nil
		}) {
			out.Server.Update, updateAlerts = update, alerts
		}
	}
	if d.transfer != nil {
		var info api.TransferInfo
		var alerts []api.Alert
		if ask(DashboardSourceTransfer, func(context.Context) error {
			info, alerts = d.transfer.Info(), d.transfer.Alerts()
			return nil
		}) {
			out.Transfer, transferAlerts = info, alerts
		}
	}
	if d.live != nil {
		var rooms []api.RoomLive
		if ask(DashboardSourceRooms, func(ctx context.Context) error { rooms = d.live.Rooms(ctx); return nil }) && rooms != nil {
			out.Rooms = rooms
		}
		var media api.MediaTotals
		if ask(DashboardSourceMedia, func(ctx context.Context) error { media = d.live.Media(ctx); return nil }) {
			out.Media = media
		}
	}
	out.Clients = d.clientCounts(out.Rooms)
	if d.doctor != nil {
		var summary *api.DoctorSummary
		var alerts []api.Alert
		if ask(DashboardSourceDoctor, func(context.Context) error {
			summary, alerts = d.doctor.Summary(), d.doctor.Alerts()
			return nil
		}) {
			out.Doctor, doctorAlerts = summary, alerts
		}
	}
	if d.accounts != nil {
		var acc api.DashboardAccounts
		if ask(DashboardSourceAccounts, func(ctx context.Context) error {
			var err error
			acc, err = d.accounts.Accounts(ctx)
			return err
		}) {
			out.Accounts = &acc
		}
	}

	// The alerts in a fixed order: what is missing from the document first, then the server's own state.
	out.Alerts = make([]api.Alert, 0, len(failed))
	for _, source := range failed {
		out.Alerts = append(out.Alerts, api.Alert{
			Code:     api.AlertCodeDashboardSourceFailed,
			Severity: api.AlertSeverityWarn,
			Params:   map[string]any{"source": source},
		})
	}
	out.Alerts = append(out.Alerts, transferAlerts...)
	out.Alerts = append(out.Alerts, updateAlerts...)
	if statusOK && d.publicAddrs != nil {
		if pub, changed := d.publicAddrs(); changed {
			out.Alerts = append(out.Alerts, publicIPAlerts(status, pub)...)
		}
	}
	if statusOK {
		out.Alerts = append(out.Alerts, tlsAlerts(status.TLS, now)...)
	}
	out.Alerts = append(out.Alerts, doctorAlerts...)
	return out
}

// callSource runs one source under its own timeout, and turns what makes it fail into an error: the error it
// returns, a panic (a bug, which costs the section and not the response), or a context that ended before it
// returned, which makes what it gave a part of the section at best.
func callSource(ctx context.Context, source string, fn func(context.Context) error) (err error) {
	ctx, cancel := context.WithTimeout(ctx, dashboardSourceTimeout)
	defer cancel()
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("ops: dashboard: the %s source panicked: %v", source, v)
		}
	}()
	if err := fn(ctx); err != nil {
		return fmt.Errorf("ops: dashboard: the %s source: %w", source, err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("ops: dashboard: the %s source did not answer in time: %w", source, err)
	}
	return nil
}

// noteFailure logs a failing source once per run of failures: a page that polls every 2 s would otherwise repeat
// the line. A failure that came with the caller's own context (the page was closed) is no failure of the source.
func (d *Dashboard) noteFailure(ctx context.Context, source string, err error) {
	if ctx.Err() != nil || d.failing[source] {
		return
	}
	d.failing[source] = true
	d.log.LogAttrs(ctx, slog.LevelWarn, "dashboard: a source failed; its section stays empty",
		slog.String("source", source), logx.Err(err))
}

// clientCounts counts the connections of the rooms by client kind and version (api.ClientVersionCount), sorted by
// kind and then version. A client is outdated when its version is below the setting minClientVersion or below
// this server's own: a web client that is older than the server it talks to is a tab that was not reloaded since
// an upgrade. A version that is not SemVer is never outdated, and neither is one when the server's is not SemVer.
func (d *Dashboard) clientCounts(rooms []api.RoomLive) []api.ClientVersionCount {
	type key struct{ kind, version string }
	counts := map[key]int{}
	for _, r := range rooms {
		for _, p := range r.Participants {
			for _, c := range p.Connections {
				counts[key{c.Kind, c.Version}]++
			}
		}
	}
	minimum := ""
	if d.minClientVersion != nil {
		minimum = d.minClientVersion()
	}
	out := make([]api.ClientVersionCount, 0, len(counts))
	for k, n := range counts {
		out = append(out, api.ClientVersionCount{
			Kind: k.kind, Version: k.version, Count: n, Outdated: below(k.version, minimum) || below(k.version, d.version),
		})
	}
	slices.SortFunc(out, func(a, b api.ClientVersionCount) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Version, b.Version))
	})
	return out
}

// below reports whether version v is older than version than; both are SemVer without the leading v, and either
// one that is not makes the answer false.
func below(v, than string) bool {
	a, b := "v"+v, "v"+than
	return semver.IsValid(a) && semver.IsValid(b) && semver.Compare(a, b) < 0
}

// publicIPAlerts is the alert public_ip.changed (04 §7.4, §11.4): one per address family whose public address,
// as the server's last look found it, is not the one the server started with and still uses in its candidates
// and its site. params are {family: "ipv4" | "ipv6", from, to}; from is left out when the server started without
// an address of that family.
func publicIPAlerts(status api.ServerStatus, now netx.PublicAddrs) []api.Alert {
	var out []api.Alert
	for _, f := range [...]struct {
		family, from string
		to           netip.Addr
	}{{"ipv4", status.PublicIPv4, now.V4}, {"ipv6", status.PublicIPv6, now.V6}} {
		if !f.to.IsValid() || f.to.String() == f.from {
			continue
		}
		params := map[string]any{"family": f.family, "to": f.to.String()}
		if f.from != "" {
			params["from"] = f.from
		}
		out = append(out, api.Alert{Code: api.AlertCodePublicIPChanged, Severity: api.AlertSeverityWarn, Params: params})
	}
	return out
}

// tlsAlerts is the alert tls.renewal_failing (04 §11.4): in the modes that renew by themselves (auto and ip), a
// server that has had a certificate and whose last attempt at a new one failed, the rule of doctor's tls check
// for its code tls.renewal_failing (04 §13.2). It is a warning while the certificate is still valid and served, and
// an error once it has expired or is no longer served. params are {code, notAfter}: the error code of 04 §8.7,
// for 05's text and fix, and the certificate's expiry. A first order that fails is no renewal: the TLS object
// shows it (ready false, lastErrorCode).
func tlsAlerts(t api.TLSInfo, now time.Time) []api.Alert {
	if t.LastErrorCode == "" || t.NotAfter.IsZero() || (t.Mode != api.TLSModeAuto && t.Mode != api.TLSModeIP) {
		return nil
	}
	severity := api.AlertSeverityWarn
	if !t.Ready || !now.Before(t.NotAfter) {
		severity = api.AlertSeverityError
	}
	return []api.Alert{{
		Code:     api.AlertCodeTLSRenewalFailing,
		Severity: severity,
		Params:   map[string]any{"code": t.LastErrorCode, "notAfter": api.WireTime(t.NotAfter).String()},
	}}
}

// processReader reads the process's numbers for the dashboard: CPU seconds and resident memory from a process
// collector of its own (Prometheus's, the one the metrics endpoint has too, so both agree), the CPUs and the
// goroutines from the runtime.
type processReader struct{ reg *prometheus.Registry }

func newProcessReader() processReader {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return processReader{reg: reg}
}

// read returns the numbers now. A platform where the collector has no series for one leaves it at 0.
func (p processReader) read() api.ProcessInfo {
	info := api.ProcessInfo{NumCPU: runtime.NumCPU(), Goroutines: runtime.NumGoroutine()}
	families, _ := p.reg.Gather() // an error comes with what could be read
	for _, f := range families {
		if len(f.GetMetric()) == 0 {
			continue
		}
		m := f.GetMetric()[0]
		switch f.GetName() {
		case "process_cpu_seconds_total":
			info.CPUSeconds = m.GetCounter().GetValue()
		case "process_resident_memory_bytes":
			info.RSSBytes = int64(m.GetGauge().GetValue())
		}
	}
	return info
}
