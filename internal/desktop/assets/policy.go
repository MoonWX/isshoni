package assets

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// permissionsPolicy is the Permissions-Policy of every HTML response (§5.3). display-capture=(): the app shares
// natively, so getDisplayMedia stays off in its webview.
const permissionsPolicy = "fullscreen=(self), picture-in-picture=(self), autoplay=(self), display-capture=(), " +
	"camera=(), microphone=(), geolocation=(), usb=(), payment=()"

// setHTMLHeaders sets the headers of §5.3 on an HTML response.
func setHTMLHeaders(h http.Header, csp string) {
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Permissions-Policy", permissionsPolicy)
}

// policy returns the CSP of §5.3 for c's active server: connect-src names https:// and wss:// of its host (with the
// port); without a server, or with an origin that isn't an https origin, only 'self'. In dev and e2e builds
// (Options.Dev) an http:// loopback server gets http:// and ws:// instead. With the dev server (dev.go), its origin is
// added to script-src, style-src and connect-src (and its ws:// for live reload), and nonce, when set, to script-src
// and style-src for the inline scripts and styles Vite's dev pages use. The policy never holds '"', '<', '>' or '&':
// every source is built from a parsed, validated host.
func (h *handler) policy(c BridgeConfig, nonce string) string {
	script := []string{"'self'"}
	style := []string{"'self'"}
	connect := []string{"'self'"}
	if c.Server != nil {
		connect = append(connect, serverSources(c.Server.Origin, h.opts.Dev)...)
	}
	if h.dev != nil {
		script = append(script, h.dev.origin)
		style = append(style, h.dev.origin)
		connect = append(connect, h.dev.origin, h.dev.wsOrigin)
	}
	if nonce != "" {
		script = append(script, "'nonce-"+nonce+"'")
		style = append(style, "'nonce-"+nonce+"'")
	}
	return strings.Join([]string{
		"default-src 'self'",
		"script-src " + strings.Join(script, " "),
		"style-src " + strings.Join(style, " "),
		"img-src 'self' data: blob:",
		"media-src 'self' blob:",
		"connect-src " + strings.Join(connect, " "),
		"worker-src 'none'",
		"manifest-src 'none'",
		"font-src 'self'",
		"object-src 'none'",
		"base-uri 'none'",
		"form-action 'none'",
		"frame-src 'none'",
		"frame-ancestors 'none'",
	}, "; ")
}

// serverSources returns the connect-src sources of a server origin: https://<host> and wss://<host>; for an http://
// loopback origin, only when dev, http://<host> and ws://<host>; nothing for anything else.
func serverSources(origin string, dev bool) []string {
	scheme, host, err := parseOrigin(origin)
	if err != nil {
		return nil
	}
	switch {
	case scheme == "https":
		return []string{"https://" + host, "wss://" + host}
	case scheme == "http" && dev && isLoopbackHost(host):
		return []string{"http://" + host, "ws://" + host}
	default:
		return nil
	}
}

// parseOrigin splits an http(s) origin ("https://watch.example.com:8443", no path, query, fragment or user info)
// into its scheme and lower-case host with the port. The host holds only letters, digits, '-', '.' and, for an IPv6
// literal, its brackets, hex digits and ':'; the port only digits.
func parseOrigin(origin string) (scheme, host string, err error) {
	u, err := url.Parse(origin)
	if err != nil {
		return "", "", err
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", "", fmt.Errorf("origin %q: not http or https", origin)
	}
	if u.Opaque != "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" {
		return "", "", fmt.Errorf("origin %q: not a bare origin", origin)
	}
	host = strings.ToLower(u.Host)
	if !validHost(host) {
		return "", "", fmt.Errorf("origin %q: invalid host", origin)
	}
	return u.Scheme, host, nil
}

// validHost reports whether host (lower case, with an optional port) is a DNS name, an IPv4 address or a bracketed
// IPv6 literal, with only the characters a CSP source may hold unquoted.
func validHost(host string) bool {
	name, port := host, ""
	if strings.HasPrefix(host, "[") {
		end := strings.IndexByte(host, ']')
		if end < 0 {
			return false
		}
		name, port = host[:end+1], host[end+1:]
		if ip := net.ParseIP(name[1 : len(name)-1]); ip == nil || ip.To4() != nil {
			return false
		}
		if !onlyOf(name[1:len(name)-1], true, ":") {
			return false
		}
	} else {
		if i := strings.LastIndexByte(host, ':'); i >= 0 {
			name, port = host[:i], host[i:]
		}
		if name == "" || len(name) > 253 {
			return false
		}
		if !onlyOf(name, false, "-.") {
			return false
		}
	}
	if port != "" {
		digits := strings.TrimPrefix(port, ":")
		if digits == port || digits == "" || len(digits) > 5 {
			return false
		}
		for _, c := range digits {
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	return true
}

// isLoopbackHost reports whether host (with an optional port) is localhost or a loopback IP literal.
func isLoopbackHost(host string) bool {
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	name = strings.TrimSuffix(strings.TrimPrefix(name, "["), "]")
	if name == "localhost" {
		return true
	}
	ip := net.ParseIP(name)
	return ip != nil && ip.IsLoopback()
}

// checkAssetOrigin checks Options.Origin: a bare scheme://host origin such as "http://wails.localhost" or
// "wails://localhost" (Wails on macOS), compared byte for byte with the Origin header.
func checkAssetOrigin(origin string) error {
	scheme, rest, ok := strings.Cut(origin, "://")
	if !ok || scheme == "" || rest == "" {
		return errors.New("assets: Options.Origin must be an origin such as http://wails.localhost")
	}
	if !onlyOf(scheme, false, "+-.") {
		return errors.New("assets: Options.Origin has an invalid scheme")
	}
	if !validHost(rest) {
		return errors.New("assets: Options.Origin has an invalid host")
	}
	return nil
}

// onlyOf reports whether every byte of s is a digit, a lower-case letter (a–f only with hexOnly) or a byte of extra.
func onlyOf(s string, hexOnly bool, extra string) bool {
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		case c >= 'g' && c <= 'z' && !hexOnly:
		case strings.IndexByte(extra, c) >= 0:
		default:
			return false
		}
	}
	return true
}
