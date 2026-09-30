package netx

import (
	"net"
	"net/netip"
	"slices"
	"testing"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/protocol/api"
)

func addrs(ss ...string) []netip.Addr {
	out := make([]netip.Addr, 0, len(ss))
	for _, s := range ss {
		out = append(out, netip.MustParseAddr(s))
	}
	return out
}

func mustFilter(t *testing.T, globs ...string) func(string) bool {
	t.Helper()
	f, err := interfaceFilter(globs)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestPlanAddrs covers each row of 04 §7.5 (and the fallback when no public address is known).
func TestPlanAddrs(t *testing.T) {
	v4Rule := func(ext, local string, mode webrtc.ICEAddressRewriteMode) webrtc.ICEAddressRewriteRule {
		return webrtc.ICEAddressRewriteRule{
			External: []string{ext}, Local: local, AsCandidateType: webrtc.ICECandidateTypeHost, Mode: mode,
			Networks: []webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeTCP4},
		}
	}
	for _, tc := range []struct {
		name      string
		in        planInput
		keep      []netip.Addr
		rules     []webrtc.ICEAddressRewriteRule
		advertise map[string][]netip.Addr // local → advertised
	}{
		{
			name: "public IP on an interface: no rule, private VPC address not advertised",
			in: planInput{
				ifs: []Interface{loIface(), iface("eth0", 0, "203.0.113.7", "10.0.0.5", "2001:db8::7", "fe80::1",
					"2001:db8::a1~"), iface("docker0", 0, "172.17.0.1")},
				ifFilter: mustFilter(t, "docker*"),
				public:   PublicAddrs{V4: netip.MustParseAddr("203.0.113.7"), V4Method: MethodInterface, NAT: api.NATKindNone},
				ipv6:     true,
			},
			keep:      addrs("203.0.113.7", "2001:db8::7"),
			advertise: map[string][]netip.Addr{"203.0.113.7": addrs("203.0.113.7")},
		},
		{
			name: "the same in development: loopback kept",
			in: planInput{
				ifs:             []Interface{loIface(), iface("eth0", 0, "203.0.113.7", "10.0.0.5")},
				public:          PublicAddrs{V4: netip.MustParseAddr("203.0.113.7"), V4Method: MethodInterface},
				includeLoopback: true,
				ipv6:            true,
			},
			keep: addrs("127.0.0.1", "203.0.113.7", "::1"),
		},
		{
			name: "1:1 NAT on a cloud provider: Replace",
			in: planInput{
				ifs: []Interface{loIface(), iface("ens5", 0, "10.0.1.5", "10.0.9.9")},
				public: PublicAddrs{V4: netip.MustParseAddr("198.51.100.9"), V4Method: MethodSTUN,
					LocalV4: netip.MustParseAddr("10.0.1.5"), NAT: api.NATKindOneToOne},
				provider: api.CloudProviderAWS,
			},
			keep:      addrs("10.0.1.5"),
			rules:     []webrtc.ICEAddressRewriteRule{v4Rule("198.51.100.9", "10.0.1.5", webrtc.ICEAddressRewriteReplace)},
			advertise: map[string][]netip.Addr{"10.0.1.5": addrs("198.51.100.9"), "10.0.9.9": addrs("10.0.9.9")},
		},
		{
			name: "Docker bridge: Replace",
			in: planInput{
				ifs: []Interface{loIface(), iface("eth0", 0, "172.17.0.2")},
				public: PublicAddrs{V4: netip.MustParseAddr("203.0.113.7"), V4Method: MethodSTUN,
					LocalV4: netip.MustParseAddr("172.17.0.2"), NAT: api.NATKindOneToOne},
				inContainer: true,
				provider:    api.CloudProviderUnknown,
			},
			keep:      addrs("172.17.0.2"),
			rules:     []webrtc.ICEAddressRewriteRule{v4Rule("203.0.113.7", "172.17.0.2", webrtc.ICEAddressRewriteReplace)},
			advertise: map[string][]netip.Addr{"172.17.0.2": addrs("203.0.113.7")},
		},
		{
			name: "home LAN server behind a router: Append",
			in: planInput{
				ifs: []Interface{loIface(), iface("eth0", 0, "192.168.1.20", "fd00::20", "2001:db8::20")},
				public: PublicAddrs{V4: netip.MustParseAddr("198.51.100.9"), V4Method: MethodSTUN,
					LocalV4: netip.MustParseAddr("192.168.1.20"), NAT: api.NATKindPortForward},
				provider: api.CloudProviderUnknown,
				ipv6:     true,
			},
			keep:      addrs("192.168.1.20", "2001:db8::20"),
			rules:     []webrtc.ICEAddressRewriteRule{v4Rule("198.51.100.9", "192.168.1.20", webrtc.ICEAddressRewriteAppend)},
			advertise: map[string][]netip.Addr{"192.168.1.20": addrs("192.168.1.20", "198.51.100.9")},
		},
		{
			name: "public_ipv6 literal not on an interface: Replace on one local IPv6",
			in: planInput{
				ifs: []Interface{loIface(), iface("eth0", 0, "203.0.113.7", "fd00::5", "fd00::6")},
				public: PublicAddrs{V4: netip.MustParseAddr("203.0.113.7"), V4Method: MethodInterface,
					V6: netip.MustParseAddr("2001:db8::99"), V6Method: MethodConfig},
				includeLoopback: true,
				ipv6:            true,
			},
			keep: addrs("127.0.0.1", "203.0.113.7", "::1", "fd00::5"),
			rules: []webrtc.ICEAddressRewriteRule{{
				External: []string{"2001:db8::99"}, Local: "fd00::5", AsCandidateType: webrtc.ICECandidateTypeHost,
				Mode:     webrtc.ICEAddressRewriteReplace,
				Networks: []webrtc.NetworkType{webrtc.NetworkTypeUDP6, webrtc.NetworkTypeTCP6},
			}},
			advertise: map[string][]netip.Addr{"fd00::5": addrs("2001:db8::99"), "::1": addrs("::1")},
		},
		{
			name: "IPv6 global address on an interface: kept, no rule",
			in: planInput{
				ifs: []Interface{iface("eth0", 0, "2001:db8::7", "fd00::7")},
				public: PublicAddrs{V6: netip.MustParseAddr("2001:db8::7"), V6Method: MethodInterface,
					NAT: api.NATKindUnknown},
				ipv6: true,
			},
			keep: addrs("2001:db8::7"),
		},
		{
			name: "IPv6 off",
			in: planInput{
				ifs:    []Interface{iface("eth0", 0, "203.0.113.7", "2001:db8::7")},
				public: PublicAddrs{V4: netip.MustParseAddr("203.0.113.7"), V6: netip.MustParseAddr("2001:db8::7")},
			},
			keep: addrs("203.0.113.7"),
		},
		{
			name: "nothing known: every usable IPv4 kept, private ones too",
			in: planInput{
				ifs: []Interface{loIface(), iface("eth0", 0, "192.168.1.20", "169.254.3.3"),
					{Name: "eth1", Flags: 0, Addrs: []InterfaceAddr{{Addr: netip.MustParseAddr("10.9.9.9")}}}},
			},
			keep: addrs("192.168.1.20"),
		},
		{
			name: "public IP known but LocalV4 unusable: no rule",
			in: planInput{
				ifs: []Interface{iface("eth0", 0, "192.168.1.20")},
				public: PublicAddrs{V4: netip.MustParseAddr("198.51.100.9"), V4Method: MethodSTUN,
					LocalV4: netip.MustParseAddr("192.168.7.7")},
			},
			keep: addrs("192.168.1.20"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := planAddrs(tc.in)
			if !slices.Equal(p.keep, tc.keep) {
				t.Errorf("keep = %v, want %v", p.keep, tc.keep)
			}
			if len(p.rules) != len(tc.rules) {
				t.Fatalf("rules = %+v, want %+v", p.rules, tc.rules)
			}
			for i := range p.rules {
				g, w := p.rules[i], tc.rules[i]
				if !slices.Equal(g.External, w.External) || g.Local != w.Local || g.Mode != w.Mode ||
					g.AsCandidateType != w.AsCandidateType || !slices.Equal(g.Networks, w.Networks) {
					t.Errorf("rule %d = %+v, want %+v", i, g, w)
				}
			}
			for local, want := range tc.advertise {
				if got := advertise(p.rules, netip.MustParseAddr(local)); !slices.Equal(got, want) {
					t.Errorf("advertise(%s) = %v, want %v", local, got, want)
				}
			}
			// The rules must be acceptable to pion.
			var se webrtc.SettingEngine
			if err := se.SetICEAddressRewriteRules(p.rules...); err != nil {
				t.Errorf("SetICEAddressRewriteRules: %v", err)
			}
		})
	}
}

func TestPlanSkipsDownInterfaces(t *testing.T) {
	p := planAddrs(planInput{ifs: []Interface{
		{Name: "eth0", Flags: net.FlagUp, Addrs: []InterfaceAddr{{Addr: netip.MustParseAddr("203.0.113.7")}}},
		{Name: "eth1", Addrs: []InterfaceAddr{{Addr: netip.MustParseAddr("203.0.113.8")}}},
	}})
	if !slices.Equal(p.keep, addrs("203.0.113.7")) {
		t.Errorf("keep = %v, want only the up interface's address", p.keep)
	}
}
