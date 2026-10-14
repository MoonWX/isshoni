package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// The result of a run (02 §15.2): a human summary, and the JSON report
//
//	{"scenario", "params", "server": {"cpu": {avg, p95, max}, "rssPerDownTrack", "egressMbps", …},
//	 "loss": {"raw", "final", …}, "pli": {…}, "switchMs": {p50, p95, max, …}, "avOffsetMs": {p50, p99, …},
//	 "checks": […], "users": {…}, "pass": true, "failures": []}
//
// CPU and loss are in percent (12.5 is 12.5 %), RSS in bytes, rates in Mbit/s, times in milliseconds. "checks"
// lists every criterion with its value and limit; "checked": false means the scenario only reports it (or there
// was nothing to measure). "failures" says why a run did not pass: the checks that failed, what went wrong for a
// client, a setup error, or a teardown that left accounts behind. all-high adds "steps" and, when it found it,
// "knee".

// report is the JSON report.
type report struct {
	Scenario        string        `json:"scenario"`
	Params          params        `json:"params"`
	StartedAt       time.Time     `json:"startedAt,omitzero"` // when every client was up; the warm-up began
	MeasuredSeconds float64       `json:"measuredSeconds"`    // after the warm-up
	Server          serverReport  `json:"server"`
	Loss            lossReport    `json:"loss"`
	PLI             pliReport     `json:"pli"`
	SwitchMs        switchReport  `json:"switchMs"`
	AVOffsetMs      avReport      `json:"avOffsetMs"`
	Steps           []stepReport  `json:"steps,omitempty"` // all-high
	Knee            *stepReport   `json:"knee,omitempty"`  // all-high: the step that stopped it
	Checks          []checkReport `json:"checks"`
	Users           usersReport   `json:"users"`
	Resumes         int           `json:"resumes"` // signaling connections that dropped and resumed
	SetupError      string        `json:"setupError,omitempty"`
	Pass            bool          `json:"pass"`
	Failures        []string      `json:"failures"`
}

// params are the run's parameters, as flags take them.
type params struct {
	Server        string `json:"server"`
	Publishers    int    `json:"publishers"`
	Subscribers   int    `json:"subscribers"`
	Duration      string `json:"duration"`
	Warmup        string `json:"warmup"`
	Full          string `json:"full"`
	Preview       string `json:"preview"`
	GOP           string `json:"gop"`
	Profile       string `json:"profile"`
	Decodable     bool   `json:"decodable"`
	FocusRotate   string `json:"focusRotate"`
	LoginInterval string `json:"loginInterval"`
	KeepUsers     bool   `json:"keepUsers"`
}

// serverReport is what the dashboard said after the warm-up.
type serverReport struct {
	CPU                cpuReport `json:"cpu"`
	RSSPerDownTrack    float64   `json:"rssPerDownTrack"` // bytes: (RSS − baseline RSS) ÷ DownTracks, the worst sample
	BaselineRSS        int64     `json:"baselineRss"`
	PeakRSS            int64     `json:"peakRss"`
	DownTracks         int       `json:"downTracks"`
	EgressMbps         float64   `json:"egressMbps"`         // media.egressBps, the mean of the samples
	ExpectedEgressMbps float64   `json:"expectedEgressMbps"` // what the subscriptions take at the publishers' rates
	// ReceivedMbps is what the subscribers received over the measurement, payload and repairs: a check of EgressMbps
	// from the other end.
	ReceivedMbps float64 `json:"receivedMbps"`
	Samples      int     `json:"samples"`
}

// cpuReport is process CPU ÷ (wall × NumCPU) in percent: over the whole measurement, and per sample interval.
type cpuReport struct {
	Avg float64 `json:"avg"`
	P95 float64 `json:"p95"`
	Max float64 `json:"max"`
}

// lossReport is over every subscriber track, in percent of the expected packets.
type lossReport struct {
	Raw             float64 `json:"raw"`   // missing before NACK/RTX
	Final           float64 `json:"final"` // still missing after 1 s
	ExpectedPackets int     `json:"expectedPackets"`
	Tracks          int     `json:"tracks"`
	Duplicates      int     `json:"duplicates"`
}

// pliReport is the keyframe requests (PLI and FIR) the publishers' layers received.
type pliReport struct {
	MaxPerSecond float64          `json:"maxPerSecond"` // the worst layer's average
	MaxIn10s     int              `json:"maxIn10s"`     // the most one layer got in any 10 s
	Total        int              `json:"total"`
	Layers       []pliLayerReport `json:"layers"`
}

type pliLayerReport struct {
	Share     string  `json:"share"`
	Layer     string  `json:"layer"`
	Count     int     `json:"count"`
	PerSecond float64 `json:"perSecond"`
	MaxIn10s  int     `json:"maxIn10s"`
}

// switchReport is the latency of the layer switches of the focus moves: subscribe.update sent → first packet of
// the new layer.
type switchReport struct {
	Count           int     `json:"count"`
	P50             float64 `json:"p50"`
	P95             float64 `json:"p95"`
	Max             float64 `json:"max"`
	WithoutKeyframe int     `json:"withoutKeyframe"` // switches that did not start on a keyframe with its SPS
	LostStart       int     `json:"lostStart"`       // switches whose start was lost (counted as loss)
}

// avReport is |flash − beep| of the fake media at the viewers, through the SFU's sender reports.
type avReport struct {
	Count int     `json:"count"`
	P50   float64 `json:"p50"`
	P99   float64 `json:"p99"`
	Max   float64 `json:"max"`
}

// stepReport is one step of all-high.
type stepReport struct {
	High               int     `json:"high"` // full-layer subscriptions
	Seconds            float64 `json:"seconds"`
	EgressMbps         float64 `json:"egressMbps"`
	ExpectedEgressMbps float64 `json:"expectedEgressMbps"`
	CPUAvg             float64 `json:"cpuAvg"`
	CPUMax             float64 `json:"cpuMax"`
	LossRaw            float64 `json:"lossRaw"`
	LossFinal          float64 `json:"lossFinal"`
}

// checkReport is one criterion of the scenario.
type checkReport struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	Limit   string `json:"limit,omitempty"`
	Checked bool   `json:"checked"`
	Pass    bool   `json:"pass"`
}

// usersReport says what became of the run's accounts.
type usersReport struct {
	Created int  `json:"created"`
	Deleted int  `json:"deleted"`
	Kept    bool `json:"kept"` // -keep-users
}

// newReport returns a report with the run's parameters.
func newReport(cfg *config) *report {
	return &report{
		Scenario: cfg.scenario.name,
		Params: params{
			Server: cfg.server.String(), Publishers: cfg.publishers, Subscribers: cfg.subscribers,
			Duration: cfg.duration.String(), Warmup: cfg.warmup.String(), Full: formatLayer(cfg.layers[0]),
			Preview: formatLayer(cfg.layers[1]), GOP: cfg.gop.String(), Profile: cfg.profile, Decodable: cfg.decodable,
			FocusRotate: cfg.focusRotate.String(), LoginInterval: cfg.loginInterval.String(), KeepUsers: cfg.keepUsers,
		},
		Checks:   []checkReport{},
		Failures: []string{},
	}
}

// writeJSON writes the report to path, or to stdout for "-".
func writeJSON(path string, stdout io.Writer, rep *report) error {
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if path == "-" {
		_, err = stdout.Write(b)
		return err
	}
	return os.WriteFile(path, b, 0o644) //nolint:gosec // G306: a report, not a secret
}

// writeSummary writes the human summary.
func writeSummary(w io.Writer, rep *report) {
	verdict := "PASS"
	if !rep.Pass {
		verdict = "FAIL"
	}
	p := rep.Params
	fmt.Fprintf(w, "isshoni-loadtest %s: %s\n", rep.Scenario, verdict)
	video := "synthetic"
	if p.Decodable {
		video = "decodable"
	}
	fmt.Fprintf(w, "  publishers %d, subscribers %d, %s with a warm-up of %s, %s video f %s q %s\n", p.Publishers,
		p.Subscribers, p.Duration, p.Warmup, video, p.Full, p.Preview)
	for _, c := range rep.Checks {
		mark := "  "
		switch {
		case c.Checked && c.Pass:
			mark = "ok"
		case c.Checked:
			mark = "!!"
		}
		var limit string
		switch {
		case c.Checked:
			limit = "(" + c.Limit + ")"
		case c.Limit != "":
			limit = "(not checked: " + c.Limit + ")"
		default:
			limit = "(reported only)"
		}
		fmt.Fprintf(w, "  %s %-15s %s %s\n", mark, c.Name, c.Value, limit)
	}
	if rep.Server.ReceivedMbps > 0 {
		fmt.Fprintf(w, "     received        %.2f Mbps at the subscribers\n", rep.Server.ReceivedMbps)
	}
	for _, s := range rep.Steps {
		fmt.Fprintf(w, "  step: %3d full layers  %8.2f Mbps (expected %.2f)  cpu %5.1f%% (max %.1f%%)  loss %.3f%% (raw %.3f%%)\n",
			s.High, s.EgressMbps, s.ExpectedEgressMbps, s.CPUAvg, s.CPUMax, s.LossFinal, s.LossRaw)
	}
	if rep.Knee != nil {
		fmt.Fprintf(w, "  knee at %d full layers: %.2f Mbps, cpu %.1f%%, loss %.3f%%\n", rep.Knee.High, rep.Knee.EgressMbps,
			rep.Knee.CPUAvg, rep.Knee.LossFinal)
	}
	u := rep.Users
	switch {
	case u.Kept:
		fmt.Fprintf(w, "  accounts: %d created, kept (-keep-users)\n", u.Created)
	default:
		fmt.Fprintf(w, "  accounts: %d created, %d deleted\n", u.Created, u.Deleted)
	}
	if len(rep.Failures) > 0 {
		fmt.Fprintln(w, "  failures:")
		for _, f := range rep.Failures {
			fmt.Fprintf(w, "    - %s\n", strings.ReplaceAll(f, "\n", " "))
		}
	}
}
