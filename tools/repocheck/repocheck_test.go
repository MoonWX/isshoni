package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

const (
	goodToolVersions = `# comment
golang 1.27.1
nodejs 26.5.0
task 3.53.1   # trailing comment
golangci-lint 2.14.0
shfmt 3.14.1
actionlint 1.7.12
goreleaser 2.18.2
`
	goodMainMod = `module github.com/MoonWX/isshoni

go 1.26

toolchain go1.27.1
`
	goodToolsMod = `module github.com/MoonWX/isshoni/tools

go 1.26.0

toolchain go1.27.1

tool github.com/go-task/task/v3/cmd/task

require (
	github.com/go-task/task/v3 v3.53.1 // indirect
	github.com/golangci/golangci-lint/v2 v2.14.0 // indirect
	mvdan.cc/sh/v3 v3.14.1 // indirect
)
`
	goodGoreleaserMod = `module github.com/MoonWX/isshoni/tools/goreleaser

go 1.27.1

toolchain go1.27.1

tool github.com/goreleaser/goreleaser/v2

require github.com/goreleaser/goreleaser/v2 v2.18.2 // indirect
`

	// readmeTemplate mirrors the root README.md around its "Release signing keys" section, with RELEASE and BACKUP
	// in the two keys' Fingerprint cells; ^ stands for a backtick. The release key's row is line 15, the backup
	// key's line 16.
	readmeTemplate = `# isshoni

## Status

<!-- Before each release tag: fill in the fingerprints under "Release signing keys". -->
- **No release yet.**

## Release signing keys

Every release's ^checksums.txt^ is signed with ^ssh-keygen -Y sign^. The public keys are in
[^deploy/keys/allowed_signers^](deploy/keys/README.md), and their fingerprints here:

| Key | Use | Fingerprint |
|---|---|---|
| ^isshoni-release-1^ | signs every release | RELEASE |
| ^isshoni-release-backup-1^ | kept offline; signs nothing until a key is replaced | BACKUP |

^^^sh
ssh-keygen -Y verify -f allowed_signers -I isshoni-release -n isshoni-checksums -s checksums.txt.sig < checksums.txt
^^^

^ssh-keygen^ prints the fingerprint of the key that made the signature: it must be one of the fingerprints above.

## Privacy

There is no telemetry.
`
)

// Throwaway ed25519 keys, derived in the test from fixed seeds. They are no release key and sign nothing.
var (
	releaseKey = newTestKey("isshoni-release-1")
	backupKey  = newTestKey("isshoni-release-backup-1")
	otherKey   = newTestKey("isshoni-release-2")

	signers    = releaseKey.line + "\n" + backupKey.line + "\n"
	goodScript = scriptWith(signers)
	goodReadme = readmeWith(releaseKey.fingerprint, backupKey.fingerprint)
)

type testKey struct {
	name        string // the comment of its allowed_signers line
	key         string // "ssh-ed25519 AAAA…"
	line        string // its allowed_signers line, without the newline
	fingerprint string // as `ssh-keygen -l` prints it, computed here without ssh.FingerprintSHA256
}

func newTestKey(name string) testKey {
	seed := sha256.Sum256([]byte("repocheck test key " + name))
	pub, err := ssh.NewPublicKey(ed25519.NewKeyFromSeed(seed[:]).Public())
	if err != nil {
		panic(err)
	}
	key := strings.TrimSuffix(string(ssh.MarshalAuthorizedKey(pub)), "\n")
	blob, err := base64.StdEncoding.DecodeString(strings.Fields(key)[1])
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(blob)
	return testKey{
		name:        name,
		key:         key,
		line:        `isshoni-release namespaces="isshoni-checksums" ` + key + " " + name,
		fingerprint: "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:]),
	}
}

// scriptWith returns an install.sh whose key block holds signers, as `task release:keys` writes it.
func scriptWith(signers string) string {
	return "#!/bin/sh\nset -eu\n" + keysBegin + " (generated from deploy/keys/allowed_signers; do not edit here)\n" +
		keysPrefix + strings.TrimSuffix(signers, "\n") + "'\n" + keysEnd + "\nmain \"$@\"\n"
}

// readmeWith returns readmeTemplate with release and backup in the keys' Fingerprint cells.
func readmeWith(release, backup string) string {
	return strings.NewReplacer("^", "`", "RELEASE", release, "BACKUP", backup).Replace(readmeTemplate)
}

// keyTree returns the files of the keys check: install.sh, allowed_signers and a README.md that lists the
// fingerprints of releaseKey and backupKey.
func keyTree(script, signers string) map[string]string {
	return map[string]string{
		"deploy/install.sh":           script,
		"deploy/keys/allowed_signers": signers,
		"README.md":                   goodReadme,
	}
}

// writeTree creates files (slash-separated names) under a new temporary directory and returns it.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// crlf converts LF line ends to CRLF, as a Git for Windows checkout does.
func crlf(s string) string {
	return strings.ReplaceAll(s, "\n", "\r\n")
}

func pinTree(toolVersions, mainMod, toolsMod string) map[string]string {
	return map[string]string{
		".tool-versions":          toolVersions,
		"go.mod":                  mainMod,
		"tools/go.mod":            toolsMod,
		"tools/goreleaser/go.mod": goodGoreleaserMod,
	}
}

// withGoreleaserMod replaces tools/goreleaser/go.mod in a tree from pinTree.
func withGoreleaserMod(files map[string]string, mod string) map[string]string {
	files["tools/goreleaser/go.mod"] = mod
	return files
}

func TestCheckPins(t *testing.T) {
	tests := []struct {
		name    string
		files   map[string]string
		wantErr []string // substrings of the error; nil means no error
	}{
		{
			name:  "consistent",
			files: pinTree(goodToolVersions, goodMainMod, goodToolsMod),
		},
		{
			name:    "go.mod toolchain differs",
			files:   pinTree(goodToolVersions, strings.Replace(goodMainMod, "go1.27.1", "go1.27.2", 1), goodToolsMod),
			wantErr: []string{`go.mod has "toolchain go1.27.2" but .tool-versions pins golang 1.27.1`},
		},
		{
			name:    "tools toolchain differs",
			files:   pinTree(goodToolVersions, goodMainMod, strings.Replace(goodToolsMod, "go1.27.1", "go1.27.0", 1)),
			wantErr: []string{`tools/go.mod has "toolchain go1.27.0"`},
		},
		{
			name:    "no toolchain directive",
			files:   pinTree(goodToolVersions, "module github.com/MoonWX/isshoni\n\ngo 1.26\n", goodToolsMod),
			wantErr: []string{`go.mod has no toolchain directive; want "toolchain go1.27.1"`},
		},
		{
			name:    "no golang pin",
			files:   pinTree(strings.Replace(goodToolVersions, "golang 1.27.1\n", "", 1), goodMainMod, goodToolsMod),
			wantErr: []string{".tool-versions has no golang line"},
		},
		{
			name:    "task pin differs",
			files:   pinTree(strings.Replace(goodToolVersions, "task 3.53.1", "task 3.52.0", 1), goodMainMod, goodToolsMod),
			wantErr: []string{"tools/go.mod has github.com/go-task/task/v3 v3.53.1 but .tool-versions pins task 3.52.0"},
		},
		{
			name:    "shfmt pin differs (MVS raised mvdan.cc/sh/v3 for Task)",
			files:   pinTree(goodToolVersions, goodMainMod, strings.Replace(goodToolsMod, "sh/v3 v3.14.1", "sh/v3 v3.15.0", 1)),
			wantErr: []string{"tools/go.mod has mvdan.cc/sh/v3 v3.15.0 but .tool-versions pins shfmt 3.14.1"},
		},
		{
			name:    "no shfmt pin",
			files:   pinTree(strings.Replace(goodToolVersions, "shfmt 3.14.1\n", "", 1), goodMainMod, goodToolsMod),
			wantErr: []string{".tool-versions has no shfmt line"},
		},
		{
			name: "golangci-lint missing from tools/go.mod, several problems at once",
			files: pinTree(goodToolVersions, strings.Replace(goodMainMod, "go1.27.1", "go1.27.2", 1),
				strings.Replace(goodToolsMod, "\tgithub.com/golangci/golangci-lint/v2 v2.14.0 // indirect\n", "", 1)),
			wantErr: []string{
				`go.mod has "toolchain go1.27.2"`,
				"tools/go.mod does not require github.com/golangci/golangci-lint/v2 (.tool-versions: golangci-lint 2.14.0)",
			},
		},
		{
			name: "goreleaser toolchain differs",
			files: withGoreleaserMod(pinTree(goodToolVersions, goodMainMod, goodToolsMod),
				strings.Replace(goodGoreleaserMod, "toolchain go1.27.1", "toolchain go1.27.0", 1)),
			wantErr: []string{`tools/goreleaser/go.mod has "toolchain go1.27.0" but .tool-versions pins golang 1.27.1`},
		},
		{
			name:    "goreleaser pin differs",
			files:   pinTree(strings.Replace(goodToolVersions, "goreleaser 2.18.2", "goreleaser 2.18.3", 1), goodMainMod, goodToolsMod),
			wantErr: []string{"tools/goreleaser/go.mod has github.com/goreleaser/goreleaser/v2 v2.18.2 but .tool-versions pins goreleaser 2.18.3"},
		},
		{
			name: "goreleaser missing from tools/goreleaser/go.mod",
			files: withGoreleaserMod(pinTree(goodToolVersions, goodMainMod, goodToolsMod),
				"module github.com/MoonWX/isshoni/tools/goreleaser\n\ngo 1.27.1\n\ntoolchain go1.27.1\n"),
			wantErr: []string{"tools/goreleaser/go.mod does not require github.com/goreleaser/goreleaser/v2 (.tool-versions: goreleaser 2.18.2)"},
		},
		{
			name:    "no goreleaser pin",
			files:   pinTree(strings.Replace(goodToolVersions, "goreleaser 2.18.2\n", "", 1), goodMainMod, goodToolsMod),
			wantErr: []string{".tool-versions has no goreleaser line"},
		},
		{
			name:    "missing tools/goreleaser/go.mod",
			files:   map[string]string{".tool-versions": goodToolVersions, "go.mod": goodMainMod, "tools/go.mod": goodToolsMod},
			wantErr: []string{"goreleaser"},
		},
		{
			name:    "tool without version",
			files:   pinTree(goodToolVersions+"syft\n", goodMainMod, goodToolsMod),
			wantErr: []string{`"syft" has no version`},
		},
		{
			name:    "tool listed twice",
			files:   pinTree(goodToolVersions+"task 3.53.1\n", goodMainMod, goodToolsMod),
			wantErr: []string{`"task" is listed twice`},
		},
		{
			name:    "missing tools/go.mod",
			files:   map[string]string{".tool-versions": goodToolVersions, "go.mod": goodMainMod},
			wantErr: []string{"go.mod"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkPins(writeTree(t, tt.files))
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("checkPins: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("checkPins: no error")
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestCheckKeys(t *testing.T) {
	tests := []struct {
		name     string
		files    map[string]string
		wantNote string // substring of the note
		wantErr  string // substring of the error; empty means no error
	}{
		{
			name:     "neither file yet",
			files:    map[string]string{},
			wantNote: "deploy/install.sh and deploy/keys/allowed_signers do not exist yet",
		},
		{
			name:     "no key file yet",
			files:    map[string]string{"deploy/install.sh": goodScript},
			wantNote: "deploy/keys/allowed_signers does not exist yet",
		},
		{
			name:  "equal",
			files: keyTree(goodScript, signers),
		},
		{
			// A Git for Windows checkout (core.autocrlf=true) has CRLF line ends in both files.
			name:  "equal with CRLF line ends",
			files: keyTree(crlf(goodScript), crlf(signers)),
		},
		{
			name:  "only the script has CRLF line ends",
			files: keyTree(crlf(goodScript), signers),
		},
		{
			name:  "only the key file has CRLF line ends",
			files: keyTree(goodScript, crlf(signers)),
		},
		{
			name:    "second key differs with CRLF line ends",
			files:   keyTree(crlf(strings.Replace(goodScript, backupKey.key, otherKey.key, 1)), crlf(signers)),
			wantErr: "differs from deploy/keys/allowed_signers at line 2",
		},
		{
			name:    "second key differs",
			files:   keyTree(strings.Replace(goodScript, backupKey.key, otherKey.key, 1), signers),
			wantErr: "differs from deploy/keys/allowed_signers at line 2",
		},
		{
			name:    "key file without final newline",
			files:   keyTree(goodScript, strings.TrimSuffix(signers, "\n")),
			wantErr: "must end with a newline",
		},
		{
			name:    "extra key in the file",
			files:   keyTree(goodScript, signers+otherKey.line+"\n"),
			wantErr: "at line 3",
		},
		{
			name:    "no end marker",
			files:   keyTree(strings.Replace(goodScript, keysEnd, "# END", 1), signers),
			wantErr: `no "# END ALLOWED SIGNERS" line`,
		},
		{
			name:    "markers in the wrong order",
			files:   keyTree("# END ALLOWED SIGNERS\nALLOWED_SIGNERS='x'\n# BEGIN ALLOWED SIGNERS\n", "x\n"),
			wantErr: "comes before",
		},
		{
			name:    "two blocks",
			files:   keyTree(goodScript+goodScript, signers),
			wantErr: "a second",
		},
		{
			name:    "other code inside the block",
			files:   keyTree(strings.Replace(goodScript, "# END ALLOWED SIGNERS", "echo hi\n# END ALLOWED SIGNERS", 1), signers),
			wantErr: "must be ALLOWED_SIGNERS=",
		},
		{
			name:    "empty assignment",
			files:   keyTree("# BEGIN ALLOWED SIGNERS\nALLOWED_SIGNERS='\n# END ALLOWED SIGNERS\n", signers),
			wantErr: "must be ALLOWED_SIGNERS=",
		},
		{
			name:    "quote in the key file",
			files:   keyTree(goodScript, "it's\n"),
			wantErr: "single quote",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var wantErr []string
			if tt.wantErr != "" {
				wantErr = []string{tt.wantErr}
			}
			assertCheckKeys(t, tt.files, tt.wantNote, wantErr)
		})
	}
}

// assertCheckKeys runs checkKeys on a tree of files. wantNote is a substring of the note ("" means no note), and
// wantErr the substrings of the error (nil means no error).
func assertCheckKeys(t *testing.T, files map[string]string, wantNote string, wantErr []string) {
	t.Helper()
	note, err := checkKeys(writeTree(t, files))
	switch {
	case wantErr == nil && err != nil:
		t.Errorf("checkKeys: %v", err)
	case wantErr != nil && err == nil:
		t.Errorf("checkKeys: no error, want one containing %q", wantErr)
	case err != nil:
		for _, want := range wantErr {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("checkKeys error %q does not contain %q", err, want)
			}
		}
	}
	if wantNote == "" && note != "" {
		t.Errorf("unexpected note %q", note)
	}
	if !strings.Contains(note, wantNote) {
		t.Errorf("note %q does not contain %q", note, wantNote)
	}
}

func TestRun(t *testing.T) {
	good := writeTree(t, pinTree(goodToolVersions, goodMainMod, goodToolsMod))
	bad := writeTree(t, pinTree(goodToolVersions, strings.Replace(goodMainMod, "go1.27.1", "go1.27.2", 1), goodToolsMod))
	keysGood := writeTree(t, keyTree(goodScript, signers))
	keysNoFingerprints := writeTree(t, map[string]string{
		"deploy/install.sh":           goodScript,
		"deploy/keys/allowed_signers": signers,
		"README.md":                   readmeWith("not created yet", "not created yet"),
	})
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "pins ok", args: []string{"-root", good, "pins"}, wantCode: 0},
		{name: "pins fail", args: []string{"-root", bad, "pins"}, wantCode: 1, wantStderr: "repocheck pins: the pins differ"},
		{name: "keys skipped", args: []string{"-root", good, "keys"}, wantCode: 0, wantStdout: "repocheck keys: skipped"},
		{name: "keys ok", args: []string{"-root", keysGood, "keys"}, wantCode: 0},
		{
			name: "keys fail: README.md has no fingerprints", args: []string{"-root", keysNoFingerprints, "keys"}, wantCode: 1,
			wantStderr: `repocheck keys: the "Release signing keys" section of README.md does not match deploy/keys/allowed_signers`,
		},
		{name: "no check", args: nil, wantCode: 2, wantStderr: "usage: repocheck"},
		{name: "two checks", args: []string{"pins", "keys"}, wantCode: 2, wantStderr: "usage: repocheck"},
		{name: "unknown check", args: []string{"lint"}, wantCode: 2, wantStderr: `unknown check "lint"`},
		{name: "bad flag", args: []string{"-nope", "pins"}, wantCode: 2},
		{name: "help", args: []string{"-h"}, wantCode: 0, wantStderr: "usage: repocheck"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(tt.args, &stdout, &stderr); code != tt.wantCode {
				t.Errorf("exit code %d, want %d (stderr: %s)", code, tt.wantCode, stderr.String())
			}
			if !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout %q does not contain %q", stdout.String(), tt.wantStdout)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr %q does not contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}
