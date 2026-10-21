// Command isshoni-desktop is the isshoni desktop app, shipped as isshoni.exe for Windows x64
// (docs/m2/03-desktop-app.md, "03" below): a Wails v3 shell around the bundled SPA (web.Dist()), the share sheet and
// share safety, with the app's logic in internal/desktop/* and the engine in isshoni-media.dll (docs/m2/04).
//
// This is the only package that imports Wails (03 D2, §4.5). It builds for Windows (CGO_ENABLED=0, the shipped mode)
// and for macOS with cgo (the dev build of 03 D3: `task desktop:dev`); everywhere else main_other.go prints that the
// app runs on Windows and macOS and exits 1, so `go build ./...` and `go vet ./...` work on every OS (06 §5.2, D18).
//
// Files:
//
//	main.go              the start (03 §13.4 step 7 so far: the env scrub, the WebView2 runtime check, the asset
//	                     handler, Wails, the window)
//	shell.go             the Wails shell the app drives (03 §6.1 Shell; minimal until 03 DK9)
//	webview2.go          the WebView2 runtime minimum and its message box text (03 §5.4)
//	platform_windows.go  Windows facts: the asset origin and its Guard, the WebView2 runtime check and profile
//	                     folder, the OS build
//	platform_darwin.go   macOS dev-build facts (WKWebView's Guard)
//	flavor_*.go          what the build tags production, dev and e2e change (03 §5.3, §5.4)
//	main_other.go        the stub of every other OS
//	build/windows/       icon.ico and app.manifest, which `task desktop:exe:windows` puts into the exe's resources
//	                     with syso.go (a //go:build ignore program built in the tools/wails3 module)
//	checklist_test.go    03 §3.2's Wails checklist in the real webview (-tags isshoni_hw: the owner's PC, a Mac)
//
// Build tags: production (every release, and Wails' own release mode: no DevTools), dev (`task desktop:dev`: Vite's
// dev server, http loopback servers) and e2e (the CDP build: http loopback servers, the WEBVIEW2_* variables kept).
// A release has exactly production (06 §5.2).
//
// Still to come, by README slice: the mode dispatch, single instance, the running mutex and the engine files
// (WA41), the bound services and their generated bindings (WA48), the tray and notifications (WA68).
package main
