package httpapi

import (
	"net/http"
	"net/netip"
	"strings"
)

// Header values of 04 §9.6.
const (
	// cspOther is the policy of every response that is not HTML and not /sw.js: JSON, scripts, styles, images, errors.
	cspOther = "default-src 'none'; frame-ancestors 'none'"
	// permissionsPolicy is sent with HTML documents only.
	permissionsPolicy = "display-capture=(self), fullscreen=(self), picture-in-picture=(self), autoplay=(self), " +
		"camera=(), microphone=(), geolocation=(), usb=(), payment=(), browsing-topics=()"
	// hstsValue has no includeSubDomains and no preload: other subdomains of the admin's domain are not ours.
	hstsValue = "max-age=31536000"

	cacheNoStore   = "no-store"
	cacheNoCache   = "no-cache" // revalidate with the ETag
	cacheImmutable = "public, max-age=31536000, immutable"
	cacheIcons     = "public, max-age=86400"
)

// secHeaders holds the security header values that depend on the site, computed once by NewRouter.
type secHeaders struct {
	htmlCSP string // policy for HTML documents and /sw.js
	hsts    bool   // send Strict-Transport-Security on TLS responses
}

func newSecHeaders(site Site, hsts bool) secHeaders {
	return secHeaders{
		htmlCSP: htmlCSP(site),
		// HSTS only for a domain: RFC 6797 hosts ignore it for IP literals, and tls.hsts can turn it off.
		hsts: hsts && site.Hostname != "" && !isIPLiteral(site.Hostname),
	}
}

// htmlCSP returns the policy for HTML and /sw.js. connect-src names the WebSocket origin explicitly because older
// Safari versions don't match wss: with 'self'. It is ws:// when the site origin is plain http (dev); a reverse
// proxy that terminates TLS has an https public_url, so it keeps wss://.
func htmlCSP(site Site) string {
	connect := "'self'"
	if site.Host != "" {
		scheme := "wss://"
		if strings.HasPrefix(strings.ToLower(site.Origin), "http://") {
			scheme = "ws://"
		}
		connect += " " + scheme + site.Host
	}
	return "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; " +
		"media-src 'self' blob:; connect-src " + connect + "; worker-src 'self'; manifest-src 'self'; " +
		"font-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
}

// setBaseHeaders sets the headers that every response carries (04 §9.6 rows "all"), the non-HTML policy and
// Cache-Control: no-store. The SPA handler upgrades HTML responses with setHTML and sets its own Cache-Control; 03's
// chain keeps no-store under /api/v1/. Responses written before the security-headers step (gate, Host check,
// recover) call it themselves, so they carry the same set.
func setBaseHeaders(h http.Header) {
	h.Set("Content-Security-Policy", cspOther)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("X-Robots-Tag", "noindex")
	h.Set("Cache-Control", cacheNoStore)
}

// setHTML switches a response to the HTML policy. document is true for HTML pages, which also get COOP and the
// Permissions-Policy; /sw.js gets only the policy (its fetches are governed by its script response's CSP).
func (s secHeaders) setHTML(h http.Header, document bool) {
	h.Set("Content-Security-Policy", s.htmlCSP)
	if document {
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Permissions-Policy", permissionsPolicy)
	}
}

// securityHeaders is step 8 of the chain (04 §9.3, §9.6).
func (rt *Router) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		setBaseHeaders(h)
		if rt.sec.hsts && r.TLS != nil {
			h.Set("Strict-Transport-Security", hstsValue)
		}
		next.ServeHTTP(w, r)
	})
}

// isIPLiteral reports whether host (without port, brackets allowed) is an IP address.
func isIPLiteral(host string) bool {
	_, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"))
	return err == nil
}
