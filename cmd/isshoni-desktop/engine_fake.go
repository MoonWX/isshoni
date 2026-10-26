//go:build (windows || (darwin && cgo)) && (dev || e2e)

package main

import (
	"log/slog"
	"os"

	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/fake"
)

// fakeAudioEnv names 05's fake audio scenario (05 §13.3: normal, degraded, degrade-live, …); dev and e2e builds
// only read it.
const fakeAudioEnv = "ISSHONI_FAKE_AUDIO"

// openEngine is 01's fake engine in dev and e2e builds (03 §4.5, §22.4): Decodable frames at 640×360, which a
// browser decodes and whose I_PCM IDRs stay small enough for the pacer (01 §6.2), with 05's audio scenarios. No
// release build compiles this file (06's release check fails one that links internal/media/fake).
func openEngine(*slog.Logger, bool) (media.Engine, error) {
	return fake.NewEngine(fake.EngineConfig{
		Mode:          fake.Decodable,
		SourceSize:    [2]int{640, 360},
		AudioScenario: os.Getenv(fakeAudioEnv),
	}), nil
}
