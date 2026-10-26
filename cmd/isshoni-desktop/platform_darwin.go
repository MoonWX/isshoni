//go:build darwin && cgo

package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"github.com/MoonWX/isshoni/internal/desktop/assets"
	"github.com/MoonWX/isshoni/internal/desktop/i18n"
	"github.com/MoonWX/isshoni/internal/protocol"
)

// assetOrigin is the origin of the SPA's pages on macOS: Wails v3's wails:// scheme in WKWebView. 02 reserves it as
// the second entry of protocol.DesktopOrigins() (the later DesktopAssetOriginMacOS, M3).
var assetOrigin = protocol.DesktopOrigins()[1]

// assetGuard is the /wails/* guard for WKWebView's URL-scheme handler, which gets neither Origin nor Sec-Fetch-Site
// (assets.GuardWKWebView: such a runtime call must carry the runtime's own header). M3's macOS release re-checks
// this exception against its own webview (03 §21).
func assetGuard() func(http.Handler) http.Handler { return assets.GuardWKWebView(assetOrigin) }

// checkWebView2 is always ok on macOS: WebView2 is Windows-only (03 §5.4).
func checkWebView2() (version string, ok bool) { return "", true }

// webView2Missing is never called on macOS.
func webView2Missing(*i18n.Catalog) {}

// clientOS is BridgeConfig.App.OS: the dev build of 03 D3 (the macOS app itself is M3).
const clientOS = protocol.ClientOSMacOS

// defaultDeviceName is 02's device name when the computer has no name (AppInfo.DeviceName).
const defaultDeviceName = "Mac"

// dataRoot is the macOS dev build's data folder: the same folder credstore's development file store uses
// (os.UserConfigDir()/isshoni-dev), so the dev build's servers, settings and logs never mix with M3's app.
func dataRoot() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(dir) {
		return "", errors.New("the user's config folder is not an absolute path")
	}
	return filepath.Join(dir, "isshoni-dev"), nil
}

// profileDir is the user's home folder, which the log writes as %USERPROFILE%.
func profileDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

// webviewDataDir is "" on macOS: WKWebView keeps its default store (the WebView2 folder is Windows-only, 03 §5.4).
func webviewDataDir() (string, error) { return "", nil }

// osBuild is 0 on macOS: BridgeConfig.App.OSBuild is a Windows build number.
func osBuild() int { return 0 }
