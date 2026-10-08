package ops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof" // its init also registers on http.DefaultServeMux, which no listener of this server serves
	"runtime"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/MoonWX/isshoni/internal/server/netx"
	"github.com/MoonWX/isshoni/internal/version"
)

// Metrics (04 §11.2). The server always has a Metrics, with a private Prometheus registry: every component
// registers its series through Registerer whether or not anything is scraped. Only with metrics.enabled does the
// wiring bind metrics.listen (127.0.0.1:9469 by default) and serve Handler there; otherwise the registry is one
// that nobody reads. The listener has no authentication, which is why it belongs on loopback (config warns
// otherwise).
//
// One owner per metric. Metrics itself has the ones of 04's table that describe the server as a whole:
//
//	isshoni_build_info{version,commit,go}             gauge, always 1
//	isshoni_ready                                     gauge: 1 while every readiness check passes
//	isshoni_transfer_bytes_total{direction,path}      counter: bytes on the wire (netx.TransferCounter)
//	isshoni_portmux_conns_total{result}               counter: outcomes of the 443 multiplexer (netx.PortMux.Stats)
//	isshoni_http_responses_total{route,class}         counter: responses of the main router (ObserveRoute)
//	isshoni_tls_cert_not_after_seconds                gauge: the certificate's expiry as unix time
//
// plus the Go runtime and process collectors. The others come through Registerer: 01's hub registers the signaling
// and presence series, push registers isshoni_push_sent_total, and the wiring registers a collector over 02's
// SFU.Metrics() for the isshoni_sfu_* names. No series has a per-user label.
//
// The wiring uses it in the steps of 04 §6.1:
//
//	m := ops.NewMetrics(ops.MetricsOptions{Health: health, Transfer: counter, PortMux: mux.Stats, …}) // step 8
//	hub := signal.New(signal.Deps{Metrics: m.Registerer(), …})
//	router := httpapi.NewRouter(httpapi.RouterOptions{Observer: m, …})
//	if cfg.Metrics.Enabled { go func() { err := m.Serve(ln) … }() }                                  // step 9
//	…
//	err = m.Shutdown(ctx)                                                                             // 04 §6.4 step 5

// MetricsOptions configures NewMetrics. Only Health is required; a source that is left out has its series at zero,
// so the set of series does not depend on the configuration.
type MetricsOptions struct {
	// Health backs isshoni_ready, and /healthz and /readyz on the metrics listener. Required.
	Health *Health
	// Transfer is the server's byte counter, read on every scrape for isshoni_transfer_bytes_total. nil counts
	// nothing.
	Transfer *netx.TransferCounter
	// PortMux returns the 443 multiplexer's outcomes ((*netx.PortMux).Stats) for isshoni_portmux_conns_total. nil
	// means there is no multiplexer (tls.mode = off): every result stays 0.
	PortMux func() netx.PortMuxStats
	// CertNotAfter returns the expiry of the certificate the server presents (tlsmgr.Status().NotAfter) for
	// isshoni_tls_cert_not_after_seconds. nil, or a zero time while there is no certificate, leaves the series out:
	// an expiry of 0 would look like a certificate that ran out in 1970.
	CertNotAfter func() time.Time
	// PProf is metrics.pprof: also serve /debug/pprof/ from Handler.
	PProf bool
	// Logger is the server's logger; Metrics adds component=ops. nil means slog.Default().
	Logger *slog.Logger
}

// Metrics is the server's Prometheus registry and the handler of the metrics listener. All methods are safe for
// concurrent use.
type Metrics struct {
	reg       *prometheus.Registry
	responses *prometheus.CounterVec // isshoni_http_responses_total{route,class}
	handler   http.Handler
	log       *slog.Logger

	srv   *http.Server
	fresh freshConns // see freshConns: Shutdown closes them itself
}

// Limits of the metrics listener's HTTP server. There is no write timeout: a CPU profile or a trace from
// /debug/pprof/ runs for as long as its request asks.
const (
	metricsReadHeaderTimeout = 10 * time.Second
	metricsIdleTimeout       = 120 * time.Second
	metricsMaxHeaderBytes    = 16 << 10
	// metricsMaxScrapes is how many scrapes are answered at once; one more gets 503. One Prometheus server, or one
	// agent, sends one at a time.
	metricsMaxScrapes = 4
)

// NewMetrics builds the registry with the Go and process collectors and the series listed on Metrics. It starts
// nothing. A nil o.Health is a wiring bug and panics.
func NewMetrics(o MetricsOptions) *Metrics {
	if o.Health == nil {
		panic("ops: NewMetrics: nil Health")
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With(slog.String("component", "ops"))

	m := &Metrics{
		reg: prometheus.NewRegistry(),
		responses: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "isshoni_http_responses_total", Help: "Responses of the main HTTP router, by route pattern and status class.",
		}, []string{"route", "class"}),
		log: log,
	}
	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "isshoni_build_info", Help: "The running build: always 1, the labels carry the information.",
	}, []string{"version", "commit", "go"})
	buildInfo.WithLabelValues(version.Version(), version.Commit(), runtime.Version()).Set(1)

	// Our own collectors with fixed, distinct names: MustRegister cannot fail on them.
	m.reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		buildInfo,
		m.responses,
		&stateCollector{health: o.Health, transfer: o.Transfer, portMux: o.PortMux, certNotAfter: o.CertNotAfter},
	)

	m.handler = m.newHandler(o.Health, o.PProf)

	var protocols http.Protocols
	protocols.SetHTTP1(true)
	m.srv = &http.Server{
		Handler:           m.handler,
		ReadHeaderTimeout: metricsReadHeaderTimeout,
		IdleTimeout:       metricsIdleTimeout,
		MaxHeaderBytes:    metricsMaxHeaderBytes,
		Protocols:         &protocols,
		ConnState:         m.fresh.track,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
	return m
}

// The router's observer (httpapi.RouteObserver, 04 §9.2). ops may not import httpapi (04 §2), so the method set is
// spelled out here: a change on either side fails to compile in this package or in the wiring.
var _ interface {
	ObserveRoute(pattern string, status int, bytes int64)
} = (*Metrics)(nil)

// Registerer returns the registry for the components that own series (04 §11.2): they register unconditionally,
// with metrics on or off. A name registered twice is the second caller's error.
func (m *Metrics) Registerer() prometheus.Registerer { return m.reg }

// Gatherer returns the registry's read side: what GET /metrics encodes. It is for tests and tools that want the
// series without the listener.
func (m *Metrics) Gatherer() prometheus.Gatherer { return m.reg }

// ObserveRoute counts one response of the main router in isshoni_http_responses_total{route,class}. It implements
// httpapi.RouteObserver (04 §9.3 step 4); the wiring passes the Metrics as RouterOptions.Observer.
//
// pattern is the route's http.ServeMux pattern, never the URL, so the label has as many values as the server has
// routes. class is "2xx" to "5xx", and "1xx" for a WebSocket upgrade (101). bytes is not counted here: the web
// bytes are counted where the provider bills them, at the socket (isshoni_transfer_bytes_total{path="web"}).
func (m *Metrics) ObserveRoute(pattern string, status int, bytes int64) {
	_ = bytes
	m.responses.WithLabelValues(pattern, statusClass(status)).Inc()
}

// statusClass is the class label of an HTTP status. net/http writes no status below 100; one outside 100–599 counts
// with its nearest class.
func statusClass(status int) string {
	switch {
	case status < 200:
		return "1xx"
	case status < 300:
		return "2xx"
	case status < 400:
		return "3xx"
	case status < 500:
		return "4xx"
	default:
		return "5xx"
	}
}

// Handler returns what the metrics listener serves (04 §9.1):
//
//	GET /metrics         the registry in the Prometheus text format
//	GET /healthz         as on the main listener (Health.Handlers)
//	GET /readyz          the same; a client on this machine also gets the checks
//	/debug/pprof/…       net/http/pprof, only with MetricsOptions.PProf
//
// and 404 for everything else. It carries no authentication. Serve serves the same handler; tests drive it with
// httptest.
func (m *Metrics) Handler() http.Handler { return m.handler }

func (m *Metrics) newHandler(health *Health, withPProf bool) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{
		ErrorLog:            scrapeLog{m.log},
		ErrorHandling:       promhttp.ContinueOnError, // one failing collector must not hide the other series
		MaxRequestsInFlight: metricsMaxScrapes,
	}))
	healthz, readyz := health.Handlers()
	mux.Handle("/healthz", healthz)
	mux.Handle("/readyz", readyz)
	if withPProf {
		mux.HandleFunc("/debug/pprof/", pprof.Index) // also the named profiles: heap, goroutine, allocs, …
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}
	return mux
}

// Serve answers requests on ln, the listener bound to metrics.listen, until Shutdown; it then returns nil. Any
// other return value is the listener's error. The caller runs it in a goroutine of its own and waits for it.
func (m *Metrics) Serve(ln net.Listener) error {
	if err := m.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("ops: serving metrics on %s: %w", ln.Addr(), err)
	}
	return nil
}

// Shutdown stops the metrics listener (04 §6.4 step 5): the listener closes, and requests in flight (a scrape, a
// profile) may finish until ctx ends. What is left then is closed by force, and Shutdown returns an error that
// wraps ctx's. It is harmless on a Metrics that never served.
func (m *Metrics) Shutdown(ctx context.Context) error {
	m.fresh.closeAll()
	if err := m.srv.Shutdown(ctx); err != nil {
		_ = m.srv.Close()
		if ctx.Err() != nil {
			return fmt.Errorf("metrics listener: requests still running were cut off: %w", err)
		}
		return fmt.Errorf("ops: closing the metrics listener: %w", err)
	}
	return nil
}

// scrapeLog is promhttp's error log: a collector that fails during a scrape gets one line.
type scrapeLog struct{ log *slog.Logger }

func (l scrapeLog) Println(v ...any) {
	l.log.Warn("metrics scrape: " + fmt.Sprint(v...))
}

// The label values of the collected series. They are part of the metric contract (04 §11.2): never renamed.
const (
	directionEgress  = "egress"
	directionIngress = "ingress"
)

// transferPaths is the order in which the paths are exported.
var transferPaths = [...]netx.Path{netx.PathMediaUDP, netx.PathMediaTCP, netx.PathWeb}

var (
	readyDesc = prometheus.NewDesc("isshoni_ready",
		"1 while every readiness check passes, 0 otherwise and during a shutdown.", nil, nil)
	transferDesc = prometheus.NewDesc("isshoni_transfer_bytes_total",
		"Bytes on the wire since the server started, counted at the sockets.", []string{"direction", "path"}, nil)
	portMuxDesc = prometheus.NewDesc("isshoni_portmux_conns_total",
		"Connections accepted on the HTTPS port, by what the first byte made of them.", []string{"result"}, nil)
	certNotAfterDesc = prometheus.NewDesc("isshoni_tls_cert_not_after_seconds",
		"Expiry of the TLS certificate in seconds since the Unix epoch; absent while there is none.", nil, nil)
)

// stateCollector reads the server's state on every scrape: the values live in Health, netx and tlsmgr, so there
// is nothing to keep in step.
type stateCollector struct {
	health       *Health
	transfer     *netx.TransferCounter
	portMux      func() netx.PortMuxStats
	certNotAfter func() time.Time
}

func (c *stateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- readyDesc
	ch <- transferDesc
	ch <- portMuxDesc
	ch <- certNotAfterDesc
}

func (c *stateCollector) Collect(ch chan<- prometheus.Metric) {
	ready := 0.0
	if ok, _ := c.health.Ready(); ok {
		ready = 1
	}
	ch <- prometheus.MustNewConstMetric(readyDesc, prometheus.GaugeValue, ready)

	totals := c.transfer.Totals() // a nil counter has every path at zero
	for _, p := range transferPaths {
		t := totals[p]
		ch <- prometheus.MustNewConstMetric(transferDesc, prometheus.CounterValue, float64(t.Egress), directionEgress, string(p))
		ch <- prometheus.MustNewConstMetric(transferDesc, prometheus.CounterValue, float64(t.Ingress), directionIngress, string(p))
	}

	var mux netx.PortMuxStats
	if c.portMux != nil {
		mux = c.portMux()
	}
	for _, r := range [...]struct {
		result string
		n      uint64
	}{
		{"tls", mux.TLS}, {"ice", mux.ICE}, {"plain_http", mux.PlainHTTP},
		{"garbage", mux.Garbage}, {"timeout", mux.Timeout}, {"limited", mux.Limited},
	} {
		ch <- prometheus.MustNewConstMetric(portMuxDesc, prometheus.CounterValue, float64(r.n), r.result)
	}

	if c.certNotAfter != nil {
		if t := c.certNotAfter(); !t.IsZero() {
			ch <- prometheus.MustNewConstMetric(certNotAfterDesc, prometheus.GaugeValue, float64(t.Unix()))
		}
	}
}
