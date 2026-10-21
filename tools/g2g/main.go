// Command g2g measures glass-to-glass latency on one PC for the M2 exit criterion (docs/m2/01-client-core.md
// §12.2 and D17, "01" below): a clock page on the shared monitor and its copy in a viewer on the same monitor are
// read from the same desktop screenshot, so their difference is the time from the sharer's screen to the viewer's,
// accurate to a display frame, without a photodiode. It is in the tools module and cross-compiles from any OS
// (no cgo):
//
//	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go -C tools build -trimpath -ldflags "-X main.version=<v>" -o ../bin/g2g.exe ./g2g
//
// # serve
//
//	g2g serve [-port 8765]
//
// serves the clock page on http://127.0.0.1:<port>/ until Ctrl+C (any OS). The page draws, at every animation
// frame, v = floor(performance.now()) mod 65536 as a 16-bit Gray code in a strip of 18 cells of 48 × 48 device
// pixels (a black-white guard at each end, 16 data cells, black = 1) on a mid-grey frame at the top-left, a
// readable counter with "cuts on|off" and "tone on|off" under it, and moving content over the rest (a wandering
// gradient and a band of scrolling text). Behind the moving content the background swaps every 5 s between two
// full-window images made from a fixed seed, a dense page of small black text and coarse colour noise (the content
// cuts; ?cuts=0 turns them off). The Start button plays a 440 Hz sine at −30 dBFS, so a share of the page carries
// sound.
//
// # measure
//
//	g2g measure [-d 60s] [-run gate|nosound|nocuts] [-o result.json] [-server https://…] [-dump DIR]
//
// runs on the owner's Windows PC (01 §12.2's run book). It takes a screenshot of the primary monitor 20 times a
// second with GDI (BitBlt with SRCCOPY|CAPTUREBLT into a DIB section, per-monitor DPI aware so the screenshot is in
// physical pixels). For the first 2 s it calibrates: it finds every copy of the strip, the clock's own at scale 1
// (normally at its known position, the top-left corner of a full-screen page) and the largest other copy at a
// scale of 0.25–1.0 (the viewer; the nested copies inside it are smaller). Then, for -d, it reads both strips in
// each screenshot, following the copy if the viewer moves, and records Δ = v_clock − v_copy (mod 65536) in ms. The
// strip is found by its guards, whose black halves give its scale before anything is matched, and the guards' ends
// place every cell to a fraction of a pixel at any scale. A strip caught half way between two frames reads within
// half a frame of them (see resolve). A reading that disagrees with the capture time, or a Δ more than 3 s from the median
// of the last ones, counts as a decode failure ("implausible"), not as a sample.
//
// With -server it first measures the TCP connect time to the server (port 443 for https) five times, after one DNS
// lookup, and records the median as the RTT, and reads the server's version from its /api/v1/info. -run names the
// run in the JSON: the operator states which run it is, and the tool doesn't check it. -dump DIR saves PNG crops of
// the screen where the two strips are (each strip with its grey frame, nothing else of the desktop unless something
// covers it) at calibration and for the first screenshots that fail to decode, for diagnosis; look at them before
// sharing them.
//
// The JSON (to -o, or stdout without it) has the run name, the samples, p50, p95 and max, the decode failures by
// cause, the monitor's refresh rate, the RTT, the versions (g2g, Go, Windows, the server), the result and every
// screenshot's Δ. It names no host, user, machine or path: the exit-test record it goes into is public.
//
// # Exit status
//
// measure: 0 when p95 < 200 ms with at least 90 % of the screenshots decoded; 1 when the latency fails; 2 when too
// few decoded or the run was interrupted (inconclusive, S1's convention), and also for a usage error or when the
// measurement could not run (no Windows, no clock or copy on the screen). Every run is judged so; only the gate run
// decides the exit criterion. serve: 0 after Ctrl+C, 2 for a usage error or when it could not serve.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"
)

// Exit statuses (01 §12.2).
const (
	exitOK           = 0
	exitLatency      = 1
	exitInconclusive = 2
)

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
}

// realMain is main without the exit.
func realMain(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx, args, stdout, stderr)
}

const usageText = `usage: g2g serve [-port 8765]      serve the clock page on 127.0.0.1
       g2g measure [-d 60s] [-run gate|nosound|nocuts] [-o result.json] [-server https://…] [-dump DIR]
                                     measure glass-to-glass latency (Windows)
       g2g version
Run "g2g serve -h" or "g2g measure -h" for the flags; docs/m2/01-client-core.md §12.2 has the run book.`

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usageText)
		return exitInconclusive
	}
	switch args[0] {
	case "serve":
		return runServe(ctx, args[1:], stdout, stderr)
	case "measure":
		return runMeasure(ctx, args[1:], stdout, stderr)
	case "version":
		v := currentVersions()
		fmt.Fprintf(stdout, "g2g %s (%s, %s)\n", v.G2G, v.Go, v.OS)
		return exitOK
	case "help", "-h", "-help", "--help":
		fmt.Fprintln(stdout, usageText)
		return exitOK
	default:
		fmt.Fprintf(stderr, "g2g: unknown command %q\n%s\n", args[0], usageText)
		return exitInconclusive
	}
}

// newFlagSet is a flag set whose usage prints the command line and what it does.
func newFlagSet(name string, stderr io.Writer, line, what string) *flag.FlagSet {
	fs := flag.NewFlagSet("g2g "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: %s\n%s\n", line, what)
		fs.PrintDefaults()
	}
	return fs
}

// parseFlags parses args; ok is false when the command should exit with code (0 for -h, 2 for a usage error).
func parseFlags(fs *flag.FlagSet, args []string) (code int, ok bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK, false
		}
		return exitInconclusive, false
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(fs.Output(), "%s: unexpected argument %q\n", fs.Name(), fs.Arg(0))
		fs.Usage()
		return exitInconclusive, false
	}
	return 0, true
}

// runMeasure is `g2g measure`.
func runMeasure(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("measure", stderr,
		"g2g measure [-d 60s] [-run gate|nosound|nocuts] [-o result.json] [-server https://…] [-dump DIR]",
		"Measures glass-to-glass latency from screenshots of the primary monitor (Windows; 01 §12.2).")
	dur := fs.Duration("d", 60*time.Second, "how long to measure, after the 2 s calibration")
	runName := fs.String("run", "gate", "the run, for the JSON: "+strings.Join(runNames, ", "))
	out := fs.String("o", "", "the JSON file to write (default: stdout)")
	server := fs.String("server", "", "the isshoni server's URL: record the RTT to it and its version")
	dumpDir := fs.String("dump", "", "save crops of the strips here (calibration and the first failed screenshots)")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	usage := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "g2g measure: "+format+"\n", a...)
		fs.Usage()
		return exitInconclusive
	}
	if !slices.Contains(runNames, *runName) {
		return usage("-run %q is not one of %s", *runName, strings.Join(runNames, ", "))
	}
	if *dur < time.Second {
		return usage("-d %v is shorter than a second", *dur)
	}
	warn := func(format string, a ...any) { fmt.Fprintf(stderr, "g2g: "+format+"\n", a...) }
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "g2g measure: "+format+"\n", a...)
		return exitInconclusive
	}

	rep := &report{Tool: "g2g", Run: *runName, IntervalMs: captureInterval.Milliseconds(), Versions: currentVersions()}
	if *server != "" {
		u, err := parseServer(*server)
		if err != nil {
			return usage("%v", err)
		}
		rtts, err := measureRTT(ctx, u)
		if err != nil {
			return fail("%v", err)
		}
		med := medianFloat(rtts)
		rep.RTTMs, rep.RTTSamplesMs = &med, rtts
		if med > 50 {
			warn("the RTT to the server is %.1f ms; the run book asks for at most 50 ms", med)
		}
		if v, err := serverVersion(ctx, u); err != nil {
			warn("%v", err)
		} else {
			rep.Versions.Server = v
		}
	}
	// The result file is created first, so a path that can't be written fails before the minute of measuring; it
	// is removed again when no result is written into it.
	w, closeOut := stdout, func(bool) error { return nil }
	if *out != "" {
		f, err := os.Create(*out) //nolint:gosec // G304: the file the operator named with -o
		if err != nil {
			return fail("%v", err)
		}
		written := false
		closeOut = func(ok bool) error {
			written = ok
			return f.Close()
		}
		defer func() {
			if !written {
				_ = f.Close()
				_ = os.Remove(*out)
			}
		}()
		w = f
	}
	var dump *dumper
	if *dumpDir != "" {
		if err := os.MkdirAll(*dumpDir, 0o750); err != nil {
			return fail("-dump: %v", err)
		}
		dump = &dumper{dir: *dumpDir, warn: warn}
	}

	capt, err := openCapturer()
	if err != nil {
		return fail("%v", err)
	}
	defer func() { _ = capt.close() }()
	m := newMeter(capt, warn, dump)
	warn("calibrating for %v: the clock page full screen, the viewer showing its copy", calibrationTime)
	if err := m.calibrate(ctx, calibrationTime); err != nil {
		return fail("%v", err)
	}
	warn("clock at (%.0f, %.0f); copy at scale %.2f at (%.0f, %.0f); measuring for %v",
		m.clock.X0, m.clock.Y0, m.copy.scale(), m.copy.X0, m.copy.Y0, *dur)
	start := time.Now()
	rep.StartedAt = start.UTC().Format(time.RFC3339)
	rep.Series, rep.Interrupted = m.measure(ctx, *dur)
	rep.DurationMs = time.Since(start).Milliseconds()
	rep.RefreshHz = capt.refreshHz()
	rep.Screen = size{Width: m.screen.X, Height: m.screen.Y}
	rep.Copy = copyInfo{Scale: roundTo(m.copy.scale(), 3), X: int(m.copy.X0), Y: int(m.copy.Y0)}
	rep.CopyMoves = m.moves
	rep.summarize()

	err = rep.writeJSON(w)
	if cerr := closeOut(err == nil); err == nil {
		err = cerr
	}
	if err != nil {
		return fail("writing the result: %v", err)
	}
	if rep.Interrupted {
		warn("interrupted after %d screenshots: the result is inconclusive", rep.Screenshots)
	}
	fmt.Fprintln(stderr, rep.summaryLine())
	return rep.ExitCode
}

// roundTo rounds x to n decimals.
func roundTo(x float64, n int) float64 {
	p := math.Pow(10, float64(n))
	return math.Round(x*p) / p
}
