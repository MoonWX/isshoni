package httpapi

import (
	"net/http"
	"slices"
	"strings"
)

// CORS for the desktop app's bundled SPA (M2 02 §6.4, step 0 of the /api/v1 chain). Every Wails app on a PC has the
// same origin, so the origin is no identity: a request from it is let in only with a bearer token, never with a
// cookie, and the answers never carry Access-Control-Allow-Credentials. CORS gives such a page nothing it couldn't do
// with net/http; what it protects is the cookie of the browser, which these answers never let a page use.

// The CORS answers of M2 02 §6.4 and §7.2. PUT is in the list for PUT /push/preferences.
const (
	corsAllowMethods  = "GET, POST, PUT, PATCH, DELETE"
	corsAllowHeaders  = "Authorization, Content-Type"
	corsExposeHeaders = "Retry-After, WWW-Authenticate, X-Request-Id"
	corsMaxAge        = "600" // seconds a webview may cache a preflight (M2 02 §15)
)

// corsMethods are the methods of corsAllowMethods, which a preflight may ask for (compared as sent: browsers send
// these in upper case).
var corsMethods = []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}

// corsRequestHeaders are the request headers a preflight may ask for, in lower case (compared without regard to
// case).
var corsRequestHeaders = []string{"authorization", "content-type"}

// originSet returns the set of origins; nil for none.
func originSet(origins []string) map[string]struct{} {
	if len(origins) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(origins))
	for _, o := range origins {
		set[o] = struct{}{}
	}
	return set
}

// cors is step 0 of the chain. For a request whose one Origin header is one of Deps.DesktopOrigins (exactly as the
// list spells it):
//   - a preflight (OPTIONS with Access-Control-Request-Method) is answered here and goes no further: 204, with
//     Access-Control-Allow-Origin, -Allow-Methods, -Allow-Headers and -Max-Age (600 s) when the method is one of
//     GET, POST, PUT, PATCH and DELETE and Access-Control-Request-Headers lists only authorization and content-type
//     (any case, any order, or none), and without them otherwise, which makes the webview refuse the request;
//   - any other request loses its Cookie header, so that it never uses a cookie the webview might hold, and its
//     answer gets Access-Control-Allow-Origin and Access-Control-Expose-Headers (Retry-After, WWW-Authenticate,
//     X-Request-Id), whatever answers it later in the chain: 401, 413, 429 and every other error included;
//   - every answer carries Vary: Origin, and none Access-Control-Allow-Credentials.
//
// A request from any other origin, or with no or several Origin headers, passes untouched; its preflight reaches
// the route lookup and gets M1's 405. Answers of 04's global chain before the API (421, the shutdown 503) carry no
// CORS headers: the SPA sees a network error and retries.
func (a *API) cors(next http.Handler) http.Handler {
	if len(a.desktop) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin, ok := a.desktopOrigin(r)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Add("Vary", "Origin")
		if r.Method == http.MethodOptions && len(r.Header.Values("Access-Control-Request-Method")) > 0 {
			if preflightAllowed(r.Header) {
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Access-Control-Allow-Methods", corsAllowMethods)
				h.Set("Access-Control-Allow-Headers", corsAllowHeaders)
				h.Set("Access-Control-Max-Age", corsMaxAge)
			}
			h.Set("Cache-Control", cacheNoStore)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Expose-Headers", corsExposeHeaders)
		next.ServeHTTP(w, withoutCookies(r))
	})
}

// desktopOrigin returns the request's Origin when it is exactly one header whose value is a desktop origin.
func (a *API) desktopOrigin(r *http.Request) (string, bool) {
	vals := r.Header.Values("Origin")
	if len(vals) != 1 {
		return "", false
	}
	if _, ok := a.desktop[vals[0]]; !ok {
		return "", false
	}
	return vals[0], true
}

// preflightAllowed reports whether a preflight asks for one of corsMethods and for no request header but
// corsRequestHeaders. Access-Control-Request-Headers is a comma-separated list, possibly over several header lines;
// empty elements are skipped, and a preflight without the header asks for none.
func preflightAllowed(h http.Header) bool {
	methods := h.Values("Access-Control-Request-Method")
	if len(methods) != 1 || !slices.Contains(corsMethods, methods[0]) {
		return false
	}
	for _, line := range h.Values("Access-Control-Request-Headers") {
		for name := range strings.SplitSeq(line, ",") {
			name = strings.ToLower(strings.Trim(name, " \t"))
			if name != "" && !slices.Contains(corsRequestHeaders, name) {
				return false
			}
		}
	}
	return true
}
