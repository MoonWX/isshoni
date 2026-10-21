package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
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
	goodWailsMod = `module github.com/MoonWX/isshoni/tools/wails3

go 1.26.0

toolchain go1.27.1

tool github.com/wailsapp/wails/v3/cmd/wails3

require (
	github.com/wailsapp/task/v3 v3.40.1-patched3 // indirect
	github.com/wailsapp/wails/v3 v3.0.0-beta.34 // indirect
)
`
	// goodMainModWails is goodMainMod once the desktop app has added Wails.
	goodMainModWails = goodMainMod + `
require github.com/wailsapp/wails/v3 v3.0.0-beta.34
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
		"tools/wails3/go.mod":     goodWailsMod,
	}
}

const (
	goodToolsJSON = `{"msvc": {"vs": "[17.0,19.0)", "toolset": "14.44", "windowsSdk": "10.0.26100.0"}}` + "\n"
	goodTriplet   = "set(VCPKG_TARGET_ARCHITECTURE x64)\nset(VCPKG_PLATFORM_TOOLSET v143)\nset(VCPKG_PLATFORM_TOOLSET_VERSION 14.44)\n"
)

// msvcTree is a consistent pinTree with deploy/windows/tools.json and the overlay triplet ("" leaves the triplet out).
func msvcTree(toolsJSON, triplet string) map[string]string {
	files := withFile(pinTree(goodToolVersions, goodMainMod, goodToolsMod), "deploy/windows/tools.json", toolsJSON)
	if triplet != "" {
		files = withFile(files, "native/triplets/x64-windows-static.cmake", triplet)
	}
	return files
}

// withGoreleaserMod replaces tools/goreleaser/go.mod in a tree from pinTree.
func withGoreleaserMod(files map[string]string, mod string) map[string]string {
	return withFile(files, "tools/goreleaser/go.mod", mod)
}

// mustJSON marshals v with two-space indentation, as npm writes its files.
func mustJSON(v any) string {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(err)
	}
	return string(data) + "\n"
}

// packageJSON returns a web/package.json that lists @wailsio/runtime as spec in section ("" leaves it out).
func packageJSON(section, spec string) string {
	pkg := map[string]any{
		"name":            "isshoni-web",
		"dependencies":    map[string]string{"react": "^19.3.0"},
		"devDependencies": map[string]string{"vite": "^8.0.0"},
	}
	if section != "" {
		pkg[section].(map[string]string)[wailsRuntime] = spec
	}
	return mustJSON(pkg)
}

// packageLock returns a lockfileVersion 3 web/package-lock.json whose root entry lists @wailsio/runtime as rootSpec
// ("" leaves it out) and which installs it at the given versions, keyed by path (nil installs nothing).
func packageLock(rootSpec string, installed map[string]string) string {
	rootDeps := map[string]string{"react": "^19.3.0"}
	if rootSpec != "" {
		rootDeps[wailsRuntime] = rootSpec
	}
	packages := map[string]any{
		"": map[string]any{
			"name":            "isshoni-web",
			"dependencies":    rootDeps,
			"devDependencies": map[string]string{"vite": "^8.0.0"},
		},
		"node_modules/react": map[string]string{"version": "19.3.0"},
	}
	for key, version := range installed {
		packages[key] = map[string]string{"version": version, "license": "MIT"}
	}
	return mustJSON(map[string]any{"name": "isshoni-web", "lockfileVersion": 3, "requires": true, "packages": packages})
}

const runtimeKey = "node_modules/@wailsio/runtime"

// wailsTree is a pinTree with go.mod, web/package.json and web/package-lock.json given ("" leaves a web file out).
func wailsTree(mainMod, pkg, lock string) map[string]string {
	files := pinTree(goodToolVersions, mainMod, goodToolsMod)
	for name, content := range map[string]string{"web/package.json": pkg, "web/package-lock.json": lock} {
		if content == "" {
			withoutFile(files, name)
		} else {
			withFile(files, name, content)
		}
	}
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
		{
			name: "wails3 toolchain differs",
			files: withFile(pinTree(goodToolVersions, goodMainMod, goodToolsMod), "tools/wails3/go.mod",
				strings.Replace(goodWailsMod, "toolchain go1.27.1", "toolchain go1.27.0", 1)),
			wantErr: []string{`tools/wails3/go.mod has "toolchain go1.27.0" but .tool-versions pins golang 1.27.1`},
		},
		{
			name: "missing tools/wails3/go.mod",
			files: map[string]string{".tool-versions": goodToolVersions, "go.mod": goodMainMod, "tools/go.mod": goodToolsMod,
				"tools/goreleaser/go.mod": goodGoreleaserMod},
			wantErr: []string{"wails3"},
		},
		{
			name:  "MSVC toolset: tools.json and the triplet agree",
			files: msvcTree(goodToolsJSON, goodTriplet),
		},
		{
			name:  "MSVC toolset: a quoted version and CRLF line ends",
			files: msvcTree(goodToolsJSON, crlf(strings.Replace(goodTriplet, "VERSION 14.44)", `VERSION "14.44")`, 1))),
		},
		{
			name:    "MSVC toolset: the triplet names another",
			files:   msvcTree(goodToolsJSON, strings.Replace(goodTriplet, "VERSION 14.44)", "VERSION 14.51)", 1)),
			wantErr: []string{"sets VCPKG_PLATFORM_TOOLSET_VERSION 14.51 but deploy/windows/tools.json's MSVC toolset is 14.44"},
		},
		{
			name:    "MSVC toolset: the triplet sets none",
			files:   msvcTree(goodToolsJSON, "set(VCPKG_CRT_LINKAGE static)\n# set(VCPKG_PLATFORM_TOOLSET_VERSION 14.44)\n"),
			wantErr: []string{"does not set VCPKG_PLATFORM_TOOLSET_VERSION; want 14.44"},
		},
		{
			name:    "MSVC toolset: no triplet",
			files:   msvcTree(goodToolsJSON, ""),
			wantErr: []string{"native/triplets/x64-windows-static.cmake does not exist"},
		},
		{
			name:    "MSVC toolset: tools.json without one",
			files:   msvcTree(`{"msvc": {"vs": "[17.0,19.0)"}}`, goodTriplet),
			wantErr: []string{`deploy/windows/tools.json has no "msvc".toolset`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := checkPins(writeTree(t, tt.files))
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

func TestCheckPinsWails(t *testing.T) {
	goodLock := packageLock("3.0.0-beta.34", map[string]string{runtimeKey: "3.0.0-beta.34"})
	otherCLI := strings.Replace(goodWailsMod, "beta.34", "beta.33", 1)
	noWailsCLI := "module github.com/MoonWX/isshoni/tools/wails3\n\ngo 1.26.0\n\ntoolchain go1.27.1\n"
	const noWailsNote = "go.mod does not require github.com/wailsapp/wails/v3 yet, so only tools/wails3/go.mod's Wails pin was checked"
	tests := []struct {
		name     string
		files    map[string]string
		wantNote string   // substring of the note; "" means no note
		wantErr  []string // substrings of the error; nil means no error
	}{
		{
			// Before the desktop app adds Wails to go.mod (README WA22), the CLI alone is pinned.
			name:     "go.mod has no Wails yet",
			files:    wailsTree(goodMainMod, packageJSON("", ""), packageLock("", nil)),
			wantNote: noWailsNote,
		},
		{
			name:     "go.mod has no Wails yet, no web/",
			files:    wailsTree(goodMainMod, "", ""),
			wantNote: noWailsNote,
		},
		{
			name:     "go.mod has no Wails yet, tools/wails3 names any version",
			files:    withFile(wailsTree(goodMainMod, packageJSON("", ""), ""), "tools/wails3/go.mod", otherCLI),
			wantNote: noWailsNote,
		},
		{
			name:  "all three equal",
			files: wailsTree(goodMainModWails, packageJSON("dependencies", "3.0.0-beta.34"), goodLock),
		},
		{
			name:  "all three equal, the runtime in devDependencies",
			files: wailsTree(goodMainModWails, packageJSON("devDependencies", "3.0.0-beta.34"), goodLock),
		},
		{
			name:    "tools/wails3 differs from go.mod",
			files:   withFile(wailsTree(goodMainModWails, packageJSON("dependencies", "3.0.0-beta.34"), goodLock), "tools/wails3/go.mod", otherCLI),
			wantErr: []string{"tools/wails3/go.mod has github.com/wailsapp/wails/v3 v3.0.0-beta.33 but go.mod has v3.0.0-beta.34"},
		},
		{
			name:    "tools/wails3 lacks the Wails module",
			files:   withFile(wailsTree(goodMainModWails, packageJSON("dependencies", "3.0.0-beta.34"), goodLock), "tools/wails3/go.mod", noWailsCLI),
			wantErr: []string{"tools/wails3/go.mod does not require github.com/wailsapp/wails/v3"},
		},
		{
			name:    "tools/wails3 lacks the Wails module while go.mod has none",
			files:   withFile(wailsTree(goodMainMod, packageJSON("", ""), ""), "tools/wails3/go.mod", noWailsCLI),
			wantErr: []string{"tools/wails3/go.mod does not require github.com/wailsapp/wails/v3"},
		},
		{
			name:    "caret range",
			files:   wailsTree(goodMainModWails, packageJSON("dependencies", "^3.0.0-beta.34"), goodLock),
			wantErr: []string{`web/package.json has @wailsio/runtime "^3.0.0-beta.34" in dependencies, which is a range or a tag, not an exact version; it must be "3.0.0-beta.34"`},
		},
		{
			name:    "tilde range",
			files:   wailsTree(goodMainModWails, packageJSON("dependencies", "~3.0.0-beta.34"), goodLock),
			wantErr: []string{"which is a range or a tag"},
		},
		{
			name:    "comparator range",
			files:   wailsTree(goodMainModWails, packageJSON("dependencies", ">=3.0.0-beta.34 <3.0.1"), goodLock),
			wantErr: []string{"which is a range or a tag"},
		},
		{
			name:    "dist-tag",
			files:   wailsTree(goodMainModWails, packageJSON("dependencies", "latest"), goodLock),
			wantErr: []string{`"latest" in dependencies, which is a range or a tag`},
		},
		{
			name:    "v prefix",
			files:   wailsTree(goodMainModWails, packageJSON("dependencies", "v3.0.0-beta.34"), goodLock),
			wantErr: []string{"which is a range or a tag"},
		},
		{
			name:    "x range",
			files:   wailsTree(goodMainModWails, packageJSON("dependencies", "3.0.x"), goodLock),
			wantErr: []string{"which is a range or a tag"},
		},
		{
			name:    "exact but another version",
			files:   wailsTree(goodMainModWails, packageJSON("dependencies", "3.0.0-beta.33"), goodLock),
			wantErr: []string{`web/package.json has @wailsio/runtime "3.0.0-beta.33" in dependencies but go.mod has github.com/wailsapp/wails/v3 v3.0.0-beta.34, so it must be "3.0.0-beta.34"`},
		},
		{
			name:    "go.mod has Wails, web/package.json lacks the runtime",
			files:   wailsTree(goodMainModWails, packageJSON("", ""), packageLock("", nil)),
			wantErr: []string{`web/package.json does not list @wailsio/runtime; go.mod has github.com/wailsapp/wails/v3 v3.0.0-beta.34, so it must be exactly "3.0.0-beta.34"`},
		},
		{
			name:    "the runtime without Wails in go.mod",
			files:   wailsTree(goodMainMod, packageJSON("dependencies", "3.0.0-beta.34"), goodLock),
			wantErr: []string{`web/package.json has @wailsio/runtime "3.0.0-beta.34" in dependencies, but go.mod does not require github.com/wailsapp/wails/v3`},
		},
		{
			name:    "lockfile installs another version",
			files:   wailsTree(goodMainModWails, packageJSON("dependencies", "3.0.0-beta.34"), packageLock("3.0.0-beta.34", map[string]string{runtimeKey: "3.0.0-beta.35"})),
			wantErr: []string{`web/package-lock.json installs @wailsio/runtime "3.0.0-beta.35" at node_modules/@wailsio/runtime but go.mod has github.com/wailsapp/wails/v3 v3.0.0-beta.34`},
		},
		{
			name:    "lockfile root entry still has a range",
			files:   wailsTree(goodMainModWails, packageJSON("dependencies", "3.0.0-beta.34"), packageLock("^3.0.0-beta.34", map[string]string{runtimeKey: "3.0.0-beta.34"})),
			wantErr: []string{`web/package-lock.json's root entry has @wailsio/runtime "^3.0.0-beta.34" in dependencies, which is a range or a tag`},
		},
		{
			name:    "lockfile root entry lacks the runtime",
			files:   wailsTree(goodMainModWails, packageJSON("dependencies", "3.0.0-beta.34"), packageLock("", map[string]string{runtimeKey: "3.0.0-beta.34"})),
			wantErr: []string{"web/package-lock.json's root entry does not list @wailsio/runtime"},
		},
		{
			name:    "lockfile does not install the runtime",
			files:   wailsTree(goodMainModWails, packageJSON("dependencies", "3.0.0-beta.34"), packageLock("3.0.0-beta.34", nil)),
			wantErr: []string{"web/package-lock.json installs no node_modules/@wailsio/runtime"},
		},
		{
			name: "a nested copy of another version",
			files: wailsTree(goodMainModWails, packageJSON("dependencies", "3.0.0-beta.34"), packageLock("3.0.0-beta.34", map[string]string{
				runtimeKey: "3.0.0-beta.34", "node_modules/some-lib/node_modules/@wailsio/runtime": "3.0.0-beta.20",
			})),
			wantErr: []string{`installs @wailsio/runtime "3.0.0-beta.20" at node_modules/some-lib/node_modules/@wailsio/runtime`},
		},
		{
			name:    "no lockfile",
			files:   wailsTree(goodMainModWails, packageJSON("dependencies", "3.0.0-beta.34"), ""),
			wantErr: []string{"web/package-lock.json does not exist"},
		},
		{
			name:    "lockfile version 1",
			files:   wailsTree(goodMainModWails, packageJSON("dependencies", "3.0.0-beta.34"), `{"lockfileVersion": 1, "dependencies": {}}`),
			wantErr: []string{`web/package-lock.json has no "packages" (lockfileVersion 1)`},
		},
		{
			name: "several problems at once",
			files: withFile(wailsTree(goodMainModWails, packageJSON("dependencies", "^3.0.0-beta.33"),
				packageLock("^3.0.0-beta.33", map[string]string{runtimeKey: "3.0.0-beta.33"})), "tools/wails3/go.mod", otherCLI),
			wantErr: []string{
				"tools/wails3/go.mod has github.com/wailsapp/wails/v3 v3.0.0-beta.33 but go.mod has v3.0.0-beta.34",
				`web/package.json has @wailsio/runtime "^3.0.0-beta.33"`,
				`web/package-lock.json's root entry has @wailsio/runtime "^3.0.0-beta.33"`,
				`web/package-lock.json installs @wailsio/runtime "3.0.0-beta.33"`,
				"Wails moves in go.mod, tools/wails3/go.mod, web/package.json and web/package-lock.json at once",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			note, err := checkPins(writeTree(t, tt.files))
			if tt.wantErr == nil && err != nil {
				t.Fatalf("checkPins: %v", err)
			}
			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("checkPins: no error, want one containing %q", tt.wantErr)
				}
				for _, want := range tt.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not contain %q", err, want)
					}
				}
				return
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

// A web file that doesn't parse fails the check, naming the file, rather than passing it.
func TestCheckPinsWailsBadJSON(t *testing.T) {
	for _, tt := range []struct {
		name, pkg, lock, want string
	}{
		{name: "package.json", pkg: "{", lock: packageLock("", nil), want: "web/package.json"},
		{name: "package-lock.json", pkg: packageJSON("dependencies", "3.0.0-beta.34"), lock: "{", want: "web/package-lock.json"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := checkPins(writeTree(t, wailsTree(goodMainModWails, tt.pkg, tt.lock)))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("checkPins: %v, want an error naming %s", err, tt.want)
			}
		})
	}
}

func TestExactVersion(t *testing.T) {
	for _, v := range []string{"3.0.0", "3.0.0-beta.34", "0.1.0-rc.1", "1.2.3+build.5", "1.0.0-alpha-a.b-c"} {
		if !exactVersion.MatchString(v) {
			t.Errorf("%q is not taken for an exact version", v)
		}
	}
	for _, v := range []string{"", "^3.0.0", "~3.0.0", ">=3.0.0", "3.0", "3.0.x", "*", "latest", "v3.0.0", "=3.0.0",
		"3.0.0 || 3.0.1", "01.0.0", "3.0.0-beta.034", "npm:@wailsio/runtime@3.0.0", "file:../runtime"} {
		if exactVersion.MatchString(v) {
			t.Errorf("%q is taken for an exact version", v)
		}
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

// noUpdateKeysNote is checkKeys' note while deploy/keys/update_signers does not exist.
const noUpdateKeysNote = "skipped the update keys: deploy/keys/update_signers does not exist yet"

// assertCheckKeys runs checkKeys on a tree of files. wantNote is a substring of the note ("" means no note), and
// wantErr the substrings of the error (nil means no error). A tree without deploy/keys/update_signers must get
// noUpdateKeysNote too, which wantNote need not repeat.
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
	rest := note
	if _, ok := files["deploy/keys/update_signers"]; !ok {
		if !strings.Contains(note, noUpdateKeysNote) {
			t.Errorf("note %q does not contain %q", note, noUpdateKeysNote)
		}
		rest = strings.TrimSuffix(strings.TrimSuffix(note, noUpdateKeysNote), "; ")
	}
	if wantNote == "" && rest != "" {
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
		{name: "pins ok", args: []string{"-root", good, "pins"}, wantCode: 0, wantStdout: "repocheck pins: note: go.mod does not require github.com/wailsapp/wails/v3 yet"},
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
