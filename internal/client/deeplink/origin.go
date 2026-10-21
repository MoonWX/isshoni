package deeplink

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/idna"
)

// ErrInvalidOrigin is returned (wrapped, with the rule that failed and never the input) by NormalizeOrigin for
// anything that isn't a server address it accepts. 03-desktop-app maps it to invalid_address.
var ErrInvalidOrigin = errors.New("deeplink: invalid server address")

// MaxBytes bounds a link and an address NormalizeOrigin reads, in bytes.
const MaxBytes = 2048

const (
	maxHostBytes  = 253 // a DNS name in ASCII, without a trailing dot (RFC 1035 §2.3.4)
	maxLabelBytes = 63
)

// loopbackHosts are the only hosts NormalizeOrigin accepts http for, and only with allowInsecureLoopback (02 §11.2,
// 03 §8.1): dev and e2e builds talk to a dev server on the same machine.
var loopbackHosts = map[string]bool{"localhost": true, "127.0.0.1": true, "[::1]": true}

// NormalizeOrigin turns what a user typed or a link named into "https://host[:port]": the host as for links, no path
// other than "/", no query, fragment or userinfo; http only for a loopback host when allowInsecureLoopback.
//
// Accepted: an address with "https://" or "http://" (the scheme in any case), or one without a scheme, which is
// https ("watch.example.com", "watch.example.com:8443", an IP literal, an IPv6 literal also without brackets and
// then without a port). The host is canonicalized as in a link (see Parse): an IDN in its ASCII (xn--) form, lower
// case, IPv6 compressed in brackets; the default port of the scheme (443 or 80) is dropped. Surrounding white space
// is trimmed; any other space, a control character, invalid UTF-8 or more than MaxBytes is refused. The loopback
// hosts are exactly localhost, 127.0.0.1 and [::1]; https needs no permission for them. The result has no trailing
// slash.
//
// Any other scheme, a path other than "/", a query or a fragment (even an empty "?" or "#"), userinfo, a host that
// isn't a DNS name or IP literal, or a port outside 1–65535 is refused with an error wrapping ErrInvalidOrigin.
// 03-desktop-app's address field drops a URL's path, query and fragment itself before it calls this.
func NormalizeOrigin(raw string, allowInsecureLoopback bool) (string, error) {
	s := strings.TrimSpace(raw)
	if rule := checkChars(s); rule != "" {
		return "", originErr(rule)
	}
	scheme := "https"
	if i := strings.Index(s, "://"); i >= 0 {
		switch {
		case equalFoldASCII(s[:i], "https"):
			scheme = "https"
		case equalFoldASCII(s[:i], "http"):
			scheme = "http"
		default:
			return "", originErr("the scheme is neither https nor http")
		}
		s = s[i+len("://"):]
	} else if a, err := netip.ParseAddr(s); err == nil && a.Is6() && a.Zone() == "" {
		s = "[" + s + "]" // an IPv6 literal typed without brackets: two or more colons can't be host:port
	}
	auth, path := s, ""
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		auth, path = s[:i], s[i:]
	}
	if path != "" && path != "/" {
		return "", originErr("an address has no path, query or fragment")
	}
	host, port, rule := parseAuthority(auth)
	if rule != "" {
		return "", originErr(rule)
	}
	if scheme == "http" && (!allowInsecureLoopback || !loopbackHosts[host]) {
		return "", originErr("http is allowed only for a loopback host in a dev build")
	}
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	return scheme + "://" + joinHostPort(host, port), nil
}

func originErr(rule string) error { return fmt.Errorf("%w: %s", ErrInvalidOrigin, rule) }

// checkChars applies the rules every link and address shares (02 §12.1): at most MaxBytes, valid UTF-8, no spaces
// (white space or a Unicode separator), no control characters and no bidi controls. It returns the rule that failed,
// or "". It reads only the characters' classes, never what they say.
func checkChars(s string) string {
	switch {
	case len(s) > MaxBytes:
		return "longer than 2048 bytes"
	case !utf8.ValidString(s):
		return "not valid UTF-8"
	}
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.In(r, unicode.Z, unicode.Bidi_Control) {
			return "contains a space or a control character"
		}
	}
	return ""
}

// parseAuthority splits and canonicalizes "host[:port]" (no userinfo): the canonical host (see canonicalHost) and
// the port as written ("" when absent), which is 1–65535 without leading zeros. It returns the rule that failed, or
// "".
func parseAuthority(a string) (host, port, rule string) {
	if strings.Contains(a, "@") {
		return "", "", "userinfo is not allowed"
	}
	hostPart := a
	if strings.HasPrefix(a, "[") {
		end := strings.IndexByte(a, ']')
		if end < 0 {
			return "", "", "an IPv6 literal without its closing bracket"
		}
		hostPart, port = a[:end+1], a[end+1:]
		if port != "" {
			if port[0] != ':' {
				return "", "", "characters after an IPv6 literal"
			}
			port = port[1:]
			if port == "" {
				return "", "", "an empty port"
			}
		}
	} else if i := strings.LastIndexByte(a, ':'); i >= 0 {
		hostPart, port = a[:i], a[i+1:]
		if port == "" {
			return "", "", "an empty port"
		}
	}
	if port != "" && !validPort(port) {
		return "", "", "the port is not 1-65535"
	}
	host, rule = canonicalHost(hostPart)
	return host, port, rule
}

// validPort reports whether p is a decimal 1–65535 without a leading zero.
func validPort(p string) bool {
	if len(p) > 5 || p[0] == '0' {
		return false
	}
	n := 0
	for i := range len(p) {
		if p[i] < '0' || p[i] > '9' {
			return false
		}
		n = n*10 + int(p[i]-'0')
	}
	return n >= 1 && n <= 65535
}

// canonicalHost returns the one spelling of a host (02 §12.1):
//   - "[IPv6]": a valid IPv6 address without a zone, compressed and lower case in brackets ("[2001:db8::1]");
//   - an IPv4 address in dotted decimal, exactly as netip parses it (no leading zeros, four parts);
//   - otherwise a DNS name, IDN allowed: idna.Lookup.ToASCII (lower case, UTS #46 mapping, STD3 rules, the bidi
//     and joiner checks), then labels of 1–63 bytes, at most 253 bytes, no trailing dot, and the result must be its
//     own ToASCII. A name whose last label is a number ("127.1", "a.0x7f", a fullwidth "１２７.０.０.１") is refused:
//     browsers and resolvers read those as IPv4 addresses, which would show the user one host and connect to
//     another.
//
// Nothing is percent-decoded: a "%" is refused by the STD3 rules. It returns the rule that failed, or "".
func canonicalHost(h string) (string, string) {
	if h == "" {
		return "", "the host is empty"
	}
	if strings.HasPrefix(h, "[") {
		a, err := netip.ParseAddr(strings.TrimSuffix(h[1:], "]"))
		if err != nil || !a.Is6() || a.Zone() != "" || !strings.HasSuffix(h, "]") {
			return "", "not an IPv6 literal"
		}
		return "[" + a.String() + "]", ""
	}
	if a, err := netip.ParseAddr(h); err == nil {
		if !a.Is4() {
			return "", "an IPv6 literal needs brackets"
		}
		return a.String(), ""
	}
	ascii, err := idna.Lookup.ToASCII(h)
	if err != nil || ascii == "" {
		return "", "not a valid host name"
	}
	if again, err := idna.Lookup.ToASCII(ascii); err != nil || again != ascii {
		return "", "not a valid host name"
	}
	if len(ascii) > maxHostBytes {
		return "", "the host name is too long"
	}
	labels := strings.Split(ascii, ".")
	for _, l := range labels {
		if l == "" || len(l) > maxLabelBytes {
			return "", "a host name label is empty or too long"
		}
	}
	if numericLabel(labels[len(labels)-1]) {
		return "", "a host name that ends in a number"
	}
	return ascii, ""
}

// numericLabel reports whether a label is a number as the WHATWG URL parser's IPv4 rules see it ("ends in a
// number"): all decimal digits, or "0x"/"0X" followed by hex digits (or nothing).
func numericLabel(l string) bool {
	digits := "0123456789"
	if len(l) >= 2 && l[0] == '0' && (l[1] == 'x' || l[1] == 'X') {
		l, digits = l[2:], "0123456789abcdefABCDEF"
		if l == "" {
			return true
		}
	}
	for i := range len(l) {
		if !strings.ContainsRune(digits, rune(l[i])) {
			return false
		}
	}
	return l != ""
}

// unicodeHost returns the Unicode form of a canonical host when it has an xn-- label, else the host itself.
func unicodeHost(host string) (string, bool) {
	if !strings.HasPrefix(host, "xn--") && !strings.Contains(host, ".xn--") {
		return host, true
	}
	u, err := idna.Lookup.ToUnicode(host)
	if err != nil {
		return "", false
	}
	return u, true
}

// joinHostPort appends ":port" unless port is "". The host is canonical, so an IPv6 one already has its brackets.
func joinHostPort(host, port string) string {
	if port == "" {
		return host
	}
	return host + ":" + port
}
