package httpapi

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
)

var requestIDPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// TestChainOrder pins the global chain to 04 §9.3, outermost first. The behavior tests below show the orderings that
// are observable from outside.
func TestChainOrder(t *testing.T) {
	rt := NewRouter(RouterOptions{})
	var got []string
	for _, m := range rt.chain() {
		got = append(got, m.name)
	}
	want := []string{"recover", "request_id", "read_deadline", "transfer", "gate", "host_check", "real_ip", "security_headers"}
	if !slices.Equal(got, want) {
		t.Fatalf("chain = %v, want %v (04 §9.3)", got, want)
	}
}

// TestChainOrderObservable checks the orderings of 04 §9.3 that a client or the observer can see.
func TestChainOrderObservable(t *testing.T) {
	t.Run("recover outside request ID: a panic's 500 carries the request ID", func(t *testing.T) {
		f := newFixture(t, nil)
		f.rt.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) { panic("boom") })
		rec := f.get("/boom")
		e := wantError(t, rec, http.StatusInternalServerError, api.CodeInternal)
		if id := rec.Header().Get("X-Request-Id"); !requestIDPattern.MatchString(id) || e.RequestID != id {
			t.Fatalf("X-Request-Id %q, requestId %q: want the same 16 hex characters", id, e.RequestID)
		}
	})
	t.Run("transfer outside gate: the observer sees the gate's 503", func(t *testing.T) {
		f := newFixture(t, nil)
		f.gate.SetShuttingDown()
		f.get("/r/lounge")
		if got := f.obs.all(); len(got) != 1 || got[0].status != http.StatusServiceUnavailable || got[0].pattern != "/" {
			t.Fatalf("observations = %+v, want one 503 for pattern /", got)
		}
	})
	t.Run("gate before Host check: shutting down wins over a wrong Host", func(t *testing.T) {
		f := newFixture(t, nil)
		f.gate.SetShuttingDown()
		req := newReq(http.MethodGet, "/")
		req.Host = "evil.example"
		wantError(t, f.serve(req), http.StatusServiceUnavailable, api.CodeServerShutdown)
	})
	t.Run("Host check before real IP and headers: 421 never reaches the mux", func(t *testing.T) {
		called := false
		f := newFixture(t, nil)
		f.rt.HandleFunc("GET /probe", func(http.ResponseWriter, *http.Request) { called = true })
		req := newReq(http.MethodGet, "/probe")
		req.Host = "evil.example"
		if rec := f.serve(req); rec.Code != http.StatusMisdirectedRequest || called {
			t.Fatalf("status %d, handler called %v; want 421 and no call", rec.Code, called)
		}
	})
	t.Run("real IP and security headers before the handler", func(t *testing.T) {
		f := newFixture(t, func(o *RouterOptions) { o.Site.TLSMode = "off" })
		var csp, ip string
		f.rt.HandleFunc("GET /probe", func(w http.ResponseWriter, r *http.Request) {
			csp, ip = w.Header().Get("Content-Security-Policy"), ClientIP(r).String()
		})
		f.get("/probe")
		if csp != cspOther || ip != "192.0.2.1" {
			t.Fatalf("handler saw CSP %q and client IP %q", csp, ip)
		}
	})
}

func TestRequestID(t *testing.T) {
	f := newFixture(t, nil)
	var seen string
	f.rt.HandleFunc("GET /id", func(_ http.ResponseWriter, r *http.Request) { seen = RequestID(r) })
	ids := map[string]bool{}
	for range 50 {
		rec := f.get("/id")
		id := rec.Header().Get("X-Request-Id")
		if !requestIDPattern.MatchString(id) || id != seen {
			t.Fatalf("X-Request-Id %q, RequestID(r) %q", id, seen)
		}
		ids[id] = true
	}
	if len(ids) != 50 {
		t.Fatalf("%d distinct IDs in 50 requests", len(ids))
	}
	// An incoming X-Request-Id is not trusted.
	if rec := f.get("/id", "X-Request-Id", "attacker-chosen"); rec.Header().Get("X-Request-Id") == "attacker-chosen" {
		t.Fatal("the client's X-Request-Id was reused")
	}
	if got := RequestID(httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)); got != "" {
		t.Fatalf("RequestID outside the router = %q, want empty", got)
	}
}

func TestRecover(t *testing.T) {
	f := newFixture(t, nil)
	f.rt.HandleFunc("GET /boom", func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "half-made", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
		w.Header().Set("Content-Type", "text/html")
		panic("handler bug")
	})
	rec := f.get("/boom?token=s3cret-query")
	wantError(t, rec, http.StatusInternalServerError, api.CodeInternal)
	if c := rec.Header().Get("Set-Cookie"); c != "" {
		t.Errorf("Set-Cookie %q survived the panic", c)
	}
	if got := rec.Header().Get("Content-Security-Policy"); got != cspOther {
		t.Errorf("CSP = %q", got)
	}
	logs := f.logs.records(t)
	if len(logs) != 1 {
		t.Fatalf("want exactly one log line for the panic, got %d:\n%s", len(logs), f.logs)
	}
	l := logs[0]
	if l["level"] != "ERROR" || l["route"] != "GET /boom" || l["request_id"] != rec.Header().Get("X-Request-Id") ||
		l["panic"] != "handler bug" || l["component"] != "http" {
		t.Errorf("panic log = %v", l)
	}
	if stack, _ := l["stack"].(string); !strings.Contains(stack, "TestRecover") {
		t.Errorf("the stack does not show the panicking handler:\n%s", stack)
	}
	if strings.Contains(f.logs.String(), "s3cret-query") || strings.Contains(f.logs.String(), "/boom?") {
		t.Errorf("the URL or query was logged:\n%s", f.logs)
	}
	if got := f.obs.all(); len(got) != 1 || got[0] != (observation{"GET /boom", 500, 0}) {
		t.Errorf("observations = %+v, want one 500 for GET /boom", got)
	}
}

func TestRecoverAfterHeaderAborts(t *testing.T) {
	f := newFixture(t, nil)
	f.rt.HandleFunc("GET /half", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "partial")
		panic("late bug")
	})
	f.rt.HandleFunc("GET /abort", func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })
	for _, path := range []string{"/half", "/abort"} {
		func() {
			defer func() {
				if p := recover(); p != http.ErrAbortHandler { //nolint:errorlint // the sentinel value itself
					t.Errorf("%s: panic value %v, want http.ErrAbortHandler (net/http then breaks the connection)", path, p)
				}
			}()
			f.get(path)
		}()
	}
	if logs := f.logs.records(t); len(logs) != 1 || logs[0]["panic"] != "late bug" {
		t.Errorf("want one log line for the late panic and none for ErrAbortHandler:\n%s", f.logs)
	}
}

func TestGate(t *testing.T) {
	var nilGate *Gate
	if nilGate.ShuttingDown() {
		t.Fatal("a nil gate is shutting down")
	}
	f := newFixture(t, nil)
	f.rt.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	f.rt.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	if rec := f.get("/api/v1/me"); rec.Code != http.StatusNotFound {
		t.Fatalf("serving: status %d", rec.Code)
	}
	f.gate.SetShuttingDown()
	if !f.gate.ShuttingDown() {
		t.Fatal("ShuttingDown() = false after SetShuttingDown")
	}
	for _, path := range []string{"/", "/api/v1/me", "/ws", "/assets/" + strings.TrimPrefix(testJS, "assets/")} {
		rec := f.get(path)
		e := wantError(t, rec, http.StatusServiceUnavailable, api.CodeServerShutdown)
		if e.RetryAfter != 5 || rec.Header().Get("Retry-After") != "5" {
			t.Errorf("%s: retryAfter %d, Retry-After %q; want 5", path, e.RetryAfter, rec.Header().Get("Retry-After"))
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("X-Robots-Tag") != "noindex" {
			t.Errorf("%s: the 503 misses the headers every response carries: %v", path, rec.Header())
		}
	}
	// The health endpoints stay reachable, also with a wrong Host (monitors use IPs).
	for _, path := range []string{"/healthz", "/readyz"} {
		req := newReq(http.MethodGet, path)
		req.Host = "10.0.0.5:443"
		if rec := f.serve(req); rec.Code != http.StatusTeapot {
			t.Errorf("%s while shutting down: status %d, want the handler's 418", path, rec.Code)
		}
	}
	// One warn line per 503, with route, status and request ID.
	logs := f.logs.records(t)
	if len(logs) != 4 {
		t.Fatalf("want 4 log lines, got:\n%s", f.logs)
	}
	for _, l := range logs {
		if l["level"] != "WARN" || l["status"] != float64(503) || l["route"] == nil || l["request_id"] == "" {
			t.Errorf("5xx log line = %v", l)
		}
	}
}

func TestHostCheck(t *testing.T) {
	ipSite := Site{Origin: "https://203.0.113.7", Host: "203.0.113.7", Hostname: "203.0.113.7", TLSMode: "ip"}
	v6Site := Site{Origin: "https://[2001:db8::1]:8443", Host: "[2001:db8::1]:8443", Hostname: "2001:db8::1", TLSMode: "ip"}
	proxied := Site{Origin: "http://friends.example.net", Host: "friends.example.net", Hostname: "friends.example.net", TLSMode: "off"}
	tests := []struct {
		site Site
		host string
		ok   bool
	}{
		{domainSite(), testHost, true},
		{domainSite(), "WATCH.Example.COM", true},
		{domainSite(), testHost + ":443", true},
		{domainSite(), testHost + ":8443", false},
		{domainSite(), "evil.example", false},
		{domainSite(), "203.0.113.7", false},
		{domainSite(), "localhost", false},
		{domainSite(), "", false},
		{domainSite(), testHost + ".", false},
		{ipSite, "203.0.113.7", true},
		{ipSite, "203.0.113.7:443", true},
		{ipSite, testHost, false},
		{v6Site, "[2001:db8::1]:8443", true},
		{v6Site, "[2001:DB8::1]:8443", true},
		{v6Site, "[2001:db8::1]", false},
		{proxied, "friends.example.net", true},
		{proxied, "friends.example.net:80", true},
		{proxied, "127.0.0.1:8080", false},
		{devSite(), "localhost:8080", true},
		{devSite(), "localhost:5173", true},
		{devSite(), "localhost", true},
		{devSite(), "LOCALHOST:3000", true},
		{devSite(), "127.0.0.1:8080", true},
		{devSite(), "[::1]:8080", true},
		{devSite(), "localhost.evil.example", false},
		{devSite(), "127.0.0.2:8080", false},
		{devSite(), "192.168.1.10:8080", false},
		{Site{}, "anything", false},
	}
	for _, tt := range tests {
		f := newFixture(t, func(o *RouterOptions) { o.Site = tt.site })
		req := newReq(http.MethodGet, "/")
		req.Host = tt.host
		rec := f.serve(req)
		if got := rec.Code != http.StatusMisdirectedRequest; got != tt.ok {
			t.Errorf("site %q, Host %q: status %d, want allowed=%v", tt.site.Host, tt.host, rec.Code, tt.ok)
			continue
		}
		if !tt.ok {
			if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
				t.Errorf("421 Content-Type = %q, want plain text", ct)
			}
			if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Cache-Control") != cacheNoStore {
				t.Errorf("421 misses the base headers: %v", rec.Header())
			}
		}
	}
}

// TestMounts checks the reserved paths of 04 §9.2 and §9.5.
func TestMounts(t *testing.T) {
	var apiPath, wsPath string
	f := newFixture(t, func(o *RouterOptions) {
		o.API = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apiPath = r.URL.Path
			WriteJSON(w, http.StatusOK, map[string]string{"ok": "api"})
		})
		o.WS = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			wsPath = r.URL.Path
			w.WriteHeader(http.StatusSwitchingProtocols)
		})
	})
	if rec := f.get("/api/v1/auth/login"); rec.Code != http.StatusOK || apiPath != "/api/v1/auth/login" {
		t.Errorf("/api/v1/auth/login: status %d, API saw %q", rec.Code, apiPath)
	}
	if rec := f.serve(newReq(http.MethodPost, "/api/v1/rooms")); rec.Code != http.StatusOK || apiPath != "/api/v1/rooms" {
		t.Errorf("POST /api/v1/rooms: status %d, API saw %q", rec.Code, apiPath)
	}
	if rec := f.get("/ws"); rec.Code != http.StatusSwitchingProtocols || wsPath != "/ws" {
		t.Errorf("GET /ws: status %d, hub saw %q", rec.Code, wsPath)
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/"},
		{http.MethodGet, "/api/v2/info"},
		{http.MethodPost, "/api/anything"},
		{http.MethodPost, "/ws"},
		{http.MethodGet, "/healthz"},
		{http.MethodGet, "/readyz"},
	} {
		wantError(t, f.serve(newReq(tc.method, tc.path)), http.StatusNotFound, api.CodeNotFound)
	}

	// Without API and WS (tests, and before the wiring), the mounts answer JSON 404 too.
	bare := newFixture(t, nil)
	for _, path := range []string{"/api/v1/info", "/ws"} {
		wantError(t, bare.get(path), http.StatusNotFound, api.CodeNotFound)
	}

	// Routes added with Handle take precedence over the reserved fallbacks.
	bare.rt.Handle("GET /healthz", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}))
	if rec := bare.get("/healthz"); rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != cacheNoStore {
		t.Errorf("/healthz: status %d, Cache-Control %q; want 200 and no-store", rec.Code, rec.Header().Get("Cache-Control"))
	}
	wantError(t, bare.serve(newReq(http.MethodPost, "/healthz")), http.StatusNotFound, api.CodeNotFound)
}

func TestTransferObserver(t *testing.T) {
	f := newFixture(t, func(o *RouterOptions) {
		o.API = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { WriteJSON(w, http.StatusCreated, []int{1}) })
		o.WS = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusSwitchingProtocols) })
	})
	f.rt.HandleFunc("GET /healthz", func(http.ResponseWriter, *http.Request) {}) // writes nothing: 200
	f.get("/" + testJS)
	f.get("/r/lounge")
	f.get("/api/v1/rooms")
	f.get("/ws")
	f.get("/healthz")
	req := newReq(http.MethodGet, "/")
	req.Host = "evil.example"
	f.serve(req)
	want := []observation{
		{"/", 200, int64(len("console.log('app')\n"))},
		{"/", 200, int64(len(indexHTML))},
		{"/api/v1/", 201, int64(len("[1]\n"))},
		{"GET /ws", 101, 0},
		{"GET /healthz", 200, 0},
		{"/", 421, int64(len("421 misdirected request\n"))},
	}
	if got := f.obs.all(); !slices.Equal(got, want) {
		t.Fatalf("observations:\n got %+v\nwant %+v", got, want)
	}
	if logs := f.logs.records(t); len(logs) != 0 {
		t.Errorf("no access log: got\n%s", f.logs)
	}
}

// TestReadDeadline runs a real server: a trickled body is cut at the read deadline (04 §9.3 step 3, shortened
// here), while /ws clears it, so the hub's request context survives a slow upgrade and the socket reads afterwards.
func TestReadDeadline(t *testing.T) {
	type result struct {
		ctxErr error
		line   string
		err    error
	}
	results := make(chan result, 2)
	upgrade := func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond) // past the deadline, before the upgrade
		ctxErr := r.Context().Err()
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			results <- result{ctxErr, "", err}
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: test\r\nConnection: Upgrade\r\n\r\n")
		_ = brw.Flush()
		line, err := brw.ReadString('\n')
		results <- result{ctxErr, line, err}
	}
	f := newFixture(t, func(o *RouterOptions) {
		o.Site = devSite()
		o.WS = http.HandlerFunc(upgrade)
	})
	f.rt.readTimeout = 150 * time.Millisecond
	f.rt.HandleFunc("GET /not-ws", upgrade)
	bodyErr := make(chan error, 1)
	f.rt.HandleFunc("POST /upload", func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		bodyErr <- err
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(f.rt.Handler())
	defer srv.Close()

	t.Run("trickled body is cut", func(t *testing.T) {
		conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		_, _ = fmt.Fprint(conn, "POST /upload HTTP/1.1\r\nHost: localhost\r\nContent-Length: 1000\r\n\r\n")
		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() {
			tick := time.NewTicker(40 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-stop:
					return
				case <-tick.C:
					_, _ = conn.Write([]byte("x"))
				}
			}
		})
		defer func() { close(stop); wg.Wait() }()
		select {
		case err := <-bodyErr:
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("body read error = %v, want the read deadline", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the trickled body was never cut")
		}
	})

	exchange := func(t *testing.T, path string) result {
		t.Helper()
		conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		_, _ = fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: localhost\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n", path)
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		time.Sleep(300 * time.Millisecond) // the socket idles past the deadline again
		_, _ = io.WriteString(conn, "hello\n")
		select {
		case res := <-results:
			return res
		case <-time.After(5 * time.Second):
			t.Fatal("no result from the upgrade handler")
			return result{}
		}
	}
	t.Run("/ws has no deadline", func(t *testing.T) {
		res := exchange(t, "/ws")
		if res.ctxErr != nil || res.err != nil || res.line != "hello\n" {
			t.Fatalf("ctx err %v, read %q, err %v; want a live context and the client's line", res.ctxErr, res.line, res.err)
		}
	})
	t.Run("other routes keep it", func(t *testing.T) {
		// Without the /ws mount's clear, net/http's background read hits the deadline and cancels the request.
		if res := exchange(t, "/not-ws"); res.ctxErr == nil {
			t.Fatal("the request context of a slow non-/ws route survived the read deadline")
		}
	})
}
