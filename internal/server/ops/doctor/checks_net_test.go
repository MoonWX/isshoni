package doctor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/url"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/netx"
)

// public_ip with a running server reports the server's own detection, one case per NAT kind of 04 §7.4.
func TestCheckPublicIPLive(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		live    func(*api.ServerStatus)
		status  api.DoctorStatus
		code    string
		fix     string
		message string
	}{
		{
			name:   "on an interface",
			status: api.DoctorStatusOK, code: codePublicIPOK,
			message: "203.0.113.7 (on interface eth0, confirmed by STUN), 2001:db8::7",
		},
		{
			name:   "set in the config",
			live:   func(s *api.ServerStatus) { s.PublicIPv4Method = "config" },
			status: api.DoctorStatusOK, code: codePublicIPOK,
			message: "203.0.113.7 (set in the config), 2001:db8::7",
		},
		{
			name:   "on an interface without a STUN answer",
			live:   func(s *api.ServerStatus) { s.NAT = api.NATKindUnknown },
			status: api.DoctorStatusOK, code: codePublicIPOK,
			message: "203.0.113.7 (on interface eth0), 2001:db8::7",
		},
		{
			name: "1:1 NAT of a cloud",
			live: func(s *api.ServerStatus) {
				s.PublicIPv4Method, s.NAT, s.LocalIPv4 = "stun", api.NATKindOneToOne, "10.0.0.5"
			},
			status: api.DoctorStatusOK, code: codePublicIPOK,
			message: "203.0.113.7 (seen by STUN, 1:1 NAT from 10.0.0.5), 2001:db8::7",
		},
		{
			name:   "no IPv6",
			live:   func(s *api.ServerStatus) { s.PublicIPv6, s.PublicIPv6Method = "", "" },
			status: api.DoctorStatusInfo, code: codePublicIPNoIPv6,
			message: "203.0.113.7 (on interface eth0, confirmed by STUN); no public IPv6 address",
		},
		{
			name:   "no IPv6, and none wanted",
			args:   []string{"--network.ipv6=false"},
			live:   func(s *api.ServerStatus) { s.PublicIPv6, s.PublicIPv6Method = "", "" },
			status: api.DoctorStatusOK, code: codePublicIPOK,
		},
		{
			name: "behind a home router",
			live: func(s *api.ServerStatus) {
				s.PublicIPv4Method, s.NAT, s.LocalIPv4 = "stun", api.NATKindPortForward, "192.168.1.20"
			},
			status: api.DoctorStatusWarn, code: codePublicIPPortForward, fix: fixPublicIPForward,
			message: "203.0.113.7 (seen by STUN), 2001:db8::7; this machine is 192.168.1.20 behind a router",
		},
		{
			name: "symmetric NAT",
			live: func(s *api.ServerStatus) {
				s.PublicIPv4Method, s.NAT, s.LocalIPv4 = "stun", api.NATKindSymmetric, "192.168.1.20"
			},
			status: api.DoctorStatusFail, code: codePublicIPSymmetric, fix: fixPublicIPNAT,
		},
		{
			name: "carrier-grade NAT",
			live: func(s *api.ServerStatus) {
				s.PublicIPv4Method, s.NAT, s.LocalIPv4 = "stun", api.NATKindCGNATLikely, "100.72.3.4"
			},
			status: api.DoctorStatusFail, code: codePublicIPCGNAT, fix: fixPublicIPNAT,
			message: "this machine's address 100.72.3.4 is carrier-grade NAT (100.64.0.0/10): friends can't connect from outside",
		},
		{
			name: "IPv6 only",
			live: func(s *api.ServerStatus) {
				s.PublicIPv4, s.PublicIPv4Method, s.LocalIPv4, s.NAT = "", "", "", api.NATKindUnknown
			},
			status: api.DoctorStatusWarn, code: codePublicIPv6Only,
			message: "2001:db8::7; no public IPv4 address, so friends without IPv6 can't connect",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t, append([]string{"--tls.mode", "ip"}, tt.args...)...)
			if tt.live != nil {
				tt.live(w.live)
			}
			stun := &fakeSTUN{}
			w.env.STUN = stun
			c := w.check("public_ip")
			want(t, c, tt.status, tt.code, tt.fix)
			if tt.message != "" && c.Message != tt.message {
				t.Errorf("message:\n got %q\nwant %q", c.Message, tt.message)
			}
			if stun.calls != 0 {
				t.Error("with a running server the check asked a STUN server")
			}
		})
	}

	// The fix of the router case names the ports of this configuration and the machine to forward them to.
	w := newWorld(t, "--tls.mode", "ip")
	w.live.PublicIPv4Method, w.live.NAT, w.live.LocalIPv4 = "stun", api.NATKindPortForward, "192.168.1.20"
	if c := w.check("public_ip"); c.Fix != "on your router, forward TCP 80, 443, 7882 and UDP 7882 to 192.168.1.20" {
		t.Errorf("fix %q", c.Fix)
	}
}

// Offline, public_ip runs the detection of 04 §7.4 itself, with the injected STUN client and interfaces.
func TestCheckPublicIPOffline(t *testing.T) {
	const stunServer = "stun.example.net:3478"
	offline := func(t *testing.T, args ...string) *world {
		w := newWorld(t, append([]string{"--tls.mode", "ip", "--network.stun-servers", stunServer}, args...)...)
		w.live = nil
		return w
	}
	t.Run("a home server", func(t *testing.T) {
		w := offline(t)
		w.files = withoutDMI(w)
		w.env.Interfaces = &fakeIfaces{ifs: []netx.Interface{iface("en0", "192.168.1.20")}, route: "192.168.1.20"}
		w.env.STUN = &fakeSTUN{answers: map[string]string{stunServer: "198.51.100.9:40000"}}
		c := w.check("public_ip")
		want(t, c, api.DoctorStatusWarn, codePublicIPPortForward, fixPublicIPForward)
		contains(t, c.Message, "198.51.100.9 (seen by STUN)", "this machine is 192.168.1.20 behind a router")
	})
	t.Run("a cloud with 1:1 NAT", func(t *testing.T) {
		w := offline(t, "--network.ipv6=false")
		w.files["sys/class/dmi/id/sys_vendor"].Data = []byte("Amazon EC2\n")
		w.env.Interfaces = &fakeIfaces{ifs: []netx.Interface{iface("ens5", "10.0.0.5")}, route: "10.0.0.5"}
		w.env.STUN = &fakeSTUN{answers: map[string]string{stunServer: "198.51.100.9:7882"}}
		// ip mode on AWS: the address is fine and will not stay (below).
		c := w.check("public_ip")
		want(t, c, api.DoctorStatusInfo, codePublicIPEphemeral, fixPublicIPStatic)
		contains(t, c.Message, "198.51.100.9 (seen by STUN, 1:1 NAT from 10.0.0.5)")
	})
	t.Run("a literal asks no STUN server when none is configured", func(t *testing.T) {
		w := newWorld(t, "--tls.mode", "ip", "--public-ip", testIP, "--network.stun-servers", "")
		w.live = nil
		stun := &fakeSTUN{}
		w.env.STUN = stun
		c := w.check("public_ip")
		want(t, c, api.DoctorStatusOK, codePublicIPOK, "")
		contains(t, c.Message, testIP+" (set in the config)")
		if stun.calls != 0 {
			t.Errorf("%d STUN queries with network.stun_servers = []", stun.calls)
		}
	})
}

// withoutDMI returns the world's files without the DMI strings: a machine that is no cloud instance.
func withoutDMI(w *world) map[string]*fsFile {
	out := map[string]*fsFile{}
	for name, f := range w.files {
		if !strings.HasPrefix(name, "sys/class/dmi/") {
			out[name] = f
		}
	}
	return out
}

// No public address at all (04 §13.2, decided after group 5): public_ip fails with this state's own words, which
// differ by what the missing address costs, and tls in ip mode fails without waiting ten minutes.
func TestNoPublicAddress(t *testing.T) {
	none := func(s *api.ServerStatus) {
		s.PublicIPv4, s.PublicIPv4Method, s.PublicIPv6, s.PublicIPv6Method, s.LocalIPv4 = "", "", "", "", ""
		s.NAT = api.NATKindUnknown
		s.Origin = ""
		s.TLS = api.TLSInfo{Mode: api.TLSModeIP, Names: []string{}}
		s.UptimeS = 30
	}
	const first = "no public IP address found (no public address on an interface, no STUN answer)"

	t.Run("ip mode, running", func(t *testing.T) {
		w := newWorld(t, "--tls.mode", "ip")
		none(w.live)
		rep := Run(t.Context(), w.build(), api.BandwidthInput{}, []string{"public_ip", "tls"})
		pub, cert := rep.Checks[0], rep.Checks[1]
		want(t, pub, api.DoctorStatusFail, codePublicIPNone, fixPublicIPSet)
		if want := first + "\nisshoni is running but not ready, and nobody can open it"; pub.Message != want {
			t.Errorf("message:\n got %q\nwant %q", pub.Message, want)
		}
		wantFix := "set public_ip to this server's public address, or set domain, then restart isshoni\n" +
			"(if the network wasn't up yet when isshoni started: sudo systemctl restart isshoni)"
		if pub.Fix != wantFix {
			t.Errorf("fix:\n got %q\nwant %q", pub.Fix, wantFix)
		}
		// tls: no certificate, no ACME error to name, and no ten minutes to wait for (the server is 30 s old).
		want(t, cert, api.DoctorStatusFail, codeTLSNoPublicIP, "")
		if want := "no certificate: there is no public IP address to get one for (see public_ip)"; cert.Message != want {
			t.Errorf("tls message %q, want %q", cert.Message, want)
		}

		// The text output of 04 §13.2.
		var b strings.Builder
		RenderText(&b, rep, false)
		_, lines, _ := strings.Cut(b.String(), "\n")
		wantText := `[fail] public_ip     no public IP address found (no public address on an interface, no STUN answer)
       isshoni is running but not ready, and nobody can open it
       fix: set public_ip to this server's public address, or set domain, then restart isshoni
            (if the network wasn't up yet when isshoni started: sudo systemctl restart isshoni)
       more: https://moonwx.github.io/isshoni/troubleshooting#doctor-public_ip
[fail] tls           no certificate: there is no public IP address to get one for (see public_ip)
       more: https://moonwx.github.io/isshoni/troubleshooting#doctor-tls
0 ok · 0 warn · 2 fail
`
		if lines != wantText {
			t.Errorf("text output:\n%s\nwant:\n%s", lines, wantText)
		}
	})
	t.Run("ip mode, offline", func(t *testing.T) {
		w := newWorld(t, "--tls.mode", "ip", "--network.stun-servers", "stun.example.net:3478")
		w.live = nil
		w.env.Interfaces = &fakeIfaces{ifs: []netx.Interface{iface("eth0", "10.0.0.5")}, route: "10.0.0.5"}
		pub := w.check("public_ip")
		want(t, pub, api.DoctorStatusFail, codePublicIPNone, fixPublicIPSet)
		contains(t, pub.Message, first+"\nisshoni will start, but it won't be ready")
	})
	t.Run("manual mode without a domain is the same", func(t *testing.T) {
		w := newWorld(t, "--tls.mode", "manual", "--tls.cert-file", "/etc/ssl/c.pem", "--tls.key-file", "/etc/ssl/k.pem")
		none(w.live)
		contains(t, w.check("public_ip").Message, "isshoni is running but not ready, and nobody can open it")
	})
	t.Run("with a domain the site opens", func(t *testing.T) {
		w := newWorld(t, "--domain", testDomain)
		none(w.live)
		w.live.TLS = api.TLSInfo{Mode: api.TLSModeAuto, Names: []string{testDomain}, Ready: true, NotAfter: testNow.Add(60 * 24 * time.Hour)}
		pub := w.check("public_ip")
		want(t, pub, api.DoctorStatusFail, codePublicIPNone, fixPublicIPSet)
		contains(t, pub.Message, first+"\nthe site opens, but nobody can share or watch")
		// The certificate does not wait for the address.
		want(t, w.check("tls"), api.DoctorStatusOK, codeTLSOK, "")
	})
	t.Run("in a container the fix is the container's", func(t *testing.T) {
		w := newWorld(t, "--tls.mode", "ip")
		none(w.live)
		w.container("docker", true)
		wantFix := "set ISSHONI_PUBLIC_IP in .env to this server's public address, or set a domain, then run docker compose up -d\n" +
			"(if the network wasn't up yet when isshoni started: docker compose restart)"
		if got := w.check("public_ip").Fix; got != wantFix {
			t.Errorf("fix:\n got %q\nwant %q", got, wantFix)
		}
	})
}

// ip mode on AWS and GCP gets the note about the address that changes on stop/start, with the provider's install
// page; auto mode and other providers do not (04 §13.2, §17).
func TestPublicIPEphemeral(t *testing.T) {
	tests := []struct {
		vendor   string
		args     []string
		wantInfo bool
		fix      string
	}{
		{"Amazon EC2", []string{"--tls.mode", "ip"}, true, "Attach an Elastic IP (AWS) to this instance, or use a domain"},
		{"Google", []string{"--tls.mode", "ip"}, true, "Reserve a static external IP (GCP) for this instance, or use a domain"},
		{"Amazon EC2", []string{"--domain", testDomain}, false, ""},
		{"Hetzner", []string{"--tls.mode", "ip"}, false, ""},
		{"DigitalOcean", []string{"--tls.mode", "ip"}, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.vendor+" "+strings.Join(tt.args, " "), func(t *testing.T) {
			w := newWorld(t, tt.args...)
			w.files["sys/class/dmi/id/sys_vendor"].Data = []byte(tt.vendor + "\n")
			w.files["sys/class/dmi/id/bios_vendor"].Data = []byte(tt.vendor + "\n")
			rep := Run(t.Context(), w.build(), api.BandwidthInput{}, []string{"public_ip"})
			c := rep.Checks[0]
			var b strings.Builder
			RenderText(&b, rep, false)
			link := "       https://moonwx.github.io/isshoni/install/vps#" + string(rep.Env.Provider) + "\n"
			if !tt.wantInfo {
				want(t, c, api.DoctorStatusOK, codePublicIPOK, "")
				if strings.Contains(b.String(), "install/vps#") {
					t.Errorf("a result without the note links to the provider's page:\n%s", b.String())
				}
				return
			}
			want(t, c, api.DoctorStatusInfo, codePublicIPEphemeral, fixPublicIPStatic)
			contains(t, c.Message, "The default public IPv4 changes on stop/start; in IP mode that moves the server's "+
				"address and breaks every link, installed app and push subscription")
			if c.Fix != tt.fix {
				t.Errorf("fix %q, want %q", c.Fix, tt.fix)
			}
			contains(t, b.String(), link)
			if strings.Contains(b.String(), "more: ") {
				t.Errorf("an info result got a troubleshooting link:\n%s", b.String())
			}
		})
	}
}

func TestCheckDNS(t *testing.T) {
	const other, otherV6 = "198.51.100.4", "2001:db8::bad"
	auto := []string{"--domain", testDomain}
	tests := []struct {
		name     string
		args     []string
		hosts    map[string][]string
		lookup   error
		live     func(*api.ServerStatus)
		caa      []CAARecord
		caaErr   error
		status   api.DoctorStatus
		code     string
		fix      string
		message  string
		fixText  string
		wantCAA  string
		noLookup bool
	}{
		{name: "ip mode has no domain", args: []string{"--tls.mode", "ip"}, status: api.DoctorStatusOK, code: codeDNSNoDomain, noLookup: true},
		{name: "off mode is the proxy's", args: []string{"--tls.mode", "off"}, status: api.DoctorStatusOK, code: codeDNSProxy, noLookup: true},
		{
			name: "points here", args: auto, hosts: map[string][]string{testDomain: {testIP, testIPv6}},
			status: api.DoctorStatusOK, code: codeDNSOK, wantCAA: "none",
			message: "watch.example.com → 203.0.113.7 and 2001:db8::7 (this server)",
		},
		{
			name: "CAA allows Let's Encrypt", args: auto, hosts: map[string][]string{testDomain: {testIP}},
			live:   func(s *api.ServerStatus) { s.PublicIPv6 = "" },
			caa:    []CAARecord{{Name: "example.com", Tag: "issue", Value: "letsencrypt.org; validationmethods=http-01"}},
			status: api.DoctorStatusOK, code: codeDNSOK, wantCAA: "allows",
			message: "watch.example.com → 203.0.113.7 (this server); CAA allows letsencrypt.org",
		},
		{
			name: "CAA keeps Let's Encrypt out", args: auto, hosts: map[string][]string{testDomain: {testIP, testIPv6}},
			caa:    []CAARecord{{Name: "example.com", Tag: "issue", Value: "digicert.com"}},
			status: api.DoctorStatusFail, code: codeDNSCAAForbids, fix: fixDNSCAA,
			message: "a CAA record on example.com doesn't allow letsencrypt.org",
		},
		{
			name: "CAA could not be looked up", args: auto, hosts: map[string][]string{testDomain: {testIP, testIPv6}},
			caaErr: errors.New("no resolver"),
			status: api.DoctorStatusOK, code: codeDNSOK, wantCAA: "unchecked",
		},
		{
			name:   "manual mode orders no certificate, so CAA is not asked",
			args:   []string{"--tls.mode", "manual", "--tls.cert-file", "/c.pem", "--tls.key-file", "/k.pem", "--domain", testDomain},
			hosts:  map[string][]string{testDomain: {testIP, testIPv6}},
			caa:    []CAARecord{{Name: "example.com", Tag: "issue", Value: "digicert.com"}},
			status: api.DoctorStatusOK, code: codeDNSOK,
		},
		{
			name: "does not resolve", args: auto,
			status: api.DoctorStatusFail, code: codeDNSMissing, fix: fixDNSAddRecord,
			message: "watch.example.com doesn't resolve", fixText: "add an A record for watch.example.com pointing to 203.0.113.7",
		},
		{
			name: "points elsewhere", args: auto, hosts: map[string][]string{testDomain: {other}},
			status: api.DoctorStatusFail, code: codeDNSWrong, fix: fixDNSPointHere,
			message: "watch.example.com points to 198.51.100.4, but this server is 203.0.113.7",
		},
		{
			name: "points here and elsewhere", args: auto, hosts: map[string][]string{testDomain: {other, testIP}},
			status: api.DoctorStatusWarn, code: codeDNSAlsoElsewhere, fix: fixDNSPointHere,
		},
		{
			name: "AAAA is not this server", args: auto, hosts: map[string][]string{testDomain: {testIP, otherV6}},
			status: api.DoctorStatusWarn, code: codeDNSAAAAWrong, fix: fixDNSAAAA,
			message: "watch.example.com has the AAAA record 2001:db8::bad, which is not this server: browsers that prefer IPv6 end up elsewhere",
			fixText: "point the AAAA record of watch.example.com to 2001:db8::7, or remove it",
		},
		{
			name: "AAAA, but the server has no IPv6", args: auto, hosts: map[string][]string{testDomain: {testIP, otherV6}},
			live:   func(s *api.ServerStatus) { s.PublicIPv6 = "" },
			status: api.DoctorStatusWarn, code: codeDNSAAAAWrong, fix: fixDNSAAAA,
			fixText: "remove the AAAA record of watch.example.com: this server has no public IPv6 address",
		},
		{
			name: "AAAA only", args: auto, hosts: map[string][]string{testDomain: {testIPv6}},
			status: api.DoctorStatusWarn, code: codeDNSNoA, fix: fixDNSAddRecord,
		},
		{
			name: "AAAA only, elsewhere", args: auto, hosts: map[string][]string{testDomain: {otherV6}},
			status: api.DoctorStatusFail, code: codeDNSWrong, fix: fixDNSPointHere,
		},
		{
			name: "the resolver fails", args: auto, lookup: &net.DNSError{Err: "server misbehaving", Name: testDomain, IsTemporary: true},
			status: api.DoctorStatusWarn, code: codeDNSLookupFailed,
			message: "could not look up watch.example.com: server misbehaving",
		},
		{
			name: "no public address to compare with", args: auto, hosts: map[string][]string{testDomain: {other}},
			live:   func(s *api.ServerStatus) { s.PublicIPv4, s.PublicIPv6 = "", "" },
			status: api.DoctorStatusWarn, code: codeDNSUnverified,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t, tt.args...)
			if tt.live != nil {
				tt.live(w.live)
			}
			resolver := &fakeResolver{hosts: tt.hosts, err: tt.lookup}
			w.env.Resolver = resolver
			caaAsked := 0
			w.env.CAA = func(_ context.Context, name string) ([]CAARecord, error) {
				caaAsked++
				if name != testDomain {
					t.Errorf("CAA asked for %q", name)
				}
				return tt.caa, tt.caaErr
			}
			c := w.check("dns")
			want(t, c, tt.status, tt.code, tt.fix)
			if tt.message != "" && c.Message != tt.message {
				t.Errorf("message:\n got %q\nwant %q", c.Message, tt.message)
			}
			if tt.fixText != "" && c.Fix != tt.fixText {
				t.Errorf("fix:\n got %q\nwant %q", c.Fix, tt.fixText)
			}
			if tt.wantCAA != "" && c.Params["caa"] != tt.wantCAA {
				t.Errorf("params.caa = %v, want %s", c.Params["caa"], tt.wantCAA)
			}
			if tt.noLookup && len(resolver.calls) != 0 {
				t.Errorf("the resolver was asked for %v", resolver.calls)
			}
			if !tt.noLookup && !slices.Equal(resolver.calls, []string{testDomain}) {
				t.Errorf("the resolver was asked for %v, want the domain once", resolver.calls)
			}
			if strings.Contains(tt.name, "manual") && caaAsked != 0 {
				t.Error("CAA was looked up in manual mode")
			}
		})
	}

	// Another CA than Let's Encrypt: doctor does not know its CAA name and asks nothing.
	w := newWorld(t, "--domain", testDomain, "--tls.acme-ca", "https://acme.example.net/directory")
	w.env.Resolver = &fakeResolver{hosts: map[string][]string{testDomain: {testIP, testIPv6}}}
	w.env.CAA = func(context.Context, string) ([]CAARecord, error) {
		t.Error("CAA was looked up for another CA")
		return nil, nil
	}
	want(t, w.check("dns"), api.DoctorStatusOK, codeDNSOK, "")
}

func TestCAAAllows(t *testing.T) {
	const le = "letsencrypt.org"
	tests := []struct {
		name string
		recs []CAARecord
		want bool
	}{
		{"no records", nil, true},
		{"the issuer", []CAARecord{{Tag: "issue", Value: "letsencrypt.org"}}, true},
		{"the issuer with parameters", []CAARecord{{Tag: "issue", Value: " LetsEncrypt.org ; accounturi=https://x"}}, true},
		{"one of two", []CAARecord{{Tag: "issue", Value: "digicert.com"}, {Tag: "issue", Value: le}}, true},
		{"another issuer", []CAARecord{{Tag: "issue", Value: "digicert.com"}}, false},
		{"nobody may issue", []CAARecord{{Tag: "issue", Value: ";"}}, false},
		{"only wildcards are restricted", []CAARecord{{Tag: "issuewild", Value: "digicert.com"}}, true},
		{"only a report address", []CAARecord{{Tag: "iodef", Value: "mailto:a@example.com"}}, true},
		{"an unknown critical property", []CAARecord{{Tag: "issue", Value: le}, {Tag: "future", Critical: true}}, false},
		{"an unknown property that is not critical", []CAARecord{{Tag: "future", Value: "x"}}, true},
	}
	for _, tt := range tests {
		if got := caaAllows(tt.recs, le); got != tt.want {
			t.Errorf("%s: caaAllows = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// tls reports the certificate of a running server. Without one it is a note while the first order runs, a warning
// with the cause from the first failed attempt on, and a failure after ten minutes (04 §13.2, §17).
func TestCheckTLS(t *testing.T) {
	noCert := func(code string, uptime time.Duration) func(*api.ServerStatus) {
		return func(s *api.ServerStatus) {
			s.TLS = api.TLSInfo{Mode: api.TLSModeIP, Names: []string{testIP}, LastErrorCode: code}
			if code != "" {
				s.TLS.LastErrorAt = testNow.Add(-10 * time.Second)
			}
			s.UptimeS = int64(uptime / time.Second)
		}
	}
	manual := []string{"--tls.mode", "manual", "--tls.cert-file", "/etc/ssl/c.pem", "--tls.key-file", "/etc/ssl/k.pem", "--domain", testDomain}
	manualCert := func(left time.Duration, ready bool, code string) func(*api.ServerStatus) {
		return func(s *api.ServerStatus) {
			s.TLS = api.TLSInfo{Mode: api.TLSModeManual, Names: []string{testDomain, "www.example.com"}, Ready: ready, LastErrorCode: code}
			if left != 0 {
				s.TLS.NotAfter = testNow.Add(left)
			}
		}
	}
	tests := []struct {
		name    string
		args    []string
		live    func(*api.ServerStatus)
		status  api.DoctorStatus
		code    string
		fix     string
		message string
		fixText string
	}{
		{
			name: "ip certificate", args: []string{"--tls.mode", "ip"},
			status: api.DoctorStatusOK, code: codeTLSOK,
			message: "ip certificate for 203.0.113.7, valid until 2026-10-05 10:00 UTC, renews 2026-10-02",
		},
		{
			name: "off", args: []string{"--tls.mode", "off"}, live: func(s *api.ServerStatus) { s.TLS = api.TLSInfo{Mode: api.TLSModeOff} },
			status: api.DoctorStatusOK, code: codeTLSOff,
		},
		{
			name: "the first order is under way", args: []string{"--tls.mode", "ip"}, live: noCert("", 40*time.Second),
			status: api.DoctorStatusInfo, code: codeTLSPending,
		},
		{
			name: "right after one failed attempt", args: []string{"--tls.mode", "ip"}, live: noCert(tlsACMEUnreachable, 40*time.Second),
			status: api.DoctorStatusWarn, code: codeTLSFailing, fix: tlsACMEUnreachable,
			fixText: "Let's Encrypt couldn't connect to 203.0.113.7 on port 80 or 443. Open TCP 80 and 443 in your cloud firewall",
		},
		{
			name: "still failing after ten minutes", args: []string{"--tls.mode", "ip"}, live: noCert(tlsACMEUnreachable, 10*time.Minute),
			status: api.DoctorStatusFail, code: codeTLSNoCert, fix: tlsACMEUnreachable,
			message: "still no ip certificate for 203.0.113.7, 10 minutes after the start",
		},
		{
			name: "no certificate after ten minutes, and no attempt failed", args: []string{"--tls.mode", "ip"}, live: noCert("", 11*time.Minute),
			status: api.DoctorStatusFail, code: codeTLSNoCert,
		},
		{
			name: "the domain points elsewhere", args: []string{"--domain", testDomain},
			live: func(s *api.ServerStatus) {
				s.TLS = api.TLSInfo{Mode: api.TLSModeAuto, Names: []string{testDomain}, LastErrorCode: tlsDNSWrong}
				s.UptimeS = 60
			},
			status: api.DoctorStatusWarn, code: codeTLSFailing, fix: tlsDNSWrong,
			fixText: "watch.example.com doesn't point to this server (203.0.113.7). Change its A record (see dns)",
		},
		{
			name: "the renewal fails, the certificate holds", args: []string{"--tls.mode", "ip"},
			live:   func(s *api.ServerStatus) { s.TLS.LastErrorCode = tlsRateLimited },
			status: api.DoctorStatusWarn, code: codeTLSRenewalFailing, fix: tlsRateLimited,
			message: "ip certificate for 203.0.113.7, valid until 2026-10-05 10:00 UTC, but its renewal is failing",
		},
		{
			name: "an ACME certificate that ran out", args: []string{"--tls.mode", "ip"},
			live: func(s *api.ServerStatus) {
				s.TLS.Ready, s.TLS.NotAfter, s.TLS.LastErrorCode = false, testNow.Add(-time.Hour), tlsACMEFailed
			},
			status: api.DoctorStatusFail, code: codeTLSExpired, fix: tlsACMEFailed,
		},
		{
			name: "manual, fine", args: manual, live: manualCert(60*24*time.Hour, true, ""),
			status: api.DoctorStatusOK, code: codeTLSOK,
			message: "manual certificate for watch.example.com and www.example.com, valid until 2026-11-28 20:15 UTC",
		},
		{
			name: "manual, 13 days left", args: manual, live: manualCert(13*24*time.Hour+time.Hour, true, ""),
			status: api.DoctorStatusWarn, code: codeTLSExpiresSoon, fix: fixTLSReplace,
			message: "manual certificate for watch.example.com and www.example.com ends on 2026-10-12, in 13 days",
		},
		{
			name: "manual, 14 days left", args: manual, live: manualCert(14*24*time.Hour, true, ""),
			status: api.DoctorStatusOK, code: codeTLSOK,
		},
		{
			name: "manual, expired", args: manual, live: manualCert(-24*time.Hour, false, tlsCertExpired),
			status: api.DoctorStatusFail, code: codeTLSExpired, fix: tlsCertExpired,
			message: "the manual certificate for watch.example.com and www.example.com expired on 2026-09-28",
		},
		{
			name: "manual, the key does not match", args: manual, live: manualCert(0, false, tlsCertInvalid),
			status: api.DoctorStatusFail, code: codeTLSNotLoaded, fix: tlsCertInvalid,
		},
		{
			name: "manual, the files can't be read", args: manual, live: manualCert(0, false, tlsCertUnreadable),
			status: api.DoctorStatusFail, code: codeTLSNotLoaded, fix: tlsCertUnreadable,
		},
		{
			name: "manual, new files refused while the old pair serves", args: manual, live: manualCert(60*24*time.Hour, true, tlsCertInvalid),
			status: api.DoctorStatusWarn, code: codeTLSReloadFailed, fix: tlsCertInvalid,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t, tt.args...)
			if tt.live != nil {
				tt.live(w.live)
			}
			c := w.check("tls")
			want(t, c, tt.status, tt.code, tt.fix)
			if tt.message != "" && c.Message != tt.message {
				t.Errorf("message:\n got %q\nwant %q", c.Message, tt.message)
			}
			if tt.fixText != "" && c.Fix != tt.fixText {
				t.Errorf("fix:\n got %q\nwant %q", c.Fix, tt.fixText)
			}
		})
	}

	// A code of a later tlsmgr that this doctor has no text for is passed on, with the general text.
	w := newWorld(t, "--tls.mode", "ip")
	noCert("tls.something_new", time.Minute)(w.live)
	if c := w.check("tls"); c.FixCode != "tls.something_new" || !strings.Contains(c.Fix, "the server's log (journalctl -u isshoni)") {
		t.Errorf("an unknown certificate code: fix code %q, fix %q", c.FixCode, c.Fix)
	}

	// Every certificate code of 04 §8.7 has a fix text for a failing server.
	for _, code := range []string{
		tlsACMEUnreachable, tlsDNSMissing, tlsDNSWrong, tlsRateLimited, tlsIPRejected, tlsCAAForbids, tlsACMEFailed,
		tlsCertUnreadable, tlsCertInvalid, tlsCertExpired,
	} {
		w := newWorld(t, "--tls.mode", "ip")
		noCert(code, time.Minute)(w.live)
		if c := w.check("tls"); c.FixCode != code || c.Fix == "" {
			t.Errorf("%s: fix code %q, fix %q", code, c.FixCode, c.Fix)
		}
	}
}

// clock: the kernel's sync flag and the skew against the ACME directory's Date header (04 §13.2, §17).
func TestCheckClock(t *testing.T) {
	synced := func() (bool, error) { return false, nil }
	unsynced := func() (bool, error) { return true, nil }
	// What systemd's ProtectClock= and a container's seccomp profile answer.
	eperm := func() (bool, error) { return false, syscall.EPERM }
	enosys := func() (bool, error) { return false, syscall.ENOSYS }
	ip := []string{"--tls.mode", "ip"}
	off := []string{"--tls.mode", "off"}
	manual := []string{"--tls.mode", "manual", "--tls.cert-file", "/c.pem", "--tls.key-file", "/k.pem", "--domain", testDomain}

	tests := []struct {
		name     string
		args     []string
		adjtimex func() (bool, error)
		skew     time.Duration // of the local clock against the Date header
		httpErr  error
		status   api.DoctorStatus
		code     string
		fix      string
		message  string
		requests int
	}{
		{name: "synchronized and in step", args: ip, adjtimex: synced, status: api.DoctorStatusOK, code: codeClockOK,
			message: "synchronized, within 1 second of acme-v02.api.letsencrypt.org", requests: 1},
		{name: "sync unknown (EPERM), the skew decides: fine", args: ip, adjtimex: eperm, skew: 3 * time.Second,
			status: api.DoctorStatusOK, code: codeClockOK, message: "within 3 seconds of acme-v02.api.letsencrypt.org", requests: 1},
		{name: "sync unknown (ENOSYS), the skew decides: off", args: ip, adjtimex: enosys, skew: -45 * time.Second,
			status: api.DoctorStatusWarn, code: codeClockSkew, fix: fixClockNTP,
			message: "the clock is 45s behind (compared with acme-v02.api.letsencrypt.org)", requests: 1},
		{name: "29 seconds are fine", args: ip, adjtimex: eperm, skew: 29 * time.Second, status: api.DoctorStatusOK, code: codeClockOK, requests: 1},
		{name: "30 seconds warn", args: ip, adjtimex: synced, skew: 30 * time.Second, status: api.DoctorStatusWarn, code: codeClockSkew, fix: fixClockNTP, requests: 1},
		{name: "5 minutes warn", args: ip, adjtimex: synced, skew: 5 * time.Minute, status: api.DoctorStatusWarn, code: codeClockSkew, fix: fixClockNTP, requests: 1},
		{name: "more than 5 minutes fail", args: ip, adjtimex: eperm, skew: 5*time.Minute + time.Second,
			status: api.DoctorStatusFail, code: codeClockSkew, fix: fixClockNTP,
			message: "the clock is 5m1s ahead (compared with acme-v02.api.letsencrypt.org)", requests: 1},
		{name: "not synchronized, in step", args: ip, adjtimex: unsynced, status: api.DoctorStatusWarn, code: codeClockUnsynced, fix: fixClockNTP, requests: 1},
		{name: "synchronized, the directory does not answer", args: ip, adjtimex: synced, httpErr: errors.New("connection refused"),
			status: api.DoctorStatusOK, code: codeClockOK, message: "synchronized", requests: 1},
		{name: "sync unknown, the directory does not answer", args: ip, adjtimex: eperm, httpErr: errors.New("connection refused"),
			status: api.DoctorStatusInfo, code: codeClockUnknown, requests: 1},
		{name: "the CA's certificate looks expired from here", args: ip, adjtimex: eperm,
			httpErr: &tls.CertificateVerificationError{Err: x509.CertificateInvalidError{Reason: x509.Expired}},
			status:  api.DoctorStatusFail, code: codeClockCertTime, fix: fixClockNTP, requests: 1},
		// manual and off order no certificate: no request, and without the sync flag nothing to say.
		{name: "manual: sync unknown is a note", args: manual, adjtimex: eperm, status: api.DoctorStatusInfo, code: codeClockUnknown},
		{name: "off: sync unknown is a note", args: off, adjtimex: eperm, skew: time.Hour, status: api.DoctorStatusInfo, code: codeClockUnknown},
		{name: "off: synchronized", args: off, adjtimex: synced, status: api.DoctorStatusOK, code: codeClockOK, message: "synchronized"},
		{name: "off: not synchronized", args: off, adjtimex: unsynced, status: api.DoctorStatusWarn, code: codeClockUnsynced, fix: fixClockNTP},
		{name: "auto mode asks too", args: []string{"--domain", testDomain}, adjtimex: synced, status: api.DoctorStatusOK, code: codeClockOK, requests: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t, tt.args...)
			w.env.Adjtimex = tt.adjtimex
			var requests []string
			w.env.HTTP = dateServer(testNow.Add(-tt.skew), &requests)
			if tt.httpErr != nil {
				w.env.HTTP = failingHTTP(tt.httpErr, &requests)
			}
			c := w.check("clock")
			want(t, c, tt.status, tt.code, tt.fix)
			if tt.message != "" && c.Message != tt.message {
				t.Errorf("message:\n got %q\nwant %q", c.Message, tt.message)
			}
			if len(requests) != tt.requests {
				t.Errorf("%d requests %v, want %d", len(requests), requests, tt.requests)
			}
			for _, r := range requests {
				if r != "GET https://acme-v02.api.letsencrypt.org/directory" {
					t.Errorf("request %q, want one GET of the ACME directory", r)
				}
			}
		})
	}

	// The directory is the one the certificate comes from: staging, or the operator's own CA.
	for args, wantURL := range map[string]string{
		"--tls.acme-staging": "GET https://acme-staging-v02.api.letsencrypt.org/directory",
		"--tls.acme-ca=https://acme.example.net/directory": "GET https://acme.example.net/directory",
	} {
		w := newWorld(t, "--tls.mode", "ip", args)
		var requests []string
		w.env.HTTP = dateServer(testNow, &requests)
		w.check("clock")
		if !slices.Equal(requests, []string{wantURL}) {
			t.Errorf("%s: requests %v, want %s", args, requests, wantURL)
		}
	}

	// Without an Adjtimex (not Linux) the sync state is unknown.
	w := newWorld(t, "--tls.mode", "off")
	env := w.build()
	env.Adjtimex, env.GOOS = nil, "plan9"
	rep := Run(t.Context(), env, api.BandwidthInput{}, []string{"clock"})
	want(t, rep.Checks[0], api.DoctorStatusInfo, codeClockUnknown, "")
}

// The texts of a failed request are short: no URL, and a timeout in plain words.
func TestHTTPErrText(t *testing.T) {
	timeout := &url.Error{Op: "Get", URL: "https://acme.example/directory", Err: context.DeadlineExceeded}
	if got := httpErrText(timeout); got != "no answer in time" {
		t.Errorf("timeout: %q", got)
	}
	refused := &url.Error{Op: "Get", URL: "https://acme.example/directory", Err: errors.New("connection refused")}
	if got := httpErrText(refused); got != "connection refused" {
		t.Errorf("refused: %q", got)
	}
	if got := dnsErrText(&net.DNSError{Err: "i/o timeout", IsTimeout: true}); got != "the resolver did not answer in time" {
		t.Errorf("DNS timeout: %q", got)
	}
}

// ports with a running server: what the server lists as bound, by config key, and the media ports also by what it
// advertises.
func TestCheckPortsLive(t *testing.T) {
	const note = "\nThis only checks this machine. The browser connection test checks from outside."
	w := newWorld(t, "--tls.mode", "ip")
	c := w.check("ports")
	want(t, c, api.DoctorStatusInfo, codePortsListening, "")
	if want := "listening on 443/tcp, 80/tcp, 7882/udp, 7882/tcp (local check only)" + note; c.Message != want {
		t.Errorf("message:\n got %q\nwant %q", c.Message, want)
	}
	if !c.LocalOnly {
		t.Error("ports is not marked local only")
	}

	// The media listeners by config key, as a later status lists them, and with the port the kernel picked.
	w.live.Advertised = nil
	w.live.Listeners = append(w.live.Listeners,
		api.ListenerInfo{Key: "listen.ice_udp", Network: "udp", Addr: "203.0.113.7:50123"},
		api.ListenerInfo{Key: "listen.ice_tcp", Network: "tcp", Addr: "[::]:7882"})
	contains(t, w.check("ports").Message, "listening on 443/tcp, 80/tcp, 50123/udp, 7882/tcp")

	// A media port the server does not have, though the config wants it.
	w.live.Listeners = w.live.Listeners[:3]
	c = w.check("ports")
	want(t, c, api.DoctorStatusWarn, codePortsNotListening, "")
	contains(t, c.Message, "not listening on 7882/udp, 7882/tcp; listening on 443/tcp, 80/tcp (local check only)"+note)

	// A port the config turns off is a warning: one way to connect less.
	w = newWorld(t, "--tls.mode", "ip", "--listen.ice-udp", "")
	w.live.Advertised = w.live.Advertised[1:]
	c = w.check("ports")
	want(t, c, api.DoctorStatusWarn, codePortsDisabled, "")
	contains(t, c.Message, "listening on 443/tcp, 80/tcp, 7882/tcp; turned off: 7882/udp (listen.ice_udp is empty) (local check only)"+note)

	// off mode: no 443 of its own, and the plain port is the proxy's upstream.
	w = newWorld(t, "--tls.mode", "off")
	w.live.Listeners = []api.ListenerInfo{{Key: "listen.http", Network: "tcp", Addr: "127.0.0.1:8080"}}
	w.live.Advertised = w.live.Advertised[:1]
	w.live.Advertised = append(w.live.Advertised, api.AdvertisedAddr{Proto: "tcp", Addr: testIP + ":7882", Via: api.TransportTCP7882})
	c = w.check("ports")
	want(t, c, api.DoctorStatusInfo, codePortsListening, "")
	contains(t, c.Message, "listening on 8080/tcp, 7882/udp, 7882/tcp (local check only)")
}

// ports offline: each port is bound and released; one that another program holds fails.
func TestCheckPortsOffline(t *testing.T) {
	offline := func(t *testing.T, bind map[string]error, args ...string) (*world, *[]string) {
		w := newWorld(t, append([]string{"--tls.mode", "ip"}, args...)...)
		w.live = nil
		var tried []string
		w.env.Bind = func(_ context.Context, network, addr string) error {
			tried = append(tried, network+" "+addr)
			return bind[network+" "+addr]
		}
		return w, &tried
	}
	t.Run("all free", func(t *testing.T) {
		w, tried := offline(t, nil)
		c := w.check("ports")
		want(t, c, api.DoctorStatusInfo, codePortsFree, "")
		contains(t, c.Message, "443/tcp, 80/tcp, 7882/udp and 7882/tcp are free (local check only)\nThis only checks this machine.")
		if want := []string{"tcp :443", "tcp :80", "udp :7882", "tcp :7882"}; !slices.Equal(*tried, want) {
			t.Errorf("tried %v, want %v", *tried, want)
		}
	})
	t.Run("a web server holds 443 and 80", func(t *testing.T) {
		w, _ := offline(t, map[string]error{"tcp :443": errInUse, "tcp :80": errInUse})
		c := w.check("ports")
		want(t, c, api.DoctorStatusFail, codePortsInUse, fixPortsFree)
		contains(t, c.Message, "443/tcp and 80/tcp are in use by another program (local check only)")
		if want := `stop the other program (another web server?), or run isshoni behind it with tls.mode = "off"`; c.Fix != want {
			t.Errorf("fix %q, want %q", c.Fix, want)
		}
	})
	t.Run("something holds the media port", func(t *testing.T) {
		w, _ := offline(t, map[string]error{"udp :7882": errInUse})
		c := w.check("ports")
		want(t, c, api.DoctorStatusFail, codePortsInUse, fixPortsFree)
		contains(t, c.Message, "7882/udp is in use by another program")
		if want := "stop the other program (another isshoni?), or change listen.ice_udp"; c.Fix != want {
			t.Errorf("fix %q, want %q", c.Fix, want)
		}
	})
	t.Run("privileged ports as an ordinary user", func(t *testing.T) {
		w, _ := offline(t, map[string]error{"tcp :443": errDenied, "tcp :80": errDenied})
		c := w.check("ports")
		want(t, c, api.DoctorStatusInfo, codePortsFree, "")
		contains(t, c.Message, "7882/udp and 7882/tcp are free; not checked without root: 443/tcp, 80/tcp (local check only)")
	})
	t.Run("a port that is turned off is not bound", func(t *testing.T) {
		w, tried := offline(t, nil, "--listen.ice-tcp", "")
		c := w.check("ports")
		want(t, c, api.DoctorStatusWarn, codePortsDisabled, "")
		contains(t, c.Message, "443/tcp, 80/tcp and 7882/udp are free; turned off: 7882/tcp (listen.ice_tcp is empty)")
		if slices.Contains(*tried, "tcp ") || len(*tried) != 3 {
			t.Errorf("tried %v", *tried)
		}
	})
}

// The real bind: a free port binds, a taken one is "in use". Both on loopback, on ports the kernel picks.
func TestBind(t *testing.T) {
	for _, network := range []string{"tcp", "udp"} {
		if err := bind(t.Context(), network, "127.0.0.1:0"); err != nil {
			t.Errorf("bind %s on a free port: %v", network, err)
		}
	}
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	if err := bind(t.Context(), "tcp", ln.Addr().String()); !isAddrInUse(err) {
		t.Errorf("bind on a taken port: %v, want address in use", err)
	}
	pc, err := (&net.ListenConfig{}).ListenPacket(t.Context(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()
	if err := bind(t.Context(), "udp", pc.LocalAddr().String()); !isAddrInUse(err) {
		t.Errorf("bind on a taken UDP port: %v, want address in use", err)
	}
	if isAddrInUse(errDenied) || !isAddrInUse(errInUse) {
		t.Error("isAddrInUse mixes up the two errors")
	}
}

// firewall_hint names the ports in the provider's own words, one text per provider id of 04 §13.3, with the
// provider's install page after it.
func TestFirewallHintProviders(t *testing.T) {
	dmi := map[api.CloudProvider][3]string{ // sys_vendor, bios_vendor, chassis_asset_tag
		api.CloudProviderAWS:          {"Amazon EC2", "Amazon EC2", ""},
		api.CloudProviderGCP:          {"Google", "Google", ""},
		api.CloudProviderAzure:        {"Microsoft Corporation", "American Megatrends Inc.", "7783-7084-3265-9085-8269-3286-77"},
		api.CloudProviderOracle:       {"QEMU", "SeaBIOS", "OracleCloud.com"},
		api.CloudProviderHetzner:      {"Hetzner", "Hetzner", ""},
		api.CloudProviderDigitalOcean: {"DigitalOcean", "DigitalOcean", ""},
		api.CloudProviderVultr:        {"Vultr", "SeaBIOS", ""},
		api.CloudProviderLinode:       {"Linode", "SeaBIOS", ""},
		api.CloudProviderScaleway:     {"Scaleway", "EFI Development Kit II / OVMF", ""},
		api.CloudProviderOVH:          {"OVHcloud", "SeaBIOS", ""},
		api.CloudProviderAlibaba:      {"Alibaba Cloud", "SeaBIOS", ""},
		api.CloudProviderTencent:      {"Tencent Cloud", "SeaBIOS", ""},
		api.CloudProviderUnknown:      {"Dell Inc.", "Dell Inc.", "ABC123"},
	}
	seen := map[string]api.CloudProvider{}
	for _, provider := range api.CloudProviders() {
		strs, ok := dmi[provider]
		if !ok {
			t.Errorf("no DMI strings in this test for provider %q", provider)
			continue
		}
		t.Run(string(provider), func(t *testing.T) {
			w := newWorld(t, "--tls.mode", "ip")
			w.files["sys/class/dmi/id/sys_vendor"].Data = []byte(strs[0] + "\n")
			w.files["sys/class/dmi/id/bios_vendor"].Data = []byte(strs[1] + "\n")
			w.files["sys/class/dmi/id/chassis_asset_tag"] = &fsFile{Data: []byte(strs[2] + "\n")}
			rep := Run(t.Context(), w.build(), api.BandwidthInput{}, []string{"firewall_hint"})
			if rep.Env.Provider != provider {
				t.Fatalf("env.provider = %q, want %q", rep.Env.Provider, provider)
			}
			c := rep.Checks[0]
			want(t, c, api.DoctorStatusInfo, codeFirewallHintOpenPorts, "")
			if c.Params["provider"] != string(provider) {
				t.Errorf("params.provider = %v", c.Params["provider"])
			}
			contains(t, c.Message, "TCP 80, 443, 7882 and UDP 7882")
			first, _, _ := strings.Cut(c.Message, ": ")
			if other, dup := seen[first]; dup {
				t.Errorf("%s and %s share the text %q", provider, other, first)
			}
			seen[first] = provider

			var b strings.Builder
			RenderText(&b, rep, false)
			link := "\n       https://moonwx.github.io/isshoni/install/vps#" + string(provider) + "\n"
			if has := strings.Contains(b.String(), link); has != (provider != api.CloudProviderUnknown) {
				t.Errorf("link to the provider's page: %v\n%s", has, b.String())
			}
		})
	}

	w := newWorld(t, "--tls.mode", "ip")
	if c := w.check("firewall_hint"); c.Message != "Hetzner: Cloud Console → Firewalls → allow TCP 80, 443, 7882 and UDP 7882" {
		t.Errorf("Hetzner: %q", c.Message)
	}
	w.files["sys/class/dmi/id/chassis_asset_tag"] = &fsFile{Data: []byte("OracleCloud.com\n")}
	w.files["sys/class/dmi/id/sys_vendor"].Data = []byte("QEMU\n")
	contains(t, w.check("firewall_hint").Message, "Oracle's images also block these ports with their own iptables rules")

	// off mode: the web ports are the proxy's.
	w = newWorld(t, "--tls.mode", "off")
	c := w.check("firewall_hint")
	contains(t, c.Message, "allow TCP 7882 and UDP 7882", "TCP 80 and 443 belong to the proxy in front")

	// A Docker bridge: published ports go past ufw.
	w = newWorld(t, "--tls.mode", "ip")
	w.container("docker", true)
	w.files["sys/class/net/eth0/iflink"].Data = []byte("17\n")
	contains(t, w.check("firewall_hint").Message, "Docker publishes ports past ufw")
	w.files["sys/class/net/eth0/iflink"].Data = []byte("2\n") // the host's network
	if c := w.check("firewall_hint"); strings.Contains(c.Message, "ufw") {
		t.Errorf("host network: %q", c.Message)
	}
}
