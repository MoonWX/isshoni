package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/MoonWX/isshoni/internal/version"
)

func versionCmd() *command {
	return &command{
		name:    "version",
		summary: "Print build information",
		help: "Prints the version, commit, commit date, Go version, platform, and the signaling protocol and database " +
			"schema this binary speaks. 'isshoni --version' prints the same line.",
		setup: func(fs *flag.FlagSet) runFunc {
			short := fs.Bool("short", false, "print only the version, e.g. 0.3.0")
			asJSON := fs.Bool("json", false, "print the build information as JSON")
			return func(_ context.Context, inv *invocation, _ []string) error {
				if *short && *asJSON {
					return usageErrorf("--short and --json can't be combined")
				}
				return printVersion(inv.stdout, buildInfo(), *short, *asJSON)
			}
		},
	}
}

// buildInfo is version.Info plus the numbers only this command knows: Protocol from protocol.Version (01) and
// Schema from store.LatestSchemaVersion (03). Both stay zero, and out of the output, until those packages exist and
// are wired in here.
func buildInfo() version.BuildInfo {
	return version.Info()
}

func printVersion(w io.Writer, bi version.BuildInfo, short, asJSON bool) error {
	var err error
	switch {
	case short:
		_, err = fmt.Fprintln(w, bi.Version)
	case asJSON: // one JSON document on one line, like every --json of the CLI
		err = json.NewEncoder(w).Encode(bi)
	default:
		_, err = fmt.Fprintln(w, versionLine(bi))
	}
	return err
}

// versionLine is the human form, e.g.
// "isshoni 0.3.0 (commit 1a2b3c4, built 2026-09-29, go1.26.5, linux/amd64, protocol 1, schema 7)".
func versionLine(bi version.BuildInfo) string {
	var parts []string
	if bi.Commit != "" {
		parts = append(parts, "commit "+bi.Commit[:min(7, len(bi.Commit))])
	}
	if bi.Date != "" {
		built := bi.Date
		if t, err := time.Parse(time.RFC3339, bi.Date); err == nil {
			built = t.UTC().Format(time.DateOnly)
		}
		parts = append(parts, "built "+built)
	}
	parts = append(parts, bi.Go, bi.OS+"/"+bi.Arch)
	if bi.Protocol != 0 {
		parts = append(parts, fmt.Sprintf("protocol %d", bi.Protocol))
	}
	if bi.Schema != 0 {
		parts = append(parts, fmt.Sprintf("schema %d", bi.Schema))
	}
	return "isshoni " + bi.Version + " (" + strings.Join(parts, ", ") + ")"
}
