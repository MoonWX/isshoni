package audio

import (
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
)

// viewerRules returns the exes of ex's viewer rules.
func viewerRules(ex media.Exclusions) []string {
	var exes []string
	for _, r := range ex.Rules {
		if r.Source == media.SourceViewer {
			exes = append(exes, r.Value)
		}
	}
	return exes
}

// TestViewerBrowser is 05 §5.9 steps 3–4 with active and inactive sessions, two tabs of one family, the 30 s
// re-check, families without a rule, tabs on other OSes, the end of the share, and the next share with tabs that
// still play.
func TestViewerBrowser(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctl := newStub()
		ctl.set(func(r *media.AudioReport) {
			r.Sessions = []media.AudioSession{session("chrome.exe", 100, "active"), session("brave.exe", 104,
				"inactive")}
		})
		s := New(ctl, newStore(DefaultSettings()), testSelf, nil)
		stop := runService(t, s)
		defer stop()
		_ = s.Watch(true)
		s.OnShareStarted(0, true)
		settle(time.Second)

		// An active session: kept out at once (brave.exe's session is inactive).
		s.SetViewerBrowser("c1", "chrome", "windows", true)
		if got := viewerRules(s.Exclusions()); !slices.Equal(got, []string{"chrome.exe"}) {
			t.Fatalf("viewer rules %v", got)
		}
		if got := viewerRules(ctl.lastEx(t)); !slices.Equal(got, []string{"chrome.exe"}) {
			t.Errorf("the engine's viewer rules %v", got)
		}

		// No active session yet: re-checked for 30 s. Firefox plays at 10 s; Edge only at 31 s.
		s.SetViewerBrowser("c2", "firefox", "windows", true)
		s.SetViewerBrowser("c3", "edge", "windows", true)
		if got := viewerRules(s.Exclusions()); !slices.Equal(got, []string{"chrome.exe"}) {
			t.Fatalf("viewer rules %v", got)
		}
		settle(10 * time.Second)
		ctl.set(func(r *media.AudioReport) { r.Sessions = append(r.Sessions, session("firefox.exe", 108, "active")) })
		settle(time.Second)
		if got := viewerRules(s.Exclusions()); !slices.Equal(got, []string{"chrome.exe", "firefox.exe"}) {
			t.Fatalf("viewer rules at 11 s %v", got)
		}
		settle(20 * time.Second)
		ctl.set(func(r *media.AudioReport) { r.Sessions = append(r.Sessions, session("msedge.exe", 112, "active")) })
		settle(time.Second)
		if got := viewerRules(s.Exclusions()); !slices.Equal(got, []string{"chrome.exe", "firefox.exe"}) {
			t.Errorf("viewer rules at 32 s %v (Edge came too late)", got)
		}

		// Two tabs of one family: one stops, the family stays; both stop, its rules go.
		s.SetViewerBrowser("c4", "chrome", "windows", true)
		s.SetViewerBrowser("c1", "chrome", "windows", false)
		if got := viewerRules(s.Exclusions()); !slices.Contains(got, "chrome.exe") {
			t.Errorf("one chrome tab stopped: %v", got)
		}
		s.SetViewerBrowser("c4", "", "", false)
		if got := viewerRules(s.Exclusions()); !slices.Equal(got, []string{"firefox.exe"}) {
			t.Errorf("both chrome tabs stopped: %v", got)
		}

		// No rule for safari, other or an unknown family: a banner while such a tab on Windows plays.
		for _, fam := range []string{"safari", "other", "netscape"} {
			s.SetViewerBrowser("c5", fam, "windows", true)
			if _, ok := banner(s.FriendsHear(), BannerViewerUnknownBrowser); !ok {
				t.Errorf("%s: no banner", fam)
			}
			if got := viewerRules(s.Exclusions()); !slices.Equal(got, []string{"firefox.exe"}) {
				t.Errorf("%s: viewer rules %v", fam, got)
			}
		}
		s.SetViewerBrowser("c5", "netscape", "windows", false)
		if _, ok := banner(s.FriendsHear(), BannerViewerUnknownBrowser); ok {
			t.Error("the banner stays after the tab stopped")
		}
		// A tab on another OS is on another PC: no rule, no banner.
		s.SetViewerBrowser("c6", "chrome", "macos", true)
		s.SetViewerBrowser("c7", "safari", "macos", true)
		if _, ok := banner(s.FriendsHear(), BannerViewerUnknownBrowser); ok ||
			!slices.Equal(viewerRules(s.Exclusions()), []string{"firefox.exe"}) {
			t.Errorf("a Mac's tabs: %v, %+v", viewerRules(s.Exclusions()), s.FriendsHear().Banners)
		}

		// The share ends: every viewer rule goes.
		s.OnShareStopped()
		if got := viewerRules(s.Exclusions()); len(got) != 0 {
			t.Errorf("after the share: %v", got)
		}
		settle(time.Second)
		if got := viewerRules(ctl.lastEx(t)); len(got) != 0 {
			t.Errorf("the engine's viewer rules after the share: %v", got)
		}

		// The next share: the tabs that still play (c2 Firefox, c3 Edge, whose session is active by now) report
		// nothing new (03 §12.2), and their browsers are kept out again, matched in the latest report.
		s.OnShareStarted(0, true)
		if got := viewerRules(s.Exclusions()); !slices.Equal(got, []string{"msedge.exe", "firefox.exe"}) {
			t.Errorf("the next share's viewer rules %v", got)
		}
		settle(time.Second)
		if got := viewerRules(ctl.lastEx(t)); !slices.Equal(got, []string{"msedge.exe", "firefox.exe"}) {
			t.Errorf("the engine's viewer rules in the next share %v", got)
		}
		// … with a fresh 30 s re-check (c2's own ended long ago): Brave's session (Chrome's family, whose tabs all
		// stopped) adds nothing, Firefox's fork LibreWolf does.
		settle(5 * time.Second)
		ctl.set(func(r *media.AudioReport) {
			r.Sessions = append(r.Sessions, session("librewolf.exe", 116, "active"), session("brave.exe", 120,
				"active"))
		})
		settle(time.Second)
		if got := viewerRules(s.Exclusions()); !slices.Equal(got, []string{"msedge.exe", "firefox.exe",
			"librewolf.exe"}) {
			t.Errorf("viewer rules after the re-check %v", got)
		}
		s.OnShareStopped()
		stop()
	})
}

// TestViewerRecheckFromWatch: a tab that plays while nothing is watched is checked for 30 s from the moment the
// watcher starts, when the reports begin.
func TestViewerRecheckFromWatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctl := newStub()
		s := New(ctl, newStore(DefaultSettings()), testSelf, nil)
		stop := runService(t, s)
		defer stop()
		s.SetViewerBrowser("c1", "edge", "windows", true)
		settle(time.Minute)
		ctl.set(func(r *media.AudioReport) { r.Sessions = []media.AudioSession{session("msedge.exe", 100, "active")} })
		_ = s.Watch(true)
		settle(time.Second)
		if got := viewerRules(s.Exclusions()); !slices.Equal(got, []string{"msedge.exe"}) {
			t.Errorf("viewer rules %v", got)
		}
		stop()
	})
}

// TestViewerWithSavedInclude: a browser with a saved include (row 5 allow) still gets its viewer rule (row 4), so
// both are in the rule set (05 §5.3, audiotest case 24).
func TestViewerWithSavedInclude(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctl := newStub()
		ctl.set(func(r *media.AudioReport) { r.Sessions = []media.AudioSession{session("msedge.exe", 100, "active")} })
		st := DefaultSettings().withDecision("exe:msedge.exe", DecisionInclude)
		s := New(ctl, newStore(st), testSelf, nil)
		stop := runService(t, s)
		defer stop()
		_ = s.Watch(true)
		settle(time.Second)
		s.SetViewerBrowser("c1", "edge", "windows", true)
		ex := s.Exclusions()
		if !hasRule(ex, media.RuleAppAllow, media.SourceUser, "msedge.exe") ||
			!hasRule(ex, media.RuleAppExclude, media.SourceViewer, "msedge.exe") {
			t.Errorf("rules %v", ex.Rules)
		}
		stop()
	})
}

// TestViewerInclude: "Include" on a viewer row (with the fake's Chrome, a mic user from 5 s) is the per-run override:
// its viewer rules go, appRules is unchanged, mic keep-out still applies (and only now asks about the mic), and a new
// Service has no override. While the viewer rule holds, the browser gets no mic prompt.
func TestViewerInclude(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		eng := newFake("normal")
		defer func() { _ = eng.Close() }()
		st := newStore(DefaultSettings())
		s := New(eng, st, testSelf, nil)
		stop := runService(t, s)
		defer stop()
		_ = s.Watch(true)
		s.SetViewerBrowser("c1", "chrome", "windows", true) // before Chrome's session exists
		settle(6 * time.Second)
		fh := s.FriendsHear()
		if r, g := rowByID(fh, "exe:chrome.exe"); g != "keptOut" || r.Reason != reasonViewer || r.Control != ControlToggle {
			t.Fatalf("Chrome %s %+v", g, r)
		}
		if slices.ContainsFunc(fh.Prompts, func(p Prompt) bool { return p.AppID == "exe:chrome.exe" }) {
			t.Errorf("a mic prompt for the viewer's browser: %+v", fh.Prompts)
		}

		if err := s.SetAppDecision("exe:chrome.exe", DecisionInclude); err != nil {
			t.Fatal(err)
		}
		if got := viewerRules(s.Exclusions()); len(got) != 0 {
			t.Errorf("viewer rules after the override: %v", got)
		}
		settle(time.Second)
		fh = s.FriendsHear()
		if r, g := rowByID(fh, "exe:chrome.exe"); g != "keptOut" || r.Reason != reasonMic || r.Decision != DecisionAuto {
			t.Errorf("Chrome after the override %s %+v", g, r)
		}
		if !slices.ContainsFunc(fh.Prompts, func(p Prompt) bool { return p.AppID == "exe:chrome.exe" }) {
			t.Errorf("no mic prompt once the viewer rule went: %+v", fh.Prompts)
		}
		if saved, n := st.get(); len(saved.AppRules) != 0 || n != 0 {
			t.Errorf("the override saved %+v (%d saves)", saved.AppRules, n)
		}
		// The override outlives the share and its viewer rules, until the app quits.
		s.OnShareStarted(0, false)
		s.OnShareStopped()
		s.SetViewerBrowser("c1", "chrome", "windows", true)
		if got := viewerRules(s.Exclusions()); len(got) != 0 {
			t.Errorf("viewer rules with the override: %v", got)
		}
		stop()

		// A new run has no override.
		s2 := New(eng, st, testSelf, nil)
		stop2 := runService(t, s2)
		defer stop2()
		_ = s2.Watch(true)
		settle(6 * time.Second) // the fake's script restarts with the watcher: Chrome plays from 5 s
		s2.SetViewerBrowser("c1", "chrome", "windows", true)
		if got := viewerRules(s2.Exclusions()); !slices.Equal(got, []string{"chrome.exe"}) {
			t.Errorf("a new run's viewer rules %v", got)
		}
		stop2()
	})
}
