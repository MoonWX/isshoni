package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

const testOrigin = "http://wails.localhost"

// testDist is a bundle with the injection point assets.New needs.
var testDist = fstest.MapFS{"index.html": {Data: []byte(
	`<!doctype html><html><head><meta charset="utf-8"></head><body><script src="/boot-check.js"></script>` +
		`</body></html>`)}}

func newTestHost(t *testing.T) *host {
	t.Helper()
	h, err := newHost(context.Background(), options{dataDir: t.TempDir(), origin: testOrigin, dist: testDist},
		slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.close)
	return h
}

// do sends one request to the harness; origin "" sends none.
func do(t *testing.T, h *host, method, path, origin, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(context.Background(), method, "http://wails.localhost"+path,
		strings.NewReader(body))
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	return w
}

func call(t *testing.T, h *host, service, method string, args ...any) *httptest.ResponseRecorder {
	t.Helper()
	if args == nil {
		args = []any{}
	}
	b, _ := json.Marshal(map[string]any{"service": service, "method": method, "args": args})
	return do(t, h, http.MethodPost, callPath, testOrigin, string(b))
}

func state(t *testing.T, h *host) hostState {
	t.Helper()
	w := do(t, h, http.MethodGet, "/__e2e/state", "", "")
	var s hostState
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &s) != nil {
		t.Fatalf("state: %d %s", w.Code, w.Body)
	}
	return s
}

// TestHostServesTheApp: the SPA with bridge.js transport "http", the CSP, no service worker.
func TestHostServesTheApp(t *testing.T) {
	h := newTestHost(t)
	w := do(t, h, http.MethodGet, "/", "", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `<script src="/desktop/bridge.js">`) ||
		!strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'self'") {
		t.Fatalf("GET /: %d %v", w.Code, w.Header())
	}
	w = do(t, h, http.MethodGet, "/desktop/bridge.js", "", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"transport":"http"`) ||
		!strings.Contains(w.Body.String(), `"server":null`) || !strings.Contains(w.Body.String(), `"dev":true`) {
		t.Fatalf("bridge.js: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, http.MethodGet, "/sw.js", "", ""); w.Code != http.StatusNotFound {
		t.Fatalf("/sw.js: %d", w.Code)
	}
}

// TestHostCalls is the HTTP transport: results, bridge errors with non-2xx statuses, the harness's own failures,
// and the recording shell behind App.OpenExternal.
func TestHostCalls(t *testing.T) {
	h := newTestHost(t)
	w := call(t, h, "App", "Info")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"osBuild":26100`) {
		t.Fatalf("App.Info: %d %s", w.Code, w.Body)
	}
	if w := call(t, h, "App", "OpenExternal", "https://example.com/x"); w.Code != http.StatusOK ||
		w.Body.String() != "null" {
		t.Fatalf("OpenExternal https: %d %s", w.Code, w.Body)
	}
	w = call(t, h, "App", "OpenExternal", "ms-settings:display")
	if w.Code != http.StatusUnprocessableEntity ||
		w.Body.String() != `{"code":"invalid_argument","params":{"field":"url"}}` {
		t.Fatalf("OpenExternal ms-settings: %d %s", w.Code, w.Body)
	}
	if s := state(t, h); len(s.OpenedURLs) != 1 || s.OpenedURLs[0] != "https://example.com/x" {
		t.Fatalf("opened %v", s.OpenedURLs)
	}
	// A missing trailing argument is the zero value, as HttpBridge leaves trailing undefined values out.
	if w := call(t, h, "App", "Log", "warn", "a line"); w.Code != http.StatusOK {
		t.Fatalf("App.Log without attrs: %d %s", w.Code, w.Body)
	}
	if w := call(t, h, "Auth", "AccessToken", "http://127.0.0.1:1", false); w.Code != http.StatusOK ||
		w.Body.String() != "null" {
		t.Fatalf("AccessToken without a server: %d %s", w.Code, w.Body)
	}
	// Share runs WA58's controller over the fake engine: a pick, then the back-out that releases it.
	w = call(t, h, "Share", "Pick")
	var picked struct {
		PickID string `json:"pickId"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &picked) != nil || picked.PickID == "" {
		t.Fatalf("Share.Pick: %d %s", w.Code, w.Body)
	}
	if w := call(t, h, "Share", "Release", picked.PickID); w.Code != http.StatusOK {
		t.Fatalf("Share.Release: %d %s", w.Code, w.Body)
	}
	// A bridge method still unimplemented answers 422 not_implemented (README WA71).
	if w := call(t, h, "Auth", "SwitchServer", "http://127.0.0.1:1"); w.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(w.Body.String(), `"code":"not_implemented"`) {
		t.Fatalf("Auth.SwitchServer: %d %s", w.Code, w.Body)
	}
	if w := call(t, h, "Audio", "FriendsHear"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"seq"`) {
		t.Fatalf("Audio.FriendsHear over 05's service: %d %s", w.Code, w.Body)
	}
	for _, c := range []struct {
		service, method, reason string
		args                    []any
	}{
		{"Nope", "Info", "unknown_service", nil},
		{"App", "Nope", "unknown_method", nil},
		{"App", "OpenExternal", "bad_argument", []any{42}},
		{"App", "Info", "too_many_arguments", []any{1}},
	} {
		w := call(t, h, c.service, c.method, c.args...)
		if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), `"reason":"`+c.reason+`"`) {
			t.Errorf("%s.%s: %d %s", c.service, c.method, w.Code, w.Body)
		}
	}
	if w := do(t, h, http.MethodGet, callPath, testOrigin, ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET call: %d", w.Code)
	}
	if w := do(t, h, http.MethodPost, callPath, testOrigin, `{"service":"App","method":"Info","x":1}`); w.Code !=
		http.StatusBadRequest {
		t.Fatalf("unknown field: %d", w.Code)
	}
}

// TestHostGuard: the real Guard in front of the stub runtime; the transport refuses other origins too.
func TestHostGuard(t *testing.T) {
	h := newTestHost(t)
	runtimeBody := func(object, method int) string {
		b, _ := json.Marshal(map[string]any{"object": object, "method": method, "args": map[string]any{}})
		return string(b)
	}
	if w := do(t, h, http.MethodPost, "/wails/runtime", "https://evil.example", runtimeBody(0, 0)); w.Code !=
		http.StatusForbidden {
		t.Fatalf("foreign origin: %d", w.Code)
	}
	for _, c := range [][2]int{{9, 0}, {1, 1}, {6, 11}, {2, 2}} { // Browser.OpenURL, Clipboard.Text, Window.Hide, Quit
		if w := do(t, h, http.MethodPost, "/wails/runtime", testOrigin, runtimeBody(c[0], c[1])); w.Code !=
			http.StatusForbidden {
			t.Errorf("object %d method %d: %d", c[0], c[1], w.Code)
		}
	}
	if s := state(t, h); len(s.RuntimeCalls) != 0 {
		t.Fatalf("refused calls reached the stub: %v", s.RuntimeCalls)
	}
	if w := do(t, h, http.MethodPost, "/wails/runtime", testOrigin, runtimeBody(0, 0)); w.Code != http.StatusOK {
		t.Fatalf("a bound-method Call: %d %s", w.Code, w.Body)
	}
	if s := state(t, h); len(s.RuntimeCalls) != 1 || s.RuntimeCalls[0] != (runtimeCall{0, 0}) {
		t.Fatalf("runtime calls %v", s.RuntimeCalls)
	}
	if w := call(t, h, "App", "Info"); w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
	if w := do(t, h, http.MethodPost, callPath, "https://evil.example", `{"service":"App","method":"Info"}`); w.Code !=
		http.StatusForbidden {
		t.Fatalf("a call from another origin: %d", w.Code)
	}
}

// TestHostInjection: /__e2e/args reaches HandleArgs (an unknown server gives the add question), hotkeys and OS
// events are delivered, and the event stream carries the app's events.
func TestHostInjection(t *testing.T) {
	h := newTestHost(t)
	srv := httptest.NewServer(h.handler)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+eventsPath, nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("events: %d %v", resp.StatusCode, resp.Header)
	}
	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	if l := <-lines; !strings.HasPrefix(l, ":") {
		t.Fatalf("first line %q", l)
	}

	w := do(t, h, http.MethodPost, "/__e2e/args", "", `{"args":["--server","http://127.0.0.1:9"]}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("args: %d %s", w.Code, w.Body)
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatal("the stream ended")
			}
			if !strings.HasPrefix(l, "data: ") {
				continue
			}
			var ev struct {
				Name string          `json:"name"`
				Data json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(l, "data: ")), &ev); err != nil {
				t.Fatal(err)
			}
			if ev.Name != "isshoni:deeplink.open" || !bytes.Contains(ev.Data, []byte(`"origin":"http://127.0.0.1:9"`)) {
				t.Fatalf("event %s %s", ev.Name, ev.Data)
			}
			goto delivered
		case <-deadline:
			t.Fatal("no deeplink.open event")
		}
	}
delivered:
	if w := call(t, h, "App", "PendingLink"); !strings.Contains(w.Body.String(), `"add"`) {
		t.Fatalf("PendingLink: %s", w.Body)
	}
	if s := state(t, h); s.ShowWindowCalls == 0 {
		t.Fatal("the window wasn't shown for the link")
	}

	if w := do(t, h, http.MethodPost, "/__e2e/hotkey?id=pause", "", ""); w.Code != http.StatusNoContent {
		t.Fatalf("hotkey: %d", w.Code)
	}
	if w := do(t, h, http.MethodPost, "/__e2e/hotkey?id=other", "", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("bad hotkey: %d", w.Code)
	}
	if w := do(t, h, http.MethodPost, "/__e2e/system?event=lock", "", ""); w.Code != http.StatusNoContent {
		t.Fatalf("system: %d", w.Code)
	}
	if w := do(t, h, http.MethodPost, "/__e2e/system?event=reboot", "", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("bad system event: %d", w.Code)
	}
	if w := do(t, h, http.MethodGet, "/__e2e/args", "", ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET args: %d", w.Code)
	}
	cancel()
	_, _ = io.Copy(io.Discard, resp.Body)
}

// TestParseFlags: the harness listens on loopback only and keeps the app's command line.
func TestParseFlags(t *testing.T) {
	o, err := parseFlags([]string{"-reload-delay=2s", "--", "--server", "http://127.0.0.1:9"}, io.Discard)
	if err != nil || o.reloadDelay != 2*time.Second || o.origin != testOrigin || len(o.args) != 2 ||
		o.listen != "127.0.0.1:0" {
		t.Fatalf("%+v %v", o, err)
	}
	for _, l := range []string{"0.0.0.0:0", ":8080", "192.0.2.1:80", "127.0.0.1"} {
		if _, err := parseFlags([]string{"-listen", l}, io.Discard); err == nil {
			t.Errorf("-listen %s accepted", l)
		}
	}
}

// TestRun starts the harness for real: it prints its address, serves, and stops when its context ends.
func TestRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, options{listen: "127.0.0.1:0", origin: testOrigin, dist: testDist}, pw)
		_ = pw.Close()
	}()
	line, err := bufio.NewReader(pr).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = io.Copy(io.Discard, pr) }()
	var out struct{ Addr, Origin, Log string }
	if err := json.Unmarshal([]byte(line), &out); err != nil || !strings.HasPrefix(out.Addr, "127.0.0.1:") ||
		out.Origin != testOrigin {
		t.Fatalf("first line %q: %v", line, err)
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+out.Addr+"/__e2e/state", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("state: %d", resp.StatusCode)
	}
	http.DefaultClient.CloseIdleConnections()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run didn't stop")
	}
}
