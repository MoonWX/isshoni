// Package app is the desktop app's core (docs/m2/03-desktop-app.md, "03" below, §6.1): the App that owns the share
// controller, the sessions, the tray view, the window and close policy, system events and the wiring between 02's
// servers, 01's core, 05's audio service and 06's updater. It knows nothing of Wails: the shell reaches it through
// the Shell interface, the Windows layer through OSHooks (internal/desktop/winos), and page script only through the
// bound services of internal/desktop/bridge.
//
// It also owns the command line's meaning (03 §13.1: ParseArgs, the --open-url rule, --server) and deep-link
// handling (§13.2: HandleLink, PendingLink, LinkHandled), including the Pending values the installer leaves in the
// registry (TakePending, Boot).
//
// Slices fill it in order (README §5): the skeleton, the Shell and OSHooks interfaces, the command line, links and
// the Pending values first (WA41); C2's session and the share controller (WA58: share.go builds them in New),
// auth.state and server switching from the SPA (WA71, which calls the session's Refresh after 02's SetActive), audio
// (WA77), tray, toasts and system events (WA68, WA80), the updater (WA84).
package app

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/client/core"
	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/desktop/i18n"
	"github.com/MoonWX/isshoni/internal/desktop/settings"
	"github.com/MoonWX/isshoni/internal/desktop/share"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/version"
)

// Deps is what the app consumes (03 §6.1). Settings, Servers, Shell and OS are required.
type Deps struct {
	Settings *settings.Store     // settings.json; also 05's audio.SettingsStore through settings.AudioStore
	Servers  *deviceauth.Servers // 02: servers.json, one Manager per server, the credential store
	Session  Session             // C2 (03 §9); nil: New builds internal/desktop/session for Engine (share.go)
	Engine   media.Engine        // native.Open on Windows; fake.NewEngine in dev and e2e builds; nil when none opened
	Audio    AudioService        // 05's *audio.Service (internal/desktop/audio, wired by WA77)
	Updater  Updater             // 06's *update.Updater, built in the first instance only (wired by WA84)
	Shell    Shell               // cmd/isshoni-desktop over Wails; e2ehost: a recording fake
	OS       OSHooks             // winos on Windows; stubs elsewhere
	I18n     *i18n.Catalog
	Clock    func() time.Time // nil means time.Now
	Log      *slog.Logger     // nil means slog.Default()
	// InsecureLoopback is true in dev and e2e builds: --server and the Pending values may name an http:// loopback
	// server (03 §8.1, §13.1). A link never may.
	InsecureLoopback bool
}

// Session is what the app runs of internal/desktop/session (03 §9): C2 for the active linked server. The share
// controller and 02's OnState reach C2 only through a *session.Session (share.go's publisher), which New builds when
// Deps.Session is nil and an engine is open.
type Session interface {
	Run(ctx context.Context) error // until ctx ends
}

// AudioService is what the app runs of 05's audio.Service (05 §14.2).
type AudioService interface {
	Run(ctx context.Context) error // the polling loop; returns when ctx ends
	Close() error
}

// Updater is what the app runs of 06's update.Updater (06 §8.4).
type Updater interface {
	Run(ctx context.Context) error // the schedule; returns when ctx ends
}

// QuitTimeout bounds Quit's wait for the server's replies (03 §11.6, §20).
const QuitTimeout = 3 * time.Second

// probeWait is how long Probe waits for the start-time probe (03 §6.1: Share.Capabilities waits at most 2 s).
const probeWait = 2 * time.Second

// App is the desktop app. Its methods are safe for concurrent use.
type App struct {
	d     Deps
	log   *slog.Logger // component=app
	spa   *slog.Logger // component=spa: App.Log's lines (the same unscoped logger, so one component key)
	clock func() time.Time

	ctx    context.Context // the app's life: cancelled by Quit, or when Run returns
	cancel context.CancelFunc
	wg     sync.WaitGroup

	quitOnce sync.Once

	probeDone chan struct{}
	probe     media.Probe
	probeErr  error

	pub    publisher         // C2 for the share controller; nil without a *session.Session
	shares *share.Controller // 03 §10.1

	// sharing reports a live share and its room (03 §13.2's "live share" rows): the share controller's Live.
	sharing func() (roomID string, live bool)

	spaLog spaLog // App.Log's rate limit

	mu       sync.Mutex
	running  bool
	pending  *PendingLink // the link not yet acted on (App.PendingLink)
	lastLink time.Time    // when the last link was handled (03 §13.2: at most one per second)
	ownConns []string     // App.OwnConnections: C1's user.connections, for playback.state's expiry (WA77)
}

// New checks d and returns the app; it starts nothing (Run does).
func New(d Deps) (*App, error) {
	switch {
	case d.Settings == nil:
		return nil, errors.New("app: Deps.Settings is required")
	case d.Servers == nil:
		return nil, errors.New("app: Deps.Servers is required")
	case d.Shell == nil:
		return nil, errors.New("app: Deps.Shell is required")
	case d.OS == nil:
		return nil, errors.New("app: Deps.OS is required")
	}
	if d.Clock == nil {
		d.Clock = time.Now
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Session == nil && d.Engine != nil {
		s, err := newSession(d)
		if err != nil {
			return nil, err
		}
		d.Session = s
	}
	pub, _ := d.Session.(publisher)
	ctx, cancel := context.WithCancel(context.Background())
	a := &App{
		d:         d,
		log:       d.Log.With("component", "app"),
		spa:       d.Log.With("component", "spa"),
		clock:     d.Clock,
		ctx:       ctx,
		cancel:    cancel,
		probeDone: make(chan struct{}),
		pub:       pub,
		shares:    newShares(d, sharePublisher{pub}),
	}
	a.sharing = a.shares.Live
	return a, nil
}

// Run runs the app's parts until ctx ends or Quit: the active server's manager (02's Start), the engine's
// start-time probe, the session, 05's audio service and 06's updater when present (the tray and system events join
// with their slices). It returns after every part it started has returned. Run may be called once.
func (a *App) Run(ctx context.Context) error {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return errors.New("app: Run called twice")
	}
	a.running = true
	a.mu.Unlock()

	stop := context.AfterFunc(ctx, a.cancel)
	defer stop()
	defer a.cancel()

	a.startActive()
	if e := a.d.Engine; e != nil {
		if b := e.Info().Build; b != "" && b != version.Version() {
			// The installer ships both together; dev builds mix them on purpose (03 §13.4 step 7, 04 §5.5).
			a.log.Warn("app: the engine is another build than the app", "engine", b, "app", version.Version())
		}
		a.wg.Go(func() {
			defer close(a.probeDone)
			a.probe, a.probeErr = e.Probe(a.ctx, false)
			if a.probeErr != nil && a.ctx.Err() == nil {
				a.log.Warn("app: the engine's probe failed", "err", a.probeErr)
			}
		})
	} else {
		close(a.probeDone)
	}
	for _, p := range []struct {
		name string
		r    interface{ Run(context.Context) error }
	}{{"session", a.d.Session}, {"audio", a.d.Audio}, {"updater", a.d.Updater}} {
		if p.r == nil {
			continue
		}
		a.wg.Go(func() {
			if err := p.r.Run(a.ctx); err != nil && a.ctx.Err() == nil {
				a.log.Error("app: a part stopped", "part", p.name, errAttr(err))
			}
		})
	}
	<-a.ctx.Done()
	a.wg.Wait()
	a.shares.Close() // C2 has closed, so its share has ended: Close finds its watcher done
	return nil
}

// errAttr is the log attribute of an error that one of the app's parts returned (Session, AudioService, Updater).
// Log files are what users attach to public issues (03 §17), and a part is an interface whose errors the app can't
// vouch for: one may wrap a server's answer, a request's headers, a path with the user's name or the engine's detail
// (05 §16 keeps that out of 05's own lines). So the attribute names the error's kind, never its text: a media error's
// Redacted form (op, code and reason, never its detail), a context error's text, and "error" for anything else, as
// 05's errAttr does.
//
// 01's *core.StopError is logged with its text, which is only its reason and the server's code ("core: stopped:
// unlinked (session_revoked)").
func errAttr(err error) slog.Attr {
	var (
		me *media.Error
		se *core.StopError
	)
	switch {
	case errors.As(err, &me):
		return slog.String("err", me.Redacted())
	case errors.As(err, &se):
		return slog.String("err", (&core.StopError{Reason: se.Reason, Code: se.Code}).Error())
	case errors.Is(err, context.Canceled):
		return slog.String("err", context.Canceled.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return slog.String("err", context.DeadlineExceeded.Error())
	}
	return slog.String("err", "error")
}

// Probe returns the engine's start-time probe, waiting for it at most 2 s (03 §6.1); media.ErrUnsupportedOS's kind
// of error when the app has no engine.
func (a *App) Probe(ctx context.Context) (media.Probe, error) {
	if a.d.Engine == nil {
		return media.Probe{}, &media.Error{Code: media.CodeUnsupportedOS, Op: "probe", Detail: "no media engine"}
	}
	t := time.NewTimer(probeWait)
	defer t.Stop()
	select {
	case <-a.probeDone:
		return a.probe, a.probeErr
	case <-t.C:
		return media.Probe{}, context.DeadlineExceeded
	case <-ctx.Done():
		return media.Probe{}, ctx.Err()
	}
}

// startActive starts the active server's manager in the background (02 §11.6: the app calls Start on the active
// one at boot and after Add or SetActive). Start returns once the first refresh settled or the app's context ends.
func (a *App) startActive() {
	m := a.d.Servers.Active()
	if m == nil {
		return
	}
	a.wg.Go(func() {
		if err := m.Start(a.ctx); err != nil && a.ctx.Err() == nil {
			a.log.Warn("app: the active server's link didn't start", "err", err)
		}
	})
}

// Quit stops the app (03 §11.6): the share first (the controller's Stop: 01's Share.Stop, bounded by QuitTimeout),
// then 05's audio service, the settings flush, and the shell's quit; Run returns. stopShare false while a share is
// live is the bridge's busy (App.Quit), never this method's concern: the share then ends with C2's close. Quit runs
// once; later calls do nothing.
func (a *App) Quit(stopShare bool) {
	a.quitOnce.Do(func() {
		if stopShare {
			ctx, cancel := context.WithTimeout(context.Background(), QuitTimeout)
			a.shares.Stop(ctx, share.CauseUser)
			cancel()
		}
		if a.d.Audio != nil {
			if err := a.d.Audio.Close(); err != nil {
				a.log.Warn("app: the audio service didn't close cleanly", errAttr(err))
			}
		}
		if err := a.d.Settings.Flush(); err != nil {
			a.log.Warn("app: settings not saved at quit", "err", err)
		}
		a.cancel()
		a.d.Shell.Quit()
	})
}

// OnAuthState is deviceauth.ServersOptions.OnState: a server's link changed. C2's session looks again (a relink
// builds a new core client, an unlinked or incompatible active server closes it, 03 §9). The auth.state event for
// the active server is README WA71's; until then it is logged at debug only. It never blocks.
func (a *App) OnAuthState(s deviceauth.State) {
	a.log.Debug("app: link state", "state", s)
	if a.pub != nil {
		a.pub.OnAuthState(s)
	}
}
