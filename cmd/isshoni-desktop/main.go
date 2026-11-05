//go:build windows || (darwin && cgo)

package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"github.com/MoonWX/isshoni/internal/client/credstore"
	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/desktop/app"
	"github.com/MoonWX/isshoni/internal/desktop/applog"
	"github.com/MoonWX/isshoni/internal/desktop/assets"
	"github.com/MoonWX/isshoni/internal/desktop/audio"
	"github.com/MoonWX/isshoni/internal/desktop/bridge"
	"github.com/MoonWX/isshoni/internal/desktop/i18n"
	"github.com/MoonWX/isshoni/internal/desktop/servers"
	"github.com/MoonWX/isshoni/internal/desktop/settings"
	"github.com/MoonWX/isshoni/internal/desktop/winos"
	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/update"
	"github.com/MoonWX/isshoni/internal/version"
	"github.com/MoonWX/isshoni/web"
)

// appID is the Wails application name: the toast AppID, the process AppUserModelID and the single-instance id
// (README §4 Naming, 03 §13.3, §15, W8).
const appID = winos.InstanceID

// The main window (03 §4.4).
const (
	windowTitle     = "isshoni"
	mainWindowName  = "main"
	windowWidth     = 1200
	windowHeight    = 760
	windowMinWidth  = 800
	windowMinHeight = 560
)

// embeddingWait bounds -Embedding's wait for its toast activation: the mode exits within 5 s (03 §13.4 step 1, §20).
const embeddingWait = 4 * time.Second

// autoplayArg lets viewer audio play without a click (03 §5.4, W13).
const autoplayArg = "--autoplay-policy=no-user-gesture-required"

// firstActionErr is main's first action's error, logged at WARN once logging exists (03 §13.4).
var firstActionErr error

func main() {
	// The first action, before step 0 and in every mode: the DLL search order (03 §13.4, 04 §13.3 layer 2).
	firstActionErr = restrictDLLSearch()
	// Step 0, right after it: the sound check's helper plays the chime and exits, with no logging, single instance,
	// Wails, WebView2 check, relaunch or env scrub (03 §13.4, 05 §11.3).
	if len(os.Args) > 1 && os.Args[1] == audio.SoundCheckHelperFlag {
		os.Exit(audio.RunSoundCheckHelper())
	}
	os.Exit(run(os.Args[1:]))
}

// run is 03 §13.4's order for os.Args[1:] with the real OS parts, and returns the exit code.
func run(args []string) int {
	log := setupLogging(args, nil)
	if firstActionErr != nil {
		log.Warn("desktop: "+firstActionError(firstActionErr).Error(), "err", firstActionErr)
	}
	early := &earlyParts{}
	s := &starter{
		log:              log,
		stdout:           versionOutput,
		soundCheckHelper: audio.RunSoundCheckHelper,
		runningExists:    func() (bool, error) { return winos.MutexExists(winos.RunningMutex) },
		shellRelaunch:    winos.RelaunchUnderShell,
		installerRunning: func() (bool, error) { return mutexOpens(installingMutex) },
		writePending:     winos.NewRegistry(winos.RegistryKey).WritePending,
		openEngine:       selfCheckEngineOf(slog.Default()),
		checkSPA:         checkBundledSPA,
		openStore:        openAppCredentials,
		openServers:      openAppServers,
		dataRoot:         dataRoot,
		removeAll:        os.RemoveAll,
		singleInstance: func() (instance, error) {
			si, err := winos.NewSingleInstance(winos.InstanceID, slog.Default())
			if err != nil {
				return nil, err
			}
			return si, nil
		},
		holdRunning: func() error { return winos.HoldRunningMutex(winos.RunningMutex) },
		openUpdater: early.open,
		takePending: func() (app.Pending, error) { return app.TakePending(winos.NewRegistry(winos.RegistryKey)) },
		updateNow:   updateNowMode(log),
		embedding:   func() bool { return awaitToastActivation(slog.Default(), embeddingWait) },
		relaunch:    startFresh,
		runApp: func(p app.Args, args []string, pending app.Pending, fwd *forwarded, inst instance) int {
			return runApp(p, args, pending, fwd, inst, early)
		},
	}
	return s.start(args)
}

// runApp is 03 §13.4 step 7 in the first instance: the env scrub, the AppUserModelID, the WebView2 runtime check,
// logs, servers, settings (step 5's, when it opened them), the engine, the asset handler, Wails and the app with 06's
// updater; then, once Wails runs, the app's Run, HandleArgs with the Pending values (App.Boot) and the command lines
// second instances forwarded (fwd, served since step 3). An --autostart start creates no window (03 §16.1).
func runApp(p app.Args, args []string, pending app.Pending, fwd *forwarded, inst instance, early *earlyParts) int {
	log := slog.Default().With("component", "app")

	// Before application.New: Wails reads its updater-helper variables there, and WebView2 reads WEBVIEW2_* when
	// the window's environment is created.
	for _, name := range scrubEnv(os.Environ(), flavor.production) {
		if err := os.Unsetenv(name); err != nil {
			log.Warn("desktop: environment variable not removed", "err", err)
		}
	}
	if err := winos.SetAppUserModelID(appID); err != nil {
		log.Warn("desktop: the AppUserModelID wasn't set", "err", err)
	}
	cat, err := i18n.Load(web.DesktopCatalog(), slog.Default())
	if err != nil {
		log.Error("desktop: the bundled texts are unusable", "err", err)
		return exitFailed
	}

	// 03 §5.4: without a current WebView2 runtime the window would stay blank (the pinned Wails only logs it).
	if v, ok := checkWebView2(); !ok {
		log.Error("desktop: no usable WebView2 runtime", "version", v, "min", webview2Min)
		webView2Missing(cat)
		return exitFailed
	}

	if flavor.err != nil {
		log.Error("desktop: the e2e build's variables are unusable", "err", flavor.err)
		return exitFailed
	}
	root, err := dataRoot()
	if err != nil {
		log.Error("desktop: no data folder", "err", err)
		return exitFailed
	}

	// Logs (03 §17): the file log at --log-level, raised by settings' log.level below.
	level := new(slog.LevelVar)
	if p.Debug {
		level.Set(slog.LevelDebug)
	}
	var mirror io.Writer
	if insecureLoopback() {
		mirror = os.Stderr // dev and e2e builds also log to the terminal
	}
	wailsLog := logx.New(logx.Options{}).With("component", "wails")
	if logs, err := applog.Open(applog.Options{Dir: filepath.Join(root, "logs"), Level: level, Profile: profileDir(),
		Mirror: mirror}); err != nil {
		log.Warn("desktop: no log file; logging to stderr only", "err", err)
	} else {
		defer func() { _ = logs.Close() }()
		slog.SetDefault(logs.Logger())
		log = slog.Default().With("component", "app")
		wailsLog = logs.WailsLogger().With("component", "wails")
		if name, ok := logs.FindCrash(); ok {
			log.Warn("desktop: isshoni closed unexpectedly last time", "crash_file", name)
		}
		if _, err := logs.SetCrashOutput(); err != nil {
			log.Warn("desktop: no crash file", "err", err)
		}
		defer func() {
			if err := logs.WriteCleanExit(); err != nil {
				log.Warn("desktop: the clean exit wasn't recorded", "err", err)
			}
		}()
	}
	if firstActionErr != nil {
		log.Warn("desktop: "+firstActionError(firstActionErr).Error(), "err", firstActionErr)
	}
	log.Info("desktop: starting", "version", version.Version(), "os", runtime.GOOS, "dev", insecureLoopback(),
		"args", p)
	if p.UpdatedFrom != "" {
		log.Info("desktop: started after an update", "from", p.UpdatedFrom)
	}

	// 02's servers, then settings.json (which drops the preferences of servers that aren't listed).
	var theApp atomic.Pointer[app.App]
	creds, err := credstore.Open(appCredentialOptions(root))
	if err != nil {
		log.Error("desktop: no credential store", "err", err)
		return exitFailed
	}
	defer dropTestCredentials(creds, log)
	srv, err := deviceauth.OpenServers(deviceauth.ServersOptions{
		Dir:                   root,
		Store:                 creds,
		App:                   appInfo(),
		AllowInsecureLoopback: insecureLoopback(),
		OnState: func(s deviceauth.State) {
			if a := theApp.Load(); a != nil {
				a.OnAuthState(s)
			}
		},
		Logger: slog.Default(),
	})
	if err != nil {
		log.Error("desktop: servers.json is unusable", "err", err)
		return exitFailed
	}
	defer func() { _ = srv.Close() }()
	st, err := openSettings(root, early, srv.Has)
	if err != nil {
		log.Error("desktop: settings.json is unusable", "err", err)
		return exitFailed
	}
	defer func() { _ = st.Close() }()
	setLogDebug := func(debug bool) {
		if debug || p.Debug {
			level.Set(slog.LevelDebug)
		} else {
			level.Set(slog.LevelInfo)
		}
	}
	setLogDebug(st.DebugLogging())
	// 06's updater (built at step 5 in the first instance, or here when step 5 couldn't): its schedule runs with the
	// app, and Settings → Updates, the tray and the toast reach it through the app.
	upd := early.updater
	if upd == nil {
		if upd, err = newUpdater(root, st); err != nil {
			log.Warn("desktop: no updater", "err", err)
			upd = nil
		}
	}

	var engine media.Engine
	if e, err := openEngine(slog.Default(), level.Level() <= slog.LevelDebug); err != nil {
		log.Warn("desktop: no media engine; sharing is off", "err", err)
	} else {
		engine = e
		defer func() { _ = e.Close() }()
	}
	// The Cisco OpenH264 opt-in (04 §9.9): a file installed earlier goes to the engine again.
	h264 := newOpenH264(engine, root, slog.Default())
	h264.applyAtStart()

	webviewData, err := webviewDataDir()
	if err != nil {
		log.Error("desktop: no folder for the WebView2 profile", "err", err)
		return exitFailed
	}
	handler, err := assets.New(assets.Options{
		Dist:      web.Dist(),
		Bridge:    bridgeConfig(appFacts(), srv),
		Origin:    assetOrigin,
		Dev:       insecureLoopback(),
		DevServer: devServerOrigin(),
	})
	if err != nil {
		log.Error("desktop: the bundled web app is unusable", "err", err)
		return exitFailed
	}
	// 05's audio policy over the engine's audio part, with settings.json's audio section (03 §6.1, 05 §14). Without
	// an engine there is none, and the bound Audio service answers not_implemented.
	audioSvc := newAudioService(engine, st, webviewData)
	// Wails' own logger never logs below Info, whatever the log level is: at Debug the pinned Wails logs every
	// bound call's raw arguments and result (03 §17).
	// W5's fallback (03 §3.2, §5.5 layer 2): Go sends a window that left the asset origin back to the start page.
	nav := newNavGuard(assetOrigin, slog.Default())
	opts := appOptions(handler, assetGuard(), webviewData, wailsLog)
	opts.RawMessageHandler = navMessages(nav)
	wailsApp := application.New(opts)
	var shell *wailsShell
	if p.Autostart {
		shell = newDeferredShell(wailsApp, mainWindowOptions(), slog.Default())
	} else {
		shell = newShell(wailsApp, mainWindowOptions(), slog.Default())
	}
	shell.guardNavigation(nav)
	deps := app.Deps{
		Settings:         st,
		Servers:          srv,
		Engine:           engine,
		Shell:            shell,
		OS:               winos.New(),
		I18n:             cat,
		Log:              slog.Default(),
		InsecureLoopback: insecureLoopback(),
		LogsDir:          filepath.Join(root, "logs"),
		NoticesFile:      noticesFile(),
		SetLogDebug:      setLogDebug,
	}
	bdeps := bridge.Deps{Servers: bridge.ServersOf(srv), ProbeApp: appInfo(),
		AllowInsecureLoopback: insecureLoopback(), Settings: st, PolicyOff: update.PolicyDisabled,
		Log: slog.Default()}
	if upd != nil { // never a typed nil inside the interfaces
		deps.Updater = upd
	}
	if h264 != nil {
		bdeps.OpenH264 = h264
	}
	if audioSvc != nil { // never a typed nil inside the interfaces
		deps.Audio, bdeps.Audio = audioSvc, audioSvc
	}
	a, err := app.New(deps)
	if err != nil {
		log.Error("desktop: the app can't start", "err", err)
		return exitFailed
	}
	theApp.Store(a)
	shell.bind(a)
	// The bound services (03 §6.2), registered before Wails runs.
	bdeps.App = a
	svcs, err := registerServices(shell, bdeps)
	if err != nil {
		log.Error("desktop: the bound services can't start", "err", err)
		return exitFailed
	}
	defer svcs.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan struct{})
	var started sync.Once
	// Once Wails shuts down, its window can't be shown: a forwarded command line would block on it for good.
	wailsApp.OnShutdown(fwd.stop)
	wailsApp.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		started.Do(func() {
			if err := shell.started(); err != nil {
				log.Warn("desktop: no toasts", "err", err)
			}
			go func() {
				defer close(runDone)
				if err := a.Run(ctx); err != nil {
					log.Error("desktop: the app stopped", "err", err)
				}
			}()
			a.Boot(args, pending)
			fwd.start(a.HandleArgs)
		})
	})
	err = shell.Run()
	// Nothing of Wails runs any more. Forwarded command lines are dropped, and the pipe and the single-instance lock
	// go first: a launch from now on starts a new first instance instead of forwarding to this one while it finishes
	// (the running mutex stays held until the process exits, 03 §13.4 step 4).
	fwd.stop()
	if inst != nil {
		_ = inst.Close()
	}
	cancel()
	started.Do(func() { close(runDone) }) // Wails never started: nothing runs
	select {
	case <-runDone:
	case <-time.After(app.QuitTimeout):
		log.Warn("desktop: the app didn't stop in time")
	}
	if err != nil {
		log.Error("desktop: the app stopped", "err", err)
		return exitFailed
	}
	return exitOK
}

// newAudioService is 05's audio.Service over engine's audio part (media.AudioController), with settings.json's
// audio section (05's defaults under it) and the app's SelfInfo: this process, the WebView2 profile folder given to
// Wails (webviewData, absolute, without a trailing separator; 05 §5.3) and the sound check's helper (this exe in a
// build with the shipped engine, "" with the fake, which plays the chime itself). nil without an engine or with one
// that has no audio part. App.Run runs it and Quit closes it (03 §6.1).
func newAudioService(engine media.Engine, st *settings.Store, webviewData string) *audio.Service {
	ctl, ok := engine.(media.AudioController)
	if engine == nil || !ok {
		return nil
	}
	self := audio.SelfInfo{PID: uint32(os.Getpid()), WebViewDataDir: webviewData, //nolint:gosec // G115: a PID fits
		Exe: soundCheckExe()}
	return audio.New(ctl, settings.NewAudioStore(st, audio.DefaultSettings), self, slog.Default())
}

// noticesFile is the installed THIRD_PARTY_NOTICES.txt (06 §2.2): <exe dir>\licenses\. "" when the exe's path is
// unknown (App.OpenNotices then answers not_implemented).
func noticesFile() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(exe), "licenses", "THIRD_PARTY_NOTICES.txt")
}

// openSettings is settings.json for step 7: step 5's store when it opened one (without servers.json, which wasn't
// open yet: the preferences of servers that aren't listed are dropped now, as Open's KnownServer does), else a store
// opened here.
func openSettings(root string, early *earlyParts, known func(origin string) bool) (*settings.Store, error) {
	if early == nil || early.settings == nil {
		return settings.Open(settings.Options{Dir: root, UpdateChecks: winos.NewRegistry(winos.RegistryKey).UpdateChecks,
			KnownServer: known, Logger: slog.Default()})
	}
	st := early.settings
	if err := st.Update(func(s *settings.Settings) {
		for origin := range s.ServerPrefs {
			if !known(origin) {
				delete(s.ServerPrefs, origin)
			}
		}
	}); err != nil {
		slog.Default().Warn("desktop: the preferences of removed servers weren't dropped", "err", err)
	}
	return st, nil
}

// setupLogging makes the process's default logger and returns the app's own, which adds component=app. The default
// stays unscoped: packages that log through slog.Default() name their own component (internal/desktop/assets adds
// component=assets), and a second component key would make a JSON line with a duplicate key. out nil is stderr.
// Step 7 replaces it with the file log (applog).
func setupLogging(args []string, out io.Writer) *slog.Logger {
	level := new(slog.LevelVar)
	if debugLogging(args) {
		level.Set(slog.LevelDebug)
	}
	base := logx.New(logx.Options{Level: level, Out: out})
	slog.SetDefault(base)
	return base.With("component", "app")
}

// debugLogging reports --log-level debug (or --log-level=debug) among args (03 §4.2), which a launch with
// --open-url never honors (03 §13.1).
func debugLogging(args []string) bool { return app.ParseArgs(args).Debug }

// scrubbedEverywhere are variables removed in every build: FRONTEND_DEVSERVER_URL would move Wails' start URL to
// another port, and so the page off the asset origin the Guard accepts (the app's own handler serves the SPA, never
// Wails' dev proxy); WAILS_UPDATER_HELPER* would make application.New run Wails' own updater helper, which swaps
// the app's files (isshoni's updates are 06's, never Wails').
var scrubbedEverywhere = []string{"FRONTEND_DEVSERVER_URL", "WAILS_UPDATER_HELPER"}

// wailsWebView2Values are the values the pinned Wails gives five WEBVIEW2_* variables on Windows, in every build, at
// package init (before main) and again right before it creates the webview environment, overwriting what the parent
// passed: a variable that exists makes WebView2 skip its registry override of the same setting
// (internal/webview2/webviewloader, preventEnvAndRegistryOverrides). scrubEnv leaves these values in place. So in
// e2e builds too, which keep WEBVIEW2_*, a parent's WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS never reaches the webview
// at beta.34: 03 §22.4 B's CDP port needs another way in (WA72).
var wailsWebView2Values = map[string]string{
	"WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS": "",
	"WEBVIEW2_PIPE_FOR_SCRIPT_DEBUGGER":     "",
	"WEBVIEW2_BROWSER_EXECUTABLE_FOLDER":    "",
	"WEBVIEW2_USER_DATA_FOLDER":             "",
	"WEBVIEW2_RELEASE_CHANNEL_PREFERENCE":   "0",
}

// scrubEnv returns the names of the variables of environ to unset before Wails starts (03 §5.4, W19): in production
// builds every name that starts with WEBVIEW2_ (compared case-insensitively, as Windows compares names), except the
// placeholders of wailsWebView2Values, so nothing outside the app can start its webview with a debugging port or a
// script debugger; in every build the names of scrubbedEverywhere (FRONTEND_DEVSERVER_URL exactly,
// WAILS_UPDATER_HELPER as a prefix). e2e builds keep WEBVIEW2_*.
func scrubEnv(environ []string, production bool) []string {
	var names []string
	for _, kv := range environ {
		name, value, _ := strings.Cut(kv, "=")
		if name == "" { // Windows' per-drive "=C:" entries
			continue
		}
		upper := strings.ToUpper(name)
		placeholder, isWails := wailsWebView2Values[upper]
		keep := isWails && value == placeholder
		switch {
		case production && strings.HasPrefix(upper, "WEBVIEW2_") && !keep:
		case upper == scrubbedEverywhere[0]:
		case strings.HasPrefix(upper, scrubbedEverywhere[1]):
		default:
			continue
		}
		names = append(names, name)
	}
	return names
}

// appFacts describes this build for bridge.js.
func appFacts() assets.AppFacts {
	return assets.AppFacts{Version: version.Version(), OS: string(clientOS), OSBuild: osBuild(), Arch: runtime.GOARCH}
}

// serverList is what bridge.js needs of 02's servers.
type serverList interface {
	List() []deviceauth.ServerEntry
	Active() *deviceauth.Manager
}

// bridgeConfig is the asset handler's BridgeConfig source: the active server (null before the first one is added)
// and the derived id of every listed server (03 §5.2, §8.1, §14.3). srv nil lists none.
func bridgeConfig(facts assets.AppFacts, srv serverList) func() assets.BridgeConfig {
	return func() assets.BridgeConfig {
		c := assets.BridgeConfig{
			V:         assets.BridgeConfigVersion,
			Transport: "wails",
			App:       facts,
			ServerIDs: []string{},
			Dev:       insecureLoopback(),
		}
		if srv == nil {
			return c
		}
		active := ""
		if m := srv.Active(); m != nil {
			active = m.State().Origin
		}
		for _, e := range srv.List() {
			id := servers.LocalID(e.Origin)
			c.ServerIDs = append(c.ServerIDs, id)
			if e.Origin == active {
				c.Server = &assets.ServerFacts{ID: id, Origin: e.Origin, Name: e.Name}
			}
		}
		return c
	}
}

// appInfo describes this app to servers (02 §11.2): the device name is the computer's name cut to 64 code points,
// or defaultDeviceName.
func appInfo() deviceauth.AppInfo {
	host, _ := os.Hostname()
	return deviceauth.AppInfo{Version: version.Version(), ClientKind: protocol.ClientKindDesktop, OS: clientOS,
		DeviceName: deviceName(host)}
}

// deviceName is 02's default device name for a computer name.
func deviceName(host string) string {
	host = strings.TrimSpace(host)
	if host == "" || !utf8.ValidString(host) {
		return defaultDeviceName
	}
	if utf8.RuneCountInString(host) > 64 {
		host = string([]rune(host)[:64])
	}
	return host
}

// appOptions are the Wails application's options (03 §4, §5.4).
func appOptions(handler http.Handler, guard func(http.Handler) http.Handler, webviewData string,
	wailsLog *slog.Logger) application.Options {
	return application.Options{
		Name:        appID,
		Description: windowTitle,
		Logger:      wailsLog,
		LogLevel:    slog.LevelInfo,
		Assets: application.AssetOptions{
			Handler:    handler,
			Middleware: application.Middleware(guard),
			// The asset server's request lines hold the page's path, a deep link's room id included (03 §17).
			DisableLogging: true,
		},
		Windows: application.WindowsOptions{
			WebviewUserDataPath:   webviewData,
			AdditionalBrowserArgs: append([]string{autoplayArg}, flavor.browserArgs...),
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
	}
}

// deniedPermissions are the webview permissions the page never gets. Setting any of them also stops Wails from
// granting every permission without a prompt, its default when a window has no policy; the kinds left out keep the
// webview's own prompt. Camera, microphone and geolocation are also off in the page's Permissions-Policy (03 §5.3).
var deniedPermissions = map[application.PermissionType]application.Permission{
	application.PermissionMicrophone:    application.PermissionDeny,
	application.PermissionCamera:        application.PermissionDeny,
	application.PermissionGeolocation:   application.PermissionDeny,
	application.PermissionNotifications: application.PermissionDeny,
}

// mainWindowOptions are the main window's options (03 §4.4) with the WebView2 settings of 03 §5.4 that the pinned
// Wails exposes (W9). DevTools follow the build: Wails turns them off with -tags production; the default context
// menu is off in production too (Wails keeps it on in dev and e2e builds). Wails itself always turns off the status
// bar, the browser accelerator keys and swipe navigation.
func mainWindowOptions() application.WebviewWindowOptions {
	return application.WebviewWindowOptions{
		Name:                       mainWindowName,
		Title:                      windowTitle,
		Width:                      windowWidth,
		Height:                     windowHeight,
		MinWidth:                   windowMinWidth,
		MinHeight:                  windowMinHeight,
		InitialPosition:            application.WindowCentered,
		URL:                        "/",
		ZoomControlEnabled:         false,
		DefaultContextMenuDisabled: true,
		EnableFileDrop:             false,
		Permissions:                deniedPermissions,
		Windows: application.WindowsWindow{
			GeneralAutofillEnabled:  false,
			PasswordAutosaveEnabled: false,
			EnableSwipeGestures:     false,
		},
	}
}
