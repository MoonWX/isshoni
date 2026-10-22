package mediatest_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/fake"
	"github.com/MoonWX/isshoni/internal/media/mediatest"
)

// mutantEnv names the mutant that TestMutantHelper runs Conformance against, in TestConformanceCatchesMutants's
// child process.
const mutantEnv = "ISSHONI_MEDIATEST_MUTANT"

// TestConformanceCatchesMutants: Conformance fails engines that break its rules, at the checks they break, and passes
// them on the others. Each mutant is the fake engine with some rules broken; it runs in a child process, because a
// failing Conformance fails the test that runs it.
func TestConformanceCatchesMutants(t *testing.T) {
	if os.Getenv(mutantEnv) != "" {
		t.Skip("in a mutant's child process")
	}
	for _, tc := range []struct {
		mutant     string
		fail, pass []string
	}{
		// No Reinit flag on the first packets: check 2 fails and stops the suite.
		{"no-reinit", []string{"2-start"}, []string{"1-info-probe"}},
		// SetLayerActive, Reconfigure and the busy error ignored, and a Next after Stop that isn't ErrStopped.
		{"controls", []string{"6-layer-active", "7-profile", "9-busy", "8-stop"},
			[]string{"2-start", "3-media", "10-now", "4-keyframe", "5-bitrate"}},
		// Every other f delta frame lost, keyframe requests ignored, and a clock 100 ms ahead.
		{"timing", []string{"3-media", "10-now", "4-keyframe"}, []string{"2-start", "6-layer-active", "7-profile",
			"9-busy", "8-stop"}},
	} {
		t.Run(tc.mutant, func(t *testing.T) {
			t.Parallel()
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestMutantHelper$", "-test.v", //nolint:gosec // the test binary
				"-test.count=1")
			cmd.Env = append(os.Environ(), mutantEnv+"="+tc.mutant)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("Conformance passed the %s mutant:\n%s", tc.mutant, out)
			}
			for _, c := range tc.fail {
				if !strings.Contains(string(out), "--- FAIL: TestMutantHelper/"+c+" ") {
					t.Errorf("check %s did not fail", c)
				}
			}
			for _, c := range tc.pass {
				if !strings.Contains(string(out), "--- PASS: TestMutantHelper/"+c+" ") {
					t.Errorf("check %s did not pass", c)
				}
			}
			if t.Failed() {
				t.Logf("the child's output:\n%s", out)
			}
		})
	}
}

// TestMutantHelper runs Conformance against the mutant that mutantEnv names; it runs only in a child process.
func TestMutantHelper(t *testing.T) {
	m := os.Getenv(mutantEnv)
	if m == "" {
		t.Skip("runs only in TestConformanceCatchesMutants's child processes")
	}
	mediatest.Conformance(t, func(*testing.T) media.Engine {
		return &mutantEngine{Engine: fake.NewEngine(fake.EngineConfig{}), mutant: m}
	}, media.Source{ID: fake.ListedMonitorID, Kind: media.SourceScreen}, mediatest.Options{Listed: true})
}

// mutantEngine is a fake engine with some of the contract's rules broken.
type mutantEngine struct {
	*fake.Engine
	mutant string
}

func (e *mutantEngine) Start(ctx context.Context, cfg media.Config) (media.Session, error) {
	s, err := e.Engine.Start(ctx, cfg)
	if e.mutant == "controls" && errors.Is(err, media.ErrBusy) {
		return nil, &media.Error{Code: media.CodeInternal, Op: "start"}
	}
	if err != nil {
		return nil, err
	}
	return &mutantSession{Session: s, mutant: e.mutant}, nil
}

type mutantSession struct {
	media.Session
	mutant string
	frames int // f frames seen (Next's goroutine only)
}

func (s *mutantSession) Next(ctx context.Context) (media.Packet, error) {
	for {
		p, err := s.Session.Next(ctx)
		switch s.mutant {
		case "no-reinit":
			p.Reinit = false
		case "controls":
			if errors.Is(err, media.ErrStopped) {
				err = &media.Error{Code: media.CodeInternal, Op: "next"}
			}
		case "timing":
			if err == nil && p.Kind == media.KindVideo && p.Layer == "f" {
				s.frames++
				if s.frames%2 == 0 && !p.Keyframe { // every other delta frame
					continue
				}
			}
		}
		return p, err
	}
}

func (s *mutantSession) Now() int64 {
	if s.mutant == "timing" {
		return s.Session.Now() + 100_000_000
	}
	return s.Session.Now()
}

func (s *mutantSession) RequestKeyframe(layer string) {
	if s.mutant != "timing" {
		s.Session.RequestKeyframe(layer)
	}
}

func (s *mutantSession) SetLayerActive(layer string, active bool) error {
	if s.mutant == "controls" {
		return nil
	}
	return s.Session.SetLayerActive(layer, active)
}

func (s *mutantSession) Reconfigure(r media.Reconfig) error {
	if s.mutant == "controls" {
		return nil
	}
	return s.Session.Reconfigure(r)
}
