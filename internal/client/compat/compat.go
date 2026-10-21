// Package compat decides whether this native app and a server can work together (docs/m2/02-device-auth.md §13): one
// table turns a server's GET /api/v1/info, or a hello refused with protocol_unsupported or client_outdated, into
// "Update the app" or "Ask your admin to update the server".
//
// The desktop app checks before linking and at every start (FromInfo, through deviceauth.Manager), and whenever a
// hello is refused (FromError, through Manager.HelloRefused). FromError can't always decide: when the server closed
// with 4426 without an error message first, M1's Go signal client reports protocol_unsupported with empty params, even
// when the real cause was client_outdated. FromError then answers false, and the caller asks /info again
// (Manager.Recheck), whose FromInfo also catches a minClientVersion floor.
//
// The package is pure Go with no I/O; it imports only the standard library, internal/protocol, internal/protocol/api
// and internal/version (02 §11.1), so the pending mobile apps can reuse it.
package compat

import (
	"encoding/json"
	"math"
	"slices"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/version"
)

// Verdict is what the app tells the user (02 §13.3).
type Verdict string

const (
	OK           Verdict = "ok"            // the app and the server work together
	UpdateApp    Verdict = "update_app"    // "This server needs a newer isshoni app."
	UpdateServer Verdict = "update_server" // "This server runs isshoni {version}, which is too old for this app."
)

// Reason says which row of the table decided a verdict that isn't OK.
type Reason string

const (
	ReasonNone             Reason = ""                   // the verdict is OK
	ReasonProtocolTooNew   Reason = "protocol_too_new"   // the server's oldest protocol is newer than the app's newest
	ReasonProtocolTooOld   Reason = "protocol_too_old"   // the server's newest protocol is older than the app's oldest
	ReasonNoDeviceFlow     Reason = "no_device_flow"     // /info lists no deviceFlow feature (a 0.1.x server)
	ReasonMinClientVersion Reason = "min_client_version" // the app is below the server's minClientVersion floor
)

// Check is one decision. The JSON names are those of AuthState.compat (02 §11.7) and of the desktop bridge's
// incompatible_server params (03 §6.4).
type Check struct {
	Verdict          Verdict `json:"verdict"`
	Reason           Reason  `json:"reason"`
	ServerVersion    string  `json:"serverVersion"`    // the server's version as it reported it; "" when unknown
	MinClientVersion string  `json:"minClientVersion"` // the server's floor for native apps; "" when none or unknown
}

// App is what this build speaks: its own version and the protocol versions it accepts.
type App struct {
	Version               string // SemVer, as version.Version() reports it
	Protocol, MinProtocol int    // newest and oldest signaling protocol versions this build speaks
}

// Default describes this build: version.Version(), protocol.Version and protocol.MinVersion.
func Default() App {
	return App{Version: version.Version(), Protocol: protocol.Version, MinProtocol: protocol.MinVersion}
}

// FromInfo decides from a server's /info (02 §13.2). The first matching row wins:
//
//  1. info.Protocol.Current < app.MinProtocol: UpdateServer, protocol_too_old;
//  2. info.Protocol.Min > app.Protocol: UpdateApp, protocol_too_new;
//  3. no deviceFlow in info.Features (a 0.1.x server): UpdateServer, no_device_flow;
//  4. a minClientVersion floor that app.Version is below (version.Below): UpdateApp, min_client_version. A floor that
//     isn't SemVer can't be enforced and counts as no floor; an app version that isn't SemVer (a dev build counts as
//     below every release) is below any floor;
//  5. otherwise OK.
//
// Every Check carries the server's version and floor as /info reported them.
func FromInfo(info api.Info, app App) Check {
	c := Check{Verdict: OK, ServerVersion: info.Server.Version, MinClientVersion: info.MinClientVersion}
	switch {
	case info.Protocol.Current < app.MinProtocol:
		c.Verdict, c.Reason = UpdateServer, ReasonProtocolTooOld
	case info.Protocol.Min > app.Protocol:
		c.Verdict, c.Reason = UpdateApp, ReasonProtocolTooNew
	case !slices.Contains(info.Features, api.InfoFeatureDeviceFlow):
		c.Verdict, c.Reason = UpdateServer, ReasonNoDeviceFlow
	case info.MinClientVersion != "" && belowFloor(app.Version, info.MinClientVersion):
		c.Verdict, c.Reason = UpdateApp, ReasonMinClientVersion
	}
	return c
}

// FromError decides from the error a server refused a hello with (02 §13.2). It answers false (undecided, with a
// zero Check) for a nil error, for any code but client_outdated and protocol_unsupported, and for protocol_unsupported
// whose params.serverMax is absent, 0 or not a positive whole number: M1's errorForClose reports a bare 4426 close
// that way even when the cause was client_outdated, and reading the missing serverMax as 0 would blame the server
// wrongly. The caller then runs /info and FromInfo (Manager.Recheck).
//
//   - client_outdated: UpdateApp, min_client_version;
//   - protocol_unsupported with serverMax < app.MinProtocol: UpdateServer, protocol_too_old;
//   - protocol_unsupported with any other serverMax above 0: UpdateApp, protocol_too_new.
//
// A decided Check carries params.serverVersion (and params.minClientVersion) when the server sent them as strings.
func FromError(e *protocol.Error, app App) (Check, bool) {
	if e == nil {
		return Check{}, false
	}
	c := Check{ServerVersion: stringParam(e.Params, "serverVersion"),
		MinClientVersion: stringParam(e.Params, "minClientVersion")}
	switch e.Code {
	case protocol.ErrorCodeClientOutdated:
		c.Verdict, c.Reason = UpdateApp, ReasonMinClientVersion
	case protocol.ErrorCodeProtocolUnsupported:
		serverMax, ok := positiveInt(e.Params["serverMax"])
		if !ok {
			return Check{}, false
		}
		if serverMax < int64(app.MinProtocol) {
			c.Verdict, c.Reason = UpdateServer, ReasonProtocolTooOld
		} else {
			c.Verdict, c.Reason = UpdateApp, ReasonProtocolTooNew
		}
	default:
		return Check{}, false
	}
	return c, true
}

// belowFloor is version.Below with a floor that isn't SemVer treated as no floor.
func belowFloor(v, floor string) bool {
	below, ok := version.Below(v, floor)
	return ok && below
}

// stringParam returns params[key] when it is a string, else "".
func stringParam(params map[string]any, key string) string {
	s, _ := params[key].(string)
	return s
}

// positiveInt reads a protocol version from an error's params: a whole number above 0, as Go code builds it (any
// integer type) or as encoding/json decodes it (float64, or json.Number with UseNumber). Anything else, absent and 0
// included, is not a version. A whole number beyond int64 saturates: it is still "above every version this build
// speaks".
func positiveInt(v any) (int64, bool) {
	var f float64
	switch n := v.(type) {
	case int:
		return int64(n), n > 0
	case int8:
		return int64(n), n > 0
	case int16:
		return int64(n), n > 0
	case int32:
		return int64(n), n > 0
	case int64:
		return n, n > 0
	case uint:
		return saturate(uint64(n)), n > 0
	case uint8:
		return int64(n), n > 0
	case uint16:
		return int64(n), n > 0
	case uint32:
		return int64(n), n > 0
	case uint64:
		return saturate(n), n > 0
	case float32:
		f = float64(n)
	case float64:
		f = n
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, i > 0
		}
		var err error
		if f, err = n.Float64(); err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 || f != math.Trunc(f) {
		return 0, false
	}
	if f >= math.MaxInt64 {
		return math.MaxInt64, true
	}
	return int64(f), true
}

func saturate(n uint64) int64 {
	if n > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(n)
}
