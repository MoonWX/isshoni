package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// desktopConfig is the desktop file's run over the fixtures (docs/m2/06-windows-release.md §9.2): the app for
// windows, with the native components of testdata/native/native-windows.json and the windows-native artifact's
// release folder of testdata/native/release. Its ../ license file is testdata/app/deploy/notices/licenses/, since
// -root is testdata/app.
func desktopConfig(t *testing.T) config {
	t.Helper()
	cfg := fixtureConfig(t, "./cmd/app")
	td := testdataDir(t)
	cfg.goos = []string{"windows"}
	cfg.goarch = []string{"amd64"}
	cfg.nativePath = filepath.Join(td, "native", "native-windows.json")
	cfg.nativeRoot = filepath.Join(td, "native", "release")
	return cfg
}

// copyNativeFixture copies the native fixture (the JSON, the release folder and the app's deploy/ folder with the
// ../ license file) into a temporary directory and points cfg at the copy, so a test can change it.
func copyNativeFixture(t *testing.T, cfg *config) {
	t.Helper()
	dir := t.TempDir()
	td := testdataDir(t)
	if err := os.CopyFS(filepath.Join(dir, "native"), os.DirFS(filepath.Join(td, "native"))); err != nil {
		t.Fatal(err)
	}
	cfg.nativePath = filepath.Join(dir, "native", "native-windows.json")
	cfg.nativeRoot = filepath.Join(dir, "native", "release")
}

// TestDesktopGolden is 06 §16's "tools/notices desktop golden": the Go modules of the app for windows, the web
// client's packages, the native section and the adapted code, in that order.
func TestDesktopGolden(t *testing.T) {
	res, err := generate(t.Context(), desktopConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "THIRD_PARTY_NOTICES-desktop.golden")
	if *update {
		if err := os.WriteFile(golden, res.text, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run go test -run TestDesktopGolden -update)", err)
	}
	want = bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")) // a Git checkout with core.autocrlf
	if !bytes.Equal(res.text, want) {
		t.Errorf("the output differs from %s (run go test -run TestDesktopGolden -update and review the diff):\n%s", golden, res.text)
	}

	text := string(res.text)
	last := -1
	for _, s := range []string{
		"\n1. Go modules and the Go runtime (5 modules)\n", "\nexample.com/winonly v1.1.0 ",
		"\n2. Web client (bundled JavaScript)\n", "\nreact 19.3.0\n",
		"\n" + nativeSectionTitle + "\n", "\nExample bootstrapper\nLicense: LicenseRef-Example-Distribution\n",
		"\n== deploy/notices/licenses/bootstrapper.txt ==\n",
		"\nFFmpeg (libavcodec, libavutil) 9.0.2\nLicense: LGPL-2.1-or-later\nLinking: dynamic\nFiles: avcodec-*.dll, avutil-*.dll\n",
		"\n== licenses/ffmpeg/COPYING.LGPLv2.1 ==\n",
		"\nlibopus (vcpkg port opus 1.5.2)\nLicense: BSD-3-Clause\n",
		"\n4. Adapted source code and data\n",
	} {
		i := strings.Index(text, s)
		if i < 0 {
			t.Errorf("the output has no %q", s)
			continue
		}
		if i < last {
			t.Errorf("%q is out of order", s)
		}
		last = i
	}
	if res.native != 4 {
		t.Errorf("native = %d, want 4", res.native)
	}
	if len(res.warnings) != 1 || !strings.Contains(res.warnings[0], "example.com/gone") {
		t.Errorf("warnings = %q, want only the stale example.com/gone exception's", res.warnings)
	}
	if strings.Contains(text, testdataDir(t)) {
		t.Error("the output contains a local path")
	}

	// The server's file is unchanged by the native rows of exceptions.md: no native section, no warning for them.
	server, err := generate(t.Context(), fixtureConfig(t, "./cmd/app"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(server.text, []byte("Native components")) || len(server.warnings) != 1 {
		t.Errorf("the server's file has a native section or more warnings: %q", server.warnings)
	}
}

// TestNativeGate covers 06 §9.3's gate and §16's negative cases: a GPL native entry, LGPL without dynamic linking,
// an unlisted vcpkg port and a LicenseRef without an exception each fail, and no file is written.
func TestNativeGate(t *testing.T) {
	type fixture struct {
		t   *testing.T
		cfg *config
	}
	editJSON := func(f fixture, edit func([]map[string]any) []map[string]any) {
		f.t.Helper()
		data, err := os.ReadFile(f.cfg.nativePath)
		if err != nil {
			f.t.Fatal(err)
		}
		var entries []map[string]any
		if err := json.Unmarshal(data, &entries); err != nil {
			f.t.Fatal(err)
		}
		out, err := json.Marshal(edit(entries))
		if err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(f.cfg.nativePath, out, 0o600); err != nil {
			f.t.Fatal(err)
		}
	}
	setField := func(f fixture, name, key string, value any) {
		editJSON(f, func(es []map[string]any) []map[string]any {
			for _, e := range es {
				if e["name"] == name {
					e[key] = value
				}
			}
			return es
		})
	}
	add := func(f fixture, e map[string]any) {
		editJSON(f, func(es []map[string]any) []map[string]any { return append(es, e) })
	}
	write := func(f fixture, rel, text string) {
		f.t.Helper()
		p := filepath.Join(f.cfg.nativeRoot, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			f.t.Fatal(err)
		}
	}
	remove := func(f fixture, rel string) {
		f.t.Helper()
		if err := os.RemoveAll(filepath.Join(f.cfg.nativeRoot, filepath.FromSlash(rel))); err != nil {
			f.t.Fatal(err)
		}
	}
	const ffmpeg = "FFmpeg (libavcodec, libavutil)"

	for _, tc := range []struct {
		name string
		edit func(f fixture)
		want []string
	}{
		{"GPL native entry", func(f fixture) {
			write(f, "licenses/x264/COPYING", "GNU GENERAL PUBLIC LICENSE (fixture)\n")
			add(f, map[string]any{"name": "x264", "license": "GPL-2.0-or-later", "linking": "static, inside avcodec",
				"licenseFiles": []string{"x264/COPYING"}})
		}, []string{"native-windows.json: x264: GPL-2.0-or-later may not be shipped"}},
		{"AGPL native entry", func(f fixture) {
			setField(f, "Example headers", "license", "AGPL-3.0-only")
		}, []string{"native-windows.json: Example headers: AGPL-3.0-only may not be shipped"}},
		{"LGPL without dynamic linking", func(f fixture) {
			setField(f, ffmpeg, "linking", "static, inside isshoni-media.dll")
		}, []string{`native-windows.json: ` + ffmpeg + `: LGPL-2.1-or-later is accepted only with "linking": "dynamic", not "static, inside isshoni-media.dll"`}},
		{"LGPL for another component", func(f fixture) {
			setField(f, "Example headers", "license", "LGPL-2.1-or-later")
			setField(f, "Example headers", "linking", "dynamic")
			setField(f, "Example headers", "files", []string{"example.dll"})
		}, []string{"native-windows.json: Example headers: LGPL-2.1-or-later is accepted only for the FFmpeg DLLs"}},
		{"LGPL without the compliance files", func(f fixture) {
			remove(f, "licenses/ffmpeg/README-LGPL.txt")
		}, []string{"native-windows.json: " + ffmpeg + ": LGPL-2.1-or-later needs the LGPL compliance file licenses/ffmpeg/README-LGPL.txt"}},
		{"another LGPL", func(f fixture) {
			setField(f, ffmpeg, "license", "LGPL-3.0-or-later")
		}, []string{"native-windows.json: " + ffmpeg + ": LGPL-3.0-or-later may not be shipped"}},
		{"unlisted vcpkg port", func(f fixture) {
			write(f, "licenses/vcpkg/speexdsp/copyright", "speexdsp (fixture)\n")
		}, []string{"native-windows.json: the vcpkg port speexdsp is shipped (licenses/vcpkg/speexdsp/copyright) but no entry lists it"}},
		{"listed vcpkg port gone", func(f fixture) {
			remove(f, "licenses/vcpkg/opus")
		}, []string{
			"native-windows.json: the vcpkg port opus is listed but not shipped",
			"native-windows.json: libopus: licenseFiles: licenses/vcpkg/opus/copyright is missing",
		}},
		{"vcpkg port without its copyright file listed", func(f fixture) {
			write(f, "licenses/opus/COPYING", "BSD (fixture)\n")
			setField(f, "libopus", "licenseFiles", []string{"opus/COPYING"})
		}, []string{"native-windows.json: libopus: a vcpkg port's licenseFiles include vcpkg/opus/copyright"}},
		{"LicenseRef without an exception", func(f fixture) {
			f.cfg.exceptionsPath = filepath.Join(f.t.TempDir(), "exceptions.md")
			if err := os.WriteFile(f.cfg.exceptionsPath, []byte("| Module | License | Reason |\n|---|---|---|\n"+
				"| `example.com/mpl` | MPL-2.0 | x |\n"), 0o600); err != nil {
				f.t.Fatal(err)
			}
		}, []string{"native-windows.json: Example bootstrapper: LicenseRef-Example-Distribution needs a row with a reason in deploy/notices/exceptions.md's Native component table"}},
		{"LicenseRef excepted for another name", func(f fixture) {
			setField(f, "Example bootstrapper", "name", "Other bootstrapper")
		}, []string{"native-windows.json: Other bootstrapper: LicenseRef-Example-Distribution needs a row"}},
		{"license not on the allowlist", func(f fixture) {
			setField(f, "Example headers", "license", "MPL-2.0")
		}, []string{"native-windows.json: Example headers: MPL-2.0 is not on the shipped allowlist"}},
		{"missing license file", func(f fixture) {
			remove(f, "licenses/example/LICENSE")
		}, []string{"native-windows.json: Example headers: licenseFiles: licenses/example/LICENSE is missing"}},
		{"empty license file", func(f fixture) {
			write(f, "licenses/example/LICENSE", "\n \n")
		}, []string{"native-windows.json: Example headers: licenseFiles: licenses/example/LICENSE is empty"}},
		{"license file outside the folders", func(f fixture) {
			setField(f, "Example headers", "licenseFiles", []string{"../../../../etc/passwd", "example/../example/LICENSE",
				`example\LICENSE`, "/etc/passwd"})
		}, []string{
			`native-windows.json: Example headers: licenseFiles: "../../../../etc/passwd" leaves the repository`,
			`native-windows.json: Example headers: licenseFiles: "example/../example/LICENSE" is not a clean path`,
			`native-windows.json: Example headers: licenseFiles: "example\\LICENSE" is not a relative path`,
			`native-windows.json: Example headers: licenseFiles: "/etc/passwd" is not a relative path`,
		}},
		{"fields", func(f fixture) {
			add(f, map[string]any{"name": "Bare", "license": "MIT", "files": []string{"sub/x.dll", "[x.dll"}})
		}, []string{
			`native-windows.json: Bare: "linking" says how it is shipped`,
			"native-windows.json: Bare: no licenseFiles",
			`native-windows.json: Bare: files: "sub/x.dll" is not a file name glob`,
			`native-windows.json: Bare: files: "[x.dll" is not a file name glob`,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := desktopConfig(t)
			copyNativeFixture(t, &cfg)
			tc.edit(fixture{t, &cfg})
			res, err := generate(t.Context(), cfg)
			var gate *gateError
			if !errors.As(err, &gate) {
				t.Fatalf("err = %v, want a *gateError", err)
			}
			if res.text != nil {
				t.Error("the gate failed but generate returned a text")
			}
			for _, want := range tc.want {
				if !slices.ContainsFunc(gate.problems, func(p string) bool { return strings.HasPrefix(p, want) }) {
					t.Errorf("no problem starts with %q; problems:\n  %s", want, strings.Join(gate.problems, "\n  "))
				}
			}
			// Only the problems of this case: each other entry still passes.
			for _, p := range gate.problems {
				if strings.HasPrefix(p, "The shipped allowlist is: ") {
					continue
				}
				if !slices.ContainsFunc(tc.want, func(w string) bool { return strings.HasPrefix(p, w) }) {
					t.Errorf("unexpected problem %q", p)
				}
			}
		})
	}
}

// TestNativeInputErrors: a native list that can't be read is an error, not a gate failure.
func TestNativeInputErrors(t *testing.T) {
	for _, tc := range []struct {
		name, json string
		noRoot     bool
		want       string
	}{
		{"unknown field", `[{"name": "a", "license": "MIT", "linking": "x", "licenseFiles": ["a"], "licence": "MIT"}]`, false, `unknown field "licence"`},
		{"empty", `[]`, false, "no entries"},
		{"two values", `[{"name": "a"}] []`, false, "more than one JSON value"},
		{"duplicate", `[{"name": "a"}, {"name": "a"}]`, false, `"a" is listed twice`},
		{"not JSON", `{`, false, "unexpected EOF"},
		{"no native root", `[{"name": "a"}]`, true, "is not a folder"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := desktopConfig(t)
			copyNativeFixture(t, &cfg)
			if err := os.WriteFile(cfg.nativePath, []byte(tc.json), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.noRoot {
				cfg.nativeRoot = filepath.Join(t.TempDir(), "missing")
			}
			_, err := generate(t.Context(), cfg)
			var gate *gateError
			if err == nil || errors.As(err, &gate) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want an input error with %q", err, tc.want)
			}
		})
	}
	cfg := desktopConfig(t)
	cfg.nativePath += ".missing"
	if _, err := generate(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), "is missing") {
		t.Errorf("a missing -native: %v", err)
	}
}

// TestRepoNativeWindows runs the gate over the repository's own deploy/notices/native-windows.json and exceptions.md,
// with a release folder made from what the entries name (the windows-native artifact's layout, 06 §5.1): every
// entry passes, the WebView2 bootstrapper's ../ license file is in the repository, every native exception is used,
// and the FFmpeg entry names native/ffmpeg/pins.json's version and source.
func TestRepoNativeWindows(t *testing.T) {
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config{
		root:       repo,
		nativePath: filepath.Join(repo, "deploy", "notices", "native-windows.json"),
		nativeRoot: t.TempDir(),
	}
	data, err := os.ReadFile(cfg.nativePath)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := parseNative(data)
	if err != nil {
		t.Fatal(err)
	}
	stub := func(rel string) {
		p := filepath.Join(cfg.nativeRoot, "licenses", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("license text\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range entries {
		for _, rel := range e.LicenseFiles {
			if !strings.HasPrefix(rel, "../") {
				stub(rel)
			}
		}
	}
	for _, name := range lgplComplianceFiles {
		stub("ffmpeg/" + name)
	}

	text, err := os.ReadFile(filepath.Join(repo, "deploy", "notices", "exceptions.md"))
	if err != nil {
		t.Fatal(err)
	}
	exceptions, err := parseExceptions(string(text))
	if err != nil {
		t.Fatal(err)
	}
	comps, problems, err := loadNative(cfg, exceptions)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Errorf("the repository's native-windows.json fails the gate:\n  %s", strings.Join(problems, "\n  "))
	}
	for _, e := range exceptions {
		if e.native && !e.used {
			t.Errorf("exceptions.md line %d: the native exception for %s matches no entry", e.line, e.module)
		}
	}
	if len(comps) != len(entries) {
		t.Errorf("%d components, want %d", len(comps), len(entries))
	}

	var pins struct {
		FFmpeg struct {
			Version string `json:"version"`
			URL     string `json:"url"`
		} `json:"ffmpeg"`
	}
	pinsData, err := os.ReadFile(filepath.Join(repo, "native", "ffmpeg", "pins.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(pinsData, &pins); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.License == lgplID {
			found = true
			if e.Version != pins.FFmpeg.Version || e.Source != pins.FFmpeg.URL {
				t.Errorf("the FFmpeg entry names %s from %s; native/ffmpeg/pins.json %s from %s", e.Version, e.Source,
					pins.FFmpeg.Version, pins.FFmpeg.URL)
			}
		}
	}
	if !found {
		t.Error("no FFmpeg (LGPL-2.1-or-later) entry")
	}
}

func TestResolveLicenseFile(t *testing.T) {
	cfg := config{root: filepath.FromSlash("/repo"), nativeRoot: filepath.FromSlash("/art/release")}
	for _, tc := range []struct {
		rel, file, shown, err string
	}{
		{"ffmpeg/LICENSE.md", "/art/release/licenses/ffmpeg/LICENSE.md", "licenses/ffmpeg/LICENSE.md", ""},
		{"../deploy/notices/licenses/x.txt", "/repo/deploy/notices/licenses/x.txt", "deploy/notices/licenses/x.txt", ""},
		{"../../x", "", "", "leaves the repository"},
		{"..", "", "", "not a clean path"},
		{"./a", "", "", "not a clean path"},
		{"", "", "", "not a relative path"},
		{`C:\x`, "", "", "not a relative path"},
	} {
		file, shown, err := resolveLicenseFile(tc.rel, cfg)
		switch {
		case tc.err != "":
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%q: err = %v, want %q", tc.rel, err, tc.err)
			}
		case err != nil || file != filepath.FromSlash(tc.file) || shown != tc.shown:
			t.Errorf("%q: %q, %q, %v; want %q, %q", tc.rel, file, shown, err, tc.file, tc.shown)
		}
	}
}
