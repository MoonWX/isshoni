package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/desktop/bridge"
	"github.com/MoonWX/isshoni/internal/desktop/bridge/codes"
)

// The HTTP transport of 03 §7.4 (the SPA's HttpBridge): POST /__bridge/call with {"service", "method", "args"}
// calls the bound Go method of that name on the real bridge services, the way Wails calls it (a context.Context
// first parameter gets the request's context, the JSON arguments are decoded into the parameters' types), and
// answers 200 with the result's JSON or a non-2xx status with 03 §6.4's bridge error JSON. GET /__bridge/events is
// an event stream whose messages are {"name": "isshoni:<event>", "data": <payload>}.

// Transport paths (web/src/platform/desktop/http/HttpBridge.ts).
const (
	callPath   = "/__bridge/call"
	eventsPath = "/__bridge/events"
)

// maxCallBody bounds a call's body (the Guard's runtime cap, 03 §5.6).
const maxCallBody = 64 << 10

// callRequest is a call's body.
type callRequest struct {
	Service string            `json:"service"`
	Method  string            `json:"method"`
	Args    []json.RawMessage `json:"args"`
}

// dispatcher calls bound methods by name.
type dispatcher struct {
	services map[string]reflect.Value // "App" → *bridge.App
	log      *slog.Logger
	// observe sees every successful call's arguments and result (the harness records the tokens handed out).
	observe func(service, method string, args []json.RawMessage, result any)
}

var ctxType = reflect.TypeFor[context.Context]()
var errType = reflect.TypeFor[error]()

// newDispatcher indexes the services by their 03 §6.2 names: every exported field of bridge.Services.
func newDispatcher(s *bridge.Services, log *slog.Logger) *dispatcher {
	d := &dispatcher{services: map[string]reflect.Value{}, log: log}
	v := reflect.ValueOf(s).Elem()
	for i := range v.NumField() {
		f := v.Type().Field(i)
		if f.IsExported() && f.Type.Kind() == reflect.Pointer {
			d.services[f.Name] = v.Field(i)
		}
	}
	return d
}

// call runs one bound method. It returns the result (nil for none) or an error: a *bridge.Error from the method, or
// one of the harness's own (an unknown method, bad arguments, a panic) as internal.
func (d *dispatcher) call(ctx context.Context, req callRequest) (result any, err error) {
	svc, ok := d.services[req.Service]
	if !ok {
		return nil, transportError("unknown_service")
	}
	m := svc.MethodByName(req.Method)
	if !m.IsValid() || req.Method == "" || req.Method[0] < 'A' || req.Method[0] > 'Z' {
		return nil, transportError("unknown_method")
	}
	t := m.Type()
	in := make([]reflect.Value, 0, t.NumIn())
	first := 0
	if t.NumIn() > 0 && t.In(0) == ctxType {
		in = append(in, reflect.ValueOf(ctx))
		first = 1
	}
	if len(req.Args) > t.NumIn()-first {
		return nil, transportError("too_many_arguments")
	}
	for i := first; i < t.NumIn(); i++ {
		p := reflect.New(t.In(i))
		if j := i - first; j < len(req.Args) && len(req.Args[j]) > 0 {
			// A missing trailing argument (HttpBridge leaves out trailing undefined values) is the zero value.
			if err := json.Unmarshal(req.Args[j], p.Interface()); err != nil {
				return nil, transportError("bad_argument")
			}
		}
		in = append(in, p.Elem())
	}
	defer func() {
		if p := recover(); p != nil {
			d.log.Error("e2ehost: a bound method panicked", "service", req.Service, "method", req.Method,
				"panic", fmt.Sprint(p))
			result, err = nil, transportError("panic")
		}
	}()
	out := m.Call(in)
	for _, o := range out {
		if o.Type() == errType {
			if !o.IsNil() {
				return nil, o.Interface().(error)
			}
			continue
		}
		result = o.Interface()
	}
	if d.observe != nil {
		d.observe(req.Service, req.Method, req.Args, result)
	}
	return result, nil
}

// transportError is the harness's own failure: internal with params.reason.
func transportError(reason string) *bridge.Error {
	return &bridge.Error{Code: codes.CodeInternal, Params: map[string]any{"reason": reason}}
}

// serveCall is POST /__bridge/call.
func (d *dispatcher) serveCall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, transportError("method_not_allowed"))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCallBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, transportError("body_too_large"))
		return
	}
	var req callRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, transportError("bad_request"))
		return
	}
	result, err := d.call(r.Context(), req)
	if err != nil {
		var be *bridge.Error
		if !errors.As(err, &be) {
			d.log.Warn("e2ehost: a bound method returned an error that isn't a bridge error", "service", req.Service,
				"method", req.Method, "err", err)
			be = transportError("not_a_bridge_error")
		}
		status := http.StatusUnprocessableEntity
		if be.Code == codes.CodeInternal {
			status = http.StatusInternalServerError
		}
		writeError(w, status, be)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func writeError(w http.ResponseWriter, status int, e *bridge.Error) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, e.Error())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, transportError("result_not_json"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// eventHub fans the app's events out to every open event stream.
type eventHub struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
	log  *slog.Logger
}

func newEventHub(log *slog.Logger) *eventHub {
	return &eventHub{subs: map[chan []byte]struct{}{}, log: log}
}

// emit is the Shell's Emit: one message {"name", "data"} to every stream; a stream that lags 64 messages behind
// loses the message (the SPA pulls state after it subscribes, 03 §6.3).
func (h *eventHub) emit(name string, data any) {
	b, err := json.Marshal(struct {
		Name string `json:"name"`
		Data any    `json:"data"`
	}{name, data})
	if err != nil {
		h.log.Warn("e2ehost: an event that isn't JSON", "event", name, "err", err)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- b:
		default:
			h.log.Warn("e2ehost: an event stream lags; message dropped", "event", name)
		}
	}
}

// keepAlive is the comment the stream sends while nothing happens, so no proxy or browser closes it.
const keepAlive = 15 * time.Second

// serveEvents is GET /__bridge/events: text/event-stream until the client goes away.
func (h *eventHub) serveEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, transportError("method_not_allowed"))
		return
	}
	rc := http.NewResponseController(w)
	ch := make(chan []byte, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if _, err := io.WriteString(w, ": isshoni e2e events\n\n"); err != nil || rc.Flush() != nil {
		return
	}
	tick := time.NewTicker(keepAlive)
	defer tick.Stop()
	for {
		var chunk []byte
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			chunk = []byte(": keep-alive\n\n")
		case b := <-ch:
			chunk = append(append([]byte("data: "), b...), '\n', '\n')
		}
		if _, err := w.Write(chunk); err != nil || rc.Flush() != nil {
			return
		}
	}
}
