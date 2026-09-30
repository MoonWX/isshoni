package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	goodToolVersions = `# comment
golang 1.27.1
nodejs 26.5.0
task 3.53.1   # trailing comment
golangci-lint 2.14.0
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
)
`
	signers = `isshoni-release namespaces="isshoni-checksums" ssh-ed25519 AAAAone isshoni-release-1
isshoni-release namespaces="isshoni-checksums" ssh-ed25519 AAAAtwo isshoni-release-backup-1
`
	goodScript = `#!/bin/sh
set -eu
# BEGIN ALLOWED SIGNERS (generated from deploy/keys/allowed_signers; do not edit here)
ALLOWED_SIGNERS='isshoni-release namespaces="isshoni-checksums" ssh-ed25519 AAAAone isshoni-release-1
isshoni-release namespaces="isshoni-checksums" ssh-ed25519 AAAAtwo isshoni-release-backup-1'
# END ALLOWED SIGNERS
main "$@"
`
)

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

func pinTree(toolVersions, mainMod, toolsMod string) map[string]string {
	return map[string]string{".tool-versions": toolVersions, "go.mod": mainMod, "tools/go.mod": toolsMod}
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
			name: "golangci-lint missing from tools/go.mod, several problems at once",
			files: pinTree(goodToolVersions, strings.Replace(goodMainMod, "go1.27.1", "go1.27.2", 1),
				strings.Replace(goodToolsMod, "\tgithub.com/golangci/golangci-lint/v2 v2.14.0 // indirect\n", "", 1)),
			wantErr: []string{
				`go.mod has "toolchain go1.27.2"`,
				"tools/go.mod does not require github.com/golangci/golangci-lint/v2 (.tool-versions: golangci-lint 2.14.0)",
			},
		},
		{
			name:    "tool without version",
			files:   pinTree(goodToolVersions+"shfmt\n", goodMainMod, goodToolsMod),
			wantErr: []string{`"shfmt" has no version`},
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
			files: map[string]string{"deploy/install.sh": goodScript, "deploy/keys/allowed_signers": signers},
		},
		{
			name: "second key differs",
			files: map[string]string{
				"deploy/install.sh":           strings.Replace(goodScript, "AAAAtwo", "AAAAxyz", 1),
				"deploy/keys/allowed_signers": signers,
			},
			wantErr: "differs from deploy/keys/allowed_signers at line 2",
		},
		{
			name: "key file without final newline",
			files: map[string]string{
				"deploy/install.sh":           goodScript,
				"deploy/keys/allowed_signers": strings.TrimSuffix(signers, "\n"),
			},
			wantErr: "must end with a newline",
		},
		{
			name: "extra key in the file",
			files: map[string]string{
				"deploy/install.sh":           goodScript,
				"deploy/keys/allowed_signers": signers + "isshoni-release ssh-ed25519 AAAAthree extra\n",
			},
			wantErr: "at line 3",
		},
		{
			name: "no end marker",
			files: map[string]string{
				"deploy/install.sh":           strings.Replace(goodScript, keysEnd, "# END", 1),
				"deploy/keys/allowed_signers": signers,
			},
			wantErr: `no "# END ALLOWED SIGNERS" line`,
		},
		{
			name: "markers in the wrong order",
			files: map[string]string{
				"deploy/install.sh":           "# END ALLOWED SIGNERS\nALLOWED_SIGNERS='x'\n# BEGIN ALLOWED SIGNERS\n",
				"deploy/keys/allowed_signers": "x\n",
			},
			wantErr: "comes before",
		},
		{
			name: "two blocks",
			files: map[string]string{
				"deploy/install.sh":           goodScript + goodScript,
				"deploy/keys/allowed_signers": signers,
			},
			wantErr: "a second",
		},
		{
			name: "other code inside the block",
			files: map[string]string{
				"deploy/install.sh":           strings.Replace(goodScript, "# END ALLOWED SIGNERS", "echo hi\n# END ALLOWED SIGNERS", 1),
				"deploy/keys/allowed_signers": signers,
			},
			wantErr: "must be ALLOWED_SIGNERS=",
		},
		{
			name: "empty assignment",
			files: map[string]string{
				"deploy/install.sh":           "# BEGIN ALLOWED SIGNERS\nALLOWED_SIGNERS='\n# END ALLOWED SIGNERS\n",
				"deploy/keys/allowed_signers": signers,
			},
			wantErr: "must be ALLOWED_SIGNERS=",
		},
		{
			name: "quote in the key file",
			files: map[string]string{
				"deploy/install.sh":           goodScript,
				"deploy/keys/allowed_signers": "it's\n",
			},
			wantErr: "single quote",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			note, err := checkKeys(writeTree(t, tt.files))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("checkKeys: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("checkKeys error = %v, want it to contain %q", err, tt.wantErr)
			}
			if tt.wantNote == "" && note != "" {
				t.Errorf("unexpected note %q", note)
			}
			if !strings.Contains(note, tt.wantNote) {
				t.Errorf("note %q does not contain %q", note, tt.wantNote)
			}
		})
	}
}

func TestRun(t *testing.T) {
	good := writeTree(t, pinTree(goodToolVersions, goodMainMod, goodToolsMod))
	bad := writeTree(t, pinTree(goodToolVersions, strings.Replace(goodMainMod, "go1.27.1", "go1.27.2", 1), goodToolsMod))
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
