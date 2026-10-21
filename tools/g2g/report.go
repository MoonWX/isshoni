package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"time"
)

// The criterion (01 D17, §12.2): p95 under 200 ms, with at least 90 % of the screenshots decoded.
const (
	p95LimitMs    = 200
	minDecodedPct = 90
)

// The runs of the exit run book (01 §12.2): the gate, and the two diagnostic runs. The operator names the run;
// the tool doesn't check it.
var runNames = []string{"gate", "nosound", "nocuts"}

// Results; judge gives each its exit status.
const (
	resultPass         = "pass"
	resultFail         = "fail"
	resultInconclusive = "inconclusive"
)

// report is the JSON file that measure writes. It names no host, user, machine or path: the exit-test record it
// goes into is in a public repository. The server's address is used for the RTT and its version only.
type report struct {
	Tool         string    `json:"tool"`        // "g2g"
	Run          string    `json:"run"`         // gate, nosound or nocuts
	StartedAt    string    `json:"startedAt"`   // the measurement's start, UTC
	DurationMs   int64     `json:"durationMs"`  // as measured
	IntervalMs   int64     `json:"intervalMs"`  // between two screenshots
	Screenshots  int       `json:"screenshots"` // taken during the measurement (not the calibration)
	Samples      int       `json:"samples"`     // screenshots decoded into a latency
	DecodedPct   float64   `json:"decodedPct"`
	Failures     failures  `json:"decodeFailures"`
	Transitions  int       `json:"transitions"` // samples with a strip caught between two frames
	CopyMoves    int       `json:"copyMoves"`   // times the copy was found again after it moved by over half a cell
	LatencyMs    *latency  `json:"latencyMs"`   // null without samples
	RefreshHz    int       `json:"refreshHz"`   // the monitor's refresh rate, 0 when unknown
	Screen       size      `json:"screen"`
	Copy         copyInfo  `json:"copy"`
	RTTMs        *float64  `json:"rttMs"`                  // median TCP connect time to the server; null without -server
	RTTSamplesMs []float64 `json:"rttSamplesMs,omitempty"` // the five connects
	Versions     versions  `json:"versions"`
	Criterion    criterion `json:"criterion"`
	Result       string    `json:"result"`   // pass, fail or inconclusive
	ExitCode     int       `json:"exitCode"` // 0, 1 or 2, as the result
	Interrupted  bool      `json:"interrupted,omitempty"`
	Series       []point   `json:"series"` // every screenshot, in order
}

type failures struct {
	Total       int `json:"total"`
	Capture     int `json:"capture"`
	Clock       int `json:"clock"`
	Copy        int `json:"copy"`
	Implausible int `json:"implausible"`
}

type latency struct {
	P50 int `json:"p50"`
	P95 int `json:"p95"`
	Max int `json:"max"`
}

type size struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// copyInfo is where the tracked copy of the strip was at the end, and its scale.
type copyInfo struct {
	Scale float64 `json:"scale"`
	X     int     `json:"x"`
	Y     int     `json:"y"`
}

type versions struct {
	G2G    string `json:"g2g"`
	Go     string `json:"go"`
	OS     string `json:"os"`
	Server string `json:"server,omitempty"` // from the server's /api/v1/info, with -server
}

type criterion struct {
	P95BelowMs    int `json:"p95BelowMs"`
	MinDecodedPct int `json:"minDecodedPct"`
}

// summarize fills the report's statistics and result from its series.
func (r *report) summarize() {
	r.Screenshots = len(r.Series)
	r.Samples, r.Transitions = 0, 0
	r.Failures = failures{}
	var deltas []int
	for _, p := range r.Series {
		if p.DeltaMs != nil {
			deltas = append(deltas, *p.DeltaMs)
			if p.Transition {
				r.Transitions++
			}
			continue
		}
		r.Failures.Total++
		switch p.Fail {
		case failCapture:
			r.Failures.Capture++
		case failClock:
			r.Failures.Clock++
		case failCopy:
			r.Failures.Copy++
		default:
			r.Failures.Implausible++
		}
	}
	r.Samples = len(deltas)
	r.DecodedPct = 0
	if r.Screenshots > 0 {
		r.DecodedPct = math.Round(1000*float64(r.Samples)/float64(r.Screenshots)) / 10
	}
	r.LatencyMs = nil
	if len(deltas) > 0 {
		slices.Sort(deltas)
		r.LatencyMs = &latency{P50: percentile(deltas, 50), P95: percentile(deltas, 95), Max: deltas[len(deltas)-1]}
	}
	r.Criterion = criterion{P95BelowMs: p95LimitMs, MinDecodedPct: minDecodedPct}
	r.Result, r.ExitCode = judge(r.Samples, r.Screenshots, r.LatencyMs, r.Interrupted)
}

// judge applies the criterion (01 §12.2): pass (exit 0) when p95 < 200 ms with at least 90 % of the screenshots
// decoded, fail (exit 1) when the latency misses, inconclusive (exit 2) when too few decoded or the run was cut
// short. Every run is judged so; only the gate run decides the exit criterion.
func judge(samples, screenshots int, l *latency, interrupted bool) (string, int) {
	switch {
	case interrupted || screenshots == 0 || l == nil || 100*samples < minDecodedPct*screenshots:
		return resultInconclusive, exitInconclusive
	case l.P95 < p95LimitMs:
		return resultPass, exitOK
	default:
		return resultFail, exitLatency
	}
}

// percentile is the nearest-rank percentile p of sorted values.
func percentile(sorted []int, p float64) int {
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	return sorted[min(max(rank, 1), len(sorted))-1]
}

// writeJSON writes the report, indented, with a final newline.
func (r *report) writeJSON(w io.Writer) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// summaryLine is the one-line result for the terminal.
func (r *report) summaryLine() string {
	l := "no samples"
	if r.LatencyMs != nil {
		l = fmt.Sprintf("p50 %d ms, p95 %d ms, max %d ms", r.LatencyMs.P50, r.LatencyMs.P95, r.LatencyMs.Max)
	}
	rtt := ""
	if r.RTTMs != nil {
		rtt = fmt.Sprintf(", RTT %.1f ms", *r.RTTMs)
	}
	return fmt.Sprintf("g2g: run %s: %d of %d screenshots decoded (%.1f %%), %s%s: %s (exit %d)",
		r.Run, r.Samples, r.Screenshots, r.DecodedPct, l, rtt, r.Result, r.ExitCode)
}

// version is the tool's version, stamped by a release build: -ldflags "-X main.version=v0.2.0".
var version string

// toolVersion is version, or else the module version the go command recorded (a VCS pseudo-version, or
// "(devel)").
func toolVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "(devel)"
}

func currentVersions() versions {
	return versions{G2G: toolVersion(), Go: runtime.Version(), OS: osVersion()}
}

// Server measurements (with -server).
const (
	rttConnects    = 5
	connectTimeout = 3 * time.Second
	infoTimeout    = 5 * time.Second
	maxInfoBytes   = 1 << 20
)

// parseServer checks -server: an http or https URL with a host and nothing else secret in it.
func parseServer(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("-server: %w", err)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil {
		return nil, errors.New("-server must be the server's URL, like https://isshoni.example.com")
	}
	return u, nil
}

// measureRTT returns the TCP connect times to the server's port (443 for https), five times, after resolving its
// name once, so DNS is not in them (01 §12.2).
func measureRTT(ctx context.Context, u *url.URL) ([]float64, error) {
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	addrs, err := net.DefaultResolver.LookupHost(ctx, u.Hostname())
	if err != nil {
		return nil, fmt.Errorf("resolving the server's name: %w", err)
	}
	if len(addrs) == 0 {
		return nil, errors.New("the server's name resolves to no address")
	}
	d := net.Dialer{Timeout: connectTimeout}
	addr := net.JoinHostPort(addrs[0], port)
	var ms []float64
	for range rttConnects {
		t0 := time.Now()
		c, err := d.DialContext(ctx, "tcp", addr)
		el := time.Since(t0)
		if err != nil {
			return nil, fmt.Errorf("connecting to the server: %w", err)
		}
		_ = c.Close()
		ms = append(ms, math.Round(float64(el.Microseconds())/100)/10)
	}
	return ms, nil
}

// medianFloat is the median of a non-empty slice (the mean of the middle two for an even count).
func medianFloat(v []float64) float64 {
	s := slices.Clone(v)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// serverVersion reads the server's version from its public GET /api/v1/info (server.version).
func serverVersion(ctx context.Context, u *url.URL) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, infoTimeout)
	defer cancel()
	infoURL := strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/") + "/api/v1/info"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, infoURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("reading the server's version: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("reading the server's version: HTTP %d", res.StatusCode)
	}
	var info struct {
		Server struct {
			Version string `json:"version"`
		} `json:"server"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, maxInfoBytes)).Decode(&info); err != nil {
		return "", fmt.Errorf("reading the server's version: %w", err)
	}
	if info.Server.Version == "" {
		return "", errors.New("reading the server's version: the info has none")
	}
	return info.Server.Version, nil
}
