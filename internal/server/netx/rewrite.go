package netx

import (
	"net/netip"
	"slices"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// addrPlan is the address policy of 04 §7.5: which local addresses carry media (UDP sockets are bound to them,
// and the IP filter lets pion gather TCP candidates on them) and the rewrite rules that turn them into the
// advertised addresses. The goal is to advertise exactly the addresses friends can reach.
type addrPlan struct {
	keep  []netip.Addr // in interface order, IPv4 first
	rules []webrtc.ICEAddressRewriteRule
}

// planInput is what the plan depends on. ifs are the host's interfaces, enumerated once at startup.
type planInput struct {
	ifs             []Interface
	ifFilter        func(name string) bool // network.exclude_interfaces
	public          PublicAddrs
	includeLoopback bool
	ipv6            bool
	inContainer     bool
	provider        api.CloudProvider
}

// planAddrs applies 04 §7.5 (plus the "nothing known" fallback that table leaves open):
//
//   - the public IPv4 is on an interface (Hetzner, DigitalOcean, …): no rule; keep that address and the other
//     public IPv4 addresses, so private VPC addresses are not advertised;
//   - the public IPv4 is elsewhere, seen through 1:1 NAT (a cloud provider: AWS, GCP, …) or a container bridge:
//     {External: [V4], Local: LocalV4, AsCandidateType: host, Mode: Replace, Networks: [udp4, tcp4]}; keep LocalV4;
//   - the same without a cloud provider or a container, with a private (RFC 1918 or CGNAT) LocalV4, is a home or
//     LAN server behind a router: the same rule with Mode: Append, so LAN friends connect to LocalV4 directly and
//     remote friends use the forwarded public address;
//   - no public IPv4 is known (or LocalV4 is not usable): no rule; keep every usable IPv4 address, private ones
//     included, so a LAN-only server still works;
//   - IPv6 (network.ipv6): global addresses on interfaces are kept as they are; a public_ipv6 that is on no
//     interface gets {External: [V6], Local: <one local IPv6>, Mode: Replace, Networks: [udp6, tcp6]} on the
//     first global (else ULA) address. The Local pin keeps the rule off ::1 in development.
//
// Loopback addresses are kept only with includeLoopback (development). Link-local, multicast, temporary IPv6 and
// excluded interfaces never carry media.
func planAddrs(in planInput) addrPlan {
	var v4, v6 []netip.Addr
	for _, it := range in.ifs {
		if !it.Up() || (it.Loopback() && !in.includeLoopback) || (in.ifFilter != nil && !in.ifFilter(it.Name)) {
			continue
		}
		for _, ia := range it.Addrs {
			a := ia.Addr.Unmap()
			if !usableIfaceAddr(ia) || (a.IsLoopback() && !in.includeLoopback) {
				continue
			}
			switch {
			case a.Is4() && !slices.Contains(v4, a):
				v4 = append(v4, a)
			case a.Is6() && in.ipv6 && !slices.Contains(v6, a):
				v6 = append(v6, a)
			}
		}
	}

	var p addrPlan
	pub := in.public
	v4Public, localV4 := pub.V4.Unmap(), pub.LocalV4.Unmap()
	switch {
	case v4Public.Is4() && slices.Contains(v4, v4Public):
		for _, a := range v4 {
			if a.IsLoopback() || a == v4Public || isPublicV4(a) {
				p.keep = append(p.keep, a)
			}
		}
	case v4Public.Is4() && localV4.Is4() && slices.Contains(v4, localV4):
		mode := webrtc.ICEAddressRewriteReplace
		cloud := in.provider != "" && in.provider != api.CloudProviderUnknown
		if !in.inContainer && !cloud && isLANAddr(localV4) {
			mode = webrtc.ICEAddressRewriteAppend
		}
		for _, a := range v4 {
			if a.IsLoopback() || a == localV4 {
				p.keep = append(p.keep, a)
			}
		}
		p.rules = append(p.rules, webrtc.ICEAddressRewriteRule{
			External:        []string{v4Public.String()},
			Local:           localV4.String(),
			AsCandidateType: webrtc.ICECandidateTypeHost,
			Mode:            mode,
			Networks:        []webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeTCP4},
		})
	default:
		p.keep = append(p.keep, v4...)
	}

	v6Public := pub.V6
	if v6Public.Is6() && !v6Public.Is4In6() && !slices.Contains(v6, v6Public) {
		var src netip.Addr
		for _, a := range v6 {
			if isGlobalV6(a) {
				src = a
				break
			}
		}
		if !src.IsValid() {
			for _, a := range v6 {
				if a.IsPrivate() {
					src = a
					break
				}
			}
		}
		for _, a := range v6 {
			if a.IsLoopback() || a == src {
				p.keep = append(p.keep, a)
			}
		}
		if src.IsValid() {
			p.rules = append(p.rules, webrtc.ICEAddressRewriteRule{
				External:        []string{v6Public.String()},
				Local:           src.String(),
				AsCandidateType: webrtc.ICECandidateTypeHost,
				Mode:            webrtc.ICEAddressRewriteReplace,
				Networks:        []webrtc.NetworkType{webrtc.NetworkTypeUDP6, webrtc.NetworkTypeTCP6},
			})
		}
	} else {
		for _, a := range v6 {
			if a.IsLoopback() || isGlobalV6(a) {
				p.keep = append(p.keep, a)
			}
		}
	}
	return p
}

// advertise returns the addresses pion advertises for a host candidate on local, after the rules (every rule
// planAddrs builds is pinned to one Local address): Replace gives the external addresses, Append the local one
// and then the external ones.
func advertise(rules []webrtc.ICEAddressRewriteRule, local netip.Addr) []netip.Addr {
	local = local.Unmap()
	for _, r := range rules {
		l, err := netip.ParseAddr(r.Local)
		if err != nil || l.Unmap() != local {
			continue
		}
		var out []netip.Addr
		if r.Mode == webrtc.ICEAddressRewriteAppend {
			out = append(out, local)
		}
		for _, e := range r.External {
			if a, err := netip.ParseAddr(e); err == nil {
				out = append(out, a.Unmap())
			}
		}
		return out
	}
	return []netip.Addr{local}
}
