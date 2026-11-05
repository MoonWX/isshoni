package app

import (
	"github.com/MoonWX/isshoni/internal/desktop/audio"
	"github.com/MoonWX/isshoni/internal/desktop/settings"
	"github.com/MoonWX/isshoni/internal/desktop/share"
	"github.com/MoonWX/isshoni/internal/update"
)

// What main hands to Deps fits the interfaces app declares for it: 05's audio service, with settings.json as its
// settings store, and 06's updater (README WA41, WA37, WA40). The share controller and C2 find 05's share calls and
// AudioConfig on Deps.Audio by type assertions (share.go), and the app its window, prompt and viewer calls
// (window.go, audio.go), so these lines are what keeps a drifted signature from silently dropping them (README WA58,
// WA77).
var (
	_ AudioService        = (*audio.Service)(nil)
	_ Updater             = (*update.Updater)(nil)
	_ audio.SettingsStore = (*settings.AudioStore[audio.Settings])(nil)
	_ share.AudioPolicy   = (*audio.Service)(nil)
	_ audioConfigSource   = (*audio.Service)(nil)
	_ visibility          = (*audio.Service)(nil)
	_ viewerPolicy        = (*audio.Service)(nil)
	_ audioEvents         = (*audio.Service)(nil)
	_ promptDecider       = (*audio.Service)(nil)
)
