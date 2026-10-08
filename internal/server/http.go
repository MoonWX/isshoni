package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/httpapi"
	"github.com/MoonWX/isshoni/web"
)

// Limits of the HTTP servers (04 §7.8). There is no server-wide ReadTimeout or WriteTimeout: either would cut
// WebSockets. The router gives every request but /ws a 30 s read deadline instead (04 §9.3).
const (
	readHeaderTimeout = 10 * time.Second  // also bounds the TLS handshake once the server speaks TLS
	idleTimeout       = 120 * time.Second // keep-alive connections waiting for their next request
	maxHeaderBytes    = 16 << 10
)

// newMainServer builds the main http.Server (04 §9.1) around the router's handler: HTTP/1.1, and HTTP/2 where the
// listener speaks TLS (behind the 443 multiplexer; a plain-HTTP listener never negotiates it, so off mode stays on
// HTTP/1.1). net/http's own error lines (a failed handshake, a broken connection: scanners make them constant) go
// to the log at debug level, so they show only with log.level = "debug". pending follows the server's connections
// until their first request header is in, so that the shutdown can close the ones that never sent it.
func newMainServer(h http.Handler, log *slog.Logger, pending *pendingConns) *http.Server {
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		Protocols:         &protocols,
		ConnState:         pending.track,
		ErrorLog:          slog.NewLogLogger(log.With(slog.String("component", "http")).Handler(), slog.LevelDebug),
	}
}

// pendingConns are the connections that net/http has accepted and that have not delivered a complete request header
// yet (http.StateNew): a browser's spare connection, a port scanner, a request cut in half.
//
// http.Server.Shutdown waits for such a connection until it is 5 s old, which is the whole budget of the shutdown's
// HTTP step (04 §6.4 step 5): a single one would make every stop take 5 s and end by force. So the server closes
// them itself when the shutdown reaches that step. No answer is lost: a request that completed its header then
// would only get the 503 of the shutdown gate.
//
// One pendingConns serves every http.Server of a Server. The zero value is ready to use.
type pendingConns struct {
	mu      sync.Mutex
	closing bool // closeAll ran: nothing is tracked any more
	conns   map[net.Conn]struct{}
}

// track is the servers' http.Server.ConnState hook. Every connection starts in StateNew and leaves it for good
// with its first request header, or when it closes.
func (p *pendingConns) track(c net.Conn, state http.ConnState) {
	p.mu.Lock()
	if state != http.StateNew {
		delete(p.conns, c)
		p.mu.Unlock()
		return
	}
	closing := p.closing
	if !closing {
		if p.conns == nil {
			p.conns = make(map[net.Conn]struct{})
		}
		p.conns[c] = struct{}{}
	}
	p.mu.Unlock()
	if closing {
		// Accepted between closeAll and the moment the listener closed.
		_ = c.Close()
	}
}

// closeAll closes every pending connection, and makes track close the ones that are still accepted afterwards.
// net/http's goroutine of a closed connection sees the read fail and lets go of it at once. The shutdown calls it
// right before http.Server.Shutdown.
func (p *pendingConns) closeAll() {
	p.mu.Lock()
	p.closing = true
	conns := p.conns
	p.conns = nil
	p.mu.Unlock()
	for c := range conns {
		_ = c.Close()
	}
}

// routerOptions fills httpapi's options from the config and the site (04 §9.2).
func (s *Server) routerOptions() httpapi.RouterOptions {
	spa := s.deps.SPA
	if spa == nil {
		spa = web.Dist()
	}
	return httpapi.RouterOptions{
		Site: s.site,
		// The effective network.trusted_proxies: in off mode the server sits behind a proxy, and without it every
		// client would be the proxy's address (04 §8.5). The router ignores it in the TLS modes.
		TrustedProxies: s.cfg.Network.TrustedProxies,
		DisableHSTS:    !s.cfg.TLS.HSTS,
		SPA:            spa,
		// SPAStatus (03's "/setup is gone after setup" hook) and Observer (ops.Metrics) come with their components.
		Gate:   s.gate,
		API:    s.deps.API,
		WS:     s.deps.WS,
		Logger: s.log,
	}
}

// newRouter builds the main router and adds this package's routes to it.
//
// Every route on the router but /ws runs under the router's 30 s read deadline (04 §9.3): when it passes, net/http's
// background read fails and the request context is cancelled. A handler that may run longer than that (none so
// far; the connection test and a doctor run are the candidates) has to clear the deadline with
// http.ResponseController, as the /ws mount does.
func (s *Server) newRouter() *httpapi.Router {
	rt := httpapi.NewRouter(s.routerOptions())
	healthz, readyz := s.health.Handlers()
	rt.Handle("GET /healthz", healthz)
	rt.Handle("GET /readyz", readyz)
	return rt
}

// listenHTTP binds the TCP listener of listen.http: the app itself in off mode, the plain-HTTP port of 04 §8.3 in
// the other modes. Its errors are for the operator (04 §6.1 step 6): a busy port names the port and the ways out.
// They are runtime errors (exit 1), not refusals: stopping the other program and restarting helps.
func listenHTTP(ctx context.Context, addr string, mode config.TLSMode) (net.Listener, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err == nil {
		return ln, nil
	}
	_, port, _ := net.SplitHostPort(addr)
	off := mode == config.TLSOff
	switch {
	case errors.Is(err, syscall.EADDRINUSE) && off:
		return nil, fmt.Errorf("port %s is in use (another isshoni, or another web server?). Stop it, or change "+
			"listen.http (now %q): %w", port, addr, err)
	case errors.Is(err, syscall.EADDRINUSE):
		return nil, fmt.Errorf("port %s is in use (another web server?). isshoni answers certificate challenges there "+
			`and redirects to HTTPS. Stop the other program, or run isshoni behind it with tls.mode = "off" (listen.http `+
			"is %q): %w", port, addr, err)
	case errors.Is(err, fs.ErrPermission) && off:
		return nil, fmt.Errorf("isshoni may not listen on listen.http = %q (a port below 1024 needs "+
			`CAP_NET_BIND_SERVICE). With tls.mode = "off" use a high port such as "127.0.0.1:8080": %w`, addr, err)
	case errors.Is(err, fs.ErrPermission):
		return nil, fmt.Errorf("isshoni may not listen on listen.http = %q (a port below 1024 needs "+
			"CAP_NET_BIND_SERVICE, which the systemd unit and the container image grant): %w", addr, err)
	default:
		return nil, fmt.Errorf("server: listen on listen.http = %q: %w", addr, err)
	}
}

// withBoundPort returns a listen address with the bound port in place of a configured port 0 (tests let the kernel
// pick one). The host stays as configured, so a loopback address stays one for config.NewSite. Any other address
// comes back unchanged.
func withBoundPort(configured string, bound net.Addr) string {
	host, port, err := net.SplitHostPort(configured)
	if err != nil {
		return configured
	}
	if n, err := strconv.Atoi(port); err != nil || n != 0 {
		return configured
	}
	tcp, ok := bound.(*net.TCPAddr)
	if !ok {
		return configured
	}
	return net.JoinHostPort(host, strconv.Itoa(tcp.Port))
}
