package main

import (
	"bytes"
	"context"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
)

// `desktoppkg check [-stage DIR] [-installer FILE] [-exe FILE -version V]` (06 §5.2 step 3, §6.10, §9.3, §16):
//
//   - always, the strings rule of 06 §6.10 over deploy/windows/nsis/: no MessageBox, DetailPrint,
//     nsDialogs::Create*/${NSD_Create*}, Banner::show, MUI_HEADER_TEXT, `!define MUI_*TEXT*` or (stricter than 06
//     §6.10) `!define MUI_*TITLE*` text that is not a $(…) reference; every $(…) the scripts use is a LangString of
//     lang/English.nsh, and every LangString there is used, once;
//   - -stage: every binary in the stage is isshoni's own (isshoni.exe, isshoni-media.dll, uninstall.exe) or matches
//     a files glob of deploy/notices/native-windows.json (06 §9.3);
//   - -installer: the size budget, a warning above 40 MiB and a failure above 100 MiB;
//   - -exe with -version: 06 §5.2 step 3 on a default-TAGS isshoni.exe (checkExe).
//
// Every problem is reported, not only the first.

const (
	nsisDir           = "deploy/windows/nsis"
	nsisLangFile      = "lang/English.nsh"
	nativeNoticesFile = "deploy/notices/native-windows.json"
	appModule         = "github.com/MoonWX/isshoni/cmd/isshoni-desktop"

	installerWarnSize = 40 << 20
	installerMaxSize  = 100 << 20
	exeWarnSize       = 45 << 20
)

// nsisChecked are the scripts the strings rule reads (06 §6.10 names the first three; uninst-gen.nsi has strings of
// its own too).
var nsisChecked = []string{"installer.nsi", "common.nsh", "uninstall.nsh", "uninst-gen.nsi"}

func runCheck(_ context.Context, e *env, args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	stage := fs.String("stage", "", "check the binaries of this stage (dist-desktop/stage) against 06 §9.3")
	installer := fs.String("installer", "", "check this installer's size budget")
	exe := fs.String("exe", "", "check this isshoni.exe as 06 §5.2 step 3 does (needs -version)")
	version := fs.String("version", "", "the version -exe was built with")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errUsage
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("%w: check takes no arguments", errUsage)
	}
	if (*exe == "") != (*version == "") {
		return fmt.Errorf("%w: -exe and -version go together", errUsage)
	}

	var r report
	r.add(checkNSISStrings(filepath.Join(e.root, filepath.FromSlash(nsisDir))))
	if *stage != "" {
		r.add(checkStageBinaries(e.root, *stage))
	}
	if *installer != "" {
		r.add(checkInstallerSize(*installer))
	}
	if *exe != "" {
		r.add(checkExe(*exe, *version))
	}
	for _, line := range r.notes {
		fmt.Fprintf(e.stdout, "check: %s\n", line)
	}
	if len(r.problems) > 0 {
		return fmt.Errorf("%d problem(s):\n  - %s", len(r.problems), strings.Join(r.problems, "\n  - "))
	}
	fmt.Fprintln(e.stdout, "check: passed")
	return nil
}

// ---- 06 §5.2 step 3 ----

// checkExe is 06 §5.2 step 3 for a default-TAGS build: the build info says -trimpath, CGO_ENABLED=0, GOOS=windows,
// GOARCH=amd64 and the tags exactly production (dev, e2e and updatetest fail), the main package is the app's, the
// binary carries version (the -X version ldflag: -trimpath keeps -ldflags out of the build info, so the stamped
// string itself is the evidence) and neither ISSHONI_UPDATE_TEST nor localhost:5173; above 45 MiB is a warning.
func checkExe(file, version string) report {
	var r report
	data, err := os.ReadFile(file)
	if err != nil {
		r.fail("isshoni.exe: %v", err)
		return r
	}
	bi, err := buildinfo.Read(bytes.NewReader(data))
	if err != nil {
		r.fail("%s has no Go build info: %v", filepath.Base(file), err)
		return r
	}
	return checkExeInfo(filepath.Base(file), data, bi, version)
}

// checkExeInfo is checkExe on an exe's bytes and build info.
func checkExeInfo(name string, data []byte, bi *debug.BuildInfo, version string) report {
	var r report
	if bi.Path != appModule {
		r.fail("%s is %s, not %s", name, bi.Path, appModule)
	}
	settings := map[string]string{}
	for _, s := range bi.Settings {
		settings[s.Key] = s.Value
	}
	for _, want := range [][2]string{{"-trimpath", "true"}, {"CGO_ENABLED", "0"}, {"GOOS", "windows"}, {"GOARCH", "amd64"}} {
		if got := settings[want[0]]; got != want[1] {
			r.fail("build setting %s is %q, not %q", want[0], got, want[1])
		}
	}
	if tags := settings["-tags"]; tags != "production" {
		r.fail("build tags are %q, not exactly \"production\" (dev, e2e and updatetest builds are never shipped)", tags)
	}
	if !bytes.Contains(data, []byte(version)) {
		r.fail("%s doesn't carry its version %s (the -X …/internal/version.version ldflag)", name, version)
	}
	for _, s := range []string{"ISSHONI_UPDATE_TEST", "localhost:5173"} {
		if bytes.Contains(data, []byte(s)) {
			r.fail("%s contains %q (a test or dev build)", name, s)
		}
	}
	if len(data) > exeWarnSize {
		r.note("warning: %s is %.1f MiB, above 45 MiB", name, float64(len(data))/(1<<20))
	}
	return r
}

// ---- 06 §9.3: the binaries of the stage ----

// checkStageBinaries fails on a binary (.exe, .dll, or any file starting with MZ) that is neither isshoni's own nor
// matched by a files glob of native-windows.json.
func checkStageBinaries(root, stage string) report {
	var r report
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(nativeNoticesFile)))
	if err != nil {
		r.fail("%v", err)
		return r
	}
	var entries []struct {
		Name  string   `json:"name"`
		Files []string `json:"files"`
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		r.fail("%s: %v", nativeNoticesFile, err)
		return r
	}
	own := map[string]bool{appExeName: true, engineDLLName: true, uninstallerName: true}
	n := 0
	err = filepath.WalkDir(stage, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(stage, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		bin, err := isBinary(p)
		if err != nil {
			return err
		}
		if !bin {
			return nil
		}
		n++
		if own[rel] {
			return nil
		}
		for _, ent := range entries {
			for _, g := range ent.Files {
				if ok, _ := path.Match(g, rel); ok {
					return nil
				}
			}
		}
		r.fail("the stage has a binary that is neither isshoni's nor matched by a files glob of %s: %s", nativeNoticesFile, rel)
		return nil
	})
	if err != nil {
		r.fail("the stage: %v", err)
	}
	if n == 0 {
		r.fail("the stage %s has no binary at all", stage)
	}
	return r
}

func isBinary(p string) (bool, error) {
	lower := strings.ToLower(p)
	if strings.HasSuffix(lower, ".exe") || strings.HasSuffix(lower, ".dll") {
		return true, nil
	}
	f, err := os.Open(p)
	if err != nil {
		return false, err
	}
	defer f.Close()
	var b [2]byte
	n, _ := f.Read(b[:])
	return n == 2 && b[0] == 'M' && b[1] == 'Z', nil
}

// ---- the size budget ----

func checkInstallerSize(file string) report {
	var r report
	fi, err := os.Stat(file)
	if err != nil {
		r.fail("the installer: %v", err)
		return r
	}
	mib := float64(fi.Size()) / (1 << 20)
	switch {
	case fi.Size() > installerMaxSize:
		r.fail("%s is %.1f MiB, above the 100 MiB budget", filepath.Base(file), mib)
	case fi.Size() > installerWarnSize:
		r.note("warning: %s is %.1f MiB, above 40 MiB", filepath.Base(file), mib)
	default:
		r.note("%s is %.1f MiB", filepath.Base(file), mib)
	}
	return r
}

// ---- 06 §6.10: the strings rule ----

var (
	reLangRef    = regexp.MustCompile(`\$\(([^)^][^)]*)\)`)
	reLangString = regexp.MustCompile(`^LangString$`)
	// reNonText removes what is not text from an argument: $(…) and ${…} references, $\x escapes, $$ and variables.
	reNonText = regexp.MustCompile(`\$\([^)]*\)|\$\{[^}]*\}|\$\\.|\$\$|\$[A-Za-z_][A-Za-z0-9_.]*|\$[0-9]`)
	reLetter  = regexp.MustCompile(`[A-Za-z]`)
	reMUIText = regexp.MustCompile(`^MUI_[A-Z0-9_]*(TEXT|TITLE)[A-Z0-9_]*$`)
)

// checkNSISStrings applies 06 §6.10 to the scripts in dir (see the comment at the top).
func checkNSISStrings(dir string) report {
	var r report
	used := map[string][]string{} // LangString name → where
	for _, name := range nsisChecked {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			r.fail("%s: %v", nsisDir+"/"+name, err)
			continue
		}
		for _, ln := range nsisLines(string(data)) {
			where := fmt.Sprintf("%s/%s:%d", nsisDir, name, ln.num)
			for _, m := range reLangRef.FindAllStringSubmatch(ln.text, -1) {
				used[m[1]] = append(used[m[1]], where)
			}
			for _, text := range visibleText(ln.words) {
				if literalText(text) {
					r.fail("%s: %s has the literal text %q: put it into %s as a LangString and use $(…)",
						where, ln.words[0], text, nsisLangFile)
				}
			}
		}
	}

	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(nsisLangFile)))
	if err != nil {
		r.fail("%s: %v", nsisDir+"/"+nsisLangFile, err)
		return r
	}
	defined := map[string]int{}
	for _, ln := range nsisLines(string(data)) {
		if len(ln.words) == 0 {
			continue
		}
		if !reLangString.MatchString(ln.words[0]) {
			r.fail("%s/%s:%d: only LangString lines belong here, not %s", nsisDir, nsisLangFile, ln.num, ln.words[0])
			continue
		}
		if len(ln.words) != 4 {
			r.fail("%s/%s:%d: LangString takes a name, a language and a text", nsisDir, nsisLangFile, ln.num)
			continue
		}
		if ln.words[2] != "${LANG_ENGLISH}" {
			r.fail("%s/%s:%d: %s is not for ${LANG_ENGLISH}", nsisDir, nsisLangFile, ln.num, ln.words[1])
		}
		defined[ln.words[1]]++
	}
	for _, name := range sortedKeys(boolSetOfInts(defined)) {
		if defined[name] > 1 {
			r.fail("%s: LangString %s is defined %d times", nsisLangFile, name, defined[name])
		}
		if len(used[name]) == 0 {
			r.fail("%s: LangString %s is never used", nsisLangFile, name)
		}
	}
	names := make([]string, 0, len(used))
	for name := range used {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if defined[name] == 0 {
			r.fail("%s: $(%s) is no LangString of %s", used[name][0], name, nsisLangFile)
		}
	}
	return r
}

func boolSetOfInts(m map[string]int) map[string]bool {
	s := make(map[string]bool, len(m))
	for k := range m {
		s[k] = true
	}
	return s
}

// literalText reports whether an argument still has letters once references, escapes and variables are removed.
func literalText(s string) bool {
	return reLetter.MatchString(reNonText.ReplaceAllString(s, ""))
}

// visibleText returns the arguments of a command line that a user sees, for the commands of 06 §6.10.
func visibleText(w []string) []string {
	if len(w) == 0 {
		return nil
	}
	cmd := w[0]
	arg := func(i int) []string {
		if i < len(w) {
			return []string{w[i]}
		}
		return nil
	}
	switch {
	case strings.EqualFold(cmd, "MessageBox"): // MessageBox options text [/SD return] [checks…]
		return arg(2)
	case strings.EqualFold(cmd, "DetailPrint"):
		return arg(1)
	case strings.EqualFold(cmd, "Banner::show"): // Banner::show [/NOUNLOAD] [/set id text]… text
		var out []string
		for i := 1; i < len(w); i++ {
			switch {
			case strings.EqualFold(w[i], "/NOUNLOAD"):
			case strings.EqualFold(w[i], "/set") && i+2 < len(w):
				out = append(out, w[i+2])
				i += 2
			default:
				out = append(out, w[i])
			}
		}
		return out
	case strings.EqualFold(cmd, "nsDialogs::CreateControl"): // class style exstyle x y w h text
		i := 1
		if i < len(w) && strings.EqualFold(w[i], "/NOUNLOAD") {
			i++
		}
		return arg(i + 7)
	case strings.HasPrefix(cmd, "${NSD_Create"): // x y w h text
		return arg(5)
	case strings.EqualFold(cmd, "!insertmacro") && len(w) > 1 && w[1] == "MUI_HEADER_TEXT":
		return w[2:]
	case strings.EqualFold(cmd, "!define"):
		i := 1
		for i < len(w) && strings.HasPrefix(w[i], "/") {
			i++
		}
		if i < len(w) && reMUIText.MatchString(w[i]) {
			return arg(i + 1)
		}
	}
	return nil
}

// nsisLine is one logical line of an NSIS script: its first physical line's number and its words.
type nsisLine struct {
	num   int
	text  string
	words []string
}

// nsisLines splits a script into logical lines (a \ at the end joins the next line), drops comments (; and # outside
// quotes, /* … */) and splits each line into words, unquoting "…", '…' and `…`.
func nsisLines(src string) []nsisLine {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	var out []nsisLine
	phys := strings.Split(src, "\n")
	inBlock := false
	for i := 0; i < len(phys); i++ {
		num := i + 1
		text := phys[i]
		for strings.HasSuffix(text, `\`) && i+1 < len(phys) {
			i++
			text = text[:len(text)-1] + phys[i]
		}
		var words []string
		var cur strings.Builder
		inWord, quote := false, byte(0)
		flush := func() {
			if inWord {
				words = append(words, cur.String())
			}
			cur.Reset()
			inWord = false
		}
		var kept strings.Builder
	scan:
		for j := 0; j < len(text); j++ {
			c := text[j]
			if inBlock {
				if c == '*' && j+1 < len(text) && text[j+1] == '/' {
					inBlock = false
					j++
				}
				continue
			}
			switch {
			case quote != 0:
				if c == quote {
					quote = 0
				} else {
					if c == '$' && j+1 < len(text) && text[j+1] == '\\' && j+2 < len(text) {
						cur.WriteString(text[j : j+3])
						kept.WriteString(text[j : j+3])
						j += 2
						continue
					}
					cur.WriteByte(c)
				}
			case c == '"' || c == '\'' || c == '`':
				quote = c
				inWord = true
			case c == ' ' || c == '\t':
				flush()
			case (c == ';' || c == '#') && !inWord:
				break scan
			case c == '/' && !inWord && j+1 < len(text) && text[j+1] == '*':
				inBlock = true
				j++
				continue
			default:
				cur.WriteByte(c)
				inWord = true
			}
			kept.WriteByte(c)
		}
		flush()
		if len(words) > 0 {
			out = append(out, nsisLine{num: num, text: kept.String(), words: words})
		}
	}
	return out
}
