package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// The native section of the desktop notices and its gate (docs/m2/06-windows-release.md §9.2–§9.3, "M2 06" below).
//
// -native names deploy/notices/native-windows.json, one entry per native component of the Windows app: the static
// libraries inside isshoni-media.dll, the FFmpeg DLLs and what is built into them, and the WebView2 bootstrapper the
// installer carries. -native-root is the windows-native artifact's release\ folder, whose licenses/ holds the
// license files the entries name. A licenseFiles path is relative to that licenses/ folder; one that starts with ../
// names a file of this repository and is relative to the tools module (<root>/tools), as the command line's own
// paths are under `go -C tools`.
//
// The gate fails the build unless, for every entry:
//   - its license is on the shipped allowlist; or it is LGPL-2.1-or-later with "linking": "dynamic", the entry is
//     FFmpeg's (its files are the avcodec-*.dll and avutil-*.dll globs) and the LGPL compliance files of M2 06 §9.1
//     are in -native-root's licenses/ffmpeg/ (the stage copies them from there; desktoppkg stage checks them
//     again); or it is a LicenseRef-* that deploy/notices/exceptions.md lists, with a reason, in its "Native
//     component" table. Any other GPL, AGPL, LGPL, SSPL or BUSL license fails, and so does any other license;
//   - every license file it names exists and is not empty;
//
// and unless the vcpkg ports that have a licenses/vcpkg/<port>/copyright in -native-root (the release build tree's
// share/<port>/copyright) are exactly the entries' vcpkgPort values: a port that is shipped but not listed, or
// listed but gone, fails. M2 06 §9.3's other two checks run elsewhere: the FFmpeg build's ffmpeg-config.json passes
// §4.4's LGPL checks in desktoppkg ffmpeg-fetch (windows-native), and the staged DLLs are lock.json's by SHA-256
// (desktoppkg stage); desktoppkg check matches every binary of the stage against the entries' files globs.

const (
	// lgplID is the one LGPL license the gate accepts, for FFmpeg only, linked dynamically (the plan, M2 06 §9.1).
	lgplID = "LGPL-2.1-or-later"
	// licenseRefPrefix marks a license that is not an SPDX id; such an entry needs an exception.
	licenseRefPrefix = "LicenseRef-"
	// nativeManifestName is the windows-native artifact's build record (M2 06 §5.1); the vcpkg ports' versions come
	// from it when it is there.
	nativeManifestName = "native-manifest.json"
)

// lgplComplianceFiles are the files of M2 06 §9.1 in -native-root's licenses/ffmpeg/.
var lgplComplianceFiles = []string{"COPYING.LGPLv2.1", "LICENSE.md", "README-LGPL.txt"}

// ffmpegGlobs are the files globs of the FFmpeg entry, the only one that may be LGPL.
var ffmpegGlobs = []string{"avcodec-*.dll", "avutil-*.dll"}

// nativeEntry is one entry of native-windows.json (M2 06 §9.2).
type nativeEntry struct {
	Name         string   `json:"name"`
	Version      string   `json:"version,omitempty"`
	VcpkgPort    string   `json:"vcpkgPort,omitempty"`
	License      string   `json:"license"`
	Linking      string   `json:"linking"`
	Files        []string `json:"files,omitempty"`
	Source       string   `json:"source,omitempty"`
	LicenseFiles []string `json:"licenseFiles"`
}

// nativeComponent is one entry of the native section, with its license files read.
type nativeComponent struct {
	entry       nativeEntry
	portVersion string        // the vcpkg port's version from native-manifest.json, when known
	files       []licenseFile // name: the path shown in the output
}

// loadNative reads -native and -native-root and applies the native gate. It returns the components sorted by name
// and every problem found; err is for an input that can't be read at all.
func loadNative(cfg config, exceptions []*exception) ([]nativeComponent, []string, error) {
	data, err := os.ReadFile(cfg.nativePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("%s is missing: the native components (deploy/notices/native-windows.json)", cfg.nativePath)
	} else if err != nil {
		return nil, nil, err
	}
	entries, err := parseNative(data)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", cfg.nativePath, err)
	}
	if fi, err := os.Stat(cfg.nativeRoot); err != nil || !fi.IsDir() {
		return nil, nil, fmt.Errorf("-native-root %s is not a folder: it is the windows-native artifact's release folder", cfg.nativeRoot)
	}
	ports, err := readPortVersions(cfg.nativeRoot)
	if err != nil {
		return nil, nil, err
	}

	label := filepath.Base(cfg.nativePath)
	var problems []string
	comps := make([]nativeComponent, 0, len(entries))
	for _, e := range entries {
		var p []string
		p = append(p, entryProblems(e)...)
		p = append(p, licenseProblems(e, cfg.nativeRoot, exceptions)...)
		c := nativeComponent{entry: e, portVersion: ports[e.VcpkgPort]}
		files, fp := readNativeLicenseFiles(e, cfg)
		c.files = files
		p = append(p, fp...)
		for _, s := range p {
			problems = append(problems, fmt.Sprintf("%s: %s: %s", label, e.Name, s))
		}
		comps = append(comps, c)
	}
	for _, s := range vcpkgProblems(entries, cfg.nativeRoot) {
		problems = append(problems, fmt.Sprintf("%s: %s", label, s))
	}
	sort.SliceStable(comps, func(i, j int) bool {
		return strings.ToLower(comps[i].entry.Name) < strings.ToLower(comps[j].entry.Name)
	})
	return comps, problems, nil
}

// parseNative decodes native-windows.json strictly: unknown fields, a second value and duplicate names fail.
func parseNative(data []byte) ([]nativeEntry, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var entries []nativeEntry
	if err := dec.Decode(&entries); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("more than one JSON value")
	}
	if len(entries) == 0 {
		return nil, errors.New("no entries")
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if seen[e.Name] {
			return nil, fmt.Errorf("%q is listed twice", e.Name)
		}
		seen[e.Name] = true
	}
	return entries, nil
}

// entryProblems checks an entry's fields.
func entryProblems(e nativeEntry) []string {
	var p []string
	if strings.TrimSpace(e.Name) == "" || strings.Contains(e.Name, "`") {
		p = append(p, "an entry needs a name (without backquotes)")
	}
	if e.License == "" || strings.ContainsAny(e.License, " \t") {
		p = append(p, "license must be one SPDX id or LicenseRef-*")
	}
	if strings.TrimSpace(e.Linking) == "" {
		p = append(p, `"linking" says how it is shipped (dynamic, static inside ..., headers inside ...)`)
	}
	if len(e.LicenseFiles) == 0 {
		p = append(p, "no licenseFiles")
	}
	for _, g := range e.Files {
		if _, err := path.Match(g, ""); err != nil || g == "" || strings.ContainsAny(g, `/\`) {
			p = append(p, fmt.Sprintf("files: %q is not a file name glob", g))
		}
	}
	if e.VcpkgPort != "" && !slices.Contains(e.LicenseFiles, "vcpkg/"+e.VcpkgPort+"/copyright") {
		p = append(p, fmt.Sprintf("a vcpkg port's licenseFiles include vcpkg/%s/copyright", e.VcpkgPort))
	}
	return p
}

// licenseProblems applies M2 06 §9.3's license rule to one entry.
func licenseProblems(e nativeEntry, nativeRoot string, exceptions []*exception) []string {
	id := e.License
	switch {
	case id == "":
		return nil // entryProblems reports it
	case allowlist[id]:
		return nil
	case id == lgplID:
		var p []string
		if e.Linking != "dynamic" {
			p = append(p, fmt.Sprintf(`%s is accepted only with "linking": "dynamic", not %q`, lgplID, e.Linking))
		}
		if !slices.Equal(sortedCopy(e.Files), ffmpegGlobs) {
			p = append(p, fmt.Sprintf("%s is accepted only for the FFmpeg DLLs (files %s), not %q", lgplID,
				strings.Join(ffmpegGlobs, ", "), e.Files))
		}
		for _, name := range lgplComplianceFiles {
			if fi, err := os.Stat(filepath.Join(nativeRoot, "licenses", "ffmpeg", name)); err != nil || !fi.Mode().IsRegular() {
				p = append(p, fmt.Sprintf("%s needs the LGPL compliance file licenses/ffmpeg/%s in -native-root (M2 06 §9.1)", lgplID, name))
			}
		}
		return p
	case neverExcepted(id):
		return []string{fmt.Sprintf("%s may not be shipped", id)}
	case strings.HasPrefix(id, licenseRefPrefix):
		for _, ex := range exceptions {
			if ex.native && ex.module == e.Name && ex.license == id {
				ex.used = true
				return nil
			}
		}
		return []string{fmt.Sprintf("%s needs a row with a reason in deploy/notices/exceptions.md's Native component table", id)}
	default:
		return []string{fmt.Sprintf("%s is not on the shipped allowlist", id)}
	}
}

func sortedCopy(s []string) []string {
	c := slices.Clone(s)
	slices.Sort(c)
	return c
}

// readNativeLicenseFiles reads an entry's license files, in its order, with the names the output shows:
// licenses/<path> for the artifact's files, the repository path for ../ files.
func readNativeLicenseFiles(e nativeEntry, cfg config) ([]licenseFile, []string) {
	var (
		files    []licenseFile
		problems []string
	)
	for _, rel := range e.LicenseFiles {
		file, shown, err := resolveLicenseFile(rel, cfg)
		if err != nil {
			problems = append(problems, fmt.Sprintf("licenseFiles: %v", err))
			continue
		}
		data, err := os.ReadFile(file) //nolint:gosec // G304: a path checked to stay inside -native-root or -root
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				problems = append(problems, fmt.Sprintf("licenseFiles: %s is missing", shown))
			} else {
				problems = append(problems, fmt.Sprintf("licenseFiles: %s: %v", shown, err))
			}
			continue
		}
		text := normalize(string(data))
		if text == "" {
			problems = append(problems, fmt.Sprintf("licenseFiles: %s is empty", shown))
			continue
		}
		files = append(files, licenseFile{name: shown, text: text})
	}
	return files, problems
}

// resolveLicenseFile maps a licenseFiles path to a file and the name shown for it. A path must be relative, with /
// separators, and stay inside its folder: -native-root's licenses/, or the repository for ../ paths.
func resolveLicenseFile(rel string, cfg config) (file, shown string, err error) {
	if rel == "" || strings.Contains(rel, `\`) || path.IsAbs(rel) || filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" {
		return "", "", fmt.Errorf("%q is not a relative path with / separators", rel)
	}
	if strings.HasPrefix(rel, "../") {
		repoRel := path.Clean(path.Join("tools", rel))
		if repoRel == "." || strings.HasPrefix(repoRel, "../") || repoRel == ".." {
			return "", "", fmt.Errorf("%q leaves the repository", rel)
		}
		return filepath.Join(cfg.root, filepath.FromSlash(repoRel)), repoRel, nil
	}
	clean := path.Clean(rel)
	if clean != rel || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", "", fmt.Errorf("%q is not a clean path inside -native-root's licenses folder", rel)
	}
	return filepath.Join(cfg.nativeRoot, "licenses", filepath.FromSlash(clean)), "licenses/" + clean, nil
}

// vcpkgProblems compares the vcpkg ports that the artifact carries a copyright file for with the entries' ports.
func vcpkgProblems(entries []nativeEntry, nativeRoot string) []string {
	shipped := map[string]bool{}
	dirs, err := os.ReadDir(filepath.Join(nativeRoot, "licenses", "vcpkg"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return []string{fmt.Sprintf("the vcpkg ports of -native-root: %v", err)}
	}
	for _, d := range dirs {
		if fi, err := os.Stat(filepath.Join(nativeRoot, "licenses", "vcpkg", d.Name(), "copyright")); err == nil && fi.Mode().IsRegular() {
			shipped[d.Name()] = true
		}
	}
	listed := map[string]bool{}
	for _, e := range entries {
		if e.VcpkgPort != "" {
			listed[e.VcpkgPort] = true
		}
	}
	var p []string
	for _, port := range sortedKeys(shipped) {
		if !listed[port] {
			p = append(p, fmt.Sprintf("the vcpkg port %s is shipped (licenses/vcpkg/%s/copyright) but no entry lists it: add its entry", port, port))
		}
	}
	for _, port := range sortedKeys(listed) {
		if !shipped[port] {
			p = append(p, fmt.Sprintf("the vcpkg port %s is listed but not shipped (no licenses/vcpkg/%s/copyright in -native-root): remove its entry", port, port))
		}
	}
	return p
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// readPortVersions reads the vcpkg ports' versions from -native-root's native-manifest.json; without the file
// (a hand-made native folder) there are none.
func readPortVersions(nativeRoot string) (map[string]string, error) {
	data, err := os.ReadFile(filepath.Join(nativeRoot, nativeManifestName))
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	} else if err != nil {
		return nil, err
	}
	var m struct {
		Vcpkg struct {
			Ports map[string]string `json:"ports"`
		} `json:"vcpkg"`
	}
	if err := json.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &m); err != nil {
		return nil, fmt.Errorf("-native-root's %s: %w", nativeManifestName, err)
	}
	if m.Vcpkg.Ports == nil {
		return map[string]string{}, nil
	}
	return m.Vcpkg.Ports, nil
}

// nativeEntryText writes one entry of the native section.
func nativeEntryText(b *bytes.Buffer, c nativeComponent) {
	e := c.entry
	title := e.Name
	if e.Version != "" {
		title += " " + e.Version
	}
	if e.VcpkgPort != "" {
		port := "vcpkg port " + e.VcpkgPort
		if c.portVersion != "" {
			port += " " + c.portVersion
		}
		title += " (" + port + ")"
	}
	fmt.Fprintf(b, "\n%s\n%s\nLicense: %s\nLinking: %s\n", entryRule, title, e.License, e.Linking)
	if len(e.Files) > 0 {
		fmt.Fprintf(b, "Files: %s\n", strings.Join(e.Files, ", "))
	}
	if e.Source != "" {
		fmt.Fprintf(b, "Source: %s\n", e.Source)
	}
	if e.License == lgplID {
		b.WriteString("The LGPL notices, where to get the complete corresponding source and how to replace these DLLs:\n" +
			"licenses\\ffmpeg\\README-LGPL.txt in the install folder.\n")
	}
	b.WriteString(entryRule + "\n")
	for _, f := range c.files {
		fmt.Fprintf(b, "\n== %s ==\n\n", f.name)
		b.WriteString(f.text)
	}
}
