// Package settings keeps the desktop app's settings.json (docs/m2/03-desktop-app.md §14.2, "03" below) in the data
// root (%LOCALAPPDATA%\isshoni on Windows): its schema, load with recovery and migrations, and debounced atomic
// saves. It also holds 05's opaque `audio` section and implements 05's audio.SettingsStore through AudioStore.
//
// The server list, the active server, users and devices are not here: they are 02's servers.json
// (internal/client/deviceauth). settings.json keeps per-server preferences only (serverPrefs, keyed by origin); an
// entry whose origin isn't in servers.json is dropped at load. No token, password, window title or share label is
// ever written here, and nothing in this package touches the credential store.
//
// Reads: unknown fields are kept and written back, so a newer app's settings survive a downgrade. A file that
// doesn't parse is renamed settings.json.bad-<UTC timestamp> and settings.json.bak (the last good copy) is tried; if
// both fail, the defaults apply. A file that exists but can't be read (an antivirus holding it, an ACL) is left as it
// is: settings.json.bak or the defaults are used in memory, and nothing is written until a setting changes.
// Migrations are forward-only, one function per schema step, run at load; a file with a newer schema than this build
// knows is read best-effort and not migrated (its schema is written back unchanged).
//
// Writes: debounced (500 ms), then settings.json.tmp is written and fsynced, the old settings.json is copied to
// settings.json.bak, and the temporary file is renamed over settings.json (os.Rename: MoveFileEx with
// MOVEFILE_REPLACE_EXISTING on Windows). A failed write leaves settings.json as it was and is retried with the next
// change, or by Flush.
package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/desktop/hotkey"
	"github.com/MoonWX/isshoni/internal/protocol"
)

// The files of 03 §14.1.
const (
	FileName = "settings.json"
	tmpName  = FileName + ".tmp"
	bakName  = FileName + ".bak"
	badName  = FileName + ".bad-" // + UTC timestamp
)

// CurrentSchema is the schema this build writes (03 §14.2).
const CurrentSchema = 1

// DefaultDebounce is how long a change waits before it is written (03 §20).
const DefaultDebounce = 500 * time.Millisecond

// maxObjectKeys bounds the members of every JSON object in settings.json (03 §14.2, §20): four times its largest
// object, 05's appRules with at most 256 entries (05 §14.1); serverPrefs has one entry per server, at most 20 (02),
// and pill.positions one per monitor. A file with a larger object anywhere is not a settings file: it counts as one
// that doesn't parse. Update and SetAudioSection refuse a value with one, and a write that would make one fails, so
// the store never writes a file it would set aside. It is also the bound of what a merge allocates.
const maxObjectKeys = 1024

// errTooManyKeys is the error of a JSON object with more than maxObjectKeys members.
var errTooManyKeys = fmt.Errorf("settings: a JSON object with more than %d keys", maxObjectKeys)

// The values of the enumerated settings.
const (
	PillAuto   = "auto"   // "Only when Windows shows no border" (the default)
	PillAlways = "always" // "Always"

	ChannelStable = "stable"
	ChannelBeta   = "beta"

	LogInfo  = "info"
	LogDebug = "debug"
)

// ErrInvalid is wrapped by Update's error for a value outside its setting's range; the error names the field.
var ErrInvalid = errors.New("settings: invalid value")

// Settings is settings.json, schema 1 (03 §14.2).
type Settings struct {
	Schema          int                    `json:"schema"`
	ServerPrefs     map[string]ServerPrefs `json:"serverPrefs"` // keyed by the server's origin (servers.json's)
	WizardDone      bool                   `json:"wizardDone"`  // the wizard's sound and voice steps were seen once
	Window          *Window                `json:"window"`      // the main window's last bounds; nil before the first
	CloseToTray     bool                   `json:"closeToTray"`
	Autostart       bool                   `json:"autostart"`
	HideFromCapture bool                   `json:"hideFromCapture"`
	Notifications   bool                   `json:"notifications"`
	Hotkeys         Hotkeys                `json:"hotkeys"`
	Pill            Pill                   `json:"pill"`
	Updates         Updates                `json:"updates"`
	Tips            Tips                   `json:"tips"`
	Log             Log                    `json:"log"`
	// Audio is 05's section (05 §14.1), opaque here: AudioStore reads and writes it as 05's audio.Settings.
	Audio json.RawMessage `json:"audio"`
}

// ServerPrefs are one server's preferences.
type ServerPrefs struct {
	Preset protocol.Preset `json:"preset"` // the default "Optimize for": auto, game, movie or text
}

// Window is the main window's last placement (03 §4.4), clamped to a visible monitor when it is used.
type Window struct {
	X         int  `json:"x"`
	Y         int  `json:"y"`
	W         int  `json:"w"`
	H         int  `json:"h"`
	Maximized bool `json:"maximized"`
}

// Hotkeys are the Pause and Stop combinations in hotkey's text form. A value that doesn't parse is kept as it is;
// the app reports it as "invalid" (03 §6.2 Settings.Hotkeys) and registers nothing for it.
type Hotkeys struct {
	Pause string `json:"pause"`
	Stop  string `json:"stop"`
}

// Pill is the Sharing pill's settings (03 §11.4).
type Pill struct {
	Mode      string           `json:"mode"`      // PillAuto or PillAlways
	Positions map[string]Point `json:"positions"` // the last position per monitor device name
}

// Point is a position in screen coordinates.
type Point struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// Updates are the update settings (03 §16.2, 06 §8).
type Updates struct {
	Check   bool   `json:"check"`
	Channel string `json:"channel"` // ChannelStable or ChannelBeta
}

// Tips record the one-time sheets and notices that were shown.
type Tips struct {
	FirstShareShown bool `json:"firstShareShown"`
	DNDShown        bool `json:"dndShown"`
	TrayNoticeShown bool `json:"trayNoticeShown"`
}

// Log is the log settings (03 §17).
type Log struct {
	Level string `json:"level"` // LogInfo or LogDebug
}

// Defaults returns the settings of a new install (03 §14.2).
func Defaults() Settings {
	return Settings{
		Schema:          CurrentSchema,
		ServerPrefs:     map[string]ServerPrefs{},
		HideFromCapture: true,
		Notifications:   true,
		Hotkeys:         Hotkeys{Pause: hotkey.DefaultPause.String(), Stop: hotkey.DefaultStop.String()},
		Pill:            Pill{Mode: PillAuto, Positions: map[string]Point{}},
		Updates:         Updates{Check: true, Channel: ChannelStable},
		Log:             Log{Level: LogInfo},
		Audio:           json.RawMessage(`{}`),
	}
}

// clone returns a deep copy.
func (s Settings) clone() Settings {
	s.ServerPrefs = maps.Clone(s.ServerPrefs)
	if s.ServerPrefs == nil {
		s.ServerPrefs = map[string]ServerPrefs{}
	}
	if s.Window != nil {
		w := *s.Window
		s.Window = &w
	}
	s.Pill.Positions = maps.Clone(s.Pill.Positions)
	if s.Pill.Positions == nil {
		s.Pill.Positions = map[string]Point{}
	}
	s.Audio = slices.Clone(s.Audio)
	return s
}

// Options configures Open.
type Options struct {
	// Dir is the data root (03 §14.1); settings.json lives in it. Required.
	Dir string
	// UpdateChecks reads the installer's HKCU\Software\isshoni\UpdateChecks DWORD (06 §2.3): ok false when it is
	// absent. Open asks it once, only when settings.json has no updates.check yet, and saves the answer. nil means
	// absent (every OS but Windows).
	UpdateChecks func() (value uint32, ok bool)
	// KnownServer reports whether an origin is in servers.json (deviceauth.Servers.Has). serverPrefs entries of
	// other origins are dropped at load. nil keeps every entry.
	KnownServer func(origin string) bool
	// Debounce is how long a change waits before it is written; 0 means DefaultDebounce.
	Debounce time.Duration
	// Clock names the .bad-<timestamp> copies; nil means time.Now.
	Clock func() time.Time
	// Logger gets the WARNs of recovery and failed writes; nil means slog.Default(). Lines never name a path (the
	// data root holds the Windows account name).
	Logger *slog.Logger

	fs fileSystem // tests inject faults; nil means the OS
}

// Store is the loaded settings.json. Safe for concurrent use.
type Store struct {
	dir      string
	debounce time.Duration
	clock    func() time.Time
	log      *slog.Logger
	fs       fileSystem

	writeMu sync.Mutex // one write at a time, in the order the snapshots were taken

	mu     sync.Mutex
	cur    Settings
	raw    map[string]any // the file as last read or written: unknown fields live here
	dirty  bool
	timer  *time.Timer
	closed bool
}

// Open loads settings.json from o.Dir (03 §14.2). A missing file gives the defaults; a broken one is set aside and
// the last good copy or the defaults are used, so Open fails only when o.Dir is unusable. When settings.json had no
// updates.check, the installer's value is read (o.UpdateChecks) and the result is saved at once, so it is read
// only once ever. A settings.json that can't be read is neither written nor answered from the installer's value at
// start.
func Open(o Options) (*Store, error) {
	if o.Dir == "" {
		return nil, errors.New("settings: Open needs a Dir")
	}
	s := &Store{
		dir:      o.Dir,
		debounce: o.Debounce,
		clock:    o.Clock,
		log:      o.Logger,
		fs:       o.fs,
	}
	if s.debounce <= 0 {
		s.debounce = DefaultDebounce
	}
	if s.clock == nil {
		s.clock = time.Now
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	s.log = s.log.With("component", "settings")
	if s.fs == nil {
		s.fs = osFS{}
	}
	if err := s.fs.MkdirAll(o.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("settings: the data folder can't be created: %w", pathless(err))
	}

	l := s.read()
	cur, repaired := decode(l.raw, s.log)
	// A newer app's file is written back only when something else needs it: a repair would replace its values with
	// this build's defaults.
	save := l.save || (repaired && cur.Schema <= CurrentSchema)
	// A settings.json that exists but can't be read most likely has updates.check: the installer's value is not
	// asked for it (it is read only when the file has none, once ever).
	if !hasUpdateCheck(l.raw) && !l.unreadable {
		cur.Updates.Check = true
		if o.UpdateChecks != nil {
			if v, ok := o.UpdateChecks(); ok && v == 0 {
				cur.Updates.Check = false
			}
		}
		save = true
	}
	if o.KnownServer != nil {
		for origin := range cur.ServerPrefs {
			if !o.KnownServer(origin) {
				delete(cur.ServerPrefs, origin)
				save = true
			}
		}
	}
	s.cur, s.raw = cur, l.raw
	// An unreadable settings.json is not overwritten at start: what was read stays in memory, and only a later change
	// writes.
	if (save || !l.found) && !l.unreadable {
		s.dirty = true
		if err := s.Flush(); err != nil {
			s.log.Warn("settings: settings.json not written at start; the next change tries again", "err", err)
		}
	}
	return s, nil
}

// loaded is what read found.
type loaded struct {
	raw   map[string]any // the file's JSON object, migrated (settings.json, else settings.json.bak, else empty)
	found bool           // there was a settings.json
	save  bool           // something had to be repaired: write it back at start
	// unreadable: settings.json exists but couldn't be read (an antivirus holding it, an ACL), so raw is
	// settings.json.bak or empty. Nothing is written or asked at start: the file may be fine at the next start.
	unreadable bool
}

// read returns the file's JSON object, migrated: settings.json, else settings.json.bak, else an empty object.
func (s *Store) read() loaded {
	path := filepath.Join(s.dir, FileName)
	data, err := s.fs.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return loaded{raw: map[string]any{}, save: true}
	case err != nil:
		// Not set aside: the file itself may be fine.
		s.log.Warn("settings: settings.json can't be read; it is left as it is until a setting changes",
			"err", pathless(err))
		raw, ok := s.readBak()
		if !ok {
			s.log.Warn("settings: no usable copy; using the defaults")
		}
		return loaded{raw: raw, found: true, unreadable: true}
	}
	if raw, err := parse(data); err == nil {
		raw, migrated := migrate(raw, s.log)
		return loaded{raw: raw, found: true, save: migrated}
	}
	bad := s.badName()
	if err := s.fs.Rename(path, filepath.Join(s.dir, bad)); err != nil {
		s.log.Warn("settings: a settings.json that doesn't parse couldn't be set aside", "err", pathless(err))
	} else {
		s.log.Warn("settings: settings.json doesn't parse; set it aside", "file", bad)
	}
	raw, ok := s.readBak()
	if !ok {
		s.log.Warn("settings: no usable copy; using the defaults")
	}
	return loaded{raw: raw, found: true, save: true}
}

// readBak returns settings.json.bak's JSON object, migrated; ok false (and an empty object) when there is none
// that parses.
func (s *Store) readBak() (raw map[string]any, ok bool) {
	bak, err := s.fs.ReadFile(filepath.Join(s.dir, bakName))
	if err != nil {
		return map[string]any{}, false
	}
	raw, err = parse(bak)
	if err != nil {
		return map[string]any{}, false
	}
	s.log.Warn("settings: using the last good copy, settings.json.bak")
	raw, _ = migrate(raw, s.log)
	return raw, true
}

// badName is settings.json.bad-<UTC timestamp>, with a counter when that name is taken.
func (s *Store) badName() string {
	base := badName + s.clock().UTC().Format("20060102T150405Z")
	name := base
	for i := 1; i < 100; i++ {
		if _, err := s.fs.ReadFile(filepath.Join(s.dir, name)); errors.Is(err, fs.ErrNotExist) {
			break
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}
	return name
}

// parse reads a JSON object; numbers stay json.Number, so unknown fields are written back exactly. An object with
// more than maxObjectKeys members, at any depth, is errTooManyKeys.
func parse(data []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, errors.New("settings: not a JSON object")
	}
	if dec.More() {
		return nil, errors.New("settings: data after the JSON object")
	}
	if !fits(raw) {
		return nil, errTooManyKeys
	}
	return raw, nil
}

// fits reports whether every object in v, a decoded JSON value, has at most maxObjectKeys members.
func fits(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		if len(x) > maxObjectKeys {
			return false
		}
		for _, e := range x {
			if !fits(e) {
				return false
			}
		}
	case []any:
		for _, e := range x {
			if !fits(e) {
				return false
			}
		}
	}
	return true
}

// decode turns the raw object into Settings on top of the defaults and repairs values outside their ranges.
// changed reports a repair.
func decode(raw map[string]any, log *slog.Logger) (Settings, bool) {
	cur := Defaults()
	data, err := json.Marshal(raw)
	if err == nil {
		err = json.Unmarshal(data, &cur)
	}
	changed := false
	if err != nil {
		// A field of another type (a newer build's, or a hand edit) keeps its default; the rest is read.
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) {
			log.Warn("settings: a value of the wrong type; using its default", "field", te.Field)
		} else {
			log.Warn("settings: settings.json can't be read as settings; using the defaults", "err", err)
			cur = Defaults()
		}
		changed = true
	}
	if cur.Schema < CurrentSchema {
		cur.Schema = CurrentSchema
		changed = true
	}
	if repair(&cur) {
		changed = true
	}
	return cur, changed
}

// repair puts every value outside its range back to its default; it reports whether it changed anything.
func repair(s *Settings) bool {
	d := Defaults()
	changed := false
	if s.ServerPrefs == nil {
		s.ServerPrefs = map[string]ServerPrefs{}
	}
	for origin, p := range s.ServerPrefs {
		if !p.Preset.Valid() {
			p.Preset = protocol.PresetAuto
			s.ServerPrefs[origin] = p
			changed = true
		}
	}
	if s.Window != nil && (s.Window.W <= 0 || s.Window.H <= 0) {
		s.Window = nil
		changed = true
	}
	if s.Hotkeys.Pause == "" {
		s.Hotkeys.Pause, changed = d.Hotkeys.Pause, true
	}
	if s.Hotkeys.Stop == "" {
		s.Hotkeys.Stop, changed = d.Hotkeys.Stop, true
	}
	if s.Pill.Mode != PillAuto && s.Pill.Mode != PillAlways {
		s.Pill.Mode, changed = PillAuto, true
	}
	if s.Pill.Positions == nil {
		s.Pill.Positions = map[string]Point{}
	}
	if s.Updates.Channel != ChannelStable && s.Updates.Channel != ChannelBeta {
		s.Updates.Channel, changed = ChannelStable, true
	}
	if s.Log.Level != LogInfo && s.Log.Level != LogDebug {
		s.Log.Level, changed = LogInfo, true
	}
	if !isObject(s.Audio) {
		s.Audio, changed = json.RawMessage(`{}`), true
	}
	return changed
}

// validate checks the values Update changed (prev to next); the error names the field. A value that didn't change
// is not checked: a hotkey this build can't parse is kept as it was loaded (a newer build's key, or a hand edit;
// see Hotkeys), and it must not make every later change of another setting fail.
func validate(prev, next *Settings) error {
	switch {
	case len(next.ServerPrefs) > maxObjectKeys:
		return fieldError("serverPrefs")
	case len(next.Pill.Positions) > maxObjectKeys:
		return fieldError("pill.positions")
	}
	for origin, p := range next.ServerPrefs {
		if old, ok := prev.ServerPrefs[origin]; ok && old == p {
			continue
		}
		if !p.Preset.Valid() {
			return fieldError("serverPrefs[" + origin + "].preset")
		}
	}
	for _, h := range []struct {
		name, prev, next string
	}{
		{"hotkeys.pause", prev.Hotkeys.Pause, next.Hotkeys.Pause},
		{"hotkeys.stop", prev.Hotkeys.Stop, next.Hotkeys.Stop},
	} {
		if h.next == h.prev {
			continue
		}
		if _, err := hotkey.Parse(h.next); err != nil {
			return fieldError(h.name)
		}
	}
	w := next.Window
	switch {
	case !sameWindow(prev.Window, w) && w != nil && (w.W <= 0 || w.H <= 0):
		return fieldError("window")
	case next.Pill.Mode != prev.Pill.Mode && next.Pill.Mode != PillAuto && next.Pill.Mode != PillAlways:
		return fieldError("pill.mode")
	case next.Updates.Channel != prev.Updates.Channel && next.Updates.Channel != ChannelStable &&
		next.Updates.Channel != ChannelBeta:
		return fieldError("updates.channel")
	case next.Log.Level != prev.Log.Level && next.Log.Level != LogInfo && next.Log.Level != LogDebug:
		return fieldError("log.level")
	case !bytes.Equal(next.Audio, prev.Audio) && !isObject(next.Audio):
		return fieldError("audio")
	}
	return nil
}

func sameWindow(a, b *Window) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// FieldError is Update's error for one field; it wraps ErrInvalid.
type FieldError struct{ Field string }

func (e *FieldError) Error() string { return fmt.Sprintf("%v: %s", ErrInvalid, e.Field) }
func (e *FieldError) Unwrap() error { return ErrInvalid }

func fieldError(field string) error { return &FieldError{Field: field} }

// isObject reports whether m is a JSON object whose objects have at most maxObjectKeys members each.
func isObject(m json.RawMessage) bool {
	var v any
	if json.Unmarshal(m, &v) != nil {
		return false
	}
	o, ok := v.(map[string]any)
	return ok && fits(o)
}

// hasUpdateCheck reports whether the file had updates.check.
func hasUpdateCheck(raw map[string]any) bool {
	u, ok := raw["updates"].(map[string]any)
	if !ok {
		return false
	}
	_, ok = u["check"].(bool)
	return ok
}

// Get returns a copy of the settings.
func (s *Store) Get() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur.clone()
}

// Update changes the settings: fn edits a copy, which replaces them when it is valid (else Update returns a
// *FieldError and nothing changes); the change is written after the debounce. fn runs under the store's lock and must
// not call the Store.
func (s *Store) Update(fn func(*Settings)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cur.clone()
	fn(&next)
	next.Schema = s.cur.Schema
	if err := validate(&s.cur, &next); err != nil {
		return err
	}
	s.cur = next
	s.changedLocked()
	return nil
}

// changedLocked marks the settings dirty and arms the debounce timer.
func (s *Store) changedLocked() {
	s.dirty = true
	if s.closed || s.timer != nil {
		return
	}
	s.timer = time.AfterFunc(s.debounce, func() {
		s.mu.Lock()
		s.timer = nil
		s.mu.Unlock()
		if err := s.Flush(); err != nil {
			s.log.Warn("settings: settings.json not written; the next change tries again", "err", err)
		}
	})
}

// Flush writes a pending change now.
func (s *Store) Flush() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	cur, raw := s.cur.clone(), s.raw
	s.dirty = false
	s.mu.Unlock()

	merged, data, err := encode(raw, cur)
	if err == nil {
		err = s.write(data)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.dirty = true
		return err
	}
	s.raw = merged
	return nil
}

// Close writes a pending change and stops the debounce timer; later changes stay in memory only.
func (s *Store) Close() error {
	s.mu.Lock()
	s.closed = true
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.mu.Unlock()
	return s.Flush()
}

// write is the atomic write of 03 §14.2.
func (s *Store) write(data []byte) error {
	path := filepath.Join(s.dir, FileName)
	tmp := filepath.Join(s.dir, tmpName)
	if err := s.fs.WriteFile(tmp, data, 0o600); err != nil {
		_ = s.fs.Remove(tmp)
		return fmt.Errorf("settings: writing the new settings: %w", pathless(err))
	}
	if old, err := s.fs.ReadFile(path); err == nil {
		if _, perr := parse(old); perr == nil {
			if err := s.fs.WriteFile(filepath.Join(s.dir, bakName), old, 0o600); err != nil {
				s.log.Warn("settings: the last good copy wasn't updated", "err", pathless(err))
			}
		}
	}
	if err := s.fs.Rename(tmp, path); err != nil {
		_ = s.fs.Remove(tmp)
		return fmt.Errorf("settings: replacing settings.json: %w", pathless(err))
	}
	return nil
}

// keyed are the objects whose keys are data (origins, monitor names) that a change may remove: a key the file had
// and the settings no longer have must not come back. Each entry that stays is merged like any known object, so the
// fields a newer build added inside it are kept.
var keyed = map[string]bool{"serverPrefs": true, "pill.positions": true}

// replaced are the objects this build writes whole: 05's audio section, whose keys that 05's Settings doesn't have
// AudioStore keeps itself.
var replaced = map[string]bool{"audio": true}

// encode returns the file's new JSON object, cur over raw: unknown fields of raw (and of its known objects) stay.
func encode(raw map[string]any, cur Settings) (map[string]any, []byte, error) {
	data, err := json.Marshal(cur)
	if err != nil {
		return nil, nil, err
	}
	known, err := parse(data)
	if err != nil {
		return nil, nil, err
	}
	merged, err := mergeKnown(raw, known, "")
	if err != nil {
		return nil, nil, err
	}
	out, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return merged, append(out, '\n'), nil
}

// mergeKnown returns base with every key of known set to known's value; objects present in both are merged the same
// way, except the replaced ones, which known replaces. path is the object's place in the file ("" for the file,
// "serverPrefs[]" for an entry of a keyed object); a keyed object keeps only known's keys. An object of base or known
// with more than maxObjectKeys members, or a merged one that would have more, is errTooManyKeys: parse refused such a
// file, and the store must not write one.
func mergeKnown(base, known map[string]any, path string) (map[string]any, error) {
	if len(base) > maxObjectKeys || len(known) > maxObjectKeys {
		return nil, errTooManyKeys
	}
	out := make(map[string]any, len(base)+len(known))
	if !keyed[path] {
		maps.Copy(out, base)
	}
	for k, v := range known {
		var p string
		switch {
		case keyed[path]:
			p = path + "[]"
		case path != "":
			p = path + "." + k
		default:
			p = k
		}
		bo, bok := base[k].(map[string]any)
		ko, kok := v.(map[string]any)
		if bok && kok && !replaced[p] {
			m, err := mergeKnown(bo, ko, p)
			if err != nil {
				return nil, err
			}
			out[k] = m
			continue
		}
		out[k] = v
	}
	if len(out) > maxObjectKeys {
		return nil, errTooManyKeys
	}
	return out, nil
}

// WizardDone reports settings.wizardDone (06's update.Options.WizardDone).
func (s *Store) WizardDone() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur.WizardDone
}

// Updates returns the update settings (06's update.Options.Enabled and Channel read them before every check).
func (s *Store) Updates() Updates {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur.Updates
}

// ServerPrefs returns origin's preferences, the defaults (preset auto) when it has none.
func (s *Store) ServerPrefs(origin string) ServerPrefs {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.cur.ServerPrefs[origin]; ok {
		return p
	}
	return ServerPrefs{Preset: protocol.PresetAuto}
}

// DebugLogging reports log.level debug (03 §17).
func (s *Store) DebugLogging() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur.Log.Level == LogDebug
}

// AudioSection returns 05's section as JSON (a copy).
func (s *Store) AudioSection() json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.cur.Audio)
}

// SetAudioSection replaces 05's section, which must be a JSON object; it is written after the debounce.
func (s *Store) SetAudioSection(section json.RawMessage) error {
	if !isObject(section) {
		return fieldError("audio")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, section); err != nil {
		return fieldError("audio")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cur.Audio = compact.Bytes()
	s.changedLocked()
	return nil
}

// pathless drops the path from a file-system error (an *fs.PathError or *os.LinkError keeps its operation and its
// cause): the data root holds the Windows account name, which no log line carries.
func pathless(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: %w", pe.Op, pe.Err)
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return fmt.Errorf("%s: %w", le.Op, le.Err)
	}
	return err
}

// fileSystem is what the store needs from the OS; tests inject faults.
type fileSystem interface {
	ReadFile(name string) ([]byte, error)
	WriteFile(name string, data []byte, perm fs.FileMode) error // the data is on disk (fsynced) when it returns nil
	Rename(oldpath, newpath string) error
	Remove(name string) error
	MkdirAll(path string, perm fs.FileMode) error
}

type osFS struct{}

func (osFS) ReadFile(name string) ([]byte, error)         { return os.ReadFile(name) }
func (osFS) Rename(oldpath, newpath string) error         { return os.Rename(oldpath, newpath) }
func (osFS) Remove(name string) error                     { return os.Remove(name) }
func (osFS) MkdirAll(path string, perm fs.FileMode) error { return os.MkdirAll(path, perm) }

func (osFS) WriteFile(name string, data []byte, perm fs.FileMode) (err error) {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}
