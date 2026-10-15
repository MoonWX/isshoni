package ops

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// The connection test's typed function (04 §7.7, §17 "ops"): transport validation, transport_disabled for each
// transport without a listener, the per-user limit, the Prober's errors (sfu.probe_limit arrives as 429), and the
// server object: publicIp "" without a public IPv4 address, the container, the ports.

// fakeProber records what it was asked and answers with answer, or with err.
type fakeProber struct {
	mu     sync.Mutex
	calls  []string // "user transport offer"
	answer string
	err    error
}

func (f *fakeProber) Probe(_ context.Context, userID, transport, offerSDP string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID+" "+transport+" "+offerSDP)
	if f.err != nil {
		return "", f.err
	}
	return f.answer, nil
}

func (f *fakeProber) asked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// fakeClock is a settable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

var (
	connTestStart = time.Date(2026, 10, 14, 20, 0, 0, 0, time.UTC)
	allPorts      = ConnTestPorts{UDP: 7882, TCP443: 443, TCP7882: 7882}
)

// newTestConnTest returns a ConnTest on a fake Prober and a fake clock, with every transport on its usual port, at
// Hetzner without a container or NAT, unless o says otherwise.
func newTestConnTest(o ConnTestOptions) (*ConnTest, *fakeProber, *fakeClock) {
	p := &fakeProber{answer: "v=0 answer"}
	clock := &fakeClock{t: connTestStart}
	if o.Prober == nil {
		o.Prober = p
	}
	if o.Ports == (ConnTestPorts{}) {
		o.Ports = allPorts
	}
	o.Now = clock.now
	return NewConnTest(o), p, clock
}

func probeReq(t api.Transport) api.ConnTestRequest {
	return api.ConnTestRequest{Transport: t, Offer: "v=0 offer"}
}

// wantAPIError fails unless err is an *api.Error equal to want.
func wantAPIError(t *testing.T, what string, err error, want *api.Error) {
	t.Helper()
	var ae *api.Error
	if !errors.As(err, &ae) {
		t.Fatalf("%s: err = %v, want %+v", what, err, want)
	}
	if !reflect.DeepEqual(ae, want) {
		t.Errorf("%s: err = %+v, want %+v", what, ae, want)
	}
}

func TestConnTestAnswers(t *testing.T) {
	ct, p, _ := newTestConnTest(ConnTestOptions{
		PublicIPv4: netip.MustParseAddr("203.0.113.7"),
		Provider:   api.CloudProviderHetzner,
		Container:  api.ContainerKindNone,
		NAT:        api.NATKindNone,
	})
	ctx := context.Background()
	for _, tr := range []api.Transport{api.TransportUDP, api.TransportTCP443, api.TransportTCP7882} {
		res, err := ct.Probe(ctx, "u1", probeReq(tr))
		if err != nil {
			t.Fatalf("Probe(%s): %v", tr, err)
		}
		if res.Answer != "v=0 answer" || res.ExpiresInS != 20 {
			t.Errorf("Probe(%s) = %+v, want the Prober's answer, expiring in 20 s", tr, res)
		}
	}
	want := []string{"u1 udp v=0 offer", "u1 tcp443 v=0 offer", "u1 tcp7882 v=0 offer"}
	if got := p.asked(); !reflect.DeepEqual(got, want) {
		t.Errorf("the Prober was asked %q, want %q", got, want)
	}

	// The wire form is the example of 04 §7.7.
	res, err := ct.Probe(ctx, "u2", probeReq(api.TransportUDP))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	const wire = `{"answer":"v=0 answer","expiresInS":20,"server":{"publicIp":"203.0.113.7","provider":"hetzner",` +
		`"container":"none","udpPort":7882,"tcpPorts":[443,7882],"nat":"none"}}`
	if string(b) != wire {
		t.Errorf("answer JSON\n got %s\nwant %s", b, wire)
	}
}

// TestConnTestServerInfo is the server object for the servers 05's fix text tells apart.
func TestConnTestServerInfo(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    ConnTestOptions
		want api.ConnTestServerInfo
	}{
		{
			name: "no public IPv4 address, nothing detected",
			o:    ConnTestOptions{Ports: allPorts},
			want: api.ConnTestServerInfo{
				PublicIP: "", Provider: api.CloudProviderUnknown, Container: api.ContainerKindNone, UDPPort: 7882,
				TCPPorts: []int{443, 7882}, NAT: api.NATKindUnknown,
			},
		},
		{
			name: "only an IPv6 address",
			o:    ConnTestOptions{Ports: allPorts, PublicIPv4: netip.MustParseAddr("2001:db8::7")},
			want: api.ConnTestServerInfo{
				Provider: api.CloudProviderUnknown, Container: api.ContainerKindNone, UDPPort: 7882,
				TCPPorts: []int{443, 7882}, NAT: api.NATKindUnknown,
			},
		},
		{
			name: "in Docker behind a 1:1 NAT, off mode",
			o: ConnTestOptions{
				Ports:      ConnTestPorts{UDP: 7882, TCP7882: 7882},
				PublicIPv4: netip.MustParseAddr("::ffff:198.51.100.4"), // IPv4-mapped: still the IPv4 address
				Provider:   api.CloudProviderDigitalOcean,
				Container:  api.ContainerKindDocker,
				NAT:        api.NATKindOneToOne,
			},
			want: api.ConnTestServerInfo{
				PublicIP: "198.51.100.4", Provider: api.CloudProviderDigitalOcean, Container: api.ContainerKindDocker,
				UDPPort: 7882, TCPPorts: []int{7882}, NAT: api.NATKindOneToOne,
			},
		},
		{
			name: "UDP off, ports of the operator's choice",
			o: ConnTestOptions{
				Ports:      ConnTestPorts{TCP443: 8443, TCP7882: 50000},
				PublicIPv4: netip.MustParseAddr("192.0.2.10"),
				Container:  api.ContainerKindPodman,
				NAT:        api.NATKindPortForward,
			},
			want: api.ConnTestServerInfo{
				PublicIP: "192.0.2.10", Provider: api.CloudProviderUnknown, Container: api.ContainerKindPodman,
				UDPPort: 0, TCPPorts: []int{8443, 50000}, NAT: api.NATKindPortForward,
			},
		},
		{
			name: "one port for both TCP transports",
			o:    ConnTestOptions{Ports: ConnTestPorts{UDP: 7882, TCP443: 443, TCP7882: 443}},
			want: api.ConnTestServerInfo{
				Provider: api.CloudProviderUnknown, Container: api.ContainerKindNone, UDPPort: 7882,
				TCPPorts: []int{443}, NAT: api.NATKindUnknown,
			},
		},
		// README F22: where doctor's public_ip lets a container bridge's symmetric NAT pass, the server object says
		// so next to the NAT kind.
		{
			name: "in Docker on a bridge network, symmetric NAT that doctor lets pass",
			o: ConnTestOptions{
				Ports:        allPorts,
				PublicIPv4:   netip.MustParseAddr("203.0.113.7"),
				Provider:     api.CloudProviderHetzner,
				Container:    api.ContainerKindDocker,
				NAT:          api.NATKindSymmetric,
				DockerBridge: true,
			},
			want: api.ConnTestServerInfo{
				PublicIP: "203.0.113.7", Provider: api.CloudProviderHetzner, Container: api.ContainerKindDocker,
				UDPPort: 7882, TCPPorts: []int{443, 7882}, NAT: api.NATKindSymmetric, DockerBridge: true,
			},
		},
		{
			name: "symmetric NAT that doctor doesn't let pass (the host's network, no container)",
			o: ConnTestOptions{
				Ports: allPorts, PublicIPv4: netip.MustParseAddr("203.0.113.7"), NAT: api.NATKindSymmetric,
			},
			want: api.ConnTestServerInfo{
				PublicIP: "203.0.113.7", Provider: api.CloudProviderUnknown, Container: api.ContainerKindNone,
				UDPPort: 7882, TCPPorts: []int{443, 7882}, NAT: api.NATKindSymmetric,
			},
		},
		{
			// The rule lets only symmetric NAT pass (cgnat_likely stays a failure in a bridge), so the flag never
			// stands next to another kind, whatever the wiring says.
			name: "a bridge flag next to another NAT kind is dropped",
			o: ConnTestOptions{
				Ports: allPorts, Container: api.ContainerKindDocker, NAT: api.NATKindCGNATLikely, DockerBridge: true,
			},
			want: api.ConnTestServerInfo{
				Provider: api.CloudProviderUnknown, Container: api.ContainerKindDocker, UDPPort: 7882,
				TCPPorts: []int{443, 7882}, NAT: api.NATKindCGNATLikely,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ct, _, _ := newTestConnTest(tc.o)
			tr := api.TransportTCP7882
			if tc.o.Ports.UDP > 0 {
				tr = api.TransportUDP
			}
			res, err := ct.Probe(context.Background(), "u1", probeReq(tr))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(res.Server, tc.want) {
				t.Errorf("server = %+v, want %+v", res.Server, tc.want)
			}
		})
	}

	// Without a TCP listener tcpPorts is [] on the wire, not null.
	ct, _, _ := newTestConnTest(ConnTestOptions{Ports: ConnTestPorts{UDP: 7882}})
	res, err := ct.Probe(context.Background(), "u1", probeReq(api.TransportUDP))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(res.Server)
	if err != nil {
		t.Fatal(err)
	}
	const wire = `{"publicIp":"","provider":"unknown","container":"none","udpPort":7882,"tcpPorts":[],"nat":"unknown"}`
	if string(b) != wire {
		t.Errorf("server JSON = %s, want %s", b, wire)
	}

	// The bridge's flag is an additive field (README F22): it is on the wire next to nat where it is true, and an
	// older SPA ignores it; everywhere else the server object is what it was, without the key.
	ct, _, _ = newTestConnTest(ConnTestOptions{
		Ports: ConnTestPorts{UDP: 7882}, Container: api.ContainerKindDocker, NAT: api.NATKindSymmetric, DockerBridge: true,
	})
	res, err = ct.Probe(context.Background(), "u1", probeReq(api.TransportUDP))
	if err != nil {
		t.Fatal(err)
	}
	if b, err = json.Marshal(res.Server); err != nil {
		t.Fatal(err)
	}
	const bridgeWire = `{"publicIp":"","provider":"unknown","container":"docker","udpPort":7882,"tcpPorts":[],` +
		`"nat":"symmetric","dockerBridge":true}`
	if string(b) != bridgeWire {
		t.Errorf("server JSON = %s, want %s", b, bridgeWire)
	}
}

// TestConnTestValidation: a transport that the SFU doesn't know never reaches it, and costs the user nothing.
func TestConnTestValidation(t *testing.T) {
	ct, p, _ := newTestConnTest(ConnTestOptions{})
	ctx := context.Background()
	for _, tc := range []struct {
		transport api.Transport
		field     string
	}{
		{"", api.FieldRequired},
		{"tcp", api.FieldInvalid},
		{"UDP", api.FieldInvalid},
		{"tcp443 ", api.FieldInvalid},
		{"tcp80", api.FieldInvalid},
	} {
		_, err := ct.Probe(ctx, "u1", probeReq(tc.transport))
		wantAPIError(t, "transport "+string(tc.transport), err, &api.Error{
			Code: api.CodeValidationFailed, Fields: map[string]string{"transport": tc.field},
		})
	}
	if got := p.asked(); len(got) != 0 {
		t.Errorf("the Prober was asked %q", got)
	}
	// None of them counted: the user still has all 12 requests of the minute.
	for i := range connTestPerWindow {
		if _, err := ct.Probe(ctx, "u1", probeReq(api.TransportUDP)); err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
	}

	var ae *api.Error
	if _, err := ct.Probe(ctx, "", probeReq(api.TransportUDP)); err == nil || errors.As(err, &ae) {
		t.Errorf("Probe without a user = %v, want a failure of the wiring's (500 internal)", err)
	}
}

// TestConnTestTransportDisabled: each transport without a listener is 409 transport_disabled with params.transport,
// before the SFU is asked and before the limit counts.
func TestConnTestTransportDisabled(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ports ConnTestPorts
		off   api.Transport
	}{
		{"off mode", ConnTestPorts{UDP: 7882, TCP7882: 7882}, api.TransportTCP443},
		{"listen.ice_udp empty", ConnTestPorts{TCP443: 443, TCP7882: 7882}, api.TransportUDP},
		{"listen.ice_tcp empty", ConnTestPorts{UDP: 7882, TCP443: 443}, api.TransportTCP7882},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ct, p, _ := newTestConnTest(ConnTestOptions{Ports: tc.ports})
			ctx := context.Background()
			for range connTestPerWindow + 3 {
				_, err := ct.Probe(ctx, "u1", probeReq(tc.off))
				wantAPIError(t, string(tc.off), err, &api.Error{
					Code: api.CodeTransportDisabled, Params: map[string]any{api.ParamTransport: tc.off},
				})
			}
			if got := p.asked(); len(got) != 0 {
				t.Errorf("the Prober was asked %q", got)
			}
			// The other two transports work, and the user's minute is untouched.
			for _, tr := range []api.Transport{api.TransportUDP, api.TransportTCP443, api.TransportTCP7882} {
				if tr == tc.off {
					continue
				}
				if _, err := ct.Probe(ctx, "u1", probeReq(tr)); err != nil {
					t.Errorf("Probe(%s): %v", tr, err)
				}
			}
			if n := len(ct.limit.users["u1"]); n != 2 {
				t.Errorf("the limit counted %d requests, want 2", n)
			}
		})
	}

	// On the wire: 409 with params.transport.
	b, err := json.Marshal(api.ErrorResponse{Error: *TransportDisabled(api.TransportTCP443)})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"error":{"code":"transport_disabled","params":{"transport":"tcp443"}}}`; string(b) != want {
		t.Errorf("transport_disabled = %s, want %s", b, want)
	}
	if s := api.StatusOf(api.CodeTransportDisabled); s != 409 {
		t.Errorf("transport_disabled is %d, want 409", s)
	}
}

// TestConnTestRateLimit is the limit of 04 §7.7: 12 requests per user and minute, then 429 rate_limited with the
// wait until the oldest one is a minute old.
func TestConnTestRateLimit(t *testing.T) {
	ct, p, clock := newTestConnTest(ConnTestOptions{})
	ctx := context.Background()
	transports := []api.Transport{api.TransportUDP, api.TransportTCP443, api.TransportTCP7882}

	// Twelve requests, one every 2 s: 0 s … 22 s.
	for i := range connTestPerWindow {
		if _, err := ct.Probe(ctx, "alice", probeReq(transports[i%3])); err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
		clock.advance(2 * time.Second)
	}
	// At 24 s the oldest leaves the minute at 60 s: retryAfter 36, whatever the transport.
	for _, tr := range transports {
		_, err := ct.Probe(ctx, "alice", probeReq(tr))
		wantAPIError(t, "13th request", err, &api.Error{Code: api.CodeRateLimited, RetryAfter: 36})
	}
	if got := len(p.asked()); got != connTestPerWindow {
		t.Errorf("the Prober was asked %d times, want %d", got, connTestPerWindow)
	}
	// Another user is not limited by Alice's tests.
	if _, err := ct.Probe(ctx, "bob", probeReq(api.TransportUDP)); err != nil {
		t.Errorf("Bob's first request: %v", err)
	}
	// A wait of less than a second is still a second.
	clock.advance(35*time.Second + 500*time.Millisecond) // 59.5 s
	_, err := ct.Probe(ctx, "alice", probeReq(api.TransportUDP))
	wantAPIError(t, "at 59.5 s", err, &api.Error{Code: api.CodeRateLimited, RetryAfter: 1})
	// At 60 s the first request has left the minute, and there is room for exactly one more.
	clock.advance(500 * time.Millisecond)
	if _, err := ct.Probe(ctx, "alice", probeReq(api.TransportUDP)); err != nil {
		t.Fatalf("at 60 s: %v", err)
	}
	_, err = ct.Probe(ctx, "alice", probeReq(api.TransportUDP))
	wantAPIError(t, "at 60 s, again", err, &api.Error{Code: api.CodeRateLimited, RetryAfter: 2})

	// A clock set back by an hour locks nobody out.
	clock.advance(-time.Hour)
	if _, err := ct.Probe(ctx, "alice", probeReq(api.TransportUDP)); err != nil {
		t.Errorf("after the clock went back: %v", err)
	}
}

// TestConnTestLimiterForgets: a user who stopped testing leaves the limiter's map within two minutes.
func TestConnTestLimiterForgets(t *testing.T) {
	ct, _, clock := newTestConnTest(ConnTestOptions{})
	ctx := context.Background()
	for _, u := range []string{"u1", "u2", "u3"} {
		if _, err := ct.Probe(ctx, u, probeReq(api.TransportUDP)); err != nil {
			t.Fatal(err)
		}
	}
	clock.advance(90 * time.Second)
	if _, err := ct.Probe(ctx, "u4", probeReq(api.TransportUDP)); err != nil {
		t.Fatal(err)
	}
	clock.advance(31 * time.Second)
	if _, err := ct.Probe(ctx, "u4", probeReq(api.TransportUDP)); err != nil {
		t.Fatal(err)
	}
	ct.limit.mu.Lock()
	defer ct.limit.mu.Unlock()
	if len(ct.limit.users) != 1 || len(ct.limit.users["u4"]) != 2 {
		t.Errorf("the limiter holds %v, want u4's two requests alone", ct.limit.users)
	}
}

// TestConnTestProberErrors: the Prober's answers, which the wiring mapped from 02's errors, go out as they are; any
// other error is a failure of the server's.
func TestConnTestProberErrors(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		err  *api.Error
	}{
		// sfu.probe_limit: 20 probes run on the server.
		{"probe_limit", &api.Error{Code: api.CodeRateLimited, RetryAfter: 7}},
		{"bad_sdp", api.NewError(api.CodeBadSDP)},
		{"transport_disabled", TransportDisabled(api.TransportUDP)},
		{"closed", api.NewError(api.CodeNotReady)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &fakeProber{err: tc.err}
			ct, _, _ := newTestConnTest(ConnTestOptions{Prober: p})
			_, err := ct.Probe(ctx, "u1", probeReq(api.TransportUDP))
			wantAPIError(t, tc.name, err, tc.err)
		})
	}

	cause := errors.New("pion: setting the remote description failed")
	ct, _, _ := newTestConnTest(ConnTestOptions{Prober: &fakeProber{err: cause}})
	_, err := ct.Probe(ctx, "u1", probeReq(api.TransportTCP7882))
	var ae *api.Error
	if !errors.Is(err, cause) || errors.As(err, &ae) {
		t.Errorf("a failure of the SFU's = %v, want it wrapped, without an *api.Error (500 internal)", err)
	}

	ct, _, _ = newTestConnTest(ConnTestOptions{Prober: &fakeProber{answer: ""}})
	if _, err := ct.Probe(ctx, "u1", probeReq(api.TransportUDP)); err == nil || errors.As(err, &ae) {
		t.Errorf("an empty answer = %v, want a failure of the server's", err)
	}

	// A server without an SFU has no API either; should one be wired all the same, it is not ready.
	noSFU := NewConnTest(ConnTestOptions{Ports: allPorts})
	_, err = noSFU.Probe(ctx, "u1", probeReq(api.TransportUDP))
	wantAPIError(t, "no Prober", err, api.NewError(api.CodeNotReady))
}

func TestRetryAfterSeconds(t *testing.T) {
	for wait, want := range map[time.Duration]int{
		0:                       1,
		time.Millisecond:        1,
		time.Second:             1,
		time.Second + 1:         2,
		36 * time.Second:        36,
		59*time.Second + 999999: 60,
	} {
		if got := retryAfterSeconds(wait); got != want {
			t.Errorf("retryAfterSeconds(%v) = %d, want %d", wait, got, want)
		}
	}
}
