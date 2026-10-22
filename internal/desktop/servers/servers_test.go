package servers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/client/compat"
	"github.com/MoonWX/isshoni/internal/client/deeplink"
	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
)

func TestNormalize(t *testing.T) {
	for _, tc := range []struct {
		in, origin, invite string
		loopback           bool
	}{
		{in: "watch.example.com", origin: "https://watch.example.com"},
		{in: "  Watch.Example.COM  ", origin: "https://watch.example.com"},
		{in: "https://watch.example.com", origin: "https://watch.example.com"},
		{in: "https://watch.example.com/", origin: "https://watch.example.com"},
		{in: "HTTPS://watch.example.com:443/r/lounge", origin: "https://watch.example.com"},
		{in: "watch.example.com:8443", origin: "https://watch.example.com:8443"},
		{in: "https://watch.example.com:8443/r/lounge?x=1#frag", origin: "https://watch.example.com:8443"},
		{in: "watch.example.com#only-a-fragment", origin: "https://watch.example.com"},
		{in: "watch.example.com?q", origin: "https://watch.example.com"},
		{in: "203.0.113.7", origin: "https://203.0.113.7"},
		{in: "203.0.113.7:9000/r/x", origin: "https://203.0.113.7:9000"},
		{in: "2001:db8::1", origin: "https://[2001:db8::1]"},
		{in: "https://[2001:db8::1]:8443/", origin: "https://[2001:db8::1]:8443"},
		{in: "bücher.example", origin: "https://xn--bcher-kva.example"},
		{in: "https://watch.example.com/invite#tok3n", origin: "https://watch.example.com",
			invite: "https://watch.example.com/invite#tok3n"},
		{in: "watch.example.com/invite/#tok", origin: "https://watch.example.com",
			invite: "https://watch.example.com/invite/#tok"},
		{in: "https://watch.example.com/setup?token=abc", origin: "https://watch.example.com",
			invite: "https://watch.example.com/setup?token=abc"},
		{in: "https://watch.example.com/%73etup", origin: "https://watch.example.com",
			invite: "https://watch.example.com/%73etup"},
		{in: "https://watch.example.com/invites", origin: "https://watch.example.com"}, // another page
		{in: "https://watch.example.com/r/invite", origin: "https://watch.example.com"},
		{in: "http://localhost:5173", origin: "http://localhost:5173", loopback: true},
		{in: "http://127.0.0.1:8080/r/lounge", origin: "http://127.0.0.1:8080", loopback: true},
		{in: "http://[::1]:8080", origin: "http://[::1]:8080", loopback: true},
	} {
		a, err := Normalize(tc.in, tc.loopback)
		if err != nil {
			t.Errorf("Normalize(%q): %v", tc.in, err)
			continue
		}
		if a.Origin != tc.origin || a.Invite != tc.invite {
			t.Errorf("Normalize(%q) = %+v, want origin %q invite %q", tc.in, a, tc.origin, tc.invite)
		}
	}
}

func TestNormalizeRejects(t *testing.T) {
	for _, tc := range []struct {
		in       string
		loopback bool
	}{
		{in: ""},
		{in: "   "},
		{in: "http://watch.example.com"}, // http only for loopback in dev builds
		{in: "http://localhost:5173"},    // a release build
		{in: "http://192.168.1.5", loopback: true},
		{in: "ftp://watch.example.com"},
		{in: "isshoni://watch.example.com/"},
		{in: "https://user:pw@watch.example.com"}, //nolint:gosec // G101: userinfo the address field must refuse
		{in: "https://watch example.com"},
		{in: "https://watch.example.com:99999"},
		{in: "https://0.0.0.0"},
		{in: "https://"},
		{in: "https://watch.example.com\u202e/x"},
		{in: strings.Repeat("a", 2049)},
	} {
		a, err := Normalize(tc.in, tc.loopback)
		if err == nil {
			t.Errorf("Normalize(%q) = %+v, want an error", tc.in, a)
			continue
		}
		if CodeOf(err) != CodeInvalidAddress {
			t.Errorf("Normalize(%q): code %q", tc.in, CodeOf(err))
		}
		if strings.Contains(err.Error(), "watch") || strings.Contains(err.Error(), "pw") {
			t.Errorf("the error names the address: %v", err)
		}
	}
}

// TestLocalID checks the vectors of 03 §8.1 that web/src/platform/desktop/testing/localIdVectors.ts holds for the
// TypeScript side.
func TestLocalID(t *testing.T) {
	want := map[string]string{
		"https://watch.example.com":          "j92qv4mz",
		"https://a.example.com":              "jfa4daer",
		"https://b.example.com":              "e9f3ch3x",
		"http://127.0.0.1:8080":              "tc55ev03",
		"https://xn--bcher-kva.example:8443": "cj3v686e",
	}
	for origin, id := range want {
		if got := LocalID(origin); got != id {
			t.Errorf("LocalID(%q) = %q, want %q", origin, got, id)
		}
	}
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "web", "src", "platform", "desktop", "testing",
		"localIdVectors.ts"))
	if err != nil {
		t.Fatal(err)
	}
	pairs := regexp.MustCompile(`\['([^']+)',\s*'([0-9a-z]{8})'\]`).FindAllStringSubmatch(string(src), -1)
	if len(pairs) != len(want) {
		t.Fatalf("localIdVectors.ts has %d pairs, this test %d: keep them the same", len(pairs), len(want))
	}
	for _, p := range pairs {
		if got := LocalID(p[1]); got != p[2] || want[p[1]] != p[2] {
			t.Errorf("TypeScript vector %s → %s; Go %s", p[1], p[2], got)
		}
	}
	if id := LocalID(""); len(id) != 8 || strings.ContainsAny(id, "ilou") {
		t.Errorf("LocalID(\"\") = %q", id)
	}
}

// server is an httptest TLS server whose /api/v1/info answers with info (or with handle when set).
type server struct {
	*httptest.Server
	hits    atomic.Int64
	cookies atomic.Int64
	ua      atomic.Value
}

func newServer(t *testing.T, handle http.HandlerFunc) *server {
	t.Helper()
	s := &server{}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		if len(r.Cookies()) > 0 {
			s.cookies.Add(1)
		}
		s.ua.Store(r.UserAgent())
		handle(w, r)
	}))
	s.Config.ErrorLog = log.New(io.Discard, "", 0) // the untrusted-certificate case's handshake error
	s.StartTLS()
	t.Cleanup(func() {
		s.Client().CloseIdleConnections()
		s.Close()
	})
	return s
}

func infoHandler(info api.Info) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/info" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(info)
	}
}

func goodInfo() api.Info {
	return api.Info{
		Server:   api.InfoServer{Name: "Lounge", Version: "0.2.0", PublicURL: "https://watch.example.com"},
		Protocol: api.InfoProtocol{Current: protocol.Version, Min: protocol.MinVersion},
		Features: []string{api.InfoFeatureDeviceFlow},
	}
}

var app = deviceauth.AppInfo{Version: "0.2.0", ClientKind: protocol.ClientKindDesktop, OS: protocol.ClientOSWindows}

func probe(t *testing.T, s *server, o ProbeOptions) (ServerProbe, error) {
	t.Helper()
	if o.HTTP == nil {
		o.HTTP = s.Client()
	}
	o.App = app
	return Probe(context.Background(), s.URL, o)
}

func wantCode(t *testing.T, err error, code Code) {
	t.Helper()
	_ = codeErr(t, err, code)
}

// codeErr is wantCode for a test that looks at the error's other fields too.
func codeErr(t *testing.T, err error, code Code) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("err = %v, want code %s", err, code)
	}
	return e
}

func TestProbeOK(t *testing.T) {
	s := newServer(t, infoHandler(goodInfo()))
	got, err := probe(t, s, ProbeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The server's own publicUrl is the origin, not what was typed (03 §8.1).
	if got != (ServerProbe{Origin: "https://watch.example.com", Name: "Lounge", Version: "0.2.0"}) {
		t.Errorf("probe = %+v", got)
	}
	if ua, _ := s.ua.Load().(string); !strings.HasPrefix(ua, "isshoni-desktop/0.2.0") {
		t.Errorf("User-Agent %q", ua)
	}
	if s.cookies.Load() != 0 {
		t.Error("the probe sent a cookie")
	}
	data, _ := json.Marshal(got)
	if string(data) != `{"origin":"https://watch.example.com","name":"Lounge","version":"0.2.0"}` {
		t.Errorf("JSON %s", data)
	}
}

func TestProbeUntrustedCertificate(t *testing.T) {
	s := newServer(t, infoHandler(goodInfo()))
	// The default client trusts the system roots only, so httptest's certificate fails.
	_, err := Probe(context.Background(), s.URL, ProbeOptions{App: app})
	wantCode(t, err, CodeTLSUntrusted)
}

func TestProbeRedirectNotFollowed(t *testing.T) {
	var target atomic.Int64
	other := newServer(t, func(w http.ResponseWriter, r *http.Request) { target.Add(1) })
	s := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/api/v1/info", http.StatusFound)
	})
	_, err := probe(t, s, ProbeOptions{})
	wantCode(t, err, CodeNotIsshoni)
	if target.Load() != 0 {
		t.Error("the redirect was followed")
	}
}

func TestProbeNotIsshoni(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"404": http.NotFound,
		"404 envelope": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found"}}`))
		},
		"500":           func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) },
		"html":          func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("<html>router login</html>")) },
		"empty {}":      func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) },
		"bad publicUrl": infoHandler(func() api.Info { i := goodInfo(); i.Server.PublicURL = "ftp://x"; return i }()),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := probe(t, newServer(t, h), ProbeOptions{})
			wantCode(t, err, CodeNotIsshoni)
		})
	}
}

func TestProbeSetupRequired(t *testing.T) {
	info := goodInfo()
	info.SetupRequired = true
	_, err := probe(t, newServer(t, infoHandler(info)), ProbeOptions{})
	wantCode(t, err, CodeServerNotSetUp)
}

func TestProbeIncompatible(t *testing.T) {
	old := goodInfo() // a 0.1.x server: no deviceFlow
	old.Features = []string{api.InfoFeaturePush}
	old.Server.Version = "0.1.4"
	newer := goodInfo()
	newer.Protocol = api.InfoProtocol{Current: protocol.Version + 2, Min: protocol.Version + 1}
	older := goodInfo()
	older.Protocol = api.InfoProtocol{Current: protocol.MinVersion - 1, Min: protocol.MinVersion - 1}
	floor := goodInfo()
	floor.MinClientVersion = "0.3.0"
	for name, tc := range map[string]struct {
		info    api.Info
		verdict compat.Verdict
		reason  compat.Reason
	}{
		"0.1.x":              {old, compat.UpdateServer, compat.ReasonNoDeviceFlow},
		"newer protocol":     {newer, compat.UpdateApp, compat.ReasonProtocolTooNew},
		"older protocol":     {older, compat.UpdateServer, compat.ReasonProtocolTooOld},
		"min client version": {floor, compat.UpdateApp, compat.ReasonMinClientVersion},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := probe(t, newServer(t, infoHandler(tc.info)), ProbeOptions{})
			e := codeErr(t, err, CodeIncompatibleServer)
			if e.Compat == nil || e.Compat.Verdict != tc.verdict || e.Compat.Reason != tc.reason ||
				e.Compat.ServerVersion != tc.info.Server.Version || e.Compat.MinClientVersion != tc.info.MinClientVersion {
				t.Errorf("compat = %+v", e.Compat)
			}
		})
	}
}

func TestProbeUnreachable(t *testing.T) {
	l, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	_, err = Probe(context.Background(), "https://"+addr, ProbeOptions{App: app})
	wantCode(t, err, CodeServerUnreachable)

	// The probe's own deadline is server_unreachable too; the caller's ctx ending is the caller's error.
	if CodeOf(classify(context.DeadlineExceeded)) != CodeServerUnreachable {
		t.Error("a deadline isn't server_unreachable")
	}
	hang := make(chan struct{})
	s := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-hang:
		case <-r.Context().Done():
		}
	})
	defer close(hang)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = Probe(ctx, s.URL, ProbeOptions{App: app, HTTP: s.Client()})
	if !errors.Is(err, context.DeadlineExceeded) || CodeOf(err) != "" {
		t.Errorf("the caller's deadline: %v", err)
	}
}

func TestProbeInvalidAddress(t *testing.T) {
	_, err := Probe(context.Background(), "http://watch.example.com", ProbeOptions{App: app})
	e := codeErr(t, err, CodeInvalidAddress)
	if !errors.Is(e, deeplink.ErrInvalidOrigin) {
		t.Errorf("cause %v", e.Err)
	}
	if (&Error{Code: CodeServerNotSetUp}).Error() != "servers: server_not_set_up" {
		t.Error("Error()")
	}
	if CodeOf(errors.New("x")) != "" {
		t.Error("CodeOf of another error")
	}
}

func TestProbeHTTPClient(t *testing.T) {
	hc := probeHTTPClient()
	if hc.Timeout != ProbeTimeout {
		t.Errorf("timeout %v", hc.Timeout)
	}
	if hc.CheckRedirect == nil {
		t.Error("redirects would be followed")
	}
	if tr, ok := hc.Transport.(*http.Transport); !ok || tr.DialContext == nil || tr.TLSClientConfig == nil {
		t.Errorf("transport %#v", hc.Transport)
	}
}
