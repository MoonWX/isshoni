// Command e2ehost is the "desktop in Chrome" harness (docs/m2/03-desktop-app.md §22.4 A, D14): the desktop app's real
// Go parts (internal/desktop/app, the bound services of internal/desktop/bridge, 02's servers and managers, 05's
// audio service) on 01's fake engine, serving the bundled SPA to Playwright's Chrome over an HTTP transport instead
// of Wails. It is built for the e2e job only (DESKTOP_E2E_BIN, web/e2e/fixtures.ts) and never shipped.
//
// The page is served at the origin http://wails.localhost (protocol.DesktopAssetOriginWindows): the spec's Chrome
// maps that host to this process with --host-resolver-rules, so the unmodified e2e server accepts the page as a
// desktop origin (bearer-only /ws and 02's CORS). What it serves:
//
//	GET  /, /assets/…, /desktop/bridge.js   internal/desktop/assets over web.Dist(), with bridge.js transport "http"
//	POST /__bridge/call                     a bound method (03 §7.4's wire)
//	GET  /__bridge/events                   the app's events as an event stream
//	POST /wails/runtime                     a stub behind the real assets.Guard: what passes the Guard is recorded
//	GET  /__e2e/state                       the recording Shell and OSHooks: window, URLs opened, toasts, hotkeys,
//	                                        keep-awake, pill, reloads, the tokens handed out (hashes), runtime calls
//	POST /__e2e/hotkey?id=pause|stop        a hotkey press
//	POST /__e2e/system?event=lock|unlock|suspend|resume|end-session|display-change   an OS event
//	POST /__e2e/args                        {"args": [...]}: a command line as a second instance would forward it
//
// Flags (e2e only; no other build has them): -listen (default 127.0.0.1:0), -data (default: a new temporary folder,
// removed at exit), -server <origin> (added and made active at start, which the add step would do in the app),
// -reload-delay (Shell.Reload waits that long), -lock-resume and -lock-stop (03 §11.6's 5 min and 30 min, for
// desktop-system.spec once the share controller takes them, README WA68). Arguments after the flags are the app's
// command line (App.Boot). The log is at debug level in <data>/logs/isshoni.log.
//
// Once it listens it prints one JSON line to stdout: {"addr": "127.0.0.1:<port>", "origin": …, "log": <file>}.
// SIGINT or SIGTERM stops it.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/MoonWX/isshoni/internal/client/credstore"
	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/desktop/app"
	"github.com/MoonWX/isshoni/internal/desktop/applog"
	"github.com/MoonWX/isshoni/internal/desktop/assets"
	"github.com/MoonWX/isshoni/internal/desktop/audio"
	"github.com/MoonWX/isshoni/internal/desktop/bridge"
	"github.com/MoonWX/isshoni/internal/desktop/i18n"
	"github.com/MoonWX/isshoni/internal/desktop/servers"
	"github.com/MoonWX/isshoni/internal/desktop/settings"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/fake"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/version"
	"github.com/MoonWX/isshoni/web"
)

// fakeAudioEnv names 05's fake audio scenario (05 §13.3), as in the dev and e2e builds of the app.
const fakeAudioEnv = "ISSHONI_FAKE_AUDIO"

// options are the harness's flags.
type options struct {
	listen      string
	dataDir     string
	server      string
	origin      string
	reloadDelay time.Duration
	lockResume  time.Duration
	lockStop    time.Duration
	args        []string
	dist        fs.FS // the bundled SPA; nil means web.Dist() (tests pass their own)
}

func main() {
	o, err := parseFlags(os.Args[1:], os.Stderr)
	if err != nil {
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, o, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "e2ehost:", err)
		stop()
		os.Exit(1)
	}
}

func parseFlags(args []string, errOut io.Writer) (options, error) {
	var o options
	fs := flag.NewFlagSet("e2ehost", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.StringVar(&o.listen, "listen", "127.0.0.1:0", "the loopback address to listen on")
	fs.StringVar(&o.dataDir, "data", "", "the data root (default: a temporary folder, removed at exit)")
	fs.StringVar(&o.server, "server", "", "a server origin to add and make active at start")
	fs.StringVar(&o.origin, "origin", protocol.DesktopAssetOriginWindows, "the asset origin the page is served at")
	fs.DurationVar(&o.reloadDelay, "reload-delay", 0, "how long Shell.Reload waits before it counts")
	fs.DurationVar(&o.lockResume, "lock-resume", 5*time.Minute, "a lock pause resumes on an unlock within this long")
	fs.DurationVar(&o.lockStop, "lock-stop", 30*time.Minute, "a lock pause stops the share after this long")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	o.args = fs.Args()
	host, _, err := net.SplitHostPort(o.listen)
	if err != nil || (host != "127.0.0.1" && host != "::1" && host != "localhost") {
		fmt.Fprintln(errOut, "e2ehost: -listen must be a loopback address with a port")
		return o, errors.New("bad -listen")
	}
	return o, nil
}

// host is the running harness.
type host struct {
	handler http.Handler
	rec     *recorder
	app     *app.App
	close   func()
}

// run starts the harness, prints its address and serves until ctx ends.
func run(ctx context.Context, o options, out io.Writer) error {
	if o.dataDir == "" {
		dir, err := os.MkdirTemp("", "isshoni-e2ehost-")
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(dir) }()
		o.dataDir = dir
	}
	level := new(slog.LevelVar)
	level.Set(slog.LevelDebug)
	logs, err := applog.Open(applog.Options{Dir: filepath.Join(o.dataDir, "logs"), Level: level, Mirror: os.Stderr})
	if err != nil {
		return err
	}
	defer func() { _ = logs.Close() }()
	log := logs.Logger()
	slog.SetDefault(log)

	h, err := newHost(ctx, o, log)
	if err != nil {
		return err
	}
	defer h.close()

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", o.listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: h.handler, ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	line, _ := json.Marshal(map[string]string{"addr": ln.Addr().String(), "origin": o.origin,
		"log": filepath.Join(o.dataDir, "logs", "isshoni.log")})
	if _, err := fmt.Fprintf(out, "%s\n", line); err != nil {
		return err
	}
	// The command line, as the first instance's own (03 §13.4 step 7).
	h.app.Boot(o.args, app.Pending{})
	select {
	case <-ctx.Done():
	case err := <-served:
		return err
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	return nil
}

// newHost wires the app's real parts on the fake engine, the recording shell and the HTTP transport.
func newHost(ctx context.Context, o options, log *slog.Logger) (*host, error) {
	rec := newRecorder(o.origin, o.reloadDelay)
	hub := newEventHub(log.With("component", "e2ehost"))
	rec.emit = hub.emit
	var closers []func()
	closeAll := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}
	fail := func(err error) (*host, error) {
		closeAll()
		return nil, err
	}

	cat, err := i18n.Load(web.DesktopCatalog(), log)
	if err != nil {
		return fail(err)
	}
	info := deviceauth.AppInfo{Version: version.Version(), ClientKind: protocol.ClientKindDesktop,
		OS: protocol.ClientOSWindows, DeviceName: "e2e PC"}
	var theApp atomic.Pointer[app.App]
	srv, err := deviceauth.OpenServers(deviceauth.ServersOptions{
		Dir:                   o.dataDir,
		Store:                 credstore.NewMemory(),
		App:                   info,
		AllowInsecureLoopback: true,
		OnState: func(s deviceauth.State) {
			if a := theApp.Load(); a != nil {
				a.OnAuthState(s)
			}
		},
		Logger: log,
	})
	if err != nil {
		return fail(err)
	}
	closers = append(closers, func() { _ = srv.Close() })
	if o.server != "" {
		actx, cancel := context.WithTimeout(ctx, 20*time.Second)
		_, err := srv.Add(actx, o.server)
		cancel()
		if err != nil {
			return fail(fmt.Errorf("-server: %w", err))
		}
	}
	st, err := settings.Open(settings.Options{Dir: o.dataDir, KnownServer: srv.Has, Logger: log})
	if err != nil {
		return fail(err)
	}
	closers = append(closers, func() { _ = st.Close() })

	// 01's fake engine as the app's dev and e2e builds open it (03 §22.4 A), and 05's real audio service on it.
	eng := fake.NewEngine(fake.EngineConfig{Mode: fake.Decodable, SourceSize: [2]int{640, 360},
		AudioScenario: os.Getenv(fakeAudioEnv)})
	closers = append(closers, func() { _ = eng.Close() })
	webviewData := filepath.Join(o.dataDir, "WebView2")
	audioSvc := audio.New(eng, settings.NewAudioStore(st, audio.DefaultSettings),
		audio.SelfInfo{PID: uint32(os.Getpid()), WebViewDataDir: webviewData}, log) //nolint:gosec // a pid fits
	a, err := app.New(app.Deps{
		Settings:         st,
		Servers:          srv,
		Engine:           eng,
		Audio:            audioSvc,
		Shell:            rec,
		OS:               rec,
		I18n:             cat,
		Log:              log,
		InsecureLoopback: true,
	})
	if err != nil {
		return fail(err)
	}
	theApp.Store(a)
	actx, cancelApp := context.WithCancel(context.Background())
	appDone := make(chan struct{})
	go func() {
		defer close(appDone)
		_ = a.Run(actx)
	}()
	closers = append(closers, func() {
		cancelApp()
		<-appDone
	})

	svcs, err := bridge.New(bridge.Deps{
		App:                   a,
		Servers:               bridge.ServersOf(srv),
		ProbeApp:              info,
		AllowInsecureLoopback: true,
		Audio:                 audioSvc,
		Settings:              st,
		Shell:                 rec,
		Emit:                  rec.Emit,
		Log:                   log,
	})
	if err != nil {
		return fail(err)
	}
	closers = append(closers, svcs.Close)

	dist := o.dist
	if dist == nil {
		dist = web.Dist()
	}
	assetHandler, err := assets.New(assets.Options{
		Dist:   dist,
		Bridge: bridgeConfig(srv),
		Origin: o.origin,
		Dev:    true,
	})
	if err != nil {
		return fail(fmt.Errorf("the web app (web/dist) is unusable; run task build:web first: %w", err))
	}
	d := newDispatcher(svcs, log.With("component", "e2ehost"))
	activeOrigin := func() string {
		if m := srv.Active(); m != nil {
			return m.State().Origin
		}
		return ""
	}
	d.before = func(service, method string) any {
		if service != "Auth" || method != "AccessToken" {
			return nil
		}
		return activeOrigin()
	}
	d.observe = func(service, method string, args []json.RawMessage, pre, result any) {
		if service != "Auth" || method != "AccessToken" {
			return
		}
		tok, ok := result.(*bridge.AccessToken)
		if !ok || tok == nil {
			return
		}
		var asked string
		if len(args) > 0 {
			_ = json.Unmarshal(args[0], &asked)
		}
		atCall, _ := pre.(string)
		atReturn := activeOrigin()
		sum := sha256.Sum256([]byte(tok.Token.Reveal()))
		rec.update(func(s *hostState) {
			s.Tokens = append(s.Tokens, tokenRecord{Origin: asked, ActiveOrigin: atCall, ActiveAtReturn: atReturn,
				SHA256: hex.EncodeToString(sum[:8])})
		})
	}

	mux := http.NewServeMux()
	mux.HandleFunc(callPath, sameOrigin(o.origin, d.serveCall))
	mux.HandleFunc(eventsPath, sameOrigin(o.origin, hub.serveEvents))
	mux.HandleFunc("/__e2e/state", sameOrigin(o.origin, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, rec.snapshot())
	}))
	mux.HandleFunc("/__e2e/hotkey", sameOrigin(o.origin, post(func(w http.ResponseWriter, r *http.Request) {
		id := app.HotkeyID(r.URL.Query().Get("id"))
		if id != app.HotkeyPause && id != app.HotkeyStop {
			http.Error(w, "id must be pause or stop", http.StatusBadRequest)
			return
		}
		deliver(w, rec.keys, id)
	})))
	mux.HandleFunc("/__e2e/system", sameOrigin(o.origin, post(func(w http.ResponseWriter, r *http.Request) {
		ev := app.SystemEvent(r.URL.Query().Get("event"))
		switch ev {
		case app.EventLock, app.EventUnlock, app.EventSuspend, app.EventResume, app.EventEndSession,
			app.EventDisplayChange:
		default:
			http.Error(w, "unknown event", http.StatusBadRequest)
			return
		}
		deliver(w, rec.events, ev)
	})))
	mux.HandleFunc("/__e2e/args", sameOrigin(o.origin, post(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Args []string `json:"args"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCallBody)).Decode(&body); err != nil {
			http.Error(w, "the body must be {\"args\": [...]}", http.StatusBadRequest)
			return
		}
		a.HandleArgs(body.Args)
		w.WriteHeader(http.StatusNoContent)
	})))
	mux.HandleFunc("/wails/runtime", rec.stubRuntime)
	mux.Handle("/wails/", http.NotFoundHandler())
	mux.Handle("/", assetHandler)

	return &host{handler: assets.Guard(o.origin)(mux), rec: rec, app: a, close: closeAll}, nil
}

// bridgeConfig is bridge.js's config in the harness: the HTTP transport, the active server and every listed server's
// derived id (03 §5.2).
func bridgeConfig(srv *deviceauth.Servers) func() assets.BridgeConfig {
	facts := assets.AppFacts{Version: version.Version(), OS: string(protocol.ClientOSWindows), OSBuild: osBuild,
		Arch: runtime.GOARCH}
	return func() assets.BridgeConfig {
		c := assets.BridgeConfig{V: assets.BridgeConfigVersion, Transport: "http", App: facts, ServerIDs: []string{},
			Dev: true}
		active := ""
		if m := srv.Active(); m != nil {
			active = m.State().Origin
		}
		for _, e := range srv.List() {
			id := servers.LocalID(e.Origin)
			c.ServerIDs = append(c.ServerIDs, id)
			if e.Origin == active {
				c.Server = &assets.ServerFacts{ID: id, Origin: e.Origin, Name: e.Name}
			}
		}
		return c
	}
}

// sameOrigin lets a request through when it comes from the asset origin, or from no browser page at all (the spec's
// own requests carry neither Origin nor Sec-Fetch-Site): a page of another origin gets 403, as the Guard does.
func sameOrigin(origin string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		o := r.Header.Get("Origin")
		site := r.Header.Get("Sec-Fetch-Site")
		if (o != "" && o != origin) || (o == "" && site != "" && site != "same-origin" && site != "none") {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// post refuses every method but POST.
func post(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next(w, r)
	}
}

// deliver puts v on ch, or answers 503 when nothing has taken the earlier ones.
func deliver[T any](w http.ResponseWriter, ch chan<- T, v T) {
	select {
	case ch <- v:
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "the app isn't reading these events", http.StatusServiceUnavailable)
	}
}

// stubRuntime is the stub /wails/runtime behind the real Guard (03 §22.4 A): what reaches it passed the Guard's
// allowlist. It records the object and method of each call (from the JSON body, or the query when the body is
// empty, as the pinned transport reads them) and answers a bound-method Call with null.
func (r *recorder) stubRuntime(w http.ResponseWriter, req *http.Request) {
	var call struct {
		Object *int `json:"object"`
		Method *int `json:"method"`
	}
	body, _ := io.ReadAll(http.MaxBytesReader(w, req.Body, maxCallBody))
	if len(body) > 0 {
		_ = json.Unmarshal(body, &call)
	} else {
		var ob, me int
		if _, err := fmt.Sscan(req.URL.Query().Get("object"), &ob); err == nil {
			call.Object = &ob
		}
		if _, err := fmt.Sscan(req.URL.Query().Get("method"), &me); err == nil {
			call.Method = &me
		}
	}
	rc := runtimeCall{Object: -1, Method: -1}
	if call.Object != nil {
		rc.Object = *call.Object
	}
	if call.Method != nil {
		rc.Method = *call.Method
	}
	r.update(func(s *hostState) { s.RuntimeCalls = append(s.RuntimeCalls, rc) })
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, "null")
}

var _ media.AudioController = (*fake.Engine)(nil)
