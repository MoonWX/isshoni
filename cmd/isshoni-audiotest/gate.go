package main

// This file is the gate configuration and the exit-code rule (docs/m2/05-windows-audio.md §18.2, §18.10). It is
// portable and the acceptance tests cover it: with the shipped default audio.fastPath false, cases 4, 5 and 26 run
// as INFO and never change the exit code.

// requiredUnder reports whether cs counts toward the gate exit code under cfg (05 §18.10). A fast-path case counts
// only when the gate ships audio.fastPath on; INFO and tier cases never count; a mic case counts but SKIPs without a
// microphone (WA62b).
func (cs caseSpec) requiredUnder(cfg gateConfig) bool {
	switch cs.req {
	case reqYes, reqMic:
		return true
	case reqFastPath:
		return cfg.FastPath
	default: // reqInfo, reqTierMinor
		return false
	}
}

// effectiveResult is cs's result as it counts for the gate: a fast-path case that the gate does not require is
// reported as INFO whatever it measured (05 §18.10: "they still run … as INFO and never change the exit code").
func effectiveResult(cs caseSpec, cfg gateConfig, result string) string {
	if !cs.requiredUnder(cfg) && cs.req == reqFastPath {
		return resultInfo
	}
	return result
}

// exitCode is the gate exit code over the case reports (05 §18.2): 0 all required cases PASS; 1 a required FAIL;
// 3 a required case SKIPPED (the gate is incomplete); 2 no required FAIL or SKIP but a required INCONCLUSIVE. Only
// cases marked required contribute; INFO and optional cases run but never change it. setupErr (exit 4) is handled
// by the caller before any case runs.
func exitCode(reports []caseReport) int {
	var fail, skip, inconclusive bool
	for _, r := range reports {
		if !r.Required {
			continue
		}
		switch r.Result {
		case resultFail:
			fail = true
		case resultSkip:
			skip = true
		case resultInconclusive:
			inconclusive = true
		}
	}
	switch {
	case fail:
		return 1
	case skip:
		return 3
	case inconclusive:
		return 2
	default:
		return 0
	}
}

// The process exit codes of isshoni-audiotest (05 §18.2; docs/m2/README.md §4): 0 all required PASS, 1 a FAIL,
// 2 INCONCLUSIVE (no FAIL), 3 a required case SKIPPED, 4 a setup error (DLL missing, wrong ABI, no endpoint).
const (
	exitAllPass      = 0
	exitFail         = 1
	exitInconclusive = 2
	exitSkipped      = 3
	exitSetup        = 4
)
