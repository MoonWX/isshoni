package app

import (
	"github.com/MoonWX/isshoni/internal/desktop/audio"
	"github.com/MoonWX/isshoni/internal/desktop/settings"
	"github.com/MoonWX/isshoni/internal/desktop/share"
	"github.com/MoonWX/isshoni/internal/update"
)

// What main hands to Deps fits the interfaces app declares for it: 05's audio service, with settings.json as its
// settings store, and 06's updater (README WA41, WA37, WA40). The wiring itself comes with WA77 and WA84. The share
// controller and C2 find 05's share calls and AudioConfig on Deps.Audio by type assertions (share.go), so these
// lines are what keeps a drifted signature from silently dropping them (README WA58).
var (
	_ AudioService        = (*audio.Service)(nil)
	_ Updater             = (*update.Updater)(nil)
	_ audio.SettingsStore = (*settings.AudioStore[audio.Settings])(nil)
	_ share.AudioPolicy   = (*audio.Service)(nil)
	_ audioConfigSource   = (*audio.Service)(nil)
)
