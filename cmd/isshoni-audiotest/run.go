package main

import (
	"encoding/json"
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
// (no engine, no DLL, no endpoint) is exit 4; otherwise the gate rule decides.
func runCases(w io.Writer, opts runOptions) int {
	drv, err := newDriver(driverOptions{keepWAV: opts.keepWAV})
	if err != nil {
		fmt.Fprintln(w, "setup error:", err)
		return exitSetup
	}
	defer func() { _ = drv.close() }()

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
	if opts.long {
		fmt.Fprintln(w, "note: -long (soak) cases are added by WA62b and are not in this build")
	}

	reports := make([]caseReport, 0, len(selected))
	for _, cs := range selected {
		var cr caseReport
		if cs.special != "" {
			cr, err = drv.runSpecial(cs, cfg)
			if err != nil {
				cr = inconclusive(cs, cfg, "the case could not run: %s", err)
			}
		} else {
			rec, rerr := drv.runCase(cs, cfg)
			if rerr != nil {
				cr = inconclusive(cs, cfg, "the case could not run: %s", rerr)
			} else {
				cr = evaluate(cs, rec, cfg)
			}
		}
		reports = append(reports, cr)
	}

	exit := exitCode(reports)
	r := report{
		V: reportVersion, Harness: harnessVer(), DLL: dll, OS: osi, Machine: mach,
		Gate: opts.gate, GateCfg: cfg, Summary: summarize(reports, exit), Cases: reports,
	}
	if err := writeReport(w, r, opts.outDir); err != nil {
		fmt.Fprintln(w, "error: the report could not be written:", err)
		if exit == exitAllPass {
			exit = exitSetup
		}
	}
	fmt.Fprintln(w, summaryLine(r))
	return exit
}

// selectCases is the cases to run: the explicit -case list (by name or number), or every case in the table.
func selectCases(opts runOptions) ([]caseSpec, error) {
	if len(opts.cases) == 0 {
		return cases(), nil
	}
	var out []caseSpec
	for _, want := range opts.cases {
		want = strings.TrimSpace(want)
		if want == "" {
			continue
		}
		if n, err := strconv.Atoi(want); err == nil {
			cs, ok := caseByNum(n)
			if !ok {
				return nil, fmt.Errorf("no case numbered %d (WA62b adds 9, 12–14, 22, 23)", n)
			}
			out = append(out, cs)
			continue
		}
		cs, ok := caseByName(want)
		if !ok {
			return nil, fmt.Errorf("no case named %q", want)
		}
		out = append(out, cs)
	}
	return out, nil
}

// writeReport writes r into outDir under 05 §18.7's name (default "."). The name carries no machine identifier.
func writeReport(w io.Writer, r report, outDir string) error {
	if outDir == "" {
		outDir = "."
	}
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	stamp := time.Now().Format("20060102-1504")
	name := reportName(r.OS.Product, r.OS.Build, stamp)
	path := filepath.Join(outDir, name)
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
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
