package app

import "github.com/MoonWX/isshoni/internal/desktop/tray"

// Shell is what the app needs from the Wails shell (03 §6.1): cmd/isshoni-desktop implements it over Wails, the e2e
// harness with a recording fake. Its methods may be called from any goroutine once the Wails application runs.
//
// The tray is TrayShell, which a shell implements beside this interface (03 §6.1 lists SetTray here): a Shell without
// it, such as an older recording fake, gets no tray. The shell reports back through the App's methods: WindowClosing
// (its close hook, W7), WindowCreated (the main window's HWND exists), TrayClicked and TrayAction (W18),
// ToastAction (a toast's button or body, W8), Shutdown (Wails quits on its own, W14) and, from the page, Heartbeat
// (W17's watchdog).
type Shell interface {
	ShowWindow(focus bool) // creates it first after --autostart
	HideWindow()
	WindowVisible() bool
	SetFullscreen(on bool)
	// Reload reloads the SPA at the window's current address (every change of the active server, crash recovery); a
	// window that isn't showing the asset origin gets the start page instead (W5's fallback, 03 §3.2).
	Reload()
	MainWindowHandle() uintptr
	Emit(name string, data any) // Wails event to the SPA
	// OpenURL opens the system browser. It validates with App.OpenExternal's rule itself (03 §5.5): absolute https,
	// http loopback only in dev and e2e builds, else invalid_argument (file:, ms-settings:, search-ms:, javascript:
	// and UNC paths are refused whoever calls it).
	OpenURL(url string) error
	Notify(n Notification) error
	Quit()
}

// TrayShell is the shell's tray (03 §11.1, W18): the icon, tooltip and menu of v, drawn at once. The app calls it
// at most every 250 ms, and only with a view that differs from the last one.
type TrayShell interface {
	SetTray(v tray.View)
}

// Notification is one Windows toast (03 §15). Toasts with buttons carry a random 128-bit nonce, and the app accepts
// an action only for a nonce of a toast it sent in this run and hasn't settled (toasts.go); the shell hands the
// nonce and the action back through App.ToastAction.
type Notification struct {
	ID      string // unique in this run
	Kind    string // the toast's kind (ToastStillSharing, …): the shell's category for its buttons
	Title   string
	Body    string
	Actions []NotificationAction
	Nonce   string // hex, 32 characters; "" for a toast without buttons
}

// NotificationAction is one toast button.
type NotificationAction struct {
	ID    string // the action ("keep-out", "include", "open", "resume", "stop", "restart")
	Title string // from the catalog (desktop.native.toast.*)
}

// The events the app emits to the SPA (03 §6.3), each with the "isshoni:" prefix.
const (
	EventDeepLinkOpen = "isshoni:deeplink.open" // PendingLink
)
