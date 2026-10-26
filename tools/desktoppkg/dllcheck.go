package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// runDLLCheck is `desktoppkg dll-check` (06 §5.1, §16; slice WA11): the checks of the win-x64-release
// isshoni-media.dll and, with -ffmpeg, of the FFmpeg DLLs next to it.
//
//	dll-check -dll FILE -version SEMVER -winver A.B.C.D [-ffmpeg DIR] [-load-config]
//
// It reads the PE image itself (pe.go) instead of dumpbin's text, so it needs no Visual Studio environment and runs on
// the Mac and on Linux too, and it can read VERSIONINFO, which dumpbin can't print. The DLL:
//   - is an x64 DLL with ASLR (high entropy), DEP and Control Flow Guard (04 §13.1's linker flags);
//   - exports exactly the functions native/include/isshoni_media.h declares with IM_API, by name (no ordinal-only and
//     no forwarded export), under the export name isshoni-media.dll; native/windows/isshoni-media.def lists the same;
//   - imports only DLLs that deploy/windows/dll-imports.txt lists, each in the section matching how it is imported
//     (static: or delay:). mfplat.dll and avrt.dll only ever as delay imports; avcodec-*, avutil-*, the C runtime
//     (VCRUNTIME*, MSVCP*, MSVCR*, api-ms-win-crt-*, ucrtbase*, concrt*, vccorlib*, their debug builds) and
//     opus*.dll never: the engine must load, probe and capture audio when FFmpeg is missing or blocked (04 §9.2,
//     §13.3);
//   - has VERSIONINFO with FILEVERSION and PRODUCTVERSION = -winver and the strings ProductVersion = -version,
//     FileVersion = -winver, ProductName "isshoni", CompanyName "isshoni project", LegalCopyright "Copyright the
//     isshoni authors. Apache-2.0.", OriginalFilename "isshoni-media.dll", FileDescription "isshoni media engine" and
//     InternalName "isshoni-media" (04 §13.4);
//   - with -load-config: has DependentLoadFlags 0x800 (LOAD_LIBRARY_SEARCH_SYSTEM32) in its load config. No job
//     passes it: WA11's dll_search_test found that the delay-load helper doesn't honor /DEPENDENTLOADFLAG ("not
//     honored", README §3 item 34, 04 §13.1, 06 §5.1), so the DLL is linked without the flag and the engine's
//     delay-load hook is the guard. Without -load-config the value is printed, unchecked (loadConfigNote).
//
// With -ffmpeg DIR, the FFmpeg DLLs in DIR (avcodec-*.dll, avutil-*.dll) are exactly the two that
// native/ffmpeg/lock.json names after its majors, have its SHA-256s, and import neither Media Foundation (mfplat.dll,
// mf.dll, mfreadwrite.dll, evr.dll) nor a C runtime DLL (06 §4.4).
//
// Every problem is reported, not just the first; the exit status is 1 when there is one.
func runDLLCheck(_ context.Context, e *env, args []string) error {
	fs := flag.NewFlagSet("dll-check", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	dll := fs.String("dll", "", "isshoni-media.dll to check (the win-x64-release build)")
	version := fs.String("version", "", "the release SemVer the DLL was built with (IM_VERSION): its ProductVersion")
	winver := fs.String("winver", "", "its Windows version a.b.c.d (IM_WINVER): FILEVERSION, PRODUCTVERSION, FileVersion")
	ffmpegDir := fs.String("ffmpeg", "", "also check the FFmpeg DLLs in this folder against native/ffmpeg/lock.json")
	loadConfig := fs.Bool("load-config", false,
		"require DependentLoadFlags 0x800 in the load config (unused: the delay-load helper doesn't honor it, 06 §5.1)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("%w: dll-check takes no arguments", errUsage)
	}
	if *dll == "" || *version == "" || *winver == "" {
		return fmt.Errorf("%w: dll-check needs -dll, -version and -winver", errUsage)
	}
	if !reSemVer.MatchString(*version) {
		return fmt.Errorf("%w: -version %q is not a SemVer version", errUsage, *version)
	}
	want, err := parseWinver(*winver)
	if err != nil {
		return fmt.Errorf("%w: -winver: %w", errUsage, err)
	}

	rules, err := loadDLLRules(e.root)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(*dll)
	if err != nil {
		return err
	}
	r := checkEngineDLL(data, rules, dllExpect{version: *version, winver: want, loadConfig: *loadConfig})
	if *ffmpegDir != "" {
		st, err := readFFmpegState(e.root)
		if err != nil {
			return err
		}
		if st.lock == nil {
			return errNoLock
		}
		r.add(checkFFmpegDLLs(*ffmpegDir, st.lock))
	}
	for _, line := range r.notes {
		fmt.Fprintf(e.stdout, "dll-check: %s\n", line)
	}
	if len(r.problems) > 0 {
		return fmt.Errorf("%d problem(s):\n  - %s", len(r.problems), strings.Join(r.problems, "\n  - "))
	}
	fmt.Fprintf(e.stdout, "dll-check: %s passed\n", filepath.Base(*dll))
	return nil
}

// reSemVer is the SemVer 2.0.0 grammar, as native/CMakeLists.txt checks IM_VERSION.
var reSemVer = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)` +
	`(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)

func parseWinver(s string) ([4]uint16, error) {
	var v [4]uint16
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return v, fmt.Errorf("%q is not four numbers", s)
	}
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil || (len(p) > 1 && p[0] == '0') {
			return v, fmt.Errorf("%q: each part must be a number 0..65535", s)
		}
		v[i] = uint16(n)
	}
	return v, nil
}

// dllRules is what the checks compare with: the header's exports, the .def's, and the import list.
type dllRules struct {
	exports    []string // IM_API functions of the header, in header order
	defExports []string // isshoni-media.def's EXPORTS
	defLibrary string
	static     map[string]bool // dll-imports.txt, lower case
	delay      map[string]bool
}

var (
	headerPath     = filepath.Join("native", "include", "isshoni_media.h")
	defPath        = filepath.Join("native", "windows", "isshoni-media.def")
	dllImportsPath = filepath.Join("deploy", "windows", "dll-imports.txt")
)

func loadDLLRules(root string) (*dllRules, error) {
	header, err := os.ReadFile(filepath.Join(root, headerPath))
	if err != nil {
		return nil, err
	}
	def, err := os.ReadFile(filepath.Join(root, defPath))
	if err != nil {
		return nil, err
	}
	list, err := os.ReadFile(filepath.Join(root, dllImportsPath))
	if err != nil {
		return nil, err
	}
	r := &dllRules{exports: headerExports(header)}
	if len(r.exports) == 0 {
		return nil, fmt.Errorf("%s declares no IM_API function", headerPath)
	}
	if r.defLibrary, r.defExports, err = parseDef(def); err != nil {
		return nil, fmt.Errorf("%s: %w", defPath, err)
	}
	if r.static, r.delay, err = parseDLLImports(list); err != nil {
		return nil, fmt.Errorf("%s: %w", dllImportsPath, err)
	}
	return r, nil
}

var (
	reCComment = regexp.MustCompile(`(?s)/\*.*?\*/|//[^\n]*`)
	reIMAPI    = regexp.MustCompile(`IM_API\s+[^;(]*?\b(im_[a-z0-9_]+)\s*\(`)
	reCIdent   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// headerExports lists the functions a C header declares with IM_API (04 §5.1), in order.
func headerExports(header []byte) []string {
	text := reCComment.ReplaceAll(header, []byte(" "))
	var out []string
	for _, m := range reIMAPI.FindAllSubmatch(text, -1) {
		out = append(out, string(m[1]))
	}
	return out
}

// parseDef reads a module-definition file's LIBRARY name and EXPORTS names (';' starts a comment).
func parseDef(def []byte) (library string, exports []string, err error) {
	inExports := false
	sc := bufio.NewScanner(bytes.NewReader(def))
	for n := 1; sc.Scan(); n++ {
		line, _, _ := strings.Cut(sc.Text(), ";")
		line = strings.TrimSpace(line)
		switch {
		case line == "":
		case strings.HasPrefix(line, "LIBRARY"):
			library = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "LIBRARY")), `"`)
			inExports = false
		case line == "EXPORTS":
			inExports = true
		case inExports:
			name := strings.Fields(line)[0]
			if name != line || !reCIdent.MatchString(name) {
				return "", nil, fmt.Errorf("line %d: %q: plain names only (no ordinals, aliases or flags)", n, line)
			}
			exports = append(exports, name)
		default:
			return "", nil, fmt.Errorf("line %d: unexpected %q", n, line)
		}
	}
	return library, exports, sc.Err()
}

// parseDLLImports reads deploy/windows/dll-imports.txt: a "static:" and a "delay:" section, one DLL name per line,
// '#' comments. It refuses a name that may never be imported, mfplat.dll or avrt.dll as a static import, a name in
// both sections or twice, and anything that isn't a plain DLL file name.
func parseDLLImports(data []byte) (static, delay map[string]bool, err error) {
	static, delay = map[string]bool{}, map[string]bool{}
	var section map[string]bool
	var sectionName string
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line, _, _ := strings.Cut(sc.Text(), "#")
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			continue
		case line == "static:":
			section, sectionName = static, "static"
			continue
		case line == "delay:":
			section, sectionName = delay, "delay"
			continue
		case section == nil:
			return nil, nil, fmt.Errorf("line %d: %q before a static: or delay: section", n, line)
		}
		name := strings.ToLower(line)
		if strings.ContainsAny(name, ` /\:`) || !strings.HasSuffix(name, ".dll") {
			return nil, nil, fmt.Errorf("line %d: %q is not a DLL file name", n, line)
		}
		if why := forbiddenImport(name); why != "" {
			return nil, nil, fmt.Errorf("line %d: %s may never be imported: %s", n, name, why)
		}
		if sectionName == "static" && delayOnly(name) {
			return nil, nil, fmt.Errorf("line %d: %s is allowed only under delay: (04 §13.1)", n, name)
		}
		if static[name] || delay[name] {
			return nil, nil, fmt.Errorf("line %d: %s is listed twice", n, name)
		}
		section[name] = true
	}
	return static, delay, sc.Err()
}

// forbiddenImport says why isshoni-media.dll may never import the DLL name (lower case), or "".
func forbiddenImport(name string) string {
	switch {
	case strings.HasPrefix(name, "avcodec-") || strings.HasPrefix(name, "avutil-"):
		return "FFmpeg is loaded at run time, never import-linked (04 §9.2): the engine must work without it"
	case isCRuntime(name):
		return "a C runtime DLL: the DLL links the static CRT (/MT, 06 D7)"
	case strings.HasPrefix(name, "opus"):
		return "libopus is linked statically (vcpkg x64-windows-static)"
	}
	return ""
}

func isCRuntime(name string) bool {
	for _, p := range []string{"vcruntime", "msvcp", "msvcr", "api-ms-win-crt-", "ucrtbase", "concrt", "vccorlib"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// delayOnly are the DLLs that may only be delay-loaded: absent on N editions and Windows Server (04 §13.1).
func delayOnly(name string) bool { return name == "mfplat.dll" || name == "avrt.dll" }

// mediaFoundation are the DLLs FFmpeg may not import (06 §4.4).
func mediaFoundation(name string) bool {
	return name == "mfplat.dll" || name == "mf.dll" || name == "mfreadwrite.dll" || name == "evr.dll"
}

type dllExpect struct {
	version    string
	winver     [4]uint16
	loadConfig bool
}

// report collects problems and notes.
type report struct {
	problems []string
	notes    []string
}

func (r *report) fail(format string, args ...any) {
	r.problems = append(r.problems, fmt.Sprintf(format, args...))
}
func (r *report) note(format string, args ...any) {
	r.notes = append(r.notes, fmt.Sprintf(format, args...))
}
func (r *report) add(o report) {
	r.problems = append(r.problems, o.problems...)
	r.notes = append(r.notes, o.notes...)
}

// Strings of 04 §13.4's VERSIONINFO besides the two version strings.
var wantVersionStrings = [][2]string{
	{"ProductName", "isshoni"},
	{"CompanyName", "isshoni project"},
	{"LegalCopyright", "Copyright the isshoni authors. Apache-2.0."},
	{"OriginalFilename", "isshoni-media.dll"},
	{"FileDescription", "isshoni media engine"},
	{"InternalName", "isshoni-media"},
}

const engineDLLName = "isshoni-media.dll"

func checkEngineDLL(data []byte, rules *dllRules, want dllExpect) report {
	var r report
	img, err := parsePE(data)
	if err != nil {
		r.fail("%s", err)
		return r
	}
	if img.machine != 0x8664 || !img.is64 {
		r.fail("the image is not x64 (machine 0x%04x)", img.machine)
	}
	if !img.isDLL {
		r.fail("the image is not a DLL")
	}
	for _, f := range []struct {
		bit  uint16
		name string
	}{{dllDynamicBase, "DYNAMIC_BASE (/DYNAMICBASE)"}, {dllHighEntropyVA, "HIGH_ENTROPY_VA (/HIGHENTROPYVA)"},
		{dllNXCompat, "NX_COMPAT (/NXCOMPAT)"}, {dllGuardCF, "GUARD_CF (/guard:cf)"}} {
		if img.dllChars&f.bit == 0 {
			r.fail("DllCharacteristics lacks %s", f.name)
		}
	}

	checkExports(&r, img, rules)
	checkImports(&r, img, rules)
	checkVersion(&r, img, want)

	switch flags, ok, err := img.dependentLoadFlags(); {
	case err != nil:
		r.fail("%s", err)
	case want.loadConfig && (!ok || flags != 0x800):
		r.fail("load config: DependentLoadFlags is 0x%04x, want 0x0800 (link with /DEPENDENTLOADFLAG:0x800)", flags)
	case want.loadConfig:
		r.note("load config: DependentLoadFlags 0x%04x", flags)
	default:
		r.note(loadConfigNote, flags)
	}
	return r
}

// loadConfigNote is the load-config line without -load-config: printed for people, never a problem (06 §5.1).
const loadConfigNote = "load config: DependentLoadFlags 0x%04x (not checked: the delay-load helper doesn't honor " +
	"/DEPENDENTLOADFLAG, so the engine's delay-load hook is the guard, 04 §13.1)"

func checkExports(r *report, img *peImage, rules *dllRules) {
	if !slices.Equal(sortedCopy(rules.defExports), sortedCopy(rules.exports)) {
		r.fail("%s does not list exactly the IM_API functions of %s: %s", filepath.ToSlash(defPath),
			filepath.ToSlash(headerPath), setDiff(rules.defExports, rules.exports))
	}
	if rules.defLibrary != "isshoni-media" {
		r.fail("%s: LIBRARY is %q, want isshoni-media", filepath.ToSlash(defPath), rules.defLibrary)
	}
	ex, err := img.exports()
	if err != nil {
		r.fail("%s", err)
		return
	}
	if ex.dllName == "" && len(ex.names) == 0 {
		r.fail("the DLL has no export table")
	} else if !strings.EqualFold(ex.dllName, engineDLLName) {
		r.fail("the export directory names the DLL %q, want %s", ex.dllName, engineDLLName)
	}
	if !slices.Equal(sortedCopy(ex.names), sortedCopy(rules.exports)) {
		r.fail("the exports differ from the IM_API functions of %s: %s", filepath.ToSlash(headerPath),
			setDiff(ex.names, rules.exports))
	}
	if ex.unnamed > 0 {
		r.fail("%d function(s) exported by ordinal only", ex.unnamed)
	}
	if len(ex.forwarded) > 0 {
		r.fail("forwarded exports: %s", strings.Join(ex.forwarded, ", "))
	}
	r.note("exports: %d, the IM_API functions of %s", len(ex.names), filepath.ToSlash(headerPath))
}

func checkImports(r *report, img *peImage, rules *dllRules) {
	static, err := img.imports()
	if err != nil {
		r.fail("%s", err)
	}
	delay, err := img.delayImports()
	if err != nil {
		r.fail("%s", err)
	}
	for _, kind := range []struct {
		name, otherName string
		dlls            []string
		allowed, other  map[string]bool
	}{{"static", "delay", static, rules.static, rules.delay}, {"delay", "static", delay, rules.delay, rules.static}} {
		for _, dll := range kind.dlls {
			name := strings.ToLower(dll)
			switch {
			case forbiddenImport(name) != "":
				r.fail("%s import of %s: %s", kind.name, dll, forbiddenImport(name))
			case kind.name == "static" && delayOnly(name):
				r.fail("static import of %s: it must be delay-loaded (absent on N editions and Windows Server, 04 §13.1)", dll)
			case kind.allowed[name]:
			case kind.other[name]:
				r.fail("%s import of %s: %s lists it under %s:", kind.name, dll, filepath.ToSlash(dllImportsPath),
					kind.otherName)
			default:
				r.fail("%s import of %s is not listed in %s (add it under %s: with a reason)", kind.name, dll,
					filepath.ToSlash(dllImportsPath), kind.name)
			}
		}
		r.note("%s imports: %s", kind.name, listOrNone(kind.dlls))
	}
}

func checkVersion(r *report, img *peImage, want dllExpect) {
	v, err := img.versionResource()
	switch {
	case err != nil:
		r.fail("%s", err)
		return
	case v == nil:
		r.fail("no VERSIONINFO resource (native/windows/resources/version.rc.in)")
		return
	}
	if v.fileVersion != want.winver {
		r.fail("VERSIONINFO FILEVERSION is %s, want %s", formatVersion(v.fileVersion), formatVersion(want.winver))
	}
	if v.productVersion != want.winver {
		r.fail("VERSIONINFO PRODUCTVERSION is %s, want %s", formatVersion(v.productVersion), formatVersion(want.winver))
	}
	expect := append([][2]string{{"ProductVersion", want.version}, {"FileVersion", formatVersion(want.winver)}},
		wantVersionStrings...)
	for _, kv := range expect {
		got, ok := v.strings[kv[0]]
		switch {
		case !ok:
			r.fail("VERSIONINFO has no %s string (want %q)", kv[0], kv[1])
		case got != kv[1]:
			r.fail("VERSIONINFO %s is %q, want %q", kv[0], got, kv[1])
		}
	}
	r.note("version: ProductVersion %s, FileVersion %s, OriginalFilename %s", v.strings["ProductVersion"],
		v.strings["FileVersion"], v.strings["OriginalFilename"])
}

// checkFFmpegDLLs checks the FFmpeg DLLs in dir against the lock (06 §4.4, §5.1).
func checkFFmpegDLLs(dir string, lock *Lock) report {
	var r report
	entries, err := os.ReadDir(dir)
	if err != nil {
		r.fail("ffmpeg: %s", err)
		return r
	}
	want := map[string]bool{lock.AvcodecDLL(): false, lock.AvutilDLL(): false}
	for _, ent := range entries {
		name := strings.ToLower(ent.Name())
		if !strings.HasSuffix(name, ".dll") || (!strings.HasPrefix(name, "avcodec-") && !strings.HasPrefix(name, "avutil-")) {
			continue
		}
		if _, ok := want[name]; !ok {
			r.fail("ffmpeg: %s is not named after lock.json's majors (%s, %s)", ent.Name(), lock.AvcodecDLL(),
				lock.AvutilDLL())
			continue
		}
		want[name] = true
		data, err := os.ReadFile(filepath.Join(dir, ent.Name()))
		if err != nil {
			r.fail("ffmpeg: %s", err)
			continue
		}
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); got != lock.DLLs[name] {
			r.fail("ffmpeg: %s has SHA-256 %s, lock.json has %s", ent.Name(), got, lock.DLLs[name])
		}
		img, err := parsePE(data)
		if err != nil {
			r.fail("ffmpeg: %s: %s", ent.Name(), err)
			continue
		}
		static, err1 := img.imports()
		delay, err2 := img.delayImports()
		if err := errors.Join(err1, err2); err != nil {
			r.fail("ffmpeg: %s: %s", ent.Name(), err)
		}
		for _, dll := range append(static, delay...) {
			l := strings.ToLower(dll)
			switch {
			case mediaFoundation(l):
				r.fail("ffmpeg: %s imports %s: Media Foundation must be loaded at run time (06 §4.4)", ent.Name(), dll)
			case isCRuntime(l):
				r.fail("ffmpeg: %s imports %s: FFmpeg is built with the static CRT (-MT)", ent.Name(), dll)
			}
		}
	}
	for _, name := range sortedKeys(want) {
		if !want[name] {
			r.fail("ffmpeg: %s is missing from %s", name, dir)
		}
	}
	r.note("ffmpeg: %s, %s checked against native/ffmpeg/lock.json (%s)", lock.AvcodecDLL(), lock.AvutilDLL(), lock.Tag)
	return r
}

func sortedCopy(s []string) []string {
	c := slices.Clone(s)
	sort.Strings(c)
	return c
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// setDiff describes how got differs from want.
func setDiff(got, want []string) string {
	g, w := map[string]int{}, map[string]bool{}
	for _, s := range got {
		g[s]++
	}
	for _, s := range want {
		w[s] = true
	}
	var extra, missing, twice []string
	for _, s := range sortedKeys(boolSet(got)) {
		if !w[s] {
			extra = append(extra, s)
		}
		if g[s] > 1 {
			twice = append(twice, s)
		}
	}
	for _, s := range sortedKeys(w) {
		if g[s] == 0 {
			missing = append(missing, s)
		}
	}
	var parts []string
	if len(extra) > 0 {
		parts = append(parts, "extra "+strings.Join(extra, ", "))
	}
	if len(missing) > 0 {
		parts = append(parts, "missing "+strings.Join(missing, ", "))
	}
	if len(twice) > 0 {
		parts = append(parts, "twice "+strings.Join(twice, ", "))
	}
	return strings.Join(parts, "; ")
}

func boolSet(s []string) map[string]bool {
	m := make(map[string]bool, len(s))
	for _, v := range s {
		m[v] = true
	}
	return m
}

func listOrNone(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
}
