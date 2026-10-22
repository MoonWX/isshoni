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
// the Pending values first (WA41); the session and the share controller (WA58, WA65), auth.state and server
// switching from the SPA (WA71), audio (WA77), tray, toasts and system events (WA68, WA80), the updater (WA84).
package app

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/desktop/i18n"
	"github.com/MoonWX/isshoni/internal/desktop/settings"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/version"
)

// Deps is what the app consumes (03 §6.1). Settings, Servers, Shell and OS are required.
type Deps struct {
	Settings *settings.Store     // settings.json; also 05's audio.SettingsStore through settings.AudioStore
	Servers  *deviceauth.Servers // 02: servers.json, one Manager per server, the credential store
	Session  Session             // C2: 01's core for the active linked server (internal/desktop/session, WA58)
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

// Session is what the app runs of internal/desktop/session (03 §9): C2 for the active linked server.
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
	log   *slog.Logger
	clock func() time.Time

	ctx    context.Context // the app's life: cancelled by Quit, or when Run returns
	cancel context.CancelFunc
	wg     sync.WaitGroup

	quitOnce sync.Once

	probeDone chan struct{}
	probe     media.Probe
	probeErr  error

	// sharing reports a live share and its room (03 §13.2's "live share" rows). The share controller replaces it
	// (README WA65); until then there is never a live share.
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
	ctx, cancel := context.WithCancel(context.Background())
	a := &App{
		d:         d,
		log:       d.Log.With("component", "app"),
		clock:     d.Clock,
		ctx:       ctx,
		cancel:    cancel,
		probeDone: make(chan struct{}),
		sharing:   func() (string, bool) { return "", false },
	}
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
				a.log.Error("app: a part stopped", "part", p.name, "err", err)
			}
		})
	}
	<-a.ctx.Done()
	a.wg.Wait()
	return nil
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

// Quit stops the app (03 §11.6): the share first (01's Share.Stop, once the share controller exists), then 05's
// audio service, the settings flush, and the shell's quit; Run returns. stopShare false while a share is live is
// the bridge's busy (App.Quit), never this method's concern. Quit runs once; later calls do nothing.
func (a *App) Quit(stopShare bool) {
	_ = stopShare // the share controller (README WA65) stops a live share here, bounded by QuitTimeout
	a.quitOnce.Do(func() {
		if a.d.Audio != nil {
			if err := a.d.Audio.Close(); err != nil {
				a.log.Warn("app: the audio service didn't close cleanly", "err", err)
			}
		}
		if err := a.d.Settings.Flush(); err != nil {
			a.log.Warn("app: settings not saved at quit", "err", err)
		}
		a.cancel()
		a.d.Shell.Quit()
	})
}

// OnAuthState is deviceauth.ServersOptions.OnState: a server's link changed. The auth.state event for the active
// server is README WA71's; until then it is logged at debug only.
func (a *App) OnAuthState(s deviceauth.State) {
	a.log.Debug("app: link state", "state", s)
}
