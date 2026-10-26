package main

import (
	"fmt"
	"math"
	"slices"
)

// This file is the soak case's analysis (docs/m2/05-windows-audio.md §18.5 case 23, `run -long`): 30 minutes of a
// music-like fixture (1000 + 683 Hz with a slow AM) and an idle voice app holding the mic, checked for zero
// underruns, trims and mixer resyncs, no clicks, maxResamplePpm ≤ 1000, and 440 Hz never present. Thirty minutes of
// the tap are about 1.4 GB as float64, so the soak never keeps them: the capture streams the timeline into a
// soakAnalyzer, which keeps running results only (the 50 ms short windows of §18.6, 10 s block levels, and the
// clicks). It is portable and tested on every OS with synthetic signals.

// soakBlockSec is the soak's block length for levels (s): each block's Goertzel level of each checked frequency;
// a present check takes the lowest block, an absent check the highest. One block spans one AM period.
const soakBlockSec = 10.0

// soakMinBlockSec is the shortest final block that still counts (s).
const soakMinBlockSec = 1.0

// maxSoakClicks is how many click times the soak keeps for the report (it counts them all).
const maxSoakClicks = 50

// soakAnalyzer is the streaming analysis of a soak recording: feed it the timeline chunk by chunk (a capture's
// sink), then read its result.
type soakAnalyzer struct {
	from  int       // the first analyzed sample (the check window's start)
	freqs []float64 // every checked frequency
	short []float64 // the frequencies whose 50 ms windows are tracked (the absent checks)

	n int // samples fed so far (the timeline index of the next sample)

	// Clicks (S1's rule, per channel, merged within 10 ms).
	prev       [2][2]float64 // the last two samples of each channel
	lastClick  [2]int        // per channel, the index of the last click (−1: none)
	clicks     []float64     // the first maxSoakClicks click times (s)
	clickCount int
	lastEvent  int // the index of the last recorded click on either channel (−1: none)

	// Short windows: the mono tail, a window computed every hop.
	tail     []float64
	sinceHop int
	maxShort []float64 // per short frequency: the loudest window (dB)
	maxAt    []float64 // and its centre (s)

	// Blocks.
	block    []float64
	blockMin []float64 // per frequency
	blockMax []float64
	blocks   int
}

// newSoakAnalyzer analyzes from fromSec on; freqs are every checked frequency, short those of the absent checks.
func newSoakAnalyzer(fromSec float64, freqs, short []float64) *soakAnalyzer {
	sa := &soakAnalyzer{
		from: int(math.Round(fromSec * sampleRate)), freqs: slices.Clone(freqs), short: slices.Clone(short),
		lastClick: [2]int{-1, -1}, lastEvent: -1,
		maxShort: make([]float64, len(short)), maxAt: make([]float64, len(short)),
		blockMin: make([]float64, len(freqs)), blockMax: make([]float64, len(freqs)),
	}
	for i := range sa.maxShort {
		sa.maxShort[i] = silenceFloorDB
	}
	for i := range freqs {
		sa.blockMin[i], sa.blockMax[i] = math.Inf(1), math.Inf(-1)
	}
	return sa
}

// feed adds one stretch of the timeline (a tap chunk, or the zeros that pad a gap).
func (sa *soakAnalyzer) feed(left, right []float64) {
	win := int(math.Round(shortWindowSec * sampleRate))
	hop := int(math.Round(shortHopSec * sampleRate))
	blockLen := int(soakBlockSec * sampleRate)
	n := min(len(left), len(right))
	for i := range n {
		idx := sa.n
		sa.n++
		l, r := left[i], right[i]
		sa.clickSample(0, idx, l)
		sa.clickSample(1, idx, r)
		if idx < sa.from {
			continue
		}
		m := (l + r) / 2
		sa.tail = append(sa.tail, m)
		sa.sinceHop++
		if len(sa.tail) >= win && sa.sinceHop >= hop {
			sa.sinceHop = 0
			w := sa.tail[len(sa.tail)-win:]
			centre := float64(idx+1-win)/sampleRate + shortWindowSec/2
			for k, f := range sa.short {
				if db := toneLevelDB(w, sampleRate, f); db > sa.maxShort[k] {
					sa.maxShort[k], sa.maxAt[k] = db, centre
				}
			}
			if len(sa.tail) > 8*win {
				sa.tail = append(sa.tail[:0], sa.tail[len(sa.tail)-win:]...)
			}
		}
		sa.block = append(sa.block, m)
		if len(sa.block) == blockLen {
			sa.closeBlock()
		}
	}
}

// clickSample runs S1's click rule on one sample of channel ch at timeline index idx.
func (sa *soakAnalyzer) clickSample(ch, idx int, v float64) {
	p := &sa.prev[ch]
	if idx >= 2 && idx >= sa.from {
		d2 := math.Abs(v - 2*p[1] + p[0])
		merge := int(sampleRate / 100) // 10 ms
		if d2 > clickThreshold && (sa.lastClick[ch] < 0 || idx-sa.lastClick[ch] > merge) {
			sa.lastClick[ch] = idx
			if sa.lastEvent < 0 || idx-sa.lastEvent >= merge {
				sa.lastEvent = idx
				sa.clickCount++
				if len(sa.clicks) < maxSoakClicks {
					sa.clicks = append(sa.clicks, round3(float64(idx)/sampleRate))
				}
			}
		}
	}
	p[0], p[1] = p[1], v
}

// closeBlock takes the current block's levels.
func (sa *soakAnalyzer) closeBlock() {
	for k, f := range sa.freqs {
		db := toneLevelDB(sa.block, sampleRate, f)
		sa.blockMin[k] = math.Min(sa.blockMin[k], db)
		sa.blockMax[k] = math.Max(sa.blockMax[k], db)
	}
	sa.blocks++
	sa.block = sa.block[:0]
}

// soakResult is what the soak analysis found.
type soakResult struct {
	analyzedSec float64
	blocks      int
	blockMin    map[float64]float64 // per frequency, dB
	blockMax    map[float64]float64
	maxShort    map[float64]float64 // per short frequency
	maxShortAt  map[float64]float64
	clicks      []float64
	clickCount  int
}

// result closes the last block (if long enough) and returns the analysis.
func (sa *soakAnalyzer) result() soakResult {
	if float64(len(sa.block)) >= soakMinBlockSec*sampleRate {
		sa.closeBlock()
	}
	res := soakResult{
		analyzedSec: float64(max(sa.n-sa.from, 0)) / sampleRate, blocks: sa.blocks,
		blockMin: map[float64]float64{}, blockMax: map[float64]float64{}, maxShort: map[float64]float64{},
		maxShortAt: map[float64]float64{}, clicks: slices.Clone(sa.clicks), clickCount: sa.clickCount,
	}
	for k, f := range sa.freqs {
		lo, hi := sa.blockMin[k], sa.blockMax[k]
		if sa.blocks == 0 {
			lo, hi = silenceFloorDB, silenceFloorDB
		}
		res.blockMin[f], res.blockMax[f] = lo, hi
	}
	for k, f := range sa.short {
		res.maxShort[f], res.maxShortAt[f] = sa.maxShort[k], sa.maxAt[k]
	}
	return res
}

// soakFreqs are the frequencies the soak's checks name: all of them, and those of the absent checks.
func soakFreqs(cs caseSpec) (all, short []float64) {
	for _, c := range cs.checks {
		if !slices.Contains(all, c.freq) {
			all = append(all, c.freq)
		}
		if c.expect == expectAbsent && !slices.Contains(short, c.freq) {
			short = append(short, c.freq)
		}
	}
	return all, short
}

// scoreSoak is the soak's verdict (05 §18.5 case 23): the premises and controls of §18.6 over [1.5 s, end]; every
// present frequency at or above −45 dBFS in every 10 s block; 440 Hz absent (every block ≤ −50 dBFS and ≤ P_min −
// 40 dB, every 50 ms window ≤ −50 dBFS); no clicks; underruns, trims and mixerResyncs 0; maxResamplePpm ≤ 1000. rec
// carries the samples, events and counters but no audio (the analyzer had it).
func scoreSoak(cs caseSpec, cfg gateConfig, rec recording, res soakResult) caseReport {
	if rec.micFail && cs.needsMic() {
		return skipped(cs, cfg, micSkip)
	}
	if p := premiseProblem(cs, rec); p != "" {
		return inconclusive(cs, cfg, "%s", p)
	}
	if res.blocks == 0 {
		return inconclusive(cs, cfg, "the soak recorded less than %.0f s", soakMinBlockSec)
	}
	cr := newCaseReport(cs, cfg)
	cr.Counters = cloneCounters(rec.counters)
	cr.Engine = enginesSeen(rec)
	cr.Events = reportEvents(rec.events, rec.roleApp)
	cr.Info = map[string]any{"minutes": math.Round(res.analyzedSec/60*10) / 10, "blocks": res.blocks,
		"clicks": res.clickCount}
	fail := false

	pMin := 0.0
	for _, c := range cs.checks {
		if c.expect == expectPresent {
			pMin = math.Min(pMin, res.blockMin[c.freq])
		}
	}
	for _, c := range cs.checks {
		rep := checkReport{Freq: c.freq, From: 1.5, To: 0, Expect: c.expect}
		switch c.expect {
		case expectPresent:
			rep.LevelDB = roundDB(res.blockMin[c.freq])
			rep.Pass = res.blockMin[c.freq] >= presentMinDB
		case expectAbsent:
			lvl, short := res.blockMax[c.freq], res.maxShort[c.freq]
			rep.LevelDB, rep.MaxShortDB, rep.MaxShortAtS = roundDB(lvl), roundDB(short), round3(res.maxShortAt[c.freq])
			rep.Pass = lvl <= absentMaxDB && lvl <= pMin-absentMarginDB && short <= absentMaxDB
		default:
			rep.LevelDB, rep.Pass = roundDB(res.blockMax[c.freq]), true
		}
		if !rep.Pass {
			fail = true
		}
		cr.Checks = append(cr.Checks, rep)
		if problem := rec.controlProblem(c, 1.5, rec.recordDur); problem != "" {
			cr.Problems = append(cr.Problems, problem)
			fail = true
		}
	}
	if res.clickCount > 0 {
		cr.ClicksAt = append(cr.ClicksAt, res.clicks...)
		cr.Problems = append(cr.Problems, fmt.Sprintf("%d clicks", res.clickCount))
		fail = true
	}
	if problems := counterProblems(cs, rec.counters); len(problems) > 0 {
		cr.Problems = append(cr.Problems, problems...)
		fail = true
	}
	result := resultPass
	if fail {
		result = resultFail
	}
	return withResult(cs, cfg, cr, result)
}
