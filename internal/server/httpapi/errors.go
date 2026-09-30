package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// WriteError writes 03's envelope {"error": {…}} (03 §12.2, 04 §9.4):
//   - an *api.Error (also wrapped) is sent with the status api.StatusOf(code), and a Retry-After header when
//     RetryAfter is set; a code the table doesn't know is a bug and goes out as 500 internal (its code is logged);
//   - an *http.MaxBytesError (a body over its limit) becomes payload_too_large;
//   - any other error becomes 500 internal with the request ID in requestId, and is logged with the route pattern
//     and the request ID (at debug level when the client has gone away). The error text never reaches the client.
//
// A 500 carries only code and requestId, never fields or params. Error responses are never cached
// (Cache-Control: no-store). Headers the handler set before (Allow, Set-Cookie) are kept.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	var out api.Error
	var ae *api.Error
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &ae) && ae != nil:
		out = *ae
	case errors.As(err, &mbe):
		out = api.Error{Code: api.CodePayloadTooLarge}
	default:
		out = api.Error{Code: api.CodeInternal}
	}
	status := api.StatusOf(out.Code)
	body, encErr := encodeJSON(api.ErrorResponse{Error: out})
	if encErr != nil { // Params held a value JSON can't encode: a bug in the handler
		err = errors.Join(err, encErr)
		status = http.StatusInternalServerError
	}
	if status == http.StatusInternalServerError {
		// A 500 is always internal: a code without a row in the table (a typo, a code added without its StatusOf
		// row) is a bug, and the SPA has no text for it. The log keeps the original code.
		if out.Code != api.CodeInternal && encErr == nil {
			err = fmt.Errorf("httpapi.WriteError: code %q has no HTTP status: %w", out.Code, err)
		}
		out = api.Error{Code: api.CodeInternal, RequestID: RequestID(r)}
		if out.RequestID == "" {
			out.RequestID = newRequestID() // not through the router: still give the report something to quote
		}
		logInternal(r, stateOf(r), err, out.RequestID)
		body, _ = encodeJSON(api.ErrorResponse{Error: out}) // a code and a hex ID always encode
	}

	h := w.Header()
	h.Del("Content-Encoding")
	h.Del("ETag")
	h.Del("Last-Modified")
	h.Set("Cache-Control", cacheNoStore)
	if out.RetryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(out.RetryAfter))
	}
	writeJSONBytes(w, status, body)
}

// logInternal logs the cause of a 500 once per request.
func logInternal(r *http.Request, st *reqState, err error, requestID string) {
	log := slog.Default()
	route := ""
	if st != nil {
		if st.logged {
			return
		}
		st.logged = true
		log, route = st.log, st.route
	}
	level := slog.LevelError
	if r.Context().Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		level = slog.LevelDebug // the client went away; nothing is wrong with the server
	}
	attrs := []slog.Attr{slog.String("route", route), slog.String("request_id", requestID)}
	if err != nil {
		attrs = append(attrs, logx.Err(err))
	}
	log.LogAttrs(r.Context(), level, "http internal error", attrs...)
}
