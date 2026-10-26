package bridge

import (
	"errors"
	"log/slog"

	"github.com/MoonWX/isshoni/internal/desktop/app"
	"github.com/MoonWX/isshoni/internal/desktop/bridge/codes"
)

// App is the bound App service (03 §6.2): the app's facts, the external browser, OS settings pages, Quit, the SPA's
// log lines, the pending deep link and C1's connection list.
type App struct {
	core Core
	log  *slog.Logger
}

// AppInfo is App.Info()'s answer: version, os ("windows", the macOS dev build "macos"), osBuild, arch,
// publisherConnectionId (C2's connection id, "" while it isn't ready), wizardDone, dev.
type AppInfo = app.Info

// PendingLink is the deeplink.open payload and App.PendingLink()'s answer (03 §6.3, §13.2).
type PendingLink = app.PendingLink

// Info describes the app (03 §6.2).
func (s *App) Info() AppInfo { return s.core.Info() }

// Heartbeat is the SPA's 5 s watchdog beat (W17's fallback).
func (s *App) Heartbeat() { s.core.Heartbeat() }

// OpenExternal opens url in the system browser: only an absolute https URL (and an http loopback URL in dev and e2e
// builds), else invalid_argument and nothing opens (03 §5.5).
func (s *App) OpenExternal(url string) error {
	if !app.ValidExternalURL(url, s.core.Info().Dev) {
		return invalidArgument("url")
	}
	return mapError(s.core.OpenExternal(url), s.log)
}

// OpenOSSettings opens a Windows settings page of the fixed list: notifications, dnd, graphics-capture, sound,
// sound-communications, taskbar (03 §6.2); invalid_argument for anything else.
func (s *App) OpenOSSettings(page string) error {
	p := app.OSPage(page)
	if !p.Valid() {
		return invalidArgument("page")
	}
	return mapError(s.core.OpenOSSettings(p), s.log)
}

// Quit quits the app (03 §11.6); busy while a share is live and stopShare is false.
func (s *App) Quit(stopShare bool) error {
	if _, live := s.core.Live(); live && !stopShare {
		return newError(codes.CodeBusy, nil)
	}
	s.core.Quit(stopShare)
	return nil
}

// Log writes one of the SPA's log lines (03 §17): level debug, info, warn or error, at most 1 KiB of message and 16
// attributes of 256 bytes, at most 10 lines a second (the app cuts and counts the rest).
func (s *App) Log(level, msg string, attrs map[string]string) { s.core.Log(level, msg, attrs) }

// OpenLogsFolder opens Explorer at the logs folder (io_error when it can't).
func (s *App) OpenLogsFolder() error { return mapError(s.core.OpenLogsFolder(), s.log) }

// OpenNotices opens the installed THIRD_PARTY_NOTICES.txt (io_error when it can't).
func (s *App) OpenNotices() error { return mapError(s.core.OpenNotices(), s.log) }

// PendingLink returns the deep link not yet acted on (03 §13.2), null when there is none.
func (s *App) PendingLink() *PendingLink { return s.core.PendingLink() }

// LinkHandled clears the pending link: the SPA navigated to the room, or the add question was answered.
func (s *App) LinkHandled() { s.core.LinkHandled() }

// OwnConnections takes the online connection ids of C1's user.connections (at most 64), sent by the SPA on every
// change (03 §12.2); invalid_argument for more or for a malformed id.
func (s *App) OwnConnections(ids []string) error {
	err := s.core.OwnConnections(ids)
	if errors.Is(err, app.ErrInvalidArgument) {
		return invalidArgument("ids")
	}
	return mapError(err, s.log)
}
