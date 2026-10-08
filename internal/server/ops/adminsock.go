package ops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// The admin socket (04 §12.1): HTTP/1.1 over a unix socket at listen.admin_socket, for the operator's CLI on the
// same machine. The socket file is 0600 and belongs to the server's user, and every connection's peer credentials
// are checked: only root and the server's own uid are served. Root therefore never has to touch the data files:
// `sudo isshoni admin …` only talks to the socket, and the server process does the reads and writes.
//
// Linux and macOS have the check. A build for another platform serves every connection and says so in the log
// (peercred_other.go has what that means on Windows).
//
// The wiring uses it in three steps, which follow the startup sequence of 04 §6.1:
//
//	ln, err := ops.ListenAdmin(ctx, ops.AdminListenOptions{Path: cfg.Listen.AdminSocket, Logger: log}) // step 6
//	admin := ops.NewAdminServer(ops.AdminOptions{Health: health, Accounts: accounts, …})                // step 8
//	go func() { err := admin.Serve(ln) … }()                                                            // step 9
//	…
//	err = admin.Shutdown(ctx)                                                                            // 04 §6.4 step 6

// AdminListenOptions configures ListenAdmin.
type AdminListenOptions struct {
	// Path is listen.admin_socket. Its directory must exist (config.PrepareDataDir creates it).
	Path string
	// Allow decides whether a peer with this uid is served. nil means the rule of 04 §12.1: root and the server's
	// own uid. Tests pass another rule, and one that refuses the test's own uid needs a client that expects it
	// (AdminClient.AssumeStranger).
	Allow func(uid uint32) bool
	// Logger gets a line for each refused connection (rate-limited). nil means slog.Default().
	Logger *slog.Logger
}

// refusalLogEvery is the shortest time between two log lines about refused connections; the refusals in between
// are counted in the next line.
const refusalLogEvery = 10 * time.Second

// ListenAdmin binds the admin socket (04 §6.1 step 6) and returns a listener that only hands out connections whose
// peer may use it; the others are closed at once, before anything is read from them. The socket file gets mode 0600.
// A socket file that a crashed server left behind is replaced; a path where another process answers, or that is
// not a socket, is an error: a runtime error (exit 1), since stopping the other process helps.
//
// Closing the listener removes the socket file.
func ListenAdmin(ctx context.Context, o AdminListenOptions) (net.Listener, error) {
	if o.Path == "" {
		return nil, errors.New("ops: ListenAdmin: empty socket path (listen.admin_socket)")
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With(slog.String("component", "admin"))
	if err := clearStaleSocket(ctx, o.Path); err != nil {
		return nil, err
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", o.Path)
	if err != nil {
		return nil, fmt.Errorf("ops: listen on the admin socket %s (listen.admin_socket): %w", o.Path, err)
	}
	unixLn, ok := ln.(*net.UnixListener)
	if !ok { // net.Listen("unix") always returns one
		_ = ln.Close()
		return nil, fmt.Errorf("ops: admin socket %s: unexpected listener type %T", o.Path, ln)
	}
	// bind creates the file with the umask's mode (0700 under the server's umask of 0077). 0600 is what 04 §12.1
	// promises, whatever the umask; until the chmod the peer-credential check alone keeps strangers out.
	if err := os.Chmod(o.Path, 0o600); err != nil {
		_ = unixLn.Close()
		return nil, fmt.Errorf("ops: admin socket %s: %w", o.Path, err)
	}
	allow := o.Allow
	if allow == nil {
		allow = defaultPeerRule(os.Geteuid())
	}
	if !peerCredSupported {
		log.Warn(noPeerCheckWarning(runtime.GOOS), slog.String("path", o.Path))
	}
	return &adminListener{UnixListener: unixLn, allow: allow, peerUID: peerUID, checked: peerCredSupported, log: log}, nil
}

// noPeerCheckWarning is what ListenAdmin logs once on a platform without the peer-credential check (goos): every
// connection is served there. Where file modes count, the socket's 0600 still keeps other users out. On Windows it
// does not: the socket file has the access rights of its directory, so whoever can open it is an admin.
func noPeerCheckWarning(goos string) string {
	if goos == "windows" {
		return "this platform can't check who connects to the admin socket, and the socket's file mode does not " +
			"limit it either: every local user who can open the socket can administer this server"
	}
	return "this platform can't check who connects to the admin socket; its file mode (0600) is the only guard"
}

// defaultPeerRule is the rule of 04 §12.1: root and the server's own uid (self; -1 on Windows, which matches no
// peer).
func defaultPeerRule(self int) func(uid uint32) bool {
	return func(uid uint32) bool { return uid == 0 || int64(uid) == int64(self) }
}

// clearStaleSocket makes path free for bind: a socket file on which nobody listens (the server crashed, or was
// killed) is removed. Anything else that is in the way is an error.
func clearStaleSocket(ctx context.Context, path string) error {
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("ops: admin socket %s (listen.admin_socket): %w", path, err)
	case fi.Mode()&fs.ModeSocket == 0:
		return fmt.Errorf("%s (listen.admin_socket) exists and is not a socket. Move it away, or set "+
			"listen.admin_socket to another path", path)
	}
	d := net.Dialer{Timeout: time.Second}
	c, err := d.DialContext(ctx, "unix", path)
	if err == nil {
		_ = c.Close()
		return fmt.Errorf("another process is listening on the admin socket %s (another isshoni?). Stop it, or set "+
			"listen.admin_socket to another path", path)
	}
	if !isConnRefused(err) {
		return fmt.Errorf("ops: admin socket %s (listen.admin_socket) is in the way: %w", path, err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("ops: removing the stale admin socket %s: %w", path, err)
	}
	return nil
}

// adminListener is the admin socket's listener: Accept returns only connections of peers that allow admits.
type adminListener struct {
	*net.UnixListener
	allow   func(uid uint32) bool
	peerUID func(*net.UnixConn) (uint32, error)
	checked bool // false on a platform without peer credentials: every connection is served
	log     *slog.Logger

	mu       sync.Mutex
	lastLine time.Time // of the last log line about a refusal
	unlogged int       // refusals since then
}

// Accept waits for the next connection of an admitted peer. A refused peer sees its connection closed before the
// first response; the CLI reads that as "permission denied" (04 §12.1).
func (l *adminListener) Accept() (net.Conn, error) {
	for {
		c, err := l.AcceptUnix()
		if err != nil {
			return nil, err
		}
		if !l.checked {
			return &adminConn{UnixConn: c}, nil
		}
		uid, err := l.peerUID(c)
		if err == nil && l.allow(uid) {
			return &adminConn{UnixConn: c, uid: uid, known: true}, nil
		}
		_ = c.Close()
		l.logRefusal(uid, err)
	}
}

// logRefusal writes at most one line per refusalLogEvery, so that a local user who keeps connecting can't flood
// the log.
func (l *adminListener) logRefusal(uid uint32, credErr error) {
	now := time.Now()
	l.mu.Lock()
	if !l.lastLine.IsZero() && now.Sub(l.lastLine) < refusalLogEvery {
		l.unlogged++
		l.mu.Unlock()
		return
	}
	skipped := l.unlogged
	l.lastLine, l.unlogged = now, 0
	l.mu.Unlock()

	attrs := []slog.Attr{}
	if credErr != nil {
		attrs = append(attrs, logx.Err(credErr))
	} else {
		attrs = append(attrs, slog.Uint64("peer_uid", uint64(uid)))
	}
	if skipped > 0 {
		attrs = append(attrs, slog.Int("refused_since_last_line", skipped))
	}
	l.log.LogAttrs(context.Background(), slog.LevelWarn,
		"refused a connection to the admin socket: only root and the server's own user may use it", attrs...)
}

// adminConn is an accepted connection with its peer's uid.
type adminConn struct {
	*net.UnixConn
	uid   uint32
	known bool // false when the platform has no peer credentials
}

// peerUIDKey is the context key of a request's peer uid.
type peerUIDKey struct{}

// peerUIDAttr is the log attribute for the uid of the request's peer, or an empty attribute when it is not known.
// backup, restore and log-level log it (04 §12.1); it never goes into the audit log.
func peerUIDAttr(ctx context.Context) slog.Attr {
	if uid, ok := ctx.Value(peerUIDKey{}).(uint32); ok {
		return slog.Uint64("peer_uid", uint64(uid))
	}
	return slog.Attr{}
}

// AdminAccounts is 03's account service as the admin socket uses it (03 §12.6, 04 §12.5). ops may not import auth
// or store (04 §2), so the wiring implements it over auth.Service and store.DB (04 §6.6); every call is audited there
// with store.CLIActor. Errors are 03's *api.Error values, which the socket passes on unchanged; any other error is
// answered as 500 internal.
type AdminAccounts interface {
	// SetupAvailable reports whether the setup link still works: no admin exists yet (auth.SetupAvailable).
	SetupAvailable(ctx context.Context) (bool, error)
	// IssueSetupLink mints a new setup token and cancels the earlier unused ones, so only the newest link works
	// (auth.IssueSetupToken). It returns setup_unavailable once an admin exists.
	IssueSetupLink(ctx context.Context) (IssuedLink, error)
	// Users lists every account (store.ListUsers), ordered as the store returns them.
	Users(ctx context.Context) ([]AdminUser, error)
	// IssueResetLink mints a one-time password-reset link for the user with this name (auth.UserByUsername, then
	// auth.IssuePasswordReset). Issuing it clears the password and signs the user out everywhere (03 §7.10). It
	// returns user_not_found for an unknown name.
	IssueResetLink(ctx context.Context, username string) (IssuedLink, error)
	// SetRole makes the user an admin or a regular user (auth.UpdateUser with UserChange.Role): user_not_found, or
	// last_admin when it would leave no active admin.
	SetRole(ctx context.Context, username string, role api.Role) error
	// SetDisabled blocks (true) or allows (false) sign-in (auth.UpdateUser with UserChange.Status); disabling
	// revokes the user's sessions and devices. user_not_found, or last_admin.
	SetDisabled(ctx context.Context, username string, disabled bool) error
	// CreateInvite mints an invite link (auth.CreateInvite). maxUses and ttlHours are 0 for "the server's invite
	// setting" (03 §9). registration_closed when registration is closed, validation_failed for values out of range.
	CreateInvite(ctx context.Context, maxUses, ttlHours int) (IssuedLink, error)
}

// IssuedLink is a one-time link from AdminAccounts: a setup, password-reset or invite link. Its URL carries the
// token in the fragment, so it travels as a logx.Secret and is revealed only into the socket's response.
type IssuedLink struct {
	URL       logx.Secret
	ExpiresAt time.Time
}

// AdminOptions configures NewAdminServer. Later slices of the M1 plan add what backup, restore, rotate-secrets and
// doctor need; until then those endpoints say that this build does not have them.
type AdminOptions struct {
	// Health answers /v1/health and /v1/ready, and tells setup-url when the server is ready. Required.
	Health *Health
	// Status builds the document of GET /v1/status (04 §12.2). nil means what ops knows by itself: the version, the
	// start time and the uptime.
	Status func(ctx context.Context) (api.ServerStatus, error)
	// Accounts serves setup-url, users and invites. nil means those endpoints answer 500: a server without accounts
	// (tests, or a build whose wiring does not have them yet).
	Accounts AdminAccounts
	// LogLevel serves /v1/log-level. nil means that endpoint answers 500.
	LogLevel *LogLevel
	// Logger is the server's logger. nil means slog.Default().
	Logger *slog.Logger
}

// AdminServer serves the admin socket's API (04 §12.2). One AdminServer is one life: NewAdminServer, Serve on the
// listener of ListenAdmin, then Shutdown.
type AdminServer struct {
	health   *Health
	status   func(ctx context.Context) (api.ServerStatus, error)
	accounts AdminAccounts
	logLevel *LogLevel
	log      *slog.Logger
	started  time.Time

	srv      *http.Server
	stopOnce sync.Once
	stopping chan struct{} // closed when Shutdown begins: a setup-url that waits for readiness stops waiting

	// The connections that have not sent a request header yet. Shutdown closes them itself: http.Server.Shutdown
	// would wait up to 5 s for each, longer than the whole step may take (04 §6.4 step 6).
	connMu  sync.Mutex
	closing bool
	fresh   map[net.Conn]struct{}
}

// Limits of the admin socket's HTTP server. There is no read or write timeout: backup and restore stream for as
// long as they need, and setup-url may wait for readiness.
const (
	adminReadHeaderTimeout = 10 * time.Second
	adminIdleTimeout       = 60 * time.Second
	adminMaxHeaderBytes    = 16 << 10
)

// NewAdminServer builds the server. It starts nothing. A nil o.Health is a wiring bug and panics.
func NewAdminServer(o AdminOptions) *AdminServer {
	if o.Health == nil {
		panic("ops: NewAdminServer: nil Health")
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With(slog.String("component", "admin"))
	s := &AdminServer{
		health:   o.Health,
		status:   o.Status,
		accounts: o.Accounts,
		logLevel: o.LogLevel,
		log:      log,
		started:  time.Now(),
		stopping: make(chan struct{}),
	}
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	s.srv = &http.Server{
		Handler:           s.routes(),
		ReadHeaderTimeout: adminReadHeaderTimeout,
		IdleTimeout:       adminIdleTimeout,
		MaxHeaderBytes:    adminMaxHeaderBytes,
		Protocols:         &protocols,
		ConnState:         s.trackConn,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if ac, ok := c.(*adminConn); ok && ac.known {
				return context.WithValue(ctx, peerUIDKey{}, ac.uid)
			}
			return ctx
		},
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
	return s
}

// Handler returns the API's handler, without the socket: for tests that drive it with httptest.
func (s *AdminServer) Handler() http.Handler { return s.srv.Handler }

// Serve answers requests on ln, the listener of ListenAdmin, until Shutdown; it then returns nil. Any other return
// value is the listener's error. The caller runs it in a goroutine of its own and waits for it.
func (s *AdminServer) Serve(ln net.Listener) error {
	if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("ops: serving the admin socket: %w", err)
	}
	return nil
}

// Shutdown stops the server (04 §6.4 step 6): the listener closes, which removes the socket file, and requests in
// flight may finish until ctx ends. What is left then is closed by force, and Shutdown returns ctx's error. A
// request must not wait for Shutdown: it would wait for itself.
func (s *AdminServer) Shutdown(ctx context.Context) error {
	s.stopOnce.Do(func() { close(s.stopping) })
	s.closeFresh()
	err := s.srv.Shutdown(ctx)
	if err != nil {
		_ = s.srv.Close()
		if ctx.Err() != nil {
			return fmt.Errorf("admin socket: requests still running were cut off: %w", err)
		}
		return fmt.Errorf("ops: closing the admin socket: %w", err)
	}
	return nil
}

// trackConn is the server's ConnState hook: it follows each connection until its first request header is in.
func (s *AdminServer) trackConn(c net.Conn, state http.ConnState) {
	s.connMu.Lock()
	if state != http.StateNew {
		delete(s.fresh, c)
		s.connMu.Unlock()
		return
	}
	closing := s.closing
	if !closing {
		if s.fresh == nil {
			s.fresh = make(map[net.Conn]struct{})
		}
		s.fresh[c] = struct{}{}
	}
	s.connMu.Unlock()
	if closing {
		_ = c.Close() // accepted between closeFresh and the moment the listener closed
	}
}

// closeFresh closes every connection that has not sent a request yet, and makes trackConn close the ones that are
// still accepted afterwards.
func (s *AdminServer) closeFresh() {
	s.connMu.Lock()
	s.closing = true
	conns := s.fresh
	s.fresh = nil
	s.connMu.Unlock()
	for c := range conns {
		_ = c.Close()
	}
}
