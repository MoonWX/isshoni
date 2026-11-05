//go:build (windows || (darwin && cgo)) && e2e

package main

import (
	"net/http"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// TestE2ESettings checks the e2e build's two variables (03 §22.4 B, README WA72): the CDP port becomes a WebView2
// argument after the autoplay one, the data folder replaces FOLDERID_LocalAppData, and a malformed value is refused.
func TestE2ESettings(t *testing.T) {
	abs := `/tmp/isshoni-cdp-1`
	if runtime.GOOS == "windows" {
		abs = `C:\Temp\isshoni-cdp-1`
	}
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	args, dir, err := e2eSettings(env(nil))
	if err != nil || args != nil || dir != "" {
		t.Errorf("unset: %v %q %v; want the defaults", args, dir, err)
	}

	args, dir, err = e2eSettings(env(map[string]string{e2eCDPPortEnv: " 9333 ", e2eDataDirEnv: abs + string(filepath.Separator)}))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(args, []string{"--remote-debugging-port=9333"}) {
		t.Errorf("args %v", args)
	}
	if dir != filepath.Clean(abs) {
		t.Errorf("data folder %q, want %q", dir, filepath.Clean(abs))
	}

	for _, bad := range []map[string]string{
		{e2eCDPPortEnv: "0"},
		{e2eCDPPortEnv: "65536"},
		{e2eCDPPortEnv: "-1"},
		{e2eCDPPortEnv: "9333 --disable-web-security"},
		{e2eDataDirEnv: "relative"},
	} {
		if _, _, err := e2eSettings(env(bad)); err == nil {
			t.Errorf("%v: no error", bad)
		} else {
			for k := range bad {
				if !strings.Contains(err.Error(), k) {
					t.Errorf("%v: error %q doesn't name %s", bad, err, k)
				}
			}
		}
	}

	if !flavor.e2e || flavor.production || flavor.dev {
		t.Errorf("flavor %+v: want exactly e2e", flavor)
	}
}

// TestAppOptionsE2EPort checks that appOptions appends the e2e build's arguments after the autoplay one.
func TestAppOptionsE2EPort(t *testing.T) {
	saved := flavor.browserArgs
	t.Cleanup(func() { flavor.browserArgs = saved })
	flavor.browserArgs = []string{"--remote-debugging-port=9333"}
	o := appOptions(http.NotFoundHandler(), func(h http.Handler) http.Handler { return h }, "", nil)
	want := []string{autoplayArg, "--remote-debugging-port=9333"}
	if !slices.Equal(o.Windows.AdditionalBrowserArgs, want) {
		t.Errorf("AdditionalBrowserArgs %v, want %v", o.Windows.AdditionalBrowserArgs, want)
	}
}
