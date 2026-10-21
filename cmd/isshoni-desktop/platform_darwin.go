//go:build darwin && cgo

package main

import "github.com/MoonWX/isshoni/internal/protocol"

// assetOrigin is the origin of the SPA's pages on macOS: Wails v3's wails:// scheme in WKWebView. 02 reserves it as
// the second entry of protocol.DesktopOrigins() (the later DesktopAssetOriginMacOS, M3).
var assetOrigin = protocol.DesktopOrigins()[1]

// clientOS is BridgeConfig.App.OS: the dev build of 03 D3 (the macOS app itself is M3).
const clientOS = protocol.ClientOSMacOS

// webviewDataDir is "" on macOS: WKWebView keeps its default store (the WebView2 folder is Windows-only, 03 §5.4).
func webviewDataDir() (string, error) { return "", nil }

// osBuild is 0 on macOS: BridgeConfig.App.OSBuild is a Windows build number.
func osBuild() int { return 0 }
