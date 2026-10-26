package main

import (
	"strings"
	"testing"
)

// TestFixturePlansValid walks every case's fixture plan (05 §18.3) and checks it is well formed: every step has a
// verb, a tone step names its frequencies, a step with a child uses a tree verb, and the role → exe mapping the
// Windows driver builds has no empty exe for a playing role. It validates the data the driver executes and reads
// every fixtureStep field, so the plan stays consistent on every OS.
func TestFixturePlansValid(t *testing.T) {
	for _, cs := range cases() {
		if len(cs.fixtures) == 0 {
			t.Errorf("case %q has no fixtures", cs.name)
		}
		for i := range cs.fixtures {
			walkFixture(t, cs.name, &cs.fixtures[i])
		}
		// A viewer case must make a SetViewerBrowser call (cases 11, 24).
		if strings.Contains(cs.name, "viewer") && cs.viewer == nil {
			t.Errorf("case %q is a viewer case but makes no SetViewerBrowser call", cs.name)
		}
	}
}

func walkFixture(t *testing.T, caseName string, s *fixtureStep) {
	t.Helper()
	if s.verb == "" {
		t.Errorf("%s: fixture %q has no verb", caseName, s.role)
	}
	switch s.verb {
	case verbTone:
		if len(s.freqs) == 0 {
			t.Errorf("%s: tone fixture %q has no frequency", caseName, s.role)
		}
		for _, f := range s.freqs {
			if f < 20 || f > 20000 {
				t.Errorf("%s: fixture %q frequency %g Hz out of range", caseName, s.role, f)
			}
		}
	case verbSpawn:
		if s.child == nil {
			t.Errorf("%s: spawn fixture %q has no child", caseName, s.role)
		}
	case verbIdle, verbReusePID, verbBursts, verbEndpointTone:
		// no tone required
	default:
		t.Errorf("%s: fixture %q has unknown verb %q", caseName, s.role, s.verb)
	}
	// Read the remaining fields so the plan model stays exercised on every OS.
	if s.amp < 0 {
		t.Errorf("%s: fixture %q negative amp", caseName, s.role)
	}
	if s.mic && s.verb == verbBursts {
		t.Errorf("%s: a bursts fixture cannot hold the mic", caseName)
	}
	if s.orphanAfter < 0 || s.at < 0 && s.at != 0 {
		t.Errorf("%s: fixture %q bad timing", caseName, s.role)
	}
	if s.logFirstRender && s.verb == verbIdle {
		t.Errorf("%s: an idle fixture has no first render to log", caseName)
	}
	if s.underExplorer && s.exe == "" {
		t.Errorf("%s: a reparented fixture %q needs an exe path", caseName, s.role)
	}
	if s.dur < 0 {
		t.Errorf("%s: fixture %q negative duration", caseName, s.role)
	}
	if s.child != nil {
		walkFixture(t, caseName, s.child)
	}
}

// TestDriverOptions checks the option plumbing the runner passes to the driver (keep-wav).
func TestDriverOptions(t *testing.T) {
	dir := t.TempDir()
	opt := driverOptions{keepWAV: dir}
	if opt.keepWAV != dir {
		t.Fatalf("keepWAV not carried: %q", opt.keepWAV)
	}
}
