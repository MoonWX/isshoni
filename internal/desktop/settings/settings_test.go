package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/client/credstore"
	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/protocol"
)

var fixedNow = time.Date(2026, 10, 22, 12, 30, 45, 0, time.UTC)

func open(t *testing.T, o Options) *Store {
	t.Helper()
	if o.Clock == nil {
		o.Clock = func() time.Time { return fixedNow }
	}
	s, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return m
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultsAndRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := open(t, Options{Dir: dir})
	got := s.Get()
	want := Defaults()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("a new install: %+v, want %+v", got, want)
	}
	if got.Hotkeys.Pause != "Ctrl+Alt+Shift+F9" || got.Hotkeys.Stop != "Ctrl+Alt+Shift+F10" || !got.HideFromCapture ||
		!got.Notifications || got.CloseToTray || got.Autostart || got.Pill.Mode != PillAuto ||
		!got.Updates.Check || got.Updates.Channel != ChannelStable || got.Log.Level != LogInfo {
		t.Errorf("defaults differ from 03 §14.2: %+v", got)
	}
	// The first start writes the file (updates.check read once, 03 §14.2).
	if m := readJSON(t, filepath.Join(dir, FileName)); m["schema"] != 1.0 {
		t.Errorf("written: %v", m)
	}

	err := s.Update(func(v *Settings) {
		v.WizardDone = true
		v.ServerPrefs["https://watch.example.com"] = ServerPrefs{Preset: protocol.PresetGame}
		v.Window = &Window{X: 10, Y: 20, W: 1200, H: 760}
		v.Pill.Positions[`\\.\DISPLAY1`] = Point{X: 830, Y: 8}
		v.Hotkeys.Pause = "Ctrl+Alt+F8"
		v.Updates.Channel = ChannelBeta
		v.Tips.DNDShown = true
		v.Log.Level = LogDebug
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	again := open(t, Options{Dir: dir, KnownServer: func(string) bool { return true }})
	g := again.Get()
	if !g.WizardDone || g.ServerPrefs["https://watch.example.com"].Preset != protocol.PresetGame ||
		g.Window == nil || g.Window.X != 10 || g.Pill.Positions[`\\.\DISPLAY1`].X != 830 ||
		g.Hotkeys.Pause != "Ctrl+Alt+F8" || g.Updates.Channel != ChannelBeta || !g.Tips.DNDShown ||
		g.Log.Level != LogDebug {
		t.Errorf("round trip: %+v", g)
	}
	if !again.WizardDone() || again.Updates().Channel != ChannelBeta || !again.DebugLogging() {
		t.Error("accessors")
	}
	if p := again.ServerPrefs("https://other.example.com"); p.Preset != protocol.PresetAuto {
		t.Errorf("prefs of a server without any: %+v", p)
	}
	// Get returns a copy.
	g.ServerPrefs["x"] = ServerPrefs{}
	g.Window.X = 99
	if h := again.Get(); h.Window.X != 10 || len(h.ServerPrefs) != 1 {
		t.Error("Get shares memory with the store")
	}
}

func TestUnknownFieldsKept(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, FileName), `{
	  "schema": 1,
	  "future": {"x": [1, 2.50, "y"], "big": 12345678901234567890},
	  "updates": {"check": true, "channel": "stable", "autoInstall": true},
	  "pill": {"mode": "auto", "positions": {"D1": {"x": 1, "y": 2, "dpi": 144}, "D2": {"x": 3, "y": 4}}, "size": "small"},
	  "tips": {"firstShareShown": true, "laterTip": true},
	  "audio": {"keepOutMicUsers": true, "newAudioKey": 3},
	  "serverPrefs": {
	    "https://a.example.com": {"preset": "game", "newerField": {"on": true}},
	    "https://b.example.com": {"preset": "movie", "newerField": 1}
	  }
	}`)
	s := open(t, Options{Dir: dir})
	err := s.Update(func(v *Settings) {
		v.CloseToTray = true
		v.ServerPrefs["https://a.example.com"] = ServerPrefs{Preset: protocol.PresetText}
		delete(v.ServerPrefs, "https://b.example.com")
		delete(v.Pill.Positions, "D2")
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	// Numbers are written back exactly as they were (json.Number), also beyond float64's precision.
	if !bytes.Contains(data, []byte("12345678901234567890")) || !bytes.Contains(data, []byte("2.50")) {
		t.Errorf("numbers changed:\n%s", data)
	}
	m := readJSON(t, filepath.Join(dir, FileName))
	if m["closeToTray"] != true {
		t.Error("the change is missing")
	}
	if _, ok := m["future"].(map[string]any); !ok {
		t.Error("an unknown top-level field was dropped")
	}
	if m["updates"].(map[string]any)["autoInstall"] != true || m["pill"].(map[string]any)["size"] != "small" ||
		m["tips"].(map[string]any)["laterTip"] != true {
		t.Errorf("unknown fields of known objects were dropped: %v", m)
	}
	if m["audio"].(map[string]any)["newAudioKey"] != 3.0 {
		t.Errorf("the audio section lost a key: %v", m["audio"])
	}
	// Inside the keyed objects, the entries that stay keep a newer build's fields; the removed ones stay removed.
	prefs := m["serverPrefs"].(map[string]any)
	a, _ := prefs["https://a.example.com"].(map[string]any)
	if len(prefs) != 1 || a["preset"] != "text" || a["newerField"].(map[string]any)["on"] != true {
		t.Errorf("serverPrefs = %v", prefs)
	}
	pos := m["pill"].(map[string]any)["positions"].(map[string]any)
	if d1, _ := pos["D1"].(map[string]any); len(pos) != 1 || d1["x"] != 1.0 || d1["dpi"] != 144.0 {
		t.Errorf("pill.positions = %v", pos)
	}
}

// countingStore is a credential store that counts its calls: a settings problem never touches credentials.
type countingStore struct {
	credstore.Store
	calls atomic.Int64
}

func (c *countingStore) Get(o string) (credstore.Credential, error) {
	c.calls.Add(1)
	return c.Store.Get(o)
}

func (c *countingStore) Put(o string, cr credstore.Credential) error {
	c.calls.Add(1)
	return c.Store.Put(o, cr)
}

func (c *countingStore) Delete(o string) error {
	c.calls.Add(1)
	return c.Store.Delete(o)
}

func (c *countingStore) List() ([]string, error) {
	c.calls.Add(1)
	return c.Store.List()
}

func TestCorruptFileUsesBak(t *testing.T) {
	dir := t.TempDir()
	creds := &countingStore{Store: credstore.NewMemory()}
	srv, err := deviceauth.OpenServers(deviceauth.ServersOptions{Dir: dir, Store: creds,
		Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	before := creds.calls.Load()

	writeFile(t, filepath.Join(dir, FileName), `{"schema": 1, "wizardDone": tru`)
	writeFile(t, filepath.Join(dir, bakName), `{"schema": 1, "wizardDone": true, "closeToTray": true}`)
	var logs bytes.Buffer
	s := open(t, Options{Dir: dir, KnownServer: srv.Has, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	if g := s.Get(); !g.WizardDone || !g.CloseToTray {
		t.Errorf("the last good copy wasn't used: %+v", g)
	}
	bad := filepath.Join(dir, badName+"20261022T123045Z")
	if data, err := os.ReadFile(bad); err != nil || string(data) != `{"schema": 1, "wizardDone": tru` {
		t.Errorf("the broken file wasn't set aside as %s: %q, %v", filepath.Base(bad), data, err)
	}
	// settings.json is good again.
	if m := readJSON(t, filepath.Join(dir, FileName)); m["wizardDone"] != true {
		t.Errorf("settings.json: %v", m)
	}
	if creds.calls.Load() != before {
		t.Error("a settings problem touched the credential store")
	}
	if strings.Contains(logs.String(), dir) {
		t.Errorf("a log line names the data folder:\n%s", logs.String())
	}
}

func TestCorruptFileAndBakGiveDefaults(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, FileName), `not json`)
	writeFile(t, filepath.Join(dir, bakName), `[1, 2]`)
	// A second broken file at the same second gets its own name.
	writeFile(t, filepath.Join(dir, badName+"20261022T123045Z"), `older`)
	s := open(t, Options{Dir: dir})
	if g := s.Get(); !reflect.DeepEqual(g, Defaults()) {
		t.Errorf("not the defaults: %+v", g)
	}
	if data, err := os.ReadFile(filepath.Join(dir, badName+"20261022T123045Z-1")); err != nil || string(data) != "not json" {
		t.Errorf("set aside: %q, %v", data, err)
	}
	if m := readJSON(t, filepath.Join(dir, FileName)); m["schema"] != 1.0 {
		t.Errorf("settings.json: %v", m)
	}
}

func TestMigrationFromSchema0(t *testing.T) {
	dir := t.TempDir()
	fixture, err := os.ReadFile(filepath.Join("testdata", "schema0.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), fixture, 0o600); err != nil { //nolint:gosec // G703: t.TempDir()
		t.Fatal(err)
	}
	s := open(t, Options{Dir: dir, UpdateChecks: func() (uint32, bool) {
		t.Error("UpdateChecks read although the file has updates.check")
		return 1, true
	}})
	g := s.Get()
	if g.Schema != CurrentSchema || !g.WizardDone || !g.CloseToTray || g.Window == nil || !g.Window.Maximized ||
		g.Pill.Mode != PillAlways || g.Pill.Positions[`\\.\DISPLAY1`].Y != 8 || g.Updates.Check ||
		g.Updates.Channel != ChannelBeta || !g.Tips.FirstShareShown ||
		g.ServerPrefs["https://watch.example.com"].Preset != protocol.PresetGame {
		t.Errorf("migrated: %+v", g)
	}
	// The nulls took their defaults.
	if g.Hotkeys != Defaults().Hotkeys || g.Log.Level != LogInfo {
		t.Errorf("nulls: %+v %+v", g.Hotkeys, g.Log)
	}
	if string(g.Audio) != `{"keepOutMicUsers":false}` {
		t.Errorf("audio: %s", g.Audio)
	}
	m := readJSON(t, filepath.Join(dir, FileName))
	if m["schema"] != 1.0 {
		t.Errorf("the migrated file wasn't written with schema 1: %v", m["schema"])
	}
	if _, ok := m["fromAnOlderBuild"]; !ok {
		t.Error("the migration dropped an unknown field")
	}
}

func TestNewerSchemaReadBestEffort(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	newer := `{"schema": 7, "wizardDone": true, "pill": {"mode": "never", "positions": {}}, "tips": "moved",
	  "updates": {"check": false, "channel": "nightly"}, "newThing": {"a": 1}}`
	writeFile(t, path, newer)
	s := open(t, Options{Dir: dir})
	g := s.Get()
	if g.Schema != 7 || !g.WizardDone || g.Updates.Check {
		t.Errorf("read: %+v", g)
	}
	if g.Pill.Mode != PillAuto || g.Updates.Channel != ChannelStable || g.Tips != (Tips{}) {
		t.Errorf("unknown values should read as their defaults: %+v", g)
	}
	// Nothing is written at start: a repair would replace the newer app's values.
	if data, _ := os.ReadFile(path); string(data) != newer {
		t.Errorf("the newer file was rewritten at start:\n%s", data)
	}
	if err := s.Update(func(v *Settings) { v.Notifications = false }); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	m := readJSON(t, path)
	if m["schema"] != 7.0 || m["newThing"] == nil || m["notifications"] != false {
		t.Errorf("after a change: %v", m)
	}
}

func TestUpdateChecksReadOnce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value uint32
		ok    bool
		want  bool
	}{
		{"absent", 0, false, true},
		{"zero", 0, true, false},
		{"one", 1, true, true},
		{"other", 7, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			reads := 0
			s := open(t, Options{Dir: dir, UpdateChecks: func() (uint32, bool) { reads++; return tc.value, tc.ok }})
			if s.Updates().Check != tc.want || reads != 1 {
				t.Errorf("check = %v after %d reads, want %v after 1", s.Updates().Check, reads, tc.want)
			}
			if m := readJSON(t, filepath.Join(dir, FileName)); m["updates"].(map[string]any)["check"] != tc.want {
				t.Errorf("not saved: %v", m["updates"])
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			// Never read again, whatever the registry says now.
			again := open(t, Options{Dir: dir, UpdateChecks: func() (uint32, bool) {
				t.Error("UpdateChecks read a second time")
				return 1 - tc.value, true
			}})
			if again.Updates().Check != tc.want {
				t.Errorf("second start: %v", again.Updates().Check)
			}
		})
	}
	t.Run("a file without updates.check", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, FileName), `{"schema": 1, "updates": {"channel": "beta"}}`)
		s := open(t, Options{Dir: dir, UpdateChecks: func() (uint32, bool) { return 0, true }})
		if u := s.Updates(); u.Check || u.Channel != ChannelBeta {
			t.Errorf("updates = %+v", u)
		}
	})
}

func TestServerPrefsOfUnknownServersDropped(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, FileName), `{"schema": 1, "serverPrefs": {
	  "https://kept.example.com": {"preset": "movie"},
	  "https://gone.example.com": {"preset": "game"},
	  "https://bad.example.com": {"preset": "nope"}}}`)
	known := map[string]bool{"https://kept.example.com": true, "https://bad.example.com": true}
	s := open(t, Options{Dir: dir, KnownServer: func(o string) bool { return known[o] }})
	g := s.Get()
	if len(g.ServerPrefs) != 2 || g.ServerPrefs["https://kept.example.com"].Preset != protocol.PresetMovie ||
		g.ServerPrefs["https://bad.example.com"].Preset != protocol.PresetAuto {
		t.Errorf("serverPrefs = %v", g.ServerPrefs)
	}
	prefs := readJSON(t, filepath.Join(dir, FileName))["serverPrefs"].(map[string]any)
	if _, ok := prefs["https://gone.example.com"]; ok || len(prefs) != 2 {
		t.Errorf("written serverPrefs = %v", prefs)
	}
}

func TestUpdateValidates(t *testing.T) {
	s := open(t, Options{Dir: t.TempDir()})
	before := s.Get()
	for field, fn := range map[string]func(*Settings){
		"pill.mode":       func(v *Settings) { v.Pill.Mode = "sometimes" },
		"updates.channel": func(v *Settings) { v.Updates.Channel = "nightly" },
		"log.level":       func(v *Settings) { v.Log.Level = "trace" },
		"hotkeys.pause":   func(v *Settings) { v.Hotkeys.Pause = "Win+F9" },
		"hotkeys.stop":    func(v *Settings) { v.Hotkeys.Stop = "F10" },
		"window":          func(v *Settings) { v.Window = &Window{W: 0, H: 10} },
		"audio":           func(v *Settings) { v.Audio = json.RawMessage(`[]`) },
		"serverPrefs[https://a.example.com].preset": func(v *Settings) { v.ServerPrefs["https://a.example.com"] = ServerPrefs{Preset: "fast"} },
	} {
		err := s.Update(fn)
		var fe *FieldError
		if !errors.As(err, &fe) || fe.Field != field || !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", field, err)
		}
	}
	if !reflect.DeepEqual(s.Get(), before) {
		t.Error("a refused Update changed the settings")
	}
	if err := s.SetAudioSection(json.RawMessage(`"x"`)); !errors.Is(err, ErrInvalid) {
		t.Errorf("SetAudioSection of a string: %v", err)
	}
}

// TestUnparsableHotkeyKept: a hotkey this build can't parse (a newer build's key, or a hand edit) is kept as it was
// loaded, and Update checks only what it changes: other settings still save, and the hotkey itself is checked when it
// changes.
func TestUnparsableHotkeyKept(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	writeFile(t, path, `{"schema": 1, "updates": {"check": true, "channel": "stable"},
	  "hotkeys": {"pause": "Ctrl+Alt+Shift+F25", "stop": "Ctrl+Alt+Shift+F10"}}`)
	s := open(t, Options{Dir: dir})
	if g := s.Get(); g.Hotkeys.Pause != "Ctrl+Alt+Shift+F25" {
		t.Fatalf("the hotkey wasn't kept: %+v", g.Hotkeys)
	}
	for name, fn := range map[string]func(*Settings){
		"tips.dndShown": func(v *Settings) { v.Tips.DNDShown = true },
		"wizardDone":    func(v *Settings) { v.WizardDone = true },
		"log.level":     func(v *Settings) { v.Log.Level = LogDebug },
		"window":        func(v *Settings) { v.Window = &Window{W: 900, H: 600} },
		"hotkeys.stop":  func(v *Settings) { v.Hotkeys.Stop = "Ctrl+Alt+F10" },
		"same pause":    func(v *Settings) { v.Hotkeys.Pause = "Ctrl+Alt+Shift+F25" },
	} {
		if err := s.Update(fn); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	err := s.Update(func(v *Settings) { v.WizardDone = false; v.Hotkeys.Pause = "Ctrl+Alt+Shift+F26" })
	var fe *FieldError
	if !errors.As(err, &fe) || fe.Field != "hotkeys.pause" {
		t.Errorf("an unparsable new hotkey: %v", err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	m := readJSON(t, path)
	if h := m["hotkeys"].(map[string]any); h["pause"] != "Ctrl+Alt+Shift+F25" || h["stop"] != "Ctrl+Alt+F10" ||
		m["wizardDone"] != true || m["tips"].(map[string]any)["dndShown"] != true {
		t.Errorf("written: %v", m)
	}
	if err := s.Update(func(v *Settings) { v.Hotkeys.Pause = "Ctrl+Alt+F8" }); err != nil {
		t.Errorf("a valid hotkey in its place: %v", err)
	}
}

// TestUnreadableFileLeftAlone: a settings.json that exists but can't be read (an antivirus holding it) is neither
// set aside nor overwritten at start, and the installer's UpdateChecks isn't asked for it; settings.json.bak is used
// meanwhile, and the next start reads the file as it was.
func TestUnreadableFileLeftAlone(t *testing.T) {
	const content = `{"schema": 1, "closeToTray": true, "updates": {"check": true, "channel": "beta"}}`
	for _, withBak := range []bool{false, true} {
		dir := t.TempDir()
		path := filepath.Join(dir, FileName)
		writeFile(t, path, content)
		if withBak {
			writeFile(t, filepath.Join(dir, bakName), `{"schema": 1, "closeToTray": true, "autostart": true,
			  "updates": {"check": true, "channel": "beta"}}`)
		}
		ffs := &faultFS{failRead: map[string]int{FileName: 1}}
		s := open(t, Options{Dir: dir, fs: ffs, UpdateChecks: func() (uint32, bool) {
			t.Error("UpdateChecks read for an unreadable settings.json")
			return 0, true
		}})
		g := s.Get()
		switch {
		case withBak && (!g.CloseToTray || !g.Autostart || g.Updates.Channel != ChannelBeta):
			t.Errorf("not the last good copy: %+v", g)
		case !withBak && !reflect.DeepEqual(g, Defaults()):
			t.Errorf("not the defaults: %+v", g)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if data, err := os.ReadFile(path); err != nil || string(data) != content {
			t.Errorf("settings.json changed at start: %s (%v)", data, err)
		}
		if bad, _ := filepath.Glob(filepath.Join(dir, badName+"*")); len(bad) != 0 {
			t.Errorf("set aside: %v", bad)
		}
		if ffs.count(FileName) != 0 || ffs.count(tmpName) != 0 {
			t.Error("written at start")
		}
		again := open(t, Options{Dir: dir})
		if g := again.Get(); !g.CloseToTray || g.Updates.Channel != ChannelBeta || !g.Updates.Check {
			t.Errorf("the next start: %+v", g)
		}
	}
	// A change after such a start is written.
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, FileName), content)
	s := open(t, Options{Dir: dir, fs: &faultFS{failRead: map[string]int{FileName: 1}}})
	if err := s.Update(func(v *Settings) { v.Notifications = false }); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if m := readJSON(t, filepath.Join(dir, FileName)); m["notifications"] != false {
		t.Errorf("the change wasn't written: %v", m)
	}
}

// TestNoSecrets: no field of Settings can hold a token, a password, a window title or a share label (03 §14.2).
func TestNoSecrets(t *testing.T) {
	banned := []string{"token", "password", "secret", "credential", "title", "label", "cookie"}
	secretType := reflect.TypeFor[protocol.Secret]()
	credType := reflect.TypeFor[credstore.Credential]()
	seen := map[reflect.Type]bool{}
	var walk func(path string, typ reflect.Type)
	walk = func(path string, typ reflect.Type) {
		if typ == secretType || typ == credType {
			t.Errorf("%s has the type %v", path, typ)
		}
		if seen[typ] {
			return
		}
		seen[typ] = true
		switch typ.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(path+"[]", typ.Elem())
		case reflect.Map:
			walk(path+"{}", typ.Elem())
		case reflect.Struct:
			for i := range typ.NumField() {
				f := typ.Field(i)
				name := strings.ToLower(f.Name + " " + f.Tag.Get("json"))
				for _, b := range banned {
					if strings.Contains(name, b) {
						t.Errorf("%s.%s may hold a %s", path, f.Name, b)
					}
				}
				walk(path+"."+f.Name, f.Type)
			}
		}
	}
	walk("Settings", reflect.TypeFor[Settings]())
}

// faultFS fails chosen operations; WriteFile of a failing name writes half of the data first, as a crash would.
type faultFS struct {
	osFS
	mu         sync.Mutex
	failWrite  map[string]bool // by base name
	failRead   map[string]int  // by base name: how many reads fail with fs.ErrPermission
	failRename bool
	writes     map[string]int
}

func (f *faultFS) ReadFile(name string) ([]byte, error) {
	f.mu.Lock()
	fail := f.failRead[filepath.Base(name)] > 0
	if fail {
		f.failRead[filepath.Base(name)]--
	}
	f.mu.Unlock()
	if fail {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrPermission}
	}
	return f.osFS.ReadFile(name)
}

func (f *faultFS) WriteFile(name string, data []byte, perm fs.FileMode) error {
	f.mu.Lock()
	fail := f.failWrite[filepath.Base(name)]
	if f.writes == nil {
		f.writes = map[string]int{}
	}
	f.writes[filepath.Base(name)]++
	f.mu.Unlock()
	if fail {
		_ = f.osFS.WriteFile(name, data[:len(data)/2], perm)
		return errors.New("disk full")
	}
	return f.osFS.WriteFile(name, data, perm)
}

func (f *faultFS) Rename(oldpath, newpath string) error {
	f.mu.Lock()
	fail := f.failRename
	f.mu.Unlock()
	if fail && filepath.Base(newpath) == FileName {
		return errors.New("access denied")
	}
	return f.osFS.Rename(oldpath, newpath)
}

func (f *faultFS) set(fn func(*faultFS)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *faultFS) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes[name]
}

func TestAtomicWriteWithFaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	ffs := &faultFS{}
	s := open(t, Options{Dir: dir, fs: ffs})
	if err := s.Update(func(v *Settings) { v.WizardDone = true }); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// The temporary file fails half-way: settings.json stays as it was, and no temporary file is left.
	ffs.set(func(f *faultFS) { f.failWrite = map[string]bool{tmpName: true} })
	if err := s.Update(func(v *Settings) { v.CloseToTray = true }); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err == nil {
		t.Fatal("Flush succeeded although the write failed")
	}
	if data, _ := os.ReadFile(path); !bytes.Equal(data, good) {
		t.Errorf("settings.json changed by a failed write:\n%s", data)
	}
	if _, err := os.Stat(filepath.Join(dir, tmpName)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the temporary file was left: %v", err)
	}

	// The rename fails: the same.
	ffs.set(func(f *faultFS) { f.failWrite = nil; f.failRename = true })
	if err := s.Flush(); err == nil {
		t.Fatal("Flush succeeded although the rename failed")
	}
	if data, _ := os.ReadFile(path); !bytes.Equal(data, good) {
		t.Errorf("settings.json changed by a failed rename:\n%s", data)
	}

	// The last good copy can't be written: the save still happens.
	ffs.set(func(f *faultFS) { f.failRename = false; f.failWrite = map[string]bool{bakName: true} })
	if err := s.Flush(); err != nil {
		t.Fatalf("a failed .bak stopped the save: %v", err)
	}
	if m := readJSON(t, path); m["closeToTray"] != true || m["wizardDone"] != true {
		t.Errorf("after the retry: %v", m)
	}

	// With every step working, .bak is the previous settings.json.
	ffs.set(func(f *faultFS) { f.failWrite = nil })
	prev, _ := os.ReadFile(path)
	if err := s.Update(func(v *Settings) { v.Autostart = true }); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if bak, _ := os.ReadFile(filepath.Join(dir, bakName)); !bytes.Equal(bak, prev) {
		t.Errorf(".bak is not the last good copy:\n%s\nwant\n%s", bak, prev)
	}
}

func TestDebouncedWrites(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		ffs := &faultFS{}
		s, err := Open(Options{Dir: dir, fs: ffs})
		if err != nil {
			t.Fatal(err)
		}
		start := ffs.count(tmpName) // the first start's write
		for i := range 5 {
			if err := s.Update(func(v *Settings) { v.Pill.Positions["m"] = Point{X: i} }); err != nil {
				t.Fatal(err)
			}
			time.Sleep(90 * time.Millisecond)
		}
		// 450 ms after the first change: nothing written yet.
		synctest.Wait()
		if n := ffs.count(tmpName) - start; n != 0 {
			t.Fatalf("%d writes before the debounce ended", n)
		}
		time.Sleep(60 * time.Millisecond)
		synctest.Wait()
		if n := ffs.count(tmpName) - start; n != 1 {
			t.Fatalf("%d writes after 500 ms, want 1", n)
		}
		m := readJSON(t, filepath.Join(dir, FileName))
		if x := m["pill"].(map[string]any)["positions"].(map[string]any)["m"].(map[string]any)["x"]; x != 4.0 {
			t.Errorf("the last change wasn't written: %v", x)
		}
		// Close writes what is pending, and later changes stay in memory.
		if err := s.Update(func(v *Settings) { v.Autostart = true }); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if readJSON(t, filepath.Join(dir, FileName))["autostart"] != true {
			t.Error("Close didn't write the pending change")
		}
		if err := s.Update(func(v *Settings) { v.Autostart = false }); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if readJSON(t, filepath.Join(dir, FileName))["autostart"] != true {
			t.Error("a change after Close was written")
		}
	})
}

// audioSettings mirrors 05's audio.Settings (05 §14.2), which lives in internal/desktop/audio.
type audioSettings struct {
	KeepOutMicUsers bool              `json:"keepOutMicUsers"`
	AppRules        map[string]string `json:"appRules"`
	FastPath        bool              `json:"fastPath"`
	DismissedTips   []string          `json:"dismissedTips"`
}

func audioDefaults() audioSettings {
	return audioSettings{KeepOutMicUsers: true, AppRules: map[string]string{}, FastPath: true, DismissedTips: []string{}}
}

// audioSettingsStore is 05's audio.SettingsStore with the mirrored type.
type audioSettingsStore interface {
	AudioSettings() audioSettings
	SaveAudioSettings(audioSettings) error
}

func TestAudioStore(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, FileName), `{"schema": 1, "audio": {"fastPath": false, "laterKey": "kept"}}`)
	s := open(t, Options{Dir: dir})
	var st audioSettingsStore = NewAudioStore(s, audioDefaults)
	got := st.AudioSettings()
	if !got.KeepOutMicUsers || got.FastPath || len(got.AppRules) != 0 {
		t.Errorf("defaults under the saved section: %+v", got)
	}
	got.AppRules["exe:kook.exe"] = "keep-out"
	got.DismissedTips = []string{"ducking"}
	if err := st.SaveAudioSettings(got); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	m := readJSON(t, filepath.Join(dir, FileName))["audio"].(map[string]any)
	if m["laterKey"] != "kept" || m["fastPath"] != false ||
		m["appRules"].(map[string]any)["exe:kook.exe"] != "keep-out" {
		t.Errorf("audio section: %v", m)
	}
	again := NewAudioStore(open(t, Options{Dir: dir}), audioDefaults).AudioSettings()
	if again.AppRules["exe:kook.exe"] != "keep-out" || again.DismissedTips[0] != "ducking" || again.FastPath {
		t.Errorf("round trip: %+v", again)
	}
	// A section that doesn't fit T gives T's defaults for what failed.
	s3 := open(t, Options{Dir: t.TempDir()})
	if err := s3.SetAudioSection(json.RawMessage(`{"keepOutMicUsers": "yes"}`)); err != nil {
		t.Fatal(err)
	}
	if v := NewAudioStore(s3, audioDefaults).AudioSettings(); !v.KeepOutMicUsers {
		t.Errorf("a broken value: %+v", v)
	}
	if v := NewAudioStore[audioSettings](s3, nil).AudioSettings(); v.KeepOutMicUsers {
		t.Error("nil defaults should be the zero value")
	}
}
