package server

import (
	"context"
	"log/slog"
	"net/netip"
	"os"
	"time"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/netx"
)

// This file is the server's view of its own network: the public addresses (04 §7.4), found at startup and looked at
// again on a ticker, and the options of the ICE Transport (04 §7.3), which the SFU's wiring binds (README S59).

// publicRedetectEvery is how often a running server looks at its public addresses again (04 §7.4).
const publicRedetectEvery = 10 * time.Minute

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

// publicAddrsChanged is what the running server does about a public address that changed (04 §7.4): it says so and
// keeps going. The ICE rewrite rules, the site and (in ip mode) the certificate are fixed for the process's life, so
// only a restart applies the new address.
func (s *Server) publicAddrsChanged(from, to netx.PublicAddrs) {
	log := s.log.With(slog.String("component", "netx"))
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

// transportOptions fills the options of the ICE Transport (04 §7.3) from the config, the detected public addresses
// and the environment. README S59 calls netx.NewTransport with them in step 6 of the startup sequence, once the 443
// multiplexer is bound, and adds the transfer counter of ops (README S55).
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
