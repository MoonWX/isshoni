package compat_test

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/MoonWX/isshoni/internal/client/compat"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/version"
)

// app is a released build that speaks protocols 1 and 2, so rows 1 and 2 can be told apart; dev is the same as a
// dev build (version.Version() without linker flags).
var (
	app = compat.App{Version: "0.2.0", Protocol: 2, MinProtocol: 1}
	dev = compat.App{Version: "0.0.0-dev+0123abc", Protocol: 2, MinProtocol: 1}
)

// info is a current server's /info with the given protocol range, features and floor.
func info(current, minimum int, features []string, floor string) api.Info {
	return api.Info{
		Server:           api.InfoServer{Name: "Alex's server", Version: "0.2.3", PublicURL: "https://watch.example.com"},
		Protocol:         api.InfoProtocol{Current: current, Min: minimum},
		MinClientVersion: floor,
		Features:         features,
	}
}

var deviceFlow = []string{api.InfoFeaturePush, api.InfoFeatureDeviceFlow}

func TestFromInfo(t *testing.T) {
	tests := []struct {
		name    string
		info    api.Info
		app     compat.App
		verdict compat.Verdict
		reason  compat.Reason
	}{
		{"current server", info(2, 1, deviceFlow, ""), app, compat.OK, compat.ReasonNone},
		{"server one protocol behind (N-1)", info(1, 1, deviceFlow, ""), app, compat.OK, compat.ReasonNone},
		{"server one protocol ahead (N+1)", info(3, 2, deviceFlow, ""), app, compat.OK, compat.ReasonNone},

		// Row 1: the server's newest protocol is below the app's oldest.
		{"row 1: protocol too old", info(0, 0, deviceFlow, ""), app, compat.UpdateServer, compat.ReasonProtocolTooOld},
		{"row 1 beats row 3", info(0, 0, nil, ""), app, compat.UpdateServer, compat.ReasonProtocolTooOld},
		{"row 1 beats row 4", info(0, 0, deviceFlow, "9.0.0"), app, compat.UpdateServer, compat.ReasonProtocolTooOld},
		{"row 1 at the boundary", info(1, 1, deviceFlow, ""), compat.App{Version: "0.2.0", Protocol: 3, MinProtocol: 2},
			compat.UpdateServer, compat.ReasonProtocolTooOld},

		// Row 2: the server's oldest protocol is above the app's newest.
		{"row 2: protocol too new", info(4, 3, deviceFlow, ""), app, compat.UpdateApp, compat.ReasonProtocolTooNew},
		{"row 2 beats row 3", info(4, 3, nil, ""), app, compat.UpdateApp, compat.ReasonProtocolTooNew},
		{"row 2 beats row 4", info(4, 3, deviceFlow, "9.0.0"), app, compat.UpdateApp, compat.ReasonProtocolTooNew},
		{"row 2 just inside", info(4, 2, deviceFlow, ""), app, compat.OK, compat.ReasonNone},

		// Row 3: a 0.1.x server has no device flow.
		{"row 3: no features", info(1, 1, nil, ""), app, compat.UpdateServer, compat.ReasonNoDeviceFlow},
		{"row 3: other features only", info(1, 1, []string{api.InfoFeaturePush, api.InfoFeaturePasswordReset}, ""),
			app, compat.UpdateServer, compat.ReasonNoDeviceFlow},
		{"row 3: the feature name is exact", info(1, 1, []string{"deviceflow", "DeviceFlow"}, ""), app,
			compat.UpdateServer, compat.ReasonNoDeviceFlow},
		{"row 3 beats row 4", info(1, 1, nil, "9.0.0"), app, compat.UpdateServer, compat.ReasonNoDeviceFlow},

		// Row 4: the minClientVersion floor.
		{"row 4: below the floor", info(2, 1, deviceFlow, "0.3.0"), app, compat.UpdateApp, compat.ReasonMinClientVersion},
		{"row 4: at the floor", info(2, 1, deviceFlow, "0.2.0"), app, compat.OK, compat.ReasonNone},
		{"row 4: above the floor", info(2, 1, deviceFlow, "0.1.9"), app, compat.OK, compat.ReasonNone},
		{"row 4: a floor with a v", info(2, 1, deviceFlow, "v0.3.0"), app, compat.UpdateApp, compat.ReasonMinClientVersion},
		{"row 4: a pre-release is below its release", info(2, 1, deviceFlow, "0.2.0"),
			compat.App{Version: "0.2.0-rc.1", Protocol: 2, MinProtocol: 1}, compat.UpdateApp, compat.ReasonMinClientVersion},
		{"row 4: build metadata has no precedence", info(2, 1, deviceFlow, "0.2.0+other"),
			compat.App{Version: "0.2.0+abc", Protocol: 2, MinProtocol: 1}, compat.OK, compat.ReasonNone},
		{"row 4: a floor that isn't SemVer is no floor", info(2, 1, deviceFlow, "latest"), app, compat.OK, compat.ReasonNone},
		{"row 4: a two-part floor isn't SemVer", info(2, 1, deviceFlow, "9.0"), app, compat.OK, compat.ReasonNone},
		{"row 4: an app version that isn't SemVer is below", info(2, 1, deviceFlow, "0.0.1"),
			compat.App{Version: "unknown", Protocol: 2, MinProtocol: 1}, compat.UpdateApp, compat.ReasonMinClientVersion},
		{"row 4: an empty app version is below", info(2, 1, deviceFlow, "0.0.1"),
			compat.App{Protocol: 2, MinProtocol: 1}, compat.UpdateApp, compat.ReasonMinClientVersion},

		// Dev builds (0.0.0-dev+<commit>) are below every floor and fine without one.
		{"dev build, no floor", info(2, 1, deviceFlow, ""), dev, compat.OK, compat.ReasonNone},
		{"dev build below a floor", info(2, 1, deviceFlow, "0.1.0"), dev, compat.UpdateApp, compat.ReasonMinClientVersion},
		{"dev build, a floor that isn't SemVer", info(2, 1, deviceFlow, "x"), dev, compat.OK, compat.ReasonNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := compat.FromInfo(tt.info, tt.app)
			want := compat.Check{
				Verdict: tt.verdict, Reason: tt.reason,
				ServerVersion: tt.info.Server.Version, MinClientVersion: tt.info.MinClientVersion,
			}
			if got != want {
				t.Errorf("FromInfo = %+v, want %+v", got, want)
			}
		})
	}
}

// TestFromInfoZeroInfo: a zero Info (a body that decoded to nothing) never reads as OK.
func TestFromInfoZeroInfo(t *testing.T) {
	got := compat.FromInfo(api.Info{}, compat.Default())
	if got.Verdict == compat.OK {
		t.Fatalf("FromInfo(zero Info) = %+v, want a verdict that isn't ok", got)
	}
}

// TestFromInfoCurrentServer: this build and a server of this build agree.
func TestFromInfoCurrentServer(t *testing.T) {
	cur := api.Info{
		Server:   api.InfoServer{Version: version.Version()},
		Protocol: api.InfoProtocol{Current: protocol.Version, Min: protocol.MinVersion},
		Features: []string{api.InfoFeatureDeviceFlow},
	}
	if got := compat.FromInfo(cur, compat.Default()); got.Verdict != compat.OK {
		t.Fatalf("FromInfo(this build's own /info) = %+v, want ok", got)
	}
}

func TestDefault(t *testing.T) {
	want := compat.App{Version: version.Version(), Protocol: protocol.Version, MinProtocol: protocol.MinVersion}
	if got := compat.Default(); got != want {
		t.Fatalf("Default() = %+v, want %+v", got, want)
	}
}

// protocolUnsupported is the hub's error with the given params (internal/server/signal, step 4 of the handshake).
func protocolUnsupported(params map[string]any) *protocol.Error {
	e := protocol.NewError(protocol.ErrorCodeProtocolUnsupported, protocol.ErrorScopeConnection)
	e.Params = params
	return &e
}

// params is an error's params map.
type params = map[string]any

func TestFromError(t *testing.T) {
	one := compat.App{Version: "0.2.0", Protocol: 1, MinProtocol: 1}   // M2: protocol 1 only
	three := compat.App{Version: "0.4.0", Protocol: 3, MinProtocol: 2} // a later app that dropped protocol 1
	outdated := protocol.NewError(protocol.ErrorCodeClientOutdated, protocol.ErrorScopeConnection)
	undecided := compat.Check{}
	tooNew := func(serverVersion string) compat.Check {
		return compat.Check{Verdict: compat.UpdateApp, Reason: compat.ReasonProtocolTooNew, ServerVersion: serverVersion}
	}
	tooOld := func(serverVersion string) compat.Check {
		return compat.Check{Verdict: compat.UpdateServer, Reason: compat.ReasonProtocolTooOld, ServerVersion: serverVersion}
	}
	tests := []struct {
		name string
		err  *protocol.Error
		app  compat.App
		want compat.Check
		ok   bool
	}{
		// The undecided cases: Recheck (/info) decides.
		{"nil", nil, app, undecided, false},
		{"protocol_unsupported without params (M1's errorForClose)", protocolUnsupported(nil), app, undecided, false},
		{"protocol_unsupported with empty params", protocolUnsupported(params{}), app, undecided, false},
		{"protocol_unsupported without serverMax",
			protocolUnsupported(params{"serverMin": 1, "serverVersion": "0.9.0"}), app, undecided, false},
		{"serverMax 0", protocolUnsupported(params{"serverMax": 0}), app, undecided, false},
		{"serverMax 0.0 (JSON)", protocolUnsupported(params{"serverMax": 0.0}), app, undecided, false},
		{"a negative serverMax", protocolUnsupported(params{"serverMax": -1}), app, undecided, false},
		{"a fractional serverMax", protocolUnsupported(params{"serverMax": 1.5}), app, undecided, false},
		{"a string serverMax", protocolUnsupported(params{"serverMax": "3"}), app, undecided, false},
		{"a null serverMax", protocolUnsupported(params{"serverMax": nil}), app, undecided, false},
		{"a NaN serverMax", protocolUnsupported(params{"serverMax": math.NaN()}), app, undecided, false},
		{"a fractional json.Number", protocolUnsupported(params{"serverMax": json.Number("1.5")}), app, undecided, false},
		{"another code", &protocol.Error{Code: protocol.ErrorCodeUnauthenticated, Params: params{"serverMax": 3}},
			app, undecided, false},
		{"a code this build doesn't know", &protocol.Error{Code: "future_code"}, app, undecided, false},
		{"an empty code", &protocol.Error{}, app, undecided, false},

		// client_outdated.
		{"client_outdated", &outdated, app,
			compat.Check{Verdict: compat.UpdateApp, Reason: compat.ReasonMinClientVersion}, true},
		{"client_outdated with params", &protocol.Error{Code: protocol.ErrorCodeClientOutdated,
			Params: params{"serverVersion": "0.4.0", "minClientVersion": "0.3.0"}}, app,
			compat.Check{Verdict: compat.UpdateApp, Reason: compat.ReasonMinClientVersion, ServerVersion: "0.4.0",
				MinClientVersion: "0.3.0"}, true},

		// protocol_unsupported with serverMax.
		{"serverMax 1, MinProtocol 1",
			protocolUnsupported(params{"serverMin": 1, "serverMax": 1, "serverVersion": "0.9.0"}),
			one, tooNew("0.9.0"), true},
		{"serverMax above the app's newest",
			protocolUnsupported(params{"serverMin": 3, "serverMax": 4, "serverVersion": "0.9.0"}),
			app, tooNew("0.9.0"), true},
		{"serverMax below the app's oldest",
			protocolUnsupported(params{"serverMin": 1, "serverMax": 1, "serverVersion": "0.1.0"}),
			three, tooOld("0.1.0"), true},
		{"serverMax at the app's oldest", protocolUnsupported(params{"serverMax": 2}), three, tooNew(""), true},
		{"serverMax as JSON decodes it", protocolUnsupported(params{"serverMax": 1.0}), three, tooOld(""), true},
		{"serverMax as json.Number", protocolUnsupported(params{"serverMax": json.Number("1")}), three, tooOld(""), true},
		{"a whole json.Number with an exponent", protocolUnsupported(params{"serverMax": json.Number("1e1")}),
			app, tooNew(""), true},
		{"a huge serverMax", protocolUnsupported(params{"serverMax": 1e300}), app, tooNew(""), true},
		{"a huge unsigned serverMax", protocolUnsupported(params{"serverMax": uint64(math.MaxUint64)}),
			app, tooNew(""), true},
		{"a serverVersion that isn't a string is dropped",
			protocolUnsupported(params{"serverMax": 4, "serverVersion": 9}), app, tooNew(""), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := compat.FromError(tt.err, tt.app)
			if got != tt.want || ok != tt.ok {
				t.Errorf("FromError = %+v, %v; want %+v, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

// TestFromErrorDecodedFixture: the hub's protocol_unsupported as the Go signal client decodes it from the wire.
func TestFromErrorDecodedFixture(t *testing.T) {
	var e protocol.Error
	raw := `{"code":"protocol_unsupported","retryable":false,"scope":"connection",` +
		`"params":{"serverMin":1,"serverMax":1,"serverVersion":"0.1.0"}}`
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		t.Fatal(err)
	}
	got, ok := compat.FromError(&e, compat.App{Version: "0.3.0", Protocol: 3, MinProtocol: 2})
	want := compat.Check{Verdict: compat.UpdateServer, Reason: compat.ReasonProtocolTooOld, ServerVersion: "0.1.0"}
	if !ok || got != want {
		t.Fatalf("FromError = %+v, %v; want %+v, true", got, ok, want)
	}
}

// TestFromErrorUndecidedThenInfo: the case FromError leaves open is the one /info decides. A bare 4426 close for a
// client below the floor reads as protocol_unsupported without params; FromInfo then finds the floor.
func TestFromErrorUndecidedThenInfo(t *testing.T) {
	bare := protocolUnsupported(nil)
	if c, ok := compat.FromError(bare, app); ok {
		t.Fatalf("FromError(bare 4426) = %+v, true; want undecided", c)
	}
	got := compat.FromInfo(info(2, 1, deviceFlow, "0.3.0"), app)
	if got.Verdict != compat.UpdateApp || got.Reason != compat.ReasonMinClientVersion {
		t.Fatalf("FromInfo = %+v, want update_app/min_client_version", got)
	}
}

// TestCheckJSON pins the JSON names AuthState.compat uses (02 §11.7).
func TestCheckJSON(t *testing.T) {
	tests := []struct {
		c    compat.Check
		want string
	}{
		{compat.Check{Verdict: compat.OK, ServerVersion: "0.2.0"},
			`{"verdict":"ok","reason":"","serverVersion":"0.2.0","minClientVersion":""}`},
		{compat.Check{Verdict: compat.UpdateApp, Reason: compat.ReasonMinClientVersion, ServerVersion: "0.4.0",
			MinClientVersion: "0.3.0"},
			`{"verdict":"update_app","reason":"min_client_version","serverVersion":"0.4.0","minClientVersion":"0.3.0"}`},
		{compat.Check{Verdict: compat.UpdateServer, Reason: compat.ReasonNoDeviceFlow, ServerVersion: "0.1.4"},
			`{"verdict":"update_server","reason":"no_device_flow","serverVersion":"0.1.4","minClientVersion":""}`},
	}
	for _, tt := range tests {
		b, err := json.Marshal(tt.c)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != tt.want {
			t.Errorf("json.Marshal(%+v) = %s, want %s", tt.c, b, tt.want)
		}
		var back compat.Check
		if err := json.Unmarshal(b, &back); err != nil || back != tt.c {
			t.Errorf("round trip of %s = %+v, %v; want %+v", b, back, err, tt.c)
		}
	}
}

// TestStrings pins the wire strings of the verdicts and reasons (02 §13.2; the SPA maps them to texts).
func TestStrings(t *testing.T) {
	for _, tt := range [][2]string{
		{string(compat.OK), "ok"},
		{string(compat.UpdateApp), "update_app"},
		{string(compat.UpdateServer), "update_server"},
		{string(compat.ReasonNone), ""},
		{string(compat.ReasonProtocolTooNew), "protocol_too_new"},
		{string(compat.ReasonProtocolTooOld), "protocol_too_old"},
		{string(compat.ReasonNoDeviceFlow), "no_device_flow"},
		{string(compat.ReasonMinClientVersion), "min_client_version"},
	} {
		if tt[0] != tt[1] {
			t.Errorf("got %q, want %q", tt[0], tt[1])
		}
	}
}
