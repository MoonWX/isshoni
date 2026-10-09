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
	"sync/atomic"
	"time"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/auth"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/httpapi"
	"github.com/MoonWX/isshoni/internal/server/netx"
	"github.com/MoonWX/isshoni/internal/server/ops"
	"github.com/MoonWX/isshoni/internal/server/sfu"
	"github.com/MoonWX/isshoni/internal/server/signal"
	"github.com/MoonWX/isshoni/internal/server/store"
	"github.com/MoonWX/isshoni/internal/server/tlsmgr"
	"github.com/MoonWX/isshoni/internal/version"
)

// Deps are what the server takes from its caller besides the config and the logger: the test seams, and LogLevel.
// The zero value of every seam means the real implementation, so cmd/isshoni passes only LogLevel, the level
// variable of the logger it built, and leaves the rest zero.
type Deps struct {
	// Now is the clock of the time-dependent components (the store, the account service, the REST API). nil means
	// time.Now.
	Now func() time.Time
	// LogLevel is the level variable the logger was built with (logx.Options.Level). With it, `isshoni admin
	// log-level` changes the process's log level for a while through the admin socket (04 §10, §12.2); nil means
	// the level can't be changed while the server runs, and that endpoint says so.
	LogLevel *slog.LevelVar
	// Argon is the cost of a password hash (03 §7.2); the zero value means auth.DefaultArgon. Tests lower it: the
	// account service computes one hash when it starts, which under the race detector takes a good part of a second
	// at the real cost.
	Argon auth.ArgonParams
	// STUN and Resolver are what public-address detection asks (04 §7.4). nil means netx.NewSTUNClient and
	// net.DefaultResolver. Resolver also answers the TLS manager's look at its own domain (04 §8.7). The detection
	// runs in every mode: off mode needs none for its site, but its media addresses are the detected ones too.
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
	// API and WS are the handlers mounted at /api/v1/ and GET /ws. nil means the real ones: 03's httpapi.API and
	// 01's signal hub, which the wiring builds (wire.go). A test's stub takes the real one's place on the router; the
	// accounts and the hub behind it are built all the same, for the admin socket.
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
	// ShutdownRestart is a restart the server asked for itself (rotate-secrets, and the public address that a server
	// without one was waiting for, 04 §7.4): Run returns ErrRestartRequested.
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
// (04 §6.3): an invalid config (a policy key whose value the settings refuse included), a container without a data
// volume, a data directory or lock file the process can't write, an admin socket directory it can't create, a
// corrupt or wrongly owned secrets.json, and a database that 03's store can't bring to this binary's schema: one
// that a newer isshoni wrote, a failed migration, a corrupt file (store.ErrNeedsOperator). cmd/isshoni prints such an
// error (it carries its own fix line) and exits 78, on which systemd stops restarting. Any other error is a runtime
// error (exit 1): a busy port, a data directory locked by another isshoni (config.ErrDataDirLocked).
// config.ReasonOf(err) gives the reason code for the data-directory and secrets refusals.
//
// A certificate that can't be had is neither: the server starts, and is not ready until it has one (04 §6.2, §8.4).
func NeedsOperator(err error) bool {
	var ve *config.ValidationError
	return errors.Is(err, config.ErrNeedsOperator) || errors.Is(err, store.ErrNeedsOperator) || errors.As(err, &ve)
}

// Addrs are the addresses the server listens on, as bound: a configured port 0 (tests) shows as the port the
// kernel picked. A later slice adds the metrics listener (README S85).
type Addrs struct {
	// HTTP is the listener on listen.http: the app itself in off mode; in the other modes the plain-HTTP port,
	// which answers ACME http-01 challenges and redirects everything else to HTTPS (04 §8.3).
	HTTP net.Addr
	// HTTPS is the 443 multiplexer on listen.https: HTTPS and WSS, and ICE-TCP by the first byte (04 §7.2). nil in
	// off mode, where a proxy owns that port.
	HTTPS net.Addr
	// ICEUDP is the media socket on listen.ice_udp (04 §7.3): a *net.UDPAddr, nil when UDP is off. The server binds
	// one socket per local address that carries media, all on one port; this is the first of them.
	ICEUDP net.Addr
	// ICETCP is the ICE-TCP listener on listen.ice_tcp: a *net.TCPAddr with the host as configured, nil when that
	// listener is off. ICE-TCP on the HTTPS port needs no address of its own: it is HTTPS's (04 §7.2).
	//
	// Both are nil on a server that started without media sockets: one that waits for its public address on a
	// machine without a local address for media (04 §6.2).
	ICETCP net.Addr
}

// lifecycle states of a Server; they only move forward.
type state uint8

const (
	stateNew      state = iota // New returned; nothing is open
	stateServing               // Start succeeded
	stateStopping              // Shutdown is running
	stateStopped               // Shutdown finished, or Start failed
)

// Budgets of the shutdown steps (04 §6.4). Those of steps 3 to 6 add up to half a second more than the default
// shutdown_timeout of 10 s, which bounds the whole shutdown whatever the steps would like: a shutdown in which every
// step runs out of time has that much less for its last one. Only the store's close may go beyond it.
const (
	hubShutdownBudget = 2 * time.Second // step 3: the hub's server.shutdown and close 1012
	// Step 4 has one budget for each of its two parts, so that the second never starts out of time. SFU.Close ends
	// by itself after the second it gives its PeerConnections (02's closeTimeout, README §4); its budget is that
	// second and a margin, so the step sees the SFU return and does not run out at the same moment. Transport.Close
	// then closes sockets, which takes no time to speak of.
	sfuShutdownBudget       = 1250 * time.Millisecond
	transportShutdownBudget = 250 * time.Millisecond
	httpShutdownBudget      = 5 * time.Second // step 5: http.Server.Shutdown on every server
	tailShutdownBudget      = 2 * time.Second // step 6: the TLS manager and the admin socket
	// storeCloseBudget is the store's own, at the end of step 6 and outside tailShutdownBudget: what closing it may
	// take, also when the time above is used up. It is the one thing a shutdown that is out of time still waits for
	// (see shutdown).
	storeCloseBudget = 1 * time.Second
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
	lock    *config.DataDirLock
	secrets *config.SecretStore // the session, invite and resume keys (wire.go); the VAPID pair (README S71)
	store   *store.DB           // 03's database (wire.go); closed last but the lock
	public  netx.PublicAddrs    // the public addresses as detected at startup, in every mode (public.go)
	mux     *netx.PortMux       // the 443 multiplexer on listen.https; nil in off mode
	httpLn  net.Listener        // listen.http
	adminLn net.Listener        // listen.admin_socket (04 §12.1)
	// transport is the ICE sockets and muxes: listen.ice_udp, listen.ice_tcp, the multiplexer's ICE side. It is nil
	// on the one server that starts without media: it waits for its public address on a machine that has no local
	// address for media either (listenICE), and then media is nil too.
	transport *netx.Transport
	tls       *tlsmgr.Manager // the certificate, in every mode (off: a manager with nothing to do)
	health    *ops.Health
	media     *sfu.SFU         // 02's SFU on transport, behind 01's sfuplane (wire.go); closed before transport
	accounts  *auth.Service    // 03's account service; nil on a server without a site (wire.go)
	hub       *signal.Hub      // 01's hub, mounted at GET /ws; nil without a site
	api       *httpapi.API     // 03's REST API, mounted at /api/v1/; nil without a site
	logLevel  *ops.LogLevel    // the runtime log level; nil without Deps.LogLevel
	doctor    *ops.Doctor      // doctor inside the server: its own runs, and the admin socket's (wire.go)
	admin     *ops.AdminServer // the admin socket's API, served on adminLn
	gate      *httpapi.Gate
	httpSrv   *http.Server   // the main server: on httpLn in off mode, on the multiplexer's TLS side otherwise
	plainSrv  *http.Server   // the port 80 server on httpLn (04 §8.3); nil in off mode
	pending   pendingConns   // the HTTP connections without a request yet; the shutdown closes them
	serving   sync.WaitGroup // the Serve goroutines of the HTTP servers; the shutdown's HTTP step waits for them
	// stopLimits ends the settings callback that passes the admin's limits on to the SFU (wire.go).
	stopLimits func()
	// awaitsAddress says that the site is the server's public address and the startup found none (04 §6.2): the
	// server runs without a site, not ready, and restarts itself when a later look finds the address (public.go).
	awaitsAddress bool

	// run is the life of what the server does on its own: the admin socket's Serve, the janitor, doctor's own runs,
	// the look at the public addresses, and the reads behind hooks that have no context (the readiness check "db",
	// the router's SPAStatus). The shutdown's last step cancels it; background waits for the goroutines.
	run        context.Context
	stopRun    context.CancelFunc
	background sync.WaitGroup

	setupDone atomic.Bool                      // an admin account exists: /setup answers 404 (spaStatus)
	publicNow atomic.Pointer[netx.PublicAddrs] // the public addresses as last seen, once they differ from public

	// restartAsked is closed when the server wants a restart for a reason of its own: the public address it was
	// waiting for is there (public.go). Run then shuts down for ShutdownRestart.
	restartOnce  sync.Once
	restartAsked chan struct{}

	// hookDraining, set by tests (under life), runs in Shutdown once readiness is off and the gate is closed, while
	// the listeners still accept: the window in which the hub and the SFU say goodbye (steps 3 and 4).
	hookDraining func()
	// hookStep, set by tests (under life), runs in Shutdown right before a component is stopped, with its name:
	// "signaling", "sfu", "transport", "http", "tls", "admin socket", "store", in the order of 04 §6.4.
	hookStep func(step string)
	// hookDetect, set by tests before Start, stands in for the public-address detection (04 §7.4), which reads
	// the machine's interfaces: a test says what the server finds, at startup and at every later look.
	hookDetect func() netx.PublicAddrs
	// redetectEvery, set by tests before Start, replaces publicRedetectEvery.
	redetectEvery time.Duration
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
		cfg:          *cfg,
		warnings:     warnings,
		log:          log,
		deps:         deps,
		stopping:     make(chan struct{}),
		done:         make(chan struct{}),
		failed:       make(chan struct{}),
		restartAsked: make(chan struct{}),
		stopLimits:   func() {},
	}, nil
}

// now is the server's clock: Deps.Now, or time.Now.
func (s *Server) now() time.Time {
	if s.deps.Now != nil {
		return s.deps.Now()
	}
	return time.Now()
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
	// The server's own work outlives the startup that ctx bounds; the shutdown ends it.
	s.run, s.stopRun = context.WithCancel(context.WithoutCancel(ctx))

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

	// Step 4: open the store (03 migrates, after a backup of its own), then pin the policy keys the config sets
	// (04 §4.6). A database that this binary can't serve is a refusal (NeedsOperator).
	if err := s.openStore(ctx, paths); err != nil {
		return err
	}

	// Step 5: detect the public addresses (04 §7.4; at most 5 s), in every mode. They are what the server's ICE
	// candidates name (04 §7.5), and the TLS modes need them for more: ip mode's site and certificate are the
	// address, manual mode's site is it too when there is no domain, and auto mode compares its domain's DNS records
	// with it. Off mode takes its site from public_url, or localhost in dev. The running server looks again every
	// ten minutes (startBackground).
	mode := s.cfg.EffectiveTLSMode()
	if s.hookDetect != nil {
		s.public = s.hookDetect()
	} else {
		s.public = s.detectPublicAddrs(ctx)
	}

	// Step 6: bind the listeners: the 443 multiplexer (not in off mode), listen.http, the admin socket, and then
	// the ICE Transport: the only code that binds listen.ice_udp and listen.ice_tcp, and the reader of the
	// multiplexer's ICE side (04 §7.3). A busy port ends the start; so does a machine without an address for media,
	// except on a server that has to wait for its public address anyway.
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
	// A socket on which another isshoni answers, or a path that is no socket, is a runtime error like a busy port.
	s.adminLn, err = ops.ListenAdmin(ctx, ops.AdminListenOptions{Path: s.cfg.Listen.AdminSocket, Logger: s.log})
	if err != nil {
		return err
	}
	// The site, now that the HTTP ports are bound, and whether the server has to wait for it. The Transport comes
	// after it: a server that waits for its address may have to start without one (listenICE).
	if s.site, err = s.newSite(mode); err != nil {
		return err
	}
	s.awaitsAddress = siteIsAddress(&s.cfg, mode) && s.site.Origin == ""
	if s.transport, err = s.listenICE(ctx); err != nil {
		return err
	}
	s.addrs.ICEUDP, s.addrs.ICETCP = iceAddrs(s.cfg.Listen.ICETCP, s.transport)

	// Step 7: the TLS manager. The certificate arrives in the background (04 §8): the listeners serve before it is
	// there, and the readiness check "tls" says when it is.
	if s.tls, err = tlsmgr.New(s.tlsOptions(mode, paths)); err != nil {
		return fmt.Errorf("server: %w", err)
	}
	if err := s.tls.Start(ctx); err != nil {
		return fmt.Errorf("server: %w", err)
	}

	// Step 8: build the components and register the readiness checks (04 §6.2): tls and public_ip here; db, media
	// and signal with their components in wire.
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
	if err := s.wire(ctx); err != nil {
		return err
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

	// Step 9: start the HTTP servers and the admin socket, then the server's own work. The multiplexer's ICE side
	// has had its reader since step 6: the Transport's ICE-TCP mux (without a Transport that side is closed).
	if mode == config.TLSOff {
		s.serve(s.httpSrv, s.httpLn)
	} else {
		s.serveTLS(s.httpSrv, s.mux.TLS())
		s.serve(s.plainSrv, newLimitListener(s.httpLn, s.cfg.Limits.ConnsPerIP))
	}
	s.startBackground()
	s.state, s.served = stateServing, true

	// Step 10: one line that says where the server is and which ports carry media, and how to finish setup while no
	// admin exists.
	listen := []any{slog.String("listen", s.addrs.HTTP.String())}
	if mode != config.TLSOff {
		listen = []any{slog.String("listen", s.addrs.HTTPS.String()), slog.String("listen_http", s.addrs.HTTP.String())}
	}
	if s.awaitsAddress {
		s.log.Warn(fmt.Sprintf("isshoni %s started without a public IP address (tls=%s) and is not ready: set public_ip "+
			"to this server's public address, or set domain, and restart. isshoni looks for the address again every %s "+
			"and restarts itself when it finds one", version.Version(), s.site.TLSMode, s.redetectInterval()), listen...)
		return nil
	}
	s.log.Info(fmt.Sprintf("isshoni %s ready: %s (tls=%s) %s", version.Version(), s.site.Origin, s.site.TLSMode,
		mediaPorts(s.transport.Advertised)), listen...)
	s.setupHint(ctx)
	return nil
}

// startBackground starts what the server does on its own while it runs: the admin socket's API (04 §6.1 step 9),
// 03's janitor, doctor's own runs, 20 s from now and then once a day (04 §13.1), and the look at the public
// addresses every ten minutes (04 §7.4). The admin socket's goroutine ends with its Shutdown, the others with s.run.
func (s *Server) startBackground() {
	admin, ln := s.admin, s.adminLn
	s.background.Go(func() {
		if err := admin.Serve(ln); err != nil {
			s.fail(err)
		}
	})
	doc := s.doctor
	s.background.Go(func() { _ = doc.Run(s.run) }) // nil when the server stops; an error only for a second Run
	if accounts := s.accounts; accounts != nil {
		s.background.Go(func() { accounts.RunJanitor(s.run) })
	}
	every, first := s.redetectInterval(), s.public
	s.background.Go(func() {
		watchPublicAddrs(s.run, every, first, s.redetectPublicAddrs, s.publicAddrsChanged)
	})
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
// from elsewhere begins (a restore or rotate-secrets through the admin socket), a listener fails, or the server
// wants a restart of its own: one that started without the public address that is its site restarts when a later
// look finds the address (04 §6.2, §7.4; public.go). In each case it finishes the graceful shutdown of 04 §6.4,
// within shutdown_timeout, before it returns. Its result says one thing, so that cmd/isshoni can map it to an exit
// code:
//   - nil after a stop (ctx cancelled: SIGTERM or SIGINT in cmd/isshoni);
//   - an error that wraps ErrShutdownForced after a stop that ran out of time and closed the rest by force. The
//     server is stopped all the same: exit 0;
//   - ErrRestartRequested after a shutdown for ShutdownRestart or ShutdownRestore, forced or not, which includes
//     the restart the server wanted itself;
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
	why := ShutdownStop
	select {
	case <-ctx.Done():
	case <-s.stopping:
	case <-s.failed:
	case <-s.restartAsked:
		if ctx.Err() == nil { // a stop that came at the same moment wins: the operator's word
			why = ShutdownRestart
		}
	}
	// The run context is gone or about to be; the shutdown gets shutdown_timeout of its own. If a shutdown is
	// already running or done, this waits for it and returns its result.
	err := s.Shutdown(context.WithoutCancel(ctx), why)

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
// leaves the server stopped. Its error then wraps ErrShutdownForced and names the steps that had to do so. Only the
// store's close is still waited for when the time is up, for one more second at most: it takes milliseconds, and
// it is what makes the database file complete.
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
	draining, before := s.hookDraining, s.hookStep
	if before == nil {
		before = func(string) {}
	}
	close(s.stopping)
	s.life.Unlock()

	ctx, cancel := context.WithTimeout(ctx, s.cfg.ShutdownTimeout.Duration)
	defer cancel()
	start := time.Now()
	err := s.shutdown(ctx, reason, draining, before)
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
// force what it still has and returns its context's error (see stepError). before is called with a component's
// name right before that component is stopped: tests watch the order with it.
func (s *Server) shutdown(ctx context.Context, reason ShutdownReason, draining func(), before func(step string)) error {
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

	// Step 3: the hub sends server.shutdown and error{server_shutdown} to every connection and closes with 1012
	// (01 §11.6). The wire reason is "restart" whatever reason is: SIGTERM can't tell a stop from a restart, and the
	// clients reconnect either way. A server without a site has no hub.
	if s.hub != nil {
		before("signaling")
		step("signaling", hubShutdownBudget, func(ctx context.Context) error {
			return s.hub.Shutdown(ctx, protocol.ShutdownReasonRestart)
		})
	}

	// Step 4: the SFU ends what the hub left (every share, with server_shutdown) and closes its PeerConnections,
	// all at once; then the Transport closes the ICE sockets and muxes under it, the multiplexer's ICE side
	// included. The order matters: the SFU never closes a socket, and the Transport must outlive it (02 §6.1), so
	// each has a budget of its own and the Transport's begins when the SFU has returned. Only an SFU that breaks
	// its word and is still closing when its budget ends has the Transport closed under what it still holds, which
	// is what "by force" means here; either call then finishes on its own. A server that started without media
	// sockets has neither (listenICE).
	if s.media != nil {
		before("sfu")
		step("sfu", sfuShutdownBudget, func(ctx context.Context) error { return within(ctx, s.media.Close) })
	}
	if s.transport != nil {
		before("transport")
		step("transport", transportShutdownBudget, func(ctx context.Context) error { return within(ctx, s.transport.Close) })
	}

	// Step 5: the HTTP servers, then the 443 multiplexer.
	before("http")
	step("http", httpShutdownBudget, s.shutdownHTTP)

	// Step 6, in tailShutdownBudget altogether: the TLS manager, now that nothing handshakes any more; then the
	// server's own work and the admin socket, which the operator's CLI could ask until now (its health answers have
	// said "shutting down" since step 1). README S85 and S71 add here: flush the transfer counters, drain the push
	// queue.
	tailCtx, cancelTail := context.WithTimeout(ctx, tailShutdownBudget)
	defer cancelTail()
	tail := func(name string, fn func(context.Context) error) {
		if err := fn(tailCtx); err != nil {
			errs = append(errs, stepError(tailCtx, name, err))
		}
	}
	before("tls")
	tail("tls", s.tls.Shutdown)
	s.stopRun()
	s.stopLimits() // a settings change no longer reaches the SFU, which is closed
	before("admin socket")
	tail("admin socket", s.admin.Shutdown)
	if s.logLevel != nil {
		s.logLevel.Close()
	}

	// The store closes once nothing uses it any more: it checkpoints the WAL, so the .db file alone is complete. In
	// a shutdown that had the time this is the end of step 6 and takes milliseconds. In one that ran out of time (a
	// request that had to be cut off) the store still gets storeCloseBudget, on top of what is over: giving up on
	// the checkpoint too would leave the next start to recover it.
	before("store")
	storeCtx, cancelStore := context.WithTimeout(context.WithoutCancel(ctx), storeCloseBudget)
	defer cancelStore()
	err := within(storeCtx, func() error {
		s.background.Wait() // the admin socket's Serve, the janitor, doctor, the look at the public addresses
		return s.store.Close()
	})
	if err != nil {
		errs = append(errs, stepError(storeCtx, "store", err))
	}

	// Last: the data-directory lock, so that the process that follows (a re-exec, the next start) can take it.
	if err := s.lock.Close(); err != nil {
		errs = append(errs, fmt.Errorf("server: shutdown: data-directory lock: %w", err))
	}
	return errors.Join(errs...)
}

// within runs fn, which takes no context, and waits for it until ctx ends; it then returns ctx's error and lets fn
// finish on its own. The shutdown uses it for the wait that ends by itself once everything else is stopped (the
// server's goroutines, then the store's last checkpoint): if it hangs all the same (a disk that does not answer),
// it must not keep the process from stopping in time.
func within(ctx context.Context, fn func() error) error {
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
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
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tailShutdownBudget)
	defer cancel()
	if s.stopRun != nil {
		s.stopRun()
	}
	if s.hub != nil {
		_ = s.hub.Shutdown(stopCtx, protocol.ShutdownReasonRestart) // no socket yet: this only ends the hub's context
	}
	s.stopLimits()
	if s.media != nil {
		_ = s.media.Close() // no connection has joined: there is nothing to wait for
	}
	if s.logLevel != nil {
		s.logLevel.Close()
	}
	if s.tls != nil {
		_ = s.tls.Shutdown(stopCtx)
	}
	if s.transport != nil {
		_ = s.transport.Close() // the ICE sockets, and the multiplexer's ICE side
	}
	if s.adminLn != nil {
		_ = s.adminLn.Close() // removes the socket file
	}
	if s.httpLn != nil {
		_ = s.httpLn.Close()
	}
	if s.mux != nil {
		_ = s.mux.Close()
	}
	if s.store != nil {
		_ = s.store.Close()
	}
	if s.lock != nil {
		_ = s.lock.Close()
	}
}
