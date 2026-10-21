// Package audiorules holds the built-in data of the audio exclusion policy (docs/m2/05-windows-audio.md §5.4):
//
//   - VoiceApps: the voice apps that are always kept out of a shared stream (source BUILTIN, locked; 05 §5.3 row 2).
//   - Launchers: launchers and shells. The app walk stops at them and the engine never makes one an include target,
//     so a game started from Steam is its own app (IM_RULE_STOP, 05 §5.2).
//   - SharedRuntimes: runtimes that belong to the app that started them, whatever folder they run from, such as
//     WebView2 (IM_RULE_SHARED_RUNTIME).
//   - Browsers: the browsers, grouped into the families of M1's client.browser value, which the viewer rule uses to
//     keep a browser out while it plays the user's isshoni viewer (IM_RULE_BROWSER, 05 §5.5, §5.9).
//
// The desktop audio service (internal/desktop/audio, 05 §14.3) turns this data into media rules; the desktop app's
// shell relaunch reads Launchers (03 §13.4). This package only holds the data: it matches no process and logs nothing.
//
// Every list has one field per desktop OS (OSNames). On Windows a name is an exe file name in lower case
// ("discord.exe"), the app identity the engine matches (05 §5.1): exe names, never paths, because Squirrel-updated
// apps move to a new folder on every update. The MacOS (M3) and Linux (M4) fields are empty in M2. A name appears in
// at most one list for its OS, msedgewebview2.exe is a shared runtime and not a browser, and isshoni's own exe is in
// no list (the service adds the self rules itself); the tests enforce all of it.
//
// The functions return fresh copies, so a caller can't change the built-in lists for anyone else: the built-in voice
// apps can't be turned off (docs/PLAN.md, 05 §2).
//
// Changing the data: change 05 §5.4 first, then this package and the mirror of §5.4 in its tests. Every built-in voice
// app gets a real-app check (05 §18.9); a failed check fixes the exe list or the folder and shared-runtime rules and
// never removes the app, and a voice app added after M2 needs its check before it ships.
//
// This package imports only the standard library (01 §4).
package audiorules
