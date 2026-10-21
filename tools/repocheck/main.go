// Command repocheck runs the repository consistency checks behind `task lint:pins` and `task lint:keys`
// (docs/m1/06-deploy-and-ci.md §7.1, §4.3, §9.4, §10.3).
//
//	go -C tools run ./repocheck [-root DIR] pins|keys
//
// pins: the Go version pinned in .tool-versions equals the toolchain directive of go.mod, tools/go.mod,
// tools/goreleaser/go.mod and tools/wails3/go.mod; the Task, golangci-lint and shfmt pins equal the versions that
// tools/go.mod builds into .bin/, and the goreleaser pin the version that tools/goreleaser/go.mod builds. Wails
// (docs/m2/06-windows-release.md §3): tools/wails3/go.mod requires github.com/wailsapp/wails/v3, at go.mod's version
// once go.mod requires it; @wailsio/runtime in web/package.json and web/package-lock.json is then exactly that version
// without the "v", never a range or a tag. While go.mod does not require Wails, only the CLI's pin is checked (with a
// note), and web/package.json must not list the runtime.
//
// keys: the allowed-signers block embedded in deploy/install.sh equals deploy/keys/allowed_signers byte for byte.
// Once that file is non-empty, it holds at least one key, the "## Release signing keys" section of README.md lists
// the SHA256 fingerprint of each of them, as `ssh-keygen -l` prints it, and no other fingerprint, and none of its
// lines still says a key is "not created yet". While deploy/keys/allowed_signers does not exist yet, it prints a note
// and succeeds; while deploy/install.sh does not, it checks only README.md and prints a note.
//
// Exit status: 0 when the check passes (or is skipped with a note), 1 when it fails, 2 on a usage error.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repocheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", ".", "repository root")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: repocheck [-root DIR] pins|keys")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}

	var (
		note string
		err  error
	)
	switch fs.Arg(0) {
	case "pins":
		note, err = checkPins(*root)
	case "keys":
		note, err = checkKeys(*root)
	default:
		fmt.Fprintf(stderr, "repocheck: unknown check %q\n", fs.Arg(0))
		fs.Usage()
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "repocheck %s: %v\n", fs.Arg(0), err)
		return 1
	}
	if note != "" {
		fmt.Fprintf(stdout, "repocheck %s: %s\n", fs.Arg(0), note)
	}
	return 0
}
