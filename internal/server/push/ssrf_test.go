package push

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// Addresses of the test tables: public ones, and the server's own.
const (
	publicV4 = "93.184.216.34"
	publicV6 = "2606:2800:220:1:248:1893:25c8:1946"
	ownV4    = "93.184.216.99"
	ownV6    = "2a01:4f8:c0c:1234::1"
)

func ownAddrs() []netip.Addr {
	return []netip.Addr{netip.MustParseAddr(ownV4), netip.MustParseAddr(ownV6), {}}
}

func TestValidateEndpoint(t *testing.T) {
	const (
		ok          = api.PushRejectReason("")
		notHTTPS    = api.PushRejectReasonNotHTTPS
		badPort     = api.PushRejectReasonBadPort
		userinfo    = api.PushRejectReasonUserinfo
		ipLiteral   = api.PushRejectReasonIPLiteral
		private     = api.PushRejectReasonPrivateAddress
		unresolved  = api.PushRejectReasonUnresolvable
		noLookup    = false
		wantsLookup = true
	)
	hosts := map[string][]string{
		"push.example.com":      {publicV4},
		"dual.example.com":      {publicV4, publicV6},
		"fcm.googleapis.com":    {"142.250.74.106", "2a00:1450:4001:82b::200a"},
		"nat64.example.com":     {"64:ff9b::5db8:d822"}, // 93.184.216.34 behind the NAT64 prefix
		"ten.example.com":       {"10.1.2.3"},
		"rfc1918-b.example.com": {"172.16.0.1"},
		"rfc1918-c.example.com": {"192.168.1.1"},
		"loop.example.com":      {"127.0.0.1"},
		"loop6.example.com":     {"::1"},
		"ula.example.com":       {"fd00::1"},
		"metadata.example.com":  {"169.254.169.254"},
		"cgnat.example.com":     {"100.64.0.1"},
		"zero.example.com":      {"0.0.0.0"},
		"linklocal.example.com": {"fe80::1"},
		"mapped.example.com":    {"::ffff:10.0.0.1"},
		"nat64-10.example.com":  {"64:ff9b::a00:1"},
		"multicast.example.com": {"224.0.0.251"},
		"docs.example.com":      {"203.0.113.9"},
		"mixed.example.com":     {publicV4, "10.0.0.1"},
		"mixed6.example.com":    {publicV4, "fd12:3456::1"},
		"self.example.com":      {ownV4},
		"self6.example.com":     {ownV6},
		"empty.example.com":     {},
		"under_score.example":   {publicV4},
	}
	long := strings.Repeat("a", 64)
	tests := []struct {
		endpoint string
		want     api.PushRejectReason
		lookup   bool
	}{
		// Accepted.
		{"https://push.example.com/send/abc", ok, wantsLookup},
		{"https://push.example.com:443/send/abc", ok, wantsLookup},
		{"HTTPS://PUSH.EXAMPLE.COM/send/abc", ok, wantsLookup},
		{"https://push.example.com./send/abc", ok, wantsLookup},
		{"https://dual.example.com/wpush/v2/abc?x=1", ok, wantsLookup},
		{"https://fcm.googleapis.com/fcm/send/dx1", ok, wantsLookup},
		{"https://nat64.example.com/x", ok, wantsLookup},
		{"https://under_score.example/x", ok, wantsLookup},

		// Scheme.
		{"http://push.example.com/send/abc", notHTTPS, noLookup},
		{"ws://push.example.com/send/abc", notHTTPS, noLookup},
		{"ftp://push.example.com/", notHTTPS, noLookup},
		{"push.example.com/send/abc", notHTTPS, noLookup},
		{"//push.example.com/send/abc", notHTTPS, noLookup},
		{"https:push.example.com", notHTTPS, noLookup},
		{"javascript:alert(1)", notHTTPS, noLookup},
		{"file:///etc/passwd", notHTTPS, noLookup},
		{"", notHTTPS, noLookup},
		{"\x00https://push.example.com/", notHTTPS, noLookup},

		// Port.
		{"https://push.example.com:8443/send/abc", badPort, noLookup},
		{"https://push.example.com:80/", badPort, noLookup},
		{"https://push.example.com:0/", badPort, noLookup},
		{"https://push.example.com:0443/", badPort, noLookup},
		{"https://push.example.com:/", badPort, noLookup},
		{"https://10.0.0.1:8443/", badPort, noLookup},
		{"https://push.example.com:abc/", badPort, noLookup},
		{"https://push.example.com:443x/send?a=b:c", badPort, noLookup},

		// Userinfo.
		{"https://user@push.example.com/send/abc", userinfo, noLookup},
		{"https://user:secret@push.example.com/", userinfo, noLookup},
		{"https://@push.example.com/", userinfo, noLookup},
		{"https://push.example.com@10.0.0.1/", userinfo, noLookup},

		// IP literals, in every spelling.
		{"https://93.184.216.34/send/abc", ipLiteral, noLookup},
		{"https://93.184.216.34./send/abc", ipLiteral, noLookup},
		{"https://10.0.0.1/", ipLiteral, noLookup},
		{"https://127.0.0.1/", ipLiteral, noLookup},
		{"https://169.254.169.254/latest/meta-data/", ipLiteral, noLookup},
		{"https://100.64.0.1/", ipLiteral, noLookup},
		{"https://[::1]/", ipLiteral, noLookup},
		{"https://[::1]:443/", ipLiteral, noLookup},
		{"https://[fd00::1]/", ipLiteral, noLookup},
		{"https://[2606:4700:4700::1111]/", ipLiteral, noLookup},
		{"https://[::ffff:10.0.0.1]/", ipLiteral, noLookup},
		{"https://[fe80::1%25eth0]/", ipLiteral, noLookup},
		{"https://2130706433/", ipLiteral, noLookup},
		{"https://0x7f000001/", ipLiteral, noLookup},
		{"https://0x7f.0.0.1/", ipLiteral, noLookup},
		{"https://0177.0.0.1/", ipLiteral, noLookup},
		{"https://127.1/", ipLiteral, noLookup},
		{"https://example.com.1/", ipLiteral, noLookup},

		// localhost, whatever the resolver would say.
		{"https://localhost/send/abc", private, noLookup},
		{"https://LOCALHOST/", private, noLookup},
		{"https://localhost./", private, noLookup},
		{"https://app.localhost/", private, noLookup},

		// Names that resolve to an address the server must not call.
		{"https://ten.example.com/", private, wantsLookup},
		{"https://rfc1918-b.example.com/", private, wantsLookup},
		{"https://rfc1918-c.example.com/", private, wantsLookup},
		{"https://loop.example.com/", private, wantsLookup},
		{"https://loop6.example.com/", private, wantsLookup},
		{"https://ula.example.com/", private, wantsLookup},
		{"https://metadata.example.com/latest/meta-data/", private, wantsLookup},
		{"https://cgnat.example.com/", private, wantsLookup},
		{"https://zero.example.com/", private, wantsLookup},
		{"https://linklocal.example.com/", private, wantsLookup},
		{"https://mapped.example.com/", private, wantsLookup},
		{"https://nat64-10.example.com/", private, wantsLookup},
		{"https://multicast.example.com/", private, wantsLookup},
		{"https://docs.example.com/", private, wantsLookup},
		{"https://mixed.example.com/", private, wantsLookup},
		{"https://mixed6.example.com/", private, wantsLookup},
		{"https://self.example.com/", private, wantsLookup},
		{"https://self6.example.com/", private, wantsLookup},

		// Names that don't resolve, or that are no DNS names.
		{"https://nxdomain.example.com/", unresolved, wantsLookup},
		{"https://empty.example.com/", unresolved, wantsLookup},
		{"https:///send/abc", unresolved, noLookup},
		{"https://exa mple.com/", unresolved, noLookup},
		{"https://ex*ample.com/", unresolved, noLookup},
		{"https://a..example.com/", unresolved, noLookup},
		{"https://.example.com/", unresolved, noLookup},
		{"https://" + long + ".example.com/", unresolved, noLookup},
		{"https://" + strings.Repeat("a.", 127) + "com/", unresolved, noLookup},
		{"https://bücher.example/", unresolved, noLookup},
	}
	for _, tc := range tests {
		t.Run(tc.endpoint, func(t *testing.T) {
			res := &fakeResolver{hosts: hosts}
			f := newFixture(t, func(o *Options) { o.Resolver, o.OwnAddrs = res, ownAddrs() })
			err := f.ValidateEndpoint(context.Background(), tc.endpoint)
			if tc.want == ok {
				if err != nil {
					t.Fatalf("ValidateEndpoint = %v, want nil", err)
				}
			} else {
				var ae *api.Error
				if !errors.As(err, &ae) {
					t.Fatalf("ValidateEndpoint = %v, want an *api.Error", err)
				}
				if ae.Code != api.CodePushEndpointRejected || ae.Params[api.ParamReason] != tc.want || len(ae.Params) != 1 {
					t.Fatalf("ValidateEndpoint = %s %v, want %s {reason: %s}", ae.Code, ae.Params,
						api.CodePushEndpointRejected, tc.want)
				}
				if st := api.StatusOf(ae.Code); st != http.StatusUnprocessableEntity {
					t.Errorf("status %d, want 422", st)
				}
			}
			if got := len(res.lookedUp()) > 0; got != tc.lookup {
				t.Errorf("resolver asked: %v (%v), want %v", got, res.lookedUp(), tc.lookup)
			}
		})
	}
}

// A resolver that never answers costs the subscribe request at most resolveTimeout.
func TestValidateEndpointResolverTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		res := &fakeResolver{hang: true}
		f := newFixture(t, func(o *Options) { o.Resolver = res })
		start := time.Now()
		err := f.ValidateEndpoint(context.Background(), "https://slow.example.com/x")
		if !api.IsCode(err, api.CodePushEndpointRejected) {
			t.Fatalf("ValidateEndpoint = %v, want push_endpoint_rejected", err)
		}
		var ae *api.Error
		errors.As(err, &ae)
		if ae.Params[api.ParamReason] != api.PushRejectReasonUnresolvable {
			t.Errorf("reason %v, want unresolvable", ae.Params[api.ParamReason])
		}
		if d := time.Since(start); d != resolveTimeout {
			t.Errorf("gave up after %v, want %v", d, resolveTimeout)
		}
	})
}

func TestGuardPublic(t *testing.T) {
	g := newGuard(ownAddrs(), false)
	allowed := []string{
		"8.8.8.8", "1.1.1.1", publicV4, "223.255.255.254",
		// Just outside each blocked IPv4 block.
		"9.255.255.255", "11.0.0.0", "100.63.255.255", "100.128.0.0", "126.255.255.255", "128.0.0.0",
		"169.253.255.255", "169.255.0.0", "172.15.255.255", "172.32.0.0", "191.255.255.255", "192.0.1.0",
		"192.0.3.0", "192.88.98.255", "192.88.100.0", "192.167.255.255", "192.169.0.0", "198.17.255.255",
		"198.20.0.0", "198.51.99.255", "198.51.101.0", "203.0.112.255", "203.0.114.0",
		publicV6, "2606:4700:4700::1111", "2a00:1450:4001:82b::200a", "2001:4860:4860::8888", "2001:200::1",
		"3ffe::1",
		"::ffff:8.8.8.8",     // IPv4-mapped public
		"64:ff9b::808:808",   // NAT64 of 8.8.8.8
		"64:ff9b::5db8:d822", // NAT64 of 93.184.216.34
	}
	for _, s := range allowed {
		if !g.public(netip.MustParseAddr(s)) {
			t.Errorf("%s refused, want allowed", s)
		}
	}
	refused := []string{
		"0.0.0.0", "0.1.2.3", // this network
		"10.0.0.0", "10.0.0.1", "10.255.255.255", // RFC 1918
		"172.16.0.0", "172.16.0.1", "172.31.255.255",
		"192.168.0.0", "192.168.1.1", "192.168.255.255",
		"100.64.0.0", "100.64.0.1", "100.127.255.255", // CGNAT
		"127.0.0.1", "127.255.255.254", // loopback
		"169.254.0.1", "169.254.169.254", // link-local, cloud metadata
		"192.0.0.1", "192.0.0.170", // IETF protocol assignments
		"192.0.2.1", "198.51.100.1", "203.0.113.1", // documentation
		"192.88.99.1",                  // 6to4 relay
		"198.18.0.1", "198.19.255.255", // benchmarking
		"224.0.0.1", "224.0.0.251", "239.255.255.255", // multicast
		"240.0.0.1", "255.255.255.255", // reserved, broadcast
		"::", "::1", // unspecified, loopback
		"::ffff:10.0.0.1", "::ffff:127.0.0.1", "::ffff:169.254.169.254", "::ffff:0.0.0.0", // IPv4-mapped
		"::10.0.0.1", "::8.8.8.8", // IPv4-compatible (deprecated)
		"64:ff9b::a00:1", "64:ff9b::7f00:1", "64:ff9b::a9fe:a9fe", "64:ff9b::c0a8:101", // NAT64 of private IPv4
		"64:ff9b:1::1",                                    // local-use NAT64
		"100::1",                                          // discard only
		"2001::1", "2001:0:4136:e378:8000:63bf:3fff:fdd2", // Teredo
		"2001:2::1", "2001:10::1", "2001:20::1", "2001:1ff::1", // other IETF assignments
		"2001:db8::1", "3fff::1", "3fff:fff:ffff::1", // documentation
		"2002:a00:1::1", "2002:808:808::1", // 6to4
		"fc00::1", "fd00::1", "fd12:3456:789a::1", "fdff:ffff::1", // ULA
		"fe80::1", "febf::1", // link-local
		"fec0::1",                       // site-local (deprecated)
		"ff02::1", "ff05::2", "ff0e::1", // multicast
		"5f00::1", "4000::1", "e000::1", // outside global unicast
		ownV4, ownV6, "::ffff:" + ownV4, "64:ff9b::5db8:d863", // this server (also mapped and behind NAT64)
	}
	for _, s := range refused {
		if g.public(netip.MustParseAddr(s)) {
			t.Errorf("%s allowed, want refused", s)
		}
	}
	if g.public(netip.Addr{}) {
		t.Error("the zero address is allowed")
	}
	if g.public(netip.MustParseAddr("2606:4700:4700::1111").WithZone("eth0")) {
		t.Error("an address with a zone is allowed")
	}
	if g.public(netip.MustParseAddr("fe80::1%eth0")) {
		t.Error("a link-local address with a zone is allowed")
	}

	// allowed is public unless the tests' switch is on.
	if g.allowed(netip.MustParseAddr("127.0.0.1")) || !g.allowed(netip.MustParseAddr("8.8.8.8")) {
		t.Error("allowed disagrees with public")
	}
	if lax := newGuard(nil, true); !lax.allowed(netip.MustParseAddr("127.0.0.1")) {
		t.Error("allowPrivate does not allow loopback")
	}
}

func TestGuardControl(t *testing.T) {
	g := newGuard(ownAddrs(), false)
	tests := []struct {
		address string
		blocked bool
	}{
		{"8.8.8.8:443", false},
		{"[2606:4700:4700::1111]:443", false},
		{"[64:ff9b::808:808]:443", false},
		{"8.8.8.8:80", true},
		{"8.8.8.8:8443", true},
		{"8.8.8.8:0", true},
		{"10.0.0.1:443", true},
		{"127.0.0.1:443", true},
		{"169.254.169.254:443", true},
		{"169.254.169.254:80", true},
		{"100.64.0.1:443", true},
		{"[::1]:443", true},
		{"[fd00::1]:443", true},
		{"[fe80::1%lo0]:443", true},
		{"[::ffff:10.0.0.1]:443", true},
		{ownV4 + ":443", true},
		{"[" + ownV6 + "]:443", true},
		{"push.example.com:443", true}, // the dialer only ever passes IP addresses; anything else is refused
		{"8.8.8.8", true},
		{"", true},
	}
	for _, tc := range tests {
		for _, network := range []string{"tcp4", "tcp6"} {
			err := g.control(network, tc.address, nil)
			if (err != nil) != tc.blocked {
				t.Errorf("control(%s, %q) = %v, want blocked %v", network, tc.address, err, tc.blocked)
			}
			if err != nil && (!errors.Is(err, ErrBlockedAddress) || !errors.Is(err, ErrUndeliverable)) {
				t.Errorf("control(%q) = %v, want it to wrap ErrBlockedAddress and ErrUndeliverable", tc.address, err)
			}
		}
	}

	lax := newGuard(nil, true)
	for _, address := range []string{"127.0.0.1:49152", "10.0.0.1:8443", "[::1]:1"} {
		if err := lax.control("tcp4", address, nil); err != nil {
			t.Errorf("allowPrivate: control(%q) = %v", address, err)
		}
	}
	if err := lax.control("tcp4", "not-an-address", nil); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("allowPrivate: control of a name = %v, want ErrBlockedAddress", err)
	}
}

func TestParseEndpointLax(t *testing.T) {
	// The tests' switch lets the fake push service's address through, and nothing else.
	for _, ok := range []string{"https://127.0.0.1:49152/push/abc", "https://[::1]:8443/x", "https://push.example.com/x"} {
		if u, reason := parseEndpoint(ok, true); reason != "" || u == nil {
			t.Errorf("lax parseEndpoint(%q) = %q, want accepted", ok, reason)
		}
	}
	for endpoint, want := range map[string]api.PushRejectReason{
		"http://127.0.0.1:49152/push/abc": api.PushRejectReasonNotHTTPS,
		"https://user@127.0.0.1/":         api.PushRejectReasonUserinfo,
		"https:///x":                      api.PushRejectReasonUnresolvable,
	} {
		if _, reason := parseEndpoint(endpoint, true); reason != want {
			t.Errorf("lax parseEndpoint(%q) = %q, want %q", endpoint, reason, want)
		}
	}
}

func TestPushHost(t *testing.T) {
	// CAPABILITY stands for the part of an endpoint that must stay out of the logs.
	tests := []struct{ endpoint, want string }{
		{"https://fcm.googleapis.com/fcm/send/CAPABILITY", "fcm.googleapis.com"},
		{"https://Updates.Push.Services.Mozilla.com/wpush/v2/CAPABILITY", "updates.push.services.mozilla.com"},
		{"https://web.push.apple.com:443/CAPABILITY", "web.push.apple.com"},
		{"https://[2606:4700::1111]/CAPABILITY", "2606:4700::1111"},
		{"https://push.example.com/x?k=CAPABILITY#CAPABILITY", "push.example.com"},
		{"https://CAPABILITY@push.example.com/x", "push.example.com"},
		{"", "invalid"},
		{"CAPABILITY", "invalid"},
		{"https://exa mple.com/CAPABILITY", "invalid"},
		{"https://" + strings.Repeat("a", 300) + ".example.com/CAPABILITY", strings.Repeat("a", 253)},
	}
	for _, tc := range tests {
		got := pushHost(tc.endpoint)
		if got != tc.want {
			t.Errorf("pushHost(%q) = %q, want %q", tc.endpoint, got, tc.want)
		}
		if strings.Contains(got, "CAPABILITY") {
			t.Errorf("pushHost(%q) = %q leaks the endpoint", tc.endpoint, got)
		}
	}
}
