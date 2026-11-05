//go:build windows || (darwin && cgo)

package main

import (
	"log/slog"

	"github.com/MoonWX/isshoni/internal/client/credstore"
)

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
	// credPrefix is the credential store's prefix when it is not "": the e2e build's "isshoni-test:<random>:", whose
	// entries the start deletes at exit, so a run never reads or writes the user's isshoni:v1: entries (03 §22.4 B);
	// "" (credstore's default, isshoni:v1:) in every other build.
	credPrefix string
	// err is the e2e build's refusal of a malformed ISSHONI_E2E_* value (flavor_e2e.go): the start logs it and exits
	// 1. nil in every other build.
	err error
}

// appCredentialOptions is how the app opens its credential store over the data root: flavor.credPrefix's prefix, so
// an e2e build's run stays under its own test prefix.
func appCredentialOptions(root string) credstore.Options {
	return credstore.Options{Prefix: flavor.credPrefix, Dir: root, Logger: slog.Default()}
}

// dropTestCredentials deletes every entry under an e2e build's test prefix (03 §22.4 B: they are deleted at exit). It
// does nothing in other builds, whose store holds the user's own credentials.
func dropTestCredentials(store credstore.Store, log *slog.Logger) {
	if flavor.credPrefix == "" || store == nil {
		return
	}
	origins, err := store.List()
	if err != nil {
		log.Warn("desktop: the test credentials weren't listed", "err", err)
		return
	}
	for _, origin := range origins {
		if err := store.Delete(origin); err != nil {
			log.Warn("desktop: a test credential wasn't deleted", "origin", origin, "err", err)
		}
	}
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
