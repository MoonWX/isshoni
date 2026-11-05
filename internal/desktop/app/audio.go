package app

import (
	"errors"
	"slices"
	"time"

	"github.com/MoonWX/isshoni/internal/client/core"
	"github.com/MoonWX/isshoni/internal/desktop/audio"
	"github.com/MoonWX/isshoni/internal/desktop/session"
)

// 05's audio service in the app (03 §6.1, §12.2, §15; 05 §14.2, §22; README WA77): what the app, not the SPA, tells
// the service and shows for it.
//   - EventPlaybackState → SetViewerBrowser: a web tab of the user's that plays isshoni reports it to C2 (01's
//     relay playback.state), and the service keeps that browser out of the share (05 §5.9). A sender that leaves C1's
//     user.connections (App.OwnConnections) is reported as no longer playing.
//   - The mic prompt as a toast while the window is hidden (03 §15): its Keep out and Include call 05's Decide; a
//     prompt answered in the window (or gone) settles its toast, so its buttons no longer act.
//   - SetVisible follows the window (window.go).
// Each part is found on Deps.Audio by a type assertion, like 05's share calls (share.go); deps_test.go asserts that
// *audio.Service has every one of them.

// viewerPolicy is 05's Service.SetViewerBrowser (05 §5.9).
type viewerPolicy interface {
	SetViewerBrowser(connID, family, os string, playing bool)
}

// audioEvents is 05's Service.Subscribe: audio.report, audio.levels, audio.prompt and audio.alert.
type audioEvents interface {
	Subscribe(fn func(audio.Event)) (cancel func())
}

// promptDecider is how a mic prompt's toast answers it: 05's Decide (keep-out or include, saved per app) and
// DismissPrompt (kept out for this run, nothing saved; 05 §14.6).
type promptDecider interface {
	Decide(promptID string, d audio.Decision) error
	DismissPrompt(promptID string) error
}

// focusReporter is a shell that knows whether the main window is in the foreground. A shell without it counts a
// visible window as the foreground one, so a prompt becomes a toast only while the window is hidden (05 §14.6).
type focusReporter interface {
	WindowFocused() bool
}

// The mic prompts' toast kind (Notification.Kind): Keep out (or Keep browser out) · Include (03 §15).
const ToastMicPrompt = "mic-prompt"

// playbackRetry is how long the app waits before it asks C2 again for its client, when there is none (not linked,
// no server) or the one it followed stopped; a changed link state asks at once.
const playbackRetry = 2 * time.Second

// startAudio starts the app's parts of 05's service: the prompt toasts and the viewer reports, when Deps.Audio has
// them. Run calls it; its goroutines end with the app's context.
func (a *App) startAudio() {
	if v, ok := a.d.Audio.(visibility); ok {
		v.SetVisible(a.d.Shell.WindowVisible())
	}
	if ev, ok := a.d.Audio.(audioEvents); ok {
		cancel := ev.Subscribe(a.onAudioEvent)
		a.wg.Go(func() {
			<-a.ctx.Done()
			cancel()
		})
	}
	if vp, ok := a.d.Audio.(viewerPolicy); ok && a.pub != nil {
		a.wg.Go(func() { a.followPlayback(vp) })
	}
}

// onAudioEvent is 05's event callback: a new prompt goes out as a toast while the window isn't in the foreground,
// and a report settles the toasts of prompts it no longer lists (answered in the window, dismissed, or gone).
func (a *App) onAudioEvent(ev audio.Event) {
	switch ev.Name {
	case audio.EventPrompt:
		p, ok := ev.Payload.(audio.Prompt)
		if ok && !a.windowInFront() {
			a.promptToast(p)
		}
	case audio.EventReport:
		fh, ok := ev.Payload.(audio.FriendsHear)
		if !ok {
			return
		}
		ids := make([]string, 0, len(fh.Prompts))
		for _, p := range fh.Prompts {
			ids = append(ids, p.ID)
		}
		a.toasts.dropRefsExcept(ToastMicPrompt, ids)
	}
}

// windowInFront reports a visible main window that has the focus (focusReporter), or just a visible one.
func (a *App) windowInFront() bool {
	if !a.d.Shell.WindowVisible() {
		return false
	}
	if f, ok := a.d.Shell.(focusReporter); ok {
		return f.WindowFocused()
	}
	return true
}

// promptToast shows a mic prompt as a toast (03 §15): "{{app}} is using your microphone. Keep it out of your
// stream?" with Keep out · Include, or the browser text with Keep browser out · Include. The app's name is the
// prompt's display name: shown, never logged.
func (a *App) promptToast(p audio.Prompt) {
	if _, ok := a.d.Audio.(promptDecider); !ok || p.ID == "" {
		return
	}
	t := a.d.I18n
	body := t.T("desktop.native.toast.micPrompt", "app", p.Name)
	keepOut := t.T("desktop.native.toast.keepOut")
	if p.Kind == audio.PromptBrowserMic {
		body = t.T("desktop.native.toast.browserMicPrompt")
		keepOut = t.T("desktop.native.toast.keepBrowserOut")
	}
	a.notifyRef(ToastMicPrompt, p.ID, body,
		NotificationAction{ID: ToastKeepOut, Title: keepOut},
		NotificationAction{ID: ToastInclude, Title: t.T("desktop.native.toast.include")})
}

// answerPrompt is a mic prompt toast's button: 05's Decide with keep-out or include. A prompt that was answered or
// dismissed meanwhile (05's ErrUnknownPrompt) needs nothing more.
func (a *App) answerPrompt(promptID string, d audio.Decision) bool {
	pd, ok := a.d.Audio.(promptDecider)
	if !ok {
		return false
	}
	if err := pd.Decide(promptID, d); err != nil {
		if errors.Is(err, audio.ErrUnknownPrompt) {
			a.log.Info("app: a mic prompt's toast came after the prompt was settled")
		} else {
			a.log.Warn("app: a mic prompt's answer wasn't saved", errAttr(err))
		}
		return false
	}
	return true
}

// ToastDismissed is the shell's report that the user dismissed a toast (its ✕, or swiped it away), for a shell that
// can tell: a mic prompt's toast then dismisses the prompt (05's DismissPrompt: kept out for this run, nothing saved,
// 03 §15). Any other toast is only settled. A nonce this run didn't send does nothing.
//
// It has no caller yet: the pinned Wails notifications service (v3.0.0-beta.34) reports no toast dismissal, so a
// swiped-away mic prompt stays pending (still kept out) until it is answered in the window. The slice that upgrades
// Wails wires it from the shell once the service can report a dismissal.
func (a *App) ToastDismissed(nonce string) {
	p, ok := a.toasts.take(nonce)
	if !ok || p.kind != ToastMicPrompt {
		return
	}
	if pd, ok := a.d.Audio.(promptDecider); ok {
		if err := pd.DismissPrompt(p.ref); err != nil && !errors.Is(err, audio.ErrUnknownPrompt) {
			a.log.Warn("app: a mic prompt wasn't dismissed", errAttr(err))
		}
	}
}

// followPlayback passes C2's EventPlaybackState to 05 (03 §12.2): it follows C2's core client while one is online,
// and the next one after it stops. It returns with the app's context.
func (a *App) followPlayback(vp viewerPolicy) {
	for a.ctx.Err() == nil {
		c, _, err := a.pub.Ready(a.ctx)
		if err != nil {
			if !a.waitPlayback() {
				return
			}
			continue
		}
		a.watchPlayback(c, vp)
	}
}

// waitPlayback waits playbackRetry, or less when the link state changed; false when the app ends.
func (a *App) waitPlayback() bool {
	t := time.NewTimer(playbackRetry)
	defer t.Stop()
	select {
	case <-a.ctx.Done():
		return false
	case <-t.C:
	case <-a.playbackKick:
	}
	return true
}

// watchPlayback reads one core client's events until it stops or the app ends.
func (a *App) watchPlayback(c session.Client, vp viewerPolicy) {
	events, unsubscribe := c.Subscribe()
	defer unsubscribe()
	t := time.NewTicker(playbackRetry)
	defer t.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if ev.Kind == core.EventPlaybackState && ev.Playback != nil {
				a.playbackState(*ev.Playback, vp)
			}
		case <-t.C:
			if c.Status().Conn == core.ConnStopped {
				return
			}
		}
	}
}

// playbackState records a tab's report and passes it to 05: SetViewerBrowser(From, Browser, OS, Playing).
func (a *App) playbackState(ps core.PlaybackState, vp viewerPolicy) {
	if ps.From == "" {
		return
	}
	a.mu.Lock()
	if ps.Playing {
		if a.playing == nil {
			a.playing = map[string]bool{}
		}
		a.playing[ps.From] = true
	} else {
		delete(a.playing, ps.From)
	}
	a.mu.Unlock()
	vp.SetViewerBrowser(ps.From, ps.Browser, string(ps.OS), ps.Playing)
}

// expirePlayback reports the playing senders that left C1's user.connections as no longer playing (03 §12.2): those
// in prev that aren't in next. A sender that never was in the list stays until it reports, or its connection comes and
// goes.
func (a *App) expirePlayback(prev, next []string) {
	vp, ok := a.d.Audio.(viewerPolicy)
	if !ok {
		return
	}
	var gone []string
	a.mu.Lock()
	for id := range a.playing {
		if slices.Contains(prev, id) && !slices.Contains(next, id) {
			gone = append(gone, id)
			delete(a.playing, id)
		}
	}
	a.mu.Unlock()
	slices.Sort(gone)
	for _, id := range gone {
		vp.SetViewerBrowser(id, "", "", false)
	}
}

// kickPlayback makes followPlayback ask C2 again at once (a changed link state).
func (a *App) kickPlayback() {
	select {
	case a.playbackKick <- struct{}{}:
	default:
	}
}
