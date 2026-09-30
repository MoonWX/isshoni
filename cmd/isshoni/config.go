package main

import (
	"context"
	"flag"
)

// The config commands load, print and write the server configuration (04 §3.1, §4). The config slice fills in the
// run functions; config.Load registers the config flags and parses the command line.

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
				config:  true,
				setup:   func(*flag.FlagSet) runFunc { return runConfigCheck },
			},
			{
				name:    "print",
				summary: "Print the effective configuration and where each value comes from",
				config:  true,
				setup: leaf(func(fs *flag.FlagSet, o *configPrintOptions) {
					fs.BoolVar(&o.json, "json", false, "print the configuration as JSON")
				}, runConfigPrint),
			},
			{
				name:    "example",
				summary: "Print a commented isshoni.toml with the given values set",
				help: "Prints a commented isshoni.toml with the values given as config flags set. ISSHONI_* environment " +
					"variables are ignored. Invalid values exit 78 and print nothing on stdout.",
				config: true,
				setup:  func(*flag.FlagSet) runFunc { return runConfigExample },
			},
			{
				name:    "init",
				summary: "Write a commented isshoni.toml; never overwrites a file",
				help: "Writes the file of 'isshoni config example' to --path (mode 0640) with the values given as " +
					"config flags; ISSHONI_* environment variables are ignored. It refuses to overwrite an existing " +
					"file (exit 7) and writes nothing when a value is invalid (exit 78).",
				config: true,
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

func runConfigCheck(context.Context, *invocation, []string) error {
	return errNotImplemented
}

func runConfigPrint(context.Context, *invocation, configPrintOptions, []string) error {
	return errNotImplemented
}

func runConfigExample(context.Context, *invocation, []string) error {
	return errNotImplemented
}

func runConfigInit(_ context.Context, _ *invocation, o configInitOptions, _ []string) error {
	if o.path == "" {
		return usageErrorf("--path is required")
	}
	return errNotImplemented
}
