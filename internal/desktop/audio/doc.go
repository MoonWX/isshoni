// Package audio is the desktop app's audio policy service (docs/m2/05-windows-audio.md §14): it decides what friends
// hear from this PC and shows it.
//
// The Service turns the user's settings (05 §14.1), the built-in data of internal/media/audiorules and what the app
// tells it (the shared window's process, a live share, pause, Share sound, browser tabs that play the user's isshoni
// viewer) into the engine's exclusion rules (media.Exclusions, the rules builder of 05 §14.3), and the engine's report
// into "What friends hear" (FriendsHear, 05 §14.5): the apps friends hear, the apps kept out and why, the mic prompts
// (05 §14.6), the banners and tips (05 §14.7) and the signals for outside the window. It polls the engine while the
// share sheet watches or a share is live (05 §14.4) and emits the events audio.report, audio.levels, audio.prompt and
// audio.alert, which 03's bridge forwards to the SPA as isshoni:audio.*.
//
// Who calls what:
//   - 03's bridge.Audio wraps the SPA-facing methods one to one: FriendsHear, AppRules, Watch, WatchLevels, Decide,
//     DismissPrompt, SetAppDecision, SetKeepOutMicUsers, DismissTip and SoundCheck. There is no sound-mode method:
//     the built-in voice apps can't be turned off, and Share sound is the only "off" (docs/m2/README.md §3 item 15).
//   - 03's App and share controller call the rest: AudioConfig (core.Options.AudioConfig), Exclusions, SetSharedPID
//     after a pick, OnShareStarted right before core.StartShare, OnShareStopped when the share ends, SetPaused,
//     SetShareSound, SetViewerBrowser on 01's EventPlaybackState, SetVisible, Subscribe.
//   - 03's main dispatches --sound-check-helper (SoundCheckHelperFlag) to RunSoundCheckHelper before Wails starts.
//
// The engine is a media.AudioController: the Windows engine of internal/media/native, or internal/media/fake's
// scripted scenarios on the Mac, in e2ehost on CI and in this package's tests. The service never sets a sound mode
// at start (AudioConfig's Mode is always media.AudioExcludeApps) and never touches the engine's pause: 01's core
// pauses and mutes the session (01 D14); SetPaused only updates FriendsHear.
//
// Privacy (05 §16): app ids, exe names, display names, paths, pids, rules, prompts, decisions and levels never leave
// the device and are never logged, at any level. Logs (component=audio) carry the engine, the state, the probe
// result, error codes and counts only.
//
// The package builds and tests on Linux, macOS and Windows: Windows-only code is in _windows.go files with stubs in
// _other.go (the ducking preference of 05 §14.7, the sound-check helper of 05 §11.3). It imports only the
// internal/media root, internal/media/audiorules and internal/logx from this repository (01 §4).
package audio
