package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/netip"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server"
	"github.com/MoonWX/isshoni/internal/server/netx"
	"github.com/MoonWX/isshoni/internal/server/ops"
	"github.com/MoonWX/isshoni/internal/server/servertest"
)

// README F22 in a running server: where doctor's public_ip lets a container bridge's symmetric NAT pass (README
// F21, 04 §13.2), the dashboard (04 §11.4) and the connection test (04 §7.7) say so next to nat, from the same look;
// elsewhere neither has the key.

// doctorIfaces is the interface list that doctor in the test server reads.
type doctorIfaces struct{ ifs []netx.Interface }

func (f doctorIfaces) Interfaces() ([]netx.Interface, error) { return f.ifs, nil }

func (f doctorIfaces) RouteSource(context.Context, netip.Addr) (netip.Addr, error) {
	return netip.Addr{}, errors.New("doctor in a running server takes the route's source from the status")
}

// startInContainer starts a server that runs in a Docker container and detected a symmetric NAT in front of its
// public address, the way Docker Desktop's user-space NAT looks from inside the container. Its media sockets are on
// loopback, as in every test, so the container's address (LocalV4) is 127.0.0.1, which doctor's machine holds on
// eth0: a veth end on a bridge network when iflink differs from eth0's index 16, the host's own interface when it
// is 16. The returned servertest.Server is the harness's view of it: URL, Client, AdminSocket and UDPPort.
func startInContainer(t *testing.T, iflink string) *servertest.Server {
	t.Helper()
	// STUN servers configured: a server with none and network.include_loopback is a development setup, which
	// doctor's public_ip skips. The detection itself is the test's (SetPublicAddrs) and asks none of them.
	cfg := testConfig(t, "--network.stun-servers=stun.invalid:3478", "--network.ipv6=false")
	deps := wiredDeps()
	deps.InProcess = true
	deps.Host = testDeps().Host
	deps.Host.Environ = append(deps.Host.Environ, "container=docker")
	s, err := server.New(cfg, discardLog(), deps)
	if err != nil {
		t.Fatal(err)
	}
	s.SetPublicAddrs(netx.PublicAddrs{
		V4: netip.MustParseAddr("203.0.113.7"), V4Method: netx.MethodSTUN,
		LocalV4: netip.MustParseAddr("127.0.0.1"), NAT: api.NATKindSymmetric,
	})
	s.SetDoctorMachine(fstest.MapFS{
		"sys/class/net/eth0/ifindex": {Data: []byte("16\n")},
		"sys/class/net/eth0/iflink":  {Data: []byte(iflink + "\n")},
	}, doctorIfaces{ifs: []netx.Interface{{Name: "eth0", Flags: net.FlagUp,
		Addrs: []netx.InterfaceAddr{{Addr: netip.MustParseAddr("127.0.0.1")}}}}})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx, server.ShutdownStop); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	httpAddr := s.Addrs().HTTP.String()
	client := &http.Client{Jar: jar, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", httpAddr)
		},
		DisableCompression: true,
	}}
	t.Cleanup(client.CloseIdleConnections)
	return &servertest.Server{
		URL: s.Site().Origin, Client: client, AdminSocket: cfg.Listen.AdminSocket,
		UDPPort: s.Addrs().ICEUDP.(*net.UDPAddr).Port, Cfg: cfg, Srv: s,
	}
}

// TestDockerBridgeInServer: in a container on a bridge network, doctor's public_ip lets the symmetric NAT pass with
// dockerBridge, and the dashboard's server object and the connection test's carry dockerBridge: true next to nat
// symmetric. On the host's network the same NAT fails the check, and neither object has the key.
func TestDockerBridgeInServer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		iflink  string
		network string
		bridge  bool
	}{
		{name: "a bridge network", iflink: "17", network: "bridge", bridge: true},
		{name: "the host's network", iflink: "16", network: "host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := startInContainer(t, tc.iflink)
			setUp(t, srv)

			// Doctor, as `isshoni doctor` asks the running server.
			admin, ctx := adminSocket(t, srv)
			rep, err := admin.Doctor(ctx, ops.AdminDoctorRequest{Only: []string{"container", "public_ip"}})
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range rep.Checks {
				switch c.ID {
				case "container":
					if c.Params["network"] != tc.network {
						t.Errorf("container = %s %v, want network %s", c.Code, c.Params, tc.network)
					}
				case "public_ip":
					bridge, _ := c.Params["dockerBridge"].(bool)
					failed := c.Status == api.DoctorStatusFail
					if bridge != tc.bridge || failed == tc.bridge {
						t.Errorf("public_ip = %s %s %v, want dockerBridge %v and a failure only without it",
							c.Status, c.Code, c.Params, tc.bridge)
					}
				}
			}

			// The dashboard.
			res := call(t, srv, http.MethodGet, "/api/v1/admin/dashboard", nil)
			if res.status != http.StatusOK {
				t.Fatalf("GET /admin/dashboard = %d %s", res.status, res.body)
			}
			var dash api.OpsDashboard
			if err := json.Unmarshal([]byte(res.body), &dash); err != nil {
				t.Fatal(err)
			}
			if dash.Server.NAT != api.NATKindSymmetric || dash.Server.DockerBridge != tc.bridge {
				t.Errorf("dashboard server: nat %q, dockerBridge %v; want symmetric, %v", dash.Server.NAT,
					dash.Server.DockerBridge, tc.bridge)
			}
			if has := strings.Contains(res.body, `"dockerBridge"`); has != tc.bridge {
				t.Errorf("the dashboard has a dockerBridge key: %v, want %v", has, tc.bridge)
			}

			// The connection test's server object, which every answer carries.
			pc := newProbeClient(t, false)
			offer, err := pc.Offer()
			if err != nil {
				t.Fatal(err)
			}
			res = postConnTest(t, srv, srv.Client, api.TransportUDP, offer)
			if res.status != http.StatusOK {
				t.Fatalf("POST /conntest = %d %s", res.status, res.body)
			}
			var ct api.ConnTestResponse
			if err := json.Unmarshal([]byte(res.body), &ct); err != nil {
				t.Fatal(err)
			}
			want := api.ConnTestServerInfo{
				PublicIP: "203.0.113.7", Provider: ct.Server.Provider, Container: api.ContainerKindDocker,
				UDPPort: srv.UDPPort, TCPPorts: ct.Server.TCPPorts, NAT: api.NATKindSymmetric, DockerBridge: tc.bridge,
			}
			if ct.Server.PublicIP != want.PublicIP || ct.Server.Container != want.Container ||
				ct.Server.UDPPort != want.UDPPort || ct.Server.NAT != want.NAT || ct.Server.DockerBridge != want.DockerBridge {
				t.Errorf("conntest server = %+v, want %+v", ct.Server, want)
			}
			if has := strings.Contains(res.body, `"dockerBridge"`); has != tc.bridge {
				t.Errorf("the connection test's answer has a dockerBridge key: %v, want %v", has, tc.bridge)
			}
		})
	}
}
