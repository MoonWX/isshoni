//go:build windows || (darwin && cgo)

package main

import (
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync/atomic"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"github.com/MoonWX/isshoni/internal/desktop/app"
	"github.com/MoonWX/isshoni/internal/desktop/bridge"
	"github.com/MoonWX/isshoni/internal/desktop/tray"
)

var (
	_ app.Shell     = (*wailsShell)(nil)
	_ app.TrayShell = (*wailsShell)(nil)
	_ shellApp      = (*app.App)(nil)
)

// trayIcons are 03 §11.1's tray icons (build/tray/icons.go writes them): tray-idle, tray-live, tray-paused and
// tray-warn, .ico with 16 to 32 px images.
//
//go:embed build/tray/*.ico
var trayIcons embed.FS

// trayIcon is the icon file of id; the idle icon for an id without one.
func trayIcon(id tray.IconID) []byte {
	b, err := trayIcons.ReadFile("build/tray/" + string(id) + ".ico")
	if err != nil {
		b, _ = trayIcons.ReadFile("build/tray/" + string(tray.IconIdle) + ".ico")
	}
	return b
}

// shellApp is what the shell reports to internal/desktop/app (*app.App).
type shellApp interface {
	WindowClosing()
	WindowCreated()
	TrayClicked()
	TrayAction(act tray.Action)
	ToastAction(nonce, action string) bool
	Shutdown()
}

// wailsShell is internal/desktop/app's Shell over Wails (03 §4.4, §11.1, §15): one application with its main
// window, the tray icon for the app's whole life, the close hook (W7) and toasts (W8). The app it reports to is
// bound once it exists (bind); until then a close closes and the tray does nothing.
type wailsShell struct {
	app  *application.App
	main *application.WebviewWindow
	tray *application.SystemTray
	// openURL opens a URL that passed validation in the system browser; nil means Wails' Browser.OpenURL.
	openURL func(string) error
	notes   notifier
	nav     *navGuard // W5's fallback (navguard.go); nil until guardNavigation

	target   atomic.Pointer[shellApp]
	quitting atomic.Bool // Quit or Wails' shutdown began: a close is a close
}

// newShell creates the main window, the tray icon and the toasts on app; Wails shows them when Run starts.
func newShell(a *application.App, main application.WebviewWindowOptions, log *slog.Logger) *wailsShell {
	s := &wailsShell{app: a, main: a.Window.NewWithOptions(main), tray: a.SystemTray.New(), notes: newNotifier(log)}
	// W7: closing the window is the app's choice (hide to the tray or quit, 03 §11.6), off the UI thread.
	s.main.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		t := s.app0()
		if t == nil || s.quitting.Load() {
			return
		}
		e.Cancel()
		go t.WindowClosing()
	})
	// W10: the native handle exists; WDA_EXCLUDEFROMCAPTURE goes on it (03 §11.5). Both are idempotent.
	for _, ev := range []events.WindowEventType{events.Common.WindowRuntimeReady, events.Common.WindowShow} {
		s.main.OnWindowEvent(ev, func(*application.WindowEvent) {
			if t := s.app0(); t != nil {
				go t.WindowCreated()
			}
		})
	}
	// W18: the tray. A left click opens the window; a right click opens the menu the app keeps up to date.
	s.tray.SetIcon(trayIcon(tray.IconIdle))
	s.tray.SetTooltip("isshoni")
	s.tray.SetMenu(application.NewMenu())
	s.tray.OnClick(func() {
		if t := s.app0(); t != nil {
			go t.TrayClicked()
		}
	})
	s.tray.OnRightClick(s.tray.OpenMenu)
	// W14: a quit Wails began itself still stops the share first (and after the app's own Quit it does nothing).
	a.OnShutdown(func() {
		s.quitting.Store(true)
		if t := s.app0(); t != nil {
			t.Shutdown()
		}
		s.notes.stop()
	})
	return s
}

// bind gives the shell the app it reports to, and the toasts' actions their way back.
func (s *wailsShell) bind(t shellApp) {
	s.target.Store(&t)
	// Off Wails' notification callback (COM's activation thread): a Resume or Stop waits for the server.
	s.notes.onAction(func(nonce, action string) { go t.ToastAction(nonce, action) })
}

// app0 is the bound app, nil before bind.
func (s *wailsShell) app0() shellApp {
	if p := s.target.Load(); p != nil {
		return *p
	}
	return nil
}

// started is called once Wails runs: the toasts' service starts (it needs the running application).
func (s *wailsShell) started() error { return s.notes.start() }

// Run runs Wails until the app quits.
func (s *wailsShell) Run() error { return s.app.Run() }

func (s *wailsShell) ShowWindow(focus bool) {
	s.main.Show()
	if focus {
		s.main.Focus()
	}
}

func (s *wailsShell) HideWindow() { s.main.Hide() }

func (s *wailsShell) WindowVisible() bool { return s.main.IsVisible() }

// WindowFocused reports the main window in the foreground: shown, not minimised and focused. A minimised window, or
// one behind a full-screen game, is visible to Windows but not in front, so a mic prompt then becomes a toast (03 §15).
func (s *wailsShell) WindowFocused() bool {
	return s.main.IsVisible() && !s.main.IsMinimised() && s.main.IsFocused()
}

// The app asks the shell whether its window is in front (app's focusReporter).
var _ interface{ WindowFocused() bool } = (*wailsShell)(nil)

func (s *wailsShell) SetFullscreen(on bool) {
	if on {
		s.main.Fullscreen()
	} else {
		s.main.UnFullscreen()
	}
}

// Reload reloads the SPA at its current address. When the window was last seen off the asset origin (navguard.go),
// it loads the start page instead: Wails' Reload is location.reload() of whatever page is showing (beta.34).
func (s *wailsShell) Reload() {
	if s.nav != nil && !s.nav.atHome() {
		s.ReturnHome()
		return
	}
	s.main.Reload()
}

// The app's watchdog loads the start page through the shell (app's homeShell).
var _ interface{ ReturnHome() } = (*wailsShell)(nil)

// ReturnHome loads the start page: a navigation the browser starts, which works from any page, a foreign or hung one
// included (W5's and W17's fallbacks, 03 §3.2).
func (s *wailsShell) ReturnHome() { s.main.SetURL("/") }

// guardNavigation lets nav watch the main window (W5's fallback, 03 §3.2, §5.5): it asks the page where it is after
// every completed navigation (WebView2 only; WKWebView has no such event in Wails, and the macOS build is a dev
// build) and sends the window home from anywhere but the asset origin. Call it before Run, with nav's answers wired
// through navMessages.
func (s *wailsShell) guardNavigation(nav *navGuard) {
	s.nav = nav
	nav.attach(s.main.ExecJS, func() { go s.ReturnHome() })
	s.main.OnWindowEvent(events.Windows.WebViewNavigationCompleted, func(*application.WindowEvent) { nav.navigated() })
	s.main.OnWindowEvent(events.Common.WindowRuntimeReady, func(*application.WindowEvent) { nav.runtimeReady() })
}

// navMessages is Options.RawMessageHandler: the main window's answers to the guard's question, with the browser's
// top-level source (Wails passes every message without its "wails:" prefix here).
func navMessages(nav *navGuard) func(application.Window, string, *application.OriginInfo) {
	return func(w application.Window, msg string, info *application.OriginInfo) {
		if w == nil || w.Name() != mainWindowName {
			return
		}
		n, ok := whereReply(msg)
		if !ok {
			return
		}
		top := ""
		if info != nil {
			top = info.TopOrigin
		}
		nav.reported(n, top)
	}
}

// MainWindowHandle is the main window's HWND on Windows (NSWindow* on macOS), 0 before it exists (W10).
func (s *wailsShell) MainWindowHandle() uintptr { return uintptr(s.main.NativeWindow()) }

func (s *wailsShell) Emit(name string, data any) { s.app.Event.Emit(name, data) }

// OpenURL opens url in the system browser after 03 §5.5's check, whoever calls it: an absolute https URL, or in dev
// and e2e builds an http loopback URL; anything else (file:, javascript:, ms-settings:, search-ms:, UNC paths) is
// app.ErrInvalidArgument and nothing opens. Wails' own Browser.OpenURL check refuses far less (W21).
func (s *wailsShell) OpenURL(url string) error {
	if !app.ValidExternalURL(url, insecureLoopback()) {
		return fmt.Errorf("%w: not an https URL", app.ErrInvalidArgument)
	}
	open := s.openURL
	if open == nil {
		open = s.app.Browser.OpenURL
	}
	if err := open(url); err != nil {
		return fmt.Errorf("desktop: the browser didn't open: %w", err)
	}
	return nil
}

// Notify shows a toast through Wails' notifications service (03 §15, W8).
func (s *wailsShell) Notify(n app.Notification) error {
	if s.notes == nil {
		return errNoToasts
	}
	return s.notes.notify(n)
}

// errNoToasts is Notify on a shell without its notifier.
var errNoToasts = errors.New("desktop: no toasts")

// SetTray draws the app's tray view (W18): the icon, the tooltip and a new menu whose items report their action.
// Wails has no bold menu items, so the default item is drawn like the others.
func (s *wailsShell) SetTray(v tray.View) {
	s.tray.SetIcon(trayIcon(v.Icon))
	s.tray.SetTooltip(v.Tooltip)
	s.tray.SetMenu(trayMenu(v, func(act tray.Action) {
		if t := s.app0(); t != nil {
			go t.TrayAction(act)
		}
	}))
}

// trayMenu is v's menu; a chosen item calls do with its action.
func trayMenu(v tray.View, do func(tray.Action)) *application.Menu {
	m := application.NewMenu()
	for _, item := range v.Menu {
		if item.Separator {
			m.AddSeparator()
			continue
		}
		mi := m.Add(menuLabel(item.Label)).SetEnabled(item.Enabled)
		if act := item.Action; act != tray.ActionNone && item.Enabled {
			mi.OnClick(func(*application.Context) { do(act) })
		}
	}
	return m
}

// menuLabel keeps a label's "&" (a share label may have one) from becoming a Windows menu mnemonic.
func menuLabel(s string) string {
	if runtime.GOOS == "windows" {
		return strings.ReplaceAll(s, "&", "&&")
	}
	return s
}

// Quit quits Wails; its shutdown runs the app's Shutdown, which does nothing after the app's own Quit.
func (s *wailsShell) Quit() {
	s.quitting.Store(true)
	s.app.Quit()
}

// registerServices builds the bound services over d (internal/desktop/bridge, 03 §6.2) and registers each with
// Wails, between application.New and Run. The services' events go out through the shell's Emit. Each NewService call
// names its type, which is what `wails3 generate bindings` (task gen:bindings) reads: a service added to bridge needs
// its line here, or it gets no TypeScript and no binding. Wails' notifications service is deliberately not one of
// them: registered, its methods would be bound for page script; notify_windows.go starts it itself.
func registerServices(s *wailsShell, d bridge.Deps) (*bridge.Services, error) {
	if d.Emit == nil {
		d.Emit = s.Emit
	}
	if d.Shell == nil {
		d.Shell = s
	}
	svcs, err := bridge.New(d)
	if err != nil {
		return nil, err
	}
	s.app.RegisterService(application.NewService(svcs.App))
	s.app.RegisterService(application.NewService(svcs.Auth))
	s.app.RegisterService(application.NewService(svcs.Share))
	s.app.RegisterService(application.NewService(svcs.Audio))
	s.app.RegisterService(application.NewService(svcs.Settings))
	s.app.RegisterService(application.NewService(svcs.Window))
	s.app.RegisterService(application.NewService(svcs.Updates))
	return svcs, nil
}
