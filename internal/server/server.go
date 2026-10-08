package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/httpapi"
	"github.com/MoonWX/isshoni/internal/server/netx"
	"github.com/MoonWX/isshoni/internal/server/ops"
	"github.com/MoonWX/isshoni/internal/server/tlsmgr"
	"github.com/MoonWX/isshoni/internal/version"
)

// Deps are the server's test seams. The zero value of every field means the real implementation, so cmd/isshoni
// passes Deps{}.
type Deps struct {
	// Now is the clock of the time-dependent components. nil means time.Now.
	Now func() time.Time
	// STUN and Resolver are what public-address detection asks (04 §7.4). nil means netx.NewSTUNClient and
	// net.DefaultResolver. Resolver also answers the TLS manager's look at its own domain (04 §8.7). Off mode needs
	// no detection for its site; the wiring uses them there for the media addresses (README S59).
	STUN     netx.STUNClient
	Resolver netx.Resolver
	// ReleaseHTTP is the HTTP client of the release check (04 §11.5). nil means a client of the server's own.
	ReleaseHTTP *http.Client
	// PushSender (push.Sender, 04 §6) joins this struct with the push wiring (README S71); the push package is built
	// in README S32.

	// Host is the machine as the data-directory checks see it: container detection, the mount table and the cgroup
	// memory limit (04 §5.1). The zero value is the running process.
	Host config.Host
	// SPA is the built web app. nil means web.Dist(), the one embedded in the binary.
	SPA fs.FS
	// API and WS are the handlers mounted at /api/v1/ and GET /ws. nil means the real ones (03's httpapi.API and
	// 01's signal hub) once the wiring builds them (README S54); until then the router answers JSON 404 not_found
	// there. Tests pass stubs.
	API http.Handler
	WS  http.Handler
	// InProcess says the server shares its process with other code (tests, through servertest): it then leaves the
	// process-wide settings alone, the umask and the Go memory limit of 04 §5.1. The real server sets both.
	InProcess bool
}

// ShutdownReason says why the server stops (04 §6.4).
type ShutdownReason string

const (
	// ShutdownStop is SIGTERM or SIGINT: the process exits. systemd's restart also sends SIGTERM, so clients are
	// always told to expect the server back.
	ShutdownStop ShutdownReason = "stop"
	// ShutdownRestart is a restart the server asked for itself (rotate-secrets): Run returns ErrRestartRequested.
	ShutdownRestart ShutdownReason = "restart"
	// ShutdownRestore is the restart after a restore: Run returns ErrRestartRequested.
	ShutdownRestore ShutdownReason = "restore"
)

// restarts reports whether Run returns ErrRestartRequested after a shutdown for this reason.
func (r ShutdownReason) restarts() bool { return r == ShutdownRestart || r == ShutdownRestore }

// ErrRestartRequested is returned by Run after a full graceful shutdown for ShutdownRestart or ShutdownRestore.
// cmd/isshoni then re-execs the binary; the PID stays the same, so systemd and Docker see no exit (04 §6.5).
var ErrRestartRequested = errors.New("server: restart requested")

// ErrShutdownForced marks the error of a shutdown in which a step ran out of time and closed by force what it
// still had: a request that would not finish, say. The server is stopped and everything is released all the same.
// Shutdown returns such an error whatever the reason; Run returns it only after a stop, which makes it the one
// error of Run that is no failure: Shutdown has logged the warning, and cmd/isshoni exits 0 (04 §6.4 step 7).
var ErrShutdownForced = errors.New("server: shutdown finished by force")

// errStopped is returned by Start for a Server that was shut down, and by Run for one that was shut down before it
// ever served: one Server is one life.
var errStopped = errors.New("server: already shut down; build a new Server")

// NeedsOperator reports whether err, from New, Start or Run, is a refusal to start that a restart can't fix
// (04 §6.3): an invalid config, a container without a data volume, a data directory or lock file the process can't
// write, an admin socket directory it can't create, a corrupt or wrongly owned secrets.json. cmd/isshoni prints
// such an error (it carries its own fix line) and exits 78, on which systemd stops restarting. Any other error is a
// runtime error (exit 1): a busy port, a data directory locked by another isshoni (config.ErrDataDirLocked).
// config.ReasonOf(err) gives the reason code for the data-directory and secrets refusals.
//
// A certificate that can't be had is neither: the server starts, and is not ready until it has one (04 §6.2, §8.4).
func NeedsOperator(err error) bool {
	var ve *config.ValidationError
	return errors.Is(err, config.ErrNeedsOperator) || errors.As(err, &ve)
}

// Addrs are the addresses the server listens on, as bound: a configured port 0 (tests) shows as the port the
// kernel picked. Later slices add the ICE ports and the metrics listener.
type Addrs struct {
	// HTTP is the listener on listen.http: the app itself in off mode; in the other modes the plain-HTTP port,
	// which answers ACME http-01 challenges and redirects everything else to HTTPS (04 §8.3).
	HTTP net.Addr
	// HTTPS is the 443 multiplexer on listen.https: HTTPS and WSS, and ICE-TCP by the first byte (04 §7.2). nil in
	// off mode, where a proxy owns that port.
	HTTPS net.Addr
}

// lifecycle states of a Server; they only move forward.
type state uint8

const (
	stateNew      state = iota // New returned; nothing is open
	stateServing               // Start succeeded
	stateStopping              // Shutdown is running
	stateStopped               // Shutdown finished, or Start failed
)

// Budgets of the shutdown steps (04 §6.4). They add up to the default shutdown_timeout of 10 s, which bounds the
// whole shutdown whatever the steps would like.
const (
	hubShutdownBudget   = 2 * time.Second // step 3: the hub's server.shutdown and close 1012 (README S54)
	mediaShutdownBudget = 1 * time.Second // step 4: SFU.Close, then Transport.Close (README S59)
	httpShutdownBudget  = 5 * time.Second // step 5: http.Server.Shutdown on every server
	tailShutdownBudget  = 2 * time.Second // step 6: transfer counters, push queue, admin socket, store
)

// Server is one run of the isshoni server: New, then Run (or Start and later Shutdown). A Server is not reused
// after its shutdown; a restart builds a new one (04 §6.5).
type Server struct {
	cfg      config.Config // New's copy; Start replaces a port 0 in listen.http by the bound port
	warnings []config.Problem
	log      *slog.Logger
	deps     Deps

	// life guards the lifecycle state and what Start fills in for the accessors. Start holds it for the whole
	// startup, so a Shutdown that arrives meanwhile waits for the startup to finish.
	life        sync.Mutex
	state       state
	served      bool           // Start succeeded once: the shutdown that followed, or follows, is Run's to report
	reason      ShutdownReason // of the first Shutdown call
	shutdownErr error          // set before done is closed
	site        config.Site
	addrs       Addrs

	stopping chan struct{} // closed when a shutdown begins, also when the server is given up before it served
	done     chan struct{} // closed when the shutdown is complete and everything is released

	failOnce sync.Once
	failed   chan struct{} // closed when a listener fails while the server runs
	failErr  error         // set before failed is closed

	// What Start builds, in startup order; Shutdown releases it in reverse (04 §6.4).
	lock     *config.DataDirLock
	secrets  *config.SecretStore // read by the wiring (README S54): the session, invite and resume keys, the VAPID pair
	public   netx.PublicAddrs    // the detected public addresses; zero in off mode until the SFU needs them (README S59)
	mux      *netx.PortMux       // the 443 multiplexer on listen.https; nil in off mode
	httpLn   net.Listener        // listen.http
	tls      *tlsmgr.Manager     // the certificate, in every mode (off: a manager with nothing to do)
	health   *ops.Health
	gate     *httpapi.Gate
	httpSrv  *http.Server   // the main server: on httpLn in off mode, on the multiplexer's TLS side otherwise
	plainSrv *http.Server   // the port 80 server on httpLn (04 §8.3); nil in off mode
	pending  pendingConns   // the HTTP connections without a request yet; the shutdown closes them
	serving  sync.WaitGroup // the Serve goroutines; Shutdown waits for them

	// hookDraining, set by tests (under life), runs in Shutdown once readiness is off and the gate is closed, while
	// the listeners still accept: the window in which the hub and the SFU say goodbye (steps 3 and 4).
	hookDraining func()
	// hookDetect, set by tests before Start, stands in for the public-address detection (04 §7.4), which reads
	// the machine's interfaces: a test says what the server finds.
	hookDetect func() netx.PublicAddrs
}

// New checks cfg and builds the server. It opens nothing and starts nothing; Run or Start does. cfg is copied: later
// changes to it don't reach the server. A nil log means slog.Default().
//
// Its error is a *config.ValidationError when cfg has errors (Load's, or those of values changed since); it
// satisfies NeedsOperator.
func New(cfg *config.Config, log *slog.Logger, deps Deps) (*Server, error) {
	if cfg == nil {
		return nil, errors.New("server: New: nil config")
	}
	if log == nil {
		log = slog.Default()
	}
	problems, warnings := configProblems(cfg)
	if len(problems) > len(warnings) {
		return nil, &config.ValidationError{Problems: problems}
	}
	return &Server{
		cfg:      *cfg,
		warnings: warnings,
		log:      log,
		deps:     deps,
		stopping: make(chan struct{}),
		done:     make(chan struct{}),
		failed:   make(chan struct{}),
	}, nil
}

// configProblems returns every problem of cfg, errors first, and its warnings alone: those that Load found (a
// malformed value that Load replaced by the default is only there) and those of the current values (a caller may
// have changed the config since).
func configProblems(cfg *config.Config) (all, warnings []config.Problem) {
	all = cfg.Problems()
	for _, p := range cfg.Validate() {
		if !slices.Contains(all, p) {
			all = append(all, p)
		}
	}
	slices.SortStableFunc(all, func(a, b config.Problem) int {
		return strings.Compare(a.Severity, b.Severity) // "error" sorts before "warning"
	})
	for _, p := range all {
		if p.Severity != config.SeverityError {
			warnings = append(warnings, p)
		}
	}
	return all, warnings
}

// Site returns how the outside world reaches this server (04 §4.4). It is known once Start has returned (the zero
// Site before): the site of a config with listen.http on port 0 carries the port that was bound.
func (s *Server) Site() config.Site {
	s.life.Lock()
	defer s.life.Unlock()
	return s.site
}

// Addrs returns the addresses the server listens on. They are known once Start has returned (zero before).
func (s *Server) Addrs() Addrs {
	s.life.Lock()
	defer s.life.Unlock()
	return s.addrs
}

// Start runs the startup sequence of 04 §6.1 from step 2 on (step 1, loading the config and building the logger, is
// cmd/isshoni's) and returns once the listeners serve. ctx bounds the startup only; it does not stop the server
// later. After a nil return the caller owns a running server: Run waits on it, Shutdown stops it. Run reports how
// the server ended even when a shutdown began before Run was called.
//
// On an error everything Start had opened is released again, the server counts as shut down, and NeedsOperator
// tells whether a restart could help. Start on a running server does nothing and returns nil; after a shutdown it
// returns an error.
func (s *Server) Start(ctx context.Context) (err error) {
	s.life.Lock()
	defer s.life.Unlock()
	switch s.state {
	case stateNew:
	case stateServing:
		return nil
	default:
		return errStopped
	}
	defer func() {
		if err != nil {
			s.release(ctx)
			s.state = stateStopped
			close(s.stopping)
			close(s.done)
		}
	}()

	s.logConfigWarnings(ctx)

	// Step 2: umask, the data directory checks, the data-directory lock, the memory limit (04 §5.1).
	if !s.deps.InProcess {
		config.SetPrivateUmask()
	}
	if err := config.PrepareDataDir(ctx, &s.cfg, s.deps.Host, s.log); err != nil {
		return err
	}
	paths := s.cfg.Paths()
	if s.lock, err = config.LockDataDir(ctx, paths); err != nil {
		return err
	}
	if !s.deps.InProcess {
		config.ApplyMemoryLimit(s.deps.Host, s.log)
	}

	// Step 3: secrets.json, created on first run. A corrupt or wrongly owned file is an *OperatorError.
	if s.secrets, err = config.OpenSecrets(paths.Secrets, s.log); err != nil {
		return err
	}

	// Step 4 (README S54): open the store, then pin the policy keys the config sets (cfg.IsSet, 04 §4.6).

	// Step 5: detect the public addresses (04 §7.4; at most 5 s). The TLS modes need them now: ip mode's site and
	// certificate are the address, manual mode's site is it too when there is no domain, and auto mode compares its
	// domain's DNS records with it. Off mode takes its site from public_url, or localhost in dev; the media
	// addresses there come with the SFU (README S59).
	mode := s.cfg.EffectiveTLSMode()
	switch {
	case mode == config.TLSOff:
	case s.hookDetect != nil:
		s.public = s.hookDetect()
	default:
		s.public = s.detectPublicAddrs(ctx)
	}

	// Step 6: bind the listeners: the 443 multiplexer (not in off mode), then listen.http. The admin socket comes
	// with S43, the ICE transports with the SFU (S59).
	if mode != config.TLSOff {
		if s.mux, err = s.listenHTTPS(); err != nil {
			return err
		}
		s.addrs.HTTPS = s.mux.Addr()
		s.cfg.Listen.HTTPS = withBoundPort(s.cfg.Listen.HTTPS, s.addrs.HTTPS)
	}
	if s.httpLn, err = listenHTTP(ctx, s.cfg.Listen.HTTP, mode); err != nil {
		return err
	}
	s.addrs.HTTP = s.httpLn.Addr()
	// With port 0 (tests) the site needs the ports the kernel picked.
	s.cfg.Listen.HTTP = withBoundPort(s.cfg.Listen.HTTP, s.addrs.HTTP)
	if s.site, err = s.newSite(mode); err != nil {
		return err
	}

	// Step 7: the TLS manager. The certificate arrives in the background (04 §8): the listeners serve before it is
	// there, and the readiness check "tls" says when it is.
	if s.tls, err = tlsmgr.New(s.tlsOptions(mode, paths)); err != nil {
		return fmt.Errorf("server: %w", err)
	}
	if err := s.tls.Start(ctx); err != nil {
		return fmt.Errorf("server: %w", err)
	}

	// Step 8: build the components and register the readiness checks (04 §6.2). db, media and signal come with
	// their components.
	s.health = ops.NewHealth()
	s.health.SetClientIP(httpapi.ClientIP)
	s.health.AddCheck("tls", s.tls.Ready) // off mode: always ready, the proxy has the certificate
	if siteIsAddress(&s.cfg, mode) {
		known := s.site.Origin != ""
		s.health.AddCheck("public_ip", func() (bool, string) {
			if !known {
				return false, "no public IP address found; set public_ip and restart"
			}
			return true, ""
		})
	}
	s.gate = &httpapi.Gate{}
	s.httpSrv = newMainServer(s.newRouter().Handler(), s.log, &s.pending)
	if mode != config.TLSOff {
		s.httpSrv.TLSConfig = s.tls.TLSConfig()
		s.plainSrv = newPlainServer(s.tls.HTTPHandler(nil), s.log, &s.pending)
	}
	if s.cfg.Metrics.Enabled {
		s.log.Warn("metrics.enabled is set, but this build has no metrics endpoint yet; nothing listens on metrics.listen",
			slog.String("component", "ops"))
	}

	// Step 9: start the HTTP servers. The multiplexer's ICE side, s.mux.ICE(), has no reader yet: netx.NewTransport
	// takes the multiplexer when the SFU runs in the server (README S59), and no candidate names port 443 before
	// that. An ICE-TCP connection that arrives all the same waits in the multiplexer's queue, within its limits,
	// until its client gives up or the server stops.
	if mode == config.TLSOff {
		s.serve(s.httpSrv, s.httpLn)
	} else {
		s.serveTLS(s.httpSrv, s.mux.TLS())
		s.serve(s.plainSrv, newLimitListener(s.httpLn, s.cfg.Limits.ConnsPerIP))
	}
	s.state, s.served = stateServing, true

	// Step 10: one line that says where the server is. The media ports join it with the SFU (README S59), and the
	// "finish setup" hint with auth (S54).
	listen := []any{slog.String("listen", s.addrs.HTTP.String())}
	if mode != config.TLSOff {
		listen = []any{slog.String("listen", s.addrs.HTTPS.String()), slog.String("listen_http", s.addrs.HTTP.String())}
	}
	if s.site.Origin == "" {
		s.log.Warn(fmt.Sprintf("isshoni %s started without a public IP address (tls=%s) and is not ready: set public_ip "+
			"to this server's public address, or set domain, and restart", version.Version(), s.site.TLSMode), listen...)
		return nil
	}
	s.log.Info(fmt.Sprintf("isshoni %s ready: %s (tls=%s)", version.Version(), s.site.Origin, s.site.TLSMode), listen...)
	return nil
}

// logConfigWarnings writes the config's warnings to the log, one line each with the key and the fix, so they reach
// the journal in the log's format (04 §4.5). Errors never get here: New refuses them.
func (s *Server) logConfigWarnings(ctx context.Context) {
	log := s.log.With(slog.String("component", "config"))
	for _, p := range s.warnings {
		line, _, _ := strings.Cut(p.String(), "\n") // "config warning: key = value (source) what is wrong."
		attrs := make([]slog.Attr, 0, 2)
		if p.Key != "" {
			attrs = append(attrs, slog.String("key", p.Key))
		}
		if p.Fix != "" {
			attrs = append(attrs, slog.String("fix", p.Fix))
		}
		log.LogAttrs(ctx, slog.LevelWarn, line, attrs...)
	}
}

// serve runs srv on ln until Shutdown. A Serve error other than the one Shutdown causes ends the server: Run then
// shuts down and returns it.
func (s *Server) serve(srv *http.Server, ln net.Listener) {
	s.serving.Go(func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.fail(fmt.Errorf("server: serving %s: %w", ln.Addr(), err))
		}
	})
}

// serveTLS is serve for the main server behind the 443 multiplexer: srv speaks TLS on ln with its TLSConfig, which
// carries the TLS manager's certificate (04 §7.2). net/http takes the smallest of the server's timeouts as the limit
// of a handshake: ReadHeaderTimeout, 10 s.
func (s *Server) serveTLS(srv *http.Server, ln net.Listener) {
	s.serving.Go(func() {
		if err := srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.fail(fmt.Errorf("server: serving %s: %w", ln.Addr(), err))
		}
	})
}

// fail records the first fatal error of a running server and wakes Run.
func (s *Server) fail(err error) {
	s.failOnce.Do(func() {
		s.log.Error("the server can't go on", logx.Err(err))
		s.failErr = err
		close(s.failed)
	})
}

// Run runs the server: Start (unless the caller already did), then it blocks until ctx is cancelled, a Shutdown
// from elsewhere begins (a restore or rotate-secrets through the admin socket), or a listener fails. In each case
// it finishes the graceful shutdown of 04 §6.4, within shutdown_timeout, before it returns. Its result says one
// thing, so that cmd/isshoni can map it to an exit code:
//   - nil after a stop (ctx cancelled: SIGTERM or SIGINT in cmd/isshoni);
//   - an error that wraps ErrShutdownForced after a stop that ran out of time and closed the rest by force. The
//     server is stopped all the same: exit 0;
//   - ErrRestartRequested after a shutdown for ShutdownRestart or ShutdownRestore, forced or not;
//   - Start's error when the server could not start (see NeedsOperator);
//   - the listener's error when one failed, whatever the shutdown after it had to do.
//
// What the shutdown itself ran into is Run's result only after a stop. In the other cases it is in the log, and
// Shutdown returns it to whoever asks.
//
// On a server that the caller started and that a Shutdown has stopped since, Run returns the same as if it had been
// waiting when that shutdown began.
func (s *Server) Run(ctx context.Context) error {
	if err := s.Start(ctx); err != nil {
		// errStopped from a server that has served is no start error: the caller started it, and a shutdown has
		// begun since. Run reports that shutdown below.
		if !errors.Is(err, errStopped) || !s.hasServed() {
			return err
		}
	}
	select {
	case <-ctx.Done():
	case <-s.stopping:
	case <-s.failed:
	}
	// The run context is gone or about to be; the shutdown gets shutdown_timeout of its own. If a shutdown is
	// already running or done, this waits for it and returns its result.
	err := s.Shutdown(context.WithoutCancel(ctx), ShutdownStop)

	select {
	case <-s.failed:
		return s.failErr
	default:
	}
	s.life.Lock()
	reason := s.reason
	s.life.Unlock()
	if reason.restarts() {
		return ErrRestartRequested
	}
	return err
}

// hasServed reports whether Start has succeeded on this server.
func (s *Server) hasServed() bool {
	s.life.Lock()
	defer s.life.Unlock()
	return s.served
}

// Shutdown stops the server gracefully, in the order of 04 §6.4, and returns when everything is closed and the
// data-directory lock is released. The whole shutdown takes at most shutdown_timeout (10 s), less if ctx ends
// sooner; a step that runs out of time closes what it has by force and the shutdown goes on, so Shutdown always
// leaves the server stopped. Its error then wraps ErrShutdownForced and names the steps that had to do so.
//
// reason is what Run reports afterwards: ErrRestartRequested for ShutdownRestart and ShutdownRestore; for
// ShutdownStop (an unknown reason counts as a stop) nil, or this shutdown's error. The first call decides; later
// and concurrent calls wait for that shutdown to finish, or for their own ctx, and return its result. On a server
// that never started it only marks the server as shut down.
//
// A request handler of this server that asks for a shutdown (the admin socket's restore and rotate-secrets) must not
// wait for it: its own request would hold the HTTP step until the budget runs out. It starts Shutdown in a
// goroutine and answers; Run then returns ErrRestartRequested.
func (s *Server) Shutdown(ctx context.Context, reason ShutdownReason) error {
	s.life.Lock()
	switch s.state {
	case stateNew:
		s.state, s.reason = stateStopped, reason
		close(s.stopping)
		close(s.done)
		s.life.Unlock()
		return nil
	case stateStopping, stateStopped:
		s.life.Unlock()
		select {
		case <-s.done:
			return s.shutdownErr
		case <-ctx.Done():
			return fmt.Errorf("server: waiting for the shutdown: %w", ctx.Err())
		}
	}
	switch reason {
	case ShutdownStop, ShutdownRestart, ShutdownRestore:
	default:
		s.log.Error("unknown shutdown reason, stopping", slog.String("reason", string(reason)))
		reason = ShutdownStop
	}
	s.state, s.reason = stateStopping, reason
	draining := s.hookDraining
	close(s.stopping)
	s.life.Unlock()

	ctx, cancel := context.WithTimeout(ctx, s.cfg.ShutdownTimeout.Duration)
	defer cancel()
	start := time.Now()
	err := s.shutdown(ctx, reason, draining)
	took := slog.Duration("took", time.Since(start))
	switch {
	case err == nil:
		s.log.Info("shutdown complete", took)
	case errors.Is(err, ErrShutdownForced):
		s.log.Warn("shutdown finished by force", took, logx.Err(err))
	default:
		s.log.Error("shutdown finished with errors", took, logx.Err(err))
	}

	s.life.Lock()
	s.state, s.shutdownErr = stateStopped, err
	s.life.Unlock()
	close(s.done)
	return err
}

// shutdown is the sequence of 04 §6.4. Each step gets its budget, cut short by ctx; an error doesn't stop the
// sequence, because the later steps release what the earlier ones depend on. A step whose time runs out closes by
// force what it still has and returns its context's error (see stepError).
func (s *Server) shutdown(ctx context.Context, reason ShutdownReason, draining func()) error {
	// Steps 1 and 2: /readyz and /healthz answer 503 shutting_down, so monitors and load balancers stop sending;
	// every other request and new /ws upgrade gets 503 server_shutdown with Retry-After: 5.
	s.health.SetShuttingDown()
	s.gate.SetShuttingDown()
	s.log.Info("shutting down", slog.String("reason", string(reason)))
	if draining != nil {
		draining()
	}

	var errs []error
	step := func(name string, budget time.Duration, fn func(context.Context) error) {
		stepCtx, cancel := context.WithTimeout(ctx, budget)
		defer cancel()
		if err := fn(stepCtx); err != nil {
			errs = append(errs, stepError(stepCtx, name, err))
		}
	}

	// Step 3 (README S54), within hubShutdownBudget: Hub.Shutdown sends server.shutdown and error{server_shutdown}
	// to every connection and closes with 1012. The wire reason is "restart" whatever reason is: SIGTERM can't tell
	// a stop from a restart.
	// Step 4 (README S59), within mediaShutdownBudget: SFU.Close, then Transport.Close.

	// Step 5: the HTTP servers, then the 443 multiplexer.
	step("http", httpShutdownBudget, s.shutdownHTTP)

	// Step 6, within tailShutdownBudget: the TLS manager, now that nothing handshakes any more. With README S43, S54
	// and S71 also: flush the transfer counters, drain the push queue, close the admin socket, close the store.
	step("tls", tailShutdownBudget, s.tls.Shutdown)

	// Last: the data-directory lock, so that the process that follows (a re-exec, the next start) can take it.
	if err := s.lock.Close(); err != nil {
		errs = append(errs, fmt.Errorf("server: shutdown: data-directory lock: %w", err))
	}
	return errors.Join(errs...)
}

// stepError names the shutdown step that returned err. A step that returns the error of its ended context ran out
// of time and closed by force what it still had: its error wraps ErrShutdownForced. Any other error is a failure of
// the step itself.
func stepError(stepCtx context.Context, name string, err error) error {
	if ctxErr := stepCtx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
		return fmt.Errorf("%w: %s: %w", ErrShutdownForced, name, err)
	}
	return fmt.Errorf("server: shutdown: %s: %w", name, err)
}

// shutdownHTTP stops the HTTP servers: no new connections, idle ones closed, requests in flight may finish until ctx
// ends; what is left then is closed by force. Connections that have not sent a request yet (or not finished their
// TLS handshake) are closed right away: net/http alone would wait up to 5 s for each (see pendingConns). Hijacked
// connections (WebSockets) are the hub's, closed in step 3.
//
// The 443 multiplexer closes last: its raw listener, both sub-listeners and every connection that came through it
// and is still open (04 §6.4 step 5).
func (s *Server) shutdownHTTP(ctx context.Context) error {
	s.pending.closeAll()
	var errs []error
	for _, srv := range []*http.Server{s.httpSrv, s.plainSrv} {
		if srv == nil {
			continue
		}
		err := srv.Shutdown(ctx)
		if err != nil {
			_ = srv.Close()
			if ctx.Err() != nil { // out of time; any other error is the listener's own, from closing it
				err = fmt.Errorf("requests still running were cut off: %w", err)
			}
			errs = append(errs, err)
		}
	}
	if s.mux != nil {
		if err := s.mux.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	s.serving.Wait()
	return errors.Join(errs...)
}

// release closes what a failed Start had opened, in reverse order. Nothing serves yet at that point. ctx is Start's:
// the startup it bounds may be why Start failed, so the TLS manager's shutdown gets its budget whether ctx has ended
// or not.
func (s *Server) release(ctx context.Context) {
	if s.tls != nil {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tailShutdownBudget)
		_ = s.tls.Shutdown(stopCtx)
		cancel()
	}
	if s.httpLn != nil {
		_ = s.httpLn.Close()
	}
	if s.mux != nil {
		_ = s.mux.Close()
	}
	if s.lock != nil {
		_ = s.lock.Close()
	}
}
