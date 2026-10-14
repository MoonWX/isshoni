//go:build !unix

package server

// Reexec returns ErrNoReexec: this platform (Windows, which builds the server for development only) can't replace
// a running process by a new run of itself. cmd/isshoni then exits 75, "restart required" (04 §3.2, §6.5).
func Reexec() error { return ErrNoReexec }
