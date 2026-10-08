package server

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"syscall"
	"time"

	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/netx"
	"github.com/MoonWX/isshoni/internal/server/tlsmgr"
)

// This file is the wiring of the TLS modes (04 §8; README S44): the site the public addresses make, the 443
// multiplexer, the port 80 server and the options of the TLS manager. The detection of the public addresses is in
// public.go.

// Limits of the port 80 server (04 §7.8). Unlike the main server it carries no WebSocket and no upload, so every
// exchange is bounded as a whole as well.
const (
	plainReadHeaderTimeout = 5 * time.Second
	plainIdleTimeout       = 30 * time.Second
	plainExchangeTimeout   = 30 * time.Second // a request with its body; the answer
)

// siteIsAddress reports whether the site is the server's public IP address rather than a name: ip mode, and manual
// mode without a domain (04 §4.4).
func siteIsAddress(cfg *config.Config, mode config.TLSMode) bool {
	return mode == config.TLSIP || (mode == config.TLSManual && cfg.Domain == "")
}

// newSite computes the site once the listeners are bound (04 §4.4). A site that is the public address has none
// when the detection found nothing: the server then runs without an origin, not ready, and every request but the
// health endpoints gets 421 from the Host check. The operator sets public_ip and restarts (04 §7.4).
func (s *Server) newSite(mode config.TLSMode) (config.Site, error) {
	site, err := config.NewSite(&s.cfg, s.public.V4, s.public.V6)
	if err == nil {
		return site, nil
	}
	if siteIsAddress(&s.cfg, mode) && !s.public.V4.IsValid() && !s.public.V6.IsValid() {
		return config.Site{TLSMode: mode}, nil
	}
	return config.Site{}, fmt.Errorf("server: %w", err)
}

// listenHTTPS binds the 443 multiplexer on listen.https (04 §7.2). Its errors are for the operator (04 §6.1 step
// 6); they are runtime errors (exit 1), not refusals.
func (s *Server) listenHTTPS() (*netx.PortMux, error) {
	addr := s.cfg.Listen.HTTPS
	_, port, _ := net.SplitHostPort(addr)
	// The plain-HTTP hint on this port names the site. Its host is known before the port is bound, unless the port
	// is the kernel's to pick (tests): the hint then names the address the client connected to.
	publicHost := ""
	if site, err := config.NewSite(&s.cfg, s.public.V4, s.public.V6); err == nil && port != "0" {
		publicHost = site.Host
	}
	mux, err := netx.ListenPortMux(netx.PortMuxOptions{
		Addr:          addr,
		MaxConnsPerIP: s.cfg.Limits.ConnsPerIP,
		PublicHost:    publicHost,
		Logger:        s.log,
	})
	switch {
	case err == nil:
		return mux, nil
	case errors.Is(err, syscall.EADDRINUSE):
		return nil, fmt.Errorf("port %s is in use (another web server?). Stop it, or run isshoni behind it with "+
			`tls.mode = "off" (listen.https is %q): %w`, port, addr, err)
	case errors.Is(err, fs.ErrPermission):
		return nil, fmt.Errorf("isshoni may not listen on listen.https = %q (a port below 1024 needs "+
			"CAP_NET_BIND_SERVICE, which the systemd unit and the container image grant): %w", addr, err)
	default:
		return nil, fmt.Errorf("server: listen on listen.https = %q: %w", addr, err)
	}
}

// tlsOptions fills the TLS manager's options from the config, the site and the bound listeners (04 §8.2).
func (s *Server) tlsOptions(mode config.TLSMode, paths config.Paths) tlsmgr.Options {
	opts := tlsmgr.Options{Mode: mode, Logger: s.log}
	if mode == config.TLSOff {
		return opts
	}
	// One address stands for the server (04 §7.6): the IPv4 one, or the IPv6 one on an IPv6-only host.
	public, publicV6 := s.public.V4, s.public.V6
	if !public.IsValid() {
		public, publicV6 = publicV6, netip.Addr{}
	}
	opts.Domain = s.cfg.Domain
	opts.PublicIP, opts.PublicIPv6 = public, publicV6
	opts.Email = s.cfg.TLS.ACMEEmail
	opts.CA = s.cfg.TLS.ACMECA
	opts.Staging = s.cfg.TLS.ACMEStaging
	opts.CARootFile = s.cfg.TLS.ACMECARoot
	opts.CertFile, opts.KeyFile = s.cfg.TLS.CertFile, s.cfg.TLS.KeyFile
	opts.StorageDir = paths.CertMagic
	opts.HSTS = s.cfg.TLS.HSTS
	opts.Site = s.site
	opts.HTTPAddr, opts.HTTPSAddr = s.addrs.HTTP.String(), s.addrs.HTTPS.String()
	opts.Resolver = s.deps.Resolver
	return opts
}

// newPlainServer builds the server of listen.http in the TLS modes (04 §9.1): the TLS manager's handler, which
// answers ACME http-01 challenges and redirects the rest to HTTPS. HTTP/1.1 only; nothing negotiates more on a
// plain port.
func newPlainServer(h http.Handler, log *slog.Logger, pending *pendingConns) *http.Server {
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: plainReadHeaderTimeout,
		ReadTimeout:       plainExchangeTimeout,
		WriteTimeout:      plainExchangeTimeout,
		IdleTimeout:       plainIdleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		Protocols:         &protocols,
		ConnState:         pending.track,
		ErrorLog:          slog.NewLogLogger(log.With(slog.String("component", "http")).Handler(), slog.LevelDebug),
	}
}

// limitListener caps the open connections of one client on the port 80 listener: limits.conns_per_ip, keyed like
// every per-IP count by netx.IPKey (an IPv4 address, an IPv6 /64). The 443 multiplexer has the same limit of its
// own (04 §7.2, §7.8). A connection over the limit is closed at once.
type limitListener struct {
	net.Listener
	max int

	mu   sync.Mutex
	open map[netip.Prefix]int
}

// newLimitListener wraps ln. A limit of zero or less means none.
func newLimitListener(ln net.Listener, perIP int) net.Listener {
	if perIP <= 0 {
		return ln
	}
	return &limitListener{Listener: ln, max: perIP, open: map[netip.Prefix]int{}}
}

func (l *limitListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		tcp, ok := c.RemoteAddr().(*net.TCPAddr)
		if !ok {
			return c, nil // not counted: there is no address to count by
		}
		key := netx.IPKey(tcp.AddrPort().Addr())
		l.mu.Lock()
		over := l.open[key] >= l.max
		if !over {
			l.open[key]++
		}
		l.mu.Unlock()
		if over {
			_ = c.Close()
			continue
		}
		return &limitedConn{Conn: c, release: sync.OnceFunc(func() { l.release(key) })}, nil
	}
}

func (l *limitListener) release(key netip.Prefix) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.open[key]--; l.open[key] <= 0 {
		delete(l.open, key)
	}
}

// limitedConn gives its place back when it closes.
type limitedConn struct {
	net.Conn
	release func()
}

func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.release()
	return err
}
