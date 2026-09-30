package netx

import (
	"errors"
	"net/netip"
	"testing"
)

// TestIPKey is the IPKey table of 04 §17; the wiring test pins 03's limiter key to the same results.
func TestIPKey(t *testing.T) {
	for _, tc := range []struct{ addr, want string }{
		{"203.0.113.7", "203.0.113.7/32"},
		{"::ffff:203.0.113.7", "203.0.113.7/32"}, // IPv4-mapped IPv6 is unmapped first
		{"2001:db8:1:2::1", "2001:db8:1:2::/64"},
		{"2001:db8:1:2:ffff:ffff:ffff:ffff", "2001:db8:1:2::/64"}, // same /64
		{"2001:db8:1:3::1", "2001:db8:1:3::/64"},                  // the neighbouring /64
		{"fe80::1%eth0", "fe80::/64"},
		{"::1", "::/64"},
	} {
		if got := IPKey(netip.MustParseAddr(tc.addr)); got != netip.MustParsePrefix(tc.want) {
			t.Errorf("IPKey(%s) = %v, want %s", tc.addr, got, tc.want)
		}
	}
	if got := IPKey(netip.Addr{}); got.IsValid() {
		t.Errorf("IPKey(zero) = %v, want the zero Prefix", got)
	}
}

func TestListenPortMuxNotImplemented(t *testing.T) {
	m, err := ListenPortMux(PortMuxOptions{Addr: "127.0.0.1:0"})
	if m != nil || !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("ListenPortMux = %v, %v; want ErrNotImplemented until README S26", m, err)
	}
}
