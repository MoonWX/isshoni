package httpapi

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func prefixes(t testing.TB, s ...string) []netip.Prefix {
	t.Helper()
	out := make([]netip.Prefix, 0, len(s))
	for _, p := range s {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}

// TestClientIP covers 04 §8.5: X-Forwarded-For and X-Forwarded-Proto count only in off mode and only from a trusted
// peer; the client is the right-most entry that is not a trusted proxy.
func TestClientIP(t *testing.T) {
	loopback := []string{"127.0.0.0/8", "::1/128"}
	tests := []struct {
		name    string
		mode    TLSMode
		dev     bool
		tls     bool
		trusted []string
		remote  string
		xff     []string
		xfp     []string
		wantIP  string
		secure  bool
	}{
		{name: "auto mode ignores XFF and XFP", mode: "auto", tls: true, trusted: loopback, remote: "127.0.0.1:5000",
			xff: []string{"203.0.113.9"}, xfp: []string{"http"}, wantIP: "127.0.0.1", secure: true},
		{name: "ip mode ignores XFF", mode: "ip", trusted: loopback, remote: "198.51.100.7:4000",
			xff: []string{"203.0.113.9"}, wantIP: "198.51.100.7"},
		{name: "manual mode ignores XFF", mode: "manual", trusted: loopback, remote: "127.0.0.1:4000",
			xff: []string{"203.0.113.9"}, wantIP: "127.0.0.1"},
		{name: "off mode, untrusted peer: headers ignored", mode: "off", trusted: loopback, remote: "198.51.100.7:4000",
			xff: []string{"203.0.113.9"}, xfp: []string{"https"}, wantIP: "198.51.100.7"},
		{name: "off mode, no trusted proxies", mode: "off", remote: "127.0.0.1:4000",
			xff: []string{"203.0.113.9"}, xfp: []string{"https"}, wantIP: "127.0.0.1"},
		{name: "trusted peer: the client from XFF", mode: "off", trusted: loopback, remote: "127.0.0.1:4000",
			xff: []string{"203.0.113.9"}, xfp: []string{"https"}, wantIP: "203.0.113.9", secure: true},
		{name: "spoofed left entry is skipped", mode: "off", trusted: loopback, remote: "127.0.0.1:4000",
			xff: []string{"198.51.100.1, 203.0.113.9"}, wantIP: "203.0.113.9"},
		{name: "a chain of trusted proxies", mode: "off", trusted: []string{"127.0.0.0/8", "10.0.0.0/8"},
			remote: "127.0.0.1:4000", xff: []string{"198.51.100.1, 203.0.113.9, 10.0.0.2"}, wantIP: "203.0.113.9"},
		{name: "several header lines", mode: "off", trusted: []string{"127.0.0.0/8", "10.0.0.0/8"},
			remote: "127.0.0.1:4000", xff: []string{"198.51.100.1", "203.0.113.9, 10.0.0.2"}, wantIP: "203.0.113.9"},
		{name: "all entries trusted: the left-most", mode: "off", trusted: []string{"127.0.0.0/8", "10.0.0.0/8"},
			remote: "127.0.0.1:4000", xff: []string{"10.0.0.3, 127.0.0.2"}, wantIP: "10.0.0.3"},
		{name: "trusted peer without XFF", mode: "off", trusted: loopback, remote: "127.0.0.1:4000", wantIP: "127.0.0.1"},
		{name: "unparsable right-most entry: the peer", mode: "off", trusted: loopback, remote: "127.0.0.1:4000",
			xff: []string{"203.0.113.9, unknown"}, wantIP: "127.0.0.1"},
		{name: "unparsable entry behind the client", mode: "off", trusted: loopback, remote: "127.0.0.1:4000",
			xff: []string{"bogus, 203.0.113.9"}, wantIP: "203.0.113.9"},
		{name: "unparsable entry behind trusted hops", mode: "off", trusted: []string{"127.0.0.0/8", "10.0.0.0/8"},
			remote: "127.0.0.1:4000", xff: []string{"203.0.113.9, bogus, 10.0.0.2"}, wantIP: "10.0.0.2"},
		{name: "empty entries skipped", mode: "off", trusted: loopback, remote: "127.0.0.1:4000",
			xff: []string{" 203.0.113.9 , , "}, wantIP: "203.0.113.9"},
		{name: "IPv6 peer and client", mode: "off", trusted: loopback, remote: "[::1]:5000",
			xff: []string{"2001:db8::5"}, wantIP: "2001:db8::5"},
		{name: "entries with ports and brackets", mode: "off", trusted: loopback, remote: "[::1]:5000",
			xff: []string{"[2001:db8::5]:443, 127.0.0.1:80"}, wantIP: "2001:db8::5"},
		{name: "bracketed IPv6 without port", mode: "off", trusted: loopback, remote: "[::1]:5000",
			xff: []string{"[2001:db8::6]"}, wantIP: "2001:db8::6"},
		{name: "IPv4 with port", mode: "off", trusted: loopback, remote: "[::1]:5000",
			xff: []string{"203.0.113.9:1234"}, wantIP: "203.0.113.9"},
		{name: "IPv4-mapped peer is unmapped and trusted", mode: "off", trusted: loopback,
			remote: "[::ffff:127.0.0.1]:5000", xff: []string{"203.0.113.9"}, wantIP: "203.0.113.9"},
		{name: "IPv4-mapped peer without XFF", mode: "off", trusted: loopback, remote: "[::ffff:127.0.0.1]:5000",
			wantIP: "127.0.0.1"},
		{name: "IPv4-mapped entry is unmapped", mode: "off", trusted: loopback, remote: "127.0.0.1:4000",
			xff: []string{"::ffff:203.0.113.9"}, wantIP: "203.0.113.9"},
		{name: "zone dropped", mode: "auto", remote: "[fe80::1%en0]:5000", wantIP: "fe80::1"},
		{name: "XFP http from a trusted proxy", mode: "off", trusted: loopback, remote: "127.0.0.1:4000",
			xfp: []string{"http"}, wantIP: "127.0.0.1"},
		{name: "XFP: the nearest proxy's value counts", mode: "off", trusted: loopback, remote: "127.0.0.1:4000",
			xfp: []string{"https, http"}, wantIP: "127.0.0.1"},
		{name: "XFP over two lines", mode: "off", trusted: loopback, remote: "127.0.0.1:4000",
			xfp: []string{"http", "HTTPS"}, wantIP: "127.0.0.1", secure: true},
		{name: "dev is a secure context", mode: "off", dev: true, trusted: loopback, remote: "127.0.0.1:4000",
			wantIP: "127.0.0.1", secure: true},
		{name: "unix socket peer", mode: "off", trusted: loopback, remote: "@", xff: []string{"203.0.113.9"},
			wantIP: "invalid IP"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			site := domainSite()
			site.TLSMode, site.Dev = tt.mode, tt.dev
			f := newFixture(t, func(o *RouterOptions) {
				o.Site = site
				o.TrustedProxies = prefixes(t, tt.trusted...)
			})
			var gotIP netip.Addr
			var gotSecure bool
			f.rt.HandleFunc("GET /ip", func(_ http.ResponseWriter, r *http.Request) {
				gotIP, gotSecure = ClientIP(r), IsSecure(r)
			})
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+testHost+"/ip", nil)
			if tt.tls {
				req.TLS = &tls.ConnectionState{}
			}
			req.RemoteAddr = tt.remote
			for _, v := range tt.xff {
				req.Header.Add("X-Forwarded-For", v)
			}
			for _, v := range tt.xfp {
				req.Header.Add("X-Forwarded-Proto", v)
			}
			req.Header.Set("X-Forwarded-Host", "evil.example")
			req.Header.Set("Forwarded", "for=198.51.100.99;proto=https")
			if rec := f.serve(req); rec.Code != http.StatusOK {
				t.Fatalf("status %d", rec.Code)
			}
			if gotIP.String() != tt.wantIP || gotSecure != tt.secure {
				t.Fatalf("ClientIP %v, IsSecure %v; want %s, %v", gotIP, gotSecure, tt.wantIP, tt.secure)
			}
		})
	}
}

func TestClientIPOutsideRouter(t *testing.T) {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	req.RemoteAddr = "[::ffff:198.51.100.7]:4000"
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := ClientIP(req); got != netip.MustParseAddr("198.51.100.7") {
		t.Errorf("ClientIP = %v, want the unmapped peer", got)
	}
	if IsSecure(req) {
		t.Error("IsSecure without TLS")
	}
	req.TLS = &tls.ConnectionState{}
	if !IsSecure(req) {
		t.Error("IsSecure with TLS = false")
	}
	req.RemoteAddr = "pipe"
	if got := ClientIP(req); got.IsValid() {
		t.Errorf("ClientIP of a non-IP peer = %v, want the zero Addr", got)
	}
}

// FuzzClientFromXFF checks that any header yields either the peer or one of the header's entries, never a panic.
func FuzzClientFromXFF(f *testing.F) {
	for _, s := range []string{"", "203.0.113.9", "198.51.100.1, 203.0.113.9, 10.0.0.2", "[2001:db8::5]:443",
		"unknown", ",,,", "::ffff:1.2.3.4", "fe80::1%eth0", "1.2.3.4:99999", "[1.2.3.4]"} {
		f.Add(s)
	}
	trusted := prefixes(f, "10.0.0.0/8", "127.0.0.0/8", "::1/128")
	isTrusted := func(a netip.Addr) bool {
		for _, p := range trusted {
			if p.Contains(a) {
				return true
			}
		}
		return false
	}
	peer := netip.MustParseAddr("127.0.0.1")
	f.Fuzz(func(t *testing.T, xff string) {
		got := clientFromXFF([]string{xff}, peer, isTrusted)
		if !got.IsValid() || got.Is4In6() || got.Zone() != "" {
			t.Fatalf("clientFromXFF(%q) = %v: not a normalized address", xff, got)
		}
		if got == peer {
			return
		}
		for e := range strings.SplitSeq(xff, ",") {
			if a, ok := parseForwardedAddr(strings.TrimSpace(e)); ok && a == got {
				return
			}
		}
		t.Fatalf("clientFromXFF(%q) = %v: neither the peer nor an entry", xff, got)
	})
}
