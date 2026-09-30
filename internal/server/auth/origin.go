package auth

import (
	"encoding/json"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// Error codes of the CSRF guard, from 03 §12.2.
const (
	codeCSRFFailed           = "csrf_failed"
	codeUnsupportedMediaType = "unsupported_media_type"
)

// CSRFError is a request refused by the CSRF guard. Code is the api error code of 03 §12.2 and Status its HTTP
// status: "csrf_failed" (403) for a cross-origin request, "unsupported_media_type" (415) for an unsafe request
// without a JSON Content-Type.
type CSRFError struct {
	Status int
	Code   string
}

func (e *CSRFError) Error() string { return "auth: " + e.Code }

// CSRFGuard is the REST CSRF defence of 03 §7.5 for /api/v1 (the WebSocket's Origin check is 01's). It applies to
// unsafe methods: every method except GET, HEAD and OPTIONS, which never change state.
//
//  1. Cross-origin check: Go's http.CrossOriginProtection. With Sec-Fetch-Site, only same-origin and none pass;
//     without it, the Origin host must match Host; a request with neither header is not from a browser and
//     passes; an Origin on the trusted list (Origins.Public) passes. Otherwise 403 csrf_failed.
//  2. Content type: the request must send exactly one Content-Type, application/json (a charset, if given, must be
//     utf-8), even with an empty body. Otherwise 415 unsupported_media_type. HTML forms can't send this type, and a
//     cross-origin fetch with it needs a CORS preflight, which the server never grants.
//
// The /api/v1 chain runs it after the no-store step and before authentication (03 §12.1). (*Service).CSRF (README
// S30) wraps with a guard built from Options.Origins().Public.
type CSRFGuard struct {
	cop *http.CrossOriginProtection
}

// NewCSRFGuard returns a guard that trusts the given public origins ("scheme://host[:port]"; Origins.Public). Each
// origin is normalized the way browsers send it: lower-case scheme and host, no default port, canonical IPv6. An
// origin with a path, query, fragment or user info, a scheme other than http and https, or a non-ASCII host (use
// its punycode form) is an error.
func NewCSRFGuard(public []string) (*CSRFGuard, error) {
	cop := http.NewCrossOriginProtection()
	for _, o := range public {
		n, err := normalizeOrigin(o)
		if err != nil {
			return nil, err
		}
		if err := cop.AddTrustedOrigin(n); err != nil {
			return nil, fmt.Errorf("auth: trusted origin %q: %w", o, err)
		}
	}
	return &CSRFGuard{cop: cop}, nil
}

// Check returns nil when r may go on, or a *CSRFError. It writes nothing, so a caller can report the error with its
// own error writer.
func (g *CSRFGuard) Check(r *http.Request) error {
	if ce := g.check(r); ce != nil {
		return ce
	}
	return nil
}

func (g *CSRFGuard) check(r *http.Request) *CSRFError {
	if isSafeMethod(r.Method) {
		return nil
	}
	if err := g.cop.Check(r); err != nil {
		return &CSRFError{Status: http.StatusForbidden, Code: codeCSRFFailed}
	}
	if !isJSONContentType(r.Header.Values("Content-Type")) {
		return &CSRFError{Status: http.StatusUnsupportedMediaType, Code: codeUnsupportedMediaType}
	}
	return nil
}

// Handler returns next behind Check. A refused request gets the error envelope of 03 §12.2,
// {"error":{"code":"csrf_failed"}} or {"error":{"code":"unsupported_media_type"}}, with its status.
func (g *CSRFGuard) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ce := g.check(r); ce != nil {
			writeErrorEnvelope(w, ce.Status, ce.Code)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isSafeMethod matches http.CrossOriginProtection's safe methods.
func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// isJSONContentType reports whether the Content-Type header values are exactly one application/json, with an
// optional utf-8 charset. Other parameters are ignored.
func isJSONContentType(values []string) bool {
	if len(values) != 1 {
		return false
	}
	mediaType, params, err := mime.ParseMediaType(values[0])
	if err != nil || mediaType != "application/json" {
		return false
	}
	if cs, ok := params["charset"]; ok && !strings.EqualFold(cs, "utf-8") {
		return false
	}
	return true
}

// errorEnvelope is 03 §12.2's REST error shape, reduced to the code (the only field the guard sets).
type errorEnvelope struct {
	Error struct {
		Code string `json:"code"`
	} `json:"error"`
}

// writeErrorEnvelope writes {"error":{"code":…}} with status. httpapi's WriteError writes the same shape for
// *api.Error values; the guard has its own copy because auth doesn't import httpapi (04 §2).
func writeErrorEnvelope(w http.ResponseWriter, status int, code string) {
	var env errorEnvelope
	env.Error.Code = code
	body, err := json.Marshal(env)
	if err != nil { // a struct of one string always marshals
		body = []byte(`{"error":{"code":"internal"}}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// normalizeOrigin returns o in the form a browser sends in the Origin header.
func normalizeOrigin(o string) (string, error) {
	bad := func(why string) (string, error) { return "", fmt.Errorf("auth: trusted origin %q: %s", o, why) }
	u, err := url.Parse(o)
	if err != nil {
		return bad("not a URL")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return bad("the scheme must be http or https")
	}
	if u.Opaque != "" || u.User != nil || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || u.Host == "" {
		return bad(`want "scheme://host[:port]" with no path, query, fragment or user info`)
	}
	host := strings.ToLower(u.Hostname())
	for i := range len(host) {
		if host[i] >= 0x80 {
			return bad("the host must be ASCII (use its punycode form)")
		}
	}
	if host == "" {
		return bad("empty host")
	}
	if a, err := netip.ParseAddr(host); err == nil {
		if a.Zone() != "" {
			return bad("an IPv6 zone is not allowed")
		}
		host = a.String()
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return bad("bad port")
		}
		port = strconv.Itoa(n)
		if (scheme == "https" && n == 443) || (scheme == "http" && n == 80) {
			port = ""
		}
	}
	switch {
	case port != "":
		return scheme + "://" + net.JoinHostPort(host, port), nil
	case strings.Contains(host, ":"):
		return scheme + "://[" + host + "]", nil
	default:
		return scheme + "://" + host, nil
	}
}
