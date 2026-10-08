package ops

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
)

// The status values of the health endpoints (04 §11.1). The admin socket's /v1/health and /v1/ready use the same
// ones (04 §12.2).
const (
	StatusOK           = "ok"            // /healthz 200: the process is alive and not stopping
	StatusShuttingDown = "shutting_down" // /healthz and /readyz 503: a graceful shutdown has begun (04 §6.4 step 1)
	StatusReady        = "ready"         // /readyz 200: every check passes
	StatusNotReady     = "not_ready"     // /readyz 503: a check fails, or the server is in maintenance
)

// checkOK is what a passing check shows in the checks object.
const checkOK = "ok"

// maintenanceCheck is the name under which SetMaintenance's reason appears in the checks object; AddCheck refuses
// it.
const maintenanceCheck = "maintenance"

// Health aggregates the server's liveness and readiness (04 §6.2, §11.1). Its state runs one way: starting (a check
// does not pass yet) → ready (every check passes) → shutting down (for good: a restart builds a new server).
//
//   - Liveness (Live, GET /healthz) is true until SetShuttingDown.
//   - Readiness (Ready, GET /readyz) needs every check added with AddCheck to pass, no maintenance reason, and no
//     shutdown. A Health without checks is ready.
//
// The zero value is not usable; build one with NewHealth. All methods are safe for concurrent use.
type Health struct {
	mu           sync.Mutex
	checks       []healthCheck // in AddCheck order
	maintenance  string
	shuttingDown bool
	clientIP     func(*http.Request) netip.Addr
}

type healthCheck struct {
	name string
	fn   func() (ok bool, detail string)
}

// NewHealth returns a Health without checks: alive, and ready until a check says otherwise.
func NewHealth() *Health {
	return &Health{clientIP: peerIP}
}

// AddCheck registers a named readiness check (04 §6.2: db, tls, media, signal, public_ip). fn runs on every Ready
// call and every /readyz request, so it must be quick and must not block; it returns whether the component is ready
// and, when it is not, a short detail for the operator ("waiting: obtaining certificate for 203.0.113.7"). The
// detail is shown only to clients on this machine (see Handlers) and on the admin socket, but keep secrets out of it
// all the same.
//
// It panics on an empty or reserved name ("maintenance"), a nil fn or a name added before: each is a wiring bug.
func (h *Health) AddCheck(name string, fn func() (ok bool, detail string)) {
	if name == "" || name == maintenanceCheck {
		panic("ops: Health.AddCheck: invalid check name " + strconv.Quote(name))
	}
	if fn == nil {
		panic("ops: Health.AddCheck(" + strconv.Quote(name) + "): nil check")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.checks {
		if c.name == name {
			panic("ops: Health.AddCheck(" + strconv.Quote(name) + "): check added twice")
		}
	}
	h.checks = append(h.checks, healthCheck{name: name, fn: fn})
}

// SetMaintenance makes the server not ready for as long as reason is not empty: the checks object then carries
// "maintenance": reason. Liveness is unaffected. An empty reason ends it. M1 has no maintenance mode (a state that a
// restart can't fix exits 78, 04 §6.3); the hook is there for an operation that must keep traffic away for a
// moment, such as a restore that is about to restart the server (04 §12.4).
func (h *Health) SetMaintenance(reason string) {
	h.mu.Lock()
	h.maintenance = reason
	h.mu.Unlock()
}

// SetShuttingDown ends liveness and readiness for good: /healthz and /readyz answer 503 shutting_down from now on.
// It is step 1 of a graceful shutdown (04 §6.4), so monitors and load balancers stop sending. Calling it again does
// nothing.
func (h *Health) SetShuttingDown() {
	h.mu.Lock()
	h.shuttingDown = true
	h.mu.Unlock()
}

// SetClientIP sets how the handlers find a request's client address, which decides whether /readyz may show the
// checks (loopback clients only). The default is the TCP peer. The wiring passes httpapi.ClientIP, so that in off
// mode a request that a trusted proxy on this host forwards counts as its real client, not as loopback (04 §8.5).
// A request with a forwarding header never gets the checks, whatever fn says (see Handlers). A nil fn restores the
// default. Call it before serving.
func (h *Health) SetClientIP(fn func(*http.Request) netip.Addr) {
	if fn == nil {
		fn = peerIP
	}
	h.mu.Lock()
	h.clientIP = fn
	h.mu.Unlock()
}

// Live reports liveness: (true, StatusOK) until SetShuttingDown, then (false, StatusShuttingDown).
func (h *Health) Live() (ok bool, state string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.shuttingDown {
		return false, StatusShuttingDown
	}
	return true, StatusOK
}

// Ready reports readiness and the state of every check by name: "ok", or the check's detail ("not ready" when it
// gave none), plus "maintenance" with its reason while SetMaintenance is in effect. The map is never nil and
// belongs to the caller. ok is false when a check fails, during maintenance and once shutdown has begun (Live then
// tells the two apart).
func (h *Health) Ready() (ok bool, checks map[string]string) {
	status, checks := h.readiness()
	return status == StatusReady, checks
}

// readiness runs the checks and returns the /readyz status with the checks object. The checks run outside the lock:
// they belong to other components.
func (h *Health) readiness() (status string, checks map[string]string) {
	h.mu.Lock()
	list := append([]healthCheck(nil), h.checks...)
	maintenance, shuttingDown := h.maintenance, h.shuttingDown
	h.mu.Unlock()

	ready := true
	checks = make(map[string]string, len(list)+1)
	for _, c := range list {
		ok, detail := c.fn()
		switch {
		case ok:
			checks[c.name] = checkOK
		case detail == "":
			checks[c.name] = "not ready"
			ready = false
		default:
			checks[c.name] = detail
			ready = false
		}
	}
	if maintenance != "" {
		checks[maintenanceCheck] = maintenance
		ready = false
	}
	switch {
	case shuttingDown:
		return StatusShuttingDown, checks
	case ready:
		return StatusReady, checks
	default:
		return StatusNotReady, checks
	}
}

// healthBody is the JSON document of both endpoints (04 §11.1).
type healthBody struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

// Handlers returns the handlers of GET /healthz and GET /readyz (04 §11.1):
//
//	/healthz  200 {"status":"ok"}      503 {"status":"shutting_down"}
//	/readyz   200 {"status":"ready"}   503 {"status":"not_ready"}, or {"status":"shutting_down"} once shutdown began
//
// Public responses carry no detail. A /readyz request from a loopback client (see SetClientIP) also gets
// "checks": {"db":"ok","tls":"waiting: obtaining certificate for 203.0.113.7"}; an operator on the machine, or a
// monitor next to the server, may see which check holds readiness back.
//
// A request that carries a forwarding header (Forwarded, X-Forwarded-For and the like) came through a proxy, so it
// is not one of those and gets no checks, whatever its client address resolves to. That matters in off mode, where
// every request arrives from the proxy on this machine: the address alone says "loopback" when the proxy is not a
// trusted one (network.trusted_proxies = []), or when the X-Forwarded-For it passes on is unusable or names a
// loopback address. Only a proxy that adds no forwarding header at all can't be told from a local client; behind
// such a proxy the public sees the checks.
//
// Both answer GET and HEAD (405 otherwise) with Cache-Control: no-store. The main router and the metrics listener
// mount them; the router exempts both paths from its shutdown gate and its Host check (04 §9.3), so they keep
// answering while everything else gets 503.
func (h *Health) Handlers() (healthz, readyz http.Handler) {
	healthz = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowRead(w, r) {
			return
		}
		ok, state := h.Live()
		writeHealth(w, ok, healthBody{Status: state})
	})
	readyz = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowRead(w, r) {
			return
		}
		status, checks := h.readiness()
		body := healthBody{Status: status}
		if h.fromLoopback(r) {
			body.Checks = checks
		}
		writeHealth(w, status == StatusReady, body)
	})
	return healthz, readyz
}

// forwardingHeaders are the request headers with which a proxy says for whom, and how, it forwards a request.
// Clients on this machine that talk to the server directly (curl, a monitor) send none of them.
var forwardingHeaders = [...]string{
	"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip", "Via",
}

// fromLoopback reports whether the request comes straight from a client on this machine: it has no forwarding
// header (an empty one counts as present) and its client address is a loopback address.
func (h *Health) fromLoopback(r *http.Request) bool {
	for _, name := range forwardingHeaders {
		if _, forwarded := r.Header[name]; forwarded {
			return false
		}
	}
	h.mu.Lock()
	clientIP := h.clientIP
	h.mu.Unlock()
	return clientIP(r).Unmap().IsLoopback()
}

// peerIP is the default client address: the TCP peer. A RemoteAddr that is not an IP address (a unix socket, a test
// pipe) gives the zero Addr, which is not loopback.
func peerIP(r *http.Request) netip.Addr {
	if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		return ap.Addr()
	}
	a, _ := netip.ParseAddr(r.RemoteAddr)
	return a
}

// allowRead answers 405 for every method but GET and HEAD and reports whether the handler may go on.
func allowRead(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", "GET, HEAD")
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "405 method not allowed", http.StatusMethodNotAllowed)
	return false
}

// writeHealth writes body with status 200 when ok and 503 otherwise. net/http drops the body of a HEAD response.
func writeHealth(w http.ResponseWriter, ok bool, body healthBody) {
	var b bytes.Buffer
	_ = json.NewEncoder(&b).Encode(body) // strings and a string map always encode
	status := http.StatusOK
	if !ok {
		status = http.StatusServiceUnavailable
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(b.Len()))
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(b.Bytes()) // a failed write means the client is gone
}
