package main

import (
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The session's time marks (06 §12.2), as offsets from T+0.
const (
	markCollectorStart = -15 * time.Minute // the collector starts at T-15
	markRestart        = 60 * time.Minute  // A restarts the server
	markRestartSlack   = 5 * time.Minute   // a restart this close to T+60 is the scripted one
	// markCoverSlack: a recording that starts by T+2 and ends at T+118 or later covers the session. T+0 is often
	// taken from the restart, which the collector sees some seconds after A typed the command, and A stops the
	// collector at about T+120.
	markCoverSlack = 2 * time.Minute
	markMemFrom    = 65 * time.Minute  // RSS growth is measured from T+65 ...
	markEnd        = 120 * time.Minute // ... to T+120, the end
	markFirstFrom  = 5 * time.Minute   // the first process's RSS, T+5 to T+59
	markFirstTo    = 59 * time.Minute
	markBaseline   = 85 * time.Minute // goroutines before the stress
	markStressFrom = 90 * time.Minute
	markStressTo   = 100 * time.Minute
)

// The pass limits of 06 §12.3.
const (
	limitCPUAvg      = 50.0 // percent of all CPUs
	limitCPUPeak     = 80.0
	limitRSSGrowth   = 20.0   // percent
	limitUDPErrRatio = 0.0001 // 0.01% of datagrams
	limitFreezeFail  = 3.0    // percent: above the phones' limit, which is the larger, the session failed
	goroutineSlack   = 0.10   // "back to the baseline": at most 10% above it
)

// hostCounters are the machine's counters (since boot); the scraped counters are the metric columns whose name
// ends in _total (since the process started).
var hostCounters = []string{
	colNICRx, colNICTx, colUDPIn, colUDPOut, colUDPRcvbufErrs, colUDPSndbufErrs, colUDPInErrs, colTCPRetrans,
}

func isMetricCounter(col string) bool { return strings.HasSuffix(col, "_total") }

// counterAcc adds up one counter's increases.
type counterAcc struct {
	total float64
	peak  float64 // the largest increase per second between two rows of the same process, without a reset
	seen  bool
}

func (a *counterAcc) add(inc float64, dt time.Duration, forPeak bool) {
	a.total += inc
	a.seen = true
	if forPeak && dt > 0 {
		a.peak = max(a.peak, inc/dt.Seconds())
	}
}

// segment is one isshoni process: the rows between two PID changes.
type segment struct {
	n         int
	pid       int
	from, to  time.Time // its first and last row with this PID
	restarted bool      // it began with a PID change, so its counters started at 0 inside the recording
	samples   []*runRow
	scrapes   []*runRow
	host      map[string]*counterAcc
	metric    map[string]*counterAcc
}

func (s *segment) contains(t time.Time) bool { return !t.Before(s.from) && !t.After(s.to) }

// analysis is everything summarize prints.
type analysis struct {
	rows        []*runRow
	skipped     []string
	first, last time.Time
	t0          time.Time
	anchor      string
	segs        []*segment
	interval    time.Duration // the median time between two samples
	gaps        int
	longestGap  time.Duration
	starts      int // start rows: how often the collector started
	samples     int
	downs       int
	scrapes     int
	scrapeErrs  int
	host        map[string]*counterAcc // every segment together, plus what came before the first
	metric      map[string]*counterAcc
}

// analyze splits the run into processes, adds up the counters and fixes T+0.
func analyze(rf *runFile, t0 string) (*analysis, error) {
	if len(rf.rows) == 0 {
		return nil, errors.New("no rows")
	}
	a := &analysis{
		rows: rf.rows, skipped: rf.skipped, first: rf.rows[0].t, last: rf.rows[len(rf.rows)-1].t,
		host: make(map[string]*counterAcc), metric: make(map[string]*counterAcc),
	}
	a.split()
	if len(a.segs) == 0 {
		return nil, errors.New("no sample of an isshoni process")
	}
	if err := a.setT0(t0); err != nil {
		return nil, err
	}
	return a, nil
}

// split assigns the rows to processes and adds up the counters, in one pass in the order of the file.
func (a *analysis) split() {
	var cur *segment
	newSeg := func(r *runRow, restarted bool) {
		cur = &segment{
			n: len(a.segs) + 1, pid: r.pid, from: r.t, to: r.t, restarted: restarted,
			host: make(map[string]*counterAcc), metric: make(map[string]*counterAcc),
		}
		a.segs = append(a.segs, cur)
	}
	type last struct {
		v   float64
		t   time.Time
		seg *segment
	}
	prevHost := make(map[string]last)
	prevMetric := make(map[string]last)
	var sampleTimes []time.Time

	for _, r := range a.rows {
		switch r.kind {
		case kindStart:
			a.starts++
		case kindRestart:
			if r.pid != 0 {
				newSeg(r, true)
			}
		case kindSample, kindScrape:
			if r.pid == 0 {
				break
			}
			if cur == nil {
				newSeg(r, false)
			} else if r.pid != cur.pid { // a PID change that has no restart row (an older file, or a gap)
				newSeg(r, true)
			}
			cur.to = r.t
		}
		switch r.kind {
		case kindSample:
			a.samples++
			sampleTimes = append(sampleTimes, r.t)
			if cur != nil && r.pid == cur.pid {
				cur.samples = append(cur.samples, r)
			}
		case kindDown:
			a.downs++
			sampleTimes = append(sampleTimes, r.t)
		case kindScrape:
			a.scrapes++
			if cur != nil && r.pid == cur.pid {
				cur.scrapes = append(cur.scrapes, r)
			}
		case kindScrapeError:
			a.scrapeErrs++
		}

		// The machine's counters, from every sample and down row, each from the last row that had it: an increase
		// belongs to the process of the later row. They count since boot, so only a reboot resets them.
		if r.kind == kindSample || r.kind == kindDown {
			for _, col := range hostCounters {
				v, ok := r.vals[col]
				if !ok {
					continue
				}
				if p, seen := prevHost[col]; seen {
					dt := r.t.Sub(p.t)
					inc, reset := v-p.v, v < p.v
					if reset {
						inc = v
					}
					acc(a.host, col).add(inc, dt, !reset)
					if cur != nil && r.kind == kindSample {
						acc(cur.host, col).add(inc, dt, !reset)
					}
				}
				prevHost[col] = last{v: v, t: r.t}
			}
		}

		// The scraped counters: they start at 0 with the process. The first scrape of the first process is the
		// baseline (what it counted before the recording); otherwise a series seen for the first time in a process
		// counts whole (a process that started inside the recording, or a labeled series that client_golang only
		// shows from its first increase), and one that went down counts as reset.
		if r.kind == kindScrape && cur != nil && r.pid == cur.pid {
			for _, m := range metricColumns {
				v, ok := r.vals[m.col]
				if !ok || !isMetricCounter(m.col) {
					continue
				}
				p, seen := prevMetric[m.col]
				var inc float64
				peak := false
				switch {
				case (!seen || p.seg != cur) && !cur.restarted && len(cur.scrapes) == 1:
					inc = 0 // the baseline
				case !seen || p.seg != cur || v < p.v:
					inc = v
				default:
					inc, peak = v-p.v, true
				}
				dt := r.t.Sub(p.t)
				acc(a.metric, m.col).add(inc, dt, peak)
				acc(cur.metric, m.col).add(inc, dt, peak)
				prevMetric[m.col] = last{v: v, t: r.t, seg: cur}
			}
		}
	}

	// The sampling interval and the collector's gaps (more than 2.5 intervals without a sample).
	var dts []time.Duration
	for i := 1; i < len(sampleTimes); i++ {
		if dt := sampleTimes[i].Sub(sampleTimes[i-1]); dt > 0 {
			dts = append(dts, dt)
		}
	}
	if len(dts) > 0 {
		sorted := slices.Clone(dts)
		slices.Sort(sorted)
		a.interval = sorted[len(sorted)/2]
		for _, dt := range dts {
			if dt > a.interval*5/2 {
				a.gaps++
				a.longestGap = max(a.longestGap, dt)
			}
		}
	}
}

func acc(m map[string]*counterAcc, col string) *counterAcc {
	x, ok := m[col]
	if !ok {
		x = &counterAcc{}
		m[col] = x
	}
	return x
}

// setT0 fixes T+0: the -t0 flag, else 60 minutes before the first restart (the scripted one), else 15 minutes
// after the first row (the collector starts at T-15).
func (a *analysis) setT0(flagT0 string) error {
	if flagT0 != "" {
		t, err := parseT0(flagT0, a.first)
		if err != nil {
			return usageError{err}
		}
		a.t0, a.anchor = t, "set with -t0"
		return nil
	}
	if len(a.segs) > 1 {
		a.t0 = a.segs[1].from.Add(-markRestart)
		a.anchor = "60 minutes before the first restart, the scripted one at T+60 (-t0 sets it)"
		return nil
	}
	a.t0 = a.first.Add(-markCollectorStart)
	a.anchor = "15 minutes after the first row, as the collector starts at T-15 (-t0 sets it)"
	return nil
}

func parseT0(s string, first time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"15:04", "15:04:05"} {
		if c, err := time.Parse(layout, s); err == nil {
			// That time of day within 12 hours of the first row, so a session across midnight UTC works.
			d := first.UTC()
			t := time.Date(d.Year(), d.Month(), d.Day(), c.Hour(), c.Minute(), c.Second(), 0, time.UTC)
			switch {
			case t.Before(first.Add(-12 * time.Hour)):
				t = t.AddDate(0, 0, 1)
			case t.After(first.Add(12 * time.Hour)):
				t = t.AddDate(0, 0, -1)
			}
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("-t0 %q is neither RFC 3339 nor HH:MM[:SS]", s)
}

// at returns T+d.
func (a *analysis) at(d time.Duration) time.Time { return a.t0.Add(d) }

// tOff formats t as an offset from T+0: T+60:05, T-15:00.
func (a *analysis) tOff(t time.Time) string {
	d := t.Sub(a.t0).Round(time.Second)
	sign := "+"
	if d < 0 {
		sign, d = "-", -d
	}
	return fmt.Sprintf("T%s%d:%02d", sign, int(d/time.Minute), int(d%time.Minute/time.Second))
}

// segmentAt returns the process whose rows span t, or nil.
func (a *analysis) segmentAt(t time.Time) *segment {
	for _, s := range a.segs {
		if s.contains(t) {
			return s
		}
	}
	return nil
}

// verdict is one row of the pass-criteria table.
type verdict struct {
	measure, target, measured, result string
}

const (
	resPass   = "pass"
	resFail   = "FAIL"
	resCheck  = "check"
	resManual = "manual"
	resNA     = "n/a"
)

// cpuWindow is the process's CPU over the sample pairs whose later sample is in (from, to]: the average weighted
// by time, and the largest pair.
func (a *analysis) cpuWindow(segs []*segment, from, to time.Time) (avg, peak float64, cpus int, ok bool) {
	var busy, capacity float64
	for _, s := range segs {
		for i := 1; i < len(s.samples); i++ {
			p, r := s.samples[i-1], s.samples[i]
			if !r.t.After(from) || r.t.After(to) {
				continue
			}
			t1, ok1 := p.vals[colCPUTicks]
			t2, ok2 := r.vals[colCPUTicks]
			n, ok3 := r.vals[colCPUs]
			dt := r.t.Sub(p.t).Seconds()
			if !ok1 || !ok2 || !ok3 || n <= 0 || dt <= 0 || t2 < t1 {
				continue
			}
			secs := (t2 - t1) / userHZ
			busy += secs
			// float64() rounds the product, so Go can't fuse it into a multiply-add on arm64: every machine
			// prints the same summary.
			capacity += float64(dt * n)
			peak = max(peak, 100*secs/(dt*n))
			cpus = int(n)
			ok = true
		}
	}
	if !ok {
		return 0, 0, 0, false
	}
	return 100 * busy / capacity, peak, cpus, true
}

// hostCPUWindow is the machine's CPU: the mean and the largest of host_cpu_pct in (from, to].
func (a *analysis) hostCPUWindow(from, to time.Time) (avg, peak float64, ok bool) {
	var sum float64
	n := 0
	for _, r := range a.rows {
		v, has := r.vals[colHostCPUPct]
		if !has || !r.t.After(from) || r.t.After(to) {
			continue
		}
		sum += v
		peak = max(peak, v)
		n++
	}
	if n == 0 {
		return 0, 0, false
	}
	return sum / float64(n), peak, true
}

// rssGrowth compares the RSS of the first sample at or after from with the last at or before to, both in the
// process that runs at from.
func (a *analysis) rssGrowth(from, to time.Time) (first, last *runRow, growth float64, why string) {
	s := a.segmentAt(from)
	if s == nil {
		return nil, nil, 0, "no process at " + a.tOff(from)
	}
	for _, r := range s.samples {
		if !r.has(colRSS) {
			continue
		}
		if first == nil && !r.t.Before(from) {
			first = r
		}
		if !r.t.After(to) {
			last = r
		}
	}
	if first == nil || last == nil || !last.t.After(first.t) || first.vals[colRSS] <= 0 {
		return nil, nil, 0, "too few samples of one process between " + a.tOff(from) + " and " + a.tOff(to)
	}
	return first, last, 100 * (last.vals[colRSS] - first.vals[colRSS]) / first.vals[colRSS], ""
}

func (a *analysis) verdicts() []verdict {
	return []verdict{
		{"Manual fixes", "0 (expected actions are not fixes)", "the per-person notes", resManual},
		a.sessionVerdict(),
		a.cpuVerdict(),
		a.memoryVerdict(),
		a.goroutineVerdict(),
		a.udpVerdict(),
		a.recoveryVerdict(),
		a.viewingVerdict(),
		{"Phones", "iPhone and Android: video and audio after one tap, Home Screen app, notification, playing after unlock",
			"the per-person notes", resManual},
		a.iceTCPVerdict(),
		{"Survey (1-5)", "median >= 4 for picture and sound; nobody reports audio out of sync", "the survey", resManual},
	}
}

func (a *analysis) sessionVerdict() verdict {
	v := verdict{measure: "Session", target: "2 h; the server restarts only at T+60"}
	var restarts []string
	for _, s := range a.segs[1:] {
		restarts = append(restarts, a.tOff(s.from))
	}
	covered := !a.first.After(a.at(markCoverSlack)) && !a.last.Before(a.at(markEnd-markCoverSlack))
	v.measured = fmt.Sprintf("recorded %s to %s", a.tOff(a.first), a.tOff(a.last))
	switch len(restarts) {
	case 0:
		v.measured += "; no restart"
	case 1:
		v.measured += "; restart at " + restarts[0]
	default:
		v.measured += "; restarts at " + strings.Join(restarts, ", ")
	}
	if a.downs > 0 {
		v.measured += fmt.Sprintf("; no isshoni process in %d %s", a.downs, plural(a.downs, "sample", "samples"))
	}
	switch {
	case len(restarts) > 1:
		v.result = resFail
	case len(restarts) == 0 || !covered:
		v.result = resCheck
	case absDur(a.segs[1].from.Sub(a.at(markRestart))) > markRestartSlack:
		v.result = resCheck
	default:
		v.result = resPass
	}
	return v
}

func (a *analysis) cpuVerdict() verdict {
	v := verdict{measure: "Server CPU",
		target: fmt.Sprintf("average < %.0f%%, peak < %.0f%% of all CPUs", limitCPUAvg, limitCPUPeak)}
	avg, peak, cpus, ok := a.cpuWindow(a.segs, a.t0, a.at(markEnd))
	if !ok {
		v.measured, v.result = "no CPU samples between T+0 and T+120", resNA
		return v
	}
	v.measured = fmt.Sprintf("isshoni T+0 to T+120: average %s, peak %s of %d %s", pct(avg), pct(peak), cpus,
		plural(cpus, "CPU", "CPUs"))
	if havg, hpeak, ok := a.hostCPUWindow(a.t0, a.at(markEnd)); ok {
		v.measured += fmt.Sprintf(" (whole machine: average %s, peak %s)", pct(havg), pct(hpeak))
	}
	v.result = passIf(avg < limitCPUAvg && peak < limitCPUPeak)
	return v
}

func (a *analysis) memoryVerdict() verdict {
	v := verdict{measure: "Memory", target: fmt.Sprintf("RSS growth T+65 to T+120 < %.0f%%", limitRSSGrowth)}
	first, last, growth, why := a.rssGrowth(a.at(markMemFrom), a.at(markEnd))
	if why != "" {
		v.measured, v.result = why, resNA
	} else {
		v.measured = fmt.Sprintf("%s at %s to %s at %s (%s)", bytesStr(first.vals[colRSS]), a.tOff(first.t),
			bytesStr(last.vals[colRSS]), a.tOff(last.t), signedPct(growth))
		v.result = passIf(growth < limitRSSGrowth)
	}
	// The first process, T+5 to T+59, is the second sample of the same measure (06 §12.3).
	if f, l, g, why := a.rssGrowth(a.at(markFirstFrom), a.at(markFirstTo)); why == "" {
		v.measured += fmt.Sprintf("; first process T+5 to T+59: %s to %s (%s)", bytesStr(f.vals[colRSS]),
			bytesStr(l.vals[colRSS]), signedPct(g))
	}
	return v
}

func (a *analysis) goroutineVerdict() verdict {
	v := verdict{measure: "Goroutines", target: "back to the T+85 baseline after the stress (T+90 to T+100)"}
	var base, end *runRow
	var peak float64
	var endSeg *segment
	for _, s := range a.segs {
		for _, r := range s.scrapes {
			if !r.has("go_goroutines") || r.t.After(a.at(markEnd)) {
				continue
			}
			if r.t.After(a.at(markStressTo)) {
				end, endSeg = r, s
			}
		}
	}
	if end == nil {
		v.measured, v.result = "no scrape between T+100 and T+120", resNA
		return v
	}
	for _, r := range endSeg.scrapes {
		if !r.has("go_goroutines") {
			continue
		}
		if !r.t.After(a.at(markBaseline)) {
			base = r
		}
		if !r.t.Before(a.at(markStressFrom)) && !r.t.After(a.at(markStressTo)) {
			peak = max(peak, r.vals["go_goroutines"])
		}
	}
	if base == nil {
		v.measured, v.result = "no scrape of the same process at or before T+85", resNA
		return v
	}
	b, e := base.vals["go_goroutines"], end.vals["go_goroutines"]
	stress := ""
	if peak > 0 {
		stress = ", peak " + count(peak) + " in the stress"
	}
	v.measured = fmt.Sprintf("%s at %s%s%s, %s at %s%s", count(b), a.tOff(base.t), presence(base), stress,
		count(e), a.tOff(end.t), presence(end))
	if b > 0 {
		v.measured += fmt.Sprintf(" (%s)", signedPct(100*(e-b)/b))
	}
	if e <= b*(1+goroutineSlack) {
		v.result = resPass
	} else {
		v.result = resCheck // more people or shares than at the baseline also raise it
	}
	return v
}

// presence says how many participants and shares a scrape saw, which the goroutine count follows.
func presence(r *runRow) string {
	p, ok1 := r.vals["participants"]
	s, ok2 := r.vals["shares"]
	if !ok1 || !ok2 {
		return ""
	}
	return fmt.Sprintf(" with %s %s and %s %s", count(p), plural(int(p), "participant", "participants"), count(s),
		plural(int(s), "share", "shares"))
}

func (a *analysis) udpVerdict() verdict {
	v := verdict{measure: "UDP socket errors", target: "RcvbufErrors + SndbufErrors = 0, or < 0.01% of datagrams"}
	rcv, snd := a.host[colUDPRcvbufErrs], a.host[colUDPSndbufErrs]
	in, out := a.host[colUDPIn], a.host[colUDPOut]
	if rcv == nil || snd == nil || in == nil || out == nil {
		v.measured, v.result = "no UDP counters", resNA
		return v
	}
	errs, dgrams := rcv.total+snd.total, in.total+out.total
	v.measured = fmt.Sprintf("%s of %s datagrams (RcvbufErrors %s, SndbufErrors %s)", count(errs), count(dgrams),
		count(rcv.total), count(snd.total))
	if dgrams > 0 {
		v.measured += ", " + smallPct(100*errs/dgrams)
	}
	v.result = passIf(errs == 0 || dgrams > 0 && errs/dgrams < limitUDPErrRatio)
	return v
}

func (a *analysis) recoveryVerdict() verdict {
	v := verdict{measure: "Recovery", result: resManual,
		target: "after the restart all 5 play in <= 30 s; after C's network switch <= 10 s; A's share survives a 10 s drop"}
	if len(a.segs) < 2 {
		v.measured = "no restart in the recording; the per-person notes"
		return v
	}
	prev, s := a.segs[0], a.segs[1]
	var want float64
	for _, r := range prev.scrapes {
		if p, ok := r.vals["participants"]; ok {
			want = p
		}
	}
	v.measured = "the per-person notes"
	if want == 0 {
		return v
	}
	for _, r := range s.scrapes {
		if p, ok := r.vals["participants"]; ok && p >= want {
			v.measured = fmt.Sprintf("%s %s back %s after the restart (scraped every %s); the per-person notes",
				count(want), plural(int(want), "participant", "participants"), shortDur(r.t.Sub(s.from)), a.scrapeEvery())
			return v
		}
	}
	v.measured = fmt.Sprintf("the participants never got back to %s after the restart; the per-person notes", count(want))
	v.result = resCheck
	return v
}

// scrapeEvery is the median time between two scrapes.
func (a *analysis) scrapeEvery() string {
	var dts []time.Duration
	var prev time.Time
	for _, r := range a.rows {
		if r.kind != kindScrape && r.kind != kindScrapeError {
			continue
		}
		if !prev.IsZero() && r.t.After(prev) {
			dts = append(dts, r.t.Sub(prev))
		}
		prev = r.t
	}
	if len(dts) == 0 {
		return "?"
	}
	slices.Sort(dts)
	return shortDur(dts[len(dts)/2])
}

func (a *analysis) viewingVerdict() verdict {
	v := verdict{measure: "Viewing quality", result: resManual,
		target: "freezes < 1% of watch time on desktop, < 3% on phones; no decode errors that need a reload"}
	freeze, ok := a.metric["client_freeze_seconds_total"]
	if !ok {
		v.measured = "no client stats in the scrapes; the per-person notes and the dashboard"
		return v
	}
	// Presence: participants times time, between consecutive scrapes of one process. Everyone present watches
	// something, so this is the watch time to within the sharers' own screens.
	var presenceSecs float64
	for _, s := range a.segs {
		for i := 1; i < len(s.scrapes); i++ {
			p, r := s.scrapes[i-1], s.scrapes[i]
			if n, ok := p.vals["participants"]; ok {
				presenceSecs += float64(n * r.t.Sub(p.t).Seconds()) // no fused multiply-add, as in cpuWindow
			}
		}
	}
	parts := []string{fmt.Sprintf("%s frozen", secondsStr(freeze.total))}
	share := math.NaN()
	if presenceSecs > 0 {
		share = 100 * freeze.total / presenceSecs
		parts[0] += fmt.Sprintf(" in %s of presence (%.2f%%, every device together)", hours(presenceSecs), share)
	}
	dec, drop := a.metric["client_frames_decoded_total"], a.metric["client_frames_dropped_total"]
	if dec != nil && drop != nil && dec.total+drop.total > 0 {
		parts = append(parts, fmt.Sprintf("%.2f%% of frames dropped", 100*drop.total/(dec.total+drop.total)))
	}
	smp, conc := a.metric["client_audio_samples_total"], a.metric["client_audio_concealed_samples_total"]
	if smp != nil && conc != nil && smp.total > 0 {
		parts = append(parts, fmt.Sprintf("%.2f%% of audio concealed", 100*conc.total/smp.total))
	}
	v.measured = strings.Join(parts, "; ") + "; per device: the dashboard and the notes"
	if share >= limitFreezeFail {
		v.result = resFail // whatever the split, a device class is over its limit
	}
	return v
}

func (a *analysis) iceTCPVerdict() verdict {
	v := verdict{measure: "ICE-TCP", target: "E's selected transport is TCP on 443 all session long", result: resManual}
	var n, with int
	var zero []string
	for _, s := range a.segs {
		for _, r := range s.scrapes {
			x, ok := r.vals["transport_tcp443"]
			if !ok || r.t.Before(a.t0) || r.t.After(a.at(markEnd)) {
				continue
			}
			n++
			if x >= 1 {
				with++
			} else {
				zero = append(zero, a.tOff(r.t))
			}
		}
	}
	if n == 0 {
		v.measured, v.result = "no transport series between T+0 and T+120", resNA
		return v
	}
	v.measured = fmt.Sprintf("a PeerConnection on tcp443 in %d of %d scrapes T+0 to T+120", with, n)
	if len(zero) > 0 {
		shown := zero
		if len(shown) > 6 {
			shown = append(slices.Clone(shown[:6]), "...")
		}
		v.measured += "; none at " + strings.Join(shown, ", ")
	}
	v.measured += "; E's own transport: the dashboard"
	if with == 0 {
		v.result = resFail
	}
	return v
}

// counterRow is one line of the counters table.
type counterRow struct {
	label string
	col   string
	unit  string // "bytes", "count" or "seconds"
	rate  bool   // show the average and peak rates
}

var counterRows = []counterRow{
	{"NIC received", colNICRx, "bytes", true},
	{"NIC sent", colNICTx, "bytes", true},
	{"UDP datagrams in", colUDPIn, "count", true},
	{"UDP datagrams out", colUDPOut, "count", true},
	{"UDP RcvbufErrors", colUDPRcvbufErrs, "count", false},
	{"UDP SndbufErrors", colUDPSndbufErrs, "count", false},
	{"UDP InErrors", colUDPInErrs, "count", false},
	{"TCP RetransSegs", colTCPRetrans, "count", true},
	{"isshoni CPU time", "process_cpu_seconds_total", "seconds", false},
	{"SFU ingress (RTP)", "sfu_ingress_bytes_total", "bytes", true},
	{"SFU egress (RTP)", "sfu_egress_bytes_total", "bytes", true},
	{"Transfer ingress (sockets)", "transfer_ingress_bytes_total", "bytes", true},
	{"Transfer egress (sockets)", "transfer_egress_bytes_total", "bytes", true},
	{"NACKs served", "sfu_nack_served_total", "count", true},
	{"PLIs sent", "sfu_pli_sent_total", "count", true},
	{"Layer switches", "sfu_layer_switches_total", "count", true},
	{"Resumes: resumed", "ws_resume_resumed_total", "count", false},
	{"Resumes: not resumed", "ws_resume_not_resumed_total", "count", false},
	{"Client frames decoded", "client_frames_decoded_total", "count", true},
	{"Client frames dropped", "client_frames_dropped_total", "count", false},
	{"Client freeze time", "client_freeze_seconds_total", "seconds", false},
	{"Client packets lost", "client_packets_lost_total", "count", false},
	{"Client audio samples", "client_audio_samples_total", "count", false},
	{"Client audio concealed", "client_audio_concealed_samples_total", "count", false},
}

func (cr counterRow) cell(acc *counterAcc, span time.Duration) string {
	if acc == nil || !acc.seen {
		return "-"
	}
	var s string
	switch cr.unit {
	case "bytes":
		s = bytesStr(acc.total)
	case "seconds":
		s = secondsStr(acc.total)
	default:
		s = count(acc.total)
	}
	if !cr.rate || span <= 0 {
		return s
	}
	avg := acc.total / span.Seconds()
	if cr.unit == "bytes" {
		return fmt.Sprintf("%s (%s, peak %s)", s, bitRate(avg), bitRate(acc.peak))
	}
	return fmt.Sprintf("%s (%s/s, peak %s/s)", s, rateStr(avg), rateStr(acc.peak))
}

// write prints the summary as Markdown.
func (a *analysis) write(w io.Writer, name string) error {
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	p("# Exit test summary (%s)\n\n", name)
	p("Recorded %s to %s UTC (%s), %s to %s. T+0 is %s UTC: %s.\n\n",
		a.first.UTC().Format("2006-01-02 15:04:05"), a.last.UTC().Format("15:04:05"),
		shortDur(a.last.Sub(a.first)), a.tOff(a.first), a.tOff(a.last), a.t0.UTC().Format("2006-01-02 15:04:05"),
		a.anchor)
	p("%s %s (%d without isshoni), %s %s (%d failed), %d %s; the collector started %d %s",
		count(float64(a.samples+a.downs)), plural(a.samples+a.downs, "sample", "samples"), a.downs,
		count(float64(a.scrapes+a.scrapeErrs)), plural(a.scrapes+a.scrapeErrs, "scrape", "scrapes"), a.scrapeErrs,
		len(a.segs)-1, plural(len(a.segs)-1, "restart", "restarts"), a.starts, plural(a.starts, "time", "times"))
	if a.gaps == 0 {
		p(", with no gap.\n")
	} else {
		p("; %d %s in the samples, the longest %s.\n", a.gaps, plural(a.gaps, "gap", "gaps"), shortDur(a.longestGap))
	}
	if len(a.skipped) > 0 {
		p("%d %s could not be read and %s skipped (the first: %s).\n", len(a.skipped),
			plural(len(a.skipped), "row", "rows"), plural(len(a.skipped), "was", "were"), a.skipped[0])
	}

	p("\n## Pass criteria (06 §12.3)\n\n")
	p("| Measure | Target | Measured | Result |\n|---|---|---|---|\n")
	for _, v := range a.verdicts() {
		p("| %s | %s | %s | %s |\n", v.measure, v.target, v.measured, v.result)
	}
	p("\npass: measured and met. FAIL: measured and missed. check: measured, needs a look. manual: from the\n")
	p("people, the admin dashboard or the survey. n/a: not in the recording.\n")

	p("\n## Processes\n\n")
	p("| # | PID | From | To | Length | CPU average | CPU peak | RSS first, last (max) | Threads max | FDs max |" +
		" Goroutines |\n")
	p("|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, s := range a.segs {
		cpuAvg, cpuPeak := "-", "-"
		if avg, peak, _, ok := a.cpuWindow([]*segment{s}, s.from.Add(-time.Nanosecond), s.to); ok {
			cpuAvg, cpuPeak = pct(avg), pct(peak)
		}
		p("| %d | %d | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n", s.n, s.pid, a.tOff(s.from), a.tOff(s.to),
			shortDur(s.to.Sub(s.from)), cpuAvg, cpuPeak, s.rssCell(), s.maxCell(s.samples, colThreads),
			s.maxCell(s.samples, colFDs), s.rangeCell("go_goroutines"))
	}

	p("\n## Counters\n\n")
	p("The increase per process (a counter that went down counts as reset), and for rates the average over the\n")
	p("process's time and the peak between two rows.\n\n")
	p("| Counter |")
	for _, s := range a.segs {
		p(" Process %d |", s.n)
	}
	p(" All |\n|---|")
	for range a.segs {
		p("---|")
	}
	p("---|\n")
	for _, cr := range counterRows {
		m := a.metric
		if slices.Contains(hostCounters, cr.col) {
			m = a.host
		}
		p("| %s |", cr.label)
		for _, s := range a.segs {
			sm := s.metric
			if slices.Contains(hostCounters, cr.col) {
				sm = s.host
			}
			p(" %s |", cr.cell(sm[cr.col], s.to.Sub(s.from)))
		}
		p(" %s |\n", cr.cell(m[cr.col], a.last.Sub(a.first)))
	}

	p("\n## Gauges\n\n")
	p("The largest value per process.\n\n")
	p("| Gauge |")
	for _, s := range a.segs {
		p(" Process %d |", s.n)
	}
	p("\n|---|")
	for range a.segs {
		p("---|")
	}
	p("\n")
	for _, g := range []struct{ label, col string }{
		{"Participants", "participants"}, {"Shares", "shares"}, {"DownTracks", "sfu_downtracks"},
		{"PCs on udp", "transport_udp"}, {"PCs on tcp443", "transport_tcp443"}, {"PCs on tcp7882", "transport_tcp7882"},
	} {
		p("| %s |", g.label)
		for _, s := range a.segs {
			p(" %s |", s.maxCell(s.scrapes, g.col))
		}
		p("\n")
	}
	p("| Load average (1 min) |")
	for _, s := range a.segs {
		p(" %s |", s.maxLoad())
	}
	p("\n")
	_, err := io.WriteString(w, b.String())
	return err
}

func (s *segment) rssCell() string {
	var first, last, peak float64
	n := 0
	for _, r := range s.samples {
		v, ok := r.vals[colRSS]
		if !ok {
			continue
		}
		if n == 0 {
			first = v
		}
		last, peak = v, max(peak, v)
		n++
	}
	if n == 0 {
		return "-"
	}
	return fmt.Sprintf("%s, %s (%s)", bytesStr(first), bytesStr(last), bytesStr(peak))
}

func (s *segment) maxCell(rows []*runRow, col string) string {
	m, ok := maxOf(rows, col)
	if !ok {
		return "-"
	}
	return count(m)
}

func (s *segment) rangeCell(col string) string {
	lo, hi, ok := math.Inf(1), math.Inf(-1), false
	for _, r := range s.scrapes {
		if v, has := r.vals[col]; has {
			lo, hi, ok = min(lo, v), max(hi, v), true
		}
	}
	if !ok {
		return "-"
	}
	return count(lo) + "-" + count(hi)
}

func (s *segment) maxLoad() string {
	m, ok := maxOf(s.samples, colLoad1)
	if !ok {
		return "-"
	}
	return strconv.FormatFloat(m, 'f', 2, 64)
}

func maxOf(rows []*runRow, col string) (float64, bool) {
	m, ok := math.Inf(-1), false
	for _, r := range rows {
		if v, has := r.vals[col]; has {
			m, ok = max(m, v), true
		}
	}
	return m, ok
}

func passIf(ok bool) string {
	if ok {
		return resPass
	}
	return resFail
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func pct(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) + "%" }

// smallPct formats a percentage near the 0.01% limit: four decimals, and "< 0.0001%" below them.
func smallPct(v float64) string {
	if v > 0 && v < 0.0001 {
		return "< 0.0001%"
	}
	return strconv.FormatFloat(v, 'f', 4, 64) + "%"
}

func signedPct(v float64) string {
	s := strconv.FormatFloat(v, 'f', 1, 64) + "%"
	if v >= 0 {
		s = "+" + s
	}
	return s
}

// count formats a count with thousands separators; a fraction keeps one decimal.
func count(v float64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	v = math.Round(v*10) / 10
	whole := math.Floor(v)
	tenth := int(math.Round((v - whole) * 10))
	digits := strconv.FormatFloat(whole, 'f', 0, 64)
	var b strings.Builder
	if neg && (whole > 0 || tenth > 0) {
		b.WriteByte('-')
	}
	for i, c := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if tenth > 0 {
		b.WriteString("." + strconv.Itoa(tenth))
	}
	return b.String()
}

func bytesStr(v float64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return strconv.FormatFloat(v, 'f', 0, 64) + " B"
	}
	return strconv.FormatFloat(v, 'f', 1, 64) + " " + units[i]
}

// bitRate formats bytes per second as Mbit/s.
func bitRate(bytesPerSec float64) string {
	return strconv.FormatFloat(bytesPerSec*8/1e6, 'f', 1, 64) + " Mbit/s"
}

func rateStr(v float64) string {
	if v >= 100 {
		return count(math.Round(v))
	}
	return strconv.FormatFloat(v, 'f', 1, 64)
}

func secondsStr(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) + " s" }

func hours(secs float64) string { return strconv.FormatFloat(secs/3600, 'f', 1, 64) + " h" }

// shortDur formats a duration rounded to the second: 2h15m30s, 40s.
func shortDur(d time.Duration) string { return d.Round(time.Second).String() }
