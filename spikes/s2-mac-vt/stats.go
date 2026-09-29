package main

import (
	"math"
	"sort"
)

// Dist summarises a sample (milliseconds unless said otherwise).
type Dist struct {
	N    int     `json:"n"`
	Mean float64 `json:"mean"`
	Min  float64 `json:"min"`
	P50  float64 `json:"p50"`
	P95  float64 `json:"p95"`
	P99  float64 `json:"p99"`
	Max  float64 `json:"max"`
}

// NewDist uses nearest-rank percentiles.
func NewDist(v []float64) Dist {
	if len(v) == 0 {
		return Dist{}
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	sum := 0.0
	for _, x := range s {
		sum += x
	}
	pct := func(p float64) float64 {
		k := int(math.Ceil(p/100*float64(len(s)))) - 1
		k = max(0, min(k, len(s)-1))
		return s[k]
	}
	return Dist{N: len(s), Mean: round3(sum / float64(len(s))), Min: round3(s[0]), P50: round3(pct(50)),
		P95: round3(pct(95)), P99: round3(pct(99)), Max: round3(s[len(s)-1])}
}

func round3(x float64) float64 { return math.Round(x*1000) / 1000 }
func round1(x float64) float64 { return math.Round(x*10) / 10 }
