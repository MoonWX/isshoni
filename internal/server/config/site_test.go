package config

import (
	"net/netip"
	"testing"
)

func TestNewSite(t *testing.T) {
	v4 := netip.MustParseAddr("203.0.113.7")
	v6 := netip.MustParseAddr("2001:db8::1")
	none := netip.Addr{}
	tests := []struct {
		name       string
		args       []string
		v4, v6     netip.Addr
		want       Site
		wantErrStr bool
	}{
		{"auto", []string{"--domain=Watch.Example.com"}, v4, v6,
			Site{Origin: "https://watch.example.com", Host: "watch.example.com", Hostname: "watch.example.com", TLSMode: TLSAuto}, false},
		{"auto on another port", []string{"--domain=watch.example.com", "--listen.https=:8443"}, v4, v6,
			Site{Origin: "https://watch.example.com:8443", Host: "watch.example.com:8443", Hostname: "watch.example.com", TLSMode: TLSAuto}, false},
		{"ip", nil, v4, v6,
			Site{Origin: "https://203.0.113.7", Host: "203.0.113.7", Hostname: "203.0.113.7", TLSMode: TLSIP}, false},
		{"ip on an IPv6-only host", nil, none, v6,
			Site{Origin: "https://[2001:db8::1]", Host: "[2001:db8::1]", Hostname: "2001:db8::1", TLSMode: TLSIP}, false},
		{"ip on an IPv6-only host, other port", []string{"--listen.https=[::]:8443"}, none, v6,
			Site{Origin: "https://[2001:db8::1]:8443", Host: "[2001:db8::1]:8443", Hostname: "2001:db8::1", TLSMode: TLSIP}, false},
		{"ip without an address", nil, none, none, Site{}, true},
		{"manual with domain", []string{"--tls.mode=manual", "--domain=watch.example.com"}, v4, none,
			Site{Origin: "https://watch.example.com", Host: "watch.example.com", Hostname: "watch.example.com", TLSMode: TLSManual}, false},
		{"manual with an IP", []string{"--tls.mode=manual"}, v4, none,
			Site{Origin: "https://203.0.113.7", Host: "203.0.113.7", Hostname: "203.0.113.7", TLSMode: TLSManual}, false},
		{"off behind a proxy", []string{"--tls.mode=off", "--public-url=https://Share.Example.com/", "--listen.http=0.0.0.0:8080"}, v4, none,
			Site{Origin: "https://share.example.com", Host: "share.example.com", Hostname: "share.example.com", TLSMode: TLSOff}, false},
		{"off with a proxy on this host", []string{"--tls.mode=off", "--public-url=https://share.example.com:8443"}, none, none,
			Site{Origin: "https://share.example.com:8443", Host: "share.example.com:8443", Hostname: "share.example.com", TLSMode: TLSOff}, false},
		{"off dev without public_url", []string{"--tls.mode=off"}, none, none,
			Site{Origin: "http://localhost:8080", Host: "localhost:8080", Hostname: "localhost", TLSMode: TLSOff, Dev: true}, false},
		{"off dev with the Vite origin", []string{"--tls.mode=off", "--public-url=http://localhost:5173"}, none, none,
			Site{Origin: "http://localhost:5173", Host: "localhost:5173", Hostname: "localhost", TLSMode: TLSOff, Dev: true}, false},
		{"off e2e", []string{"--tls.mode=off", "--public-url=http://127.0.0.1:18080", "--listen.http=127.0.0.1:18080"}, none, none,
			Site{Origin: "http://127.0.0.1:18080", Host: "127.0.0.1:18080", Hostname: "127.0.0.1", TLSMode: TLSOff, Dev: true}, false},
		{"off [::1]", []string{"--tls.mode=off", "--public-url=http://[::1]:8080", "--listen.http=[::1]:8080"}, none, none,
			Site{Origin: "http://[::1]:8080", Host: "[::1]:8080", Hostname: "::1", TLSMode: TLSOff, Dev: true}, false},
		{"off loopback listener, public https URL: not dev", []string{"--tls.mode=off", "--public-url=https://share.example.com"}, none, none,
			Site{Origin: "https://share.example.com", Host: "share.example.com", Hostname: "share.example.com", TLSMode: TLSOff}, false},
		{"off https localhost: dev", []string{"--tls.mode=off", "--public-url=https://localhost"}, none, none,
			Site{Origin: "https://localhost", Host: "localhost", Hostname: "localhost", TLSMode: TLSOff, Dev: true}, false},
		{"off non-loopback listener with a localhost URL: not dev", []string{"--tls.mode=off", "--public-url=http://localhost:8080", "--listen.http=0.0.0.0:8080"}, none, none,
			Site{Origin: "http://localhost:8080", Host: "localhost:8080", Hostname: "localhost", TLSMode: TLSOff}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := testLoad(t, "", nil, tt.args...)
			got, err := NewSite(c, tt.v4, tt.v6)
			if (err != nil) != tt.wantErrStr {
				t.Fatalf("err = %v", err)
			}
			if got.Origin != tt.want.Origin || got.Host != tt.want.Host || got.Hostname != tt.want.Hostname ||
				got.TLSMode != tt.want.TLSMode || got.Dev != tt.want.Dev {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

// Dev is true only for off mode with a loopback listen.http and an empty or loopback public_url (§17).
func TestSiteDevOnlyInOffMode(t *testing.T) {
	for _, args := range [][]string{
		{"--listen.http=127.0.0.1:8080", "--domain=watch.example.com"},
		{"--listen.http=127.0.0.1:8080", "--tls.mode=manual", "--tls.cert-file=c", "--tls.key-file=k"},
		{"--listen.http=127.0.0.1:8080", "--public-url=http://localhost:8080"},
	} {
		c, _ := testLoad(t, "", nil, args...)
		s, err := NewSite(c, netip.MustParseAddr("8.8.8.8"), netip.Addr{})
		if err != nil || s.Dev {
			t.Errorf("%v: %+v, %v", args, s, err)
		}
	}
}

func TestSiteURL(t *testing.T) {
	s := Site{Origin: "https://watch.example.com"}
	if got := s.URL("/setup#abc"); got != "https://watch.example.com/setup#abc" {
		t.Errorf("URL = %q", got)
	}
	defer func() {
		if recover() == nil {
			t.Error("URL without a leading / did not panic")
		}
	}()
	_ = s.URL("setup")
}

func TestPaths(t *testing.T) {
	c := mustLoad(t, "", nil, "--data-dir=/var/lib/isshoni")
	want := Paths{ //nolint:gosec // G101: file paths, not credentials
		DataDir: "/var/lib/isshoni", DB: "/var/lib/isshoni/isshoni.db", Secrets: "/var/lib/isshoni/secrets.json",
		CertMagic: "/var/lib/isshoni/certmagic", Backups: "/var/lib/isshoni/backups", Restore: "/var/lib/isshoni/restore",
		Lock: "/var/lib/isshoni/isshoni.lock",
	}
	if got := c.Paths(); got != want && !isWindowsPaths(got) {
		t.Errorf("Paths() = %+v", got)
	}
}

// isWindowsPaths accepts the backslash form filepath.Join gives on Windows.
func isWindowsPaths(p Paths) bool { return len(p.DB) > 0 && p.DB[len(p.DataDir)] == '\\' }

func TestSecretsNotImplemented(t *testing.T) {
	s, err := OpenSecrets("secrets.json", nil)
	if s != nil || err != ErrSecretsNotImplemented { //nolint:errorlint // the exact sentinel
		t.Errorf("OpenSecrets = %v, %v", s, err)
	}
	if _, err := (&SecretStore{}).Rotate(t.Context(), []KeyName{KeySession}, true); err != ErrSecretsNotImplemented { //nolint:errorlint // the exact sentinel
		t.Errorf("Rotate: %v", err)
	}
}
