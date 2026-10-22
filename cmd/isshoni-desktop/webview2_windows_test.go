//go:build windows && !isshoni_hw

package main

import (
	"strings"
	"testing"

	"github.com/MoonWX/isshoni/internal/desktop/i18n"
	"github.com/MoonWX/isshoni/web"
)

// TestWebView2Usable is 03 §5.4's minimum, Evergreen 120.0, compared part by part as numbers.
func TestWebView2Usable(t *testing.T) {
	for pv, want := range map[string]bool{
		"":                false, // no value
		"0.0.0.0":         false, // EdgeUpdate's value after an uninstall
		"119.9.9.9":       false,
		"119.0.9999.9999": false,
		"120.0.0.0":       true,
		"120.0.2210.91":   true,
		"131.0.2903.70":   true,
		"1000.0.0.0":      true, // numbers, not strings: "1000" sorts below "120" as text
		"99.0.0.0":        false,
		"120":             true,
		" 131.0.2903.70 ": true,
		"131.0.2903.70.1": false,
		"131.0.x.70":      false,
		"131..0.0":        false,
		"-131.0.0.0":      false,
	} {
		if got := webview2Usable(pv); got != want {
			t.Errorf("webview2Usable(%q) = %v, want %v", pv, got, want)
		}
	}
}

// TestWebView2Message: 03 §5.4's sentence, Microsoft's download and the project site's anchor.
func TestWebView2Message(t *testing.T) {
	cat, err := i18n.Load(web.DesktopCatalog(), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := webview2Message(cat)
	for _, want := range []string{"isshoni needs the Microsoft Edge WebView2 Runtime",
		"https://developer.microsoft.com/microsoft-edge/webview2/",
		"https://moonwx.github.io/isshoni/troubleshooting#desktop-webview2"} {
		if !strings.Contains(m, want) {
			t.Errorf("message %q lacks %q", m, want)
		}
	}
}

// TestCheckWebView2 runs the real check (06 §6.5's registry keys) and checks it agrees with webview2Usable; whether
// this machine has a runtime is not the test's business.
func TestCheckWebView2(t *testing.T) {
	v, ok := checkWebView2()
	t.Logf("WebView2 runtime %q, usable %v", v, ok)
	if ok != webview2Usable(v) {
		t.Fatalf("checkWebView2() = %q, %v; webview2Usable says %v", v, ok, webview2Usable(v))
	}
}
