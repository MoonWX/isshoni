// Package servers is the desktop app's side of adding a server (docs/m2/03-desktop-app.md §8.1, "03" below): the
// address field's normalization (Normalize), the derived local id of a server (LocalID) and the probe of a server
// before it is added (Probe), on 02's deviceauth.Client and compat. The server list itself, servers.json and the
// credentials, is 02's deviceauth.Servers.
package servers

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/MoonWX/isshoni/internal/client/compat"
	"github.com/MoonWX/isshoni/internal/client/deeplink"
	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// Codes are the probe's error codes, which the bridge passes to the SPA as they are (03 §6.4).
type Code string

const (
	CodeInvalidAddress     Code = "invalid_address"     // not an address Normalize accepts
	CodeServerUnreachable  Code = "server_unreachable"  // DNS, TCP, connect or overall timeout
	CodeTLSUntrusted       Code = "tls_untrusted"       // the certificate isn't trusted, has expired or names another host
	CodeNotIsshoni         Code = "not_isshoni"         // a status other than 200, or a body that isn't /info
	CodeServerNotSetUp     Code = "server_not_set_up"   // setupRequired
	CodeIncompatibleServer Code = "incompatible_server" // compat says update_app or update_server (Error.Compat)
)

// Error is Normalize's and Probe's error.
type Error struct {
	Code   Code
	Compat *compat.Check // CodeIncompatibleServer: the verdict, reason, server version and floor
	Err    error         // the cause, for logs; it never holds the address's path, query or fragment
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("servers: %s: %v", e.Code, e.Err)
	}
	return "servers: " + string(e.Code)
}

func (e *Error) Unwrap() error { return e.Err }

// CodeOf returns err's Code, or "" when err isn't an *Error.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// Address is a normalized server address.
type Address struct {
	// Origin is "https://host[:port]" (http only for a loopback host in dev and e2e builds).
	Origin string
	// Invite is set when the typed address was a server's invite or setup link (its path is /invite or /setup): Origin
	// plus the typed path, query and fragment, which the add step offers to open in the browser ("This is an invite
	// link. Open it in your browser first to create your account"); the fragment never reaches the server.
	Invite string
}

// Normalize reads what the user typed into the address field or passed with --server (03 §8.1): a host
// ("watch.example.com"), an origin, an IP literal, or any URL of the server ("https://watch.example.com/r/lounge").
// The path, query and fragment are dropped, never sent anywhere; an address whose path is /invite or /setup sets
// Invite. The rest goes through deeplink.NormalizeOrigin with allowInsecureLoopback (dev and e2e builds: http for
// localhost, 127.0.0.1 and [::1]). Errors are CodeInvalidAddress.
func Normalize(raw string, allowInsecureLoopback bool) (Address, error) {
	s := strings.TrimSpace(raw)
	if len(s) > deeplink.MaxBytes {
		return Address{}, &Error{Code: CodeInvalidAddress, Err: deeplink.ErrInvalidOrigin}
	}
	authStart := 0
	if i := strings.Index(s, "://"); i >= 0 {
		authStart = i + len("://")
	}
	head, path := s, ""
	if j := strings.IndexAny(s[authStart:], "/?#"); j >= 0 {
		head, path = s[:authStart+j], s[authStart+j:]
	}
	origin, err := deeplink.NormalizeOrigin(head, allowInsecureLoopback)
	if err != nil {
		return Address{}, &Error{Code: CodeInvalidAddress, Err: err}
	}
	a := Address{Origin: origin}
	if isInvitePath(path) {
		a.Invite = origin + path
	}
	return a, nil
}

// isInvitePath reports a path of /invite or /setup (with or without a trailing slash, any query or fragment).
func isInvitePath(p string) bool {
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	p = strings.TrimSuffix(p, "/")
	u, err := url.PathUnescape(p)
	if err != nil {
		return false
	}
	return u == "/invite" || u == "/setup"
}

// LocalID is a server's derived local id (03 §8.1): lowercase Crockford base32 of the first 5 bytes of
// SHA-256(origin), 8 characters. It prefixes the SPA's Web Storage keys and is bridge.js's server id; it is never
// stored. web/src/platform/desktop/testing/localIdVectors.ts holds the vectors both sides test.
func LocalID(origin string) string {
	const alphabet = "0123456789abcdefghjkmnpqrstvwxyz"
	sum := sha256.Sum256([]byte(origin))
	var bits uint64
	for _, b := range sum[:5] {
		bits = bits<<8 | uint64(b)
	}
	out := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		out[i] = alphabet[bits&31]
		bits >>= 5
	}
	return string(out)
}

// ServerProbe is what a probe found (03 §6.2 Auth.Probe).
type ServerProbe struct {
	Origin  string `json:"origin"`  // NormalizeOrigin(info.server.publicUrl): the origin the entry will have
	Name    string `json:"name"`    // the server's name
	Version string `json:"version"` // the server's version
}

// The probe's limits (03 §8.1, §20).
const (
	ConnectTimeout = 5 * time.Second
	ProbeTimeout   = 10 * time.Second
)

// ProbeOptions configures Probe.
type ProbeOptions struct {
	App                   deviceauth.AppInfo // the User-Agent's version and OS
	AllowInsecureLoopback bool               // dev and e2e builds
	// HTTP is the client to copy; nil means deviceauth.DefaultHTTPClient() with a 5 s connect timeout. Tests pass
	// one that trusts their server. Redirects are never followed, whatever it says.
	HTTP *http.Client
	// Compat is this build's protocol versions and version; the zero value means compat.Default() with App.Version.
	Compat compat.App
}

// Probe asks the server at address for its /info, without cookies, under a 10 s bound, and checks it in 03 §8.1's
// order: unreachable, TLS, not isshoni, not set up, incompatible; otherwise it returns the server's own origin,
// name and version. It never adds the server.
func Probe(ctx context.Context, address string, o ProbeOptions) (ServerProbe, error) {
	addr, err := Normalize(address, o.AllowInsecureLoopback)
	if err != nil {
		return ServerProbe{}, err
	}
	hc := o.HTTP
	if hc == nil {
		hc = probeHTTPClient()
		defer hc.CloseIdleConnections() // a probe's own client: nothing reuses its connections
	}
	c, err := deviceauth.NewClient(addr.Origin, o.App, deviceauth.ClientOptions{HTTP: hc,
		AllowInsecureLoopback: o.AllowInsecureLoopback})
	if err != nil {
		return ServerProbe{}, &Error{Code: CodeInvalidAddress, Err: err}
	}
	pctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	info, err := c.Info(pctx)
	if err != nil {
		if ctx.Err() != nil { // the caller's own ctx ended: not the server's fault
			return ServerProbe{}, ctx.Err()
		}
		return ServerProbe{}, classify(err)
	}
	origin, err := deeplink.NormalizeOrigin(info.Server.PublicURL, o.AllowInsecureLoopback)
	if err != nil {
		return ServerProbe{}, &Error{Code: CodeNotIsshoni, Err: errors.New("the answer is not an isshoni /info")}
	}
	if info.SetupRequired {
		return ServerProbe{}, &Error{Code: CodeServerNotSetUp}
	}
	app := o.Compat
	if app == (compat.App{}) {
		app = compat.Default()
		if o.App.Version != "" {
			app.Version = o.App.Version
		}
	}
	if check := compat.FromInfo(info, app); check.Verdict != compat.OK {
		return ServerProbe{}, &Error{Code: CodeIncompatibleServer, Compat: &check}
	}
	return ServerProbe{Origin: origin, Name: info.Server.Name, Version: info.Server.Version}, nil
}

// classify maps deviceauth.Client's errors to the probe's codes.
func classify(err error) error {
	var apiErr *api.Error
	switch {
	case isTLS(err):
		return &Error{Code: CodeTLSUntrusted, Err: err}
	case errors.As(err, &apiErr), errors.Is(err, deviceauth.ErrBadResponse):
		return &Error{Code: CodeNotIsshoni, Err: err}
	default: // deviceauth.ErrNetwork, and the probe's own deadline
		return &Error{Code: CodeServerUnreachable, Err: err}
	}
}

// isTLS reports a certificate the system doesn't trust: unknown authority, expired, another host's name.
func isTLS(err error) bool {
	var (
		verify    *tls.CertificateVerificationError
		authority x509.UnknownAuthorityError
		hostname  x509.HostnameError
		invalid   x509.CertificateInvalidError
	)
	return errors.As(err, &verify) || errors.As(err, &authority) || errors.As(err, &hostname) ||
		errors.As(err, &invalid)
}

// probeHTTPClient is 02's default client with a 5 s connect timeout (03 §8.1).
func probeHTTPClient() *http.Client {
	hc := deviceauth.DefaultHTTPClient()
	if t, ok := hc.Transport.(*http.Transport); ok {
		t = t.Clone()
		t.DialContext = (&net.Dialer{Timeout: ConnectTimeout, KeepAlive: 30 * time.Second}).DialContext
		hc.Transport = t
	}
	hc.Timeout = ProbeTimeout
	return hc
}
