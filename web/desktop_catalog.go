package web

import (
	_ "embed"
	"slices"
)

// desktopCatalog is the desktop app's i18n namespace, the same file the SPA's desktop pages load
// (docs/m2/03-desktop-app.md §18.1). It is plain JSON, so the package still builds without Node.
//
//go:embed src/i18n/lazy/desktop.en.json
var desktopCatalog []byte

// DesktopCatalog returns src/i18n/lazy/desktop.en.json, the `desktop` namespace in English, as a copy the caller may
// keep. internal/desktop/i18n reads its desktop.native.* keys (tray, pill, toasts, native message boxes).
func DesktopCatalog() []byte { return slices.Clone(desktopCatalog) }
