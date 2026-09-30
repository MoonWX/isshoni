// Command notices writes THIRD_PARTY_NOTICES, the license notices for everything the isshoni server binary contains,
// and is the second half of the license gate (docs/m1/06-deploy-and-ci.md §8.3–8.4, "06" below). `task notices`
// runs it:
//
//	go -C tools run ./notices -root .. -pkg ./cmd/isshoni -goos linux,darwin,windows \
//		-npm ../web/dist/licenses.txt -extra ../deploy/notices/extra.txt -version <v> -o ../THIRD_PARTY_NOTICES
//
// The file has three sections:
//
//  1. Go modules: the Go standard library and runtime (from $(go env GOROOT)/LICENSE), then every module that
//     `go list -deps` finds for the packages of -pkg with CGO_ENABLED=0, for each GOOS of -goos and GOARCH of
//     -goarch (the union), minus the main module; with the LICENSE*, LICENCE*, COPYING*, UNLICENSE* and NOTICE*
//     files of each module's root.
//  2. Web client (bundled JavaScript): web/dist/licenses.txt, which `npm run build` writes (web/scripts/licenses.mjs).
//  3. Adapted source code and data: deploy/notices/extra.txt, kept by hand.
//
// The gate: a module's own license files (LICENSE, LICENCE, COPYING or UNLICENSE, optionally .txt, .md or .rst;
// every LICENSE* file when it has none of those) are identified with github.com/google/licensecheck, and each
// license found must be on the shipped allowlist (06 §8.3), or listed for that module with a reason in
// deploy/notices/exceptions.md (MPL-2.0 modules, which go-licenses lets through as "reciprocal"). GPL, AGPL, LGPL,
// SSPL and BUSL can never be excepted. A module without a license file, or whose license text licensecheck does not
// recognize (less than 75% of it matches), fails too. Other license files, such as modernc.org/sqlite's
// LICENSE-3RD-PARTY.md, describe material the module bundles or merely mentions: they are reproduced, not gated
// (a Go module they name is gated on its own when it is linked). Every "License:" line of extra.txt must name an
// allowlisted license.
//
// Relative paths in -npm, -extra, -exceptions and -o are relative to the working directory (tools/ under
// `go -C tools`); -pkg patterns are relative to -root. The output is deterministic: sorted, no dates and no local
// paths, with the version only in the header. It is written only when the gate passes.
//
// Exit status: 0 when the file was written, 1 when the gate failed or an input is missing, 2 on a usage error.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("notices", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", ".", "repository root: the main module of the shipped binary")
	pkg := fs.String("pkg", "./cmd/isshoni", "comma-separated package patterns of the shipped binary, relative to -root")
	goos := fs.String("goos", "linux,darwin,windows", "comma-separated GOOS values the binary is shipped for")
	goarch := fs.String("goarch", "amd64,arm64", "comma-separated GOARCH values the binary is shipped for")
	npm := fs.String("npm", "", "the web client's license list, web/dist/licenses.txt (required)")
	extra := fs.String("extra", "", "the hand-kept notices for adapted code and data, deploy/notices/extra.txt (required)")
	exceptions := fs.String("exceptions", "", "the license exceptions with reasons (default <root>/deploy/notices/exceptions.md)")
	version := fs.String("version", "", "the version shown in the header (required)")
	out := fs.String("o", "-", "the output file; - is standard output")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: notices -npm FILE -extra FILE -version V [-root DIR] [-pkg PATTERNS] [-goos LIST] [-goarch LIST] [-exceptions FILE] [-o FILE]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	var missing []string
	for _, f := range []struct{ name, value string }{{"-npm", *npm}, {"-extra", *extra}, {"-version", *version}} {
		if f.value == "" {
			missing = append(missing, f.name)
		}
	}
	if fs.NArg() != 0 || len(missing) != 0 {
		if len(missing) != 0 {
			fmt.Fprintf(stderr, "notices: missing %s\n", strings.Join(missing, ", "))
		} else {
			fmt.Fprintf(stderr, "notices: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		}
		fs.Usage()
		return 2
	}

	cfg := config{
		root:           *root,
		pkgs:           splitList(*pkg),
		goos:           splitList(*goos),
		goarch:         splitList(*goarch),
		npmPath:        *npm,
		extraPath:      *extra,
		exceptionsPath: *exceptions,
		version:        *version,
		goCmd:          runGo,
	}
	if cfg.exceptionsPath == "" {
		cfg.exceptionsPath = filepath.Join(cfg.root, "deploy", "notices", "exceptions.md")
	}
	if len(cfg.pkgs) == 0 || len(cfg.goos) == 0 || len(cfg.goarch) == 0 {
		fmt.Fprintln(stderr, "notices: -pkg, -goos and -goarch need at least one value each")
		return 2
	}

	res, err := generate(ctx, cfg)
	for _, w := range res.warnings {
		fmt.Fprintf(stderr, "notices: warning: %s\n", w)
	}
	if err != nil {
		var gate *gateError
		if errors.As(err, &gate) {
			fmt.Fprintf(stderr, "notices: the license gate failed (docs/m1/06-deploy-and-ci.md §8.3):\n")
			for _, p := range gate.problems {
				fmt.Fprintf(stderr, "  %s\n", p)
			}
			return 1
		}
		fmt.Fprintf(stderr, "notices: %v\n", err)
		return 1
	}

	if *out == "-" {
		if _, err := stdout.Write(res.text); err != nil {
			fmt.Fprintf(stderr, "notices: %v\n", err)
			return 1
		}
		return 0
	}
	if err := writeFile(*out, res.text); err != nil {
		fmt.Fprintf(stderr, "notices: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "notices: wrote %s: %d Go modules, the Go runtime (%s), the web client's packages and %d adapted-code entries\n",
		*out, res.modules, res.goVersion, res.extraEntries)
	return 0
}

// splitList splits a comma-separated flag value, dropping empty items.
func splitList(s string) []string {
	var out []string
	for item := range strings.SplitSeq(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// writeFile replaces path with data through a temporary file in the same directory, so a reader never sees a
// partial file.
func writeFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".notices-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // a no-op after the rename
	if _, err := f.Write(data); err != nil {
		_ = f.Close() // the write error is the one to report
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Release archives and packages ship the file as a world-readable document.
	if err := os.Chmod(tmp, 0o644); err != nil { //nolint:gosec // G302: a public notices file, not a secret
		return err
	}
	return os.Rename(tmp, path)
}
