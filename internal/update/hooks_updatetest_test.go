//go:build updatetest

package update

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The updatetest hooks (06 §8.11). Run them with
//
//	go test -tags updatetest -run TestTestHooks ./internal/update/
//
// (manifest_test.go's TestEligible and TestCheckChannel assume a build without the hooks).
func TestTestHooks(t *testing.T) {
	if !updateTestBuild || testHooks == nil || !Eligible("0.0.1-ci.1") {
		t.Fatal("the updatetest hooks are not installed")
	}
	active, backup := newTestSigner(t, "isshoni-update-1"), newTestSigner(t, "isshoni-update-backup-1")
	signers := filepath.Join(t.TempDir(), "update_signers")
	lines := "isshoni-update namespaces=\"isshoni-update\" " + active.authorizedKey() + " " + active.Name + "\n" +
		"isshoni-update namespaces=\"isshoni-update\" " + backup.authorizedKey() + " " + backup.Name + "\n"
	if err := os.WriteFile(signers, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}

	// Without the variables: the embedded keys (none in this binary) and GitHub.
	u, err := New(Options{CurrentVersion: "0.0.1-ci.1", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if st := u.Status(); st.State != StateDisabled || st.ErrorCode != CodeNoKeys || u.env.ep.releases != ReleasesURL {
		t.Errorf("without the variables: %+v, %+v", st, u.env.ep)
	}

	// A test base over plain http on loopback, as the updater e2e's relserve, and the test signers.
	installer := installerBytes(100, 4)
	var base string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if name, ok := strings.CutPrefix(r.URL.Path, "/latest/download/"); ok {
			redirect(w, base+"/download/v0.0.1-ci.2/"+name)
			return
		}
		files := map[string][]byte{}
		rel := newRelease(t, active, base, "0.0.1-ci.2", installer)
		files[ManifestName], files[SignatureName], files[rel.name] = rel.manifest, rel.sig, rel.installer
		if name, ok := strings.CutPrefix(r.URL.Path, "/download/v0.0.1-ci.2/"); ok && files[name] != nil {
			_, _ = w.Write(files[name])
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	base = srv.URL
	t.Setenv(envTestBase, base)
	t.Setenv(envTestSigners, signers)
	u, err = New(Options{CurrentVersion: "0.0.1-ci.1", DataDir: t.TempDir(), Channel: func() Channel { return ChannelStable }})
	if err != nil {
		t.Fatal(err)
	}
	if u.disabledCode != "" || len(u.keys) != 2 || !u.keys[1].Backup || u.env.ep.releases != base || !u.rules.testBuild {
		t.Fatalf("with the variables: disabled %q, %d keys, %+v", u.disabledCode, len(u.keys), u.env.ep)
	}
	// The manifest verifies and is newer; this copy then stops at the artifact (no installer for this OS) or at the
	// install folder (this test binary is not installed).
	want := CodeNoArtifact
	if runtime.GOOS == "windows" && runtime.GOARCH == "amd64" {
		want = CodeNotInstalled
	}
	st, err := u.CheckNow(t.Context())
	if st.State != StateError || st.ErrorCode != want || st.Available != "0.0.1-ci.2" || CodeOf(err) != want {
		t.Errorf("CheckNow on the test base = %+v, %v; want error %s with 0.0.1-ci.2", st, err, want)
	}

	t.Setenv(envTestBase, "http://localhost:8123")
	if _, err := New(Options{DataDir: t.TempDir()}); err == nil || !strings.Contains(err.Error(), envTestBase) {
		t.Errorf("New with a bad test base = %v", err)
	}
	t.Setenv(envTestBase, "")
	t.Setenv(envTestSigners, filepath.Join(t.TempDir(), "missing"))
	if _, err := New(Options{DataDir: t.TempDir()}); err == nil || !strings.Contains(err.Error(), envTestSigners) {
		t.Errorf("New with a missing signers file = %v", err)
	}
}

// newRelease returns a ci release under a test base, as tools/desktoppkg manifest -releases writes it.
func newRelease(t *testing.T, s testSigner, base, v string, installer []byte) release {
	t.Helper()
	fg := &fakeGitHub{t: t, assets: map[string][]byte{}}
	rel := fg.publish(s, v, installer, func(f map[string]any) {
		f["notesUrl"] = base + "/tag/v" + v
		f["artifacts"].([]any)[0].(map[string]any)["url"] = base + "/download/v" + v + "/isshoni-desktop_" + v +
			"_windows_amd64_setup.exe"
	})
	return rel
}
