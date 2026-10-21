package assets

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingHandler is the next handler behind the Guard: Wails' runtime in the app.
type recordingHandler struct {
	mu    sync.Mutex
	calls int
	body  []byte
	clen  int64
	query string
}

func (h *recordingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	h.mu.Lock()
	h.calls++
	h.body, h.clen, h.query = b, r.ContentLength, r.URL.RawQuery
	h.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// logRecorder collects the Guard's log records.
type logRecorder struct {
	mu      sync.Mutex
	records []slog.Record
}

func (l *logRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (l *logRecorder) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	l.records = append(l.records, r.Clone())
	l.mu.Unlock()
	return nil
}
func (l *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logRecorder) WithGroup(string) slog.Handler      { return l }

func (l *logRecorder) warnings() []slog.Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []slog.Record
	for _, r := range l.records {
		if r.Level == slog.LevelWarn {
			out = append(out, r)
		}
	}
	return out
}

func attrs(r slog.Record) map[string]string {
	m := map[string]string{}
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.String()
		return true
	})
	return m
}

// fakeClock is the rate limit's clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type guardFixture struct {
	next  *recordingHandler
	log   *logRecorder
	clock *fakeClock
	h     http.Handler
}

func newGuardFixture(origin string) *guardFixture { return newGuardFixtureFor(origin, false) }

// newWKWebViewGuardFixture is GuardWKWebView's fixture.
func newWKWebViewGuardFixture(origin string) *guardFixture { return newGuardFixtureFor(origin, true) }

func newGuardFixtureFor(origin string, wkwebview bool) *guardFixture {
	f := &guardFixture{next: &recordingHandler{}, log: &logRecorder{}, clock: &fakeClock{now: time.Unix(1e9, 0)}}
	f.h = newGuard(origin, wkwebview, slog.New(f.log), f.clock.Now).middleware(f.next)
	return f
}

func (f *guardFixture) do(r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, r)
	return rec
}

// The two request forms of the pinned transport (runtime_allow.go).
const (
	formBody  = "body"  // runtime.ts: POST, JSON {object, method, args}
	formQuery = "query" // the transport's fallback for an empty body: ?object=&method=&args=
)

// newRuntimeRequest builds a /wails/runtime request the way @wailsio/runtime 3.0.0-beta.34 builds it (runtime.ts
// runtimeCallWithID): POST to <origin>/wails/runtime with x-wails-client-id, Content-Type application/json and the
// page's Origin; args is left out when nil.
func newRuntimeRequest(form string, object, method int, args any) *http.Request {
	var r *http.Request
	switch form {
	case formBody:
		body := map[string]any{"object": object, "method": method}
		if args != nil {
			body["args"] = args
		}
		b, _ := json.Marshal(body)
		r = httptest.NewRequestWithContext(context.Background(), http.MethodPost, testOrigin+runtimePath, bytes.NewReader(b))
	case formQuery:
		q := url.Values{"object": {strconv.Itoa(object)}, "method": {strconv.Itoa(method)}}
		if args != nil {
			b, _ := json.Marshal(args)
			q.Set("args", string(b))
		}
		r = httptest.NewRequestWithContext(context.Background(), http.MethodPost, testOrigin+runtimePath+"?"+q.Encode(), nil)
	}
	r.Header.Set("x-wails-client-id", "V1StGXR8_Z5jdHi6B-myT")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", testOrigin)
	return r
}

// callArgs is a bound-method call as calls.ts sends it.
var callArgs = map[string]any{"call-id": "Uakgb_J5m9g-0JDMbcJqLJ", "methodID": 1203456789, "args": []any{"x", 1}}

func TestGuardPassesRequestsOutsideWails(t *testing.T) {
	f := newGuardFixture(testOrigin)
	for _, p := range []string{"/", "/assets/index.js", BridgePath, "/wailsx", "/x/wails/runtime"} {
		r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, testOrigin+p, strings.NewReader("{}"))
		r.Header.Set("Origin", "https://evil.example")
		if rec := f.do(r); rec.Code != http.StatusOK {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	if f.next.calls != 5 {
		t.Errorf("next called %d times, want 5", f.next.calls)
	}
}

func TestGuardOrigin(t *testing.T) {
	cases := []struct {
		name    string
		origin  []string // Origin header values
		fetch   string   // Sec-Fetch-Site
		allowed bool
	}{
		{"asset origin", []string{testOrigin}, "", true},
		{"asset origin, same-origin fetch", []string{testOrigin}, "same-origin", true},
		{"no Origin, same-origin fetch", nil, "same-origin", true},
		{"no Origin, no Sec-Fetch-Site", nil, "", false},
		{"no Origin, cross-site", nil, "cross-site", false},
		{"no Origin, same-site", nil, "same-site", false},
		{"no Origin, none", nil, "none", false},
		{"foreign Origin", []string{"https://evil.example"}, "", false},
		{"foreign Origin claiming same-origin", []string{"https://evil.example"}, "same-origin", false},
		{"null Origin", []string{"null"}, "same-origin", false},
		{"another port", []string{testOrigin + ":8080"}, "", false},
		{"https", []string{"https://wails.localhost"}, "", false},
		{"upper case", []string{"HTTP://WAILS.LOCALHOST"}, "", false},
		{"trailing slash", []string{testOrigin + "/"}, "", false},
		{"a longer host", []string{"http://wails.localhost.evil.example"}, "", false},
		{"two Origin headers", []string{testOrigin, testOrigin}, "", false},
		{"empty Origin", []string{""}, "same-origin", false},
	}
	for _, c := range cases {
		for _, p := range []string{runtimePath, "/wails/runtime.js", eventPayloadPath + strings.Repeat("a", 32)} {
			f := newGuardFixture(testOrigin)
			var r *http.Request
			if p == runtimePath {
				r = newRuntimeRequest(formBody, objectCall, 0, callArgs)
			} else {
				r = httptest.NewRequestWithContext(context.Background(), http.MethodGet, testOrigin+p, nil)
			}
			r.Header.Del("Origin")
			for _, o := range c.origin {
				r.Header.Add("Origin", o)
			}
			if c.fetch != "" {
				r.Header.Set("Sec-Fetch-Site", c.fetch)
			}
			rec := f.do(r)
			if c.allowed {
				if rec.Code != http.StatusOK || f.next.calls != 1 {
					t.Errorf("%s %s: %d, next %d; want it through", c.name, p, rec.Code, f.next.calls)
				}
				continue
			}
			if rec.Code != http.StatusForbidden || f.next.calls != 0 {
				t.Errorf("%s %s: %d, next %d; want 403", c.name, p, rec.Code, f.next.calls)
			}
			w := f.log.warnings()
			if len(w) != 1 || w[0].Message != "desktop: runtime request from another origin" {
				t.Errorf("%s %s: warnings %v", c.name, p, w)
			}
		}
	}
}

// TestGuardWKWebView: GuardWKWebView with wails://localhost (Wails on macOS) lets a request without Origin and
// Sec-Fetch-Site through, as WKWebView sends neither to the scheme handler, but on /wails/runtime only with the
// runtime's own x-wails-client-id header; anything naming another origin or site is refused.
func TestGuardWKWebView(t *testing.T) {
	const macOrigin = "wails://localhost"
	cases := []struct {
		name     string
		origin   string
		fetch    string
		clientID bool
		allowed  bool
	}{
		{"neither header, the runtime's client id", "", "", true, true},
		{"neither header, no client id", "", "", false, false},
		{"the asset origin", macOrigin, "", true, true},
		{"the asset origin, no client id", macOrigin, "", false, true},
		{"same-origin fetch", "", "same-origin", true, true},
		{"cross-site", "", "cross-site", true, false},
		{"same-site", "", "same-site", true, false},
		{"none", "", "none", true, false},
		{"foreign Origin", "https://evil.example", "", true, false},
		{"null Origin", "null", "", true, false},
		{"the Windows origin", testOrigin, "", true, false},
	}
	for _, c := range cases {
		f := newWKWebViewGuardFixture(macOrigin)
		r := newRuntimeRequest(formBody, objectCall, 0, callArgs)
		r.Header.Del("Origin")
		if !c.clientID {
			r.Header.Del("x-wails-client-id")
		}
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if c.fetch != "" {
			r.Header.Set("Sec-Fetch-Site", c.fetch)
		}
		rec := f.do(r)
		if got := rec.Code == http.StatusOK && f.next.calls == 1; got != c.allowed {
			t.Errorf("%s: %d, next %d; allowed %v", c.name, rec.Code, f.next.calls, c.allowed)
		}
	}

	// A header-less simple POST (text/plain, no custom header: no preflight) gets 403, in both request forms; a
	// header-less preflight gets no 2xx either.
	for _, form := range []string{formBody, formQuery} {
		f := newWKWebViewGuardFixture(macOrigin)
		r := newRuntimeRequest(form, objectCall, 0, callArgs)
		r.Header.Del("Origin")
		r.Header.Del("x-wails-client-id")
		r.Header.Set("Content-Type", "text/plain")
		if rec := f.do(r); rec.Code != http.StatusForbidden || f.next.calls != 0 {
			t.Errorf("%s form, simple POST: %d, next %d", form, rec.Code, f.next.calls)
		}
	}
	f := newWKWebViewGuardFixture(macOrigin)
	pre := httptest.NewRequestWithContext(context.Background(), http.MethodOptions, macOrigin+runtimePath, nil)
	pre.Header.Set("Access-Control-Request-Method", http.MethodPost)
	pre.Header.Set("Access-Control-Request-Headers", "content-type,x-wails-client-id")
	if rec := f.do(pre); rec.Code < 300 || f.next.calls != 0 {
		t.Errorf("header-less preflight: %d, next %d", rec.Code, f.next.calls)
	}

	// The runtime's static GETs need no client id (fixed content), and the other checks still hold.
	f = newWKWebViewGuardFixture(macOrigin)
	get := httptest.NewRequestWithContext(context.Background(), http.MethodGet, macOrigin+"/wails/runtime.js", nil)
	if rec := f.do(get); rec.Code != http.StatusOK || f.next.calls != 1 {
		t.Errorf("GET /wails/runtime.js: %d", rec.Code)
	}
	f = newWKWebViewGuardFixture(macOrigin)
	get = httptest.NewRequestWithContext(context.Background(), http.MethodGet, macOrigin+"/wails/stream/poll", nil)
	if rec := f.do(get); rec.Code != http.StatusForbidden {
		t.Errorf("GET /wails/stream/poll: %d", rec.Code)
	}

	// The allowlist holds there too.
	f = newWKWebViewGuardFixture(macOrigin)
	r := newRuntimeRequest(formBody, 9, 0, map[string]any{"url": "x-apple.systempreferences:"})
	r.Header.Del("Origin")
	if rec := f.do(r); rec.Code != http.StatusForbidden {
		t.Errorf("Browser.OpenURL: %d", rec.Code)
	}

	// An http origin never gets the exception, not even from GuardWKWebView.
	f = newWKWebViewGuardFixture(testOrigin)
	r = newRuntimeRequest(formBody, objectCall, 0, callArgs)
	r.Header.Del("Origin")
	if rec := f.do(r); rec.Code != http.StatusForbidden {
		t.Errorf("GuardWKWebView with an http origin, neither header: %d", rec.Code)
	}
}

// TestGuardHasNoSchemeException: Guard keeps §5.6's rule for every origin; a custom scheme alone doesn't let a
// request without Origin and Sec-Fetch-Site through (only GuardWKWebView does).
func TestGuardHasNoSchemeException(t *testing.T) {
	for _, origin := range []string{"wails://localhost", "isshoni-app://localhost", testOrigin} {
		f := newGuardFixture(origin)
		r := newRuntimeRequest(formBody, objectCall, 0, callArgs)
		r.Header.Del("Origin")
		if rec := f.do(r); rec.Code != http.StatusForbidden || f.next.calls != 0 {
			t.Errorf("%s, neither header: %d, next %d", origin, rec.Code, f.next.calls)
		}
		f = newGuardFixture(origin)
		r = newRuntimeRequest(formBody, objectCall, 0, callArgs)
		r.Header.Set("Origin", origin)
		if rec := f.do(r); rec.Code != http.StatusOK {
			t.Errorf("%s, its own Origin: %d", origin, rec.Code)
		}
	}
	// The exported constructors: Guard refuses the header-less request that GuardWKWebView lets through.
	r := newRuntimeRequest(formBody, objectCall, 0, callArgs)
	r.Header.Del("Origin")
	next := &recordingHandler{}
	rec := httptest.NewRecorder()
	Guard("wails://localhost")(next).ServeHTTP(rec, r.Clone(context.Background()))
	if rec.Code != http.StatusForbidden {
		t.Errorf("Guard: %d", rec.Code)
	}
	r = newRuntimeRequest(formBody, objectCall, 0, callArgs)
	r.Header.Del("Origin")
	rec = httptest.NewRecorder()
	GuardWKWebView("wails://localhost")(next).ServeHTTP(rec, r)
	if rec.Code != http.StatusOK || next.calls != 1 {
		t.Errorf("GuardWKWebView: %d, next %d", rec.Code, next.calls)
	}
}

func TestGuardWithoutOriginRefusesEverything(t *testing.T) {
	f := newGuardFixture("")
	r := newRuntimeRequest(formBody, objectCall, 0, callArgs)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if rec := f.do(r); rec.Code != http.StatusForbidden {
		t.Fatalf("%d", rec.Code)
	}
}

func TestGuardOriginTruncatedInLog(t *testing.T) {
	f := newGuardFixture(testOrigin)
	r := newRuntimeRequest(formBody, objectCall, 0, callArgs)
	r.Header.Set("Origin", "https://"+strings.Repeat("a", 300)+".example")
	f.do(r)
	w := f.log.warnings()
	if len(w) != 1 || len(attrs(w[0])["origin"]) != 128 {
		t.Fatalf("warnings %v", w)
	}
}

// TestGuardLetsBoundCallsThrough: Call and CancelCall in both forms reach the runtime with the body intact.
func TestGuardLetsBoundCallsThrough(t *testing.T) {
	for _, form := range []string{formBody, formQuery} {
		for _, c := range []struct {
			object int
			args   any
		}{
			{objectCall, callArgs},
			{objectCall, map[string]any{"call-id": "x", "methodName": "main.Service.Method", "args": []any{}}},
			{objectCancelCall, map[string]any{"call-id": "Uakgb_J5m9g-0JDMbcJqLJ"}},
		} {
			f := newGuardFixture(testOrigin)
			r := newRuntimeRequest(form, c.object, 0, c.args)
			var want []byte
			if r.Body != nil {
				want, _ = io.ReadAll(r.Body)
				r.Body = io.NopCloser(bytes.NewReader(want))
			}
			wantQuery := r.URL.RawQuery
			rec := f.do(r)
			if rec.Code != http.StatusOK || f.next.calls != 1 {
				t.Fatalf("%s object %d: %d, next %d", form, c.object, rec.Code, f.next.calls)
			}
			if !bytes.Equal(f.next.body, want) || f.next.clen != int64(len(want)) || f.next.query != wantQuery {
				t.Errorf("%s object %d: next got body %q (length %d), query %q; want %q, %q", form, c.object,
					f.next.body, f.next.clen, f.next.query, want, wantQuery)
			}
			if w := f.log.warnings(); len(w) != 0 {
				t.Errorf("%s object %d: warnings %v", form, c.object, w)
			}
		}
	}
}

// refusedCalls are runtime calls the Guard refuses, each as the pinned runtime builds it (W21; 03 §22.1).
var refusedCalls = []struct {
	name           string
	object, method int
	args           any
	logObject      string
	logMethod      string
}{
	{"Browser.OpenURL ms-settings", 9, 0, map[string]any{"url": "ms-settings:display"}, "Browser", "OpenURL"},
	{"Browser.OpenURL search-ms", 9, 0, map[string]any{"url": "search-ms:query=x"}, "Browser", "OpenURL"},
	{"Browser.OpenURL https", 9, 0, map[string]any{"url": "https://example.com"}, "Browser", "OpenURL"},
	{"Clipboard.Text", 1, 1, nil, "Clipboard", "Text"},
	{"Clipboard.SetText", 1, 0, map[string]any{"text": "x"}, "Clipboard", "SetText"},
	{"Window.Hide", 6, 11, nil, "Window", "Hide"},
	{"Window.OpenDevTools", 6, 19, nil, "Window", "OpenDevTools"},
	{"Window.Print", 6, 51, nil, "Window", "Print"},
	{"Window.SetAlwaysOnTop", 6, 25, map[string]any{"alwaysOnTop": true}, "Window", "SetAlwaysOnTop"},
	{"Window.Reload", 6, 21, nil, "Window", "Reload"},
	{"Application.Quit", 2, 2, nil, "Application", "Quit"},
	{"Application.Hide", 2, 0, nil, "Application", "Hide"},
	{"Dialog.OpenFile", 5, 4, map[string]any{"dialog-id": "x"}, "Dialog", "OpenFile"},
	{"Dialog.Info", 5, 0, map[string]any{"dialog-id": "x", "message": "x"}, "Dialog", "Info"},
	{"System.Environment", 8, 1, nil, "System", "Environment"},
	{"System.IsDarkMode", 8, 0, nil, "System", "IsDarkMode"},
	{"Screens.GetAll", 7, 0, nil, "Screens", "GetAll"},
	{"ContextMenu.Open", 4, 0, map[string]any{"id": "x", "x": 1, "y": 2, "data": nil}, "ContextMenu", "Open"},
	{"Events.Emit", 3, 0, map[string]any{"name": "x", "data": 1}, "Events", "Emit"},
	{"IOS", 11, 0, nil, "IOS", "unknown-0"},
	{"Android", 12, 0, nil, "Android", "unknown-0"},
	{"Call, unknown method", 0, 1, callArgs, "Call", "unknown-1"},
	{"CancelCall, unknown method", 10, 1, map[string]any{"call-id": "x"}, "CancelCall", "unknown-1"},
	{"an object the pin doesn't have", 13, 0, nil, "unknown-13", "0"},
	{"a negative object", -1, 0, nil, "unknown--1", "0"},
}

func TestGuardRefusesRuntimeObjects(t *testing.T) {
	for _, form := range []string{formBody, formQuery} {
		for _, c := range refusedCalls {
			f := newGuardFixture(testOrigin)
			rec := f.do(newRuntimeRequest(form, c.object, c.method, c.args))
			if rec.Code != http.StatusForbidden || f.next.calls != 0 {
				t.Errorf("%s %s: %d, next %d; want 403", form, c.name, rec.Code, f.next.calls)
				continue
			}
			w := f.log.warnings()
			if len(w) != 1 || w[0].Message != "desktop: runtime object refused" {
				t.Errorf("%s %s: warnings %v", form, c.name, w)
				continue
			}
			a := attrs(w[0])
			if a["object"] != c.logObject || a["method"] != c.logMethod || a["component"] != "assets" {
				t.Errorf("%s %s: logged %v", form, c.name, a)
			}
			for k, v := range a {
				if strings.Contains(v, "ms-settings") || strings.Contains(v, "search-ms") {
					t.Errorf("%s %s: %s=%q logs the arguments", form, c.name, k, v)
				}
			}
		}
	}
}

// TestGuardWarnIsRateLimited: a flood of refused calls logs one WARN line per minute and reports the rest.
func TestGuardWarnIsRateLimited(t *testing.T) {
	f := newGuardFixture(testOrigin)
	for range 50 {
		if rec := f.do(newRuntimeRequest(formBody, 9, 0, map[string]any{"url": "ms-settings:display"})); rec.Code != 403 {
			t.Fatalf("%d", rec.Code)
		}
	}
	if w := f.log.warnings(); len(w) != 1 {
		t.Fatalf("%d warnings for 50 refusals in a minute, want 1", len(w))
	}
	f.clock.Advance(time.Minute)
	f.do(newRuntimeRequest(formBody, 1, 1, nil))
	w := f.log.warnings()
	if len(w) != 2 || attrs(w[1])["dropped"] != "49" || attrs(w[1])["object"] != "Clipboard" {
		t.Fatalf("after a minute: %v", w)
	}
	// The origin refusals have a limit of their own.
	r := newRuntimeRequest(formBody, objectCall, 0, callArgs)
	r.Header.Set("Origin", "https://evil.example")
	f.do(r)
	if w := f.log.warnings(); len(w) != 3 {
		t.Fatalf("an origin refusal after an object refusal: %d warnings", len(w))
	}
}

func TestGuardRefusesUnparsableRequests(t *testing.T) {
	cases := map[string]struct{ query, body string }{
		"not JSON":                  {"", "object=0&method=0"},
		"whitespace":                {"", " "},
		"null":                      {"", "null"},
		"no object":                 {"", `{"method":0}`},
		"no method":                 {"", `{"object":0}`},
		"object as a string":        {"", `{"object":"0","method":0}`},
		"object as a float":         {"", `{"object":0.5,"method":0}`},
		"object overflows":          {"", `{"object":99999999999999999999,"method":0}`},
		"trailing data":             {"", `{"object":0,"method":0}{"object":9,"method":0}`},
		"an array":                  {"", `[0,0]`},
		"empty body, no query":      {"", ""},
		"query object not a number": {"object=Browser&method=0", ""},
		"query without method":      {"object=0", ""},
		"body and query":            {"object=0&method=0", `{"object":0,"method":0,"args":{"call-id":"x"}}`},
		"body and a query method":   {"method=0", `{"object":0,"method":0,"args":{"call-id":"x"}}`},
	}
	for name, c := range cases {
		f := newGuardFixture(testOrigin)
		target := testOrigin + runtimePath
		if c.query != "" {
			target += "?" + c.query
		}
		r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, target, strings.NewReader(c.body))
		r.Header.Set("Origin", testOrigin)
		if rec := f.do(r); rec.Code != http.StatusForbidden || f.next.calls != 0 {
			t.Errorf("%s: %d, next %d; want 403", name, rec.Code, f.next.calls)
		}
	}
}

// TestGuardMatchesTheTransportsDecoding: the cases where encoding/json's rules (case-insensitive keys, the last of
// duplicate keys) decide the object; the Guard decodes like the transport, so it sees what the runtime will run.
func TestGuardMatchesTheTransportsDecoding(t *testing.T) {
	cases := map[string]bool{
		`{"OBJECT":9,"method":0,"args":{"url":"ms-settings:display"}}`:            false,
		`{"Object":0,"Method":0,"args":{"call-id":"x"}}`:                          true,
		`{"object":0,"object":9,"method":0,"args":{"url":"ms-settings:display"}}`: false,
		`{"object":9,"object":0,"method":0,"args":{"call-id":"x"}}`:               true,
		`{"object":0,"OBJECT":9,"method":0,"args":{"url":"ms-settings:display"}}`: false,
		`{"object":0,"method":0,"method":19}`:                                     false,
		`{"object":0,"method":-0,"args":{"call-id":"x"}}`:                         true,
		`{"object":10,"method":0,"args":{"call-id":"x"},"x-wails-chunk-id":"a"}`:  true,
	}
	for body, allowed := range cases {
		f := newGuardFixture(testOrigin)
		r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, testOrigin+runtimePath, strings.NewReader(body))
		r.Header.Set("Origin", testOrigin)
		rec := f.do(r)
		if got := rec.Code == http.StatusOK; got != allowed {
			t.Errorf("%s: %d", body, rec.Code)
		}
		o, m, err := transportDecode([]byte(body), url.Values{})
		if err != nil || allowedCall(*o, *m) != allowed {
			t.Errorf("%s: the transport's decoding gives %v.%v (%v)", body, o, m, err)
		}
	}
}

func TestGuardRefusesChunkedRequests(t *testing.T) {
	for _, headers := range []map[string]string{
		{"x-wails-chunk-id": "abc", "x-wails-chunk-index": "0", "x-wails-chunk-total": "2"},
		{"x-wails-chunk-id": "abc", "x-wails-chunk-index": "1", "x-wails-chunk-total": "2"},
		{"X-Wails-Chunk-Id": ""},
		{"x-wails-chunk-total": "1"},
		{"x-wails-chunk-index": "0"},
	} {
		for _, form := range []string{formBody, formQuery} {
			f := newGuardFixture(testOrigin)
			r := newRuntimeRequest(form, objectCall, 0, callArgs)
			for k, v := range headers {
				r.Header[k] = []string{v}
			}
			if rec := f.do(r); rec.Code != http.StatusForbidden || f.next.calls != 0 {
				t.Errorf("%s %v: %d, next %d; want 403", form, headers, rec.Code, f.next.calls)
			}
			if w := f.log.warnings(); len(w) != 1 || attrs(w[0])["reason"] != "chunked" {
				t.Errorf("%s %v: warnings %v", form, headers, w)
			}
		}
	}
}

func TestGuardBodyLimit(t *testing.T) {
	// A Call whose body is exactly 64 KiB passes; one byte more is refused with 413.
	prefix := `{"object":0,"method":0,"args":{"call-id":"x","methodID":1,"args":["`
	suffix := `"]}}`
	exact := prefix + strings.Repeat("a", runtimeBodyLimit-len(prefix)-len(suffix)) + suffix
	for _, c := range []struct {
		body string
		code int
	}{
		{exact, http.StatusOK},
		{exact + " ", http.StatusRequestEntityTooLarge},
		{exact + strings.Repeat(" ", 1<<20), http.StatusRequestEntityTooLarge},
	} {
		f := newGuardFixture(testOrigin)
		r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, testOrigin+runtimePath, strings.NewReader(c.body))
		r.Header.Set("Origin", testOrigin)
		rec := f.do(r)
		if rec.Code != c.code {
			t.Errorf("body of %d bytes: %d, want %d", len(c.body), rec.Code, c.code)
		}
		if c.code == http.StatusOK && len(f.next.body) != runtimeBodyLimit {
			t.Errorf("next got %d bytes, want %d", len(f.next.body), runtimeBodyLimit)
		}
		if c.code != http.StatusOK && f.next.calls != 0 {
			t.Errorf("body of %d bytes reached the runtime", len(c.body))
		}
	}
}

func TestGuardRuntimeNeedsPost(t *testing.T) {
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodOptions} {
		f := newGuardFixture(testOrigin)
		r := httptest.NewRequestWithContext(context.Background(), m, testOrigin+runtimePath+"?object=0&method=0", nil)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		if rec := f.do(r); rec.Code != http.StatusMethodNotAllowed || f.next.calls != 0 {
			t.Errorf("%s: %d, next %d", m, rec.Code, f.next.calls)
		}
	}
}

func TestGuardOtherRuntimePaths(t *testing.T) {
	cases := []struct {
		method, path string
		code         int
	}{
		{http.MethodGet, "/wails/runtime.js", http.StatusOK},
		{http.MethodHead, "/wails/custom.js", http.StatusOK},
		{http.MethodGet, "/wails/transport.js", http.StatusOK},
		{http.MethodGet, eventPayloadPath + "0123456789abcdef0123456789abcdef", http.StatusOK},
		{http.MethodPost, eventPayloadPath + "0123456789abcdef0123456789abcdef", http.StatusMethodNotAllowed},
		{http.MethodPost, "/wails/runtime.js", http.StatusMethodNotAllowed},
		{http.MethodGet, "/wails/stream/poll", http.StatusForbidden},
		{http.MethodPost, "/wails/stream/send", http.StatusForbidden},
		{http.MethodGet, "/wails/", http.StatusForbidden},
		{http.MethodGet, "/wails", http.StatusForbidden},
		{http.MethodPost, "/wails/runtime/", http.StatusForbidden},
		{http.MethodPost, "/wails/Runtime", http.StatusForbidden},
		{http.MethodGet, "/wails/unknown", http.StatusForbidden},
	}
	for _, c := range cases {
		f := newGuardFixture(testOrigin)
		r := httptest.NewRequestWithContext(context.Background(), c.method, testOrigin+c.path, nil)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		rec := f.do(r)
		if rec.Code != c.code || (f.next.calls == 1) != (c.code == http.StatusOK) {
			t.Errorf("%s %s: %d, next %d; want %d", c.method, c.path, rec.Code, f.next.calls, c.code)
		}
	}
}

// transportDecode is the pinned transport's decoding (transport_http.go processBody, v3.0.0-beta.34), copied, so the
// fuzzer can compare the Guard with it.
func transportDecode(bodyBytes []byte, query url.Values) (object, method *int, err error) {
	var body struct {
		Object *int            `json:"object"`
		Method *int            `json:"method"`
		Args   json.RawMessage `json:"args"`
	}
	if len(bodyBytes) > 0 {
		if err = json.Unmarshal(bodyBytes, &body); err != nil {
			return nil, nil, err
		}
	} else {
		if objStr := query.Get("object"); objStr != "" {
			obj, parseErr := strconv.Atoi(objStr)
			if parseErr == nil {
				body.Object = &obj
			}
		}
		if methStr := query.Get("method"); methStr != "" {
			meth, parseErr := strconv.Atoi(methStr)
			if parseErr == nil {
				body.Method = &meth
			}
		}
	}
	if body.Object == nil || body.Method == nil {
		return nil, nil, io.ErrUnexpectedEOF
	}
	return body.Object, body.Method, nil
}

// FuzzGuard: whatever body and query, a request the Guard lets through is one the pinned transport decodes to Call
// or CancelCall.
func FuzzGuard(f *testing.F) {
	f.Add(`{"object":0,"method":0,"args":{"call-id":"x"}}`, "")
	f.Add(`{"object":9,"method":0,"args":{"url":"ms-settings:display"}}`, "")
	f.Add("", "object=10&method=0&args=%7B%22call-id%22%3A%22x%22%7D")
	f.Add("", "object=9&method=0")
	f.Add(`{"OBJECT":9,"object":0,"method":0}`, "")
	f.Add(`{"object":0,"method":0}`, "object=9")
	f.Add("", "object=0&object=9&method=0")
	f.Add("", "object=+0&method=-0")
	f.Fuzz(func(t *testing.T, body, rawQuery string) {
		g := newGuardFixture(testOrigin)
		r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, testOrigin+runtimePath, strings.NewReader(body))
		r.URL.RawQuery = rawQuery
		r.Header.Set("Origin", testOrigin)
		rec := g.do(r)
		if rec.Code != http.StatusOK {
			return
		}
		if g.next.calls != 1 || string(g.next.body) != body {
			t.Fatalf("passed, but next got %q (%d calls)", g.next.body, g.next.calls)
		}
		o, m, err := transportDecode([]byte(body), r.URL.Query())
		if err != nil {
			t.Fatalf("passed, but the transport can't decode it: %v", err)
		}
		if !allowedCall(*o, *m) {
			t.Fatalf("passed, but the transport runs %d.%d", *o, *m)
		}
	})
}
