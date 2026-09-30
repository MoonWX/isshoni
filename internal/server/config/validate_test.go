package config

import (
	"net/netip"
	"strings"
	"testing"
)

// Each rule of §4.5 produces the right key, severity, source and fix. The public address in ip-mode cases is a
// well-known public resolver address, not a documentation address (those are rejected on purpose).
func TestRules(t *testing.T) {
	const public = "8.8.8.8"
	flag := func(name string) Source { return Source{Kind: SourceFlag, Name: "--" + name} }
	env := func(name string) Source { return Source{Kind: SourceEnv, Name: name} }
	def := Source{Kind: SourceDefault}
	tests := []struct {
		name     string
		file     string // "" for none
		env      []string
		args     []string
		severity string
		key      string
		src      Source // File is not compared (a temp path)
		msg      string // substring of the message
		fix      string // substring of the fix
	}{
		// Enums.
		{"tls.mode enum", "", nil, []string{"--tls.mode=IP"}, SeverityError, "tls.mode", flag("tls.mode"), "is not one of auto, ip, manual, off", `did you mean "ip"?`},
		{"registration.mode enum", "[registration]\nmode = \"open\"\n", nil, nil, SeverityError, "registration.mode", Source{Kind: SourceFile, Line: 2}, "is not one of invite, approval, closed", `"invite", "approval", "closed"`},
		{"log.level enum", "", []string{"ISSHONI_LOG_LEVEL=verbose"}, nil, SeverityError, "log.level", env("ISSHONI_LOG_LEVEL"), "is not one of debug", `"debug", "info"`},
		{"log.format enum", "", nil, []string{"--log.format=yaml"}, SeverityError, "log.format", flag("log.format"), "is not one of auto, text, json", `"json"`},

		// domain.
		{"domain IP literal", "", nil, []string{"--domain=203.0.113.7"}, SeverityError, "domain", flag("domain"), "is an IP address", `set public_ip = "203.0.113.7"`},
		{"domain IPv6 literal", "", nil, []string{"--domain=[2001:db8::1]"}, SeverityError, "domain", flag("domain"), "is an IP address", `public_ip = "2001:db8::1"`},
		{"domain localhost", "", nil, []string{"--domain=localhost"}, SeverityError, "domain", flag("domain"), "is localhost", `tls.mode = "off"`},
		{"domain with scheme", "", nil, []string{"--domain=https://watch.example.com"}, SeverityError, "domain", flag("domain"), "is not a DNS name", "no https://"},
		{"domain bad label", "", nil, []string{"--domain=-bad-.example.com"}, SeverityError, "domain", flag("domain"), "is not a valid DNS name", "hyphens"},
		{"domain trailing dot", "", nil, []string{"--domain=watch.example.com."}, SeverityError, "domain", flag("domain"), "is not a valid DNS name", "trailing dot"},

		// public_ip and public_ipv6.
		{"public_ip garbage", "", nil, []string{"--public-ip=my-server"}, SeverityError, "public_ip", flag("public-ip"), `is not "auto" or an IP address`, "public IPv4 address"},
		{"public_ipv6 garbage", "", nil, []string{"--public-ipv6=yes"}, SeverityError, "public_ipv6", flag("public-ipv6"), `is not "auto", "off" or an IPv6 address`, `"off"`},
		{"public_ipv6 is v4", "", nil, []string{"--public-ipv6=" + public}, SeverityError, "public_ipv6", flag("public-ipv6"), "is not an IPv6 address", "public_ip"},
		{"ip mode private (explicit mode)", "[tls]\nmode = \"ip\"\n", []string{"ISSHONI_PUBLIC_IP=192.168.1.20"}, nil, SeverityError, "tls.mode", Source{Kind: SourceFile, Line: 2},
			`needs a public address, but public_ip = "192.168.1.20" is private`, "Let's Encrypt only issues IP certificates for public addresses"},
		{"ip mode CGNAT (derived mode)", "", []string{"ISSHONI_PUBLIC_IP=100.64.1.2"}, nil, SeverityError, "public_ip", env("ISSHONI_PUBLIC_IP"),
			`is a shared (CGNAT) address, but tls.mode "ip" (derived: domain is empty) needs a public address`, `tls.mode = "off"`},
		{"ip mode loopback", "", nil, []string{"--public-ip=127.0.0.1"}, SeverityError, "public_ip", flag("public-ip"), "is loopback", "Use a VPS"},
		{"ip mode documentation", "", nil, []string{"--public-ip=203.0.113.7"}, SeverityError, "public_ip", flag("public-ip"), "is a documentation address", "Use a VPS"},
		{"ip mode link-local v6", "", nil, []string{"--public-ip=fe80::1"}, SeverityError, "public_ip", flag("public-ip"), "is link-local", "Use a VPS"},
		{"ip mode ULA public_ipv6", "", nil, []string{"--public-ipv6=fd00::1"}, SeverityError, "public_ipv6", flag("public-ipv6"), "is private (ULA)", "global IPv6 address"},

		// auto needs domain; manual needs the files.
		{"auto without domain", "", nil, []string{"--tls.mode=auto"}, SeverityError, "tls.mode", flag("tls.mode"), "needs a domain", `tls.mode = "ip"`},
		{"manual without files", "", []string{"ISSHONI_TLS_MODE=manual"}, nil, SeverityError, "tls.mode", env("ISSHONI_TLS_MODE"), "needs tls.cert_file and tls.key_file", "PEM files"},
		{"manual without key", "", nil, []string{"--tls.mode=manual", "--tls.cert-file=/c.pem"}, SeverityError, "tls.mode", flag("tls.mode"), "needs tls.key_file", "private key"},

		// off mode.
		{"off without public_url", "", nil, []string{"--tls.mode=off", "--listen.http=0.0.0.0:8080"}, SeverityError, "public_url", def,
			`is required with tls.mode = "off" when listen.http = "0.0.0.0:8080" is not a loopback address`, "https:// address of your proxy"},
		{"off with http public_url", "", nil, []string{"--tls.mode=off", "--public-url=http://watch.example.com"}, SeverityError, "public_url", flag("public-url"),
			"must be https:// unless its host is localhost, 127.0.0.1 or [::1]", "browsers need HTTPS"},
		{"public_url with a path", "", nil, []string{"--tls.mode=off", "--public-url=https://example.com/isshoni"}, SeverityError, "public_url", flag("public-url"),
			"must be only a scheme, a host and an optional port", "root of its own host"},
		{"public_url not absolute", "", nil, []string{"--public-url=watch.example.com"}, SeverityError, "public_url", flag("public-url"), "is not an absolute URL", "https://watch.example.com"},
		{"public_url ftp", "", nil, []string{"--public-url=ftp://watch.example.com"}, SeverityError, "public_url", flag("public-url"), "must start with https://", "https://watch.example.com"},
		{"off with listen.https", "", nil, []string{"--tls.mode=off", "--listen.https=:8443"}, SeverityWarning, "listen.https", flag("listen.https"), `is ignored with tls.mode = "off"`, "your proxy owns HTTPS"},
		{"off without trusted_proxies", "", nil, []string{"--tls.mode=off", "--listen.http=0.0.0.0:8080", "--public-url=https://watch.example.com"}, SeverityWarning,
			"network.trusted_proxies", def, "every client IP will be the proxy's", "172.30.89.0/24"},

		// Listeners.
		{"listen not host:port", "", nil, []string{"--listen.https=443"}, SeverityError, "listen.https", flag("listen.https"), "is not a host:port address", `":443"`},
		{"listen bad port", "", nil, []string{"--listen.ice-tcp=:70000"}, SeverityError, "listen.ice_tcp", flag("listen.ice-tcp"), "is not a host:port address", "host:port"},
		{"listen.https empty", "", nil, []string{"--listen.https="}, SeverityError, "listen.https", flag("listen.https"), "is empty", `":443"`},
		{"off listen.http empty", "", nil, []string{"--tls.mode=off", "--listen.http=", "--public-url=https://a.example.com"}, SeverityError, "listen.http", flag("listen.http"), "is empty", "127.0.0.1:8080"},
		{"shared TCP port", "", nil, []string{"--listen.ice-tcp=:443"}, SeverityError, "listen.ice_tcp", flag("listen.ice-tcp"), "uses port 443, like listen.https", "its own port"},
		{"shared port blames the set key", "", nil, []string{"--listen.https=:80"}, SeverityError, "listen.https", flag("listen.https"), "uses port 80, like listen.http", "its own port"},
		{"metrics port shared", "", nil, []string{"--metrics.enabled", "--metrics.listen=127.0.0.1:7882"}, SeverityError, "metrics.listen", flag("metrics.listen"), "uses port 7882, like listen.ice_tcp", "its own port"},
		{"no media", "", nil, []string{"--tls.mode=off", "--listen.ice-udp=", "--listen.ice-tcp="}, SeverityError, "listen.ice_udp", flag("listen.ice-udp"), "media has no way in", `listen.ice_udp = ":7882"`},
		{"no media without https", "", nil, []string{"--listen.ice-udp=", "--listen.ice-tcp=", "--listen.https="}, SeverityError, "listen.ice_udp", flag("listen.ice-udp"), "media has no way in", `":7882"`},

		// network.
		{"stun not host:port", "", []string{"ISSHONI_NETWORK_STUN_SERVERS=stun.example.com"}, nil, SeverityError, "network.stun_servers", env("ISSHONI_NETWORK_STUN_SERVERS"),
			`has "stun.example.com", which is not host:port`, "stun.cloudflare.com:3478"},
		{"trusted_proxies not CIDR", "", nil, []string{"--network.trusted-proxies=10.0.0.1"}, SeverityError, "network.trusted_proxies", flag("network.trusted-proxies"),
			`has "10.0.0.1", which is not a CIDR`, "10.0.0.1/32"},
		{"bad interface glob", "", nil, []string{"--network.exclude-interfaces=eth["}, SeverityError, "network.exclude_interfaces", flag("network.exclude-interfaces"), "not a valid glob", "docker*"},
		{"udp buffer too small", "", nil, []string{"--network.udp-buffer-bytes=65536"}, SeverityError, "network.udp_buffer_bytes", flag("network.udp-buffer-bytes"), "is out of range", "1048576 (1 MiB) to 67108864 (64 MiB)"},
		{"udp buffer too big", "", nil, []string{"--network.udp-buffer-bytes=67108865"}, SeverityError, "network.udp_buffer_bytes", flag("network.udp-buffer-bytes"), "is out of range", "8388608"},

		// Paths.
		{"admin socket too long", "", nil, []string{"--listen.admin-socket=/" + strings.Repeat("s", 104)}, SeverityError, "listen.admin_socket", flag("listen.admin-socket"), "is 105 bytes long", "shorter path"},
		{"admin socket empty", "", nil, []string{"--listen.admin-socket="}, SeverityError, "listen.admin_socket", flag("listen.admin-socket"), "is empty", "/run/isshoni/admin.sock"},
		{"data_dir empty", "data_dir = \"\"\n", nil, nil, SeverityError, "data_dir", Source{Kind: SourceFile, Line: 1}, "is empty", "/var/lib/isshoni"},

		// Numbers.
		{"shutdown_timeout zero", "", nil, []string{"--shutdown-timeout=0s"}, SeverityError, "shutdown_timeout", flag("shutdown-timeout"), "is not positive", `"10s"`},
		{"shutdown_timeout negative", "", nil, []string{"--shutdown-timeout=-5s"}, SeverityError, "shutdown_timeout", flag("shutdown-timeout"), "is not positive", `"10s"`},
		{"handshake guard", "", nil, []string{"--limits.ws-handshakes-per-ip-per-minute=0"}, SeverityError, "limits.ws_handshakes_per_ip_per_minute", flag("limits.ws-handshakes-per-ip-per-minute"), "is less than 1", "default is 20"},
		{"conns guard", "", nil, []string{"--limits.conns-per-ip=-1"}, SeverityError, "limits.conns_per_ip", flag("limits.conns-per-ip"), "is less than 1", "default is 256"},

		// push, clients, metrics, ACME.
		{"push.subject", "", nil, []string{"--push.subject=you@example.com"}, SeverityError, "push.subject", flag("push.subject"), "must start with mailto: or https:", "mailto:you@example.com"},
		{"clients.min_version", "", nil, []string{"--clients.min-version=v1.2"}, SeverityError, "clients.min_version", flag("clients.min-version"), "is not a SemVer version", `"0.3.0"`},
		{"metrics not loopback", "", []string{"ISSHONI_METRICS_ENABLED=true", "ISSHONI_METRICS_LISTEN=0.0.0.0:9469"}, nil, SeverityWarning, "metrics.listen", env("ISSHONI_METRICS_LISTEN"),
			"exposes unauthenticated metrics", "use 127.0.0.1:9469 and scrape through an SSH tunnel or a local agent"},
		{"metrics.listen empty", "", nil, []string{"--metrics.enabled", "--metrics.listen="}, SeverityError, "metrics.listen", flag("metrics.listen"), "is empty", "127.0.0.1:9469"},
		{"acme staging", "", nil, []string{"--tls.acme-staging"}, SeverityWarning, "tls.acme_staging", flag("tls.acme-staging"), "for testing only", "remove it for production"},
		{"acme ca root", "", nil, []string{"--tls.acme-ca-root=/pebble.pem"}, SeverityWarning, "tls.acme_ca_root", flag("tls.acme-ca-root"), "for testing only", "remove it for production"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, ve := testLoad(t, tt.file, tt.env, tt.args...)
			ps := c.Problems()
			if tt.severity == SeverityError && ve == nil {
				t.Fatalf("no ValidationError; problems:%s", problemList(ps))
			}
			p, ok := findProblem(ps, tt.key, tt.msg)
			if !ok {
				t.Fatalf("no problem for %s with %q:%s", tt.key, tt.msg, problemList(ps))
			}
			src := p.Source
			src.File = ""
			if p.Severity != tt.severity || src != tt.src || !strings.Contains(p.Fix, tt.fix) {
				t.Errorf("got %+v\nwant severity %s, source %+v, fix containing %q", p, tt.severity, tt.src, tt.fix)
			}
			if p.Value == "" {
				t.Errorf("problem without the value: %+v", p)
			}
		})
	}
}

// Valid values of every rule pass without problems.
func TestRulesAccept(t *testing.T) {
	for _, args := range [][]string{
		{"--domain=watch.example.com"},
		{"--domain=xn--bcher-kva.example", "--tls.mode=manual", "--tls.cert-file=c", "--tls.key-file=k"},
		{"--tls.mode=manual", "--tls.cert-file=c", "--tls.key-file=k", "--public-ip=192.168.1.20"}, // manual may be private
		{"--public-ip=8.8.8.8", "--public-ipv6=2606:4700:4700::1111"},
		{"--public-ip=::ffff:8.8.8.8"},
		{"--public-ipv6=off"},
		{"--tls.mode=off"},
		{"--tls.mode=off", "--public-url=http://localhost:5173"},
		{"--tls.mode=off", "--public-url=http://127.0.0.1:18080/"},
		{"--tls.mode=off", "--public-url=http://[::1]:8080"},
		{"--tls.mode=off", "--public-url=https://share.example.com", "--listen.http=0.0.0.0:8080", "--network.trusted-proxies=172.30.89.0/24"},
		{"--tls.mode=off", "--listen.ice-udp="},                                                 // TCP 7882 still carries media
		{"--listen.ice-udp=", "--listen.ice-tcp="},                                              // ICE-TCP on 443
		{"--listen.https=:0", "--listen.http=:0", "--listen.ice-tcp=:0", "--listen.ice-udp=:0"}, // ephemeral ports (tests)
		{"--listen.http="}, // no port 80 in auto/ip mode (TLS-ALPN)
		{"--metrics.enabled", "--metrics.listen=[::1]:9469"},
		{"--metrics.listen=0.0.0.0:9469"}, // metrics off: no warning
		{"--push.subject=mailto:you@example.com"},
		{"--push.subject=https://watch.example.com"},
		{"--clients.min-version=0.3.0-rc.1+build.5"},
		{"--network.udp-buffer-bytes=1048576"},
		{"--network.udp-buffer-bytes=67108864"},
		{"--network.stun-servers=[2001:db8::1]:3478,stun.example.com:3478"},
		{"--listen.admin-socket=/" + strings.Repeat("s", 103)},
	} {
		c, ve := testLoad(t, "", nil, args...)
		if ve != nil || len(c.Problems()) != 0 {
			t.Errorf("%v:%s", args, problemList(c.Problems()))
		}
	}
}

// The text form of a problem follows §4.5.
func TestProblemString(t *testing.T) {
	p := Problem{
		Key: "tls.mode", Value: `"ip"`, Severity: SeverityError,
		Source:  Source{Kind: SourceFile, File: "/etc/isshoni/isshoni.toml", Line: 7},
		Message: `needs a public address, but public_ip = "192.168.1.20" is private`,
		Fix:     ipModeFix,
	}
	want := `config error: tls.mode = "ip" (file /etc/isshoni/isshoni.toml:7) needs a public address, but public_ip = "192.168.1.20" is private.
  fix: Let's Encrypt only issues IP certificates for public addresses. Use a VPS, or set domain and tls.mode =
       "auto", or run behind your own HTTPS proxy with tls.mode = "off".`
	if got := p.String(); got != want {
		t.Errorf("String():\n%s\nwant\n%s", got, want)
	}
	w := Problem{
		Key: "metrics.listen", Value: `"0.0.0.0:9469"`, Severity: SeverityWarning,
		Source:  Source{Kind: SourceEnv, Name: "ISSHONI_METRICS_LISTEN"},
		Message: "exposes unauthenticated metrics", Fix: "use 127.0.0.1:9469 and scrape through an SSH tunnel or a local agent.",
	}
	want = `config warning: metrics.listen = "0.0.0.0:9469" (env ISSHONI_METRICS_LISTEN) exposes unauthenticated metrics.
  fix: use 127.0.0.1:9469 and scrape through an SSH tunnel or a local agent.`
	if got := w.String(); got != want {
		t.Errorf("String():\n%s\nwant\n%s", got, want)
	}
	for src, want := range map[Source]string{
		{}:                                 "default",
		{Kind: SourceDefault}:              "default",
		{Kind: SourceFile, File: "a.toml"}: "file a.toml",
		{Kind: SourceFile, File: "a", Line: 3, Column: 9}: "file a:3:9",
		{Kind: SourceFlag, Name: "--tls.mode"}:            "flag --tls.mode",
	} {
		if got := src.String(); got != want {
			t.Errorf("%+v.String() = %q, want %q", src, got, want)
		}
	}
}

func TestAddrClass(t *testing.T) {
	for addr, want := range map[string]string{
		"8.8.8.8": "", "2606:4700:4700::1111": "", "::ffff:8.8.8.8": "",
		"10.1.2.3": "private", "172.31.0.1": "private", "192.168.0.1": "private", "::ffff:192.168.0.1": "private",
		"100.64.0.1": "a shared (CGNAT) address", "127.0.0.1": "loopback", "::1": "loopback",
		"169.254.169.254": "link-local", "fe80::1": "link-local", "fd12::1": "private (ULA)",
		"192.0.2.1": "a documentation address", "198.51.100.1": "a documentation address",
		"203.0.113.7": "a documentation address", "2001:db8::1": "a documentation address",
		"0.0.0.0": "reserved", "224.0.0.1": "multicast", "255.255.255.255": "reserved", "::": "unspecified",
	} {
		if got := addrClass(netip.MustParseAddr(addr)); got != want {
			t.Errorf("addrClass(%s) = %q, want %q", addr, got, want)
		}
	}
}
