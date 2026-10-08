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
	"syscall"
	"time"

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

// newMainServer builds the main http.Server (04 §9.1) around the router's handler: HTTP/1.1, and HTTP/2 once the
// listener speaks TLS (README S44; a plain-HTTP listener never negotiates it). net/http's own error lines (a failed
// handshake, a broken connection: scanners make them constant) go to the log at debug level, so they show only
// with log.level = "debug".
func newMainServer(h http.Handler, log *slog.Logger) *http.Server {
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		Protocols:         &protocols,
		ErrorLog:          slog.NewLogLogger(log.With(slog.String("component", "http")).Handler(), slog.LevelDebug),
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

// listenHTTP binds the TCP listener of listen.http. Its errors are for the operator (04 §6.1 step 6): a busy port
// names the port and the two ways out. They are runtime errors (exit 1), not refusals: stopping the other program
// and restarting helps.
func listenHTTP(ctx context.Context, addr string) (net.Listener, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err == nil {
		return ln, nil
	}
	_, port, _ := net.SplitHostPort(addr)
	switch {
	case errors.Is(err, syscall.EADDRINUSE):
		return nil, fmt.Errorf("port %s is in use (another isshoni, or another web server?). Stop it, or change "+
			"listen.http (now %q): %w", port, addr, err)
	case errors.Is(err, fs.ErrPermission):
		return nil, fmt.Errorf("isshoni may not listen on listen.http = %q (a port below 1024 needs "+
			`CAP_NET_BIND_SERVICE). With tls.mode = "off" use a high port such as "127.0.0.1:8080": %w`, addr, err)
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
