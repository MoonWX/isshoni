package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/MoonWX/isshoni/internal/server/config"
)

// The config commands load, print and write the server configuration (04 §3.1, §4). config.Load (or LoadFlags for
// example and init) registers the config flags and parses the command line before a command runs (command.parse).

func configCmd() *command {
	return &command{
		name:    "config",
		summary: "Check, print or write the configuration",
		help: "Checks, prints or writes the server configuration. Values come from flags, ISSHONI_* environment " +
			"variables, the file (--config PATH, env ISSHONI_CONFIG, default /etc/isshoni/isshoni.toml) and the " +
			"defaults, in that order. An invalid configuration exits 78.",
		subs: []*command{
			{
				name:    "check",
				summary: "Load and validate the configuration and print its problems",
				help: "Loads and validates the configuration the server would use and prints every problem with its " +
					"fix. Exits 78 when there is an error; warnings don't change the exit code.",
				config: true,
				setup:  func(*flag.FlagSet) runFunc { return runConfigCheck },
			},
			{
				name:    "print",
				summary: "Print the effective configuration and where each value comes from",
				help: "Prints every key with its effective value and its source (default, file:line, env or flag). A " +
					"policy key that is not set shows as \"not set (admin UI value applies)\". Exits 78 when the " +
					"configuration has an error.",
				config: true,
				setup: leaf(func(fs *flag.FlagSet, o *configPrintOptions) {
					fs.BoolVar(&o.json, "json", false, "print the configuration as JSON")
				}, runConfigPrint),
			},
			{
				name:    "example",
				summary: "Print a commented isshoni.toml with the given values set",
				help: "Prints a commented isshoni.toml with the values given as config flags set. ISSHONI_* environment " +
					"variables are ignored. Invalid values exit 78 and print nothing on stdout.",
				config:          true,
				configFlagsOnly: true,
				listConfigFlags: true,
				setup:           func(*flag.FlagSet) runFunc { return runConfigExample },
			},
			{
				name:    "init",
				summary: "Write a commented isshoni.toml; never overwrites a file",
				help: "Writes the file of 'isshoni config example' to --path (mode 0640) with the values given as " +
					"config flags; ISSHONI_* environment variables are ignored. It refuses to overwrite an existing " +
					"file (exit 7) and writes nothing when a value is invalid (exit 78).",
				config:          true,
				configFlagsOnly: true,
				listConfigFlags: true,
				setup: leaf(func(fs *flag.FlagSet, o *configInitOptions) {
					fs.StringVar(&o.path, "path", "", "write the file to `PATH` (required)")
				}, runConfigInit),
			},
		},
	}
}

type configPrintOptions struct {
	json bool
}

type configInitOptions struct {
	path string
}

// configInvalid prints the problems of a config with errors and returns the exit-78 error; nil when the config has
// no errors.
func configInvalid(inv *invocation) error {
	if inv.cfgErr == nil {
		return nil
	}
	printProblems(inv.stderr, inv.cfgErr.Problems)
	return exitWith(exitConfig, nil)
}

// printProblems writes problems as in 04 §4.5, one after another.
func printProblems(w io.Writer, problems []config.Problem) {
	for _, p := range problems {
		_, _ = fmt.Fprintln(w, p.String())
	}
}

func runConfigCheck(_ context.Context, inv *invocation, _ []string) error {
	if err := configInvalid(inv); err != nil {
		return err
	}
	warnings := inv.cfg.Warnings()
	printProblems(inv.stderr, warnings)
	path, read := inv.cfg.File()
	from := "file " + path
	if !read {
		from = "no file at " + path + ", defaults and environment only"
	}
	switch n := len(warnings); n {
	case 0:
		_, err := fmt.Fprintf(inv.stdout, "config ok (%s)\n", from)
		return err
	default:
		_, err := fmt.Fprintf(inv.stdout, "config ok with %d warning%s (%s)\n", n, plural(n), from)
		return err
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// runConfigPrint prints the effective config, even an invalid one (then its problems go to stderr and it exits 78).
func runConfigPrint(_ context.Context, inv *invocation, o configPrintOptions, _ []string) error {
	var err error
	if o.json {
		err = inv.cfg.PrintJSON(inv.stdout)
	} else {
		err = inv.cfg.PrintText(inv.stdout)
	}
	if err != nil {
		return err
	}
	if inv.cfgErr != nil {
		return configInvalid(inv)
	}
	printProblems(inv.stderr, inv.cfg.Warnings())
	return nil
}

func runConfigExample(_ context.Context, inv *invocation, _ []string) error {
	if err := configInvalid(inv); err != nil {
		return err
	}
	printProblems(inv.stderr, inv.cfg.Warnings())
	_, err := inv.stdout.Write(config.Example(inv.cfg))
	return err
}

// runConfigInit writes the example file to --path: validated first (exit 78, nothing written), then created with
// O_EXCL so an existing file, or a symlink, is never touched (exit 7).
func runConfigInit(_ context.Context, inv *invocation, o configInitOptions, _ []string) error {
	if o.path == "" {
		return usageErrorf("--path is required")
	}
	if err := configInvalid(inv); err != nil {
		return err
	}
	printProblems(inv.stderr, inv.cfg.Warnings())
	if err := writeNewFile(o.path, config.Example(inv.cfg)); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return exitWith(exitRefused, fmt.Errorf("%s already exists, and config init never overwrites a file: edit it, or move it away first", o.path))
		}
		return err
	}
	_, _ = fmt.Fprintf(inv.stderr, "Wrote %s\n", o.path)
	return nil
}

// writeNewFile creates path with mode 0640 (whatever the umask) and writes data; it fails with fs.ErrExist when
// path exists. On a write error it removes the file it created.
func writeNewFile(path string, data []byte) (err error) {
	//nolint:gosec // G302, G304: the operator names the file; 0640 lets the isshoni group read it (04 §3.1, 06 §2)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	if err := f.Chmod(0o640); err != nil { //nolint:gosec // G302: see above
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}
