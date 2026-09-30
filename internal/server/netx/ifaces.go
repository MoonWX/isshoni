package netx

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path"
	"strconv"
	"strings"
)

// InterfaceLister is the host's view of its network interfaces. SystemInterfaces is the real one; tests and
// doctor pass fakes, so address selection and public-IP detection run without the host's network.
type InterfaceLister interface {
	// Interfaces returns every interface with its addresses, in the system's order.
	Interfaces() ([]Interface, error)
	// RouteSource returns the local address the kernel picks for packets to dst: a UDP "connect", no packet is
	// sent (04 §7.4 step 2).
	RouteSource(ctx context.Context, dst netip.Addr) (netip.Addr, error)
}

// Interface is one network interface.
type Interface struct {
	Name  string
	Flags net.Flags // net.FlagUp, net.FlagLoopback, …
	Addrs []InterfaceAddr
}

// InterfaceAddr is one address of an interface.
type InterfaceAddr struct {
	Addr netip.Addr // unmapped; IPv6 link-local addresses carry the interface name as zone
	// Temporary marks an IPv6 address that must not carry media: a temporary privacy address (RFC 8981), or one
	// that is deprecated, tentative or failed duplicate address detection. Only Linux reports it (from
	// /proc/net/if_inet6); elsewhere it is always false.
	Temporary bool
}

// Up reports whether the interface is up.
func (i Interface) Up() bool { return i.Flags&net.FlagUp != 0 }

// Loopback reports whether the interface is a loopback interface.
func (i Interface) Loopback() bool { return i.Flags&net.FlagLoopback != 0 }

// SystemInterfaces returns the InterfaceLister of this host.
func SystemInterfaces() InterfaceLister { return systemInterfaces{} }

type systemInterfaces struct{}

// procIfInet6 lists Linux's IPv6 addresses with their flags.
const procIfInet6 = "/proc/net/if_inet6"

func (systemInterfaces) Interfaces() ([]Interface, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("netx: list interfaces: %w", err)
	}
	var unusable map[netip.Addr]bool
	if b, err := os.ReadFile(procIfInet6); err == nil { // Linux only; elsewhere no flags are known
		unusable = parseIfInet6(b)
	}
	out := make([]Interface, 0, len(ifs))
	for _, ifi := range ifs {
		addrs, err := ifi.Addrs()
		if err != nil {
			continue // as pion does: an interface whose addresses can't be read is skipped
		}
		it := Interface{Name: ifi.Name, Flags: ifi.Flags}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(ipn.IP)
			if !ok {
				continue
			}
			ip = ip.Unmap()
			if ip.Is6() && (ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()) {
				ip = ip.WithZone(ifi.Name)
			}
			it.Addrs = append(it.Addrs, InterfaceAddr{Addr: ip, Temporary: unusable[ip.WithZone("")]})
		}
		out = append(out, it)
	}
	return out, nil
}

func (systemInterfaces) RouteSource(ctx context.Context, dst netip.Addr) (netip.Addr, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "udp", netip.AddrPortFrom(dst, 9).String())
	if err != nil {
		return netip.Addr{}, fmt.Errorf("netx: route to %s: %w", dst, err)
	}
	defer func() { _ = c.Close() }()
	ua, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok {
		return netip.Addr{}, fmt.Errorf("netx: route to %s: local address %v is not UDP", dst, c.LocalAddr())
	}
	return ua.AddrPort().Addr().Unmap(), nil
}

// Linux IPv6 address flags in /proc/net/if_inet6 (include/uapi/linux/if_addr.h).
const (
	ifaFTemporary  = 0x01 // IFA_F_TEMPORARY (the same bit as IFA_F_SECONDARY)
	ifaFDADFailed  = 0x08
	ifaFDeprecated = 0x20
	ifaFTentative  = 0x40
	ifaFUnusable   = ifaFTemporary | ifaFDADFailed | ifaFDeprecated | ifaFTentative
)

// parseIfInet6 reads /proc/net/if_inet6 ("<32 hex digits> <ifindex> <prefix len> <scope> <flags> <name>" per line)
// and returns the addresses whose flags make them unusable for media. Malformed lines are skipped.
func parseIfInet6(b []byte) map[netip.Addr]bool {
	out := map[netip.Addr]bool{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 6 || len(f[0]) != 32 {
			continue
		}
		raw, err := hex.DecodeString(f[0])
		if err != nil {
			continue
		}
		flags, err := strconv.ParseUint(f[4], 16, 32)
		if err != nil {
			continue
		}
		if flags&ifaFUnusable != 0 {
			out[netip.AddrFrom16([16]byte(raw))] = true
		}
	}
	return out
}

// interfaceFilter returns the filter of network.exclude_interfaces: an interface is kept unless its name matches
// one of the globs (path.Match syntax: "docker*", "br-*"). A malformed glob is an error.
func interfaceFilter(excludes []string) (func(name string) bool, error) {
	for _, g := range excludes {
		if _, err := path.Match(g, ""); err != nil {
			return nil, fmt.Errorf("netx: exclude_interfaces glob %q: %w", g, err)
		}
	}
	globs := append([]string(nil), excludes...)
	return func(name string) bool {
		for _, g := range globs {
			if ok, _ := path.Match(g, name); ok {
				return false
			}
		}
		return true
	}, nil
}

// Address classes (04 §7.4–§7.6).
var (
	cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10") // RFC 6598 shared address space
	v4Probe     = netip.MustParseAddr("192.0.2.1")       // TEST-NET-1: the route-source probe of 04 §7.4
	v6Probe     = netip.MustParseAddr("2001:db8::1")     // IPv6 documentation prefix, same use
)

// isPublicV4 reports a global unicast IPv4 address outside RFC 1918 and CGNAT space.
func isPublicV4(a netip.Addr) bool {
	a = a.Unmap()
	return a.Is4() && a.IsGlobalUnicast() && !a.IsPrivate() && !cgnatPrefix.Contains(a)
}

// isGlobalV6 reports a global unicast IPv6 address: not ULA (fc00::/7), link-local, loopback or IPv4-mapped.
func isGlobalV6(a netip.Addr) bool {
	return a.Is6() && !a.Is4In6() && a.IsGlobalUnicast() && !a.IsPrivate()
}

// isLANAddr reports an address that only a local network can reach: RFC 1918, CGNAT (100.64.0.0/10) or ULA.
func isLANAddr(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsPrivate() || cgnatPrefix.Contains(a)
}

// usableIfaceAddr reports whether an interface address may carry media at all, whatever the plan: no
// unspecified, multicast or link-local address, no IPv6 temporary address, and no IPv4-compatible or site-local
// IPv6 address (pion rejects those too, RFC 8445 §5.1.1.1).
func usableIfaceAddr(ia InterfaceAddr) bool {
	a := ia.Addr.Unmap()
	switch {
	case !a.IsValid(), a.IsUnspecified(), a.IsMulticast(), a.IsLinkLocalUnicast(), a.IsLinkLocalMulticast():
		return false
	case a.Is6() && ia.Temporary:
		return false
	case a.Is6() && !a.IsLoopback():
		b := a.As16()
		if [12]byte(b[:12]) == [12]byte{} {
			return false // IPv4-compatible
		}
		if b[0] == 0xfe && b[1]&0xc0 == 0xc0 {
			return false // site-local
		}
	}
	return true
}

// isTailscale reports a Tailscale interface: tailscale* (Linux) or utun* (macOS, whose Tailscale addresses are
// in 100.64.0.0/10). Its CGNAT address is not a sign of carrier-grade NAT (04 §7.4).
func isTailscale(name string) bool {
	return strings.HasPrefix(name, "tailscale") || strings.HasPrefix(name, "utun")
}

// ifaceOf returns the name of the interface that holds a, or "".
func ifaceOf(ifs []Interface, a netip.Addr) string {
	for _, it := range ifs {
		for _, ia := range it.Addrs {
			if ia.Addr.WithZone("") == a.WithZone("") {
				return it.Name
			}
		}
	}
	return ""
}
