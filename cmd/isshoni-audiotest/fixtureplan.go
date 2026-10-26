package main

import (
	"math"
	"slices"
	"strconv"
	"strings"
)

// This file turns a case's fixture plan (cases.go) into the command lines the Windows driver starts
// (docs/m2/05-windows-audio.md §18.3), and answers the timing questions the verdict asks about it (when a fixture
// was meant to start and how long it was meant to live, which frequencies a case plays). It is portable, so the argv
// of every case is pinned by tests on every OS.
//
// Every fixture verb takes `-role R` (a harness-internal flag beside 05 §18.3's): the process prints
// {"role":R,"pid":P,"startNs":N} when it starts and {"role":R,"firstRenderNs":N} before its first PlaySoundW, and a
// spawn forwards its descendants' lines on its stdout. That is how the harness maps report sessions to fixture roles
// by pid (05 §18.3: "they are per fixture pid") and resolves event-relative windows.

// settleSec is S1's settle: the recording starts 1 s after the case's initial fixtures run (05 §18.5).
const settleSec = 1.0

// caseDuration is how long a case records (seconds): the latest fixture end plus a margin, at least 6 s and at most
// 12 s. A fixture with dur 0 counts as ending 6 s after its start (it plays to the end of the case).
func caseDuration(cs caseSpec) float64 {
	d := 6.0
	var walk func(s *fixtureStep)
	walk = func(s *fixtureStep) {
		end := s.at
		if s.dur > 0 {
			end += s.dur
		} else {
			end += 6
		}
		if end+2 > d {
			d = end + 2
		}
		if s.child != nil {
			walk(s.child)
		}
	}
	for i := range cs.fixtures {
		walk(&cs.fixtures[i])
	}
	return math.Min(d, 12)
}

// startOf is when step starts on the recording's timeline: its own at when set, else its parent's start (a top-level
// fixture without at starts at the settle, −1 s).
func startOf(step *fixtureStep, parentStart float64) float64 {
	if step.at > 0 {
		return step.at
	}
	return parentStart
}

// lifetimeOf is how long step's process is meant to live (seconds from its own start): its dur, or (dur 0) until one
// second past the end of a recording of caseDur seconds, so a tone never ends inside a check window; a spawn with
// -orphan-after exits then instead.
func lifetimeOf(step *fixtureStep, start, caseDur float64) float64 {
	if step.verb == verbSpawn && step.orphanAfter > 0 {
		return step.orphanAfter
	}
	if step.dur > 0 {
		return step.dur
	}
	return caseDur + 1 - start
}

// fixtureArgs is the `fixture …` argument list of a top-level step in a case recording caseDur seconds, with child
// executables under work (a Windows folder: the paths are joined with `\`). A step with a child becomes
// `fixture spawn [-at S] [-tone F] -d D [-orphan-after O] <work>\<child.exe> <the child's arguments…>`, recursively.
func fixtureArgs(step *fixtureStep, work string, caseDur float64) []string {
	return stepArgs(step, work, caseDur, -settleSec)
}

func stepArgs(step *fixtureStep, work string, caseDur, parentStart float64) []string {
	start := startOf(step, parentStart)
	life := lifetimeOf(step, start, caseDur)
	args := []string{"fixture", step.verb, "-role", step.role}
	switch step.verb {
	case verbTone:
		args = append(args, "-f", joinFreqs(step.freqs), "-d", fmtSec(life))
		if step.am > 0 {
			args = append(args, "-am", strconv.FormatFloat(step.am, 'g', -1, 64))
		}
	case verbIdle:
		args = append(args, "-d", fmtSec(life))
	case verbSpawn:
		if step.child != nil {
			if childStart := startOf(step.child, start); childStart > start {
				args = append(args, "-at", fmtSec(childStart-start))
			}
		}
		if len(step.freqs) > 0 {
			args = append(args, "-tone", joinFreqs(step.freqs))
		}
		args = append(args, "-d", fmtSec(life))
		if step.orphanAfter > 0 {
			args = append(args, "-orphan-after", fmtSec(step.orphanAfter))
		}
	}
	if step.amp > 0 && (step.verb == verbTone || len(step.freqs) > 0) {
		args = append(args, "-amp", strconv.FormatFloat(step.amp, 'g', -1, 64))
	}
	if step.mic {
		args = append(args, "-mic")
	}
	if step.verb == verbSpawn && step.child != nil {
		args = append(args, winJoin(work, step.child.exe))
		args = append(args, stepArgs(step.child, work, caseDur, start)...)
	}
	return args
}

// planSpan is a fixture's planned start and lifetime (seconds on the recording's timeline).
type planSpan struct {
	start, life float64
}

// planSpans maps every role of cs's fixture tree to its planned start and lifetime for a recording of caseDur
// seconds (the verdict's "exited early" control, 05 §18.6).
func planSpans(cs caseSpec, caseDur float64) map[string]planSpan {
	out := map[string]planSpan{}
	var walk func(s *fixtureStep, parentStart float64)
	walk = func(s *fixtureStep, parentStart float64) {
		start := startOf(s, parentStart)
		out[s.role] = planSpan{start: start, life: lifetimeOf(s, start, caseDur)}
		if s.child != nil {
			walk(s.child, start)
		}
	}
	for i := range cs.fixtures {
		walk(&cs.fixtures[i], -settleSec)
	}
	return out
}

// fixtureRoles lists every role of cs's fixture tree, the roles of the fixtures the driver starts itself, and
// selfRole when the harness plays a tone itself.
func fixtureRoles(cs caseSpec) []string {
	var out []string
	var walk func(s *fixtureStep)
	walk = func(s *fixtureStep) {
		out = append(out, s.role)
		if s.child != nil {
			walk(s.child)
		}
	}
	for i := range cs.fixtures {
		walk(&cs.fixtures[i])
	}
	out = append(out, cs.driverRoles...)
	if cs.selfTone > 0 {
		out = append(out, selfRole)
	}
	return out
}

// caseFreqs is every frequency cs plays or checks, for the silence pre-check (05 §18.4), in ascending order.
func caseFreqs(cs caseSpec) []float64 {
	var out []float64
	add := func(f float64) {
		if f > 0 && !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	var walk func(s *fixtureStep)
	walk = func(s *fixtureStep) {
		for _, f := range s.freqs {
			add(f)
		}
		if s.child != nil {
			walk(s.child)
		}
	}
	for i := range cs.fixtures {
		walk(&cs.fixtures[i])
	}
	for _, c := range cs.checks {
		add(c.freq)
	}
	add(cs.selfTone)
	if cs.special == "timestamps" {
		add(freqBursts)
	}
	slices.Sort(out)
	return out
}

// copyList is every distinct fixture exe of cs's tree (relative paths under the work folder), in plan order: all of
// them are copied before any recording (05 §18.3).
func copyList(cs caseSpec) []string {
	var out []string
	var walk func(s *fixtureStep)
	walk = func(s *fixtureStep) {
		if s.exe != "" && !slices.Contains(out, s.exe) {
			out = append(out, s.exe)
		}
		if s.child != nil {
			walk(s.child)
		}
	}
	for i := range cs.fixtures {
		walk(&cs.fixtures[i])
	}
	return out
}

// winJoin joins a Windows folder and a relative path with `\` (the plan's paths are Windows paths on every OS).
func winJoin(dir, rel string) string {
	return strings.TrimRight(dir, `\/`) + `\` + rel
}

func joinFreqs(freqs []float64) string {
	parts := make([]string, len(freqs))
	for i, f := range freqs {
		parts[i] = strconv.FormatFloat(f, 'g', -1, 64)
	}
	return strings.Join(parts, ",")
}

// fmtSec formats seconds with at most three decimals ("4", "10.5").
func fmtSec(s float64) string {
	return strconv.FormatFloat(math.Round(s*1000)/1000, 'f', -1, 64)
}
