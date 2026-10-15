package server

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"slices"
	"testing"
	"testing/fstest"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/netx"
	"github.com/MoonWX/isshoni/internal/server/ops/doctor"
)

// README F22: the dashboard and the connection test take their dockerBridge from doctor's public_ip check, run on
// the server's own detection (dockerBridgeNAT). These cases are F21's rule (a) seen from the server: the flag is
// true exactly where the check lets a container bridge's symmetric NAT pass (04 §13.2).

// fakeMachineIfaces is the machine's interface list as doctor reads it.
type fakeMachineIfaces struct {
	ifs   []netx.Interface
	route string // the default route's source address; "" = no route
}

func (f fakeMachineIfaces) Interfaces() ([]netx.Interface, error) { return f.ifs, nil }

func (f fakeMachineIfaces) RouteSource(_ context.Context, dst netip.Addr) (netip.Addr, error) {
	if f.route == "" || !dst.Is4() {
		return netip.Addr{}, errors.New("no route")
	}
	return netip.MustParseAddr(f.route), nil
}

// machineIface is an up interface with these addresses.
func machineIface(name string, addrs ...string) netx.Interface {
	it := netx.Interface{Name: name, Flags: net.FlagUp}
	for _, a := range addrs {
		it.Addrs = append(it.Addrs, netx.InterfaceAddr{Addr: netip.MustParseAddr(a)})
	}
	return it
}

// The container's address on its network, which Start found as the default route's source (LocalV4).
const bridgeLocalV4 = "172.18.0.2"

// containerHost is a Docker container as config.Host detects it, from the variable that Podman and Docker images
// set; the root is empty, so no file of the test machine counts.
func containerHost(t *testing.T) config.Host {
	return config.Host{Environ: []string{"container=docker"}, Root: t.TempDir()}
}

// bridgeEnv is a doctor run inside a server in a Docker container on a bridge network: the default route's
// interface eth0 is one end of a veth pair (its iflink is the other end's index), and the server saw its public
// address through STUN, behind a symmetric NAT (Docker Desktop's user-space NAT gives each destination its own port).
func bridgeEnv(t *testing.T, args ...string) doctor.Env {
	t.Helper()
	return doctor.Env{
		Config: loadFlags(t, append([]string{"--public-url=https://watch.example.com", "--network.ipv6=false"}, args...)...),
		Live: &api.ServerStatus{
			PublicIPv4: "203.0.113.7", PublicIPv4Method: string(netx.MethodSTUN),
			LocalIPv4: bridgeLocalV4, NAT: api.NATKindSymmetric,
		},
		FS: fstest.MapFS{
			"sys/class/net/eth0/ifindex": {Data: []byte("16\n")},
			"sys/class/net/eth0/iflink":  {Data: []byte("17\n")},
		},
		Interfaces: fakeMachineIfaces{
			ifs:   []netx.Interface{machineIface("lo", "127.0.0.1"), machineIface("eth0", bridgeLocalV4)},
			route: bridgeLocalV4,
		},
		Host: containerHost(t),
		GOOS: "linux",
		UID:  os.Getuid(),
		User: "isshoni",
		Now:  func() time.Time { return time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC) },
	}
}

func TestDockerBridgeNAT(t *testing.T) {
	if !slices.Contains(doctor.CheckIDs(), doctorCheckPublicIP) {
		t.Fatalf("doctor has no check %q: %v", doctorCheckPublicIP, doctor.CheckIDs())
	}
	for _, tc := range []struct {
		name    string
		env     func(t *testing.T) doctor.Env
		network string // the container check's params.network: the setup the case is about ("" = no container)
		want    bool
	}{
		{
			name:    "Docker bridge, symmetric NAT, the address seen by STUN",
			env:     func(t *testing.T) doctor.Env { return bridgeEnv(t) },
			network: "bridge", want: true,
		},
		{
			name: "Docker bridge, symmetric NAT, the address set in the config",
			env: func(t *testing.T) doctor.Env {
				env := bridgeEnv(t, "--public-ip=203.0.113.7")
				env.Live.PublicIPv4Method = string(netx.MethodConfig)
				return env
			},
			network: "bridge", want: true,
		},
		{
			name: "Docker bridge, symmetric NAT, IPv6 on (the bridge has none)",
			env: func(t *testing.T) doctor.Env {
				env := bridgeEnv(t)
				env.Config.Network.IPv6 = true
				return env
			},
			network: "bridge", want: true,
		},
		{
			// The NAT in front of the machine itself: friends can't reach it, and doctor fails the check.
			name: "a container on the host's network, symmetric NAT",
			env: func(t *testing.T) doctor.Env {
				env := bridgeEnv(t)
				env.FS = fstest.MapFS{
					"sys/class/net/eth0/ifindex": {Data: []byte("2\n")},
					"sys/class/net/eth0/iflink":  {Data: []byte("2\n")},
				}
				return env
			},
			network: "host",
		},
		{
			name: "a container whose network doctor can't tell",
			env: func(t *testing.T) doctor.Env {
				env := bridgeEnv(t)
				env.FS = fstest.MapFS{}
				return env
			},
			network: "unknown",
		},
		{
			name: "no container, symmetric NAT",
			env: func(t *testing.T) doctor.Env {
				env := bridgeEnv(t)
				env.Host = config.Host{Environ: []string{}, Root: t.TempDir()}
				return env
			},
		},
		{
			name: "Docker bridge, 1:1 NAT (no symmetric NAT to let pass)",
			env: func(t *testing.T) doctor.Env {
				env := bridgeEnv(t)
				env.Live.NAT = api.NATKindOneToOne
				return env
			},
			network: "bridge",
		},
		{
			// F21 left cgnat_likely as it was: a carrier-grade NAT is in front of the host too.
			name: "Docker bridge, carrier-grade NAT",
			env: func(t *testing.T) doctor.Env {
				env := bridgeEnv(t)
				env.Live.NAT = api.NATKindCGNATLikely
				return env
			},
			network: "bridge",
		},
		{
			// F21's rule (b): an address that only this machine can reach is no public address, and the NAT kind
			// then stands in front of none.
			name: "Docker bridge, symmetric NAT, public_ip 127.0.0.1",
			env: func(t *testing.T) doctor.Env {
				env := bridgeEnv(t, "--public-ip=127.0.0.1")
				env.Live.PublicIPv4, env.Live.PublicIPv4Method = "127.0.0.1", string(netx.MethodConfig)
				return env
			},
			network: "bridge",
		},
		{
			name: "Docker bridge, symmetric NAT, no public IPv4 address",
			env: func(t *testing.T) doctor.Env {
				env := bridgeEnv(t)
				env.Live.PublicIPv4, env.Live.PublicIPv4Method = "", ""
				return env
			},
			network: "bridge",
		},
		{
			// A development setup looks for no public address, and public_ip is skipped there (04 §13.2).
			name: "a loopback setup",
			env: func(t *testing.T) doctor.Env {
				return bridgeEnv(t, "--network.include-loopback=true", "--network.stun-servers=")
			},
			network: "bridge",
		},
		{
			name: "no server status",
			env: func(t *testing.T) doctor.Env {
				env := bridgeEnv(t)
				env.Live = nil
				return env
			},
			network: "bridge",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := tc.env(t)
			got := dockerBridgeNAT(context.Background(), env)
			if got != tc.want {
				t.Errorf("dockerBridgeNAT = %v, want %v", got, tc.want)
			}

			// What doctor itself reports with the same environment: the container check sees the network the case
			// is about (so a case can't lose its setup), and public_ip's param dockerBridge is the flag.
			if env.Live == nil {
				return
			}
			rep := doctor.Run(context.Background(), env, api.BandwidthInput{}, []string{"container", doctorCheckPublicIP})
			for _, c := range rep.Checks {
				switch c.ID {
				case "container":
					if network, _ := c.Params["network"].(string); network != tc.network {
						t.Errorf("the container check sees network %q (%s), want %q", network, c.Code, tc.network)
					}
				case doctorCheckPublicIP:
					bridge, _ := c.Params[doctorParamDockerBridge].(bool)
					if bridge != got {
						t.Errorf("public_ip %s has dockerBridge %v, dockerBridgeNAT says %v", c.Code, bridge, got)
					}
					if got && c.Status == api.DoctorStatusFail {
						t.Errorf("public_ip = %s %s, want no failure where it lets the NAT pass", c.Status, c.Code)
					}
				}
			}
		})
	}
}

// TestDockerBridgeNATCancelled: a startup that ends while the check would run gives no flag.
func TestDockerBridgeNATCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if dockerBridgeNAT(ctx, bridgeEnv(t)) {
		t.Error("dockerBridgeNAT = true with a cancelled context")
	}
}
