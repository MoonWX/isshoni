package ops

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/netx"
	"github.com/MoonWX/isshoni/internal/server/ops/doctor"
)

// The Doctor of these tests runs the real checks of ops/doctor against a faked machine: a healthy server in ip
// mode at Hetzner. Nothing here asks the network: the resolver, the STUN client and the ACME directory are fakes,
// and a running server makes doctor ask none of the first two anyway.

var doctorNow = time.Date(2026, 9, 29, 20, 15, 0, 0, time.UTC)

// doctorWorld is the faked machine. Its fields may change between runs; every access is under mu.
type doctorWorld struct {
	t       *testing.T
	cfg     *config.Config
	dataDir string

	mu       sync.Mutex
	rmemMax  string
	certGone bool                  // the server has lost its certificate and its public address
	runs     int                   // Env calls: one per run
	block    chan struct{}         // when set, a run waits here (inside the clock check) until it is closed
	entered  chan struct{}         // receives one value per run that reached the block
	now      func() time.Time      // the clock of the checks; nil = doctorNow
	requests atomic.Int64          // GETs of the ACME directory
	onEnv    func(context.Context) // called at the start of every Env call
}

func newDoctorWorld(t *testing.T) *doctorWorld {
	t.Helper()
	w := &doctorWorld{t: t, dataDir: filepath.Join(t.TempDir(), "isshoni"), rmemMax: "8388608"}
	if err := os.Mkdir(w.dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := config.OpenSecrets(filepath.Join(w.dataDir, "secrets.json"), nil); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFlags(flag.NewFlagSet("test", flag.ContinueOnError), []string{"--tls.mode", "ip", "--data-dir", w.dataDir})
	if err != nil {
		t.Fatal(err)
	}
	w.cfg = cfg
	return w
}

func (w *doctorWorld) set(fn func(*doctorWorld)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	fn(w)
}

func (w *doctorWorld) runCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.runs
}

type staticIfaces struct{}

func (staticIfaces) Interfaces() ([]netx.Interface, error) {
	return []netx.Interface{{Name: "eth0", Flags: net.FlagUp, Addrs: []netx.InterfaceAddr{{Addr: netip.MustParseAddr("203.0.113.7")}}}}, nil
}

func (staticIfaces) RouteSource(context.Context, netip.Addr) (netip.Addr, error) {
	return netip.MustParseAddr("203.0.113.7"), nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// env is DoctorOptions.Env: the machine as it is at the moment of the run.
func (w *doctorWorld) env(ctx context.Context) doctor.Env {
	w.mu.Lock()
	w.runs++
	rmem, gone, block, entered, now, onEnv := w.rmemMax, w.certGone, w.block, w.entered, w.now, w.onEnv
	w.mu.Unlock()
	if onEnv != nil {
		onEnv(ctx)
	}
	if now == nil {
		now = func() time.Time { return doctorNow }
	}
	live := &api.ServerStatus{
		Version: "0.3.0", StartedAt: doctorNow.Add(-time.Hour), UptimeS: 3600, Origin: "https://203.0.113.7",
		TLS: api.TLSInfo{
			Mode: api.TLSModeIP, Names: []string{"203.0.113.7"}, Ready: true,
			NotAfter: doctorNow.Add(6 * 24 * time.Hour), NextRenewal: doctorNow.Add(3 * 24 * time.Hour),
		},
		PublicIPv4: "203.0.113.7", PublicIPv4Method: "interface", PublicIPv6: "2001:db8::7", PublicIPv6Method: "interface",
		LocalIPv4: "203.0.113.7", NAT: api.NATKindNone,
		Advertised: []api.AdvertisedAddr{
			{Proto: "udp", Addr: "203.0.113.7:7882", Via: api.TransportUDP},
			{Proto: "tcp", Addr: "203.0.113.7:7882", Via: api.TransportTCP7882},
		},
		Listeners: []api.ListenerInfo{
			{Key: "listen.https", Network: "tcp", Addr: "[::]:443"},
			{Key: "listen.http", Network: "tcp", Addr: "[::]:80"},
		},
		UDPRcvBufBytes: 8388608, UDPSndBufBytes: 8388608, SchemaVersion: 7,
		Transfer: &api.TransferInfo{Month: "2026-09", EgressBytes: 12_300_000_000},
	}
	if gone {
		live.PublicIPv4, live.PublicIPv6, live.LocalIPv4, live.NAT = "", "", "", api.NATKindUnknown
		live.TLS = api.TLSInfo{Mode: api.TLSModeIP, Names: []string{}}
	}
	return doctor.Env{
		Config: w.cfg,
		Live:   live,
		FS: fstest.MapFS{
			"proc/sys/kernel/osrelease":   {Data: []byte("6.8.0-45-generic\n")},
			"proc/sys/net/core/rmem_max":  {Data: []byte(rmem + "\n")},
			"proc/sys/net/core/wmem_max":  {Data: []byte("8388608\n")},
			"run/systemd/system/.keep":    {},
			"sys/class/dmi/id/sys_vendor": {Data: []byte("Hetzner\n")},
		},
		Resolver: failingResolver{w.t},
		STUN:     failingSTUN{w.t},
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			w.requests.Add(1)
			if block != nil {
				entered <- struct{}{}
				select {
				case <-block:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
			}
			h := http.Header{}
			h.Set("Date", now().UTC().Format(http.TimeFormat))
			return &http.Response{StatusCode: http.StatusOK, Header: h, Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
		})},
		Adjtimex:   func() (bool, error) { return false, nil },
		DB:         doctor.DBFiles{LatestSchemaVersion: func() int { return 7 }},
		Now:        now,
		GOOS:       "linux",
		UID:        os.Getuid(),
		User:       "isshoni",
		Host:       config.Host{Environ: []string{}, Root: w.dataDir},
		Interfaces: staticIfaces{},
		Bind:       func(context.Context, string, string) error { return errors.New("a running server binds nothing") },
		CAA:        func(context.Context, string) ([]doctor.CAARecord, error) { return nil, nil },
		DiskFree:   func(string) (uint64, error) { return 37_200_000_000, nil },
		NoFile:     func() (uint64, error) { return 65536, nil },
	}
}

// failingResolver and failingSTUN fail a test that asks them: a run inside the server asks neither.
type failingResolver struct{ t *testing.T }

func (f failingResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	f.t.Errorf("doctor asked the resolver for %q", host)
	return nil, errors.New("no network in tests")
}

type failingSTUN struct{ t *testing.T }

func (f failingSTUN) Mapped(_ context.Context, _ net.PacketConn, server string) (netip.AddrPort, error) {
	f.t.Errorf("doctor asked the STUN server %q", server)
	return netip.AddrPort{}, errors.New("no network in tests")
}

func newTestDoctor(t *testing.T, w *doctorWorld, meta MetaStore, now func() time.Time) (*Doctor, *syncBuffer) {
	t.Helper()
	log, logs := testLogger()
	return NewDoctor(DoctorOptions{Env: w.env, Meta: meta, Now: now, Logger: log}), logs
}

// jsonKeys returns the top-level keys of a JSON object, in the order they were written.
func jsonKeys(t *testing.T, data []byte) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(string(data)))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not a JSON object: %s", data)
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

// The typed function of POST /api/v1/admin/doctor returns the JSON of 04 §13.5: the report of a run inside the
// server, with every check, the summary and the bandwidth estimate.
func TestDoctorCheckReport(t *testing.T) {
	w := newDoctorWorld(t)
	w.set(func(w *doctorWorld) { w.rmemMax = "212992" })
	d, _ := newTestDoctor(t, w, nil, nil)

	rep, err := d.Check(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Schema != 1 || rep.Mode != api.DoctorModeServer || rep.Version != "0.3.0" || !rep.RanAt.Equal(doctorNow) {
		t.Errorf("header: schema %d, mode %s, version %s, ranAt %s", rep.Schema, rep.Mode, rep.Version, rep.RanAt)
	}
	if want := (api.DoctorCounts{OK: 12, Warn: 1, Info: 4}); rep.Summary != want {
		t.Errorf("summary %+v, want %+v", rep.Summary, want)
	}
	var ids []string
	for _, c := range rep.Checks {
		ids = append(ids, c.ID)
	}
	if !slices.Equal(ids, doctor.CheckIDs()) {
		t.Errorf("checks %v", ids)
	}
	if w.requests.Load() != 1 {
		t.Errorf("%d requests to the ACME directory, want 1", w.requests.Load())
	}

	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := jsonKeys(t, data), []string{"schema", "version", "mode", "summary", "env", "checks", "bandwidth", "ranAt"}; !slices.Equal(got, want) {
		t.Errorf("JSON keys %v, want %v", got, want)
	}
	var doc struct {
		RanAt   string `json:"ranAt"`
		Summary map[string]int
		Env     map[string]any
		Checks  []map[string]any
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.RanAt != "2026-09-29T20:15:00.000Z" {
		t.Errorf("ranAt %q, want the wire form", doc.RanAt)
	}
	if len(doc.Summary) != 5 || doc.Summary["ok"] != 12 || doc.Summary["info"] != 4 {
		t.Errorf("summary %v", doc.Summary)
	}
	wantEnv := map[string]any{"os": "linux", "kernel": "6.8.0-45-generic", "container": "none", "systemd": true, "provider": "hetzner", "user": "isshoni"}
	for k, v := range wantEnv {
		if doc.Env[k] != v {
			t.Errorf("env.%s = %v, want %v", k, doc.Env[k], v)
		}
	}
	if _, ok := doc.Env["arch"]; !ok || len(doc.Env) != 7 {
		t.Errorf("env %v", doc.Env)
	}
	// The check of 04 §13.5's example, field by field.
	var udp map[string]any
	for _, c := range doc.Checks {
		for _, key := range []string{"id", "status", "code", "message", "localOnly", "durationMs"} {
			if _, ok := c[key]; !ok {
				t.Errorf("check %v has no %s", c["id"], key)
			}
		}
		if c["id"] == "udp_buffers" {
			udp = c
		}
	}
	const fix = `printf 'net.core.rmem_max=8388608\nnet.core.wmem_max=8388608\n' | sudo tee /etc/sysctl.d/60-isshoni.conf && sudo sysctl --system`
	if udp["status"] != "warn" || udp["code"] != "udp_buffers.low" || udp["message"] != "net.core.rmem_max is 212992, isshoni wants 8388608" ||
		udp["fixCode"] != "udp_buffers.fix_sysctl" || udp["fix"] != fix || udp["localOnly"] != false {
		t.Errorf("udp_buffers: %v", udp)
	}
	if p, _ := udp["params"].(map[string]any); p["rmemMax"] != float64(212992) || p["want"] != float64(8388608) {
		t.Errorf("udp_buffers params: %v", udp["params"])
	}
	// The bandwidth part is the calculator's output for the default session.
	want, _ := Bandwidth(doctor.DefaultBandwidthInput())
	if rep.Bandwidth == nil || *rep.Bandwidth != want {
		t.Errorf("bandwidth %+v, want %+v", rep.Bandwidth, want)
	}
}

// On demand doctor runs at most once per 10 s, and one run at a time: 429 doctor_busy with the seconds to wait
// (04 §13.1).
func TestDoctorCheckBusy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newDoctorWorld(t)
		d, _ := newTestDoctor(t, w, nil, nil)
		if _, err := d.Last(t.Context()); !api.IsCode(err, api.CodeNotFound) {
			t.Errorf("Last before the first run: %v, want not_found", err)
		}

		first, err := d.Check(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		// Right after it, and until 10 s have passed: busy, with the time left.
		for _, tt := range []struct {
			after time.Duration
			retry int
		}{{0, 10}, {3500 * time.Millisecond, 7}, {6400 * time.Millisecond, 1}} {
			time.Sleep(tt.after)
			_, err := d.Check(t.Context())
			var ae *api.Error
			if !errors.As(err, &ae) || ae.Code != api.CodeDoctorBusy || ae.RetryAfter != tt.retry {
				t.Errorf("Check: %v (%+v), want doctor_busy with retryAfter %d", err, ae, tt.retry)
			}
			if api.StatusOf(api.CodeDoctorBusy) != http.StatusTooManyRequests {
				t.Error("doctor_busy is not a 429")
			}
		}
		if w.runCount() != 1 {
			t.Errorf("%d runs, want 1: a refused request must not run the checks", w.runCount())
		}
		last, err := d.Last(t.Context())
		if err != nil || !last.RanAt.Equal(first.RanAt) || len(last.Checks) != len(first.Checks) {
			t.Errorf("Last = %v, %v; want the first report", last.RanAt, err)
		}
		time.Sleep(100 * time.Millisecond) // 10 s after the first run ended
		if _, err := d.Check(t.Context()); err != nil {
			t.Errorf("Check 10 s later: %v", err)
		}

		// While a run is under way a second request is refused at once.
		time.Sleep(time.Minute)
		block, entered := make(chan struct{}), make(chan struct{}, 4)
		w.set(func(w *doctorWorld) { w.block, w.entered = block, entered })
		done := make(chan error, 1)
		go func() {
			_, err := d.Check(t.Context())
			done <- err
		}()
		<-entered
		_, err = d.Check(t.Context())
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Code != api.CodeDoctorBusy || ae.RetryAfter < 1 {
			t.Errorf("Check during a run: %v, want doctor_busy", err)
		}
		// The CLI's request waits for its turn instead.
		cli := make(chan api.DoctorReport, 1)
		go func() {
			rep, err := d.RunFor(t.Context(), AdminDoctorRequest{Only: []string{"nofile"}})
			if err != nil {
				t.Errorf("RunFor: %v", err)
			}
			cli <- rep
		}()
		synctest.Wait()
		select {
		case <-cli:
			t.Fatal("RunFor did not wait for the run under way")
		default:
		}
		w.set(func(w *doctorWorld) { w.block = nil })
		close(block)
		if err := <-done; err != nil {
			t.Errorf("the blocked Check: %v", err)
		}
		rep := <-cli
		if rep.Mode != api.DoctorModeCLIWithServer || len(rep.Checks) != 1 || rep.Checks[0].ID != "nofile" {
			t.Errorf("RunFor report: mode %s, %d checks", rep.Mode, len(rep.Checks))
		}
		// Its run counts for the spacing too, and its partial report did not replace the last one.
		if _, err := d.Check(t.Context()); !api.IsCode(err, api.CodeDoctorBusy) {
			t.Errorf("Check right after the CLI's run: %v, want doctor_busy", err)
		}
		if last, _ := d.Last(t.Context()); len(last.Checks) != len(doctor.CheckIDs()) || last.Mode != api.DoctorModeServer {
			t.Errorf("Last after a partial run: %d checks, mode %s", len(last.Checks), last.Mode)
		}
	})
}

// A request whose caller goes away gets its context's error, and its cut-off report is not kept.
func TestDoctorCancelled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newDoctorWorld(t)
		d, _ := newTestDoctor(t, w, nil, nil)
		block, entered := make(chan struct{}), make(chan struct{}, 2)
		w.set(func(w *doctorWorld) { w.block, w.entered = block, entered })

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			_, err := d.Check(ctx)
			done <- err
		}()
		<-entered
		// A CLI request that waits for the turn and gives up.
		cliCtx, cliCancel := context.WithCancel(t.Context())
		cliDone := make(chan error, 1)
		go func() {
			_, err := d.RunFor(cliCtx, AdminDoctorRequest{})
			cliDone <- err
		}()
		synctest.Wait()
		cliCancel()
		if err := <-cliDone; !errors.Is(err, context.Canceled) {
			t.Errorf("RunFor of a caller that left: %v", err)
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("Check of a caller that left: %v", err)
		}
		if _, err := d.Last(t.Context()); !api.IsCode(err, api.CodeNotFound) {
			t.Errorf("Last after a cancelled run: %v, want not_found", err)
		}
		if d.Summary() != nil || d.Alerts() != nil {
			t.Error("a cancelled run left a summary")
		}
	})
}

// The server runs doctor 20 s after its start and every 24 h, and keeps the report in the meta table (04 §13.1).
func TestDoctorPeriodic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newDoctorWorld(t)
		now := clockAt(doctorNow)
		w.set(func(w *doctorWorld) { w.now = now })
		meta := newFakeTransferStore()
		d, logs := newTestDoctor(t, w, meta, now)
		if d.Summary() != nil || d.Alerts() != nil {
			t.Error("a summary before the first run")
		}

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- d.Run(ctx) }()
		time.Sleep(doctorFirstRun - time.Second)
		synctest.Wait()
		if w.runCount() != 0 {
			t.Fatalf("%d runs before 20 s", w.runCount())
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if w.runCount() != 1 {
			t.Fatalf("%d runs at 20 s, want 1", w.runCount())
		}
		sum := d.Summary()
		if sum == nil || !sum.RanAt.Equal(doctorNow.Add(doctorFirstRun)) || sum.OK != 13 || sum.Warn != 0 || sum.Fail != 0 {
			t.Errorf("summary %+v", sum)
		}
		if d.Alerts() != nil {
			t.Errorf("alerts %v for a healthy server", d.Alerts())
		}
		contains := func(parts ...string) {
			t.Helper()
			for _, p := range parts {
				if !strings.Contains(logs.String(), p) {
					t.Errorf("the log has no %q:\n%s", p, logs.String())
				}
			}
		}
		contains(`"msg":"doctor ran"`, `"component":"ops"`, `"ok":13`, `"fail":0`)

		// The report is in the meta table as JSON.
		var state struct {
			V      int
			Report api.DoctorReport
		}
		if err := json.Unmarshal([]byte(meta.metaValue(MetaDoctorLast)), &state); err != nil {
			t.Fatalf("meta value: %v", err)
		}
		if state.V != doctorStateVersion || state.Report.Summary.OK != 13 || len(state.Report.Checks) != len(doctor.CheckIDs()) {
			t.Errorf("meta value: v %d, summary %+v", state.V, state.Report.Summary)
		}
		if MetaDoctorLast != "ops.doctor_last" {
			t.Errorf("the meta key is %q; 03 §5 lists ops.doctor_last", MetaDoctorLast)
		}

		// A day later: the next run. The server has lost its address meanwhile, which fails two checks.
		w.set(func(w *doctorWorld) { w.certGone = true })
		time.Sleep(doctorEvery - time.Second)
		synctest.Wait()
		if w.runCount() != 1 {
			t.Fatalf("%d runs before 24 h", w.runCount())
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if w.runCount() != 2 {
			t.Fatalf("%d runs after 24 h, want 2", w.runCount())
		}
		sum = d.Summary()
		if sum == nil || sum.Fail != 2 || !sum.RanAt.Equal(doctorNow.Add(doctorFirstRun+doctorEvery)) {
			t.Errorf("summary %+v, want 2 failures", sum)
		}
		alerts := d.Alerts()
		if len(alerts) != 1 || alerts[0].Code != api.AlertCodeDoctorFail || alerts[0].Severity != api.AlertSeverityError ||
			alerts[0].Params["fail"] != 2 || !slices.Equal(alerts[0].Params["checks"].([]string), []string{"public_ip", "tls"}) {
			t.Errorf("alerts %+v", alerts)
		}
		contains(`"msg":"doctor found problems; `+"`isshoni doctor`"+` shows them with their fixes"`, `"level":"WARN"`, `"failed":["public_ip","tls"]`)

		// Run can't be called twice, and ends with its context.
		if err := d.Run(ctx); err == nil {
			t.Error("a second Run returned nil")
		}
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run returned %v, want nil", err)
		}
	})
}

// A restarted server has the last report at once, from the meta table; a value it can't use is no report.
func TestDoctorLoadsLastReport(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newDoctorWorld(t)
		meta := newFakeTransferStore()
		first, _ := newTestDoctor(t, w, meta, nil)
		rep, err := first.Check(t.Context())
		if err != nil {
			t.Fatal(err)
		}

		start := func(meta MetaStore) (*Doctor, *syncBuffer, func()) {
			d, logs := newTestDoctor(t, w, meta, nil)
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- d.Run(ctx) }()
			synctest.Wait()
			return d, logs, func() {
				cancel()
				if err := <-done; err != nil {
					t.Errorf("Run: %v", err)
				}
			}
		}
		d, _, stop := start(meta)
		last, err := d.Last(t.Context())
		if err != nil || !last.RanAt.Equal(rep.RanAt) || last.Summary != rep.Summary || len(last.Checks) != len(rep.Checks) {
			t.Errorf("Last after a restart: %+v, %v", last.Summary, err)
		}
		if sum := d.Summary(); sum == nil || sum.OK != rep.Summary.OK {
			t.Errorf("Summary after a restart: %+v", sum)
		}
		// The loaded report survives JSON: its texts and params are those of the run.
		a, _ := json.Marshal(rep)
		b, _ := json.Marshal(last)
		if string(a) != string(b) {
			t.Errorf("the loaded report differs from the one that was kept:\n%s\n%s", a, b)
		}
		stop()

		for name, value := range map[string]string{
			"empty":          "",
			"not JSON":       "{",
			"another layout": `{"v":99,"report":{"schema":1,"ranAt":"2026-09-29T20:15:00.000Z"}}`,
			"another schema": `{"v":1,"report":{"schema":2,"ranAt":"2026-09-29T20:15:00.000Z"}}`,
			"no time":        `{"v":1,"report":{"schema":1}}`,
		} {
			bad := newFakeTransferStore()
			bad.meta[MetaDoctorLast] = value
			d, _, stop := start(bad)
			if _, err := d.Last(t.Context()); !api.IsCode(err, api.CodeNotFound) {
				t.Errorf("%s: Last = %v, want not_found", name, err)
			}
			stop()
		}

		// A store that fails costs the report, not the run.
		broken := newFakeTransferStore()
		broken.failGM, broken.failSet = errors.New("database is locked"), errors.New("database is locked")
		d, logs, stop := start(broken)
		if _, err := d.Check(t.Context()); err != nil {
			t.Errorf("Check with a failing store: %v", err)
		}
		if d.Summary() == nil {
			t.Error("the report was not kept in memory")
		}
		for _, line := range []string{"reading the last doctor report failed", "keeping the doctor report failed"} {
			if !strings.Contains(logs.String(), line) {
				t.Errorf("the log has no %q:\n%s", line, logs.String())
			}
		}
		stop()
	})
}

// NewDoctor without an Env is a wiring bug.
func TestNewDoctorNeedsEnv(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewDoctor without Env did not panic")
		}
	}()
	NewDoctor(DoctorOptions{})
}

// POST /v1/doctor on the admin socket: the CLI's way to doctor a running server (04 §12.2, §13.1).
func TestAdminDoctor(t *testing.T) {
	w := newDoctorWorld(t)
	log, _ := testLogger()
	d := NewDoctor(DoctorOptions{Env: w.env, Logger: log})
	a := startAdmin(t, func(o *AdminOptions, _ *AdminListenOptions) { o.Doctor = d })

	rep, err := a.client.Doctor(t.Context(), AdminDoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Mode != api.DoctorModeCLIWithServer || len(rep.Checks) != len(doctor.CheckIDs()) || rep.Summary.Fail != 0 ||
		rep.Bandwidth == nil || rep.Bandwidth.Input != doctor.DefaultBandwidthInput() {
		t.Errorf("report: mode %s, %d checks, summary %+v, bandwidth %+v", rep.Mode, len(rep.Checks), rep.Summary, rep.Bandwidth)
	}
	// A full run through the socket is the server's own run: the dashboard has it.
	if sum := d.Summary(); sum == nil || sum.OK != rep.Summary.OK {
		t.Errorf("Summary after the socket's run: %+v", sum)
	}

	// Some checks, and another session for the estimate.
	in := api.BandwidthInput{People: 10, Sharing: 10, Thumbnails: 8, Quality: api.BandwidthQuality1080p60, Preset: api.BandwidthPresetAuto, Hours: 2}
	rep, err = a.client.Doctor(t.Context(), AdminDoctorRequest{Only: []string{"bandwidth", "clock"}, Bandwidth: &in})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Checks) != 2 || rep.Checks[0].ID != "clock" || rep.Checks[1].ID != "bandwidth" || rep.Bandwidth == nil ||
		rep.Bandwidth.Input != in || rep.Bandwidth.EgressMediaMbps != 105.28 {
		t.Errorf("partial report: %+v, bandwidth %+v", rep.Checks, rep.Bandwidth)
	}
	if !strings.Contains(rep.Checks[1].Message, "10 people, 10 sharing: ~105.3 Mbps egress") {
		t.Errorf("bandwidth message %q", rep.Checks[1].Message)
	}
	if last, _ := d.Last(t.Context()); len(last.Checks) != len(doctor.CheckIDs()) {
		t.Errorf("a partial run replaced the last report (%d checks)", len(last.Checks))
	}

	// The body on the wire: empty is fine (curl -X POST), and the report is 04 §13.5's document.
	code, _, body := a.raw(t, http.MethodPost, "/v1/doctor", `{"only":["nofile"]}`)
	if code != 200 || !strings.Contains(body, `"mode":"cli-with-server"`) || !strings.Contains(body, `"id":"nofile"`) ||
		!strings.Contains(body, `"ranAt":"2026-09-29T20:15:00.000Z"`) {
		t.Errorf("POST /v1/doctor = %d %s", code, body)
	}
	if code, _, body := a.raw(t, http.MethodPost, "/v1/doctor", ""); code != 200 || !strings.Contains(body, `"id":"config"`) {
		t.Errorf("POST /v1/doctor without a body = %d %s", code, body)
	}

	// What doctor does not have is refused with the list of what it has.
	_, err = a.client.Doctor(t.Context(), AdminDoctorRequest{Only: []string{"dns", "nope"}})
	ae := adminError(t, err, api.CodeBadRequest)
	if !strings.Contains(ae.Message, `doctor has no check "nope"`) || !strings.Contains(ae.Message, "config, data_dir, secrets") {
		t.Errorf("unknown check: %q", ae.Message)
	}
	bad := in
	bad.Sharing = 11
	_, err = a.client.Doctor(t.Context(), AdminDoctorRequest{Bandwidth: &bad})
	if ae := adminError(t, err, api.CodeBadRequest); !strings.Contains(ae.Message, "bandwidth.sharing") {
		t.Errorf("bad bandwidth input: %q", ae.Message)
	}
	if code, hdr, _ := a.raw(t, http.MethodGet, "/v1/doctor", ""); code != 405 || hdr.Get("Allow") != "POST" {
		t.Errorf("GET /v1/doctor = %d (Allow %q)", code, hdr.Get("Allow"))
	}

	// During a shutdown it answers 503 like every other call.
	a.health.SetShuttingDown()
	_, err = a.client.Doctor(t.Context(), AdminDoctorRequest{})
	wantCode(t, err, api.CodeServerShutdown)
}

// A server whose wiring has no doctor says so, and a request is still checked first.
func TestAdminDoctorUnavailable(t *testing.T) {
	a := startAdmin(t, nil)
	_, err := a.client.Doctor(t.Context(), AdminDoctorRequest{Only: []string{"dns"}})
	if ae := adminError(t, err, api.CodeInternal); !strings.Contains(ae.Message, "this server's admin socket has no doctor") {
		t.Errorf("Doctor without a doctor: %q", ae.Message)
	}
	_, err = a.client.Doctor(t.Context(), AdminDoctorRequest{Only: []string{"nope"}})
	wantCode(t, err, api.CodeBadRequest)
}

// A CLI that hangs up while its run waits or runs does not hold the turn.
func TestAdminDoctorClientGone(t *testing.T) {
	w := newDoctorWorld(t)
	block, entered := make(chan struct{}), make(chan struct{}, 2)
	w.set(func(w *doctorWorld) { w.block, w.entered = block, entered })
	log, _ := testLogger()
	d := NewDoctor(DoctorOptions{Env: w.env, Logger: log})
	a := startAdmin(t, func(o *AdminOptions, _ *AdminListenOptions) { o.Doctor = d })

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := a.client.Doctor(ctx, AdminDoctorRequest{})
		done <- err
	}()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Doctor of a client that left: %v", err)
	}
	// The server's run ends with the request's context, and the next one gets its turn.
	w.set(func(w *doctorWorld) { w.block = nil })
	rep, err := a.client.Doctor(t.Context(), AdminDoctorRequest{Only: []string{"nofile"}})
	if err != nil || len(rep.Checks) != 1 {
		t.Errorf("the next run: %v, %v", rep.Checks, err)
	}
	close(block)
}

// A shutdown does not wait for a doctor run: the run ends, and its caller is told that the server stops (04 §6.4).
func TestAdminDoctorShutdown(t *testing.T) {
	w := newDoctorWorld(t)
	block, entered := make(chan struct{}), make(chan struct{}, 2)
	defer close(block)
	w.set(func(w *doctorWorld) { w.block, w.entered = block, entered })
	log, _ := testLogger()
	d := NewDoctor(DoctorOptions{Env: w.env, Logger: log})
	a := startAdmin(t, func(o *AdminOptions, _ *AdminListenOptions) { o.Doctor = d })

	running, waiting := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := a.client.Doctor(t.Context(), AdminDoctorRequest{})
		running <- err
	}()
	<-entered
	go func() { // a second request, which waits for its turn
		_, err := a.client.Doctor(t.Context(), AdminDoctorRequest{Only: []string{"nofile"}})
		waiting <- err
	}()
	// Give the second request the time to reach the server; it is refused the same way if it arrives later.
	time.Sleep(50 * time.Millisecond)

	started := time.Now()
	a.stop(t)
	if took := time.Since(started); took > 3*time.Second {
		t.Errorf("the shutdown took %s: it waited for the doctor run", took)
	}
	for name, ch := range map[string]chan error{"the running request": running, "the waiting request": waiting} {
		err := <-ch
		var ae *AdminError
		if !errors.As(err, &ae) || ae.API.Code != api.CodeServerShutdown {
			// A request that had not been read yet when the server stopped sees the connection close instead.
			if name == "the waiting request" && errors.Is(err, ErrAdminNotRunning) {
				continue
			}
			t.Errorf("%s: %v, want server_shutdown", name, err)
		}
	}
	if d.Summary() != nil {
		t.Error("the run that the shutdown cut off was kept")
	}
}

// The typed functions of GET /api/v1/admin/bandwidth (04 §13.4).
func TestBandwidthFunctions(t *testing.T) {
	want := api.BandwidthEstimate{
		Input:           api.BandwidthInput{People: 5, Sharing: 2, Thumbnails: 8, Quality: "1080p60", Preset: "auto", Hours: 2},
		PerViewerMbps:   api.BandwidthPerViewer{Sharer: 8.13, Viewer: 8.43},
		EgressMediaMbps: 41.54, EgressWireMbps: 43.62, IngressMediaMbps: 16.86, TransferPerSessionGB: 39.3,
	}
	// The query of 04 §13.4, and nothing at all: the defaults are that session.
	for _, query := range []string{"people=5&sharing=2&thumbnails=8&quality=1080p60&preset=auto&hours=2", "", "color=blue"} {
		q, err := url.ParseQuery(query)
		if err != nil {
			t.Fatal(err)
		}
		got, err := BandwidthFromQuery(q)
		if err != nil || got != want {
			t.Errorf("?%s: %+v, %v; want %+v", query, got, err, want)
		}
	}
	data, _ := json.Marshal(want)
	const wantJSON = `{"input":{"people":5,"sharing":2,"thumbnails":8,"quality":"1080p60","preset":"auto","hours":2},` +
		`"perViewerMbps":{"sharer":8.13,"viewer":8.43},"egressMediaMbps":41.54,"egressWireMbps":43.62,` +
		`"ingressMediaMbps":16.86,"transferPerSessionGb":39.3}`
	if string(data) != wantJSON {
		t.Errorf("JSON:\n got %s\nwant %s", data, wantJSON)
	}

	// The plan's example through the query.
	q, _ := url.ParseQuery("people=10&sharing=10&hours=2.5&preset=movie&quality=2160p60&thumbnails=0")
	got, err := BandwidthFromQuery(q)
	if err != nil || got.Input != (api.BandwidthInput{People: 10, Sharing: 10, Thumbnails: 0, Quality: "2160p60", Preset: "movie", Hours: 2.5}) {
		t.Errorf("a query with every parameter: %+v, %v", got.Input, err)
	}

	// A parameter that is no number, or out of range: 400 bad_request naming it.
	for query, field := range map[string]string{
		"people=five":        "people",
		"people=":            "people",
		"people=0":           "people",
		"people=5.5":         "people",
		"sharing=6":          "sharing",
		"sharing=-1":         "sharing",
		"thumbnails=x":       "thumbnails",
		"thumbnails=101":     "thumbnails",
		"quality=720p30":     "quality",
		"quality=":           "quality",
		"preset=game":        "preset",
		"hours=two":          "hours",
		"hours=0":            "hours",
		"hours=NaN":          "hours",
		"hours=1e9":          "hours",
		"people=3&sharing=4": "sharing",
	} {
		q, err := url.ParseQuery(query)
		if err != nil {
			t.Fatal(err)
		}
		_, err = BandwidthFromQuery(q)
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Code != api.CodeBadRequest || ae.Params[api.ParamField] != field {
			t.Errorf("?%s: %v (%+v), want bad_request for %s", query, err, ae, field)
		}
	}
	if _, err := Bandwidth(api.BandwidthInput{}); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("Bandwidth of the zero input: %v, want bad_request", err)
	}
	if got, err := Bandwidth(doctor.DefaultBandwidthInput()); err != nil || got != want {
		t.Errorf("Bandwidth of the default input: %+v, %v", got, err)
	}
}
