package app

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/MoonWX/isshoni/internal/version"
)

// What the bound App service (03 §6.2, internal/desktop/bridge) asks of the app. The bridge maps the errors to its
// codes (03 §6.4): ErrInvalidArgument to invalid_argument, ErrBusy to busy, ErrNotImplemented to not_implemented.

var (
	// ErrInvalidArgument is a bad argument from page script.
	ErrInvalidArgument = errors.New("app: invalid argument")
	// ErrBusy is refused while a share is live.
	ErrBusy = errors.New("app: busy")
	// ErrNotImplemented is returned by the methods whose slices haven't landed yet (README §4 "Interfaces first").
	ErrNotImplemented = errors.New("app: not implemented yet")
)

// Info is App.Info()'s AppInfo (03 §6.2).
type Info struct {
	Version               string `json:"version"`
	OS                    string `json:"os"`
	OSBuild               int    `json:"osBuild"`
	Arch                  string `json:"arch"`
	PublisherConnectionID string `json:"publisherConnectionId"` // C2's connection id; "" while it isn't ready (WA58)
	WizardDone            bool   `json:"wizardDone"`
	Dev                   bool   `json:"dev"`
}

// Info describes the app. OS is the client OS name of internal/protocol ("windows"; the macOS dev build "macos").
func (a *App) Info() Info {
	return Info{
		Version:    version.Version(),
		OS:         clientOS(),
		OSBuild:    a.d.OS.OSBuild(),
		Arch:       runtime.GOARCH,
		WizardDone: a.d.Settings.WizardDone(),
		Dev:        a.d.InsecureLoopback,
	}
}

func clientOS() string {
	if runtime.GOOS == "darwin" {
		return "macos"
	}
	return runtime.GOOS
}

// Heartbeat is the SPA's watchdog beat (W17's fallback, 03 §20): every 5 s. The watchdog itself is README WA68's.
func (a *App) Heartbeat() {}

// Live reports a live share and its room (03 §11.6's "while live"): the bridge answers busy for what a live share
// forbids. The share controller fills it (README WA65); until then nothing is ever live.
func (a *App) Live() (roomID string, live bool) { return a.sharing() }

// OpenExternal opens url in the system browser after 03 §5.5's check (ValidExternalURL); ErrInvalidArgument
// otherwise, and nothing is opened.
func (a *App) OpenExternal(raw string) error {
	if !ValidExternalURL(raw, a.d.InsecureLoopback) {
		return fmt.Errorf("%w: not an https URL", ErrInvalidArgument)
	}
	return a.d.Shell.OpenURL(raw)
}

// maxExternalURL bounds an external URL.
const maxExternalURL = 8 << 10

// ValidExternalURL is 03 §5.5's rule for App.OpenExternal and Shell.OpenURL: an absolute https: URL with a host and
// no user info, or, in dev and e2e builds (dev), an http: URL whose host is localhost, a .localhost name or a
// loopback IP. Anything else is refused: file:, javascript:, ms-settings:, search-ms: and every other scheme, UNC
// paths, backslashes, spaces and control characters.
func ValidExternalURL(raw string, dev bool) bool {
	if raw == "" || len(raw) > maxExternalURL || strings.ContainsAny(raw, `\`) {
		return false
	}
	for _, r := range raw {
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.In(r, unicode.Bidi_Control) {
			return false
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil || u.Host == "" || u.Hostname() == "" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return true
	case "http":
		h := strings.ToLower(u.Hostname())
		return dev && (h == "localhost" || strings.HasSuffix(h, ".localhost") || h == "127.0.0.1" || h == "::1")
	}
	return false
}

// OpenOSSettings opens a Windows settings page of the fixed list (03 §6.2); ErrInvalidArgument for another page.
func (a *App) OpenOSSettings(p OSPage) error {
	if !p.Valid() {
		return fmt.Errorf("%w: not a settings page", ErrInvalidArgument)
	}
	return a.d.OS.OpenSettingsPage(p)
}

// OpenLogsFolder opens Explorer at the logs folder (03 §6.2); README WA68 adds the shell open.
func (a *App) OpenLogsFolder() error { return ErrNotImplemented }

// OpenNotices opens <exe dir>\licenses\THIRD_PARTY_NOTICES.txt (03 §6.2, 06 §2.2); README WA68 adds it.
func (a *App) OpenNotices() error { return ErrNotImplemented }

// MaxOwnConnections bounds App.OwnConnections (03 §20).
const MaxOwnConnections = 64

// OwnConnections takes the online connection ids of C1's user.connections, which the SPA sends on every change; a
// playback.state sender that isn't among them is expired (03 §12.2, README WA77). More than 64 ids, or an id that
// isn't a connection id's shape, is ErrInvalidArgument.
func (a *App) OwnConnections(ids []string) error {
	if len(ids) > MaxOwnConnections {
		return fmt.Errorf("%w: more than %d connections", ErrInvalidArgument, MaxOwnConnections)
	}
	for _, id := range ids {
		if !validID(id) {
			return fmt.Errorf("%w: not a connection id", ErrInvalidArgument)
		}
	}
	a.mu.Lock()
	a.ownConns = append(a.ownConns[:0], ids...)
	a.mu.Unlock()
	return nil
}

// validID reports an opaque id of the protocol (01 §4.1): 1–64 of [A-Za-z0-9_-].
func validID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for i := range len(id) {
		switch c := id[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// The SPA's log lines (03 §6.2 App.Log, §17).
const (
	spaLogPerSecond = 10
	spaLogMaxMsg    = 1 << 10
	spaLogMaxAttrs  = 16
	spaLogMaxKey    = 64
	spaLogMaxValue  = 256
	spaDropReport   = time.Minute
)

// spaLog is App.Log's rate limit: at most 10 lines a second; the excess is dropped, counted and reported in one line
// per minute.
type spaLog struct {
	mu         sync.Mutex
	second     time.Time
	inSecond   int
	dropped    int
	lastReport time.Time
}

// Log writes a line the SPA sent (lib/log.ts forwards warn and error) with component=spa: level debug, info, warn or
// error (another level is info); the message cut to 1 KiB, at most 16 attributes cut to 256 bytes each, in a group
// "attrs" so that no attribute of the page can stand in for the line's own time, level, msg or component. The
// SPA's own code never passes an app name, window title or share label (03 §17).
func (a *App) Log(level, msg string, attrs map[string]string) {
	now := a.clock()
	l := &a.spaLog
	l.mu.Lock()
	if now.Sub(l.second) >= time.Second || now.Before(l.second) {
		l.second, l.inSecond = now, 0
	}
	if l.inSecond >= spaLogPerSecond {
		l.dropped++
		l.mu.Unlock()
		return
	}
	l.inSecond++
	dropped := 0
	if l.dropped > 0 && now.Sub(l.lastReport) >= spaDropReport {
		dropped, l.dropped, l.lastReport = l.dropped, 0, now
	}
	l.mu.Unlock()

	if dropped > 0 {
		a.spa.Warn("spa: messages dropped", "count", dropped)
	}
	lv := slog.LevelInfo
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	}
	var args []any
	if len(attrs) > 0 {
		group := make([]any, 0, min(len(attrs), spaLogMaxAttrs))
		seen := make(map[string]bool, min(len(attrs), spaLogMaxAttrs))
		for k, v := range attrs {
			if len(group) == spaLogMaxAttrs {
				break
			}
			k = cut(k, spaLogMaxKey)
			if seen[k] { // two long keys that differ only after the cut
				continue
			}
			seen[k] = true
			group = append(group, slog.String(k, cut(v, spaLogMaxValue)))
		}
		args = []any{slog.Group("attrs", group...)}
	}
	a.spa.Log(a.ctx, lv, cut(msg, spaLogMaxMsg), args...)
}

// cut shortens s to at most n bytes, on a rune boundary.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }
