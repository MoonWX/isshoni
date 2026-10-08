package server

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"reflect"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/netx"
)

// TestWatchPublicAddrs runs the ten-minute look at the public addresses (04 §7.4) on a fake clock: a change is
// reported once, with the address the server last saw; a look that fails or finds nothing changes nothing.
func TestWatchPublicAddrs(t *testing.T) {
	a, b := netip.MustParseAddr("203.0.113.7"), netip.MustParseAddr("203.0.113.8")
	v6 := netip.MustParseAddr("2001:db8::7")
	type change struct{ from, to netx.PublicAddrs }
	type look struct {
		pub  netx.PublicAddrs
		err  error
		want *change // reported after this look; nil for none
	}
	v4 := func(addr netip.Addr) netx.PublicAddrs { return netx.PublicAddrs{V4: addr, V4Method: netx.MethodSTUN} }
	both := netx.PublicAddrs{V4: b, V4Method: netx.MethodSTUN, V6: v6, V6Method: netx.MethodInterface}

	looks := []look{
		{pub: v4(a)},                  // the same address
		{err: errors.New("no route")}, // the detection failed
		{pub: netx.PublicAddrs{}},     // no STUN answer this time: the server keeps what it has
		{pub: v4(b), want: &change{v4(a), v4(b)}},
		{pub: v4(b)},                            // reported once
		{pub: both, want: &change{v4(b), both}}, // an IPv6 address appears
		{pub: v4(b)},                            // and is not found the next time: kept
		{pub: netx.PublicAddrs{V4: a, V4Method: netx.MethodSTUN, V6: v6, V6Method: netx.MethodInterface},
			want: &change{both, netx.PublicAddrs{V4: a, V4Method: netx.MethodSTUN, V6: v6, V6Method: netx.MethodInterface}}}, // back to a
	}

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		// What the watch has done so far; the watch's goroutine writes, the test reads between its steps.
		var mu sync.Mutex
		var changes []change
		n := 0
		seen := func() (int, []change) {
			mu.Lock()
			defer mu.Unlock()
			out := changes
			changes = nil
			return n, out
		}
		detect := func(context.Context) (netx.PublicAddrs, error) {
			mu.Lock()
			defer mu.Unlock()
			l := looks[min(n, len(looks)-1)]
			n++
			return l.pub, l.err
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			watchPublicAddrs(ctx, publicRedetectEvery, v4(a), detect, func(from, to netx.PublicAddrs) {
				mu.Lock()
				defer mu.Unlock()
				changes = append(changes, change{from, to})
			})
		}()

		// Nothing happens before the first interval is over.
		time.Sleep(publicRedetectEvery - time.Second)
		synctest.Wait()
		if n, _ := seen(); n != 0 {
			t.Fatalf("%d looks before the first interval was over", n)
		}
		time.Sleep(time.Second)
		for i, l := range looks {
			synctest.Wait()
			n, got := seen()
			if n != i+1 {
				t.Fatalf("after %d intervals: %d looks", i+1, n)
			}
			var want []change
			if l.want != nil {
				want = []change{*l.want}
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("look %d: changes %+v, want %+v", i+1, got, want)
			}
			if i < len(looks)-1 {
				time.Sleep(publicRedetectEvery)
			}
		}

		// The watch ends with the server's context, without another look.
		cancel()
		<-done
		if n, got := seen(); n != len(looks) || len(got) != 0 {
			t.Errorf("%d looks and %d changes after the end, want %d and none", n, len(got), len(looks))
		}
	})
}

// TestTransportOptions: what the SFU's wiring (README S59) hands to netx.NewTransport comes from the config, the
// detected addresses and the environment, and agrees with what the detection was told (04 §7.3–7.6).
func TestTransportOptions(t *testing.T) {
	container := config.Host{Root: t.TempDir(), Environ: []string{config.EnvInContainer + "=1"}}
	bare := config.Host{Root: t.TempDir(), Environ: []string{}}
	pub := netx.PublicAddrs{V4: netip.MustParseAddr("203.0.113.7"), V4Method: netx.MethodSTUN, LocalV4: netip.MustParseAddr("10.0.0.5")}

	for _, tc := range []struct {
		name          string
		flags         []string
		host          config.Host
		wantUDP       string
		wantTCP       string
		wantIPv6      bool
		wantLoopback  bool
		wantContainer bool
		wantBuffers   int
	}{
		{name: "the defaults", host: bare, wantUDP: ":7882", wantTCP: ":7882", wantIPv6: true, wantBuffers: 8 << 20},
		{name: "public_ipv6 off turns IPv6 media off", flags: []string{"--public-ipv6=off"}, host: bare,
			wantUDP: ":7882", wantTCP: ":7882", wantBuffers: 8 << 20},
		{name: "network.ipv6 off", flags: []string{"--network.ipv6=false"}, host: bare,
			wantUDP: ":7882", wantTCP: ":7882", wantBuffers: 8 << 20},
		{name: "a literal IPv6 address keeps it on", flags: []string{"--public-ipv6=2001:db8::7"}, host: bare,
			wantUDP: ":7882", wantTCP: ":7882", wantIPv6: true, wantBuffers: 8 << 20},
		{name: "in a container", host: container, wantUDP: ":7882", wantTCP: ":7882", wantIPv6: true,
			wantContainer: true, wantBuffers: 8 << 20},
		{name: "TCP only", flags: []string{"--listen.ice-udp="}, host: bare, wantTCP: ":7882", wantIPv6: true, wantBuffers: 8 << 20},
		{name: "development", flags: []string{"--listen.ice-udp=127.0.0.1:0", "--listen.ice-tcp=127.0.0.1:0",
			"--network.include-loopback=true", "--network.udp-buffer-bytes=2097152", "--network.exclude-interfaces=docker*,tun*"},
			host: bare, wantUDP: "127.0.0.1:0", wantTCP: "127.0.0.1:0", wantIPv6: true, wantLoopback: true, wantBuffers: 2 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := loadFlags(t, tc.flags...)
			s, err := New(cfg, discardLog(), Deps{Host: tc.host})
			if err != nil {
				t.Fatal(err)
			}
			s.public = pub
			o := s.transportOptions()
			if o.UDPAddr != tc.wantUDP || o.TCPAddr != tc.wantTCP {
				t.Errorf("UDPAddr %q, TCPAddr %q; want %q, %q", o.UDPAddr, o.TCPAddr, tc.wantUDP, tc.wantTCP)
			}
			if o.IPv6 != tc.wantIPv6 {
				t.Errorf("IPv6 = %v with network.ipv6 = %v and public_ipv6 = %q", o.IPv6, cfg.Network.IPv6, cfg.PublicIPv6)
			}
			if o.IncludeLoopback != tc.wantLoopback || o.UDPBufferBytes != tc.wantBuffers {
				t.Errorf("IncludeLoopback %v, UDPBufferBytes %d; want %v, %d", o.IncludeLoopback, o.UDPBufferBytes, tc.wantLoopback, tc.wantBuffers)
			}
			if !slices.Equal(o.ExcludeInterfaces, cfg.Network.ExcludeInterfaces) || len(o.ExcludeInterfaces) == 0 {
				t.Errorf("ExcludeInterfaces = %v, config has %v", o.ExcludeInterfaces, cfg.Network.ExcludeInterfaces)
			}
			if !reflect.DeepEqual(o.Public, pub) {
				t.Errorf("Public = %+v, want the detected %+v", o.Public, pub)
			}
			if o.PortMux != nil {
				t.Error("PortMux is set in off mode")
			}
			if o.Logger == nil {
				t.Error("Logger is nil: netx's one warning about small buffers would be lost")
			}
			// The detection and the Transport must agree on what stands in front of the server.
			d := s.detectOptions(true)
			if o.InContainer != tc.wantContainer || d.InContainer != o.InContainer {
				t.Errorf("InContainer: transport %v, detection %v; want %v", o.InContainer, d.InContainer, tc.wantContainer)
			}
			if d.CloudProvider != o.CloudProvider || o.CloudProvider == "" {
				t.Errorf("CloudProvider: transport %q, detection %q", o.CloudProvider, d.CloudProvider)
			}
		})
	}
}

// TestDetectOptions: the detection gets the config's addresses and STUN servers; a look without STUN names no
// server, so it sends nothing.
func TestDetectOptions(t *testing.T) {
	cfg := loadFlags(t, "--public-ip=203.0.113.7", "--public-ipv6=off", "--network.ipv6=false",
		"--network.stun-servers=stun.example.net:3478,stun.example.org:3478")
	stun := fakeSTUN{}
	s, err := New(cfg, discardLog(), Deps{STUN: stun, Host: config.Host{Root: t.TempDir(), Environ: []string{}}})
	if err != nil {
		t.Fatal(err)
	}
	with := s.detectOptions(true)
	if with.PublicIP != "203.0.113.7" || with.PublicIPv6 != "off" || with.IPv6 {
		t.Errorf("detection options = %+v", with)
	}
	if !slices.Equal(with.STUNServers, []string{"stun.example.net:3478", "stun.example.org:3478"}) || with.STUN != netx.STUNClient(stun) {
		t.Errorf("STUN: servers %v, client %v", with.STUNServers, with.STUN)
	}
	without := s.detectOptions(false)
	if len(without.STUNServers) != 0 || without.STUN != nil {
		t.Errorf("a look without STUN names servers %v", without.STUNServers)
	}
	if without.PublicIP != with.PublicIP || without.PublicIPv6 != with.PublicIPv6 || without.InContainer != with.InContainer {
		t.Errorf("the two looks differ in more than STUN: %+v, %+v", with, without)
	}

	// Without Deps.STUN the real client asks; there is one whenever STUN is used.
	s, err = New(loadFlags(t), discardLog(), Deps{})
	if err != nil {
		t.Fatal(err)
	}
	if o := s.detectOptions(true); o.STUN == nil || len(o.STUNServers) == 0 || !o.IPv6 {
		t.Errorf("default detection options = %+v", o)
	}

	// The looks of a running server ask STUN only while public_ip is "auto": an operator who set the address gets
	// the one NAT check at startup and no packets to third parties afterwards (04 §7.4).
	for publicIP, want := range map[string]bool{
		"":            true, // not set: the default, auto
		"auto":        true,
		"203.0.113.7": false,
		"2001:db8::7": false, // an IPv6-only server: the literal is the operator's word all the same
	} {
		var flags []string
		if publicIP != "" {
			flags = append(flags, "--public-ip="+publicIP)
		}
		s, err := New(loadFlags(t, flags...), discardLog(), Deps{})
		if err != nil {
			t.Fatal(err)
		}
		if got := s.redetectWithSTUN(); got != want {
			t.Errorf("public_ip = %q: later looks with STUN = %v, want %v", publicIP, got, want)
		}
	}
}

// fakeSTUN is a STUN client that is never asked.
type fakeSTUN struct{}

func (fakeSTUN) Mapped(context.Context, net.PacketConn, string) (netip.AddrPort, error) {
	return netip.AddrPort{}, errors.New("not asked in this test")
}
