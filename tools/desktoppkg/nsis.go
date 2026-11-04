package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// `desktoppkg nsis` (06 §6.2, §6.9) runs makensis -WX on deploy/windows/nsis/installer.nsi with the defines of
// 06 §6.2:
//
//	VERSION               the SemVer (-version)
//	WINVER                its four-part number (winver, 06 §5.3)
//	STAGE                 the staged tree (-stage, default dist-desktop/stage)
//	OUTFILE               the installer (-out, default dist-desktop/isshoni-desktop_<v>_windows_amd64_setup.exe)
//	WEBVIEW2_SETUP        MicrosoftEdgeWebview2Setup.exe (-webview2)
//	PREBUILT_UNINSTALLER  uninstall.exe from uninst-gen (-prebuilt-uninstaller; CI only, 06 §6.9)
//
// With -uninstaller-generator it compiles uninst-gen.nsi instead (VERSION, WINVER, OUTFILE; -out default
// dist-desktop/.build/uninst-gen.exe): an exe that, run with /S on Windows, writes uninstall.exe next to itself
// (06 §6.9). -print prints the makensis command line instead of running it. -makensis names the binary (default
// makensis from PATH: Homebrew's on the Mac, the pinned portable zip's in CI).

const (
	installerScript = "installer.nsi"
	uninstGenScript = "uninst-gen.nsi"
)

// installerName is the release asset name of 06 §2.1.
func installerName(version string) string {
	return "isshoni-desktop_" + version + "_windows_amd64_setup.exe"
}

func runNSIS(ctx context.Context, e *env, args []string) error {
	fs := flag.NewFlagSet("nsis", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	version := fs.String("version", "", "the SemVer of the build (required)")
	stage := fs.String("stage", "", "the staged tree (default <root>/dist-desktop/stage)")
	out := fs.String("out", "", "the output file (default <root>/dist-desktop/"+installerName("<version>")+
		", or <root>/dist-desktop/.build/uninst-gen.exe with -uninstaller-generator)")
	webview2 := fs.String("webview2", "", "MicrosoftEdgeWebview2Setup.exe to embed (required for the installer)")
	prebuilt := fs.String("prebuilt-uninstaller", "", "uninstall.exe written by the uninstaller generator (06 §6.9)")
	gen := fs.Bool("uninstaller-generator", false, "compile uninst-gen.nsi, the uninstaller generator, instead")
	makensis := fs.String("makensis", "makensis", "the makensis binary")
	printOnly := fs.Bool("print", false, "print the makensis command line instead of running it")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errUsage
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("%w: nsis takes no arguments", errUsage)
	}
	if *version == "" {
		return fmt.Errorf("%w: nsis needs -version", errUsage)
	}
	w, err := winver(*version)
	if err != nil {
		return fmt.Errorf("%w: -version: %w", errUsage, err)
	}
	dir := filepath.Join(e.root, filepath.FromSlash(nsisDir))

	var argv []string
	if *gen {
		if *stage != "" || *webview2 != "" || *prebuilt != "" {
			return fmt.Errorf("%w: -uninstaller-generator takes only -version, -out and -makensis", errUsage)
		}
		o := *out
		if o == "" {
			o = filepath.Join(e.root, "dist-desktop", ".build", "uninst-gen.exe")
		}
		if o, err = filepath.Abs(o); err != nil {
			return err
		}
		argv = makensisArgs(map[string]string{"VERSION": *version, "WINVER": w, "OUTFILE": o},
			filepath.Join(dir, uninstGenScript))
		return runMakensis(ctx, e, *makensis, argv, o, *printOnly)
	}

	if *webview2 == "" {
		return fmt.Errorf("%w: nsis needs -webview2 (the installer embeds the WebView2 bootstrapper, 06 D9)", errUsage)
	}
	st := *stage
	if st == "" {
		st = filepath.Join(e.root, "dist-desktop", stageDirName)
	}
	o := *out
	if o == "" {
		o = filepath.Join(e.root, "dist-desktop", installerName(*version))
	}
	defs := map[string]string{"VERSION": *version, "WINVER": w}
	for _, p := range []struct {
		name, value string
		file        bool
	}{{"STAGE", st, false}, {"OUTFILE", o, false}, {"WEBVIEW2_SETUP", *webview2, true}, {"PREBUILT_UNINSTALLER", *prebuilt, true}} {
		if p.value == "" {
			continue
		}
		abs, err := filepath.Abs(p.value)
		if err != nil {
			return err
		}
		if p.file {
			if fi, err := os.Stat(abs); err != nil || !fi.Mode().IsRegular() {
				return fmt.Errorf("%s: %s is not a file", strings.ToLower(p.name), p.value)
			}
		}
		defs[p.name] = abs
	}
	if !*printOnly {
		if err := checkStageForNSIS(defs["STAGE"]); err != nil {
			return err
		}
	}
	argv = makensisArgs(defs, filepath.Join(dir, installerScript))
	return runMakensis(ctx, e, *makensis, argv, defs["OUTFILE"], *printOnly)
}

// makensisArgs is makensis' command line: -WX (warnings are errors, 06 §6.1), -INPUTCHARSET UTF8, -NOCONFIG (no
// nsisconf.nsh of the machine changes the build), the defines in a fixed order, then the script.
func makensisArgs(defs map[string]string, script string) []string {
	argv := []string{"-WX", "-V2", "-NOCONFIG", "-INPUTCHARSET", "UTF8"}
	for _, name := range []string{"VERSION", "WINVER", "STAGE", "OUTFILE", "WEBVIEW2_SETUP", "PREBUILT_UNINSTALLER"} {
		if v, ok := defs[name]; ok {
			argv = append(argv, "-D"+name+"="+v)
		}
	}
	return append(argv, script)
}

// checkStageForNSIS fails early, with a clearer message than makensis', when the stage isn't one.
func checkStageForNSIS(stage string) error {
	data, err := os.ReadFile(filepath.Join(stage, installFilesName))
	if err != nil {
		return fmt.Errorf("the stage %s has no %s: run desktoppkg stage first (%w)", stage, installFilesName, err)
	}
	var missing []string
	for line := range strings.SplitSeq(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		if line == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(stage, filepath.FromSlash(strings.ReplaceAll(line, `\`, "/")))); err != nil {
			missing = append(missing, line)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the stage %s lacks what its %s lists: %s", stage, installFilesName, strings.Join(missing, ", "))
	}
	return nil
}

// makensisEnv is makensis' environment. On macOS and Linux makensis converts the scripts with iconv under the
// process locale, and lang/English.nsh is UTF-8: under the C locale (Task's and CI's default) it fails ("Bad text
// encoding", or a crash), so a locale that isn't UTF-8 is replaced by one that is. Windows needs nothing.
func makensisEnv(goos string, environ []string) []string {
	if goos == "windows" {
		return environ
	}
	vars := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			vars[k] = v
		}
	}
	// The effective LC_CTYPE: LC_ALL, else LC_CTYPE, else LANG.
	cur := vars["LC_ALL"]
	if cur == "" {
		cur = vars["LC_CTYPE"]
	}
	if cur == "" {
		cur = vars["LANG"]
	}
	if up := strings.ToUpper(cur); strings.Contains(up, "UTF-8") || strings.Contains(up, "UTF8") {
		return environ
	}
	locale := "C.UTF-8"
	if goos == "darwin" {
		locale = "en_US.UTF-8"
	}
	out := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		if !strings.HasPrefix(kv, "LC_ALL=") {
			out = append(out, kv)
		}
	}
	return append(out, "LC_ALL="+locale)
}

func runMakensis(ctx context.Context, e *env, bin string, argv []string, out string, printOnly bool) error {
	if printOnly {
		fmt.Fprintln(e.stdout, strings.Join(append([]string{bin}, argv...), " "))
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o750); err != nil {
		return err
	}
	//nolint:gosec // G204: the makensis the caller names, with this command's own arguments
	cmd := exec.CommandContext(ctx, bin, argv...)
	cmd.Stdout = e.stdout
	cmd.Stderr = e.stderr
	cmd.Env = makensisEnv(runtime.GOOS, os.Environ())
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("makensis: %w", err)
	}
	fi, err := os.Stat(out)
	if err != nil {
		return fmt.Errorf("makensis wrote no %s: %w", out, err)
	}
	fmt.Fprintf(e.stdout, "nsis: %s (%.1f MiB)\n", out, float64(fi.Size())/(1<<20))
	return nil
}
