//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"

	"github.com/MoonWX/isshoni/internal/desktop/app"
)

// wailsNotifier is the toasts of 03 §15 over Wails' notifications service (W8). The service is started here, not
// registered with application.RegisterService: a registered service's methods are bound for page script, and the
// page must not send toasts. At beta.34 its Startup writes the HKCU keys of 03 §14.1 under the application's
// Options.Name (appID, so the toast AppID, the process AppUserModelID and 06's shortcut agree):
// Software\Classes\AppUserModelId\<Name> (CustomActivator = a GUID), Software\Classes\CLSID\{GUID}\LocalServer32 =
// "<exe>" %1 (a toast button pressed after the app exited starts isshoni.exe -Embedding) and, once a category is
// registered, Software\<Name>\NotificationCategories.
type wailsNotifier struct {
	log *slog.Logger
	ns  *notifications.NotificationService

	mu      sync.Mutex
	started bool
	fn      func(nonce, action string)
}

func newNotifier(log *slog.Logger) notifier {
	n := &wailsNotifier{log: log, ns: notifications.New()}
	n.ns.OnNotificationResponse(n.response)
	return n
}

// errNotStarted is a toast before the service started.
var errNotStarted = errors.New("desktop: the notifications service isn't running")

func (n *wailsNotifier) start() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.started {
		return nil
	}
	if err := n.ns.ServiceStartup(context.Background(), application.ServiceOptions{}); err != nil {
		return fmt.Errorf("desktop: the notifications service didn't start: %w", err)
	}
	n.started = true
	return nil
}

func (n *wailsNotifier) stop() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.started {
		return
	}
	n.started = false
	if err := n.ns.ServiceShutdown(); err != nil {
		n.log.Warn("desktop: the notifications service didn't stop cleanly", "err", err)
	}
}

func (n *wailsNotifier) onAction(fn func(nonce, action string)) {
	n.mu.Lock()
	n.fn = fn
	n.mu.Unlock()
}

// response is Wails' activation callback: a button or the toast's body.
func (n *wailsNotifier) response(r notifications.NotificationResult) {
	if r.Error != nil {
		n.log.Info("desktop: a toast activation that couldn't be read", "err", r.Error)
		return
	}
	nonce, action := activation(r.Response.ActionIdentifier, r.Response.UserInfo)
	n.mu.Lock()
	fn := n.fn
	n.mu.Unlock()
	if fn != nil {
		fn(nonce, action)
	}
}

func (n *wailsNotifier) notify(t app.Notification) error {
	n.mu.Lock()
	started := n.started
	n.mu.Unlock()
	if !started {
		return errNotStarted
	}
	opts := notifications.NotificationOptions{ID: t.ID, Title: t.Title, Body: t.Body}
	if t.Nonce != "" {
		opts.Data = map[string]any{nonceKey: t.Nonce}
	}
	if len(t.Actions) == 0 {
		return n.ns.SendNotification(opts)
	}
	cat := notifications.NotificationCategory{ID: toastCategory(t.Kind)}
	for _, a := range t.Actions {
		cat.Actions = append(cat.Actions, notifications.NotificationAction{ID: a.ID, Title: a.Title})
	}
	if err := n.ns.RegisterNotificationCategory(cat); err != nil {
		return fmt.Errorf("desktop: the toast's buttons: %w", err)
	}
	opts.CategoryID = cat.ID
	return n.ns.SendNotificationWithActions(opts)
}

// awaitToastActivation is -Embedding (03 §13.4 step 1, §15): COM started the app for a toast activation while no
// instance ran. It registers Wails' activator (the notifications service over an application that never runs) and
// waits at most timeout for the activation: true when it is open (the toast's body or its Open button), for which
// start starts the app in a fresh process (this one's Wails application is the activator's); every other action
// carries a nonce of a run that is gone and is dropped.
func awaitToastActivation(log *slog.Logger, timeout time.Duration) bool {
	_ = application.New(application.Options{Name: appID, Description: windowTitle, LogLevel: slog.LevelInfo})
	ns := notifications.New()
	got := make(chan string, 1)
	ns.OnNotificationResponse(func(r notifications.NotificationResult) {
		if r.Error != nil {
			return
		}
		_, action := activation(r.Response.ActionIdentifier, r.Response.UserInfo)
		select {
		case got <- action:
		default:
		}
	})
	if err := ns.ServiceStartup(context.Background(), application.ServiceOptions{}); err != nil {
		log.Warn("desktop: -Embedding: the toast activator didn't register", "err", err)
		return false
	}
	defer func() { _ = ns.ServiceShutdown() }()
	select {
	case action := <-got:
		if action == app.ToastOpen {
			return true
		}
		log.Info("desktop: a toast action of a run that is gone; dropped")
	case <-time.After(timeout):
		log.Info("desktop: -Embedding without an activation")
	}
	return false
}
