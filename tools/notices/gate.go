package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/google/licensecheck"
)

// allowlist is the shipped-license allowlist of 06 §8.3, the same list web/scripts/licenses.mjs applies to the
// bundled npm packages.
var allowlist = map[string]bool{
	"MIT":           true,
	"ISC":           true,
	"BSD-2-Clause":  true,
	"BSD-3-Clause":  true,
	"Apache-2.0":    true,
	"0BSD":          true,
	"Zlib":          true,
	"CC0-1.0":       true,
	"Unlicense":     true,
	"BlueOak-1.0.0": true,
}

// neverExcepted reports whether a license can never be shipped, not even with an exception: GPL, AGPL and LGPL
// (the plan's rule; LGPL cannot be linked statically into the binary), SSPL and BUSL.
func neverExcepted(id string) bool {
	for _, prefix := range []string{"GPL", "AGPL", "LGPL", "SSPL", "BUSL"} {
		if strings.HasPrefix(id, prefix) {
			return true
		}
	}
	return false
}

// minCoverage is the share of a license file's words, in percent, that must match known license texts before
// its license counts as identified. pkg.go.dev uses the same threshold with licensecheck.
const minCoverage = 75

var (
	// licenseFileRE matches the files that are reproduced: LICENSE*, LICENCE*, COPYING*, UNLICENSE* and NOTICE*,
	// except source files such as license.go (codeExtRE).
	licenseFileRE = regexp.MustCompile(`(?i)^((un)?licen[cs]e|copying|notice)`)
	codeExtRE     = regexp.MustCompile(`(?i)\.([cm]?js|[cm]?ts|[jt]sx|go|json|ya?ml|toml|sh|py|rb|rs|java|[ch]|cc|cpp|s|css|html?)$`)
	// noticeFileRE matches the NOTICE files among them, which are reproduced but not classified.
	noticeFileRE = regexp.MustCompile(`(?i)^notice`)
	// primaryFileRE matches a module's own license files: LICENSE, LICENCE, COPYING or UNLICENSE, optionally with
	// a text extension. Other license files (LICENSE-3RD-PARTY.md, LICENSE-GO, ...) usually describe bundled or
	// related material: they are reproduced and scanned, but the allowlist and the coverage rule apply to them only
	// when a module has no primary file. A GPL, AGPL, LGPL, SSPL or BUSL license in any of them fails the gate.
	primaryFileRE = regexp.MustCompile(`(?i)^((un)?licen[cs]e|copying)(\.(txt|md|markdown|rst))?$`)
)

// licenseFile is one reproduced file of a module's root directory.
type licenseFile struct {
	name     string   // file name, relative to the module root
	text     string   // normalized text
	notice   bool     // a NOTICE file, reproduced but not scanned
	gated    bool     // the module's license file, subject to the whole gate
	ids      []string // licenses found in it (every file but NOTICE files)
	coverage float64  // percent of the text that matches known licenses (every file but NOTICE files)
}

// readLicenseFiles reads and classifies the license and notice files in dir, license files first, each group
// sorted by name. Every file but the NOTICE files is scanned with licensecheck; the gated ones are the primary
// files, or every license file when there is no primary one.
func readLicenseFiles(dir string) ([]licenseFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []licenseFile
	hasPrimary := false
	for _, e := range entries {
		if !e.Type().IsRegular() || !licenseFileRE.MatchString(e.Name()) || codeExtRE.MatchString(e.Name()) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		f := licenseFile{name: e.Name(), text: normalize(string(data)), notice: noticeFileRE.MatchString(e.Name())}
		if !f.notice && primaryFileRE.MatchString(f.name) {
			hasPrimary = true
		}
		files = append(files, f)
	}
	for i := range files {
		f := &files[i]
		if f.notice {
			continue
		}
		f.gated = !hasPrimary || primaryFileRE.MatchString(f.name)
		cov := licensecheck.Scan([]byte(f.text))
		f.coverage = cov.Percent
		for _, m := range cov.Match {
			if !slices.Contains(f.ids, m.ID) {
				f.ids = append(f.ids, m.ID)
			}
		}
		sort.Strings(f.ids)
	}
	sort.SliceStable(files, func(i, j int) bool {
		if files[i].notice != files[j].notice {
			return !files[i].notice
		}
		return files[i].name < files[j].name
	})
	return files, nil
}

// normalize makes a text file's content deterministic: LF line ends, no BOM, no trailing blanks, no leading or
// trailing empty lines, and a final newline.
func normalize(s string) string {
	s = strings.TrimPrefix(s, "\ufeff")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	s = strings.Trim(strings.Join(lines, "\n"), "\n")
	if s == "" {
		return ""
	}
	return s + "\n"
}

// licensesOf applies the gate to one component's files. It returns the licenses found in the gated files, sorted,
// and one problem per failed rule. exceptionFor reports whether the component may ship under a license that is not
// on the allowlist.
//
// The other license files (LICENSE-3RD-PARTY.md, LICENSE-GPL, COPYING.LESSER, ...) get neither the allowlist nor
// the coverage rule: they often list bundled material under other licenses (modernc.org/sqlite's
// LICENSE-3RD-PARTY.md names MPL-2.0, at 74% coverage) or are not license texts at all (its LICENSE-SQLITE). But a
// license that may never be shipped fails the gate there too, whatever the primary file says.
func licensesOf(files []licenseFile, exceptionFor func(id string) bool) (ids, problems []string) {
	gated := 0
	for _, f := range files {
		if f.notice {
			continue
		}
		if !f.gated {
			for _, id := range f.ids {
				if neverExcepted(id) {
					problems = append(problems, fmt.Sprintf("%s names %s, which may not be shipped", f.name, id))
				}
			}
			continue
		}
		gated++
		if len(f.ids) == 0 || f.coverage < minCoverage {
			problems = append(problems, fmt.Sprintf("%s is not a license text licensecheck recognizes (%.0f%% of it matches a known license, %d%% needed)",
				f.name, f.coverage, minCoverage))
		}
		for _, id := range f.ids {
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	if gated == 0 {
		problems = append(problems, "no LICENSE, LICENCE, COPYING or UNLICENSE file in the module root")
	}
	sort.Strings(ids)
	for _, id := range ids {
		switch {
		case allowlist[id]:
		case neverExcepted(id):
			problems = append(problems, fmt.Sprintf("%s may not be shipped", id))
		case exceptionFor(id):
		default:
			problems = append(problems, fmt.Sprintf("%s is not on the shipped allowlist: remove the dependency, or list it with a reason in deploy/notices/exceptions.md", id))
		}
	}
	return ids, problems
}

// allowlistString lists the allowlist for messages, sorted.
func allowlistString() string {
	ids := make([]string, 0, len(allowlist))
	for id := range allowlist {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return strings.Join(ids, ", ")
}

// exception is one row of deploy/notices/exceptions.md: a Go module that may ship under a license that is not on the
// allowlist, or (native) a native component of the desktop app, named as in native-windows.json, that ships under a
// LicenseRef-* license (docs/m2/06-windows-release.md §9.3).
type exception struct {
	module, license, reason string // module: the module path, or the native component's name
	native                  bool
	line                    int
	used                    bool
}

// parseExceptions reads the Markdown table rows of exceptions.md. A table whose header's first cell is "Module" holds
// Go modules (| `module/path` | License | Reason |); one whose first header cell is "Native component" holds native
// components (| `Name as in native-windows.json` | LicenseRef-... | Reason |). Rows before any header are module
// rows. Other lines (prose, headings) and the separator rows are skipped.
func parseExceptions(text string) ([]*exception, error) {
	var (
		list     []*exception
		problems []string
		seen     = map[string]int{}
		native   bool
	)
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for j := range cells {
			cells[j] = strings.TrimSpace(cells[j])
		}
		if kind, ok := headerRow(cells); ok {
			switch kind {
			case "module":
				native = false
			case "native":
				native = true
			}
			continue
		}
		n := i + 1
		if len(cells) != 3 {
			problems = append(problems, fmt.Sprintf("line %d: want 3 cells (| `module` | License | Reason |), got %d", n, len(cells)))
			continue
		}
		name := strings.Trim(cells[0], "`")
		e := &exception{module: name, license: cells[1], reason: cells[2], native: native, line: n}
		var problem string
		if native {
			problem = nativeExceptionProblem(e, cells[0])
		} else {
			problem = moduleExceptionProblem(e, cells[0])
		}
		if problem != "" {
			problems = append(problems, fmt.Sprintf("line %d: %s", n, problem))
			continue
		}
		key := fmt.Sprintf("%t %s %s", native, name, e.license)
		if prev, dup := seen[key]; dup {
			problems = append(problems, fmt.Sprintf("line %d: %s (%s) is already listed on line %d", n, name, e.license, prev))
			continue
		}
		seen[key] = n
		list = append(list, e)
	}
	if len(problems) != 0 {
		return nil, fmt.Errorf("%s", strings.Join(problems, "\n"))
	}
	return list, nil
}

// moduleExceptionProblem checks a Go module row; first is its first cell as written.
func moduleExceptionProblem(e *exception, first string) string {
	switch {
	case e.module == "" || strings.ContainsAny(e.module, " \t`"):
		return fmt.Sprintf("the first cell must be a module path in backquotes, got %q", first)
	case e.license == "":
		return fmt.Sprintf("%s has no license", e.module)
	case neverExcepted(e.license):
		return fmt.Sprintf("%s: %s may not be shipped, not even with an exception", e.module, e.license)
	case allowlist[e.license]:
		return fmt.Sprintf("%s: %s is on the allowlist and needs no exception", e.module, e.license)
	case e.reason == "":
		return fmt.Sprintf("%s: an exception needs a reason", e.module)
	}
	return ""
}

// nativeExceptionProblem checks a native component row: only a LicenseRef-* license is excepted (06 §9.3); the
// FFmpeg rule for LGPL-2.1-or-later is the gate's own, and every other license is an SPDX id the allowlist decides.
func nativeExceptionProblem(e *exception, first string) string {
	switch {
	case e.module == "" || strings.Contains(e.module, "`") || !strings.HasPrefix(first, "`") || !strings.HasSuffix(first, "`"):
		return fmt.Sprintf("the first cell must be a native component's name in backquotes, got %q", first)
	case !strings.HasPrefix(e.license, licenseRefPrefix) || e.license == licenseRefPrefix:
		return fmt.Sprintf("%s: only a %s* license can be excepted for a native component, not %q", e.module, licenseRefPrefix, e.license)
	case e.reason == "":
		return fmt.Sprintf("%s: an exception needs a reason", e.module)
	}
	return ""
}

// headerRow reports whether a table row is a header row (| Module | ... | or | Native component | ... |, kind "module"
// or "native") or a separator row (|---|---|, kind "").
func headerRow(cells []string) (kind string, ok bool) {
	if len(cells) > 0 {
		switch {
		case strings.EqualFold(cells[0], "module"):
			return "module", true
		case strings.EqualFold(cells[0], "native component"):
			return "native", true
		}
	}
	for _, c := range cells {
		if strings.Trim(c, ":-") != "" || c == "" {
			return "", false
		}
	}
	return "", true
}

// licenseLineRE matches the "License:" lines of extra.txt.
var licenseLineRE = regexp.MustCompile(`(?m)^[ \t]*License:[ \t]*(.*)$`)

// checkExtra applies the gate to extra.txt: every "License:" line names allowlisted licenses (comma-separated), and
// there is at least one. It returns the number of entries.
func checkExtra(text string) (int, []string) {
	var problems []string
	matches := licenseLineRE.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		problems = append(problems, `extra.txt: no entry has a "License:" line`)
	}
	for _, m := range matches {
		value := strings.TrimSpace(m[1])
		if value == "" {
			problems = append(problems, `extra.txt: an empty "License:" line`)
			continue
		}
		for id := range strings.SplitSeq(value, ",") {
			if id = strings.TrimSpace(id); !allowlist[id] {
				problems = append(problems, fmt.Sprintf("extra.txt: License: %s is not on the shipped allowlist (%s)", id, allowlistString()))
			}
		}
	}
	return len(matches), problems
}
