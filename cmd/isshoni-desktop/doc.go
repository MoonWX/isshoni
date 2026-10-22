// Command isshoni-desktop is the isshoni desktop app, shipped as isshoni.exe for Windows x64
// (docs/m2/03-desktop-app.md, "03" below): a Wails v3 shell around the bundled SPA (web.Dist()), the share sheet and
// share safety, with the app's logic in internal/desktop/* and the engine in isshoni-media.dll (docs/m2/04).
//
// This is the only package that imports Wails (03 D2, §4.5). It builds for Windows (CGO_ENABLED=0, the shipped mode)
// and for macOS with cgo (the dev build of 03 D3: `task desktop:dev`); everywhere else main_other.go prints that the
// app runs on Windows and macOS and exits 1, so `go build ./...` and `go vet ./...` work on every OS (06 §5.2, D18).
//
// main handles a start in 03 §13.4's order: the first action (the DLL search order), then step 0
// (--sound-check-helper), the one-shot modes (--version so far), single instance, the running mutex, the Pending
// values and the app. Steps 2, 2a and 5 and the other one-shot modes are marked in start.go where their slices put
// them.
//
// Files:
//
//	main.go              main, the real OS parts of the order, and step 7: the env scrub, the AppUserModelID, the
//	                     WebView2 runtime check, logs, servers, settings, the engine, the asset handler, Wails and
//	                     the app (internal/desktop/app), which gets the command line, the Pending values and every
//	                     second launch's forwarded arguments
//	start.go             03 §13.4's order with its OS parts behind functions (tested with fakes), --version
//	firstaction_*.go     main's first action (SetDefaultDllDirectories, SetDllDirectory) and --version's stdout
//	engine_release.go    the engine of every release: native.Open (01 §7.1)
//	engine_fake.go       the engine of dev and e2e builds: 01's fake with 05's audio scenarios (03 §4.5, §22.4)
//	shell.go             internal/desktop/app's Shell over Wails (03 §6.1; the tray and toasts come with WA68)
//	webview2.go          the WebView2 runtime minimum and its message box text (03 §5.4)
//	platform_windows.go  Windows facts: the asset origin and its Guard, the data root and the profile folder from
//	                     the shell's known folders, the WebView2 profile folder, the OS build
//	platform_darwin.go   macOS dev-build facts (WKWebView's Guard, the dev data folder)
//	flavor_*.go          what the build tags production, dev and e2e change (03 §5.3, §5.4)
//	main_other.go        the stub of every other OS
//	build/windows/       icon.ico and app.manifest, which `task desktop:exe:windows` puts into the exe's resources
//	                     with syso.go (a //go:build ignore program built in the tools/wails3 module)
//	checklist_test.go    03 §3.2's Wails checklist in the real webview (-tags isshoni_hw: the owner's PC, a Mac)
//	main_windows_test.go 03 §22.2's Windows tests of main: the DLL search-order marker, single instance (W6), the
//	                     running mutex and --version of the real exe
//
// Build tags: production (every release, and Wails' own release mode: no DevTools), dev (`task desktop:dev`: Vite's
// dev server, http loopback servers, the fake engine) and e2e (the CDP build: http loopback servers, the WEBVIEW2_*
// variables kept, the fake engine). A release has exactly production (06 §5.2).
//
// Still to come, by README slice: the bound services and their generated bindings (WA48), the tray and
// notifications (WA68), the one-shot modes --self-check, --forget-credentials and --purge-data with the installer
// handover (WA59), the shell relaunch and --sound-check-helper (WA77), the start-time update (WA84).
package main
