package ops

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/ops/doctor"
	"github.com/MoonWX/isshoni/internal/version"
)

// The admin socket's API (04 §12.2). JSON is camelCase like every other API, timestamps have 03 §3.3's fixed form
// (api.WireTime), and an error is 03's envelope with two English fields for the CLI around it (AdminErrorResponse).
// The types below are the wire documents; AdminClient returns them and `--json` prints them as they are.

// AdminHealth is the document of GET /v1/health and GET /v1/ready:
//
//	/v1/health  200 {"status":"ok","version":"0.3.0"}       503 {"status":"shutting_down"}
//	/v1/ready   200 {"status":"ready","checks":{…}}         503 {"status":"not_ready","checks":{…}}, or shutting_down
//
// Status is one of StatusOK, StatusShuttingDown, StatusReady and StatusNotReady. Unlike the public /readyz, the
// socket always shows the checks: whoever reaches it is the operator.
type AdminHealth struct {
	Status  string            `json:"status"`
	Version string            `json:"version,omitempty"`
	Checks  map[string]string `json:"checks,omitempty"`
}

// AdminSetupURLRequest is the body of POST /v1/setup-url. With WaitReadyS > 0 the server first waits up to that
// many seconds for readiness (`isshoni setup-url --wait`); it mints the link either way.
type AdminSetupURLRequest struct {
	WaitReadyS int `json:"waitReadyS"` // 0 to AdminMaxWaitReady, in seconds
}

// AdminMaxWaitReady bounds AdminSetupURLRequest.WaitReadyS, so that a typo can't park a request for days. The CLI
// checks `setup-url --wait` against it.
const AdminMaxWaitReady = time.Hour

// AdminSetupURL is the answer of POST /v1/setup-url, and what `isshoni setup-url --json` prints (04 §3.3).
type AdminSetupURL struct {
	URL       string       `json:"url"` // <origin>/setup#<token>
	ExpiresAt api.WireTime `json:"expiresAt"`
	// TLSReady is false while the certificate is not there yet: the link works once it is.
	TLSReady bool `json:"tlsReady"`
}

// AdminLink is the answer of POST /v1/users/{name}/reset-link and POST /v1/invites: a one-time link.
type AdminLink struct {
	URL       string       `json:"url"`
	ExpiresAt api.WireTime `json:"expiresAt"`
}

// AdminUser is one account in AdminUsers.
type AdminUser struct {
	ID         string         `json:"id"`
	Username   string         `json:"username"`
	Role       api.Role       `json:"role"`
	Status     api.UserStatus `json:"status"`
	CreatedAt  api.WireTime   `json:"createdAt"`
	LastSeenAt api.WireTime   `json:"lastSeenAt,omitzero"` // absent for a user who never signed in
}

// AdminUsers is the answer of GET /v1/users, and what `isshoni admin users list --json` prints.
type AdminUsers struct {
	Users []AdminUser `json:"users"`
}

// AdminRoleRequest is the body of POST /v1/users/{name}/role.
type AdminRoleRequest struct {
	Role api.Role `json:"role"` // admin | user
}

// AdminInviteRequest is the body of POST /v1/invites. Both fields are optional: a field left out is 0 for 03, so
// the server's invite setting applies (03 §9).
type AdminInviteRequest struct {
	Uses int    `json:"uses,omitempty"` // 1 to AdminInviteMaxUses
	TTL  string `json:"ttl,omitempty"`  // a Go duration in whole hours, "1h" to "720h"
}

// Limits of AdminInviteRequest (04 §12.2, 03 §7.9).
const (
	AdminInviteMaxUses = 1000
	AdminInviteMaxTTL  = 720 * time.Hour
)

// AdminLogLevelRequest is the body of POST /v1/log-level. For is a Go duration ("30m"); "" means
// DefaultLogLevelFor. After it the configured level is back.
type AdminLogLevelRequest struct {
	Level string `json:"level"` // one of LogLevelNames
	For   string `json:"for,omitempty"`
}

// AdminRotateRequest is the body of POST /v1/rotate-secrets (04 §5.3); the CLI always asks for all four.
type AdminRotateRequest struct {
	Keys  []string `json:"keys"` // "session", "invite", "resume" (config.KeyName)
	VAPID bool     `json:"vapid"`
}

// AdminRotateResult is its 202 answer. The server restarts right after it.
type AdminRotateResult struct {
	Rotated    []string `json:"rotated"`
	Restarting bool     `json:"restarting"`
}

// AdminRestoreResult is the 202 answer of POST /v1/restore (04 §12.4). The server restarts right after it.
type AdminRestoreResult struct {
	Restarting       bool   `json:"restarting"`
	PreRestoreBackup string `json:"preRestoreBackup"` // relative to data_dir: backups/pre-restore-<ts>.tar.gz
}

// AdminDoctorRequest is the body of POST /v1/doctor (04 §13.1). Both fields are optional.
type AdminDoctorRequest struct {
	// Only names the checks to run (doctor.CheckIDs); none means all of them.
	Only []string `json:"only,omitempty"`
	// Bandwidth is the session the bandwidth check estimates: the bandwidth flags of `isshoni doctor` (04 §13.4).
	// Without it the estimate is for doctor.DefaultBandwidthInput.
	Bandwidth *api.BandwidthInput `json:"bandwidth,omitempty"`
}

// AdminErrorResponse is the socket's error body (04 §12.1): 03's error with a message and a fix in English, which
// the CLI prints. The two never go into api.Error itself. The status is always api.StatusOf(Error.Code), and the
// CLI branches on the code, never on the status.
type AdminErrorResponse struct {
	Error   api.Error `json:"error"`
	Message string    `json:"message,omitempty"`
	Fix     string    `json:"fix,omitempty"`
}

// adminMaxJSONBytes bounds a JSON request body on the socket.
const adminMaxJSONBytes = 64 << 10

// readyPollInterval is how often a waiting setup-url looks at the readiness checks.
const readyPollInterval = 250 * time.Millisecond

// socketError is an error with the English text of its answer. Handlers return one where the generic text of the
// code (describe) is not enough.
type socketError struct {
	api     api.Error
	message string
	fix     string
}

func (e *socketError) Error() string { return "ops: admin socket: " + e.api.Code + ": " + e.message }

func badRequest(format string, args ...any) error {
	return &socketError{api: api.Error{Code: api.CodeBadRequest}, message: fmt.Sprintf(format, args...)}
}

// unavailable is the answer of an endpoint whose part this server does not have: 500 internal with the reason.
func unavailable(format string, args ...any) error {
	return &socketError{api: api.Error{Code: api.CodeInternal}, message: fmt.Sprintf(format, args...)}
}

// adminRoute is one row of 04 §12.2.
type adminRoute struct {
	method string
	path   string // a ServeMux pattern without the method
	handle func(w http.ResponseWriter, r *http.Request) error
	// always marks the routes that keep answering while the server shuts down (health, ready, status); the others
	// then get 503 server_shutdown, like every API call (04 §6.4 step 2).
	always bool
}

func (s *AdminServer) routes() http.Handler {
	routes := []adminRoute{
		{http.MethodGet, "/v1/health", s.handleHealth, true},
		{http.MethodGet, "/v1/ready", s.handleReady, true},
		{http.MethodGet, "/v1/status", s.handleStatus, true},
		{http.MethodPost, "/v1/setup-url", s.handleSetupURL, false},
		{http.MethodGet, "/v1/users", s.handleUsers, false},
		{http.MethodPost, "/v1/users/{name}/reset-link", s.handleResetLink, false},
		{http.MethodPost, "/v1/users/{name}/role", s.handleRole, false},
		{http.MethodPost, "/v1/users/{name}/disable", s.handleDisabled(true), false},
		{http.MethodPost, "/v1/users/{name}/enable", s.handleDisabled(false), false},
		{http.MethodPost, "/v1/invites", s.handleInvite, false},
		{http.MethodPost, "/v1/log-level", s.handleLogLevel, false},
		{http.MethodPost, "/v1/doctor", s.handleDoctor, false},
		// A later slice of the M1 plan (README S65) fills these in.
		{http.MethodGet, "/v1/backup", notImplemented("backup"), false},
		{http.MethodPost, "/v1/restore", notImplemented("restore"), false},
		{http.MethodPost, "/v1/rotate-secrets", notImplemented("rotate-secrets"), false},
	}
	mux := http.NewServeMux()
	for _, rt := range routes {
		mux.HandleFunc(rt.path, s.serve(rt))
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.writeError(w, r, &socketError{
			api:     api.Error{Code: api.CodeNotFound},
			message: fmt.Sprintf("this server's admin socket has no %s %s", r.Method, r.URL.Path),
			fix:     "the running server and this isshoni binary may be different versions: restart the server after an upgrade",
		})
	})
	return mux
}

// serve wraps a route: the method check (a GET route also answers HEAD), the shutdown gate, the error answer, and
// the answer to a handler that panics.
func (s *AdminServer) serve(rt adminRoute) http.HandlerFunc {
	allow := rt.method
	if rt.method == http.MethodGet {
		allow = "GET, HEAD"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		defer s.recoverPanic(w, r)
		if r.Method != rt.method && (rt.method != http.MethodGet || r.Method != http.MethodHead) {
			w.Header().Set("Allow", allow)
			s.writeError(w, r, &socketError{
				api:     api.Error{Code: api.CodeMethodNotAllowed},
				message: fmt.Sprintf("%s takes %s, not %s", r.URL.Path, rt.method, r.Method),
			})
			return
		}
		if !rt.always {
			if live, _ := s.health.Live(); !live {
				s.writeError(w, r, api.NewError(api.CodeServerShutdown))
				return
			}
		}
		if err := rt.handle(w, r); err != nil {
			s.writeError(w, r, err)
		}
	}
}

// recoverPanic turns a panic of a handler (a bug) into a logged stack and a 500 internal answer, so that the
// operator's command gets a reference to quote instead of a connection that just closes. net/http would recover it
// too, but log it only at debug level, through the server's ErrorLog.
func (s *AdminServer) recoverPanic(w http.ResponseWriter, r *http.Request) {
	v := recover()
	if v == nil {
		return
	}
	if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
		panic(v) // net/http's own way to abort a response
	}
	ref := newRef()
	s.log.LogAttrs(r.Context(), slog.LevelError, "an admin socket request panicked",
		slog.String("route", r.Method+" "+r.Pattern), slog.String("ref", ref),
		slog.String("panic", fmt.Sprint(v)), slog.String("stack", string(debug.Stack())))
	// If the handler had written its header already, this one is dropped and the client sees a cut-off answer.
	writeAdminJSON(w, http.StatusInternalServerError, internalAnswer(ref))
}

// internalAnswer is the error document of a 500 that is a bug or an unexpected failure: nothing of the error, only
// the reference of its log line.
func internalAnswer(ref string) AdminErrorResponse {
	return AdminErrorResponse{
		Error:   api.Error{Code: api.CodeInternal, RequestID: ref},
		Message: "the server ran into an internal error (ref " + ref + ")",
		Fix:     "look for that ref in the server's log (journalctl -u isshoni, or docker compose logs isshoni)",
	}
}

func notImplemented(what string) func(http.ResponseWriter, *http.Request) error {
	return func(http.ResponseWriter, *http.Request) error {
		return unavailable("%s over the admin socket is not implemented in this build yet", what)
	}
}

func (s *AdminServer) handleHealth(w http.ResponseWriter, _ *http.Request) error {
	live, state := s.health.Live()
	if !live {
		writeAdminJSON(w, http.StatusServiceUnavailable, AdminHealth{Status: state})
		return nil
	}
	writeAdminJSON(w, http.StatusOK, AdminHealth{Status: state, Version: version.Version()})
	return nil
}

func (s *AdminServer) handleReady(w http.ResponseWriter, _ *http.Request) error {
	state, checks := s.health.readiness()
	status := http.StatusOK
	if state != StatusReady {
		status = http.StatusServiceUnavailable
	}
	writeAdminJSON(w, status, AdminHealth{Status: state, Checks: checks})
	return nil
}

func (s *AdminServer) handleStatus(w http.ResponseWriter, r *http.Request) error {
	if s.status == nil {
		writeAdminJSON(w, http.StatusOK, api.ServerStatus{
			Version:   version.Version(),
			StartedAt: s.started,
			UptimeS:   int64(time.Since(s.started) / time.Second),
		})
		return nil
	}
	st, err := s.status(r.Context())
	if err != nil {
		return err
	}
	writeAdminJSON(w, http.StatusOK, st)
	return nil
}

func (s *AdminServer) handleSetupURL(w http.ResponseWriter, r *http.Request) error {
	var in AdminSetupURLRequest
	if err := decodeAdminJSON(w, r, &in); err != nil {
		return err
	}
	wait := time.Duration(in.WaitReadyS) * time.Second
	if in.WaitReadyS < 0 || wait > AdminMaxWaitReady {
		return badRequest("waitReadyS %d: want 0 to %d", in.WaitReadyS, int(AdminMaxWaitReady/time.Second))
	}
	if s.accounts == nil {
		return unavailable("this server's admin socket has no accounts")
	}
	// Ask before waiting: once an admin exists there is nothing to wait for.
	ok, err := s.accounts.SetupAvailable(r.Context())
	if err != nil {
		return err
	}
	if !ok {
		return api.NewError(api.CodeSetupUnavailable)
	}
	if err := s.waitReady(r.Context(), wait); err != nil {
		return err
	}
	link, err := s.accounts.IssueSetupLink(r.Context())
	if err != nil {
		return err
	}
	_, checks := s.health.Ready()
	tls, has := checks["tls"]
	writeAdminJSON(w, http.StatusOK, AdminSetupURL{
		URL:       link.URL.Reveal(),
		ExpiresAt: api.WireTime(link.ExpiresAt),
		TLSReady:  !has || tls == checkOK,
	})
	return nil
}

// waitReady waits until the server is ready, for at most d. Running out of time is no error: the link is minted
// all the same, and its answer says whether the certificate is there. It returns an error only when the wait
// can't go on: the client is gone, or the server shuts down.
func (s *AdminServer) waitReady(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	deadline := time.NewTimer(d)
	defer deadline.Stop()
	tick := time.NewTicker(readyPollInterval)
	defer tick.Stop()
	for {
		if ready, _ := s.health.Ready(); ready {
			return nil
		}
		select {
		case <-deadline.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-s.stopping:
			return api.NewError(api.CodeServerShutdown)
		case <-tick.C:
			if live, _ := s.health.Live(); !live {
				return api.NewError(api.CodeServerShutdown)
			}
		}
	}
}

func (s *AdminServer) handleUsers(w http.ResponseWriter, r *http.Request) error {
	if s.accounts == nil {
		return unavailable("this server's admin socket has no accounts")
	}
	users, err := s.accounts.Users(r.Context())
	if err != nil {
		return err
	}
	if users == nil {
		users = []AdminUser{} // "users": [], never null
	}
	writeAdminJSON(w, http.StatusOK, AdminUsers{Users: users})
	return nil
}

func (s *AdminServer) handleResetLink(w http.ResponseWriter, r *http.Request) error {
	if s.accounts == nil {
		return unavailable("this server's admin socket has no accounts")
	}
	link, err := s.accounts.IssueResetLink(r.Context(), r.PathValue("name"))
	if err != nil {
		return err
	}
	writeAdminJSON(w, http.StatusOK, AdminLink{URL: link.URL.Reveal(), ExpiresAt: api.WireTime(link.ExpiresAt)})
	return nil
}

func (s *AdminServer) handleRole(w http.ResponseWriter, r *http.Request) error {
	var in AdminRoleRequest
	if err := decodeAdminJSON(w, r, &in); err != nil {
		return err
	}
	if in.Role != api.RoleAdmin && in.Role != api.RoleUser {
		return &api.Error{Code: api.CodeValidationFailed, Fields: map[string]string{"role": api.FieldInvalid}}
	}
	if s.accounts == nil {
		return unavailable("this server's admin socket has no accounts")
	}
	if err := s.accounts.SetRole(r.Context(), r.PathValue("name"), in.Role); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *AdminServer) handleDisabled(disabled bool) func(http.ResponseWriter, *http.Request) error {
	return func(w http.ResponseWriter, r *http.Request) error {
		if s.accounts == nil {
			return unavailable("this server's admin socket has no accounts")
		}
		if err := s.accounts.SetDisabled(r.Context(), r.PathValue("name"), disabled); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
}

func (s *AdminServer) handleInvite(w http.ResponseWriter, r *http.Request) error {
	var in AdminInviteRequest
	if err := decodeAdminJSON(w, r, &in); err != nil {
		return err
	}
	// The socket's own fields are checked here, so that the answer names them; 03 checks its ranges again.
	fields := map[string]string{}
	if in.Uses < 0 || in.Uses > AdminInviteMaxUses {
		fields["uses"] = api.FieldOutOfRange
	}
	ttlHours := 0
	if in.TTL != "" {
		d, err := time.ParseDuration(in.TTL)
		switch {
		case err != nil:
			fields["ttl"] = api.FieldInvalid
		case d < time.Hour || d > AdminInviteMaxTTL || d%time.Hour != 0:
			fields["ttl"] = api.FieldOutOfRange
		default:
			ttlHours = int(d / time.Hour)
		}
	}
	if len(fields) > 0 {
		return &api.Error{Code: api.CodeValidationFailed, Fields: fields}
	}
	if s.accounts == nil {
		return unavailable("this server's admin socket has no accounts")
	}
	link, err := s.accounts.CreateInvite(r.Context(), in.Uses, ttlHours)
	if err != nil {
		return err
	}
	writeAdminJSON(w, http.StatusOK, AdminLink{URL: link.URL.Reveal(), ExpiresAt: api.WireTime(link.ExpiresAt)})
	return nil
}

func (s *AdminServer) handleLogLevel(w http.ResponseWriter, r *http.Request) error {
	var in AdminLogLevelRequest
	if err := decodeAdminJSON(w, r, &in); err != nil {
		return err
	}
	level, ok := ParseLogLevel(in.Level)
	if !ok {
		return badRequest("log level %q: want %s", in.Level, strings.Join(LogLevelNames, ", "))
	}
	d := DefaultLogLevelFor
	if in.For != "" {
		var err error
		if d, err = time.ParseDuration(in.For); err != nil || d <= 0 {
			return badRequest("for %q: want a positive duration such as 30m", in.For)
		}
	}
	if s.logLevel == nil {
		return unavailable("this server's log level can't be changed while it runs")
	}
	// Not audited (04 §12.1): one info line with the peer's uid instead. It is written on the side of the switch
	// that shows info lines: before it when the level goes up to warn or error, after it otherwise.
	line := func() {
		s.log.LogAttrs(r.Context(), slog.LevelInfo, "log level changed through the admin socket",
			slog.String("log_level", in.Level), slog.String("for", d.String()), peerUIDAttr(r.Context()))
	}
	before := s.log.Enabled(r.Context(), slog.LevelInfo)
	if before {
		line()
	}
	s.logLevel.Override(level, d)
	if !before {
		line()
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// handleDoctor runs doctor inside the server (04 §13.1) and answers with its report. The request waits while
// another run is under way.
func (s *AdminServer) handleDoctor(w http.ResponseWriter, r *http.Request) error {
	var in AdminDoctorRequest
	if err := decodeAdminJSON(w, r, &in); err != nil {
		return err
	}
	// A request that asks for something doctor does not have is refused, not answered with a report that leaves
	// it out without a word.
	known := doctor.CheckIDs()
	for _, id := range in.Only {
		if !slices.Contains(known, id) {
			return badRequest("doctor has no check %q; its checks are %s", id, strings.Join(known, ", "))
		}
	}
	if in.Bandwidth != nil {
		if field, _, ok := doctor.BandwidthInputProblem(*in.Bandwidth); !ok {
			return badRequest("bandwidth.%s is not a value the bandwidth estimate takes", field)
		}
	}
	if s.doctor == nil {
		return unavailable("this server's admin socket has no doctor")
	}
	// A run, or the wait for one, must not hold up a shutdown: it ends when the server stops, like the wait of
	// setup-url.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		select {
		case <-s.stopping:
			cancel()
		case <-ctx.Done():
		}
	}()
	rep, err := s.doctor.RunFor(ctx, in)
	if err != nil {
		select {
		case <-s.stopping:
			return api.NewError(api.CodeServerShutdown)
		default:
			return err
		}
	}
	writeAdminJSON(w, http.StatusOK, rep)
	return nil
}

// decodeAdminJSON reads a JSON request body into dst. An empty body leaves dst as it is, because every field of
// the socket's requests is optional (so `curl --unix-socket … -X POST http://isshoni/v1/setup-url` works). Unknown
// fields are ignored, like everywhere in the API (03 §12.1).
func decodeAdminJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, adminMaxJSONBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return api.NewError(api.CodePayloadTooLarge)
		}
		return badRequest("the request body could not be read")
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(dst); err != nil {
		return badRequest("the request body is not the JSON this endpoint takes")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return badRequest("the request body has data after its JSON value")
	}
	return nil
}

// writeAdminJSON writes v with the given status. A value that can't be encoded is a programming error.
func writeAdminJSON(w http.ResponseWriter, status int, v any) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false) // nothing here is HTML, and the CLI prints these documents: keep "&" in a URL readable
	if err := enc.Encode(v); err != nil {
		panic(fmt.Errorf("ops: admin socket: encoding %T: %w", v, err))
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(b.Len()))
	w.WriteHeader(status)
	_, _ = w.Write(b.Bytes()) // a failed write means the client is gone
}

// writeError answers err: an *api.Error (03's service errors pass through unchanged) with the English text of its
// code, a socketError with its own text, and anything else as 500 internal with a reference that is also logged.
func (s *AdminServer) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var out AdminErrorResponse
	var se *socketError
	var ae *api.Error
	switch {
	case errors.As(err, &se):
		out = AdminErrorResponse{Error: se.api, Message: se.message, Fix: se.fix}
	case errors.As(err, &ae) && ae != nil:
		out.Error = *ae
		out.Message, out.Fix = describe(ae, r)
	default:
		ref := newRef()
		level := slog.LevelError
		if r.Context().Err() != nil { // the client hung up while the request ran
			level = slog.LevelDebug
		}
		s.log.LogAttrs(r.Context(), level, "an admin socket request failed",
			slog.String("route", r.Method+" "+r.Pattern), slog.String("ref", ref), logx.Err(err))
		out = internalAnswer(ref)
	}
	if out.Error.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(out.Error.RetryAfter))
	}
	writeAdminJSON(w, api.StatusOf(out.Error.Code), out)
}

// describe returns the English message and fix of a 03 error for the CLI. An empty message makes the CLI print the
// code.
func describe(e *api.Error, r *http.Request) (message, fix string) {
	name := r.PathValue("name")
	switch e.Code {
	case api.CodeSetupUnavailable:
		return "setup is already done: an admin account exists, so there is no setup link",
			"use `isshoni admin users reset-password <name>` to get back into an admin account"
	case api.CodeUserNotFound:
		return userNotFoundText(name)
	case api.CodeLastAdmin:
		return fmt.Sprintf("%q is the last active admin, and the server must keep one", name),
			"make another user an admin first: `isshoni admin users set-role <name> admin`"
	case api.CodeRegistrationClosed:
		return "registration is closed, so an invite link would not work",
			"set the registration mode to invite or open first (Admin → Settings, or registration.mode in the config)"
	case api.CodeLimitReached:
		return fmt.Sprintf("the server has reached its limit (%v)", e.Params[api.ParamLimit]),
			"revoke what is no longer needed in the admin pages, then try again"
	case api.CodeValidationFailed:
		parts := make([]string, 0, len(e.Fields))
		for field, code := range e.Fields {
			parts = append(parts, field+" ("+code+")")
		}
		slices.Sort(parts)
		return "the server refused the values: " + strings.Join(parts, ", "), ""
	case api.CodePayloadTooLarge:
		return "the request is too large", ""
	case api.CodeServerShutdown:
		return "the server is shutting down", "try again once it is back"
	case api.CodeServerBusy, api.CodeRateLimited:
		return "the server is busy", "try again in a moment"
	default:
		return "", ""
	}
}

// userNotFoundText is the message and fix of user_not_found: the server's for a name it does not know, and the
// client's for a name that it can't send (AdminClient.userCall).
func userNotFoundText(name string) (message, fix string) {
	return fmt.Sprintf("there is no user named %q", name), "`isshoni admin users list` shows the accounts"
}

// newRef returns the 8-character reference that ties an internal error's answer to its log line.
func newRef() string {
	var b [4]byte
	_, _ = rand.Read(b[:]) // never fails (crypto/rand)
	return hex.EncodeToString(b[:])
}
