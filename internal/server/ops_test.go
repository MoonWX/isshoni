package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server"
	"github.com/MoonWX/isshoni/internal/server/netx"
	"github.com/MoonWX/isshoni/internal/server/ops"
	"github.com/MoonWX/isshoni/internal/server/servertest"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

// The ops data in a running server (README S85; 04 §6.6, §11): the admin routes of ops on the REST API, the
// dashboard over the live hub and SFU, the metrics endpoint with every component's series, and the transfer
// accounting from the sockets to 03's table.

// opsRoutes are the admin routes of ops (04 §9.2), with a body for the unsafe one.
var opsRoutes = []struct {
	method, path string
	body         any
}{
	{http.MethodGet, "/api/v1/admin/dashboard", nil},
	{http.MethodGet, "/api/v1/admin/doctor", nil},
	{http.MethodPost, "/api/v1/admin/doctor", struct{}{}},
	{http.MethodGet, "/api/v1/admin/bandwidth?people=5&sharing=2", nil},
}

// TestOpsRoutesForAdminsOnly: the dashboard, doctor's report and run, and the bandwidth estimate answer 403
// forbidden to a signed-in member and 401 to a browser without a session, before anything runs; an admin gets
// them: the dashboard, doctor's 404 before its first report, a run on demand that is then the last report, 429
// doctor_busy for a second run within 10 s, and the estimate with 400 bad_request for a value out of range.
func TestOpsRoutesForAdminsOnly(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{Deps: wiredDeps()})
	setUp(t, srv)
	member := register(t, srv, "Kim")
	stranger := browser(t, srv)
	for _, r := range opsRoutes {
		wantStatus(t, "a member's "+r.method+" "+r.path, callAs(t, member, srv, r.method, r.path, r.body),
			http.StatusForbidden, api.CodeForbidden)
		wantStatus(t, "a stranger's "+r.method+" "+r.path, callAs(t, stranger, srv, r.method, r.path, r.body),
			http.StatusUnauthorized, api.CodeUnauthenticated)
	}
	// The member's POST ran nothing: there is still no report.
	if _, err := srv.Srv.Doctor().Last(context.Background()); err == nil {
		t.Error("a member's POST /admin/doctor ran doctor")
	}

	// The admin's.
	res := call(t, srv, http.MethodGet, "/api/v1/admin/dashboard", nil)
	var dash api.OpsDashboard
	if res.status != http.StatusOK || json.Unmarshal([]byte(res.body), &dash) != nil || dash.Server.Origin != srv.Srv.Site().Origin {
		t.Fatalf("the admin's GET /admin/dashboard = %d %s", res.status, res.body)
	}
	if res.header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", res.header.Get("Cache-Control"))
	}
	wantStatus(t, "GET /admin/doctor before the first report", call(t, srv, http.MethodGet, "/api/v1/admin/doctor", nil),
		http.StatusNotFound, api.CodeNotFound)

	res = call(t, srv, http.MethodGet, "/api/v1/admin/bandwidth?people=5&sharing=2&quality=1080p60", nil)
	var est api.BandwidthEstimate
	if res.status != http.StatusOK || json.Unmarshal([]byte(res.body), &est) != nil || est.Input.People != 5 || est.Input.Sharing != 2 {
		t.Errorf("GET /admin/bandwidth = %d %s", res.status, res.body)
	}
	res = call(t, srv, http.MethodGet, "/api/v1/admin/bandwidth?people=0", nil)
	if res.status != http.StatusBadRequest || errorCode(res.body) != api.CodeBadRequest || !strings.Contains(res.body, `"field":"people"`) {
		t.Errorf("GET /admin/bandwidth?people=0 = %d %s, want 400 bad_request with field people", res.status, res.body)
	}

	res = call(t, srv, http.MethodPost, "/api/v1/admin/doctor", struct{}{})
	var rep api.DoctorReport
	if res.status != http.StatusOK || json.Unmarshal([]byte(res.body), &rep) != nil || rep.Mode != api.DoctorModeServer ||
		len(rep.Checks) == 0 {
		t.Fatalf("POST /admin/doctor = %d %s", res.status, res.body)
	}
	res = call(t, srv, http.MethodGet, "/api/v1/admin/doctor", nil)
	var last api.DoctorReport
	if res.status != http.StatusOK || json.Unmarshal([]byte(res.body), &last) != nil || !last.RanAt.Equal(rep.RanAt) {
		t.Errorf("GET /admin/doctor after a run = %d %s, want the run of %v", res.status, res.body, rep.RanAt)
	}
	res = call(t, srv, http.MethodPost, "/api/v1/admin/doctor", struct{}{})
	if res.status != http.StatusTooManyRequests || errorCode(res.body) != api.CodeDoctorBusy || res.header.Get("Retry-After") == "" {
		t.Errorf("a second POST /admin/doctor within 10 s = %d %s (Retry-After %q), want 429 doctor_busy",
			res.status, res.body, res.header.Get("Retry-After"))
	}
}

// getDashboard is the admin's GET /api/v1/admin/dashboard.
func getDashboard(t *testing.T, srv *servertest.Server) api.OpsDashboard {
	t.Helper()
	res := call(t, srv, http.MethodGet, "/api/v1/admin/dashboard", nil)
	var dash api.OpsDashboard
	if res.status != http.StatusOK {
		t.Fatalf("GET /admin/dashboard = %d %s", res.status, res.body)
	}
	if err := json.Unmarshal([]byte(res.body), &dash); err != nil {
		t.Fatalf("GET /admin/dashboard: %v: %s", err, res.body)
	}
	return dash
}

// TestDashboardInServer: the dashboard of a running server has every source (04 §11.4): the status, the process,
// the month's transfer, the live room with the admin's connection, 03's accounts, and no failed source; doctor's
// summary once doctor has run; and the alert public_ip.changed once the server's look found another address.
func TestDashboardInServer(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{Deps: wiredDeps()})
	setUp(t, srv)
	c := dialWS(t, srv)
	welcome, _ := joinLounge(t, c, signaltest.DefaultHello())

	dash := getDashboard(t, srv)
	for _, a := range dash.Alerts {
		if a.Code == api.AlertCodeDashboardSourceFailed {
			t.Errorf("a source failed: %+v", a)
		}
	}
	site := srv.Srv.Site()
	if dash.Server.Origin != site.Origin || dash.Server.TLS.Mode != api.TLSModeOff || dash.Server.Process.NumCPU < 1 ||
		dash.Server.Process.Goroutines < 1 || dash.Server.StartedAt.IsZero() || len(dash.Server.Advertised) == 0 {
		t.Errorf("server = %+v", dash.Server)
	}
	if month := time.Now().UTC().Format("2006-01"); dash.Transfer.Month != month || dash.Transfer.EgressBytes <= 0 {
		t.Errorf("transfer = %+v, want %s with the web bytes so far", dash.Transfer, month)
	}
	if dash.Accounts == nil || dash.Accounts.Users.Admins != 1 || dash.Accounts.Users.Active != 1 {
		t.Errorf("accounts = %+v, want the one admin", dash.Accounts)
	}
	if dash.Doctor != nil {
		t.Errorf("doctor = %+v before the first run", dash.Doctor)
	}
	if len(dash.Rooms) != 1 || dash.Rooms[0].ID != "lounge" || dash.Rooms[0].Name != "Lounge" ||
		len(dash.Rooms[0].Participants) != 1 || dash.Rooms[0].Participants[0].Username != adminName {
		t.Fatalf("rooms = %+v, want the admin in Lounge", dash.Rooms)
	}
	conns := dash.Rooms[0].Participants[0].Connections
	hello := signaltest.DefaultHello()
	if len(conns) != 1 || conns[0].ID != welcome.ConnectionID || conns[0].Kind != string(hello.Client.Kind) ||
		conns[0].Version != hello.Client.Version || conns[0].OS != string(hello.Client.OS) || conns[0].ConnectedAt.IsZero() {
		t.Errorf("connections = %+v, want the one of welcome %s", conns, welcome.ConnectionID)
	}
	if want := (api.ClientVersionCount{Kind: "web", Version: "0.1.0", Count: 1}); len(dash.Clients) != 1 || dash.Clients[0].Kind != want.Kind ||
		dash.Clients[0].Version != want.Version || dash.Clients[0].Count != 1 {
		t.Errorf("clients = %+v, want %+v", dash.Clients, want)
	}

	// The public address changed since the start (04 §7.4): after the second the snapshot is kept, it says so.
	srv.Srv.SetPublicAddrsSeen(netx.PublicAddrs{V4: netip.MustParseAddr("203.0.113.99")})
	if _, err := srv.Srv.Doctor().Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	dash = getDashboard(t, srv)
	var changed *api.Alert
	for i, a := range dash.Alerts {
		if a.Code == api.AlertCodePublicIPChanged {
			changed = &dash.Alerts[i]
		}
	}
	if changed == nil || changed.Params["to"] != "203.0.113.99" || changed.Params["family"] != "ipv4" {
		t.Errorf("alerts = %+v, want public_ip.changed to 203.0.113.99", dash.Alerts)
	}
	if dash.Doctor == nil || dash.Doctor.OK == 0 {
		t.Errorf("doctor = %+v after a run", dash.Doctor)
	}
}

// metricsOf scrapes GET /metrics on the metrics listener and returns its samples as `name{labels}` → value, with
// the labels as the text format writes them.
func metricsOf(t *testing.T, addr net.Addr) map[string]float64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr.String()+"/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("scrape: %d %s", res.StatusCode, b)
	}
	out := map[string]float64{}
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		v, err := strconv.ParseFloat(line[i+1:], 64)
		if i < 0 || err != nil {
			t.Fatalf("scrape: a line that is no sample: %q", line)
		}
		out[line[:i]] = v
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scrape: %v", err)
	}
	return out
}

// hasSeries reports whether a scrape has a sample of the metric name.
func hasSeries(samples map[string]float64, name string) bool {
	for k := range samples {
		if k == name || strings.HasPrefix(k, name+"{") {
			return true
		}
	}
	return false
}

// TestMetricsEndpoint: with metrics.enabled the server serves its registry on metrics.listen (04 §11.2), which the
// ready line, Addrs and the status name: the server's own series (readiness, the web bytes the sockets counted, the
// router's responses by route), the hub's (01 §18), the SFU's (02 §13, through the wiring's collector) and
// isshoni_push_sent_total, which counts what the push service sent (04 §6.6). /healthz is there too, and the
// listener goes with the shutdown.
func TestMetricsEndpoint(t *testing.T) {
	deps, pushes := pushDeps()
	srv := servertest.Start(t, servertest.Options{Deps: deps,
		Flags: []string{"--metrics.enabled", "--metrics.listen=127.0.0.1:0"}})
	setUp(t, srv)
	addr := srv.Srv.Addrs().Metrics
	if _, ok := addr.(*net.TCPAddr); !ok {
		t.Fatalf("Addrs().Metrics = %v, want the bound listener", addr)
	}
	ready := false
	for _, rec := range logRecords(t, srv.Logs()) {
		if msg, _ := rec["msg"].(string); strings.Contains(msg, " ready: ") && rec["listen_metrics"] == addr.String() {
			ready = true
		}
		if msg, _ := rec["msg"].(string); strings.Contains(msg, "metrics.enabled") {
			t.Errorf("the log still warns about metrics.enabled: %v", rec)
		}
	}
	if !ready {
		t.Errorf("no ready line with listen_metrics=%s:\n%s", addr, srv.Logs())
	}
	admin, ctx := adminSocket(t, srv)
	status, err := admin.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, l := range status.Listeners {
		found = found || l == (api.ListenerInfo{Key: "metrics.listen", Network: "tcp", Addr: addr.String()})
	}
	if !found || status.Transfer == nil {
		t.Errorf("status: listeners %+v, transfer %+v; want metrics.listen and the month's transfer", status.Listeners, status.Transfer)
	}

	// Some of everything: a signaling connection in Lounge, and a test notification that the push service sends.
	c := dialWS(t, srv)
	joinLounge(t, c, signaltest.DefaultHello())
	subscribe(t, srv, srv.Client, pushes, "alex-laptop")
	wantStatus(t, "POST /push/test", call(t, srv, http.MethodPost, "/api/v1/push/test", struct{}{}), http.StatusAccepted, "")
	waitPushes(t, pushes, 1)

	var samples map[string]float64
	eventually(t, func() bool {
		samples = metricsOf(t, addr)
		return samples[`isshoni_push_sent_total{result="ok"}`] == 1
	}, "isshoni_push_sent_total{result=\"ok\"} counts the test notification")
	for key, want := range map[string]float64{
		`isshoni_ready`:        1,
		`isshoni_rooms`:        1,
		`isshoni_participants`: 1,
		`isshoni_ws_connections{kind="web",role="full"}`:                           1,
		`isshoni_http_responses_total{class="2xx",route="POST /api/v1/push/test"}`: 1,
		`isshoni_push_sent_total{result="gone"}`:                                   0,
		`isshoni_sfu_selected_transport{transport="tcp443"}`:                       0,
		`isshoni_sfu_conns{role="full"}`:                                           1,
	} {
		if got, ok := samples[key]; !ok || got != want {
			t.Errorf("%s = %v (present: %t), want %v", key, got, ok, want)
		}
	}
	if samples[`isshoni_transfer_bytes_total{direction="egress",path="web"}`] <= 0 ||
		samples[`isshoni_transfer_bytes_total{direction="ingress",path="web"}`] <= 0 {
		t.Errorf("the web bytes of listen.http are not counted: %v", samples)
	}
	// Every series of 02 §13, and the Go and process collectors.
	for _, name := range []string{
		"isshoni_build_info", "isshoni_portmux_conns_total", "go_goroutines",
		"isshoni_sfu_conns", "isshoni_sfu_peerconnections", "isshoni_sfu_selected_transport", "isshoni_sfu_shares",
		"isshoni_sfu_downtracks", "isshoni_sfu_ingress_bytes_total", "isshoni_sfu_ingress_packets_total",
		"isshoni_sfu_ingress_duplicates_total", "isshoni_sfu_egress_bytes_total", "isshoni_sfu_egress_packets_total",
		"isshoni_sfu_nack_received_total", "isshoni_sfu_nack_served_total", "isshoni_sfu_nack_missed_total",
		"isshoni_sfu_rtx_skipped_total", "isshoni_sfu_pli_sent_total", "isshoni_sfu_pli_throttled_total",
		"isshoni_sfu_keyframes_received_total", "isshoni_sfu_layer_switches_total", "isshoni_sfu_downgrades_total",
		"isshoni_sfu_queue_drops_total", "isshoni_sfu_handshake_timeouts_total", "isshoni_sfu_packet_cache_bytes",
	} {
		if !hasSeries(samples, name) {
			t.Errorf("no %s in the scrape", name)
		}
	}
	if hasSeries(samples, "isshoni_tls_cert_not_after_seconds") {
		t.Error("an off-mode server has no certificate, and no isshoni_tls_cert_not_after_seconds")
	}

	// /healthz on the metrics listener, and the listener closes with the server.
	res, err := http.Get("http://" + addr.String() + "/healthz") //nolint:noctx // a loopback test server
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz on the metrics listener = %d", res.StatusCode)
	}
	srv.Stop(t)
	d := net.Dialer{Timeout: time.Second}
	if conn, err := d.DialContext(context.Background(), "tcp", addr.String()); err == nil {
		_ = conn.Close()
		t.Errorf("%s still accepts connections after the shutdown", addr)
	}
}

// TestMetricsOff: without metrics.enabled nothing listens, but the registry is there all the same (04 §11.2): the
// components register on it unconditionally.
func TestMetricsOff(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{Deps: wiredDeps()})
	if addr := srv.Srv.Addrs().Metrics; addr != nil {
		t.Errorf("Addrs().Metrics = %v without metrics.enabled", addr)
	}
	families, err := srv.Srv.Metrics().Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range families {
		names[f.GetName()] = true
	}
	for _, name := range []string{"isshoni_ready", "isshoni_rooms", "isshoni_sfu_conns", "isshoni_push_sent_total"} {
		if !names[name] {
			t.Errorf("no %s in the registry", name)
		}
	}
}

// TestPushMetricsNeedPush: with push.enabled = false there is no push service, and no isshoni_push_sent_total.
func TestPushMetricsNeedPush(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{Deps: wiredDeps(), Flags: []string{"--push.enabled=false"}})
	families, err := srv.Srv.Metrics().Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == "isshoni_push_sent_total" {
			t.Error("isshoni_push_sent_total without a push service")
		}
	}
}

// TestMetricsPortInUse: a metrics.listen that can't be bound ends the start with a runtime error that names the key
// (04 §6.1 step 6), not a refusal.
func TestMetricsPortInUse(t *testing.T) {
	busy := listenLoopback(t)
	_, err := servertest.Try(t, servertest.Options{Deps: wiredDeps(),
		Flags: []string{"--metrics.enabled", "--metrics.listen=" + busy.Addr().String()}})
	if err == nil || server.NeedsOperator(err) || !strings.Contains(err.Error(), "metrics.listen") {
		t.Errorf("a busy metrics.listen: %v, want a runtime error that names metrics.listen", err)
	}
}

// TestTransferAccounting: the bytes the sockets count reach 03's monthly table (04 §11.3): a backup flushes them
// first, so it holds the month as it is, and the shutdown stores what is left.
func TestTransferAccounting(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{Deps: wiredDeps()})
	setUp(t, srv)
	month := time.Now().UTC().Format("2006-01")
	monthRow := func() (egress, ingress int64) {
		rows := query(t, srv, true, `SELECT egress_bytes, ingress_bytes FROM transfer_months WHERE month = ?`, month)
		if len(rows) != 1 {
			return 0, 0
		}
		egress, _ = strconv.ParseInt(rows[0][0], 10, 64)
		ingress, _ = strconv.ParseInt(rows[0][1], 10, 64)
		return egress, ingress
	}

	takeBackup(t, srv) // flushes the counters before the database's snapshot
	egress, ingress := monthRow()
	if egress <= 0 || ingress <= 0 {
		t.Fatalf("after a backup the month's row has %d bytes out and %d in, want the setup's", egress, ingress)
	}
	info := srv.Srv.TransferAccounting().Info()
	if info.Month != month || info.EgressBytes < egress {
		t.Errorf("transfer = %+v, want %s with at least the %d bytes stored", info, month, egress)
	}

	for range 3 {
		call(t, srv, http.MethodGet, "/api/v1/me", nil)
	}
	srv.Stop(t)
	if e, i := monthRow(); e <= egress || i <= ingress {
		t.Errorf("after the shutdown the month's row has %d bytes out and %d in, want more than %d and %d", e, i, egress, ingress)
	}
	if strings.Contains(srv.Logs(), "transfer accounting:") {
		t.Errorf("the transfer accounting complained:\n%s", srv.Logs())
	}
}

// TestDashboardRoutesNeedASite: a server without a site has no REST API, so no ops routes; its admin socket still
// answers status with the transfer (04 §6.6).
func TestDashboardRoutesNeedASite(t *testing.T) {
	cfg := testConfig(t, tlsFlags("--tls.mode=ip", closedCA)...)
	s, _ := startDirect(t, cfg, testDeps(), netx.PublicAddrs{})
	if s.Site().Origin != "" {
		t.Fatalf("site %+v, want none", s.Site())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	status, err := ops.DialAdmin(cfg.Listen.AdminSocket).Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Transfer == nil {
		t.Errorf("status of a server without a site has no transfer: %+v", status)
	}
	if s.Metrics() == nil || s.TransferAccounting() == nil {
		t.Error("a server without a site has no metrics or no transfer accounting")
	}
	var ae *api.Error
	if _, err := s.Doctor().Last(ctx); !errors.As(err, &ae) {
		t.Errorf("doctor's last report: %v", err)
	}
}
