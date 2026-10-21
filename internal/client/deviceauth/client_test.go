package deviceauth_test

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/MoonWX/isshoni/internal/client/deeplink"
	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// Every request carries the app's User-Agent and JSON content type, and nothing a browser would add.
func TestClientHeaders(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fs := newFakeServer(t)
		c := fs.client("")
		ctx := t.Context()
		if _, err := c.Info(ctx); err != nil {
			t.Fatalf("Info: %v", err)
		}
		if _, err := c.StartLink(ctx); err != nil {
			t.Fatalf("StartLink: %v", err)
		}
		_, err := c.Poll(ctx, b64("device-code"))
		if !isAPI(err, api.CodeAuthorizationPending) {
			t.Fatalf("Poll: %s, want authorization_pending", describe(err))
		}
		if _, err := c.Refresh(ctx, seedToken, "q9V3xT0bLm2Rk7aZ4cWfYg"); err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		must(t, c.Revoke(ctx, accessToken(1)))
		reqs := fs.seen("")
		if len(reqs) != 5 {
			t.Fatalf("%d requests, want 5", len(reqs))
		}
		for _, r := range reqs {
			if ua := r.Header.Get("User-Agent"); ua != "isshoni-desktop/"+testVersion {
				t.Errorf("%s: User-Agent %q", r.Path, ua)
			}
			if ct := r.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("%s: Content-Type %q", r.Path, ct)
			}
			for _, h := range []string{"Origin", "Cookie", "Referer"} {
				if v := r.Header.Get(h); v != "" {
					t.Errorf("%s: %s %q", r.Path, h, v)
				}
			}
		}
		if got := reqs[4].Header.Get("Authorization"); got != "Bearer "+accessToken(1) {
			t.Errorf("revoke Authorization = %q", got)
		}
		for _, r := range reqs[:4] {
			if r.Header.Get("Authorization") != "" {
				t.Errorf("%s sent an Authorization header", r.Path)
			}
		}
	})
}

// The bodies of the device routes: the app's kind, OS, version and device name; the refresh grant's exchange id
// and app version; the password link's credentials.
func TestClientBodies(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fs := newFakeServer(t)
		c := fs.client("")
		ctx := t.Context()
		_, err := c.StartLink(ctx)
		must(t, err)
		var code api.DeviceCodeRequest
		must(t, json.Unmarshal(fs.seen("/api/v1/device/code")[0].Body, &code))
		want := api.DeviceCodeRequest{ClientKind: "desktop", DeviceName: testApp.DeviceName, OS: "windows",
			AppVersion: testVersion}
		if code != want {
			t.Errorf("device/code body = %+v, want %+v", code, want)
		}

		_, err = c.PasswordLink(ctx, "Alex", "correct horse")
		must(t, err)
		var pw api.DevicePasswordRequest
		must(t, json.Unmarshal(fs.seen("/api/v1/device/password")[0].Body, &pw))
		wantPW := api.DevicePasswordRequest{Username: "Alex", Password: "correct horse", ClientKind: "desktop",
			DeviceName: testApp.DeviceName, OS: "windows", AppVersion: testVersion}
		if pw != wantPW {
			t.Errorf("device/password body = %+v, want %+v", pw, wantPW)
		}

		_, err = c.Refresh(ctx, seedToken, "q9V3xT0bLm2Rk7aZ4cWfYg")
		must(t, err)
		ref := fs.refreshes()[0]
		wantRef := api.DeviceTokenRequest{GrantType: api.GrantTypeRefreshToken, RefreshToken: seedToken,
			ExchangeID: "q9V3xT0bLm2Rk7aZ4cWfYg", AppVersion: testVersion}
		if ref != wantRef {
			t.Errorf("refresh body = %+v, want %+v", ref, wantRef)
		}
	})
}

// An app version the server's rule refuses isn't sent with a refresh: it would turn the refresh into a 422.
func TestClientRefreshOmitsInvalidAppVersion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fs := newFakeServer(t)
		app := testApp
		app.Version = "0.0.0-dev+0123456789abcdef0123456789abcdef" // 41 bytes
		c, err := deviceauth.NewClient(testOrigin, app, deviceauth.ClientOptions{HTTP: fs.httpClient(),
			AllowInsecureLoopback: true})
		must(t, err)
		_, err = c.Refresh(t.Context(), seedToken, "")
		must(t, err)
		if got := fs.refreshes()[0]; got.AppVersion != "" || got.ExchangeID != "" {
			t.Errorf("refresh sent appVersion %q, exchangeId %q; want neither", got.AppVersion, got.ExchangeID)
		}
	})
}

// Error answers: the envelope as *api.Error (Retry-After from the header when the body has none), anything else
// as ErrBadResponse, transport failures as ErrNetwork.
func TestClientErrors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fs := newFakeServer(t)
		c := fs.client("")
		ctx := t.Context()

		fs.with(func(fs *fakeServer) {
			r := apiErr(api.CodeRateLimited, nil)
			r.header = http.Header{"Retry-After": {"7"}}
			fs.override = map[string]reply{"/api/v1/device/code": r}
		})
		_, err := c.StartLink(ctx)
		var e *api.Error
		if !errors.As(err, &e) || e.Code != api.CodeRateLimited || e.RetryAfter != 7 {
			t.Fatalf("StartLink: %s, want rate_limited with RetryAfter 7", describe(err))
		}

		for name, r := range map[string]reply{
			"html 502":        {status: http.StatusBadGateway, body: []byte("<html>bad gateway</html>")},
			"broken 200":      {status: http.StatusOK, body: []byte("{")},
			"over 64 KiB":     {status: http.StatusOK, body: []byte(`{"name":"` + strings.Repeat("x", 64<<10) + `"}`)},
			"empty envelope":  {status: http.StatusBadRequest, body: []byte(`{"error":{}}`)},
			"redirect answer": {status: http.StatusTemporaryRedirect, header: http.Header{"Location": {"/x"}}},
		} {
			fs.with(func(fs *fakeServer) { fs.override = map[string]reply{"/api/v1/info": r} })
			if _, err := c.Info(ctx); !errors.Is(err, deviceauth.ErrBadResponse) {
				t.Errorf("%s: Info = %s, want ErrBadResponse", name, describe(err))
			}
		}

		fs.ln.down.Store(true)
		_, err = c.Info(ctx)
		var opErr interface{ Timeout() bool }
		if !errors.Is(err, deviceauth.ErrNetwork) || !errors.As(err, &opErr) {
			t.Fatalf("Info while down = %s, want ErrNetwork wrapping the dial error", describe(err))
		}
	})
}

// A token pair of the wrong shape is never handed on.
func TestClientChecksTokenShapes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fs := newFakeServer(t)
		c := fs.client("")
		for name, mutate := range map[string]func(*api.DeviceTokenResponse){
			"access token":  func(r *api.DeviceTokenResponse) { r.AccessToken = "isa_short" },
			"refresh token": func(r *api.DeviceTokenResponse) { r.RefreshToken = "isa_" + b64("x") },
			"no device":     func(r *api.DeviceTokenResponse) { r.Device.ID = "" },
			"no lifetime":   func(r *api.DeviceTokenResponse) { r.ExpiresIn = 0 },
		} {
			fs.with(func(fs *fakeServer) {
				fs.refresh = func(api.DeviceTokenRequest, int) reply {
					p := fs.pairLocked()
					mutate(&p)
					return ok(p)
				}
			})
			if _, err := c.Refresh(t.Context(), seedToken, ""); !errors.Is(err, deviceauth.ErrBadResponse) {
				t.Errorf("%s: Refresh = %s, want ErrBadResponse", name, describe(err))
			}
		}
	})
}

// No redirect is followed, also with a caller's http.Client that would follow one: a 307/308 would re-send a
// token body to another location.
func TestClientFollowsNoRedirect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fs := newFakeServer(t)
		fs.with(func(fs *fakeServer) {
			fs.override = map[string]reply{
				"/api/v1/info":         {status: http.StatusTemporaryRedirect, header: http.Header{"Location": {"/api/v1/elsewhere"}}},
				"/api/v1/device/token": {status: http.StatusPermanentRedirect, header: http.Header{"Location": {"http://127.0.0.1:9999/steal"}}},
			}
		})
		following := &http.Client{Transport: fs.tr} // CheckRedirect nil: net/http would follow
		for name, c := range map[string]*deviceauth.Client{
			"default client": fs.client(""),
			"caller's client": func() *deviceauth.Client {
				c, err := deviceauth.NewClient(testOrigin, testApp, deviceauth.ClientOptions{HTTP: following,
					AllowInsecureLoopback: true})
				must(t, err)
				return c
			}(),
		} {
			if _, err := c.Info(t.Context()); !errors.Is(err, deviceauth.ErrBadResponse) {
				t.Errorf("%s: Info = %s, want ErrBadResponse", name, describe(err))
			}
			if _, err := c.Refresh(t.Context(), seedToken, "q9V3xT0bLm2Rk7aZ4cWfYg"); !errors.Is(err, deviceauth.ErrBadResponse) {
				t.Errorf("%s: Refresh = %s, want ErrBadResponse", name, describe(err))
			}
		}
		if following.CheckRedirect != nil {
			t.Error("NewClient changed the caller's http.Client")
		}
		for _, r := range fs.seen("") {
			if r.Path != "/api/v1/info" && r.Path != "/api/v1/device/token" {
				t.Errorf("a redirect was followed to %s", r.Path)
			}
		}
	})
}

// A TLS failure is ErrNetwork, and the certificate error stays reachable for 03-desktop-app's tls_untrusted.
func TestClientTLSFailure(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the handshake error the test causes
	srv.StartTLS()
	defer srv.Close()
	c, err := deviceauth.NewClient(srv.URL, testApp, deviceauth.ClientOptions{})
	must(t, err)
	_, err = c.Info(t.Context())
	var cv *tls.CertificateVerificationError
	if !errors.Is(err, deviceauth.ErrNetwork) || !errors.As(err, &cv) {
		t.Fatalf("Info = %s, want ErrNetwork with a *tls.CertificateVerificationError", describe(err))
	}
}

// NewClient refuses an origin NormalizeOrigin refuses, and normalizes the others.
func TestNewClientOrigin(t *testing.T) {
	for _, bad := range []string{"", "ftp://watch.example.com", "http://watch.example.com", "https://x.example/path"} {
		if _, err := deviceauth.NewClient(bad, testApp, deviceauth.ClientOptions{}); !errors.Is(err, deeplink.ErrInvalidOrigin) {
			t.Errorf("NewClient(%q) = %s, want ErrInvalidOrigin", bad, describe(err))
		}
	}
	if _, err := deviceauth.NewClient("http://localhost:8080", testApp, deviceauth.ClientOptions{}); err == nil {
		t.Error("NewClient accepted http://localhost without AllowInsecureLoopback")
	}
	if _, err := deviceauth.NewClient("WATCH.example.com:443", testApp, deviceauth.ClientOptions{}); err != nil {
		t.Errorf("NewClient(WATCH.example.com:443): %v", err)
	}
}

// DefaultHTTPClient: 15 s, TLS 1.2 or later, no redirects, the environment's proxy.
func TestDefaultHTTPClient(t *testing.T) {
	hc := deviceauth.DefaultHTTPClient()
	if hc.Timeout.Seconds() != 15 {
		t.Errorf("Timeout = %v", hc.Timeout)
	}
	if hc.CheckRedirect == nil || !errors.Is(hc.CheckRedirect(nil, nil), http.ErrUseLastResponse) {
		t.Error("CheckRedirect doesn't stop redirects")
	}
	tr, isTransport := hc.Transport.(*http.Transport)
	if !isTransport || tr.Proxy == nil || tr.TLSClientConfig == nil || tr.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("Transport = %#v", hc.Transport)
	}
	if deviceauth.DefaultHTTPClient().Transport == hc.Transport {
		t.Error("two calls share a Transport")
	}
}

// StartLink refuses verification URIs that aren't the server's own /link page (02 §11.2), and hands out Go's
// canonical strings for the ones it accepts.
func TestStartLinkVerificationURIs(t *testing.T) {
	const code = "WDJB-MJHT"
	good := func(uri string) func(string) (string, string) {
		return func(string) (string, string) { return testOrigin + "/link", uri }
	}
	plain := func(uri string) func(string) (string, string) {
		return func(string) (string, string) { return uri, testOrigin + "/link?code=" + code }
	}
	bad := map[string]func(string) (string, string){
		"file":            good("file:///C:/x"),
		"ms-settings":     good("ms-settings:display"),
		"search-ms":       good("search-ms:query=x"),
		"javascript":      good("javascript:alert(1)"),
		"unc":             good(`\\host\share\x`),
		"file unc":        good("file://host/share/x"),
		"other host":      good("http://127.0.0.2:8443/link?code=" + code),
		"other port":      good("http://127.0.0.1:8444/link?code=" + code),
		"https":           good("https://127.0.0.1:8443/link?code=" + code),
		"non-loopback":    good("http://watch.example.com/link?code=" + code),
		"userinfo":        good("http://user@127.0.0.1:8443/link?code=" + code),
		"fragment":        good(testOrigin + "/link?code=" + code + "#x"),
		"empty fragment":  good(testOrigin + "/link?code=" + code + "#"),
		"trailing slash":  good(testOrigin + "/link/?code=" + code),
		"upper-case path": good(testOrigin + "/Link?code=" + code),
		"encoded path":    good(testOrigin + "/%6Cink?code=" + code),
		"extra query":     good(testOrigin + "/link?code=" + code + "&next=https://evil.example"),
		"other code":      good(testOrigin + "/link?code=WDJB-MJHB"),
		"unformatted":     good(testOrigin + "/link?code=WDJBMJHT"),
		"plain query":     plain(testOrigin + "/link?code=" + code),
		"plain empty ?":   plain(testOrigin + "/link?"),
		"plain other":     plain("https://evil.example/link"),
	}
	for name, uris := range bad {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t, false)
				e.fs.with(func(fs *fakeServer) { fs.codeURIs = uris })
				if _, err := e.fs.client("").StartLink(t.Context()); !errors.Is(err, deviceauth.ErrBadResponse) {
					t.Fatalf("StartLink = %s, want ErrBadResponse", describe(err))
				}
				if err := e.m.Link(t.Context()); !errors.Is(err, deviceauth.ErrBadResponse) {
					t.Fatalf("Link = %s, want ErrBadResponse", describe(err))
				}
				s := e.m.State()
				if s.Status != deviceauth.StatusUnlinked || s.Link != nil {
					t.Fatalf("state after a refused code = %s with link %v", s.Status, s.Link)
				}
			})
		})
	}

	// Accepted spellings: the components normalize to the client's origin. LinkState then holds Go's strings.
	for name, tc := range map[string]struct{ origin, uri, complete string }{
		"upper-case host": {"http://localhost:8443", "http://LOCALHOST:8443/link", "http://LocalHost:8443/link?code=" + code},
		"default port":    {"http://localhost", "http://localhost:80/link", "HTTP://localhost:80/link?code=" + code},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fs := newFakeServer(t)
				fs.with(func(fs *fakeServer) {
					fs.codeURIs = func(string) (string, string) { return tc.uri, tc.complete }
				})
				c := fs.client(tc.origin)
				resp, err := c.StartLink(t.Context())
				must(t, err)
				if resp.VerificationURI != tc.origin+"/link" || resp.VerificationURIComplete != tc.origin+"/link?code="+code {
					t.Fatalf("URIs = %q, %q; want Go's canonical strings", resp.VerificationURI, resp.VerificationURIComplete)
				}
				m := deviceauth.NewManager(deviceauth.ManagerOptions{Client: c, Store: newMemoryStore(), Logger: quiet})
				defer func() { _ = m.Close() }()
				must(t, m.Link(t.Context()))
				l := m.State().Link
				if l == nil || l.UserCode != code || l.VerificationURI != tc.origin+"/link" ||
					l.VerificationURIComplete != tc.origin+"/link?code="+code {
					t.Fatalf("LinkState = %+v", l)
				}
			})
		})
	}
}

// A user code that doesn't normalize, a device code of the wrong shape and a non-positive interval or lifetime are
// refused too.
func TestStartLinkChecksTheCode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fs := newFakeServer(t)
		c := fs.client("")
		for name, mutate := range map[string]func(*api.DeviceCodeResponse){
			"vowel":       func(r *api.DeviceCodeResponse) { r.UserCode = "WDJB-MJHA" },
			"device code": func(r *api.DeviceCodeResponse) { r.DeviceCode = "short" },
			"interval":    func(r *api.DeviceCodeResponse) { r.Interval = 0 },
			"expiresIn":   func(r *api.DeviceCodeResponse) { r.ExpiresIn = -1 },
		} {
			fs.with(func(fs *fakeServer) {
				r := fs.codeResponseLocked()
				mutate(&r)
				fs.override = map[string]reply{"/api/v1/device/code": ok(r)}
			})
			if _, err := c.StartLink(t.Context()); !errors.Is(err, deviceauth.ErrBadResponse) {
				t.Errorf("%s: StartLink = %s, want ErrBadResponse", name, describe(err))
			}
		}
	})
}

func isAPI(err error, code string) bool {
	var e *api.Error
	return errors.As(err, &e) && e.Code == code
}
