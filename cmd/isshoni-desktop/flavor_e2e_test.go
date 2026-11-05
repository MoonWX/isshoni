//go:build (windows || (darwin && cgo)) && e2e

package main

import (
	"log/slog"
	"net/http"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/client/credstore"
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

// TestE2ECredentials checks the e2e build's credential store (03 §22.4 B): a random "isshoni-test:<16 hex>:" prefix
// in place of the user's isshoni:v1:, used by every credstore.Open of the app, and dropTestCredentials deleting what
// is under that prefix and nothing else. It writes only under test prefixes (Credential Manager on Windows, a
// temporary file store on macOS), never under isshoni:v1:.
func TestE2ECredentials(t *testing.T) {
	re := regexp.MustCompile(`^isshoni-test:[0-9a-f]{16}:$`)
	if !re.MatchString(flavor.credPrefix) {
		t.Fatalf("flavor.credPrefix %q, want isshoni-test:<16 hex>:", flavor.credPrefix)
	}
	if a, b := e2eCredPrefix(), e2eCredPrefix(); !re.MatchString(a) || a == b {
		t.Errorf("e2eCredPrefix: %q, %q; want two different test prefixes", a, b)
	}
	root := t.TempDir()
	if o := appCredentialOptions(root); o.Prefix != flavor.credPrefix || o.Dir != root {
		t.Errorf("appCredentialOptions: prefix %q dir %q, want %q %q", o.Prefix, o.Dir, flavor.credPrefix, root)
	}

	store, err := credstore.Open(appCredentialOptions(root))
	if err != nil {
		t.Fatal(err)
	}
	// Another run's prefix in the same store: dropTestCredentials must leave it alone.
	other, err := credstore.Open(credstore.Options{Prefix: e2eCredPrefix(), Dir: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, s := range []credstore.Store{store, other} {
			if origins, err := s.List(); err == nil {
				for _, o := range origins {
					_ = s.Delete(o)
				}
			}
		}
	})
	cred := credstore.Credential{RefreshToken: "isr_test", DeviceID: "dev_test", SavedAt: time.Now()}
	for _, origin := range []string{"http://127.0.0.1:1", "http://127.0.0.1:2"} {
		if err := store.Put(origin, cred); err != nil {
			t.Fatal(err)
		}
	}
	if err := other.Put("http://127.0.0.1:3", cred); err != nil {
		t.Fatal(err)
	}

	dropTestCredentials(store, slog.Default())
	if origins, err := store.List(); err != nil || len(origins) != 0 {
		t.Errorf("after dropTestCredentials: %v %v; want none", origins, err)
	}
	if origins, err := other.List(); err != nil || !slices.Equal(origins, []string{"http://127.0.0.1:3"}) {
		t.Errorf("the other prefix after dropTestCredentials: %v %v; want its one entry", origins, err)
	}
}
