package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestNSISPrint: the makensis command line carries -WX and exactly 06 §6.2's defines, with absolute paths.
func TestNSISPrint(t *testing.T) {
	root := t.TempDir()
	webview2 := filepath.Join(root, "MicrosoftEdgeWebview2Setup.exe")
	prebuilt := filepath.Join(root, "uninstall.exe")
	writeFile(t, webview2, "bootstrapper")
	writeFile(t, prebuilt, "MZ")
	script := filepath.Join(root, "deploy", "windows", "nsis")

	code, stdout, stderr := runCmd(t, "-root", root, "nsis", "-version", "0.2.0-rc.2", "-webview2", webview2,
		"-print", "-makensis", "mk")
	want := strings.Join([]string{"mk", "-WX", "-V2", "-NOCONFIG", "-INPUTCHARSET", "UTF8", "-DVERSION=0.2.0-rc.2",
		"-DWINVER=0.2.0.2", "-DSTAGE=" + filepath.Join(root, "dist-desktop", "stage"),
		"-DOUTFILE=" + filepath.Join(root, "dist-desktop", "isshoni-desktop_0.2.0-rc.2_windows_amd64_setup.exe"),
		"-DWEBVIEW2_SETUP=" + webview2, filepath.Join(script, "installer.nsi")}, " ") + "\n"
	if code != 0 || stdout != want {
		t.Errorf("exit %d, stderr %q, stdout\n%q, want\n%q", code, stderr, stdout, want)
	}

	code, stdout, _ = runCmd(t, "-root", root, "nsis", "-version", "0.2.0", "-webview2", webview2, "-print",
		"-prebuilt-uninstaller", prebuilt, "-stage", filepath.Join(root, "s"), "-out", filepath.Join(root, "o.exe"))
	if code != 0 || !strings.Contains(stdout, " -DWINVER=0.2.0.65000 -DSTAGE="+filepath.Join(root, "s")+" -DOUTFILE="+
		filepath.Join(root, "o.exe")+" -DWEBVIEW2_SETUP="+webview2+" -DPREBUILT_UNINSTALLER="+prebuilt+" ") ||
		!strings.HasPrefix(stdout, "makensis -WX ") {
		t.Errorf("prebuilt: exit %d, stdout %q", code, stdout)
	}

	code, stdout, _ = runCmd(t, "-root", root, "nsis", "-uninstaller-generator", "-version", "0.0.0-ci.9", "-print")
	want = "makensis -WX -V2 -NOCONFIG -INPUTCHARSET UTF8 -DVERSION=0.0.0-ci.9 -DWINVER=0.0.0.9 -DOUTFILE=" +
		filepath.Join(root, "dist-desktop", ".build", "uninst-gen.exe") + " " + filepath.Join(script, "uninst-gen.nsi") + "\n"
	if code != 0 || stdout != want {
		t.Errorf("generator: exit %d, stdout %q, want %q", code, stdout, want)
	}

	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"nsis"}, 2, "needs -version"},
		{[]string{"nsis", "-version", "0.2"}, 2, "not X.Y.Z"},
		{[]string{"nsis", "-version", "0.2.0"}, 2, "needs -webview2"},
		{[]string{"nsis", "-version", "0.2.0", "-webview2", filepath.Join(root, "none.exe")}, 1, "is not a file"},
		{[]string{"nsis", "-version", "0.2.0", "-uninstaller-generator", "-webview2", webview2}, 2, "takes only"},
		{[]string{"nsis", "-version", "0.2.0", "-webview2", webview2}, 1, "run desktoppkg stage first"},
	} {
		code, _, stderr := runCmd(t, append([]string{"-root", root}, tc.args...)...)
		if code != tc.code || !strings.Contains(stderr, tc.want) {
			t.Errorf("%q: exit %d, stderr %q; want exit %d and %q", tc.args, code, stderr, tc.code, tc.want)
		}
	}
}

func TestCheckStageForNSIS(t *testing.T) {
	stage := t.TempDir()
	writeFile(t, filepath.Join(stage, "install-files.txt"), "isshoni.exe\r\nlicenses\\LICENSE.txt\r\n")
	writeFile(t, filepath.Join(stage, "isshoni.exe"), "MZ")
	if err := checkStageForNSIS(stage); err == nil || !strings.Contains(err.Error(), `licenses\LICENSE.txt`) {
		t.Errorf("a missing file: %v", err)
	}
	writeFile(t, filepath.Join(stage, "licenses", "LICENSE.txt"), "Apache")
	if err := checkStageForNSIS(stage); err != nil {
		t.Errorf("a complete stage: %v", err)
	}
}

func TestMakensisEnv(t *testing.T) {
	base := []string{"PATH=/bin", "LC_ALL=C"}
	if got := makensisEnv("windows", base); !slices.Equal(got, base) {
		t.Errorf("windows: %q", got)
	}
	if got := makensisEnv("darwin", base); !slices.Equal(got, []string{"PATH=/bin", "LC_ALL=en_US.UTF-8"}) {
		t.Errorf("darwin under C: %q", got)
	}
	if got := makensisEnv("linux", []string{"PATH=/bin"}); !slices.Equal(got, []string{"PATH=/bin", "LC_ALL=C.UTF-8"}) {
		t.Errorf("linux without a locale: %q", got)
	}
	for _, env := range [][]string{{"LANG=en_GB.UTF-8"}, {"LC_CTYPE=C.utf8"}, {"LC_ALL=de_DE.UTF-8", "LANG=C"}} {
		if got := makensisEnv("linux", env); !slices.Equal(got, env) {
			t.Errorf("%q: %q, want it unchanged", env, got)
		}
	}
}

// TestMakensisBuilds compiles the committed scripts with -WX when a makensis is on PATH (Homebrew's on a Mac;
// CI's windows-desktop job builds them for real): the installer without and with a prebuilt uninstaller, and
// the uninstaller generator. It needs the repository's nsis folder, so it runs against the real root with a
// fixture stage.
func TestMakensisBuilds(t *testing.T) {
	makensis, err := exec.LookPath("makensis")
	if err != nil {
		t.Skip("no makensis on PATH (brew install makensis)")
	}
	root := repoRoot(t)
	dir := t.TempDir()
	f := newStageFixture(t)
	o := f.options()
	o.out = filepath.Join(dir, "stage")
	if err := stageTree(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	webview2 := filepath.Join(f.native, "MicrosoftEdgeWebview2Setup.exe")
	run := func(args ...string) {
		t.Helper()
		code, stdout, stderr := runCmd(t, append([]string{"-root", root, "nsis", "-makensis", makensis}, args...)...)
		if code != 0 {
			t.Fatalf("nsis %q: exit %d\n%s\n%s", args, code, stdout, stderr)
		}
	}
	gen := filepath.Join(dir, "uninst-gen.exe")
	run("-uninstaller-generator", "-version", "0.2.0", "-out", gen)
	plain := filepath.Join(dir, "plain.exe")
	run("-version", "0.2.0", "-stage", o.out, "-webview2", webview2, "-out", plain)
	// Any file stands in for the generator's output here: the installer only packs it.
	prebuilt := filepath.Join(dir, "prebuilt.exe")
	run("-version", "0.2.0", "-stage", o.out, "-webview2", webview2, "-out", prebuilt, "-prebuilt-uninstaller", gen)
	for _, exe := range []string{gen, plain, prebuilt} {
		data, err := os.ReadFile(exe)
		if err != nil || len(data) < 2 || string(data[:2]) != "MZ" {
			t.Errorf("%s is no PE file (%v)", exe, err)
		}
	}

	// The same inputs give the same installer (06 §6.2: the stage's mtimes are the commit time), also when the
	// bootstrapper and the prebuilt uninstaller are copies with other mtimes (CI downloads the bootstrapper anew).
	again := filepath.Join(dir, "again.exe")
	time.Sleep(1100 * time.Millisecond) // a second later, so a build time would show
	webview2b := filepath.Join(dir, "webview2-copy", "MicrosoftEdgeWebview2Setup.exe")
	genb := filepath.Join(dir, "gen-copy", "uninst-gen.exe")
	other := time.Now().Add(-37 * time.Hour)
	for _, c := range [][2]string{{webview2, webview2b}, {gen, genb}} {
		if err := os.MkdirAll(filepath.Dir(c[1]), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := copyStaged(c[0], c[1], other); err != nil {
			t.Fatal(err)
		}
	}
	run("-version", "0.2.0", "-stage", o.out, "-webview2", webview2b, "-out", again)
	prebuiltAgain := filepath.Join(dir, "prebuilt-again.exe")
	run("-version", "0.2.0", "-stage", o.out, "-webview2", webview2b, "-out", prebuiltAgain, "-prebuilt-uninstaller", genb)
	for _, pair := range [][2]string{{plain, again}, {prebuilt, prebuiltAgain}} {
		a, _ := os.ReadFile(pair[0])
		b, _ := os.ReadFile(pair[1])
		if !slices.Equal(a, b) {
			t.Errorf("two builds from the same stage differ: %s, %s", filepath.Base(pair[0]), filepath.Base(pair[1]))
		}
	}
	if left, _ := filepath.Glob(filepath.Join(root, "dist-desktop", ".build", "nsis-inputs-*")); len(left) > 0 {
		t.Errorf("the pinned copies are left: %v", left)
	}
}
