package tlsmgr

import (
	"net/http"
	"strings"

	"github.com/MoonWX/isshoni/internal/server/config"
)

// acmeChallengePrefix is where a CA fetches http-01 challenge responses (RFC 8555 §8.3).
const acmeChallengePrefix = "/.well-known/acme-challenge/"

// waitingPage is what port 80 shows before the first certificate. A redirect to an HTTPS port that can't complete
// a handshake yet would look like a failed install. The page reloads itself.
const waitingPage = `<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="30">
<title>isshoni</title>
<p>isshoni is getting its certificate. Refresh in a minute.</p>
`

// HTTPHandler returns the handler of the listen.http server (04 §8.3):
//
//  1. auto, ip: a request under /.well-known/acme-challenge/ goes to certmagic, which answers the http-01
//     challenges it has open; any other request there is a 404.
//  2. off: every request goes to app. This is the server itself; a reverse proxy connects here.
//  3. auto, ip, while there is no valid certificate: 503 with Retry-After: 30 and a small page that says isshoni
//     is getting its certificate.
//  4. Otherwise: 308 to the site's origin with the request's path and query. The target host is the configured
//     one, never the request's Host header, so the redirect can't be pointed elsewhere.
//
// app is used in off mode only and may be nil in the others.
func (m *Manager) HTTPHandler(app http.Handler) http.Handler {
	if m.opts.Mode == config.TLSOff {
		if app == nil {
			return http.NotFoundHandler()
		}
		return app
	}
	return http.HandlerFunc(m.servePlain)
}

// servePlain serves a plain-HTTP request in the modes that have a certificate.
func (m *Manager) servePlain(w http.ResponseWriter, r *http.Request) {
	if m.usesACME() {
		if strings.HasPrefix(r.URL.Path, acmeChallengePrefix) {
			if st := m.acme.Load(); st != nil && st.issuer.HandleHTTPChallenge(w, r) {
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "no such ACME challenge", http.StatusNotFound)
			return
		}
		if ok, _ := m.Ready(); !ok {
			writeWaiting(w)
			return
		}
	}
	origin := m.opts.Site.Origin
	if origin == "" {
		// The site's address is not known (no public IP address was found), so there is nowhere to send people.
		writeWaiting(w)
		return
	}
	// The origin is fixed, so whatever the path holds ("//host", "/\host") stays a path on our own site.
	path := r.URL.EscapedPath()
	if !strings.HasPrefix(path, "/") {
		path = "/" // "OPTIONS *", or a request line with an authority
	}
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	h := w.Header()
	h.Set("Location", origin+path)
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Robots-Tag", "noindex")
	w.WriteHeader(http.StatusPermanentRedirect)
	_, _ = w.Write([]byte("This server speaks HTTPS: " + origin + "\n"))
}

// writeWaiting answers 503 with the waiting page.
func writeWaiting(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Retry-After", "30")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", "default-src 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Robots-Tag", "noindex")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(waitingPage))
}
