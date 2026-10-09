package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/netx"
	"github.com/MoonWX/isshoni/internal/server/ops"
	"github.com/MoonWX/isshoni/internal/server/servertest"
	"github.com/MoonWX/isshoni/internal/server/sfu"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

// The SFU in the server (04 W2, README S59): the ICE Transport among the listeners, the SFU behind the hub, their
// place in the shutdown, and the public addresses that every mode now detects. Media itself, through real
// PeerConnections, is tested in internal/server/itest.

// request sends a request on c and returns the payload of its ok.
func request[T any](t *testing.T, c *signaltest.Client, typ protocol.MessageType, data any) T {
	t.Helper()
	id := c.NextID()
	if err := c.Send(typ, id, data); err != nil {
		t.Fatalf("send %s: %v", typ, err)
	}
	for {
		env := nextOf(t, c, protocol.MessageTypeOK, protocol.MessageTypeError)
		if env.Re != id {
			continue
		}
		if env.Type != protocol.MessageTypeOK {
			t.Fatalf("%s was answered with %s %s", typ, env.Type, env.Data)
		}
		v, err := protocol.Decode[T](env)
		if err != nil {
			t.Fatalf("the reply to %s: %v", typ, err)
		}
		return v
	}
}

// startShare starts a share on c, which is in a room, and returns the SFU's parameters for it.
func startShare(t *testing.T, c *signaltest.Client, ref string) protocol.ShareParams {
	t.Helper()
	return request[protocol.ShareParams](t, c, protocol.MessageTypeShareStart, protocol.ShareStart{
		Kind: protocol.ShareKindScreen, Preset: protocol.PresetAuto, Audio: true, Ref: ref,
	})
}

// udpBound reports whether a UDP socket is bound to addr: binding a second one fails then.
func udpBound(addr net.Addr) bool {
	var lc net.ListenConfig
	pc, err := lc.ListenPacket(context.Background(), "udp4", addr.String())
	if err != nil {
		return true
	}
	_ = pc.Close()
	return false
}

// TestShutdownOrder: the shutdown stops the hub first, then the SFU, then the Transport under it (04 §6.4 steps 3
// and 4), and only then the HTTP servers and the rest. Each step finds the earlier ones done: the SFU still has its
// sockets when it closes its PeerConnections, and the Transport closes under an SFU that is closed.
func TestShutdownOrder(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{Deps: wiredDeps()})
	setUp(t, srv)
	c := dialWS(t, srv)
	joinLounge(t, c, signaltest.DefaultHello())
	share := startShare(t, c, "r1")
	s := srv.Srv
	media, udp := s.Media(), s.Addrs().ICEUDP
	if _, ok := media.Share(sfu.ShareID(share.ShareID)); !ok || !udpBound(udp) {
		t.Fatalf("before the shutdown: the SFU has the share: %v; a socket on %v: %v", ok, udp, udpBound(udp))
	}

	type moment struct {
		step              string
		signal, mediaOpen bool // the hub takes connections; the SFU does
		socket            bool // the media socket is bound
	}
	var mu sync.Mutex
	var got []moment
	s.SetShutdownStepHook(func(step string) {
		_, checks := s.Health().Ready()
		m := moment{step: step, signal: checks["signal"] == "ok", mediaOpen: media.Ready() == nil, socket: udpBound(udp)}
		if (checks["media"] == "ok") != m.mediaOpen {
			t.Errorf("before %q: the media check says %q, the SFU is open: %v", step, checks["media"], m.mediaOpen)
		}
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
	})
	srv.Stop(t)

	want := []moment{
		{step: "signaling", signal: true, mediaOpen: true, socket: true},
		{step: "sfu", mediaOpen: true, socket: true}, // the hub is down, the SFU and its sockets are not
		{step: "transport", socket: true},            // the SFU is closed; its sockets outlived it
		{step: "http"},                               // media is gone before the HTTP servers stop
		{step: "tls"},
		{step: "admin socket"},
		{step: "store"},
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(got, want) {
		t.Errorf("the shutdown's steps:\n got %+v\nwant %+v", got, want)
	}
	// The SFU ended what the hub had left, and nothing had to be cut off.
	if _, ok := media.Share(sfu.ShareID(share.ShareID)); ok {
		t.Error("the share is still in the SFU after the shutdown")
	}
	if logs := srv.Logs(); !strings.Contains(logs, "shutdown complete") || strings.Contains(logs, "shutdown finished by force") {
		t.Errorf("the log does not end the shutdown with \"shutdown complete\":\n%s", logs)
	}
	// Both ICE ports are free again.
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", s.Addrs().ICETCP.String())
	if err != nil {
		t.Fatalf("the ICE-TCP port after the shutdown: %v", err)
	}
	_ = ln.Close()
}

// TestMediaListeners: the server binds its media sockets with its other listeners (04 §6.1 step 6), tells where
// they are, and gives the ports back when it stops, so that a restart finds them free.
func TestMediaListeners(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{})
	addrs := srv.Srv.Addrs()
	udp, okUDP := addrs.ICEUDP.(*net.UDPAddr)
	tcp, okTCP := addrs.ICETCP.(*net.TCPAddr)
	if !okUDP || !okTCP || udp.Port == 0 || tcp.Port == 0 || !udp.IP.IsLoopback() || !tcp.IP.IsLoopback() {
		t.Fatalf("Addrs = %+v, want the bound loopback ports of listen.ice_udp and listen.ice_tcp", addrs)
	}
	if addrs.HTTPS != nil {
		t.Errorf("an HTTPS listener in off mode: %v", addrs.HTTPS)
	}
	if !udpBound(udp) {
		t.Errorf("no socket is bound on %v", udp)
	}
	// The ICE-TCP listener takes connections: its mux reads them.
	c, err := tryDial(tcp)
	if err != nil {
		t.Fatalf("the ICE-TCP listener: %v", err)
	}
	_ = c.Close()

	// The SFU answers on them: the Transport that Start bound is the one it runs on.
	tr := srv.Srv.Transport()
	if tr.UDPMux == nil || tr.TCPMux == nil || tr.TCPMux7882 == nil || tr.TCPMux443 != nil || !tr.IncludeLoopback {
		t.Errorf("Transport = %+v, want UDP and 7882/tcp on loopback and no 443 side in off mode", tr)
	}
	if err := srv.Srv.Media().Ready(); err != nil {
		t.Errorf("SFU.Ready = %v", err)
	}

	srv.Stop(t)
	if udpBound(udp) {
		t.Errorf("%v is still bound after the shutdown", udp)
	}
	if c, err := tryDial(tcp); err == nil {
		_ = c.Close()
		t.Errorf("%v still accepts connections after the shutdown", tcp)
	}
}

// TestMediaOnThe443Port: with UDP and listen.ice_tcp both turned off, a TLS-mode server still has a way in for
// media, the ICE side of its 443 multiplexer, and is ready with it (04 §6.2, §7.3). In off mode the same config is
// refused: media would have no way in.
func TestMediaOnThe443Port(t *testing.T) {
	noICE := func(c *config.Config) { c.Listen.ICEUDP, c.Listen.ICETCP = "", "" }
	srv := servertest.Start(t, servertest.Options{TLS: true, Config: noICE})
	addrs := srv.Srv.Addrs()
	if addrs.ICEUDP != nil || addrs.ICETCP != nil {
		t.Errorf("Addrs = %+v, want no ICE listener of its own", addrs)
	}
	tr := srv.Srv.Transport()
	if tr.UDPMux != nil || tr.TCPMux == nil || tr.TCPMux443 == nil || tr.TCPMux7882 != nil {
		t.Errorf("Transport = %+v, want the 443 side alone", tr)
	}
	port := addrs.HTTPS.(*net.TCPAddr).AddrPort().Port()
	want := []netx.AdvertisedAddr{{Proto: "tcp", Addr: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port), Via: netx.ViaTCP443}}
	if !slices.Equal(tr.Advertised, want) {
		t.Errorf("advertised = %+v, want %+v", tr.Advertised, want)
	}
	if ok, checks := srv.Srv.Health().Ready(); !ok || checks["media"] != "ok" {
		t.Errorf("Ready = %v %v", ok, checks)
	}
	if !strings.Contains(srv.Logs(), fmt.Sprintf(" (tls=manual) media udp/off ice-tcp %d\"", port)) {
		t.Errorf("no ready line with the media port %d:\n%s", port, srv.Logs())
	}

	_, err := servertest.Try(t, servertest.Options{Config: noICE})
	if err == nil || !server.NeedsOperator(err) || !strings.Contains(err.Error(), "listen.ice_udp") {
		t.Errorf("an off-mode server without an ICE listener: %v, want a config error about listen.ice_udp", err)
	}
}

// TestICEPortInUse: a busy media port is a runtime error that names the port and the key (04 §6.1 step 6), like a
// busy HTTP port, and the failed start leaves nothing behind.
func TestICEPortInUse(t *testing.T) {
	var lc net.ListenConfig
	busyUDP, err := lc.ListenPacket(context.Background(), "udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = busyUDP.Close() })
	busyTCP := listenLoopback(t)

	for _, tc := range []struct {
		name string
		flag string
		want string
	}{
		{"UDP", "--listen.ice-udp=" + busyUDP.LocalAddr().String(),
			fmt.Sprintf("port %d/udp is in use (another isshoni?). Stop it, or change listen.ice_udp (now %q)",
				busyUDP.LocalAddr().(*net.UDPAddr).Port, busyUDP.LocalAddr().String())},
		{"TCP", "--listen.ice-tcp=" + busyTCP.Addr().String(),
			fmt.Sprintf("port %d/tcp is in use (another isshoni?). Stop it, or change listen.ice_tcp (now %q)",
				busyTCP.Addr().(*net.TCPAddr).Port, busyTCP.Addr().String())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t, tc.flag)
			s, err := server.New(cfg, discardLog(), testDeps())
			if err != nil {
				t.Fatal(err)
			}
			err = s.Start(context.Background())
			if err == nil {
				_ = s.Shutdown(context.Background(), server.ShutdownStop)
				t.Fatal("Start succeeded on a busy media port")
			}
			if !strings.HasPrefix(err.Error(), tc.want) || !errors.Is(err, syscall.EADDRINUSE) || server.NeedsOperator(err) {
				t.Errorf("Start = %v\nwant a runtime error that starts with %q", err, tc.want)
			}
			// The listeners it had bound before are closed again, and so is the data directory's lock: the same
			// config starts once the port is free.
			if a := s.Addrs().HTTP; a != nil {
				if c, err := tryDial(a); err == nil {
					_ = c.Close()
					t.Errorf("%v still accepts connections after the failed start", a)
				}
			}
		})
	}
}

// TestPublicAddressesInOffMode: the detection of 04 §7.4 runs in off mode too. The site there is the proxy's, but
// the addresses in the server's ICE candidates are the detected ones, the status shows them, and the running
// server looks again; an address that changed is reported and applied by a restart of the operator's.
func TestPublicAddressesInOffMode(t *testing.T) {
	first := netx.PublicAddrs{
		V4: netip.MustParseAddr("203.0.113.7"), V4Method: netx.MethodSTUN,
		LocalV4: netip.MustParseAddr("127.0.0.1"), NAT: api.NATKindOneToOne,
	}
	moved := first
	moved.V4 = netip.MustParseAddr("203.0.113.99")
	var looks atomic.Int32

	cfg := testConfig(t)
	deps := testDeps()
	// A cloud machine behind 1:1 NAT, as the detection's result says: the candidates name the public address.
	deps.Host.Environ = append(deps.Host.Environ, "ISSHONI_IN_CONTAINER=1")
	logs := &logCapture{}
	s, err := server.New(cfg, logs.logger(), deps)
	if err != nil {
		t.Fatal(err)
	}
	s.SetPublicAddrsFunc(func() netx.PublicAddrs {
		if looks.Add(1) <= 2 { // the start, and one look that finds the same address
			return first
		}
		return moved
	})
	s.SetRedetectEvery(5 * time.Millisecond)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx, server.ShutdownStop); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	if looks.Load() == 0 {
		t.Fatal("an off-mode server did not look for its public addresses at startup")
	}

	// The site is not the address, so no check waits for one; the status and the candidates carry it.
	if ok, checks := s.Health().Ready(); !ok || checks["public_ip"] != "" {
		t.Errorf("Ready = %v %v, want ready without a public_ip check in off mode", ok, checks)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st, err := ops.DialAdmin(cfg.Listen.AdminSocket).Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.PublicIPv4 != "203.0.113.7" || st.PublicIPv4Method != "stun" || st.NAT != api.NATKindOneToOne || st.LocalIPv4 != "127.0.0.1" {
		t.Errorf("status = %+v, want the detected addresses", st)
	}
	udp, tcp := s.Addrs().ICEUDP.(*net.UDPAddr).Port, s.Addrs().ICETCP.(*net.TCPAddr).Port
	wantAdvertised := []api.AdvertisedAddr{
		{Proto: "udp", Addr: fmt.Sprintf("203.0.113.7:%d", udp), Via: api.TransportUDP},
		{Proto: "tcp", Addr: fmt.Sprintf("203.0.113.7:%d", tcp), Via: api.TransportTCP7882},
	}
	if !slices.Equal(st.Advertised, wantAdvertised) {
		t.Errorf("advertised = %+v, want the public address on the bound ports: %+v", st.Advertised, wantAdvertised)
	}

	// The watch: the change is reported once, and nothing restarts.
	waitFor(t, "the changed address", func() bool { return s.PublicAddrsNow() != nil })
	waitFor(t, "the warning", func() bool { return strings.Contains(logs.String(), "restart isshoni to apply") })
	before := looks.Load()
	waitFor(t, "more looks", func() bool { return looks.Load() >= before+3 })
	if n := strings.Count(logs.String(), "Public IPv4 address changed from 203.0.113.7 to 203.0.113.99; restart isshoni to apply"); n != 1 {
		t.Errorf("the change was reported %d times, want once:\n%s", n, logs.String())
	}
	select {
	case <-s.RestartAsked():
		t.Error("a serving server asked for a restart over a changed address")
	default:
	}
	if ok, _ := s.Health().Ready(); !ok {
		t.Error("the server is no longer ready after the address changed")
	}
}

// TestRestartsWhenThePublicAddressAppears: an ip-mode server that found no public address at startup runs without
// a site, not ready (04 §6.2). It keeps looking, and when a look finds an address, Run shuts the server down and
// returns ErrRestartRequested: the next process starts with the address (cmd/isshoni re-execs, or exits for
// systemd and Docker to start it again). The detector is a fake; public_test.go has the same on a fake clock.
func TestRestartsWhenThePublicAddressAppears(t *testing.T) {
	found := netx.PublicAddrs{V4: netip.MustParseAddr("203.0.113.7"), V4Method: netx.MethodSTUN}
	var looks atomic.Int32
	var appear atomic.Bool

	cfg := testConfig(t, tlsFlags("--tls.mode=ip", closedCA)...)
	logs := &logCapture{}
	s, err := server.New(cfg, logs.logger(), testDeps())
	if err != nil {
		t.Fatal(err)
	}
	s.SetPublicAddrsFunc(func() netx.PublicAddrs {
		looks.Add(1)
		if appear.Load() {
			return found
		}
		return netx.PublicAddrs{}
	})
	s.SetRedetectEvery(5 * time.Millisecond)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ran := make(chan error, 1)
	go func() { ran <- s.Run(context.Background()) }()

	// Without an address: no site, not ready, and it says that it keeps looking.
	ok, checks := s.Health().Ready()
	if ok || s.Site().Origin != "" || !strings.HasPrefix(checks["public_ip"], "no public IP address found") || checks["media"] != "ok" {
		t.Errorf("Ready = %v %v with the site %+v", ok, checks, s.Site())
	}
	if !strings.Contains(logs.String(), "isshoni looks for the address again every 5ms and restarts itself when it finds one") {
		t.Errorf("the start line does not say that the server keeps looking:\n%s", logs.String())
	}
	// Looks that find nothing change nothing.
	waitFor(t, "a few looks", func() bool { return looks.Load() >= 4 })
	select {
	case err := <-ran:
		t.Fatalf("Run returned %v while no address was found", err)
	case <-s.RestartAsked():
		t.Fatal("a restart was asked for while no address was found")
	default:
	}

	appear.Store(true)
	select {
	case err := <-ran:
		if !errors.Is(err, server.ErrRestartRequested) {
			t.Errorf("Run = %v, want ErrRestartRequested", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("Run did not return after the address appeared:\n%s", logs.String())
	}
	// It was a full graceful shutdown, for a restart, and everything is released for the next process.
	var said bool
	for _, rec := range logRecords(t, logs.String()) {
		switch rec["msg"] {
		case "public IP address found: isshoni restarts to take it as its site":
			said = rec["level"] == "INFO" && rec["public_ip"] == "203.0.113.7" && rec["component"] == "netx"
		case "shutting down":
			if rec["component"] == nil && rec["reason"] != "restart" {
				t.Errorf("the shutdown's reason = %v, want restart", rec["reason"])
			}
		}
	}
	if !said || !strings.Contains(logs.String(), "shutdown complete") {
		t.Errorf("the log does not show the address and a complete shutdown:\n%s", logs.String())
	}
	if now := s.PublicAddrsNow(); now == nil || now.V4 != found.V4 {
		t.Errorf("the addresses last seen = %+v", now)
	}
	if c, err := tryDial(s.Addrs().HTTPS); err == nil {
		_ = c.Close()
		t.Error("the HTTPS port still accepts connections")
	}

	// The next process: the same config and data directory, and the address is there at startup.
	next, _ := startDirect(t, cfg, testDeps(), found)
	if site := next.Site(); site.Hostname != "203.0.113.7" {
		t.Errorf("the restarted server's site = %+v", site)
	}
	if _, checks := next.Health().Ready(); checks["public_ip"] != "ok" {
		t.Errorf("the restarted server's checks = %v", checks)
	}
	select {
	case <-next.RestartAsked():
		t.Error("the restarted server asks for another restart")
	default:
	}
}

// noLocalAddress are the flags of a machine whose network is not up: no interface counts, and loopback, which the
// tests' servers otherwise run on, carries no media.
var noLocalAddress = []string{"--network.include-loopback=false", "--network.exclude-interfaces=*"}

// TestStartsBeforeTheNetworkIsUp: what most often keeps the public address from a server at startup is a network
// that is not up yet (04 §6.2), and such a machine has no local address for the media sockets either. The server
// that waits for its address starts all the same (04 §6.3: what it lacks from outside never stops the start):
// without media sockets and without an SFU, not ready, with the checks "public_ip" and "media" saying why. When a
// look finds the address, it restarts like every server that waited for one.
func TestStartsBeforeTheNetworkIsUp(t *testing.T) {
	found := netx.PublicAddrs{V4: netip.MustParseAddr("203.0.113.7"), V4Method: netx.MethodSTUN}
	for _, tc := range []struct {
		name  string
		flags []string
		cause string // netx's reason, in the warning
	}{
		{"UDP", []string{"--listen.ice-udp=:0", "--listen.ice-tcp="}, "no usable local address for ICE UDP"},
		// The Transport got as far as its ICE-TCP mux on the multiplexer's ICE side before it gave up.
		{"ICE-TCP on the HTTPS port alone", []string{"--listen.ice-udp=", "--listen.ice-tcp="}, "no usable address for ICE-TCP"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var looks atomic.Int32
			var appear atomic.Bool
			flags := slices.Concat(tlsFlags("--tls.mode=ip", closedCA), tc.flags, noLocalAddress)
			cfg := testConfig(t, flags...)
			logs := &logCapture{}
			s, err := server.New(cfg, logs.logger(), testDeps())
			if err != nil {
				t.Fatal(err)
			}
			s.SetPublicAddrsFunc(func() netx.PublicAddrs {
				looks.Add(1)
				if appear.Load() {
					return found
				}
				return netx.PublicAddrs{}
			})
			s.SetRedetectEvery(5 * time.Millisecond)
			if err := s.Start(context.Background()); err != nil {
				t.Fatalf("Start = %v, want a server that waits for its address", err)
			}
			t.Cleanup(func() { // for a test that fails before the restart; after it, the server is stopped already
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				_ = s.Shutdown(ctx, server.ShutdownStop)
			})
			ran := make(chan error, 1)
			go func() { ran <- s.Run(context.Background()) }()

			// No media sockets, no SFU, and both checks say what is missing.
			const noMedia = "no usable local network address for media; isshoni restarts itself when it finds its public address"
			ok, checks := s.Health().Ready()
			if ok || !strings.HasPrefix(checks["public_ip"], "no public IP address found") || checks["media"] != noMedia || checks["db"] != "ok" {
				t.Errorf("Ready = %v %v", ok, checks)
			}
			if addrs := s.Addrs(); addrs.ICEUDP != nil || addrs.ICETCP != nil || addrs.HTTPS == nil || addrs.HTTP == nil {
				t.Errorf("Addrs = %+v, want the HTTP listeners and no media socket", addrs)
			}
			if s.Transport() != nil || s.Media() != nil {
				t.Errorf("a Transport (%v) or an SFU (%v) on a machine without an address for media", s.Transport(), s.Media())
			}
			var warned bool
			for _, rec := range logRecords(t, logs.String()) {
				if msg, _ := rec["msg"].(string); strings.HasPrefix(msg, "no local network address can carry media yet") {
					cause, _ := rec["err"].(string)
					warned = rec["level"] == "WARN" && rec["component"] == "netx" && strings.Contains(cause, tc.cause)
				}
			}
			if !warned {
				t.Errorf("no warning about the missing address with the cause %q:\n%s", tc.cause, logs.String())
			}

			// The operator's view through the admin socket: the same checks, and a status without media.
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			admin := ops.DialAdmin(cfg.Listen.AdminSocket)
			if ready, err := admin.Ready(ctx); err != nil || ready.Status != ops.StatusNotReady || ready.Checks["media"] != noMedia {
				t.Errorf("ready = %+v, %v", ready, err)
			}
			st, err := admin.Status(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if st.Advertised == nil || len(st.Advertised) != 0 || st.UDPRcvBufBytes != 0 || st.UDPSndBufBytes != 0 {
				t.Errorf("status = %+v, want nothing advertised and no socket buffers", st)
			}
			for _, l := range st.Listeners {
				if strings.HasPrefix(l.Key, "listen.ice_") {
					t.Errorf("the status lists the media listener %+v", l)
				}
			}

			// Nothing reads ICE-TCP on the HTTPS port: such a connection is closed, not kept waiting. (The server
			// closes it with the client's bytes unread, so the end may arrive as a reset.)
			ice := dialRaw(t, s.Addrs().HTTPS)
			if _, err := ice.Write(stunBindingFrame()); err != nil {
				t.Fatal(err)
			}
			_ = ice.SetReadDeadline(time.Now().Add(10 * time.Second))
			n, err := ice.Read(make([]byte, 64))
			if closed := errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET); n != 0 || !closed {
				t.Errorf("an ICE-TCP connection read %d bytes, %v; want it closed without a byte", n, err)
			}

			// Looks that find nothing change nothing; the one that finds the address restarts the server.
			waitFor(t, "a few looks", func() bool { return looks.Load() >= 4 })
			select {
			case err := <-ran:
				t.Fatalf("Run returned %v while no address was found", err)
			case <-s.RestartAsked():
				t.Fatal("a restart was asked for while no address was found")
			default:
			}
			appear.Store(true)
			select {
			case err := <-ran:
				if !errors.Is(err, server.ErrRestartRequested) {
					t.Errorf("Run = %v, want ErrRestartRequested", err)
				}
			case <-time.After(20 * time.Second):
				t.Fatalf("Run did not return after the address appeared:\n%s", logs.String())
			}
			// The shutdown of a server without media has nothing to stop there, and nothing to force.
			if out := logs.String(); !strings.Contains(out, "shutdown complete") || strings.Contains(out, "by force") {
				t.Errorf("the log does not end with a complete shutdown:\n%s", out)
			}
			for _, rec := range logRecords(t, logs.String()) {
				if rec["level"] == "ERROR" && rec["scope"] == nil { // a scope marks Pion's own lines
					t.Errorf("the server logged %v", rec)
				}
			}
			if c, err := tryDial(s.Addrs().HTTPS); err == nil {
				_ = c.Close()
				t.Error("the HTTPS port still accepts connections")
			}
		})
	}
}

// TestNoLocalAddressWithASite: a server that knows its site and finds no local address for media does not start:
// it would be ready for everything but what it is for. That is a runtime error, which the service manager's
// restart fixes once the network is up, and it says so (04 §6.1 step 6). Only the server that waits for its public
// address starts without media (TestStartsBeforeTheNetworkIsUp).
func TestNoLocalAddressWithASite(t *testing.T) {
	const want = "no usable network address for media yet (is the network up?). isshoni starts once there is one"
	for _, tc := range []struct {
		name   string
		flags  []string
		public netx.PublicAddrs
	}{
		{name: "off mode", flags: []string{"--listen.ice-udp=:0", "--listen.ice-tcp="}},
		// The next process of a server that waited: it has its address now, and still no way in for media.
		{name: "ip mode with its address", flags: append(tlsFlags("--tls.mode=ip", closedCA), "--listen.ice-udp=:0", "--listen.ice-tcp="),
			public: netx.PublicAddrs{V4: netip.MustParseAddr("203.0.113.7"), V4Method: netx.MethodSTUN}},
		{name: "ip mode with its address, ICE-TCP on the HTTPS port alone",
			flags:  append(tlsFlags("--tls.mode=ip", closedCA), "--listen.ice-udp=", "--listen.ice-tcp="),
			public: netx.PublicAddrs{V6: netip.MustParseAddr("2001:db8::7"), V6Method: netx.MethodInterface}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t, append(tc.flags, noLocalAddress...)...)
			s, err := server.New(cfg, discardLog(), testDeps())
			if err != nil {
				t.Fatal(err)
			}
			s.SetPublicAddrs(tc.public)
			err = s.Start(context.Background())
			if err == nil {
				_ = s.Shutdown(context.Background(), server.ShutdownStop)
				t.Fatal("Start succeeded without an address for media")
			}
			if !strings.HasPrefix(err.Error(), want) || !errors.Is(err, netx.ErrNoTransport) || server.NeedsOperator(err) {
				t.Errorf("Start = %v\nwant a runtime error that starts with %q", err, want)
			}
			// The listeners it had bound before are closed again.
			for _, a := range []net.Addr{s.Addrs().HTTP, s.Addrs().HTTPS} {
				if a == nil {
					continue
				}
				if c, err := tryDial(a); err == nil {
					_ = c.Close()
					t.Errorf("%v still accepts connections after the failed start", a)
				}
			}
		})
	}
}

// TestLimitsReachTheSFU is the check of 04 §17 for the SFU's limits: the admin's maxShareBitrateKbps caps the full
// layer of a share from the start, and a change in the admin's settings reaches the running SFU (SFU.SetLimits)
// without a restart.
func TestLimitsReachTheSFU(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{Deps: wiredDeps()})
	setUp(t, srv)
	c := dialWS(t, srv)
	joinLounge(t, c, signaltest.DefaultHello())
	full := func(p protocol.ShareParams) int64 {
		t.Helper()
		for _, e := range p.Encodings {
			if e.RID == protocol.RIDHigh {
				return e.MaxBitrate
			}
		}
		t.Fatalf("share params without a full layer: %+v", p)
		return 0
	}
	patch := func(kbps int) {
		t.Helper()
		body, _ := json.Marshal(map[string]int{"maxShareBitrateKbps": kbps})
		wantStatus(t, "PATCH /admin/settings", call(t, srv, http.MethodPatch, "/api/v1/admin/settings", json.RawMessage(body)),
			http.StatusOK, "")
	}

	uncapped := full(startShare(t, c, "r1"))
	if uncapped <= 2_000_000 {
		t.Fatalf("the full layer of an uncapped share may send %d bit/s; the test needs more than 2 Mbit/s", uncapped)
	}
	patch(2000)
	if got := full(startShare(t, c, "r2")); got != 2_000_000 {
		t.Errorf("after maxShareBitrateKbps = 2000 the full layer may send %d bit/s, want 2000000", got)
	}
	patch(0)
	if got := full(startShare(t, c, "r3")); got != uncapped {
		t.Errorf("after the cap was lifted the full layer may send %d bit/s, want %d again", got, uncapped)
	}

	// A cap the operator set in the config is there from the first share on (04 §4.6: the pin precedes the SFU).
	pinned := servertest.Start(t, servertest.Options{Deps: wiredDeps(), Flags: []string{"--limits.max-bitrate-kbps=1500"}})
	setUp(t, pinned)
	pc := dialWS(t, pinned)
	joinLounge(t, pc, signaltest.DefaultHello())
	if got := full(startShare(t, pc, "r1")); got != 1_500_000 {
		t.Errorf("with limits.max_bitrate_kbps = 1500 the full layer may send %d bit/s, want 1500000", got)
	}
}

// TestUnbuiltSFUMethodsAreQuiet: the messages that reach an SFU method a later slice of the plan implements are
// answered, and none of them puts a warning or an error into the server's log (wire.go: notImplementedAsDebug).
// Every client sends pc.close when its last share stops, and Firefox sends caps.update while it gets its decoder.
// The test holds whichever of these methods are built: it asks only that the log stays quiet.
func TestUnbuiltSFUMethodsAreQuiet(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{Deps: wiredDeps()})
	setUp(t, srv)
	c := dialWS(t, srv)
	hello := signaltest.DefaultHello()
	joinLounge(t, c, hello)
	startShare(t, c, "r1")

	for _, m := range []struct {
		typ  protocol.MessageType
		data any
	}{
		{protocol.MessageTypeCapsUpdate, protocol.CapsUpdate{Caps: hello.Caps}},
		{protocol.MessageTypePCRestart, protocol.PCRestart{
			PC: protocol.PCKindSub, Gen: 1, Mode: protocol.RestartModeICE, Reason: protocol.RestartReasonDisconnected,
		}},
		{protocol.MessageTypePCClose, protocol.PCClose{PC: protocol.PCKindPub, Gen: 1}},
	} {
		if err := c.Send(m.typ, "", m.data); err != nil {
			t.Fatalf("send %s: %v", m.typ, err)
		}
	}
	// The hub handles a connection's messages in order: once this request is answered, the three before it are
	// done, whatever each of them answered.
	startShare(t, c, "r2")

	// Nothing is an error, and the adapter between the hub and the SFU, which is where the answers of an unbuilt
	// method arrive, has no warning either. (Pion's own lines, which carry a scope, are not the server's.)
	for _, rec := range logRecords(t, srv.Logs()) {
		if rec["scope"] != nil {
			continue
		}
		if level := rec["level"]; level == "ERROR" || (level == "WARN" && rec["component"] == "sfuplane") {
			t.Errorf("the server logged %v", rec)
		}
	}
}
