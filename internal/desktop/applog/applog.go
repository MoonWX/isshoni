// Package applog is the desktop app's log (docs/m2/03-desktop-app.md §17, "03" below): log/slog JSON lines through
// internal/logx into rotating files in the logs folder (%LOCALAPPDATA%\isshoni\logs on Windows), the redaction the
// files need because users attach them to public issues, the crash output and the clean-exit marker.
//
//   - Files: isshoni.log, rotated at 5 MiB; the five previous files are kept as isshoni.1.log (newest) …
//     isshoni.5.log.
//   - Redaction: logx's rules (protocol.Secret and logx.Secret print [redacted]; attributes named password, token,
//     … are replaced), and the user's profile folder is written as %USERPROFILE% in every message and string value,
//     errors included. Code that logs follows 03 §17's list of what is never logged at any level: window titles,
//     share labels, a deep link's path, executable and app names.
//   - Wails: WailsLogger is the logger the Wails application gets as Options.Logger. Its minimum level is Info
//     whatever log.level says, because the pinned Wails logs each bound call's raw arguments and result at Debug
//     ("Binding call complete"), a password or a token among them.
//   - Crashes: SetCrashOutput points Go's crash output (panics, fatal runtime errors) at crash-<UTC timestamp>.txt;
//     FindCrash finds a crash file newer than the last clean exit, which WriteCleanExit records in last-exit.json.
package applog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/logx"
)

// The files of the logs folder (03 §14.1, §17).
const (
	FileName     = "isshoni.log"
	LastExitName = "last-exit.json"
	crashPrefix  = "crash-"
	crashSuffix  = ".txt"
	crashLayout  = "20060102T150405.000Z"
)

// The rotation (03 §20: 5 MiB × 5 files).
const (
	DefaultMaxBytes = 5 << 20
	DefaultKeep     = 5
)

// ProfileToken replaces the user's profile folder in log lines.
const ProfileToken = "%USERPROFILE%"

// Options configures Open.
type Options struct {
	// Dir is the logs folder; it is created when missing. Required.
	Dir string
	// Level is the app logger's minimum level (log.level, --log-level debug); nil means a fixed Info.
	Level *slog.LevelVar
	// Profile is the user's profile folder (Windows: FOLDERID_Profile), written as %USERPROFILE%; "" means none.
	Profile string
	// MaxBytes and Keep are the rotation; 0 means DefaultMaxBytes and DefaultKeep.
	MaxBytes int64
	Keep     int
	// Mirror also gets every line (dev builds: stderr); nil means none.
	Mirror io.Writer
	// Clock names the crash files and dates last-exit.json; nil means time.Now.
	Clock func() time.Time
}

// Log is the open log. Its loggers are safe for concurrent use.
type Log struct {
	dir     string
	clock   func() time.Time
	out     *rotator
	handler slog.Handler // logx's JSON handler behind the redaction
}

// Open opens (appends to) isshoni.log in o.Dir.
func Open(o Options) (*Log, error) {
	if o.Dir == "" {
		return nil, errors.New("applog: Open needs a Dir")
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMaxBytes
	}
	if o.Keep <= 0 {
		o.Keep = DefaultKeep
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("applog: the logs folder can't be created: %w", pathless(err))
	}
	r := &rotator{dir: o.Dir, max: o.MaxBytes, keep: o.Keep, mirror: o.Mirror}
	if err := r.open(); err != nil {
		return nil, err
	}
	return &Log{dir: o.Dir, clock: o.Clock, out: r, handler: NewHandler(r, o.Level, o.Profile)}, nil
}

// NewHandler is the app's handler on any writer: logx's JSON lines at level (nil means Info) with the profile
// folder redacted. The e2e harness and tests use it without files.
func NewHandler(w io.Writer, level slog.Leveler, profile string) slog.Handler {
	var lv *slog.LevelVar
	switch l := level.(type) {
	case *slog.LevelVar:
		lv = l
	case nil:
	default:
		lv = new(slog.LevelVar)
		lv.Set(l.Level())
	}
	base := logx.New(logx.Options{Level: lv, Format: "json", Out: w}).Handler()
	return newRedactor(base, profile)
}

// Logger is the app's logger.
func (l *Log) Logger() *slog.Logger { return slog.New(l.handler) }

// WailsLogger is the Wails application's Options.Logger: the same file, never below Info (03 §17).
func (l *Log) WailsLogger() *slog.Logger { return slog.New(MinLevel(l.handler, slog.LevelInfo)) }

// Close closes the file. Loggers keep working afterwards and drop their lines.
func (l *Log) Close() error { return l.out.close() }

// MinLevel returns h with a floor: records below min are never handled, whatever h's own level allows.
func MinLevel(h slog.Handler, min slog.Level) slog.Handler { return &floor{h: h, min: min} }

type floor struct {
	h   slog.Handler
	min slog.Level
}

func (f *floor) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= f.min && f.h.Enabled(ctx, l)
}

func (f *floor) Handle(ctx context.Context, r slog.Record) error {
	if r.Level < f.min {
		return nil
	}
	return f.h.Handle(ctx, r)
}

func (f *floor) WithAttrs(as []slog.Attr) slog.Handler {
	return &floor{h: f.h.WithAttrs(as), min: f.min}
}
func (f *floor) WithGroup(name string) slog.Handler {
	return &floor{h: f.h.WithGroup(name), min: f.min}
}

// SetCrashOutput sends Go's crash output to a new crash-<UTC timestamp>.txt (03 §17). Empty crash files of earlier
// runs (runs that didn't crash) are removed first. It returns the new file's name.
func (l *Log) SetCrashOutput() (string, error) {
	l.removeEmptyCrashFiles()
	name := crashPrefix + l.clock().UTC().Format(crashLayout) + crashSuffix
	f, err := os.OpenFile(filepath.Join(l.dir, name), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return "", fmt.Errorf("applog: the crash file can't be created: %w", pathless(err))
	}
	defer func() { _ = f.Close() }() // SetCrashOutput keeps its own duplicate of the file
	if err := debug.SetCrashOutput(f, debug.CrashOptions{}); err != nil {
		return "", fmt.Errorf("applog: %w", err)
	}
	return name, nil
}

func (l *Log) removeEmptyCrashFiles() {
	for _, c := range l.crashFiles() {
		if c.size == 0 {
			_ = os.Remove(filepath.Join(l.dir, c.name))
		}
	}
}

type crashFile struct {
	name string
	at   time.Time
	size int64
}

// crashFiles lists the crash files, oldest first.
func (l *Log) crashFiles() []crashFile {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return nil
	}
	var out []crashFile
	for _, e := range entries {
		at, ok := crashTime(e.Name())
		if !ok || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, crashFile{name: e.Name(), at: at, size: info.Size()})
	}
	slices.SortFunc(out, func(a, b crashFile) int { return a.at.Compare(b.at) })
	return out
}

func crashTime(name string) (time.Time, bool) {
	ts, ok := strings.CutPrefix(name, crashPrefix)
	if !ok {
		return time.Time{}, false
	}
	ts, ok = strings.CutSuffix(ts, crashSuffix)
	if !ok {
		return time.Time{}, false
	}
	at, err := time.Parse(crashLayout, ts)
	return at, err == nil
}

// lastExit is last-exit.json.
type lastExit struct {
	Clean      bool      `json:"clean"`
	At         time.Time `json:"at"`
	CrashShown string    `json:"crashShown,omitempty"` // the crash file whose notice was shown
}

func (l *Log) readLastExit() (lastExit, bool) {
	data, err := os.ReadFile(filepath.Join(l.dir, LastExitName))
	if err != nil {
		return lastExit{}, false
	}
	var le lastExit
	if json.Unmarshal(data, &le) != nil {
		return lastExit{}, false
	}
	return le, true
}

func (l *Log) writeLastExit(le lastExit) error {
	data, err := json.Marshal(le)
	if err != nil {
		return err
	}
	path := filepath.Join(l.dir, LastExitName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("applog: %w", pathless(err))
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("applog: %w", pathless(err))
	}
	return nil
}

// FindCrash returns the newest crash file with content that is newer than the last clean exit and whose notice
// wasn't shown yet: "isshoni closed unexpectedly last time" (03 §17). Without a last-exit.json any such file counts.
func (l *Log) FindCrash() (name string, ok bool) {
	le, _ := l.readLastExit()
	files := l.crashFiles()
	for _, c := range slices.Backward(files) {
		if c.size == 0 {
			continue
		}
		if (le.Clean && !c.at.After(le.At)) || c.name == le.CrashShown {
			return "", false
		}
		return c.name, true
	}
	return "", false
}

// AckCrash records that the notice for crash file name was shown, so FindCrash doesn't return it again.
func (l *Log) AckCrash(name string) error {
	le, _ := l.readLastExit()
	le.CrashShown = name
	return l.writeLastExit(le)
}

// WriteCleanExit writes last-exit.json {"clean": true, "at": …}; Quit calls it (03 §17).
func (l *Log) WriteCleanExit() error {
	return l.writeLastExit(lastExit{Clean: true, At: l.clock().UTC()})
}

// rotator is the file writer: one line per Write (slog's handlers write a whole record at once), rotated before a
// line would take the file past max.
type rotator struct {
	dir    string
	max    int64
	keep   int
	mirror io.Writer

	mu     sync.Mutex
	f      *os.File
	size   int64
	closed bool
}

func (r *rotator) path(n int) string {
	if n == 0 {
		return filepath.Join(r.dir, FileName)
	}
	return filepath.Join(r.dir, fmt.Sprintf("isshoni.%d.log", n))
}

func (r *rotator) open() error {
	f, err := os.OpenFile(r.path(0), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("applog: the log file can't be opened: %w", pathless(err))
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("applog: %w", pathless(err))
	}
	r.f, r.size = f, info.Size()
	return nil
}

func (r *rotator) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.mirror != nil {
		_, _ = r.mirror.Write(p)
	}
	if r.closed || r.f == nil {
		return len(p), nil
	}
	if r.size > 0 && r.size+int64(len(p)) > r.max {
		if err := r.rotateLocked(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// rotateLocked closes the file (Windows renames no open file), shifts isshoni.N.log up by one, dropping the
// oldest, and starts a new isshoni.log.
func (r *rotator) rotateLocked() error {
	_ = r.f.Close()
	r.f = nil
	_ = os.Remove(r.path(r.keep))
	for n := r.keep - 1; n >= 0; n-- {
		if err := os.Rename(r.path(n), r.path(n+1)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			// Keep logging into the current file rather than losing lines.
			break
		}
	}
	return r.open()
}

func (r *rotator) close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

// pathless drops the path from a file-system error: the logs folder holds the Windows account name.
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
