//go:build windows || (darwin && cgo)

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/MoonWX/isshoni/internal/desktop/app"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/version"
)

// instance is the single-instance lock (winos.SingleInstance).
type instance interface {
	Acquire() (first bool, err error)
	Serve(onArgs func(args []string)) error
	Forward(args []string) error
	Close() error
}

// starter is 03 §13.4's order of main, with its OS parts behind functions so that the order is tested with fakes.
// main's first action (the DLL search order) has run before start is called.
type starter struct {
	log            *slog.Logger
	stdout         func() io.Writer            // --version's output
	singleInstance func() (instance, error)    // step 3
	holdRunning    func() error                // step 4: the running mutex
	takePending    func() (app.Pending, error) // step 6
	// runApp is step 7: the app with its services and Wails, for the first instance; its result is the exit code.
	// fwd holds what second launches forwarded since step 3 and hands it to the app once it has booted; inst is the
	// held single-instance lock (nil when it is unavailable), which runApp closes as soon as Wails has stopped.
	runApp func(p app.Args, args []string, pending app.Pending, fwd *forwarded, inst instance) int
}

// The exit codes of the app's modes (03 §13.4; README §4 "Errors").
const (
	exitOK          = 0
	exitFailed      = 1 // a one-shot mode failed, or the app couldn't start
	exitUnavailable = 2 // a mode this build doesn't have yet
)

// start runs a start in 03 §13.4's order and returns the exit code. Each step is skipped or answered as that
// section says; the steps that later slices own are marked where they go.
func (s *starter) start(args []string) int {
	p := app.ParseArgs(args)

	// Step 0: --sound-check-helper as os.Args[1] runs 05's helper before any other step (README WA77 dispatches it
	// to audio.RunSoundCheckHelper; until then this build has none).
	if p.SoundCheckHelper {
		s.log.Error("desktop: --sound-check-helper is not in this build yet")
		return exitUnavailable
	}

	// Step 1: the one-shot modes, never with --open-url (ParseArgs leaves OneShot empty then). No window, no single
	// instance, no mutex.
	if p.OneShot != "" {
		return s.oneShot(p)
	}

	// Step 2, the shell relaunch (README WA77), and step 2a, the installer probe with its Pending values (README
	// WA59), go here.

	// Step 3: single instance. A second instance forwards its arguments unchanged and exits 0 (03 §13.3).
	fwd := newForwarded(s.log)
	inst, err := s.singleInstance()
	if err != nil {
		s.log.Warn("desktop: single instance unavailable; starting as the first instance", "err", err)
		inst = nil
	}
	if inst != nil {
		first, err := inst.Acquire()
		switch {
		case err != nil:
			s.log.Warn("desktop: single instance unavailable; starting as the first instance", "err", err)
			inst = nil
		case !first:
			if err := inst.Forward(args); err != nil {
				s.log.Warn("desktop: the running app didn't take this start's arguments", "err", err)
			}
			return exitOK
		default:
			held := inst
			defer func() { _ = held.Close() }()
			// Second launches are served from here on, so one that comes while step 7 loads the engine, checks
			// WebView2 or opens Credential Manager doesn't give up after its 2 s: what it forwards waits in fwd
			// until the app has booted (03 §13.3).
			if err := inst.Serve(fwd.receive); err != nil {
				s.log.Warn("desktop: second launches can't reach this one", "err", err)
			}
		}
	}

	// Step 4: the running mutex, held until the process exits (06 §2.4, §6.7).
	if err := s.holdRunning(); err != nil {
		s.log.Warn("desktop: the running mutex wasn't created", "err", err)
	}

	// Step 5, the start-time update with its Pending values (README WA84), goes here. A start that exits there also
	// exits with what fwd queued meanwhile.

	// Step 6: the Pending values, read and deleted before anything of the app runs (03 §13.2).
	pending, err := s.takePending()
	if err != nil {
		s.log.Warn("desktop: the pending server values weren't all read and deleted", "err", err)
	}

	// Step 7: the app.
	return s.runApp(p, args, pending, fwd, inst)
}

// maxQueuedForwards bounds what second launches can queue before the first instance's app has booted.
const maxQueuedForwards = 8

// forwarded holds the command lines that second launches forward (single instance's onArgs, served from step 3 on)
// until the app has booted (Boot ran), then hands each to the app (App.HandleArgs), in the order they came. Once
// Wails shuts down it drops them: the window a command line would show is being destroyed, and showing it then
// would block forever.
type forwarded struct {
	log *slog.Logger

	mu      sync.Mutex
	handle  func(args []string) // set by start
	started bool                // start handed over the queue: later command lines go to handle at once
	stopped bool
	queue   [][]string
}

func newForwarded(log *slog.Logger) *forwarded { return &forwarded{log: log} }

// receive is single instance's onArgs.
func (f *forwarded) receive(args []string) {
	f.mu.Lock()
	switch {
	case f.stopped:
		f.mu.Unlock()
		f.log.Debug("desktop: a second launch's command line came while the app quits; dropped")
		return
	case !f.started:
		queued := len(f.queue) < maxQueuedForwards
		if queued {
			f.queue = append(f.queue, args)
		}
		f.mu.Unlock()
		if !queued {
			f.log.Info("desktop: too many second launches before the app started; one dropped")
		}
		return
	}
	handle := f.handle
	f.mu.Unlock()
	handle(args)
}

// start hands what was queued to handle, in order (also what comes meanwhile); later command lines go to handle at
// once. It does nothing after stop.
func (f *forwarded) start(handle func(args []string)) {
	f.mu.Lock()
	f.handle = handle
	f.mu.Unlock()
	for {
		f.mu.Lock()
		queue := f.queue
		f.queue = nil
		if f.stopped {
			f.mu.Unlock()
			return
		}
		if len(queue) == 0 {
			f.started = true
			f.mu.Unlock()
			return
		}
		f.mu.Unlock()
		for _, args := range queue {
			if f.isStopped() {
				return
			}
			handle(args)
		}
	}
}

// stop drops what is queued and every later command line. Wails' shutdown calls it, and runApp once Wails has
// stopped.
func (f *forwarded) stop() {
	f.mu.Lock()
	f.stopped = true
	f.queue = nil
	f.mu.Unlock()
}

func (f *forwarded) isStopped() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}

// oneShot runs a one-shot mode (03 §13.4 step 1).
func (s *starter) oneShot(p app.Args) int {
	switch p.OneShot {
	case app.ModeVersion:
		if err := writeVersion(s.stdout()); err != nil {
			s.log.Error("desktop: --version not written", "err", err)
			return exitFailed
		}
		return exitOK
	case app.ModeEmbedding:
		// A toast button pressed while no instance runs: every action carries a nonce of a run that is gone, so all
		// are dropped (03 §15). The activator and the "open" action come with toasts (README WA68).
		s.log.Info("desktop: a toast activation without a running app; dropped")
		return exitOK
	default:
		// --self-check, --forget-credentials and --purge-data (README WA59); --update-now (WA84, updatetest builds
		// only).
		s.log.Error("desktop: this mode is not in this build yet", "mode", string(p.OneShot))
		return exitUnavailable
	}
}

// versionLine is --version's JSON line (03 §13.4 step 1, 06 §2.4).
type versionLine struct {
	Version  string `json:"version"`
	Commit   string `json:"commit"`
	Date     string `json:"date"`
	Protocol struct {
		Min int `json:"min"`
		Max int `json:"max"`
	} `json:"protocol"`
	// EngineABI is the ABI major Go was built for (media.ABIVersion, 1), read without loading the DLL.
	EngineABI int `json:"engineAbi"`
}

// writeVersion writes exactly one JSON line.
func writeVersion(w io.Writer) error {
	info := version.Info()
	v := versionLine{Version: info.Version, Commit: info.Commit, Date: info.Date, EngineABI: media.ABIVersion}
	v.Protocol.Min, v.Protocol.Max = protocol.MinVersion, protocol.Version
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "%s\n", data); err != nil {
		return err
	}
	return nil
}
