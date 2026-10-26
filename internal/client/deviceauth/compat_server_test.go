package deviceauth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/client/compat"
	"github.com/MoonWX/isshoni/internal/client/credstore"
	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/version"
)

// TestCompatServerDeviceFlow is docs/m2/02-device-auth.md §13.4's test against a running server, for 06's nightly
// compat-servers job (06 §10.3), which starts each of the last two server releases and runs
// `go test ./internal/client/... -run '^TestCompatServer'` (its name matches, so it runs next to 01's
// TestCompatServer) with:
//
//   - ISSHONI_COMPAT_SERVER_URL: the server's origin (http://127.0.0.1:<port>), the test's only source of a server
//     (README §3 item 24). Without it the test skips.
//   - ISSHONI_COMPAT_ADMIN_FILE: a JSON file {"username": "…", "password": "…"} of the server's admin, which the job
//     writes after creating the admin.
//
// Against a server whose /api/v1/info lists no deviceFlow (every 0.1.x server) it asserts only compat.FromInfo's
// verdict update_server/no_device_flow: this app can't link to such a server. Against any other server it signs in
// with POST /api/v1/auth/login (cookie), runs Manager.Link with a memory store, approves the code through
// /device/lookup and /device/decide with the cookie (as the /link page does), then calls AccessToken(ctx, true) once
// (one refresh) and Unlink (revoke): the manager is linked, the refresh gives a new access token, and Unlink returns
// nil (the server confirmed the revoke).
func TestCompatServerDeviceFlow(t *testing.T) {
	origin := strings.TrimSuffix(os.Getenv(envCompatServer), "/")
	if origin == "" {
		t.Skip(envCompatServer + " is not set: no server to test against (06 §10.3's compat-servers job sets it)")
	}
	if u, err := url.Parse(origin); err != nil || u.Host == "" {
		t.Fatalf("%s = %q is not an origin", envCompatServer, origin)
	}
	dc, err := deviceauth.NewClient(origin, deviceauth.AppInfo{Version: version.Version(),
		ClientKind: protocol.ClientKindDesktop, OS: protocol.ClientOSWindows, DeviceName: "Compat test"},
		deviceauth.ClientOptions{AllowInsecureLoopback: true})
	if err != nil {
		t.Fatalf("deviceauth.NewClient(%s): %v", origin, err)
	}
	// The manager starts nothing until it is used; its Close also closes the idle connections of the client's own
	// HTTP client, which the /info request below leaves open on either branch (TestMain's goleak).
	store := credstore.NewMemory()
	logs := &logBuffer{}
	mgr := deviceauth.NewManager(deviceauth.ManagerOptions{Client: dc, Store: store, Logger: logs.logger()})
	t.Cleanup(func() {
		if err := mgr.Close(); err != nil {
			t.Errorf("Manager.Close: %v", err)
		}
		if t.Failed() {
			t.Logf("the device manager's log:\n%s", logs.String())
		}
	})
	info, err := dc.Info(compatCtx(t))
	if err != nil {
		t.Fatalf("GET /api/v1/info: %v", err)
	}
	check := compat.FromInfo(info, compat.Default())
	t.Logf("server %s, protocol %d–%d, features %v: %s %s", info.Server.Version, info.Protocol.Min,
		info.Protocol.Current, info.Features, check.Verdict, check.Reason)
	if !slices.Contains(info.Features, api.InfoFeatureDeviceFlow) {
		if check.Verdict != compat.UpdateServer || check.Reason != compat.ReasonNoDeviceFlow {
			t.Fatalf("compat.FromInfo for a server without deviceFlow = %+v, want update_server/no_device_flow", check)
		}
		return
	}
	if check.Verdict != compat.OK {
		t.Fatalf("compat.FromInfo = %+v, want ok: this build can't use the server", check)
	}

	srv := &compatServer{t: t, origin: origin, http: newCompatHTTP(t)}
	srv.cookie = srv.login(readCompatAdmin(t))
	if err := mgr.Start(compatCtx(t)); err != nil {
		t.Fatalf("Manager.Start: %v", err)
	}
	if st := mgr.State(); st.Status != deviceauth.StatusUnlinked {
		t.Fatalf("a new manager with an empty store is %s after Start, want unlinked", st.Status)
	}

	// The device flow: Link, then the /link page's lookup and decide with the admin's cookie.
	if err := mgr.Link(compatCtx(t)); err != nil {
		t.Fatalf("Manager.Link: %v", err)
	}
	st := mgr.State()
	if st.Status != deviceauth.StatusLinking || st.Link == nil {
		t.Fatalf("after Link the manager is %s, want linking with a code", st.Status)
	}
	var lookup api.DeviceLookup
	srv.do(http.MethodPost, "/api/v1/device/lookup", api.DeviceLookupRequest{UserCode: st.Link.UserCode},
		http.StatusOK, &lookup)
	if lookup.UserCode != st.Link.UserCode || lookup.ClientKind != string(protocol.ClientKindDesktop) {
		t.Fatalf("the lookup shows the manager's code %v of a %q client, want it of a desktop app",
			lookup.UserCode == st.Link.UserCode, lookup.ClientKind)
	}
	name := "Compat test"
	srv.do(http.MethodPost, "/api/v1/device/decide",
		api.DeviceDecideRequest{UserCode: st.Link.UserCode, Approve: true, DeviceName: &name}, http.StatusNoContent, nil)
	waitFor(t, func() bool { return mgr.State().Status == deviceauth.StatusLinked },
		func() string {
			return fmt.Sprintf("the manager is %s after the approval, want linked", mgr.State().Status)
		})
	st = mgr.State()
	if st.Device == nil || st.Device.Name != name || st.User == nil {
		t.Fatalf("the linked state has device %+v and user %v, want the device named %q", st.Device, st.User != nil, name)
	}
	deviceID := st.Device.ID
	if storedToken(t, store, st.Origin) == "" {
		t.Fatal("the linked manager stored no credential")
	}

	// One refresh: a new access token.
	linked, err := mgr.AccessToken(compatCtx(t), false)
	if err != nil {
		t.Fatalf("AccessToken after the link: %v", err)
	}
	refreshed, err := mgr.AccessToken(compatCtx(t), true)
	if err != nil {
		t.Fatalf("AccessToken(ctx, true): %v", err)
	}
	if refreshed == linked {
		t.Fatal("the forced refresh gave the access token the link had issued")
	}
	if st := mgr.State(); st.Status != deviceauth.StatusLinked {
		t.Fatalf("after the refresh the manager is %s, want linked", st.Status)
	}

	// Unlink revokes the device: the server confirms it, the credential is gone, and the Devices page no longer
	// lists the device.
	if err := mgr.Unlink(compatCtx(t)); err != nil {
		t.Fatalf("Unlink: %v, want nil (the revoke confirmed)", err)
	}
	if st := mgr.State(); st.Status != deviceauth.StatusUnlinked {
		t.Errorf("after Unlink the manager is %s, want unlinked", st.Status)
	}
	if storedToken(t, store, st.Origin) != "" {
		t.Error("Unlink left the credential in the store")
	}
	var list api.DevicesResponse
	srv.do(http.MethodGet, "/api/v1/me/devices", nil, http.StatusOK, &list)
	if slices.ContainsFunc(list.Devices, func(d api.DeviceInfo) bool { return d.ID == deviceID }) {
		t.Error("GET /me/devices still lists the unlinked device")
	}
}

// The environment of TestCompatServerDeviceFlow (06 §10.3), the same as 01's TestCompatServer.
const (
	envCompatServer = "ISSHONI_COMPAT_SERVER_URL"
	envCompatAdmin  = "ISSHONI_COMPAT_ADMIN_FILE"
)

// compatTimeout bounds every request and wait of the test; a device flow link takes one 5 s poll interval.
const compatTimeout = 30 * time.Second

// compatAdmin is ISSHONI_COMPAT_ADMIN_FILE's content (README §3 item 24).
type compatAdmin struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// readCompatAdmin reads ISSHONI_COMPAT_ADMIN_FILE. A server with deviceFlow needs it: the code is approved with the
// admin's cookie.
func readCompatAdmin(t *testing.T) compatAdmin {
	t.Helper()
	path := os.Getenv(envCompatAdmin)
	if path == "" {
		t.Fatalf("%s is set, %s isn't: the test signs in as the server's admin", envCompatServer, envCompatAdmin)
	}
	b, err := os.ReadFile(path) //nolint:gosec // G304: the file the job names, read in a test
	if err != nil {
		t.Fatalf("%s: %v", envCompatAdmin, err)
	}
	var a compatAdmin
	if err := json.Unmarshal(b, &a); err != nil || a.Username == "" || a.Password == "" {
		t.Fatalf("%s: want {\"username\", \"password\"} (%v)", envCompatAdmin, err)
	}
	return a
}

// compatServer is the server under test as the admin's browser reaches it, with the admin's session cookie once
// signed in.
type compatServer struct {
	t      *testing.T
	origin string
	http   *http.Client
	cookie string // the Cookie header of the admin's session
}

// newCompatHTTP returns an HTTP client for the test's own REST calls that follows no redirect; its idle
// connections close when the test ends.
func newCompatHTTP(t *testing.T) *http.Client {
	tr := &http.Transport{}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: compatTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// do sends a JSON request with the admin's cookie when there is one, decodes the answer into out (nil drops it) and
// returns the cookies it set. Any status but want fails the test; the body is printed only for an error answer,
// which holds no token. No Origin header: a request without browser headers passes the server's CSRF check.
func (s *compatServer) do(method, path string, body any, want int, out any) []*http.Cookie {
	s.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			s.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(compatCtx(s.t), method, s.origin+path, rd)
	if err != nil {
		s.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if s.cookie != "" {
		req.Header.Set("Cookie", s.cookie)
	}
	res, err := s.http.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if err != nil {
		s.t.Fatalf("%s %s: reading the answer: %v", method, path, err)
	}
	if res.StatusCode != want {
		if res.StatusCode >= http.StatusBadRequest {
			s.t.Fatalf("%s %s = %d %s, want %d", method, path, res.StatusCode, b, want)
		}
		s.t.Fatalf("%s %s = %d, want %d", method, path, res.StatusCode, want)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			s.t.Fatalf("%s %s: decoding the answer: %v", method, path, err)
		}
	}
	return res.Cookies()
}

// login signs the admin in (POST /api/v1/auth/login) and returns the session cookie as a Cookie header's value.
func (s *compatServer) login(a compatAdmin) string {
	s.t.Helper()
	set := s.do(http.MethodPost, "/api/v1/auth/login", api.LoginRequest{Username: a.Username, Password: a.Password},
		http.StatusOK, nil)
	var cookies []string
	for _, c := range set {
		if c.Value != "" {
			cookies = append(cookies, c.Name+"="+c.Value)
		}
	}
	if len(cookies) == 0 {
		s.t.Fatal("the login set no cookie")
	}
	return strings.Join(cookies, "; ")
}

// compatCtx returns a context that ends after compatTimeout or with the test.
func compatCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), compatTimeout)
	t.Cleanup(cancel)
	return ctx
}

// waitFor polls cond until it holds, failing the test after compatTimeout.
func waitFor(t *testing.T, cond func() bool, what func() string) {
	t.Helper()
	for deadline := time.Now().Add(compatTimeout); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
