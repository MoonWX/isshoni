package sfutest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/client/publish"
	"github.com/MoonWX/isshoni/internal/server/netx"
	"github.com/MoonWX/isshoni/internal/server/sfu"
)

// HarnessOptions configure a Harness.
type HarnessOptions struct {
	// NoUDP leaves out the SFU's UDP socket (the stand-in for 7882/udp), as listen.ice_udp = "" does.
	NoUDP bool
	// TCP adds the SFU's ICE-TCP listener (the stand-in for 7882/tcp).
	TCP bool
	// TCP443 adds 04's 443 multiplexer (netx.ListenPortMux) on 127.0.0.1:0; its ICE side gives the SFU's passive
	// TCP 443 candidates. Nothing serves the multiplexer's TLS side.
	TCP443 bool
	// Limits and PauseUnwatchedLayers go into sfu.Config.
	Limits               sfu.Limits
	PauseUnwatchedLayers bool
	// Logger gets the SFU's and the Transport's logs (default: discarded).
	Logger *slog.Logger
}

// Harness is a real sfu.SFU on loopback for in-process tests (02 §17): a netx.Transport on 127.0.0.1 with ephemeral
// ports, a RoomEventLog as the SFU's RoomEvents, and one DirectSignaler per joined Conn in place of the hub. Tests
// drive Conns directly, publish with a publish.Publisher (Publish) and watch with a Viewer (DirectSignaler.Attach).
type Harness struct {
	SFU       *sfu.SFU
	Transport *netx.Transport
	PortMux   *netx.PortMux // nil without HarnessOptions.TCP443
	Events    *RoomEventLog

	mu        sync.Mutex // guards signalers
	signalers []*DirectSignaler
	closeOnce sync.Once
	closeErr  error
}

// loopbackOnly is a netx.InterfaceLister with only 127.0.0.1, so the Transport's address plan doesn't depend on the
// host's interfaces.
type loopbackOnly struct{}

func (loopbackOnly) Interfaces() ([]netx.Interface, error) {
	return []netx.Interface{{
		Name:  "lo",
		Flags: net.FlagUp | net.FlagLoopback,
		Addrs: []netx.InterfaceAddr{{Addr: netip.MustParseAddr("127.0.0.1")}},
	}}, nil
}

func (loopbackOnly) RouteSource(context.Context, netip.Addr) (netip.Addr, error) {
	return netip.Addr{}, errors.New("sfutest: no route on the loopback-only host")
}

// NewHarness binds the Transport's sockets on 127.0.0.1 and builds the SFU on it. Close releases everything.
func NewHarness(ctx context.Context, o HarnessOptions) (*Harness, error) {
	h := &Harness{Events: &RoomEventLog{}}
	opts := netx.TransportOptions{IncludeLoopback: true, Interfaces: loopbackOnly{}, Logger: o.Logger}
	if !o.NoUDP {
		opts.UDPAddr = "127.0.0.1:0"
	}
	if o.TCP {
		opts.TCPAddr = "127.0.0.1:0"
	}
	if o.TCP443 {
		pm, err := netx.ListenPortMux(netx.PortMuxOptions{Addr: "127.0.0.1:0", Logger: o.Logger})
		if err != nil {
			return nil, fmt.Errorf("sfutest: 443 multiplexer: %w", err)
		}
		h.PortMux, opts.PortMux = pm, pm
	}
	tr, err := netx.NewTransport(ctx, opts)
	if err != nil {
		if h.PortMux != nil {
			_ = h.PortMux.Close()
		}
		return nil, fmt.Errorf("sfutest: transport: %w", err)
	}
	h.Transport = tr
	s, err := sfu.New(
		sfu.Config{Transport: tr, PauseUnwatchedLayers: o.PauseUnwatchedLayers, Limits: o.Limits},
		sfu.Deps{Events: h.Events, Logger: o.Logger},
	)
	if err != nil {
		_ = h.Close()
		return nil, fmt.Errorf("sfutest: new SFU: %w", err)
	}
	h.SFU = s
	return h, nil
}

// Close stops the DirectSignalers, then closes the SFU, the Transport and the 443 multiplexer, in the server's
// shutdown order (04 §6.4). It is idempotent. Viewers and Publishers are the test's to close.
func (h *Harness) Close() error {
	h.closeOnce.Do(func() {
		h.mu.Lock()
		signalers := h.signalers
		h.signalers = nil
		h.mu.Unlock()
		for _, d := range signalers {
			d.Close()
		}
		var errs []error
		if h.SFU != nil {
			errs = append(errs, h.SFU.Close())
		}
		if h.Transport != nil {
			errs = append(errs, h.Transport.Close())
		}
		if h.PortMux != nil {
			errs = append(errs, h.PortMux.Close())
		}
		h.closeErr = errors.Join(errs...)
	})
	return h.closeErr
}

// Join joins a Conn with a new DirectSignaler as its Signaler. An unset Client is the web client.
func (h *Harness) Join(p sfu.JoinParams) (*sfu.Conn, *DirectSignaler, error) {
	d := NewDirectSignaler()
	p.Signaler = d
	if p.Client == 0 {
		p.Client = sfu.ClientWeb
	}
	conn, err := h.SFU.Join(p)
	if err != nil {
		return nil, nil, err
	}
	h.mu.Lock()
	h.signalers = append(h.signalers, d)
	h.mu.Unlock()
	return conn, d, nil
}

// TCP443Port returns the port of the 443 multiplexer; 0 without HarnessOptions.TCP443.
func (h *Harness) TCP443Port() uint16 {
	if h.PortMux == nil {
		return 0
	}
	if ta, ok := h.PortMux.Addr().(*net.TCPAddr); ok {
		return ta.AddrPort().Port()
	}
	return 0
}

// LoopbackTCPSettings returns Pion settings for a client that uses ICE-TCP only, on loopback: it dials the SFU's
// passive TCP candidates (7882/tcp, or 443 through the multiplexer).
func LoopbackTCPSettings() webrtc.SettingEngine {
	var se webrtc.SettingEngine
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeTCP4})
	se.SetIncludeLoopbackCandidate(true)
	se.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	return se
}

// Bindings returns the tracks binding of a Publisher's offer (01's TrackRefs): its video and its audio m-section,
// each bound to share. Call it after Publisher.Offer, which assigns the mids.
func Bindings(pub *publish.Publisher, share sfu.ShareID) []sfu.TrackBinding {
	var out []sfu.TrackBinding
	for _, t := range pub.Tracks() {
		b := sfu.TrackBinding{MID: t.MID, Share: share, Kind: t.Kind}
		if t.MID != "" && !slices.Contains(out, b) {
			out = append(out, b)
		}
	}
	return out
}

// Publish runs one publish negotiation the way the hub does: the Publisher's offer with its tracks bound to share
// goes to conn.HandleOffer with gen and neg, and the answer back to the Publisher. It returns the answer. The
// Publisher's media starts with Publisher.Start.
//
// A second negotiation on the same Publisher (a re-offer, neg + 1) works for a Publisher with one video layer. With
// simulcast, Pion's re-offer lists every rid twice ("a=rid:f recv" from the SFU's answer next to "a=rid:f send"),
// which the SFU refuses as sfu.bad_rid like any repeated rid: publish.Publisher has to leave the recv lines out of
// the offers it sends before a simulcast Publisher can renegotiate.
func Publish(ctx context.Context, conn *sfu.Conn, pub *publish.Publisher, gen, neg uint32, share sfu.ShareID,
) (answerSDP string, err error) {
	offer, err := pub.Offer(ctx)
	if err != nil {
		return "", err
	}
	answerSDP, err = conn.HandleOffer(ctx, sfu.PCPub, gen, neg, offer.SDP, Bindings(pub, share))
	if err != nil {
		return "", err
	}
	if err := pub.SetAnswer(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answerSDP}); err != nil {
		return "", err
	}
	return answerSDP, nil
}

// ---- DirectSignaler ----

// Offer is one sub offer a Conn sent through its Signaler.
type Offer struct {
	PC       sfu.PCKind
	Gen, Neg uint32
	SDP      string
	Tracks   []sfu.TrackBinding
}

// DirectSignaler is an sfu.Signaler without a hub or a WebSocket: it records the offers and events a Conn sends and,
// once Attach gave it a Viewer, answers the Conn's sub offers with it. Like every Signaler it never blocks and never
// calls the SFU from the Conn's actor: the answers come from its own goroutine.
type DirectSignaler struct {
	mu       sync.Mutex // guards the fields below
	offers   []Offer
	events   []sfu.Event
	answered int     // offers[:answered] went to the Viewer
	errs     []error // what the answer loop ran into
	attached bool
	closed   bool

	wake   chan struct{} // capacity 1: an offer arrived
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewDirectSignaler returns a DirectSignaler that only records, until Attach.
func NewDirectSignaler() *DirectSignaler {
	return &DirectSignaler{wake: make(chan struct{}, 1)}
}

// SendOffer implements sfu.Signaler.
func (d *DirectSignaler) SendOffer(pc sfu.PCKind, gen, neg uint32, sdp string, tracks []sfu.TrackBinding) {
	d.mu.Lock()
	d.offers = append(d.offers, Offer{PC: pc, Gen: gen, Neg: neg, SDP: sdp, Tracks: slices.Clone(tracks)})
	d.mu.Unlock()
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// SendEvent implements sfu.Signaler.
func (d *DirectSignaler) SendEvent(ev sfu.Event) {
	d.mu.Lock()
	d.events = append(d.events, ev)
	d.mu.Unlock()
}

// Offers returns the offers sent so far, in order.
func (d *DirectSignaler) Offers() []Offer {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.offers)
}

// Events returns the events sent so far, in order.
func (d *DirectSignaler) Events() []sfu.Event {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.events)
}

// Errs returns the errors of the answer loop so far: a Viewer that couldn't answer, or what Conn.HandleAnswer
// returned.
func (d *DirectSignaler) Errs() []error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.errs)
}

// WaitOffer waits for an offer that match accepts and returns the first one.
func (d *DirectSignaler) WaitOffer(ctx context.Context, match func(Offer) bool) (Offer, error) {
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	for {
		for _, o := range d.Offers() {
			if match(o) {
				return o, nil
			}
		}
		select {
		case <-ctx.Done():
			return Offer{}, fmt.Errorf("sfutest: no matching offer: %w", ctx.Err())
		case <-tick.C:
		}
	}
}

// WaitEvent waits for an event that match accepts and returns the first one.
func (d *DirectSignaler) WaitEvent(ctx context.Context, match func(sfu.Event) bool) (sfu.Event, error) {
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	for {
		for _, ev := range d.Events() {
			if match(ev) {
				return ev, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("sfutest: no matching event: %w", ctx.Err())
		case <-tick.C:
		}
	}
}

// WaitPCState waits for a PCStateEvent of a PC kind and gen with the given state ("connected", …).
func (d *DirectSignaler) WaitPCState(ctx context.Context, pc sfu.PCKind, gen uint32, state string) error {
	_, err := d.WaitEvent(ctx, func(ev sfu.Event) bool {
		st, ok := ev.(sfu.PCStateEvent)
		return ok && st.PC == pc && st.Gen == gen && st.State == state
	})
	return err
}

// Attach makes v answer conn's sub offers, the ones already recorded first, each with Viewer.Answer and
// Conn.HandleAnswer, in order, until Close. A DirectSignaler takes one Viewer.
func (d *DirectSignaler) Attach(conn *sfu.Conn, v *Viewer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.attached || d.closed {
		return
	}
	d.attached = true
	// The answer loop is the DirectSignaler's own top-level goroutine; Close ends it.
	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	d.wg.Go(func() { d.answerLoop(ctx, conn, v) })
}

func (d *DirectSignaler) answerLoop(ctx context.Context, conn *sfu.Conn, v *Viewer) {
	for {
		for {
			d.mu.Lock()
			if d.answered == len(d.offers) {
				d.mu.Unlock()
				break
			}
			o := d.offers[d.answered]
			d.answered++
			d.mu.Unlock()
			if err := answerOffer(ctx, conn, v, o); err != nil && ctx.Err() == nil {
				d.mu.Lock()
				d.errs = append(d.errs, fmt.Errorf("sub offer gen %d neg %d: %w", o.Gen, o.Neg, err))
				d.mu.Unlock()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-d.wake:
		}
	}
}

// answerOffer applies one sub offer on the Viewer and gives its answer to the Conn.
func answerOffer(ctx context.Context, conn *sfu.Conn, v *Viewer, o Offer) error {
	answer, err := v.Answer(ctx, webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: o.SDP})
	if err != nil {
		return err
	}
	return conn.HandleAnswer(ctx, o.PC, o.Gen, o.Neg, answer.SDP)
}

// Close stops the answer loop and waits for it. Recording goes on. It is idempotent.
func (d *DirectSignaler) Close() {
	d.mu.Lock()
	d.closed = true
	cancel := d.cancel
	d.cancel = nil
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	d.wg.Wait()
}

// ---- RoomEventLog ----

// RoomEvent is one call of the SFU's RoomEvents.
type RoomEvent struct {
	Kind   RoomEventKind
	Room   sfu.RoomID
	Share  sfu.ShareInfo  // ShareUpdated, ShareEnded
	Reason sfu.EndReason  // ShareEnded
	Policy sfu.ProfileKey // CodecPolicyChanged
}

// RoomEventKind names the RoomEvents method of a RoomEvent.
type RoomEventKind uint8

// The RoomEvents methods.
const (
	ShareUpdated RoomEventKind = iota + 1
	ShareEnded
	CodecPolicyChanged
)

// RoomEventLog is an sfu.RoomEvents that records every call, in order. It never blocks.
type RoomEventLog struct {
	mu     sync.Mutex
	events []RoomEvent
}

// ShareUpdated implements sfu.RoomEvents.
func (l *RoomEventLog) ShareUpdated(room sfu.RoomID, s sfu.ShareInfo) {
	l.add(RoomEvent{Kind: ShareUpdated, Room: room, Share: s})
}

// ShareEnded implements sfu.RoomEvents.
func (l *RoomEventLog) ShareEnded(room sfu.RoomID, s sfu.ShareInfo, r sfu.EndReason) {
	l.add(RoomEvent{Kind: ShareEnded, Room: room, Share: s, Reason: r})
}

// CodecPolicyChanged implements sfu.RoomEvents.
func (l *RoomEventLog) CodecPolicyChanged(room sfu.RoomID, p sfu.ProfileKey) {
	l.add(RoomEvent{Kind: CodecPolicyChanged, Room: room, Policy: p})
}

func (l *RoomEventLog) add(ev RoomEvent) {
	l.mu.Lock()
	l.events = append(l.events, ev)
	l.mu.Unlock()
}

// Events returns the calls recorded so far, in order.
func (l *RoomEventLog) Events() []RoomEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

// Wait waits for a recorded call that match accepts and returns the first one.
func (l *RoomEventLog) Wait(ctx context.Context, match func(RoomEvent) bool) (RoomEvent, error) {
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	for {
		for _, ev := range l.Events() {
			if match(ev) {
				return ev, nil
			}
		}
		select {
		case <-ctx.Done():
			return RoomEvent{}, fmt.Errorf("sfutest: no matching room event: %w", ctx.Err())
		case <-tick.C:
		}
	}
}
