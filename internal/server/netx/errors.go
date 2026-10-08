package netx

import (
	"errors"
	"fmt"
)

// ErrNoTransport is returned by NewTransport when no ICE transport is left:
//   - UDP and 7882/tcp are both turned off (listen.ice_udp and listen.ice_tcp are "") and there is no 443
//     multiplexer;
//   - UDP found no usable local address;
//   - there is no UDP socket and no ICE-TCP listener has an address pion gathers on: loopback without
//     network.include_loopback, IPv6 without network.ipv6, or [::1], which never carries ICE-TCP (04 §7.6).
//
// The error's text says which.
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
