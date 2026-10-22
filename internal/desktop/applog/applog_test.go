package applog

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol"
)

func openLog(t *testing.T, o Options) *Log {
	t.Helper()
	if o.Dir == "" {
		o.Dir = t.TempDir()
	}
	l, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func readLog(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func lines(t *testing.T, text string) []map[string]any {
	t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("not a JSON line: %q: %v", sc.Text(), err)
		}
		out = append(out, m)
	}
	return out
}

// TestRotation: isshoni.log rotates before it would pass 5 MiB, and five older files are kept (03 §17, §20).
func TestRotation(t *testing.T) {
	dir := t.TempDir()
	l := openLog(t, Options{Dir: dir})
	log := l.Logger()
	pad := strings.Repeat("x", 1000)
	const total = 33_000 // about 33 MB: more than six full files
	for i := range total {
		log.Info("line", "n", i, "pad", pad)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	for n := range DefaultKeep + 1 {
		name := FileName
		if n > 0 {
			name = fmt.Sprintf("isshoni.%d.log", n)
		}
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if info.Size() > DefaultMaxBytes {
			t.Errorf("%s is %d bytes, over 5 MiB", name, info.Size())
		}
		if n > 0 && info.Size() < DefaultMaxBytes-4096 {
			t.Errorf("%s is %d bytes: rotated early", name, info.Size())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "isshoni.6.log")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a sixth old file exists: %v", err)
	}
	// The newest line is in isshoni.log, and isshoni.1.log holds the lines right before it.
	cur := lines(t, readLog(t, dir))
	if last := cur[len(cur)-1]["n"]; last != float64(total-1) {
		t.Errorf("the last line of isshoni.log is %v", last)
	}
	prev, err := os.ReadFile(filepath.Join(dir, "isshoni.1.log"))
	if err != nil {
		t.Fatal(err)
	}
	old := lines(t, string(prev))
	if got, want := old[len(old)-1]["n"], cur[0]["n"].(float64)-1; got != want {
		t.Errorf("isshoni.1.log ends with %v, want %v", got, want)
	}
	// Reopening appends.
	l2 := openLog(t, Options{Dir: dir})
	l2.Logger().Info("after reopen")
	if text := readLog(t, dir); !strings.Contains(text, `"after reopen"`) || !strings.Contains(text, fmt.Sprint(total-1)) {
		t.Error("reopening didn't append")
	}
}

func TestRotationKeepsSmallSettings(t *testing.T) {
	dir := t.TempDir()
	l := openLog(t, Options{Dir: dir, MaxBytes: 2048, Keep: 2})
	for i := range 200 {
		l.Logger().Info("line", "n", i)
	}
	_ = l.Close()
	for _, name := range []string{FileName, "isshoni.1.log", "isshoni.2.log"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "isshoni.3.log")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a third old file exists: %v", err)
	}
	// After Close, lines are dropped without an error.
	l.Logger().Info("dropped")
}

type pathStruct struct {
	Folder string `json:"folder"`
}

func TestRedaction(t *testing.T) {
	dir := t.TempDir()
	var mirror bytes.Buffer
	l := openLog(t, Options{Dir: dir, Profile: `C:\Users\Alexandra\`, Mirror: &mirror})
	log := l.Logger().With("component", "app", "home", `C:\Users\Alexandra\Documents`)
	perr := &fs.PathError{Op: "open", Path: `C:\users\alexandra\AppData\Local\isshoni\settings.json`, Err: fs.ErrPermission}
	log.Info(`opened c:\USERS\ALEXANDRA\AppData\Local\isshoni`,
		"err", perr,
		"slashes", "C:/Users/Alexandra/AppData",
		"struct", pathStruct{Folder: `C:\Users\Alexandra\Videos`},
		slog.Group("g", slog.String("p", `C:\Users\Alexandra\x`)),
		"access", protocol.Secret("isa_secret-access-token"),
		"token", "isr_refresh-token",
		"password", "hunter2",
		"other", "C:\\Users\\Alexander", // another user's folder that only starts the same
	)
	log.WithGroup("grp").Info("grouped", "p", `C:\Users\Alexandra\y`)
	text := readLog(t, dir)
	for _, leak := range []string{"Alexandra", "ALEXANDRA", "alexandra", "isa_secret", "isr_refresh", "hunter2"} {
		if strings.Contains(text, leak) {
			t.Errorf("the log holds %q:\n%s", leak, text)
		}
	}
	if n := strings.Count(text, ProfileToken); n < 7 {
		t.Errorf("%d replacements, want 7:\n%s", n, text)
	}
	if !strings.Contains(text, "Alexander") {
		t.Error("a folder that isn't the profile was changed")
	}
	if mirror.String() != text {
		t.Error("the mirror differs from the file")
	}
	ls := lines(t, text)
	if ls[0]["component"] != "app" || ls[0]["access"] != "[redacted]" {
		t.Errorf("line: %v", ls[0])
	}
	// Only whole folder names match.
	var b2 bytes.Buffer
	slog.New(NewHandler(&b2, nil, `C:\Users\Al`)).Info(`C:\Users\Alexander\x C:\Users\Al Smith C:\Users\al\x "C:\Users\Al"`)
	if got := lines(t, b2.String())[0]["msg"]; got != `C:\Users\Alexander\x C:\Users\Al Smith %USERPROFILE%\x "%USERPROFILE%"` {
		t.Errorf("boundaries: %q", got)
	}
	// Without a profile nothing is rewritten.
	var buf bytes.Buffer
	slog.New(NewHandler(&buf, nil, "")).Info(`C:\Users\Alexandra`)
	if !strings.Contains(buf.String(), "Alexandra") {
		t.Error("an empty profile rewrote text")
	}
	slog.New(NewHandler(&buf, nil, `C:\`)).Info("short")
}

// TestWailsLogger: at log.level debug the app logs debug, but the logger Wails gets never does, so its "Binding call
// complete" lines never put a password (args) or a token (result) into the file (03 §17).
func TestWailsLogger(t *testing.T) {
	dir := t.TempDir()
	level := new(slog.LevelVar)
	level.Set(slog.LevelDebug)
	l := openLog(t, Options{Dir: dir, Level: level})
	l.Logger().Debug("app debug line")
	wails := l.WailsLogger()
	if wails.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("the Wails logger has Debug enabled")
	}
	// What the pinned Wails logs for each bound call (application/bindings.go).
	wails.Debug("Binding call complete", "id", "1", "method", "Auth.LinkWithPassword",
		"args", `["alex","correct horse battery staple"]`, "result", `{"status":"linked"}`)
	wails.Debug("Binding call complete", "id", "2", "method", "Auth.AccessToken",
		"args", `["https://watch.example.com",false]`, "result", `{"token":"isa_0123456789abcdef","expiresAt":1}`)
	wails.With("x", 1).WithGroup("g").Debug("Binding call complete", "result", "isa_nested")
	wails.Info("wails info line")
	text := readLog(t, dir)
	for _, leak := range []string{"correct horse", "isa_0123456789abcdef", "isa_nested", "Binding call complete"} {
		if strings.Contains(text, leak) {
			t.Errorf("the log holds %q", leak)
		}
	}
	if !strings.Contains(text, "app debug line") || !strings.Contains(text, "wails info line") {
		t.Errorf("lines missing:\n%s", text)
	}
}

const crashChildEnv = "ISSHONI_APPLOG_CRASH_CHILD"

// TestMain runs the crash child: a process that points its crash output at a crash file and panics.
func TestMain(m *testing.M) {
	if dir := os.Getenv(crashChildEnv); dir != "" {
		l, err := Open(Options{Dir: dir})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		if _, err := l.SetCrashOutput(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		panic("isshoni-applog-test-crash")
	}
	os.Exit(m.Run())
}

func TestCrashOutput(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	l := openLog(t, Options{Dir: dir, Clock: func() time.Time { return now }})
	if _, ok := l.FindCrash(); ok {
		t.Fatal("a crash in an empty folder")
	}
	if err := l.WriteCleanExit(); err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$") //nolint:gosec // G204: this test binary itself
	cmd.Env = append(os.Environ(), crashChildEnv+"="+dir)
	if err := cmd.Run(); err == nil {
		t.Fatal("the crash child exited cleanly")
	}
	name, ok := l.FindCrash()
	if !ok {
		t.Fatal("no crash found after a panic")
	}
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil || !strings.Contains(string(data), "isshoni-applog-test-crash") {
		t.Fatalf("the crash file %s: %q, %v", name, data, err)
	}
	if err := l.AckCrash(name); err != nil {
		t.Fatal(err)
	}
	if _, ok := l.FindCrash(); ok {
		t.Error("the same crash was found after its notice was shown")
	}

	// A clean exit after the crash: nothing to show either.
	now = time.Now().UTC().Add(time.Hour)
	if err := l.WriteCleanExit(); err != nil {
		t.Fatal(err)
	}
	if _, ok := l.FindCrash(); ok {
		t.Error("a crash older than the last clean exit was found")
	}
}

func TestFindCrashRules(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	l := openLog(t, Options{Dir: dir, Clock: func() time.Time { return now }})
	write := func(at time.Time, content string) string {
		name := crashPrefix + at.UTC().Format(crashLayout) + crashSuffix
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return name
	}
	old := write(now.Add(-time.Hour), "panic: old")
	if got, ok := l.FindCrash(); !ok || got != old {
		t.Errorf("without last-exit.json: %q %v", got, ok)
	}
	if err := l.WriteCleanExit(); err != nil {
		t.Fatal(err)
	}
	if _, ok := l.FindCrash(); ok {
		t.Error("a crash before the last clean exit")
	}
	write(now.Add(time.Minute), "") // an empty file: the run didn't crash
	if _, ok := l.FindCrash(); ok {
		t.Error("an empty crash file counted")
	}
	newer := write(now.Add(2*time.Minute), "fatal error: x")
	if got, ok := l.FindCrash(); !ok || got != newer {
		t.Errorf("a newer crash: %q %v", got, ok)
	}
	// The next start removes the empty files of earlier runs and makes its own.
	now = now.Add(3 * time.Minute)
	if err := os.WriteFile(filepath.Join(dir, "crash-not-a-time.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	empty := 0
	for _, e := range entries {
		if info, _ := e.Info(); strings.HasPrefix(e.Name(), crashPrefix) && info.Size() == 0 {
			empty++
		}
	}
	if empty != 2 {
		t.Fatalf("%d empty crash files before", empty)
	}
	l.removeEmptyCrashFiles()
	if _, err := os.Stat(filepath.Join(dir, crashPrefix+now.Add(-2*time.Minute).Format(crashLayout)+crashSuffix)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("an empty crash file of an earlier run stayed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "crash-not-a-time.txt")); err != nil {
		t.Error("a file that isn't a crash file was removed")
	}
	if _, err := os.Stat(filepath.Join(dir, newer)); err != nil {
		t.Error("a crash file with content was removed")
	}
	// A broken last-exit.json counts as none.
	if err := os.WriteFile(filepath.Join(dir, LastExitName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok := l.FindCrash(); !ok || got != newer {
		t.Errorf("with a broken last-exit.json: %q %v", got, ok)
	}
}

func TestMinLevel(t *testing.T) {
	var buf bytes.Buffer
	h := MinLevel(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}), slog.LevelWarn)
	log := slog.New(h)
	log.Info("no")
	log.Warn("yes")
	// A record handed over directly below the floor is dropped too.
	_ = h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelDebug, "direct", 0))
	if s := buf.String(); strings.Contains(s, "no") || strings.Contains(s, "direct") || !strings.Contains(s, "yes") {
		t.Errorf("%s", s)
	}
}

func TestOpenNeedsDir(t *testing.T) {
	if _, err := Open(Options{}); err == nil {
		t.Error("Open without a Dir")
	}
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(Options{Dir: filepath.Join(file, "logs")})
	if err == nil || strings.Contains(err.Error(), file) {
		t.Errorf("an unusable folder: %v (no path may show)", err)
	}
}
