package audio

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
)

// TestSettingsJSON: 05 §14.1's object. Absent keys and null give the defaults, a wrong type keeps a key's default,
// appRules keeps its order, and unknown keys are written back unchanged.
func TestSettingsJSON(t *testing.T) {
	for _, in := range []string{`{}`, `null`, `{"keepOutMicUsers": "yes", "fastPath": 1, "appRules": [],
		"dismissedTips": {}}`} {
		var s Settings
		if err := json.Unmarshal([]byte(in), &s); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if !reflect.DeepEqual(s, DefaultSettings()) {
			t.Errorf("%s: %+v, want the defaults", in, s)
		}
	}
	var bad Settings
	if err := json.Unmarshal([]byte(`[1]`), &bad); err == nil {
		t.Error("an array is settings")
	}

	in := `{"zeta": {"a": [1, 2]}, "keepOutMicUsers": false, "appRules": {"exe:z.exe": "include",
		"exe:kook.exe": "keep-out", "exe:n.exe": 3, "exe:a.exe": "keep-out"}, "fastPath": false,
		"dismissedTips": ["ducking"], "alpha": true}`
	var s Settings
	if err := json.Unmarshal([]byte(in), &s); err != nil {
		t.Fatal(err)
	}
	if s.KeepOutMicUsers || s.FastPath || len(s.AppRules) != 3 || s.AppRules["exe:kook.exe"] != DecisionKeepOut ||
		!slices.Equal(s.DismissedTips, []string{"ducking"}) {
		t.Errorf("read %+v", s)
	}
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"keepOutMicUsers":false,"appRules":{"exe:z.exe":"include","exe:kook.exe":"keep-out",` +
		`"exe:a.exe":"keep-out"},"fastPath":false,"dismissedTips":["ducking"],"alpha":true,"zeta":{"a":[1,2]}}`
	if string(out) != want {
		t.Errorf("wrote\n %s\nwant\n %s", out, want)
	}
	// A pointer marshals the same; the zero value writes empty lists, not null.
	if p, _ := json.Marshal(&s); string(p) != want {
		t.Errorf("pointer: %s", p)
	}
	if z, _ := json.Marshal(Settings{}); string(z) !=
		`{"keepOutMicUsers":false,"appRules":{},"fastPath":false,"dismissedTips":[]}` {
		t.Errorf("zero value: %s", z)
	}
	if d, _ := json.Marshal(DefaultSettings()); string(d) !=
		`{"keepOutMicUsers":true,"appRules":{},"fastPath":true,"dismissedTips":[]}` {
		t.Errorf("defaults: %s", d)
	}
	// Decisions set without withDecision (a hand-made map) count as older, sorted.
	h := Settings{AppRules: map[string]Decision{"exe:b.exe": DecisionInclude, "exe:a.exe": DecisionKeepOut}}
	h = h.withDecision("exe:c.exe", DecisionKeepOut)
	if got := h.ruleIDs(); !slices.Equal(got, []string{"exe:a.exe", "exe:b.exe", "exe:c.exe"}) {
		t.Errorf("order %v", got)
	}
}

// TestSettingsNormalized: the service keeps only valid, unlocked decisions with a known value (the newest 256) and
// known tips; it never changes the store's copy.
func TestSettingsNormalized(t *testing.T) {
	s := DefaultSettings()
	for _, id := range []string{"exe:kook.exe", "discord", "exe:discord.exe", "isshoni", "exe:Bad.exe", "spotify"} {
		s = s.withDecision(id, DecisionKeepOut)
	}
	s.AppRules["exe:odd.exe"] = "maybe"
	s.AppRules["exe:auto.exe"] = DecisionAuto
	s.DismissedTips = []string{"ducking", "volume", "ducking"}
	n := s.normalized()
	if !reflect.DeepEqual(n.AppRules, map[string]Decision{"exe:kook.exe": DecisionKeepOut}) ||
		!slices.Equal(n.DismissedTips, []string{"ducking"}) {
		t.Errorf("normalized %+v", n)
	}
	if len(s.AppRules) != 8 || len(s.DismissedTips) != 3 {
		t.Errorf("normalized changed its source: %+v", s)
	}
	c := n.clone()
	c.AppRules["exe:x.exe"] = DecisionInclude
	c.DismissedTips[0] = "x"
	if len(n.AppRules) != 1 || n.DismissedTips[0] != "ducking" {
		t.Error("clone shares its maps or slices")
	}
}

// TestSettingsPersisted: the service reads the store once, normalized, and saves every change; the saved object
// keeps the unknown keys it was read with.
func TestSettingsPersisted(t *testing.T) {
	var start Settings
	if err := json.Unmarshal([]byte(`{"appRules": {"discord": "include", "exe:kook.exe": "keep-out"},
		"future": 1}`), &start); err != nil {
		t.Fatal(err)
	}
	st := newStore(start)
	s := New(newStub(), st, testSelf, nil)
	defer func() { _ = s.Close() }()
	if err := s.SetAppDecision("exe:spotify.exe", DecisionInclude); err != nil {
		t.Fatal(err)
	}
	saved, n := st.get()
	b, _ := json.Marshal(saved)
	if want := `{"keepOutMicUsers":true,"appRules":{"exe:kook.exe":"keep-out","exe:spotify.exe":"include"},` +
		`"fastPath":true,"dismissedTips":[],"future":1}`; string(b) != want || n != 1 {
		t.Errorf("saved %s (%d saves), want %s", b, n, want)
	}
	// The same decision again saves nothing.
	_ = s.SetAppDecision("exe:spotify.exe", DecisionInclude)
	_ = s.SetKeepOutMicUsers(true)
	if _, n := st.get(); n != 1 {
		t.Errorf("%d saves for no change", n)
	}
}

// noSectionStore is a store that reads a settings.json without an "audio" section as the zero Settings (what
// json.Unmarshal into a Settings never gives), as `var s Settings; if raw != nil { json.Unmarshal(raw, &s) }` does.
type noSectionStore struct{}

func (noSectionStore) AudioSettings() Settings          { return Settings{} }
func (noSectionStore) SaveAudioSettings(Settings) error { return nil }

// TestSettingsMissingSection: a store that returns the zero Settings for a settings.json without an "audio"
// section gets 05's defaults (03 §14.2), not mic users heard and the fast path off; an explicit section that turns
// them off is kept.
func TestSettingsMissingSection(t *testing.T) {
	s := New(newStub(), noSectionStore{}, testSelf, nil)
	defer func() { _ = s.Close() }()
	if ex := s.Exclusions(); !ex.KeepOutMicUsers || !ex.FastPath || !s.FriendsHear().KeepOutMicUsers {
		t.Errorf("zero Settings: KeepOutMicUsers %v, FastPath %v", ex.KeepOutMicUsers, ex.FastPath)
	}
	var off Settings
	if err := json.Unmarshal([]byte(`{"keepOutMicUsers": false, "fastPath": false}`), &off); err != nil {
		t.Fatal(err)
	}
	s2 := New(newStub(), newStore(off), testSelf, nil)
	defer func() { _ = s2.Close() }()
	if ex := s2.Exclusions(); ex.KeepOutMicUsers || ex.FastPath {
		t.Errorf("explicit off: KeepOutMicUsers %v, FastPath %v", ex.KeepOutMicUsers, ex.FastPath)
	}
}

// FuzzSettingsJSON: settings.json is hand-editable, so any audio section reads without a panic, and what the
// service writes back reads back to the same bytes; normalizing never keeps more than 256 decisions or a locked or
// invalid one.
func FuzzSettingsJSON(f *testing.F) {
	for _, seed := range []string{`{}`, `null`, `[]`, `{"appRules": {"exe:kook.exe": "keep-out", "discord": "include"}}`,
		`{"keepOutMicUsers": false, "fastPath": false, "dismissedTips": ["ducking", 3], "x": {"y": null}}`,
		`{"appRules": {"exe:a.exe": "include", "exe:a.exe": "keep-out"}}`, `{"appRules": "no"}`, `{"appRules": {`} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		var s Settings
		if json.Unmarshal(in, &s) != nil {
			return
		}
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		var back Settings
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatalf("what Marshal wrote doesn't read: %v\n%s", err, b)
		}
		if b2, _ := json.Marshal(back); string(b2) != string(b) {
			t.Fatalf("not stable:\n%s\n%s", b, b2)
		}
		n := s.normalized()
		if len(n.AppRules) > maxAppRules {
			t.Fatalf("%d decisions", len(n.AppRules))
		}
		for id, d := range n.AppRules {
			if !validAppID(id) || lockedApp(id) || !validDecision(d) {
				t.Fatalf("kept %q: %q", id, d)
			}
		}
		_ = buildExclusions(ruleInput{self: testSelf, settings: n})
	})
}

// TestAppRules lists the remembered decisions, also of apps that aren't running, sorted by name: the display name
// seen in this run, else the exe name.
func TestAppRules(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := DefaultSettings().withDecision("exe:zoomit.exe", DecisionKeepOut).withDecision("exe:kook.exe",
			DecisionKeepOut).withDecision("exe:chrome.exe", DecisionInclude)
		ctl := newStub()
		ctl.set(func(r *media.AudioReport) {
			r.Apps = []media.AudioApp{app("chrome.exe", "Google Chrome", verdictHeard, ""),
				app("kook.exe", "KOOK", verdictKeptOut, "user")}
		})
		s := New(ctl, newStore(st), testSelf, nil)
		stop := runService(t, s)
		defer stop()
		if got := s.AppRules(); !reflect.DeepEqual(got, []AppRule{
			{ID: "exe:chrome.exe", Name: "chrome.exe", Decision: DecisionInclude},
			{ID: "exe:kook.exe", Name: "kook.exe", Decision: DecisionKeepOut},
			{ID: "exe:zoomit.exe", Name: "zoomit.exe", Decision: DecisionKeepOut},
		}) {
			t.Errorf("before a report: %+v", got)
		}
		_ = s.Watch(true)
		settle(time.Second)
		_ = s.Watch(false) // the names stay for the run
		if got := s.AppRules(); !reflect.DeepEqual(got, []AppRule{
			{ID: "exe:chrome.exe", Name: "Google Chrome", Decision: DecisionInclude},
			{ID: "exe:kook.exe", Name: "KOOK", Decision: DecisionKeepOut},
			{ID: "exe:zoomit.exe", Name: "zoomit.exe", Decision: DecisionKeepOut},
		}) {
			t.Errorf("after a report: %+v", got)
		}
		stop()
	})
}
