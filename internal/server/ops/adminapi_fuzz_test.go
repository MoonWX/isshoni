package ops

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// FuzzAdminRequest sends arbitrary requests to the admin API's handler. Whatever arrives, the handler must not
// panic, and its answer is one of: a success (2xx), a redirect of the mux for a path that is not clean, or the
// socket's error document with the status api.StatusOf(code) (04 §12.1).
func FuzzAdminRequest(f *testing.F) {
	for _, seed := range [][3]string{
		{"GET", "/v1/health", ""},
		{"HEAD", "/v1/ready", ""},
		{"GET", "/v1/status", ""},
		{"POST", "/v1/setup-url", `{"waitReadyS": 0}`},
		{"POST", "/v1/setup-url", `{"waitReadyS": -1}`},
		{"GET", "/v1/users", ""},
		{"POST", "/v1/users/alex/reset-link", ""},
		{"POST", "/v1/users/alex/role", `{"role":"admin"}`},
		{"POST", "/v1/users/a%2Fb/role", `{"role":"owner"}`},
		{"POST", "/v1/users/alex/disable", ""},
		{"POST", "/v1/users/nobody/enable", ""},
		{"POST", "/v1/invites", `{"uses":10,"ttl":"168h"}`},
		{"POST", "/v1/invites", `{"uses":1e9,"ttl":"1h1ns"}`},
		{"POST", "/v1/log-level", `{"level":"debug","for":"30m"}`},
		{"POST", "/v1/log-level", `{"level":"debug","for":"9223372036854775807ns"}`},
		{"GET", "/v1/backup?certs=1", ""},
		{"POST", "/v1/restore", "\x1f\x8b\x08"},
		{"POST", "/v1/rotate-secrets", `{"keys":["session"],"vapid":true}`},
		{"POST", "/v1/doctor", `{"only":["dns"]}`},
		{"DELETE", "/v1/users/alex", ""},
		{"GET", "/v1//users/../health", ""},
		{"POST", "/v1/invites", `{"ttl":"1h"} trailing`},
		{"POST", "/v1/invites", `[1,2,3]`},
		{"POST", "/v1/invites", strings.Repeat("[", 10000)},
	} {
		f.Add(seed[0], seed[1], seed[2])
	}
	log := slog.New(slog.DiscardHandler)
	f.Fuzz(func(t *testing.T, method, path, body string) {
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		r, err := http.NewRequestWithContext(context.Background(), method, "http://isshoni"+path, strings.NewReader(body))
		if err != nil {
			t.Skip() // not a request net/http would hand to a handler
		}
		accounts := newFakeAccounts()
		accounts.users = someUsers()
		accounts.admins["alex"] = true
		logLevel := NewLogLevel(new(slog.LevelVar), log)
		defer logLevel.Close()
		srv := NewAdminServer(AdminOptions{Health: NewHealth(), Accounts: accounts, LogLevel: logLevel, Logger: log})

		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		switch code := w.Code; {
		case code >= 200 && code <= 299:
		case code == http.StatusMovedPermanently || code == http.StatusTemporaryRedirect || code == http.StatusPermanentRedirect:
			// ServeMux sends a path that is not clean to its clean form.
		default:
			var doc AdminErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil || doc.Error.Code == "" {
				t.Fatalf("%s %q: HTTP %d with a body that is not the error document: %q", method, path, code, w.Body.String())
			}
			if want := api.StatusOf(doc.Error.Code); code != want {
				t.Fatalf("%s %q: %s came with HTTP %d, want %d", method, path, doc.Error.Code, code, want)
			}
			if doc.Message == "" {
				t.Fatalf("%s %q: %s without a message for the CLI", method, path, doc.Error.Code)
			}
		}
	})
}
