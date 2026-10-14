package doctor

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"testing"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/netx"
)

// The two rules of public_ip that README F21 reviewed (04 §13.2): symmetric NAT in a container on a bridge network,
// and an address that only this machine can reach.

// bridgeWorld turns the world into a Docker container on a bridge network: the default route's interface is one end
// of a veth pair (its iflink is the other end's index), with the container's private address. A running server
// found that address at its start and saw its public address through STUN, with this NAT kind.
func bridgeWorld(w *world, nat api.NATKind) {
	w.t.Helper()
	w.container("docker", true)
	w.files["sys/class/net/eth0/iflink"].Data = []byte("17\n")
	w.env.Interfaces = &fakeIfaces{ifs: []netx.Interface{iface("lo", "127.0.0.1"), iface("eth0", "172.18.0.2")}, route: "172.18.0.2"}
	if w.live != nil {
		w.live.LocalIPv4, w.live.PublicIPv4Method, w.live.NAT = "172.18.0.2", "stun", nat
		w.live.PublicIPv6, w.live.PublicIPv6Method = "", ""
	}
}

// hostNetworkWorld turns the world into a container on the host's network: the interface is the host's own.
func hostNetworkWorld(w *world, nat api.NATKind) {
	w.t.Helper()
	w.container("docker", true)
	w.env.Interfaces = &fakeIfaces{ifs: []netx.Interface{iface("lo", "127.0.0.1"), iface("eth0", "192.168.1.20")}, route: "192.168.1.20"}
	w.live.LocalIPv4, w.live.PublicIPv4Method, w.live.NAT = "192.168.1.20", "stun", nat
	w.live.PublicIPv6, w.live.PublicIPv6Method = "", ""
}

// bridgeNote is the line public_ip adds where it lets a bridge's symmetric NAT pass.
const bridgeNote = "\nthe container's bridge network gives each outgoing connection its own port (symmetric NAT), which " +
	"doesn't matter here: friends reach the published ports"

// Rule (a): in a container on a bridge network, symmetric NAT is no failure. Friends reach the ports the runtime
// publishes, which carry the media whatever the bridge's NAT does with connections that start inside the container.
// On the host's network, in a container whose network doctor can't tell, and outside a container, symmetric NAT
// is the NAT in front of the machine and stays a failure.
func TestPublicIPBridgeSymmetric(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		setup   func(*world)
		status  api.DoctorStatus
		code    string
		fix     string
		message string // "" = not compared
		bridge  bool   // the param dockerBridge
	}{
		{
			name:   "Docker bridge, symmetric NAT, address seen by STUN",
			args:   []string{"--network.ipv6=false"},
			setup:  func(w *world) { bridgeWorld(w, api.NATKindSymmetric) },
			status: api.DoctorStatusOK, code: codePublicIPOK, bridge: true,
			message: testIP + " (seen by STUN)" + bridgeNote,
		},
		{
			name: "Docker bridge, symmetric NAT, address set in the config",
			args: []string{"--network.ipv6=false", "--public-ip", testIP},
			setup: func(w *world) {
				bridgeWorld(w, api.NATKindSymmetric)
				w.live.PublicIPv4Method = "config"
			},
			status: api.DoctorStatusOK, code: codePublicIPOK, bridge: true,
			message: testIP + " (set in the config)" + bridgeNote,
		},
		{
			name:   "Docker bridge, symmetric NAT, no IPv6 (the bridge has none)",
			setup:  func(w *world) { bridgeWorld(w, api.NATKindSymmetric) },
			status: api.DoctorStatusInfo, code: codePublicIPNoIPv6, bridge: true,
			message: testIP + " (seen by STUN); IPv6 media off (Docker bridge)" + bridgeNote,
		},
		{
			name: "Docker bridge, symmetric NAT, ip mode on AWS",
			args: []string{"--network.ipv6=false"},
			setup: func(w *world) {
				bridgeWorld(w, api.NATKindSymmetric)
				w.files["sys/class/dmi/id/sys_vendor"].Data = []byte("Amazon EC2\n")
			},
			status: api.DoctorStatusInfo, code: codePublicIPEphemeral, fix: fixPublicIPStatic, bridge: true,
		},
		{
			name:   "Docker bridge, 1:1 NAT, as before",
			args:   []string{"--network.ipv6=false"},
			setup:  func(w *world) { bridgeWorld(w, api.NATKindOneToOne) },
			status: api.DoctorStatusOK, code: codePublicIPOK,
			message: testIP + " (seen by STUN, 1:1 NAT from 172.18.0.2)",
		},
		{
			name: "Docker bridge, carrier-grade NAT stays a failure",
			setup: func(w *world) {
				bridgeWorld(w, api.NATKindCGNATLikely)
				w.live.LocalIPv4 = "100.72.3.4"
			},
			status: api.DoctorStatusFail, code: codePublicIPCGNAT, fix: fixPublicIPNAT,
		},
		{
			name:   "container on the host's network, symmetric NAT",
			setup:  func(w *world) { hostNetworkWorld(w, api.NATKindSymmetric) },
			status: api.DoctorStatusFail, code: codePublicIPSymmetric, fix: fixPublicIPNAT,
		},
		{
			name: "container whose network doctor can't tell, symmetric NAT",
			setup: func(w *world) {
				bridgeWorld(w, api.NATKindSymmetric)
				delete(w.files, "sys/class/net/eth0/iflink")
			},
			status: api.DoctorStatusFail, code: codePublicIPSymmetric, fix: fixPublicIPNAT,
		},
		{
			name: "no container, symmetric NAT",
			setup: func(w *world) {
				w.live.PublicIPv4Method, w.live.NAT, w.live.LocalIPv4 = "stun", api.NATKindSymmetric, "192.168.1.20"
			},
			status: api.DoctorStatusFail, code: codePublicIPSymmetric, fix: fixPublicIPNAT,
			message: testIP + " (seen by STUN), " + testIPv6 + "; the NAT in front gives every connection another " +
				"port (symmetric NAT), so friends probably can't connect",
		},
		{
			// The veth pair is there, but the interface files say the host's network: the rule follows them.
			name: "outside a container, a veth interface does not count",
			setup: func(w *world) {
				bridgeWorld(w, api.NATKindSymmetric)
				w.env.Host = config.Host{Environ: []string{}, Root: w.t.TempDir()}
			},
			status: api.DoctorStatusFail, code: codePublicIPSymmetric, fix: fixPublicIPNAT,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t, append([]string{"--tls.mode", "ip"}, tt.args...)...)
			tt.setup(w)
			stun := &fakeSTUN{}
			w.env.STUN = stun
			c := w.check("public_ip")
			want(t, c, tt.status, tt.code, tt.fix)
			if tt.message != "" && c.Message != tt.message {
				t.Errorf("message:\n got %q\nwant %q", c.Message, tt.message)
			}
			if got := c.Params["dockerBridge"] == true; got != tt.bridge {
				t.Errorf("dockerBridge = %v, want %v (params %v)", c.Params["dockerBridge"], tt.bridge, c.Params)
			}
			// The NAT kind is reported as the server measured it.
			if c.Params != nil && c.Params["nat"] != string(w.live.NAT) {
				t.Errorf("nat = %v, want %s", c.Params["nat"], w.live.NAT)
			}
			if stun.calls != 0 {
				t.Error("with a running server the check asked a STUN server")
			}
		})
	}
}

// parallelSTUN answers Mapped from a table by server, like fakeSTUN, and may be asked from several goroutines at once:
// the detection asks every STUN server in parallel (04 §7.4).
type parallelSTUN struct {
	answers map[string]string
}

func (f *parallelSTUN) Mapped(_ context.Context, _ net.PacketConn, server string) (netip.AddrPort, error) {
	a, ok := f.answers[server]
	if !ok {
		return netip.AddrPort{}, errors.New("no answer")
	}
	return netip.MustParseAddrPort(a), nil
}

// Offline, the same rule applies to doctor's own detection: two STUN servers that see the container's bridge NAT
// map it to two ports.
func TestPublicIPBridgeSymmetricOffline(t *testing.T) {
	const a, b = "stun-a.example.net:3478", "stun-b.example.net:3478"
	answers := map[string]string{a: testIP + ":40000", b: testIP + ":40001"}
	run := func(t *testing.T, bridge bool) api.DoctorCheck {
		w := newWorld(t, "--tls.mode", "ip", "--network.ipv6=false", "--network.stun-servers", a+","+b)
		w.live = nil
		if bridge {
			bridgeWorld(w, "")
		} else {
			w.files = withoutDMI(w)
			w.env.Interfaces = &fakeIfaces{ifs: []netx.Interface{iface("en0", "192.168.1.20")}, route: "192.168.1.20"}
		}
		w.env.STUN = &parallelSTUN{answers: answers}
		return w.check("public_ip")
	}
	t.Run("Docker bridge", func(t *testing.T) {
		c := run(t, true)
		want(t, c, api.DoctorStatusOK, codePublicIPOK, "")
		if c.Params["nat"] != string(api.NATKindSymmetric) || c.Params["dockerBridge"] != true {
			t.Errorf("params %v", c.Params)
		}
		if want := testIP + " (seen by STUN)" + bridgeNote; c.Message != want {
			t.Errorf("message:\n got %q\nwant %q", c.Message, want)
		}
	})
	t.Run("a home server", func(t *testing.T) {
		c := run(t, false)
		want(t, c, api.DoctorStatusFail, codePublicIPSymmetric, fixPublicIPNAT)
		if c.Params["dockerBridge"] != nil {
			t.Errorf("params %v", c.Params)
		}
	})
}

// Rule (b): an address that only this machine can reach is no public address. A public_ip literal on the loopback
// interface (the smoke test's ISSHONI_PUBLIC_IP=127.0.0.1, 06 §11.3) or the unspecified address is reported in the
// param unreachable and judged as no address at all, whatever the NAT kind; dns compares the domain with no address
// either. Another private address (a LAN setup) is still the server's address.
func TestPublicIPUnreachable(t *testing.T) {
	manualDomain := []string{"--domain", testDomain, "--tls.mode", "manual", "--tls.cert-file", "/etc/ssl/c.pem",
		"--tls.key-file", "/etc/ssl/k.pem"}
	manualNoDomain := []string{"--tls.mode", "manual", "--tls.cert-file", "/etc/ssl/c.pem", "--tls.key-file", "/etc/ssl/k.pem"}
	const (
		siteOpens = "\nthe site opens, but friends can't share or watch: media has no public address to offer"
		fixHost   = "set public_ip to this server's public address, or to auto to detect it, then restart isshoni: " +
			"sudo systemctl restart isshoni"
	)
	// v4 and v6 are what the running server found: the address and its method.
	type addr struct{ ip, method string }
	tests := []struct {
		name        string
		args        []string
		v4, v6      addr
		nat         api.NATKind
		docker      bool
		status      api.DoctorStatus
		code        string
		fix         string
		unreachable []string
		message     string
		fixText     string // "" = not compared
	}{
		{
			name: "127.0.0.1 set in the config", args: manualDomain,
			v4: addr{"127.0.0.1", "config"}, nat: api.NATKindOneToOne,
			status: api.DoctorStatusFail, code: codePublicIPNone, fix: fixPublicIPSet, unreachable: []string{"127.0.0.1"},
			message: "no public IP address: 127.0.0.1 can only be reached from this machine" + siteOpens,
			fixText: fixHost,
		},
		{
			name: "127.0.0.1, and STUN saw symmetric NAT", args: manualDomain,
			v4: addr{"127.0.0.1", "config"}, nat: api.NATKindSymmetric,
			status: api.DoctorStatusFail, code: codePublicIPNone, fix: fixPublicIPSet, unreachable: []string{"127.0.0.1"},
		},
		{
			name: "any loopback address", args: manualDomain,
			v4: addr{"127.3.2.1", "config"}, nat: api.NATKindUnknown,
			status: api.DoctorStatusFail, code: codePublicIPNone, fix: fixPublicIPSet, unreachable: []string{"127.3.2.1"},
		},
		{
			name: "the unspecified address", args: manualDomain,
			v4: addr{"0.0.0.0", "config"}, nat: api.NATKindUnknown,
			status: api.DoctorStatusFail, code: codePublicIPNone, fix: fixPublicIPSet, unreachable: []string{"0.0.0.0"},
		},
		{
			name: "::1 as the only address", args: manualDomain,
			v6: addr{"::1", "config"}, nat: api.NATKindUnknown,
			status: api.DoctorStatusFail, code: codePublicIPNone, fix: fixPublicIPSet, unreachable: []string{"::1"},
			message: "no public IP address: ::1 can only be reached from this machine" + siteOpens,
		},
		{
			name: "both families on the loopback interface", args: manualDomain,
			v4: addr{"127.0.0.1", "config"}, v6: addr{"::1", "config"}, nat: api.NATKindUnknown,
			status: api.DoctorStatusFail, code: codePublicIPNone, fix: fixPublicIPSet, unreachable: []string{"127.0.0.1", "::1"},
			message: "no public IP address: 127.0.0.1 and ::1 can only be reached from this machine" + siteOpens,
		},
		{
			name: "the site is that address", args: manualNoDomain,
			v4: addr{"127.0.0.1", "config"}, nat: api.NATKindUnknown,
			status: api.DoctorStatusFail, code: codePublicIPNone, fix: fixPublicIPSet, unreachable: []string{"127.0.0.1"},
			message: "no public IP address: 127.0.0.1 can only be reached from this machine\n" +
				"the site is at that address too, so only this machine can open it",
		},
		{
			name: "in a container", args: manualDomain, docker: true,
			v4: addr{"127.0.0.1", "config"}, nat: api.NATKindOneToOne,
			status: api.DoctorStatusFail, code: codePublicIPNone, fix: fixPublicIPSet, unreachable: []string{"127.0.0.1"},
			fixText: "set ISSHONI_PUBLIC_IP in .env to this server's public address, or leave it empty to detect it, " +
				"then run docker compose up -d",
		},
		{
			name: "127.0.0.1 and a public IPv6 address", args: manualDomain,
			v4: addr{"127.0.0.1", "config"}, v6: addr{testIPv6, "interface"}, nat: api.NATKindUnknown,
			status: api.DoctorStatusWarn, code: codePublicIPv6Only, unreachable: []string{"127.0.0.1"},
			message: testIPv6 + "; no public IPv4 address (127.0.0.1 can only be reached from this machine), so " +
				"friends without IPv6 can't connect",
		},
		{
			name: "a public IPv4 address and ::1", args: manualDomain,
			v4: addr{testIP, "interface"}, v6: addr{"::1", "config"}, nat: api.NATKindNone,
			status: api.DoctorStatusInfo, code: codePublicIPNoIPv6, unreachable: []string{"::1"},
			message: testIP + " (on interface eth0, confirmed by STUN); no public IPv6 address " +
				"(::1 can only be reached from this machine)",
		},
		{
			name: "a private address is a LAN's, and stays", args: append([]string{"--network.ipv6=false"}, manualDomain...),
			v4: addr{"192.168.1.20", "config"}, nat: api.NATKindNone,
			status: api.DoctorStatusOK, code: codePublicIPOK,
			message: "192.168.1.20 (set in the config)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t, tt.args...)
			if tt.docker {
				w.container("docker", true)
			}
			w.live.PublicIPv4, w.live.PublicIPv4Method = tt.v4.ip, tt.v4.method
			w.live.PublicIPv6, w.live.PublicIPv6Method = tt.v6.ip, tt.v6.method
			w.live.NAT = tt.nat
			c := w.check("public_ip")
			want(t, c, tt.status, tt.code, tt.fix)
			if tt.message != "" && c.Message != tt.message {
				t.Errorf("message:\n got %q\nwant %q", c.Message, tt.message)
			}
			if tt.fixText != "" && c.Fix != tt.fixText {
				t.Errorf("fix:\n got %q\nwant %q", c.Fix, tt.fixText)
			}
			got, _ := c.Params["unreachable"].([]string)
			if !slices.Equal(got, tt.unreachable) {
				t.Errorf("unreachable = %v, want %v", c.Params["unreachable"], tt.unreachable)
			}
			// The address is never reported as the server's.
			for _, key := range []string{"ipv4", "ipv6"} {
				if s, ok := c.Params[key].(string); ok && slices.Contains(tt.unreachable, s) {
					t.Errorf("%s = %s, an address only this machine reaches", key, s)
				}
			}
		})
	}
}

// Offline, doctor's own detection takes the literal as netx gives it, and the check drops it the same way.
func TestPublicIPUnreachableOffline(t *testing.T) {
	w := newWorld(t, "--domain", testDomain, "--tls.mode", "manual", "--tls.cert-file", "/etc/ssl/c.pem",
		"--tls.key-file", "/etc/ssl/k.pem", "--public-ip", "127.0.0.1", "--network.ipv6=false",
		"--network.stun-servers", "stun.example.net:3478")
	w.live = nil
	w.env.STUN = &fakeSTUN{answers: map[string]string{"stun.example.net:3478": testIP + ":40000"}}
	c := w.check("public_ip")
	want(t, c, api.DoctorStatusFail, codePublicIPNone, fixPublicIPSet)
	if got, _ := c.Params["unreachable"].([]string); !slices.Equal(got, []string{"127.0.0.1"}) || c.Params["ipv4"] != nil {
		t.Errorf("params %v", c.Params)
	}
	contains(t, c.Message, "no public IP address: 127.0.0.1 can only be reached from this machine")
}

// dns compares the domain's records with the public addresses, so with a loopback public_ip it has nothing to
// compare them with: dns.unverified (public_ip says why), not dns.ok for a record that points at 127.0.0.1 and not
// dns.wrong for one that points elsewhere. A domain without records is missing as before. These are the smoke
// test's cases: its test domain points nowhere on a CI runner, and somewhere else behind a resolver that answers
// every name.
func TestDNSWithUnreachablePublicIP(t *testing.T) {
	tests := []struct {
		name   string
		hosts  map[string][]string
		status api.DoctorStatus
		code   string
		fix    string
	}{
		{"the domain points to 127.0.0.1", map[string][]string{testDomain: {"127.0.0.1"}}, api.DoctorStatusWarn, codeDNSUnverified, ""},
		{"the domain points elsewhere", map[string][]string{testDomain: {"198.51.100.4"}}, api.DoctorStatusWarn, codeDNSUnverified, ""},
		{"the domain does not resolve", nil, api.DoctorStatusFail, codeDNSMissing, fixDNSAddRecord},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t, "--domain", testDomain, "--tls.mode", "manual", "--tls.cert-file", "/etc/ssl/c.pem",
				"--tls.key-file", "/etc/ssl/k.pem", "--public-ip", "127.0.0.1")
			w.live.PublicIPv4, w.live.PublicIPv4Method, w.live.PublicIPv6, w.live.PublicIPv6Method = "127.0.0.1", "config", "", ""
			w.env.Resolver = &fakeResolver{hosts: tt.hosts}
			c := w.check("dns")
			want(t, c, tt.status, tt.code, tt.fix)
			if c.Params["ip"] != nil {
				t.Errorf("dns compared with %v", c.Params["ip"])
			}
		})
	}
}

func TestOnlyThisMachine(t *testing.T) {
	for addr, wantIt := range map[string]bool{
		"127.0.0.1":        true,
		"127.255.255.254":  true,
		"::1":              true,
		"::ffff:127.0.0.1": true,
		"0.0.0.0":          true,
		"::":               true,
		testIP:             false,
		testIPv6:           false,
		"10.0.0.5":         false,
		"192.168.1.20":     false,
		"172.18.0.2":       false,
		"169.254.1.1":      false,
		"fe80::1":          false,
		"100.72.3.4":       false,
	} {
		if got := onlyThisMachine(netip.MustParseAddr(addr)); got != wantIt {
			t.Errorf("onlyThisMachine(%s) = %v, want %v", addr, got, wantIt)
		}
	}
}
