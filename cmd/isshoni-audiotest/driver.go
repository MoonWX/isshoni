package main

import (
	"context"
	"errors"
	"io"
	"runtime"
	"sync"

	"github.com/MoonWX/isshoni/internal/media"
)

// This file is the portable boundary between the audiotest's OS-neutral core (the runner, analyzer, report and gate)
// and the engine driver (docs/m2/05-windows-audio.md §18.1). The Windows driver (driver_windows.go) loads
// isshoni-media.dll through native.Open, drives it through internal/desktop/audio.Service, starts the fixtures as
// children of explorer.exe and records the PCM tap; every other OS gets a driver that reports the engine is not
// available yet (driver_other.go returns errUnsupportedOS), so the binary builds on the Mac and CI ubuntu/macOS
// (GOOS=windows is the one that runs the cases).

// errUnsupportedOS is what newDriver returns on an OS without the engine. It is the only newDriver error selfcheck
// skips: on Windows a DLL that is missing, of the wrong ABI or without an export fails selfcheck (05 §18.2).
var errUnsupportedOS = errors.New("isshoni-audiotest: the Windows audio engine is required for this; this build is " +
	runtime.GOOS)

// driver runs the engine side of the audiotest. Only the Windows driver captures audio; the others return
// errUnsupportedOS. A driver is used for one process invocation and closed at the end.
type driver interface {
	// env is the report's environment (the DLL version and SHA-256, the OS, the machine), recorded in every report.
	env() (dllInfo, osInfo, machineInfo, error)
	// selfCheck loads the DLL, checks the version and every export, runs the probe and a watcher start/stop, with no
	// sound (05 §18.2). It writes progress to w.
	selfCheck(w io.Writer) error
	// probe prints caps, the startup-probe result, the ducking preference and the endpoints, names redacted
	// (05 §18.2).
	probe(w io.Writer) error
	// apps prints "What friends hear" with the built-in rules, as text (05 §18.2).
	apps(w io.Writer) error
	// runCase runs one tone case and returns its recording for the portable verdict (05 §18.5).
	runCase(cs caseSpec, cfg gateConfig) (recording, error)
	// runSpecial runs a case the driver scores itself (timestamps, opus, multi-endpoint, soak, pid-reuse) and returns
	// its report, scored by the portable functions of special.go and soak.go.
	runSpecial(cs caseSpec, cfg gateConfig) (caseReport, error)
	// record records the PCM tap for ro.dur seconds with the built-in rules plus ro.exclude, handing every chunk and a
	// status every second to sink (05 §18.2's `record`).
	record(ro recordOptions, sink recordSink) error
	// redaction is what the driver collected for the report's redaction so far (05 §18.7): its work folders, its
	// own folder, and the app and exe names the engine's reports showed.
	redaction() redactInfo
	// close stops the engine.
	close() error
}

// driverOptions configures newDriver.
type driverOptions struct {
	keepWAV string    // a folder to write the captured tap WAVs into; "" = don't keep
	console io.Writer // where the driver names local things the report never carries (a kept-out process); nil = none
}

// recorder wraps a media.AudioController and stamps every SetExclusions call with the engine clock just before
// forwarding it (05 §18.1), so cases 13 and 15 can measure a rule change from the exact moment it reached the DLL.
// It is portable (the clock is injected), so it is tested on the Mac with the fake engine.
type recorder struct {
	media.AudioController
	clock func() int64

	mu     sync.Mutex
	stamps []int64 // engine-clock ns, oldest first
}

// newRecorder wraps eng; clock returns the engine clock in ns (AudioTest.EngineClockNS on Windows).
func newRecorder(eng media.AudioController, clock func() int64) *recorder {
	return &recorder{AudioController: eng, clock: clock}
}

// SetExclusions stamps the call with the engine clock, then forwards it.
func (r *recorder) SetExclusions(ctx context.Context, ex media.Exclusions) error {
	r.mu.Lock()
	r.stamps = append(r.stamps, r.clock())
	r.mu.Unlock()
	return r.AudioController.SetExclusions(ctx, ex)
}

// lastStamp is the engine clock of the most recent SetExclusions call, and whether there was one.
func (r *recorder) lastStamp() (int64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.stamps) == 0 {
		return 0, false
	}
	return r.stamps[len(r.stamps)-1], true
}

// stampAt is the engine clock of the i-th SetExclusions call (0-based), and whether there was one. Cases 13 and 15
// read stampCount right before their Decide or SetAppDecision and take the next stamp as T_set (05 §18.5).
func (r *recorder) stampAt(i int) (int64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i < 0 || i >= len(r.stamps) {
		return 0, false
	}
	return r.stamps[i], true
}

// stampCount is how many SetExclusions calls have been recorded.
func (r *recorder) stampCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.stamps)
}
