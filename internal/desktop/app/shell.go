package app

// Shell is what the app needs from the Wails shell (03 §6.1): cmd/isshoni-desktop implements it over Wails, the e2e
// harness with a recording fake. Its methods may be called from any goroutine once the Wails application runs.
//
// The tray (SetTray with internal/desktop/tray's View) joins this interface with the tray package (README WA68).
type Shell interface {
	ShowWindow(focus bool) // creates it first after --autostart
	HideWindow()
	WindowVisible() bool
	SetFullscreen(on bool)
	Reload() // reloads the SPA (every change of the active server, crash recovery)
	MainWindowHandle() uintptr
	Emit(name string, data any) // Wails event to the SPA
	// OpenURL opens the system browser. It validates with App.OpenExternal's rule itself (03 §5.5): absolute https,
	// http loopback only in dev and e2e builds, else invalid_argument (file:, ms-settings:, search-ms:, javascript:
	// and UNC paths are refused whoever calls it).
	OpenURL(url string) error
	Notify(n Notification) error
	Quit()
}

// Notification is one Windows toast (03 §15). Toasts with buttons carry a random 128-bit nonce, and the app accepts
// an action only for a nonce of a toast it sent in this run and hasn't settled (README WA68 builds them).
type Notification struct {
	ID      string
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
