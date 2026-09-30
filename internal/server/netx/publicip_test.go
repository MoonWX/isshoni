package netx

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// fakeSTUN answers Mapped from a table: an address, an error, or (block) nothing until the context ends. fails
// makes the first n calls to a server fail.
type fakeSTUN struct {
	mu      sync.Mutex
	answers map[string]netip.AddrPort
	block   map[string]bool
	fails   map[string]int
	calls   map[string]int
	sockets map[net.PacketConn]bool
}

func newFakeSTUN(answers map[string]string) *fakeSTUN {
	f := &fakeSTUN{answers: map[string]netip.AddrPort{}, block: map[string]bool{}, fails: map[string]int{},
		calls: map[string]int{}, sockets: map[net.PacketConn]bool{}}
	for s, a := range answers {
		f.answers[s] = netip.MustParseAddrPort(a)
	}
	return f
}

func (f *fakeSTUN) Mapped(ctx context.Context, sock net.PacketConn, server string) (netip.AddrPort, error) {
	f.mu.Lock()
	f.calls[server]++
	f.sockets[sock] = true
	fail := f.fails[server] > 0
	if fail {
		f.fails[server]--
	}
	a, ok := f.answers[server]
	block := f.block[server]
	f.mu.Unlock()
	switch {
	case block:
		<-ctx.Done()
		return netip.AddrPort{}, ctx.Err()
	case fail:
		return netip.AddrPort{}, errors.New("stun: fake failure")
	case !ok:
		return netip.AddrPort{}, errors.New("stun: no answer")
	}
	return a, nil
}

func (f *fakeSTUN) callCount(server string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[server]
}

var testSTUNServers = []string{"stun.example.net:3478", "stun2.example.net:19302"}

// memSocket is the listenSTUN hook for tests: an in-memory socket, so no test here touches the network.
func memSocket(context.Context) (net.PacketConn, error) {
	return newMemPacketConn("0.0.0.0:40000"), nil
}

// TestDetectPublicAddrsV4 covers each row of the classification table of 04 §7.4.
func TestDetectPublicAddrsV4(t *testing.T) {
	const s1, s2 = "stun.example.net:3478", "stun2.example.net:19302"
	type want struct {
		v4     string
		method Method
		nat    api.NATKind
	}
	for _, tc := range []struct {
		name      string
		ifs       []Interface
		localV4   string
		answers   map[string]string
		provider  api.CloudProvider
		container bool
		publicIP  string
		want      want
	}{
		{
			name:    "STUN IP is on an interface",
			ifs:     []Interface{loIface(), iface("eth0", 0, "203.0.113.7", "10.0.0.5")},
			localV4: "203.0.113.7",
			answers: map[string]string{s1: "203.0.113.7:40000", s2: "203.0.113.7:40000"},
			want:    want{"203.0.113.7", MethodInterface, api.NATKindNone},
		},
		{
			name:    "no STUN answer, a public IPv4 on an interface",
			ifs:     []Interface{loIface(), iface("eth0", 0, "10.0.0.5", "203.0.113.7")},
			localV4: "10.0.0.5",
			want:    want{"203.0.113.7", MethodInterface, api.NATKindUnknown},
		},
		{
			name:     "servers agree on another address, cloud provider: 1:1 NAT",
			ifs:      []Interface{loIface(), iface("ens5", 0, "10.0.1.5")},
			localV4:  "10.0.1.5",
			answers:  map[string]string{s1: "198.51.100.9:40000", s2: "198.51.100.9:40000"},
			provider: api.CloudProviderAWS,
			want:     want{"198.51.100.9", MethodSTUN, api.NATKindOneToOne},
		},
		{
			name:      "servers agree on another address, container: 1:1 NAT",
			ifs:       []Interface{loIface(), iface("eth0", 0, "172.17.0.2")},
			localV4:   "172.17.0.2",
			answers:   map[string]string{s1: "198.51.100.9:40000", s2: "198.51.100.9:40000"},
			provider:  api.CloudProviderUnknown,
			container: true,
			want:      want{"198.51.100.9", MethodSTUN, api.NATKindOneToOne},
		},
		{
			name:     "servers agree on another address, no cloud or container: port forward",
			ifs:      []Interface{loIface(), iface("eth0", 0, "192.168.1.20")},
			localV4:  "192.168.1.20",
			answers:  map[string]string{s1: "198.51.100.9:40000", s2: "198.51.100.9:40000"},
			provider: api.CloudProviderUnknown,
			want:     want{"198.51.100.9", MethodSTUN, api.NATKindPortForward},
		},
		{
			name:    "one server answers: port forward",
			ifs:     []Interface{iface("eth0", 0, "192.168.1.20")},
			localV4: "192.168.1.20",
			answers: map[string]string{s2: "198.51.100.9:40000"},
			want:    want{"198.51.100.9", MethodSTUN, api.NATKindPortForward},
		},
		{
			name:    "servers see different mapped ports: symmetric",
			ifs:     []Interface{iface("eth0", 0, "192.168.1.20")},
			localV4: "192.168.1.20",
			answers: map[string]string{s1: "198.51.100.9:40000", s2: "198.51.100.9:51234"},
			want:    want{"198.51.100.9", MethodSTUN, api.NATKindSymmetric},
		},
		{
			name:    "LocalV4 in 100.64/10 on a normal interface: CGNAT likely",
			ifs:     []Interface{iface("wwan0", 0, "100.72.3.4")},
			localV4: "100.72.3.4",
			answers: map[string]string{s1: "198.51.100.9:40000", s2: "198.51.100.9:51234"},
			want:    want{"198.51.100.9", MethodSTUN, api.NATKindCGNATLikely},
		},
		{
			name:    "CGNAT likely without STUN answers",
			ifs:     []Interface{iface("wwan0", 0, "100.72.3.4")},
			localV4: "100.72.3.4",
			want:    want{"", MethodNone, api.NATKindCGNATLikely},
		},
		{
			name:    "LocalV4 in 100.64/10 on Tailscale is ignored",
			ifs:     []Interface{iface("eth0", 0, "192.168.1.20"), iface("tailscale0", 0, "100.101.102.103")},
			localV4: "100.101.102.103",
			answers: map[string]string{s1: "198.51.100.9:40000", s2: "198.51.100.9:40000"},
			want:    want{"198.51.100.9", MethodSTUN, api.NATKindPortForward},
		},
		{
			name:    "nothing found",
			ifs:     []Interface{loIface(), iface("eth0", 0, "192.168.1.20")},
			localV4: "192.168.1.20",
			want:    want{"", MethodNone, api.NATKindUnknown},
		},
		{
			name:     "public_ip literal: config wins, STUN only reports the NAT",
			ifs:      []Interface{iface("eth0", 0, "192.168.1.20")},
			localV4:  "192.168.1.20",
			answers:  map[string]string{s1: "198.51.100.9:40000", s2: "198.51.100.9:40000"},
			publicIP: "203.0.113.50",
			want:     want{"203.0.113.50", MethodConfig, api.NATKindPortForward},
		},
		{
			name:     "public_ip literal on an interface without STUN answers",
			ifs:      []Interface{iface("eth0", 0, "203.0.113.7")},
			localV4:  "203.0.113.7",
			publicIP: "203.0.113.7",
			want:     want{"203.0.113.7", MethodConfig, api.NATKindNone},
		},
		{
			name:     "IPv6 public_ip: no IPv4",
			ifs:      []Interface{iface("eth0", 0, "203.0.113.7", "2001:db8::7")},
			localV4:  "203.0.113.7",
			publicIP: "2001:db8::7",
			want:     want{"", MethodNone, api.NATKindUnknown},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lister := &fakeIfaces{ifs: tc.ifs, routes: map[netip.Addr]netip.Addr{}}
			if tc.localV4 != "" {
				lister.routes[v4Probe] = netip.MustParseAddr(tc.localV4)
			}
			stun := newFakeSTUN(tc.answers)
			got, err := DetectPublicAddrs(context.Background(), DetectOptions{
				PublicIP: tc.publicIP, STUNServers: testSTUNServers, Timeout: 50 * time.Millisecond,
				STUN: stun, Interfaces: lister, CloudProvider: tc.provider, InContainer: tc.container,
				listenSTUN: memSocket,
			})
			if err != nil {
				t.Fatal(err)
			}
			wantV4 := netip.Addr{}
			if tc.want.v4 != "" {
				wantV4 = netip.MustParseAddr(tc.want.v4)
			}
			if got.V4 != wantV4 || got.V4Method != tc.want.method || got.NAT != tc.want.nat {
				t.Errorf("got V4 %v (%s), NAT %s; want %v (%s), %s",
					got.V4, got.V4Method, got.NAT, wantV4, tc.want.method, tc.want.nat)
			}
			if tc.localV4 != "" && got.LocalV4 != netip.MustParseAddr(tc.localV4) {
				t.Errorf("LocalV4 = %v, want %s", got.LocalV4, tc.localV4)
			}
			if len(got.STUNMapped) != len(testSTUNServers) {
				t.Fatalf("STUNMapped = %v, want one entry per server", got.STUNMapped)
			}
			for i, s := range testSTUNServers {
				want := netip.AddrPort{}
				if a, ok := tc.answers[s]; ok {
					want = netip.MustParseAddrPort(a)
				}
				if got.STUNMapped[i] != want {
					t.Errorf("STUNMapped[%d] = %v, want %v", i, got.STUNMapped[i], want)
				}
				if n := stun.callCount(s); n == 0 {
					t.Errorf("server %s was not asked", s)
				}
			}
			if len(stun.sockets) != 1 {
				t.Errorf("STUN used %d sockets, want one shared socket", len(stun.sockets))
			}
			if got.DetectedAt.IsZero() {
				t.Error("DetectedAt not set")
			}
		})
	}
}

func TestDetectPublicAddrsSTUNOff(t *testing.T) {
	lister := &fakeIfaces{ifs: []Interface{iface("eth0", 0, "203.0.113.7")}}
	for _, opts := range []DetectOptions{
		{PublicIP: "off", STUNServers: testSTUNServers},
		{PublicIP: "auto"}, // network.stun_servers = []
	} {
		stun := newFakeSTUN(map[string]string{testSTUNServers[0]: "198.51.100.9:1"})
		opts.STUN, opts.Interfaces, opts.listenSTUN = stun, lister, memSocket
		got, err := DetectPublicAddrs(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		if n := stun.callCount(testSTUNServers[0]); n != 0 || got.STUNMapped != nil {
			t.Errorf("public_ip %q with %d servers: %d STUN calls, STUNMapped %v; want none",
				opts.PublicIP, len(opts.STUNServers), n, got.STUNMapped)
		}
		if opts.PublicIP == "off" && (got.V4.IsValid() || got.V4Method != MethodNone) {
			t.Errorf("public_ip off: V4 %v (%s)", got.V4, got.V4Method)
		}
		if opts.PublicIP == "auto" && got.V4 != netip.MustParseAddr("203.0.113.7") {
			t.Errorf("without STUN the interface address must be found, got %v", got.V4)
		}
	}
}

func TestDetectPublicAddrsV6(t *testing.T) {
	ifs := []Interface{
		loIface(),
		iface("eth0", 0, "203.0.113.7", "fd00::1", "fe80::1", "2001:db8::a1~", "2001:db8::7", "2001:db8::8"),
	}
	for _, tc := range []struct {
		name               string
		ipv6               bool
		publicIP, publicV6 string
		route              string
		want               string
		method             Method
	}{
		{"auto: the routed global address", true, "", "auto", "2001:db8::8", "2001:db8::8", MethodInterface},
		{"auto: first global address when the route source is unusable", true, "", "", "2001:db8::a1", "2001:db8::7", MethodInterface},
		{"auto: no route", true, "", "", "", "2001:db8::7", MethodInterface},
		{"literal", true, "", "2001:db8::99", "", "2001:db8::99", MethodConfig},
		{"IPv6 public_ip literal", true, "2001:db8::7", "auto", "", "2001:db8::7", MethodConfig},
		{"off", true, "", "off", "2001:db8::8", "", MethodNone},
		{"network.ipv6 false", false, "", "2001:db8::99", "", "", MethodNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lister := &fakeIfaces{ifs: ifs, routes: map[netip.Addr]netip.Addr{}}
			if tc.route != "" {
				lister.routes[v6Probe] = netip.MustParseAddr(tc.route)
			}
			got, err := DetectPublicAddrs(context.Background(), DetectOptions{
				PublicIP: tc.publicIP, PublicIPv6: tc.publicV6, IPv6: tc.ipv6, Interfaces: lister,
			})
			if err != nil {
				t.Fatal(err)
			}
			want := netip.Addr{}
			if tc.want != "" {
				want = netip.MustParseAddr(tc.want)
			}
			if got.V6 != want || got.V6Method != tc.method {
				t.Errorf("V6 = %v (%s), want %v (%s)", got.V6, got.V6Method, want, tc.method)
			}
		})
	}
	// Only ULA and link-local addresses: no public IPv6.
	lister := &fakeIfaces{ifs: []Interface{iface("eth0", 0, "fd00::1", "fe80::1")}}
	got, err := DetectPublicAddrs(context.Background(), DetectOptions{IPv6: true, Interfaces: lister})
	if err != nil || got.V6.IsValid() || got.V6Method != MethodNone {
		t.Errorf("ULA only: V6 %v (%s), err %v; want none", got.V6, got.V6Method, err)
	}
}

func TestDetectPublicAddrsErrors(t *testing.T) {
	lister := &fakeIfaces{ifs: []Interface{iface("eth0", 0, "203.0.113.7")}}
	for _, opts := range []DetectOptions{
		{PublicIP: "not-an-ip"},
		{PublicIP: "fe80::1%eth0"},
		{PublicIPv6: "203.0.113.7"},
		{PublicIPv6: "bogus"},
	} {
		opts.Interfaces = lister
		if _, err := DetectPublicAddrs(context.Background(), opts); err == nil {
			t.Errorf("DetectPublicAddrs(%q, %q) succeeded, want an error", opts.PublicIP, opts.PublicIPv6)
		}
	}
	broken := &fakeIfaces{err: errors.New("netlink: permission denied")}
	if _, err := DetectPublicAddrs(context.Background(), DetectOptions{Interfaces: broken}); err == nil {
		t.Error("an interface listing error must be returned")
	}
}

// TestDetectPublicAddrsRetry: a server that fails once is asked again, and its second answer counts.
func TestDetectPublicAddrsRetry(t *testing.T) {
	stun := newFakeSTUN(map[string]string{testSTUNServers[0]: "198.51.100.9:40000",
		testSTUNServers[1]: "198.51.100.9:40000"})
	stun.fails[testSTUNServers[0]] = 1
	stun.fails[testSTUNServers[1]] = 5 // fails both attempts
	got, err := DetectPublicAddrs(context.Background(), DetectOptions{
		STUNServers: testSTUNServers, STUN: stun, listenSTUN: memSocket,
		Interfaces: &fakeIfaces{ifs: []Interface{iface("eth0", 0, "192.168.1.20")},
			routes: map[netip.Addr]netip.Addr{v4Probe: netip.MustParseAddr("192.168.1.20")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stun.callCount(testSTUNServers[0]) != 2 || stun.callCount(testSTUNServers[1]) != 2 {
		t.Errorf("calls = %d, %d; want 2 each (one retry)", stun.callCount(testSTUNServers[0]),
			stun.callCount(testSTUNServers[1]))
	}
	want := []netip.AddrPort{netip.MustParseAddrPort("198.51.100.9:40000"), {}}
	if !slices.Equal(got.STUNMapped, want) {
		t.Errorf("STUNMapped = %v, want %v", got.STUNMapped, want)
	}
	if got.V4 != netip.MustParseAddr("198.51.100.9") || got.NAT != api.NATKindPortForward {
		t.Errorf("V4 %v NAT %s", got.V4, got.NAT)
	}
}

// TestDetectPublicAddrsTimeouts runs on a fake clock: servers that never answer cost two attempts of Timeout
// each, in parallel, and the whole detection never takes longer than 5 s.
func TestDetectPublicAddrsTimeouts(t *testing.T) {
	for _, tc := range []struct {
		timeout, want time.Duration
	}{
		{0, 2 * DefaultSTUNTimeout}, // the default: 2 s per attempt
		{time.Second, 2 * time.Second},
		{3 * time.Second, detectBudget},
	} {
		synctest.Test(t, func(t *testing.T) {
			stun := newFakeSTUN(nil)
			for _, s := range testSTUNServers {
				stun.block[s] = true
			}
			start := time.Now()
			got, err := DetectPublicAddrs(context.Background(), DetectOptions{
				STUNServers: testSTUNServers, Timeout: tc.timeout, STUN: stun, listenSTUN: memSocket,
				Interfaces: &fakeIfaces{ifs: []Interface{iface("eth0", 0, "192.168.1.20")}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if d := time.Since(start); d != tc.want {
				t.Errorf("Timeout %v: detection took %v, want %v", tc.timeout, d, tc.want)
			}
			if got.V4.IsValid() || got.NAT != api.NATKindUnknown {
				t.Errorf("V4 %v NAT %s, want none and unknown", got.V4, got.NAT)
			}
		})
	}
}
