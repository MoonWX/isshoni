package config

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Validate checks the config's current values against the rules of §4.5 and returns every problem, errors and
// warnings, each with the key, its source and a fix. Load calls it; checks that need the machine (a file is
// readable, a port is free, a certificate matches) run at startup or in doctor instead. Policy values are checked by
// 03's SettingsCache.Pin when the wiring pins them (§4.6), except the enum of registration.mode and the SemVer of
// clients.min_version.
func (c *Config) Validate() []Problem {
	v := &validator{c: c, mode: c.EffectiveTLSMode()}
	v.enums()
	v.domain()
	v.publicAddrs()
	v.tlsMode()
	v.publicURL()
	v.offMode()
	v.listeners()
	v.network()
	v.paths()
	v.numbers()
	v.push()
	v.clients()
	v.acme()
	return sortProblems(v.problems)
}

type validator struct {
	c        *Config
	mode     TLSMode
	problems []Problem
}

// add records a problem for key with its current value and source.
func (v *validator) add(severity, key, msg, fix string) {
	k := &registry[keyByPath[key]]
	v.problems = append(v.problems, Problem{
		Key: key, Value: formatTOML(k.get(v.c)), Source: v.c.Source(key), Severity: severity, Message: msg, Fix: fix,
	})
}

func (v *validator) errorf(key, fix, format string, args ...any) {
	v.add(SeverityError, key, fmt.Sprintf(format, args...), fix)
}

func (v *validator) warnf(key, fix, format string, args ...any) {
	v.add(SeverityWarning, key, fmt.Sprintf(format, args...), fix)
}

// modeText describes the effective TLS mode with its origin, e.g. `tls.mode "ip" (derived: domain is empty)`.
func (v *validator) modeText() string {
	if v.c.IsSet("tls.mode") {
		return fmt.Sprintf("tls.mode = %s", tomlString(string(v.mode)))
	}
	if v.c.Domain != "" {
		return fmt.Sprintf("tls.mode %s (derived: domain is set)", tomlString(string(v.mode)))
	}
	return fmt.Sprintf("tls.mode %s (derived: domain is empty)", tomlString(string(v.mode)))
}

// Enum keys have a known value.
func (v *validator) enums() {
	for i := range registry {
		k := &registry[i]
		if k.Enum == nil {
			continue
		}
		s := k.get(v.c).(string)
		if (s == "" && k.Default == "") || slices.Contains(k.Enum, s) {
			continue
		}
		quoted := make([]string, len(k.Enum))
		for i, e := range k.Enum {
			quoted[i] = tomlString(e)
		}
		fix := "use one of " + strings.Join(quoted, ", ")
		if s2 := suggest(strings.ToLower(s), k.Enum); s2 != "" {
			fix = "did you mean " + tomlString(s2) + "? The values are " + strings.Join(quoted, ", ")
		}
		v.errorf(k.Path, fix, "is not one of %s", strings.Join(k.Enum, ", "))
	}
}

// dnsLabel is one label of a DNS name (letters, digits and hyphens; punycode is plain ASCII).
var dnsLabel = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

// domain is a valid DNS name, not an IP literal and not localhost.
func (v *validator) domain() {
	d := v.c.Domain
	if d == "" {
		return
	}
	switch {
	case isIPLiteral(d):
		v.errorf("domain", fmt.Sprintf("set public_ip = %s instead and leave domain empty", tomlString(strings.Trim(d, "[]"))),
			"is an IP address, not a domain name")
	case strings.Contains(d, "://") || strings.ContainsAny(d, "/:@ "):
		v.errorf("domain", "write only the name, like watch.example.com: no https://, port or path", "is not a DNS name")
	case strings.EqualFold(strings.TrimSuffix(d, "."), "localhost") || strings.HasSuffix(strings.ToLower(d), ".localhost"):
		v.errorf("domain", `use the DNS name that points to this server; for a test on this machine use tls.mode = "off" with listen.http = "127.0.0.1:8080"`,
			"is localhost, which your friends can't reach")
	case !validDNSName(d):
		v.errorf("domain", "use letters, digits, hyphens and dots, like watch.example.com (punycode for other scripts), without a trailing dot",
			"is not a valid DNS name")
	}
}

func isIPLiteral(s string) bool {
	_, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(s, "["), "]"))
	return err == nil
}

func validDNSName(d string) bool {
	if len(d) > 253 {
		return false
	}
	for _, label := range strings.Split(d, ".") {
		if !dnsLabel.MatchString(label) {
			return false
		}
	}
	return true
}

// ipModeFix is the fix for a non-public address in ip mode (§4.5).
const ipModeFix = `Let's Encrypt only issues IP certificates for public addresses. Use a VPS, or set domain and tls.mode = "auto", ` +
	`or run behind your own HTTPS proxy with tls.mode = "off"`

// public_ip / public_ipv6 literals are valid; with tls.mode ip they must be public.
func (v *validator) publicAddrs() {
	check := func(key, val string, v6 bool) {
		if val == "auto" || (v6 && val == "off") {
			return
		}
		a, err := netip.ParseAddr(val)
		switch {
		case err != nil || a.Zone() != "":
			if v6 {
				v.errorf(key, `use "auto", "off" or this server's global IPv6 address`, `is not "auto", "off" or an IPv6 address`)
			} else {
				v.errorf(key, `use "auto" to detect it, or this server's public IPv4 address`, `is not "auto" or an IP address`)
			}
			return
		case v6 && (!a.Is6() || a.Is4In6()):
			v.errorf(key, `put an IPv4 address in public_ip; public_ipv6 takes "auto", "off" or an IPv6 address`, "is not an IPv6 address")
			return
		}
		if v.mode != TLSIP {
			return
		}
		class := addrClass(a)
		if class == "" {
			return
		}
		if !v6 && v.c.IsSet("tls.mode") {
			v.errorf("tls.mode", ipModeFix, "needs a public address, but public_ip = %s is %s", tomlString(val), class)
			return
		}
		if v6 {
			v.errorf(key, `use this server's global IPv6 address, "auto" or "off"`, "is %s, but %s needs public addresses", class, v.modeText())
			return
		}
		v.errorf(key, ipModeFix, "is %s, but %s needs a public address", class, v.modeText())
	}
	check("public_ip", v.c.PublicIP, false)
	check("public_ipv6", v.c.PublicIPv6, true)
}

// nonPublic are the ranges a Let's Encrypt IP certificate can't be issued for, with how the messages call them.
var nonPublic = func() []struct {
	p    netip.Prefix
	what string
} {
	var out []struct {
		p    netip.Prefix
		what string
	}
	for _, r := range [][2]string{
		{"0.0.0.0/8", "reserved"},
		{"10.0.0.0/8", "private"},
		{"100.64.0.0/10", "a shared (CGNAT) address"},
		{"127.0.0.0/8", "loopback"},
		{"169.254.0.0/16", "link-local"},
		{"172.16.0.0/12", "private"},
		{"192.0.0.0/24", "reserved"},
		{"192.0.2.0/24", "a documentation address"},
		{"192.168.0.0/16", "private"},
		{"198.18.0.0/15", "reserved"},
		{"198.51.100.0/24", "a documentation address"},
		{"203.0.113.0/24", "a documentation address"},
		{"224.0.0.0/4", "multicast"},
		{"240.0.0.0/4", "reserved"},
		{"::/128", "unspecified"},
		{"::1/128", "loopback"},
		{"100::/64", "reserved"},
		{"2001:db8::/32", "a documentation address"},
		{"3fff::/20", "a documentation address"},
		{"fc00::/7", "private (ULA)"},
		{"fe80::/10", "link-local"},
		{"ff00::/8", "multicast"},
	} {
		out = append(out, struct {
			p    netip.Prefix
			what string
		}{netip.MustParsePrefix(r[0]), r[1]})
	}
	return out
}()

// addrClass returns why a is not a public address ("private", "loopback", "a documentation address", …), or "" when
// it is public.
func addrClass(a netip.Addr) string {
	a = a.Unmap()
	for _, r := range nonPublic {
		if r.p.Contains(a) {
			return r.what
		}
	}
	return ""
}

// auto needs domain; manual needs cert_file and key_file.
func (v *validator) tlsMode() {
	switch v.mode {
	case TLSAuto:
		if v.c.Domain == "" {
			v.errorf("tls.mode", `set domain to a DNS name that points to this server (domain = "watch.example.com"), or use tls.mode = "ip" for a certificate for the public IP`,
				"needs a domain")
		}
	case TLSManual:
		var missing []string
		if v.c.TLS.CertFile == "" {
			missing = append(missing, "tls.cert_file")
		}
		if v.c.TLS.KeyFile == "" {
			missing = append(missing, "tls.key_file")
		}
		if len(missing) > 0 {
			v.errorf("tls.mode", "set tls.cert_file to the certificate chain and tls.key_file to the private key: PEM files the isshoni user can read",
				"needs %s", strings.Join(missing, " and "))
		}
	}
}

// public_url, when set, is an absolute http(s) URL with only a scheme, host and port; in off mode it is https://
// unless its host is a loopback name.
func (v *validator) publicURL() {
	s := v.c.PublicURL
	if s == "" {
		return
	}
	const fix = "use the address your friends open, like https://watch.example.com"
	u, err := url.Parse(s)
	switch {
	case err != nil || !u.IsAbs() || u.Opaque != "" || u.Host == "" || u.Hostname() == "":
		v.errorf("public_url", fix, "is not an absolute URL")
	case u.Scheme != "https" && u.Scheme != "http":
		v.errorf("public_url", fix, "must start with https://")
	case u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		v.errorf("public_url", "isshoni is served at the root of its own host: "+fix, "must be only a scheme, a host and an optional port")
	case v.mode == TLSOff && u.Scheme == "http" && !isLoopbackName(u.Hostname()):
		v.errorf("public_url", "browsers need HTTPS for screen sharing and notifications: use the https:// address of your proxy",
			"must be https:// unless its host is localhost, 127.0.0.1 or [::1]")
	}
}

// isLoopbackName reports whether a URL host (without port) is one of the loopback names that may use http://:
// localhost, 127.0.0.1 or ::1 (06 §7.3).
func isLoopbackName(host string) bool {
	return strings.EqualFold(host, "localhost") || host == "127.0.0.1" || host == "::1"
}

// Off mode: public_url or a loopback listen.http; listen.https ignored; trusted_proxies for a non-loopback proxy.
func (v *validator) offMode() {
	if v.mode != TLSOff {
		return
	}
	loopback := isLoopbackListen(v.c.Listen.HTTP)
	if v.c.PublicURL == "" && !loopback {
		v.errorf("public_url", `set public_url to the https:// address of your proxy, e.g. public_url = "https://watch.example.com"`,
			`is required with tls.mode = "off" when listen.http = %s is not a loopback address`, tomlString(v.c.Listen.HTTP))
	}
	if v.c.IsSet("listen.https") {
		v.warnf("listen.https", "remove it: your proxy owns HTTPS, and isshoni serves plain HTTP on listen.http",
			`is ignored with tls.mode = "off"`)
	}
	if !v.c.IsSet("network.trusted_proxies") && !loopback {
		v.warnf("network.trusted_proxies", `set it to your proxy's address or network, e.g. ["172.30.89.0/24"]; otherwise every per-IP limit sees all your friends as one client`,
			`is not set, so with tls.mode = "off" and listen.http = %s every client IP will be the proxy's`, tomlString(v.c.Listen.HTTP))
	}
}

// Listen addresses parse as host:port; no two TCP listeners share a port; at least one listener carries media.
func (v *validator) listeners() {
	off := v.mode == TLSOff
	type listener struct {
		key      string
		val      string
		tcp      bool
		active   bool
		emptyOK  bool
		emptyFix string
	}
	ls := []listener{
		{"listen.https", v.c.Listen.HTTPS, true, !off, false, `use ":443"`},
		{"listen.http", v.c.Listen.HTTP, true, true, !off, `use "127.0.0.1:8080" for a proxy on this host`},
		{"listen.ice_udp", v.c.Listen.ICEUDP, false, true, true, ""},
		{"listen.ice_tcp", v.c.Listen.ICETCP, true, true, true, ""},
		{"metrics.listen", v.c.Metrics.Listen, true, v.c.Metrics.Enabled, false, `use "127.0.0.1:9469"`},
	}
	ports := map[int]string{}
	for _, l := range ls {
		if !l.active {
			continue
		}
		if l.val == "" {
			if !l.emptyOK {
				v.errorf(l.key, l.emptyFix, "is empty")
			}
			continue
		}
		host, port, ok := splitListen(l.val)
		if !ok {
			v.errorf(l.key, `use host:port, like ":443", "0.0.0.0:443" or "127.0.0.1:8080"`, "is not a host:port address")
			continue
		}
		if l.tcp && port != 0 {
			if other, dup := ports[port]; dup {
				key := l.key
				if !v.c.IsSet(key) && v.c.IsSet(other) { // blame the value someone set, not a default
					key, other = other, key
				}
				v.errorf(key, "give each TCP listener (listen.https, listen.http, listen.ice_tcp, metrics.listen) its own port",
					"uses port %d, like %s", port, other)
			} else {
				ports[port] = l.key
			}
		}
		if l.key == "metrics.listen" && !isLoopbackHost(host) {
			v.warnf(l.key, "use 127.0.0.1:9469 and scrape through an SSH tunnel or a local agent", "exposes unauthenticated metrics")
		}
	}
	if v.c.Listen.ICEUDP == "" && v.c.Listen.ICETCP == "" && (off || v.c.Listen.HTTPS == "") {
		msg := "is empty, and so is listen.ice_tcp: media has no way in"
		if off {
			msg += ` (with tls.mode = "off" the HTTPS port belongs to your proxy)`
		}
		v.errorf("listen.ice_udp", `set listen.ice_udp = ":7882" (best), or listen.ice_tcp = ":7882"`, "%s", msg)
	}
}

// splitListen parses a listen address: an optional host and a port from 0 to 65535.
func splitListen(addr string) (host string, port int, ok bool) {
	host, p, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, false
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 0 || n > 65535 || strings.HasPrefix(p, "+") {
		return "", 0, false
	}
	return host, n, true
}

// trusted_proxies are CIDRs (checked while parsing); stun_servers are host:port; interface globs are valid;
// udp_buffer_bytes is between 1 MiB and 64 MiB.
func (v *validator) network() {
	for _, s := range v.c.Network.STUNServers {
		host, port, ok := splitListen(s)
		if !ok || host == "" || port == 0 {
			v.errorf("network.stun_servers", `write each server as host:port, e.g. "stun.cloudflare.com:3478"`,
				"has %s, which is not host:port", tomlString(s))
		}
	}
	for _, g := range v.c.Network.ExcludeInterfaces {
		if _, err := path.Match(g, ""); err != nil {
			v.errorf("network.exclude_interfaces", `use shell globs like "docker*" or "veth*"`, "has %s, which is not a valid glob", tomlString(g))
		}
	}
	const mib = 1 << 20
	if n := v.c.Network.UDPBufferBytes; n < mib || n > 64*mib {
		v.errorf("network.udp_buffer_bytes", "use a value from 1048576 (1 MiB) to 67108864 (64 MiB); the default is 8388608 (8 MiB)",
			"is out of range")
	}
}

// maxSocketPath is the longest unix socket path macOS accepts (sun_path).
const maxSocketPath = 104

// admin_socket is at most 104 bytes; data_dir is not empty.
func (v *validator) paths() {
	switch s := v.c.Listen.AdminSocket; {
	case s == "":
		v.errorf("listen.admin_socket", `use "/run/isshoni/admin.sock"`, "is empty")
	case len(s) > maxSocketPath:
		v.errorf("listen.admin_socket", "use a shorter path, like /run/isshoni/admin.sock",
			"is %d bytes long, but a unix socket path can have at most %d (macOS)", len(s), maxSocketPath)
	}
	if v.c.DataDir == "" {
		v.errorf("data_dir", `use "/var/lib/isshoni"`, "is empty")
	}
}

// Durations are positive; the guards are at least 1.
func (v *validator) numbers() {
	if v.c.ShutdownTimeout.Duration <= 0 {
		v.errorf("shutdown_timeout", `use a positive duration like "10s"`, "is not positive")
	}
	if v.c.Limits.WSHandshakesPerIPPerMinute < 1 {
		v.errorf("limits.ws_handshakes_per_ip_per_minute", "use a positive number; the default is 20", "is less than 1")
	}
	if v.c.Limits.ConnsPerIP < 1 {
		v.errorf("limits.conns_per_ip", "use a positive number; the default is 256", "is less than 1")
	}
}

// push.subject starts with mailto: or https:.
func (v *validator) push() {
	if s := v.c.Push.Subject; s != "" && !strings.HasPrefix(s, "mailto:") && !strings.HasPrefix(s, "https:") {
		v.errorf("push.subject", `use "mailto:you@example.com" or an https:// URL, or leave it empty (then mailto:<tls.acme_email> or the public origin)`,
			"must start with mailto: or https:")
	}
}

// semver matches a SemVer 2.0.0 version without a leading v.
var semver = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)` +
	`(-((0|[1-9]\d*|\d*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9]\d*|\d*[A-Za-z-][0-9A-Za-z-]*))*))?` +
	`(\+([0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*))?$`)

// clients.min_version is SemVer.
func (v *validator) clients() {
	if s := v.c.Clients.MinVersion; s != "" && !semver.MatchString(s) {
		v.errorf("clients.min_version", `use a version like "0.3.0" (SemVer without a leading v), or "" for no minimum`, "is not a SemVer version")
	}
}

// tls.acme_staging and tls.acme_ca_root are for testing only.
func (v *validator) acme() {
	if v.c.TLS.ACMEStaging {
		v.warnf("tls.acme_staging", "remove it for production", "gives untrusted certificates: for testing only")
	}
	if v.c.TLS.ACMECARoot != "" {
		v.warnf("tls.acme_ca_root", "remove it for production", "trusts a private ACME server: for testing only")
	}
}
