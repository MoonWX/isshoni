package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// repoRules are the committed header, .def and dll-imports.txt (repoRoot is ffmpeg_test.go's).
func repoRules(t *testing.T) *dllRules {
	t.Helper()
	rules, err := loadDLLRules(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	return rules
}

var goodWinver = [4]uint16{0, 0, 0, 7}

func goodStrings() [][2]string {
	return [][2]string{
		{"CompanyName", "isshoni project"},
		{"FileDescription", "isshoni media engine"},
		{"FileVersion", "0.0.0.7"},
		{"InternalName", "isshoni-media"},
		{"LegalCopyright", "Copyright the isshoni authors. Apache-2.0."},
		{"OriginalFilename", "isshoni-media.dll"},
		{"ProductName", "isshoni"},
		{"ProductVersion", "0.0.0-ci.7"},
	}
}

// goodDLL is the skeleton DLL as it should come out of win-x64-release, with the delay imports later slices add.
func goodDLL(rules *dllRules) fakeDLL {
	return fakeDLL{
		exports: slices.Clone(rules.exports),
		imports: []string{"KERNEL32.dll", "ole32.dll"},
		delay:   []string{"mfplat.dll", "avrt.dll"},
		version: &fakeVersion{file: goodWinver, product: goodWinver, strings: goodStrings()},
	}
}

var goodExpect = dllExpect{version: "0.0.0-ci.7", winver: goodWinver}

func withString(d fakeDLL, key, value string) fakeDLL {
	v := *d.version
	v.strings = slices.Clone(v.strings)
	for i := range v.strings {
		if v.strings[i][0] == key {
			v.strings[i][1] = value
		}
	}
	d.version = &v
	return d
}

// TestRepoFilesAgree: the committed header, isshoni-media.def and dll-imports.txt load, and the .def lists exactly
// the header's 39 IM_API functions (the same check exports_test makes in C++, here on every OS).
func TestRepoFilesAgree(t *testing.T) {
	rules := repoRules(t)
	if len(rules.exports) != 39 {
		t.Errorf("the header declares %d IM_API functions, want v1.0's 39", len(rules.exports))
	}
	if !slices.Equal(rules.defExports, rules.exports) {
		t.Errorf("isshoni-media.def differs from the header (order included): %s", setDiff(rules.defExports, rules.exports))
	}
	if rules.defLibrary != "isshoni-media" {
		t.Errorf("LIBRARY %q", rules.defLibrary)
	}
	for _, dll := range []string{"kernel32.dll", "ole32.dll"} {
		if !rules.static[dll] {
			t.Errorf("dll-imports.txt lacks %s under static:", dll)
		}
	}
	for _, dll := range []string{"mfplat.dll", "avrt.dll"} {
		if !rules.delay[dll] {
			t.Errorf("dll-imports.txt lacks %s under delay:", dll)
		}
	}
}

func TestEngineDLLChecks(t *testing.T) {
	rules := repoRules(t)
	good := goodDLL(rules)
	for _, tc := range []struct {
		name   string
		dll    fakeDLL
		expect dllExpect
		want   []string // fragments of the problems; none = passes
	}{
		{name: "good", dll: good},
		{name: "String lengths in bytes", dll: func() fakeDLL {
			d := good
			v := *d.version
			v.byteLengths = true
			d.version = &v
			return d
		}()},
		{name: "another string table", dll: func() fakeDLL {
			d := good
			v := *d.version
			v.table = "000004b0"
			d.version = &v
			return d
		}()},
		{name: "no delay imports yet", dll: func() fakeDLL { d := good; d.delay = nil; return d }()},
		{name: "an extra export", dll: func() fakeDLL { d := good; d.exports = append(slices.Clone(d.exports), "im_extra"); return d }(),
			want: []string{"extra im_extra"}},
		{name: "a missing export", dll: func() fakeDLL { d := good; d.exports = d.exports[1:]; return d }(),
			want: []string{"missing " + good.exports[0]}},
		{name: "an export by ordinal only", dll: func() fakeDLL { d := good; d.unnamed = 1; return d }(),
			want: []string{"1 function(s) exported by ordinal only"}},
		{name: "a forwarded export", dll: func() fakeDLL {
			d := good
			d.exports = slices.DeleteFunc(slices.Clone(d.exports), func(s string) bool { return s == "im_version" })
			d.forwarded = []string{"im_version"}
			return d
		}(), want: []string{"forwarded exports: im_version"}},
		{name: "another export name", dll: func() fakeDLL { d := good; d.exportName = "isshoni_media.dll"; return d }(),
			want: []string{`names the DLL "isshoni_media.dll"`}},
		{name: "VCRUNTIME140.dll", dll: func() fakeDLL {
			d := good
			d.imports = append(slices.Clone(d.imports), "VCRUNTIME140.dll")
			return d
		}(), want: []string{"static import of VCRUNTIME140.dll: a C runtime DLL"}},
		{name: "the UCRT", dll: func() fakeDLL {
			d := good
			d.imports = append(slices.Clone(d.imports), "api-ms-win-crt-runtime-l1-1-0.dll", "ucrtbased.dll", "MSVCP140D.dll")
			return d
		}(), want: []string{"api-ms-win-crt-runtime-l1-1-0.dll: a C runtime", "ucrtbased.dll: a C runtime",
			"MSVCP140D.dll: a C runtime"}},
		{name: "avcodec statically", dll: func() fakeDLL {
			d := good
			d.imports = append(slices.Clone(d.imports), "avcodec-62.dll")
			return d
		}(), want: []string{"static import of avcodec-62.dll: FFmpeg is loaded at run time"}},
		{name: "avutil delay-loaded", dll: func() fakeDLL {
			d := good
			d.delay = append(slices.Clone(d.delay), "avutil-60.dll")
			return d
		}(), want: []string{"delay import of avutil-60.dll: FFmpeg is loaded at run time"}},
		{name: "opus.dll", dll: func() fakeDLL { d := good; d.imports = append(slices.Clone(d.imports), "opus.dll"); return d }(),
			want: []string{"opus.dll: libopus is linked statically"}},
		{name: "mfplat.dll statically", dll: func() fakeDLL {
			d := good
			d.imports = append(slices.Clone(d.imports), "mfplat.dll")
			return d
		}(), want: []string{"static import of mfplat.dll: it must be delay-loaded"}},
		{name: "avrt.dll statically", dll: func() fakeDLL {
			d := good
			d.imports = append(slices.Clone(d.imports), "AVRT.dll")
			d.delay = nil
			return d
		}(), want: []string{"static import of AVRT.dll: it must be delay-loaded"}},
		{name: "an unlisted DLL", dll: func() fakeDLL {
			d := good
			d.imports = append(slices.Clone(d.imports), "WINHTTP.dll")
			return d
		}(), want: []string{"static import of WINHTTP.dll is not listed"}},
		{name: "a static DLL delay-loaded", dll: func() fakeDLL {
			d := good
			d.delay = append(slices.Clone(d.delay), "user32.dll")
			return d
		}(), want: []string{"delay import of user32.dll: deploy/windows/dll-imports.txt lists it under static:"}},
		{name: "a wrong ProductVersion", dll: withString(good, "ProductVersion", "0.0.0-dev"),
			want: []string{`VERSIONINFO ProductVersion is "0.0.0-dev", want "0.0.0-ci.7"`}},
		{name: "a wrong FileVersion", dll: withString(good, "FileVersion", "0.0.0.0"),
			want: []string{`FileVersion is "0.0.0.0", want "0.0.0.7"`}},
		{name: "a wrong OriginalFilename", dll: withString(good, "OriginalFilename", "isshoni_media.dll"),
			want: []string{`OriginalFilename is "isshoni_media.dll", want "isshoni-media.dll"`}},
		{name: "a wrong binary FILEVERSION", dll: func() fakeDLL {
			d := good
			v := *d.version
			v.file = [4]uint16{0, 0, 0, 0}
			d.version = &v
			return d
		}(), want: []string{"FILEVERSION is 0.0.0.0, want 0.0.0.7"}},
		{name: "a version for another release", dll: good,
			expect: dllExpect{version: "0.2.0", winver: [4]uint16{0, 2, 0, 65000}},
			want:   []string{"PRODUCTVERSION is 0.0.0.7, want 0.2.0.65000", `ProductVersion is "0.0.0-ci.7", want "0.2.0"`}},
		{name: "no VERSIONINFO", dll: func() fakeDLL { d := good; d.version = nil; return d }(),
			want: []string{"no VERSIONINFO resource"}},
		{name: "a missing string", dll: func() fakeDLL {
			d := good
			v := *d.version
			v.strings = v.strings[1:] // CompanyName
			d.version = &v
			return d
		}(), want: []string{"no CompanyName string"}},
		{name: "32-bit", dll: func() fakeDLL { d := good; d.pe32 = true; d.machine = 0x14c; return d }(),
			want: []string{"not x64"}},
		{name: "an executable", dll: func() fakeDLL { d := good; d.exe = true; return d }(),
			want: []string{"not a DLL"}},
		{name: "no Control Flow Guard", dll: func() fakeDLL {
			d := good
			d.dllChars = dllDynamicBase | dllHighEntropyVA | dllNXCompat
			return d
		}(), want: []string{"lacks GUARD_CF"}},
		{name: "load config required and set", dll: func() fakeDLL { d := good; d.loadConfig = true; d.dependFlags = 0x800; return d }(),
			expect: dllExpect{version: goodExpect.version, winver: goodWinver, loadConfig: true}},
		{name: "load config required, flag missing", dll: func() fakeDLL { d := good; d.loadConfig = true; return d }(),
			expect: dllExpect{version: goodExpect.version, winver: goodWinver, loadConfig: true},
			want:   []string{"DependentLoadFlags is 0x0000, want 0x0800"}},
		{name: "load config required, none", dll: good,
			expect: dllExpect{version: goodExpect.version, winver: goodWinver, loadConfig: true},
			want:   []string{"DependentLoadFlags is 0x0000, want 0x0800"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expect := tc.expect
			if expect.version == "" {
				expect = goodExpect
			}
			r := checkEngineDLL(tc.dll.build(), rules, expect)
			if len(tc.want) == 0 {
				if len(r.problems) > 0 {
					t.Fatalf("problems: %q", r.problems)
				}
				return
			}
			all := strings.Join(r.problems, "\n")
			for _, w := range tc.want {
				if !strings.Contains(all, w) {
					t.Errorf("no problem mentions %q; problems:\n%s", w, all)
				}
			}
		})
	}
}

// TestPEReader reads back what the fake image holds, field by field.
func TestPEReader(t *testing.T) {
	d := fakeDLL{
		exports: []string{"b", "a"}, imports: []string{"KERNEL32.dll"}, delay: []string{"mfplat.dll"},
		loadConfig: true, dependFlags: 0x800,
		version: &fakeVersion{file: [4]uint16{1, 2, 3, 4}, product: [4]uint16{5, 6, 7, 8},
			strings: [][2]string{{"ProductVersion", "1.2.3-rc.1+abc"}, {"Comments", "ünïcödé"}, {"Empty", ""}}},
	}
	img, err := parsePE(d.build())
	if err != nil {
		t.Fatal(err)
	}
	ex, err := img.exports()
	if err != nil || ex.dllName != "isshoni-media.dll" || !slices.Equal(ex.names, []string{"a", "b"}) {
		t.Errorf("exports %+v, %v", ex, err)
	}
	if im, err := img.imports(); err != nil || !slices.Equal(im, []string{"KERNEL32.dll"}) {
		t.Errorf("imports %v, %v", im, err)
	}
	if dl, err := img.delayImports(); err != nil || !slices.Equal(dl, []string{"mfplat.dll"}) {
		t.Errorf("delay imports %v, %v", dl, err)
	}
	if f, ok, err := img.dependentLoadFlags(); err != nil || !ok || f != 0x800 {
		t.Errorf("DependentLoadFlags 0x%x, %v, %v", f, ok, err)
	}
	v, err := img.versionResource()
	if err != nil {
		t.Fatal(err)
	}
	if v.fileVersion != [4]uint16{1, 2, 3, 4} || v.productVersion != [4]uint16{5, 6, 7, 8} {
		t.Errorf("versions %v %v", v.fileVersion, v.productVersion)
	}
	if v.strings["ProductVersion"] != "1.2.3-rc.1+abc" || v.strings["Comments"] != "ünïcödé" || v.strings["Empty"] != "" {
		t.Errorf("strings %q", v.strings)
	}
	if !slices.Equal(v.order, []string{"ProductVersion", "Comments", "Empty"}) {
		t.Errorf("order %q", v.order)
	}

	// Address-based delay descriptors (Visual C++ 6) exist in 32-bit images only.
	old, err := parsePE(fakeDLL{pe32: true, machine: 0x14c, delay: []string{"a.dll", "b.dll"}, delayVA: true}.build())
	if err != nil {
		t.Fatal(err)
	}
	if dl, err := old.delayImports(); err != nil || !slices.Equal(dl, []string{"a.dll", "b.dll"}) {
		t.Errorf("32-bit address-based delay imports %v, %v", dl, err)
	}
	bad, err := parsePE(fakeDLL{delay: []string{"a.dll"}, delayVA: true}.build())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.delayImports(); err == nil || !strings.Contains(err.Error(), "address-based") {
		t.Errorf("an address-based descriptor in a 64-bit image: %v", err)
	}

	// An image without the optional directories has none of them, and no error.
	bare, err := parsePE(fakeDLL{}.build())
	if err != nil {
		t.Fatal(err)
	}
	if ex, err := bare.exports(); err != nil || len(ex.names) != 0 {
		t.Errorf("bare exports %+v, %v", ex, err)
	}
	if v, err := bare.versionResource(); err != nil || v != nil {
		t.Errorf("bare version %v, %v", v, err)
	}
	if _, ok, err := bare.dependentLoadFlags(); err != nil || ok {
		t.Errorf("bare load config: %v, %v", ok, err)
	}

	for _, junk := range [][]byte{nil, []byte("MZ"), []byte("not a PE file at all, just text")} {
		if _, err := parsePE(junk); err == nil {
			t.Errorf("parsePE(%q) succeeded", junk)
		}
	}
}

func TestHeaderExports(t *testing.T) {
	header := []byte(`
/* IM_API im_commented(void); */
// IM_API im_line_commented(void);
IM_API uint32_t IM_CALL im_one(void);
IM_API int32_t IM_CALL im_two(const char *a,
                             int b);
#define IM_API __declspec(dllexport)
IM_API im_status IM_CALL
im_three(void); /* a declaration over two lines */
`)
	if got := headerExports(header); !slices.Equal(got, []string{"im_one", "im_two", "im_three"}) {
		t.Errorf("headerExports = %q", got)
	}
}

func TestParseDef(t *testing.T) {
	lib, ex, err := parseDef([]byte("; comment\r\nLIBRARY \"isshoni-media\"\r\nEXPORTS\r\n    im_a ; why\r\n\r\n    im_b\r\n"))
	if err != nil || lib != "isshoni-media" || !slices.Equal(ex, []string{"im_a", "im_b"}) {
		t.Errorf("parseDef = %q, %q, %v", lib, ex, err)
	}
	for _, bad := range []string{"EXPORTS\n  im_a @1\n", "im_a\n", "LIBRARY x\nEXPORTS\n im_a=im_b\n"} {
		if _, _, err := parseDef([]byte(bad)); err == nil {
			t.Errorf("parseDef(%q) accepted it", bad)
		}
	}
}

func TestParseDLLImports(t *testing.T) {
	static, delay, err := parseDLLImports([]byte("# x\nstatic:\n  KERNEL32.dll # why\n\ndelay:\n  mfplat.dll\n"))
	if err != nil || !static["kernel32.dll"] || !delay["mfplat.dll"] || len(static) != 1 || len(delay) != 1 {
		t.Errorf("parse = %v %v %v", static, delay, err)
	}
	for _, tc := range []struct{ list, want string }{
		{"kernel32.dll\n", "before a static: or delay: section"},
		{"static:\n  mfplat.dll\n", "allowed only under delay:"},
		{"static:\n  avrt.dll\n", "allowed only under delay:"},
		{"delay:\n  avcodec-62.dll\n", "may never be imported"},
		{"static:\n  vcruntime140.dll\n", "may never be imported"},
		{"static:\n  ucrtbased.dll\n", "may never be imported"},
		{"static:\n  opus.dll\n", "may never be imported"},
		{"static:\n  a.dll\n  A.DLL\n", "listed twice"},
		{"static:\n  a.dll\ndelay:\n  a.dll\n", "listed twice"},
		{"static:\n  kernel32\n", "not a DLL file name"},
		{"static:\n  C:\\x\\a.dll\n", "not a DLL file name"},
		{"static:\n  a.dll b.dll\n", "not a DLL file name"},
	} {
		if _, _, err := parseDLLImports([]byte(tc.list)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parseDLLImports(%q) = %v, want %q", tc.list, err, tc.want)
		}
	}
}

func TestParseWinver(t *testing.T) {
	if v, err := parseWinver("0.2.0.65000"); err != nil || v != [4]uint16{0, 2, 0, 65000} {
		t.Errorf("parseWinver = %v, %v", v, err)
	}
	for _, bad := range []string{"", "1.2.3", "1.2.3.4.5", "1.2.3.65536", "1.2.3.-1", "1.2.3.x", "01.2.3.4"} {
		if _, err := parseWinver(bad); err == nil {
			t.Errorf("parseWinver(%q) accepted it", bad)
		}
	}
}

func TestFFmpegDLLChecks(t *testing.T) {
	plain := fakeDLL{exports: []string{"avcodec_open2"}, imports: []string{"KERNEL32.dll", "avutil-60.dll"}}
	write := func(t *testing.T, dir string, files map[string]fakeDLL) map[string]string {
		sums := map[string]string{}
		for name, d := range files {
			data := d.build()
			if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(data)
			sums[strings.ToLower(name)] = hex.EncodeToString(sum[:])
		}
		return sums
	}
	lockFor := func(sums map[string]string) *Lock {
		return &Lock{Tag: "deps-ffmpeg-9.0.2-r1", AvcodecMajor: 62, AvutilMajor: 60,
			DLLs: map[string]string{"avcodec-62.dll": sums["avcodec-62.dll"], "avutil-60.dll": sums["avutil-60.dll"]}}
	}
	for _, tc := range []struct {
		name  string
		files map[string]fakeDLL
		edit  func(l *Lock)
		want  []string
	}{
		{name: "good", files: map[string]fakeDLL{"avcodec-62.dll": plain, "avutil-60.dll": {imports: []string{"KERNEL32.dll"}}}},
		{name: "a Media Foundation import", files: map[string]fakeDLL{
			"avcodec-62.dll": {imports: []string{"KERNEL32.dll", "MFPlat.DLL"}}, "avutil-60.dll": plain},
			want: []string{"avcodec-62.dll imports MFPlat.DLL: Media Foundation"}},
		{name: "a delay-loaded Media Foundation import", files: map[string]fakeDLL{
			"avcodec-62.dll": {delay: []string{"mfreadwrite.dll"}}, "avutil-60.dll": plain},
			want: []string{"imports mfreadwrite.dll"}},
		{name: "a C runtime import", files: map[string]fakeDLL{
			"avcodec-62.dll": {imports: []string{"VCRUNTIME140.dll"}}, "avutil-60.dll": plain},
			want: []string{"imports VCRUNTIME140.dll: FFmpeg is built with the static CRT"}},
		{name: "another major", files: map[string]fakeDLL{"avcodec-61.dll": plain, "avutil-60.dll": plain},
			want: []string{"avcodec-61.dll is not named after lock.json's majors", "avcodec-62.dll is missing"}},
		{name: "a changed DLL", files: map[string]fakeDLL{"avcodec-62.dll": plain, "avutil-60.dll": plain},
			edit: func(l *Lock) { l.DLLs["avutil-60.dll"] = strings.Repeat("0", 64) },
			want: []string{"avutil-60.dll has SHA-256"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			lock := lockFor(write(t, dir, tc.files))
			if tc.edit != nil {
				tc.edit(lock)
			}
			r := checkFFmpegDLLs(dir, lock)
			all := strings.Join(r.problems, "\n")
			if len(tc.want) == 0 && len(r.problems) > 0 {
				t.Fatalf("problems: %q", r.problems)
			}
			for _, w := range tc.want {
				if !strings.Contains(all, w) {
					t.Errorf("no problem mentions %q; problems:\n%s", w, all)
				}
			}
		})
	}
}

// TestDLLCheckCommand runs the command on fixture files: exit 0 with a summary, 1 with every problem, 2 on usage.
func TestDLLCheckCommand(t *testing.T) {
	root := repoRoot(t)
	rules := repoRules(t)
	dir := t.TempDir()
	good := filepath.Join(dir, "isshoni-media.dll")
	if err := os.WriteFile(good, goodDLL(rules).build(), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := goodDLL(rules)
	bad.exports = append(bad.exports, "im_extra")
	bad.imports = append(bad.imports, "VCRUNTIME140.dll", "mfplat.dll")
	bad = withString(bad, "ProductVersion", "9.9.9")
	badPath := filepath.Join(dir, "bad.dll")
	if err := os.WriteFile(badPath, bad.build(), 0o600); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCmd(t, "-root", root, "dll-check", "-dll", good, "-version", "0.0.0-ci.7", "-winver", "0.0.0.7")
	if code != 0 || !strings.Contains(stdout, "isshoni-media.dll passed") || !strings.Contains(stdout, "exports: 39") ||
		!strings.Contains(stdout, "static imports: KERNEL32.dll, ole32.dll") {
		t.Errorf("good: exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}

	code, _, stderr = runCmd(t, "-root", root, "dll-check", "-dll", badPath, "-version", "0.0.0-ci.7", "-winver", "0.0.0.7")
	if code != 1 || !strings.Contains(stderr, "4 problem(s)") || !strings.Contains(stderr, "extra im_extra") ||
		!strings.Contains(stderr, "VCRUNTIME140.dll") || !strings.Contains(stderr, "mfplat.dll: it must be delay-loaded") {
		t.Errorf("bad: exit %d\nstderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, `ProductVersion is "9.9.9"`) {
		t.Errorf("bad: the ProductVersion problem is missing:\n%s", stderr)
	}

	for _, args := range [][]string{
		{"-dll", good},
		{"-dll", good, "-version", "0.0.0-ci.7"},
		{"-dll", good, "-version", "v1", "-winver", "0.0.0.7"},
		{"-dll", good, "-version", "0.0.0-ci.7", "-winver", "1.2.3"},
		{"-dll", good, "-version", "0.0.0-ci.7", "-winver", "0.0.0.7", "extra"},
	} {
		if code, _, stderr := runCmd(t, append([]string{"-root", root, "dll-check"}, args...)...); code != 2 {
			t.Errorf("%q: exit %d, want 2 (stderr %q)", args, code, stderr)
		}
	}
	if code, _, _ := runCmd(t, "-root", root, "dll-check", "-dll", filepath.Join(dir, "missing.dll"), "-version",
		"0.0.0-ci.7", "-winver", "0.0.0.7"); code != 1 {
		t.Errorf("a missing DLL: exit %d, want 1", code)
	}
}
