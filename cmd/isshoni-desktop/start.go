//go:build windows || (darwin && cgo)

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/MoonWX/isshoni/internal/client/credstore"
	"github.com/MoonWX/isshoni/internal/desktop/app"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/version"
)

// exitUnavailable is the exit code of a mode this build doesn't have yet (03 §13.4; exitOK and exitFailed are in
// oneshot.go).
const exitUnavailable = 2

// installingMutex is the installer's mutex (06 §2.4): it exists while an installer runs (interactive, /S or a
// start-time update), and the app probes it at step 2a.
const installingMutex = `Local\io.isshoni.desktop.installing`

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
	log    *slog.Logger
	stdout func() io.Writer // --version's output
	// installerRunning is step 2a's probe of installingMutex (OpenMutexW): true while an installer runs, false
	// when the name doesn't exist (ERROR_FILE_NOT_FOUND), an error otherwise.
	installerRunning func() (bool, error)
	writePending     func(app.Pending) error // step 2a: a link start's PendingServer and PendingRoom
	// The one-shot modes' parts (oneshot.go): --self-check's engine (native.Open from the exe's folder) and SPA
	// check (the asset handler over the bundled SPA); the credential store and 02's servers of
	// --forget-credentials and --purge-data; --purge-data's data root and how it is deleted (os.RemoveAll).
	openEngine     func() (selfCheckEngine, error)
	checkSPA       func() error
	openStore      func() (credstore.Store, error)
	openServers    func(credstore.Store) (credentialForgetter, error)
	dataRoot       func() (string, error)
	removeAll      func(string) error
	singleInstance func() (instance, error)    // step 3
	holdRunning    func() error                // step 4: the running mutex
	takePending    func() (app.Pending, error) // step 6
	// runApp is step 7: the app with its services and Wails, for the first instance; its result is the exit code.
	// fwd holds what second launches forwarded since step 3 and hands it to the app once it has booted; inst is the
	// held single-instance lock (nil when it is unavailable), which runApp closes as soon as Wails has stopped.
	runApp func(p app.Args, args []string, pending app.Pending, fwd *forwarded, inst instance) int
}

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

	// Step 2, the shell relaunch (README WA77), goes here.

	// Step 2a: an installer runs right now. A link start leaves its link to the installer's own app start, and
	// every start exits: no single instance, running mutex, updater or Apply (06 §2.4, §8.7).
	if s.installerHandover(p) {
		return exitOK
	}

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

// installerHandover is 03 §13.4 step 2a. It reports whether the start must exit 0 now: when installingMutex
// exists. A start from --open-url then writes its link's origin to PendingServer and its room id to PendingRoom
// (overwriting older values: the newest link wins), which the installer's own app start handles at step 6. When
// the probe fails with anything but "not found", the error is logged and the start goes on.
func (s *starter) installerHandover(p app.Args) bool {
	running, err := s.installerRunning()
	if err != nil {
		s.log.Warn("desktop: couldn't tell whether an installer runs; starting normally", "err", err)
		return false
	}
	if !running {
		return false
	}
	if p.Link != "" {
		// Never log the link: its path names a room. A link that doesn't parse leaves the Pending values alone.
		if pending, err := pendingFromLink(p.Link); err != nil {
			s.log.Info("desktop: an installer runs and this start's link isn't valid; nothing kept", "err", err)
		} else if err := s.writePending(pending); err != nil {
			s.log.Warn("desktop: an installer runs and this start's link wasn't kept", "err", err)
		} else {
			s.log.Info("desktop: an installer runs; its app start opens this start's link")
		}
	}
	s.log.Info("desktop: an installer runs; it starts the app when it is done")
	return true
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
	case app.ModeSelfCheck:
		return selfCheckMode(context.Background(), s.log, p.SelfCheckPath, s.openEngine, s.checkSPA)
	case app.ModeForgetCredentials:
		forgetCredentialsMode(context.Background(), s.log, s.openStore, s.openServers, s.dataRoot, s.removeAll)
		return exitOK // also offline, and whatever went wrong: the uninstaller goes on regardless (06 §6.8)
	case app.ModePurgeData:
		if err := purgeData(context.Background(), s.log, s.openStore, s.openServers, s.dataRoot,
			s.removeAll); err != nil {
			s.log.Error("desktop: the data folder wasn't deleted", "err", err)
		}
		return exitOK // exit 0 whatever happened (03 §13.4 step 1, 06 §2.4): the uninstaller only logs it (06 §6.8)
	default:
		// --update-now (README WA84, updatetest builds only).
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
