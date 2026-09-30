package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// defaultMaxJSONBytes is DecodeJSON's body limit when the caller passes maxBytes ≤ 0 (04 §7.8). 03's body-limit
// step sets 16 KiB for auth endpoints and 64 KiB elsewhere (03 §12.1).
const defaultMaxJSONBytes = 64 << 10

// jsonContentType is the Content-Type of every JSON response (03 §12.1).
const jsonContentType = "application/json; charset=utf-8"

// WriteJSON writes v as JSON with the given status and Content-Type: application/json; charset=utf-8. A 204 or 304
// has no body. A value that cannot be encoded is a programming error: WriteJSON panics before writing anything, and
// the router's recover step answers 500 internal and logs the stack with the route.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	if !bodyAllowed(status) {
		w.WriteHeader(status)
		return
	}
	b, err := encodeJSON(v)
	if err != nil {
		panic(fmt.Errorf("httpapi.WriteJSON: %w", err))
	}
	writeJSONBytes(w, status, b)
}

func encodeJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := json.NewEncoder(&b).Encode(v); err != nil { // HTML-escaped, with a trailing newline
		return nil, err
	}
	return b.Bytes(), nil
}

func writeJSONBytes(w http.ResponseWriter, status int, b []byte) {
	h := w.Header()
	h.Set("Content-Type", jsonContentType)
	h.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b) // a failed write means the client is gone; there is no one left to tell
}

func bodyAllowed(status int) bool {
	return status >= 200 && status != http.StatusNoContent && status != http.StatusNotModified
}

// DecodeJSON decodes the request body into dst (03 §12.1). It checks, in order:
//   - Content-Type is application/json (parameters allowed; a charset other than utf-8 is refused) → otherwise
//     *api.Error unsupported_media_type;
//   - the body is at most maxBytes (≤ 0 means 64 KiB), by Content-Length up front and by an http.MaxBytesReader while
//     reading (which also makes net/http close the connection) → otherwise payload_too_large;
//   - the body is exactly one JSON value, followed by nothing but white space → otherwise bad_request (empty body,
//     malformed JSON, wrong types, trailing data, a failed read).
//
// Unknown fields are ignored (03 §12.1). A dst that is not a non-nil pointer is a programming error: that error is
// returned as is, so WriteError turns it into 500 internal.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) error {
	if maxBytes <= 0 {
		maxBytes = defaultMaxJSONBytes
	}
	if !isJSONRequest(r.Header.Get("Content-Type")) {
		return api.NewError(api.CodeUnsupportedMediaType)
	}
	if r.ContentLength > maxBytes {
		return api.NewError(api.CodePayloadTooLarge)
	}
	if r.Body == nil {
		return api.NewError(api.CodeBadRequest)
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBytes))
	if err := dec.Decode(dst); err != nil {
		return decodeError(err)
	}
	// Only white space may follow the value: the next token must be the end of the body.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return api.NewError(api.CodeBadRequest)
		}
		return decodeError(err)
	}
	return nil
}

func decodeError(err error) error {
	var mbe *http.MaxBytesError
	var iue *json.InvalidUnmarshalError
	switch {
	case errors.As(err, &mbe):
		return api.NewError(api.CodePayloadTooLarge)
	case errors.As(err, &iue):
		return fmt.Errorf("httpapi.DecodeJSON: %w", err)
	default:
		return api.NewError(api.CodeBadRequest)
	}
}

// isJSONRequest reports whether a request Content-Type is application/json, with no charset or charset=utf-8.
func isJSONRequest(contentType string) bool {
	mt, params, err := mime.ParseMediaType(contentType)
	if err != nil || mt != "application/json" {
		return false
	}
	cs, ok := params["charset"]
	return !ok || strings.EqualFold(cs, "utf-8")
}
