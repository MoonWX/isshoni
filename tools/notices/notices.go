package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// config is one run of notices; run fills it from the flags.
type config struct {
	root           string   // the main module
	pkgs           []string // package patterns of the binary, relative to root
	goos, goarch   []string // the targets the binary ships for
	npmPath        string   // web/dist/licenses.txt
	extraPath      string   // deploy/notices/extra.txt
	exceptionsPath string   // deploy/notices/exceptions.md
	version        string   // shown in the header
	goCmd          goCmdFunc
}

// result is what generate produced.
type result struct {
	text         []byte   // THIRD_PARTY_NOTICES, nil when the gate failed
	modules      int      // Go modules listed, without the runtime
	goVersion    string   // the Go runtime's version, go1.x.y
	extraEntries int      // "License:" entries of extra.txt
	warnings     []string // stale exceptions
}

// gateError lists every reason the license gate failed, one line each.
type gateError struct {
	problems []string
}

func (e *gateError) Error() string {
	return "the license gate failed:\n" + strings.Join(e.problems, "\n")
}

// component is one entry of the Go section.
type component struct {
	title string        // module path and version, or the runtime's name
	ids   []string      // the licenses found, sorted
	files []licenseFile // reproduced in this order
}

// generate collects the components, applies the gate and renders THIRD_PARTY_NOTICES. A failed gate returns a
// *gateError that lists every problem found, not only the first.
func generate(ctx context.Context, cfg config) (result, error) {
	var res result
	exceptionsText, err := readInput(cfg.exceptionsPath, "the license exceptions")
	if err != nil {
		return res, err
	}
	exceptions, err := parseExceptions(exceptionsText)
	if err != nil {
		return res, fmt.Errorf("%s:\n%w", cfg.exceptionsPath, err)
	}
	npmText, err := readInput(cfg.npmPath, "the web client's license list (npm run build writes it; run task build:web)")
	if err != nil {
		return res, err
	}
	extraText, err := readInput(cfg.extraPath, "the notices for adapted code")
	if err != nil {
		return res, err
	}

	goroot, goVersion, err := goRuntime(ctx, cfg.goCmd, cfg.root)
	if err != nil {
		return res, err
	}
	res.goVersion = goVersion
	mods, err := listLinkedModules(ctx, cfg.goCmd, cfg.root, cfg.pkgs, cfg.goos, cfg.goarch)
	if err != nil {
		return res, err
	}
	res.modules = len(mods)

	var problems []string

	runtimeFiles, err := readRuntimeLicense(goroot)
	if err != nil {
		return res, err
	}
	rt := component{title: fmt.Sprintf("Go standard library and runtime (%s)", goVersion), files: runtimeFiles}
	var failed []string
	rt.ids, failed = licensesOf(rt.files, func(string) bool { return false })
	for _, p := range failed {
		problems = append(problems, fmt.Sprintf("the Go runtime (%s): %s", goVersion, p))
	}

	comps := make([]component, 0, len(mods))
	for _, m := range mods {
		files, err := readLicenseFiles(m.dir)
		if err != nil {
			return res, fmt.Errorf("module %s %s: %w", m.path, m.version, err)
		}
		c := component{title: moduleTitle(m), files: files}
		c.ids, failed = licensesOf(files, func(id string) bool {
			for _, e := range exceptions {
				if e.module == m.path && e.license == id {
					e.used = true
					return true
				}
			}
			return false
		})
		for _, p := range failed {
			problems = append(problems, fmt.Sprintf("%s %s: %s", m.path, m.version, p))
		}
		comps = append(comps, c)
	}

	n, extraProblems := checkExtra(extraText)
	res.extraEntries = n
	for _, p := range extraProblems {
		problems = append(problems, strings.Replace(p, "extra.txt", cfg.extraPath, 1))
	}

	for _, e := range exceptions {
		if !e.used {
			res.warnings = append(res.warnings, fmt.Sprintf("%s line %d: the exception for %s (%s) matches no linked module; remove it",
				cfg.exceptionsPath, e.line, e.module, e.license))
		}
	}
	if len(problems) != 0 {
		problems = append(problems, "The shipped allowlist is: "+allowlistString()+".")
		return res, &gateError{problems: problems}
	}

	res.text = render(cfg.version, rt, comps, npmText, extraText)
	return res, nil
}

// readInput reads one of the input files; what says what it is, for the error.
func readInput(path, what string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%s is missing: %s", path, what)
	} else if err != nil {
		return "", err
	}
	text := normalize(string(data))
	if text == "" {
		return "", fmt.Errorf("%s is empty: %s", path, what)
	}
	return text, nil
}

// readRuntimeLicense reads $GOROOT/LICENSE: the runtime is linked into every Go binary.
func readRuntimeLicense(goroot string) ([]licenseFile, error) {
	files, err := readLicenseFiles(goroot)
	if err != nil {
		return nil, fmt.Errorf("the Go runtime's license: %w", err)
	}
	for _, f := range files {
		if f.name == "LICENSE" {
			return []licenseFile{f}, nil
		}
	}
	return nil, fmt.Errorf("the Go runtime's license: %s has no LICENSE file", goroot)
}

// moduleTitle names a module in the output: path and version, and the replacement when go.mod replaces it.
// A local directory replacement is not named, so no local path reaches the file.
func moduleTitle(m linkedModule) string {
	title := m.path + " " + m.version
	switch {
	case m.replace == nil:
	case m.replace.Version != "":
		title += " => " + m.replace.Path + " " + m.replace.Version
	default:
		title += " (replaced by a local directory)"
	}
	return strings.TrimSpace(title)
}

const (
	sectionRule = "================================================================================"
	entryRule   = "--------------------------------------------------------------------------------"
)

// render writes THIRD_PARTY_NOTICES.
func render(version string, rt component, mods []component, npmText, extraText string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "THIRD_PARTY_NOTICES for isshoni %s\n\n", version)
	b.WriteString(`isshoni is licensed under the Apache License, Version 2.0 (see LICENSE and NOTICE). The isshoni server
binary also contains the third-party software listed below. Each entry names the component, its version and
license, and reproduces the license and notice files that come with it.

  1. Go modules and the Go runtime
  2. Web client (bundled JavaScript)
  3. Adapted source code and data
`)

	section(&b, fmt.Sprintf("1. Go modules and the Go runtime (%d modules)", len(mods)))
	entry(&b, rt)
	for _, c := range mods {
		entry(&b, c)
	}

	section(&b, "2. Web client (bundled JavaScript)")
	b.WriteString("\n")
	b.WriteString(npmText)

	section(&b, "3. Adapted source code and data")
	b.WriteString("\n")
	b.WriteString(extraText)
	return b.Bytes()
}

func section(b *bytes.Buffer, title string) {
	fmt.Fprintf(b, "\n\n%s\n%s\n%s\n", sectionRule, title, sectionRule)
}

func entry(b *bytes.Buffer, c component) {
	fmt.Fprintf(b, "\n%s\n%s\nLicense: %s\n%s\n", entryRule, c.title, strings.Join(c.ids, ", "), entryRule)
	for _, f := range c.files {
		fmt.Fprintf(b, "\n== %s ==\n\n", filepath.ToSlash(f.name))
		if f.text == "" {
			b.WriteString("(empty file)\n")
			continue
		}
		b.WriteString(f.text)
	}
}
