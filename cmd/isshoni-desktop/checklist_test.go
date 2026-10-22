//go:build (windows || (darwin && cgo)) && isshoni_hw

package main

// The Wails checklist (03 §3.2) as one test that runs the real shell: the pinned Wails, the app's appOptions and
// mainWindowOptions, the real asset handler and Guard, in the OS's real webview. A checklist page
// (testdata/checklist) runs every check from page script and reports to this test, which decides W1–W4, W10–W13,
// W19 and W21 and prints the rows for the PR's "Wails checklist". It needs a desktop session, so it runs only with
// the isshoni_hw tag (the owner's PC; on a Mac it runs in WKWebView, the dev build's webview):
//
//	go test -tags isshoni_hw -count=1 -v -run TestWailsChecklist ./cmd/isshoni-desktop
//
// W19 needs a production build started with the WEBVIEW2_* variables set by its parent, and the port and pipe names
// told to the test:
//
//	$env:WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS = '--remote-debugging-port=9333'
//	$env:WEBVIEW2_PIPE_FOR_SCRIPT_DEBUGGER = 'isshoni-test-w19'
//	$env:WEBVIEW2_X = '1'
//	$env:ISSHONI_CHECKLIST_W19_PORT = '9333'; $env:ISSHONI_CHECKLIST_W19_PIPE = 'isshoni-test-w19'
//	go test -tags "isshoni_hw production" -count=1 -v -run TestWailsChecklist ./cmd/isshoni-desktop
//
// The pinned Wails overwrites five WEBVIEW2_* variables in every build (at package init, before any test code, and
// again right before it creates the webview environment; main.go wailsWebView2Values), the two above included, so
// "no port, no pipe" alone would pass without the scrub. The row therefore also checks that WEBVIEW2_X, which Wails
// leaves alone, was there before the scrub and is gone after it, that no other WEBVIEW2_* value survives, and that no
// msedgewebview2.exe below this process has the port or the pipe on its command line. The same run without
// "production" is the control (WEBVIEW2_X stays). By hand, while the window is open:
//
//	Get-CimInstance Win32_Process -Filter "Name='msedgewebview2.exe'" | Select-Object ProcessId, ParentProcessId, CommandLine
//
// ISSHONI_CHECKLIST_OUT=<file> also writes the rows there. The run opens the app's window for a few seconds and
// closes it; nothing it writes names the machine or the user.

import (
	"bytes"
	"context"
	"embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/MoonWX/isshoni/internal/desktop/assets"
	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/protocol"
)

//go:embed testdata/checklist
var checklistFiles embed.FS

// checklistFailMessage is the error the bound Fail returns; the page must see it verbatim (W12).
const checklistFailMessage = `checklist: a Go error, verbatim <&> "quoted" ✓`

// checklistService is the bound service of the run (W11, W12, W21's control).
type checklistService struct {
	waitDone chan time.Duration // how long Wait ran before its context ended; -1: never cancelled
}

// Echo returns v.
func (s *checklistService) Echo(v string) string { return v }

// Fail returns checklistFailMessage as an error.
func (s *checklistService) Fail() error { return errors.New(checklistFailMessage) }

// Wait blocks until its context ends (the page cancels the call) or 10 s pass.
func (s *checklistService) Wait(ctx context.Context) error {
	start := time.Now()
	select {
	case <-ctx.Done():
		s.waitDone <- time.Since(start)
		return ctx.Err()
	case <-time.After(10 * time.Second):
		s.waitDone <- -1
		return errors.New("checklist: never cancelled")
	}
}

// wailsRequest is one request under /wails/ as the outermost middleware saw it, with the Guard's answer.
type wailsRequest struct {
	Method, Path string
	Origin       []string
	SecFetchSite string
	Status       int
}

type checklist struct {
	app     *application.App
	shell   *wailsShell
	svc     *checklistService
	dataDir string
	csp     string // the asset handler's CSP header, for the header-only page
	// visibleAtStart is the window's state when the page started, before it tried Window.Hide (a locked or
	// asleep session can leave a window not visible from the start).
	visibleAtStart atomic.Bool

	mu      sync.Mutex
	seen    []wailsRequest
	reports map[string]json.RawMessage
	arrived chan struct{} // gets a value whenever a report arrives
}

var theChecklist *checklist

// envBeforeScrub is the environment TestMain found, after Wails' package init and before the scrub (W19).
var envBeforeScrub []string

func TestMain(m *testing.M) {
	envBeforeScrub = os.Environ()
	for _, name := range scrubEnv(os.Environ(), flavor.production) {
		_ = os.Unsetenv(name)
	}
	c, err := newChecklist()
	if err != nil {
		fmt.Fprintln(os.Stderr, "checklist:", err)
		os.Exit(2)
	}
	theChecklist = c
	go func() {
		// os.Exit ends the Wails app too: Quit's return isn't the same on every OS.
		os.Exit(m.Run())
	}()
	if err := c.app.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "checklist: Wails stopped:", err)
		os.Exit(2)
	}
	select {} // the window was closed by hand; the test times out and exits
}

func newChecklist() (*checklist, error) {
	dist, err := fs.Sub(checklistFiles, "testdata/checklist")
	if err != nil {
		return nil, err
	}
	dataDir, err := checklistDataDir()
	if err != nil {
		return nil, err
	}
	c := &checklist{
		svc:     &checklistService{waitDone: make(chan time.Duration, 1)},
		dataDir: dataDir,
		reports: map[string]json.RawMessage{},
		arrived: make(chan struct{}, 16),
	}
	handler, err := assets.New(assets.Options{
		Dist: dist, Bridge: bridgeConfig(appFacts(), nil), Origin: assetOrigin, Dev: insecureLoopback(),
	})
	if err != nil {
		return nil, err
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, assetOrigin+"/", nil))
	c.csp = rec.Header().Get("Content-Security-Policy")

	guard := assetGuard()
	middleware := func(next http.Handler) http.Handler { return c.record(guard(next)) }
	opts := appOptions(c.routes(handler), middleware, dataDir, logx.New(logx.Options{}).With("component", "wails"))
	opts.Services = []application.Service{application.NewService(c.svc)}
	c.app = application.New(opts)
	c.shell = newShell(c.app, mainWindowOptions())
	return c, nil
}

// record is the outermost middleware: it notes every /wails/ request and the status the Guard (or Wails) gave it.
func (c *checklist) record(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/wails") {
			next.ServeHTTP(w, r)
			return
		}
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		c.mu.Lock()
		c.seen = append(c.seen, wailsRequest{Method: r.Method, Path: r.URL.Path, Origin: r.Header.Values("Origin"),
			SecFetchSite: r.Header.Get("Sec-Fetch-Site"), Status: sw.status})
		c.mu.Unlock()
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// routes serves the checklist's own endpoints under /__checklist/ and passes everything else to the asset handler.
func (c *checklist) routes(assetHandler http.Handler) http.Handler {
	rt := os.DirFS(filepath.Join("..", "..", "web", "node_modules", "@wailsio", "runtime", "dist"))
	typ := reflect.TypeFor[checklistService]()
	fqn := func(method string) string { return typ.PkgPath() + "." + typ.Name() + "." + method }
	mux := http.NewServeMux()
	js := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			b, err := checklistFiles.ReadFile("testdata/checklist/" + name)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			_, _ = w.Write(b)
		}
	}
	mux.Handle("GET /__checklist/page.js", js("page.js"))
	mux.Handle("GET /__checklist/w3a.js", js("w3a.js"))
	mux.Handle("GET /__checklist/w3b.js", js("w3b.js"))
	mux.HandleFunc("GET /__checklist/w3", func(w http.ResponseWriter, _ *http.Request) {
		b, _ := checklistFiles.ReadFile("testdata/checklist/w3.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", c.csp)
		_, _ = w.Write(b)
	})
	mux.HandleFunc("GET /__checklist/config", func(w http.ResponseWriter, _ *http.Request) {
		c.visibleAtStart.Store(c.shell.WindowVisible())
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"fqn": map[string]string{"echo": fqn("Echo"), "fail": fqn("Fail"), "wait": fqn("Wait")},
		})
	})
	mux.HandleFunc("GET /__checklist/rt/{file...}", func(w http.ResponseWriter, r *http.Request) {
		b, err := fs.ReadFile(rt, r.PathValue("file"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		_, _ = w.Write(b) //nolint:gosec // G705: the npm runtime's own files, for the checklist page
	})
	mux.HandleFunc("GET /__checklist/tone.wav", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(toneWAV())
	})
	mux.HandleFunc("GET /__checklist/wait-status", func(w http.ResponseWriter, _ *http.Request) {
		res := map[string]any{"cancelled": false}
		select {
		case d := <-c.svc.waitDone:
			res["cancelled"], res["afterMs"] = d >= 0, d.Milliseconds()
		case <-time.After(5 * time.Second):
			res["timeout"] = true
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	})
	mux.HandleFunc("POST /__checklist/emit-big", func(w http.ResponseWriter, _ *http.Request) {
		c.shell.Emit("checklist:big", strings.Repeat("x", 12<<10))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /__checklist/report", func(w http.ResponseWriter, r *http.Request) {
		var body json.RawMessage
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		c.reports[r.URL.Query().Get("step")] = body
		c.mu.Unlock()
		select {
		case c.arrived <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/__checklist/") {
			mux.ServeHTTP(w, r)
			return
		}
		assetHandler.ServeHTTP(w, r)
	})
}

// toneWAV is 0.3 s of a quiet 440 Hz tone, 8 kHz 16-bit mono (W13's autoplay check).
func toneWAV() []byte {
	const rate, n = 8000, 2400
	var data bytes.Buffer
	for i := range n {
		s := int16(800 * math.Sin(2*math.Pi*440*float64(i)/rate))
		_ = binary.Write(&data, binary.LittleEndian, s)
	}
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+data.Len())) //nolint:gosec // G115: 4.8 KB
	b.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(1), uint32(rate), uint32(rate * 2), uint16(2), uint16(16)} {
		_ = binary.Write(&b, binary.LittleEndian, v)
	}
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(data.Len())) //nolint:gosec // G115: 4.8 KB
	b.Write(data.Bytes())
	return b.Bytes()
}

// wait returns the report of step, failing the test on a "fatal" report or after timeout.
func (c *checklist) wait(t *testing.T, step string, timeout time.Duration) json.RawMessage {
	t.Helper()
	deadline := time.After(timeout)
	for {
		c.mu.Lock()
		r, ok := c.reports[step]
		fatal, failed := c.reports["fatal"]
		c.mu.Unlock()
		if failed {
			c.mu.Lock()
			seen := slices.Clone(c.seen)
			c.mu.Unlock()
			t.Fatalf("the checklist page failed: %s\n/wails/ requests so far: %+v", fatal, seen)
		}
		if ok {
			return r
		}
		select {
		case <-c.arrived:
		case <-time.After(200 * time.Millisecond):
		case <-deadline:
			t.Fatalf("no %q report from the checklist page within %s", step, timeout)
		}
	}
}

// settled is page.js's settle result.
type settled struct {
	Resolved bool   `json:"resolved"`
	Value    any    `json:"value"`
	Name     string `json:"name"`
	Message  string `json:"message"`
}

type rawResult struct {
	Status int    `json:"status"`
	Text   string `json:"text"`
}

type mainReport struct {
	W1     struct{ Origin string }        `json:"W1"`
	W2     struct{ IsSecureContext bool } `json:"W2"`
	Bridge struct {
		Present, Frozen bool
		V               int
		OS              string
	} `json:"bridge"`
	W3meta struct {
		InlineRan  bool
		Violations []string
	} `json:"W3meta"`
	Runtime struct{ From string } `json:"runtime"`
	W4      struct {
		RuntimeJS, CustomJSHead, EventPayload, Stream int
	} `json:"W4"`
	Echo settled `json:"echo"`
	W12  settled `json:"W12"`
	W11  struct {
		Call settled
		Go   struct {
			Cancelled bool
			AfterMs   int64
			Timeout   bool
		}
	} `json:"W11"`
	BigEvent struct {
		Delivered bool
		Size      int
	} `json:"bigEvent"`
	W21runtime map[string]settled             `json:"W21runtime"`
	Visibility struct{ Before, After string } `json:"visibility"`
	W21raw     map[string]rawResult           `json:"W21raw"`
	W13        struct {
		Autoplay bool
		Error    string
	} `json:"W13"`
	Stray []string `json:"stray"`
}

// row is one line of the "Wails checklist" table.
type row struct {
	id, result, detail string
}

func TestWailsChecklist(t *testing.T) {
	c := theChecklist
	var m mainReport
	if err := json.Unmarshal(c.wait(t, "main", 90*time.Second), &m); err != nil {
		t.Fatal(err)
	}
	var w3 struct {
		InlineRan  bool
		Violations []string
	}
	if err := json.Unmarshal(c.wait(t, "w3", 20*time.Second), &w3); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	seen := slices.Clone(c.seen)
	c.mu.Unlock()

	var rows []row
	check := func(id string, ok bool, detail string, args ...any) {
		res := "pass"
		if !ok {
			res = "FAIL"
			t.Errorf("%s: "+detail, append([]any{id}, args...)...)
		}
		rows = append(rows, row{id, res, fmt.Sprintf(detail, args...)})
	}
	info := func(id, result, detail string, args ...any) {
		rows = append(rows, row{id, result, fmt.Sprintf(detail, args...)})
	}

	check("W1", m.W1.Origin == assetOrigin && slices.Contains(protocol.DesktopOrigins(), m.W1.Origin),
		"location.origin %q, the asset origin %q, in protocol.DesktopOrigins()", m.W1.Origin, assetOrigin)
	check("W2", m.W2.IsSecureContext, "window.isSecureContext %v", m.W2.IsSecureContext)
	check("bridge.js", m.Bridge.Present && m.Bridge.Frozen && m.Bridge.V == 1 && m.Bridge.OS == string(clientOS),
		"window.__ISSHONI_DESKTOP__ present %v, frozen %v, v %d, os %q", m.Bridge.Present, m.Bridge.Frozen,
		m.Bridge.V, m.Bridge.OS)
	check("W3", !m.W3meta.InlineRan && len(m.W3meta.Violations) > 0 && !w3.InlineRan && len(w3.Violations) > 0,
		"an inline script is refused with header and <meta> (ran %v, violations %v) and with the header alone "+
			"(ran %v, violations %v)", m.W3meta.InlineRan, m.W3meta.Violations, w3.InlineRan, w3.Violations)

	var posts, posted, getsNoOrigin []string
	for _, s := range seen {
		switch {
		case s.Method == http.MethodPost && s.Path == "/wails/runtime":
			posts = append(posts, fmt.Sprint(s.Status))
			posted = append(posted, fmt.Sprintf("Origin %q", s.Origin))
		case s.Method == http.MethodGet || s.Method == http.MethodHead:
			getsNoOrigin = append(getsNoOrigin, fmt.Sprintf("%s %s: Origin %q, Sec-Fetch-Site %q, %d", s.Method,
				s.Path, s.Origin, s.SecFetchSite, s.Status))
		}
	}
	slices.Sort(posted)
	check("W4", len(posts) > 0, "AssetOptions.Middleware saw %d POST /wails/runtime (%s); same-origin GETs and "+
		"HEADs: %s; page statuses runtime.js %d, HEAD custom.js %d, eventpayload %d, stream %d", len(posts),
		strings.Join(slices.Compact(posted), ", "), strings.Join(getsNoOrigin, "; "), m.W4.RuntimeJS,
		m.W4.CustomJSHead, m.W4.EventPayload, m.W4.Stream)

	h := c.shell.MainWindowHandle()
	check("W10", isNativeWindow(h), "MainWindowHandle() is a window handle: %v", isNativeWindow(h))

	check("W11", m.W11.Go.Cancelled && m.W11.Go.AfterMs < 2000 && !m.W11.Call.Resolved,
		"cancelling the JS call ended the Go context after %d ms (cancelled %v, timeout %v); the call rejected with %s",
		m.W11.Go.AfterMs, m.W11.Go.Cancelled, m.W11.Go.Timeout, m.W11.Call.Name)
	check("W12", !m.W12.Resolved && m.W12.Message == checklistFailMessage,
		"the promise rejected (%s) with message %q", m.W12.Name, m.W12.Message)

	if runtime.GOOS == "windows" {
		entries, _ := os.ReadDir(c.dataDir)
		check("W13", m.W13.Autoplay && len(entries) > 0, "audio played without a click: %v (%s); the user data "+
			"folder got %d entries", m.W13.Autoplay, m.W13.Error, len(entries))
	} else {
		info("W13", "n/a", "WebView2 only; WKWebView autoplay without a click: %v (%s)", m.W13.Autoplay, m.W13.Error)
	}

	if port, pipe := os.Getenv("ISSHONI_CHECKLIST_W19_PORT"), os.Getenv("ISSHONI_CHECKLIST_W19_PIPE"); port != "" &&
		pipe != "" {
		w19(t, port, pipe, check)
	} else {
		info("W19", "not run", "needs -tags \"isshoni_hw production\" with WEBVIEW2_* and ISSHONI_CHECKLIST_W19_* set")
	}

	var refusedOK []string
	allRefused := true
	for name, r := range m.W21runtime {
		if r.Resolved {
			allRefused = false
			refusedOK = append(refusedOK, name+" RESOLVED")
		}
	}
	raw := m.W21raw
	rawOK := raw["callBody"].Status != http.StatusForbidden && raw["callQuery"].Status != http.StatusForbidden &&
		raw["openURLBody"].Status == http.StatusForbidden && raw["openURLQuery"].Status == http.StatusForbidden &&
		raw["windowHideQuery"].Status == http.StatusForbidden && raw["chunked"].Status == http.StatusForbidden &&
		raw["oversize"].Status == http.StatusRequestEntityTooLarge && raw["unparsable"].Status == http.StatusForbidden
	// A simple request (text/plain, no x-wails-client-id): WebView2 sends the page's Origin, so the Guard lets it
	// through; WKWebView sends no Origin, and GuardWKWebView then needs the runtime's header.
	if runtime.GOOS == "windows" {
		rawOK = rawOK && raw["simpleCall"].Status != http.StatusForbidden && raw["simpleCall"].Status != 0
	} else {
		rawOK = rawOK && raw["simpleCall"].Status == http.StatusForbidden
	}
	// The window's own state, not document.visibilityState (a window behind others reads "hidden" on macOS):
	// Window.Hide from the page must not have changed it.
	visible := c.shell.WindowVisible()
	check("W21", allRefused && rawOK && m.Echo.Resolved && m.Echo.Value == "checklist" && visible == c.visibleAtStart.Load(),
		"runtime objects refused: %d of %d %v; bound Echo answered %v; raw: Call body %d, Call query %d, "+
			"Browser.OpenURL body %d, query %d, Window.Hide query %d, chunked %d, 70 KB %d, unparsable %d, Call as a "+
			"simple request %d; the window visible before %v, after %v (page visibilityState %s, then %s)",
		len(m.W21runtime)-len(refusedOK), len(m.W21runtime), refusedOK, m.Echo.Value, raw["callBody"].Status,
		raw["callQuery"].Status, raw["openURLBody"].Status, raw["openURLQuery"].Status, raw["windowHideQuery"].Status,
		raw["chunked"].Status, raw["oversize"].Status, raw["unparsable"].Status, raw["simpleCall"].Status,
		c.visibleAtStart.Load(), visible, m.Visibility.Before, m.Visibility.After)
	info("events", map[bool]string{true: "pass", false: "FAIL"}[m.BigEvent.Delivered],
		"a 12 KiB Go event (fetched from /wails/eventpayload/) reached the page: %v", m.BigEvent.Delivered)
	info("runtime", "info", "the page used the %s runtime; stray page errors: %q", m.Runtime.From, m.Stray)

	var out strings.Builder
	fmt.Fprintf(&out, "Wails checklist, %s, Wails v3.0.0-beta.34, build tags production=%v dev=%v e2e=%v\n\n",
		webviewName(), flavor.production, flavor.dev, flavor.e2e)
	out.WriteString("| Row | Result | What the run saw |\n|---|---|---|\n")
	for _, r := range rows {
		fmt.Fprintf(&out, "| %s | %s | %s |\n", r.id, r.result, strings.ReplaceAll(r.detail, "|", "\\|"))
	}
	t.Log("\n" + out.String())
	if path := os.Getenv("ISSHONI_CHECKLIST_OUT"); path != "" {
		if err := os.WriteFile(path, []byte(out.String()), 0o600); err != nil { //nolint:gosec // G703: the caller's file
			t.Error(err)
		}
	}
}

// w19 decides W19 (03 §3.2, §5.4): in a production run the scrub removed every WEBVIEW2_* the parent set and the
// webview opened no debugging port or pipe; without production it is the control run, where WEBVIEW2_X stays.
func w19(t *testing.T, port, pipe string, check func(id string, ok bool, detail string, args ...any)) {
	d := net.Dialer{Timeout: time.Second}
	conn, err := d.DialContext(t.Context(), "tcp", net.JoinHostPort("127.0.0.1", port)) //nolint:gosec // G704: the caller's loopback port
	if err == nil {
		_ = conn.Close()
	}
	portOpen, pipeOpen := err == nil, pipeExists(pipe)
	xBefore := slices.ContainsFunc(envBeforeScrub, func(kv string) bool { return strings.HasPrefix(kv, "WEBVIEW2_X=") })
	_, xAfter := os.LookupEnv("WEBVIEW2_X")
	// What a production scrub would still remove now: nothing, after a production scrub.
	left := slices.DeleteFunc(scrubEnv(os.Environ(), true), func(name string) bool {
		return !strings.HasPrefix(strings.ToUpper(name), "WEBVIEW2_")
	})
	cmdlines, cmdErr := webviewCommandLines()
	var carrying int
	for _, cl := range cmdlines {
		if strings.Contains(cl, "--remote-debugging-port") || strings.Contains(cl, pipe) {
			carrying++
		}
	}
	// On Windows the command lines must have been read; macOS has no WebView2 processes.
	cmdOK := cmdErr == nil && carrying == 0 && (runtime.GOOS != "windows" || len(cmdlines) > 0)
	if !flavor.production {
		check("W19", xBefore && xAfter, "control run without production: WEBVIEW2_X before the scrub %v, after %v "+
			"(it stays without the scrub); WEBVIEW2_* a production scrub would remove: %v; debugging port %s open: "+
			"%v; pipe exists: %v; %d of %d msedgewebview2.exe command lines carry the port or pipe (%v) - Wails "+
			"overwrites WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS and WEBVIEW2_PIPE_FOR_SCRIPT_DEBUGGER in every build",
			xBefore, xAfter, left, port, portOpen, pipeOpen, carrying, len(cmdlines), cmdErr)
		return
	}
	check("W19", xBefore && !xAfter && len(left) == 0 && !portOpen && !pipeOpen && cmdOK,
		"WEBVIEW2_X before the scrub %v, after %v; other WEBVIEW2_* left: %v; debugging port %s open: %v; pipe "+
			"exists: %v; %d of %d msedgewebview2.exe command lines carry the port or pipe (%v)", xBefore, xAfter,
		left, port, portOpen, pipeOpen, carrying, len(cmdlines), cmdErr)
}

// webviewName names the run's OS and webview for the table's title.
func webviewName() string {
	if runtime.GOOS == "windows" {
		return "Windows (WebView2)"
	}
	return "macOS (WKWebView, the dev build's webview)"
}
