package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"testing"
)

// switchingService stands in for Auth.AccessToken racing a server switch: its method hands out the token of the
// server active when it was called, and the switch lands before it returns.
type switchingService struct{ active string }

func (s *switchingService) Token(_ context.Context, origin string) (string, error) {
	own := s.active
	s.active = "https://b.example" // the switch, during the call
	return own + "|" + origin, nil
}

// TestDispatcherBeforeRunsAtTheCallsStart: before's value is read before the method runs and reaches observe with
// the result, so the harness records the active server of the call's start, not the one after a switch.
func TestDispatcherBeforeRunsAtTheCallsStart(t *testing.T) {
	svc := &switchingService{active: "https://a.example"}
	d := &dispatcher{services: map[string]reflect.Value{"Svc": reflect.ValueOf(svc)},
		log: slog.New(slog.DiscardHandler)}
	var (
		beforeCalls int
		gotPre      any
		gotResult   any
	)
	d.before = func(service, method string) any {
		beforeCalls++
		if service != "Svc" || method != "Token" {
			t.Errorf("before(%q, %q)", service, method)
		}
		return svc.active
	}
	d.observe = func(_, _ string, _ []json.RawMessage, pre, result any) {
		gotPre, gotResult = pre, result
	}
	arg, _ := json.Marshal("https://a.example")
	res, err := d.call(context.Background(), callRequest{Service: "Svc", Method: "Token",
		Args: []json.RawMessage{arg}})
	if err != nil || res != "https://a.example|https://a.example" {
		t.Fatalf("call = %v, %v", res, err)
	}
	if beforeCalls != 1 || gotPre != "https://a.example" || gotResult != res {
		t.Fatalf("before ran %d times; observe got pre %v, result %v", beforeCalls, gotPre, gotResult)
	}
	if svc.active != "https://b.example" {
		t.Fatal("the method didn't switch")
	}

	// A call refused before the method runs (bad arguments) reaches neither hook.
	beforeCalls, gotPre = 0, nil
	if _, err := d.call(context.Background(), callRequest{Service: "Svc", Method: "Token",
		Args: []json.RawMessage{json.RawMessage(`42`)}}); err == nil {
		t.Fatal("a number for a string argument was accepted")
	}
	if beforeCalls != 0 || gotPre != nil {
		t.Fatalf("hooks ran for a refused call: before %d, pre %v", beforeCalls, gotPre)
	}
}
