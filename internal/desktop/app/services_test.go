package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/desktop/applog"
	"github.com/MoonWX/isshoni/internal/desktop/settings"
	"github.com/MoonWX/isshoni/internal/version"
)

func TestValidExternalURL(t *testing.T) {
	for raw, want := range map[string][2]bool{ // {release, dev}
		"https://watch.example.com/troubleshooting#desktop-linking": {true, true},
		"HTTPS://moonwx.github.io/isshoni/privacy#desktop":          {true, true},
		"https://[2001:db8::1]:8443/x":                              {true, true},
		"http://localhost:5173/link":                                {false, true},
		"http://app.localhost/":                                     {false, true},
		"http://127.0.0.1:8080/":                                    {false, true},
		"http://[::1]:8080/":                                        {false, true},
		"http://watch.example.com/":                                 {false, false},
		"http://localhost.evil.example/":                            {false, false},
		"https://user:pw@watch.example.com/":                        {false, false},
		"https:watch.example.com":                                   {false, false},
		"https:///path":                                             {false, false},
		"https://":                                                  {false, false},
		"//watch.example.com/":                                      {false, false},
		"/r/lounge":                                                 {false, false},
		"file:///C:/Windows/System32/calc.exe":                      {false, false},
		`\\host\share\x.exe`:                                        {false, false},
		`https://watch.example.com\..\x`:                            {false, false},
		"javascript:alert(1)":                                       {false, false},
		"ms-settings:display":                                       {false, false},
		"search-ms:query=x":                                         {false, false},
		"isshoni://watch.example.com/":                              {false, false},
		"https://watch.example.com/a b":                             {false, false},
		"https://watch.example.com/\u202e":                          {false, false},
		"":                                                          {false, false},
		"https://watch.example.com/" + strings.Repeat("a", 9000):    {false, false},
	} {
		for i, dev := range []bool{false, true} {
			if got := ValidExternalURL(raw, dev); got != want[i] {
				t.Errorf("ValidExternalURL(%.60q, dev %v) = %v", raw, dev, got)
			}
		}
	}
}

// recordingShell is fakeShell with the URLs it was asked to open.
type recordingShell struct {
	fakeShell
	opened []string
}

func (s *recordingShell) OpenURL(u string) error {
	s.opened = append(s.opened, u)
	return nil
}

func TestAppServiceMethods(t *testing.T) {
	e := newEnv(t, nil, nil)
	sh := &recordingShell{}
	e.app.d.Shell = sh
	if err := e.app.OpenExternal("https://watch.example.com/x"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"ms-settings:display", "http://localhost:5173/", "file:///x"} {
		if err := e.app.OpenExternal(bad); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("OpenExternal(%q) = %v", bad, err)
		}
	}
	if len(sh.opened) != 1 {
		t.Errorf("opened %v", sh.opened)
	}
	if err := e.app.OpenOSSettings("display"); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("OpenOSSettings(display) = %v", err)
	}
	if err := e.app.OpenOSSettings(PageSoundCommunications); err != nil {
		t.Errorf("OpenOSSettings: %v", err)
	}
	if !errors.Is(e.app.OpenLogsFolder(), ErrNotImplemented) || !errors.Is(e.app.OpenNotices(), ErrNotImplemented) {
		t.Error("OpenLogsFolder/OpenNotices before their slice")
	}
	e.app.Heartbeat()
	if _, live := e.app.Live(); live {
		t.Error("a share is live before the share controller exists")
	}
	if err := e.app.d.Settings.Update(func(s *settings.Settings) { s.WizardDone = true }); err != nil {
		t.Fatal(err)
	}
	info := e.app.Info()
	if info.Version != version.Version() || info.Arch != runtime.GOARCH || !info.WizardDone || info.Dev ||
		info.PublisherConnectionID != "" || info.OS == "" {
		t.Errorf("Info = %+v", info)
	}
	ids := make([]string, MaxOwnConnections)
	for i := range ids {
		ids[i] = fmt.Sprintf("c_%d", i)
	}
	if err := e.app.OwnConnections(ids); err != nil {
		t.Error(err)
	}
	for _, bad := range [][]string{append(ids, "c_x"), {""}, {"c 1"}, {strings.Repeat("a", 65)}} {
		if err := e.app.OwnConnections(bad); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("OwnConnections(%d ids) = %v", len(bad), err)
		}
	}
}

// TestSPALog: App.Log keeps 10 lines a second, cuts long ones, and reports what it dropped once a minute (03 §17).
func TestSPALog(t *testing.T) {
	e := newEnv(t, nil, nil)
	attrs := map[string]string{"long": strings.Repeat("x", 300)}
	for i := range 15 {
		e.app.Log("warn", fmt.Sprintf("line %d %s", i, strings.Repeat("y", 2000)), attrs)
	}
	out := e.logs.String()
	if n := strings.Count(out, "component=spa"); n != 10 {
		t.Errorf("%d lines in the first second, want 10", n)
	}
	if strings.Contains(out, strings.Repeat("y", 1100)) || strings.Contains(out, strings.Repeat("x", 257)) {
		t.Error("a line or an attribute wasn't cut")
	}
	e.clock.Advance(2 * time.Minute)
	e.app.Log("error", "after", nil)
	out = e.logs.String()
	if !strings.Contains(out, "messages dropped") || !strings.Contains(out, "count=5") || !strings.Contains(out, "msg=after") {
		t.Errorf("no drop report:\n%s", out)
	}
	many := map[string]string{}
	for i := range 40 {
		many[fmt.Sprintf("k%d", i)] = "v"
	}
	e.clock.Advance(2 * time.Second)
	e.logs.Reset()
	e.app.Log("info", "many", many)
	if n := strings.Count(e.logs.String(), "=v"); n != spaLogMaxAttrs {
		t.Errorf("%d attributes kept, want %d", n, spaLogMaxAttrs)
	}
	if got := cut("añb", 2); got != "a" {
		t.Errorf("cut on a rune boundary: %q", got)
	}
}

// TestSPALogKeys: an SPA line through the app's own handler (applog) is a JSON object without a duplicate key: one
// component, spa, and the page's attributes in a group of their own, whatever they are named (03 §17).
func TestSPALogKeys(t *testing.T) {
	e := newEnv(t, nil, nil)
	var buf bytes.Buffer
	a, err := New(Deps{Settings: e.app.d.Settings, Servers: e.srv, Shell: e.shell, OS: fakeOS{}, Clock: e.clock.Now,
		Log: slog.New(applog.NewHandler(&lockedWriter{w: &buf}, slog.LevelDebug, ""))})
	if err != nil {
		t.Fatal(err)
	}
	defer a.cancel()
	long := strings.Repeat("k", 70)
	a.Log("warn", "hello", map[string]string{"component": "evil", "msg": "x", "level": "ERROR", "time": "y",
		"source": "z", "attrs": "w", long + "1": "a", long + "2": "b"})
	a.Log("error", "plain", nil)
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines:\n%s", len(lines), buf.String())
	}
	for _, line := range lines {
		if err := noDuplicateKeys(json.NewDecoder(strings.NewReader(line))); err != nil {
			t.Errorf("%v: %s", err, line)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		if m["component"] != "spa" {
			t.Errorf("component %v: %s", m["component"], line)
		}
	}
	var first map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &first)
	group, _ := first["attrs"].(map[string]any)
	if first["msg"] != "hello" || first["level"] != "WARN" || group["component"] != "evil" || group["msg"] != "x" ||
		len(group) != 7 || group[long[:64]] == nil {
		t.Errorf("the line: %s", lines[0])
	}
}

// noDuplicateKeys reads one JSON value from dec and fails on an object that has a key twice.
func noDuplicateKeys(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch tok {
	case json.Delim('{'):
		seen := map[string]bool{}
		for dec.More() {
			k, err := dec.Token()
			if err != nil {
				return err
			}
			key, _ := k.(string)
			if seen[key] {
				return fmt.Errorf("the key %q twice", key)
			}
			seen[key] = true
			if err := noDuplicateKeys(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	case json.Delim('['):
		for dec.More() {
			if err := noDuplicateKeys(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	}
	return nil
}
