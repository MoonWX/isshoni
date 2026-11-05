//go:build windows || (darwin && cgo)

package main

import (
	"log/slog"

	"github.com/MoonWX/isshoni/internal/client/credstore"
	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/desktop/assets"
	"github.com/MoonWX/isshoni/internal/media/native"
	"github.com/MoonWX/isshoni/web"
)

// selfCheckEngineOf is --self-check's engine in every build, dev and e2e included: the shipped engine,
// isshoni-media.dll with its FFmpeg DLLs from the exe's folder (native.Open with Options.Dir "", 06 §2.4), never
// the fake. On macOS (no engine before M3) it fails, and the report says so.
func selfCheckEngineOf(log *slog.Logger) func() (selfCheckEngine, error) {
	return func() (selfCheckEngine, error) {
		e, err := native.Open(native.Options{Logger: log})
		if err != nil {
			return nil, err
		}
		return e, nil
	}
}

// checkBundledSPA is --self-check's spa.ok: the asset handler built over the bundled SPA (web.Dist()) as step 7
// builds it, without a server and never from a dev server, serves its index with the bridge tag (03 §13.4).
func checkBundledSPA() error {
	h, err := assets.New(assets.Options{
		Dist:   web.Dist(),
		Bridge: bridgeConfig(appFacts(), nil),
		Origin: assetOrigin,
		Dev:    insecureLoopback(),
	})
	if err != nil {
		return err
	}
	return serveIndex(h, assetOrigin)
}

// openAppCredentials is the app's credential store as step 7 opens it: Credential Manager under isshoni:v1: on
// Windows (an e2e build's own test prefix, flavor.credPrefix), the development file store in the data root on macOS.
func openAppCredentials() (credstore.Store, error) {
	root, err := dataRoot()
	if err != nil {
		return nil, err
	}
	return credstore.Open(appCredentialOptions(root))
}

// openAppServers is 02's servers over store, from servers.json in the data root, as step 7 opens them (without
// OnState: nothing shows them).
func openAppServers(store credstore.Store) (credentialForgetter, error) {
	root, err := dataRoot()
	if err != nil {
		return nil, err
	}
	srv, err := deviceauth.OpenServers(deviceauth.ServersOptions{
		Dir:                   root,
		Store:                 store,
		App:                   appInfo(),
		AllowInsecureLoopback: insecureLoopback(),
		Logger:                slog.Default(),
	})
	if err != nil {
		return nil, err
	}
	return srv, nil
}
