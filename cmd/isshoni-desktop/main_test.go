//go:build (windows || (darwin && cgo)) && !isshoni_hw

package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/MoonWX/isshoni/internal/desktop/assets"
	"github.com/MoonWX/isshoni/internal/protocol"
)

func TestScrubEnv(t *testing.T) {
	environ := []string{
		"PATH=/usr/bin",
		"WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS=--remote-debugging-port=9222",
		"WEBVIEW2_PIPE_FOR_SCRIPT_DEBUGGER=isshoni-test-1",
		"WebView2_Wait_For_Script_Debugger=1", // Windows compares names case-insensitively
		"WEBVIEW2_X=1",
		"WEBVIEW2_=",
		// Wails' own placeholders stay (main.go wailsWebView2Values); with any other value they go.
		"WEBVIEW2_USER_DATA_FOLDER=",
		"WEBVIEW2_RELEASE_CHANNEL_PREFERENCE=0",
		"WEBVIEW2_BROWSER_EXECUTABLE_FOLDER=C:\\evil",
		"WEBVIEW2_RELEASE_CHANNEL_PREFERENCE=1",
		"XWEBVIEW2_Y=1",
		"FRONTEND_DEVSERVER_URL=http://localhost:5173",
		"frontend_devserver_url=x",
		"FRONTEND_DEVSERVER_URL_X=1",
		"WAILS_UPDATER_HELPER=1",
		"WAILS_UPDATER_HELPER_TARGET=x",
		"WAILS_OTHER=1",
		"=C:=C:\\work", // a per-drive entry of Windows
		"ISSHONI_DESKTOP_DEV_URL=http://localhost:5173",
	}
	always := []string{"FRONTEND_DEVSERVER_URL", "frontend_devserver_url", "WAILS_UPDATER_HELPER",
		"WAILS_UPDATER_HELPER_TARGET"}
	if got := scrubEnv(environ, false); !slices.Equal(got, always) {
		t.Errorf("not production: %v, want %v", got, always)
	}
	want := []string{"WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS", "WEBVIEW2_PIPE_FOR_SCRIPT_DEBUGGER",
		"WebView2_Wait_For_Script_Debugger", "WEBVIEW2_X", "WEBVIEW2_", "WEBVIEW2_BROWSER_EXECUTABLE_FOLDER",
		"WEBVIEW2_RELEASE_CHANNEL_PREFERENCE", "FRONTEND_DEVSERVER_URL",
		"frontend_devserver_url", "WAILS_UPDATER_HELPER", "WAILS_UPDATER_HELPER_TARGET"}
	if got := scrubEnv(environ, true); !slices.Equal(got, want) {
		t.Errorf("production: %v, want %v", got, want)
	}
}

func TestDebugLogging(t *testing.T) {
	for args, want := range map[string]bool{
		"--log-level debug":                         true,
		"--log-level=debug":                         true,
		"--open-url isshoni://x/ --log-level debug": true,
		"--log-level info":                          false,
		"--log-level":                               false,
		"debug":                                     false,
		"":                                          false,
	} {
		if got := debugLogging(strings.Fields(args)); got != want {
			t.Errorf("%q: %v", args, got)
		}
	}
}

func TestBridgeConfig(t *testing.T) {
	facts := appFacts()
	if facts.Version == "" || facts.OS != string(clientOS) || facts.Arch == "" {
		t.Fatalf("appFacts %+v", facts)
	}
	c := bridgeConfig(facts)()
	if c.V != 1 || c.Transport != "wails" || c.Server != nil || c.ServerIDs == nil || c.Dev != insecureLoopback() {
		t.Fatalf("bridgeConfig %+v", c)
	}
	body, err := assets.BridgeJS(c)
	if err != nil || !strings.Contains(string(body), `"server":null,"serverIds":[]`) {
		t.Fatalf("bridge.js %s (%v)", body, err)
	}
}

// TestAssetOrigin is W1's Go half: the origin the handler and the Guard use is one of 02's desktop origins, and on
// Windows exactly protocol.DesktopAssetOriginWindows.
func TestAssetOrigin(t *testing.T) {
	if !slices.Contains(protocol.DesktopOrigins(), assetOrigin) {
		t.Fatalf("asset origin %q is not in protocol.DesktopOrigins()", assetOrigin)
	}
	if clientOS == protocol.ClientOSWindows && assetOrigin != protocol.DesktopAssetOriginWindows {
		t.Fatalf("asset origin %q on Windows", assetOrigin)
	}
}

func TestWebviewDataDir(t *testing.T) {
	dir, err := webviewDataDir()
	if err != nil {
		t.Fatal(err)
	}
	if clientOS != protocol.ClientOSWindows {
		if dir != "" {
			t.Fatalf("webviewDataDir() = %q off Windows", dir)
		}
		return
	}
	if !filepath.IsAbs(dir) || filepath.Base(dir) != "WebView2" || filepath.Base(filepath.Dir(dir)) != "isshoni" ||
		strings.HasSuffix(dir, `\`) {
		t.Fatalf("webviewDataDir() = %q, want an absolute ...\\isshoni\\WebView2", dir)
	}
}

// TestAppOptions checks the Wails options the app starts with (03 §4.4, §5.4, §17; W9 and W13 as far as Go sets
// them).
func TestAppOptions(t *testing.T) {
	handler := http.NotFoundHandler()
	var guarded bool
	guard := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			guarded = true
			next.ServeHTTP(w, r)
		})
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	o := appOptions(handler, guard, `C:\Users\x\AppData\Local\isshoni\WebView2`, log)
	if o.Name != appID || appID != "io.isshoni.desktop" {
		t.Errorf("Name %q", o.Name)
	}
	if o.Logger != log || o.LogLevel != slog.LevelInfo {
		t.Errorf("Wails' logger must be the one given, at Info (03 §17)")
	}
	if o.Assets.Handler == nil || o.Assets.Middleware == nil || !o.Assets.DisableLogging {
		t.Errorf("Assets %+v", o.Assets)
	}
	o.Assets.Middleware(handler).ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
	if !guarded {
		t.Error("the Guard is not Wails' asset middleware")
	}
	if o.Windows.WebviewUserDataPath != `C:\Users\x\AppData\Local\isshoni\WebView2` {
		t.Errorf("WebviewUserDataPath %q", o.Windows.WebviewUserDataPath)
	}
	if !slices.Equal(o.Windows.AdditionalBrowserArgs, []string{"--autoplay-policy=no-user-gesture-required"}) {
		t.Errorf("AdditionalBrowserArgs %v", o.Windows.AdditionalBrowserArgs)
	}
	if o.SingleInstance != nil || len(o.Services) != 0 || o.Transport != nil {
		t.Errorf("single instance, services and the transport are later slices': %+v", o)
	}
}

func TestMainWindowOptions(t *testing.T) {
	w := mainWindowOptions()
	if w.Title != "isshoni" || w.Width != 1200 || w.Height != 760 || w.MinWidth != 800 || w.MinHeight != 560 ||
		w.URL != "/" || w.InitialPosition != application.WindowCentered || w.Frameless || w.Hidden {
		t.Errorf("window %+v", w)
	}
	// W9: the WebView2 settings Wails exposes.
	if w.ZoomControlEnabled || !w.DefaultContextMenuDisabled || w.EnableFileDrop || w.DevToolsEnabled ||
		w.OpenInspectorOnStartup || w.Windows.GeneralAutofillEnabled || w.Windows.PasswordAutosaveEnabled ||
		w.Windows.EnableSwipeGestures {
		t.Errorf("WebView2 settings %+v", w)
	}
	for _, p := range []application.PermissionType{application.PermissionMicrophone, application.PermissionCamera,
		application.PermissionGeolocation, application.PermissionNotifications} {
		if w.Permissions[p] != application.PermissionDeny {
			t.Errorf("permission %d: %d, want deny", p, w.Permissions[p])
		}
	}
}

// TestAppStartsOverTheBundle builds the asset handler as run does, over a fixture bundle, and serves its index.
func TestAppStartsOverTheBundle(t *testing.T) {
	dist := fstest.MapFS{"index.html": {Data: []byte(
		`<html><head><meta charset="utf-8"></head><body><script src="/boot-check.js"></script></body></html>`)}}
	h, err := assets.New(assets.Options{Dist: dist, Bridge: bridgeConfig(appFacts()), Origin: assetOrigin,
		Dev: insecureLoopback(), DevServer: devServerOrigin()})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, assetOrigin+"/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `<script src="/desktop/bridge.js">`) {
		t.Fatalf("GET /: %d %s", rec.Code, rec.Body.String())
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "connect-src 'self';") {
		t.Fatalf("CSP %q", csp)
	}
}

func TestShellIsNotImplementedYetWhereSaid(t *testing.T) {
	var s wailsShell
	if err := s.OpenURL("https://example.com"); err == nil {
		t.Fatal("OpenURL works before its slice")
	}
}
