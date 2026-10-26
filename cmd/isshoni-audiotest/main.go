// Command isshoni-audiotest is the M2 audio exit gate (docs/m2/05-windows-audio.md §18): it drives the Windows
// audio engine (isshoni-media.dll) through the desktop policy service, plays tones from fixtures started under
// explorer.exe, records the PCM tap and proves that friends hear everything except the apps that should be kept out.
// The analyzer, runner, report and gate are portable and tested on every OS (05 §19.1); the cases run on Windows.
//
// This binary is also every fixture: a case starts renamed copies of it that play tones (the `fixture` subcommand).
// It is not part of any installer; for a release, 06's release.yml builds it next to the DLL as the workflow artifact
// isshoni-exit-tools (05 §18.1).
//
// WA62a built the runner, the analyzer, the report schema, selfcheck, apps, `run -gate` with the gate configuration
// and the cases that need no microphone (1–8, 10, 11, 15–21, 24–27). WA62b added the mic cases 9 and 12–14, the
// multi-endpoint case 22, the soak (case 23, `run -long`), `record` and `merge`, the report's redaction and the
// README (how to run it, and the results table).
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

const usage = `isshoni-audiotest — the isshoni M2 audio exit gate (docs/m2/05-windows-audio.md §18)

  selfcheck                              load the DLL, check the exports and probe, run the analyzer; no sound
  probe                                  print caps, the startup probe, endpoints (redacted)
  apps                                   print "What friends hear" with the built-in rules, as text
  run [-case a,b] [-gate] [-long]        run cases (§18.5); -gate uses the gate configuration (§18.10); -long adds
      [-keep-wav dir] [-out dir]         the 30-minute soak; -no-redact is for local debugging only
      [-no-redact]
  record -label R1 [-d 30] [-o r1.wav]   real-app check (§18.9): record, then answer Pass? [y/n]; stores
      [-exclude exe,…]                   manual-<label>.json next to the WAV
  merge [-o out.json] report.json        add the manual entries to a gate report (refuses another OS build)
      manual-*.json

Exit codes: 0 all required cases PASS · 1 a FAIL · 2 INCONCLUSIVE (no FAIL) · 3 a required case SKIPPED · 4 setup error
(record: 0 pass, 1 fail, 4 error; merge: 0 merged, 4 refused)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(exitSetup)
	}
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "selfcheck":
		os.Exit(cmdSelfcheck(os.Stdout))
	case "probe":
		os.Exit(cmdProbe(os.Stdout))
	case "apps":
		os.Exit(cmdApps(os.Stdout))
	case "run":
		os.Exit(cmdRun(os.Stdout, args))
	case "record":
		os.Exit(cmdRecord(os.Stdout, os.Stdin, args))
	case "merge":
		os.Exit(cmdMerge(os.Stdout, args))
	case "fixture":
		os.Exit(runFixture(args))
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(exitSetup)
	}
}

// cmdSelfcheck runs the portable analyzer self-test, then the engine self-check (Windows). On an OS without the
// engine it runs the analyzer only and skips the engine part; on Windows a DLL that doesn't load (missing, wrong
// ABI, an export missing, the test hooks unresolved) fails it with exit 1 (05 §18.2 runs it on CI Windows).
func cmdSelfcheck(w io.Writer) int {
	return selfcheckWith(w, newDriver)
}

// selfcheckWith is cmdSelfcheck with the driver constructor injected (tests).
func selfcheckWith(w io.Writer, open func(driverOptions) (driver, error)) int {
	fmt.Fprintln(w, "isshoni-audiotest selfcheck")
	if err := analyzerSelfCheck(w); err != nil {
		fmt.Fprintln(w, "analyzer self-check failed:", err)
		return exitFail
	}
	drv, err := open(driverOptions{})
	if errors.Is(err, errUnsupportedOS) {
		fmt.Fprintln(w, "  engine self-check skipped:", err)
		return exitAllPass
	}
	if err != nil {
		fmt.Fprintln(w, "engine self-check failed:", err)
		return exitFail
	}
	defer func() { _ = drv.close() }()
	if err := drv.selfCheck(w); err != nil {
		fmt.Fprintln(w, "engine self-check failed:", err)
		return exitFail
	}
	fmt.Fprintln(w, "selfcheck: ok")
	return exitAllPass
}

// cmdProbe prints the engine's caps and startup probe (05 §18.2); it needs the engine.
func cmdProbe(w io.Writer) int {
	drv, err := newDriver(driverOptions{})
	if err != nil {
		fmt.Fprintln(w, "error:", err)
		return exitSetup
	}
	defer func() { _ = drv.close() }()
	if err := drv.probe(w); err != nil {
		fmt.Fprintln(w, "error:", err)
		return exitSetup
	}
	return exitAllPass
}

// cmdApps prints "What friends hear" with the built-in rules (05 §18.2); it needs the engine.
func cmdApps(w io.Writer) int {
	drv, err := newDriver(driverOptions{})
	if err != nil {
		fmt.Fprintln(w, "error:", err)
		return exitSetup
	}
	defer func() { _ = drv.close() }()
	if err := drv.apps(w); err != nil {
		fmt.Fprintln(w, "error:", err)
		return exitSetup
	}
	return exitAllPass
}

// cmdRun parses `run`'s flags and runs the cases.
func cmdRun(w io.Writer, args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(w)
	caseList := fs.String("case", "", "comma-separated case names or numbers to run (default: every case)")
	gate := fs.Bool("gate", false, "run with the gate configuration (§18.10)")
	long := fs.Bool("long", false, "also run the 30-minute soak (case 23)")
	keepWAV := fs.String("keep-wav", "", "keep the captured tap WAVs in this folder")
	outDir := fs.String("out", ".", "the folder the report is written into (§18.7)")
	noRedact := fs.Bool("no-redact", false, "do not redact the report (local debugging only)")
	if err := fs.Parse(args); err != nil {
		return exitSetup
	}
	var list []string
	if *caseList != "" {
		list = strings.Split(*caseList, ",")
	}
	return runCases(w, runOptions{
		cases: list, gate: *gate, long: *long, keepWAV: *keepWAV, outDir: *outDir, noRedact: *noRedact,
	})
}
