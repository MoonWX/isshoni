package deviceauth

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// Errors of Client (02 §11.2).
var (
	// ErrNetwork wraps a transport failure: fmt.Errorf("%w: %w", ErrNetwork, cause), so errors.As still reaches a
	// *tls.CertificateVerificationError or an x509 error in the cause (03-desktop-app tells tls_untrusted from other
	// network errors that way).
	ErrNetwork = errors.New("deviceauth: network error")
	// ErrBadResponse is wrapped by every answer the client can't use: a body that doesn't parse or is larger than
	// 64 KiB, a non-2xx answer without the REST error envelope (a redirect included: none is followed), a token of
	// the wrong shape, and verification URIs that fail StartLink's check.
	ErrBadResponse = errors.New("deviceauth: unexpected response")
)

// Errors of Manager and Servers (02 §11.3). 01-client-core's core decides "unlinked" only from an Auth error whose
// chain has a RelinkRequired() bool method returning true, "incompatible" from one with UpdateRequired() bool, and
// treats every other error as transient (01 §11.2); these values give it that answer.

// ErrNotLinked: no credential exists, or this call's refresh got invalid_grant (the credential is deleted before the
// call returns). AccessToken returns an error wrapping it (errors.Is(err, ErrNotLinked)), whose chain has
// RelinkRequired() == true.
var ErrNotLinked error = notLinked{}

type notLinked struct{}

func (notLinked) Error() string { return "deviceauth: not linked" }

// RelinkRequired reports that the app must be linked again.
func (notLinked) RelinkRequired() bool { return true }

// ErrIncompatible: the manager is in StatusIncompatible (compat says update_app or update_server). The signal client
// never reaches hello while its token callback fails, so it never sees client_outdated itself; UpdateRequired says
// it instead.
var ErrIncompatible error = incompatible{}

type incompatible struct{}

func (incompatible) Error() string { return "deviceauth: incompatible server" }

// UpdateRequired reports that the app or the server must be updated before they can work together.
func (incompatible) UpdateRequired() bool { return true }

// ErrOffline is transient (it has neither method). AccessToken wraps the network error it got:
// fmt.Errorf("%w: %w", ErrOffline, cause).
var ErrOffline = errors.New("deviceauth: server unreachable")

var (
	// ErrRevokeNotConfirmed: Unlink couldn't reach the server, so the device may still be listed on the user's
	// Devices page; the app is unlinked locally anyway.
	ErrRevokeNotConfirmed = errors.New("deviceauth: revoke not confirmed")
	// ErrTooManyServers: Servers already holds 20 servers.
	ErrTooManyServers = errors.New("deviceauth: too many servers")
)

// Unexported errors: neither says "relink" or "update", so the core treats them as transient.
var (
	// errClosed is returned by the methods of a closed Manager or Servers.
	errClosed = errors.New("deviceauth: closed")
	// errUnknownServer is returned by Servers for an origin it doesn't list.
	errUnknownServer = errors.New("deviceauth: no such server")
	// errStale ends a refresh whose credential was replaced or removed while it ran: it commits nothing, and its
	// waiters look at the manager again.
	errStale = errors.New("deviceauth: the credential changed during the refresh")
	// errCancelled ends a refresh that Unlink, ForgetAll, a new link or Close stopped.
	errCancelled = errors.New("deviceauth: refresh cancelled")
)

// responseError is an answer the client can't use. It wraps ErrBadResponse and keeps the HTTP status, so a retry
// can tell a proxy's 502 page from a 2xx with a broken body.
type responseError struct {
	status int
	why    string
}

func (e *responseError) Error() string {
	return fmt.Sprintf("%s: HTTP %d: %s", ErrBadResponse.Error(), e.status, e.why)
}

func (e *responseError) Unwrap() error { return ErrBadResponse }

// badResponse returns a responseError; status 0 means the answer was a 2xx that failed a check.
func badResponse(status int, why string) error {
	if status == 0 {
		status = http.StatusOK
	}
	return &responseError{status: status, why: why}
}

// apiError returns err's *api.Error, or nil.
func apiError(err error) *api.Error {
	var e *api.Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}

// retryable reports whether a refresh or a poll that failed with err is tried again after a delay (02 §11.4): a
// network error, an answer the client couldn't use (the server may have committed it, so the retry with the same
// exchange id takes its grace path), a 5xx or server_busy, and a 429.
func retryable(err error) bool {
	if errors.Is(err, ErrNetwork) || errors.Is(err, ErrBadResponse) {
		return true
	}
	e := apiError(err)
	return e != nil && (e.Code == api.CodeRateLimited || api.StatusOf(e.Code) >= http.StatusInternalServerError)
}

// grantReused reports whether an invalid_grant says params.reason "reused" (02 §6.2).
func grantReused(e *api.Error) bool {
	r, _ := e.Params[api.ParamReason].(string)
	return r == api.InvalidGrantReused
}
