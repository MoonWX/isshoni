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
	// related material; they are reproduced, and classified only when a module has no primary file.
	primaryFileRE = regexp.MustCompile(`(?i)^((un)?licen[cs]e|copying)(\.(txt|md|markdown|rst))?$`)
)

// licenseFile is one reproduced file of a module's root directory.
type licenseFile struct {
	name     string   // file name, relative to the module root
	text     string   // normalized text
	notice   bool     // a NOTICE file
	gated    bool     // classified, and subject to the gate
	ids      []string // licenses found in it (gated files only)
	coverage float64  // percent of the text that matches known licenses (gated files only)
}

// readLicenseFiles reads and classifies the license and notice files in dir, license files first, each group
// sorted by name.
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
		if f.notice || (hasPrimary && !primaryFileRE.MatchString(f.name)) {
			continue
		}
		f.gated = true
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
func licensesOf(files []licenseFile, exceptionFor func(id string) bool) (ids, problems []string) {
	gated := 0
	for _, f := range files {
		if !f.gated {
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

// exception is one row of deploy/notices/exceptions.md: a module that may ship under a license that is not on the
// allowlist.
type exception struct {
	module, license, reason string
	line                    int
	used                    bool
}

// parseExceptions reads the Markdown table rows of exceptions.md: | `module/path` | License | Reason |. Other lines
// (prose, headings) and the table's header and separator rows are skipped.
func parseExceptions(text string) ([]*exception, error) {
	var (
		list     []*exception
		problems []string
		seen     = map[string]int{}
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
		if isHeaderRow(cells) {
			continue
		}
		n := i + 1
		if len(cells) != 3 {
			problems = append(problems, fmt.Sprintf("line %d: want 3 cells (| `module` | License | Reason |), got %d", n, len(cells)))
			continue
		}
		module := strings.Trim(cells[0], "`")
		e := &exception{module: module, license: cells[1], reason: cells[2], line: n}
		switch {
		case module == "" || strings.ContainsAny(module, " \t`"):
			problems = append(problems, fmt.Sprintf("line %d: the first cell must be a module path in backquotes, got %q", n, cells[0]))
		case e.license == "":
			problems = append(problems, fmt.Sprintf("line %d: %s has no license", n, module))
		case neverExcepted(e.license):
			problems = append(problems, fmt.Sprintf("line %d: %s: %s may not be shipped, not even with an exception", n, module, e.license))
		case allowlist[e.license]:
			problems = append(problems, fmt.Sprintf("line %d: %s: %s is on the allowlist and needs no exception", n, module, e.license))
		case e.reason == "":
			problems = append(problems, fmt.Sprintf("line %d: %s: an exception needs a reason", n, module))
		default:
			key := module + " " + e.license
			if prev, dup := seen[key]; dup {
				problems = append(problems, fmt.Sprintf("line %d: %s (%s) is already listed on line %d", n, module, e.license, prev))
				continue
			}
			seen[key] = n
			list = append(list, e)
		}
	}
	if len(problems) != 0 {
		return nil, fmt.Errorf("%s", strings.Join(problems, "\n"))
	}
	return list, nil
}

// isHeaderRow reports whether a table row is a header (| Module | License | Reason |) or separator (|---|---|) row.
func isHeaderRow(cells []string) bool {
	if len(cells) > 0 && strings.EqualFold(cells[0], "module") {
		return true
	}
	for _, c := range cells {
		if strings.Trim(c, ":-") != "" || c == "" {
			return false
		}
	}
	return true
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
