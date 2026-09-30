// Command tsregistry writes web/src/protocol/registry.gen.ts from protocol.Registry (docs/m1/01-protocol.md §14.4,
// §16): the TypeScript message maps ClientRequests, ClientNotifications and ServerMessages, the envelope types
// ServerEnvelope and ClientEnvelope, and runtime arrays and maps of the message types. The payload types it names
// are tygo's output in types.gen.ts (tygo.yaml), which `task gen` writes first.
//
//	go run ./internal/protocol/gen/tsregistry -o web/src/protocol/registry.gen.ts
//
// Without -o it writes to stdout. The output depends only on the Registry, so running it twice gives the same bytes,
// and a file that is already current is left untouched. `task gen:check` (CI) fails when the committed file differs.
//
// Exit status: 0 on success, 1 when the Registry cannot be expressed in TypeScript or the file cannot be written,
// 2 on a usage error.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/MoonWX/isshoni/internal/protocol"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("tsregistry", flag.ContinueOnError)
	flags.SetOutput(stderr)
	out := flags.String("o", "", "output file (default: stdout)")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "usage: tsregistry [-o FILE]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		flags.Usage()
		return 2
	}

	src, err := Generate(protocol.Registry)
	if err != nil {
		fmt.Fprintf(stderr, "tsregistry: %v\n", err)
		return 1
	}
	if *out == "" {
		if _, err := stdout.Write(src); err != nil {
			fmt.Fprintf(stderr, "tsregistry: %v\n", err)
			return 1
		}
		return 0
	}
	if err := writeIfChanged(*out, src); err != nil {
		fmt.Fprintf(stderr, "tsregistry: %v\n", err)
		return 1
	}
	return 0
}

// writeIfChanged writes src to path unless the file already holds exactly src, so an unchanged registry does not
// wake up file watchers (Vite, tsc -b).
func writeIfChanged(path string, src []byte) error {
	old, err := os.ReadFile(path) //nolint:gosec // G304: the path is the -o flag of a developer tool
	switch {
	case err == nil && bytes.Equal(old, src):
		return nil
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("read %s: %w", path, err)
	}
	// 0644 like every other source file in the tree: the file is committed, not a secret.
	if err := os.WriteFile(path, src, 0o644); err != nil { //nolint:gosec // G306: see above
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
