package ops

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/MoonWX/isshoni/internal/server/netx"
	"github.com/MoonWX/isshoni/internal/version"
)

// metricsListener serves m on a loopback port of the kernel's choice, as the server does on metrics.listen, and
// returns the base URL. The cleanup shuts the listener down and checks that Serve returned nil.
func metricsListener(t *testing.T, m *Metrics) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- m.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
		if err := <-served; err != nil {
			t.Errorf("Serve returned %v after Shutdown, want nil", err)
		}
	})
	return "http://" + ln.Addr().String()
}

// httpGet does one request over a connection of its own and returns the status, the header and the body.
func httpGet(t *testing.T, method, url string) (int, http.Header, string) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(t.Context(), method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, res.Header, string(body)
}

// seriesValue returns the value of one series in a text-format scrape. series is the line's start as Prometheus
// writes it, labels in alphabetical order: `isshoni_ready` or `isshoni_transfer_bytes_total{direction="egress",path="web"}`.
func seriesValue(t *testing.T, scrape, series string) (float64, bool) {
	t.Helper()
	for line := range strings.SplitSeq(scrape, "\n") {
		rest, ok := strings.CutPrefix(line, series+" ")
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(rest, 64)
		if err != nil {
			t.Fatalf("series %s: value %q: %v", series, rest, err)
		}
		return v, true
	}
	return 0, false
}

// wantSample fails unless the scrape has series with this value.
func wantSample(t *testing.T, scrape, series string, want float64) {
	t.Helper()
	got, ok := seriesValue(t, scrape, series)
	if !ok {
		t.Errorf("no series %s in the scrape", series)
		return
	}
	if got != want {
		t.Errorf("%s = %v, want %v", series, got, want)
	}
}

// TestMetricsScrape is the scrape test of the slice: a Metrics served on 127.0.0.1 answers GET /metrics with every
// series of 04 §11.2 that ops owns, with the label values of that table, plus the Go and process collectors and
// what a component registered through Registerer.
func TestMetricsScrape(t *testing.T) {
	health := NewHealth()
	var dbUp atomic.Bool
	health.AddCheck("db", func() (bool, string) { return dbUp.Load(), "opening" })

	counter := new(netx.TransferCounter)
	counter.Add(netx.PathMediaUDP, true, 1500)
	counter.Add(netx.PathMediaUDP, false, 90)
	counter.Add(netx.PathMediaTCP, true, 700)
	counter.Add(netx.PathWeb, true, 4096)
	counter.Add(netx.PathWeb, false, 512)

	var certAt atomic.Int64 // unix seconds; 0 = no certificate yet
	m := NewMetrics(MetricsOptions{
		Health:   health,
		Transfer: counter,
		PortMux: func() netx.PortMuxStats {
			return netx.PortMuxStats{TLS: 11, ICE: 2, PlainHTTP: 3, Garbage: 4, Timeout: 5, Limited: 6}
		},
		CertNotAfter: func() time.Time {
			if s := certAt.Load(); s != 0 {
				return time.Unix(s, 0)
			}
			return time.Time{}
		},
	})
	base := metricsListener(t, m)
	if !strings.HasPrefix(base, "http://127.0.0.1:") {
		t.Fatalf("the test listener is at %s, want 127.0.0.1", base)
	}

	// The hub and push register like this, whether metrics are served or not.
	hubConns := prometheus.NewGauge(prometheus.GaugeOpts{Name: "isshoni_test_component_gauge", Help: "A component's own series."})
	if err := m.Registerer().Register(hubConns); err != nil {
		t.Fatalf("Registerer().Register: %v", err)
	}
	hubConns.Set(7)
	if err := m.Registerer().Register(prometheus.NewGauge(prometheus.GaugeOpts{Name: "isshoni_test_component_gauge", Help: "Again."})); err == nil {
		t.Error("registering a name twice succeeded; the second owner must get an error")
	}

	m.ObserveRoute("GET /api/v1/info", 200, 120)
	m.ObserveRoute("GET /api/v1/info", 204, 0)
	m.ObserveRoute("GET /api/v1/info", 404, 30)
	m.ObserveRoute("GET /ws", 101, 0)
	m.ObserveRoute("/", 304, 0)
	m.ObserveRoute("/api/v1/", 500, 60)

	code, header, scrape := httpGet(t, http.MethodGet, base+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics = %d\n%s", code, scrape)
	}
	if ct := header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want the Prometheus text format", ct)
	}

	wantSample(t, scrape, `isshoni_build_info{commit="`+version.Commit()+`",go="`+runtime.Version()+`",version="`+version.Version()+`"}`, 1)
	wantSample(t, scrape, "isshoni_ready", 0) // the db check does not pass yet
	wantSample(t, scrape, `isshoni_transfer_bytes_total{direction="egress",path="media_udp"}`, 1500)
	wantSample(t, scrape, `isshoni_transfer_bytes_total{direction="ingress",path="media_udp"}`, 90)
	wantSample(t, scrape, `isshoni_transfer_bytes_total{direction="egress",path="media_tcp"}`, 700)
	wantSample(t, scrape, `isshoni_transfer_bytes_total{direction="ingress",path="media_tcp"}`, 0)
	wantSample(t, scrape, `isshoni_transfer_bytes_total{direction="egress",path="web"}`, 4096)
	wantSample(t, scrape, `isshoni_transfer_bytes_total{direction="ingress",path="web"}`, 512)
	wantSample(t, scrape, `isshoni_portmux_conns_total{result="tls"}`, 11)
	wantSample(t, scrape, `isshoni_portmux_conns_total{result="ice"}`, 2)
	wantSample(t, scrape, `isshoni_portmux_conns_total{result="plain_http"}`, 3)
	wantSample(t, scrape, `isshoni_portmux_conns_total{result="garbage"}`, 4)
	wantSample(t, scrape, `isshoni_portmux_conns_total{result="timeout"}`, 5)
	wantSample(t, scrape, `isshoni_portmux_conns_total{result="limited"}`, 6)
	wantSample(t, scrape, `isshoni_http_responses_total{class="2xx",route="GET /api/v1/info"}`, 2)
	wantSample(t, scrape, `isshoni_http_responses_total{class="4xx",route="GET /api/v1/info"}`, 1)
	wantSample(t, scrape, `isshoni_http_responses_total{class="1xx",route="GET /ws"}`, 1)
	wantSample(t, scrape, `isshoni_http_responses_total{class="3xx",route="/"}`, 1)
	wantSample(t, scrape, `isshoni_http_responses_total{class="5xx",route="/api/v1/"}`, 1)
	wantSample(t, scrape, "isshoni_test_component_gauge", 7)
	if _, ok := seriesValue(t, scrape, "isshoni_tls_cert_not_after_seconds"); ok {
		t.Error("isshoni_tls_cert_not_after_seconds is exported without a certificate; 0 would read as expired in 1970")
	}

	// The types of 04's table.
	for _, typeLine := range []string{
		"# TYPE isshoni_build_info gauge",
		"# TYPE isshoni_ready gauge",
		"# TYPE isshoni_transfer_bytes_total counter",
		"# TYPE isshoni_portmux_conns_total counter",
		"# TYPE isshoni_http_responses_total counter",
	} {
		if !strings.Contains(scrape, typeLine+"\n") {
			t.Errorf("the scrape has no line %q", typeLine)
		}
	}
	// The Go and process collectors of the private registry.
	if _, ok := seriesValue(t, scrape, "go_goroutines"); !ok {
		t.Error("no go_goroutines: the Go collector is missing")
	}
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		if _, ok := seriesValue(t, scrape, "process_start_time_seconds"); !ok {
			t.Error("no process_start_time_seconds: the process collector is missing")
		}
	}
	// No per-user labels, and nothing that is not ours or the two collectors'.
	for line := range strings.SplitSeq(scrape, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, "isshoni_") && !strings.HasPrefix(line, "go_") && !strings.HasPrefix(line, "process_") {
			t.Errorf("unexpected series: %s", line)
		}
		if strings.HasPrefix(line, "isshoni_") && strings.Contains(line, "user") {
			t.Errorf("a series with a user label: %s", line)
		}
	}

	// The values are read at scrape time: ready, a new certificate, more bytes.
	dbUp.Store(true)
	notAfter := time.Date(2026, 12, 28, 10, 0, 0, 0, time.UTC)
	certAt.Store(notAfter.Unix())
	counter.Add(netx.PathMediaUDP, true, 500)
	_, _, scrape = httpGet(t, http.MethodGet, base+"/metrics")
	wantSample(t, scrape, "isshoni_ready", 1)
	wantSample(t, scrape, "isshoni_tls_cert_not_after_seconds", float64(notAfter.Unix()))
	wantSample(t, scrape, `isshoni_transfer_bytes_total{direction="egress",path="media_udp"}`, 2000)
	if !strings.Contains(scrape, "# TYPE isshoni_tls_cert_not_after_seconds gauge\n") {
		t.Error("isshoni_tls_cert_not_after_seconds is not a gauge")
	}

	// A shutdown turns ready off.
	health.SetShuttingDown()
	_, _, scrape = httpGet(t, http.MethodGet, base+"/metrics")
	wantSample(t, scrape, "isshoni_ready", 0)
}

// Without sources every series of the table is still there, at zero: off mode has no 443 multiplexer and no
// certificate, and the set of series must not depend on the configuration.
func TestMetricsWithoutSources(t *testing.T) {
	m := NewMetrics(MetricsOptions{Health: NewHealth()})
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	scrape := w.Body.String()
	wantSample(t, scrape, "isshoni_ready", 1) // a Health without checks is ready
	for _, path := range []string{"media_udp", "media_tcp", "web"} {
		for _, direction := range []string{"egress", "ingress"} {
			wantSample(t, scrape, `isshoni_transfer_bytes_total{direction="`+direction+`",path="`+path+`"}`, 0)
		}
	}
	for _, result := range []string{"tls", "ice", "plain_http", "garbage", "timeout", "limited"} {
		wantSample(t, scrape, `isshoni_portmux_conns_total{result="`+result+`"}`, 0)
	}
	if _, ok := seriesValue(t, scrape, "isshoni_tls_cert_not_after_seconds"); ok {
		t.Error("isshoni_tls_cert_not_after_seconds is exported without a source")
	}
	// A Metrics that never served shuts down without an error: metrics.enabled = false.
	if err := m.Shutdown(t.Context()); err != nil {
		t.Errorf("Shutdown of a Metrics that never served: %v", err)
	}
}

// The metrics listener also answers /healthz and /readyz (04 §9.1), /debug/pprof/ only with metrics.pprof, and
// nothing else.
func TestMetricsHandlerRoutes(t *testing.T) {
	health := NewHealth()
	health.AddCheck("tls", func() (bool, string) { return false, "waiting: obtaining certificate for 203.0.113.7" })
	plain := NewMetrics(MetricsOptions{Health: health}).Handler()
	withPProf := NewMetrics(MetricsOptions{Health: health, PProf: true}).Handler()

	do := func(h http.Handler, method, path, remoteAddr string) (int, string) {
		t.Helper()
		r := httptest.NewRequestWithContext(t.Context(), method, path, nil)
		r.RemoteAddr = remoteAddr
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}

	if code, body := do(plain, http.MethodGet, "/healthz", loopbackPeer); code != http.StatusOK || body != `{"status":"ok"}`+"\n" {
		t.Errorf("/healthz = %d %q", code, body)
	}
	// A scraper next to the server sees which check holds readiness back; a remote one does not (metrics.listen
	// on another address is allowed, with a warning).
	wantLocal := `{"status":"not_ready","checks":{"tls":"waiting: obtaining certificate for 203.0.113.7"}}` + "\n"
	if code, body := do(plain, http.MethodGet, "/readyz", loopbackPeer); code != http.StatusServiceUnavailable || body != wantLocal {
		t.Errorf("/readyz from loopback = %d %q", code, body)
	}
	if code, body := do(plain, http.MethodGet, "/readyz", publicPeer); code != http.StatusServiceUnavailable || body != `{"status":"not_ready"}`+"\n" {
		t.Errorf("/readyz from elsewhere = %d %q", code, body)
	}

	if code, _ := do(plain, http.MethodHead, "/metrics", loopbackPeer); code != http.StatusOK {
		t.Errorf("HEAD /metrics = %d, want 200", code)
	}
	if code, _ := do(plain, http.MethodPost, "/metrics", loopbackPeer); code != http.StatusMethodNotAllowed {
		t.Errorf("POST /metrics = %d, want 405", code)
	}
	for _, path := range []string{"/", "/metrics/", "/api/v1/info", "/debug/pprof/", "/debug/pprof/heap", "/debug/pprof/cmdline"} {
		if code, _ := do(plain, http.MethodGet, path, loopbackPeer); code != http.StatusNotFound {
			t.Errorf("GET %s without pprof = %d, want 404", path, code)
		}
	}

	if code, body := do(withPProf, http.MethodGet, "/debug/pprof/", loopbackPeer); code != http.StatusOK || !strings.Contains(body, "goroutine") {
		t.Errorf("GET /debug/pprof/ with pprof = %d, want the profile index", code)
	}
	if code, body := do(withPProf, http.MethodGet, "/debug/pprof/goroutine?debug=1", loopbackPeer); code != http.StatusOK || !strings.Contains(body, "goroutine profile:") {
		t.Errorf("GET /debug/pprof/goroutine with pprof = %d", code)
	}
	if code, _ := do(withPProf, http.MethodGet, "/debug/pprof/cmdline", loopbackPeer); code != http.StatusOK {
		t.Errorf("GET /debug/pprof/cmdline with pprof = %d", code)
	}
	if code, _ := do(withPProf, http.MethodGet, "/metrics", loopbackPeer); code != http.StatusOK {
		t.Errorf("GET /metrics with pprof = %d", code)
	}
}

func TestStatusClass(t *testing.T) {
	for status, want := range map[int]string{
		100: "1xx", 101: "1xx", 200: "2xx", 204: "2xx", 299: "2xx", 301: "3xx", 304: "3xx", 400: "4xx", 404: "4xx",
		421: "4xx", 429: "4xx", 499: "4xx", 500: "5xx", 503: "5xx", 599: "5xx", 999: "5xx",
	} {
		if got := statusClass(status); got != want {
			t.Errorf("statusClass(%d) = %q, want %q", status, got, want)
		}
	}
}

// A connection that never sends a request must not hold a shutdown: http.Server.Shutdown alone would wait until
// the connection is 5 s old, the whole budget of the shutdown's HTTP step (04 §6.4).
func TestMetricsShutdownClosesIdleClients(t *testing.T) {
	m := NewMetrics(MetricsOptions{Health: NewHealth()})
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- m.Serve(ln) }()

	var d net.Dialer
	silent, err := d.DialContext(t.Context(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = silent.Close() }()
	waitFor(t, func() bool {
		m.fresh.mu.Lock()
		defer m.fresh.mu.Unlock()
		return len(m.fresh.conns) == 1
	})

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if err := m.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("Shutdown took %v with a silent client connected", took)
	}
	if err := <-served; err != nil {
		t.Errorf("Serve returned %v, want nil", err)
	}
	// The listener is closed: nothing answers any more.
	if c, err := d.DialContext(t.Context(), "tcp", ln.Addr().String()); err == nil {
		_ = c.Close()
		t.Error("the metrics port still accepts connections after Shutdown")
	}
}

func TestNewMetricsNeedsHealth(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewMetrics without Health did not panic")
		}
	}()
	NewMetrics(MetricsOptions{})
}
