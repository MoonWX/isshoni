package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
)

// TestCommittedNSISSources: the committed scripts pass 06 §6.10's strings rule, and the texts the acceptance names
// come from the language file: the Finish page's "Next:" line (06 §6.3), the /UPDATE banner and the exit-8 message
// (06 §6.4).
func TestCommittedNSISSources(t *testing.T) {
	root := repoRoot(t)
	r := checkNSISStrings(filepath.Join(root, filepath.FromSlash(nsisDir)))
	if len(r.problems) > 0 {
		t.Fatalf("the strings rule fails on the committed scripts:\n%s", strings.Join(r.problems, "\n"))
	}
	lang, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(nsisDir), filepath.FromSlash(nsisLangFile)))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		`LangString FinishNext ${LANG_ENGLISH} "Next: go back to your browser and click Open isshoni"`,
		`LangString UpdatingBanner ${LANG_ENGLISH} "Updating isshoni to ${VERSION}…"`,
		`LangString AnotherSetupRunning ${LANG_ENGLISH} "Another isshoni setup is running. Wait for it to finish, then try again."`,
		`LangString AppRunning ${LANG_ENGLISH} "isshoni is running. Quit it from the tray icon, then click Retry."`,
		`LangString UnPurgeOption ${LANG_ENGLISH} "Also remove my settings and logs on this PC"`,
	} {
		if !strings.Contains(strings.ReplaceAll(string(lang), "\r\n", "\n"), line+"\n") {
			t.Errorf("%s lacks the line\n  %s", nsisLangFile, line)
		}
	}
	installer, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(nsisDir), "installer.nsi"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`!define MUI_FINISHPAGE_TEXT "$(FinishInstalled)$\r$\n$\r$\n$(FinishNext)"`,
		`Banner::show "$(UpdatingBanner)"`,
	} {
		if !strings.Contains(string(installer), want) {
			t.Errorf("installer.nsi lacks %s", want)
		}
	}
	common, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(nsisDir), "common.nsh"))
	if err != nil {
		t.Fatal(err)
	}
	// 03 §5.4's runtime minimum (Evergreen 120.0).
	if !strings.Contains(string(common), `!define WEBVIEW2_MIN "120.0.0.0"`) {
		t.Error("common.nsh doesn't define WEBVIEW2_MIN as 120.0.0.0")
	}
	// The scripts are ASCII; only the language file holds other characters (UTF-8).
	for _, name := range nsisChecked {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(nsisDir), name))
		if err != nil {
			t.Fatal(err)
		}
		for i, b := range data {
			if b >= 0x80 {
				t.Errorf("%s has a non-ASCII byte at offset %d", name, i)
				break
			}
		}
	}
}

// writeNSIS writes a minimal script set for the strings rule; files maps a script name to its body.
func writeNSIS(t *testing.T, files map[string]string, lang string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range nsisChecked {
		writeFile(t, filepath.Join(dir, name), files[name])
	}
	writeFile(t, filepath.Join(dir, filepath.FromSlash(nsisLangFile)), lang)
	return dir
}

func TestNSISStringsRule(t *testing.T) {
	const lang = "; strings\nLangString Hello ${LANG_ENGLISH} \"Hello ${VERSION}\"\nLangString Bye ${LANG_ENGLISH} 'Bye'\n"
	good := strings.Join([]string{
		`MessageBox MB_OK "$(Hello)" /SD IDOK`,
		`MessageBox MB_OK|MB_ICONSTOP "$R8" /SD IDOK ; a variable that holds a LangString`,
		`DetailPrint "$(Bye)"`,
		`${NSD_CreateLabel} 0 0 100% 12u "$(Hello)"`,
		`nsDialogs::CreateControl STATIC 0 0 0 0 10u 10u "$(Bye)"`,
		`Banner::show /set 76 "$(Hello)" "$(Bye)"`,
		`!define MUI_FINISHPAGE_TEXT "$(Hello)$\r$\n$(Bye)"`,
		`!insertmacro MUI_HEADER_TEXT "$(Hello)" "$(Bye)"`,
		`; MessageBox MB_OK "a comment"`,
		`/* DetailPrint "a block`,
		`   comment" */`,
		`DetailPrint \`,
		`  "$(Hello)"`,
		`StrCpy $0 "not a visible command"`,
		`!define MUI_FINISHPAGE_RUN_FUNCTION FinishRun`,
	}, "\n")
	r := checkNSISStrings(writeNSIS(t, map[string]string{"installer.nsi": good}, lang))
	if len(r.problems) > 0 {
		t.Fatalf("good scripts fail:\n%s", strings.Join(r.problems, "\n"))
	}

	for _, tc := range []struct{ line, want string }{
		{`MessageBox MB_OK "Hello there" /SD IDOK`, `MessageBox has the literal text "Hello there"`},
		{`messagebox MB_OK 'Hello'`, `has the literal text "Hello"`},
		{`DetailPrint "Copying $0"`, `DetailPrint has the literal text "Copying $0"`},
		{`DetailPrint Copying`, `DetailPrint has the literal text "Copying"`},
		{`${NSD_CreateCheckbox} 0 0 100% 12u "Create a shortcut"`, `has the literal text "Create a shortcut"`},
		{`nsDialogs::CreateControl BUTTON 0 0 0 0 10u 10u "Click"`, `has the literal text "Click"`},
		{`Banner::show "Updating isshoni ${VERSION}"`, `Banner::show has the literal text`},
		{`Banner::show /set 76 "Please wait" "$(Hello)"`, `has the literal text "Please wait"`},
		{`!define MUI_FINISHPAGE_TEXT "Next: go back"`, `has the literal text "Next: go back"`},
		{`!define /ifndef MUI_WELCOMEPAGE_TITLE "Welcome"`, `has the literal text "Welcome"`},
		{`!insertmacro MUI_HEADER_TEXT "Options" "$(Bye)"`, `has the literal text "Options"`},
		{"DetailPrint \\\n  \"Split literal\"", `has the literal text "Split literal"`},
		{`DetailPrint "$(Missing)"`, `$(Missing) is no LangString`},
	} {
		dir := writeNSIS(t, map[string]string{"common.nsh": tc.line, "installer.nsi": good}, lang)
		r := checkNSISStrings(dir)
		if !slices.ContainsFunc(r.problems, func(p string) bool { return strings.Contains(p, tc.want) }) {
			t.Errorf("%q: problems %q; want one with %q", tc.line, r.problems, tc.want)
		}
	}

	for _, tc := range []struct{ lang, want string }{
		{lang + "LangString Unused ${LANG_ENGLISH} \"x\"\n", "LangString Unused is never used"},
		{lang + "LangString Hello ${LANG_ENGLISH} \"again\"\n", "LangString Hello is defined 2 times"},
		{lang + "LangString Other ${LANG_FRENCH} \"x\"\n", "Other is not for ${LANG_ENGLISH}"},
		{lang + "!define X 1\n", "only LangString lines belong here"},
		{lang + "LangString Short ${LANG_ENGLISH}\n", "LangString takes a name, a language and a text"},
	} {
		r := checkNSISStrings(writeNSIS(t, map[string]string{"installer.nsi": good}, tc.lang))
		if !slices.ContainsFunc(r.problems, func(p string) bool { return strings.Contains(p, tc.want) }) {
			t.Errorf("lang %q: problems %q; want one with %q", tc.lang, r.problems, tc.want)
		}
	}

	r = checkNSISStrings(t.TempDir())
	if len(r.problems) != len(nsisChecked)+1 {
		t.Errorf("an empty folder: %d problems %q; want one per missing file", len(r.problems), r.problems)
	}
}

func TestNSISLines(t *testing.T) {
	src := "Name \"a b\" 'c d' `e f` g ; comment\r\n# hash comment\n  \n" +
		"Str \"$\\\"quoted$\\\"\" x#y\nJoined \\\nnext\n/* block\nstill */ After\n"
	got := nsisLines(src)
	want := [][]string{
		{"Name", "a b", "c d", "e f", "g"},
		{"Str", `$\"quoted$\"`, "x#y"},
		{"Joined", "next"},
		{"After"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines %+v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if !slices.Equal(got[i].words, want[i]) {
			t.Errorf("line %d: %q, want %q", i, got[i].words, want[i])
		}
	}
	if got[0].num != 1 || got[2].num != 5 || got[3].num != 8 {
		t.Errorf("line numbers %d %d %d, want 1 5 8", got[0].num, got[2].num, got[3].num)
	}
}

// TestStageBinaries: 06 §9.3, every binary of the stage is isshoni's own or matches a files glob of
// native-windows.json; a file that starts with MZ counts whatever its name.
func TestStageBinaries(t *testing.T) {
	root := repoRoot(t)
	stage := t.TempDir()
	for _, f := range []string{"isshoni.exe", "isshoni-media.dll", "uninstall.exe", "avcodec-63.dll", "avutil-61.dll"} {
		writeFile(t, filepath.Join(stage, f), "MZ")
	}
	writeFile(t, filepath.Join(stage, "install-files.txt"), "isshoni.exe\r\n")
	writeFile(t, filepath.Join(stage, "licenses", "LICENSE.txt"), "Apache")
	if r := checkStageBinaries(root, stage); len(r.problems) > 0 {
		t.Fatalf("a good stage fails: %q", r.problems)
	}
	for _, bad := range []string{"opus.dll", "licenses/hidden.dat", "tools/isshoni.exe", "x.EXE"} {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "isshoni.exe"), "MZ")
		writeFile(t, filepath.Join(dir, filepath.FromSlash(bad)), "MZ\x90")
		r := checkStageBinaries(root, dir)
		if len(r.problems) != 1 || !strings.Contains(r.problems[0], bad) {
			t.Errorf("%s: problems %q; want one naming it", bad, r.problems)
		}
	}
	if r := checkStageBinaries(root, t.TempDir()); len(r.problems) != 1 || !strings.Contains(r.problems[0], "no binary") {
		t.Errorf("an empty stage: %q", r.problems)
	}
}

func TestInstallerSize(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		size        int64
		fail, warns bool
	}{{1 << 20, false, false}, {installerWarnSize + 1, false, true}, {installerMaxSize + 1, true, false}} {
		f := filepath.Join(dir, "setup.exe")
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(f, tc.size); err != nil {
			t.Fatal(err)
		}
		r := checkInstallerSize(f)
		warned := len(r.notes) == 1 && strings.HasPrefix(r.notes[0], "warning:")
		if (len(r.problems) > 0) != tc.fail || warned != tc.warns {
			t.Errorf("size %d: problems %q, notes %q", tc.size, r.problems, r.notes)
		}
	}
	if r := checkInstallerSize(filepath.Join(dir, "missing.exe")); len(r.problems) != 1 {
		t.Errorf("a missing installer: %q", r.problems)
	}
}

// goodBuildInfo is what `go version -m` shows of a release isshoni.exe (06 §5.2 step 3).
func goodBuildInfo() *debug.BuildInfo {
	return &debug.BuildInfo{
		Path: appModule,
		Settings: []debug.BuildSetting{
			{Key: "-buildmode", Value: "exe"}, {Key: "-tags", Value: "production"}, {Key: "-trimpath", Value: "true"},
			{Key: "CGO_ENABLED", Value: "0"}, {Key: "GOARCH", Value: "amd64"}, {Key: "GOOS", Value: "windows"},
		},
	}
}

func withSetting(bi *debug.BuildInfo, key, value string) *debug.BuildInfo {
	out := *bi
	out.Settings = nil
	for _, s := range bi.Settings {
		if s.Key != key {
			out.Settings = append(out.Settings, s)
		}
	}
	if value != "" {
		out.Settings = append(out.Settings, debug.BuildSetting{Key: key, Value: value})
	}
	return &out
}

func TestCheckExeInfo(t *testing.T) {
	data := []byte("MZ ... 0.2.0-rc.1 ...")
	if r := checkExeInfo("isshoni.exe", data, goodBuildInfo(), "0.2.0-rc.1"); len(r.problems) > 0 {
		t.Fatalf("a good exe fails: %q", r.problems)
	}
	other := goodBuildInfo()
	other.Path = "github.com/MoonWX/isshoni/cmd/isshoni"
	for _, tc := range []struct {
		name string
		bi   *debug.BuildInfo
		data []byte
		want string
	}{
		{"e2e tags", withSetting(goodBuildInfo(), "-tags", "e2e"), data, `build tags are "e2e"`},
		{"dev tags", withSetting(goodBuildInfo(), "-tags", "dev"), data, `build tags are "dev"`},
		{"updatetest", withSetting(goodBuildInfo(), "-tags", "production,updatetest"), data, "not exactly"},
		{"no tags", withSetting(goodBuildInfo(), "-tags", ""), data, `build tags are ""`},
		{"no trimpath", withSetting(goodBuildInfo(), "-trimpath", ""), data, "-trimpath"},
		{"cgo", withSetting(goodBuildInfo(), "CGO_ENABLED", "1"), data, "CGO_ENABLED"},
		{"linux", withSetting(goodBuildInfo(), "GOOS", "linux"), data, "GOOS"},
		{"arm64", withSetting(goodBuildInfo(), "GOARCH", "arm64"), data, "GOARCH"},
		{"another program", other, data, "not " + appModule},
		{"unstamped", goodBuildInfo(), []byte("MZ 0.0.0-dev"), "doesn't carry its version"},
		{"test hook", goodBuildInfo(), append(slices.Clone(data), "ISSHONI_UPDATE_TEST_BASE"...), `"ISSHONI_UPDATE_TEST"`},
		{"vite", goodBuildInfo(), append(slices.Clone(data), "http://localhost:5173"...), `"localhost:5173"`},
	} {
		r := checkExeInfo("isshoni.exe", tc.data, tc.bi, "0.2.0-rc.1")
		if !slices.ContainsFunc(r.problems, func(p string) bool { return strings.Contains(p, tc.want) }) {
			t.Errorf("%s: problems %q; want one with %q", tc.name, r.problems, tc.want)
		}
	}
	if r := checkExeInfo("isshoni.exe", make([]byte, exeWarnSize+1), goodBuildInfo(), ""); len(r.notes) != 1 {
		t.Errorf("a large exe: notes %q, want the size warning", r.notes)
	}
}

// TestCheckExeBuilt cross-builds a small program at the app's import path as desktop:exe:windows does (-trimpath,
// no cgo, the version ldflag) and checks it, then the same with TAGS=e2e. It also shows why the version is checked
// in the bytes: -trimpath keeps -ldflags out of the build info.
func TestCheckExeBuilt(t *testing.T) {
	if testing.Short() {
		t.Skip("builds Go programs")
	}
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go on PATH")
	}
	mod := t.TempDir()
	writeFile(t, filepath.Join(mod, "go.mod"), "module github.com/MoonWX/isshoni\n\ngo 1.26\n")
	writeFile(t, filepath.Join(mod, "internal", "version", "version.go"),
		"package version\n\nvar version = \"0.0.0-dev\"\n\nfunc Version() string { return version }\n")
	writeFile(t, filepath.Join(mod, "cmd", "isshoni-desktop", "main.go"),
		"package main\n\nimport \"github.com/MoonWX/isshoni/internal/version\"\n\nfunc main() { println(version.Version()) }\n")
	build := func(tags, out string) string {
		t.Helper()
		exe := filepath.Join(t.TempDir(), out)
		//nolint:gosec // G204: the go command with the test's own arguments
		cmd := exec.CommandContext(t.Context(), gobin, "build", "-trimpath", "-tags", tags, "-ldflags",
			"-s -w -X github.com/MoonWX/isshoni/internal/version.version=0.0.0-ci.42", "-o", exe, "./cmd/isshoni-desktop")
		cmd.Dir = mod
		cmd.Env = append(os.Environ(), "GOOS=windows", "GOARCH=amd64", "CGO_ENABLED=0", "GOFLAGS=", "GOTOOLCHAIN=local")
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build: %v\n%s", err, b)
		}
		return exe
	}
	exe := build("production", "isshoni.exe")
	if r := checkExe(exe, "0.0.0-ci.42"); len(r.problems) > 0 {
		t.Errorf("a production build fails: %q", r.problems)
	}
	if r := checkExe(exe, "0.0.0-ci.43"); len(r.problems) != 1 {
		t.Errorf("the wrong version: %q, want one problem", r.problems)
	}
	if r := checkExe(build("e2e", "isshoni.exe"), "0.0.0-ci.42"); len(r.problems) != 1 || !strings.Contains(r.problems[0], `"e2e"`) {
		t.Errorf("an e2e build: %q", r.problems)
	}
	if r := checkExe(filepath.Join(mod, "go.mod"), "0.0.0-ci.42"); len(r.problems) != 1 || !strings.Contains(r.problems[0], "no Go build info") {
		t.Errorf("not a Go binary: %q", r.problems)
	}
}

func TestCheckCommand(t *testing.T) {
	root := repoRoot(t)
	if code, stdout, stderr := runCmd(t, "-root", root, "check"); code != 0 || !strings.Contains(stdout, "check: passed") {
		t.Errorf("check on the repository: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if code, _, stderr := runCmd(t, "-root", root, "check", "-exe", "x.exe"); code != 2 || !strings.Contains(stderr, "go together") {
		t.Errorf("-exe without -version: exit %d, stderr %q", code, stderr)
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "opus.dll"), "MZ")
	if code, _, stderr := runCmd(t, "-root", root, "check", "-stage", dir); code != 1 || !strings.Contains(stderr, "opus.dll") {
		t.Errorf("a stage with opus.dll: exit %d, stderr %q", code, stderr)
	}
}
