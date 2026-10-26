package main

import (
	"fmt"
	"strings"
)

// This file is the audiotest report schema, redaction and summary line (docs/m2/05-windows-audio.md §18.7). It is
// portable: the report is built on every OS and written by run.go. The JSON field names and order are §18.7's.
//
// Redaction (the default; -no-redact is for local debugging only): the report carries no user, host or device
// friendly names, no absolute paths, and no non-fixture app names. Fixture app ids become "fixture:<role>" and any
// other app "other". The redaction of names collected during a run (record/merge) is WA62b's; WA62a redacts what the
// cases it runs produce.

// reportVersion is the schema version written into the report's "v".
const reportVersion = 1

// report is the top-level audiotest report (05 §18.7, schema v1).
type report struct {
	V       int           `json:"v"`
	Harness string        `json:"harness"` // the harness version (git describe), e.g. "v0.2.0-14-g1a2b3c4"
	DLL     dllInfo       `json:"dll"`
	OS      osInfo        `json:"os"`
	Machine machineInfo   `json:"machine"`
	Gate    bool          `json:"gate"`       // a -gate run
	GateCfg gateConfig    `json:"gateConfig"` // §18.10's gate configuration
	Summary summary       `json:"summary"`
	Cases   []caseReport  `json:"cases"`
	Manual  []manualEntry `json:"manual,omitempty"` // added by WA62b's merge
}

// dllInfo is the report's "dll": the engine DLL under test. sha256 is compared with native-manifest.json for the
// gate (05 §18.1); an empty version/sha means no DLL was loaded (a non-Windows build).
type dllInfo struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
}

// osInfo is the report's "os".
type osInfo struct {
	Product string `json:"product"` // "win10" | "win11" | the GOOS on a non-Windows diagnostic run
	Build   int    `json:"build"`
	UBR     int    `json:"ubr"`
	Edition string `json:"edition"`
}

// machineInfo is the report's "machine": recorded, not enforced (05 §18.4). Endpoint names are already
// {formFactor, bus}: no friendly names.
type machineInfo struct {
	CPU               string         `json:"cpu"`
	RenderEndpoints   []endpointInfo `json:"renderEndpoints"`
	Mic               bool           `json:"mic"`
	MasterVolume      float64        `json:"masterVolume"`
	Muted             bool           `json:"muted"`
	DuckingPreference int            `json:"duckingPreference"`
}

// endpointInfo is one render endpoint, redacted to its form factor and bus (05 §18.7).
type endpointInfo struct {
	FormFactor string `json:"formFactor"`
	Bus        string `json:"bus"`
}

// gateConfig is the gate configuration recorded as the report's "gateConfig" (05 §18.10): the shipped default of
// audio.fastPath under which the run was gated. With FastPath true, cases 4, 5 and 26 are required; with it false
// they run as INFO.
type gateConfig struct {
	FastPath bool `json:"fastPath"`
}

// summary is the report's "summary": the case tallies and the exit code.
type summary struct {
	Pass         int `json:"pass"`
	Fail         int `json:"fail"`
	Inconclusive int `json:"inconclusive"`
	Skip         int `json:"skip"`
	Info         int `json:"info"`
	Exit         int `json:"exit"`
}

// caseReport is one case's result (05 §18.7).
type caseReport struct {
	Name     string             `json:"name"`
	Required bool               `json:"required"` // under the active gate configuration
	Result   string             `json:"result"`   // resultPass | resultFail | resultInconclusive | resultSkip | resultInfo
	Checks   []checkReport      `json:"checks"`
	Measured map[string]measure `json:"measured"`
	ClicksAt []float64          `json:"clicksAtS"`
	Problems []string           `json:"problems"`
	Engine   []string           `json:"engine"`
	Counters map[string]int     `json:"counters"`
	Events   []reportEvent      `json:"events"`
}

// checkReport is one frequency check of a case (05 §18.7). MaxShortDB/MaxShortAtS are the short-window rule's
// maximum and where it was (absent checks only); To 0 means the end of the recording.
type checkReport struct {
	Freq        float64 `json:"freq"`
	From        float64 `json:"from"`
	To          float64 `json:"to"`
	Expect      string  `json:"expect"` // "present" | "absent" | "info"
	LevelDB     float64 `json:"levelDb"`
	MaxShortDB  float64 `json:"maxShortDb,omitempty"`
	MaxShortAtS float64 `json:"maxShortAtS,omitempty"`
	Pass        bool    `json:"pass"`
}

// measure is one event-relative value with its budget (05 §18.7): cutMs (case 15), onsetMs (cases 8, 13, 25),
// sessionLagMs (case 5), fastpathExitMs (case 5).
type measure struct {
	ValueMs  int `json:"valueMs"`
	BudgetMs int `json:"budgetMs"`
}

// reportEvent is one engine event in a case's report (05 §18.7): the time in ms from the recording start, the code,
// and the (redacted) app.
type reportEvent struct {
	TMs  int    `json:"tMs"`
	Code string `json:"code"`
	App  string `json:"app"`
}

// manualEntry is one manual real-app check merged into a gate report (05 §18.7; WA62b's merge).
type manualEntry struct {
	Label string `json:"label"`
	Pass  bool   `json:"pass"`
}

// The case result strings (05 §18.6).
const (
	resultPass         = "PASS"
	resultFail         = "FAIL"
	resultInconclusive = "INCONCLUSIVE"
	resultSkip         = "SKIP"
	resultInfo         = "INFO"
)

// The measured value keys and their budgets in ms (05 §17, §18.7).
const (
	measureCut          = "cutMs"
	measureOnset        = "onsetMs"
	measureSessionLag   = "sessionLagMs"
	measureFastpathExit = "fastpathExitMs"

	budgetCutMs          = 100 // a newly kept-out app silent (§17)
	budgetOnsetMs        = 500 // a new app heard (§17)
	budgetSessionLagMs   = 20  // the fast path's cut margin (§6.4)
	budgetFastpathExitMs = 15  // the fast-path exit decision (§17)
)

// fixtureRedacted is a fixture app's redacted id ("fixture:<role>"); otherRedacted is any non-fixture app.
const (
	fixturePrefix = "fixture:"
	otherRedacted = "other"
)

// summarize fills the summary tallies from the case reports; exit is set by the caller (the gate logic).
func summarize(cases []caseReport, exit int) summary {
	var s summary
	for _, c := range cases {
		switch c.Result {
		case resultPass:
			s.Pass++
		case resultFail:
			s.Fail++
		case resultInconclusive:
			s.Inconclusive++
		case resultSkip:
			s.Skip++
		case resultInfo:
			s.Info++
		}
	}
	s.Exit = exit
	return s
}

// reportName is the report's file name under -out (05 §18.7): audiotest-<product>-<build>-<stamp>.json.
func reportName(product string, build int, stamp string) string {
	return fmt.Sprintf("audiotest-%s-%d-%s.json", product, build, stamp)
}

// summaryLine is the one line the harness prints after a run (05 §18.7), for example:
//
//	isshoni-audiotest v0.2.0-14-g1a2b3c4 · DLL 0.2.0 sha256:3f9a1c0b7d2e · Windows 11 Pro 26200.6899 · 24/24 PASS · 0 FAIL · 0 INCONCLUSIVE · 0 SKIP · gate=PASS
//
// The sha256 is shortened to 12 hex; gate= is PASS, FAIL or absent for a non-gate run.
func summaryLine(r report) string {
	total := r.Summary.Pass + r.Summary.Fail + r.Summary.Inconclusive + r.Summary.Skip + r.Summary.Info
	sha := r.DLL.SHA256
	if len(sha) > 12 {
		sha = sha[:12]
	}
	osName := osProductLabel(r.OS)
	var b strings.Builder
	fmt.Fprintf(&b, "isshoni-audiotest %s", r.Harness)
	if r.DLL.Version != "" {
		fmt.Fprintf(&b, " · DLL %s sha256:%s", r.DLL.Version, sha)
	}
	fmt.Fprintf(&b, " · %s · %d/%d PASS · %d FAIL · %d INCONCLUSIVE · %d SKIP",
		osName, r.Summary.Pass, total, r.Summary.Fail, r.Summary.Inconclusive, r.Summary.Skip)
	if r.Summary.Info > 0 {
		fmt.Fprintf(&b, " · %d INFO", r.Summary.Info)
	}
	if r.Gate {
		gate := "PASS"
		if r.Summary.Exit != 0 {
			gate = "FAIL"
		}
		fmt.Fprintf(&b, " · gate=%s", gate)
	}
	return b.String()
}

// osProductLabel is a short OS label for the summary line (no machine name): "Windows 11 Pro 26200.6899", or the
// GOOS for a non-Windows diagnostic run.
func osProductLabel(o osInfo) string {
	switch o.Product {
	case "win11", "win10":
		name := "Windows 11"
		if o.Product == "win10" {
			name = "Windows 10"
		}
		if o.Edition != "" {
			name += " " + o.Edition
		}
		if o.Build != 0 {
			name += fmt.Sprintf(" %d.%d", o.Build, o.UBR)
		}
		return name
	case "":
		return "unknown OS"
	default:
		return o.Product
	}
}

// redactAppID maps an app id to its redacted form (05 §18.7): a fixture id stays "fixture:<role>", every other app
// is "other". A fixture role is recognized by the "fixture:" prefix the driver assigns.
func redactAppID(id string) string {
	if strings.HasPrefix(id, fixturePrefix) {
		return id
	}
	return otherRedacted
}
