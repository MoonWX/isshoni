package netx

import (
	"context"
	"net/netip"
	"testing"
)

func TestParseIfInet6(t *testing.T) {
	// Columns: address, ifindex, prefix length, scope, flags, name (hex).
	const procFile = `20010db8000000000000000000000007 02 40 00 80     eth0
20010db800000000a1b2c3d4e5f60718 02 40 00 01     eth0
20010db800000000000000000000beef 02 40 00 20     eth0
20010db800000000000000000000cafe 02 40 00 40     eth0
fe800000000000000000000000000001 02 40 20 80     eth0
00000000000000000000000000000001 01 80 10 80       lo
garbage line
zz0db800000000000000000000000001 02 40 00 01     eth0
`
	got := parseIfInet6([]byte(procFile))
	want := map[netip.Addr]bool{
		netip.MustParseAddr("2001:db8::a1b2:c3d4:e5f6:718"): true, // temporary
		netip.MustParseAddr("2001:db8::beef"):               true, // deprecated
		netip.MustParseAddr("2001:db8::cafe"):               true, // tentative
	}
	if len(got) != len(want) {
		t.Fatalf("parseIfInet6 = %v, want %v", got, want)
	}
	for a := range want {
		if !got[a] {
			t.Errorf("%v not marked unusable", a)
		}
	}
}

func TestInterfaceFilterGlobs(t *testing.T) {
	defaults := []string{"docker*", "br-*", "veth*", "virbr*", "cni*", "flannel*", "cali*", "kube-*"}
	keep, err := interfaceFilter(defaults)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{
		"eth0": true, "ens3": true, "lo": true, "wlan0": true, "tailscale0": true,
		"docker0": false, "br-3f2a9c": false, "veth12ab": false, "virbr0": false, "cni0": false,
		"flannel.1": false, "cali9e8d": false, "kube-ipvs0": false,
	} {
		if got := keep(name); got != want {
			t.Errorf("filter(%q) = %v, want %v", name, got, want)
		}
	}
	if _, err := interfaceFilter([]string{"eth["}); err == nil {
		t.Error("a malformed glob must be an error")
	}
	none, err := interfaceFilter(nil)
	if err != nil || !none("docker0") {
		t.Errorf("no globs must keep everything (err %v)", err)
	}
}

func TestAddressClasses(t *testing.T) {
	for _, tc := range []struct {
		addr                    string
		publicV4, globalV6, lan bool
	}{
		{"203.0.113.7", true, false, false},
		{"8.8.8.8", true, false, false},
		{"10.0.0.5", false, false, true},
		{"172.17.0.2", false, false, true},
		{"192.168.1.20", false, false, true},
		{"100.64.0.1", false, false, true},
		{"100.127.255.254", false, false, true},
		{"100.128.0.1", true, false, false},
		{"127.0.0.1", false, false, false},
		{"169.254.1.1", false, false, false},
		{"2001:db8::7", false, true, false},
		{"fd00::5", false, false, true},
		{"fe80::1", false, false, false},
		{"::1", false, false, false},
		{"::ffff:203.0.113.7", true, false, false},
	} {
		a := netip.MustParseAddr(tc.addr)
		if got := isPublicV4(a); got != tc.publicV4 {
			t.Errorf("isPublicV4(%s) = %v", tc.addr, got)
		}
		if got := isGlobalV6(a); got != tc.globalV6 {
			t.Errorf("isGlobalV6(%s) = %v", tc.addr, got)
		}
		if got := isLANAddr(a); got != tc.lan {
			t.Errorf("isLANAddr(%s) = %v", tc.addr, got)
		}
	}
}

func TestUsableIfaceAddr(t *testing.T) {
	for _, tc := range []struct {
		addr      string
		temporary bool
		want      bool
	}{
		{"203.0.113.7", false, true},
		{"127.0.0.1", false, true},
		{"::1", false, true},
		{"2001:db8::7", false, true},
		{"2001:db8::7", true, false},
		{"fd00::5", false, true},
		{"fe80::1", false, false},
		{"169.254.10.1", false, false},
		{"0.0.0.0", false, false},
		{"::", false, false},
		{"224.0.0.1", false, false},
		{"::203.0.113.7", false, false}, // IPv4-compatible
		{"fec0::1", false, false},       // site-local
	} {
		ia := InterfaceAddr{Addr: netip.MustParseAddr(tc.addr), Temporary: tc.temporary}
		if got := usableIfaceAddr(ia); got != tc.want {
			t.Errorf("usableIfaceAddr(%s, temporary %v) = %v, want %v", tc.addr, tc.temporary, got, tc.want)
		}
	}
	var zero InterfaceAddr
	if usableIfaceAddr(zero) {
		t.Error("the zero address must not be usable")
	}
}

// TestPionSkipsV6: the IPv6 addresses pion's interface scan drops (ice's isSupportedIPv6Partial): ::/96, with the
// loopback ::1 in it, and site-local fec0::/10. IPv4 addresses, mapped ones included, are not its business.
func TestPionSkipsV6(t *testing.T) {
	for addr, want := range map[string]bool{
		"::1":                true, // usable for UDP, never for ICE-TCP
		"::":                 true,
		"::203.0.113.7":      true, // IPv4-compatible
		"::7f00:1":           true, // ::127.0.0.1
		"fec0::1":            true, // site-local
		"feff::1":            true,
		"::1:0:0":            false, // just above ::/96
		"2001:db8::7":        false,
		"fd00::5":            false,
		"fe80::1":            false, // link-local: dropped elsewhere (usableIfaceAddr)
		"127.0.0.1":          false,
		"203.0.113.7":        false,
		"::ffff:127.0.0.1":   false,
		"::ffff:203.0.113.7": false,
	} {
		if got := pionSkipsV6(netip.MustParseAddr(addr)); got != want {
			t.Errorf("pionSkipsV6(%s) = %v, want %v", addr, got, want)
		}
	}
	if pionSkipsV6(netip.Addr{}) {
		t.Error("the zero address is not an IPv6 address")
	}
}

func TestIsTailscale(t *testing.T) {
	for name, want := range map[string]bool{"tailscale0": true, "utun4": true, "eth0": false, "wg0": false} {
		if got := isTailscale(name); got != want {
			t.Errorf("isTailscale(%q) = %v", name, got)
		}
	}
}

func TestSystemInterfaces(t *testing.T) {
	ifs, err := SystemInterfaces().Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range ifs {
		for _, ia := range it.Addrs {
			if ia.Addr == netip.MustParseAddr("127.0.0.1") && it.Loopback() && it.Up() {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("no up loopback interface with 127.0.0.1 in %+v", ifs)
	}
	src, err := SystemInterfaces().RouteSource(context.Background(), netip.MustParseAddr("127.0.0.1"))
	if err != nil || src != netip.MustParseAddr("127.0.0.1") {
		t.Errorf("RouteSource(127.0.0.1) = %v, %v", src, err)
	}
}
