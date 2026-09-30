package netx

import (
	"errors"
	"fmt"
)

// ErrNotImplemented is returned by the parts of the package that a later slice fills in (the 443 multiplexer,
// README S26).
var ErrNotImplemented = errors.New("netx: not implemented")

// ErrNoTransport is returned by NewTransport when no ICE transport is left: UDP and 7882/tcp are both turned off
// (listen.ice_udp and listen.ice_tcp are "") and there is no 443 multiplexer, or UDP found no usable local address.
var ErrNoTransport = errors.New("netx: no ICE transport")

// ListenError reports a socket that NewTransport could not bind. errors.Is(err, syscall.EADDRINUSE) tells a busy
// port, which the wiring reports as "port 7882/udp is in use (another isshoni?) …" (04 §6.1 step 6).
type ListenError struct {
	Proto string // "udp" | "tcp"
	Addr  string // the address it tried, e.g. "203.0.113.7:7882" or ":7882"
	Err   error
}

func (e *ListenError) Error() string {
	return fmt.Sprintf("netx: listen %s %s: %v", e.Proto, e.Addr, e.Err)
}

func (e *ListenError) Unwrap() error { return e.Err }
