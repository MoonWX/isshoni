package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/MoonWX/isshoni/internal/desktop/audio"
)

// This file is the portable runner (docs/m2/05-windows-audio.md §18.2, §18.5): it drives the engine driver over the
// cases, turns each recording into a verdict (verdict.go), assembles the report (report.go) and returns the exit
// code (gate.go). The engine work is the driver's; this file is OS-neutral.

// harnessVersion is set by the release build with -ldflags; a dev build falls back to the module's VCS revision.
var harnessVersion string

// runOptions are the parsed flags of `run` (05 §18.2).
type runOptions struct {
	cases    []string // explicit case names or numbers; empty = every case in the table
	gate     bool
	long     bool
	keepWAV  string
	outDir   string
	noRedact bool
}

// runCases is the body of the `run` subcommand. It returns the process exit code (05 §18.2's 0–4). A setup error
// (no engine, no DLL) is exit 4; otherwise the gate rule decides.
func runCases(w io.Writer, opts runOptions) int {
	drv, err := newDriver(driverOptions{keepWAV: opts.keepWAV, console: w})
	if err != nil {
		fmt.Fprintln(w, "setup error:", err)
		return exitSetup
	}
	defer func() { _ = drv.close() }()
	return runWith(w, drv, opts)
}

// runWith runs the selected cases on drv, redacts and writes the report, and prints the summary line.
func runWith(w io.Writer, drv driver, opts runOptions) int {
	dll, osi, mach, err := drv.env()
	if err != nil {
		fmt.Fprintln(w, "setup error:", err)
		return exitSetup
	}

	cfg := gateConfig{FastPath: shippedFastPath()}
	selected, err := selectCases(opts)
	if err != nil {
		fmt.Fprintln(w, "error:", err)
		return exitSetup
	}
	pre := preconditions{renderEndpoints: len(mach.RenderEndpoints), mic: mach.Mic}
	if pre.renderEndpoints == 0 {
		fmt.Fprintln(w, "note: no active render endpoint: every case is SKIP (05 §18.4)")
	} else if !pre.mic {
		fmt.Fprintln(w, "note: no microphone with desktop-app access: the mic cases are SKIP (05 §18.4)")
	}
	if opts.noRedact {
		fmt.Fprintln(w, "note: -no-redact: the report names apps and paths; it is for local debugging only and "+
			"must never be committed")
	}

	reports := runSelected(w, drv, selected, cfg, pre)
	exit := exitCode(reports)
	r := report{
		V: reportVersion, Harness: harnessVer(), DLL: dll, OS: osi, Machine: mach,
		Gate: opts.gate, GateCfg: cfg, Summary: summarize(reports, exit), Cases: reports,
	}
	if !opts.noRedact {
		newRedactor(drv.redaction(), currentLocalIDs()).report(&r)
	}
	if err := writeReport(w, r, opts.outDir, opts.noRedact); err != nil {
		fmt.Fprintln(w, "error: the report could not be written:", err)
		if exit == exitAllPass {
			exit = exitSetup
		}
	}
	fmt.Fprintln(w, summaryLine(r))
	return exit
}

// preconditions are the run-wide preconditions of 05 §18.4 that decide a SKIP before a case runs.
type preconditions struct {
	renderEndpoints int  // active render endpoints
	mic             bool // a microphone with desktop-app access
}

// baselineBroken is the problem every case after a failed case 1 carries (05 §18.5).
const baselineBroken = "case 1 (baseline-endpoint) failed: the audio setup is broken, not isshoni"

// runSelected runs the cases in order (05 §18.4, §18.5): without an active render endpoint every case is SKIP; a
// case that needs a microphone, or more endpoints than there are, is SKIP; once case 1 FAILs, every later case is
// INCONCLUSIVE without running. It names each case on w as it starts (the soak takes 30 minutes).
func runSelected(w io.Writer, drv driver, selected []caseSpec, cfg gateConfig, pre preconditions) []caseReport {
	reports := make([]caseReport, 0, len(selected))
	broken := false
	for _, cs := range selected {
		var cr caseReport
		switch {
		case pre.renderEndpoints == 0:
			cr = skipped(cs, cfg, "no active render endpoint")
		case broken:
			cr = inconclusive(cs, cfg, "%s", baselineBroken)
		case cs.minEndpoints > pre.renderEndpoints:
			cr = skipped(cs, cfg, fmt.Sprintf("needs %d active render endpoints, the machine has %d",
				cs.minEndpoints, pre.renderEndpoints))
		case cs.needsMic() && !pre.mic:
			cr = skipped(cs, cfg, "no microphone with desktop-app access (05 §18.4)")
		default:
			fmt.Fprintf(w, "case %d %s …\n", cs.num, cs.name)
			cr = runOne(drv, cs, cfg)
		}
		if cs.num == 1 && cr.Result == resultFail {
			broken = true
		}
		fmt.Fprintf(w, "case %d %s: %s\n", cs.num, cs.name, cr.Result)
		reports = append(reports, cr)
	}
	return reports
}

// runOne runs one case on drv and scores it; a case that could not run is INCONCLUSIVE.
func runOne(drv driver, cs caseSpec, cfg gateConfig) caseReport {
	if cs.special != "" {
		cr, err := drv.runSpecial(cs, cfg)
		if err != nil {
			return inconclusive(cs, cfg, "the case could not run: %s", err)
		}
		return withResult(cs, cfg, cr, cr.Result)
	}
	rec, err := drv.runCase(cs, cfg)
	if err != nil {
		return inconclusive(cs, cfg, "the case could not run: %s", err)
	}
	return evaluate(cs, rec, cfg)
}

// selectCases is the cases to run: the explicit -case list (by name or number), or every case in the table. A
// -long case (the soak) runs only with -long, and then last.
func selectCases(opts runOptions) ([]caseSpec, error) {
	if len(opts.cases) == 0 {
		var out, long []caseSpec
		for _, cs := range cases() {
			switch {
			case !cs.long:
				out = append(out, cs)
			case opts.long:
				long = append(long, cs)
			}
		}
		return append(out, long...), nil
	}
	var out []caseSpec
	for _, want := range opts.cases {
		want = strings.TrimSpace(want)
		if want == "" {
			continue
		}
		var cs caseSpec
		var ok bool
		if n, err := strconv.Atoi(want); err == nil {
			if cs, ok = caseByNum(n); !ok {
				return nil, fmt.Errorf("no case numbered %d", n)
			}
		} else if cs, ok = caseByName(want); !ok {
			return nil, fmt.Errorf("no case named %q", want)
		}
		if cs.long && !opts.long {
			return nil, fmt.Errorf("case %d (%s) runs only with -long", cs.num, cs.name)
		}
		out = append(out, cs)
	}
	return out, nil
}

// unredactedSuffix ends the name of a report written with -no-redact, so it is never mistaken for a gate report.
const unredactedSuffix = "-unredacted"

// writeReport writes r into outDir under 05 §18.7's name (default "."); a -no-redact report's name ends in
// "-unredacted". The name carries no machine identifier.
func writeReport(w io.Writer, r report, outDir string, unredacted bool) error {
	if outDir == "" {
		outDir = "."
	}
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return err
	}
	b, err := encodeReport(r)
	if err != nil {
		return err
	}
	stamp := time.Now().Format("20060102-1504")
	name := reportName(r.OS.Product, r.OS.Build, stamp)
	if unredacted {
		name = strings.TrimSuffix(name, ".json") + unredactedSuffix + ".json"
	}
	path := filepath.Join(outDir, name)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return err
	}
	fmt.Fprintln(w, "report:", path)
	return nil
}

// shippedFastPath is the release's shipped default of audio.fastPath, which the gate configuration follows
// (05 §18.10). It is the default of internal/desktop/audio's settings.
func shippedFastPath() bool { return audio.DefaultSettings().FastPath }

// harnessVer is the harness version for the report (05 §18.7): the ldflags value, else the module's VCS revision,
// else "dev".
func harnessVer() string {
	if harnessVersion != "" {
		return harnessVersion
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				rev := s.Value
				if len(rev) > 12 {
					rev = rev[:12]
				}
				return "0.0.0-g" + rev
			}
		}
	}
	return "dev"
}
