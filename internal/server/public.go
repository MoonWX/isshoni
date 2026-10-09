package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/netx"
)

// This file is the server's view of its own network: the public addresses (04 §7.4), found at startup and looked at
// again on a ticker, and the ICE Transport (04 §7.3) that the SFU runs on: its options, its sockets and what the
// server says about them.

// publicRedetectEvery is how often a running server looks at its public addresses again (04 §7.4).
const publicRedetectEvery = 10 * time.Minute

// redetectInterval is publicRedetectEvery, or what a test put in its place.
func (s *Server) redetectInterval() time.Duration {
	if s.redetectEvery > 0 {
		return s.redetectEvery
	}
	return publicRedetectEvery
}

// inContainer reports whether the server runs in a container (04 §5.1). Public-address detection and the Transport
// read it the same way: behind a container bridge the address STUN sees is a 1:1 NAT, not a home router's port
// forwarding (04 §7.4, §7.5).
func (s *Server) inContainer() bool { return s.deps.Host.Container() != config.ContainerNone }

// cloudProvider is the hosting provider by the machine's DMI strings (04 §13.3). Like inContainer it goes to the
// detection and to the Transport alike.
func (s *Server) cloudProvider() api.CloudProvider {
	return netx.DetectCloudProvider(os.DirFS(netx.DMIDir))
}

// detectOptions fills netx's detection options from the config and the environment. withSTUN false leaves the STUN
// servers out: the detection then only looks at the machine's interfaces and sends nothing.
func (s *Server) detectOptions(withSTUN bool) netx.DetectOptions {
	opts := netx.DetectOptions{
		PublicIP:      s.cfg.PublicIP,
		PublicIPv6:    s.cfg.PublicIPv6,
		IPv6:          s.cfg.Network.IPv6,
		CloudProvider: s.cloudProvider(),
		InContainer:   s.inContainer(),
	}
	if withSTUN {
		opts.STUNServers = s.cfg.Network.STUNServers
		opts.STUN = s.deps.STUN
		if opts.STUN == nil {
			opts.STUN = netx.NewSTUNClient(s.deps.Resolver)
		}
	}
	return opts
}

// detectPublicAddrs finds the server's public addresses at startup (04 §7.4). It takes at most 5 s: STUN gets 2 s
// and one retry. A detection that fails is no error here: the result holds what was found, and an ip-mode server
// without an address starts and is not ready (04 §6.2).
func (s *Server) detectPublicAddrs(ctx context.Context) netx.PublicAddrs {
	pub, err := netx.DetectPublicAddrs(ctx, s.detectOptions(true))
	log := s.log.With(slog.String("component", "netx"))
	if err != nil {
		log.Warn("public address detection failed", logx.Err(err))
	}
	// The server's own address is no secret of a user's: it is what friends type.
	attrs := []any{slog.String("nat", string(pub.NAT))}
	if pub.V4.IsValid() {
		attrs = append(attrs, slog.String("public_ipv4", pub.V4.String()), slog.String("method", string(pub.V4Method)))
	}
	if pub.V6.IsValid() {
		attrs = append(attrs, slog.String("public_ipv6", pub.V6.String()))
	}
	log.Info("public addresses", attrs...)
	return pub
}

// redetectPublicAddrs is the detection of a running server (04 §7.4). With public_ip set to a literal it asks no
// STUN server: the address can't change, and the operator who set it gets the one NAT check at startup and no
// packets to third parties afterwards.
func (s *Server) redetectPublicAddrs(ctx context.Context) (netx.PublicAddrs, error) {
	if s.hookDetect != nil {
		return s.hookDetect(), nil
	}
	return netx.DetectPublicAddrs(ctx, s.detectOptions(s.redetectWithSTUN()))
}

// redetectWithSTUN reports whether the later looks ask the STUN servers: only while public_ip is "auto". A literal
// (of either family) or "off" is the operator's word on the IPv4 address.
func (s *Server) redetectWithSTUN() bool {
	return s.cfg.PublicIP == "" || s.cfg.PublicIP == "auto"
}

// watchPublicAddrs looks at the public addresses every interval until ctx ends and calls changed when they are no
// longer the ones the server last saw; first is what the server started with. A detection that fails, or that finds
// no address where there was one (the network is down for a moment, a STUN server did not answer), changes nothing:
// the server keeps what it has and looks again at the next tick.
func watchPublicAddrs(ctx context.Context, interval time.Duration, first netx.PublicAddrs,
	detect func(context.Context) (netx.PublicAddrs, error), changed func(from, to netx.PublicAddrs),
) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	last := first
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		now, err := detect(ctx)
		if err != nil || ctx.Err() != nil {
			continue
		}
		// An address that is gone is taken as "not found this time"; a new one where there was none counts.
		if !now.V4.IsValid() {
			now.V4, now.V4Method = last.V4, last.V4Method
		}
		if !now.V6.IsValid() {
			now.V6, now.V6Method = last.V6, last.V6Method
		}
		if now.V4 == last.V4 && now.V6 == last.V6 {
			continue
		}
		changed(last, now)
		last = now
	}
}

// publicAddrsChanged is what the running server does about a public address that changed (04 §7.4). The ICE rewrite
// rules, the site and (in ip mode) the certificate are fixed for the process's life, so only a restart applies a new
// address.
//
// A server that serves says so and keeps going: the operator restarts it when it suits the people who are watching.
// The one server that restarts by itself is the one that has nothing to interrupt: it started without the public
// address that is its site (ip mode, or manual mode without a domain) and has been running without a site, not
// ready, since (04 §6.2). When a look finds an address that makes a site, it asks Run for a restart, and the next
// process starts with it. What kept the address away at startup is often gone a minute later (the network was not
// up yet at boot, STUN did not answer), and nothing else would restart a process that runs.
func (s *Server) publicAddrsChanged(from, to netx.PublicAddrs) {
	log := s.log.With(slog.String("component", "netx"))
	if s.awaitsAddress {
		if site, err := config.NewSite(&s.cfg, to.V4, to.V6); err == nil {
			log.Info("public IP address found: isshoni restarts to take it as its site",
				slog.String("public_ip", site.Hostname))
			s.publicNow.Store(&to)
			s.askRestart()
			return
		}
	}
	for _, c := range []struct {
		family   string
		from, to netip.Addr
	}{{"IPv4", from.V4, to.V4}, {"IPv6", from.V6, to.V6}} {
		if c.from == c.to {
			continue
		}
		was := "none"
		if c.from.IsValid() {
			was = c.from.String()
		}
		log.Warn("Public "+c.family+" address changed from "+was+" to "+c.to.String()+"; restart isshoni to apply",
			slog.String("from", was), slog.String("to", c.to.String()))
	}
	s.publicNow.Store(&to)
}

// askRestart makes Run shut the server down for a restart (ShutdownRestart), so that it returns
// ErrRestartRequested. It only leaves word: the caller is one of the server's own goroutines, which the shutdown
// waits for.
func (s *Server) askRestart() {
	s.restartOnce.Do(func() { close(s.restartAsked) })
}

// transportOptions fills the options of the ICE Transport (04 §7.3) from the config, the detected public addresses
// and the environment, for listenICE. The transfer counter of ops joins them with the ops data (README S85).
//
// IPv6 is off when network.ipv6 is false or public_ipv6 is "off": the operator wants no IPv6 media then, whatever
// the interfaces have. InContainer and CloudProvider are the values the detection got, so that both agree on what a
// NAT in front of the server is (04 §7.4, §7.5).
func (s *Server) transportOptions() netx.TransportOptions {
	return netx.TransportOptions{
		UDPAddr:           s.cfg.Listen.ICEUDP,
		TCPAddr:           s.cfg.Listen.ICETCP,
		PortMux:           s.mux, // nil in off mode
		ExcludeInterfaces: s.cfg.Network.ExcludeInterfaces,
		IncludeLoopback:   s.cfg.Network.IncludeLoopback,
		IPv6:              s.cfg.Network.IPv6 && s.cfg.PublicIPv6 != "off",
		UDPBufferBytes:    s.cfg.Network.UDPBufferBytes,
		Public:            s.public,
		InContainer:       s.inContainer(),
		CloudProvider:     s.cloudProvider(),
		Logger:            s.log,
	}
}

// listenICE binds the ICE Transport (04 §6.1 step 6, §7.3): listen.ice_udp on every local address that carries
// media, listen.ice_tcp, and the ICE side of the 443 multiplexer when there is one. Its errors are for the
// operator, like those of the other listeners; they are runtime errors (exit 1), not refusals. That goes for a
// machine without a usable address too (netx.ErrNoTransport): the interfaces are read once, here, so the fix is the
// restart that systemd and Docker make by themselves, which helps once the network is up.
func (s *Server) listenICE(ctx context.Context) (*netx.Transport, error) {
	tr, err := netx.NewTransport(ctx, s.transportOptions())
	if err == nil {
		return tr, nil
	}
	var le *netx.ListenError
	if !errors.As(err, &le) {
		return nil, fmt.Errorf("server: %w", err)
	}
	key, configured := "listen.ice_udp", s.cfg.Listen.ICEUDP
	if le.Proto == "tcp" {
		key, configured = "listen.ice_tcp", s.cfg.Listen.ICETCP
	}
	_, port, _ := net.SplitHostPort(le.Addr)
	switch {
	case errors.Is(err, syscall.EADDRINUSE):
		return nil, fmt.Errorf("port %s/%s is in use (another isshoni?). Stop it, or change %s (now %q): %w",
			port, le.Proto, key, configured, err)
	case errors.Is(err, fs.ErrPermission):
		return nil, fmt.Errorf("isshoni may not listen on %s = %q (a port below 1024 needs CAP_NET_BIND_SERVICE, "+
			"which the systemd unit and the container image grant): %w", key, configured, err)
	default:
		return nil, fmt.Errorf("server: listen on %s = %q: %w", key, configured, err)
	}
}

// iceAddrs returns the Transport's ICE listeners as bound, for Addrs: the first UDP socket, and the ICE-TCP
// listener of listen.ice_tcp. The Transport does not name that listener, so its port is the one the Transport
// advertises for it, and its host the configured one. Either is nil when that listener is off.
func iceAddrs(configuredTCP string, tr *netx.Transport) (udp, tcp net.Addr) {
	if tr.UDPMux != nil {
		if addrs := tr.UDPMux.GetListenAddresses(); len(addrs) > 0 {
			udp = addrs[0]
		}
	}
	for _, adv := range tr.Advertised {
		if adv.Via != netx.ViaTCP7882 {
			continue
		}
		host, _, _ := net.SplitHostPort(configuredTCP)
		ip, _ := netip.ParseAddr(host) // the zero Addr for an empty host: every address
		tcp = net.TCPAddrFromAddrPort(netip.AddrPortFrom(ip, adv.Addr.Port()))
		break
	}
	return udp, tcp
}

// mediaPorts is the media part of the "ready" line (04 §6.1 step 10): the ports that the server's ICE candidates
// name, "media udp/7882 ice-tcp 443,7882". A transport that is off shows as "off".
func mediaPorts(advertised []netx.AdvertisedAddr) string {
	ports := map[string]string{} // by Via
	for _, adv := range advertised {
		if _, seen := ports[adv.Via]; !seen {
			ports[adv.Via] = strconv.Itoa(int(adv.Addr.Port()))
		}
	}
	udp, ok := ports[netx.ViaUDP]
	if !ok {
		udp = "off"
	}
	var tcp []string
	for _, via := range []string{netx.ViaTCP443, netx.ViaTCP7882} {
		if p, ok := ports[via]; ok {
			tcp = append(tcp, p)
		}
	}
	if len(tcp) == 0 {
		tcp = []string{"off"}
	}
	return "media udp/" + udp + " ice-tcp " + strings.Join(tcp, ",")
}
