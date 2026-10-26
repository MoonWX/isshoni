//go:build windows || (darwin && cgo)

package main

import (
	"errors"
	"fmt"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/MoonWX/isshoni/internal/desktop/app"
	"github.com/MoonWX/isshoni/internal/desktop/bridge"
)

// errNotImplemented is returned by the Shell methods whose slices have not landed yet.
var errNotImplemented = errors.New("desktop: not implemented yet")

var _ app.Shell = (*wailsShell)(nil)

// wailsShell is internal/desktop/app's Shell over Wails: one application with its main window (03 §4.4). Notify and
// the tray come with WA68.
type wailsShell struct {
	app  *application.App
	main *application.WebviewWindow
	// openURL opens a URL that passed validation in the system browser; nil means Wails' Browser.OpenURL.
	openURL func(string) error
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

func (s *wailsShell) Notify(app.Notification) error { return errNotImplemented }

func (s *wailsShell) Quit() { s.app.Quit() }

// registerServices builds the bound services over d (internal/desktop/bridge, 03 §6.2) and registers each with
// Wails, between application.New and Run. The services' events go out through the shell's Emit. Each NewService call
// names its type, which is what `wails3 generate bindings` (task gen:bindings) reads: a service added to bridge needs
// its line here, or it gets no TypeScript and no binding.
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
