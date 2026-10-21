package protocol

import (
	"net/url"
	"slices"
	"strings"
	"testing"
)

// TestDesktopOrigins: the list is M2 02 §6.4's, in its order and with its exact strings; the Windows origin comes
// first; every call returns a new slice.
func TestDesktopOrigins(t *testing.T) {
	if DesktopAssetOriginWindows != "http://wails.localhost" { //nolint:gosec // G101 false positive: an origin
		t.Errorf("DesktopAssetOriginWindows = %q, want http://wails.localhost", DesktopAssetOriginWindows)
	}
	want := []string{
		"http://wails.localhost",
		"wails://localhost",
		"https://wails.localhost",
		"wails://wails",
		"wails://wails.localhost",
	}
	got := DesktopOrigins()
	if !slices.Equal(got, want) {
		t.Fatalf("DesktopOrigins() = %q\nwant %q", got, want)
	}
	if got[0] != DesktopAssetOriginWindows {
		t.Errorf("DesktopOrigins()[0] = %q, want DesktopAssetOriginWindows", got[0])
	}

	// Exact origins as a browser sends them: scheme and host only, lower case, no trailing slash, no duplicates.
	for _, o := range got {
		u, err := url.Parse(o)
		if err != nil {
			t.Errorf("%q: %v", o, err)
			continue
		}
		if o != strings.ToLower(o) || strings.HasSuffix(o, "/") || u.Path != "" || u.RawQuery != "" ||
			u.Fragment != "" || u.User != nil || u.Scheme+"://"+u.Host != o {
			t.Errorf("%q is not an exact origin", o)
		}
	}
	if len(slices.Compact(slices.Sorted(slices.Values(got)))) != len(got) {
		t.Errorf("DesktopOrigins() has duplicates: %q", got)
	}

	// A new slice on every call: a caller that changes its copy changes nobody else's.
	a, b := DesktopOrigins(), DesktopOrigins()
	if &a[0] == &b[0] {
		t.Fatal("two calls return the same backing array")
	}
	a[0] = "https://evil.example"
	if b[0] != DesktopAssetOriginWindows || DesktopOrigins()[0] != DesktopAssetOriginWindows {
		t.Error("changing one call's slice changed another's")
	}
}
