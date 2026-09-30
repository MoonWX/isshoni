package auth

import "strings"

// uaMaxBytes bounds the part of a User-Agent header that DescribeUserAgent reads; real ones are under 300 bytes.
const uaMaxBytes = 512

// DescribeUserAgent turns a User-Agent header into a session name such as "Chrome on Windows", "Edge on macOS",
// "Safari on iPhone", "Firefox on Linux" or "Samsung Internet on Android" (03 §7.4). An unknown browser gives
// "Browser on <OS>", an unknown OS just the browser, and neither "Browser". The result is always one of a fixed set
// of labels, never text from the header, and the raw header is never stored.
//
// iPadOS Safari sends a macOS User-Agent by default, so it shows as "Safari on macOS". On iOS every other WebKit
// client without a browser token (a Home Screen web app, an in-app browser) shows as Safari.
func DescribeUserAgent(ua string) string {
	if len(ua) > uaMaxBytes {
		ua = ua[:uaMaxBytes]
	}
	platform := uaOS(ua)
	browser := uaBrowser(ua, platform)
	switch {
	case browser != "" && platform != "":
		return browser + " on " + platform
	case browser != "":
		return browser
	case platform != "":
		return "Browser on " + platform
	}
	return "Browser"
}

// uaOS names the operating system, or "". The order matters: iOS User-Agents say "like Mac OS X", Android ones
// say "Linux", and ChromeOS ones say "X11".
func uaOS(ua string) string {
	has := func(s string) bool { return strings.Contains(ua, s) }
	switch {
	case has("iPhone"):
		return "iPhone"
	case has("iPad"):
		return "iPad"
	case has("iPod"):
		return "iPod"
	case has("Android"):
		return "Android"
	case has("CrOS"):
		return "ChromeOS"
	case has("Windows"):
		return "Windows"
	case has("Macintosh") || has("Mac OS X"):
		return "macOS"
	case has("Linux") || has("X11"):
		return "Linux"
	}
	return ""
}

// uaBrowser names the browser, or "". Chromium-based browsers also send "Chrome/" and "Safari/", and every iOS
// browser is WebKit, so the specific tokens are checked first.
func uaBrowser(ua, platform string) string {
	has := func(s string) bool { return strings.Contains(ua, s) }
	switch {
	case has("SamsungBrowser/"):
		return "Samsung Internet"
	case has("Edg/") || has("EdgA/") || has("EdgiOS/") || has("Edge/"):
		return "Edge"
	case has("OPR/") || has("OPiOS/") || has("OPT/"):
		return "Opera"
	case has("Vivaldi/"):
		return "Vivaldi"
	case has("Firefox/") || has("FxiOS/"):
		return "Firefox"
	case has("CriOS/"):
		return "Chrome"
	case has("; wv)"): // Android WebView: an app's embedded browser
		return "WebView"
	case has("Chromium/"):
		return "Chromium"
	case has("Chrome/"):
		return "Chrome"
	case has("AppleWebKit/") && (platform == "iPhone" || platform == "iPad" || platform == "iPod" || platform == "macOS"):
		return "Safari"
	}
	return ""
}
