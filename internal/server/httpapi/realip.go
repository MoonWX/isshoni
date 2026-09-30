package httpapi

import (
	"net/http"
	"net/netip"
	"strings"
)

// realIP is step 7: it resolves the client address and whether the request came over HTTPS, once, for ClientIP and
// IsSecure (04 §8.5).
func (rt *Router) realIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := stateOf(r)
		st.ip, st.secure = rt.resolveClient(r)
		st.ipSet = true
		next.ServeHTTP(w, r)
	})
}

// resolveClient applies 04 §8.5. X-Forwarded-For and X-Forwarded-Proto count only in off mode and only when the TCP
// peer is a trusted proxy; the client is then the right-most X-Forwarded-For entry that is not itself a trusted
// proxy. X-Forwarded-Host and Forwarded are always ignored (links use Site.Origin).
func (rt *Router) resolveClient(r *http.Request) (netip.Addr, bool) {
	peer := peerAddr(r.RemoteAddr)
	secure := r.TLS != nil || rt.opts.Site.Dev
	if rt.opts.Site.TLSMode != tlsOff || !rt.trusted(peer) {
		return peer, secure
	}
	client := clientFromXFF(r.Header.Values("X-Forwarded-For"), peer, rt.trusted)
	if proto := lastListValue(r.Header.Values("X-Forwarded-Proto")); strings.EqualFold(proto, "https") {
		secure = true
	}
	return client, secure
}

func (rt *Router) trusted(a netip.Addr) bool {
	if !a.IsValid() {
		return false
	}
	for _, p := range rt.opts.TrustedProxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// clientFromXFF walks the X-Forwarded-For entries (all header lines, in order) from the right. The first entry that
// is not a trusted proxy is the client. When every entry is trusted, the left-most one is. An entry that doesn't
// parse ends the walk: nothing left of it can be trusted, so the last trusted hop is the client. peer is the trusted
// proxy that sent the request.
func clientFromXFF(values []string, peer netip.Addr, trusted func(netip.Addr) bool) netip.Addr {
	client := peer
	for i := len(values) - 1; i >= 0; i-- {
		entries := strings.Split(values[i], ",")
		for j := len(entries) - 1; j >= 0; j-- {
			e := strings.TrimSpace(entries[j])
			if e == "" {
				continue
			}
			a, ok := parseForwardedAddr(e)
			if !ok {
				return client
			}
			client = a
			if !trusted(a) {
				return a
			}
		}
	}
	return client
}

// parseForwardedAddr parses one X-Forwarded-For entry: an IP address, optionally in brackets or with a port (some
// proxies add one). The result is unmapped and without a zone.
func parseForwardedAddr(s string) (netip.Addr, bool) {
	if a, err := netip.ParseAddr(s); err == nil {
		return normalizeAddr(a), true
	}
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return normalizeAddr(ap.Addr()), true
	}
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		if a, err := netip.ParseAddr(s[1 : len(s)-1]); err == nil && a.Is6() {
			return normalizeAddr(a), true
		}
	}
	return netip.Addr{}, false
}

// lastListValue returns the right-most element of a comma-separated header (over all its lines): the value that the
// nearest proxy set.
func lastListValue(values []string) string {
	for i := len(values) - 1; i >= 0; i-- {
		entries := strings.Split(values[i], ",")
		for j := len(entries) - 1; j >= 0; j-- {
			if e := strings.TrimSpace(entries[j]); e != "" {
				return e
			}
		}
	}
	return ""
}

// peerAddr parses http.Request.RemoteAddr ("203.0.113.7:51234", "[2001:db8::1]:51234"). An address that doesn't
// parse (a unix socket, a test pipe) gives the zero Addr.
func peerAddr(remote string) netip.Addr {
	if ap, err := netip.ParseAddrPort(remote); err == nil {
		return normalizeAddr(ap.Addr())
	}
	if a, err := netip.ParseAddr(remote); err == nil {
		return normalizeAddr(a)
	}
	return netip.Addr{}
}

// normalizeAddr unmaps IPv4-mapped IPv6 addresses and drops the zone, as 03 stores IPs (03 §3.3).
func normalizeAddr(a netip.Addr) netip.Addr { return a.Unmap().WithZone("") }

// ClientIP returns the client's address (04 §8.5): the TCP peer, or in off mode behind a trusted proxy the right-most
// X-Forwarded-For entry that is not a trusted proxy. IPv4-mapped addresses are unmapped. For a request that did not
// pass through the router it is the TCP peer; the zero Addr when RemoteAddr is not an IP address.
func ClientIP(r *http.Request) netip.Addr {
	if st := stateOf(r); st != nil && st.ipSet {
		return st.ip
	}
	return peerAddr(r.RemoteAddr)
}

// IsSecure reports whether the client reached the server over HTTPS: the connection is TLS, a trusted proxy said
// X-Forwarded-Proto: https (off mode), or the site is a dev site on localhost (a secure context for browsers).
func IsSecure(r *http.Request) bool {
	if st := stateOf(r); st != nil && st.ipSet {
		return st.secure
	}
	return r.TLS != nil
}
