package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/MoonWX/isshoni/internal/media"
)

func init() {
	register(command{name: "sources", summary: "list the monitors (im_list_sources) with their mon: ids", run: cmdSources})
}

// sourceLister is what sources needs of the engine (*native.Engine; tests fake it).
type sourceLister interface {
	Sources(ctx context.Context) ([]media.Source, error)
}

// listedSource is one monitor as sources prints it. The monitor's name only with -name, like pick's: output that may
// be attached to a public PR carries no names (README §4 "Logging").
type listedSource struct {
	ID     string `json:"id"` // "mon:" + the output's DeviceName (04 §5.6)
	Name   string `json:"name,omitempty"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// cmdSources is 04 §14's sources: the monitors im_list_sources reports, each with the id capture -source takes
// (mon:<id>). Nothing is asserted. With -name it also prints each monitor's name (its friendly name, else its
// DeviceName): beside pick -name's DisplayName() of a monitor pick, that is what 04 §7.5 records for Windows 10
// (README WA60: `sources -name`, then `pick -name -count 2` picking each monitor). Exit 0 when at least one monitor is
// listed, 2 when none is (or the engine doesn't load), 1 for another failure.
func cmdSources(ctx context.Context, g *globals, args []string) int {
	fs := newFlags(g, "sources", "[-name]")
	withName := fs.Bool("name", false, "also print each monitor's name")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	e, code, err := g.openEngine(true)
	if err != nil {
		fmt.Fprintln(g.stderr, "sources: the engine didn't load:", err)
		return code
	}
	defer func() { _ = e.Close() }()
	return runSources(ctx, e, *withName, g.json, g.stdout, g.stderr)
}

// runSources lists l's monitors and prints them (JSON: one array).
func runSources(ctx context.Context, l sourceLister, withName, asJSON bool, stdout, stderr io.Writer) int {
	srcs, err := l.Sources(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "sources:", err)
		return envCode(err)
	}
	out := make([]listedSource, 0, len(srcs))
	for _, s := range srcs {
		r := listedSource{ID: s.ID, Width: s.Width, Height: s.Height}
		if withName {
			r.Name = s.Name
		}
		out = append(out, r)
	}
	if asJSON {
		data, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			fmt.Fprintln(stderr, "sources:", err)
			return exitFail
		}
		_, _ = stdout.Write(append(data, '\n'))
	} else {
		for _, r := range out {
			fmt.Fprintf(stdout, "%s %dx%d", r.ID, r.Width, r.Height)
			if r.Name != "" {
				fmt.Fprintf(stdout, ", name %q", r.Name)
			}
			fmt.Fprintln(stdout)
		}
	}
	if len(out) == 0 {
		fmt.Fprintln(stderr, "sources: no monitor is attached to the desktop")
		return exitEnv
	}
	return exitPass
}
