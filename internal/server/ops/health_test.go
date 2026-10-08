package ops

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

// get runs one request against h and returns the status, the header and the body.
func get(t *testing.T, h http.Handler, method, remoteAddr string) (int, http.Header, string) {
	t.Helper()
	r := httptest.NewRequestWithContext(context.Background(), method, "/", nil)
	r.RemoteAddr = remoteAddr
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	res := w.Result()
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, res.Header, string(b)
}

const (
	publicPeer   = "203.0.113.9:40000"
	loopbackPeer = "127.0.0.1:40000"
)

// TestHealthStateMachine walks starting → ready → shutting_down (04 §17) and checks both endpoints' public
// documents at each state (04 §11.1).
func TestHealthStateMachine(t *testing.T) {
	h := NewHealth()
	var certReady atomic.Bool
	h.AddCheck("db", func() (bool, string) { return true, "" })
	h.AddCheck("tls", func() (bool, string) {
		if certReady.Load() {
			return true, "ignored when ok"
		}
		return false, "waiting: obtaining certificate for 203.0.113.7"
	})
	healthz, readyz := h.Handlers()

	assert := func(state string, wantLive int, wantLiveBody string, wantReady int, wantReadyBody string) {
		t.Helper()
		if code, _, body := get(t, healthz, http.MethodGet, publicPeer); code != wantLive || body != wantLiveBody {
			t.Errorf("%s: /healthz = %d %q, want %d %q", state, code, body, wantLive, wantLiveBody)
		}
		if code, _, body := get(t, readyz, http.MethodGet, publicPeer); code != wantReady || body != wantReadyBody {
			t.Errorf("%s: /readyz = %d %q, want %d %q", state, code, body, wantReady, wantReadyBody)
		}
	}

	// Starting: alive, not ready.
	assert("starting", 200, `{"status":"ok"}`+"\n", 503, `{"status":"not_ready"}`+"\n")
	if ok, state := h.Live(); !ok || state != StatusOK {
		t.Errorf("starting: Live() = %v, %q", ok, state)
	}
	ok, checks := h.Ready()
	want := map[string]string{"db": "ok", "tls": "waiting: obtaining certificate for 203.0.113.7"}
	if ok || !reflect.DeepEqual(checks, want) {
		t.Errorf("starting: Ready() = %v, %v, want false, %v", ok, checks, want)
	}

	// Ready.
	certReady.Store(true)
	assert("ready", 200, `{"status":"ok"}`+"\n", 200, `{"status":"ready"}`+"\n")
	ok, checks = h.Ready()
	if want := map[string]string{"db": "ok", "tls": "ok"}; !ok || !reflect.DeepEqual(checks, want) {
		t.Errorf("ready: Ready() = %v, %v, want true, %v", ok, checks, want)
	}

	// Shutting down, for good: passing checks no longer make it ready.
	h.SetShuttingDown()
	h.SetShuttingDown()
	assert("shutting down", 503, `{"status":"shutting_down"}`+"\n", 503, `{"status":"shutting_down"}`+"\n")
	if ok, state := h.Live(); ok || state != StatusShuttingDown {
		t.Errorf("shutting down: Live() = %v, %q", ok, state)
	}
	if ok, _ := h.Ready(); ok {
		t.Error("shutting down: Ready() = true")
	}
}

func TestHealthWithoutChecksIsReady(t *testing.T) {
	h := NewHealth()
	ok, checks := h.Ready()
	if !ok || checks == nil || len(checks) != 0 {
		t.Errorf("Ready() = %v, %#v, want true and an empty, non-nil map", ok, checks)
	}
	_, readyz := h.Handlers()
	// No checks, so even a loopback client sees no checks object.
	if code, _, body := get(t, readyz, http.MethodGet, loopbackPeer); code != 200 || body != `{"status":"ready"}`+"\n" {
		t.Errorf("/readyz = %d %q", code, body)
	}
}

// TestReadyzDetailOnlyForLoopback: public responses carry no detail; loopback clients get the checks (04 §11.1).
func TestReadyzDetailOnlyForLoopback(t *testing.T) {
	h := NewHealth()
	h.AddCheck("db", func() (bool, string) { return true, "" })
	h.AddCheck("media", func() (bool, string) { return false, "" })
	healthz, readyz := h.Handlers()

	const (
		bare     = `{"status":"not_ready"}` + "\n"
		detailed = `{"status":"not_ready","checks":{"db":"ok","media":"not ready"}}` + "\n"
	)
	for _, tc := range []struct {
		remote string
		want   string
	}{
		{publicPeer, bare},
		{"[2001:db8::1]:40000", bare},
		{"192.168.1.20:40000", bare}, // a LAN client is not the operator
		{"@", bare},                  // not an IP address (a unix socket): no detail by default
		{"", bare},
		{loopbackPeer, detailed},
		{"127.8.9.10:1", detailed},
		{"[::1]:40000", detailed},
		{"[::ffff:127.0.0.1]:40000", detailed}, // IPv4-mapped loopback
		{"127.0.0.1", detailed},                // no port
	} {
		if code, _, body := get(t, readyz, http.MethodGet, tc.remote); code != 503 || body != tc.want {
			t.Errorf("peer %q: /readyz = %d %q, want 503 %q", tc.remote, code, body, tc.want)
		}
	}
	// Liveness has no checks, whoever asks.
	if code, _, body := get(t, healthz, http.MethodGet, loopbackPeer); code != 200 || body != `{"status":"ok"}`+"\n" {
		t.Errorf("/healthz from loopback = %d %q", code, body)
	}

	// The checks stay visible to loopback clients during shutdown, under the shutting_down status.
	h.SetShuttingDown()
	want := `{"status":"shutting_down","checks":{"db":"ok","media":"not ready"}}` + "\n"
	if code, _, body := get(t, readyz, http.MethodGet, loopbackPeer); code != 503 || body != want {
		t.Errorf("shutting down: /readyz from loopback = %d %q, want 503 %q", code, body, want)
	}
}

// TestSetClientIP: the wiring's resolver decides who is local, so a request forwarded by a proxy on this host is
// judged by its real client (04 §8.5).
func TestSetClientIP(t *testing.T) {
	h := NewHealth()
	h.AddCheck("db", func() (bool, string) { return true, "" })
	_, readyz := h.Handlers()
	const (
		bare     = `{"status":"ready"}` + "\n"
		detailed = `{"status":"ready","checks":{"db":"ok"}}` + "\n"
	)

	// A resolver that reads a header, standing in for httpapi.ClientIP behind a trusted proxy.
	h.SetClientIP(func(r *http.Request) netip.Addr {
		a, _ := netip.ParseAddr(r.Header.Get("X-Test-Client"))
		return a
	})
	req := func(client string) string {
		r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", nil)
		r.RemoteAddr = loopbackPeer // the proxy
		r.Header.Set("X-Test-Client", client)
		w := httptest.NewRecorder()
		readyz.ServeHTTP(w, r)
		return w.Body.String()
	}
	if got := req("203.0.113.9"); got != bare {
		t.Errorf("forwarded public client: %q, want %q", got, bare)
	}
	if got := req("127.0.0.1"); got != detailed {
		t.Errorf("forwarded loopback client: %q, want %q", got, detailed)
	}
	if got := req("not an address"); got != bare {
		t.Errorf("unknown client: %q, want %q", got, bare)
	}

	// nil restores the TCP peer.
	h.SetClientIP(nil)
	if got := req("203.0.113.9"); got != detailed {
		t.Errorf("default resolver with a loopback peer: %q, want %q", got, detailed)
	}
}

func TestHealthHandlersHeadersAndMethods(t *testing.T) {
	h := NewHealth()
	healthz, readyz := h.Handlers()
	for name, handler := range map[string]http.Handler{"healthz": healthz, "readyz": readyz} {
		code, hdr, body := get(t, handler, http.MethodGet, publicPeer)
		if code != 200 {
			t.Errorf("%s: GET = %d", name, code)
		}
		if got := hdr.Get("Content-Type"); got != "application/json; charset=utf-8" {
			t.Errorf("%s: Content-Type = %q", name, got)
		}
		if got := hdr.Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", name, got)
		}
		if got := hdr.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q", name, got)
		}
		if got := hdr.Get("Content-Length"); got != strconv.Itoa(len(body)) {
			t.Errorf("%s: Content-Length = %q for a %d-byte body", name, got, len(body))
		}

		// HEAD answers like GET. (A real server drops the body; the recorder keeps it.)
		if code, hdr, _ := get(t, handler, http.MethodHead, publicPeer); code != 200 || hdr.Get("Cache-Control") != "no-store" {
			t.Errorf("%s: HEAD = %d, Cache-Control %q", name, code, hdr.Get("Cache-Control"))
		}

		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions} {
			code, hdr, _ := get(t, handler, method, loopbackPeer)
			if code != http.StatusMethodNotAllowed {
				t.Errorf("%s: %s = %d, want 405", name, method, code)
			}
			if got := hdr.Get("Allow"); got != "GET, HEAD" {
				t.Errorf("%s: %s Allow = %q", name, method, got)
			}
			if got := hdr.Get("Cache-Control"); got != "no-store" {
				t.Errorf("%s: %s Cache-Control = %q, want no-store", name, method, got)
			}
		}
	}
}

// TestMaintenance: a maintenance reason makes the server not ready without touching liveness, and ends with "".
func TestMaintenance(t *testing.T) {
	h := NewHealth()
	h.AddCheck("db", func() (bool, string) { return true, "" })
	healthz, readyz := h.Handlers()

	h.SetMaintenance("restoring a backup")
	ok, checks := h.Ready()
	if want := map[string]string{"db": "ok", "maintenance": "restoring a backup"}; ok || !reflect.DeepEqual(checks, want) {
		t.Errorf("Ready() = %v, %v, want false, %v", ok, checks, want)
	}
	if ok, state := h.Live(); !ok || state != StatusOK {
		t.Errorf("Live() = %v, %q during maintenance", ok, state)
	}
	if code, _, body := get(t, healthz, http.MethodGet, publicPeer); code != 200 || body != `{"status":"ok"}`+"\n" {
		t.Errorf("/healthz = %d %q", code, body)
	}
	if code, _, body := get(t, readyz, http.MethodGet, publicPeer); code != 503 || body != `{"status":"not_ready"}`+"\n" {
		t.Errorf("/readyz = %d %q", code, body)
	}
	want := `{"status":"not_ready","checks":{"db":"ok","maintenance":"restoring a backup"}}` + "\n"
	if code, _, body := get(t, readyz, http.MethodGet, loopbackPeer); code != 503 || body != want {
		t.Errorf("/readyz from loopback = %d %q, want 503 %q", code, body, want)
	}

	h.SetMaintenance("")
	if ok, checks := h.Ready(); !ok || len(checks) != 1 {
		t.Errorf("after maintenance: Ready() = %v, %v", ok, checks)
	}
}

func TestAddCheckPanics(t *testing.T) {
	pass := func() (bool, string) { return true, "" }
	for name, add := range map[string]func(h *Health){
		"empty name":    func(h *Health) { h.AddCheck("", pass) },
		"reserved name": func(h *Health) { h.AddCheck("maintenance", pass) },
		"nil check":     func(h *Health) { h.AddCheck("db", nil) },
		"added twice":   func(h *Health) { h.AddCheck("db", pass); h.AddCheck("db", pass) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("AddCheck did not panic")
				}
			}()
			add(NewHealth())
		})
	}
}

// TestReadyMapBelongsToCaller: changing the returned map does not change what the next caller sees.
func TestReadyMapBelongsToCaller(t *testing.T) {
	h := NewHealth()
	h.AddCheck("db", func() (bool, string) { return true, "" })
	_, checks := h.Ready()
	checks["db"] = "tampered"
	delete(checks, "db")
	if _, again := h.Ready(); again["db"] != "ok" {
		t.Errorf("second Ready() = %v", again)
	}
}

// TestCheckMayCallHealth: checks run outside the Health's lock, so a check that reads the Health (or a component
// that does) can't deadlock.
func TestCheckMayCallHealth(t *testing.T) {
	h := NewHealth()
	h.AddCheck("self", func() (bool, string) {
		ok, _ := h.Live()
		return ok, "stopping"
	})
	if ok, _ := h.Ready(); !ok {
		t.Error("Ready() = false")
	}
}

// TestHealthConcurrent runs every method at once; the race detector is the assertion.
func TestHealthConcurrent(t *testing.T) {
	h := NewHealth()
	var flip atomic.Bool
	h.AddCheck("db", func() (bool, string) { return flip.Load(), "waiting" })
	healthz, readyz := h.Handlers()

	serve := func(handler http.Handler, remoteAddr string) {
		r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
		r.RemoteAddr = remoteAddr
		handler.ServeHTTP(httptest.NewRecorder(), r)
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for j := range 200 {
				switch (i + j) % 6 {
				case 0:
					flip.Store(j%2 == 0)
				case 1:
					h.Ready()
				case 2:
					h.Live()
				case 3:
					h.SetMaintenance([]string{"", "restore"}[j%2])
				case 4:
					serve(readyz, loopbackPeer)
				case 5:
					serve(healthz, publicPeer)
				}
			}
		})
	}
	wg.Go(func() { h.AddCheck("late", func() (bool, string) { return true, "" }) })
	wg.Go(func() { h.SetClientIP(nil) })
	wg.Wait()
	h.SetShuttingDown()
	if ok, _ := h.Live(); ok {
		t.Error("Live() = true after SetShuttingDown")
	}
}
