package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
}

// checkPins compares .tool-versions with go.mod and tools/go.mod. It reports every difference at once.
func checkPins(root string) error {
	pins, err := readToolVersions(filepath.Join(root, ".tool-versions"))
	if err != nil {
		return err
	}
	mainMod, err := readModFile(root, "go.mod")
	if err != nil {
		return err
	}
	toolsMod, err := readModFile(root, filepath.Join("tools", "go.mod"))
	if err != nil {
		return err
	}

	var problems []string
	if goPin, ok := pins["golang"]; !ok {
		problems = append(problems, ".tool-versions has no golang line")
	} else {
		want := "go" + goPin
		for _, m := range []struct {
			name string
			file *modfile.File
		}{{"go.mod", mainMod}, {"tools/go.mod", toolsMod}} {
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
	if len(problems) > 0 {
		return fmt.Errorf("the pins differ:\n  - %s\nChange .tool-versions, go.mod and tools/go.mod together", strings.Join(problems, "\n  - "))
	}
	return nil
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
