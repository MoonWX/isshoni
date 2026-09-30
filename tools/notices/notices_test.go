package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/THIRD_PARTY_NOTICES.golden")

// testdataDir is the absolute path of testdata/.
func testdataDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs("testdata")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// fixtureGo runs the real go command, offline, and answers `go env GOROOT GOVERSION` with the fake GOROOT of
// testdata/goroot, so the golden file does not change with the toolchain.
func fixtureGo(goroot string) goCmdFunc {
	return func(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "env" {
			return []byte(goroot + "\ngo1.99.0\n"), nil
		}
		env = append(env, "GOPROXY=off", "GOFLAGS=", "GOTOOLCHAIN=local")
		return runGo(ctx, dir, env, args...)
	}
}

// fixtureConfig is a run over the testdata fixtures: the app module (a stand-in for the isshoni main module), whose
// dependencies are the modules under testdata/mods.
func fixtureConfig(t *testing.T, pkg string) config {
	t.Helper()
	td := testdataDir(t)
	return config{
		root:           filepath.Join(td, "app"),
		pkgs:           []string{pkg},
		goos:           []string{"linux", "darwin", "windows"},
		goarch:         []string{"amd64", "arm64"},
		npmPath:        filepath.Join(td, "licenses.txt"),
		extraPath:      filepath.Join(td, "extra.txt"),
		exceptionsPath: filepath.Join(td, "exceptions.md"),
		version:        "1.2.3-test",
		goCmd:          fixtureGo(filepath.Join(td, "goroot")),
	}
}

func TestGolden(t *testing.T) {
	res, err := generate(t.Context(), fixtureConfig(t, "./cmd/app"))
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "THIRD_PARTY_NOTICES.golden")
	if *update {
		if err := os.WriteFile(golden, res.text, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run go test -run TestGolden -update)", err)
	}
	want = bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")) // a Git checkout with core.autocrlf
	if !bytes.Equal(res.text, want) {
		t.Errorf("the output differs from %s (run go test -run TestGolden -update and review the diff):\n%s", golden, res.text)
	}

	if res.modules != 5 || res.goVersion != "go1.99.0" || res.extraEntries != 1 {
		t.Errorf("modules, goVersion, extraEntries = %d, %q, %d; want 5, go1.99.0, 1", res.modules, res.goVersion, res.extraEntries)
	}
	if len(res.warnings) != 1 || !strings.Contains(res.warnings[0], "example.com/gone (MPL-2.0) matches no linked module") {
		t.Errorf("warnings = %q, want one for the stale example.com/gone exception", res.warnings)
	}
	if bytes.Contains(res.text, []byte(testdataDir(t))) {
		t.Error("the output contains a local path")
	}
}

// TestGoldenIsDeterministic runs generate twice: the same inputs give the same bytes.
func TestGoldenIsDeterministic(t *testing.T) {
	a, err := generate(t.Context(), fixtureConfig(t, "./cmd/app"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := generate(t.Context(), fixtureConfig(t, "./cmd/app"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.text, b.text) {
		t.Error("two runs over the same inputs differ")
	}
}

// TestGOOSUnion: a module linked for one GOOS only is listed when that GOOS is among -goos.
func TestGOOSUnion(t *testing.T) {
	for _, tc := range []struct {
		goos    []string
		modules int
		winonly bool
	}{
		{goos: []string{"linux"}, modules: 4, winonly: false},
		{goos: []string{"darwin", "linux"}, modules: 4, winonly: false},
		{goos: []string{"windows"}, modules: 5, winonly: true},
		{goos: []string{"linux", "windows"}, modules: 5, winonly: true},
	} {
		t.Run(strings.Join(tc.goos, ","), func(t *testing.T) {
			cfg := fixtureConfig(t, "./cmd/app")
			cfg.goos = tc.goos
			res, err := generate(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if got := bytes.Contains(res.text, []byte("\nexample.com/winonly v1.1.0 ")); got != tc.winonly || res.modules != tc.modules {
				t.Errorf("modules = %d, winonly listed = %v; want %d, %v", res.modules, got, tc.modules, tc.winonly)
			}
		})
	}
}

// TestGPLDependencyFailsTheGate is the acceptance case of README S18 for the Go side: a GPL-3.0 fixture dependency
// fails the gate, and no file is written.
func TestGPLDependencyFailsTheGate(t *testing.T) {
	res, err := generate(t.Context(), fixtureConfig(t, "./cmd/gplapp"))
	var gate *gateError
	if !errors.As(err, &gate) {
		t.Fatalf("err = %v, want a *gateError", err)
	}
	if res.text != nil {
		t.Error("the gate failed but generate returned a text")
	}
	want := "example.com/gpl v1.0.0: GPL-3.0-or-later may not be shipped"
	if !slices.Contains(gate.problems, want) {
		t.Errorf("problems = %q, want %q", gate.problems, want)
	}
	for _, p := range gate.problems {
		if strings.HasPrefix(p, "example.com/mit ") {
			t.Errorf("unexpected problem for an MIT module: %q", p)
		}
	}
}

func TestUnrecognizedAndMissingLicensesFailTheGate(t *testing.T) {
	_, err := generate(t.Context(), fixtureConfig(t, "./cmd/badapp"))
	var gate *gateError
	if !errors.As(err, &gate) {
		t.Fatalf("err = %v, want a *gateError", err)
	}
	for _, want := range []string{
		"example.com/custom v0.1.0: LICENSE is not a license text licensecheck recognizes (0% of it matches a known license, 75% needed)",
		"example.com/nolicense v0.1.0: no LICENSE, LICENCE, COPYING or UNLICENSE file in the module root",
	} {
		if !slices.Contains(gate.problems, want) {
			t.Errorf("problems = %q, want %q", gate.problems, want)
		}
	}
}

// TestMPLNeedsAnException: go-licenses lets MPL-2.0 through as "reciprocal"; notices needs an exceptions.md entry.
func TestMPLNeedsAnException(t *testing.T) {
	cfg := fixtureConfig(t, "./cmd/app")
	cfg.exceptionsPath = filepath.Join(t.TempDir(), "exceptions.md")
	if err := os.WriteFile(cfg.exceptionsPath, []byte("# License exceptions\n\nNone.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := generate(t.Context(), cfg)
	var gate *gateError
	if !errors.As(err, &gate) {
		t.Fatalf("err = %v, want a *gateError", err)
	}
	want := "example.com/mpl v0.2.0: MPL-2.0 is not on the shipped allowlist: remove the dependency, or list it with a reason in deploy/notices/exceptions.md"
	if !slices.Contains(gate.problems, want) {
		t.Errorf("problems = %q, want %q", gate.problems, want)
	}
}

func TestExtraLicensesAreGated(t *testing.T) {
	cfg := fixtureConfig(t, "./cmd/app")
	cfg.extraPath = filepath.Join(t.TempDir(), "extra.txt")
	if err := os.WriteFile(cfg.extraPath, []byte("Some code\nLicense: GPL-3.0-only\n\n(text)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := generate(t.Context(), cfg)
	var gate *gateError
	if !errors.As(err, &gate) || !strings.Contains(gate.Error(), "License: GPL-3.0-only is not on the shipped allowlist") {
		t.Fatalf("err = %v, want a gate failure for the GPL entry of extra.txt", err)
	}
}

func TestMissingInputs(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*config)
		want string
	}{
		{"npm", func(c *config) { c.npmPath += ".missing" }, "npm run build writes it"},
		{"extra", func(c *config) { c.extraPath += ".missing" }, "the notices for adapted code"},
		{"exceptions", func(c *config) { c.exceptionsPath += ".missing" }, "the license exceptions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fixtureConfig(t, "./cmd/app")
			tc.edit(&cfg)
			_, err := generate(t.Context(), cfg)
			if err == nil || !strings.Contains(err.Error(), "is missing") || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want a missing-input error mentioning %q", err, tc.want)
			}
		})
	}
}

func TestReadLicenseFiles(t *testing.T) {
	td := testdataDir(t)
	type file struct {
		name  string
		gated bool
		ids   string
	}
	summarize := func(files []licenseFile) []file {
		var out []file
		for _, f := range files {
			out = append(out, file{f.name, f.gated, strings.Join(f.ids, ",")})
		}
		return out
	}

	dual := t.TempDir()
	mit, err := os.ReadFile(filepath.Join(td, "mods", "mit", "LICENSE.txt"))
	if err != nil {
		t.Fatal(err)
	}
	apache, err := os.ReadFile(filepath.Join(td, "mods", "apache", "LICENSE"))
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"LICENSE-MIT":      mit,
		"LICENSE-APACHE":   apache,
		"README.md":        []byte("# dual\n"),
		"license.go":       []byte("package dual\n"), // source code, not a license file
		"license_test.go":  []byte("package dual\n"),
		"notice_render.ts": []byte("export {};\n"),
	} {
		if err := os.WriteFile(filepath.Join(dual, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dual, "LICENSES"), 0o750); err != nil { // a directory is not a license file
		t.Fatal(err)
	}

	for _, tc := range []struct {
		dir  string
		want []file
	}{
		// A primary LICENSE: other LICENSE* files are reproduced, not gated. NOTICE files come last.
		{filepath.Join(td, "mods", "bsd"), []file{{"LICENSE", true, "BSD-3-Clause"}, {"LICENSE-THIRD-PARTY.md", false, ""}}},
		{filepath.Join(td, "mods", "apache"), []file{{"LICENSE", true, "Apache-2.0"}, {"NOTICE", false, ""}}},
		{filepath.Join(td, "mods", "mit"), []file{{"LICENSE.txt", true, "MIT"}}},
		{filepath.Join(td, "mods", "winonly"), []file{{"COPYING", true, "ISC"}}},
		{filepath.Join(td, "mods", "nolicense"), nil},
		// No primary file: every LICENSE* file is gated.
		{dual, []file{{"LICENSE-APACHE", true, "Apache-2.0"}, {"LICENSE-MIT", true, "MIT"}}},
	} {
		t.Run(filepath.Base(tc.dir), func(t *testing.T) {
			files, err := readLicenseFiles(tc.dir)
			if err != nil {
				t.Fatal(err)
			}
			if got := summarize(files); !slices.Equal(got, tc.want) {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestLicensesOf(t *testing.T) {
	gated := func(name string, coverage float64, ids ...string) licenseFile {
		return licenseFile{name: name, gated: true, ids: ids, coverage: coverage}
	}
	none := func(string) bool { return false }
	mpl := func(id string) bool { return id == "MPL-2.0" }
	for _, tc := range []struct {
		name      string
		files     []licenseFile
		exception func(string) bool
		ids       string
		problems  []string
	}{
		{"mit", []licenseFile{gated("LICENSE", 98, "MIT")}, none, "MIT", nil},
		{"dual", []licenseFile{gated("LICENSE", 85, "Apache-2.0", "MIT")}, none, "Apache-2.0,MIT", nil},
		{"notice only", []licenseFile{{name: "NOTICE", notice: true}}, none, "",
			[]string{"no LICENSE, LICENCE, COPYING or UNLICENSE file in the module root"}},
		{"low coverage", []licenseFile{gated("LICENSE", 40, "MIT")}, none, "MIT",
			[]string{"LICENSE is not a license text licensecheck recognizes (40% of it matches a known license, 75% needed)"}},
		{"mpl excepted", []licenseFile{gated("LICENSE", 100, "MPL-2.0")}, mpl, "MPL-2.0", nil},
		{"mpl", []licenseFile{gated("LICENSE", 100, "MPL-2.0")}, none, "MPL-2.0",
			[]string{"MPL-2.0 is not on the shipped allowlist: remove the dependency, or list it with a reason in deploy/notices/exceptions.md"}},
		// Even an exception cannot let the GPL family through.
		{"lgpl", []licenseFile{gated("COPYING.LESSER", 100, "LGPL-3.0")}, func(string) bool { return true }, "LGPL-3.0",
			[]string{"LGPL-3.0 may not be shipped"}},
		{"agpl", []licenseFile{gated("LICENSE", 100, "AGPL-3.0")}, none, "AGPL-3.0", []string{"AGPL-3.0 may not be shipped"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ids, problems := licensesOf(tc.files, tc.exception)
			if strings.Join(ids, ",") != tc.ids || !slices.Equal(problems, tc.problems) {
				t.Errorf("got %q, %q; want %q, %q", ids, problems, tc.ids, tc.problems)
			}
		})
	}
}

// TestAllowlistMatchesNPM: Go modules and npm packages ship under the same allowlist (06 §8.3), which
// web/scripts/licenses.mjs keeps as ALLOWLIST.
func TestAllowlistMatchesNPM(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "web", "scripts", "licenses.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`(?s)export const ALLOWLIST = Object\.freeze\(\[(.*?)\]\)`).FindSubmatch(src)
	if block == nil {
		t.Fatal("web/scripts/licenses.mjs has no `export const ALLOWLIST = Object.freeze([...])`")
	}
	var npm []string
	for _, m := range regexp.MustCompile(`'([^']+)'`).FindAllSubmatch(block[1], -1) {
		npm = append(npm, string(m[1]))
	}
	var goList []string
	for id := range allowlist {
		goList = append(goList, id)
	}
	slices.Sort(npm)
	slices.Sort(goList)
	if !slices.Equal(npm, goList) {
		t.Errorf("allowlists differ:\n  licenses.mjs: %q\n  notices:      %q", npm, goList)
	}
}

func TestNeverExcepted(t *testing.T) {
	for _, id := range []string{"GPL-2.0", "GPL-3.0-or-later", "AGPL-3.0-only", "LGPL-2.1", "SSPL-1.0", "BUSL-1.1"} {
		if !neverExcepted(id) {
			t.Errorf("neverExcepted(%q) = false", id)
		}
	}
	for _, id := range []string{"MPL-2.0", "EPL-2.0", "MIT", "CDDL-1.0"} {
		if neverExcepted(id) {
			t.Errorf("neverExcepted(%q) = true", id)
		}
	}
}

func TestParseExceptions(t *testing.T) {
	const header = "# Exceptions\n\nSome prose | with a bar.\n\n| Module | License | Reason |\n|:---|---|---:|\n"
	for _, tc := range []struct {
		name, rows string
		want       []string // module license
		err        string
	}{
		{"empty", "", nil, ""},
		{"one", "| `example.com/a` | MPL-2.0 | Unmodified library. |\n", []string{"example.com/a MPL-2.0"}, ""},
		{"two", "| `example.com/a` | MPL-2.0 | x |\n| example.com/b | EPL-2.0 | y |\n",
			[]string{"example.com/a MPL-2.0", "example.com/b EPL-2.0"}, ""},
		{"no reason", "| `example.com/a` | MPL-2.0 | |\n", nil, "line 7: example.com/a: an exception needs a reason"},
		{"gpl", "| `example.com/a` | GPL-3.0 | we like it |\n", nil, "line 7: example.com/a: GPL-3.0 may not be shipped, not even with an exception"},
		{"allowlisted", "| `example.com/a` | MIT | x |\n", nil, "line 7: example.com/a: MIT is on the allowlist and needs no exception"},
		{"cells", "| `example.com/a` | MPL-2.0 |\n", nil, "line 7: want 3 cells"},
		{"no module", "| `` | MPL-2.0 | x |\n", nil, "line 7: the first cell must be a module path"},
		{"duplicate", "| `example.com/a` | MPL-2.0 | x |\n| `example.com/a` | MPL-2.0 | y |\n", nil,
			"line 8: example.com/a (MPL-2.0) is already listed on line 7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			list, err := parseExceptions(header + tc.rows)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, e := range list {
				got = append(got, e.module+" "+e.license)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCheckExtra(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		entries    int
		problems   int
	}{
		{"ok", "A\nLicense: MIT\n\nB\n  License: BSD-3-Clause, Apache-2.0\n", 2, 0},
		{"none", "Nothing adapted yet.\n", 0, 1},
		{"gpl", "A\nLicense: GPL-2.0-only\n", 1, 1},
		{"empty", "A\nLicense:\n", 1, 1},
		{"not at line start", "The License: GPL-3.0 is mentioned in prose.\nLicense: MIT\n", 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, problems := checkExtra(tc.text)
			if n != tc.entries || len(problems) != tc.problems {
				t.Errorf("got %d entries, problems %q; want %d entries, %d problems", n, problems, tc.entries, tc.problems)
			}
		})
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"":                             "",
		"\n\n  \n":                     "",
		"\ufeffa\r\nb  \r\n\r\n":       "a\nb\n",
		"\n\nline one\t\n\nline two\n": "line one\n\nline two\n",
		"old mac\rline":                "old mac\nline\n",
	} {
		if got := normalize(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestModuleTitle(t *testing.T) {
	for _, tc := range []struct {
		m    linkedModule
		want string
	}{
		{linkedModule{path: "a.example/x", version: "v1.0.0"}, "a.example/x v1.0.0"},
		{linkedModule{path: "a.example/x", version: "v1.0.0", replace: &goModule{Path: "b.example/x", Version: "v1.0.1"}},
			"a.example/x v1.0.0 => b.example/x v1.0.1"},
		{linkedModule{path: "a.example/x", version: "v1.0.0", replace: &goModule{Path: "../x", Dir: "/somewhere/x"}},
			"a.example/x v1.0.0 (replaced by a local directory)"},
	} {
		if got := moduleTitle(tc.m); got != tc.want {
			t.Errorf("moduleTitle(%+v) = %q, want %q", tc.m, got, tc.want)
		}
	}
}

// TestRun goes through the command line, with the real `go env`.
func TestRun(t *testing.T) {
	td := testdataDir(t)
	out := filepath.Join(t.TempDir(), "THIRD_PARTY_NOTICES")
	args := func(pkg string, extra ...string) []string {
		return append([]string{
			"-root", filepath.Join(td, "app"), "-pkg", pkg, "-npm", filepath.Join(td, "licenses.txt"),
			"-extra", filepath.Join(td, "extra.txt"), "-exceptions", filepath.Join(td, "exceptions.md"),
		}, extra...)
	}
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOFLAGS", "")

	t.Run("writes the file", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := run(t.Context(), args("./cmd/app", "-version", "9.9.9", "-o", out), &stdout, &stderr); code != 0 {
			t.Fatalf("exit %d, stderr:\n%s", code, &stderr)
		}
		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"THIRD_PARTY_NOTICES for isshoni 9.9.9\n", "\nGo standard library and runtime (go1.", "\nexample.com/winonly v1.1.0 "} {
			if !bytes.Contains(data, []byte(want)) {
				t.Errorf("the output has no %q", want)
			}
		}
		if !strings.Contains(stdout.String(), "5 Go modules") || !strings.Contains(stderr.String(), "warning: ") {
			t.Errorf("stdout = %q, stderr = %q", &stdout, &stderr)
		}
		if runtime.GOOS != "windows" {
			if fi, err := os.Stat(out); err != nil || fi.Mode().Perm() != 0o644 {
				t.Errorf("mode = %v, %v; want 0644", fi.Mode(), err)
			}
		}
	})

	t.Run("gate failure writes nothing", func(t *testing.T) {
		gplOut := filepath.Join(t.TempDir(), "THIRD_PARTY_NOTICES")
		var stdout, stderr bytes.Buffer
		if code := run(t.Context(), args("./cmd/gplapp", "-version", "9.9.9", "-o", gplOut), &stdout, &stderr); code != 1 {
			t.Fatalf("exit %d, want 1; stderr:\n%s", code, &stderr)
		}
		if !strings.Contains(stderr.String(), "example.com/gpl v1.0.0: GPL-3.0-or-later may not be shipped") {
			t.Errorf("stderr = %q", &stderr)
		}
		if _, err := os.Stat(gplOut); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the output exists after a failed gate: %v", err)
		}
	})

	for _, tc := range []struct {
		name string
		args []string
		code int
	}{
		{"help", []string{"-h"}, 0},
		{"no version", args("./cmd/app"), 2},
		{"extra argument", args("./cmd/app", "-version", "1", "stray"), 2},
		{"bad flag", []string{"-nope"}, 2},
		{"empty goos", args("./cmd/app", "-version", "1", "-goos", ","), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(t.Context(), tc.args, &stdout, &stderr); code != tc.code {
				t.Errorf("exit %d, want %d; stderr:\n%s", code, tc.code, &stderr)
			}
		})
	}
}

// TestGoLicensesGate runs the first gate, go-licenses with the flags of `task licenses` (06 §8.3), over the
// fixtures: the GPL-3.0 dependency fails it and the app passes (MPL-2.0 is "reciprocal", which it allows; notices
// is the gate for MPL). It needs the pinned go-licenses: ISSHONI_GO_LICENSES, or .bin/go-licenses after
// `task tools`; the CI licenses job runs it with that variable set.
//
// go-licenses does not identify a license from the short notices the fixtures carry, so the test runs it over a
// copy of the fixtures with the full GPL-3.0, MPL-2.0 and Apache-2.0 texts, taken from the license assets of
// github.com/google/licenseclassifier/v2 (go-licenses' own classifier, pinned through tools/go.mod). The repository
// itself holds no GPL text.
func TestGoLicensesGate(t *testing.T) {
	bin := os.Getenv("ISSHONI_GO_LICENSES")
	if bin == "" {
		exe := ""
		if runtime.GOOS == "windows" {
			exe = ".exe"
		}
		bin = filepath.Join("..", "..", ".bin", "go-licenses"+exe)
		if _, err := os.Stat(bin); err != nil { //nolint:gosec // G703: the repository's own .bin/
			t.Skip("go-licenses is not built: run task tools:go-licenses, or set ISSHONI_GO_LICENSES")
		}
	}
	bin, err := filepath.Abs(bin)
	if err != nil {
		t.Fatal(err)
	}

	// The classifier's module directory, from the tools module (the parent directory).
	out, err := runGo(t.Context(), "..", []string{"GOFLAGS=", "GOWORK=off"}, "mod", "download", "-json", "github.com/google/licenseclassifier/v2")
	if err != nil {
		t.Fatal(err)
	}
	var classifier goModule
	if err := json.Unmarshal(out, &classifier); err != nil || classifier.Dir == "" {
		t.Fatalf("go mod download -json printed %s (%v)", out, err)
	}
	assets := filepath.Join(classifier.Dir, "assets", "License")

	dir := t.TempDir()
	td := testdataDir(t)
	for _, sub := range []string{"app", "mods"} {
		if err := os.CopyFS(filepath.Join(dir, sub), os.DirFS(filepath.Join(td, sub))); err != nil {
			t.Fatal(err)
		}
	}
	for dst, src := range map[string]string{
		filepath.Join("mods", "gpl", "COPYING"):    filepath.Join("GPL-3.0", "license.txt"),
		filepath.Join("mods", "mpl", "LICENSE"):    filepath.Join("MPL-2.0", "license.txt"),
		filepath.Join("mods", "apache", "LICENSE"): filepath.Join("Apache-2.0", "pristine.txt"),
	} {
		text, err := os.ReadFile(filepath.Join(assets, src))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, dst), text, 0o600); err != nil { //nolint:gosec // G703: a path in t.TempDir()
			t.Fatal(err)
		}
	}

	check := func(pkg string) (string, error) {
		//nolint:gosec // G702: bin is the go-licenses binary the developer or CI named in ISSHONI_GO_LICENSES
		cmd := exec.CommandContext(t.Context(), bin, "check", pkg, "--ignore", "example.com/app",
			"--disallowed_types=forbidden,restricted,unknown")
		cmd.Dir = filepath.Join(dir, "app")
		cmd.Env = append(os.Environ(), "GOPROXY=off", "GOFLAGS=", "GOWORK=off", "GOOS=linux", "CGO_ENABLED=0")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	if out, err := check("./cmd/app"); err != nil {
		t.Errorf("go-licenses check ./cmd/app: %v\n%s", err, out)
	}
	report, err := check("./cmd/gplapp")
	if err == nil {
		t.Fatalf("go-licenses check ./cmd/gplapp passed, want a failure for the GPL-3.0 dependency:\n%s", report)
	}
	if !strings.Contains(report, "example.com/gpl") || !strings.Contains(report, "GPL-3.0") {
		t.Errorf("go-licenses failed, but not for example.com/gpl (GPL-3.0):\n%s", report)
	}
}
