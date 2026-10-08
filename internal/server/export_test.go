package server

import (
	"net/http"

	"github.com/MoonWX/isshoni/internal/server/ops"
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

// CloseHTTPListener closes the main listener under the running server, as a failing socket would.
func (s *Server) CloseHTTPListener() error {
	s.life.Lock()
	defer s.life.Unlock()
	return s.httpLn.Close()
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
