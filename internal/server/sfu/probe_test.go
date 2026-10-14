package sfu_test

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/server/netx"
	"github.com/MoonWX/isshoni/internal/server/sfu"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
)

// Integration 18 of 02 §17 (README S76), through sfutest.Harness: the connection-test probe of 02 §7.6 answers a
// browser's data-channel offer per transport with only that transport's candidates, connects, echoes what the
// client sends and reports the result; a user's second probe of a transport closes the first; the 21st probe on the
// server is sfu.probe_limit; a transport without its mux is sfu.transport_disabled.

// probeSettings are the client's Pion settings for a probe of transport t: UDP, or ICE-TCP to the passive candidates.
func probeSettings(t sfu.ProbeTransport) webrtc.SettingEngine {
	if t == sfu.ProbeUDP {
		return sfutest.LoopbackSettings()
	}
	return sfutest.LoopbackTCPSettings()
}

func newProbeClient(t *testing.T, pt sfu.ProbeTransport) *sfutest.ProbeClient {
	t.Helper()
	c, err := sfutest.NewProbeClient(probeSettings(pt))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("close the probe client: %v", err)
		}
	})
	return c
}

func probeOffer(t *testing.T, c *sfutest.ProbeClient) string {
	t.Helper()
	offer, err := c.Offer()
	if err != nil {
		t.Fatal(err)
	}
	return offer
}

// runProbe starts a probe for user on transport pt with a new client and has the client take the answer.
func runProbe(t *testing.T, h *sfutest.Harness, user sfu.UserID, pt sfu.ProbeTransport,
) (*sfutest.ProbeClient, string, <-chan sfu.ProbeResult) {
	t.Helper()
	c := newProbeClient(t, pt)
	answer, res, err := h.SFU.Probe(testCtx(t), user, pt, probeOffer(t, c))
	if err != nil {
		t.Fatalf("Probe(%s, %s): %v", user, pt, err)
	}
	if res == nil || cap(res) != 1 {
		t.Fatalf("Probe(%s, %s): result channel %v, want one of capacity 1", user, pt, res)
	}
	if err := c.Accept(answer); err != nil {
		t.Fatal(err)
	}
	return c, answer, res
}

func waitOpen(t *testing.T, c *sfutest.ProbeClient, what string) {
	t.Helper()
	if err := c.WaitOpen(testCtx(t)); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// probeResult waits for a probe's one result.
func probeResult(t *testing.T, res <-chan sfu.ProbeResult, what string) sfu.ProbeResult {
	t.Helper()
	select {
	case r := <-res:
		return r
	case <-time.After(25 * time.Second):
		t.Fatalf("%s: no probe result", what)
		return sfu.ProbeResult{}
	}
}

// checkEcho sends text and binary messages, the browser's RTT pings among them (05 §14.2), and wants each back
// unchanged, of the same kind.
func checkEcho(t *testing.T, c *sfutest.ProbeClient, what string) {
	t.Helper()
	ctx := testCtx(t)
	msgs := []webrtc.DataChannelMessage{
		{IsString: true, Data: []byte(`{"n":0,"t":1234.5}`)},
		{IsString: false, Data: []byte{0, 1, 2, 0xfe, 0xff}},
		{IsString: true, Data: []byte(`{"n":1,"t":1434.5}`)},
		{IsString: false, Data: bytes.Repeat([]byte{0xa5}, 8000)},
		{IsString: true, Data: []byte("ünïcødé ✓")},
	}
	for _, m := range msgs {
		if err := c.Send(m.IsString, m.Data); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	for i, want := range msgs {
		got, err := c.Receive(ctx)
		if err != nil {
			t.Fatalf("%s: echo %d: %v", what, i, err)
		}
		if got.IsString != want.IsString || !bytes.Equal(got.Data, want.Data) {
			t.Errorf("%s: echo %d is %v %q, want %v %q", what, i, got.IsString, truncate(got.Data), want.IsString,
				truncate(want.Data))
		}
	}
}

func truncate(b []byte) []byte {
	if len(b) > 32 {
		return b[:32]
	}
	return b
}

// advertisedVia returns the advertised addresses of a transport, as "via addr:port".
func advertisedVia(h *sfutest.Harness, pt sfu.ProbeTransport) []string {
	var out []string
	for _, a := range h.Transport.Advertised {
		if a.Via == string(pt) {
			out = append(out, a.Via+" "+a.Addr.String())
		}
	}
	return out
}

// TestProbe is integration 18.
func TestProbe(t *testing.T) {
	logs := &logBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h := newHarness(t, sfutest.HarnessOptions{TCP: true, TCP443: true, Logger: logger})
	all := []sfu.ProbeTransport{sfu.ProbeUDP, sfu.ProbeTCP443, sfu.ProbeTCP7882}

	t.Run("per transport", func(t *testing.T) {
		for _, pt := range all {
			t.Run(string(pt), func(t *testing.T) {
				c, answer, res := runProbe(t, h, "alice", pt)
				// The answer is complete and holds only this transport's candidates: every one of them, and no other.
				if vias := checkProbeCandidates(t, h, pt, answer); len(vias) == 0 {
					t.Fatalf("the %s answer has no candidate", pt)
				}
				waitOpen(t, c, string(pt))
				checkEcho(t, c, string(pt))

				r := probeResult(t, res, string(pt))
				t.Logf("result: %+v", r)
				if !r.Connected || r.Transport != pt || r.Err != "" {
					t.Fatalf("result %+v, want a connected %s probe", r, pt)
				}
				if want := advertisedVia(h, pt); !slices.Contains(want, r.Selected) {
					t.Errorf("Selected = %q, want one of %q (the server side of the pair, on %s)", r.Selected, want, pt)
				}
				if r.RTT <= 0 || r.RTT > time.Second {
					t.Errorf("RTT = %v, want the loopback pair's round-trip time", r.RTT)
				}
				// The probe keeps echoing after its result, for the rest of its life.
				checkEcho(t, c, string(pt)+" after the result")
				select {
				case extra := <-res:
					t.Errorf("a second result: %+v", extra)
				default:
				}
			})
		}
	})

	t.Run("one per user and transport", func(t *testing.T) {
		first, _, firstRes := runProbe(t, h, "bob", sfu.ProbeUDP)
		waitOpen(t, first, "bob's first udp probe")
		otherTransport, _, _ := runProbe(t, h, "bob", sfu.ProbeTCP7882)
		waitOpen(t, otherTransport, "bob's tcp7882 probe")
		otherUser, _, _ := runProbe(t, h, "carol", sfu.ProbeUDP)
		waitOpen(t, otherUser, "carol's udp probe")
		if r := probeResult(t, firstRes, "bob's first udp probe"); !r.Connected {
			t.Fatalf("bob's first probe: %+v", r)
		}

		second, _, secondRes := runProbe(t, h, "bob", sfu.ProbeUDP)
		select {
		case <-first.Done():
		case <-time.After(10 * time.Second):
			t.Fatalf("bob's first udp probe still runs after his second (client PC %s)", first.PC().ConnectionState())
		}
		waitOpen(t, second, "bob's second udp probe")
		checkEcho(t, second, "bob's second udp probe")
		if r := probeResult(t, secondRes, "bob's second udp probe"); !r.Connected {
			t.Errorf("bob's second probe: %+v", r)
		}
		// Bob's probe of another transport and Carol's of the same one are not his udp probe.
		for name, c := range map[string]*sfutest.ProbeClient{"bob's tcp7882 probe": otherTransport, "carol's udp probe": otherUser} {
			select {
			case <-c.Done():
				t.Errorf("%s ended with bob's second udp probe", name)
			default:
				checkEcho(t, c, name)
			}
		}
	})

	t.Run("ends with its channel", func(t *testing.T) {
		c, _, res := runProbe(t, h, "dave", sfu.ProbeTCP443)
		waitOpen(t, c, "dave's probe")
		if r := probeResult(t, res, "dave's probe"); !r.Connected {
			t.Fatalf("dave's probe: %+v", r)
		}
		if !h.SFU.HasProbe("dave", sfu.ProbeTCP443) {
			t.Fatal("dave's probe holds no slot")
		}
		if err := c.CloseChannel(); err != nil {
			t.Fatal(err)
		}
		// The probe PC closes with its channel, and the client's PC with it (DTLS close_notify).
		select {
		case <-c.Done():
		case <-time.After(10 * time.Second):
			t.Fatal("the probe did not end with its data channel")
		}
		eventually(t, func() bool { return !h.SFU.HasProbe("dave", sfu.ProbeTCP443) },
			func() string { return "dave's probe still holds its slot" })
	})

	// At info the probe's lines hold the user, the transport and the outcome, never an address or SDP (02 §13).
	var probeLines int
	for line := range strings.Lines(logs.String()) {
		if !strings.Contains(line, "component=sfu") || strings.Contains(line, " scope=") {
			continue
		}
		if strings.Contains(line, "probe") {
			probeLines++
		}
		for _, secret := range []string{"v=0", "candidate", "ice-ufrag", "ice-pwd", "fingerprint", "127.0.0.1", "a=", "m="} {
			if strings.Contains(line, secret) {
				t.Errorf("an SFU log line at info holds %q: %s", secret, line)
			}
		}
	}
	if !strings.Contains(logs.String(), `msg="connection test probe connected" component=sfu user_id=alice transport=udp`) {
		t.Errorf("no info line for alice's udp probe (%d probe lines)", probeLines)
	}
}

// checkProbeCandidates checks a probe answer: complete (no trickle), and exactly the host candidates the Transport
// advertises for transport pt, the TCP ones passive. It returns the candidates' transports.
func checkProbeCandidates(t *testing.T, h *sfutest.Harness, pt sfu.ProbeTransport, answer string) []string {
	t.Helper()
	if !strings.Contains(answer, "a=end-of-candidates") {
		t.Errorf("the %s answer has no a=end-of-candidates: the SFU never trickles", pt)
	}
	var vias []string
	got := candidates(t, answer)
	for _, c := range got {
		i := slices.IndexFunc(h.Transport.Advertised, func(a netx.AdvertisedAddr) bool {
			return a.Proto == c.proto && a.Addr == c.addr
		})
		switch {
		case i < 0:
			t.Errorf("%s answer: candidate %+v is not an advertised address", pt, c)
			continue
		case h.Transport.Advertised[i].Via != string(pt):
			t.Errorf("%s answer: candidate %+v is on %s", pt, c, h.Transport.Advertised[i].Via)
		case c.typ != "host":
			t.Errorf("%s answer: candidate %+v is not a host candidate", pt, c)
		case c.proto == "tcp" && !c.passive:
			t.Errorf("%s answer: TCP candidate %+v is not passive: the server never dials out", pt, c)
		}
		vias = append(vias, h.Transport.Advertised[i].Via)
	}
	if want := advertisedVia(h, pt); len(got) != len(want) {
		t.Errorf("the %s answer has %d candidates, the Transport advertises %d on it", pt, len(got), len(want))
	}
	return vias
}

// TestProbeOfferCandidates: a probe offer's candidates count toward the cap of 64 remote candidates (02 §12), so an
// offer with 100 public host candidates loses 36 of them, and the probe connects as one without any: the server
// learns the client's address from its checks (peer-reflexive).
func TestProbeOfferCandidates(t *testing.T) {
	logs := &logBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := newHarness(t, sfutest.HarnessOptions{Logger: logger})
	c := newProbeClient(t, sfu.ProbeUDP)
	offer := probeOffer(t, c)
	if strings.Contains(offer, "a=candidate:") {
		t.Fatal("the client's offer has candidates of its own")
	}
	// Documentation addresses (RFC 5737), which the filter keeps: the SFU's loopback socket can't reach them.
	var many strings.Builder
	for i := range 100 {
		fmt.Fprintf(&many, "a=candidate:%d 1 udp 2122260223 203.0.113.%d %d typ host\r\n", i, 1+i%50, 50000+i)
	}
	answer, res, err := h.SFU.Probe(testCtx(t), "alice", sfu.ProbeUDP, offer+many.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Accept(answer); err != nil {
		t.Fatal(err)
	}
	waitOpen(t, c, "the probe with 100 candidates")
	checkEcho(t, c, "the probe with 100 candidates")
	if r := probeResult(t, res, "the probe with 100 candidates"); !r.Connected {
		t.Errorf("result %+v, want a connected probe", r)
	}
	var found bool
	for line := range strings.Lines(logs.String()) {
		if strings.Contains(line, `msg="remote candidates dropped from a probe offer"`) {
			found = true
			if !strings.Contains(line, " count=36") {
				t.Errorf("want 36 of the 100 candidates dropped: %s", line)
			}
		}
	}
	if !found {
		t.Error("no debug line about the candidates dropped from the probe offer")
	}
}

// TestProbeLimit: 20 probes run on a server; the 21st is sfu.probe_limit, retryable, with RetryAfter until the
// oldest one's life ends, while a user's new probe of a transport takes the slot of the old one, which ends with
// "closed". A probe that nothing connects to ends with "timeout" at the end of its life and frees its slot.
func TestProbeLimit(t *testing.T) {
	h := newHarness(t, sfutest.HarnessOptions{TCP: true})
	const life = 10 * time.Second // room for creating the 21 probes on a loaded machine
	h.SFU.SetTimings(sfu.Timings{ProbeLife: life})
	ctx := testCtx(t)
	// One offer for all: the probes are separate PCs with credentials of their own, and nothing answers them.
	offer := probeOffer(t, newProbeClient(t, sfu.ProbeUDP))

	results := map[sfu.UserID]<-chan sfu.ProbeResult{}
	for i := range sfu.MaxProbes {
		user := sfu.UserID(fmt.Sprintf("u%02d", i))
		pt := sfu.ProbeUDP
		if i%2 == 1 {
			pt = sfu.ProbeTCP7882 // the limit is per server, whatever the transport
		}
		_, res, err := h.SFU.Probe(ctx, user, pt, offer)
		if err != nil {
			t.Fatalf("probe %d: %v", i+1, err)
		}
		results[user] = res
	}
	if slots, running := h.SFU.Probes(); slots != sfu.MaxProbes || running != sfu.MaxProbes {
		t.Fatalf("%d slots and %d probes running, want %d", slots, running, sfu.MaxProbes)
	}

	_, res, err := h.SFU.Probe(ctx, "u20", sfu.ProbeUDP, offer)
	wantCode(t, err, sfu.CodeProbeLimit)
	var e *sfu.Error
	if !errors.As(err, &e) || e.RetryAfter < time.Second || e.RetryAfter > life {
		t.Errorf("RetryAfter = %v, want until the oldest probe's %v life ends", e.RetryAfter, life)
	}
	if res != nil {
		t.Error("a refused probe has a result channel")
	}

	// u00's new udp probe takes the slot of its old one, which ends with "closed".
	_, replaced, err := h.SFU.Probe(ctx, "u00", sfu.ProbeUDP, offer)
	if err != nil {
		t.Fatalf("a user's new probe at the limit: %v", err)
	}
	if r := probeResult(t, results["u00"], "u00's first probe"); r.Connected || r.Err != sfu.ProbeErrClosed || r.Transport != sfu.ProbeUDP {
		t.Errorf("u00's replaced probe: %+v, want closed", r)
	}
	results["u00"] = replaced
	if slots, _ := h.SFU.Probes(); slots != sfu.MaxProbes {
		t.Errorf("%d slots after a replacement, want %d", slots, sfu.MaxProbes)
	}

	// The end of their life: each reports a timeout, and the slots are free again.
	for user, res := range results {
		if r := probeResult(t, res, string(user)); r.Connected || r.Err != sfu.ProbeErrTimeout {
			t.Errorf("%s's probe: %+v, want a timeout", user, r)
		}
	}
	eventually(t, func() bool {
		slots, running := h.SFU.Probes()
		return slots == 0 && running == 0
	}, func() string {
		slots, running := h.SFU.Probes()
		return fmt.Sprintf("%d slots and %d probes still running", slots, running)
	})
	c, _, res := runProbe(t, h, "u20", sfu.ProbeUDP)
	waitOpen(t, c, "the probe after the others ended")
	if r := probeResult(t, res, "u20"); !r.Connected {
		t.Errorf("u20's probe: %+v", r)
	}
}

// TestProbeErrors: a transport without its mux is sfu.transport_disabled, for each of the three; an offer that isn't
// one data channel without media is sfu.bad_sdp; a call that breaks the contract is sfu.internal, not retryable;
// after Close every probe is sfu.closed, and Close ends the running ones.
func TestProbeErrors(t *testing.T) {
	ctx := testCtx(t)
	offer := probeOffer(t, newProbeClient(t, sfu.ProbeUDP))

	// UDP only: no 443 multiplexer, no 7882/tcp listener.
	udpOnly := newHarness(t, sfutest.HarnessOptions{})
	// 443 only: listen.ice_udp = "" and listen.ice_tcp = "".
	tcp443Only := newHarness(t, sfutest.HarnessOptions{NoUDP: true, TCP443: true})
	for _, tc := range []struct {
		h  *sfutest.Harness
		pt sfu.ProbeTransport
	}{{udpOnly, sfu.ProbeTCP443}, {udpOnly, sfu.ProbeTCP7882}, {tcp443Only, sfu.ProbeUDP}, {tcp443Only, sfu.ProbeTCP7882}} {
		_, res, err := tc.h.SFU.Probe(ctx, "alice", tc.pt, offer)
		wantCode(t, err, sfu.CodeTransportDisabled)
		if res != nil {
			t.Errorf("%s: a refused probe has a result channel", tc.pt)
		}
	}
	// What each of them has works.
	for _, tc := range []struct {
		h  *sfutest.Harness
		pt sfu.ProbeTransport
	}{{udpOnly, sfu.ProbeUDP}, {tcp443Only, sfu.ProbeTCP443}} {
		c, _, res := runProbe(t, tc.h, "alice", tc.pt)
		waitOpen(t, c, string(tc.pt))
		if r := probeResult(t, res, string(tc.pt)); !r.Connected {
			t.Errorf("%s alone: %+v", tc.pt, r)
		}
	}

	audio := strings.Replace(offer, "m=application", "m=audio 9 UDP/TLS/RTP/SAVPF 111\r\na=rtpmap:111 opus/48000/2\r\nm=application", 1)
	twoChannels := offer + offer[strings.Index(offer, "m=application"):]
	for name, bad := range map[string]string{
		"empty":           "",
		"not SDP":         "hello",
		"no m-section":    offer[:strings.Index(offer, "m=application")],
		"media":           audio,
		"two data m-line": twoChannels,
		"too large":       offer + strings.Repeat("a=x-padding:"+strings.Repeat("p", 1000)+"\r\n", 70),
	} {
		_, _, err := udpOnly.SFU.Probe(ctx, "alice", sfu.ProbeUDP, bad)
		var e *sfu.Error
		if !errors.As(err, &e) || e.Code != sfu.CodeBadSDP || e.Retryable {
			t.Errorf("%s offer: %v, want sfu.bad_sdp", name, err)
		}
	}

	// An offer that passes the SDP guard but that Pion refuses (no ICE credentials) is sfu.bad_sdp too, and its probe
	// gives its slot back.
	var noUfrag []string
	for line := range strings.Lines(offer) {
		if !strings.HasPrefix(line, "a=ice-ufrag:") && !strings.HasPrefix(line, "a=ice-pwd:") {
			noUfrag = append(noUfrag, line)
		}
	}
	_, _, err := udpOnly.SFU.Probe(ctx, "frank", sfu.ProbeUDP, strings.Join(noUfrag, ""))
	wantCode(t, err, sfu.CodeBadSDP)
	eventually(t, func() bool { return !udpOnly.SFU.HasProbe("frank", sfu.ProbeUDP) },
		func() string { return "the refused probe still holds its slot" })

	for name, call := range map[string]func() error{
		"empty user": func() error {
			_, _, err := udpOnly.SFU.Probe(ctx, "", sfu.ProbeUDP, offer)
			return err
		},
		"unknown transport": func() error {
			_, _, err := udpOnly.SFU.Probe(ctx, "alice", "tcp9999", offer)
			return err
		},
	} {
		var e *sfu.Error
		if err := call(); !errors.As(err, &e) || e.Code != sfu.CodeInternal || e.Retryable {
			t.Errorf("%s: %v, want sfu.internal, not retryable", name, err)
		}
	}

	// Close ends the running probes and refuses new ones.
	c, _, res := runProbe(t, udpOnly, "erin", sfu.ProbeUDP)
	waitOpen(t, c, "erin's probe")
	start := time.Now()
	if err := udpOnly.SFU.Close(); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 1500*time.Millisecond {
		t.Errorf("SFU.Close took %v with a probe running", took)
	}
	if slots, running := udpOnly.SFU.Probes(); slots != 0 || running != 0 {
		t.Errorf("%d slots and %d probes running after Close", slots, running)
	}
	if r := probeResult(t, res, "erin's probe"); !r.Connected {
		t.Errorf("erin's probe: %+v (it had connected)", r)
	}
	select {
	case <-c.Done():
	case <-time.After(10 * time.Second):
		t.Error("the client's probe PC still runs after SFU.Close")
	}
	_, _, err = udpOnly.SFU.Probe(ctx, "alice", sfu.ProbeUDP, offer)
	wantCode(t, err, sfu.CodeClosed)
}

// TestProbeConcurrent: a user's probes of one transport that race each other replace one another. Each call returns
// an answer, or sfu.closed when the next one replaced it before it had answered; one probe keeps the slot, and every
// other one ends with "closed".
func TestProbeConcurrent(t *testing.T) {
	h := newHarness(t, sfutest.HarnessOptions{})
	ctx := testCtx(t)
	offer := probeOffer(t, newProbeClient(t, sfu.ProbeUDP))
	const n = 10
	var (
		wg      sync.WaitGroup
		errs    [n]error
		results [n]<-chan sfu.ProbeResult
	)
	for i := range n {
		wg.Go(func() { _, results[i], errs[i] = h.SFU.Probe(ctx, "alice", sfu.ProbeUDP, offer) })
	}
	wg.Wait()
	answered := 0
	for i, err := range errs {
		switch {
		case err == nil:
			answered++
		case !errors.Is(err, &sfu.Error{Code: sfu.CodeClosed}):
			t.Errorf("probe %d: %v, want an answer or sfu.closed", i, err)
		case results[i] != nil:
			t.Errorf("probe %d: a result channel with an error", i)
		}
	}
	if answered == 0 {
		t.Fatal("no probe answered: the last one is never replaced")
	}
	eventually(t, func() bool {
		slots, running := h.SFU.Probes()
		return slots == 1 && running == 1
	}, func() string {
		slots, running := h.SFU.Probes()
		return fmt.Sprintf("%d slots and %d probes running, want the last one alone", slots, running)
	})
	closed := 0
	for _, res := range results {
		select {
		case r := <-res: // a nil channel never has one
			if r.Connected || r.Err != sfu.ProbeErrClosed {
				t.Errorf("a replaced probe: %+v", r)
			}
			closed++
		default:
		}
	}
	if closed != answered-1 {
		t.Errorf("%d of the %d answered probes ended, want all but the last", closed, answered)
	}
	if !h.SFU.HasProbe("alice", sfu.ProbeUDP) {
		t.Error("alice's last probe holds no slot")
	}
}
