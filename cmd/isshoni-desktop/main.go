//go:build windows || (darwin && cgo)

package main

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/MoonWX/isshoni/internal/desktop/assets"
	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/version"
	"github.com/MoonWX/isshoni/web"
)

// appID is the Wails application name: the toast AppID, the process AppUserModelID and the single-instance id
// (README §4 Naming, 03 §13.3, §15, W8).
const appID = "io.isshoni.desktop"

// The main window (03 §4.4).
const (
	windowTitle     = "isshoni"
	windowWidth     = 1200
	windowHeight    = 760
	windowMinWidth  = 800
	windowMinHeight = 560
)

// autoplayArg lets viewer audio play without a click (03 §5.4, W13).
const autoplayArg = "--autoplay-policy=no-user-gesture-required"

func main() {
	os.Exit(run(os.Args[1:]))
}

// run starts the app and returns its exit code. So far it is 03 §13.4's step 7 without the app's services: the
// environment scrub, the WebView2 runtime check, the asset handler and Wails with the main window. WA41 puts the mode
// dispatch, single instance, the running mutex and the rest of §13.4 in front of it.
func run(args []string) int {
	log := setupLogging(args, nil)

	// Before application.New: Wails reads its updater-helper variables there, and WebView2 reads WEBVIEW2_* when
	// the window's environment is created.
	for _, name := range scrubEnv(os.Environ(), flavor.production) {
		if err := os.Unsetenv(name); err != nil {
			log.Warn("desktop: environment variable not removed", "err", err)
		}
	}

	// 03 §5.4: without a current WebView2 runtime the window would stay blank (the pinned Wails only logs it).
	if v, ok := checkWebView2(); !ok {
		log.Error("desktop: no usable WebView2 runtime", "version", v, "min", webview2Min)
		webView2Missing()
		return 1
	}

	dataDir, err := webviewDataDir()
	if err != nil {
		log.Error("desktop: no folder for the WebView2 profile", "err", err)
		return 1
	}
	handler, err := assets.New(assets.Options{
		Dist:      web.Dist(),
		Bridge:    bridgeConfig(appFacts()),
		Origin:    assetOrigin,
		Dev:       insecureLoopback(),
		DevServer: devServerOrigin(),
	})
	if err != nil {
		log.Error("desktop: the bundled web app is unusable", "err", err)
		return 1
	}
	// Wails' own logger never logs below Info, whatever --log-level says: at Debug the pinned Wails logs every bound
	// call's raw arguments and result (03 §17).
	wailsLog := logx.New(logx.Options{}).With("component", "wails")
	app := application.New(appOptions(handler, assetGuard(), dataDir, wailsLog))
	s := newShell(app, mainWindowOptions())
	log.Info("desktop: starting", "version", version.Version(), "os", runtime.GOOS, "dev", insecureLoopback())
	if err := s.Run(); err != nil {
		log.Error("desktop: the app stopped", "err", err)
		return 1
	}
	return 0
}

// setupLogging makes the process's default logger and returns the app's own, which adds component=app. The default
// stays unscoped: packages that log through slog.Default() name their own component (internal/desktop/assets adds
// component=assets), and a second component key would make a JSON line with a duplicate key. out nil is stderr.
func setupLogging(args []string, out io.Writer) *slog.Logger {
	level := new(slog.LevelVar)
	if debugLogging(args) {
		level.Set(slog.LevelDebug)
	}
	base := logx.New(logx.Options{Level: level, Out: out})
	slog.SetDefault(base)
	return base.With("component", "app")
}

// debugLogging reports --log-level debug (or --log-level=debug) among args (03 §4.2).
func debugLogging(args []string) bool {
	for i, a := range args {
		if a == "--log-level=debug" || (a == "--log-level" && i+1 < len(args) && args[i+1] == "debug") {
			return true
		}
	}
	return false
}

// scrubbedEverywhere are variables removed in every build: FRONTEND_DEVSERVER_URL would move Wails' start URL to
// another port, and so the page off the asset origin the Guard accepts (the app's own handler serves the SPA, never
// Wails' dev proxy); WAILS_UPDATER_HELPER* would make application.New run Wails' own updater helper, which swaps
// the app's files (isshoni's updates are 06's, never Wails').
var scrubbedEverywhere = []string{"FRONTEND_DEVSERVER_URL", "WAILS_UPDATER_HELPER"}

// wailsWebView2Values are the values the pinned Wails gives five WEBVIEW2_* variables on Windows, in every build, at
// package init (before main) and again right before it creates the webview environment, overwriting what the parent
// passed: a variable that exists makes WebView2 skip its registry override of the same setting
// (internal/webview2/webviewloader, preventEnvAndRegistryOverrides). scrubEnv leaves these values in place. So in
// e2e builds too, which keep WEBVIEW2_*, a parent's WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS never reaches the webview
// at beta.34: 03 §22.4 B's CDP port needs another way in (WA72).
var wailsWebView2Values = map[string]string{
	"WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS": "",
	"WEBVIEW2_PIPE_FOR_SCRIPT_DEBUGGER":     "",
	"WEBVIEW2_BROWSER_EXECUTABLE_FOLDER":    "",
	"WEBVIEW2_USER_DATA_FOLDER":             "",
	"WEBVIEW2_RELEASE_CHANNEL_PREFERENCE":   "0",
}

// scrubEnv returns the names of the variables of environ to unset before Wails starts (03 §5.4, W19): in production
// builds every name that starts with WEBVIEW2_ (compared case-insensitively, as Windows compares names), except the
// placeholders of wailsWebView2Values, so nothing outside the app can start its webview with a debugging port or a
// script debugger; in every build the names of scrubbedEverywhere (FRONTEND_DEVSERVER_URL exactly,
// WAILS_UPDATER_HELPER as a prefix). e2e builds keep WEBVIEW2_*.
func scrubEnv(environ []string, production bool) []string {
	var names []string
	for _, kv := range environ {
		name, value, _ := strings.Cut(kv, "=")
		if name == "" { // Windows' per-drive "=C:" entries
			continue
		}
		upper := strings.ToUpper(name)
		placeholder, isWails := wailsWebView2Values[upper]
		keep := isWails && value == placeholder
		switch {
		case production && strings.HasPrefix(upper, "WEBVIEW2_") && !keep:
		case upper == scrubbedEverywhere[0]:
		case strings.HasPrefix(upper, scrubbedEverywhere[1]):
		default:
			continue
		}
		names = append(names, name)
	}
	return names
}

// appFacts describes this build for bridge.js.
func appFacts() assets.AppFacts {
	return assets.AppFacts{Version: version.Version(), OS: string(clientOS), OSBuild: osBuild(), Arch: runtime.GOARCH}
}

// bridgeConfig is the asset handler's BridgeConfig source. Until the app's servers exist (WA41, WA48) there is no
// active server: bridge.js says server null and connect-src is 'self' only.
func bridgeConfig(facts assets.AppFacts) func() assets.BridgeConfig {
	return func() assets.BridgeConfig {
		return assets.BridgeConfig{
			V:         assets.BridgeConfigVersion,
			Transport: "wails",
			App:       facts,
			ServerIDs: []string{},
			Dev:       insecureLoopback(),
		}
	}
}

// appOptions are the Wails application's options (03 §4, §5.4).
func appOptions(handler http.Handler, guard func(http.Handler) http.Handler, webviewData string,
	wailsLog *slog.Logger) application.Options {
	return application.Options{
		Name:        appID,
		Description: windowTitle,
		Logger:      wailsLog,
		LogLevel:    slog.LevelInfo,
		Assets: application.AssetOptions{
			Handler:    handler,
			Middleware: application.Middleware(guard),
			// The asset server's request lines hold the page's path, a deep link's room id included (03 §17).
			DisableLogging: true,
		},
		Windows: application.WindowsOptions{
			WebviewUserDataPath:   webviewData,
			AdditionalBrowserArgs: []string{autoplayArg},
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
	}
}

// deniedPermissions are the webview permissions the page never gets. Setting any of them also stops Wails from
// granting every permission without a prompt, its default when a window has no policy; the kinds left out keep the
// webview's own prompt. Camera, microphone and geolocation are also off in the page's Permissions-Policy (03 §5.3).
var deniedPermissions = map[application.PermissionType]application.Permission{
	application.PermissionMicrophone:    application.PermissionDeny,
	application.PermissionCamera:        application.PermissionDeny,
	application.PermissionGeolocation:   application.PermissionDeny,
	application.PermissionNotifications: application.PermissionDeny,
}

// mainWindowOptions are the main window's options (03 §4.4) with the WebView2 settings of 03 §5.4 that the pinned
// Wails exposes (W9). DevTools follow the build: Wails turns them off with -tags production; the default context
// menu is off in production too (Wails keeps it on in dev and e2e builds). Wails itself always turns off the status
// bar, the browser accelerator keys and swipe navigation.
func mainWindowOptions() application.WebviewWindowOptions {
	return application.WebviewWindowOptions{
		Name:                       "main",
		Title:                      windowTitle,
		Width:                      windowWidth,
		Height:                     windowHeight,
		MinWidth:                   windowMinWidth,
		MinHeight:                  windowMinHeight,
		InitialPosition:            application.WindowCentered,
		URL:                        "/",
		ZoomControlEnabled:         false,
		DefaultContextMenuDisabled: true,
		EnableFileDrop:             false,
		Permissions:                deniedPermissions,
		Windows: application.WindowsWindow{
			GeneralAutofillEnabled:  false,
			PasswordAutosaveEnabled: false,
			EnableSwipeGestures:     false,
		},
	}
}
