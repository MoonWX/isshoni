package netx

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// Method says how a public address was found.
type Method string

const (
	MethodConfig    Method = "config"    // public_ip / public_ipv6 literal
	MethodInterface Method = "interface" // the address is on a local interface
	MethodSTUN      Method = "stun"      // the address STUN servers saw (the host is behind NAT)
	MethodNone      Method = "none"      // no address
)

// NAT kinds are api.NATKind (internal/protocol/api/conntest.go, 04 §7.7):
// "none" | "one_to_one" | "port_forward" | "symmetric" | "cgnat_likely" | "unknown".

// PublicAddrs is the result of DetectPublicAddrs: the addresses friends reach the server on, and how the server's
// IPv4 path looks from outside.
type PublicAddrs struct {
	V4, V6     netip.Addr // zero if none
	V4Method   Method
	V6Method   Method
	LocalV4    netip.Addr // IPv4 of the default-route interface (may equal V4)
	NAT        api.NATKind
	STUNMapped []netip.AddrPort // one entry per STUN server, in DetectOptions.STUNServers order; zero when it didn't answer
	DetectedAt time.Time
}

// DetectOptions configures DetectPublicAddrs. The wiring fills it from the config (public_ip, public_ipv6,
// network.stun_servers, network.ipv6) and the environment; tests and doctor inject STUN and Interfaces.
type DetectOptions struct {
	PublicIP, PublicIPv6 string        // config values: "auto" (or ""), "off" or literals
	STUNServers          []string      // "host:port"; empty turns STUN off (then set public_ip to a literal)
	Timeout              time.Duration // per server and attempt; 0 means 2 s. The whole detection takes at most 5 s
	IPv6                 bool          // network.ipv6
	STUN                 STUNClient    // nil: NewSTUNClient(nil), pion/stun over an ephemeral UDP socket
	Interfaces           InterfaceLister

	// CloudProvider and InContainer tell 1:1 NAT (a cloud provider or a container bridge) from a home router's port
	// forwarding when STUN sees another address than the interfaces hold (04 §7.4). The wiring passes
	// DetectCloudProvider's result and the container detection of 04 §5.1, the same values as TransportOptions.
	CloudProvider api.CloudProvider
	InContainer   bool

	// listenSTUN opens the ephemeral socket the STUN queries share; nil binds udp4 0.0.0.0:0. Tests replace it.
	listenSTUN func(ctx context.Context) (net.PacketConn, error)
}

// Detection limits (04 §7.8).
const (
	DefaultSTUNTimeout = 2 * time.Second // per server and attempt
	stunAttempts       = 2               // one retry
	detectBudget       = 5 * time.Second // the whole detection
)

type literalMode int

const (
	modeAuto literalMode = iota
	modeOff
	modeLiteral
)

// parsePublicIP reads a public_ip / public_ipv6 value: "auto" or "", "off", or an IP literal.
func parsePublicIP(key, v string) (literalMode, netip.Addr, error) {
	switch v {
	case "", "auto":
		return modeAuto, netip.Addr{}, nil
	case "off":
		return modeOff, netip.Addr{}, nil
	}
	a, err := netip.ParseAddr(v)
	if err != nil || a.Zone() != "" {
		return 0, netip.Addr{}, fmt.Errorf("netx: %s = %q is not \"auto\", \"off\" or an IP address", key, v)
	}
	return modeLiteral, a.Unmap(), nil
}

// DetectPublicAddrs finds the server's public IPv4 and IPv6 addresses and classifies its IPv4 NAT (04 §7.4):
//
//  1. a public_ip literal is used as is (MethodConfig); STUN still runs for the NAT report;
//  2. LocalV4 is the source address the kernel picks for 192.0.2.1:9;
//  3. every STUN server is queried in parallel from one ephemeral UDP socket (Timeout per attempt, one retry);
//  4. the observations are classified: a STUN address that is on an interface → NAT none; LocalV4 in
//     100.64.0.0/10 on a non-Tailscale interface → cgnat_likely; servers that disagree → symmetric; servers that
//     agree on another address → one_to_one with a cloud provider or a container, else port_forward; no STUN
//     answer but a public IPv4 on an interface → that address, NAT unknown; nothing → no V4, NAT unknown.
//
// IPv6 comes from public_ipv6 (or an IPv6 public_ip literal), else from the interfaces: the global address the
// kernel routes from, or the first global one (ULA, link-local and temporary addresses are skipped).
//
// STUN failures are not errors: they show in NAT and STUNMapped. The error is non-nil only for a malformed
// literal or when the interfaces can't be listed; the result then holds what was found. The whole call returns
// within 5 s.
func DetectPublicAddrs(ctx context.Context, opts DetectOptions) (PublicAddrs, error) {
	res := PublicAddrs{V4Method: MethodNone, V6Method: MethodNone, NAT: api.NATKindUnknown}
	v4mode, v4lit, err := parsePublicIP("public_ip", opts.PublicIP)
	if err != nil {
		return res, err
	}
	v6mode, v6lit, err := parsePublicIP("public_ipv6", opts.PublicIPv6)
	if err != nil {
		return res, err
	}
	if v6mode == modeLiteral && !v6lit.Is6() {
		return res, fmt.Errorf("netx: public_ipv6 = %q is not an IPv6 address", opts.PublicIPv6)
	}

	ctx, cancel := context.WithTimeout(ctx, detectBudget)
	defer cancel()
	lister := opts.Interfaces
	if lister == nil {
		lister = SystemInterfaces()
	}
	ifs, err := lister.Interfaces()
	if err != nil {
		return res, err
	}
	local := localAddrs(ifs)
	if a, err := lister.RouteSource(ctx, v4Probe); err == nil && a.Unmap().Is4() {
		res.LocalV4 = a.Unmap()
	}

	if v4mode != modeOff && len(opts.STUNServers) > 0 {
		res.STUNMapped = queryAll(ctx, opts)
	}
	classifyV4(&res, opts, ifs, local, v4mode, v4lit)

	if opts.IPv6 {
		switch {
		case v6mode == modeLiteral:
			res.V6, res.V6Method = v6lit, MethodConfig
		case v4mode == modeLiteral && v4lit.Is6():
			res.V6, res.V6Method = v4lit, MethodConfig
		case v6mode == modeAuto:
			if a := globalV6(ctx, lister, ifs); a.IsValid() {
				res.V6, res.V6Method = a, MethodInterface
			}
		}
	}
	res.DetectedAt = time.Now()
	return res, nil
}

// classifyV4 fills V4, V4Method and NAT from the STUN answers, LocalV4 and the interfaces (the table of 04 §7.4).
func classifyV4(res *PublicAddrs, opts DetectOptions, ifs []Interface, local map[netip.Addr]bool,
	mode literalMode, literal netip.Addr,
) {
	if mode == modeOff {
		return
	}
	var answers []netip.AddrPort
	for _, a := range res.STUNMapped {
		if a.IsValid() {
			answers = append(answers, a)
		}
	}
	agree := true
	for _, a := range answers[min(1, len(answers)):] {
		if a != answers[0] {
			agree = false
		}
	}
	var onIface netip.Addr // a STUN address that is on a local interface
	for _, a := range answers {
		if local[a.Addr()] && !a.Addr().IsLoopback() {
			onIface = a.Addr()
			break
		}
	}
	cgnat := res.LocalV4.IsValid() && cgnatPrefix.Contains(res.LocalV4) && !isTailscale(ifaceOf(ifs, res.LocalV4))
	cloudOrContainer := opts.InContainer || (opts.CloudProvider != "" && opts.CloudProvider != api.CloudProviderUnknown)

	var v4 netip.Addr
	method := MethodNone
	switch {
	case onIface.IsValid():
		v4, method, res.NAT = onIface, MethodInterface, api.NATKindNone
	case cgnat:
		res.NAT = api.NATKindCGNATLikely
		if len(answers) > 0 {
			v4, method = answers[0].Addr(), MethodSTUN
		}
	case len(answers) > 0 && !agree:
		v4, method, res.NAT = answers[0].Addr(), MethodSTUN, api.NATKindSymmetric
	case len(answers) > 0 && cloudOrContainer:
		v4, method, res.NAT = answers[0].Addr(), MethodSTUN, api.NATKindOneToOne
	case len(answers) > 0:
		v4, method, res.NAT = answers[0].Addr(), MethodSTUN, api.NATKindPortForward
	default:
		res.NAT = api.NATKindUnknown
		if a := publicV4OnIface(ifs, res.LocalV4); a.IsValid() {
			v4, method = a, MethodInterface
		}
	}
	if mode == modeLiteral {
		if !literal.Is4() {
			return // an IPv6 public_ip: no public IPv4; the NAT report above still stands
		}
		v4, method = literal, MethodConfig
		if len(answers) == 0 && local[literal] {
			res.NAT = api.NATKindNone
		}
	}
	res.V4, res.V4Method = v4, method
}

// queryAll asks every STUN server in parallel from one ephemeral socket and returns the mapped IPv4 addresses in
// server order (zero for no answer).
func queryAll(ctx context.Context, opts DetectOptions) []netip.AddrPort {
	out := make([]netip.AddrPort, len(opts.STUNServers))
	listen := opts.listenSTUN
	if listen == nil {
		listen = func(ctx context.Context) (net.PacketConn, error) {
			var lc net.ListenConfig
			return lc.ListenPacket(ctx, "udp4", "0.0.0.0:0")
		}
	}
	sock, err := listen(ctx)
	if err != nil {
		return out
	}
	defer func() { _ = sock.Close() }()
	client := opts.STUN
	if client == nil {
		client = NewSTUNClient(nil)
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultSTUNTimeout
	}
	var wg sync.WaitGroup
	for i, server := range opts.STUNServers {
		wg.Go(func() {
			for range stunAttempts {
				actx, cancel := context.WithTimeout(ctx, timeout)
				a, err := client.Mapped(actx, sock, server)
				cancel()
				if err == nil && a.Addr().Unmap().Is4() && a.Port() != 0 {
					out[i] = netip.AddrPortFrom(a.Addr().Unmap(), a.Port())
					return
				}
				if ctx.Err() != nil {
					return
				}
			}
		})
	}
	wg.Wait()
	return out
}

// localAddrs returns the usable addresses of every up interface (loopback included).
func localAddrs(ifs []Interface) map[netip.Addr]bool {
	m := map[netip.Addr]bool{}
	for _, it := range ifs {
		if !it.Up() {
			continue
		}
		for _, ia := range it.Addrs {
			if usableIfaceAddr(ia) {
				m[ia.Addr.Unmap()] = true
			}
		}
	}
	return m
}

// publicV4OnIface returns a public IPv4 of an up interface: LocalV4 when it is public, else the first one.
func publicV4OnIface(ifs []Interface, localV4 netip.Addr) netip.Addr {
	if isPublicV4(localV4) && localAddrs(ifs)[localV4] {
		return localV4
	}
	for _, it := range ifs {
		if !it.Up() || it.Loopback() {
			continue
		}
		for _, ia := range it.Addrs {
			if usableIfaceAddr(ia) && isPublicV4(ia.Addr) {
				return ia.Addr.Unmap()
			}
		}
	}
	return netip.Addr{}
}

// globalV6 returns the global IPv6 address the kernel routes from when it is a usable interface address, else the
// first usable global IPv6 of an up interface.
func globalV6(ctx context.Context, lister InterfaceLister, ifs []Interface) netip.Addr {
	usable := map[netip.Addr]bool{}
	var first netip.Addr
	for _, it := range ifs {
		if !it.Up() || it.Loopback() {
			continue
		}
		for _, ia := range it.Addrs {
			if usableIfaceAddr(ia) && isGlobalV6(ia.Addr) {
				usable[ia.Addr] = true
				if !first.IsValid() {
					first = ia.Addr
				}
			}
		}
	}
	if a, err := lister.RouteSource(ctx, v6Probe); err == nil && usable[a] {
		return a
	}
	return first
}
