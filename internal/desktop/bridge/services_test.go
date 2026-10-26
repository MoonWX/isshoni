package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/MoonWX/isshoni/internal/client/credstore"
	"github.com/MoonWX/isshoni/internal/desktop/app"
	"github.com/MoonWX/isshoni/internal/desktop/audio"
	"github.com/MoonWX/isshoni/internal/desktop/bridge/codes"
	"github.com/MoonWX/isshoni/internal/desktop/settings"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/protocol"
)

// allCodes reads bridge/codes' Code constants from its source: name → value.
func allCodes(t *testing.T) map[string]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "codes/codes.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, s := range g.Specs {
			vs := s.(*ast.ValueSpec)
			if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "Code" {
				continue
			}
			v, err := strconv.Unquote(vs.Values[0].(*ast.BasicLit).Value)
			if err != nil {
				t.Fatal(err)
			}
			out[vs.Names[0].Name] = v
		}
	}
	return out
}

// boundMethods is 03 §6.2's table: every service's methods, and whether each takes a context first (W11: the
// methods that do I/O or may wait).
var boundMethods = map[string]map[string]bool{
	"App": {"Info": false, "Heartbeat": false, "OpenExternal": false, "OpenOSSettings": false, "Quit": false,
		"Log": false, "OpenLogsFolder": false, "OpenNotices": false, "PendingLink": false, "LinkHandled": false,
		"OwnConnections": false},
	"Auth": {"State": false, "Servers": false, "Probe": true, "AddServer": true, "SwitchServer": false,
		"RemoveServer": true, "Link": true, "CancelLink": false, "LinkWithPassword": true, "Unlink": true,
		"Recheck": true, "AccessToken": true, "SessionEnded": true, "HelloRefused": false},
	"Share": {"Capabilities": true, "Pick": true, "Release": false, "Start": true, "Current": false, "Stop": true,
		"StopOther": true, "SetPaused": true, "SetPreset": true, "SetAudioEnabled": false, "Stats": false,
		"WatchServerStats": false},
	"Audio": {"FriendsHear": false, "Watch": false, "WatchLevels": false, "SetAppDecision": false,
		"SetKeepOutMicUsers": false, "Decide": false, "DismissPrompt": false, "DismissTip": false, "AppRules": false,
		"SoundCheck": true},
	"Settings": {"Get": false, "Update": false, "Hotkeys": false, "TestHotkey": false},
	"Window":   {"SetFullscreen": false},
	"Updates":  {"Status": false, "Check": true, "Install": true},
}

var ctxType = reflect.TypeFor[context.Context]()
var errType = reflect.TypeFor[error]()

// TestBoundMethods: every exported method of a service is one of 03 §6.2's (Wails binds every exported method), each
// of §6.2's methods exists, I/O methods take a context first, and every method returns at most a value and an error.
func TestBoundMethods(t *testing.T) {
	var buf bytes.Buffer
	s := newTestServices(&fakeCore{}, &fakeServers{}, nil, &buf)
	v := reflect.ValueOf(s).Elem()
	seen := map[string]bool{}
	for i := range v.NumField() {
		f := v.Type().Field(i)
		if !f.IsExported() {
			continue
		}
		want, ok := boundMethods[f.Name]
		if !ok {
			t.Errorf("service %s isn't in 03 §6.2", f.Name)
			continue
		}
		seen[f.Name] = true
		typ := f.Type
		got := map[string]bool{}
		for j := range typ.NumMethod() {
			m := typ.Method(j)
			got[m.Name] = true
			ctxFirst, listed := want[m.Name]
			if !listed {
				t.Errorf("%s.%s is exported, so Wails binds it, but it isn't in 03 §6.2", f.Name, m.Name)
				continue
			}
			mt := m.Type // receiver first
			if has := mt.NumIn() > 1 && mt.In(1) == ctxType; has != ctxFirst {
				t.Errorf("%s.%s: context first = %v, want %v", f.Name, m.Name, has, ctxFirst)
			}
			if mt.NumOut() > 2 || (mt.NumOut() == 2 && mt.Out(1) != errType) {
				t.Errorf("%s.%s returns %d values", f.Name, m.Name, mt.NumOut())
			}
		}
		for name := range want {
			if !got[name] {
				t.Errorf("%s.%s of 03 §6.2 is missing", f.Name, name)
			}
		}
	}
	for name := range boundMethods {
		if !seen[name] {
			t.Errorf("service %s is missing", name)
		}
	}
}

// TestNoCredentialInResults is 03 §22.1's type check: no result type of any bound method holds a refresh token or a
// credential, and only Auth.AccessToken's result holds a secret.
func TestNoCredentialInResults(t *testing.T) {
	var buf bytes.Buffer
	s := newTestServices(&fakeCore{}, &fakeServers{}, nil, &buf)
	secretType := reflect.TypeFor[protocol.Secret]()
	credType := reflect.TypeFor[credstore.Credential]()
	v := reflect.ValueOf(s).Elem()
	for i := range v.NumField() {
		f := v.Type().Field(i)
		if !f.IsExported() {
			continue
		}
		for j := range f.Type.NumMethod() {
			m := f.Type.Method(j)
			for k := range m.Type.NumOut() {
				path := f.Name + "." + m.Name
				walkType(m.Type.Out(k), path, map[reflect.Type]bool{}, func(p string, field reflect.StructField,
					typ reflect.Type) {
					if typ == credType {
						t.Errorf("%s: a credstore.Credential", p)
					}
					if strings.Contains(strings.ToLower(field.Name), "refresh") {
						t.Errorf("%s: a field named %s", p, field.Name)
					}
					if typ == secretType && path != "Auth.AccessToken" {
						t.Errorf("%s: a protocol.Secret outside Auth.AccessToken", p)
					}
				})
			}
		}
	}
}

// walkType calls fn for every struct field reachable from typ.
func walkType(typ reflect.Type, path string, seen map[reflect.Type]bool, fn func(string, reflect.StructField,
	reflect.Type)) {
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array ||
		typ.Kind() == reflect.Map {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct || seen[typ] {
		return
	}
	seen[typ] = true
	for i := range typ.NumField() {
		f := typ.Field(i)
		fn(path+"."+f.Name, f, f.Type)
		walkType(f.Type, path+"."+f.Name, seen, fn)
	}
}

// TestNotImplemented: the methods whose slices haven't landed answer not_implemented with the method's name.
func TestNotImplemented(t *testing.T) {
	var buf bytes.Buffer
	s := newTestServices(&fakeCore{}, &fakeServers{}, nil, &buf)
	ctx := context.Background()
	calls := map[string]func() error{
		"Auth.AddServer":          func() error { return s.Auth.AddServer(ctx, originA) },
		"Auth.SwitchServer":       func() error { return s.Auth.SwitchServer(originA) },
		"Auth.RemoveServer":       func() error { return s.Auth.RemoveServer(ctx, originA) },
		"Auth.Unlink":             func() error { _, err := s.Auth.Unlink(ctx); return err },
		"Share.Pick":              func() error { _, err := s.Share.Pick(ctx); return err },
		"Share.Start":             func() error { _, err := s.Share.Start(ctx, StartRequest{}); return err },
		"Share.StopOther":         func() error { return s.Share.StopOther(ctx, "lounge", "s_1") },
		"Share.SetPaused":         func() error { return s.Share.SetPaused(ctx, true) },
		"Share.SetPreset":         func() error { return s.Share.SetPreset(ctx, "game") },
		"Share.SetAudioEnabled":   func() error { return s.Share.SetAudioEnabled(true) },
		"Audio.FriendsHear":       func() error { _, err := s.Audio.FriendsHear(); return err },
		"Audio.SoundCheck":        func() error { _, err := s.Audio.SoundCheck(ctx); return err },
		"Audio.Watch":             func() error { return s.Audio.Watch(true) },
		"Settings.Get":            func() error { _, err := s.Settings.Get(); return err },
		"Settings.Update":         func() error { _, err := s.Settings.Update(SettingsPatch{}); return err },
		"Settings.Hotkeys":        func() error { _, err := s.Settings.Hotkeys(); return err },
		"Settings.TestHotkey":     func() error { _, err := s.Settings.TestHotkey("Ctrl+Alt+Shift+F9"); return err },
		"Updates.Status":          func() error { _, err := s.Updates.Status(); return err },
		"Updates.Check":           func() error { _, err := s.Updates.Check(ctx); return err },
		"Updates.Install":         func() error { return s.Updates.Install(ctx) },
		"Window.SetFullscreen(∅)": func() error { return (&Window{}).SetFullscreen(true) },
	}
	for name, call := range calls {
		err := call()
		if codeOf(t, err) != codes.CodeNotImplemented {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if method := strings.TrimSuffix(name, "(∅)"); !strings.Contains(err.Error(), `"method":"`+method+`"`) {
			t.Errorf("%s: %s doesn't name the method", name, err)
		}
	}
	// What works without the controller: no share exists, so nothing to show or stop.
	if s.Share.Current() != nil || s.Share.Stats() != nil || s.Share.Stop(ctx) != nil {
		t.Error("a share before the share controller")
	}
	s.Share.Release("p_1")
}

// TestApp is the App service: the external-URL rule, OS settings pages, Quit's busy, the log and the pending link.
func TestApp(t *testing.T) {
	var buf bytes.Buffer
	core := &fakeCore{}
	s := newTestServices(core, &fakeServers{}, nil, &buf)

	for _, u := range []string{"https://example.com/x", "https://watch.example.com:8443/r/lounge"} {
		if err := s.App.OpenExternal(u); err != nil {
			t.Errorf("OpenExternal(%q): %v", u, err)
		}
	}
	for _, u := range []string{"http://example.com", "http://localhost:5173", "file:///C:/Windows/System32",
		"javascript:alert(1)", "ms-settings:display", "search-ms:query=x", `\\host\share`, "//host/share",
		"https://user:pw@example.com", "", "HTTPS://", "https://example.com/a b"} {
		err := s.App.OpenExternal(u)
		if codeOf(t, err) != codes.CodeInvalidArgument || err.Error() != `{"code":"invalid_argument","params":{"field":"url"}}` {
			t.Errorf("OpenExternal(%q): %v", u, err)
		}
	}
	core.dev = true
	if err := s.App.OpenExternal("http://localhost:5173/x"); err != nil {
		t.Errorf("dev http localhost: %v", err)
	}
	if err := s.App.OpenExternal("http://127.0.0.1:8080/link"); err != nil {
		t.Errorf("dev http loopback: %v", err)
	}
	if err := s.App.OpenExternal("http://example.com"); codeOf(t, err) != codes.CodeInvalidArgument {
		t.Errorf("dev http example.com: %v", err)
	}
	if len(core.opened) != 4 {
		t.Fatalf("opened %v", core.opened)
	}

	for _, p := range []string{"notifications", "dnd", "graphics-capture", "sound", "sound-communications",
		"taskbar"} {
		if err := s.App.OpenOSSettings(p); err != nil {
			t.Errorf("OpenOSSettings(%q): %v", p, err)
		}
	}
	for _, p := range []string{"", "ms-settings:display", "display", "control.exe"} {
		if err := s.App.OpenOSSettings(p); codeOf(t, err) != codes.CodeInvalidArgument {
			t.Errorf("OpenOSSettings(%q): %v", p, err)
		}
	}
	if len(core.pages) != 6 || core.pages[4] != app.PageSoundCommunications {
		t.Fatalf("pages %v", core.pages)
	}

	core.live = true
	if err := s.App.Quit(false); codeOf(t, err) != codes.CodeBusy || len(core.quits) != 0 {
		t.Fatalf("Quit while live: %v %v", err, core.quits)
	}
	if err := s.App.Quit(true); err != nil || !slices.Equal(core.quits, []bool{true}) {
		t.Fatalf("Quit(stopShare): %v %v", err, core.quits)
	}
	core.live = false
	if err := s.App.Quit(false); err != nil || len(core.quits) != 2 {
		t.Fatalf("Quit idle: %v %v", err, core.quits)
	}

	s.App.Log("warn", "x", map[string]string{"k": "v"})
	s.App.Heartbeat()
	if len(core.logs) != 1 || core.heartbeat != 1 {
		t.Fatalf("log %v, heartbeat %d", core.logs, core.heartbeat)
	}
	if err := s.App.OwnConnections(make([]string, 65)); codeOf(t, err) != codes.CodeInvalidArgument ||
		!strings.Contains(err.Error(), `"field":"ids"`) {
		t.Fatalf("65 connections: %v", err)
	}
	if err := s.App.OwnConnections([]string{"c_1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.App.OpenLogsFolder(); codeOf(t, err) != codes.CodeNotImplemented {
		t.Fatalf("OpenLogsFolder: %v", err)
	}
	core.pending = &app.PendingLink{RoomID: "lounge"}
	if p := s.App.PendingLink(); p == nil || p.RoomID != "lounge" {
		t.Fatalf("PendingLink %+v", p)
	}
	s.App.LinkHandled()
	if core.handled != 1 {
		t.Fatal("LinkHandled didn't reach the app")
	}
	if info := s.App.Info(); info.OSBuild != 26100 {
		t.Fatalf("Info %+v", info)
	}
}

// TestShareCapabilities answers from the probe, with the media code as the reason when sharing can't work.
func TestShareCapabilities(t *testing.T) {
	var buf bytes.Buffer
	core := &fakeCore{probe: media.Probe{
		OS:       media.OSInfo{Build: 26100, Supported: true},
		Encoders: []media.EncoderInfo{{Name: "nvenc", Status: media.CodeDependency, Reason: "driver_too_old"}, {Name: "mf-sw", Tested: true}},
		Capture:  media.CaptureCaps{Borderless: true},
		Audio:    media.AudioCaps{Backend: "process-loopback"},
	}}
	s := newTestServices(core, &fakeServers{}, nil, &buf)
	got := s.Share.Capabilities(context.Background())
	b, _ := json.Marshal(got)
	if string(b) != `{"canShare":true,"encoders":["mf-sw"],"borderless":true,"osBuild":26100,`+
		`"audioBackend":"process-loopback"}` {
		t.Fatalf("caps %s", b)
	}
	// Two GPUs: one mf-hw entry per adapter, listed once, in the probe's order.
	core.probe.Encoders = []media.EncoderInfo{{Name: "mf-hw", Adapter: 0, HW: true, Tested: true},
		{Name: "mf-hw", Adapter: 1, HW: true, Tested: true}, {Name: "mf-sw", Adapter: -1, Tested: true}}
	if c := s.Share.Capabilities(context.Background()); !c.CanShare ||
		!slices.Equal(c.Encoders, []string{"mf-hw", "mf-sw"}) {
		t.Fatalf("two adapters: %+v", c)
	}
	core.probe.Encoders = nil
	if c := s.Share.Capabilities(context.Background()); c.CanShare || c.Reason != "no_encoder" {
		t.Fatalf("no encoder: %+v", c)
	}
	core.probe.OS.Supported = false
	if c := s.Share.Capabilities(context.Background()); c.CanShare || c.Reason != "unsupported_os" {
		t.Fatalf("old OS: %+v", c)
	}
	core.probeErr = &media.Error{Code: media.CodeUnsupportedOS}
	if c := s.Share.Capabilities(context.Background()); c.CanShare || c.Reason != "unsupported_os" ||
		c.AudioBackend != "none" || c.Encoders == nil {
		t.Fatalf("no engine: %+v", c)
	}
	core.probeErr = context.DeadlineExceeded
	if c := s.Share.Capabilities(context.Background()); c.CanShare || c.Reason != "internal" {
		t.Fatalf("probe timeout: %+v", c)
	}

	for range 2 {
		s.Share.WatchServerStats(true)
	}
	for range 3 {
		s.Share.WatchServerStats(false)
	}
	s.Share.WatchServerStats(true)
	if s.Share.serverStats != 1 {
		t.Fatalf("WatchServerStats count %d, want 1 (never below 0)", s.Share.serverStats)
	}
}

// TestAudio is the Audio service over 05's service: the reference counts, the decision rules and busy.
func TestAudio(t *testing.T) {
	var buf bytes.Buffer
	core := &fakeCore{}
	au := &fakeAudio{}
	s := newTestServices(core, &fakeServers{}, au, &buf)

	for _, on := range []bool{true, true, false, false, false, true} {
		if err := s.Audio.Watch(on); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(au.watches, []bool{true, false, true}) {
		t.Fatalf("Watch calls %v: 05's Watch only on 0↔1 changes", au.watches)
	}
	au.watchErr = &media.Error{Code: media.CodeAudioUnavailable}
	if err := s.Audio.Watch(false); codeOf(t, err) != codes.CodeEngineAudioUnavailable {
		t.Fatalf("failing Watch: %v", err)
	}
	au.watchErr = nil
	if err := s.Audio.Watch(false); err != nil || !slices.Equal(au.watches, []bool{true, false, true, false, false}) {
		t.Fatalf("a failed change is tried again: %v %v", err, au.watches)
	}

	for _, on := range []bool{true, true, false, false, false} {
		_ = s.Audio.WatchLevels(on)
	}
	if !slices.Equal(au.levels, []bool{true, false}) {
		t.Fatalf("WatchLevels calls %v", au.levels)
	}

	if err := s.Audio.Decide("p1", "auto"); codeOf(t, err) != codes.CodeInvalidArgument ||
		!strings.Contains(err.Error(), `"field":"decision"`) || len(au.decided) != 0 {
		t.Fatalf("Decide auto: %v %v", err, au.decided)
	}
	if err := s.Audio.Decide("p1", "keep-out"); err != nil || au.decided[0] != "p1 keep-out" {
		t.Fatalf("Decide: %v %v", err, au.decided)
	}
	au.decideErr = audio.ErrUnknownPrompt
	if err := s.Audio.DismissPrompt("p2"); codeOf(t, err) != codes.CodeNotFound || au.dismissed[0] != "p2" {
		t.Fatalf("DismissPrompt unknown: %v", err)
	}
	if err := s.Audio.SetAppDecision("exe:x.exe", "nope"); codeOf(t, err) != codes.CodeInvalidArgument {
		t.Fatalf("SetAppDecision bad decision: %v", err)
	}
	au.appErr = audio.ErrLockedApp
	if err := s.Audio.SetAppDecision("exe:discord.exe", "include"); codeOf(t, err) != codes.CodeInvalidArgument {
		t.Fatalf("SetAppDecision locked: %v", err)
	}
	au.tipErr = audio.ErrUnknownTip
	if err := s.Audio.DismissTip("nope"); codeOf(t, err) != codes.CodeInvalidArgument {
		t.Fatalf("DismissTip: %v", err)
	}
	if fh, err := s.Audio.FriendsHear(); err != nil || fh.Seq != 7 {
		t.Fatalf("FriendsHear %+v %v", fh, err)
	}
	if rules, err := s.Audio.AppRules(); err != nil || rules == nil {
		t.Fatalf("AppRules %#v %v (want an empty list, not null)", rules, err)
	}

	core.live = true
	if _, err := s.Audio.SoundCheck(context.Background()); codeOf(t, err) != codes.CodeBusy || au.soundChecks != 0 {
		t.Fatalf("SoundCheck while live: %v", err)
	}
	core.live = false
	if res, err := s.Audio.SoundCheck(context.Background()); err != nil || !res.Captured {
		t.Fatalf("SoundCheck %+v %v", res, err)
	}

	// No sound-mode method exists (README §3 item 15).
	if _, ok := reflect.TypeOf(s.Audio).MethodByName("SetMode"); ok {
		t.Fatal("Audio has a sound-mode method")
	}
}

// TestEvents: every event name has the prefix and is unique; 05's events go out with the prefix until Close.
func TestEvents(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range Events {
		if !strings.HasPrefix(e, EventPrefix) || seen[e] {
			t.Errorf("event %q", e)
		}
		seen[e] = true
	}
	if len(Events) != 12 {
		t.Fatalf("%d events, want 03 §6.3's 12", len(Events))
	}

	var buf bytes.Buffer
	au := &fakeAudio{}
	var got []string
	s, err := New(Deps{App: &fakeCore{}, Servers: &fakeServers{}, Audio: au, Log: testLog(&buf),
		Emit: func(name string, data any) {
			b, _ := json.Marshal(data)
			got = append(got, name+" "+string(b))
		}})
	if err != nil {
		t.Fatal(err)
	}
	au.subscribed(audio.Event{Name: audio.EventLevels, Payload: audio.Levels{Seq: 3}})
	if len(got) != 1 || !strings.HasPrefix(got[0], EventAudioLevels+" ") || !strings.Contains(got[0], `"seq":3`) {
		t.Fatalf("forwarded %v", got)
	}
	s.Close()
	s.Close()
	if !au.cancelled {
		t.Fatal("Close didn't unsubscribe from 05's events")
	}
}

// TestSettingsAndWindow: Settings.Get maps settings.json; Window.SetFullscreen reaches the shell.
func TestSettingsAndWindow(t *testing.T) {
	st, err := settings.Open(settings.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	shell := &fakeShell{}
	var buf bytes.Buffer
	s, err := New(Deps{App: &fakeCore{}, Servers: &fakeServers{}, Settings: st, Shell: shell,
		PolicyOff: func() bool { return true }, Log: testLog(&buf)})
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	if v.Schema != 1 || v.Hotkeys.Pause != "Ctrl+Alt+Shift+F9" || !v.HideFromCapture || v.Pill.Mode != "auto" ||
		!v.Updates.PolicyOff || v.Log.Level != "info" || v.ServerPrefs == nil || v.Pill.Positions == nil {
		t.Fatalf("settings view %+v", v)
	}
	if err := s.Window.SetFullscreen(true); err != nil || !slices.Equal(shell.fullscreen, []bool{true}) {
		t.Fatalf("SetFullscreen: %v %v", err, shell.fullscreen)
	}
}

// TestNewNeedsAppAndServers refuses missing required deps.
func TestNewNeedsAppAndServers(t *testing.T) {
	if _, err := New(Deps{Servers: &fakeServers{}}); err == nil {
		t.Error("no App")
	}
	if _, err := New(Deps{App: &fakeCore{}}); err == nil {
		t.Error("no Servers")
	}
	if _, err := New(Deps{App: &fakeCore{}, Servers: &fakeServers{}}); err != nil {
		t.Error(err)
	}
}
