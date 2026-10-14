package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"strconv"
	"strings"
	"time"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// The REST side of the tool (03 §12): the admin's session, the invite, the sign-ups and their removal, and the
// dashboard. Requests carry no Origin header, so the server takes them for what they are, a tool's (03 §7.5), and
// the session cookie lives in a cookie jar per account.

// Timeouts and bounds of the REST requests.
const (
	requestTimeout = 30 * time.Second
	// retryFor bounds the retries of a refused sign-up or login: a 429 (03 §7.3's buckets) or a 503 server_busy (the
	// server's password-hash budget) is tried again after its Retry-After for this long.
	retryFor    = 2 * time.Minute
	maxBodySize = 4 << 20
	// inviteSlack is how long the invite outlives the sign-ups it is for.
	inviteSlack = 30 * time.Minute
)

// httpError is an answer with another status than the one wanted: its status and the error envelope's code.
type httpError struct {
	method, path string
	status       int
	code         string
	retryAfter   time.Duration
}

func (e *httpError) Error() string {
	if e.code == "" {
		return fmt.Sprintf("%s %s: HTTP %d", e.method, e.path, e.status)
	}
	return fmt.Sprintf("%s %s: HTTP %d %s", e.method, e.path, e.status, e.code)
}

// restClient sends one account's requests: its cookie jar holds the session. Every account shares one transport.
type restClient struct {
	origin string
	hc     *http.Client
}

// newRESTClient returns a client with a cookie jar of its own on the shared transport.
func newRESTClient(origin string, transport http.RoundTripper) *restClient {
	jar, _ := cookiejar.New(nil) // never fails without options
	return &restClient{origin: origin, hc: &http.Client{Jar: jar, Transport: transport}}
}

// call sends a JSON request and decodes the answer into out (nil: drop it) when its status is want. Any other
// status is an *httpError; a transport failure is returned as it is.
func (c *restClient) call(ctx context.Context, method, path string, in, out any, want int) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("%s %s: %w", method, path, err)
		}
		body = bytes.NewReader(b)
	}
	// The URL is the operator's -server and a fixed path of 03's API.
	req, err := http.NewRequestWithContext(ctx, method, c.origin+path, body) //nolint:gosec // G704: see above
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	if method != http.MethodGet && method != http.MethodHead {
		// Every unsafe request is JSON, also one without a body (03 §7.5).
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	res, err := c.hc.Do(req) //nolint:gosec // G704: the operator's own server, as above
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxBodySize))
	if err != nil {
		return fmt.Errorf("%s %s: reading the answer: %w", method, path, err)
	}
	if res.StatusCode != want {
		he := &httpError{method: method, path: path, status: res.StatusCode}
		var env api.ErrorResponse
		if json.Unmarshal(data, &env) == nil {
			he.code = env.Error.Code
		}
		if s, err := strconv.Atoi(res.Header.Get("Retry-After")); err == nil && s > 0 {
			he.retryAfter = time.Duration(s) * time.Second
		}
		return he
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%s %s: the answer: %w", method, path, err)
		}
	}
	return nil
}

// callRetrying is call for the public account requests: a 429 or a 503 with a Retry-After is sent again after it,
// for up to retryFor. Neither did anything on the server: both are answered before any account is touched.
func (c *restClient) callRetrying(ctx context.Context, log *slog.Logger, method, path string, in, out any, want int) error {
	deadline := time.Now().Add(retryFor)
	for {
		err := c.call(ctx, method, path, in, out, want)
		var he *httpError
		if !errors.As(err, &he) || (he.status != http.StatusTooManyRequests && he.status != http.StatusServiceUnavailable) ||
			he.retryAfter <= 0 || time.Now().Add(he.retryAfter).After(deadline) {
			return err
		}
		log.Info("the server asks to wait", "path", path, "status", he.status, "code", he.code, "retry_after", he.retryAfter.String())
		if err := sleep(ctx, he.retryAfter); err != nil {
			return err
		}
	}
}

// account is one simulated friend's account.
type account struct {
	name     string
	password logx.Secret
	id       string
	rest     *restClient // its session cookie
}

// pacer spaces the public account requests (login, sign-ups) by -login-interval: 03's auth-ip bucket allows 20 at
// once and then one per 15 s per address, and every request of this tool comes from one address.
type pacer struct {
	every time.Duration
	last  time.Time
}

// wait returns when the next request may go, or with ctx's error.
func (p *pacer) wait(ctx context.Context) error {
	if !p.last.IsZero() && p.every > 0 {
		if err := sleep(ctx, time.Until(p.last.Add(p.every))); err != nil {
			return err
		}
	}
	p.last = time.Now()
	return nil
}

// login logs the admin in and checks the role. A server that doesn't answer is a setup error that says so.
func (lt *loadTest) login(ctx context.Context) error {
	if err := lt.pace.wait(ctx); err != nil {
		return err
	}
	var res api.UserResponse
	err := lt.admin.callRetrying(ctx, lt.log, http.MethodPost, "/api/v1/auth/login",
		api.LoginRequest{Username: lt.cfg.admin, Password: lt.cfg.password.Reveal()}, &res, http.StatusOK)
	var he *httpError
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.As(err, &he):
		return setupErrorf("logging in as %s: %w", lt.cfg.admin, err)
	case err != nil:
		return setupErrorf("cannot reach the server at %s: %w", lt.cfg.server, err)
	case res.User.Role != api.RoleAdmin:
		return setupErrorf("%s is not an admin of the server", lt.cfg.admin)
	}
	lt.adminID = res.User.ID
	return nil
}

// logout ends the admin's session that login made.
func (lt *loadTest) logout(ctx context.Context) error {
	return lt.admin.call(ctx, http.MethodPost, "/api/v1/auth/logout", api.Empty{}, nil, http.StatusNoContent)
}

// createInvite creates the invite that every account of the run registers with.
func (lt *loadTest) createInvite(ctx context.Context, uses int) error {
	signups := time.Duration(uses) * lt.cfg.loginInterval
	hours := min(max(int((signups+inviteSlack+time.Hour-1)/time.Hour), 1), 720) // 03 §12.4.5: 1–720
	var res api.CreateInviteResponse
	err := lt.admin.call(ctx, http.MethodPost, "/api/v1/invites",
		api.CreateInviteRequest{Note: "isshoni-loadtest " + lt.runID, ExpiresInHours: hours, MaxUses: uses},
		&res, http.StatusCreated)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return setupErrorf("creating an invite for %d accounts: %w", uses, err)
	}
	_, token, ok := strings.Cut(res.URL, "/invite#") // <origin>/invite#<token>
	if !ok || token == "" {
		return setupErrorf("the invite link %q has no token", res.URL)
	}
	lt.inviteID, lt.inviteToken = res.Invite.ID, logx.Secret(token)
	return nil
}

// revokeInvite deletes the run's invite, which has uses left when a sign-up failed.
func (lt *loadTest) revokeInvite(ctx context.Context) error {
	err := lt.admin.call(ctx, http.MethodDelete, "/api/v1/invites/"+lt.inviteID, nil, nil, http.StatusNoContent)
	var he *httpError
	if errors.As(err, &he) && he.status == http.StatusNotFound {
		return nil
	}
	return err
}

// register signs up the account lt-<run>-<n> with the run's invite. The answer carries its session cookie.
func (lt *loadTest) register(ctx context.Context, n int) (*account, error) {
	if err := lt.pace.wait(ctx); err != nil {
		return nil, err
	}
	a := &account{
		name:     fmt.Sprintf("lt-%s-%d", lt.runID, n),
		password: logx.Secret(randomPassword()),
		rest:     newRESTClient(lt.origin, lt.transport),
	}
	var res api.RegisterResponse
	err := a.rest.callRetrying(ctx, lt.log, http.MethodPost, "/api/v1/auth/register",
		api.RegisterRequest{InviteToken: lt.inviteToken.Reveal(), Username: a.name, Password: a.password.Reveal()},
		&res, http.StatusCreated)
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case err != nil:
		return nil, setupErrorf("signing up %s: %w", a.name, err)
	case res.User == nil || res.User.ID == "":
		return nil, setupErrorf("signing up %s: the answer has no user (status %q)", a.name, res.Status)
	}
	a.id = res.User.ID
	return a, nil
}

// deleteUser deletes one account of the run (03 §12.4.8). One that is gone already counts as deleted.
func (lt *loadTest) deleteUser(ctx context.Context, a *account) error {
	err := lt.admin.call(ctx, http.MethodDelete, "/api/v1/admin/users/"+a.id, nil, nil, http.StatusNoContent)
	var he *httpError
	if errors.As(err, &he) && he.status == http.StatusNotFound {
		return nil
	}
	return err
}

// dashboard reads GET /api/v1/admin/dashboard (04 §11.4).
func (lt *loadTest) dashboard(ctx context.Context) (api.OpsDashboard, error) {
	var d api.OpsDashboard
	err := lt.admin.call(ctx, http.MethodGet, "/api/v1/admin/dashboard", nil, &d, http.StatusOK)
	return d, err
}

// newRunID returns the run's part of the account names: 6 random lowercase letters and digits.
func newRunID() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b) // never fails (crypto/rand)
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))[:6]
}

// randomPassword returns a password of 24 random characters.
func randomPassword() string {
	b := make([]byte, 18)
	_, _ = rand.Read(b) // never fails (crypto/rand)
	return base64.RawURLEncoding.EncodeToString(b)
}

// sleep waits for d or until ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
