package assets

import "strconv"

// The Wails runtime as the pinned version builds and serves it (03 §3.2 W21). Re-check every table below at every
// Wails bump (03 §3.3) against pkg/application/messageprocessor*.go, transport_http.go, application.go and the npm
// runtime's src/runtime.ts of the new tag.
//
// Pinned: github.com/wailsapp/wails/v3 v3.0.0-beta.34 (tagged 2026-10-20), @wailsio/runtime 3.0.0-beta.34.
//
// Transport (HTTPTransport, pkg/application/transport_http.go): POST /wails/runtime, headers x-wails-client-id,
// Content-Type: application/json and, for a named window, x-wails-window-name; the body is JSON {"object": <int>,
// "method": <int>, "args": <any>}. Only when the body is empty does the transport read "object", "method" and "args"
// from the query (?object=…&method=…, the WebKitGTK fallback). A body over 512 KiB is sent in pieces with the headers
// x-wails-chunk-id, x-wails-chunk-index and x-wails-chunk-total and reassembled (up to 64 MiB) before it is
// parsed. The transport itself checks neither the method (outside -tags server) nor the Origin.
//
// At load the runtime makes no /wails/runtime call: Wails injects window._wails (flags, environment) with
// ExecuteScript, the runtime HEADs /wails/custom.js (404 outside server mode) and announces itself with the
// postMessage IPC. That IPC (window.chrome.webview.postMessage, not HTTP) carries "wails:runtime:ready", "wails:drag",
// "wails:drag:doubleclick", "wails:resize:<edge>", "wails:non-client-region:<json>" and, only for windows with
// AllowSimpleEventEmit (never ours), "wails:event:emit:<name>"; the Guard can't see it, and none of it reaches a
// bound service. Events.On/Off are local to the page.
//
// Other /wails/ paths Wails serves before the app's handler (pkg/application/application.go): GET
// /wails/runtime.js and /wails/transport.js (the runtime's own code, which the SPA bundles anyway), /wails/custom.js
// (404 outside server mode), GET /wails/eventpayload/<32 hex> (an event of more than 8 KiB that Go parked for the
// page to fetch), and /wails/stream/poll and /wails/stream/send (GoStream, a second Go surface that only streams the
// app registers would use; the app registers none).

// runtimeObject is one object of the runtime with its methods, by wire id.
type runtimeObject struct {
	name    string
	methods map[int]string
}

// runtimeObjects is every object the pinned runtime serves on /wails/runtime (objectNames in runtime.ts and
// messageprocessor.go), with the method ids of messageprocessor_<object>.go. The names are only for the refusal log.
var runtimeObjects = map[int]runtimeObject{
	0:  {"Call", map[int]string{0: "Call"}},
	1:  {"Clipboard", map[int]string{0: "SetText", 1: "Text"}},
	2:  {"Application", map[int]string{0: "Hide", 1: "Show", 2: "Quit"}},
	3:  {"Events", map[int]string{0: "Emit"}},
	4:  {"ContextMenu", map[int]string{0: "Open"}},
	5:  {"Dialog", map[int]string{0: "Info", 1: "Warning", 2: "Error", 3: "Question", 4: "OpenFile", 5: "SaveFile"}},
	6:  {"Window", windowMethods},
	7:  {"Screens", map[int]string{0: "GetAll", 1: "GetPrimary", 2: "GetCurrent", 3: "GetByID", 4: "GetByIndex"}},
	8:  {"System", map[int]string{0: "IsDarkMode", 1: "Environment", 2: "Capabilities", 3: "Flags"}},
	9:  {"Browser", map[int]string{0: "OpenURL"}},
	10: {"CancelCall", map[int]string{0: "Cancel"}},
	11: {"IOS", map[int]string{}},
	12: {"Android", map[int]string{}},
}

// windowMethods are the Window object's 53 methods at beta.34 (messageprocessor_window.go).
var windowMethods = map[int]string{
	0: "Position", 1: "Center", 2: "Close", 3: "DisableSizeConstraints", 4: "EnableSizeConstraints", 5: "Focus",
	6: "ForceReload", 7: "Fullscreen", 8: "GetScreen", 9: "GetZoom", 10: "Height", 11: "Hide", 12: "IsFocused",
	13: "IsFullscreen", 14: "IsMaximised", 15: "IsMinimised", 16: "Maximise", 17: "Minimise", 18: "Name",
	19: "OpenDevTools", 20: "RelativePosition", 21: "Reload", 22: "Resizable", 23: "Restore", 24: "SetPosition",
	25: "SetAlwaysOnTop", 26: "SetBackgroundColour", 27: "SetFrameless", 28: "SetFullscreenButtonEnabled",
	29: "SetMaxSize", 30: "SetMinSize", 31: "SetRelativePosition", 32: "SetResizable", 33: "SetSize", 34: "SetTitle",
	35: "SetZoom", 36: "Show", 37: "Size", 38: "ToggleFullscreen", 39: "ToggleMaximise", 40: "ToggleFrameless",
	41: "UnFullscreen", 42: "UnMaximise", 43: "UnMinimise", 44: "Width", 45: "Zoom", 46: "ZoomIn", 47: "ZoomOut",
	48: "ZoomReset", 49: "SnapAssist", 50: "FilesDropped", 51: "Print", 52: "SetScreen",
}

// Wire ids of the two objects the Guard lets through.
const (
	objectCall       = 0  // Call: CallBinding (method 0) with {"call-id", "methodID" or "methodName", "args"}
	objectCancelCall = 10 // CancelCall: Cancel (method 0) with {"call-id"}
)

// runtimeAllow is the Guard's allowlist (§5.6): object → the methods let through. Only the bound-method calls pass;
// the runtime calls nothing else at load (above). Every other object, Browser.OpenURL, Clipboard and Window
// included, is refused.
var runtimeAllow = map[int]map[int]bool{
	objectCall:       {0: true},
	objectCancelCall: {0: true},
}

// allowedCall reports whether the Guard lets object.method through.
func allowedCall(object, method int) bool {
	return runtimeAllow[object][method]
}

// callName names object.method for the refusal log: names from the tables above, ids for anything else.
func callName(object, method int) (objectName, methodName string) {
	o, ok := runtimeObjects[object]
	if !ok {
		return "unknown-" + strconv.Itoa(object), strconv.Itoa(method)
	}
	m, ok := o.methods[method]
	if !ok {
		m = "unknown-" + strconv.Itoa(method)
	}
	return o.name, m
}

// Runtime transport details the Guard depends on.
const (
	runtimePath       = "/wails/runtime"
	eventPayloadPath  = "/wails/eventpayload/"
	runtimeBodyLimit  = 64 << 10 // the Guard reads at most this much of a /wails/runtime body (§5.6, §20)
	headerChunkPrefix = "x-wails-chunk-"
)

// staticRuntimePaths are the runtime's GET paths that serve fixed content and reach no Go code beyond Wails' own
// file server: allowed for GET and HEAD from the asset origin.
var staticRuntimePaths = map[string]bool{
	"/wails/runtime.js":   true,
	"/wails/transport.js": true,
	"/wails/custom.js":    true,
}
