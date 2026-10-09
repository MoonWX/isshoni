package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/version"
)

// raw sends one request over the socket and returns the status, the header and the body as sent.
func (a *testAdmin) raw(t *testing.T, method, path, body string) (int, http.Header, string) {
	t.Helper()
	var r io.Reader
	contentType := ""
	if body != "" {
		r, contentType = strings.NewReader(body), "application/json"
	}
	res, err := a.client.exchange(t.Context(), method, path, contentType, r)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("%s %s: reading the answer: %v", method, path, err)
	}
	return res.StatusCode, res.Header, string(data)
}

// adminError returns err as an *AdminError with the wanted code, or fails the test.
func adminError(t *testing.T, err error, code string) *AdminError {
	t.Helper()
	var ae *AdminError
	if !errors.As(err, &ae) {
		t.Fatalf("error %v (%T), want an *AdminError with code %s", err, err, code)
	}
	if ae.API.Code != code || !api.IsCode(err, code) {
		t.Fatalf("error code %q (%v), want %s", ae.API.Code, err, code)
	}
	if ae.Status != api.StatusOf(code) {
		t.Errorf("%s came with HTTP %d, want api.StatusOf = %d", code, ae.Status, api.StatusOf(code))
	}
	return ae
}

// wantCode fails the test unless err is an *AdminError with the wanted code.
func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	_ = adminError(t, err, code)
}

func someUsers() []AdminUser {
	return []AdminUser{
		{
			ID: "0123456789ab", Username: "alex", Role: api.RoleAdmin, Status: api.UserStatusActive,
			CreatedAt:  api.WireTime(time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)),
			LastSeenAt: api.WireTime(time.Date(2026, 10, 1, 12, 0, 0, 123456789, time.FixedZone("CEST", 7200))),
		},
		{
			ID: "cdefghjkmnpq", Username: "sam", Role: api.RoleUser, Status: api.UserStatusDisabled,
			CreatedAt: api.WireTime(time.Date(2026, 10, 1, 8, 30, 0, 0, time.UTC)),
		},
	}
}

// The health endpoints over the socket (04 §12.2): the version on /v1/health, the checks on /v1/ready, and both
// keep answering during a shutdown.
func TestAdminHealthAndReady(t *testing.T) {
	a := startAdmin(t, nil)
	var certReady atomic.Bool
	a.health.AddCheck("db", func() (bool, string) { return true, "" })
	a.health.AddCheck("tls", func() (bool, string) {
		return certReady.Load(), "waiting: obtaining certificate for 203.0.113.7"
	})

	code, hdr, body := a.raw(t, http.MethodGet, "/v1/health", "")
	wantHealth := `{"status":"ok","version":"` + version.Version() + `"}` + "\n"
	if code != 200 || body != wantHealth || hdr.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Errorf("/v1/health = %d %q (%s), want 200 %q", code, body, hdr.Get("Content-Type"), wantHealth)
	}
	code, _, body = a.raw(t, http.MethodGet, "/v1/ready", "")
	if want := `{"status":"not_ready","checks":{"db":"ok","tls":"waiting: obtaining certificate for 203.0.113.7"}}` + "\n"; code != 503 || body != want {
		t.Errorf("/v1/ready while starting = %d %q, want 503 %q", code, body, want)
	}
	h, err := a.client.Ready(t.Context())
	if err != nil || h.Status != StatusNotReady || h.Checks["tls"] == "" {
		t.Errorf("client.Ready while starting = %+v, %v", h, err)
	}

	certReady.Store(true)
	code, _, body = a.raw(t, http.MethodGet, "/v1/ready", "")
	if want := `{"status":"ready","checks":{"db":"ok","tls":"ok"}}` + "\n"; code != 200 || body != want {
		t.Errorf("/v1/ready = %d %q, want 200 %q", code, body, want)
	}
	h, err = a.client.Health(t.Context())
	if err != nil || h.Status != StatusOK || h.Version != version.Version() {
		t.Errorf("client.Health = %+v, %v", h, err)
	}
	// HEAD answers like GET, without a body.
	if code, _, body := a.raw(t, http.MethodHead, "/v1/health", ""); code != 200 || body != "" {
		t.Errorf("HEAD /v1/health = %d %q", code, body)
	}

	a.health.SetShuttingDown()
	code, _, body = a.raw(t, http.MethodGet, "/v1/health", "")
	if want := `{"status":"shutting_down"}` + "\n"; code != 503 || body != want {
		t.Errorf("/v1/health during shutdown = %d %q, want 503 %q", code, body, want)
	}
	h, err = a.client.Health(t.Context())
	if err != nil || h.Status != StatusShuttingDown {
		t.Errorf("client.Health during shutdown = %+v, %v; want the status, not an error", h, err)
	}
	h, err = a.client.Ready(t.Context())
	if err != nil || h.Status != StatusShuttingDown {
		t.Errorf("client.Ready during shutdown = %+v, %v", h, err)
	}
}

func TestAdminStatus(t *testing.T) {
	t.Run("what ops knows by itself", func(t *testing.T) {
		a := startAdmin(t, nil)
		st, err := a.client.Status(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if st.Version != version.Version() || st.StartedAt.IsZero() || time.Since(st.StartedAt) > time.Minute || st.UptimeS < 0 {
			t.Errorf("status = %+v", st)
		}
		_, _, body := a.raw(t, http.MethodGet, "/v1/status", "")
		if !regexp.MustCompile(`"startedAt":"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z"`).MatchString(body) ||
			!strings.Contains(body, `"advertised":[]`) || !strings.Contains(body, `"listeners":[]`) {
			t.Errorf("status document: %s", body)
		}
	})
	t.Run("from the wiring", func(t *testing.T) {
		want := api.ServerStatus{
			Version: "0.3.0", StartedAt: time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC), UptimeS: 3600,
			Origin: "https://watch.example.com", TLS: api.TLSInfo{Mode: api.TLSModeAuto, Names: []string{"watch.example.com"}, Ready: true},
			PublicIPv4: "203.0.113.7", Rooms: 1, Participants: 3, Shares: 2, SchemaVersion: 7,
			Advertised: []api.AdvertisedAddr{}, Listeners: []api.ListenerInfo{{Key: "listen.admin_socket", Network: "unix", Addr: "/run/isshoni/admin.sock"}},
		}
		var fail atomic.Bool
		a := startAdmin(t, func(o *AdminOptions, _ *AdminListenOptions) {
			o.Status = func(context.Context) (api.ServerStatus, error) {
				if fail.Load() {
					return api.ServerStatus{}, errors.New("the hub snapshot failed")
				}
				return want, nil
			}
		})
		got, err := a.client.Status(t.Context())
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("Status = %+v, %v\nwant %+v", got, err, want)
		}
		// It answers during a shutdown too: `admin status` then still says what is going on.
		a.health.SetShuttingDown()
		if _, err := a.client.Status(t.Context()); err != nil {
			t.Errorf("Status during shutdown: %v", err)
		}
		fail.Store(true)
		_, err = a.client.Status(t.Context())
		wantCode(t, err, api.CodeInternal)
	})
}

// setup-url mints a new link each time, says whether the certificate is there, and answers setup_unavailable with
// the reset-password fix once an admin exists (04 §12.5).
func TestAdminSetupURL(t *testing.T) {
	a := startAdmin(t, nil)
	var certReady atomic.Bool
	certReady.Store(true)
	a.health.AddCheck("tls", func() (bool, string) { return certReady.Load(), "waiting" })

	code, _, body := a.raw(t, http.MethodPost, "/v1/setup-url", `{"waitReadyS": 0}`)
	if want := `{"url":"https://watch.example.com/setup#token1","expiresAt":"2026-09-30T10:00:00.000Z","tlsReady":true}` + "\n"; code != 200 || body != want {
		t.Errorf("POST /v1/setup-url = %d %q\nwant 200 %q", code, body, want)
	}
	// No body at all is a request with every field left out (curl --unix-socket … -X POST).
	if code, _, body := a.raw(t, http.MethodPost, "/v1/setup-url", ""); code != 200 || !strings.Contains(body, "/setup#token2") {
		t.Errorf("POST /v1/setup-url without a body = %d %q", code, body)
	}

	certReady.Store(false)
	got, err := a.client.SetupURL(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "https://watch.example.com/setup#token3" || got.TLSReady ||
		!time.Time(got.ExpiresAt).Equal(time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("SetupURL with the certificate pending = %+v", got)
	}

	a.accounts.with(func(f *fakeAccounts) { f.setupDone = true })
	_, err = a.client.SetupURL(t.Context(), 0)
	ae := adminError(t, err, api.CodeSetupUnavailable)
	if !strings.Contains(ae.Message, "an admin account exists") || !strings.Contains(ae.Fix, "isshoni admin users reset-password <name>") {
		t.Errorf("setup_unavailable message %q, fix %q", ae.Message, ae.Fix)
	}
	if ae.Error() != ae.Message {
		t.Errorf("Error() = %q, want the message", ae.Error())
	}
	// Even with a wait: once an admin exists there is nothing to wait for.
	start := time.Now()
	_, err = a.client.SetupURL(t.Context(), 30*time.Second)
	wantCode(t, err, api.CodeSetupUnavailable)
	if time.Since(start) > 5*time.Second {
		t.Error("setup_unavailable took as long as the wait")
	}

	// The longest wait is AdminMaxWaitReady, which the CLI checks --wait against.
	if AdminMaxWaitReady != time.Hour {
		t.Errorf("AdminMaxWaitReady = %v, want 1h", AdminMaxWaitReady)
	}
	_, err = a.client.SetupURL(t.Context(), AdminMaxWaitReady)
	wantCode(t, err, api.CodeSetupUnavailable) // accepted: the answer is about the setup, not about the wait
	_, err = a.client.SetupURL(t.Context(), AdminMaxWaitReady+time.Second)
	if ae := adminError(t, err, api.CodeBadRequest); !strings.Contains(ae.Message, "waitReadyS 3601: want 0 to 3600") {
		t.Errorf("a wait over the limit: message %q", ae.Message)
	}
	for _, bad := range []string{`{"waitReadyS": -1}`, `{"waitReadyS": 3601}`, `{"waitReadyS": 999999}`, `{"waitReadyS": "soon"}`, `{`, `{} {}`} {
		code, _, body := a.raw(t, http.MethodPost, "/v1/setup-url", bad)
		if code != 400 || !strings.Contains(body, `"code":"bad_request"`) || !strings.Contains(body, `"message":"`) {
			t.Errorf("POST /v1/setup-url %s = %d %s, want 400 bad_request with a message", bad, code, body)
		}
	}
	// The setup link's token never reaches the log.
	if strings.Contains(a.logs.String(), "token") {
		t.Errorf("the log holds a setup link:\n%s", a.logs)
	}
}

// waitReadyS: the server waits for readiness, mints the link as soon as it is there, and mints it anyway when the
// time runs out. Driven through the handler under a fake clock.
func TestAdminSetupURLWaitsForReadiness(t *testing.T) {
	post := func(srv *AdminServer, body string) (*httptest.ResponseRecorder, time.Duration) {
		start := time.Now()
		w := httptest.NewRecorder()
		r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/setup-url", strings.NewReader(body))
		srv.Handler().ServeHTTP(w, r)
		return w, time.Since(start)
	}
	t.Run("ready after 40 s", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			health := NewHealth()
			var ready atomic.Bool
			health.AddCheck("tls", func() (bool, string) { return ready.Load(), "waiting: obtaining certificate" })
			srv := NewAdminServer(AdminOptions{Health: health, Accounts: newFakeAccounts()})
			time.AfterFunc(40*time.Second, func() { ready.Store(true) })
			w, took := post(srv, `{"waitReadyS": 180}`)
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"tlsReady":true`) {
				t.Errorf("answer %d %s", w.Code, w.Body)
			}
			if took < 40*time.Second || took > 41*time.Second {
				t.Errorf("answered after %v, want right after readiness at 40 s", took)
			}
		})
	})
	t.Run("the wait runs out", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			health := NewHealth()
			health.AddCheck("tls", func() (bool, string) { return false, "waiting: obtaining certificate" })
			accounts := newFakeAccounts()
			srv := NewAdminServer(AdminOptions{Health: health, Accounts: accounts})
			w, took := post(srv, `{"waitReadyS": 180}`)
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"tlsReady":false`) || !strings.Contains(w.Body.String(), "/setup#token1") {
				t.Errorf("answer %d %s; want the link with tlsReady false", w.Code, w.Body)
			}
			if took != 180*time.Second {
				t.Errorf("answered after %v, want 180 s", took)
			}
		})
	})
	t.Run("only another check fails", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			health := NewHealth()
			health.AddCheck("tls", func() (bool, string) { return true, "" })
			health.AddCheck("media", func() (bool, string) { return false, "no socket bound" })
			srv := NewAdminServer(AdminOptions{Health: health, Accounts: newFakeAccounts()})
			w, took := post(srv, `{"waitReadyS": 5}`)
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"tlsReady":true`) || took != 5*time.Second {
				t.Errorf("answer %d %s after %v; want tlsReady true after the full 5 s", w.Code, w.Body, took)
			}
		})
	})
	t.Run("shutdown ends the wait", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			health := NewHealth()
			health.AddCheck("tls", func() (bool, string) { return false, "" })
			accounts := newFakeAccounts()
			srv := NewAdminServer(AdminOptions{Health: health, Accounts: accounts})
			time.AfterFunc(10*time.Second, func() { _ = srv.Shutdown(context.Background()) })
			w, took := post(srv, `{"waitReadyS": 180}`)
			if w.Code != 503 || !strings.Contains(w.Body.String(), `"code":"server_shutdown"`) || took != 10*time.Second {
				t.Errorf("answer %d %s after %v; want 503 server_shutdown at 10 s", w.Code, w.Body, took)
			}
			if accounts.issued != 0 {
				t.Error("a link was minted although the server shut down")
			}
		})
	})
	t.Run("the client gives up", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			health := NewHealth()
			health.AddCheck("tls", func() (bool, string) { return false, "" })
			accounts := newFakeAccounts()
			srv := NewAdminServer(AdminOptions{Health: health, Accounts: accounts})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			w := httptest.NewRecorder()
			r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/setup-url", strings.NewReader(`{"waitReadyS": 180}`))
			start := time.Now()
			srv.Handler().ServeHTTP(w, r)
			if time.Since(start) != 3*time.Second || accounts.issued != 0 {
				t.Errorf("returned after %v with %d links minted; want 3 s and none", time.Since(start), accounts.issued)
			}
		})
	})
}

func TestAdminUsers(t *testing.T) {
	a := startAdmin(t, nil)
	// No users: an empty list, never null.
	if code, _, body := a.raw(t, http.MethodGet, "/v1/users", ""); code != 200 || body != `{"users":[]}`+"\n" {
		t.Errorf("GET /v1/users without users = %d %q", code, body)
	}
	a.accounts.with(func(f *fakeAccounts) {
		f.users = someUsers()
		f.admins["alex"] = true
	})

	code, _, body := a.raw(t, http.MethodGet, "/v1/users", "")
	want := `{"users":[` +
		`{"id":"0123456789ab","username":"alex","role":"admin","status":"active","createdAt":"2026-09-30T10:00:00.000Z","lastSeenAt":"2026-10-01T10:00:00.123Z"},` +
		`{"id":"cdefghjkmnpq","username":"sam","role":"user","status":"disabled","createdAt":"2026-10-01T08:30:00.000Z"}]}` + "\n"
	if code != 200 || body != want {
		t.Errorf("GET /v1/users = %d\n got %s\nwant %s", code, body, want)
	}
	got, err := a.client.Users(t.Context())
	if err != nil || len(got.Users) != 2 || got.Users[0].Username != "alex" || !got.Users[1].LastSeenAt.IsZero() {
		t.Errorf("client.Users = %+v, %v", got, err)
	}

	// reset-link
	code, _, body = a.raw(t, http.MethodPost, "/v1/users/alex/reset-link", "")
	if want := `{"url":"https://watch.example.com/reset#reset-alex","expiresAt":"2026-09-30T10:00:00.000Z"}` + "\n"; code != 200 || body != want {
		t.Errorf("reset-link = %d %q, want %q", code, body, want)
	}
	link, err := a.client.ResetLink(t.Context(), "sam")
	if err != nil || link.URL != "https://watch.example.com/reset#reset-sam" {
		t.Errorf("client.ResetLink = %+v, %v", link, err)
	}
	_, err = a.client.ResetLink(t.Context(), "nobody")
	if ae := adminError(t, err, api.CodeUserNotFound); !strings.Contains(ae.Message, `no user named "nobody"`) {
		t.Errorf("user_not_found message %q", ae.Message)
	}
	// A name that needs escaping in a path arrives as typed.
	_, err = a.client.ResetLink(t.Context(), "a/b c?")
	if ae := adminError(t, err, api.CodeUserNotFound); !strings.Contains(ae.Message, `no user named "a/b c?"`) {
		t.Errorf("escaped name: message %q", ae.Message)
	}

	// role
	if code, _, body := a.raw(t, http.MethodPost, "/v1/users/sam/role", `{"role":"admin"}`); code != 204 || body != "" {
		t.Errorf("role = %d %q, want 204 without a body", code, body)
	}
	if !a.accounts.isAdmin("sam") {
		t.Error("sam is not an admin after the call")
	}
	if err := a.client.SetRole(t.Context(), "sam", api.RoleUser); err != nil {
		t.Errorf("SetRole user: %v", err)
	}
	err = a.client.SetRole(t.Context(), "alex", api.RoleUser)
	if ae := adminError(t, err, api.CodeLastAdmin); !strings.Contains(ae.Message, `"alex" is the last active admin`) || ae.Fix == "" {
		t.Errorf("last_admin message %q, fix %q", ae.Message, ae.Fix)
	}
	wantCode(t, a.client.SetRole(t.Context(), "nobody", api.RoleAdmin), api.CodeUserNotFound)
	err = a.client.SetRole(t.Context(), "sam", "owner")
	if ae := adminError(t, err, api.CodeValidationFailed); ae.API.Fields["role"] != api.FieldInvalid || !strings.Contains(ae.Message, "role (invalid)") {
		t.Errorf("bad role: %+v", ae)
	}
	if code, _, _ := a.raw(t, http.MethodPost, "/v1/users/sam/role", ""); code != 422 {
		t.Errorf("role without a body = %d, want 422", code)
	}

	// disable, enable
	if err := a.client.SetDisabled(t.Context(), "sam", true); err != nil || !a.accounts.isDisabled("sam") {
		t.Errorf("disable sam: %v, disabled = %v", err, a.accounts.isDisabled("sam"))
	}
	if code, _, body := a.raw(t, http.MethodPost, "/v1/users/sam/enable", ""); code != 204 || body != "" || a.accounts.isDisabled("sam") {
		t.Errorf("enable = %d %q, disabled = %v", code, body, a.accounts.isDisabled("sam"))
	}
	wantCode(t, a.client.SetDisabled(t.Context(), "alex", true), api.CodeLastAdmin)
	wantCode(t, a.client.SetDisabled(t.Context(), "nobody", false), api.CodeUserNotFound)

	// "", "." and ".." are no path elements: the router would redirect them, and the answer would read as "this
	// is not isshoni's socket". They are no usernames either, so the client answers user_not_found itself, with
	// the server's words.
	for _, name := range []string{"", ".", ".."} {
		calls := map[string]func() error{
			"ResetLink": func() error { _, err := a.client.ResetLink(t.Context(), name); return err },
			"SetRole":   func() error { return a.client.SetRole(t.Context(), name, api.RoleAdmin) },
			"disable":   func() error { return a.client.SetDisabled(t.Context(), name, true) },
			"enable":    func() error { return a.client.SetDisabled(t.Context(), name, false) },
		}
		for what, call := range calls {
			ae := adminError(t, call(), api.CodeUserNotFound)
			if want := fmt.Sprintf("there is no user named %q", name); ae.Message != want || ae.Status != 404 ||
				ae.Fix != "`isshoni admin users list` shows the accounts" {
				t.Errorf("%s(%q): %+v, want %q with the fix", what, name, ae, want)
			}
		}
	}
	// It is the client's answer, so it is the same without a server. A name with dots in it is a name.
	offline := DialAdmin(socketPath(t))
	wantCode(t, offline.SetDisabled(t.Context(), "..", true), api.CodeUserNotFound)
	if err := offline.SetDisabled(t.Context(), "...", true); !errors.Is(err, ErrAdminNotRunning) {
		t.Errorf(`SetDisabled("...") without a server: %v, want ErrAdminNotRunning`, err)
	}
	wantCode(t, a.client.SetDisabled(t.Context(), "a..b", true), api.CodeUserNotFound) // asked: the server does not know it
	wantCode(t, a.client.SetDisabled(t.Context(), "...", true), api.CodeUserNotFound)
}

// invite create sends only what the operator set; the server passes 0 for the rest, so 03's settings apply
// (04 §12.5).
func TestAdminInvites(t *testing.T) {
	a := startAdmin(t, nil)
	code, _, body := a.raw(t, http.MethodPost, "/v1/invites", `{"uses":10,"ttl":"168h"}`)
	if want := `{"url":"https://watch.example.com/invite#inv","expiresAt":"2026-10-06T10:00:00.000Z"}` + "\n"; code != 200 || body != want {
		t.Errorf("POST /v1/invites = %d %q, want %q", code, body, want)
	}
	if _, err := a.client.CreateInvite(t.Context(), 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := a.client.CreateInvite(t.Context(), 1000, 720*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := a.client.CreateInvite(t.Context(), 0, time.Hour); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := a.raw(t, http.MethodPost, "/v1/invites", ""); code != 200 {
		t.Errorf("POST /v1/invites without a body = %d", code)
	}
	want := [][2]int{{10, 168}, {0, 0}, {1000, 720}, {0, 1}, {0, 0}}
	if got := a.accounts.invitesSeen(); !reflect.DeepEqual(got, want) {
		t.Errorf("accounts saw (uses, ttl hours) %v, want %v", got, want)
	}

	for body, fields := range map[string]map[string]string{
		`{"uses":-1}`:              {"uses": "out_of_range"},
		`{"uses":1001}`:            {"uses": "out_of_range"},
		`{"ttl":"90m"}`:            {"ttl": "out_of_range"},
		`{"ttl":"30m"}`:            {"ttl": "out_of_range"},
		`{"ttl":"721h"}`:           {"ttl": "out_of_range"},
		`{"ttl":"-24h"}`:           {"ttl": "out_of_range"},
		`{"ttl":"a week"}`:         {"ttl": "invalid"},
		`{"uses":5000,"ttl":"0h"}`: {"uses": "out_of_range", "ttl": "out_of_range"},
	} {
		code, _, answer := a.raw(t, http.MethodPost, "/v1/invites", body)
		var doc AdminErrorResponse
		if err := json.Unmarshal([]byte(answer), &doc); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if code != 422 || doc.Error.Code != api.CodeValidationFailed || !reflect.DeepEqual(doc.Error.Fields, fields) {
			t.Errorf("POST /v1/invites %s = %d %s, want 422 validation_failed %v", body, code, answer, fields)
		}
	}
	if len(a.accounts.invitesSeen()) != len(want) {
		t.Error("an invalid request reached the accounts")
	}

	a.accounts.with(func(f *fakeAccounts) { f.closed = true })
	_, err := a.client.CreateInvite(t.Context(), 0, 0)
	if ae := adminError(t, err, api.CodeRegistrationClosed); !strings.Contains(ae.Message, "registration is closed") || ae.Fix == "" {
		t.Errorf("registration_closed message %q, fix %q", ae.Message, ae.Fix)
	}
}

// log-level switches the level for a while and logs one line with the peer's uid (04 §12.1).
func TestAdminLogLevel(t *testing.T) {
	a := startAdmin(t, nil)
	if code, _, body := a.raw(t, http.MethodPost, "/v1/log-level", `{"level":"debug","for":"30m"}`); code != 204 || body != "" {
		t.Fatalf("POST /v1/log-level = %d %q, want 204", code, body)
	}
	if got := a.level.Level(); got != slog.LevelDebug {
		t.Errorf("level %v after the call, want debug", got)
	}
	logs := a.logs.String()
	if !strings.Contains(logs, "log level changed through the admin socket") || !strings.Contains(logs, `"log_level":"debug"`) ||
		!strings.Contains(logs, `"for":"30m0s"`) {
		t.Errorf("no log line for the change:\n%s", logs)
	}
	if peerCredSupported && !strings.Contains(logs, `"peer_uid":`) {
		t.Errorf("the line has no peer_uid:\n%s", logs)
	}
	if err := a.client.SetLogLevel(t.Context(), "error", time.Second); err != nil || a.level.Level() != slog.LevelError {
		t.Errorf("SetLogLevel error: %v, level %v", err, a.level.Level())
	}
	// Without "for" it is the CLI's default, 30 minutes.
	if code, _, _ := a.raw(t, http.MethodPost, "/v1/log-level", `{"level":"warn"}`); code != 204 || a.level.Level() != slog.LevelWarn {
		t.Errorf("log-level without for = %d, level %v", code, a.level.Level())
	}

	for _, bad := range []string{`{"level":"loud"}`, `{"level":"DEBUG"}`, `{}`, ``, `{"level":"debug","for":"soon"}`, `{"level":"debug","for":"0s"}`, `{"level":"debug","for":"-5m"}`} {
		code, _, body := a.raw(t, http.MethodPost, "/v1/log-level", bad)
		if code != 400 || !strings.Contains(body, `"code":"bad_request"`) {
			t.Errorf("POST /v1/log-level %s = %d %s, want 400 bad_request", bad, code, body)
		}
	}
	if a.level.Level() != slog.LevelWarn {
		t.Errorf("a refused request changed the level to %v", a.level.Level())
	}
}

// What the socket answers outside the happy path: unknown paths, wrong methods, the endpoints of later slices,
// servers without accounts, internal errors, oversized bodies and the shutdown gate. The status is always
// api.StatusOf(code).
func TestAdminErrors(t *testing.T) {
	a := startAdmin(t, nil)

	code, _, body := a.raw(t, http.MethodGet, "/v2/health", "")
	if code != 404 || !strings.Contains(body, `"code":"not_found"`) || !strings.Contains(body, "no GET /v2/health") {
		t.Errorf("unknown path = %d %s", code, body)
	}
	code, hdr, body := a.raw(t, http.MethodPost, "/v1/health", "")
	if code != 405 || hdr.Get("Allow") != "GET, HEAD" || !strings.Contains(body, `"code":"method_not_allowed"`) {
		t.Errorf("POST /v1/health = %d (Allow %q) %s", code, hdr.Get("Allow"), body)
	}
	code, hdr, _ = a.raw(t, http.MethodGet, "/v1/setup-url", "")
	if code != 405 || hdr.Get("Allow") != "POST" {
		t.Errorf("GET /v1/setup-url = %d (Allow %q)", code, hdr.Get("Allow"))
	}

	// The endpoints of a later slice are declared and say so.
	for _, ep := range [][2]string{
		{http.MethodGet, "/v1/backup?certs=1"}, {http.MethodPost, "/v1/restore"}, {http.MethodPost, "/v1/rotate-secrets"},
	} {
		code, _, body := a.raw(t, ep[0], ep[1], "")
		if code != 500 || !strings.Contains(body, `"code":"internal"`) || !strings.Contains(body, "not implemented in this build yet") {
			t.Errorf("%s %s = %d %s, want 500 internal: not implemented", ep[0], ep[1], code, body)
		}
	}
	_, err := a.client.RotateSecrets(t.Context(), AdminRotateRequest{Keys: []string{"session"}, VAPID: true})
	wantCode(t, err, api.CodeInternal)
	_, err = a.client.Backup(t.Context(), true)
	wantCode(t, err, api.CodeInternal)
	_, err = a.client.Restore(t.Context(), strings.NewReader("not an archive"), AdminContentArchive)
	wantCode(t, err, api.CodeInternal)

	// An error that is not 03's: 500 internal with a ref that is in the log, and nothing of the error on the wire.
	a.accounts.with(func(f *fakeAccounts) { f.fail = errors.New("database is locked: /var/lib/isshoni/isshoni.db") })
	_, err = a.client.Users(t.Context())
	ae := adminError(t, err, api.CodeInternal)
	ref := ae.API.RequestID
	if len(ref) != 8 || !strings.Contains(ae.Message, "ref "+ref) || strings.Contains(ae.Message, "database is locked") {
		t.Errorf("internal error: ref %q, message %q", ref, ae.Message)
	}
	if logs := a.logs.String(); !strings.Contains(logs, `"ref":"`+ref+`"`) || !strings.Contains(logs, "database is locked") ||
		!strings.Contains(logs, `"route":"GET /v1/users"`) {
		t.Errorf("the log has no line for ref %s:\n%s", ref, logs)
	}
	a.accounts.with(func(f *fakeAccounts) { f.fail = nil })

	// A body over the limit.
	code, _, body = a.raw(t, http.MethodPost, "/v1/invites", `{"ttl":"`+strings.Repeat("1", adminMaxJSONBytes)+`h"}`)
	if code != 413 || !strings.Contains(body, `"code":"payload_too_large"`) {
		t.Errorf("oversized body = %d %s", code, body)
	}

	// Once the shutdown has begun, everything but health, ready and status answers 503 server_shutdown.
	a.health.SetShuttingDown()
	_, err = a.client.Users(t.Context())
	if ae := adminError(t, err, api.CodeServerShutdown); !strings.Contains(ae.Message, "shutting down") {
		t.Errorf("server_shutdown message %q", ae.Message)
	}
	_, err = a.client.SetupURL(t.Context(), 0)
	wantCode(t, err, api.CodeServerShutdown)
	wantCode(t, a.client.SetLogLevel(t.Context(), "debug", time.Minute), api.CodeServerShutdown)
}

// A handler that panics is a bug: the client still gets an answer, 500 internal with a ref, and the log has the
// stack under that ref.
func TestAdminPanic(t *testing.T) {
	a := startAdmin(t, func(o *AdminOptions, _ *AdminListenOptions) {
		o.Status = func(context.Context) (api.ServerStatus, error) { panic("the status source blew up") }
	})
	_, err := a.client.Status(t.Context())
	ae := adminError(t, err, api.CodeInternal)
	ref := ae.API.RequestID
	if len(ref) != 8 || !strings.Contains(ae.Message, "ref "+ref) || strings.Contains(ae.Message, "blew up") {
		t.Errorf("answer to a panic: ref %q, message %q", ref, ae.Message)
	}
	logs := a.logs.String()
	if !strings.Contains(logs, "an admin socket request panicked") || !strings.Contains(logs, `"ref":"`+ref+`"`) ||
		!strings.Contains(logs, "the status source blew up") || !strings.Contains(logs, `"route":"GET /v1/status"`) ||
		!strings.Contains(logs, "adminapi_test.go") {
		t.Errorf("the log has no line with the panic and its stack:\n%s", logs)
	}
	// The server goes on.
	if _, err := a.client.Health(t.Context()); err != nil {
		t.Errorf("health after a panic: %v", err)
	}
}

// A server whose wiring has no accounts or log level yet says so instead of crashing.
func TestAdminWithoutParts(t *testing.T) {
	a := startAdmin(t, func(o *AdminOptions, _ *AdminListenOptions) { o.Accounts, o.LogLevel = nil, nil })
	calls := map[string]func() error{
		"setup-url":  func() error { _, err := a.client.SetupURL(t.Context(), 0); return err },
		"users":      func() error { _, err := a.client.Users(t.Context()); return err },
		"reset-link": func() error { _, err := a.client.ResetLink(t.Context(), "alex"); return err },
		"role":       func() error { return a.client.SetRole(t.Context(), "alex", api.RoleAdmin) },
		"disable":    func() error { return a.client.SetDisabled(t.Context(), "alex", true) },
		"invite":     func() error { _, err := a.client.CreateInvite(t.Context(), 0, 0); return err },
		"log-level":  func() error { return a.client.SetLogLevel(t.Context(), "debug", time.Minute) },
	}
	for name, call := range calls {
		ae := adminError(t, call(), api.CodeInternal)
		if ae.Message == "" || strings.Contains(ae.Message, "ref ") {
			t.Errorf("%s: message %q, want the reason, not an internal-error ref", name, ae.Message)
		}
	}
	if _, err := a.client.Health(t.Context()); err != nil {
		t.Errorf("health: %v", err)
	}
}

func TestDescribeErrors(t *testing.T) {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/users/alex/role", nil)
	r.SetPathValue("name", "alex")
	for _, code := range []string{
		api.CodeSetupUnavailable, api.CodeUserNotFound, api.CodeLastAdmin, api.CodeRegistrationClosed,
		api.CodeLimitReached, api.CodeValidationFailed, api.CodePayloadTooLarge, api.CodeServerShutdown,
		api.CodeServerBusy, api.CodeRateLimited,
	} {
		if msg, _ := describe(api.NewError(code), r); msg == "" {
			t.Errorf("no message for %s", code)
		}
	}
	// A code the socket has no text for: the CLI prints the code.
	if msg, fix := describe(api.NewError(api.CodeRoomNotFound), r); msg != "" || fix != "" {
		t.Errorf("room_not_found: %q, %q", msg, fix)
	}
	msg, _ := describe(&api.Error{Code: api.CodeLimitReached, Params: map[string]any{api.ParamLimit: "invites"}}, r)
	if !strings.Contains(msg, "invites") {
		t.Errorf("limit_reached message %q", msg)
	}
	if e := (&AdminError{Status: 404, API: api.Error{Code: api.CodeRoomNotFound}}).Error(); e != "the server answered room_not_found" {
		t.Errorf("AdminError without a message: %q", e)
	}
	if e := (&AdminError{Status: 502}).Error(); e != "the server answered HTTP 502" {
		t.Errorf("AdminError without a code: %q", e)
	}
}

// Retry-After travels as a header too.
func TestAdminRetryAfter(t *testing.T) {
	a := startAdmin(t, nil)
	a.accounts.with(func(f *fakeAccounts) { f.fail = &api.Error{Code: api.CodeServerBusy, RetryAfter: 3} })
	code, hdr, body := a.raw(t, http.MethodGet, "/v1/users", "")
	if code != 503 || hdr.Get("Retry-After") != "3" || !strings.Contains(body, `"retryAfter":3`) {
		t.Errorf("server_busy = %d (Retry-After %q) %s", code, hdr.Get("Retry-After"), body)
	}
}
