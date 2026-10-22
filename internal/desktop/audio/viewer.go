package audio

import (
	"slices"
	"strings"
	"time"

	"github.com/MoonWX/isshoni/internal/media/audiorules"
)

// This file is the viewer rule (05 §5.9): a browser on this PC that plays the user's isshoni viewer is kept out of
// the share, so friends never hear themselves.

// viewerOS is the ClientOS of a tab whose browser may run on this PC.
const viewerOS = "windows"

// sessionActive is the report's state of an active session (05 §12.1).
const sessionActive = "active"

// SetViewerBrowser records what a tab of the user's own reported (05 §5.9 steps 2–4): 03 calls it with
// EventPlaybackState's From, Browser, OS and Playing, and with playing false when that connection leaves the user's
// connections. For a tab on Windows (os is M1's ClientOS) of family chrome, edge or firefox, every exe of that family
// whose session is active in the latest report is kept out (source VIEWER); with none active yet, the reports are
// re-checked for 30 s. A family stays kept out while any tab of it plays; playing false clears only that connection.
// Family safari, other or an unknown value adds no rule and shows the viewer-unknown-browser banner while the tab
// plays. The rules also end with the share (OnShareStopped) and, for one exe, with "Include" on its viewer row
// (SetAppDecision), which lasts until the app quits. What the tabs reported outlives the share: the next one
// (OnShareStarted) keeps out again the families whose tabs still play.
func (s *Service) SetViewerBrowser(connID, family, os string, playing bool) {
	defer s.deliver()
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	before := s.viewerExesLocked()
	if playing {
		s.viewers[connID] = viewerTab{family: family, os: os}
	} else {
		delete(s.viewers, connID)
	}
	for fam := range s.families {
		if !s.familyPlaysLocked(fam) {
			delete(s.families, fam)
		}
	}
	if playing {
		s.startFamilyLocked(family, os, now)
	}
	if !slices.Equal(before, s.viewerExesLocked()) {
		s.markDirtyLocked(now)
	}
	s.refreshLocked(now)
}

// startFamilyLocked starts the viewer rules of a playing tab's family (05 §5.9 step 3): for chrome, edge or firefox
// on Windows, the family's exes active in the latest report, and a 30 s re-check of the reports for more.
func (s *Service) startFamilyLocked(family, os string, now time.Time) {
	if _, known := audiorules.BrowserByFamily(family); !known || os != viewerOS {
		return
	}
	f := s.families[family]
	if f == nil {
		f = &viewerFamily{}
		s.families[family] = f
	}
	f.recheckUntil = now.Add(viewerRecheck)
	s.matchFamilyLocked(family, f)
}

// familyPlaysLocked reports whether a tab of family on Windows plays.
func (s *Service) familyPlaysLocked(family string) bool {
	for _, t := range s.viewers {
		if t.family == family && t.os == viewerOS {
			return true
		}
	}
	return false
}

// matchFamilyLocked adds to f the family's exes whose session is active in the latest report.
func (s *Service) matchFamilyLocked(family string, f *viewerFamily) {
	if !s.haveReport {
		return
	}
	b, _ := audiorules.BrowserByFamily(family)
	for _, sess := range s.report.Sessions {
		exe := strings.ToLower(sess.Exe)
		if sess.State == sessionActive && b.Contains(rulesOS, exe) && !slices.Contains(f.exes, exe) {
			f.exes = append(f.exes, exe)
		}
	}
}

// recheckViewersLocked re-checks a new report for the families still in their 30 s window.
func (s *Service) recheckViewersLocked(now time.Time) {
	before := s.viewerExesLocked()
	for fam, f := range s.families {
		if now.Before(f.recheckUntil) {
			s.matchFamilyLocked(fam, f)
		}
	}
	if !slices.Equal(before, s.viewerExesLocked()) {
		s.markDirtyLocked(now)
	}
}

// viewerExesLocked lists the exes kept out for a viewer, in 05 §5.4's order, without the per-run overrides.
func (s *Service) viewerExesLocked() []string {
	var exes []string
	for _, b := range audiorules.Browsers() {
		f := s.families[b.Family]
		if f == nil {
			continue
		}
		for _, exe := range b.For(rulesOS) {
			if slices.Contains(f.exes, exe) && !s.overrides[exe] {
				exes = append(exes, exe)
			}
		}
	}
	return exes
}

// viewerRowLocked reports whether id's row is kept out for a viewer (reason viewer): a viewer rule names its exe and
// no saved keep-out (which the row's user rule would show instead) applies.
func (s *Service) viewerRowLocked(id string) bool {
	exe, ok := exeOf(id)
	return ok && slices.Contains(s.viewerExesLocked(), exe) && s.settings.decision(id) != DecisionKeepOut
}

// unknownViewerLocked reports whether a tab on Windows plays with a family that has no rule (05 §14.7's
// viewer-unknown-browser).
func (s *Service) unknownViewerLocked() bool {
	for _, t := range s.viewers {
		if _, known := audiorules.BrowserByFamily(t.family); !known && t.os == viewerOS {
			return true
		}
	}
	return false
}

// dropViewersLocked ends every viewer rule (the share ended, 05 §5.9 step 4). The per-run overrides stay, and so do
// the playing tabs: a tab reports again only when its playback starts or stops (03 §12.2), so restartViewersLocked
// needs them for the next share.
func (s *Service) dropViewersLocked(now time.Time) {
	if len(s.viewerExesLocked()) > 0 {
		s.markDirtyLocked(now)
	}
	clear(s.families)
}

// restartViewersLocked starts the viewer rules of a new share for the tabs that still play, as if each had just
// reported (a fresh 30 s re-check).
func (s *Service) restartViewersLocked(now time.Time) {
	before := s.viewerExesLocked()
	for _, t := range s.viewers {
		s.startFamilyLocked(t.family, t.os, now)
	}
	if !slices.Equal(before, s.viewerExesLocked()) {
		s.markDirtyLocked(now)
	}
}
