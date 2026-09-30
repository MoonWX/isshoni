package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// TestWriteErrorEnvelope checks that WriteError emits 03's envelope (03 §12.2, 04 §9.4) byte for byte.
func TestWriteErrorEnvelope(t *testing.T) {
	logs := captureDefaultLog(t)
	tests := []struct {
		name       string
		err        error
		status     int
		body       string
		retryAfter string
	}{
		{"validation", &api.Error{Code: api.CodeValidationFailed, Fields: map[string]string{"username": api.FieldInvalid, "password": api.FieldTooCommon}},
			422, `{"error":{"code":"validation_failed","fields":{"password":"too_common","username":"invalid"}}}`, ""},
		{"rate limited", &api.Error{Code: api.CodeRateLimited, RetryAfter: 42},
			429, `{"error":{"code":"rate_limited","retryAfter":42}}`, "42"},
		{"params", &api.Error{Code: api.CodePushEndpointRejected, Params: map[string]any{api.ParamReason: api.PushRejectReasonPrivateAddress}},
			422, `{"error":{"code":"push_endpoint_rejected","params":{"reason":"private_address"}}}`, ""},
		{"wrapped", fmt.Errorf("rooms: %w", api.NewError(api.CodeRoomNotFound)),
			404, `{"error":{"code":"room_not_found"}}`, ""},
		{"body too large", fmt.Errorf("read: %w", &http.MaxBytesError{Limit: 10}),
			413, `{"error":{"code":"payload_too_large"}}`, ""},
		{"shutdown", &api.Error{Code: api.CodeServerShutdown, RetryAfter: 5},
			503, `{"error":{"code":"server_shutdown","retryAfter":5}}`, "5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			WriteError(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil), tt.err)
			if rec.Code != tt.status || rec.Body.String() != tt.body+"\n" {
				t.Fatalf("got %d %s, want %d %s", rec.Code, rec.Body, tt.status, tt.body)
			}
			h := rec.Header()
			if h.Get("Content-Type") != jsonContentType || h.Get("Cache-Control") != cacheNoStore ||
				h.Get("Retry-After") != tt.retryAfter || h.Get("Content-Length") != fmt.Sprint(len(tt.body)+1) {
				t.Fatalf("headers %v", h)
			}
		})
	}
	if logs.String() != "" {
		t.Errorf("client errors were logged:\n%s", logs)
	}
}

// TestWriteErrorEveryCode sends every code of the table and checks api.StatusOf is used.
func TestWriteErrorEveryCode(t *testing.T) {
	logs := captureDefaultLog(t)
	for _, code := range api.Codes() {
		rec := httptest.NewRecorder()
		WriteError(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil), api.NewError(code))
		e := wantError(t, rec, api.StatusOf(code), code)
		if (rec.Code == http.StatusInternalServerError) != (e.RequestID != "") {
			t.Errorf("%s: requestId %q with status %d", code, e.RequestID, rec.Code)
		}
		if e.RequestID != "" && !requestIDPattern.MatchString(e.RequestID) {
			t.Errorf("%s: requestId %q outside the router", code, e.RequestID)
		}
	}
	// Outside the router only the one 500 code (internal) is logged, to slog.Default, with its request ID.
	if recs := logs.records(t); len(recs) != 1 || recs[0]["level"] != "ERROR" || recs[0]["request_id"] == "" {
		t.Errorf("logs:\n%s", logs)
	}
}

// TestWriteErrorInternal: any other error is 500 internal with the request ID, logged with route and ID, and its
// text never reaches the client.
func TestWriteErrorInternal(t *testing.T) {
	f := newFixture(t, nil)
	f.rt.HandleFunc("GET /api-ish/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", "GET")
		WriteError(w, r, errors.New("store: disk I/O error at /secret/path"))
	})
	rec := f.get("/api-ish/7?token=abc")
	e := wantError(t, rec, http.StatusInternalServerError, api.CodeInternal)
	if e.RequestID != rec.Header().Get("X-Request-Id") || !requestIDPattern.MatchString(e.RequestID) {
		t.Fatalf("requestId %q, X-Request-Id %q", e.RequestID, rec.Header().Get("X-Request-Id"))
	}
	if strings.Contains(rec.Body.String(), "disk") || rec.Header().Get("Allow") != "GET" {
		t.Fatalf("body %q, Allow %q", rec.Body, rec.Header().Get("Allow"))
	}
	logs := f.logs.records(t)
	if len(logs) != 1 {
		t.Fatalf("want one log line (the cause, not a second 5xx line), got:\n%s", f.logs)
	}
	l := logs[0]
	if l["level"] != "ERROR" || l["route"] != "GET /api-ish/{id}" || l["request_id"] != e.RequestID ||
		l["err"] != "store: disk I/O error at /secret/path" {
		t.Fatalf("log = %v", l)
	}
	if strings.Contains(f.logs.String(), "token=abc") {
		t.Fatal("the query was logged")
	}
}

func TestWriteErrorEdgeCases(t *testing.T) {
	captureDefaultLog(t)
	t.Run("an internal api.Error drops its details", func(t *testing.T) {
		rec := httptest.NewRecorder()
		WriteError(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil),
			&api.Error{Code: api.CodeInternal, Params: map[string]any{"query": "SELECT"}, RetryAfter: 3})
		e := wantError(t, rec, 500, api.CodeInternal)
		if e.Params != nil || e.RetryAfter != 0 || rec.Header().Get("Retry-After") != "" || !requestIDPattern.MatchString(e.RequestID) {
			t.Fatalf("envelope %+v, headers %v", e, rec.Header())
		}
	})
	t.Run("params that don't encode become internal", func(t *testing.T) {
		rec := httptest.NewRecorder()
		WriteError(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil),
			&api.Error{Code: api.CodeLimitReached, Params: map[string]any{"limit": func() {}}})
		wantError(t, rec, 500, api.CodeInternal)
	})
	t.Run("nil error and nil *api.Error", func(t *testing.T) {
		for _, err := range []error{nil, (*api.Error)(nil)} {
			rec := httptest.NewRecorder()
			WriteError(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil), err)
			wantError(t, rec, 500, api.CodeInternal)
		}
	})
	t.Run("stale headers are removed", func(t *testing.T) {
		rec := httptest.NewRecorder()
		rec.Header().Set("Content-Encoding", "br")
		rec.Header().Set("ETag", `"abc"`)
		rec.Header().Set("Cache-Control", cacheImmutable)
		WriteError(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil), api.NewError(api.CodeNotFound))
		if h := rec.Header(); h.Get("Content-Encoding") != "" || h.Get("ETag") != "" || h.Get("Cache-Control") != cacheNoStore {
			t.Fatalf("headers %v", h)
		}
	})
	t.Run("a client that went away is logged at debug", func(t *testing.T) {
		logs := &syncBuffer{}
		f := newFixture(t, func(o *RouterOptions) {
			o.Logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		})
		f.rt.HandleFunc("GET /slow", func(w http.ResponseWriter, r *http.Request) { WriteError(w, r, r.Context().Err()) })
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		f.serve(newReq(http.MethodGet, "/slow").WithContext(ctx))
		if recs := logs.records(t); len(recs) != 1 || recs[0]["level"] != "DEBUG" {
			t.Fatalf("logs:\n%s", logs)
		}
	})
}

func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, http.StatusCreated, map[string]any{"name": "<Lounge>", "n": 1})
	esc := func(hex string) string { return `\` + "u00" + hex } // HTML characters are escaped
	if rec.Code != 201 || rec.Header().Get("Content-Type") != jsonContentType ||
		rec.Body.String() != `{"n":1,"name":"`+esc("3c")+"Lounge"+esc("3e")+`"}`+"\n" ||
		rec.Header().Get("Content-Length") != fmt.Sprint(rec.Body.Len()) {
		t.Fatalf("got %d %v %q", rec.Code, rec.Header(), rec.Body)
	}
	rec = httptest.NewRecorder()
	WriteJSON(rec, http.StatusNoContent, map[string]int{"ignored": 1})
	if rec.Code != 204 || rec.Body.Len() != 0 || rec.Header().Get("Content-Type") != "" {
		t.Fatalf("204: got %v %q", rec.Header(), rec.Body)
	}

	// An unencodable value is a bug: the recover step turns the panic into 500 internal.
	f := newFixture(t, nil)
	f.rt.HandleFunc("GET /bad", func(w http.ResponseWriter, _ *http.Request) { WriteJSON(w, 200, map[string]any{"f": func() {}}) })
	wantError(t, f.get("/bad"), 500, api.CodeInternal)
	if logs := f.logs.records(t); len(logs) != 1 || !strings.Contains(fmt.Sprint(logs[0]["panic"]), "httpapi.WriteJSON") {
		t.Fatalf("logs:\n%s", f.logs)
	}
}

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func jsonReq(body string, contentType string) *http.Request {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/auth/login", strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req
}

func TestDecodeJSON(t *testing.T) {
	const ct = "application/json"
	big := `{"username":"` + strings.Repeat("a", 100) + `"}`
	tests := []struct {
		name     string
		body     string
		ct       string
		max      int64
		chunked  bool
		wantCode string // "" = success
	}{
		{"ok", `{"username":"alex","password":"pw"}`, ct, 0, false, ""},
		{"charset utf-8", `{"username":"alex"}`, "application/json; charset=UTF-8", 0, false, ""},
		{"case-insensitive type", `{"username":"alex"}`, "Application/JSON", 0, false, ""},
		{"unknown fields are ignored", `{"username":"alex","extra":[1,2]}`, ct, 0, false, ""},
		{"trailing white space", "{\"username\":\"alex\"} \n\t ", ct, 0, false, ""},
		{"null", `null`, ct, 0, false, ""},
		{"exactly at the limit", big, ct, int64(len(big)), false, ""},
		{"no Content-Type", `{}`, "", 0, false, api.CodeUnsupportedMediaType},
		{"text/plain", `{}`, "text/plain", 0, false, api.CodeUnsupportedMediaType},
		{"form", `username=alex`, "application/x-www-form-urlencoded", 0, false, api.CodeUnsupportedMediaType},
		{"other charset", `{}`, "application/json; charset=latin1", 0, false, api.CodeUnsupportedMediaType},
		{"json suffix type", `{}`, "application/merge-patch+json", 0, false, api.CodeUnsupportedMediaType},
		{"malformed type", `{}`, "application/json; charset", 0, false, api.CodeUnsupportedMediaType},
		{"over the limit by Content-Length", big, ct, int64(len(big)) - 1, false, api.CodePayloadTooLarge},
		{"over the limit while reading", big, ct, int64(len(big)) - 1, true, api.CodePayloadTooLarge},
		{"trailing space over the limit", big + strings.Repeat(" ", 10), ct, int64(len(big)) + 5, true, api.CodePayloadTooLarge},
		{"default limit is 64 KiB", `{"username":"` + strings.Repeat("a", 64<<10) + `"}`, ct, 0, true, api.CodePayloadTooLarge},
		{"empty body", ``, ct, 0, false, api.CodeBadRequest},
		{"malformed", `{"username":`, ct, 0, false, api.CodeBadRequest},
		{"wrong type", `{"username":42}`, ct, 0, false, api.CodeBadRequest},
		{"trailing object", `{"username":"a"}{"password":"b"}`, ct, 0, false, api.CodeBadRequest},
		{"trailing garbage", `{"username":"a"} x`, ct, 0, false, api.CodeBadRequest},
		{"trailing bracket", `{"username":"a"}]`, ct, 0, false, api.CodeBadRequest},
		{"array instead of object", `[1]`, ct, 0, false, api.CodeBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := jsonReq(tt.body, tt.ct)
			if tt.chunked {
				req.ContentLength = -1
				req.Body = io.NopCloser(strings.NewReader(tt.body))
			}
			var dst loginReq
			err := DecodeJSON(httptest.NewRecorder(), req, &dst, tt.max)
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if !api.IsCode(err, tt.wantCode) {
				t.Fatalf("err = %v, want %s", err, tt.wantCode)
			}
		})
	}

	var dst loginReq
	if err := DecodeJSON(httptest.NewRecorder(), jsonReq(`{"username":"alex","password":"pw"}`, ct), &dst, 0); err != nil ||
		dst != (loginReq{"alex", "pw"}) {
		t.Fatalf("decoded %+v, %v", dst, err)
	}

	// A dst that isn't a pointer is a programming error: not an *api.Error, so WriteError answers 500.
	err := DecodeJSON(httptest.NewRecorder(), jsonReq(`{}`, ct), loginReq{}, 0)
	var ae *api.Error
	if err == nil || errors.As(err, &ae) {
		t.Fatalf("non-pointer dst: err = %v", err)
	}
}

// FuzzDecodeJSON checks that any body gives nil or one of DecodeJSON's client errors.
func FuzzDecodeJSON(f *testing.F) {
	for _, s := range []string{`{}`, `{"a":1}`, `{"a":1} {"b":2}`, `[`, ``, `null`, `"x"`, `{"username":"a"}]`, "{}\x00"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, body string) {
		var dst map[string]any
		err := DecodeJSON(httptest.NewRecorder(), jsonReq(body, "application/json"), &dst, 256)
		if err != nil && !api.IsCode(err, api.CodeBadRequest) && !api.IsCode(err, api.CodePayloadTooLarge) {
			t.Fatalf("DecodeJSON(%q) = %v", body, err)
		}
	})
}
