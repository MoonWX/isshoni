package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// goCmdFunc runs the go command in dir, with env added to the process environment, and returns its standard output.
// Tests wrap runGo to fake `go env`.
type goCmdFunc func(ctx context.Context, dir string, env []string, args ...string) ([]byte, error)

// runGo runs the go command found in PATH. In dir, the go.mod toolchain line selects the same toolchain that builds
// the binary (GOTOOLCHAIN=auto).
func runGo(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return nil, fmt.Errorf("go %s: %w", strings.Join(args, " "), err)
		}
		return nil, fmt.Errorf("go %s: %w\n%s", strings.Join(args, " "), err, msg)
	}
	return out, nil
}

// goModule is the part of `go list -m -json` output that notices reads.
type goModule struct {
	Path    string
	Version string
	Dir     string
	Main    bool
	Replace *goModule
	Error   *struct{ Err string }
}

// linkedModule is a module whose packages are linked into the binary for at least one GOOS/GOARCH pair.
type linkedModule struct {
	path, version string
	dir           string    // the module root on disk (the replacement's, when replaced)
	replace       *goModule // nil unless go.mod replaces the module
}

// listLinkedModules returns the modules of the packages pkgs depend on, for every GOOS and GOARCH pair, minus the
// main module, sorted by path. Builds are CGO_ENABLED=0, as the shipped binary is (06 §7.2).
func listLinkedModules(ctx context.Context, goCmd goCmdFunc, root string, pkgs, goos, goarch []string) ([]linkedModule, error) {
	const format = `{{with .Module}}{{if not .Main}}{{.Path}}{{"\t"}}{{.Version}}{{end}}{{end}}`
	versions := map[string]string{}
	for _, targetOS := range goos {
		for _, arch := range goarch {
			env := []string{"GOOS=" + targetOS, "GOARCH=" + arch, "CGO_ENABLED=0", "GOWORK=off"}
			args := append([]string{"list", "-deps", "-f", format}, pkgs...)
			out, err := goCmd(ctx, root, env, args...)
			if err != nil {
				return nil, fmt.Errorf("listing the dependencies for %s/%s: %w", targetOS, arch, err)
			}
			sc := bufio.NewScanner(bytes.NewReader(out))
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				if line == "" {
					continue // a standard library package
				}
				path, version, _ := strings.Cut(line, "\t")
				versions[path] = version
			}
			if err := sc.Err(); err != nil {
				return nil, err
			}
		}
	}
	if len(versions) == 0 {
		return nil, nil
	}

	paths := make([]string, 0, len(versions))
	for p := range versions {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	out, err := goCmd(ctx, root, []string{"GOWORK=off"}, append([]string{"list", "-m", "-json"}, paths...)...)
	if err != nil {
		return nil, fmt.Errorf("finding the module directories: %w", err)
	}
	byPath := map[string]goModule{}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var m goModule
		if err := dec.Decode(&m); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("reading go list -m -json: %w", err)
		}
		byPath[m.Path] = m
	}

	mods := make([]linkedModule, 0, len(paths))
	for _, p := range paths {
		m, ok := byPath[p]
		switch {
		case !ok:
			return nil, fmt.Errorf("go list -m -json did not report module %s", p)
		case m.Error != nil:
			return nil, fmt.Errorf("module %s: %s", p, m.Error.Err)
		}
		dir := m.Dir
		if m.Replace != nil && m.Replace.Dir != "" {
			dir = m.Replace.Dir
		}
		if dir == "" {
			return nil, fmt.Errorf("module %s %s is not in the module cache: run go mod download", p, m.Version)
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(root, dir)
		}
		mods = append(mods, linkedModule{path: p, version: m.Version, dir: dir, replace: m.Replace})
	}
	return mods, nil
}

// goRuntime returns GOROOT and the Go version of the toolchain that builds the binary in root.
func goRuntime(ctx context.Context, goCmd goCmdFunc, root string) (goroot, version string, err error) {
	out, err := goCmd(ctx, root, []string{"GOWORK=off"}, "env", "GOROOT", "GOVERSION")
	if err != nil {
		return "", "", fmt.Errorf("finding the Go runtime: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 || strings.TrimSpace(lines[0]) == "" || !strings.HasPrefix(strings.TrimSpace(lines[1]), "go") {
		return "", "", fmt.Errorf("go env GOROOT GOVERSION printed %q", out)
	}
	return strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1]), nil
}
