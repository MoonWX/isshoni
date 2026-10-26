package main

import (
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/desktop/app"
	"github.com/MoonWX/isshoni/internal/desktop/hotkey"
)

// recorder is the harness's recording Shell and OSHooks (03 §22.4 A): every call the app makes of the window, the
// tray, the browser, toasts and the OS is recorded, and GET /__e2e/state shows it. Its methods are safe for
// concurrent use.
type recorder struct {
	reloadDelay time.Duration // -reload-delay: Reload waits this long before it counts

	events chan app.SystemEvent // /__e2e/system
	keys   chan app.HotkeyID    // /__e2e/hotkey
	pill   *recordedPill
	emit   func(name string, data any) // the event stream (/__bridge/events)

	mu    sync.Mutex
	state hostState
}

// hostState is GET /__e2e/state's body. It names no token: an access token handed to the page is recorded as the
// origin it was asked for, the active origin at that moment and the first 8 bytes of its SHA-256 (03 §22.4 A).
type hostState struct {
	Origin          string             `json:"origin"`          // the asset origin the page is served at
	WindowVisible   bool               `json:"windowVisible"`   // ShowWindow / HideWindow
	ShowWindowCalls int                `json:"showWindowCalls"` // every ShowWindow
	Fullscreen      bool               `json:"fullscreen"`      // SetFullscreen
	Reloads         int                `json:"reloads"`         // Shell.Reload calls (after -reload-delay)
	OpenedURLs      []string           `json:"openedUrls"`      // Shell.OpenURL calls that passed its check
	RefusedURLs     int                `json:"refusedUrls"`     // Shell.OpenURL calls that didn't
	Toasts          []app.Notification `json:"toasts"`
	Quit            bool               `json:"quit"`
	Hotkeys         map[string]string  `json:"hotkeys"` // registered: id → combination
	KeepAwake       bool               `json:"keepAwake"`
	Pill            app.PillState      `json:"pill"`
	ExcludedWindows int                `json:"excludedWindows"` // ExcludeFromCapture calls
	Autostart       bool               `json:"autostart"`
	OSPages         []string           `json:"osPages"` // OpenSettingsPage calls
	Tokens          []tokenRecord      `json:"tokens"`
	RuntimeCalls    []runtimeCall      `json:"runtimeCalls"` // what reached the stub /wails/runtime behind the Guard
	Tray            any                `json:"tray"`         // the tray's view; null until the tray (README WA68)
}

// tokenRecord is one access token handed to the page.
type tokenRecord struct {
	Origin       string `json:"origin"`       // the origin the page asked for
	ActiveOrigin string `json:"activeOrigin"` // the active server's origin at that moment
	SHA256       string `json:"sha256"`       // the first 8 bytes of the token's SHA-256, hex
}

// runtimeCall is one request that reached the stub runtime handler: the object and method ids of 03 §3.2 W21.
type runtimeCall struct {
	Object int `json:"object"`
	Method int `json:"method"`
}

func newRecorder(origin string, reloadDelay time.Duration) *recorder {
	r := &recorder{
		reloadDelay: reloadDelay,
		events:      make(chan app.SystemEvent, 16),
		keys:        make(chan app.HotkeyID, 16),
		pill:        &recordedPill{actions: make(chan app.PillAction, 16)},
		emit:        func(string, any) {},
		state: hostState{
			Origin:        origin,
			WindowVisible: true,
			OpenedURLs:    []string{},
			Toasts:        []app.Notification{},
			Hotkeys:       map[string]string{},
			OSPages:       []string{},
			Tokens:        []tokenRecord{},
			RuntimeCalls:  []runtimeCall{},
		},
	}
	r.pill.r = r
	return r
}

// snapshot returns a copy of the state for /__e2e/state.
func (r *recorder) snapshot() hostState {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.state
	s.OpenedURLs = slices.Clone(s.OpenedURLs)
	s.Toasts = slices.Clone(s.Toasts)
	s.OSPages = slices.Clone(s.OSPages)
	s.Tokens = slices.Clone(s.Tokens)
	s.RuntimeCalls = slices.Clone(s.RuntimeCalls)
	hk := make(map[string]string, len(s.Hotkeys))
	for k, v := range s.Hotkeys {
		hk[k] = v
	}
	s.Hotkeys = hk
	return s
}

func (r *recorder) update(fn func(s *hostState)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(&r.state)
}

// ---- app.Shell ----

var _ app.Shell = (*recorder)(nil)

func (r *recorder) ShowWindow(bool) {
	r.update(func(s *hostState) { s.WindowVisible = true; s.ShowWindowCalls++ })
}

func (r *recorder) HideWindow() { r.update(func(s *hostState) { s.WindowVisible = false }) }

func (r *recorder) WindowVisible() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state.WindowVisible
}

func (r *recorder) SetFullscreen(on bool) { r.update(func(s *hostState) { s.Fullscreen = on }) }

// Reload counts a reload of the page after -reload-delay, so that a spec can act in the moment before it. The page
// in Chrome is the spec's: it reloads it itself when the count goes up.
func (r *recorder) Reload() {
	if r.reloadDelay > 0 {
		time.Sleep(r.reloadDelay)
	}
	r.update(func(s *hostState) { s.Reloads++ })
}

func (r *recorder) MainWindowHandle() uintptr { return 0 }

func (r *recorder) Emit(name string, data any) { r.emit(name, data) }

// OpenURL records url when it passes 03 §5.5's check of a dev or e2e build, which the real shell applies too
// (cmd/isshoni-desktop's wailsShell.OpenURL): nothing is opened here.
func (r *recorder) OpenURL(url string) error {
	if !app.ValidExternalURL(url, true) {
		r.update(func(s *hostState) { s.RefusedURLs++ })
		return fmt.Errorf("%w: not an https URL", app.ErrInvalidArgument)
	}
	r.update(func(s *hostState) { s.OpenedURLs = append(s.OpenedURLs, url) })
	return nil
}

func (r *recorder) Notify(n app.Notification) error {
	r.update(func(s *hostState) { s.Toasts = append(s.Toasts, n) })
	return nil
}

func (r *recorder) Quit() { r.update(func(s *hostState) { s.Quit = true }) }

// ---- app.OSHooks ----

var _ app.OSHooks = (*recorder)(nil)

// osBuild is what the harness reports as the Windows build (the fake engine's probe says the same).
const osBuild = 26100

func (r *recorder) OSBuild() int { return osBuild }

func (r *recorder) RegisterHotkey(id app.HotkeyID, k hotkey.Combo) error {
	r.update(func(s *hostState) { s.Hotkeys[string(id)] = k.String() })
	return nil
}

func (r *recorder) UnregisterHotkey(id app.HotkeyID) {
	r.update(func(s *hostState) { delete(s.Hotkeys, string(id)) })
}

func (r *recorder) Hotkeys() <-chan app.HotkeyID { return r.keys }

func (r *recorder) Pill() app.Pill { return r.pill }

func (r *recorder) ExcludeFromCapture(uintptr) error {
	r.update(func(s *hostState) { s.ExcludedWindows++ })
	return nil
}

func (r *recorder) SetAutostart(on bool, _ string) error {
	r.update(func(s *hostState) { s.Autostart = on })
	return nil
}

func (r *recorder) Autostart() (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state.Autostart, nil
}

func (r *recorder) OpenSettingsPage(p app.OSPage) error {
	r.update(func(s *hostState) { s.OSPages = append(s.OSPages, string(p)) })
	return nil
}

func (r *recorder) KeepAwake(on bool) { r.update(func(s *hostState) { s.KeepAwake = on }) }

func (r *recorder) Events() <-chan app.SystemEvent { return r.events }

// recordedPill is the pill's recording fake.
type recordedPill struct {
	r       *recorder
	actions chan app.PillAction
}

func (p *recordedPill) Set(st app.PillState) { p.r.update(func(s *hostState) { s.Pill = st }) }

func (p *recordedPill) Actions() <-chan app.PillAction { return p.actions }
