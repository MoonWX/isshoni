//go:build windows || (darwin && cgo)

package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/MoonWX/isshoni/internal/desktop/app"
	"github.com/MoonWX/isshoni/internal/desktop/settings"
	"github.com/MoonWX/isshoni/internal/desktop/winos"
	"github.com/MoonWX/isshoni/internal/update"
	"github.com/MoonWX/isshoni/internal/version"
)

// 06's updater in the app's start (03 §13.4 step 5, §16.1; 06 §8.7, §8.11; README WA84): the first instance builds
// the updater (second instances never do) over settings.json, which step 5 opens for it and step 7 goes on with, and
// applies a pending update before any window or connection opens, except at an --autostart start (the update then
// stays ready and the tray offers it). --update-now (builds with the updatetest tag only) checks once and applies.

// startUpdater is what step 5 runs of 06's updater (*update.Updater).
type startUpdater interface {
	PendingAtStartup(autostart bool) (*update.Pending, error)
	Apply(ctx context.Context, p *update.Pending, relaunch bool) error
}

// nowUpdater is what --update-now runs of 06's updater.
type nowUpdater interface {
	CheckNow(ctx context.Context) (update.Status, error)
	Ready() *update.Pending
	Apply(ctx context.Context, p *update.Pending, relaunch bool) error
}

var (
	_ startUpdater = (*update.Updater)(nil)
	_ nowUpdater   = (*update.Updater)(nil)
)

// startApplyTimeout bounds step 5's Apply (06 waits up to 10 s for the installer's mutex).
const startApplyTimeout = 30 * time.Second

// startTimeUpdate is 03 §13.4 step 5 in the first instance. It reports whether the start must exit 0 now: a pending
// update's installer runs (Apply returned once it holds the installing mutex). The link this start came with
// (--open-url), or the newest one a second launch forwarded meanwhile, goes to PendingServer and PendingRoom first,
// so the relaunched app opens it. Nothing is applied at an --autostart start (06's PendingAtStartup answers nil), and
// an Apply that fails is logged, the values just written are taken back (the link reaches the app once, from where
// it came), and the start goes on.
func (s *starter) startTimeUpdate(p app.Args, fwd *forwarded) bool {
	if s.openUpdater == nil {
		return false
	}
	u, err := s.openUpdater()
	if err != nil {
		s.log.Warn("desktop: no updater at this start", "err", err)
		return false
	}
	pend, err := u.PendingAtStartup(p.Autostart)
	if err != nil {
		s.log.Warn("desktop: the start-time update check had a problem", "code", update.CodeOf(err), "err", err)
	}
	if pend == nil {
		return false
	}
	wrote := false
	if link := newestLink(p.Link, fwd.queued()); link != "" {
		// Never log the link: its path names a room.
		if pv, err := pendingFromLink(link); err != nil {
			s.log.Info("desktop: this start's link isn't valid; not kept for the update's relaunch", "err", err)
		} else if err := s.writePending(pv); err != nil {
			s.log.Warn("desktop: this start's link wasn't kept for the update's relaunch", "err", err)
		} else {
			wrote = true
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), startApplyTimeout)
	defer cancel()
	if err := u.Apply(ctx, pend, true); err != nil {
		s.log.Warn("desktop: the pending update wasn't applied; starting this version", "version", pend.Version,
			"code", update.CodeOf(err), "not_installed", errors.Is(err, update.ErrNotInstalled))
		if wrote {
			// The link reaches this start's app from where it came (its own --open-url, or fwd's queue), so the
			// Pending values just written are taken back: step 6 would hand the same link to the app again.
			if _, err := s.takePending(); err != nil {
				s.log.Warn("desktop: the link kept for the update's relaunch wasn't taken back", "err", err)
			}
		}
		return false
	}
	s.log.Info("desktop: the pending update's installer runs; it starts the app when it is done",
		"version", pend.Version)
	fwd.stop()
	return true
}

// newestLink is the newest --open-url link among this start's own (own) and what second launches forwarded since
// step 3 (queued, oldest first); "" when none has one.
func newestLink(own string, queued [][]string) string {
	link := own
	for _, args := range queued {
		if l := app.ParseArgs(args).Link; l != "" {
			link = l
		}
	}
	return link
}

// queued returns a copy of the command lines waiting for the app.
func (f *forwarded) queued() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.queue))
	copy(out, f.queue)
	return out
}

// updateNow is --update-now (06 §8.11; builds with the updatetest tag): one CheckNow (it never waits for the
// wizard); when it leaves an update ready, Apply with relaunch. Exit 0 when the installer runs or the app is up to
// date, 1 otherwise. A one-shot mode: no window, single instance or running mutex.
func updateNow(ctx context.Context, log *slog.Logger, open func() (nowUpdater, error)) int {
	u, err := open()
	if err != nil {
		log.Error("desktop: --update-now has no updater", "err", err)
		return exitFailed
	}
	st, err := u.CheckNow(ctx)
	if err != nil {
		log.Error("desktop: --update-now's check failed", "code", update.CodeOf(err), "err", err)
		return exitFailed
	}
	switch st.State {
	case update.StateUpToDate:
		log.Info("desktop: --update-now: up to date", "version", st.Current)
		return exitOK
	case update.StateReady:
		p := u.Ready()
		if p == nil {
			log.Error("desktop: --update-now: the ready update went away")
			return exitFailed
		}
		if err := u.Apply(ctx, p, true); err != nil {
			log.Error("desktop: --update-now: the update wasn't applied", "version", p.Version,
				"code", update.CodeOf(err), "err", err)
			return exitFailed
		}
		log.Info("desktop: --update-now: the installer runs", "version", p.Version)
		return exitOK
	}
	log.Error("desktop: --update-now found nothing to apply", "state", string(st.State), "code", st.ErrorCode)
	return exitFailed
}

// earlyParts are what step 5 opens for the first instance and step 7 goes on with: settings.json (its updates
// settings decide whether a pending update applies, 06 §8.8) and 06's updater. Both nil when step 5 didn't open them.
type earlyParts struct {
	settings *settings.Store
	updater  *update.Updater
}

// open is the starter's openUpdater: settings.json in the data root, then the updater over it.
func (e *earlyParts) open() (startUpdater, error) {
	root, err := dataRoot()
	if err != nil {
		return nil, err
	}
	st, err := settings.Open(settings.Options{Dir: root,
		UpdateChecks: winos.NewRegistry(winos.RegistryKey).UpdateChecks, Logger: lateLogger()})
	if err != nil {
		return nil, err
	}
	e.settings = st
	u, err := newUpdater(root, st)
	if err != nil {
		return nil, err
	}
	e.updater = u
	return u, nil
}

// newUpdater is 06's updater for the data root, asking settings.json before every check: updates.check and the
// machine policy (Enabled), updates.channel, and wizardDone (the scheduled first check waits for it).
func newUpdater(root string, st *settings.Store) (*update.Updater, error) {
	return update.New(update.Options{
		CurrentVersion: version.Version(),
		Channel:        func() update.Channel { return update.Channel(st.Updates().Channel) },
		Enabled:        func() bool { return st.Updates().Check && !update.PolicyDisabled() },
		WizardDone:     st.WizardDone,
		DataDir:        root,
		Logger:         lateLogger(),
	})
}

// lateLogger logs through whatever slog.Default() is when a line is written, not when the logger was made: the
// parts step 5 opens (settings.json, the updater) keep their logger for the app's life, and step 7 replaces the
// default with the file log (03 §17).
func lateLogger() *slog.Logger { return slog.New(lateHandler{}) }

// lateHandler is lateLogger's handler: each call goes to slog.Default()'s handler of the moment, with the attributes
// and groups given to it so far.
type lateHandler struct {
	ops []func(slog.Handler) slog.Handler
}

func (h lateHandler) handler() slog.Handler {
	out := slog.Default().Handler()
	for _, op := range h.ops {
		out = op(out)
	}
	return out
}

func (h lateHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.handler().Enabled(ctx, l)
}

func (h lateHandler) Handle(ctx context.Context, r slog.Record) error {
	return h.handler().Handle(ctx, r)
}

func (h lateHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return lateHandler{ops: append(h.ops[:len(h.ops):len(h.ops)],
		func(x slog.Handler) slog.Handler { return x.WithAttrs(as) })}
}

func (h lateHandler) WithGroup(name string) slog.Handler {
	return lateHandler{ops: append(h.ops[:len(h.ops):len(h.ops)],
		func(x slog.Handler) slog.Handler { return x.WithGroup(name) })}
}
