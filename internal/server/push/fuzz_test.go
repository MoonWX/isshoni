package push

import (
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// FuzzParseEndpoint checks the URL rules of 04 §14.5 on arbitrary input: whatever parseEndpoint accepts is an
// https URL without userinfo, on port 443, whose host is a DNS name and no IP address in any spelling; whatever it
// refuses has one of the reasons of 03 §12.4.6. Neither it nor pushHost panics.
func FuzzParseEndpoint(f *testing.F) {
	for _, seed := range []string{
		"https://fcm.googleapis.com/fcm/send/dx1",
		"https://updates.push.services.mozilla.com/wpush/v2/gAAAA",
		"https://web.push.apple.com:443/QGu",
		"https://push.example.com./x?y=z#f",
		"http://push.example.com/x",
		"https://push.example.com:8443/x",
		"https://push.example.com:/x",
		"https://push.example.com:abc/x",
		"https://user:pw@push.example.com/x",
		"https://user@push.example.com:8443/x",
		"https://us er@exa mple.com:abc/x",
		"https://127.0.0.1/x",
		"https://[::1]/x",
		"https://[fe80::1%25eth0]/x",
		"https://2130706433/",
		"https://0x7f.1/",
		"https://0177.0.0.1/",
		"https://localhost/",
		"https://a.localhost./",
		"https://exa mple.com/",
		"https://bücher.example/",
		"https:///x",
		"https:opaque",
		"//push.example.com/x",
		"\x00",
		"",
	} {
		f.Add(seed)
	}
	reasons := map[api.PushRejectReason]bool{
		api.PushRejectReasonNotHTTPS: true, api.PushRejectReasonBadPort: true, api.PushRejectReasonUserinfo: true,
		api.PushRejectReasonIPLiteral: true, api.PushRejectReasonPrivateAddress: true,
		api.PushRejectReasonUnresolvable: true,
	}
	f.Fuzz(func(t *testing.T, raw string) {
		_ = pushHost(raw)
		u, reason := parseEndpoint(raw, false)
		if reason != "" {
			if u != nil || !reasons[reason] {
				t.Fatalf("parseEndpoint(%q) = %v, %q", raw, u, reason)
			}
			return
		}
		if u == nil {
			t.Fatalf("parseEndpoint(%q) accepted without a URL", raw)
		}
		if u.Scheme != "https" || u.Opaque != "" || u.User != nil {
			t.Fatalf("accepted %q: scheme %q, opaque %q, userinfo %v", raw, u.Scheme, u.Opaque, u.User)
		}
		if p := u.Port(); p != "" && p != "443" {
			t.Fatalf("accepted %q with port %q", raw, p)
		}
		host := u.Hostname()
		if _, err := netip.ParseAddr(host); err == nil {
			t.Fatalf("accepted %q: the host %q is an IP address", raw, host)
		}
		if !validDNSName(host) || endsInNumber(host) || isLocalhost(host) {
			t.Fatalf("accepted %q: the host %q is not a public DNS name", raw, host)
		}
		// The host the HTTP client would connect to is the one that was checked: nothing before an "@", and
		// only the checked port.
		again, err := url.Parse(u.String())
		if err != nil || again.Hostname() != host || again.User != nil {
			t.Fatalf("accepted %q: it reads back as host %q (%v)", raw, again.Hostname(), err)
		}
		if strings.ContainsAny(host, "@:/\\?#[]% \t\r\n") {
			t.Fatalf("accepted %q: the host %q has a character that is no part of a name", raw, host)
		}
	})
}
