// Command isshoni-desktop is the isshoni desktop app, shipped as isshoni.exe for Windows x64
// (docs/m2/03-desktop-app.md, "03" below): a Wails v3 shell around the bundled SPA (web.Dist()), the share sheet and
// share safety, with the app's logic in internal/desktop/* and the engine in isshoni-media.dll (docs/m2/04).
//
// This is the only package that imports Wails (03 D2, §4.5). It builds for Windows (CGO_ENABLED=0, the shipped mode)
// and for macOS with cgo (the dev build of 03 D3: `task desktop:dev`); everywhere else main_other.go prints that the
// app runs on Windows and macOS and exits 1, so `go build ./...` and `go vet ./...` work on every OS (06 §5.2, D18).
//
// main handles a start in 03 §13.4's order: the first action (the DLL search order), then step 0
// (--sound-check-helper: 05's helper plays the sound check's chime and exits, before anything else), the one-shot
// modes (--version, --self-check=<path>, --forget-credentials, --purge-data, -Embedding, whose toast activation
// starts the app afresh in a new process only when it is the toast's Open), step 2 (the shell relaunch, D16: a start
// whose parent isn't Explorer or a launcher starts again under the user's shell, with --relaunched first, and exits;
// skipped with --relaunched and while the running mutex exists), step 2a (an installer running: a link start leaves
// its link in PendingServer/PendingRoom and every start exits), single instance, the running mutex, step 5 (the
// first instance builds 06's updater over settings.json and applies a pending update before any window opens, the
// link of this start written to PendingServer/PendingRoom first; never at an --autostart start), the Pending values
// and the app, with 05's audio service over the engine and 06's updater. An --autostart start creates no window until
// the user opens one (03 §16.1). --update-now is a one-shot mode of builds with the updatetest tag (06 §8.11).
//
// Files:
//
//	main.go              main, the real OS parts of the order, and step 7: the env scrub, the AppUserModelID, the
//	                     WebView2 runtime check, logs, servers, settings, the engine, the asset handler, Wails and
//	                     the app (internal/desktop/app), which gets the command line, the Pending values and every
//	                     second launch's forwarded arguments
//	start.go             03 §13.4's order with its OS parts behind functions (tested with fakes), --version, step
//	                     2a's installer handover, and the queue of second launches' command lines, served from step
//	                     3 on
//	oneshot.go           the one-shot modes --self-check, --forget-credentials and --purge-data over interfaces,
//	                     and step 2a's link-to-Pending rule (no build constraint: tested on every OS, ubuntu too)
//	oneshot_os.go        their real parts: native.Open from the exe's folder, the asset handler over web.Dist(),
//	                     the credential store and 02's servers in the data root
//	update.go            step 5 (the start-time update with the Pending values), --update-now's logic, the
//	                     updater over settings.json (opened at step 5, kept for step 7) and the late logger they keep
//	updatenow_*.go       --update-now with the updatetest tag (06 §8.11), and nil (exit 2) without it
//	openh264.go          the Cisco OpenH264 opt-in over WA51's helpers and the engine's SetOpenH264Path (04 §9.9),
//	                     in the data root's codecs folder; a file installed earlier goes to the engine at every start
//	firstaction_*.go     main's first action (SetDefaultDllDirectories, SetDllDirectory) and --version's stdout
//	engine_release.go    the engine of every release: native.Open (01 §7.1), and this exe as the sound check's helper
//	engine_fake.go       the engine of dev and e2e builds: 01's fake with 05's audio scenarios (03 §4.5, §22.4),
//	                     which plays the sound check's chime itself (no helper)
//	shell.go             internal/desktop/app's Shell over Wails (03 §6.1): the main window (created at the first
//	                     ShowWindow after an --autostart start, newDeferredShell) with its close hook (W7)
//	                     and WDA report (W10), the tray drawn from internal/desktop/tray's View with the embedded
//	                     build/tray icons (W18), Wails' shutdown into App.Shutdown (W14), its validated OpenURL
//	                     (03 §5.5), and registerServices: the bound services of internal/desktop/bridge (03 §6.2)
//	                     registered with Wails, which `task gen:bindings` reads
//	notify*.go           toasts (03 §15, W8): Wails' notifications service on Windows, started by the shell and never
//	                     bound for page script, the nonce in each toast's data and -Embedding's wait for its
//	                     activation; nothing on the macOS dev build
//	webview2.go          the WebView2 runtime minimum and its message box text (03 §5.4)
//	platform_windows.go  Windows facts: the asset origin and its Guard, the data root and the profile folder from
//	                     the shell's known folders, the WebView2 profile folder, the OS build, step 2a's probe of
//	                     the installer's mutex
//	platform_darwin.go   macOS dev-build facts (WKWebView's Guard, the dev data folder)
//	flavor_*.go          what the build tags production, dev and e2e change (03 §5.3, §5.4); flavor_e2e.go also reads
//	                     ISSHONI_E2E_CDP_PORT (WebView2's debugging port) and ISSHONI_E2E_DATA_DIR (the folder in
//	                     place of %LOCALAPPDATA%), which web/e2e/cdp sets, and gives the credential store a random
//	                     isshoni-test:<hex>: prefix whose entries the start deletes at exit (03 §22.4 B)
//	main_other.go        the stub of every other OS
//	build/tray/          the tray icons (tray-idle, -live, -paused, -warn; .ico of 16 to 32 px), written by icons.go
//	                     (a //go:build ignore program)
//	build/windows/       icon.ico and app.manifest, which `task desktop:exe:windows` puts into the exe's resources
//	                     with syso.go (a //go:build ignore program built in the tools/wails3 module)
//	checklist_test.go    03 §3.2's Wails checklist in the real webview (-tags isshoni_hw: the owner's PC, a Mac)
//	main_windows_test.go 03 §22.2's Windows tests of main: the DLL search-order marker, single instance (W6), the
//	                     running mutex and --version of the real exe
//	oneshot_windows_test.go --forget-credentials and --purge-data against Credential Manager under a test prefix,
//	                     and step 2a with a real mutex and registry key (test names)
//	bindings_test.go     the services as the pinned Wails binds them: W11, W12 and the committed bindings' method ids
//	autostart_windows_test.go the real e2e exe started with --autostart shows no window until a second launch (it
//	                     needs the built SPA, so it skips where web/dist holds only .gitkeep)
//
// Build tags: production (every release, and Wails' own release mode: no DevTools), dev (`task desktop:dev`: Vite's
// dev server, http loopback servers, the fake engine) and e2e (the CDP build: http loopback servers, the WEBVIEW2_*
// variables kept, the fake engine), and updatetest beside them in the updater e2e's CI builds (--update-now and 06's
// test hooks, 06 §8.11). A release has exactly production (06 §5.2).
package main
