// Package bridge is the desktop app's bound services (docs/m2/03-desktop-app.md, "03" below, §6.2): the only Go
// surface that page script can call. Each service is a plain struct, one file per service (App, Auth, Share, Audio,
// Settings, Window, Updates), with no Wails import (03 D2): cmd/isshoni-desktop registers them with Wails'
// application.NewService, whose bindings generator writes their TypeScript into web/src/platform/desktop/bindings/
// (`task gen:bindings`, committed and drift-checked), and the e2e harness (internal/desktop/e2ehost) serves the same
// structs over HTTP (03 §7.4's HttpBridge).
//
// Rules every method follows:
//   - Every exported method of a service type is a binding, so service types have no other exported methods.
//   - A method that does I/O takes a context.Context first: Wails cancels it when the page cancels the call (W11),
//     and the method bounds its own work as well (03 §6.2's timeouts: Probe 10 s, Link 15 s, Start 20 s,
//     AccessToken 15 s, Updates.Check 45 s, any other I/O 10 s).
//   - Arguments are plain JSON values (strings, booleans, numbers, small structs of them), validated here; a bad one
//     is invalid_argument with params.field.
//   - A failing method returns an *Error, whose Error() is 03 §6.4's JSON (W12); errors.go maps every Go error.
//   - No method returns a refresh token or a credential (02 §11.7): the only secret that crosses is the access
//     token of Auth.AccessToken, for the active server's own origin only (03 §5.8).
//
// Methods whose slices haven't landed return not_implemented (README §4 "Interfaces first"): Auth's server changes
// and Unlink (WA71), Settings' writes and hotkeys and Updates (WA84). Audio works once main hands it 05's service
// (WA77); until then it is not_implemented too. Share calls the app's share controller (internal/desktop/share).
//
// Events (Go → SPA, 03 §6.3) go out through Deps.Emit with the "isshoni:" prefix (events.go): 05's audio events and
// the share controller's share.state, share.stats and share.notice.
package bridge

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/desktop/app"
	"github.com/MoonWX/isshoni/internal/desktop/audio"
	"github.com/MoonWX/isshoni/internal/desktop/servers"
	"github.com/MoonWX/isshoni/internal/desktop/settings"
	"github.com/MoonWX/isshoni/internal/desktop/share"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/protocol"
)

// The methods' own bounds (03 §6.2, W11's fallback).
const (
	probeTimeout       = 10 * time.Second
	linkTimeout        = 15 * time.Second
	startTimeout       = 20 * time.Second
	accessTokenTimeout = 15 * time.Second
	updateCheckTimeout = 45 * time.Second
	ioTimeout          = 10 * time.Second
)

// Core is what the services call of the app; *app.App implements it.
type Core interface {
	Info() app.Info
	Heartbeat()
	OpenExternal(url string) error
	OpenOSSettings(p app.OSPage) error
	Live() (roomID string, live bool)
	Quit(stopShare bool)
	Log(level, msg string, attrs map[string]string)
	OpenLogsFolder() error
	OpenNotices() error
	PendingLink() *app.PendingLink
	LinkHandled()
	OwnConnections(ids []string) error
	Probe(ctx context.Context) (media.Probe, error)
}

// shareSource has the share controller (03 §10.1): *app.App's Shares.
type shareSource interface {
	Shares() app.ShareController
}

var _ shareSource = (*app.App)(nil)

var _ Core = (*app.App)(nil)

// ServerList is what Auth calls of 02's server list; ServersOf adapts *deviceauth.Servers.
type ServerList interface {
	List() []deviceauth.ServerEntry
	// Active returns the active server's manager, nil when no server is listed.
	Active() Manager
}

// Manager is what Auth calls of one server's 02 Manager; ServersOf adapts *deviceauth.Manager.
type Manager interface {
	// Origin is the server's origin, read without any request.
	Origin() string
	State() deviceauth.State
	Link(ctx context.Context) error
	CancelLink()
	LinkWithPassword(ctx context.Context, username string, password protocol.Secret) error
	AccessTokenWithExpiry(ctx context.Context, force bool) (protocol.Secret, time.Time, error)
	SessionEnded(ctx context.Context, code protocol.ErrorCode) deviceauth.State
	HelloRefused(e *protocol.Error)
	Recheck(ctx context.Context) (deviceauth.State, error)
	Unlink(ctx context.Context) error
}

// ServersOf adapts 02's *deviceauth.Servers to ServerList.
func ServersOf(s *deviceauth.Servers) ServerList { return serverList{s} }

type serverList struct{ s *deviceauth.Servers }

func (l serverList) List() []deviceauth.ServerEntry { return l.s.List() }

func (l serverList) Active() Manager {
	m := l.s.Active()
	if m == nil {
		return nil // no typed nil inside the interface
	}
	return manager{m}
}

// manager is *deviceauth.Manager as a Manager.
type manager struct{ *deviceauth.Manager }

func (m manager) Origin() string { return m.State().Origin }

// AudioPolicy is what Audio calls of 05's *audio.Service (05 §14.2).
type AudioPolicy interface {
	FriendsHear() audio.FriendsHear
	AppRules() []audio.AppRule
	Watch(on bool) error
	WatchLevels(on bool)
	Decide(promptID string, d audio.Decision) error
	DismissPrompt(promptID string) error
	SetAppDecision(appID string, d audio.Decision) error
	SetKeepOutMicUsers(on bool) error
	DismissTip(code string) error
	SoundCheck(ctx context.Context) (media.SoundCheckResult, error)
	Subscribe(fn func(audio.Event)) (cancel func())
}

var _ AudioPolicy = (*audio.Service)(nil)

// SettingsSource is what Settings reads of settings.json; *settings.Store implements it.
type SettingsSource interface {
	Get() settings.Settings
}

var _ SettingsSource = (*settings.Store)(nil)

// WindowShell is what Window calls of the shell; app.Shell (the Wails shell, e2ehost's fake) implements it.
type WindowShell interface {
	SetFullscreen(on bool)
}

// ProbeFunc is Auth.Probe's probe: servers.Probe with the app's options.
type ProbeFunc func(ctx context.Context, address string) (servers.ServerProbe, error)

// Deps is what the services consume. App and Servers are required; a nil Audio, Settings or Shell makes that
// service's methods not_implemented (README §4).
type Deps struct {
	App     Core
	Servers ServerList
	// Probe is Auth.Probe's probe; nil means servers.Probe with ProbeOptions{App: ProbeApp, AllowInsecureLoopback}.
	Probe ProbeFunc
	// ProbeApp describes this app to the probed server (its User-Agent); used only when Probe is nil.
	ProbeApp deviceauth.AppInfo
	// AllowInsecureLoopback is true in dev and e2e builds (http:// loopback servers, 03 §8.1).
	AllowInsecureLoopback bool
	Audio                 AudioPolicy // 05's service; nil until README WA77 wires it
	// Shares is the share controller; nil means App's own (*app.App's Shares). Without one, Share's controller
	// calls answer not_implemented.
	Shares   app.ShareController
	Settings SettingsSource // settings.json
	Shell    WindowShell    // Window.SetFullscreen
	// PolicyOff reports 06's update policy (update.PolicyDisabled) for SettingsView.updates.policyOff; nil: false.
	PolicyOff func() bool
	// Emit sends an event to the page (03 §6.3): the shell's Emit. nil drops events.
	Emit func(name string, data any)
	Log  *slog.Logger // nil means slog.Default(); the services add component=bridge
}

// Services are the bound services, one struct per 03 §6.2 service name.
type Services struct {
	App      *App
	Auth     *Auth
	Share    *Share
	Audio    *Audio
	Settings *Settings
	Window   *Window
	Updates  *Updates

	closeOnce sync.Once
	closers   []func()
}

// New builds the services over d and starts forwarding 05's audio events. Close stops that.
func New(d Deps) (*Services, error) {
	switch {
	case d.App == nil:
		return nil, errors.New("bridge: Deps.App is required")
	case d.Servers == nil:
		return nil, errors.New("bridge: Deps.Servers is required")
	}
	shares := d.Shares
	if src, ok := d.App.(shareSource); ok && shares == nil {
		shares = src.Shares()
	}
	if shares == nil {
		shares = noShares{}
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	log := d.Log.With("component", "bridge")
	emit := d.Emit
	if emit == nil {
		emit = func(string, any) {}
	}
	probe := d.Probe
	if probe == nil {
		o := servers.ProbeOptions{App: d.ProbeApp, AllowInsecureLoopback: d.AllowInsecureLoopback}
		probe = func(ctx context.Context, address string) (servers.ServerProbe, error) {
			return servers.Probe(ctx, address, o)
		}
	}
	s := &Services{
		App:      &App{core: d.App, log: log},
		Auth:     &Auth{core: d.App, servers: d.Servers, probe: probe, log: log},
		Share:    &Share{core: d.App, ctl: shares, log: log},
		Audio:    &Audio{core: d.App, policy: d.Audio, log: log},
		Settings: &Settings{store: d.Settings, policyOff: d.PolicyOff, log: log},
		Window:   &Window{shell: d.Shell},
		Updates:  &Updates{},
	}
	if d.Audio != nil {
		s.closers = append(s.closers, d.Audio.Subscribe(func(ev audio.Event) {
			emit(EventPrefix+ev.Name, ev.Payload)
		}))
	}
	s.closers = append(s.closers, s.Share.ctl.Subscribe(func(ev share.Event) { s.Share.forward(ev, emit) }))
	return s, nil
}

// Close stops forwarding events. The services' methods keep working.
func (s *Services) Close() {
	s.closeOnce.Do(func() {
		for _, c := range s.closers {
			c()
		}
	})
}

// withTimeout bounds a method's own work (W11's fallback): the earlier of ctx and d.
func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, d)
}
