package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
)

// Site is how the outside world reaches this server (§4.4). The wiring builds it once at startup, after public IP
// detection, and passes it to every component.
type Site struct {
	Origin   string // "https://watch.example.com", no trailing slash
	Host     string // "watch.example.com", "203.0.113.7", "[2001:db8::1]", with ":port" if not the default
	Hostname string // without port and brackets
	TLSMode  TLSMode
	// Dev is true only in off mode with a loopback listen.http and a public_url that is empty or on a loopback host
	// (localhost, 127.0.0.1, [::1]): then the Host check accepts any localhost Host header.
	Dev bool
	// ExtraOrigins are further allowed browser origins: later (M2) the Wails asset origins, added in code, not config.
	ExtraOrigins []string
}

// NewSite computes the site for the effective TLS mode (§4.4):
//
//	auto    https://<domain>, plus :port when listen.https is not on 443
//	ip      https://<public IPv4>, or https://[<public IPv6>] on an IPv6-only host
//	manual  https://<domain> if set, else https://<public IP>
//	off     public_url; dev (loopback listen.http, no public_url): http://localhost:<port>
//
// publicV4 and publicV6 are the detected (or configured) public addresses; either may be the zero Addr. A port of 0
// in a listen address (tests) is kept as is: the caller that binds ephemeral ports sets the real ones first.
func NewSite(c *Config, publicV4, publicV6 netip.Addr) (Site, error) {
	mode := c.EffectiveTLSMode()
	s := Site{TLSMode: mode}
	httpsPort := portOf(c.Listen.HTTPS)
	publicAddr := func() (netip.Addr, error) {
		switch {
		case publicV4.IsValid():
			return publicV4.Unmap(), nil
		case publicV6.IsValid():
			return publicV6, nil
		default:
			return netip.Addr{}, errors.New(`config: no public IP address is known: set public_ip, or set domain and tls.mode = "auto"`)
		}
	}
	switch mode {
	case TLSAuto:
		if c.Domain == "" {
			return Site{}, errors.New(`config: tls.mode "auto" needs a domain`)
		}
		s.setHost("https", strings.ToLower(c.Domain), httpsPort, "443")
	case TLSIP:
		a, err := publicAddr()
		if err != nil {
			return Site{}, err
		}
		s.setHost("https", a.String(), httpsPort, "443")
	case TLSManual:
		if c.Domain != "" {
			s.setHost("https", strings.ToLower(c.Domain), httpsPort, "443")
			break
		}
		a, err := publicAddr()
		if err != nil {
			return Site{}, err
		}
		s.setHost("https", a.String(), httpsPort, "443")
	case TLSOff:
		if c.PublicURL == "" {
			if !isLoopbackListen(c.Listen.HTTP) {
				return Site{}, errors.New(`config: tls.mode "off" needs public_url when listen.http is not a loopback address`)
			}
			s.setHost("http", "localhost", portOf(c.Listen.HTTP), "80")
			s.Dev = true
			break
		}
		u, err := url.Parse(c.PublicURL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return Site{}, fmt.Errorf("config: public_url %q is not an absolute http(s) URL", c.PublicURL)
		}
		scheme := strings.ToLower(u.Scheme)
		def := "443"
		if scheme == "http" {
			def = "80"
		}
		s.setHost(scheme, strings.ToLower(u.Hostname()), u.Port(), def)
		s.Dev = isLoopbackListen(c.Listen.HTTP) && isLoopbackName(s.Hostname)
	default:
		return Site{}, fmt.Errorf("config: unknown tls.mode %q", mode)
	}
	return s, nil
}

// setHost fills Host, Hostname and Origin; port is left out when it is empty or the scheme's default.
func (s *Site) setHost(scheme, hostname, port, defaultPort string) {
	s.Hostname = strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(hostname, "["), "]"), ".")
	host := s.Hostname
	if strings.Contains(host, ":") { // an IPv6 literal
		host = "[" + host + "]"
	}
	if port != "" && port != defaultPort {
		host = net.JoinHostPort(s.Hostname, port)
	}
	s.Host = host
	s.Origin = scheme + "://" + host
}

// portOf returns the port of a host:port address, or "".
func portOf(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return port
}

// URL returns s.Origin + pathAndFragment. pathAndFragment must start with "/", e.g. "/setup#token".
func (s Site) URL(pathAndFragment string) string {
	if !strings.HasPrefix(pathAndFragment, "/") {
		panic("config: Site.URL path must start with /: " + pathAndFragment)
	}
	return s.Origin + pathAndFragment
}
