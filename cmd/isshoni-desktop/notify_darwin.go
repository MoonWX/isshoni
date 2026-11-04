//go:build darwin && cgo

package main

import (
	"log/slog"
	"time"

	"github.com/MoonWX/isshoni/internal/desktop/app"
)

// logNotifier is the macOS dev build's toasts: logged at debug (the kind only; a toast's text may name an app), never
// shown. Wails' macOS notifications need a signed bundle, which `task desktop:dev` doesn't build (M3, 03 §21).
type logNotifier struct{ log *slog.Logger }

func newNotifier(log *slog.Logger) notifier { return logNotifier{log: log} }

func (logNotifier) start() error                        { return nil }
func (logNotifier) onAction(func(nonce, action string)) {}
func (logNotifier) stop()                               {}

func (n logNotifier) notify(t app.Notification) error {
	n.log.Debug("desktop: a toast (not shown on macOS)", "kind", t.Kind, "buttons", len(t.Actions))
	return nil
}

// awaitToastActivation is -Embedding's wait on macOS, where no toast activates the app: nothing to open.
func awaitToastActivation(*slog.Logger, time.Duration) bool { return false }
