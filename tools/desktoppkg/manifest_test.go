package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// goldenManifest is the manifest of manifestTree's release, and goldenTestBaseManifest an updater e2e manifest under
// testBase. internal/update's tests parse both, so the writer and the app's parser can't drift apart.
var (
	goldenManifest         = filepath.Join("testdata", "manifest", manifestName)
	goldenTestBaseManifest = filepath.Join("testdata", "manifest", "test-base", manifestName)
)

// testBase is the -releases URL of goldenTestBaseManifest: relserve's origin as ISSHONI_UPDATE_TEST_BASE names it in
// the updater e2e (06 §8.11, §16). internal/update's test of that file uses the same value.
const testBase = "http://127.0.0.1:8123"

const (
	testVersion   = "0.2.1"
	testInstaller = "isshoni-desktop_0.2.1_windows_amd64_setup.exe"
	testDate      = "2026-12-01T11:00:00+01:00" // 10:00 UTC
	revokedHeader = "# SHA256 fingerprints of revoked update keys, one per line (06 §8.12). Empty normally.\n"
)

// manifestTree writes a repository root with the two committed inputs and an installer, and returns the root and
// the installer's path.
func manifestTree(t *testing.T, minVersion, revoked string) (root, installer string) {
	t.Helper()
	root = t.TempDir()
	writeFile(t, filepath.Join(root, "deploy", "desktop", "min-version"), minVersion)
	writeFile(t, filepath.Join(root, "deploy", "keys", "update_revoked"), revoked)
	installer = filepath.Join(root, "dist-desktop", testInstaller)
	writeFile(t, installer, "MZ not really an installer\n")
	return root, installer
}

func TestManifestGolden(t *testing.T) {
	root, installer := manifestTree(t, "0.2.0-rc.1\n", revokedHeader)
	code, stdout, stderr := runCmd(t, "-root", root, "manifest", "-version", "v"+testVersion, "-installer", installer,
		"-date", testDate)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	out := filepath.Join(root, "dist-desktop", manifestName)
	if !strings.Contains(stdout, "wrote "+out+": version 0.2.1, channel stable, minVersion 0.2.0-rc.1, 0 revoked keys") {
		t.Errorf("stdout %q", stdout)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if *update {
		writeFile(t, goldenManifest, string(got))
	}
	want, err := os.ReadFile(goldenManifest)
	if err != nil {
		t.Fatal(err)
	}
	// A Git for Windows checkout (core.autocrlf) has CRLF in the golden file; the manifest itself is always LF.
	if !bytes.Equal(got, bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n"))) {
		t.Errorf("manifest differs from %s (rerun with -update to accept):\n%s", goldenManifest, got)
	}
	if bytes.Contains(got, []byte("\r")) {
		t.Error("the manifest has a CR")
	}
	// check-draft.sh's empty-list rule (06 §11.3).
	if !bytes.Contains(got, []byte("\n  \"revokedKeys\": [],\n")) {
		t.Errorf("no exact `  \"revokedKeys\": [],` line:\n%s", got)
	}
}

// TestManifestGoldenTestBase writes an updater e2e manifest, a ci version under a test releases URL (06 §16).
func TestManifestGoldenTestBase(t *testing.T) {
	const version = "0.0.1-ci.2"
	root, _ := manifestTree(t, "0.2.0-rc.1\n", revokedHeader)
	installer := filepath.Join(root, "dist-desktop", "isshoni-desktop_"+version+"_windows_amd64_setup.exe")
	writeFile(t, installer, "MZ not really an installer either\n")
	out := filepath.Join(root, "relserve", "download", "v"+version, manifestName)
	code, stdout, stderr := runCmd(t, "-root", root, "manifest", "-version", version, "-installer", installer,
		"-date", testDate, "-out", out, "-releases", testBase)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "wrote "+out+": version 0.0.1-ci.2, channel ci, minVersion 0.2.0-rc.1, 0 revoked "+
		"keys, releases URL "+testBase+"\n") {
		t.Errorf("stdout %q", stdout)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if *update {
		writeFile(t, goldenTestBaseManifest, string(got))
	}
	want, err := os.ReadFile(goldenTestBaseManifest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n"))) {
		t.Errorf("manifest differs from %s (rerun with -update to accept):\n%s", goldenTestBaseManifest, got)
	}
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatal(err)
	}
	wantField(t, m, "channel", "ci")
	wantField(t, m, "notesUrl", testBase+"/tag/v0.0.1-ci.2")
	if a := m["artifacts"].([]any)[0].(map[string]any); a["url"] != testBase+"/download/v0.0.1-ci.2/"+filepath.Base(installer) {
		t.Errorf("artifact url %v", a["url"])
	}
}

func TestManifestFields(t *testing.T) {
	fp1 := newFingerprint("leaked")
	fp2 := newFingerprint("other")
	tests := []struct {
		name, version, minVersion, revoked string
		args                               []string // more flags
		check                              func(t *testing.T, m map[string]any)
	}{
		{
			name: "release candidate", version: "0.3.0-rc.2", minVersion: "0.2.0-rc.1\n", revoked: "",
			check: func(t *testing.T, m map[string]any) {
				wantField(t, m, "channel", "beta")
				wantField(t, m, "notesUrl", "https://github.com/MoonWX/isshoni/releases/tag/v0.3.0-rc.2")
			},
		},
		{
			name: "updater e2e build", version: "0.0.1-ci.2", minVersion: "0.2.0-rc.1", revoked: "",
			check: func(t *testing.T, m map[string]any) { wantField(t, m, "channel", "ci") },
		},
		{
			name: "updater e2e build under a release root of relserve", version: "0.0.1-ci.3", minVersion: "0.2.0",
			args: []string{"-releases", "https://127.0.0.1:8443/tampered"},
			check: func(t *testing.T, m map[string]any) {
				wantField(t, m, "notesUrl", "https://127.0.0.1:8443/tampered/tag/v0.0.1-ci.3")
				a := m["artifacts"].([]any)[0].(map[string]any)
				wantField(t, a, "url", "https://127.0.0.1:8443/tampered/download/v0.0.1-ci.3/"+
					"isshoni-desktop_0.0.1-ci.3_windows_amd64_setup.exe")
			},
		},
		{
			name: "the default releases URL named", version: "0.3.0", minVersion: "0.2.0", args: []string{"-releases", releasesURL},
			check: func(t *testing.T, m map[string]any) {
				wantField(t, m, "notesUrl", "https://github.com/MoonWX/isshoni/releases/tag/v0.3.0")
				a := m["artifacts"].([]any)[0].(map[string]any)
				wantField(t, a, "url", "https://github.com/MoonWX/isshoni/releases/download/v0.3.0/"+
					"isshoni-desktop_0.3.0_windows_amd64_setup.exe")
			},
		},
		{
			name: "release dry run", version: "0.0.0-dryrun.15", minVersion: "0.2.0-rc.1\r\n", revoked: "",
			check: func(t *testing.T, m map[string]any) { wantField(t, m, "channel", "ci") },
		},
		{
			name: "revoked keys with CRLF, comments and blank lines", version: "0.3.1", minVersion: "0.3.1\n",
			revoked: "# leak of 2027-01\r\n\r\n" + fp1 + "\r\n  " + fp2 + "  \r\n",
			check: func(t *testing.T, m map[string]any) {
				wantField(t, m, "minVersion", "0.3.1")
				got, _ := json.Marshal(m["revokedKeys"])
				if want, _ := json.Marshal([]string{fp1, fp2}); !bytes.Equal(got, want) {
					t.Errorf("revokedKeys %s, want %s", got, want)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, _ := manifestTree(t, tt.minVersion, tt.revoked)
			installer := filepath.Join(root, "in", "isshoni-desktop_"+tt.version+"_windows_amd64_setup.exe")
			writeFile(t, installer, strings.Repeat("x", 1000))
			out := filepath.Join(root, "out", "m.json")
			args := append([]string{"-root", root, "manifest", "-version", tt.version, "-installer", installer,
				"-date", "2027-01-31T23:59:59.9999Z", "-out", out}, tt.args...)
			code, stdout, stderr := runCmd(t, args...)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			data, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(data, &m); err != nil {
				t.Fatal(err)
			}
			// stdout names the releases URL only when it is a test one.
			var under string
			if i := slices.Index(tt.args, "-releases"); i >= 0 && tt.args[i+1] != releasesURL {
				under = ", releases URL " + tt.args[i+1]
			}
			if !strings.HasSuffix(stdout, " revoked keys"+under+"\n") {
				t.Errorf("stdout %q does not end in %q", stdout, " revoked keys"+under)
			}
			wantField(t, m, "version", tt.version)
			wantField(t, m, "publishedAt", "2027-01-31T23:59:59.999Z")
			wantField(t, m, "expiresAt", "2027-07-30T23:59:59.999Z")
			a := m["artifacts"].([]any)[0].(map[string]any)
			sum := sha256.Sum256([]byte(strings.Repeat("x", 1000)))
			if a["size"] != 1000.0 || a["sha256"] != hex.EncodeToString(sum[:]) {
				t.Errorf("artifact %v", a)
			}
			if tt.check != nil {
				tt.check(t, m)
			}
		})
	}
}

func wantField(t *testing.T, m map[string]any, key, want string) {
	t.Helper()
	if m[key] != want {
		t.Errorf("%s = %v, want %q", key, m[key], want)
	}
}

// newFingerprint returns the fingerprint of a throwaway ed25519 key derived from name.
func newFingerprint(name string) string {
	return ssh.FingerprintSHA256(testPublicKey(name))
}

func testPublicKey(name string) ssh.PublicKey {
	seed := sha256.Sum256([]byte("desktoppkg test key " + name))
	pub, err := ssh.NewPublicKey(ed25519.NewKeyFromSeed(seed[:]).Public())
	if err != nil {
		panic(err)
	}
	return pub
}

func TestManifestRefusals(t *testing.T) {
	active := testPublicKey("isshoni-update-1")
	backup := testPublicKey("isshoni-update-backup-1")
	signers := `isshoni-update namespaces="isshoni-update" ` + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(active))) +
		" isshoni-update-1\n" + `isshoni-update namespaces="isshoni-update" ` +
		strings.TrimSpace(string(ssh.MarshalAuthorizedKey(backup))) + " isshoni-update-backup-1\n"
	leaked := newFingerprint("leaked")

	tests := []struct {
		name       string
		args       []string // after -installer; the version defaults to 0.2.1
		minVersion string
		revoked    string
		signers    string // deploy/keys/update_signers; "" leaves it out
		installer  string // content; "" means the default
		code       int
		err        string
	}{
		{name: "no version", args: []string{"-version", ""}, code: 2, err: "needs -version and -installer"},
		{name: "extra argument", args: []string{"x"}, code: 2, err: "takes no arguments"},
		{name: "bad date", args: []string{"-date", "yesterday"}, code: 2, err: `-date "yesterday" is not RFC 3339`},
		{name: "build metadata", args: []string{"-version", "0.2.1+abc"}, code: 1, err: "not a SemVer version without build metadata"},
		{name: "dev version", args: []string{"-version", "0.0.0-dev+1a2b3c"}, code: 1, err: "not a SemVer"},
		{name: "short version", args: []string{"-version", "0.2"}, code: 1, err: "not a SemVer"},
		{name: "installer of another version", args: []string{"-version", "0.2.2"}, code: 1, err: "want isshoni-desktop_0.2.2_windows_amd64_setup.exe"},
		{name: "empty installer", installer: "-", code: 1, err: "the installer has 0 bytes"},
		{name: "minVersion above the version", minVersion: "0.3.0\n", code: 1, err: "above the release's version 0.2.1"},
		{name: "minVersion above an rc", args: []string{"-version", "0.3.0-rc.1"}, minVersion: "0.3.0\n", code: 1, err: "above the release's version"},
		{name: "minVersion with v", minVersion: "v0.2.0\n", code: 1, err: "must be one SemVer line"},
		{name: "minVersion two lines", minVersion: "0.2.0\n0.2.1\n", code: 1, err: "must be one SemVer line"},
		{name: "minVersion empty", minVersion: "\n", code: 1, err: "must be one SemVer line"},
		{name: "revoked: not a fingerprint", revoked: "isshoni-update-1\n", code: 1, err: "update_revoked line 1: \"isshoni-update-1\" is not a fingerprint"},
		{name: "revoked: twice", revoked: leaked + "\n" + leaked + "\n", code: 1, err: "line 2: " + leaked + " is listed twice"},
		{name: "revoked: the backup key", revoked: ssh.FingerprintSHA256(backup) + "\n", signers: signers, code: 1, err: "is the backup key isshoni-update-backup-1, which apps never revoke"},
		{name: "revoked: a key the file still lists", revoked: ssh.FingerprintSHA256(active) + "\n", signers: signers, code: 1, err: "is isshoni-update-1, which deploy/keys/update_signers still lists"},
		{name: "revoked: too many", revoked: manyFingerprints(65), code: 1, err: "lists 65 keys, above the 64"},
		{name: "signers unreadable", signers: "isshoni-update nothing\n", code: 1, err: "update_signers line 1: no key"},
		{name: "test releases URL for a stable version", args: []string{"-releases", testBase}, code: 1, err: "a stable release's manifest points into https://github.com/MoonWX/isshoni/releases"},
		{name: "test releases URL for an rc", args: []string{"-version", "0.3.0-rc.1", "-releases", "https://example.com/releases"}, minVersion: "0.2.0\n", code: 1, err: "another releases URL is only for a ci version"},
		{name: "releases URL: http elsewhere", args: ciReleases("http://localhost:8123"), code: 1, err: "want https://…, or http://127.0.0.1:<port>"},
		{name: "releases URL: http without a port", args: ciReleases("http://127.0.0.1"), code: 1, err: "want https://…, or http://127.0.0.1:<port>"},
		{name: "releases URL: port 0", args: ciReleases("http://127.0.0.1:0"), code: 1, err: "want https://…, or http://127.0.0.1:<port>"},
		{name: "releases URL: port with a leading zero", args: ciReleases("http://127.0.0.1:08123"), code: 1, err: "want https://…, or http://127.0.0.1:<port>"},
		{name: "releases URL: another scheme", args: ciReleases("ftp://127.0.0.1:21"), code: 1, err: "want https://…, or http://127.0.0.1:<port>"},
		{name: "releases URL: no host", args: ciReleases("https:///releases"), code: 1, err: "want https://…, or http://127.0.0.1:<port>"},
		{name: "releases URL: no scheme", args: ciReleases("127.0.0.1:8123"), code: 1, err: `-releases "127.0.0.1:8123"`},
		{name: "releases URL: trailing slash", args: ciReleases("https://127.0.0.1:8443/"), code: 1, err: "without a trailing slash"},
		{name: "releases URL: query", args: ciReleases("https://127.0.0.1:8443?x=1"), code: 1, err: "without a trailing slash, query"},
		{name: "releases URL: empty query", args: ciReleases("https://127.0.0.1:8443?"), code: 1, err: "without a trailing slash, query"},
		{name: "releases URL: fragment", args: ciReleases("https://127.0.0.1:8443#x"), code: 1, err: "without a trailing slash, query"},
		{name: "releases URL: empty fragment", args: ciReleases("https://127.0.0.1:8443#"), code: 1, err: "without a trailing slash, query"},
		{name: "releases URL: user", args: ciReleases("https://u@example.com"), code: 1, err: "without a trailing slash, query, fragment or user"},
		{name: "releases URL: dot segment", args: ciReleases("https://example.com/a/../b"), code: 1, err: "without a trailing slash"},
		{name: "releases URL: escaped path", args: ciReleases("https://example.com/a%2Fb"), code: 1, err: "without a trailing slash"},
		{name: "releases URL: upper-case scheme", args: ciReleases("HTTPS://example.com"), code: 1, err: "without a trailing slash"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			minVersion := tt.minVersion
			if minVersion == "" {
				minVersion = "0.2.0-rc.1\n"
			}
			root, installer := manifestTree(t, minVersion, tt.revoked)
			if tt.signers != "" {
				writeFile(t, filepath.Join(root, "deploy", "keys", "update_signers"), tt.signers)
			}
			if tt.installer == "-" {
				writeFile(t, installer, "")
			}
			args := append([]string{"-root", root, "manifest", "-version", testVersion, "-installer", installer,
				"-date", testDate}, tt.args...)
			code, _, stderr := runCmd(t, args...)
			if code != tt.code || !strings.Contains(stderr, tt.err) {
				t.Errorf("exit %d, stderr %q; want exit %d and %q", code, stderr, tt.code, tt.err)
			}
			if _, err := os.Stat(filepath.Join(root, "dist-desktop", manifestName)); err == nil {
				t.Error("a manifest was written")
			}
		})
	}

	t.Run("a revocation with the signers file", func(t *testing.T) {
		root, installer := manifestTree(t, "0.2.0-rc.1\n", revokedHeader+leaked+"\n")
		writeFile(t, filepath.Join(root, "deploy", "keys", "update_signers"), signers)
		code, stdout, stderr := runCmd(t, "-root", root, "manifest", "-version", testVersion, "-installer", installer,
			"-date", testDate)
		if code != 0 || !strings.Contains(stdout, "1 revoked keys") {
			t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
		}
		data, err := os.ReadFile(filepath.Join(root, "dist-desktop", manifestName))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(data, []byte("  \"revokedKeys\": [\n    \""+leaked+"\"\n  ],\n")) {
			t.Errorf("revokedKeys not written as a list:\n%s", data)
		}
	})
	t.Run("missing min-version", func(t *testing.T) {
		root, installer := manifestTree(t, "0.2.0\n", "")
		if err := os.Remove(filepath.Join(root, "deploy", "desktop", "min-version")); err != nil {
			t.Fatal(err)
		}
		if code, _, stderr := runCmd(t, "-root", root, "manifest", "-version", testVersion, "-installer", installer,
			"-date", testDate); code != 1 || !strings.Contains(stderr, "min-version") {
			t.Errorf("exit %d, stderr %q", code, stderr)
		}
	})
}

// ciReleases returns the arguments of a ci version's manifest with the releases URL base.
func ciReleases(base string) []string { return []string{"-version", "0.0.1-ci.2", "-releases", base} }

// manyFingerprints returns n distinct fingerprint lines.
func manyFingerprints(n int) string {
	var b strings.Builder
	for i := range n {
		b.WriteString(newFingerprint(strings.Repeat("k", i+1)) + "\n")
	}
	return b.String()
}

func TestChannelFor(t *testing.T) {
	for v, want := range map[string]string{
		"0.2.0": "stable", "0.2.0-rc.1": "beta", "0.2.0-rc.12": "beta", "0.0.1-ci.3": "ci",
		"0.0.0-dryrun.4": "ci", "0.2.0-rc": "ci", "0.2.0-rc.1.1": "ci", "0.2.0-rc.01x": "ci",
	} {
		if got := channelFor(v); got != want {
			t.Errorf("channelFor(%q) = %s, want %s", v, got, want)
		}
	}
}

// TestCommittedManifestInputs checks the repository's own deploy/desktop/min-version and deploy/keys/update_revoked.
func TestCommittedManifestInputs(t *testing.T) {
	root := filepath.Join("..", "..")
	if _, err := readMinVersion(root); err != nil {
		t.Fatal(err)
	}
	if _, err := readRevokedKeys(root); err != nil {
		t.Fatal(err)
	}
}
