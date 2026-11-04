package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/native"
)

func init() {
	register(command{name: "pick", summary: "show the OS picker (the engine's own owner window) and print the picked source",
		run: cmdPick})
}

// picker is what pick needs of the engine (*native.Engine; tests fake it).
type picker interface {
	Pick(ctx context.Context, req media.PickRequest) (media.Source, error)
	SourceDetail(id string) (native.SourceDetail, bool)
	Release(id string)
}

// pickOptions are pick's flags.
type pickOptions struct {
	count   int
	timeout time.Duration
	name    bool
}

// pickResult is one pick as pick prints it. The source's name (a window title or a monitor name) only with -name:
// output that may be attached to a public PR carries no names (README §4 "Logging").
type pickResult struct {
	Outcome    string `json:"outcome"` // "picked", "cancelled" or "error"
	Error      string `json:"error,omitempty"`
	ID         string `json:"id,omitempty"`
	Kind       string `json:"kind,omitempty"` // "window" or "screen" (media.SourceKind)
	Name       string `json:"name,omitempty"`
	Width      int    `json:"width,omitempty"`
	Height     int    `json:"height,omitempty"`
	Capture    string `json:"capture,omitempty"` // "wgc", "dd"
	OSBorder   bool   `json:"osBorder"`
	Mapped     bool   `json:"mapped"`
	Identified bool   `json:"identified"` // the window was identified (04 §7.6): window and, usually, pid are set
	PID        int    `json:"pid,omitempty"`
	MonitorID  string `json:"monitorId,omitempty"`
}

// cmdPick is 04 §14's pick: the OS picker with the engine's own owner window (§7.2 step 2, no parent window), once or
// -count times in a row, printing each picked source; nothing is asserted (the manual check is M-PICK, §18.4: move
// the picker, cancel once, pick a window, then a monitor: `pick -count 3`). Each token is released after it is printed.
// Exit 0 when at least one pick returned a source and nothing failed but cancels, 2 when every pick was cancelled
// (or the environment lacks the picker), 1 for another failure.
func cmdPick(ctx context.Context, g *globals, args []string) int {
	fs := newFlags(g, "pick", "[-count N] [-timeout D] [-name]")
	var o pickOptions
	fs.IntVar(&o.count, "count", 1, "picks in a row (M-PICK: 3, cancel the first)")
	fs.DurationVar(&o.timeout, "timeout", 0, "the picker's timeout (0: the engine's 120s; at most 10m)")
	fs.BoolVar(&o.name, "name", false, "also print the source's name (window title or monitor name)")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if o.count < 1 || o.count > 20 {
		fmt.Fprintln(g.stderr, "pick: -count must be 1..20")
		return exitUsage
	}
	if o.timeout < 0 || o.timeout > 10*time.Minute {
		fmt.Fprintln(g.stderr, "pick: -timeout must be 0..10m")
		return exitUsage
	}
	e, code, err := g.openEngine(true)
	if err != nil {
		fmt.Fprintln(g.stderr, "pick: the engine didn't load:", err)
		return code
	}
	defer func() { _ = e.Close() }()
	return runPicks(ctx, e, o, g.json, g.stdout, g.stderr)
}

// runPicks runs o.count picks on p and prints them (JSON: one array).
func runPicks(ctx context.Context, p picker, o pickOptions, asJSON bool, stdout, stderr io.Writer) int {
	results := make([]pickResult, 0, o.count)
	picked, cancelled := 0, 0
	var firstErr error
	for i := 0; i < o.count && ctx.Err() == nil; i++ {
		if !asJSON && o.count > 1 {
			fmt.Fprintf(stdout, "pick %d of %d: the picker is open\n", i+1, o.count)
		}
		src, err := p.Pick(ctx, media.PickRequest{Timeout: o.timeout})
		r := describePick(p, src, err, o.name)
		switch r.Outcome {
		case "picked":
			picked++
			p.Release(src.ID)
		case "cancelled":
			cancelled++
		default:
			if firstErr == nil {
				firstErr = err
			}
		}
		results = append(results, r)
		if !asJSON {
			printPick(stdout, r)
		}
	}
	if asJSON {
		data, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			fmt.Fprintln(stderr, "pick:", err)
			return exitFail
		}
		_, _ = stdout.Write(append(data, '\n'))
	}
	switch {
	case firstErr != nil:
		fmt.Fprintln(stderr, "pick:", firstErr)
		return envCode(firstErr)
	case picked == 0 && cancelled > 0:
		return exitEnv
	case picked == 0:
		return exitFail // interrupted before any pick
	}
	return exitPass
}

// describePick turns one Pick's outcome into a pickResult.
func describePick(p picker, src media.Source, err error, withName bool) pickResult {
	if err != nil {
		if errors.Is(err, media.ErrCancelled) || errors.Is(err, context.Canceled) {
			return pickResult{Outcome: "cancelled"}
		}
		return pickResult{Outcome: "error", Error: err.Error()}
	}
	r := pickResult{
		Outcome: "picked", ID: src.ID, Kind: string(src.Kind), Width: src.Width, Height: src.Height,
		Capture: src.Capture, OSBorder: src.OSBorder, PID: src.PID, MonitorID: src.MonitorID,
	}
	if withName {
		r.Name = src.Name
	}
	if d, ok := p.SourceDetail(src.ID); ok {
		r.Mapped = d.Mapped
		r.Identified = d.Window != 0
	}
	return r
}

func printPick(w io.Writer, r pickResult) {
	switch r.Outcome {
	case "cancelled":
		fmt.Fprintln(w, "cancelled")
		return
	case "error":
		fmt.Fprintln(w, "error:", r.Error)
		return
	}
	fmt.Fprintf(w, "picked %s: %s %dx%d, capture %s, os border %t, mapped %t, identified %t, pid %d", r.ID, r.Kind,
		r.Width, r.Height, r.Capture, r.OSBorder, r.Mapped, r.Identified, r.PID)
	if r.MonitorID != "" {
		fmt.Fprintf(w, ", monitor %s", r.MonitorID)
	}
	if r.Name != "" {
		fmt.Fprintf(w, ", name %q", r.Name)
	}
	fmt.Fprintln(w)
}
