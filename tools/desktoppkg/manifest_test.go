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
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// goldenManifest is the manifest of manifestTree's release. internal/update's tests parse it too, so the writer and
// the app's parser can't drift apart.
var goldenManifest = filepath.Join("testdata", "manifest", manifestName)

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

func TestManifestFields(t *testing.T) {
	fp1 := newFingerprint("leaked")
	fp2 := newFingerprint("other")
	tests := []struct {
		name, version, minVersion, revoked string
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
			code, _, stderr := runCmd(t, "-root", root, "manifest", "-version", tt.version, "-installer", installer,
				"-date", "2027-01-31T23:59:59.9999Z", "-out", out)
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
