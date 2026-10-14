package netx

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"

	"github.com/pion/ice/v4"
	"github.com/pion/stun/v4"
)

// This file is the ICE source-address guard of 04 §7.8 and 02 §12 (review R3).
//
// A STUN Binding request with a valid USERNAME and MESSAGE-INTEGRITY from a source address that Pion's ICE agent
// doesn't know makes it create a peer-reflexive remote candidate, pair it with every local candidate and answer it;
// the answer registers the address in the UDP mux's address map. Pion keeps all of that until an ICE restart or
// close and has no cap of its own, and 02's 64-candidate cap covers only the candidates a client signals. A member
// has its own PC's ufrag and password (from its SDP), so without a bound it could grow that state from any number of
// source ports. The guard bounds it where netx builds the muxes, so the excess never reaches Pion.

// maxSourcesPerUfrag is the number of distinct remote transport addresses that one local ICE ufrag accepts STUN from
// (04 §7.8, 02 §12): UDP and ICE-TCP, every local socket and both TCP ports counted together.
const maxSourcesPerUfrag = 64

// maxRemovedUfrags is the number of removed ufrags the guard remembers, so that none of them becomes known again
// (sourceGuard.forget).
const maxRemovedUfrags = 1024

// maxFirstFrame is the largest first ICE-TCP frame the guard reads ahead to find the frame's ufrag. Pion reads the
// first frame into 512 bytes and closes the connection when it is longer, so a first frame this long is never one
// Pion would take; the guard closes it itself.
const maxFirstFrame = 1500

var (
	// errICESourceLimit ends an ICE-TCP connection whose first frame names a ufrag that has its 64 sources already.
	errICESourceLimit = errors.New("netx: too many ICE source addresses for one ufrag")
	// errFirstFrameTooLarge ends an ICE-TCP connection whose first frame is longer than maxFirstFrame.
	errFirstFrameTooLarge = errors.New("netx: first ICE-TCP frame too large")
)

// sourceGuard counts, per local ICE ufrag, the distinct remote transport addresses (protocol, IP and port) that have
// sent it STUN with a USERNAME. Once a ufrag has maxSourcesPerUfrag of them, such STUN from any further address is
// dropped before Pion sees it: no Binding response, no peer-reflexive candidate, no address-map entry. One guard
// serves the whole Transport:
//   - UDP: every packet of every UDP socket, beneath the UDP mux (countingPacketConn), so the mux never routes the
//     excess by its ufrag;
//   - ICE-TCP: the first RFC 4571 frame of each connection on 443 and 7882 (firstFrameGate), the frame by which
//     Pion's TCP mux gives the connection to a ufrag. A refused connection is closed.
//
// A ufrag is known from the moment one of the Transport's muxes hands out a connection for it (GetConn and
// GetConnByUfrag, while Pion gathers, before any SDP with the ufrag leaves the server) until RemoveConnByUfrag (the
// PC closes, restarts ICE with a new ufrag, or fails). Its addresses are never released before that, as Pion keeps
// its peer-reflexive candidates until then; an address counted once costs nothing when it comes again. A removed
// ufrag never becomes known again: an ICE restart or a close cancels the agent's gathering without waiting for it,
// so the cancelled gathering may still ask the muxes for the old ufrag after Pion removed it, and Pion then closes
// that connection itself, through the inner mux, which the guard never hears of. This relies on Pion never reusing
// a ufrag: it makes a random one for every agent and ICE restart, and the server never sets ICE credentials (with
// fixed ones Pion would remove the ufrag while creating the agent, and the guard would never count it). The guard
// remembers the last maxRemovedUfrags removed ufrags, far more than are removed while a cancelled gathering runs.
//
// What the guard doesn't count passes: a packet that isn't STUN or has no USERNAME (the UDP mux routes it only by its
// address map, which holds the addresses the agent has sent to: signaled candidates, which 02 caps, and sources the
// guard admitted), and STUN for a ufrag the guard doesn't know. Either no mux has a connection for it (the UDP mux
// drops it, the TCP mux holds it 30 s at most, within the per-IP ICE-TCP limit), or the ufrag was removed and only a
// cancelled gathering's connection has it for a moment: Pion closes that at once, and the agent rejects STUN for a
// ufrag that is no longer its own.
//
// Sources are counted before Pion checks MESSAGE-INTEGRITY (netx doesn't know the password): someone who sees a PC's
// ufrag on the path and spoofs UDP sources can use up its 64 slots; sources already admitted keep working and an ICE
// restart (new ufrag) starts over. Counting a source only once Pion answers it wouldn't close this (a spoofer
// resending holds the slots all the same) and would loosen the bound, so the guard doesn't.
type sourceGuard struct {
	max int
	log *slog.Logger

	refused atomic.Uint64 // STUN packets and ICE-TCP connections dropped at the limit

	mu          sync.Mutex
	ufrags      map[string]*ufragSources
	removed     map[string]struct{} // the last maxRemovedUfrags removed ufrags, never known again
	removedRing []string            // removed's ufrags, oldest at removedNext once it holds maxRemovedUfrags
	removedNext int
}

// ufragSources is one known ufrag's admitted addresses.
type ufragSources struct {
	addrs  map[iceSource]struct{}
	capped bool // the limit was reached (logged once)
}

// iceSource is one remote transport address: a UDP and a TCP source on one address and port are two.
type iceSource struct {
	tcp  bool
	addr netip.AddrPort
}

func newSourceGuard(maxPerUfrag int, log *slog.Logger) *sourceGuard {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &sourceGuard{
		max: maxPerUfrag, log: log,
		ufrags: map[string]*ufragSources{}, removed: map[string]struct{}{},
	}
}

// open makes ufrag known with no addresses, unless it is known already or was removed.
func (g *sourceGuard) open(ufrag string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.removed[ufrag]; ok {
		return
	}
	if _, ok := g.ufrags[ufrag]; !ok {
		g.ufrags[ufrag] = &ufragSources{addrs: map[iceSource]struct{}{}}
	}
}

// forget drops ufrag and its addresses, and remembers it as removed, so that no later open makes it known again
// (a gathering that an ICE restart or a close cancelled). Pion removes a ufrag from every mux, and may remove one it
// never gathered for, so ufrag need not be known.
func (g *sourceGuard) forget(ufrag string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.ufrags, ufrag)
	if _, ok := g.removed[ufrag]; ok {
		return
	}
	if len(g.removedRing) < maxRemovedUfrags {
		g.removedRing = append(g.removedRing, ufrag)
	} else {
		delete(g.removed, g.removedRing[g.removedNext])
		g.removedRing[g.removedNext] = ufrag
		g.removedNext = (g.removedNext + 1) % maxRemovedUfrags
	}
	g.removed[ufrag] = struct{}{}
}

// admit reports whether STUN for ufrag from src may reach Pion, and counts src when it is new to ufrag.
func (g *sourceGuard) admit(ufrag []byte, src iceSource) bool {
	g.mu.Lock()
	u := g.ufrags[string(ufrag)]
	switch {
	case u == nil:
		g.mu.Unlock()
		return true
	case hasSource(u, src):
		g.mu.Unlock()
		return true
	case len(u.addrs) < g.max:
		u.addrs[src] = struct{}{}
		g.mu.Unlock()
		return true
	}
	first := !u.capped
	u.capped = true
	g.mu.Unlock()
	g.refused.Add(1)
	if first {
		// No ufrag and no address: both identify a client's connection (README §4 "Logging").
		proto := "udp"
		if src.tcp {
			proto = "tcp"
		}
		g.log.Debug("ICE source limit reached for a connection; STUN from further addresses is dropped",
			"max", g.max, "proto", proto)
	}
	return false
}

func hasSource(u *ufragSources, src iceSource) bool {
	_, ok := u.addrs[src]
	return ok
}

// admitUDP judges one UDP packet from from: STUN with a USERNAME goes through admit, everything else passes.
func (g *sourceGuard) admitUDP(b []byte, from netip.AddrPort) bool {
	if g == nil || !stun.IsMessage(b) {
		return true
	}
	ufrag, ok := stunUfrag(b)
	if !ok {
		return true
	}
	return g.admit(ufrag, iceSource{addr: canonicalSource(from)})
}

// sources returns the number of addresses ufrag has admitted (for tests).
func (g *sourceGuard) sources(ufrag string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if u := g.ufrags[ufrag]; u != nil {
		return len(u.addrs)
	}
	return 0
}

// known returns the number of known ufrags (for tests).
func (g *sourceGuard) known() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.ufrags)
}

// stunUfrag returns the local ufrag that a STUN message is for: the part of its USERNAME before the first colon,
// exactly as Pion's muxes route it ("<receiver ufrag>:<sender ufrag>"). ok is false when the message doesn't decode
// or has no USERNAME; Pion then routes it by its source address only, or drops it. The ufrag aliases b.
func stunUfrag(b []byte) (ufrag []byte, ok bool) {
	m := stun.Message{Raw: b}
	if err := m.Decode(); err != nil {
		return nil, false
	}
	u, err := m.Get(stun.AttrUsername)
	if err != nil {
		return nil, false
	}
	if i := bytes.IndexByte(u, ':'); i >= 0 {
		u = u[:i]
	}
	return u, true
}

// canonicalSource is the form of a source address that Pion keys its remote candidates by: IPv4-mapped addresses as
// IPv4, and no zone except on IPv6 link-local addresses.
func canonicalSource(ap netip.AddrPort) netip.AddrPort {
	a := ap.Addr().Unmap()
	if !a.Is6() || !a.IsLinkLocalUnicast() {
		a = a.WithZone("")
	}
	return netip.AddrPortFrom(a, ap.Port())
}

// guardedUDPMux is one UDP socket's ice.UDPMuxDefault that tells the guard which ufrags it serves. The Transport
// combines these in its ice.MultiUDPMuxDefault; LocalAddr, GetListenAddresses and Close are the mux's own.
type guardedUDPMux struct {
	*ice.UDPMuxDefault
	guard *sourceGuard
}

var _ ice.UDPMux = (*guardedUDPMux)(nil)

// GetConn makes ufrag known to the guard before the mux can route anything to it (unless it was removed already).
func (m *guardedUDPMux) GetConn(ufrag string, addr net.Addr) (net.PacketConn, error) {
	m.guard.open(ufrag)
	return m.UDPMuxDefault.GetConn(ufrag, addr)
}

// RemoveConnByUfrag forgets ufrag once the mux no longer routes anything to it.
func (m *guardedUDPMux) RemoveConnByUfrag(ufrag string) {
	m.UDPMuxDefault.RemoveConnByUfrag(ufrag)
	m.guard.forget(ufrag)
}

// guardedTCPMux is the 443 or the 7882 ice.TCPMuxDefault, telling the guard which ufrags it serves. It keeps the
// mux's LocalAddr, which Pion asks a TCP mux for when it is used alone (02's per-transport probe APIs).
type guardedTCPMux struct {
	*ice.TCPMuxDefault
	guard *sourceGuard
}

var _ ice.TCPMux = (*guardedTCPMux)(nil)

// GetConnByUfrag makes ufrag known to the guard before the mux can route a connection to it (unless it was removed
// already).
func (m *guardedTCPMux) GetConnByUfrag(ufrag string, isIPv6 bool, local net.IP) (net.PacketConn, error) {
	m.guard.open(ufrag)
	return m.TCPMuxDefault.GetConnByUfrag(ufrag, isIPv6, local)
}

// RemoveConnByUfrag forgets ufrag once the mux has closed its connections.
func (m *guardedTCPMux) RemoveConnByUfrag(ufrag string) {
	m.TCPMuxDefault.RemoveConnByUfrag(ufrag)
	m.guard.forget(ufrag)
}

// firstFrameGate is an ICE-TCP connection on its way to Pion's TCP mux. Its first Read reads the first RFC 4571 frame
// ahead and judges it: a STUN message whose USERNAME names a known ufrag from a remote address beyond that ufrag's
// limit fails the Read (Pion then closes the connection without answering), as does a first frame longer than
// maxFirstFrame. Otherwise the frame is returned before the rest of the connection, as if read directly. Pion reads
// a connection from one goroutine at a time, first the handshake goroutine, then the connection's reader.
type firstFrameGate struct {
	net.Conn
	guard *sourceGuard

	judged bool
	ahead  []byte // what is left of the first frame
}

func newFirstFrameGate(c net.Conn, guard *sourceGuard) *firstFrameGate {
	return &firstFrameGate{Conn: c, guard: guard}
}

func (c *firstFrameGate) Read(b []byte) (int, error) {
	if !c.judged {
		c.judged = true
		if err := c.readFirstFrame(); err != nil {
			return 0, err
		}
	}
	if len(c.ahead) > 0 {
		n := copy(b, c.ahead)
		c.ahead = c.ahead[n:]
		return n, nil
	}
	return c.Conn.Read(b)
}

// readFirstFrame reads the first frame into c.ahead, or fails. Pion's read deadline for the first STUN request (10 s)
// is already set on the connection.
func (c *firstFrameGate) readFirstFrame() error {
	var hdr [2]byte
	if _, err := io.ReadFull(c.Conn, hdr[:]); err != nil {
		return err
	}
	size := int(binary.BigEndian.Uint16(hdr[:]))
	if size > maxFirstFrame {
		return errFirstFrameTooLarge
	}
	frame := make([]byte, 2+size)
	copy(frame, hdr[:])
	if _, err := io.ReadFull(c.Conn, frame[2:]); err != nil {
		return err
	}
	if ufrag, ok := stunUfrag(frame[2:]); ok {
		if src, ok := tcpSource(c.RemoteAddr()); ok && !c.guard.admit(ufrag, src) {
			return errICESourceLimit
		}
	}
	c.ahead = frame
	return nil
}

// tcpSource is the guard's key of an ICE-TCP connection's remote address.
func tcpSource(a net.Addr) (iceSource, bool) {
	var ap netip.AddrPort
	switch ta := a.(type) {
	case nil:
		return iceSource{}, false
	case *net.TCPAddr:
		ap = ta.AddrPort()
	default:
		p, err := netip.ParseAddrPort(a.String())
		if err != nil {
			return iceSource{}, false
		}
		ap = p
	}
	if !ap.IsValid() {
		return iceSource{}, false
	}
	return iceSource{tcp: true, addr: canonicalSource(ap)}, true
}
