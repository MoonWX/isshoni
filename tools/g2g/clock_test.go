package main

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// pageConst returns the value of `NAME = value` in the clock page's script.
func pageConst(t *testing.T, name string) string {
	t.Helper()
	m := regexp.MustCompile(`\b` + name + ` = ("[^"]*"|-?\d+)`).FindStringSubmatch(string(clockPage))
	if m == nil {
		t.Fatalf("the clock page has no constant %s", name)
	}
	return strings.Trim(m[1], `"`)
}

// TestClockPageGeometry: the page draws the strip that decode.go reads (01 §12.2).
func TestClockPageGeometry(t *testing.T) {
	for name, want := range map[string]int{
		"CELL": cellPx, "CELLS": stripCells, "BITS": dataCells, "MARGIN": marginPx,
		"CUT_MS": 5000, "TONE_HZ": 440, "TONE_DBFS": -30,
	} {
		if got, err := strconv.Atoi(pageConst(t, name)); err != nil || got != want {
			t.Errorf("clock.html: %s = %s, want %d", name, pageConst(t, name), want)
		}
	}
	if got := pageConst(t, "GREY"); got != "#808080" {
		t.Errorf("clock.html: GREY = %s, want the mid-grey #808080", got)
	}
	if n, _ := strconv.Atoi(pageConst(t, "NOISE_BLOCK")); n == cellPx {
		t.Error("clock.html: the noise's blocks are the strip's cell size")
	}
}

// TestClockPageContract: what the run book relies on is in the page: v from performance.now() at every animation
// frame, the Gray code, ?cuts=0, the Start button and its tone, the counter's words, and nothing loaded from
// elsewhere (the page's CSP allows nothing).
func TestClockPageContract(t *testing.T) {
	page := string(clockPage)
	for _, want := range []string{
		"requestAnimationFrame(frame)",
		"draw(performance.now())",
		"Math.floor(t) % 65536",
		"v ^ (v >>> 1)",
		`params.get("cuts") !== "0"`,
		"Math.floor(t / CUT_MS) % 2",
		`<button id="start" type="button">Start</button>`,
		`new OscillatorNode(audio, { type: "sine", frequency: TONE_HZ })`,
		"Math.pow(10, TONE_DBFS / 20)",
		`cuts ${cuts ? "on" : "off"}`,
		`tone ${toneOn() ? "on" : "off"}`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("clock.html lacks %q", want)
		}
	}
	for _, banned := range []string{"src=", "href=", "http://", "https://", "@import", "url("} {
		if strings.Contains(page, banned) {
			t.Errorf("clock.html contains %q: the page loads nothing", banned)
		}
	}
	if strings.Count(page, "<script>") != 1 || strings.Count(page, "<style>") != 1 {
		t.Error("clock.html should have one inline script and one inline style")
	}
}
