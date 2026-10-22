//go:build windows || (darwin && cgo)

package main

import (
	"errors"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/MoonWX/isshoni/internal/desktop/app"
)

// errNotImplemented is returned by the Shell methods whose slices have not landed yet.
var errNotImplemented = errors.New("desktop: not implemented yet")

var _ app.Shell = (*wailsShell)(nil)

// wailsShell is internal/desktop/app's Shell over Wails: one application with its main window (03 §4.4). OpenURL's
// validated system-browser open comes with the bridge (README WA48), Notify and the tray with WA68.
type wailsShell struct {
	app  *application.App
	main *application.WebviewWindow
}

// newShell creates the main window on app; Wails shows it when Run starts.
func newShell(app *application.App, main application.WebviewWindowOptions) *wailsShell {
	return &wailsShell{app: app, main: app.Window.NewWithOptions(main)}
}

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

func (s *wailsShell) SetFullscreen(on bool) {
	if on {
		s.main.Fullscreen()
	} else {
		s.main.UnFullscreen()
	}
}

func (s *wailsShell) Reload() { s.main.Reload() }

// MainWindowHandle is the main window's HWND on Windows (NSWindow* on macOS), 0 before it exists (W10).
func (s *wailsShell) MainWindowHandle() uintptr { return uintptr(s.main.NativeWindow()) }

func (s *wailsShell) Emit(name string, data any) { s.app.Event.Emit(name, data) }

func (s *wailsShell) OpenURL(string) error { return errNotImplemented }

func (s *wailsShell) Notify(app.Notification) error { return errNotImplemented }

func (s *wailsShell) Quit() { s.app.Quit() }
