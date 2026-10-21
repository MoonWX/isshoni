package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"
)

// pinnedTools are the .tool-versions entries that tools/go.mod also builds into .bin/. Both pins must name the same
// version, so a contributor using mise and one using `task tools` run the same binaries.
var pinnedTools = []struct {
	name   string // name in .tool-versions
	module string // module path in tools/go.mod
}{
	{name: "task", module: "github.com/go-task/task/v3"},
	{name: "golangci-lint", module: "github.com/golangci/golangci-lint/v2"},
	{name: "shfmt", module: "mvdan.cc/sh/v3"},
}

// goreleaser is built into .bin/ from a module of its own, tools/goreleaser/go.mod (`task release:goreleaser`): its
// go directive is newer than the repository's, and its dependency graph would raise versions the other tools share.
// That module's toolchain line and its goreleaser version follow .tool-versions like the others.
const (
	goreleaserPin    = "goreleaser"
	goreleaserModule = "github.com/goreleaser/goreleaser/v2"
)

var goreleaserModFile = filepath.Join("tools", "goreleaser", "go.mod")

// Wails (docs/m2/06-windows-release.md §3 and D19, 03-desktop-app.md §3.1): the library in go.mod, the wails3 CLI in a
// module of its own, tools/wails3/go.mod (`task release:wails3`, kept out of tools/go.mod like goreleaser), and the
// npm runtime @wailsio/runtime in web/package.json and web/package-lock.json share one version and move together, by
// hand. npm's versions follow the Go tags one to one, so the runtime's is the Go module version without the "v"; no
// file records it separately. Dependabot ignores the library and the runtime (.github/dependabot.yml).
const (
	wailsModule  = "github.com/wailsapp/wails/v3"
	wailsRuntime = "@wailsio/runtime"
)

var (
	wailsModFile   = filepath.Join("tools", "wails3", "go.mod")
	webPackageJSON = filepath.Join("web", "package.json")
	webPackageLock = filepath.Join("web", "package-lock.json")
)

// The MSVC toolset (docs/m2/06-windows-release.md §3): deploy/windows/tools.json's "msvc".toolset is the one
// deploy/windows/msvc-env.ps1 sets up for the project's own build, and the overlay triplet native/triplets/
// x64-windows-static.cmake gives vcpkg the same one (VCPKG_PLATFORM_TOOLSET_VERSION) for the ports it builds; a port
// built with a newer toolset than the project's fails to link into it.
var (
	windowsToolsJSON = filepath.Join("deploy", "windows", "tools.json")
	vcpkgTriplet     = filepath.Join("native", "triplets", "x64-windows-static.cmake")
)

var tripletToolset = regexp.MustCompile(`(?m)^[ \t]*set\(VCPKG_PLATFORM_TOOLSET_VERSION[ \t]+"?([0-9]+\.[0-9]+)"?[ \t]*\)`)

// exactVersion is a plain SemVer 2.0.0 version, the only form of a dependency in package.json that names one version
// (npm install --save-exact writes it). Ranges (^, ~, >=, x, *, ||), tags (latest), "v" or "=" prefixes, URLs and
// npm: aliases do not match.
var exactVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)` +
	`(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?` +
	`(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)

// checkPins compares .tool-versions with go.mod, tools/go.mod, tools/goreleaser/go.mod and tools/wails3/go.mod, and
// the Wails pins with each other (checkWails). It reports every difference at once. The note says what was not
// checked yet: the Wails library and runtime while go.mod does not require Wails.
func checkPins(root string) (note string, err error) {
	pins, err := readToolVersions(filepath.Join(root, ".tool-versions"))
	if err != nil {
		return "", err
	}
	mainMod, err := readModFile(root, "go.mod")
	if err != nil {
		return "", err
	}
	toolsMod, err := readModFile(root, filepath.Join("tools", "go.mod"))
	if err != nil {
		return "", err
	}
	goreleaserMod, err := readModFile(root, goreleaserModFile)
	if err != nil {
		return "", err
	}
	wailsMod, err := readModFile(root, wailsModFile)
	if err != nil {
		return "", err
	}

	var problems []string
	if goPin, ok := pins["golang"]; !ok {
		problems = append(problems, ".tool-versions has no golang line")
	} else {
		want := "go" + goPin
		for _, m := range []struct {
			name string
			file *modfile.File
		}{
			{"go.mod", mainMod},
			{"tools/go.mod", toolsMod},
			{"tools/goreleaser/go.mod", goreleaserMod},
			{"tools/wails3/go.mod", wailsMod},
		} {
			switch got := toolchain(m.file); got {
			case want:
			case "":
				problems = append(problems, fmt.Sprintf("%s has no toolchain directive; want \"toolchain %s\" (.tool-versions: golang %s)", m.name, want, goPin))
			default:
				problems = append(problems, fmt.Sprintf("%s has \"toolchain %s\" but .tool-versions pins golang %s", m.name, got, goPin))
			}
		}
	}
	for _, t := range pinnedTools {
		pin, ok := pins[t.name]
		if !ok {
			problems = append(problems, fmt.Sprintf(".tool-versions has no %s line", t.name))
			continue
		}
		switch got := requiredVersion(toolsMod, t.module); got {
		case "v" + pin:
		case "":
			problems = append(problems, fmt.Sprintf("tools/go.mod does not require %s (.tool-versions: %s %s)", t.module, t.name, pin))
		default:
			problems = append(problems, fmt.Sprintf("tools/go.mod has %s %s but .tool-versions pins %s %s", t.module, got, t.name, pin))
		}
	}
	if pin, ok := pins[goreleaserPin]; !ok {
		problems = append(problems, ".tool-versions has no goreleaser line")
	} else {
		switch got := requiredVersion(goreleaserMod, goreleaserModule); got {
		case "v" + pin:
		case "":
			problems = append(problems, fmt.Sprintf("tools/goreleaser/go.mod does not require %s (.tool-versions: goreleaser %s)", goreleaserModule, pin))
		default:
			problems = append(problems, fmt.Sprintf("tools/goreleaser/go.mod has %s %s but .tool-versions pins goreleaser %s", goreleaserModule, got, pin))
		}
	}
	note, wailsProblems, err := checkWails(root, mainMod, wailsMod)
	if err != nil {
		return "", err
	}
	problems = append(problems, wailsProblems...)
	toolsetProblems, err := checkMSVCToolset(root)
	if err != nil {
		return "", err
	}
	problems = append(problems, toolsetProblems...)
	if len(problems) > 0 {
		return "", fmt.Errorf("the pins differ:\n  - %s\nChange .tool-versions, go.mod, tools/go.mod, tools/goreleaser/go.mod and "+
			"tools/wails3/go.mod together; Wails moves in go.mod, tools/wails3/go.mod, web/package.json and "+
			"web/package-lock.json at once, and the MSVC toolset in deploy/windows/tools.json and "+
			"native/triplets/x64-windows-static.cmake (docs/m2/06-windows-release.md §3)", strings.Join(problems, "\n  - "))
	}
	return note, nil
}

// checkWails returns the differences between the Wails pins (see wailsModule):
//   - tools/wails3/go.mod requires the Wails module, at go.mod's version once go.mod requires it;
//   - once go.mod requires Wails, web/package.json lists @wailsio/runtime as exactly that version without the "v" (no
//     range or tag), and web/package-lock.json's root entry lists the same and installs only that version;
//   - while go.mod does not require Wails, web/package.json does not list @wailsio/runtime either.
//
// While go.mod does not require Wails (before the desktop app lands), only the CLI's pin is checked, and the note
// says so. It fails only when a file can't be read or parsed.
func checkWails(root string, mainMod, cliMod *modfile.File) (note string, problems []string, err error) {
	lib := requiredVersion(mainMod, wailsModule)
	cli := requiredVersion(cliMod, wailsModule)
	switch {
	case cli == "":
		problems = append(problems, fmt.Sprintf("tools/wails3/go.mod does not require %s (the wails3 CLI)", wailsModule))
	case lib != "" && cli != lib:
		problems = append(problems, fmt.Sprintf("tools/wails3/go.mod has %s %s but go.mod has %s", wailsModule, cli, lib))
	}

	pkgPath := filepath.Join(root, webPackageJSON)
	havePkg, err := exists(pkgPath)
	if err != nil {
		return "", nil, err
	}
	if !havePkg {
		if lib == "" {
			return "note: go.mod does not require " + wailsModule + " yet, so only tools/wails3/go.mod's Wails pin was checked", problems, nil
		}
		return "note: web/package.json does not exist, so " + wailsRuntime + " was not checked", problems, nil
	}
	specs, err := readRuntimeSpecs(pkgPath)
	if err != nil {
		return "", nil, err
	}

	if lib == "" {
		for _, s := range specs {
			problems = append(problems, fmt.Sprintf("web/package.json has %s %q in %s, but go.mod does not require %s", wailsRuntime, s.spec, s.section, wailsModule))
		}
		return "note: go.mod does not require " + wailsModule + " yet, so only tools/wails3/go.mod's Wails pin was checked", problems, nil
	}

	want := strings.TrimPrefix(lib, "v")
	if len(specs) == 0 {
		problems = append(problems, fmt.Sprintf("web/package.json does not list %s; go.mod has %s %s, so it must be exactly %q", wailsRuntime, wailsModule, lib, want))
		return "", problems, nil
	}
	for _, s := range specs {
		if p := specProblem("web/package.json", s, want, lib); p != "" {
			problems = append(problems, p)
		}
	}

	lockProblems, err := checkRuntimeLock(filepath.Join(root, webPackageLock), want, lib)
	if err != nil {
		return "", nil, err
	}
	return "", append(problems, lockProblems...), nil
}

// runtimeSpec is how a package.json (or the root entry of a lockfile) lists @wailsio/runtime.
type runtimeSpec struct {
	section string // dependencies, devDependencies, ...
	spec    string
}

// depSections are the sections of a package.json, and of a lockfile's root entry, that can list the runtime.
type depSections struct {
	Dependencies         map[string]string `json:"dependencies"`
	DevDependencies      map[string]string `json:"devDependencies"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
	PeerDependencies     map[string]string `json:"peerDependencies"`
}

func (d depSections) runtimeSpecs() []runtimeSpec {
	var specs []runtimeSpec
	for _, s := range []struct {
		name string
		deps map[string]string
	}{
		{"dependencies", d.Dependencies},
		{"devDependencies", d.DevDependencies},
		{"optionalDependencies", d.OptionalDependencies},
		{"peerDependencies", d.PeerDependencies},
	} {
		if spec, ok := s.deps[wailsRuntime]; ok {
			specs = append(specs, runtimeSpec{section: s.name, spec: spec})
		}
	}
	return specs
}

func readRuntimeSpecs(path string) ([]runtimeSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var pkg depSections
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil, fmt.Errorf("%s: %w", webPackageJSON, err)
	}
	return pkg.runtimeSpecs(), nil
}

// specProblem describes what is wrong with one listing of the runtime in file, or returns "".
func specProblem(file string, s runtimeSpec, want, lib string) string {
	switch {
	case s.spec == want:
		return ""
	case exactVersion.MatchString(s.spec):
		return fmt.Sprintf("%s has %s %q in %s but go.mod has %s %s, so it must be %q", file, wailsRuntime, s.spec, s.section, wailsModule, lib, want)
	default:
		return fmt.Sprintf("%s has %s %q in %s, which is a range or a tag, not an exact version; it must be %q (go.mod: %s %s)", file, wailsRuntime, s.spec, s.section, want, wailsModule, lib)
	}
}

// checkRuntimeLock checks web/package-lock.json (lockfileVersion 2 or 3, which npm 7 and newer write): its root
// entry lists the runtime as want, and every installed copy (node_modules/@wailsio/runtime, nested ones included) is
// version want, so exactly one runtime version is bundled.
func checkRuntimeLock(path, want, lib string) ([]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return []string{"web/package-lock.json does not exist; run npm install in web/ and commit it"}, nil
	}
	if err != nil {
		return nil, err
	}
	var lock struct {
		LockfileVersion int `json:"lockfileVersion"`
		Packages        map[string]struct {
			depSections
			Version string `json:"version"`
			Link    bool   `json:"link"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		return nil, fmt.Errorf("%s: %w", webPackageLock, err)
	}
	if lock.Packages == nil {
		return []string{fmt.Sprintf("web/package-lock.json has no \"packages\" (lockfileVersion %d); regenerate it with npm 7 or newer", lock.LockfileVersion)}, nil
	}

	var problems []string
	rootSpecs := lock.Packages[""].runtimeSpecs()
	if len(rootSpecs) == 0 {
		problems = append(problems, fmt.Sprintf("web/package-lock.json's root entry does not list %s; run npm install in web/ and commit the lockfile", wailsRuntime))
	}
	for _, s := range rootSpecs {
		if p := specProblem("web/package-lock.json's root entry", s, want, lib); p != "" {
			problems = append(problems, p)
		}
	}

	const installed = "node_modules/" + wailsRuntime
	var keys []string
	for key, entry := range lock.Packages {
		if !entry.Link && (key == installed || strings.HasSuffix(key, "/"+installed)) {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	if !slices.Contains(keys, installed) {
		problems = append(problems, fmt.Sprintf("web/package-lock.json installs no %s; run npm install in web/ and commit the lockfile", installed))
	}
	for _, key := range keys {
		if got := lock.Packages[key].Version; got != want {
			problems = append(problems, fmt.Sprintf("web/package-lock.json installs %s %q at %s but go.mod has %s %s, so it must be %q", wailsRuntime, got, key, wailsModule, lib, want))
		}
	}
	return problems, nil
}

// checkMSVCToolset returns the differences between deploy/windows/tools.json's MSVC toolset and the overlay
// triplet's (see vcpkgTriplet). A tree without tools.json (before the Windows app) is not checked; once it exists, the
// triplet must exist and set VCPKG_PLATFORM_TOOLSET_VERSION to the same major.minor. It fails only when tools.json
// can't be read or parsed.
func checkMSVCToolset(root string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(root, windowsToolsJSON))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var tools struct {
		MSVC struct {
			Toolset string `json:"toolset"`
		} `json:"msvc"`
	}
	if err := json.Unmarshal(data, &tools); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.ToSlash(windowsToolsJSON), err)
	}
	want := tools.MSVC.Toolset
	if want == "" {
		return []string{"deploy/windows/tools.json has no \"msvc\".toolset"}, nil
	}
	triplet, err := os.ReadFile(filepath.Join(root, vcpkgTriplet))
	if errors.Is(err, os.ErrNotExist) {
		return []string{fmt.Sprintf("native/triplets/x64-windows-static.cmake does not exist; it must set "+
			"VCPKG_PLATFORM_TOOLSET_VERSION %s (deploy/windows/tools.json's MSVC toolset)", want)}, nil
	} else if err != nil {
		return nil, err
	}
	m := tripletToolset.FindSubmatch(triplet)
	switch {
	case m == nil:
		return []string{fmt.Sprintf("native/triplets/x64-windows-static.cmake does not set VCPKG_PLATFORM_TOOLSET_VERSION; "+
			"want %s (deploy/windows/tools.json's MSVC toolset)", want)}, nil
	case string(m[1]) != want:
		return []string{fmt.Sprintf("native/triplets/x64-windows-static.cmake sets VCPKG_PLATFORM_TOOLSET_VERSION %s but "+
			"deploy/windows/tools.json's MSVC toolset is %s", m[1], want)}, nil
	}
	return nil, nil
}

// readToolVersions parses an asdf/mise .tool-versions file into tool name → first listed version.
func readToolVersions(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pins := make(map[string]string)
	for i, line := range strings.Split(string(data), "\n") {
		if j := strings.IndexByte(line, '#'); j >= 0 {
			line = line[:j]
		}
		fields := strings.Fields(line)
		switch {
		case len(fields) == 0:
			continue
		case len(fields) == 1:
			return nil, fmt.Errorf("%s:%d: %q has no version", path, i+1, fields[0])
		}
		if _, dup := pins[fields[0]]; dup {
			return nil, fmt.Errorf("%s:%d: %q is listed twice", path, i+1, fields[0])
		}
		pins[fields[0]] = fields[1]
	}
	return pins, nil
}

func readModFile(root, name string) (*modfile.File, error) {
	path := filepath.Join(root, name)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, err := modfile.Parse(path, data, nil)
	if err != nil {
		return nil, err
	}
	if f.Module == nil {
		return nil, errors.New(name + " has no module directive")
	}
	return f, nil
}

func toolchain(f *modfile.File) string {
	if f.Toolchain == nil {
		return ""
	}
	return f.Toolchain.Name
}

func requiredVersion(f *modfile.File, module string) string {
	for _, r := range f.Require {
		if r.Mod.Path == module {
			return r.Mod.Version
		}
	}
	return ""
}
