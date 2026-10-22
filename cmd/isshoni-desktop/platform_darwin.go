//go:build darwin && cgo

package main

import (
	"net/http"

	"github.com/MoonWX/isshoni/internal/desktop/assets"
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
func webView2Missing() {}

// clientOS is BridgeConfig.App.OS: the dev build of 03 D3 (the macOS app itself is M3).
const clientOS = protocol.ClientOSMacOS

// webviewDataDir is "" on macOS: WKWebView keeps its default store (the WebView2 folder is Windows-only, 03 §5.4).
func webviewDataDir() (string, error) { return "", nil }

// osBuild is 0 on macOS: BridgeConfig.App.OSBuild is a Windows build number.
func osBuild() int { return 0 }
