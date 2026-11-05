//go:build (windows || (darwin && cgo)) && !isshoni_hw

package main

import (
	"log/slog"
	"testing"

	"github.com/MoonWX/isshoni/internal/desktop/app"
)

// TestDeferredShellOpensNoWindow (03 §16.1, README WA84): the shell of an --autostart start creates no window, so no
// SPA runs; until the user opens one, the shell's window calls do nothing and report no window. (The real exe's
// round trip is autostart_windows_test.go's.)
func TestDeferredShellOpensNoWindow(t *testing.T) {
	a := testWailsApp()
	before := len(a.Window.GetAll())
	s := newDeferredShell(a, mainWindowOptions(), slog.New(slog.DiscardHandler))
	if got := len(a.Window.GetAll()); got != before {
		t.Fatalf("%d windows after the deferred shell, %d before", got, before)
	}
	if s.window() != nil || s.MainWindowHandle() != 0 || s.WindowVisible() {
		t.Fatal("the deferred shell has a window")
	}
	if f, ok := app.Shell(s).(interface{ WindowFocused() bool }); !ok || f.WindowFocused() {
		t.Fatal("a window in front without one")
	}
	// Nothing to hide, reload, put full screen or send home: none of them creates the window.
	s.HideWindow()
	s.Reload()
	s.SetFullscreen(true)
	s.ReturnHome()
	s.guardNavigation(newNavGuard(assetOrigin, slog.New(slog.DiscardHandler)))
	if s.window() != nil || len(a.Window.GetAll()) != before {
		t.Fatal("a window call created the window")
	}
}

// TestAutostartArgs: --autostart is what the Run value starts (winos.RunCommand) and what ParseArgs reads as the
// start at sign-in, which the app's HandleArgs answers without showing the window (03 §16.1).
func TestAutostartArgs(t *testing.T) {
	if p := app.ParseArgs([]string{"--autostart"}); !p.Autostart || p.OneShot != "" {
		t.Fatalf("ParseArgs(--autostart) = %+v", p)
	}
}
