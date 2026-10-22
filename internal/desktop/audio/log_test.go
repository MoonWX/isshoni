package audio

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
)

// syncBuffer is a bytes.Buffer safe for the logger's concurrent writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestLogsNoNames: with a report full of names, paths and pids, and errors whose texts carry them, nothing of it
// reaches the log at debug level (05 §16): only engines, states, probe results, error codes and counts.
func TestLogsNoNames(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var buf syncBuffer
		log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		ctl := newStub()
		secret := app("secretvoice.exe", "SecretVoiceName", verdictKeptOut, reasonMic)
		secret.UsesMic, secret.Playing, secret.Warnings = true, true, []string{"included_uses_mic"}
		tree := app("secrettree.exe", "SecretTreeName", verdictNotCaptured, "tree")
		tree.Detail = "SecretDetailName"
		failed := app("secretfail.exe", "SecretFailName", verdictNotCaptured, "capture_failed")
		failed.Playing = true
		ctl.set(func(r *media.AudioReport) {
			r.Apps = []media.AudioApp{secret, tree, failed, app("chrome.exe", "SecretBrowserName", verdictHeard, "")}
			r.Sessions = []media.AudioSession{session("chrome.exe", 987654, "active"),
				session("secretvoice.exe", 987650, "active")}
			r.Warnings = []media.AudioWarning{{Code: "multi_process_session", App: "exe:secretvoice.exe"}}
			r.Events = []media.AudioEvent{{Code: "session_created", App: "exe:secretvoice.exe"}}
		})
		self := SelfInfo{PID: 876543, WebViewDataDir: `C:\SecretFolder\WebView2`, Exe: `C:\SecretFolder\isshoni.exe`}
		st := newStore(DefaultSettings())
		s := New(ctl, st, self, log)
		stop := runService(t, s)
		defer stop()

		_ = s.Watch(true)
		s.WatchLevels(true)
		settle(time.Second)
		s.SetSharedPID(765432)
		s.SetViewerBrowser("conn-secret", "chrome", "windows", true)
		s.SetViewerBrowser("conn-secret2", "safari", "windows", true)
		s.OnShareStarted(765432, true)
		settle(time.Second)
		if err := s.Decide("p1", DecisionKeepOut); err != nil {
			t.Fatal(err)
		}
		_ = s.SetAppDecision("exe:secretother.exe", DecisionInclude)
		_ = s.SetShareSound(false)
		s.SetPaused(true)
		ctl.set(func(r *media.AudioReport) { r.Probe.Result, r.Engine = "failed", "endpoint" })
		settle(time.Second)

		// Errors whose texts name things: the store's (a path), the engine's (a detail).
		st.mu.Lock()
		st.fail = errors.New(`open C:\SecretFolder\settings.json: access denied`)
		st.mu.Unlock()
		_ = s.SetAppDecision("exe:secretthird.exe", DecisionKeepOut)
		ctl.fail("exclusions", &media.Error{Code: media.CodeInvalid, Op: "set_exclusions",
			Detail: "rule 7: secretvoice.exe"})
		ctl.fail("levels", &media.Error{Code: media.CodeInternal, Op: "audio_levels_get",
			Detail: "secretvoice.exe 987650"})
		s.SetSharedPID(765431)
		settle(time.Second)
		_, _ = s.SoundCheck(t.Context()) // a helper path that doesn't exist here
		s.OnShareStopped()
		_ = s.Watch(false)
		stop()

		out := buf.String()
		if testing.Verbose() {
			t.Log(out)
		}
		for _, want := range []string{"component=audio", "audio watcher started", "audio report", "audio prompt",
			"audio alert", "audio rules not applied", "audio settings not saved", "audio poll failed",
			"set_exclusions: invalid"} {
			if !strings.Contains(out, want) {
				t.Errorf("the log lacks %q", want)
			}
		}
		for _, secret := range []string{"Secret", "secret", "987654", "987650", "876543", "765432", "765431",
			"chrome", "exe:", ".exe", "conn-", "WebView2", `C:\`} {
			if strings.Contains(out, secret) {
				t.Errorf("the log names %q:\n%s", secret, out)
			}
		}
	})
}
