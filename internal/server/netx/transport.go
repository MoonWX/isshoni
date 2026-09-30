package netx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/logging"
	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// TransportOptions configures NewTransport. The wiring fills it from the config (listen.ice_udp, listen.ice_tcp,
// network.*), DetectPublicAddrs and the environment.
type TransportOptions struct {
	UDPAddr           string   // listen.ice_udp; "" = no UDP. A host binds only that address; none binds every kept one
	TCPAddr           string   // listen.ice_tcp; "" = no 7882/tcp
	PortMux           *PortMux // 443 ICE sub-listener; nil in off mode
	MaxICEConnsPerIP  int      // 64 per IPKey (0 means 64); shares PortMux's ICE count when PortMux is set (one count for 443 and 7882)
	ExcludeInterfaces []string // network.exclude_interfaces globs ("docker*", "br-*", …)
	IncludeLoopback   bool     // network.include_loopback (development)
	IPv6              bool     // network.ipv6; the wiring passes false when public_ipv6 = "off"
	UDPBufferBytes    int      // network.udp_buffer_bytes; 0 leaves the OS default and never warns
	Public            PublicAddrs
	InContainer       bool
	CloudProvider     api.CloudProvider // §13.3
	Counter           *TransferCounter
	PacketConns       []net.PacketConn // tests only: use these instead of binding UDPAddr (02's sfutest.FaultConn); the Transport closes them
	Interfaces        InterfaceLister  // nil: SystemInterfaces(); tests pass a fake
	Logger            *slog.Logger
}

// Via values of AdvertisedAddr: the same value set as api.Transport, used everywhere (dashboard, metrics, 02's
// selected-pair label).
const (
	ViaUDP     = string(api.TransportUDP)
	ViaTCP443  = string(api.TransportTCP443)
	ViaTCP7882 = string(api.TransportTCP7882)
)

// AdvertisedAddr is one address the server advertises in its ICE candidates.
type AdvertisedAddr struct {
	Proto string // "udp" | "tcp"
	Addr  netip.AddrPort
	Via   string // "udp" | "tcp443" | "tcp7882"
	LAN   bool   // kept private address (Append mode, §7.5)
}

// Transport holds the ICE sockets and muxes that every PeerConnection shares (04 §7.3): one UDP port and the TCP
// muxes, routed by ICE ufrag. netx builds it once at startup; 02 applies it to its SettingEngines (publish,
// subscribe) and the connection test's probe APIs use its parts.
type Transport struct {
	UDPMux          ice.UDPMux           // *ice.MultiUDPMuxDefault; nil if UDP disabled
	TCPMux          ice.TCPMux           // *ice.MultiTCPMuxDefault over 443 and 7882; nil if neither
	TCPMux443       ice.TCPMux           // the 443 part alone (02's per-transport probe APIs); nil in off mode
	TCPMux7882      ice.TCPMux           // the 7882 part alone; nil if listen.ice_tcp is ""
	NetworkTypes    []webrtc.NetworkType // udp4/udp6/tcp4/tcp6 as available
	RewriteRules    []webrtc.ICEAddressRewriteRule
	InterfaceFilter func(name string) bool
	IPFilter        func(ip net.IP) bool
	IncludeLoopback bool
	Advertised      []AdvertisedAddr // every advertised address with its final address:port after rewrite rules
	RcvBuf, SndBuf  int              // effective socket buffers read back with getsockopt (the smallest over the UDP sockets)

	tcpLn     *iceListener // the 7882/tcp listener; nil if listen.ice_tcp is ""
	closers   []io.Closer  // what Close closes, in order
	closeOnce sync.Once
	closeErr  error
}

// ICE-TCP mux parameters for 443 and 7882 (04 §7.2–§7.3).
const (
	tcpFirstStunBindTimeout = 10 * time.Second
	tcpAliveFromStun        = 30 * time.Second
	tcpReadBufferPackets    = 64
	tcpWriteBufferBytes     = 4 << 20
)

// NewTransport binds the ICE sockets: 7882/udp on every kept local address (or the one given in UDPAddr), each
// counted and wrapped in an ice.UDPMuxDefault and combined in an ice.MultiUDPMuxDefault; the 7882/tcp listener
// with its per-IP limit and byte counter; and the 443 ICE sub-listener of PortMux. Both TCP listeners get an
// ice.TCPMuxDefault, combined in an ice.MultiTCPMuxDefault, so every kept address gets a passive TCP candidate on
// 443 and on 7882. Which addresses are kept and how they are rewritten is the plan of 04 §7.5 (planAddrs).
//
// Interfaces are enumerated once, here; a hot-plugged interface needs a restart. NewTransport is the only code that
// binds 7882: a busy port is a *ListenError. When the UDP buffers read back below UDPBufferBytes it logs one warn
// line naming the sysctl fix.
func NewTransport(ctx context.Context, opts TransportOptions) (_ *Transport, err error) {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	log = log.With("component", "netx")
	pionLog := quietLogger{logx.NewPionLoggerFactory(log).NewLogger("ice")}

	ifFilter, err := interfaceFilter(opts.ExcludeInterfaces)
	if err != nil {
		return nil, err
	}
	lister := opts.Interfaces
	if lister == nil {
		lister = SystemInterfaces()
	}
	ifs, err := lister.Interfaces()
	if err != nil {
		return nil, err
	}
	plan := planAddrs(planInput{
		ifs: ifs, ifFilter: ifFilter, public: opts.Public, includeLoopback: opts.IncludeLoopback,
		ipv6: opts.IPv6, inContainer: opts.InContainer, provider: opts.CloudProvider,
	})

	t := &Transport{
		RewriteRules:    plan.rules,
		InterfaceFilter: ifFilter,
		IncludeLoopback: opts.IncludeLoopback,
	}
	defer func() {
		if err != nil {
			_ = t.Close()
		}
	}()

	udpLocal, err := t.buildUDP(ctx, opts, plan, pionLog, log)
	if err != nil {
		return nil, err
	}
	allowed, tcpPorts, err := t.buildTCP(ctx, opts, plan, pionLog, log)
	if err != nil {
		return nil, err
	}

	allowedSet := map[netip.Addr]bool{}
	for _, a := range allowed {
		allowedSet[a] = true
	}
	t.IPFilter = func(ip net.IP) bool {
		a, ok := netip.AddrFromSlice(ip)
		return ok && allowedSet[a.Unmap()]
	}

	var has [4]bool // udp4, udp6, tcp4, tcp6
	for _, ap := range udpLocal {
		has[boolIndex(ap.Addr().Is6())] = true
	}
	if t.TCPMux != nil {
		for _, a := range allowed {
			has[2+boolIndex(a.Is6())] = true
		}
	}
	for i, nt := range []webrtc.NetworkType{
		webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6, webrtc.NetworkTypeTCP4, webrtc.NetworkTypeTCP6,
	} {
		if has[i] {
			t.NetworkTypes = append(t.NetworkTypes, nt)
		}
	}
	if len(t.NetworkTypes) == 0 {
		return nil, fmt.Errorf("%w: no UDP socket and no usable address for ICE-TCP", ErrNoTransport)
	}

	for _, ap := range udpLocal {
		for _, a := range advertise(t.RewriteRules, ap.Addr()) {
			t.Advertised = append(t.Advertised, AdvertisedAddr{
				Proto: "udp", Addr: netip.AddrPortFrom(a, ap.Port()), Via: ViaUDP, LAN: isLANAddr(a),
			})
		}
	}
	for _, tp := range tcpPorts {
		for _, local := range allowed {
			for _, a := range advertise(t.RewriteRules, local) {
				t.Advertised = append(t.Advertised, AdvertisedAddr{
					Proto: "tcp", Addr: netip.AddrPortFrom(a, tp.port), Via: tp.via, LAN: isLANAddr(a),
				})
			}
		}
	}
	return t, nil
}

func boolIndex(b bool) int {
	if b {
		return 1
	}
	return 0
}

// buildUDP binds (or takes) the UDP sockets and builds UDPMux. It returns each socket's local address.
func (t *Transport) buildUDP(ctx context.Context, opts TransportOptions, plan addrPlan, pionLog logging.LeveledLogger,
	log *slog.Logger,
) ([]netip.AddrPort, error) {
	conns := opts.PacketConns
	if len(conns) == 0 {
		if opts.UDPAddr == "" {
			return nil, nil
		}
		host, port, err := splitListenAddr("listen.ice_udp", opts.UDPAddr)
		if err != nil {
			return nil, err
		}
		addrs := plan.keep
		if host.IsValid() {
			addrs = []netip.Addr{host}
		}
		if len(addrs) == 0 {
			return nil, fmt.Errorf("%w: no usable local address for ICE UDP on %q (loopback needs "+
				"network.include_loopback)", ErrNoTransport, opts.UDPAddr)
		}
		conns, err = t.bindUDP(ctx, addrs, port, opts.UDPBufferBytes, log)
		if err != nil {
			return nil, err
		}
	}

	local := make([]netip.AddrPort, 0, len(conns))
	muxes := make([]ice.UDPMux, 0, len(conns))
	for i, c := range conns {
		ua, ok := c.LocalAddr().(*net.UDPAddr)
		if !ok {
			for _, rest := range conns[i:] {
				_ = rest.Close()
			}
			return nil, fmt.Errorf("netx: UDP socket address %v is not a *net.UDPAddr", c.LocalAddr())
		}
		ap := ua.AddrPort()
		local = append(local, netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()))
		mux := ice.NewUDPMuxDefault(ice.UDPMuxParams{Logger: pionLog, UDPConn: newCountingPacketConn(c, opts.Counter)})
		muxes = append(muxes, mux)
		t.closers = append(t.closers, mux) // closes the socket too
	}
	t.UDPMux = ice.NewMultiUDPMuxDefault(muxes...)
	return local, nil
}

// bindUDP binds one UDP socket per address on port. Port 0 (tests) binds the first address on an ephemeral port
// and reuses that port for the others. Buffers are requested with want and read back.
func (t *Transport) bindUDP(ctx context.Context, addrs []netip.Addr, port uint16, want int, log *slog.Logger,
) ([]net.PacketConn, error) {
	var lc net.ListenConfig
	conns := make([]net.PacketConn, 0, len(addrs))
	fail := func(err error) ([]net.PacketConn, error) {
		for _, c := range conns {
			_ = c.Close()
		}
		return nil, err
	}
	rcvMin, sndMin, known := 0, 0, false
	for _, a := range addrs {
		network := "udp4"
		if a.Is6() {
			network = "udp6"
		}
		addr := netip.AddrPortFrom(a, port).String()
		pc, err := lc.ListenPacket(ctx, network, addr)
		if err != nil {
			return fail(&ListenError{Proto: "udp", Addr: addr, Err: err})
		}
		conns = append(conns, pc)
		uc, ok := pc.(*net.UDPConn)
		if !ok {
			continue
		}
		if ua, ok := uc.LocalAddr().(*net.UDPAddr); ok && port == 0 {
			port = ua.AddrPort().Port()
		}
		if want > 0 {
			_ = uc.SetReadBuffer(want) // the kernel caps it at net.core.rmem_max; read back below
			_ = uc.SetWriteBuffer(want)
		}
		if rcv, snd, err := readSockBufs(uc); err == nil {
			if !known || rcv < rcvMin {
				rcvMin = rcv
			}
			if !known || snd < sndMin {
				sndMin = snd
			}
			known = true
		}
	}
	t.RcvBuf, t.SndBuf = rcvMin, sndMin
	if want > 0 && known && (rcvMin < want || sndMin < want) {
		log.Warn("UDP socket buffers are smaller than network.udp_buffer_bytes; media may drop packets under load",
			"rcvbuf", rcvMin, "sndbuf", sndMin, "want", want,
			"fix", fmt.Sprintf("Linux: raise the sysctls net.core.rmem_max and net.core.wmem_max to %d (isshoni doctor shows the command)", want))
	}
	return conns, nil
}

// tcpPort is one ICE-TCP listener's advertised port and label.
type tcpPort struct {
	port uint16
	via  string
}

// buildTCP builds the 443 and 7882 TCP muxes. It returns the local addresses pion may gather TCP candidates on
// (IPFilter) and the listeners' ports in TCPMux order.
func (t *Transport) buildTCP(ctx context.Context, opts TransportOptions, plan addrPlan, pionLog logging.LeveledLogger,
	log *slog.Logger,
) ([]netip.Addr, []tcpPort, error) {
	var limiter *connLimiter
	if opts.PortMux != nil {
		limiter = opts.PortMux.iceLimiter()
	}
	if limiter == nil {
		limiter = newConnLimiter(opts.MaxICEConnsPerIP)
	}

	var (
		muxes    []ice.TCPMux
		ports    []tcpPort
		hosts    []netip.Addr // listeners bound to one address
		wildcard bool         // a listener bound to every address
	)
	addListener := func(ln net.Listener, via string) error {
		ta, ok := ln.Addr().(*net.TCPAddr)
		if !ok {
			return fmt.Errorf("netx: ICE-TCP listener address %v is not a *net.TCPAddr", ln.Addr())
		}
		ap := ta.AddrPort()
		if a := ap.Addr().Unmap(); a.IsUnspecified() {
			wildcard = true
		} else if !slices.Contains(hosts, a) {
			hosts = append(hosts, a)
		}
		mux := ice.NewTCPMuxDefault(ice.TCPMuxParams{
			Listener:                     ln,
			Logger:                       pionLog,
			ReadBufferSize:               tcpReadBufferPackets,
			WriteBufferSize:              tcpWriteBufferBytes,
			FirstStunBindTimeout:         tcpFirstStunBindTimeout,
			AliveDurationForConnFromStun: tcpAliveFromStun,
		})
		t.closers = append(t.closers, mux) // closes the listener too
		muxes = append(muxes, mux)
		ports = append(ports, tcpPort{port: ap.Port(), via: via})
		if via == ViaTCP443 {
			t.TCPMux443 = mux
		} else {
			t.TCPMux7882 = mux
		}
		return nil
	}

	if opts.PortMux != nil {
		ln := opts.PortMux.ICE()
		if ln == nil {
			return nil, nil, errors.New("netx: PortMux has no ICE listener")
		}
		if err := addListener(ln, ViaTCP443); err != nil {
			return nil, nil, err
		}
	}
	if opts.TCPAddr != "" {
		if _, _, err := splitListenAddr("listen.ice_tcp", opts.TCPAddr); err != nil {
			return nil, nil, err
		}
		var lc net.ListenConfig
		raw, err := lc.Listen(ctx, "tcp", opts.TCPAddr)
		if err != nil {
			return nil, nil, &ListenError{Proto: "tcp", Addr: opts.TCPAddr, Err: err}
		}
		t.tcpLn = newICEListener(raw, limiter, opts.Counter, log)
		t.closers = append(t.closers, t.tcpLn) // for a failure before the mux owns it; Close is idempotent
		if err := addListener(t.tcpLn, ViaTCP7882); err != nil {
			return nil, nil, err
		}
	}
	if len(muxes) == 0 {
		return nil, nil, nil
	}
	t.TCPMux = ice.NewMultiTCPMuxDefault(muxes...)

	var allowed []netip.Addr
	if wildcard {
		allowed = append(allowed, plan.keep...)
	}
	for _, h := range hosts {
		if !slices.Contains(allowed, h) {
			allowed = append(allowed, h)
		}
	}
	// pion never gathers on loopback without include_loopback, nor IPv6 without network.ipv6.
	allowed = slices.DeleteFunc(allowed, func(a netip.Addr) bool {
		return (a.IsLoopback() && !opts.IncludeLoopback) || (a.Is6() && !opts.IPv6)
	})
	return allowed, ports, nil
}

// splitListenAddr parses a listen address "host:port" whose host is empty, an IP literal or an unspecified
// address. It returns the host (zero for every address) and the port.
func splitListenAddr(key, v string) (netip.Addr, uint16, error) {
	h, p, err := net.SplitHostPort(v)
	if err != nil {
		return netip.Addr{}, 0, fmt.Errorf("netx: %s = %q: %w", key, v, err)
	}
	port, err := strconv.ParseUint(p, 10, 16)
	if err != nil {
		return netip.Addr{}, 0, fmt.Errorf("netx: %s = %q: bad port", key, v)
	}
	if h == "" {
		return netip.Addr{}, uint16(port), nil
	}
	a, err := netip.ParseAddr(h)
	if err != nil {
		return netip.Addr{}, 0, fmt.Errorf("netx: %s = %q: the host must be an IP address", key, v)
	}
	if a.IsUnspecified() {
		return netip.Addr{}, uint16(port), nil
	}
	return a.Unmap(), uint16(port), nil
}

// Apply configures a SettingEngine: SetICEUDPMux, SetICETCPMux, SetNetworkTypes, SetInterfaceFilter,
// SetIPFilter, SetICEAddressRewriteRules, SetIncludeLoopbackCandidate, and mDNS disabled.
// 02 calls it for each webrtc.API it builds, then adds its own settings (DTLS timeout, ICE timeouts…).
// Apply only calls SettingEngine setters and keeps no other state, so a later setter call overrides it
// (02's probe APIs rely on this).
func (t *Transport) Apply(se *webrtc.SettingEngine) error {
	se.SetICEUDPMux(t.UDPMux)
	se.SetICETCPMux(t.TCPMux)
	se.SetNetworkTypes(slices.Clone(t.NetworkTypes))
	se.SetInterfaceFilter(t.InterfaceFilter)
	se.SetIPFilter(t.IPFilter)
	if err := se.SetICEAddressRewriteRules(t.RewriteRules...); err != nil {
		return fmt.Errorf("netx: address rewrite rules: %w", err)
	}
	se.SetIncludeLoopbackCandidate(t.IncludeLoopback)
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	return nil
}

// Close closes the muxes and with them every socket and listener, including PortMux's ICE sub-listener (whose
// Close is idempotent, so PortMux.Close may close it again). It waits for the TCP muxes' goroutines. Later calls
// return the first result.
func (t *Transport) Close() error {
	t.closeOnce.Do(func() {
		var errs []error
		for _, c := range t.closers {
			if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				errs = append(errs, err)
			}
		}
		t.closeErr = errors.Join(errs...)
	})
	return t.closeErr
}

// quietLogger is the pion logger of the muxes. Their per-connection and per-packet warnings ("Error reading first
// packet from <addr>", "Failed to handle decode ICE from <addr>") come from port scanners and stray packets and
// carry the remote address, so they are debug lines here; errors keep their level.
type quietLogger struct{ logging.LeveledLogger }

func (q quietLogger) Warn(msg string)                  { q.Debug(msg) }
func (q quietLogger) Warnf(format string, args ...any) { q.Debugf(format, args...) }
