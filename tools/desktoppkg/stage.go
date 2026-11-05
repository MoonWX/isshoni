package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Staging (06 §6.2): `desktoppkg stage` copies exactly the install tree of 06 §2.2 into dist-desktop/stage/, which
// the installer packs as it is (installer.nsi's File /r):
//
//	isshoni.exe                               -exe (default dist-desktop/windows_amd64/isshoni.exe)
//	isshoni-media.dll                         -native: the windows-native artifact's release\ folder
//	avcodec-<N>.dll, avutil-<N>.dll           -native, the two that native/ffmpeg/lock.json names
//	install-files.txt                         written here: every other staged file, one per line
//	licenses\LICENSE.txt                      the repository's LICENSE
//	licenses\NOTICE.txt                       the repository's NOTICE
//	licenses\THIRD_PARTY_NOTICES.txt          -notices: tools/notices' desktop file (06 §9.2)
//	licenses\ffmpeg\COPYING.LGPLv2.1.txt      -native's licenses/ffmpeg/ (from the FFmpeg build, 06 §9.1)
//	licenses\ffmpeg\LICENSE.md
//	licenses\ffmpeg\README-LGPL.txt
//
// uninstall.exe is not staged: the installer writes it (WriteUninstaller, or the prebuilt one of 06 §6.9).
//
// Text files get .txt names; every file's mtime is the commit time (git log -1 --format=%ct, or -mtime), so the
// installer is reproducible for the same makensis. A binary in -native that is none of the three DLLs (or the
// WebView2 bootstrapper, which the installer embeds without installing it) fails, and so does a missing input.
//
// On Windows the staged isshoni.exe then runs --self-check (06 §2.4): the stage fails unless its report is ok, with
// engine.testHooks false and the version staged, so a win-x64-ci DLL can never reach an installer. On the Mac (local
// packages, never released) that step is skipped with a note.
//
// The LGPL compliance bundle (06 §9.1) is checked before anything is staged: COPYING.LGPLv2.1 is the LGPL 2.1 text,
// LICENSE.md is FFmpeg's (it names the LGPL), and README-LGPL.txt was rendered by native/ffmpeg/build.sh for the
// locked build (no @PLACEHOLDER@ left; it names lock.json's two DLLs, its tag and the release's source asset). The
// notices must be tools/notices' desktop file with its "Native components" section (06 §9.2, README WA78): the
// server's file, or one written without -native, is refused.
//
// -release (release.yml's desktop-pkg, and the windows-desktop smoke installer) also refuses an exe from
// dist-desktop/windows_amd64-test/, re-runs 06 §5.2 step 3's checks on the exe (checkExe) and requires the native
// folder's native-manifest.json to name the DLLs staged (the windows-native artifact's release\ folder, 06 §5.1).

const (
	stageDirName      = "stage"
	installFilesName  = "install-files.txt"
	appExeName        = "isshoni.exe"
	uninstallerName   = "uninstall.exe"
	webview2SetupName = "MicrosoftEdgeWebview2Setup.exe"
	nativeManifest    = "native-manifest.json"
	testBuildDir      = "windows_amd64-test"
	selfCheckTimeout  = 90 * time.Second
)

// stageInput is one file of the stage: where it comes from and its path under $INSTDIR (with / separators here,
// \ in install-files.txt).
type stageInput struct {
	src string
	rel string
}

// stageOptions is what stageTree needs; the tests fill it with fixtures.
type stageOptions struct {
	root      string // repository root
	exe       string
	native    string
	notices   string
	out       string
	version   string
	mtime     time.Time
	release   bool
	selfCheck func(ctx context.Context, exe string) ([]byte, error) // nil: skipped (not Windows)
	stdout    io.Writer
}

// runStage is `desktoppkg stage [-release] -version V -native DIR -notices FILE [-exe FILE] [-out DIR] [-mtime T]`.
func runStage(ctx context.Context, e *env, args []string) error {
	fs := flag.NewFlagSet("stage", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	release := fs.Bool("release", false, "a release stage: refuse a test build of isshoni.exe, check it as 06 §5.2 "+
		"step 3 does, and require -native's native-manifest.json to name the staged DLLs")
	version := fs.String("version", "", "the version isshoni.exe was built with (required)")
	exe := fs.String("exe", "", "isshoni.exe (default <root>/dist-desktop/windows_amd64/isshoni.exe)")
	native := fs.String("native", "", "the folder with isshoni-media.dll, the FFmpeg DLLs and licenses/ffmpeg/: "+
		"the windows-native artifact's release\\ folder (required)")
	notices := fs.String("notices", "", "the desktop THIRD_PARTY_NOTICES that tools/notices wrote (required)")
	out := fs.String("out", "", "the stage folder (default <root>/dist-desktop/stage); replaced when it is a stage")
	mtime := fs.String("mtime", "", "every file's mtime, Unix seconds (default: the commit time of HEAD)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errUsage
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("%w: stage takes no arguments", errUsage)
	}
	if *version == "" || *native == "" || *notices == "" {
		return fmt.Errorf("%w: stage needs -version, -native and -notices", errUsage)
	}
	if _, err := winver(*version); err != nil {
		return fmt.Errorf("%w: -version: %w", errUsage, err)
	}
	o := stageOptions{root: e.root, exe: *exe, native: *native, notices: *notices, out: *out, version: *version,
		release: *release, stdout: e.stdout}
	if o.exe == "" {
		o.exe = filepath.Join(e.root, "dist-desktop", "windows_amd64", appExeName)
	}
	if o.out == "" {
		o.out = filepath.Join(e.root, "dist-desktop", stageDirName)
	}
	t, err := commitTime(ctx, e.root, *mtime)
	if err != nil {
		return err
	}
	o.mtime = t
	if runtime.GOOS == "windows" {
		o.selfCheck = runSelfCheck
	}
	return stageTree(ctx, o)
}

// commitTime is -mtime when set, else HEAD's commit time.
func commitTime(ctx context.Context, root, flagValue string) (time.Time, error) {
	s := flagValue
	if s == "" {
		cmd := exec.CommandContext(ctx, "git", "-C", root, "log", "-1", "--format=%ct", "HEAD")
		b, err := cmd.Output()
		if err != nil {
			return time.Time{}, fmt.Errorf("the commit time (git log): %w; pass -mtime", err)
		}
		s = strings.TrimSpace(string(b))
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}, fmt.Errorf("%w: -mtime %q is not Unix seconds", errUsage, s)
	}
	return time.Unix(n, 0).UTC(), nil
}

// stageTree builds the stage; see the comment at the top of this file.
func stageTree(ctx context.Context, o stageOptions) error {
	st, err := readFFmpegState(o.root)
	if err != nil {
		return err
	}
	if st.lock == nil {
		return errNoLock
	}
	lock := st.lock

	if o.release {
		if filepath.Base(filepath.Dir(o.exe)) == testBuildDir {
			return fmt.Errorf("-release refuses %s: a test build (TAGS other than production, 06 §5.2)", o.exe)
		}
		if r := checkExe(o.exe, o.version); len(r.problems) > 0 {
			return fmt.Errorf("isshoni.exe fails 06 §5.2 step 3:\n  - %s", strings.Join(r.problems, "\n  - "))
		}
	}
	if err := checkNativeDir(o.native, lock, o.release); err != nil {
		return err
	}

	inputs := []stageInput{
		{o.exe, appExeName},
		{filepath.Join(o.native, engineDLLName), engineDLLName},
		{filepath.Join(o.native, lock.AvcodecDLL()), lock.AvcodecDLL()},
		{filepath.Join(o.native, lock.AvutilDLL()), lock.AvutilDLL()},
		{filepath.Join(o.root, "LICENSE"), "licenses/LICENSE.txt"},
		{filepath.Join(o.root, "NOTICE"), "licenses/NOTICE.txt"},
		{o.notices, "licenses/THIRD_PARTY_NOTICES.txt"},
		{filepath.Join(o.native, "licenses", "ffmpeg", "COPYING.LGPLv2.1"), "licenses/ffmpeg/COPYING.LGPLv2.1.txt"},
		{filepath.Join(o.native, "licenses", "ffmpeg", "LICENSE.md"), "licenses/ffmpeg/LICENSE.md"},
		{filepath.Join(o.native, "licenses", "ffmpeg", "README-LGPL.txt"), "licenses/ffmpeg/README-LGPL.txt"},
	}
	var missing []string
	for _, in := range inputs {
		if fi, err := os.Stat(in.src); err != nil || !fi.Mode().IsRegular() {
			missing = append(missing, in.src)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing input file(s):\n  - %s", strings.Join(missing, "\n  - "))
	}
	if problems := append(lgplProblems(o.native, lock), noticesProblems(o.notices)...); len(problems) > 0 {
		return fmt.Errorf("the licenses to stage (06 §9.1, §9.2):\n  - %s", strings.Join(problems, "\n  - "))
	}

	if err := resetStageDir(o.out); err != nil {
		return err
	}
	rels := make([]string, 0, len(inputs))
	for _, in := range inputs {
		if err := copyStaged(in.src, filepath.Join(o.out, filepath.FromSlash(in.rel)), o.mtime); err != nil {
			return err
		}
		rels = append(rels, in.rel)
	}
	slices.Sort(rels)
	if err := writeStaged(filepath.Join(o.out, installFilesName), []byte(installFilesText(rels)), o.mtime); err != nil {
		return err
	}
	// Directories too, so that nothing in the tree carries the build's time.
	for _, d := range []string{"licenses/ffmpeg", "licenses", "."} {
		if err := os.Chtimes(filepath.Join(o.out, filepath.FromSlash(d)), o.mtime, o.mtime); err != nil {
			return err
		}
	}
	if err := verifyStage(o.out, append(slices.Clone(rels), installFilesName)); err != nil {
		return err
	}

	if o.selfCheck == nil {
		fmt.Fprintln(o.stdout, "stage: note: --self-check of the staged isshoni.exe runs on Windows only; skipped")
	} else {
		report, err := o.selfCheck(ctx, filepath.Join(o.out, appExeName))
		if err != nil {
			return fmt.Errorf("--self-check of the staged isshoni.exe: %w", err)
		}
		if err := checkSelfCheck(report, o.version); err != nil {
			return err
		}
		fmt.Fprintln(o.stdout, "stage: --self-check of the staged isshoni.exe is ok")
	}
	fmt.Fprintf(o.stdout, "stage: %s (%d files, mtime %s)\n", o.out, len(rels)+1, o.mtime.Format(time.RFC3339))
	return nil
}

// nativeSectionTitle is the heading of the desktop notices' native section, as tools/notices writes it.
const nativeSectionTitle = "3. Native components"

// placeholderRE matches a placeholder of native/ffmpeg/README-LGPL.txt that build.sh did not replace.
var placeholderRE = regexp.MustCompile(`@[A-Z0-9_]+@`)

// lgplProblems checks the LGPL compliance files of 06 §9.1 in -native's licenses/ffmpeg/ (their presence is checked
// with the other inputs): each is what it should be, and README-LGPL.txt describes the locked build.
func lgplProblems(native string, lock *Lock) []string {
	dir := filepath.Join(native, "licenses", "ffmpeg")
	read := func(name string) string {
		data, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // G304: the files this command stages
		if err != nil {
			return ""
		}
		return string(data)
	}
	var problems []string
	copying := strings.ToUpper(read("COPYING.LGPLv2.1"))
	if !strings.Contains(copying, "GNU LESSER GENERAL PUBLIC LICENSE") || !strings.Contains(copying, "VERSION 2.1") {
		problems = append(problems, "licenses/ffmpeg/COPYING.LGPLv2.1 is not the GNU Lesser General Public License, version 2.1")
	}
	if !strings.Contains(read("LICENSE.md"), "LGPL") {
		problems = append(problems, "licenses/ffmpeg/LICENSE.md is not FFmpeg's LICENSE.md (it doesn't name the LGPL)")
	}
	readme := read("README-LGPL.txt")
	if m := placeholderRE.FindAllString(readme, -1); len(m) > 0 {
		slices.Sort(m)
		problems = append(problems, fmt.Sprintf("licenses/ffmpeg/README-LGPL.txt still has the placeholder(s) %s: "+
			"native/ffmpeg/build.sh renders it", strings.Join(slices.Compact(m), ", ")))
	}
	for _, want := range []string{lock.AvcodecDLL(), lock.AvutilDLL(), lock.Tag, "_ffmpeg-source.tar"} {
		if !strings.Contains(readme, want) {
			problems = append(problems, fmt.Sprintf("licenses/ffmpeg/README-LGPL.txt doesn't name %s: it is not the "+
				"README of the locked build (%s)", want, lock.Tag))
		}
	}
	return problems
}

// noticesProblems refuses notices without the native section: the server's THIRD_PARTY_NOTICES, or a desktop file
// written without -native.
func noticesProblems(notices string) []string {
	data, err := os.ReadFile(notices) //nolint:gosec // G304: the file this command stages
	if err != nil {
		return []string{err.Error()}
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.Contains(text, "\n"+nativeSectionTitle+"\n") {
		return []string{fmt.Sprintf("-notices %s has no %q section: write it with tools/notices -native "+
			"../deploy/notices/native-windows.json -native-root <the windows-native artifact's release folder> (06 §9.2)",
			notices, nativeSectionTitle)}
	}
	return nil
}

// installFilesText is install-files.txt: one path per line relative to $INSTDIR with \ separators, sorted, CRLF
// line ends (the installer reads it with FileRead and trims both).
func installFilesText(rels []string) string {
	var b strings.Builder
	for _, r := range rels {
		b.WriteString(strings.ReplaceAll(r, "/", `\`))
		b.WriteString("\r\n")
	}
	return b.String()
}

// checkNativeDir checks -native: the three DLLs are there, and no other binary is at its top level (the WebView2
// bootstrapper aside). With release it also requires native-manifest.json to name the same isshoni-media.dll and
// FFmpeg DLLs by SHA-256: the windows-native artifact's release\ folder (06 §5.1).
func checkNativeDir(dir string, lock *Lock, release bool) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("-native: %w", err)
	}
	want := map[string]bool{engineDLLName: true, lock.AvcodecDLL(): true, lock.AvutilDLL(): true}
	var problems []string
	for _, ent := range entries {
		name := ent.Name()
		lower := strings.ToLower(name)
		if !strings.HasSuffix(lower, ".dll") && !strings.HasSuffix(lower, ".exe") {
			continue
		}
		if want[name] || name == webview2SetupName {
			continue
		}
		problems = append(problems, fmt.Sprintf("-native has a binary the stage doesn't expect: %s (lock.json names %s and %s)",
			name, lock.AvcodecDLL(), lock.AvutilDLL()))
	}
	for _, name := range sortedKeys(want) {
		if fi, err := os.Stat(filepath.Join(dir, name)); err != nil || !fi.Mode().IsRegular() {
			problems = append(problems, "-native lacks "+name)
		}
	}
	if len(problems) == 0 {
		for _, name := range []string{lock.AvcodecDLL(), lock.AvutilDLL()} {
			sum, err := fileSHA256(filepath.Join(dir, name))
			if err != nil {
				return err
			}
			if sum != lock.DLLs[name] {
				problems = append(problems, fmt.Sprintf("%s's SHA-256 is %s, not lock.json's", name, sum))
			}
		}
	}
	if release && len(problems) == 0 {
		problems = append(problems, nativeManifestProblems(dir)...)
	}
	if len(problems) > 0 {
		return fmt.Errorf("-native %s:\n  - %s", dir, strings.Join(problems, "\n  - "))
	}
	return nil
}

// nativeManifestProblems compares native-manifest.json's dll and ffmpeg SHA-256s with the files next to it.
func nativeManifestProblems(dir string) []string {
	data, err := os.ReadFile(filepath.Join(dir, nativeManifest))
	if err != nil {
		return []string{"-release needs the windows-native artifact's release\\ folder, with " + nativeManifest + ": " + err.Error()}
	}
	var m struct {
		DLL struct {
			Name   string `json:"name"`
			SHA256 string `json:"sha256"`
		} `json:"dll"`
		FFmpeg struct {
			DLLs map[string]string `json:"dlls"`
		} `json:"ffmpeg"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return []string{nativeManifest + ": " + err.Error()}
	}
	files := map[string]string{m.DLL.Name: m.DLL.SHA256}
	for k, v := range m.FFmpeg.DLLs {
		files[k] = v
	}
	var problems []string
	if m.DLL.Name != engineDLLName {
		problems = append(problems, fmt.Sprintf("%s names dll %q, not %s", nativeManifest, m.DLL.Name, engineDLLName))
	}
	for _, name := range sortedKeys(boolSetOfKeys(files)) {
		sum, err := fileSHA256(filepath.Join(dir, name))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s names %s: %v", nativeManifest, name, err))
			continue
		}
		if sum != files[name] {
			problems = append(problems, fmt.Sprintf("%s's SHA-256 differs from %s's", name, nativeManifest))
		}
	}
	return problems
}

func boolSetOfKeys(m map[string]string) map[string]bool {
	s := make(map[string]bool, len(m))
	for k := range m {
		s[k] = true
	}
	return s
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G703: the files this command stages, named by its caller
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// resetStageDir empties out: it may be missing, empty, or a previous stage (it holds install-files.txt); anything
// else is refused, so a mistyped -out never deletes someone's folder.
func resetStageDir(out string) error {
	entries, err := os.ReadDir(out)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	case len(entries) == 0:
	default:
		if _, err := os.Stat(filepath.Join(out, installFilesName)); err != nil {
			return fmt.Errorf("-out %s is not empty and is no stage (no %s): refusing to replace it", out, installFilesName)
		}
		if err := os.RemoveAll(out); err != nil {
			return err
		}
	}
	return os.MkdirAll(out, 0o750)
}

func copyStaged(src, dst string, mtime time.Time) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return writeStaged(dst, data, mtime)
}

func writeStaged(dst string, data []byte, mtime time.Time) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	//nolint:gosec // G306: a build output, read by makensis
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return err
	}
	return os.Chtimes(dst, mtime, mtime)
}

// verifyStage fails when the stage holds anything but want (slash paths) or lacks one of them.
func verifyStage(out string, want []string) error {
	var got []string
	err := filepath.WalkDir(out, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(out, p)
		if err != nil {
			return err
		}
		got = append(got, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return err
	}
	if d := setDiff(sortedCopy(got), sortedCopy(want)); d != "" {
		return fmt.Errorf("the stage %s is not 06 §2.2's tree: %s", out, d)
	}
	return nil
}

// selfCheckJSON is the part of --self-check's report (06 §2.4) that the stage checks.
type selfCheckJSON struct {
	OK      *bool  `json:"ok"`
	Version string `json:"version"`
	Engine  struct {
		Loaded        bool   `json:"loaded"`
		ABI           int    `json:"abi"`
		FFmpegLoaded  bool   `json:"ffmpegLoaded"`
		FFmpegLicense string `json:"ffmpegLicense"`
		TestHooks     *bool  `json:"testHooks"`
	} `json:"engine"`
	SPA struct {
		OK bool `json:"ok"`
	} `json:"spa"`
}

// checkSelfCheck accepts a report only when it is ok, says testHooks false and names version.
func checkSelfCheck(report []byte, version string) error {
	var r selfCheckJSON
	if err := json.Unmarshal(bytes.TrimSpace(report), &r); err != nil {
		return fmt.Errorf("--self-check's report doesn't parse: %w", err)
	}
	var problems []string
	if r.Engine.TestHooks == nil || *r.Engine.TestHooks {
		problems = append(problems, "engine.testHooks is not false: a DLL with test hooks (win-x64-ci) is never staged")
	}
	// ok, and each field 06 §2.4's ok rule needs, so that a report that says ok wrongly fails too.
	e := r.Engine
	fieldsOK := e.Loaded && e.ABI == 1 && e.FFmpegLoaded && e.FFmpegLicense == "LGPL version 2.1 or later" && r.SPA.OK
	if r.OK == nil || !*r.OK || !fieldsOK {
		problems = append(problems, fmt.Sprintf("ok is not true (loaded %t, abi %d, ffmpegLoaded %t, ffmpegLicense %q, spa.ok %t)",
			r.Engine.Loaded, r.Engine.ABI, r.Engine.FFmpegLoaded, r.Engine.FFmpegLicense, r.SPA.OK))
	}
	if r.Version != version {
		problems = append(problems, fmt.Sprintf("version is %q, not %q", r.Version, version))
	}
	if len(problems) > 0 {
		return fmt.Errorf("--self-check of the staged isshoni.exe:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

// runSelfCheck runs `<exe> --self-check=<file>` and returns the report it wrote; the exit code is not decisive
// (checkSelfCheck reads the report), but a report that wasn't written is an error.
func runSelfCheck(ctx context.Context, exe string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "desktoppkg-selfcheck-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "self-check.json")
	ctx, cancel := context.WithTimeout(ctx, selfCheckTimeout)
	defer cancel()
	//nolint:gosec // G204: the exe this command just staged
	cmd := exec.CommandContext(ctx, exe, "--self-check="+path)
	runErr := cmd.Run()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("no report: %w", errors.Join(err, runErr))
	}
	return data, nil
}
