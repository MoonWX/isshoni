package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
	"github.com/MoonWX/isshoni/internal/version"
)

// wantInfoJSON is the 03 §12.4.1 shape with the fixture's fake sources, a fresh DB (default settings, no admin yet)
// and the domain site.
const wantInfoJSON = `{
  "server": {"name": "watch.example.com", "version": "1.2.3", "publicUrl": "https://watch.example.com"},
  "protocol": {"current": 3, "min": 2},
  "minClientVersion": "",
  "registration": "invite",
  "setupRequired": true,
  "features": ["push", "passwordReset"],
  "accountRules": {"usernameMinLength": 2, "usernameMaxLength": 32, "passwordMinLength": 8, "passwordMaxLength": 128},
  "push": {"vapidPublicKey": "` + testVAPIDKey + `"}
}`

// getInfoJSON serves GET /api/v1/info and returns the decoded body, checking the status and headers of 03 §12.1.
func getInfoJSON(t *testing.T, f *apiFixture) map[string]any {
	t.Helper()
	rec := f.do(http.MethodGet, "/api/v1/info", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d (body %q)", rec.Code, rec.Body)
	}
	for h, want := range map[string]string{
		"Content-Type":            jsonContentType,
		"Cache-Control":           cacheNoStore,
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": cspOther,
	} {
		if got := rec.Header().Get(h); got != want {
			t.Errorf("%s = %q, want %q", h, got, want)
		}
	}
	if sc := rec.Header().Values("Set-Cookie"); len(sc) != 0 {
		t.Errorf("GET /info set a cookie: %q", sc)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body %q: %v", rec.Body, err)
	}
	return got
}

func decodeJSONMap(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestInfoShape: GET /api/v1/info returns exactly the 03 §12.4.1 shape with fake InfoSource and Push, without
// authentication.
func TestInfoShape(t *testing.T) {
	f := newAPIFixture(t, nil)
	got := getInfoJSON(t, f)
	if want := decodeJSONMap(t, wantInfoJSON); !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.MarshalIndent(got, "", "  ")
		t.Fatalf("GET /info =\n%s\nwant\n%s", gotJSON, wantInfoJSON)
	}
	if calls := f.auth.take(); len(calls) != 1 || calls[0] != "csrf" {
		t.Fatalf("auth calls %v; want only the CSRF wrapper (Public)", calls)
	}

	// The body decodes into the DTO without unknown fields.
	rec := f.do(http.MethodGet, "/api/v1/info", nil)
	dec := json.NewDecoder(rec.Body)
	dec.DisallowUnknownFields()
	var info api.Info
	if err := dec.Decode(&info); err != nil {
		t.Fatal(err)
	}

	// HEAD answers the same status and headers.
	head := f.do(http.MethodHead, "/api/v1/info", nil)
	if head.Code != http.StatusOK || head.Header().Get("Cache-Control") != cacheNoStore {
		t.Fatalf("HEAD: status %d, Cache-Control %q", head.Code, head.Header().Get("Cache-Control"))
	}
}

// TestInfoWithoutPush: push is omitted, and push is not a feature, when Deps.Push is nil (push.enabled = false) or
// has no key.
func TestInfoWithoutPush(t *testing.T) {
	for name, p := range map[string]Push{"nil": nil, "no key": &fakePush{}} {
		f := newAPIFixture(t, func(d *Deps) { d.Push = p })
		got := getInfoJSON(t, f)
		want := decodeJSONMap(t, wantInfoJSON)
		delete(want, "push")
		want["features"] = []any{"passwordReset"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: GET /info = %v, want %v", name, got, want)
		}
	}
}

// TestInfoSetupRequired: setupRequired is true while no admin row exists, whatever its status (03 §7.8).
func TestInfoSetupRequired(t *testing.T) {
	f := newAPIFixture(t, nil)
	setupRequired := func() bool {
		t.Helper()
		v, ok := getInfoJSON(t, f)["setupRequired"].(bool)
		if !ok {
			t.Fatal("setupRequired is not a bool")
		}
		return v
	}
	create := func(u store.User) {
		t.Helper()
		err := f.db.Write(context.Background(), func(q *store.Q) error { return q.CreateUser(&u) })
		if err != nil {
			t.Fatal(err)
		}
	}
	if !setupRequired() {
		t.Fatal("a fresh DB does not require setup")
	}
	create(store.User{Username: "Sam", UsernameKey: "sam", PasswordHash: "x", Role: store.RoleUser, CreatedVia: "invite"})
	create(store.User{Username: "Kim", UsernameKey: "kim", PasswordHash: "x", Role: store.RoleUser,
		Status: store.StatusPending, CreatedVia: "signup"})
	if !setupRequired() {
		t.Fatal("users without an admin ended setup")
	}
	create(store.User{Username: "Alex", UsernameKey: "alex", PasswordHash: "x", Role: store.RoleAdmin,
		Status: store.StatusDisabled, CreatedVia: "setup"})
	if setupRequired() {
		t.Fatal("a disabled admin still counts as set up")
	}
}

// TestInfoDefaults: without an InfoSource the API reports this binary's version and protocol range; without a host
// name in the site, server.name falls back to the origin's host.
func TestInfoDefaults(t *testing.T) {
	f := newAPIFixture(t, func(d *Deps) {
		d.Info = nil
		d.Site = Site{Origin: "https://[2001:db8::1]:8443"}
	})
	got := getInfoJSON(t, f)
	server, _ := got["server"].(map[string]any)
	proto, _ := got["protocol"].(map[string]any)
	if server["version"] != version.Version() || server["name"] != "2001:db8::1" ||
		server["publicUrl"] != "https://[2001:db8::1]:8443" {
		t.Errorf("server = %v", server)
	}
	if proto["current"] != float64(protocol.Version) || proto["min"] != float64(protocol.MinVersion) {
		t.Errorf("protocol = %v, want {%d %d}", proto, protocol.Version, protocol.MinVersion)
	}
}

func TestServerName(t *testing.T) {
	for _, tc := range []struct {
		setting string
		site    Site
		want    string
	}{
		{"Alex's server", domainSite(), "Alex's server"},
		{"", domainSite(), testHost},
		{"", Site{Origin: "https://watch.example.com:8443", Hostname: "watch.example.com"}, "watch.example.com"},
		{"", Site{Origin: "https://203.0.113.7:8443"}, "203.0.113.7"},
		{"", Site{Origin: "http://localhost:5173"}, "localhost"},
		{"", Site{}, ""},
		{"", Site{Origin: "%zz"}, ""},
	} {
		if got := serverName(tc.setting, tc.site); got != tc.want {
			t.Errorf("serverName(%q, %+v) = %q, want %q", tc.setting, tc.site, got, tc.want)
		}
	}
}

// TestInfoStoreError: a failed read answers 500 internal with the request ID, and nothing else.
func TestInfoStoreError(t *testing.T) {
	f := newAPIFixture(t, nil)
	captureDefaultLog(t)
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	rec := f.do(http.MethodGet, "/api/v1/info", nil)
	e := wantError(t, rec, http.StatusInternalServerError, api.CodeInternal)
	if e.RequestID == "" || e.RequestID != rec.Header().Get("X-Request-Id") {
		t.Fatalf("requestId %q, X-Request-Id %q", e.RequestID, rec.Header().Get("X-Request-Id"))
	}
	var logged bool
	for _, r := range f.logs.records(t) {
		if r["msg"] == "http internal error" && r["route"] == "GET /api/v1/info" && r["request_id"] == e.RequestID {
			logged = true
		}
	}
	if !logged {
		t.Fatalf("the cause was not logged with the route and request ID: %s", f.logs.String())
	}
}
