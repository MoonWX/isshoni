package protocol

// The desktop app's asset origins (M2 02 §6.4). The desktop app's bundled SPA runs in a webview whose pages have the
// origin of Wails' asset server. The server accepts these origins for CORS on /api/v1 and on /ws, but only with bearer
// tokens: every Wails app on a PC has the same origin, so the origin is no identity, and requests from it never use
// cookies (02 decision 9). The server reads the list only through config.Site.ExtraOrigins, which the wiring fills from
// DesktopOrigins (02 §6.3 item 1).

// DesktopAssetOriginWindows is the origin the desktop app's bundled SPA has on Windows (Wails v3's asset server in
// WebView2). 03-desktop-app passes it as its asset handler's Options.Origin and checks it against the pinned Wails.
//
//nolint:gosec // G101 false positive: a public origin, not a credential.
const DesktopAssetOriginWindows = "http://wails.localhost"

// later (M3): const DesktopAssetOriginMacOS = "wails://localhost"

// DesktopOrigins returns a new slice of every origin a desktop app's SPA may have; DesktopOrigins()[0] is
// DesktopAssetOriginWindows. In order: Wails v3 on Windows, Wails v3 on macOS (M3; the value of the later
// DesktopAssetOriginMacOS), then three spellings of other Wails builds. The origin is no credential, so a spare entry
// costs nothing, and it keeps an app whose Wails bump changed the spelling working against the previous server
// release (N−1). A release that needs a new spelling adds it here one release before any app uses it. Each entry is
// an exact, lower-case origin without a trailing slash, as browsers send it in the Origin header.
func DesktopOrigins() []string {
	return []string{
		DesktopAssetOriginWindows, // Wails v3, Windows (WebView2)
		"wails://localhost",       // Wails v3, macOS (WKWebView), M3
		"https://wails.localhost", // other Wails builds (M1 03 §7.6)
		"wails://wails",
		"wails://wails.localhost",
	}
}
