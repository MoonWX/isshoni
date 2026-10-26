// Package share is the desktop app's share controller (docs/m2/03-desktop-app.md, "03" below, §10.1): the single
// owner of the native share. The SPA (through the bound Share service of internal/desktop/bridge), the tray, the
// hotkeys and the pill all call it; it calls only 01's core (Client.StartShare and the core.Share methods: the core
// joins and leaves the room, sends share.start and share.stop and runs the engine), the engine's picker (Pick and
// Release) and 05's audio policy.
//
//	         Pick()                      Start(pickId, roomId, preset, withAudio)       01: State live
//	idle ─────────► picking ──picked──► picked ───────────────────────────────► starting ─────────────────► live
//	  ▲               │ cancel            │ Release / 300 s                         │ error                 │  ▲
//	  │               ▼                   ▼                                         ▼                       │  │ SetPaused
//	  ├─────────────── idle ◄──────────── idle                                    failed ──(dismiss)──► idle │  ▼
//	  │                                                                                                    paused
//	  │     C2 down or pub PC recovering: live|paused ⇄ reconnecting ⇄ unreachable (01; 30 s hold)           │
//	  └──── stopping ◄── Stop(): SPA, tray, hotkey, pill · source_gone · server ended · SPA left the room ·  ─┘
//	                     sign out · sleep · end of session · Quit
//
// The files:
//   - share.go: the controller's types (View, Picked, Stats, the events), its errors and the interfaces it consumes:
//     the engine's picker, C2 (Publisher, Client and Share over 01's core; FromCore adapts *core.Client), 05's
//     audio policy, the window and settings.json;
//   - controller.go: the Controller: Pick and the held pick (300 s, one at a time), Start (C2 ready within 10 s,
//     then 05's OnShareStarted, then StartShare with its own 10 s), Stop (idempotent, with its cause), pause,
//     preset, Share sound, StopOther, stats, the share.state events coalesced to at most 10 a second, and 01's
//     reconnecting shown only once it lasted ReconnectingAfter (a short drop that 01 cures doesn't flash);
//   - mapping.go: 03 §10.1's two tables, 01's ShareState → View.State and EndInfo → EndReason, and the snapshot's
//     other fields.
//
// The controller is pure Go over interfaces, so it builds and is tested with testing/synctest on every OS. What later
// slices add (docs/m2/README.md §5): the SharedPID hand-off to 05 at pick time (WA77), keep-awake and the tray
// (WA68), the hotkeys, the pill and the lock pause (WA80). Nothing it logs names a source, window, app or share label
// (03 §17): labels stay in View.Label, on this PC.
package share
