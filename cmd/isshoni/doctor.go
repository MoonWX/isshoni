package main

import (
	"context"
	"flag"
	"slices"
)

type doctorOptions struct {
	clientOptions
	json, strict, listChecks bool
	only                     string // comma-separated check ids

	// Bandwidth calculator input (04 §13.4).
	people, sharing, thumbnails int
	quality, preset             string
	hours                       float64
}

var (
	doctorQualities = []string{"1080p60", "1440p60", "2160p60"}
	doctorPresets   = []string{"auto", "movie"}
)

func doctorCmd() *command {
	return &command{
		name:    "doctor",
		summary: "Diagnose the server and this machine",
		help: "Runs the checks inside the running server through its admin socket, or on this machine when the " +
			"server is stopped (then the checks that need a live server are skipped). Exits 5 when a check fails " +
			"(with --strict, also when one warns). The bandwidth flags describe a session for the bandwidth estimate.",
		config: true,
		setup: leaf(func(fs *flag.FlagSet, o *doctorOptions) {
			fs.BoolVar(&o.json, "json", false, "print the report as JSON")
			fs.BoolVar(&o.strict, "strict", false, "exit 5 on a warning too")
			fs.StringVar(&o.only, "only", "", "run only the checks with these comma-separated `IDS` (see --list-checks)")
			fs.BoolVar(&o.listChecks, "list-checks", false, "print the check ids and exit (a JSON array with --json)")
			fs.IntVar(&o.people, "people", 5, "bandwidth estimate: `N` people in a session")
			fs.IntVar(&o.sharing, "sharing", 2, "bandwidth estimate: `N` of them sharing at the same time")
			fs.IntVar(&o.thumbnails, "thumbnails", 8, "bandwidth estimate: at most `N` thumbnails per viewer")
			fs.StringVar(&o.quality, "quality", "1080p60", "bandwidth estimate: `QUALITY` of the shares: 1080p60, 1440p60 or 2160p60")
			fs.StringVar(&o.preset, "preset", "auto", "bandwidth estimate: `PRESET` auto or movie (256 kbps audio)")
			fs.Float64Var(&o.hours, "hours", 2, "bandwidth estimate: session length in `HOURS`")
			o.register(fs)
		}, runDoctor),
	}
}

// runDoctor runs the checks of 04 §13 through the admin socket or offline. A later slice fills it in.
func runDoctor(_ context.Context, _ *invocation, o doctorOptions, _ []string) error {
	if !slices.Contains(doctorQualities, o.quality) {
		return usageErrorf("--quality %q: want 1080p60, 1440p60 or 2160p60", o.quality)
	}
	if !slices.Contains(doctorPresets, o.preset) {
		return usageErrorf("--preset %q: want auto or movie", o.preset)
	}
	return errNotImplemented
}
