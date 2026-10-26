package app

import (
	"context"

	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/desktop/session"
	"github.com/MoonWX/isshoni/internal/desktop/share"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/version"
)

// C2 and the share controller (03 §9, §10.1; README WA58): New builds internal/desktop/session for Deps.Engine when
// Deps.Session is nil, and the share controller over the session, the engine's picker, 05's audio policy when
// Deps.Audio has it, the main window and settings.json.

// ShareController is the share controller as the bound Share service (internal/desktop/bridge) calls it;
// *share.Controller implements it.
type ShareController interface {
	Pick(ctx context.Context) (*share.Picked, error)
	Release(pickID string)
	Start(ctx context.Context, r share.StartRequest) (share.View, error)
	Current() *share.View
	Stop(ctx context.Context, cause share.Cause)
	StopOther(ctx context.Context, roomID, shareID string) error
	SetPaused(ctx context.Context, paused bool) error
	SetPreset(ctx context.Context, p protocol.Preset) error
	SetAudioEnabled(on bool) error
	Stats() *share.Stats
	WatchServerStats(on bool)
	Subscribe(fn func(share.Event)) (cancel func())
}

var _ ShareController = (*share.Controller)(nil)

// Shares returns the share controller.
func (a *App) Shares() ShareController { return a.shares }

// publisher is what the app uses of C2 beyond Run: *session.Session. A Deps.Session without it gets no C2 for the
// share controller (Start answers publisher_offline).
type publisher interface {
	Ready(ctx context.Context) (session.Client, string, error)
	ConnectionID() string
	OnAuthState(s deviceauth.State)
	Refresh()
}

var (
	_ Session   = (*session.Session)(nil)
	_ publisher = (*session.Session)(nil)
)

// audioConfigSource is what C2 takes of 05's audio service (*audio.Service): the audio start config of a share
// with sound, its exclusions included (05 §14.2). Like share.AudioPolicy it is found on Deps.Audio by a type
// assertion, so deps_test.go asserts that *audio.Service has both: a signature that drifts would otherwise drop
// 05's calls without a failing build.
type audioConfigSource interface {
	AudioConfig() media.AudioConfig
}

// newSession is C2 for d's engine and servers (03 §6.1's wiring): hello client {desktop, the app's version, the
// core's OS}, and 05's AudioConfig when Deps.Audio has it.
func newSession(d Deps) (*session.Session, error) {
	o := session.Options{
		Servers: session.ServersOf(d.Servers),
		Engine:  d.Engine,
		Client:  protocol.ClientInfo{Kind: protocol.ClientKindDesktop, Version: version.Version()},
		Log:     d.Log,
	}
	if p, ok := d.Audio.(audioConfigSource); ok {
		o.AudioConfig = p.AudioConfig
	}
	return session.New(o)
}

// newShares is the share controller over the app's parts and C2.
func newShares(d Deps, pub share.Publisher) *share.Controller {
	o := share.Options{Publisher: pub, Log: d.Log}
	if d.Engine != nil {
		o.Engine = d.Engine
	}
	if p, ok := d.Audio.(share.AudioPolicy); ok {
		o.Audio = p
	}
	if d.Shell != nil {
		o.Window = d.Shell
	}
	if d.Settings != nil {
		o.Settings = d.Settings
	}
	return share.New(o)
}

// sharePublisher is C2 as the share controller waits for it.
type sharePublisher struct{ p publisher }

func (s sharePublisher) Ready(ctx context.Context) (share.Client, string, error) {
	if s.p == nil {
		return nil, "", share.ErrPublisherOffline
	}
	c, origin, err := s.p.Ready(ctx)
	if err != nil {
		return nil, "", err
	}
	return share.FromCore(c), origin, nil
}
