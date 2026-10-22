//go:build windows

package main

import (
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/MoonWX/isshoni/internal/version"
)

// The WebView2 message box (03 §5.4). WA41's internal/desktop/i18n moves the text to a desktop.native.* key; until
// then it is English only. Neither link is clickable in a message box; Ctrl+C copies the box's text.
const (
	// webview2DownloadURL is Microsoft's WebView2 page with the Evergreen runtime's download (the installer's message
	// names the same page, 06 §6.5).
	webview2DownloadURL = "https://developer.microsoft.com/microsoft-edge/webview2/"
	webview2HelpAnchor  = "troubleshooting#desktop-webview2" // under the project site (03 §23's anchors)
)

// webview2Message is the message box's text.
func webview2Message() string {
	return "isshoni needs the Microsoft Edge WebView2 Runtime, version 120 or newer.\n\n" +
		"Install or update it from Microsoft, then start isshoni again:\n" + webview2DownloadURL + "\n\n" +
		"Help:\n" + version.DocsURL + webview2HelpAnchor
}

// webview2Usable reports whether pv, the runtime version EdgeUpdate records (four dotted numbers, such as
// 131.0.2903.70), is at least webview2Min, compared part by part as numbers. An empty or unparsable value and 0.0.0.0
// (what EdgeUpdate leaves after an uninstall) count as no runtime.
func webview2Usable(pv string) bool {
	v, ok := parseVersion4(pv)
	if !ok || v == [4]uint64{} {
		return false
	}
	minimum, _ := parseVersion4(webview2Min)
	return slices.Compare(v[:], minimum[:]) >= 0
}

// parseVersion4 parses one to four dot-separated decimal parts; missing trailing parts are 0.
func parseVersion4(s string) ([4]uint64, bool) {
	var v [4]uint64
	parts := strings.Split(strings.TrimSpace(s), ".")
	if len(parts) > len(v) {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

// webview2ClientKeys are where EdgeUpdate records the Evergreen WebView2 runtime's version (the value pv), per
// machine and per user. 06 §6.5's installer check reads the same two keys, so the installer and the app agree.
var webview2ClientKeys = []struct {
	root registry.Key
	path string
}{
	{registry.LOCAL_MACHINE, `SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}`},
	{registry.CURRENT_USER, `Software\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}`},
}

// checkWebView2 reports the installed WebView2 runtime's version and whether it is usable (webview2Usable): ok when
// either key holds a usable pv. Otherwise version is the first pv found ("" when neither key has one).
func checkWebView2() (version string, ok bool) {
	for _, k := range webview2ClientKeys {
		pv, found := readRegistryString(k.root, k.path, "pv")
		if !found {
			continue
		}
		if webview2Usable(pv) {
			return pv, true
		}
		if version == "" {
			version = pv
		}
	}
	return version, false
}

// readRegistryString reads a string value; a missing key or value, or one that isn't a string, is not found.
func readRegistryString(root registry.Key, path, name string) (string, bool) {
	k, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
	if err != nil {
		return "", false
	}
	defer func() { _ = k.Close() }()
	v, _, err := k.GetStringValue(name)
	if err != nil {
		return "", false
	}
	return v, true
}

// webView2Missing shows 03 §5.4's message box (no window exists yet; the exe has no console) and returns when the
// user closes it.
func webView2Missing() {
	text, err := windows.UTF16PtrFromString(webview2Message())
	if err != nil {
		return
	}
	caption, err := windows.UTF16PtrFromString(windowTitle)
	if err != nil {
		return
	}
	_, _ = windows.MessageBox(0, text, caption, windows.MB_OK|windows.MB_ICONERROR|windows.MB_SETFOREGROUND)
}
