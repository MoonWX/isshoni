package ops

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/version"
)

// Why no server answered on the admin socket (04 §12.1). An *AdminUnreachableError wraps one of them; test with
// errors.Is.
var (
	// ErrAdminNotRunning: there is no socket at the path (ENOENT, also for a missing /run/isshoni), or nobody
	// listens on it (ECONNREFUSED: a stale socket file), or the server went away between the connect and its first
	// answer (it is stopping, or it failed right after it bound the socket) for a caller it would have served. The
	// CLI exits 4; doctor runs its checks offline (04 §13.1).
	ErrAdminNotRunning = errors.New("ops: no server on the admin socket")
	// ErrAdminPermission: the caller may not use the socket: EACCES or EPERM on the way to it (the directory is
	// 0750, the socket 0600), or the server closed the connection before its first answer, which is what it does
	// to a peer that is neither root nor its own user. Only such a peer gets this error for a hang-up: root and
	// the socket's owner are never refused (callerMayBeRefused). The CLI exits 4 with the sudo message; doctor runs
	// nothing.
	ErrAdminPermission = errors.New("ops: permission denied on the admin socket")
)

// AdminUnreachableError is the error of an AdminClient call that reached no server.
type AdminUnreachableError struct {
	Path string // the socket path
	Kind error  // ErrAdminNotRunning or ErrAdminPermission
	Err  error  // the dial or I/O error behind it
}

// Error is the first half of the CLI's message; the CLI adds the sudo hint for ErrAdminPermission (04 §12.1).
func (e *AdminUnreachableError) Error() string {
	if errors.Is(e.Kind, ErrAdminPermission) {
		return "permission denied on " + e.Path
	}
	return "isshoni is not running (no server on " + e.Path + ")"
}

// Unwrap returns the kind and the underlying error.
func (e *AdminUnreachableError) Unwrap() []error { return []error{e.Kind, e.Err} }

// AdminError is an error answer of the server: 03's error with the socket's English text (AdminErrorResponse).
// It unwraps to the *api.Error, so api.IsCode(err, api.CodeSetupUnavailable) works on it. For a username that
// can't be sent, the client writes the server's user_not_found answer itself (userCall).
type AdminError struct {
	Status  int       // the HTTP status; callers branch on API.Code, never on this
	API     api.Error // Code is "" when the answer was not the socket's error document
	Message string    // English, for the operator; may be ""
	Fix     string    // English; may be ""
}

func (e *AdminError) Error() string {
	switch {
	case e.Message != "":
		return e.Message
	case e.API.Code != "":
		return "the server answered " + e.API.Code
	default:
		return "the server answered HTTP " + strconv.Itoa(e.Status)
	}
}

// Unwrap returns the server's *api.Error.
func (e *AdminError) Unwrap() error { return &e.API }

// Content types of a restore's body (04 §12.2).
const (
	AdminContentArchive = "application/gzip"        // a backup archive (.tar.gz, 04 §12.3)
	AdminContentSQLite  = "application/vnd.sqlite3" // a database file, e.g. backups/pre-<schema>-<ts>.db
)

// Limits on what the client reads of an answer.
const (
	adminMaxAnswerBytes = 32 << 20 // a JSON answer; the user list of a large server stays far below it
	adminMaxErrorBytes  = 64 << 10 // an error document
)

// AdminClient is the client of the admin socket: every client command of the CLI and doctor use it. Each call
// opens a connection of its own, so an error always says what happened to that call. Its context bounds the call;
// there is no other timeout.
//
// Errors: an *AdminUnreachableError when no server answers (ErrAdminNotRunning or ErrAdminPermission), an
// *AdminError when the server answers with an error, and anything else for a failed exchange (a timeout, an answer
// that is not the socket's).
type AdminClient struct {
	path string
	// stranger makes every hang-up before the first answer a refusal (AssumeStranger).
	stranger bool
}

// DialAdmin returns a client for the admin socket at path (listen.admin_socket, or the CLI's --socket). It
// connects with the first call, not here.
func DialAdmin(path string) *AdminClient { return &AdminClient{path: path} }

// Path returns the socket path.
func (c *AdminClient) Path() string { return c.path }

// AssumeStranger makes c read a server that hangs up before its first answer as a refusal of its credentials
// (ErrAdminPermission), whoever the caller is. By itself c does that only for a caller the server can refuse:
// one who is neither root nor the owner of the socket file. It is the client's side of AdminListenOptions.Allow,
// for tests: a test can't connect as another user, so it gives the listener a rule that refuses its own uid, and
// tells the client that this can happen. It returns c.
func (c *AdminClient) AssumeStranger() *AdminClient {
	c.stranger = true
	return c
}

// Health asks GET /v1/health: liveness. A server that is shutting down answers too (StatusShuttingDown), so check
// the Status, not only the error.
func (c *AdminClient) Health(ctx context.Context) (AdminHealth, error) {
	return c.health(ctx, "/v1/health")
}

// Ready asks GET /v1/ready: readiness, with the state of every check. The Status is StatusReady, StatusNotReady or
// StatusShuttingDown.
func (c *AdminClient) Ready(ctx context.Context) (AdminHealth, error) {
	return c.health(ctx, "/v1/ready")
}

// Status asks GET /v1/status.
func (c *AdminClient) Status(ctx context.Context) (api.ServerStatus, error) {
	var out api.ServerStatus
	return out, c.call(ctx, http.MethodGet, "/v1/status", nil, &out)
}

// SetupURL asks POST /v1/setup-url for a new setup link; the earlier ones stop working. With waitReady > 0 the
// server first waits up to that long (rounded up to a second) for readiness; it refuses more than
// AdminMaxWaitReady. Once an admin exists the error has the code setup_unavailable.
func (c *AdminClient) SetupURL(ctx context.Context, waitReady time.Duration) (AdminSetupURL, error) {
	in := AdminSetupURLRequest{WaitReadyS: int((max(waitReady, 0) + time.Second - 1) / time.Second)}
	var out AdminSetupURL
	return out, c.call(ctx, http.MethodPost, "/v1/setup-url", in, &out)
}

// Users asks GET /v1/users.
func (c *AdminClient) Users(ctx context.Context) (AdminUsers, error) {
	var out AdminUsers
	return out, c.call(ctx, http.MethodGet, "/v1/users", nil, &out)
}

// ResetLink asks POST /v1/users/{name}/reset-link for a one-time password-reset link. Issuing it clears the user's
// password and signs the user out everywhere. Errors: user_not_found.
func (c *AdminClient) ResetLink(ctx context.Context, username string) (AdminLink, error) {
	var out AdminLink
	return out, c.userCall(ctx, username, "reset-link", nil, &out)
}

// SetRole asks POST /v1/users/{name}/role. Errors: user_not_found, last_admin.
func (c *AdminClient) SetRole(ctx context.Context, username string, role api.Role) error {
	return c.userCall(ctx, username, "role", AdminRoleRequest{Role: role}, nil)
}

// SetDisabled asks POST /v1/users/{name}/disable or /enable. Errors: user_not_found, last_admin.
func (c *AdminClient) SetDisabled(ctx context.Context, username string, disabled bool) error {
	verb := "enable"
	if disabled {
		verb = "disable"
	}
	return c.userCall(ctx, username, verb, nil, nil)
}

// CreateInvite asks POST /v1/invites for an invite link. uses and ttl are 0 for "the server's invite setting";
// they are then not sent. ttl is in whole hours. Errors: registration_closed, validation_failed.
func (c *AdminClient) CreateInvite(ctx context.Context, uses int, ttl time.Duration) (AdminLink, error) {
	in := AdminInviteRequest{Uses: uses}
	if ttl != 0 {
		in.TTL = strconv.FormatInt(int64(ttl/time.Hour), 10) + "h"
	}
	var out AdminLink
	return out, c.call(ctx, http.MethodPost, "/v1/invites", in, &out)
}

// SetLogLevel asks POST /v1/log-level: level (one of LogLevelNames) for d, then back to the configured level.
func (c *AdminClient) SetLogLevel(ctx context.Context, level string, d time.Duration) error {
	return c.call(ctx, http.MethodPost, "/v1/log-level", AdminLogLevelRequest{Level: level, For: d.String()}, nil)
}

// Doctor asks POST /v1/doctor: the checks run inside the server's process (04 §13.1). req names the checks, or
// none for all of them, and the session of the bandwidth estimate. The answer comes when the run is done, which
// can take a quarter of a minute; a run that is under way is waited for.
func (c *AdminClient) Doctor(ctx context.Context, req AdminDoctorRequest) (api.DoctorReport, error) {
	var out api.DoctorReport
	return out, c.call(ctx, http.MethodPost, "/v1/doctor", req, &out)
}

// RotateSecrets asks POST /v1/rotate-secrets; the server restarts after its answer (04 §5.3).
func (c *AdminClient) RotateSecrets(ctx context.Context, in AdminRotateRequest) (AdminRotateResult, error) {
	var out AdminRotateResult
	return out, c.call(ctx, http.MethodPost, "/v1/rotate-secrets", in, &out)
}

// AdminBackup is the stream of GET /v1/backup: a .tar.gz archive (04 §12.3). The caller reads it to the end and
// closes it.
type AdminBackup struct {
	io.ReadCloser
	// Filename is the name the server proposes, e.g. isshoni-backup-0.3.0-20260929T101500Z.tar.gz; "" when it
	// proposed none. It is a base name, never a path.
	Filename string
}

// Backup asks GET /v1/backup. certs says whether the archive holds the TLS certificates and keys.
func (c *AdminClient) Backup(ctx context.Context, certs bool) (*AdminBackup, error) {
	path := "/v1/backup?certs=0"
	if certs {
		path = "/v1/backup?certs=1"
	}
	res, err := c.exchange(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		defer func() { _ = res.Body.Close() }()
		return nil, c.answerError(res.StatusCode, readLimited(res.Body, adminMaxErrorBytes))
	}
	b := &AdminBackup{ReadCloser: res.Body}
	if _, params, err := mime.ParseMediaType(res.Header.Get("Content-Disposition")); err == nil && params["filename"] != "" {
		b.Filename = filepath.Base(filepath.FromSlash(params["filename"]))
	}
	return b, nil
}

// Restore sends body to POST /v1/restore: a backup archive (AdminContentArchive) or a database file
// (AdminContentSQLite). The server validates it, answers, and then restarts (04 §12.4). Errors: backup_invalid,
// backup_newer, restore_in_progress, insufficient_storage.
func (c *AdminClient) Restore(ctx context.Context, body io.Reader, contentType string) (AdminRestoreResult, error) {
	var out AdminRestoreResult
	res, err := c.exchange(ctx, http.MethodPost, "/v1/restore", contentType, body)
	if err != nil {
		return out, err
	}
	defer func() { _ = res.Body.Close() }()
	return out, c.decode(res, &out)
}

// userCall is call for POST /v1/users/{name}/verb. An empty name, "." and ".." can't be the path element: the
// server's router cleans such a path and answers with a redirect, which would read as "that is not isshoni's
// socket". None of the three is a username (03 §7.1), so the client gives the server's answer for a name it does
// not know, without asking.
func (c *AdminClient) userCall(ctx context.Context, username, verb string, in, out any) error {
	if username == "" || username == "." || username == ".." {
		message, fix := userNotFoundText(username)
		return &AdminError{
			Status: api.StatusOf(api.CodeUserNotFound), API: api.Error{Code: api.CodeUserNotFound},
			Message: message, Fix: fix,
		}
	}
	return c.call(ctx, http.MethodPost, "/v1/users/"+url.PathEscape(username)+"/"+verb, in, out)
}

// health reads a health document, which the server sends with 200 and with 503 alike.
func (c *AdminClient) health(ctx context.Context, path string) (AdminHealth, error) {
	res, err := c.exchange(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return AdminHealth{}, err
	}
	defer func() { _ = res.Body.Close() }()
	data := readLimited(res.Body, adminMaxErrorBytes)
	var h AdminHealth
	if res.StatusCode == http.StatusOK || res.StatusCode == http.StatusServiceUnavailable {
		if err := json.Unmarshal(data, &h); err == nil && h.Status != "" {
			return h, nil
		}
	}
	return AdminHealth{}, c.answerError(res.StatusCode, data)
}

// call sends one JSON request (in may be nil) and decodes a 2xx answer into out (nil for an answer without a
// body).
func (c *AdminClient) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	contentType := ""
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("ops: admin socket request: %w", err)
		}
		body, contentType = bytes.NewReader(data), "application/json"
	}
	res, err := c.exchange(ctx, method, path, contentType, body)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	return c.decode(res, out)
}

// decode turns the answer into out, or into an *AdminError when its status is not 2xx.
func (c *AdminClient) decode(res *http.Response, out any) error {
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return c.answerError(res.StatusCode, readLimited(res.Body, adminMaxErrorBytes))
	}
	if out == nil || res.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, adminMaxAnswerBytes)).Decode(out); err != nil {
		return fmt.Errorf("ops: admin socket %s: unexpected answer: %w", c.path, err)
	}
	return nil
}

// answerError builds the *AdminError of an error answer. An answer that is not the socket's error document comes
// from something else that listens on the path.
func (c *AdminClient) answerError(status int, data []byte) error {
	var doc AdminErrorResponse
	if err := json.Unmarshal(data, &doc); err != nil || doc.Error.Code == "" {
		return &AdminError{
			Status:  status,
			Message: fmt.Sprintf("unexpected answer (HTTP %d) from %s", status, c.path),
			Fix:     "check that the path is isshoni's admin socket (listen.admin_socket)",
		}
	}
	return &AdminError{Status: status, API: doc.Error, Message: doc.Message, Fix: doc.Fix}
}

// readLimited reads at most n bytes of r; what it could read is enough for an error message.
func readLimited(r io.Reader, n int64) []byte {
	data, _ := io.ReadAll(io.LimitReader(r, n))
	return data
}

// exchange sends one request on a connection of its own and returns the response as soon as its header is in.
// Closing the response body closes the connection. A plain exchange on the connection, rather than net/http's
// Transport, keeps the two failures of 04 §12.1 apart: the dial that fails, and the server that hangs up before
// it answers.
func (c *AdminClient) exchange(ctx context.Context, method, path, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://isshoni"+path, body)
	if err != nil {
		return nil, fmt.Errorf("ops: admin socket request: %w", err)
	}
	req.Close = true
	req.Header.Set("User-Agent", version.UserAgent())
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.path)
	if err != nil {
		return nil, c.dialError(ctx, err)
	}
	// ctx bounds the whole exchange, the reading of the body included: when it ends, every read and write on the
	// connection fails at once.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	w := &connWriter{conn: conn}
	writeErr := req.Write(w)
	if writeErr != nil && !w.failed {
		// Not the connection: the request's body could not be read. The server would wait for the rest forever.
		stop()
		_ = conn.Close()
		return nil, fmt.Errorf("ops: admin socket %s: sending the request: %w", c.path, writeErr)
	}
	// A write that fails on the connection means that the server hung up while the client still wrote. Its answer
	// is read all the same: a server that refuses a large body answers and closes without reading the rest.
	res, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		stop()
		_ = conn.Close()
		// The server hung up before its first answer when the read finds the connection closed. After a failed
		// write any error of the connection counts: the error number of a read on a socket that broke under a
		// write varies. An answer that is not HTTP is something else: another program listens there.
		var netErr *net.OpError
		hungUp := isHangup(err) || (writeErr != nil && errors.As(err, &netErr))
		return nil, c.exchangeError(ctx, err, hungUp)
	}
	res.Body = &connBody{ReadCloser: res.Body, conn: conn, stop: stop}
	return res, nil
}

// connWriter writes to the connection and remembers whether a write failed there, which tells a broken connection
// from a request body that could not be read.
type connWriter struct {
	conn   net.Conn
	failed bool
}

func (w *connWriter) Write(p []byte) (int, error) {
	n, err := w.conn.Write(p)
	if err != nil {
		w.failed = true
	}
	return n, err
}

// isHangup reports whether a read error says that the other side closed the connection. Which one it is depends
// on the platform and on who was faster: a clean end, or one of the platform's error numbers (isConnReset).
func isHangup(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || isConnReset(err)
}

// connBody closes the connection with the response body.
type connBody struct {
	io.ReadCloser
	conn net.Conn
	stop func() bool
}

func (b *connBody) Close() error {
	// The connection goes first: closing the body alone would first read what is left of it.
	err := b.conn.Close()
	_ = b.ReadCloser.Close()
	b.stop()
	return err
}

// dialError classifies a failed dial (04 §12.1).
func (c *AdminClient) dialError(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist), isConnRefused(err),
		errors.Is(err, syscall.ENOTDIR), errors.Is(err, syscall.ENOTSOCK):
		return &AdminUnreachableError{Path: c.path, Kind: ErrAdminNotRunning, Err: err}
	case errors.Is(err, fs.ErrPermission):
		return &AdminUnreachableError{Path: c.path, Kind: ErrAdminPermission, Err: err}
	case ctx.Err() != nil:
		return fmt.Errorf("ops: connecting to the admin socket %s: %w", c.path, ctx.Err())
	default:
		return fmt.Errorf("ops: connecting to the admin socket %s: %w", c.path, err)
	}
}

// exchangeError classifies an exchange that failed after the dial. A server that hangs up before its first answer
// (hungUp) has refused the peer's credentials (04 §12.1), if it can have: it never refuses root or its own user.
// For those two the hang-up has another cause: the server stopped while the connection waited for it (Shutdown
// closes the connections that have not sent a request), or it failed between binding the socket and serving it.
// That is "not running", and a command that waits for the server keeps waiting.
func (c *AdminClient) exchangeError(ctx context.Context, err error, hungUp bool) error {
	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("ops: no answer from the server on the admin socket %s: %w", c.path, ctx.Err())
	case hungUp && (c.stranger || callerMayBeRefused(c.path)):
		return &AdminUnreachableError{Path: c.path, Kind: ErrAdminPermission, Err: err}
	case hungUp:
		return &AdminUnreachableError{Path: c.path, Kind: ErrAdminNotRunning, Err: err}
	default:
		return fmt.Errorf("ops: admin socket %s: %w", c.path, err)
	}
}
