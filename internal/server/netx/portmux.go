package netx

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"time"
)

// This file declares the 443 first-byte multiplexer of 04 §7.2. README S26 implements it; until then
// ListenPortMux returns ErrNotImplemented. IPKey is complete: the 7882/tcp limit already uses it.

// PortMuxOptions configures ListenPortMux.
type PortMuxOptions struct {
	Addr             string        // ":443"
	ClassifyTimeout  time.Duration // 10 s: time allowed for the first byte
	MaxPending       int           // 1024 connections waiting for their first byte; when full, the oldest is closed
	MaxPendingPerIP  int           // 32 per IPKey
	MaxConnsPerIP    int           // limits.conns_per_ip (256): open TLS + ICE connections per IPKey
	MaxICEConnsPerIP int           // 64 per IPKey; one count across 443 and 7882/tcp (§7.3)
	QueueLen         int           // 128 per sub-listener; a full queue for 1 s drops the connection
	PublicHost       string        // for the plain-HTTP hint response
	Counter          *TransferCounter
	Logger           *slog.Logger
}

// PortMux splits one TCP port (443) by the first byte of each connection: TLS (0x16) to TLS(), RFC 4571 framed
// ICE (0x00–0x02) to ICE() (04 §7.2).
type PortMux struct {
	addr net.Addr
	tls  net.Listener
	ice  net.Listener
	// iceConns is the per-IPKey ICE-TCP count, shared with Transport's 7882/tcp listener (one count of
	// MaxICEConnsPerIP across both ports, 04 §7.3).
	iceConns *connLimiter
}

// PortMuxStats counts accepted-connection outcomes.
type PortMuxStats struct {
	TLS, ICE, PlainHTTP, Garbage, Timeout, Limited uint64
}

// ListenPortMux binds opts.Addr and starts classifying connections. Not implemented yet (README S26).
func ListenPortMux(opts PortMuxOptions) (*PortMux, error) {
	return nil, fmt.Errorf("netx: 443 multiplexer on %q: %w", opts.Addr, ErrNotImplemented)
}

// TLS returns the listener of connections whose first byte was 0x16; the byte is replayed.
func (m *PortMux) TLS() net.Listener { return m.tls }

// ICE returns the listener of connections that start with an RFC 4571 frame. Its Addr() is the *net.TCPAddr of
// :443, and its Close is idempotent (Transport.Close closes it, then PortMux.Close again). Close must also close
// every connection it handed out that is still open; pion's TCPMuxDefault.Close otherwise waits up to
// FirstStunBindTimeout. (Transport wraps it in an iceListener that does this as well.)
func (m *PortMux) ICE() net.Listener { return m.ice }

// Addr returns the raw listener's address.
func (m *PortMux) Addr() net.Addr { return m.addr }

// Close closes the raw listener and both sub-listeners; Accept then returns net.ErrClosed.
func (m *PortMux) Close() error {
	var errs []error
	for _, l := range []net.Listener{m.tls, m.ice} {
		if l != nil {
			if err := l.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// Stats returns the accepted-connection outcomes so far. Limited includes m.iceConns.refused: the 7882/tcp ICE
// connections that the shared ICE limit refused (04 §7.3), so they appear in
// isshoni_portmux_conns_total{result="limited"} whenever the limit is shared. README S26 adds the 443 outcomes; a 443
// refusal is counted once, either in m.iceConns.refused or in PortMux's own count.
func (m *PortMux) Stats() PortMuxStats {
	var s PortMuxStats
	if m.iceConns != nil {
		s.Limited = m.iceConns.refused.Load()
	}
	return s
}

// iceLimiter returns the per-IPKey ICE connection count that Transport's 7882/tcp listener shares.
func (m *PortMux) iceLimiter() *connLimiter { return m.iceConns }

// IPKey is the key of every per-IP count in netx (pending, open and ICE, on 443, 80 and 7882/tcp): an IPv4 address
// (IPv4-mapped IPv6 is unmapped first) as its /32, an IPv6 address as its /64. It gives the same result as 03's
// limiter key (03 §7.3) and 01's pre-auth key (01 §3.1). An invalid address gives the zero Prefix.
func IPKey(a netip.Addr) netip.Prefix {
	a = a.Unmap().WithZone("")
	bits := 64
	if a.Is4() {
		bits = 32
	}
	p, err := a.Prefix(bits)
	if err != nil {
		return netip.Prefix{}
	}
	return p
}
