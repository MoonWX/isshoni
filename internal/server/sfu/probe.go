package sfu

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
)

// This file is the connection-test probe of 02 §7.6: a PeerConnection per test and transport, on the probe API of
// that transport (api.go), that answers a browser's data-channel offer, echoes what the browser sends on the channel
// and reports once whether it connected. The REST endpoint on top of it is 04's (POST /api/v1/conntest, 04 §7.7).

// ProbeResult is the outcome of one connection-test probe PC (02 §7.6).
type ProbeResult struct {
	Transport ProbeTransport
	Connected bool
	Selected  string        // "udp 203.0.113.5:7882" (server side of the nominated pair)
	RTT       time.Duration // from the candidate-pair stats
	Err       string        // "timeout" | "closed"
}

// The Err values of a ProbeResult that didn't connect.
const (
	// ProbeErrTimeout: the probe didn't connect within its life, or its ICE failed (8 s without a working pair) or
	// its DTLS handshake did.
	ProbeErrTimeout = "timeout"
	// ProbeErrClosed: the probe ended before it connected, because the client closed its side (its data channel or
	// its PeerConnection), the user started another probe of the same transport, or the SFU closed.
	ProbeErrClosed = "closed"
)

// The connection-test probe's guards and timing (02 §7.6, §12). The ICE and DTLS timeouts of a probe PC are its API's
// (api.go: disconnected 3 s, failed 8 s, DTLS 10 s).
const (
	// maxProbes is how many probe PCs one server runs at once; one more is sfu.probe_limit (retryable). A user's new
	// probe of a transport replaces the old one, so it needs no slot of its own.
	maxProbes = 20
	// probeLifetime is how long a probe PC lives at most. It ends sooner when its data channel closes, when the client
	// closes its PeerConnection, or when ICE fails.
	probeLifetime = 20 * time.Second
	// probeRTTAfter: a probe that connected is reported this long afterwards, with the round-trip time that ICE has
	// measured on the selected candidate pair by then.
	probeRTTAfter = time.Second
	// probeMinRetryAfter is the least RetryAfter of sfu.probe_limit: a probe whose life has just ended keeps its slot
	// until its PC is closed.
	probeMinRetryAfter = time.Second
	// maxProbeOfferBytes: a probe offer above this is sfu.bad_sdp. A browser's offer with one data channel is about
	// 1 KB, a few more with its candidates; 01 §3.3 allows a pub offer as much.
	maxProbeOfferBytes = 64 << 10
	// maxProbeEchoBuffered: a message that arrives while this many bytes of echoes are still waiting to go out is not
	// echoed. Pion queues what a data channel sends without a bound, so a client that sends without reading its
	// echoes would otherwise have the server hold them all for the probe's 20 s.
	maxProbeEchoBuffered = 1 << 20
)

// valid reports whether t is one of the three probe transports.
func (t ProbeTransport) valid() bool {
	return t == ProbeUDP || t == ProbeTCP443 || t == ProbeTCP7882
}

// Probe answers a connection-test offer on one ICE transport (02 §7.6). offerSDP is a browser offer with one data
// channel (m=application) and no media; its candidates go through the remote-candidate filter (02 §7.3) and the cap
// of 64 (02 §12; past it they are dropped), and none are needed: the browser's checks reach the server's candidates,
// and the server learns the browser's address from them (peer-reflexive). The answer is complete (no trickle) and
// holds only candidates of transport t. The probe PC echoes every data-channel message unchanged, so the browser
// measures the round-trip time itself, and it lives at most 20 s, or until its data channel closes. ctx bounds the
// call only; the probe PC lives on after it returns.
//
// res gets one value, when the probe connects (1 s later, with the RTT from the candidate-pair stats) or when it
// ends without having connected (Err "timeout" or "closed"). It has capacity 1 and the SFU never blocks on it, so a
// caller may ignore it (04's wiring does).
//
// Guards: a user has one probe per transport, and a new one closes the previous one. 20 probes run per server; one
// more is sfu.probe_limit (retryable), with RetryAfter until the oldest running probe's life ends. Errors:
// sfu.transport_disabled when t has no mux (udp with listen.ice_udp = "", tcp443 in tls.mode=off, tcp7882 with
// listen.ice_tcp = ""); sfu.bad_sdp for an offer that is empty, larger than 64 KiB, unparsable, not exactly one data
// channel without media, or one Pion refuses; sfu.closed after Close, and for a probe that the same user's next one
// replaced before this call returned; sfu.internal for an empty user or an unknown transport (a contract violation,
// not retryable) and for Pion failing (retryable).
func (s *SFU) Probe(ctx context.Context, user UserID, t ProbeTransport, offerSDP string,
) (answerSDP string, res <-chan ProbeResult, err error) {
	switch {
	case user == "":
		return "", nil, errCaller("Probe: the user id is empty")
	case !t.valid():
		return "", nil, errCaller("Probe: unknown transport")
	}
	if err := s.Ready(); err != nil {
		return "", nil, err // closed; addProbe checks again, under the lock
	}
	api := s.apis.probe[t]
	if api == nil {
		return "", nil, newError(CodeTransportDisabled, "the server has no "+string(t)+" ICE transport")
	}
	if err := checkProbeOffer(offerSDP); err != nil {
		return "", nil, err
	}
	offer, dropped, err := s.apis.filter.filterProbeOffer(offerSDP)
	if err != nil {
		return "", nil, err
	}

	p := s.newProbe(user, t)
	old, err := s.addProbe(p)
	if err != nil {
		return "", nil, err
	}
	if dropped > 0 {
		p.log.Debug("remote candidates dropped from a probe offer", "count", dropped)
	}
	if old != nil {
		p.log.Debug("probe replaces the user's previous probe of the transport")
		old.stop()
	}
	answer, err := p.start(ctx, api, offer)
	if err != nil {
		return "", nil, err
	}
	return answer, p.res, nil
}

// checkProbeOffer is the SDP guard of a probe offer: sfu.bad_sdp unless it is at most 64 KiB, parses, and has exactly
// one m-section, a data channel. The remote-candidate filter and Pion check the rest.
func checkProbeOffer(raw string) error {
	if len(raw) > maxProbeOfferBytes {
		return newError(CodeBadSDP, "probe offer is "+strconv.Itoa(len(raw))+" bytes (max "+
			strconv.Itoa(maxProbeOfferBytes)+")")
	}
	desc, err := parseSDP(raw)
	if err != nil {
		return err
	}
	if n := len(desc.MediaDescriptions); n != 1 || desc.MediaDescriptions[0].MediaName.Media != "application" {
		return newError(CodeBadSDP, "a probe offer has one data channel and no media; this one has "+
			strconv.Itoa(n)+" m-sections")
	}
	return nil
}

// filterProbeOffer returns a probe offer without the candidates that the remote-candidate filter drops (02 §7.3) and
// those past the first 64 it keeps (02 §12, counted as remoteCandidates counts a Conn's), and how many it dropped.
// The offer is the probe PC's only remote description and nothing is trickled to it, so the count starts and ends
// here. A browser's probe offer has no candidates (02 §7.6), but a client could post a 64 KiB one with some 1,400,
// and the probe PC, the controlled ICE agent, sends checks to every pair it has.
func (f candidateFilter) filterProbeOffer(raw string) (string, int, error) {
	var budget remoteCandidates
	return f.limitSDP(raw, budget.admit)
}

// probeKey is what a user's probes are counted by: one per user and transport.
type probeKey struct {
	user UserID
	t    ProbeTransport
}

// probe is one connection-test probe PC and its goroutine (run), which owns the PC once it has started: it ends the
// probe at the end of its life, when the PC fails or closes, when the data channel closes, or when the probe is
// stopped (a replacing probe, SFU.Close), and then sends the result if it hasn't yet, closes the PC and frees the
// probe's slot. Pion's callbacks only wake it, and the echo writes to Pion's send queue, which never blocks (02 §5.4).
type probe struct {
	sfu      *SFU
	key      probeKey
	log      *slog.Logger
	deadline time.Time        // the end of the probe's life: its start plus probeLifetime (timings.probeLife)
	res      chan ProbeResult // capacity 1: one value, sent once and never waited on

	pc        *webrtc.PeerConnection // set by start before run starts; nil when it couldn't be created
	handedOut atomic.Bool            // Probe returned its answer: its end is worth an info line
	channel   atomic.Bool            // the PC has its data channel; later ones are closed at once

	wake       chan struct{} // capacity 1: the PC's connection state changed (run reads it then)
	stopCh     chan struct{} // closed by stop
	stopOnce   sync.Once
	chanClosed chan struct{} // closed when the data channel closed
	chanOnce   sync.Once
	done       chan struct{} // closed when the probe has ended: its PC is closed and its slot free
}

// newProbe returns a probe that hasn't started; its life starts now.
func (s *SFU) newProbe(user UserID, t ProbeTransport) *probe {
	return &probe{
		sfu:        s,
		key:        probeKey{user: user, t: t},
		log:        s.log.With("user_id", string(user), "transport", string(t)),
		deadline:   time.Now().Add(s.timing.probeLife),
		res:        make(chan ProbeResult, 1),
		wake:       make(chan struct{}, 1),
		stopCh:     make(chan struct{}),
		chanClosed: make(chan struct{}),
		done:       make(chan struct{}),
	}
}

// addProbe gives p a slot (02 §7.6): the slot of the user's probe of the same transport, which it returns for the
// caller to stop, or a free one. Without a free slot it is sfu.probe_limit, with RetryAfter until the oldest running
// probe's life ends; after Close it is sfu.closed.
func (s *SFU) addProbe(p *probe) (old *probe, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errClosed("the SFU")
	}
	old = s.probes[p.key]
	if old == nil && len(s.probes) >= maxProbes {
		oldest := p.deadline
		for _, q := range s.probes {
			if q.deadline.Before(oldest) {
				oldest = q.deadline
			}
		}
		e := newError(CodeProbeLimit, strconv.Itoa(maxProbes)+" probes are running")
		e.RetryAfter = max(time.Until(oldest), probeMinRetryAfter)
		return nil, e
	}
	s.probes[p.key] = p
	s.probing[p] = struct{}{}
	return old, nil
}

// removeProbe frees p's slot, unless a newer probe of the same user and transport has it already.
func (s *SFU) removeProbe(p *probe) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.probes[p.key] == p {
		delete(s.probes, p.key)
	}
	delete(s.probing, p)
}

// start creates the probe's PC, starts its goroutine and answers the offer (already checked and filtered) with every
// candidate in the answer. A failure ends the probe.
func (p *probe) start(ctx context.Context, api *webrtc.API, offer string) (string, error) {
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		p.log.Warn("probe PC not created", "err", err)
		p.finish()
		return "", newError(CodeInternal, "the probe PC could not be created")
	}
	p.pc = pc
	pc.OnConnectionStateChange(func(webrtc.PeerConnectionState) { p.poke() })
	pc.OnDataChannel(p.onDataChannel)
	go p.run()

	fail := func(e *Error, msg string, err error) (string, error) {
		stopped := p.stopped()
		p.stop()
		if stopped {
			// Pion's error is the PC that run closed: a replacing probe or SFU.Close came first.
			return "", errClosed("the probe")
		}
		p.log.Debug(msg, "err", err)
		return "", e
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		return fail(newError(CodeBadSDP, "the probe offer can't be applied"), "probe offer refused by SetRemoteDescription", err)
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return fail(newError(CodeInternal, "the probe answer could not be created"), "probe answer not created", err)
	}
	local, complete, err := setLocalComplete(ctx, pc, answer)
	if err != nil {
		return fail(newError(CodeInternal, "the probe answer could not be applied"), "probe answer not applied", err)
	}
	if p.stopped() {
		// Replaced or closed while the answer was made: its PC is gone, and so is the gathering the wait was for.
		return "", errClosed("the probe")
	}
	if !complete {
		p.log.Warn("ICE gathering did not finish in time; the probe answer may lack candidates")
	}
	p.handedOut.Store(true)
	p.log.Debug("probe started")
	return local, nil
}

// stop ends the probe: a newer probe of the same user and transport replaces it, or the SFU closes. It doesn't wait.
func (p *probe) stop() { p.stopOnce.Do(func() { close(p.stopCh) }) }

// stopped reports whether stop was called.
func (p *probe) stopped() bool {
	select {
	case <-p.stopCh:
		return true
	default:
		return false
	}
}

// poke wakes the probe's goroutine to read the PC's connection state. It never blocks: one waiting wake is enough.
func (p *probe) poke() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// onDataChannel serves the probe's data channel: every message goes back unchanged, and the probe ends when the
// channel closes. A probe has one channel, as the browser opens one; any further channel is closed at once, since
// each one costs Pion a goroutine for the rest of the probe's life.
func (p *probe) onDataChannel(dc *webrtc.DataChannel) {
	if !p.channel.CompareAndSwap(false, true) {
		_ = dc.Close() // before it opens: Pion closes it when it would open it
		return
	}
	dc.OnMessage(func(m webrtc.DataChannelMessage) { echo(dc, m) })
	dc.OnClose(func() { p.chanOnce.Do(func() { close(p.chanClosed) }) })
}

// echo sends a data-channel message back as it came, text as text and binary as binary, unless maxProbeEchoBuffered
// bytes of echoes are still waiting to go out. Pion calls it on the channel's read loop, one message after the other;
// a send only queues the message. An error means the channel is closing, and the probe ends with it.
func echo(dc *webrtc.DataChannel, m webrtc.DataChannelMessage) {
	if dc.BufferedAmount() > maxProbeEchoBuffered {
		return
	}
	if m.IsString {
		_ = dc.SendText(string(m.Data))
	} else {
		_ = dc.Send(m.Data)
	}
}

// run is the probe's goroutine (02 §5.4: one per probe PC, for its life). It waits for the PC to connect, reports it
// probeRTTAfter later, and ends the probe at the end of its life, when the PC fails or closes, when the data channel
// closes, or when the probe is stopped. A probe that ends before it has reported reports then.
func (p *probe) run() {
	defer p.finish()
	life := time.NewTimer(time.Until(p.deadline))
	defer life.Stop()
	sample := time.NewTimer(probeRTTAfter) // armed when the PC connects
	sample.Stop()
	defer sample.Stop()
	var (
		connected, reported bool
		rtt                 <-chan time.Time // sample.C once the PC has connected, until the report
		why                 = ProbeErrClosed // why a probe that didn't connect ended
	)
loop:
	for {
		select {
		case <-p.stopCh:
			break loop
		case <-p.chanClosed:
			break loop
		case <-life.C:
			why = ProbeErrTimeout
			break loop
		case <-rtt:
			rtt = nil
			p.report(true, "")
			reported = true
		case <-p.wake:
			switch p.pc.ConnectionState() {
			case webrtc.PeerConnectionStateConnected:
				if !connected {
					connected = true
					sample.Reset(probeRTTAfter)
					rtt = sample.C
				}
			case webrtc.PeerConnectionStateFailed:
				why = ProbeErrTimeout // ICE gave up (8 s without a working pair) or the DTLS handshake did
				break loop
			case webrtc.PeerConnectionStateClosed:
				break loop // the client closed its side (DTLS close_notify)
			default: // new, connecting, disconnected: ICE may still connect, or come back
			}
		}
	}
	if !reported {
		p.report(connected, why)
	}
}

// report sends the probe's one result: for a probe that connected, the server side of the selected candidate pair
// and its round-trip time (02 §7.6), else why it ended. It logs the outcome without addresses (02 §13).
func (p *probe) report(connected bool, why string) {
	r := ProbeResult{Transport: p.key.t, Connected: connected}
	if connected {
		r.Selected, r.RTT = p.selectedPair()
	} else {
		r.Err = why
	}
	p.res <- r // capacity 1, and the only send
	if !p.handedOut.Load() {
		return
	}
	if connected {
		p.log.Info("connection test probe connected", "rtt", r.RTT)
	} else {
		p.log.Info("connection test probe did not connect", "result", r.Err)
	}
}

// selectedPair returns the server side of the PC's selected candidate pair, labeled with its transport ("udp
// 203.0.113.5:7882"), and the pair's current round-trip time; "" and 0 when there is none, or the PC has closed
// (Pion logs an error when a closed ICE agent is asked).
func (p *probe) selectedPair() (string, time.Duration) {
	if p.pc.ConnectionState() == webrtc.PeerConnectionStateClosed {
		return "", 0
	}
	sctp := p.pc.SCTP()
	if sctp == nil || sctp.Transport() == nil {
		return "", 0
	}
	ice := sctp.Transport().ICETransport()
	if ice == nil {
		return "", 0
	}
	var (
		selected string
		rtt      time.Duration
	)
	if pair, err := ice.GetSelectedCandidatePair(); err == nil && pair != nil && pair.Local != nil {
		selected = p.sfu.apis.pairLabel(*pair.Local)
	}
	if stats, ok := ice.GetSelectedCandidatePairStats(); ok && stats.CurrentRoundTripTime > 0 {
		rtt = time.Duration(stats.CurrentRoundTripTime * float64(time.Second))
	}
	return selected, rtt
}

// pairLabel formats the server's candidate of a selected pair as ProbeResult.Selected wants it: its transport label
// (via) and its address and port, which for a host candidate behind a 1:1 NAT are the advertised ones (04's rewrite
// rules). A TCP candidate that no advertised address matches is labeled "tcp".
func (a *apis) pairLabel(c webrtc.ICECandidate) string {
	label := a.via(c)
	if label == "" {
		label = strings.ToLower(c.Protocol.String())
	}
	addr := net.JoinHostPort(c.Address, strconv.Itoa(int(c.Port)))
	if ip, err := netip.ParseAddr(c.Address); err == nil {
		addr = netip.AddrPortFrom(ip.Unmap(), c.Port).String()
	}
	return label + " " + addr
}

// finish ends the probe after run, or after a start that couldn't create the PC: it closes the PC, frees the slot
// and closes done.
func (p *probe) finish() {
	if p.pc != nil {
		if err := p.pc.Close(); err != nil {
			p.log.Debug("probe PC closed with an error", "err", err)
		}
	}
	p.sfu.removeProbe(p)
	close(p.done)
}

// stopProbes stops every probe, replaced ones included, for Close, and returns them to wait for.
func (s *SFU) stopProbes() []*probe {
	s.mu.Lock()
	probes := make([]*probe, 0, len(s.probing))
	for p := range s.probing {
		probes = append(probes, p)
	}
	s.mu.Unlock()
	for _, p := range probes {
		p.stop()
	}
	return probes
}
