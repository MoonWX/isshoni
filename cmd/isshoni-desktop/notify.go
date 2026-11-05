//go:build windows || (darwin && cgo)

package main

import (
	"github.com/MoonWX/isshoni/internal/desktop/app"
)

// notifier is the shell's toasts (03 §15): Wails' notifications service on Windows (notify_windows.go), nothing on
// the macOS dev build (notify_darwin.go: the service needs a signed bundle there, and toasts are Windows' in M2).
type notifier interface {
	// start registers the toast activator (the HKCU keys of 03 §14.1) once Wails runs.
	start() error
	// notify shows a toast; one with buttons carries the app's nonce.
	notify(n app.Notification) error
	// onAction sets where activations go: the nonce the toast carried and the action (toastOpen for its body).
	onAction(fn func(nonce, action string))
	// stop saves the service's state (Wails' shutdown).
	stop()
}

// nonceKey is the key of the toast's data that carries app.Notification.Nonce.
const nonceKey = "nonce"

// toastCategory is the category of a toast kind's buttons: one per kind, so a kind's buttons are always the same.
func toastCategory(kind string) string { return appID + "." + kind }

// activation is an activation's nonce and action from Wails' response: the toast's body (Wails'
// DEFAULT_ACTION) is app.ToastOpen; a nonce that isn't a string is "".
func activation(actionID string, userInfo map[string]any) (nonce, action string) {
	nonce, _ = userInfo[nonceKey].(string)
	action = actionID
	if action == "" || action == defaultAction {
		action = app.ToastOpen
	}
	return nonce, action
}

// defaultAction is Wails' notifications.DefaultActionIdentifier: a click on the toast's body.
const defaultAction = "DEFAULT_ACTION"
