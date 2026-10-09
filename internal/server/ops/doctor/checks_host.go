package doctor

import (
	"context"
	"net/netip"
	"path"

	"golang.org/x/mod/semver"
)

// The checks of the host and of the server's own state: udp_buffers, container, lan_privacy, transfer, release,
// nofile and bandwidth (04 §13.2).

// routeProbe is TEST-NET-1: asking the kernel for the route to it names the default route's interface, and no
// packet is sent (04 §7.4 step 2).
var routeProbe = netip.MustParseAddr("192.0.2.1")

// localV4 returns the IPv4 address of the default route's interface: the running server's, or what the kernel
// answers now. It is the zero Addr when there is none.
func (r *run) localV4(ctx context.Context) netip.Addr {
	if r.live != nil {
		a, _ := netip.ParseAddr(r.live.LocalIPv4)
		return a
	}
	a, err := r.env.Interfaces.RouteSource(ctx, routeProbe)
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap()
}

// The sysctl files that set the socket buffer limits: install.sh writes the first, the packages ship the second
// (06 §4.6).
var sysctlFiles = []string{"etc/sysctl.d/60-isshoni.conf", "usr/lib/sysctl.d/60-isshoni.conf"}

// checkUDPBuffers compares the kernel's limits for socket buffers (Linux: net.core.rmem_max and wmem_max) and the
// buffers the running server's media sockets really got with network.udp_buffer_bytes. Buffers that are too small
// drop packets under a burst of video.
func checkUDPBuffers(_ context.Context, r *run) result {
	if r.cfg.Listen.ICEUDP == "" {
		return okResult(codeUDPBuffersUDPOff, nil)
	}
	want := int64(r.cfg.Network.UDPBufferBytes)
	p := params{"want": want}

	var rcv, snd int64
	if r.live != nil {
		rcv, snd = int64(r.live.UDPRcvBufBytes), int64(r.live.UDPSndBufBytes)
	}
	haveSockets := rcv > 0 && snd > 0
	if haveSockets {
		p["rcvBuf"], p["sndBuf"] = rcv, snd
	}
	var rmem, wmem int64
	haveSysctl := false
	if r.env.GOOS == "linux" {
		rm, ok1 := r.readInt("proc/sys/net/core/rmem_max")
		wm, ok2 := r.readInt("proc/sys/net/core/wmem_max")
		if ok1 && ok2 {
			rmem, wmem, haveSysctl = rm, wm, true
			p["rmemMax"], p["wmemMax"] = rm, wm
		}
	}
	switch {
	case !haveSockets && !haveSysctl && r.env.GOOS != "linux":
		return skipResult(codeSkipUnsupported, params{"os": r.env.GOOS})
	case !haveSockets && !haveSysctl:
		return skipResult(codeUDPBuffersUnknown, nil)
	case haveSysctl && (rmem < want || wmem < want):
		// With isshoni's sysctl file in place the values only have to be loaded; the host's file is not visible
		// from inside a container.
		for _, f := range sysctlFiles {
			if !r.inContainer() && r.exists(f) {
				return warnResult(codeUDPBuffersLow, p, fixUDPBuffersSysctlReload)
			}
		}
		return warnResult(codeUDPBuffersLow, p, fixUDPBuffersSysctl)
	case haveSockets && (rcv < want || snd < want):
		if haveSysctl {
			// The limits are high enough now, but the sockets were opened under lower ones.
			return warnResult(codeUDPBuffersLow, p, fixUDPBuffersRestart)
		}
		if r.env.GOOS == "linux" {
			return warnResult(codeUDPBuffersLow, p, fixUDPBuffersSysctl)
		}
		return warnResult(codeUDPBuffersLow, p, "")
	}
	return okResult(codeUDPBuffersOK, p)
}

// The network modes of a container, as far as doctor can tell them apart.
const (
	networkBridge  = "bridge"  // its own network namespace behind a bridge: ports are published
	networkHost    = "host"    // the host's network namespace
	networkUnknown = "unknown" // the interface files could not be read
)

// containerNetwork tells a container's network mode from the default route's interface: in a network namespace of
// its own that interface is one end of a veth pair, whose link index (iflink) is the other end's and so differs
// from its own (ifindex). On the host's network the interface is a real one and the two are equal.
func (r *run) containerNetwork(ctx context.Context) string {
	name := r.ifaceOf(r.localV4(ctx))
	if name == "" {
		return networkUnknown
	}
	index, ok1 := r.readInt(path.Join("sys/class/net", name, "ifindex"))
	link, ok2 := r.readInt(path.Join("sys/class/net", name, "iflink"))
	switch {
	case !ok1 || !ok2:
		return networkUnknown
	case index != link:
		return networkBridge
	}
	return networkHost
}

// checkContainer reports the container runtime and its network mode, and reminds of the rule of 04 §7.1: published
// ports must keep their numbers, because the server advertises the container's ports in its ICE candidates.
func checkContainer(ctx context.Context, r *run) result {
	if !r.inContainer() {
		return okResult(codeContainerNone, nil)
	}
	p := params{"kind": string(r.host.Container), "network": r.containerNetwork(ctx)}
	tcp, udp := r.publicPorts()
	if len(tcp) > 0 {
		p["tcpPorts"] = tcp
	}
	if len(udp) > 0 {
		p["udpPorts"] = udp
	}
	return infoResult(codeContainerDetected, p)
}

// checkLANPrivacy is a note for macOS hosts (development, or a Mac as a home server): a browser there needs the
// Local Network permission before it reaches a server on the LAN, so its LAN ICE candidates fail without it
// (S4 finding 6, 04 §7.5).
func checkLANPrivacy(_ context.Context, r *run) result {
	if r.env.GOOS == "darwin" {
		return infoResult(codeLANPrivacyMacOS, nil)
	}
	return okResult(codeLANPrivacyNotMacOS, nil)
}

// Thresholds of the transfer check, in percent of limits.transfer_alert_gb (04 §11.3).
const (
	transferWarnPct = 80
	transferFullPct = 100
)

// checkTransfer compares the month's egress with the alert limit. It never fails: the limit is a reminder of the
// provider's allowance, not a block (04 §11.3).
func checkTransfer(_ context.Context, r *run) result {
	if r.offline() {
		return skipResult(codeSkipNeedsServer, nil)
	}
	t := r.live.Transfer
	if t == nil {
		return skipResult(codeSkipNotReported, nil)
	}
	p := params{"month": t.Month, "egressBytes": t.EgressBytes, "ingressBytes": t.IngressBytes}
	if t.AlertGB <= 0 {
		return okResult(codeTransferNoLimit, p)
	}
	limit := float64(t.AlertGB) * 1e9
	pct := int(float64(t.EgressBytes) * 100 / limit)
	p["alertGb"], p["pct"] = t.AlertGB, pct
	if t.ProjectedEgressBytes > 0 {
		p["projectedEgressBytes"] = t.ProjectedEgressBytes
	}
	switch {
	case pct >= transferFullPct:
		return warnResult(codeTransferOver, p, fixTransferReview)
	case pct >= transferWarnPct:
		return warnResult(codeTransferHigh, p, fixTransferReview)
	}
	return okResult(codeTransferOK, p)
}

// checkRelease compares the running version with the latest release the server's daily check found (04 §11.5).
// Before the first check, and while the check is switched off, there is nothing to compare.
func checkRelease(_ context.Context, r *run) result {
	if r.offline() {
		return skipResult(codeSkipNeedsServer, nil)
	}
	p := params{"version": r.live.Version}
	u := r.live.Update
	if u == nil || u.Latest == "" {
		return okResult(codeReleaseNotChecked, p)
	}
	p["latest"] = u.Latest
	if u.URL != "" {
		p["url"] = u.URL
	}
	switch {
	case semver.Compare("v"+u.Latest, "v"+r.live.Version) <= 0:
		return okResult(codeReleaseOK, p)
	case u.Security:
		return warnResult(codeReleaseSecurityUpdate, p, fixReleaseUpgrade)
	}
	return warnResult(codeReleaseUpdate, p, fixReleaseUpgrade)
}

// nofileWant is the open-file limit below which nofile warns: every viewer costs a few descriptors, and so does
// every ICE-TCP connection.
const nofileWant = 8192

// checkNofile reports the process's limit of open files: the server's own when it runs, the caller's offline.
func checkNofile(_ context.Context, r *run) result {
	limit, err := r.env.NoFile()
	if err != nil {
		return skipResult(codeSkipUnsupported, params{"os": r.env.GOOS})
	}
	p := params{"limit": int64(min(limit, 1<<62)), "want": nofileWant, "offline": r.offline()} //nolint:gosec // G115: bounded just here
	if limit < nofileWant {
		return warnResult(codeNofileLow, p, fixNofileLimit)
	}
	return infoResult(codeNofileOK, p)
}

// nicWarnShare is the part of the network interface's speed above which the estimate warns.
const nicWarnShare = 0.8

// checkBandwidth prints the calculator's estimate for the session of the bandwidth flags (04 §13.4) and compares
// it with the speed of the default route's interface, where the kernel reports one (/sys/class/net/<if>/speed;
// virtual interfaces often report none).
func checkBandwidth(ctx context.Context, r *run) result {
	est := Bandwidth(r.in)
	p := params{
		"people": est.Input.People, "sharing": est.Input.Sharing, "hours": est.Input.Hours,
		"egressMediaMbps": est.EgressMediaMbps, "egressWireMbps": est.EgressWireMbps,
		"transferPerSessionGb": est.TransferPerSessionGB,
	}
	if name := r.ifaceOf(r.localV4(ctx)); name != "" {
		if speed, ok := r.readInt(path.Join("sys/class/net", name, "speed")); ok && speed > 0 {
			p["iface"], p["nicMbps"] = name, speed
			if est.EgressWireMbps > nicWarnShare*float64(speed) {
				return warnResult(codeBandwidthNearNIC, p, fixBandwidthLink)
			}
		}
	}
	return infoResult(codeBandwidthEstimate, p)
}
