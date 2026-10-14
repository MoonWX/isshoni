package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/netx"
	"github.com/MoonWX/isshoni/internal/server/servertest"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
)

// The connection test in a running server (04 §7.7, §17; README S80): POST /api/v1/conntest on 02's probe PCs,
// answered for every signed-in user. The browser's side is sfutest.ProbeClient, a Pion PeerConnection with one data
// channel that offers as 05's runConnTest does (05 §14.2).

// newProbeClient returns the browser side of one probe: UDP on loopback, or ICE-TCP to the server's passive
// candidates. The test closes it.
func newProbeClient(t *testing.T, tcp bool) *sfutest.ProbeClient {
	t.Helper()
	se := sfutest.LoopbackSettings()
	if tcp {
		se = sfutest.LoopbackTCPSettings()
	}
	c, err := sfutest.NewProbeClient(se)
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

// postConnTest sends POST /api/v1/conntest in browser c.
func postConnTest(t *testing.T, srv *servertest.Server, c *http.Client, transport api.Transport, offer string) response {
	t.Helper()
	return callAs(t, c, srv, http.MethodPost, "/api/v1/conntest", api.ConnTestRequest{Transport: transport, Offer: offer})
}

// startProbe runs one probe as the browser c does it: a new client offers, the server answers with 200, and the
// client applies the answer. It returns the client and the answer.
func startProbe(t *testing.T, srv *servertest.Server, c *http.Client, transport api.Transport) (*sfutest.ProbeClient, api.ConnTestResponse) {
	t.Helper()
	pc := newProbeClient(t, transport != api.TransportUDP)
	offer, err := pc.Offer()
	if err != nil {
		t.Fatal(err)
	}
	res := postConnTest(t, srv, c, transport, offer)
	if res.status != http.StatusOK {
		t.Fatalf("POST /conntest %s = %d %s, want 200", transport, res.status, res.body)
	}
	var out api.ConnTestResponse
	if err := json.Unmarshal([]byte(res.body), &out); err != nil {
		t.Fatalf("POST /conntest %s: %v: %s", transport, err, res.body)
	}
	if out.ExpiresInS != 20 {
		t.Errorf("POST /conntest %s: expiresInS = %d, want 20", transport, out.ExpiresInS)
	}
	if err := pc.Accept(out.Answer); err != nil {
		t.Fatalf("%s: %v", transport, err)
	}
	return pc, out
}

// answerCandidates returns the candidates of an SDP as "protocol port".
func answerCandidates(sdp string) []string {
	var out []string
	for line := range strings.SplitSeq(sdp, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "a=candidate:")
		if !ok {
			continue
		}
		f := strings.Fields(rest) // foundation component protocol priority address port typ type …
		if len(f) < 6 {
			out = append(out, "malformed: "+rest)
			continue
		}
		out = append(out, strings.ToLower(f[2])+" "+f[5])
	}
	return out
}

// wantOnlyCandidates fails unless the answer has candidates, and every one of them is "protocol port".
func wantOnlyCandidates(t *testing.T, what, answer, protocol string, port int) {
	t.Helper()
	cands := answerCandidates(answer)
	want := protocol + " " + strconv.Itoa(port)
	if len(cands) == 0 || slices.ContainsFunc(cands, func(c string) bool { return c != want }) {
		t.Errorf("%s: the answer's candidates are %q, want %q alone", what, cands, want)
	}
}

// wantConnected waits until the probe's PeerConnection is connected and its channel open, checks that the server
// side of the selected pair is protocol on port, and that the probe PC echoes what the browser sends.
func wantConnected(t *testing.T, what string, pc *sfutest.ProbeClient, protocol webrtc.ICEProtocol, port int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := pc.WaitOpen(ctx); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	pair, err := pc.PC().SCTP().Transport().ICETransport().GetSelectedCandidatePair()
	if err != nil || pair == nil {
		t.Fatalf("%s: selected pair %v, %v", what, pair, err)
	}
	if pair.Remote.Protocol != protocol || int(pair.Remote.Port) != port {
		t.Errorf("%s: the server side of the selected pair is %s %s:%d, want %s on port %d", what,
			pair.Remote.Protocol, pair.Remote.Address, pair.Remote.Port, protocol, port)
	}
	wantEcho(t, what, pc)
}

// wantEcho sends the browser's RTT ping (05 §14.2) and wants it back unchanged.
func wantEcho(t *testing.T, what string, pc *sfutest.ProbeClient) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ping := []byte(`{"n":0,"t":1234.5}`)
	if err := pc.Send(true, ping); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	got, err := pc.Receive(ctx)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if !got.IsString || !bytes.Equal(got.Data, ping) {
		t.Errorf("%s: echo %v %q, want the text %q", what, got.IsString, got.Data, ping)
	}
}

// wantServerInfo checks the server object of an answer against what the server knows about itself: its ports, the
// public IPv4 address and NAT kind that GET /v1/status reports, and this machine's provider and container.
func wantServerInfo(t *testing.T, srv *servertest.Server, got api.ConnTestServerInfo, tcpPorts []int) {
	t.Helper()
	admin, ctx := adminSocket(t, srv)
	status, err := admin.Status(ctx)
	if err != nil {
		t.Fatalf("admin status: %v", err)
	}
	want := api.ConnTestServerInfo{
		PublicIP:  status.PublicIPv4, // "" here: no STUN, and loopback is no public address
		Provider:  netx.DetectCloudProvider(os.DirFS(netx.DMIDir)),
		Container: api.ContainerKind(config.Host{}.Container()),
		UDPPort:   srv.UDPPort,
		TCPPorts:  tcpPorts,
		NAT:       status.NAT,
	}
	if got.PublicIP != want.PublicIP || got.Provider != want.Provider || got.Container != want.Container ||
		got.UDPPort != want.UDPPort || !slices.Equal(got.TCPPorts, want.TCPPorts) || got.NAT != want.NAT {
		t.Errorf("server = %+v, want %+v", got, want)
	}
}

// TestConnTest is the acceptance of README S80 in off mode: the probes of udp and tcp7882 get answers with only
// their own candidates that connect, tcp443 is transport_disabled (no 443 multiplexer in off mode), and a second
// probe of the same user and transport replaces the first, while another user's probe and the same user's probe of
// another transport go on. A member may run the test like an admin; a visitor without a session may not.
func TestConnTest(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{Deps: wiredDeps()})
	setUp(t, srv) // srv.Client is the admin's browser
	member := register(t, srv, "Sam")

	// Without a session there is no test, and no probe.
	anon := &http.Client{Transport: srv.Client.Transport}
	wantStatus(t, "conntest without a session", postConnTest(t, srv, anon, api.TransportUDP, "v=0"),
		http.StatusUnauthorized, api.CodeUnauthenticated)

	// tcp443: the operator's proxy owns port 443 in off mode.
	res := postConnTest(t, srv, srv.Client, api.TransportTCP443, "v=0")
	wantStatus(t, "conntest tcp443", res, http.StatusConflict, api.CodeTransportDisabled)
	var env api.ErrorResponse
	if err := json.Unmarshal([]byte(res.body), &env); err != nil || env.Error.Params[api.ParamTransport] != "tcp443" {
		t.Errorf("transport_disabled = %s, want params.transport tcp443", res.body)
	}
	// An unknown transport never reaches the SFU.
	wantStatus(t, "conntest tcp80", postConnTest(t, srv, srv.Client, "tcp80", "v=0"),
		http.StatusUnprocessableEntity, api.CodeValidationFailed)

	// udp and tcp7882 connect, each on its own port alone.
	adminUDP, answer := startProbe(t, srv, srv.Client, api.TransportUDP)
	wantOnlyCandidates(t, "udp", answer.Answer, "udp", srv.UDPPort)
	wantServerInfo(t, srv, answer.Server, []int{srv.TCPPort})
	wantConnected(t, "the admin's udp probe", adminUDP, webrtc.ICEProtocolUDP, srv.UDPPort)

	adminTCP, answer := startProbe(t, srv, srv.Client, api.TransportTCP7882)
	wantOnlyCandidates(t, "tcp7882", answer.Answer, "tcp", srv.TCPPort)
	wantConnected(t, "the admin's tcp7882 probe", adminTCP, webrtc.ICEProtocolTCP, srv.TCPPort)

	memberUDP, _ := startProbe(t, srv, member, api.TransportUDP)
	wantConnected(t, "the member's udp probe", memberUDP, webrtc.ICEProtocolUDP, srv.UDPPort)

	// The admin's second udp probe replaces the first: the server closes that probe PC.
	second, _ := startProbe(t, srv, srv.Client, api.TransportUDP)
	wantConnected(t, "the admin's second udp probe", second, webrtc.ICEProtocolUDP, srv.UDPPort)
	select {
	case <-adminUDP.Done():
	case <-time.After(10 * time.Second):
		t.Fatalf("the admin's first udp probe still runs after the second (client PC %s)",
			adminUDP.PC().ConnectionState())
	}
	// The member's probe of the same transport and the admin's probe of another one go on.
	for _, p := range []struct {
		what string
		pc   *sfutest.ProbeClient
	}{{"the member's udp probe", memberUDP}, {"the admin's tcp7882 probe", adminTCP}} {
		select {
		case <-p.pc.Done():
			t.Errorf("%s ended with the admin's second udp probe", p.what)
		default:
			wantEcho(t, p.what+", after the admin's second udp probe", p.pc)
		}
	}

	// The member's limit: 12 requests a minute (the probe above was the first), then 429 with Retry-After. An offer
	// the SFU refuses counts, and is 400 bad_sdp; it doesn't touch the member's running probe.
	for i := 2; i <= 12; i++ {
		wantStatus(t, "a bad offer", postConnTest(t, srv, member, api.TransportUDP, "v=0"),
			http.StatusBadRequest, api.CodeBadSDP)
	}
	res = postConnTest(t, srv, member, api.TransportUDP, "v=0")
	wantStatus(t, "the 13th request in a minute", res, http.StatusTooManyRequests, api.CodeRateLimited)
	if s, err := strconv.Atoi(res.header.Get("Retry-After")); err != nil || s < 1 || s > 60 {
		t.Errorf("Retry-After = %q, want 1 to 60 seconds", res.header.Get("Retry-After"))
	}
	wantEcho(t, "the member's udp probe, after the bad offers", memberUDP)
}

// TestConnTestOverTLS is the connection test over TLS of 04 §17: with the 443 multiplexer, the tcp443 probe gets
// an answer whose candidates are the HTTPS port's, and it connects through the multiplexer. It is the only tcp443
// probe check: the browser e2e harness runs in off mode (05 §19.3).
func TestConnTestOverTLS(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{TLS: true, Deps: wiredDeps()})
	setUp(t, srv)
	https, ok := srv.Srv.Addrs().HTTPS.(*net.TCPAddr)
	if !ok {
		t.Fatalf("Addrs().HTTPS = %v, want the 443 multiplexer", srv.Srv.Addrs().HTTPS)
	}

	pc, answer := startProbe(t, srv, srv.Client, api.TransportTCP443)
	wantOnlyCandidates(t, "tcp443", answer.Answer, "tcp", https.Port)
	wantServerInfo(t, srv, answer.Server, []int{https.Port, srv.TCPPort})
	wantConnected(t, "the tcp443 probe", pc, webrtc.ICEProtocolTCP, https.Port)

	// The other two transports work next to it.
	udp, _ := startProbe(t, srv, srv.Client, api.TransportUDP)
	wantConnected(t, "the udp probe", udp, webrtc.ICEProtocolUDP, srv.UDPPort)
	tcp, _ := startProbe(t, srv, srv.Client, api.TransportTCP7882)
	wantConnected(t, "the tcp7882 probe", tcp, webrtc.ICEProtocolTCP, srv.TCPPort)
}

// TestConnTestWithoutUDP: with listen.ice_udp = "" the udp probe is transport_disabled, the answer's udpPort is 0,
// and TCP still connects (05 shows the UDP row as ✗ "UDP turned off", 05 §14.2).
func TestConnTestWithoutUDP(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{Deps: wiredDeps(), Config: func(c *config.Config) {
		c.Listen.ICEUDP = ""
	}})
	setUp(t, srv)
	res := postConnTest(t, srv, srv.Client, api.TransportUDP, "v=0")
	wantStatus(t, "conntest udp", res, http.StatusConflict, api.CodeTransportDisabled)
	if !strings.Contains(res.body, `"params":{"transport":"udp"}`) {
		t.Errorf("transport_disabled = %s, want params.transport udp", res.body)
	}
	pc, answer := startProbe(t, srv, srv.Client, api.TransportTCP7882)
	if answer.Server.UDPPort != 0 || !slices.Equal(answer.Server.TCPPorts, []int{srv.TCPPort}) {
		t.Errorf("server = %+v, want udpPort 0 and tcpPorts [%d]", answer.Server, srv.TCPPort)
	}
	wantConnected(t, "the tcp7882 probe", pc, webrtc.ICEProtocolTCP, srv.TCPPort)
}
