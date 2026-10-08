package push

import (
	"context"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// The SSRF guard (04 §14.5). A subscription's endpoint comes from a browser, so the server treats it as untrusted:
// without the guard, any signed-in user could make the server POST to a host on its own network (a router, a cloud
// metadata service, another container).
//
// Two checks, because DNS can change between them:
//   - at subscribe time, ValidateEndpoint: the URL rules (https, port 443, no userinfo, a DNS name) and the name
//     must resolve, now, only to public addresses;
//   - at send time, guard.control: the dialer calls it with the address it is about to connect to, after its own
//     DNS lookup, so a name that was public when it was checked and private when it is dialed (DNS rebinding) is
//     refused before a single packet leaves.

// resolveTimeout bounds ValidateEndpoint's DNS lookup.
const resolveTimeout = 5 * time.Second

// pushPort is the only port a push endpoint may use.
const pushPort = 443

// guard decides which addresses the sender may connect to.
type guard struct {
	// own are the server's public addresses (unmapped): an endpoint there would make the server call itself.
	own map[netip.Addr]struct{}
	// allowPrivate turns the address, port and IP-literal rules off. Tests only: the fake push service listens on
	// a loopback address and a random port.
	allowPrivate bool
}

func newGuard(own []netip.Addr, allowPrivate bool) *guard {
	g := &guard{own: make(map[netip.Addr]struct{}, len(own)), allowPrivate: allowPrivate}
	for _, a := range own {
		if a.IsValid() {
			g.own[a.Unmap().WithZone("")] = struct{}{}
		}
	}
	return g
}

// Address blocks that are not public unicast space (the IANA special-purpose registries). IPv6 is checked the
// other way round: only 2000::/3 is global unicast, so loopback, link-local, ULA (fc00::/7), site-local, multicast,
// the discard prefix and the local NAT64 prefix are refused without being listed, and blockedV6 holds the special
// blocks inside 2000::/3.
var (
	blockedV4 = prefixes(
		"0.0.0.0/8",       // "this network", including the unspecified address
		"10.0.0.0/8",      // private (RFC 1918)
		"100.64.0.0/10",   // carrier-grade NAT (RFC 6598)
		"127.0.0.0/8",     // loopback
		"169.254.0.0/16",  // link-local, including the cloud metadata address 169.254.169.254
		"172.16.0.0/12",   // private (RFC 1918)
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // documentation (TEST-NET-1)
		"192.88.99.0/24",  // 6to4 relay anycast (deprecated)
		"192.168.0.0/16",  // private (RFC 1918)
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // documentation (TEST-NET-2)
		"203.0.113.0/24",  // documentation (TEST-NET-3)
		"224.0.0.0/4",     // multicast
		"240.0.0.0/4",     // reserved, including the broadcast address
	)
	globalV6  = netip.MustParsePrefix("2000::/3")
	blockedV6 = prefixes(
		"2001::/23",     // IETF protocol assignments: Teredo, benchmarking, ORCHID
		"2001:db8::/32", // documentation
		"2002::/16",     // 6to4: the address embeds an IPv4 address
		"3fff::/20",     // documentation (RFC 9637)
	)
	// nat64 is the well-known NAT64 prefix (RFC 6052): its last 32 bits are the IPv4 address the packet goes to,
	// so that address decides.
	nat64 = netip.MustParsePrefix("64:ff9b::/96")
)

func prefixes(list ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(list))
	for i, s := range list {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

func inAny(ps []netip.Prefix, a netip.Addr) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// allowed reports whether the sender may connect to a.
func (g *guard) allowed(a netip.Addr) bool { return g.allowPrivate || g.public(a) }

// public reports whether a is a public unicast address that is not one of the server's own. IPv4-mapped IPv6 and
// NAT64 addresses are judged by the IPv4 address inside them.
func (g *guard) public(a netip.Addr) bool {
	if !a.IsValid() || a.Zone() != "" {
		return false
	}
	a = a.Unmap()
	if a.Is6() && nat64.Contains(a) {
		b := a.As16()
		a = netip.AddrFrom4([4]byte(b[12:]))
	}
	if _, own := g.own[a]; own {
		return false
	}
	if a.Is4() {
		return !inAny(blockedV4, a)
	}
	return globalV6.Contains(a) && !inAny(blockedV6, a)
}

// control is the sender's net.Dialer.Control: it runs for every connection attempt, with the IP address and port
// the dialer resolved, before connect. Refusing here checks the address that is really dialed, which also defeats
// DNS rebinding between ValidateEndpoint and the send.
func (g *guard) control(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: %q is not an IP address and port", ErrBlockedAddress, address)
	}
	if g.allowPrivate {
		return nil
	}
	if ap.Port() != pushPort {
		return fmt.Errorf("%w: port %d", ErrBlockedAddress, ap.Port())
	}
	if !g.public(ap.Addr()) {
		return fmt.Errorf("%w: %s", ErrBlockedAddress, ap.Addr())
	}
	return nil
}

// parseEndpoint applies the URL rules of 04 §14.5 to a subscription endpoint: https only, port 443 (explicit or
// implicit), no userinfo, and a host that is a DNS name, never an IP literal in any spelling. It returns the URL,
// or the reason it is refused. With lax (tests only, guard.allowPrivate) any port and an IP literal pass.
func parseEndpoint(raw string, lax bool) (*url.URL, api.PushRejectReason) {
	u, err := url.Parse(raw)
	if err != nil {
		// Not a URL at all. With an https:// prefix the scheme is not the problem: the port is not a number, or
		// the rest can't name a host.
		if len(raw) < 8 || !strings.EqualFold(raw[:8], "https://") {
			return nil, api.PushRejectReasonNotHTTPS
		}
		if hasBadPort(raw[8:]) {
			return nil, api.PushRejectReasonBadPort
		}
		return nil, api.PushRejectReasonUnresolvable
	}
	if u.Scheme != "https" || u.Opaque != "" {
		return nil, api.PushRejectReasonNotHTTPS
	}
	if u.User != nil {
		return nil, api.PushRejectReasonUserinfo
	}
	host := u.Hostname()
	if host == "" {
		return nil, api.PushRejectReasonUnresolvable
	}
	if lax {
		return u, ""
	}
	if port := u.Port(); (port != "" && port != "443") || strings.HasSuffix(u.Host, ":") {
		return nil, api.PushRejectReasonBadPort
	}
	if strings.HasPrefix(u.Host, "[") || endsInNumber(host) {
		return nil, api.PushRejectReasonIPLiteral
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return nil, api.PushRejectReasonIPLiteral
	}
	if !validDNSName(host) {
		return nil, api.PushRejectReasonUnresolvable
	}
	if isLocalhost(host) {
		return nil, api.PushRejectReasonPrivateAddress
	}
	return u, ""
}

// hasBadPort reports whether the authority at the start of rest (a URL without its scheme and "//") ends in a
// port that is not a number: net/url refuses such a URL as a whole, and the port is the reason to give.
func hasBadPort(rest string) bool {
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	rest = rest[strings.LastIndexByte(rest, '@')+1:]
	i := strings.LastIndexByte(rest, ':')
	if i < 0 || strings.HasSuffix(rest, "]") {
		return false
	}
	for _, c := range []byte(rest[i+1:]) {
		if c < '0' || c > '9' {
			return true
		}
	}
	return false
}

// endsInNumber reports whether a host is an IPv4 address in one of the spellings that C resolvers and browsers
// accept besides dotted decimal: "2130706433", "127.1", "0x7f.0.0.1", "0177.0.0.1". The WHATWG URL rule: a host
// whose last label is a number is an IPv4 address. No DNS top-level domain is numeric, so nothing real is refused.
func endsInNumber(host string) bool {
	host = strings.TrimSuffix(host, ".")
	last := host[strings.LastIndexByte(host, '.')+1:]
	if last == "" {
		return false
	}
	if len(last) >= 2 && last[0] == '0' && (last[1] == 'x' || last[1] == 'X') {
		last = last[2:]
		for i := 0; i < len(last); i++ {
			if !isHexDigit(last[i]) {
				return false
			}
		}
		return true
	}
	for i := 0; i < len(last); i++ {
		if last[i] < '0' || last[i] > '9' {
			return false
		}
	}
	return true
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// validDNSName reports whether host is a DNS name in its ASCII form: labels of 1 to 63 letters, digits, hyphens
// and underscores, at most 253 bytes, with an optional trailing dot. Browsers send a push endpoint's host in this
// form (an internationalized name arrives as punycode).
func validDNSName(host string) bool {
	host = strings.TrimSuffix(host, ".")
	if host == "" || len(host) > 253 {
		return false
	}
	for label := range strings.SplitSeq(host, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' {
				return false
			}
		}
	}
	return true
}

// isLocalhost reports the names that always mean this host (RFC 6761), whatever the resolver says.
func isLocalhost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return host == "localhost" || strings.HasSuffix(host, ".localhost")
}

// ValidateEndpoint runs the subscribe-time checks of 04 §14.5 on a subscription endpoint (03's subscribe handler
// calls it after checking the body's shape): https only (not_https), port 443 (bad_port), no userinfo (userinfo),
// a DNS name and not an IP literal (ip_literal), and the name must resolve now (unresolvable), only to public
// addresses that are not this server's (private_address). It returns nil, or an *api.Error with the code
// push_endpoint_rejected and params.reason. The sender checks the dialed address again at send time, because DNS
// can change.
func (s *Service) ValidateEndpoint(ctx context.Context, endpoint string) error {
	u, reason := parseEndpoint(endpoint, s.guard.allowPrivate)
	if reason != "" {
		return endpointRejected(reason)
	}
	host := u.Hostname()
	if a, err := netip.ParseAddr(host); err == nil { // only with allowPrivate (tests): parseEndpoint refuses literals
		if !s.guard.allowed(a) {
			return endpointRejected(api.PushRejectReasonPrivateAddress)
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	addrs, err := s.resolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addrs) == 0 {
		return endpointRejected(api.PushRejectReasonUnresolvable)
	}
	for _, a := range addrs {
		if !s.guard.allowed(a) {
			return endpointRejected(api.PushRejectReasonPrivateAddress)
		}
	}
	return nil
}

func endpointRejected(reason api.PushRejectReason) *api.Error {
	return &api.Error{Code: api.CodePushEndpointRejected, Params: map[string]any{api.ParamReason: reason}}
}

// pushHost is the only part of an endpoint that is ever logged (the push_host attribute): the endpoint itself is a
// capability URL.
func pushHost(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() == "" {
		return "invalid"
	}
	host := strings.ToLower(u.Hostname())
	if len(host) > 253 { // longer than any DNS name: an endpoint that never passed ValidateEndpoint
		host = host[:253]
	}
	return host
}
