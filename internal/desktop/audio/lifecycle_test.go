package audio

import (
	"reflect"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/fake"
)

// soundOffCause returns FriendsHear's sound-off banner cause, or "" without the banner.
func soundOffCause(fh FriendsHear) string {
	b, ok := banner(fh, BannerSoundOff)
	if !ok {
		return ""
	}
	c, _ := b.Params["cause"].(string)
	return c
}

// TestLifecycle is 05 §19.2's integration test: the service against the fake engine through a whole share, watch →
// pick (SetSharedPID) → start (OnShareStarted) → prompt → decide → sound off → pause → resume (stays silent) → sound
// on → stop (OnShareStopped). It checks the fake's call log (audio_mode off at sound off, no audio call from the
// service at pause or resume, audio_mode exclude-apps at sound on) and the sound-off banner's cause at each step.
// TestMain's goleak checks that nothing outlives it.
func TestLifecycle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		eng := newFake("normal")
		defer func() { _ = eng.Close() }()
		st := newStore(DefaultSettings())
		s := New(eng, st, testSelf, nil)
		rec := record(s)
		stop := runService(t, s)
		defer stop()

		// Watch: the share sheet opens.
		if err := s.Watch(true); err != nil {
			t.Fatal(err)
		}
		settle(time.Second)
		fh := s.FriendsHear()
		if fh.Live || fh.Engine != "include-set" || len(fh.Prompts) != 1 || soundOffCause(fh) != "" {
			t.Fatalf("watching: %+v", fh)
		}
		if r, g := rowByID(fh, "discord"); g != "keptOut" || r.Reason != "builtin" || r.Control != ControlLocked {
			t.Errorf("Discord %s %+v", g, r)
		}
		if r, g := rowByID(fh, "exe:spotify.exe"); g != "heard" || !r.Playing {
			t.Errorf("Spotify %s %+v", g, r)
		}
		if !slices.ContainsFunc(fh.VoiceApps, func(v VoiceAppInfo) bool { return v.ID == "discord" && v.Running }) {
			t.Errorf("voice apps %+v", fh.VoiceApps)
		}

		// Pick: the shared window is Spotify's.
		s.SetSharedPID(4120)
		settle(time.Second)
		if fh = s.FriendsHear(); fh.Heard[0].ID != "exe:spotify.exe" || !fh.Heard[0].Shared {
			t.Errorf("after the pick: %+v", fh.Heard)
		}

		// Start: OnShareStarted right before the core starts the session with AudioConfig.
		s.OnShareStarted(4120, true)
		sess := startShare(t, eng, s)
		defer func() { _ = sess.Stop() }()
		settle(time.Second)
		fh = s.FriendsHear()
		if !fh.Live || fh.Mode != ModeExcludeApps || soundOffCause(fh) != "" || fh.Paused || fh.Muted {
			t.Fatalf("live: %+v", fh)
		}
		if r, _ := rowByID(fh, "exe:spotify.exe"); !r.Shared {
			t.Errorf("Spotify not shared: %+v", r)
		}

		// Prompt → decide.
		if len(fh.Prompts) != 1 || fh.Prompts[0].AppID != "exe:steam.exe" {
			t.Fatalf("prompts %+v", fh.Prompts)
		}
		if err := s.Decide(fh.Prompts[0].ID, DecisionKeepOut); err != nil {
			t.Fatal(err)
		}
		settle(time.Second)
		if r, _ := rowByID(s.FriendsHear(), "exe:steam.exe"); r.Reason != "user" {
			t.Errorf("Steam after the decision: %+v", r)
		}
		if saved, _ := st.get(); saved.AppRules["exe:steam.exe"] != DecisionKeepOut {
			t.Errorf("saved %+v", saved.AppRules)
		}
		if calls := audioCalls(sess); len(calls) != 0 {
			t.Fatalf("audio calls before sound off: %+v", calls)
		}

		// Sound off: audio_mode off, the banner's cause off.
		if err := s.SetShareSound(false); err != nil {
			t.Fatal(err)
		}
		settle(time.Second)
		fh = s.FriendsHear()
		if want := []fake.Call{{Op: "audio_mode", Mode: media.AudioOff}}; !reflect.DeepEqual(audioCalls(sess), want) {
			t.Errorf("audio calls at sound off %+v", audioCalls(sess))
		}
		if fh.Mode != ModeOff || soundOffCause(fh) != "off" || fh.Engine != "none" {
			t.Errorf("sound off: mode %q, cause %q, engine %q", fh.Mode, soundOffCause(fh), fh.Engine)
		}

		// Pause and resume: no audio call from the service; the share stays silent (cause off throughout).
		s.SetPaused(true)
		if fh = s.FriendsHear(); !fh.Paused || soundOffCause(fh) != "off" {
			t.Errorf("paused: paused %v, cause %q", fh.Paused, soundOffCause(fh))
		}
		settle(time.Second)
		s.SetPaused(false)
		settle(time.Second)
		fh = s.FriendsHear()
		if len(audioCalls(sess)) != 1 || fh.Paused || fh.Mode != ModeOff || soundOffCause(fh) != "off" {
			t.Errorf("resumed: calls %+v, %+v", audioCalls(sess), fh)
		}

		// Sound on: audio_mode exclude-apps, the banner gone.
		if err := s.SetShareSound(true); err != nil {
			t.Fatal(err)
		}
		settle(time.Second)
		fh = s.FriendsHear()
		if want := []fake.Call{{Op: "audio_mode", Mode: media.AudioOff}, {Op: "audio_mode",
			Mode: media.AudioExcludeApps}}; !reflect.DeepEqual(audioCalls(sess), want) {
			t.Errorf("audio calls at sound on %+v", audioCalls(sess))
		}
		if fh.Mode != ModeExcludeApps || soundOffCause(fh) != "" || fh.Engine != "include-set" {
			t.Errorf("sound on: %+v", fh)
		}

		// A pause with sound on: the cause is paused (SetPaused at once, then the core's mute in the report).
		s.SetPaused(true)
		if c := soundOffCause(s.FriendsHear()); c != "paused" {
			t.Errorf("paused with sound on: cause %q", c)
		}
		_ = eng.SetAudioMuted(t.Context(), true) // the core's pause mute (01 D14)
		settle(time.Second)
		if fh = s.FriendsHear(); !fh.Muted || soundOffCause(fh) != "paused" {
			t.Errorf("muted: %+v", fh)
		}
		_ = eng.SetAudioMuted(t.Context(), false)
		s.SetPaused(false)
		settle(time.Second)
		if c := soundOffCause(s.FriendsHear()); c != "" {
			t.Errorf("resumed with sound on: cause %q", c)
		}
		if n := len(audioCalls(sess)); n != 4 { // the two mode calls and the core's two mute calls
			t.Errorf("%d audio calls", n)
		}

		// Stop: the share ends; the sheet still watches, so the watcher stays until Watch(false).
		_ = sess.Stop()
		s.OnShareStopped()
		settle(time.Second)
		fh = s.FriendsHear()
		if fh.Live || fh.Mode != ModeExcludeApps || soundOffCause(fh) != "" || s.Exclusions().SharedPID != 0 {
			t.Errorf("after the share: %+v", fh)
		}
		if _, err := eng.AudioLevels(t.Context()); err != nil {
			t.Errorf("the watcher stopped with the share although the sheet watches: %v", err)
		}
		if err := s.Watch(false); err != nil {
			t.Fatal(err)
		}
		if _, err := eng.AudioLevels(t.Context()); err == nil {
			t.Error("the watcher runs after Watch(false)")
		}
		if n := len(named(rec.take(), EventReport)); n == 0 {
			t.Error("no audio.report")
		}
		stop()
	})
}

// TestWatchAndShare: OnShareStarted starts the watcher when the sheet hasn't, Watch(false) during a live share keeps
// it, and OnShareStopped then stops it.
func TestWatchAndShare(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctl := newStub()
		s := New(ctl, newStore(DefaultSettings()), testSelf, nil)
		stop := runService(t, s)
		defer stop()
		ops := func() []string {
			var out []string
			for _, c := range ctl.ops("watch", "unwatch") {
				out = append(out, c.op)
			}
			return out
		}
		s.OnShareStarted(7, true)
		if got := ops(); !slices.Equal(got, []string{"watch"}) || ctl.lastEx(t).SharedPID != 7 {
			t.Fatalf("OnShareStarted: %v", got)
		}
		_ = s.Watch(true)
		_ = s.Watch(false)
		if got := ops(); !slices.Equal(got, []string{"watch"}) {
			t.Errorf("Watch(false) during the share: %v", got)
		}
		s.OnShareStopped()
		if got := ops(); !slices.Equal(got, []string{"watch", "unwatch"}) {
			t.Errorf("OnShareStopped: %v", got)
		}
		// Close stops a running watcher.
		_ = s.Watch(true)
		stop()
		if got := ops(); !slices.Equal(got, []string{"watch", "unwatch", "watch", "unwatch"}) {
			t.Errorf("Close: %v", got)
		}
	})
}

// TestShareSound: SetShareSound outside a share does nothing; during a share without sound it records the value
// with no call (mode stays off); during a share with sound it sets the mode, and a switch made before the session's
// audio runs is applied with the first live report. SetPaused is idempotent, makes no engine call, and does nothing
// outside a share.
func TestShareSound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctl := newStub()
		s := New(ctl, newStore(DefaultSettings()), testSelf, nil)
		stop := runService(t, s)
		defer stop()
		modes := func() []media.AudioMode {
			var out []media.AudioMode
			for _, c := range ctl.ops("mode") {
				out = append(out, c.mode)
			}
			return out
		}
		if err := s.SetShareSound(false); err != nil || len(modes()) != 0 {
			t.Fatalf("outside a share: %v, %v", err, modes())
		}
		s.SetPaused(true)
		if fh := s.FriendsHear(); fh.Paused {
			t.Error("paused outside a share")
		}

		// A share without sound.
		s.OnShareStarted(0, false)
		if err := s.SetShareSound(true); err != nil || len(modes()) != 0 || s.FriendsHear().Mode != ModeOff {
			t.Errorf("without sound: %v, %v, %q", err, modes(), s.FriendsHear().Mode)
		}
		s.OnShareStopped()

		// A share with sound whose session isn't running yet.
		s.OnShareStarted(0, true)
		ctl.fail("mode", media.ErrAudioNotRunning)
		if err := s.SetShareSound(false); err != nil {
			t.Fatal(err)
		}
		if fh := s.FriendsHear(); fh.Mode != ModeOff || soundOffCause(fh) != "off" {
			t.Errorf("pending: %+v", fh)
		}
		ctl.fail("mode", nil)
		settle(time.Second)
		if got := modes(); len(got) != 1 {
			t.Errorf("applied before the session ran: %v", got)
		}
		ctl.set(func(r *media.AudioReport) { r.Live = true })
		settle(time.Second)
		if got := modes(); !slices.Equal(got, []media.AudioMode{media.AudioOff, media.AudioOff}) {
			t.Errorf("modes %v", got)
		}
		settle(time.Second)
		if got := modes(); len(got) != 2 {
			t.Errorf("applied again: %v", got)
		}
		// Idempotent: the same value makes no call; the other one does.
		_ = s.SetShareSound(false)
		_ = s.SetShareSound(true)
		if got := modes(); !slices.Equal(got, []media.AudioMode{media.AudioOff, media.AudioOff, media.AudioExcludeApps}) {
			t.Errorf("modes %v", got)
		}
		// SetPaused: no engine call, idempotent.
		before := len(ctl.ops())
		s.SetPaused(true)
		s.SetPaused(true)
		s.SetPaused(false)
		if n := len(ctl.ops()) - before; n != 0 {
			t.Errorf("SetPaused made %d engine calls", n)
		}
		s.OnShareStopped()
		stop()
	})
}
