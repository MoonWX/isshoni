package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// stageFixture is a repository root with the FFmpeg lock of the fake DLLs, LICENSE and NOTICE, a native folder as
// the windows-native artifact's release\ holds it, a notices file and an exe.
type stageFixture struct {
	root, native, notices, exe, out string
}

const (
	fakeAvcodec = "avcodec dll"
	fakeAvutil  = "avutil dll"
	fakeEngine  = "isshoni-media dll"
)

func newStageFixture(t *testing.T) *stageFixture {
	t.Helper()
	f := &stageFixture{root: t.TempDir()}
	writeFFmpegInputs(t, f.root)
	inputs, err := inputsSHA256(f.root)
	if err != nil {
		t.Fatal(err)
	}
	lock := sampleLock(inputs)
	lock = strings.Replace(lock, strings.Repeat("3", 64), hexSHA256([]byte(fakeAvcodec)), 1)
	lock = strings.Replace(lock, strings.Repeat("4", 64), hexSHA256([]byte(fakeAvutil)), 1)
	writeFile(t, filepath.Join(f.root, lockPath), lock)
	writeFile(t, filepath.Join(f.root, "LICENSE"), "Apache License")
	writeFile(t, filepath.Join(f.root, "NOTICE"), "isshoni NOTICE")

	f.native = filepath.Join(f.root, "artifact", "release")
	writeFile(t, filepath.Join(f.native, "isshoni-media.dll"), fakeEngine)
	writeFile(t, filepath.Join(f.native, "isshoni-media.pdb"), "pdb")
	writeFile(t, filepath.Join(f.native, "avcodec-63.dll"), fakeAvcodec)
	writeFile(t, filepath.Join(f.native, "avutil-61.dll"), fakeAvutil)
	writeFile(t, filepath.Join(f.native, "MicrosoftEdgeWebview2Setup.exe"), "bootstrapper")
	writeFile(t, filepath.Join(f.native, "licenses", "ffmpeg", "COPYING.LGPLv2.1"), "LGPL")
	writeFile(t, filepath.Join(f.native, "licenses", "ffmpeg", "LICENSE.md"), "FFmpeg license")
	writeFile(t, filepath.Join(f.native, "licenses", "ffmpeg", "README-LGPL.txt"), "README-LGPL")
	writeFile(t, filepath.Join(f.native, "licenses", "vcpkg", "opus", "copyright"), "opus")
	writeFile(t, filepath.Join(f.native, nativeManifest), `{"version": "0.2.0", "dll": {"name": "isshoni-media.dll", "sha256": "`+
		hexSHA256([]byte(fakeEngine))+`", "abi": 65536}, "ffmpeg": {"tag": "deps-ffmpeg-9.0.2-r1", "dlls": {"avcodec-63.dll": "`+
		hexSHA256([]byte(fakeAvcodec))+`", "avutil-61.dll": "`+hexSHA256([]byte(fakeAvutil))+`"}}}`)
	f.notices = filepath.Join(f.root, "dist-desktop", ".build", "THIRD_PARTY_NOTICES-desktop")
	writeFile(t, f.notices, "notices")
	f.exe = filepath.Join(f.root, "dist-desktop", "windows_amd64", "isshoni.exe")
	writeFile(t, f.exe, "MZ exe 0.2.0")
	f.out = filepath.Join(f.root, "dist-desktop", "stage")
	return f
}

func (f *stageFixture) options() stageOptions {
	return stageOptions{root: f.root, exe: f.exe, native: f.native, notices: f.notices, out: f.out, version: "0.2.0",
		mtime: time.Unix(1_790_000_000, 0).UTC(), stdout: &bytes.Buffer{}}
}

// wantStage is 06 §2.2's tree without uninstall.exe, which the installer writes.
var wantStage = map[string]string{
	"isshoni.exe":                          "MZ exe 0.2.0",
	"isshoni-media.dll":                    fakeEngine,
	"avcodec-63.dll":                       fakeAvcodec,
	"avutil-61.dll":                        fakeAvutil,
	"licenses/LICENSE.txt":                 "Apache License",
	"licenses/NOTICE.txt":                  "isshoni NOTICE",
	"licenses/THIRD_PARTY_NOTICES.txt":     "notices",
	"licenses/ffmpeg/COPYING.LGPLv2.1.txt": "LGPL",
	"licenses/ffmpeg/LICENSE.md":           "FFmpeg license",
	"licenses/ffmpeg/README-LGPL.txt":      "README-LGPL",
	"install-files.txt": "avcodec-63.dll\r\navutil-61.dll\r\nisshoni-media.dll\r\nisshoni.exe\r\n" +
		"licenses\\LICENSE.txt\r\nlicenses\\NOTICE.txt\r\nlicenses\\THIRD_PARTY_NOTICES.txt\r\n" +
		"licenses\\ffmpeg\\COPYING.LGPLv2.1.txt\r\nlicenses\\ffmpeg\\LICENSE.md\r\nlicenses\\ffmpeg\\README-LGPL.txt\r\n",
}

func checkStageTree(t *testing.T, out string, mtime time.Time) {
	t.Helper()
	var got []string
	err := filepath.WalkDir(out, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if !fi.ModTime().Equal(mtime) {
			t.Errorf("%s has the mtime %s, not %s", p, fi.ModTime().UTC(), mtime)
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(out, p)
		rel = filepath.ToSlash(rel)
		got = append(got, rel)
		data, err := os.ReadFile(p) //nolint:gosec // G122: a t.TempDir() tree the test wrote
		if err != nil {
			return err
		}
		if want, ok := wantStage[rel]; !ok || string(data) != want {
			t.Errorf("%s holds %q, want %q (expected: %t)", rel, data, want, ok)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(wantStage) {
		t.Errorf("the stage holds %q, want %d files", got, len(wantStage))
	}
}

func TestStageTree(t *testing.T) {
	f := newStageFixture(t)
	o := f.options()
	if err := stageTree(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	checkStageTree(t, f.out, o.mtime)
	if out := o.stdout.(*bytes.Buffer).String(); !strings.Contains(out, "runs on Windows only; skipped") {
		t.Errorf("no note about the skipped self-check: %q", out)
	}

	// A second run replaces the stage, and a stale file in it goes.
	writeFile(t, filepath.Join(f.out, "avcodec-62.dll"), "old")
	if err := stageTree(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	checkStageTree(t, f.out, o.mtime)

	// -release with the windows-native artifact's folder (native-manifest.json) and an exe that 06 §5.2 step 3
	// accepts is not available here (the fixture's exe has no build info): it fails on the exe.
	o.release = true
	if err := stageTree(context.Background(), o); err == nil || !strings.Contains(err.Error(), "no Go build info") {
		t.Errorf("-release with a fake exe: %v", err)
	}
}

func TestStageRefuses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, f *stageFixture, o *stageOptions)
		want   string
	}{
		{"missing input", func(t *testing.T, f *stageFixture, _ *stageOptions) {
			mustRemove(t, filepath.Join(f.native, "licenses", "ffmpeg", "LICENSE.md"))
		}, "missing input file(s)"},
		{"missing notices", func(t *testing.T, f *stageFixture, _ *stageOptions) { mustRemove(t, f.notices) }, "THIRD_PARTY_NOTICES-desktop"},
		{"missing DLL", func(t *testing.T, f *stageFixture, _ *stageOptions) {
			mustRemove(t, filepath.Join(f.native, "avutil-61.dll"))
		}, "-native lacks avutil-61.dll"},
		{"unexpected DLL", func(t *testing.T, f *stageFixture, _ *stageOptions) {
			writeFile(t, filepath.Join(f.native, "avcodec-62.dll"), "old")
		}, "doesn't expect: avcodec-62.dll"},
		{"unexpected exe", func(t *testing.T, f *stageFixture, _ *stageOptions) {
			writeFile(t, filepath.Join(f.native, "isshoni-enginetest.exe"), "MZ")
		}, "doesn't expect: isshoni-enginetest.exe"},
		{"modified FFmpeg DLL", func(t *testing.T, f *stageFixture, _ *stageOptions) {
			writeFile(t, filepath.Join(f.native, "avcodec-63.dll"), "patched")
		}, "avcodec-63.dll's SHA-256"},
		{"foreign out folder", func(t *testing.T, f *stageFixture, _ *stageOptions) {
			writeFile(t, filepath.Join(f.out, "notes.txt"), "mine")
		}, "is no stage"},
		{"test build", func(t *testing.T, f *stageFixture, o *stageOptions) {
			o.release = true
			o.exe = filepath.Join(f.root, "dist-desktop", testBuildDir, "isshoni.exe")
			writeFile(t, o.exe, "MZ")
		}, "a test build"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStageFixture(t)
			o := f.options()
			tc.mutate(t, f, &o)
			err := stageTree(context.Background(), o)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("stageTree: %v; want an error with %q", err, tc.want)
			}
		})
	}
}

func mustRemove(t *testing.T, p string) {
	t.Helper()
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
}

// TestNativeManifest: -release takes the DLLs only from the windows-native artifact's release\ folder, whose
// native-manifest.json names them.
func TestNativeManifest(t *testing.T) {
	f := newStageFixture(t)
	if p := nativeManifestProblems(f.native); len(p) > 0 {
		t.Fatalf("the fixture's manifest: %q", p)
	}
	writeFile(t, filepath.Join(f.native, "isshoni-media.dll"), "the win-x64-ci build")
	if p := nativeManifestProblems(f.native); len(p) != 1 || !strings.Contains(p[0], "isshoni-media.dll's SHA-256") {
		t.Errorf("another DLL: %q", p)
	}
	mustRemove(t, filepath.Join(f.native, nativeManifest))
	if p := nativeManifestProblems(f.native); len(p) != 1 || !strings.Contains(p[0], "release\\ folder") {
		t.Errorf("no manifest: %q", p)
	}
}

// TestStageSelfCheck: on Windows the stage runs the staged exe's --self-check; a report with test hooks (a
// win-x64-ci DLL), one that isn't ok, one of another version or none at all fails the stage.
func TestStageSelfCheck(t *testing.T) {
	const ok = `{"ok":true,"version":"0.2.0","engine":{"loaded":true,"abi":1,"ffmpegLoaded":true,` +
		`"ffmpegLicense":"LGPL version 2.1 or later","testHooks":false},"spa":{"ok":true}}`
	for _, tc := range []struct {
		name, report string
		err          error
		want         string
	}{
		{"ok", ok, nil, ""},
		{"test hooks", strings.Replace(strings.Replace(ok, `"testHooks":false`, `"testHooks":true`, 1), `"ok":true,"v`, `"ok":false,"v`, 1),
			nil, "engine.testHooks is not false"},
		{"no testHooks field", strings.Replace(ok, `,"testHooks":false`, "", 1), nil, "engine.testHooks is not false"},
		{"not ok", strings.Replace(ok, `"ffmpegLoaded":true`, `"ffmpegLoaded":false`, 1), nil, "ok is not true"},
		{"another version", strings.Replace(ok, `"0.2.0"`, `"0.1.0"`, 1), nil, `version is "0.1.0"`},
		{"garbage", "not json", nil, "doesn't parse"},
		{"no report", "", errors.New("no report"), "no report"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStageFixture(t)
			o := f.options()
			var ran string
			o.selfCheck = func(_ context.Context, exe string) ([]byte, error) {
				ran = exe
				return []byte(tc.report), tc.err
			}
			err := stageTree(context.Background(), o)
			if ran != filepath.Join(f.out, "isshoni.exe") {
				t.Errorf("the self-check ran %q, not the staged exe", ran)
			}
			if tc.want == "" {
				if err != nil {
					t.Errorf("stageTree: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("stageTree: %v; want an error with %q", err, tc.want)
			}
		})
	}
}

func TestStageCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("on Windows the command runs the staged exe's --self-check, which the fixture's exe can't answer")
	}
	f := newStageFixture(t)
	code, stdout, stderr := runCmd(t, "-root", f.root, "stage", "-version", "0.2.0", "-native", f.native, "-notices",
		f.notices, "-mtime", "1790000000")
	if code != 0 || !strings.Contains(stdout, "stage: "+f.out) {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	checkStageTree(t, f.out, time.Unix(1_790_000_000, 0))
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"stage"}, "needs -version, -native and -notices"},
		{[]string{"stage", "-version", "v1", "-native", "x", "-notices", "y"}, "not X.Y.Z"},
		{[]string{"stage", "-version", "0.2.0", "-native", "x", "-notices", "y", "-mtime", "yesterday"}, "not Unix seconds"},
		{[]string{"stage", "-version", "0.2.0", "-native", "x", "-notices", "y", "extra"}, "takes no arguments"},
	} {
		if code, _, stderr := runCmd(t, append([]string{"-root", f.root}, tc.args...)...); code != 2 || !strings.Contains(stderr, tc.want) {
			t.Errorf("%q: exit %d, stderr %q; want exit 2 and %q", tc.args, code, stderr, tc.want)
		}
	}
}
