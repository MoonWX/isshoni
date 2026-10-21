package deeplink_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/MoonWX/isshoni/internal/client/deeplink"
)

func TestNormalizeOrigin(t *testing.T) {
	label63 := strings.Repeat("a", 63)
	host253 := label63 + "." + label63 + "." + label63 + "." + strings.Repeat("d", 61)
	const refused = ""
	tests := []struct {
		name     string
		raw      string
		loopback bool // allowInsecureLoopback
		want     string
	}{
		// What a user types.
		{"a bare host", "watch.example.com", false, "https://watch.example.com"},
		{"https", "https://watch.example.com", false, "https://watch.example.com"},
		{"a trailing slash", "https://watch.example.com/", false, "https://watch.example.com"},
		{"a bare host with a slash", "watch.example.com/", false, "https://watch.example.com"},
		{"surrounding white space", "  https://watch.example.com/ \t\n", false, "https://watch.example.com"},
		{"case", "HTTPS://Watch.EXAMPLE.com", false, "https://watch.example.com"},
		{"a single label", "intranet", false, "https://intranet"},

		// Paths, queries and fragments.
		{"a path", "https://watch.example.com/r/lounge", false, refused},
		{"a bare host with a path", "watch.example.com/r/lounge", false, refused},
		{"a double slash", "https://watch.example.com//", false, refused},
		{"an invite path", "https://watch.example.com/invite#token", false, refused},
		{"a query", "https://watch.example.com/?x=1", false, refused},
		{"an empty query", "https://watch.example.com?", false, refused},
		{"a fragment", "https://watch.example.com#x", false, refused},
		{"an empty fragment", "https://watch.example.com/#", false, refused},
		{"a backslash path", `https://watch.example.com\`, false, refused},

		// Ports.
		{"port 443 is dropped", "https://watch.example.com:443", false, "https://watch.example.com"},
		{"port 443 on a bare host", "watch.example.com:443", false, "https://watch.example.com"},
		{"another port is kept", "https://watch.example.com:8443/", false, "https://watch.example.com:8443"},
		{"a bare host with a port", "watch.example.com:8443", false, "https://watch.example.com:8443"},
		{"port 80 with https is kept", "https://watch.example.com:80", false, "https://watch.example.com:80"},
		{"port 1", "https://watch.example.com:1", false, "https://watch.example.com:1"},
		{"port 65535", "https://watch.example.com:65535", false, "https://watch.example.com:65535"},
		{"port 0", "https://watch.example.com:0", false, refused},
		{"port 65536", "https://watch.example.com:65536", false, refused},
		{"a port of six digits", "https://watch.example.com:000443", false, refused},
		{"a leading zero", "https://watch.example.com:0443", false, refused},
		{"an empty port", "https://watch.example.com:", false, refused},
		{"a named port", "https://watch.example.com:https", false, refused},
		{"a negative port", "https://watch.example.com:-1", false, refused},
		{"a plus sign", "https://watch.example.com:+443", false, refused},
		{"two ports", "https://watch.example.com:443:443", false, refused},

		// Userinfo.
		{"a user", "https://user@watch.example.com", false, refused},
		{"user and password", "https://user:pass@watch.example.com", false, refused},
		{"an empty userinfo", "https://@watch.example.com", false, refused},
		{"userinfo on a bare host", "user@watch.example.com", false, refused},
		{"mailto", "mailto:user@watch.example.com", false, refused},

		// IDN and host names.
		{"an IDN in Unicode", "https://bücher.example", false, "https://xn--bcher-kva.example"},
		{"an IDN in upper case", "BÜCHER.example:8443", false, "https://xn--bcher-kva.example:8443"},
		{"an IDN in ASCII", "https://xn--bcher-kva.example/", false, "https://xn--bcher-kva.example"},
		{"an IDN in ASCII upper case", "https://XN--BCHER-KVA.EXAMPLE", false, "https://xn--bcher-kva.example"},
		{"a non-transitional ß", "straße.example", false, "https://xn--strae-oqa.example"},
		{"fullwidth dots", "watch．example．com", false, "https://watch.example.com"},
		{"an IDN top-level domain", "bücher.рф", false, "https://xn--bcher-kva.xn--p1ai"},
		{"a right-to-left name", "مثال.إختبار", false, "https://xn--mgbh0fb.xn--kgbechtv"},
		{"invalid punycode", "https://xn--abc.example", false, refused},
		{"a mixed bidi label", "aمثال.example", false, refused},
		{"an underscore", "https://a_b.example", false, refused},
		{"a leading hyphen", "-watch.example.com", false, refused},
		{"a trailing hyphen", "watch-.example.com", false, refused},
		{"hyphens in the third and fourth place", "wa--tch.example.com", false, refused},
		{"a trailing dot", "watch.example.com.", false, refused},
		{"an empty label", "watch..example.com", false, refused},
		{"a leading dot", ".watch.example.com", false, refused},
		{"a label of 63 bytes", label63 + ".example", false, "https://" + label63 + ".example"},
		{"a label of 64 bytes", label63 + "a.example", false, refused},
		{"a name of 253 bytes", host253, false, "https://" + host253},
		{"a name of 254 bytes", host253 + "d", false, refused},
		{"percent escapes are not decoded", "https://ex%61mple.com", false, refused},
		{"a quote", `https://watch"example.com`, false, refused},
		{"an asterisk", "*.example.com", false, refused},
		{"an empty host", "https://", false, refused},
		{"an empty host with a port", "https://:443", false, refused},
		{"nothing", "", false, refused},
		{"only white space", " \t ", false, refused},

		// IP literals.
		{"IPv4", "192.0.2.10", false, "https://192.0.2.10"},
		{"IPv4 with a port", "https://192.0.2.10:8443", false, "https://192.0.2.10:8443"},
		{"IPv6 in brackets", "https://[2001:DB8::1]:8443/", false, "https://[2001:db8::1]:8443"},
		{"IPv6 in brackets, port 443", "[2001:db8:0:0:0:0:0:1]:443", false, "https://[2001:db8::1]"},
		{"IPv6 without brackets or scheme", "2001:db8::1", false, "https://[2001:db8::1]"},
		{"IPv6 without brackets after a scheme", "https://2001:db8::1", false, refused},
		{"IPv6 with a zone", "https://[fe80::1%25en0]", false, refused},
		{"IPv6 with a zone, no brackets", "fe80::1%en0", false, refused},
		{"IPv4 in brackets", "https://[192.0.2.1]", false, refused},
		{"an unclosed bracket", "https://[2001:db8::1", false, refused},
		{"text after the bracket", "https://[2001:db8::1]x", false, refused},
		{"a short IPv4 form", "https://127.1", false, refused},
		{"a hex IPv4 part", "https://0x7f.0.0.1", false, refused},
		{"an octal-looking IPv4", "https://010.0.0.1", false, refused},
		{"a decimal IPv4 number", "https://2130706433", false, refused},
		{"a fullwidth IPv4", "１２７.０.０.１", false, refused},
		{"a name that ends in a number", "watch.example.123", false, refused},
		{"a name that ends in hex", "watch.example.0xff", false, refused},
		{"a name that ends in 0x", "watch.example.0x", false, refused},
		{"a name with a numeric label inside", "123.example.com", false, "https://123.example.com"},
		{"a name that ends in a word with digits", "watch.example.a1", false, "https://watch.example.a1"},

		// Schemes.
		{"http without permission", "http://watch.example.com", false, refused},
		{"http for another host with permission", "http://watch.example.com", true, refused},
		{"http loopback without permission", "http://localhost:8080", false, refused},
		{"https needs no permission for loopback", "https://localhost:8443", false, "https://localhost:8443"},
		{"https for [::1]", "https://[::1]", false, "https://[::1]"},
		{"a scheme without //", "https:watch.example.com", false, refused},
		{"a scheme with one slash", "https:/watch.example.com", false, refused},
		{"ftp", "ftp://watch.example.com", false, refused},
		{"ws", "wss://watch.example.com", false, refused},
		{"an isshoni link", "isshoni://watch.example.com/", false, refused},
		{"javascript", "javascript:alert(1)", false, refused},
		{"file", "file:///C:/x", false, refused},
		{"a UNC path", `\\host\share`, false, refused},
		{"a long s in the scheme", "httpſ://watch.example.com", false, refused},
		{"a space in the scheme", "ht tps://watch.example.com", false, refused},

		// Loopback http, only when allowed and only for the three hosts.
		{"http localhost", "http://localhost:8080", true, "http://localhost:8080"},
		{"http localhost in upper case", "HTTP://LOCALHOST:8080/", true, "http://localhost:8080"},
		{"http localhost, port 80 dropped", "http://localhost:80", true, "http://localhost"},
		{"http 127.0.0.1", "http://127.0.0.1:5173", true, "http://127.0.0.1:5173"},
		{"http [::1]", "http://[::1]:8080", true, "http://[::1]:8080"},
		{"http [::1] spelled out", "http://[0:0:0:0:0:0:0:1]", true, "http://[::1]"},
		{"http 127.0.0.2 isn't one of the three", "http://127.0.0.2:8080", true, refused},
		{"http localhost. with a dot", "http://localhost.:8080", true, refused},
		{"http sub.localhost", "http://dev.localhost:8080", true, refused},
		{"http 0.0.0.0", "http://0.0.0.0:8080", true, refused},
		{"http [::ffff:127.0.0.1]", "http://[::ffff:127.0.0.1]", true, refused},
		{"a bare loopback host is https", "localhost:8080", true, "https://localhost:8080"},
		{"https loopback with permission", "https://127.0.0.1", true, "https://127.0.0.1"},

		// Characters.
		{"a space inside", "watch example.com", false, refused},
		{"a tab inside", "https://watch.example.com\t:8443", false, refused},
		{"a control character", "watch.example.com\x00", false, refused},
		{"DEL", "watch.example.com\x7f", false, refused},
		{"a no-break space inside", "watch\u00a0example.com", false, refused},
		{"a no-break space around is white space", "\u00a0watch.example.com\u00a0", false, "https://watch.example.com"},
		{"a right-to-left override", "\u202ewatch.example.com", false, refused},
		{"invalid UTF-8", "watch.exa\xffmple.com", false, refused},
		{"longer than 2048 bytes", "https://" + strings.Repeat("a", 2041), false, refused},
	}
	names := map[string]bool{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if names[tt.name] {
				t.Fatalf("duplicate case name %q", tt.name)
			}
			names[tt.name] = true
			got, err := deeplink.NormalizeOrigin(tt.raw, tt.loopback)
			if tt.want == refused {
				if err == nil || !errors.Is(err, deeplink.ErrInvalidOrigin) || got != "" {
					t.Fatalf("NormalizeOrigin(%q, %v) = %q, %v; want ErrInvalidOrigin", tt.raw, tt.loopback, got, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("NormalizeOrigin(%q, %v) = %q, %v; want %q", tt.raw, tt.loopback, got, err, tt.want)
			}
			// Normal forms are fixed points, also with loopback allowed.
			for _, lb := range []bool{tt.loopback, true} {
				if again, err := deeplink.NormalizeOrigin(got, lb); err != nil || again != got {
					t.Errorf("NormalizeOrigin(%q, %v) = %q, %v; want it unchanged", got, lb, again, err)
				}
			}
		})
	}
}

// TestNormalizeOriginErrorsNameNoInput: the error text carries the rule, never the address (it may be logged).
func TestNormalizeOriginErrorsNameNoInput(t *testing.T) {
	for _, raw := range []string{
		"https://secret:secret@watch.example.com",
		"https://watch.example.com/secret",
		"http://secret.example.com",
		"secret://watch.example.com",
		"https://secret_x.example",
	} {
		_, err := deeplink.NormalizeOrigin(raw, false)
		if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "watch") {
			t.Errorf("NormalizeOrigin(%q) error = %v, want one that names no part of the input", raw, err)
		}
	}
}

// TestNormalizeOriginAgreesWithParse: a link's Origin is NormalizeOrigin of its own https origin.
func TestNormalizeOriginAgreesWithParse(t *testing.T) {
	for _, c := range loadLinks(t).Parse {
		if !c.OK {
			continue
		}
		if got, err := deeplink.NormalizeOrigin(c.Origin, false); err != nil || got != c.Origin {
			t.Errorf("NormalizeOrigin(%q) = %q, %v; want it unchanged", c.Origin, got, err)
		}
		if got, err := deeplink.NormalizeOrigin("https://"+c.HostUnicode, false); err != nil || got != c.Origin {
			t.Errorf("NormalizeOrigin(https://%s) = %q, %v; want %q", c.HostUnicode, got, err, c.Origin)
		}
	}
}
