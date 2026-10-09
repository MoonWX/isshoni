package server

import (
	"net/http"
	"time"

	"github.com/MoonWX/isshoni/internal/server/netx"
	"github.com/MoonWX/isshoni/internal/server/ops"
	"github.com/MoonWX/isshoni/internal/server/tlsmgr"
)

// Hooks for the tests in package server_test.

// SetDrainingHook makes Shutdown call fn once readiness is off and the shutdown gate is closed, while the listeners
// still accept: the window of 04 §6.4 steps 3 and 4.
func (s *Server) SetDrainingHook(fn func()) {
	s.life.Lock()
	defer s.life.Unlock()
	s.hookDraining = fn
}

// Health returns the running server's health, so a test can add a failing check.
func (s *Server) Health() *ops.Health {
	s.life.Lock()
	defer s.life.Unlock()
	return s.health
}

// CloseHTTPListener closes the listen.http listener under the running server, as a failing socket would.
func (s *Server) CloseHTTPListener() error {
	s.life.Lock()
	defer s.life.Unlock()
	return s.httpLn.Close()
}

// SetPublicAddrs makes Start find these public addresses instead of looking at the machine's interfaces and asking
// STUN servers (04 §7.4). Call it before Start.
func (s *Server) SetPublicAddrs(pub netx.PublicAddrs) {
	s.life.Lock()
	defer s.life.Unlock()
	s.hookDetect = func() netx.PublicAddrs { return pub }
}

// SetPublicAddrsFunc is SetPublicAddrs with a function, which the running server also calls each time it looks at
// its public addresses again (04 §7.4). Call it before Start.
func (s *Server) SetPublicAddrsFunc(fn func() netx.PublicAddrs) {
	s.life.Lock()
	defer s.life.Unlock()
	s.hookDetect = fn
}

// SetRedetectEvery makes the running server look at its public addresses every d, in place of every ten minutes.
// Call it before Start.
func (s *Server) SetRedetectEvery(d time.Duration) {
	s.life.Lock()
	defer s.life.Unlock()
	s.redetectEvery = d
}

// PublicAddrsNow returns the public addresses as the running server last saw them, once they differ from the ones
// it started with; nil before.
func (s *Server) PublicAddrsNow() *netx.PublicAddrs { return s.publicNow.Load() }

// PortMux returns the running server's 443 multiplexer (nil in off mode), for a look at its counters.
func (s *Server) PortMux() *netx.PortMux {
	s.life.Lock()
	defer s.life.Unlock()
	return s.mux
}

// TLSManager returns the running server's TLS manager.
func (s *Server) TLSManager() *tlsmgr.Manager {
	s.life.Lock()
	defer s.life.Unlock()
	return s.tls
}

// PlainServer returns the running server's port 80 http.Server (nil in off mode), for a look at its limits.
func (s *Server) PlainServer() *http.Server {
	s.life.Lock()
	defer s.life.Unlock()
	return s.plainSrv
}

// MainServer returns the running server's main http.Server, for a look at its limits.
func (s *Server) MainServer() *http.Server {
	s.life.Lock()
	defer s.life.Unlock()
	return s.httpSrv
}

// PendingConns returns how many accepted connections have not delivered their first request header yet.
func (s *Server) PendingConns() int {
	s.pending.mu.Lock()
	defer s.pending.mu.Unlock()
	return len(s.pending.conns)
}
