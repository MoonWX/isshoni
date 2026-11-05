//go:build windows || (darwin && cgo)

package main

// flavor is what the build tags change (doc.go). Each tag's file sets its part in an init function; a build without
// the tag never compiles that file, so a release (exactly -tags production) holds no dev or e2e code path.
var flavor struct {
	production bool // -tags production: every release; the WEBVIEW2_* scrub (03 §5.4) and Wails' release mode
	dev        bool // -tags dev: `task desktop:dev`
	e2e        bool // -tags e2e: the CDP build (03 §22.4 B)

	// devServer returns the Vite dev server's origin for the asset handler (dev builds only; nil otherwise).
	devServer func() string

	// browserArgs are WebView2 arguments after autoplayArg: the e2e build's --remote-debugging-port (README WA72);
	// nil in every other build.
	browserArgs []string
	// localAppData replaces FOLDERID_LocalAppData on Windows when it is not "": the e2e build's ISSHONI_E2E_DATA_DIR
	// (03 §22.4 B, README WA72); "" in every other build.
	localAppData string
	// err is the e2e build's refusal of a malformed ISSHONI_E2E_* value (flavor_e2e.go): the start logs it and exits
	// 1. nil in every other build.
	err error
}

// insecureLoopback reports a dev or e2e build: http:// loopback servers are allowed there (03 §5.3, §8.1), and
// BridgeConfig.Dev is true.
func insecureLoopback() bool { return flavor.dev || flavor.e2e }

// devServerOrigin is flavor.devServer's value, "" in every build but dev.
func devServerOrigin() string {
	if flavor.devServer == nil {
		return ""
	}
	return flavor.devServer()
}
