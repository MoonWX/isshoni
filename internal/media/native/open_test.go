package native

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/MoonWX/isshoni/internal/media"
)

// openFake opens an engine on a fake caller and closes it at the end of the test. Without a logger in o, the logs go
// nowhere.
func openFake(t *testing.T, f *fakeCaller, o Options) *Engine {
	t.Helper()
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	e, err := openEngine(f, o)
	if err != nil {
		t.Fatalf("openEngine: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

// wantError fails unless err is a *media.Error with code, reason and op; it returns the error's Detail.
func wantError(t *testing.T, err error, code media.ErrorCode, reason, op string) (detail string) {
	t.Helper()
	var me *media.Error
	if !errors.As(err, &me) {
		t.Fatalf("error %v (%T), want a *media.Error", err, err)
	}
	if me.Code != code || me.Reason != reason || me.Op != op {
		t.Fatalf("error %#v, want code %q, reason %q, op %q", me, code, reason, op)
	}
	return me.Detail
}

// TestOpen: Open's portable half checks the ABI, calls im_init with the options, cross-checks the clock, reads the
// build info and fills Info.
func TestOpen(t *testing.T) {
	f := newFakeCaller(t)
	e := openFake(t, f, Options{
		FFmpegDir: "ffmpeg", OpenH264Path: "codecs/openh264-2.6.0-win64.dll", LogLevel: slog.LevelWarn, NoAudio: true,
		Logger: slog.New(slog.DiscardHandler),
	})
	if got, want := e.Info(), (media.Info{Name: "windows", ABI: 1, Build: "0.2.0-test"}); got != want {
		t.Errorf("Info() = %+v, want %+v", got, want)
	}
	b := e.BuildInfo()
	if b.Engine != "0.2.0-test" || b.ABI != 65536 || b.Commit != "0affed6" || !b.TestHooks || b.OS != "windows" ||
		b.Arch != "amd64" || b.Compiler != "MSVC 19.44" || b.Opus != "libopus 1.5.2" ||
		b.FFmpeg != (FFmpegBuildInfo{Dir: ".", Tag: "deps-ffmpeg-9.0.2-r1"}) || b.OpenH264 != (OpenH264BuildInfo{}) {
		t.Errorf("BuildInfo() = %+v", b)
	}
	p, dirs := f.initParams()
	if p == nil {
		t.Fatal("no im_init")
	}
	if p.structSize != 32 || p.abiVersion != 0x10000 || p.flags != imInitNoAudio || p.logLevel != imLogWarn {
		t.Errorf("im_init_params %+v", *p)
	}
	ffmpeg, _ := filepath.Abs("ffmpeg")
	openh264, _ := filepath.Abs("codecs/openh264-2.6.0-win64.dll")
	if dirs != [2]string{ffmpeg, openh264} {
		t.Errorf("im_init dirs %q, want absolute paths %q, %q", dirs, ffmpeg, openh264)
	}
	// The order of 04 §12.2: im_version, im_init, the clock, the build info.
	if order, want := f.sequence()[:4], []proc{procVersion, procInit, procClock, procBuildInfo}; !slices.Equal(order,
		want) {
		t.Errorf("calls %v, want %v first", order, want)
	}
}

func TestOpenDefaults(t *testing.T) {
	f := newFakeCaller(t)
	e, err := openEngine(f, Options{}) // logs to slog.Default
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	p, dirs := f.initParams()
	if p.flags != 0 || p.logLevel != imLogInfo || dirs != [2]string{} {
		t.Errorf("im_init_params %+v, dirs %q: want no flags, info, NULL dirs", *p, dirs)
	}
}

func TestLogLevel(t *testing.T) {
	for l, want := range map[slog.Level]uint32{
		slog.LevelError + 4: imLogError, slog.LevelError: imLogError, slog.LevelWarn: imLogWarn,
		slog.LevelWarn + 1: imLogWarn, slog.LevelInfo: imLogInfo, slog.LevelDebug: imLogDebug,
		slog.LevelDebug - 4: imLogDebug,
	} {
		if got := logLevel(l); got != want {
			t.Errorf("logLevel(%v) = %d, want %d", l, got, want)
		}
	}
}

// TestOpenABICheck: a DLL of another major, or of a minor below goMinMinor, is dependency/abi naming both versions,
// and im_init is never called (04 §5.5).
func TestOpenABICheck(t *testing.T) {
	for _, v := range []uint32{0x20000, 0x00000, 0xFFFF0000} {
		f := newFakeCaller(t)
		f.set(procVersion, func([]uintptr) (uintptr, string) { return fakeGarbage | uintptr(v), "" })
		_, err := openEngine(f, Options{})
		detail := wantError(t, err, media.CodeDependency, tokenABI, "open")
		if want := fmt.Sprintf("ABI %d.%d", v>>16, v&0xFFFF); !strings.Contains(detail, want) ||
			!strings.Contains(detail, "1.0") {
			t.Errorf("version %#x: detail %q names not both versions", v, detail)
		}
		if !errors.Is(err, media.ErrDependency) {
			t.Errorf("version %#x: not ErrDependency", v)
		}
		if n := f.count(procInit); n != 0 {
			t.Errorf("version %#x: im_init called %d times", v, n)
		}
	}
	// A later minor is fine.
	f := newFakeCaller(t)
	f.set(procVersion, func([]uintptr) (uintptr, string) { return fakeResult(0x10003), "" })
	if e := openFake(t, f, Options{}); e.Info().ABI != 1 {
		t.Errorf("ABI %d", e.Info().ABI)
	}
}

// TestOpenInitFails: im_init's status becomes Open's error, mapped, with no shutdown (nothing was initialized); the
// engine's records about the failure are drained into the log first (04 §12.3).
func TestOpenInitFails(t *testing.T) {
	for st, code := range statusCodes {
		if st >= 0 {
			continue
		}
		f := newFakeCaller(t)
		f.set(procInit, fakeFails(st, "dpi_unaware: the process is not per-monitor DPI aware"))
		f.queueLogs(`{"level":"error","component":"engine","msg":"worker did not start"}` + "\n")
		sink, log := newSinkLogger(slog.LevelInfo)
		_, err := openEngine(f, Options{Logger: log})
		reason := tokenDPIUnaware
		switch st {
		case imErrABI:
			reason = tokenABI
		}
		detail := wantError(t, err, code, reason, "open")
		if detail != "im_init: the process is not per-monitor DPI aware" {
			t.Errorf("%s: detail %q", statusName(st), detail)
		}
		if n := f.count(procShutdown); n != 0 {
			t.Errorf("%s: im_shutdown called %d times", statusName(st), n)
		}
		// After the failed im_init only the drain ran, and it ran before Open returned.
		seq := f.sequence()
		if len(seq) < 3 || !slices.Equal(seq[:2], []proc{procVersion, procInit}) ||
			slices.ContainsFunc(seq[2:], func(p proc) bool { return p != procReadLogs }) {
			t.Errorf("%s: calls %v, want im_version, im_init, then im_read_logs only", statusName(st), seq)
		}
		logged := false
		for _, r := range sink.records(t) {
			logged = logged || r["msg"] == "worker did not start"
		}
		if !logged {
			t.Errorf("%s: the engine's record was not logged: %v", statusName(st), sink.records(t))
		}
	}
}

// TestOpenClockCrossCheck: im_clock within Go's QPC readings passes; 2 ms off either way fails Open with internal
// naming both readings, and the engine is shut down again (04 §12.3).
func TestOpenClockCrossCheck(t *testing.T) {
	for _, off := range []time.Duration{0, 900 * time.Microsecond, -900 * time.Microsecond} {
		f := newFakeCaller(t)
		f.setClockOffset(int64(off))
		openFake(t, f, Options{})
	}
	for _, off := range []time.Duration{2 * time.Millisecond, -2 * time.Millisecond, time.Hour} {
		f := newFakeCaller(t)
		f.setClockOffset(int64(off))
		f.queueLogs(`{"t":"2026-10-21T12:00:00.000Z","level":"info","component":"engine","msg":"initialized"}` + "\n")
		_, err := openEngine(f, Options{Logger: slog.New(slog.DiscardHandler)})
		detail := wantError(t, err, media.CodeInternal, "", "open")
		if !strings.Contains(detail, "clock cross-check") || !strings.Contains(detail, "mono_ns") {
			t.Errorf("offset %v: detail %q", off, detail)
		}
		if n := f.count(procShutdown); n != 1 {
			t.Errorf("offset %v: im_shutdown called %d times, want 1", off, n)
		}
		if f.count(procBuildInfo) != 0 {
			t.Errorf("offset %v: the build info was read after a failed cross-check", off)
		}
		if f.count(procReadLogs) == 0 {
			t.Errorf("offset %v: the engine's logs were not drained", off)
		}
	}
	// im_clock itself failing.
	f := newFakeCaller(t)
	f.set(procClock, fakeFails(imErrInvalidArg, "im_clock: out is NULL"))
	_, err := openEngine(f, Options{})
	wantError(t, err, media.CodeInvalid, "", "open")
	if n := f.count(procShutdown); n != 1 {
		t.Errorf("im_shutdown called %d times, want 1", n)
	}
}

// TestOpenBuildInfoFails: an unreadable build info fails Open with internal and shuts the engine down.
func TestOpenBuildInfoFails(t *testing.T) {
	f := newFakeCaller(t)
	f.setBuildJSON(`{"engine":`)
	_, err := openEngine(f, Options{})
	wantError(t, err, media.CodeInternal, "", "open")
	if n := f.count(procShutdown); n != 1 {
		t.Errorf("im_shutdown called %d times, want 1", n)
	}
}

// TestSnprintfRetry: a document of n ≥ 64 KiB bytes is read again with exactly n + 1 bytes (04 §5.1, §12.4).
func TestSnprintfRetry(t *testing.T) {
	big := `{"engine":"0.2.0","commit":"` + strings.Repeat("a", 100_000) + `"}`
	f := newFakeCaller(t)
	f.setBuildJSON(big)
	doc, err := callJSON(f, "build_info", procBuildInfo, jsonMaxCap)
	if err != nil {
		t.Fatal(err)
	}
	if string(doc) != big {
		t.Errorf("document of %d bytes, want %d", len(doc), len(big))
	}
	calls := f.callsOf(procBuildInfo)
	if len(calls) != 2 || calls[0][1] != jsonFirstCap || calls[1][1] != uintptr(len(big)+1) {
		t.Errorf("caps %v, want 65536 then %d", capsOf(calls, 1), len(big)+1)
	}

	// Exactly cap - 1 bytes fits; exactly cap bytes doesn't (the NUL needs room).
	for _, n := range []int{jsonFirstCap - 1, jsonFirstCap} {
		f := newFakeCaller(t)
		f.setBuildJSON(strings.Repeat("x", n))
		doc, err := callJSON(f, "build_info", procBuildInfo, jsonMaxCap)
		if err != nil || len(doc) != n {
			t.Errorf("n = %d: %d bytes, %v", n, len(doc), err)
		}
		if want := 1 + btoi(n >= jsonFirstCap); f.count(procBuildInfo) != want {
			t.Errorf("n = %d: %d calls, want %d", n, f.count(procBuildInfo), want)
		}
	}

	// Leading arguments come first, the buffer and its cap last.
	f = newFakeCaller(t)
	f.set(procGetStats, func(args []uintptr) (uintptr, string) {
		if len(args) != 3 || args[0] != 7 {
			t.Errorf("im_get_stats args %v", args)
		}
		return fakeSnprintf(args[1], args[2], `{"layers":[]}`), ""
	})
	if doc, err := callJSON(f, "stats", procGetStats, jsonMaxCap, 7); err != nil || string(doc) != `{"layers":[]}` {
		t.Errorf("%q, %v", doc, err)
	}
}

// TestSnprintfLimits: over 1 MiB, a document that grows between the two calls, and a negative status.
func TestSnprintfLimits(t *testing.T) {
	f := newFakeCaller(t)
	f.setBuildJSON(strings.Repeat("x", jsonMaxCap))
	_, err := callJSON(f, "build_info", procBuildInfo, jsonMaxCap)
	wantError(t, err, media.CodeInternal, "", "build_info")
	if n := f.count(procBuildInfo); n != 1 {
		t.Errorf("%d calls for a document over the limit, want 1", n)
	}

	// The largest document that fits the 1 MiB retry.
	f = newFakeCaller(t)
	f.setBuildJSON(strings.Repeat("x", jsonMaxCap-1))
	if doc, err := callJSON(f, "build_info", procBuildInfo, jsonMaxCap); err != nil || len(doc) != jsonMaxCap-1 {
		t.Errorf("%d bytes, %v", len(doc), err)
	}

	f = newFakeCaller(t)
	n := 70_000
	f.set(procBuildInfo, func(args []uintptr) (uintptr, string) {
		n += 10 // grows every time
		return fakeSnprintf(args[0], args[1], strings.Repeat("y", n)), ""
	})
	_, err = callJSON(f, "build_info", procBuildInfo, jsonMaxCap)
	detail := wantError(t, err, media.CodeInternal, "", "build_info")
	if !strings.Contains(detail, "grew") || f.count(procBuildInfo) != 2 {
		t.Errorf("detail %q after %d calls", detail, f.count(procBuildInfo))
	}

	f = newFakeCaller(t)
	f.set(procBuildInfo, fakeFails(imErrBadState, "not_initialized: IM_ERR_BAD_STATE"))
	_, err = callJSON(f, "build_info", procBuildInfo, jsonMaxCap)
	wantError(t, err, media.CodeInternal, tokenNotInitialized, "build_info")

	// A smaller limit (05's report has a larger one) caps the first buffer too.
	f = newFakeCaller(t)
	f.setBuildJSON(strings.Repeat("z", 1000))
	if _, err := callJSON(f, "build_info", procBuildInfo, 512); err == nil {
		t.Error("a 1000-byte document passed a 512-byte limit")
	}
	if calls := f.callsOf(procBuildInfo); len(calls) != 1 || calls[0][1] != 512 {
		t.Errorf("caps %v, want one call with 512", capsOf(calls, 1))
	}
}

func capsOf(calls [][]uintptr, i int) []uintptr {
	var caps []uintptr
	for _, c := range calls {
		caps = append(caps, c[i])
	}
	return caps
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestResolveProcs: every export is looked up; missing ones fail with dependency/abi naming each (04 §12.4).
func TestResolveProcs(t *testing.T) {
	var asked []string
	addrs, err := resolveProcs(func(name string) (uintptr, bool) {
		asked = append(asked, name)
		return uintptr(0x1000 + len(asked)), true
	})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(asked) != fmt.Sprint(procNames[:]) {
		t.Errorf("looked up %v", asked)
	}
	for p, a := range addrs {
		if a != uintptr(0x1001+p) {
			t.Errorf("%s at %#x", proc(p), a)
		}
	}

	_, err = resolveProcs(func(name string) (uintptr, bool) {
		switch name {
		case "im_audio_sound_check":
			return 0, false
		case "im_test_opus_decode":
			return 0, true // a NULL address is missing too
		}
		return 0x1000, true
	})
	detail := wantError(t, err, media.CodeDependency, tokenABI, "open")
	if !strings.Contains(detail, "im_audio_sound_check, im_test_opus_decode") ||
		!strings.Contains(detail, "stale build?") {
		t.Errorf("detail %q", detail)
	}
}

func TestEngineDir(t *testing.T) {
	app := filepath.Join(t.TempDir(), "app")
	exe := func() (string, error) { return filepath.Join(app, "isshoni.exe"), nil }
	got, err := engineDir("", exe)
	if err != nil || got != app {
		t.Errorf("engineDir(\"\") = %q, %v; want %q", got, err, app)
	}
	got, err = engineDir("rel/dir", exe)
	abs, _ := filepath.Abs("rel/dir")
	if err != nil || got != abs || !filepath.IsAbs(got) {
		t.Errorf("engineDir(rel) = %q, %v; want %q", got, err, abs)
	}
	_, err = engineDir("", func() (string, error) { return "", errors.New("no executable") })
	wantError(t, err, media.CodeInternal, "", "open")
}

// TestOptionStrings: a NUL in a path option is invalid, before any engine call that would use it.
func TestOptionStrings(t *testing.T) {
	f := newFakeCaller(t)
	_, err := openEngine(f, Options{FFmpegDir: "ff\x00mpeg"})
	wantError(t, err, media.CodeInvalid, "", "open")
	if f.count(procInit) != 0 {
		t.Error("im_init called with a bad option")
	}
	if b, err := cString("x", ""); err != nil || *b != 0 {
		t.Errorf("cString(\"\") = %v, %v", b, err)
	}
	b, err := cString("x", "pick:7")
	if err != nil || fakeCString(uintptr(unsafe.Pointer(b))) != "pick:7" {
		t.Errorf("cString: %v", err)
	}
}

// TestClose: Close cancels a pick, shuts the engine down once, drains the logs after the shutdown, and later calls
// fail without reaching the DLL.
func TestClose(t *testing.T) {
	f := newFakeCaller(t)
	e, err := openEngine(f, Options{Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if n := f.count(procShutdown); n != 1 {
		t.Errorf("im_shutdown called %d times", n)
	}
	if f.count(procCancelPick) == 0 {
		t.Error("Close did not cancel a pick")
	}
	if seq := f.sequence(); slices.Contains(seq[slices.Index(seq, procShutdown):], procCancelPick) {
		t.Errorf("im_cancel_pick after im_shutdown: %v", seq)
	}
	calls := f.total()
	if _, err := e.Clock(); !errors.Is(err, &media.Error{Code: media.CodeInternal}) {
		t.Errorf("Clock after Close: %v", err)
	} else {
		wantError(t, err, media.CodeInternal, tokenNotInitialized, "clock")
	}
	e.Release("pick:1")
	if b := e.BuildInfo(); b.Engine != "0.2.0-test" {
		t.Errorf("BuildInfo after Close: %+v", b)
	}
	if f.total() != calls {
		t.Errorf("%d calls after Close", f.total()-calls)
	}
	seq := f.sequence()
	if last := seq[len(seq)-1]; last != procReadLogs {
		t.Errorf("last call %s, want a final im_read_logs after im_shutdown", last)
	}
}

// TestCloseWaitsForCalls: im_shutdown never races a call in flight (04 §5.1).
func TestCloseWaitsForCalls(t *testing.T) {
	f := newFakeCaller(t)
	e := openFake(t, f, Options{Logger: slog.New(slog.DiscardHandler)})
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.set(procClock, func(args []uintptr) (uintptr, string) {
		once.Do(func() { close(entered) })
		<-release
		return fakeResult(imOK), ""
	})
	done := make(chan error)
	go func() {
		_, err := e.Clock()
		done <- err
	}()
	<-entered
	closed := make(chan struct{})
	go func() {
		_ = e.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close returned while a call was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	if f.count(procShutdown) != 0 {
		t.Fatal("im_shutdown ran while a call was in flight")
	}
	close(release)
	if err := <-done; err != nil {
		t.Errorf("Clock: %v", err)
	}
	<-closed
	if f.count(procShutdown) != 1 {
		t.Error("no im_shutdown")
	}
}

// TestCloseCancelsLatePick: a pick that passed begin before Close and enters im_pick_source only after Close's first
// im_cancel_pick is cancelled too, so Close does not wait for the picker's timeout, and im_shutdown never runs
// while im_pick_source blocks.
func TestCloseCancelsLatePick(t *testing.T) {
	f := newFakeCaller(t)
	e := openFake(t, f, Options{Logger: slog.New(slog.DiscardHandler)})
	var (
		mu       sync.Mutex
		cancels  int
		picking  bool
		shutdown bool
	)
	firstCancel, pickCancel := make(chan struct{}), make(chan struct{}, 1)
	f.set(procCancelPick, func([]uintptr) (uintptr, string) {
		mu.Lock()
		defer mu.Unlock()
		if shutdown {
			t.Error("im_cancel_pick after im_shutdown")
		}
		if cancels++; cancels == 1 {
			close(firstCancel)
		}
		if picking {
			select {
			case pickCancel <- struct{}{}:
			default:
			}
		}
		return fakeResult(imOK), ""
	})
	f.set(procPickSource, func([]uintptr) (uintptr, string) {
		mu.Lock()
		picking = true
		mu.Unlock()
		<-pickCancel // the OS picker stays open until a cancel
		mu.Lock()
		picking = false
		mu.Unlock()
		return fakeResult(imErrCancelled), ""
	})
	f.set(procShutdown, func([]uintptr) (uintptr, string) {
		mu.Lock()
		defer mu.Unlock()
		if picking {
			t.Error("im_shutdown while im_pick_source blocks")
		}
		shutdown = true
		return fakeGarbage, ""
	})

	// WA23's Pick, reduced to its calls: begin, then im_pick_source after Close's first cancel.
	began, picked := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(picked)
		if err := e.begin("pick"); err != nil {
			t.Error(err)
			close(began)
			return
		}
		defer e.end()
		close(began)
		<-firstCancel
		e.c.call(procPickSource)
	}()
	<-began
	start := time.Now()
	closed := make(chan struct{})
	go func() {
		_ = e.Close()
		close(closed)
	}()
	select {
	case <-closed:
		if d := time.Since(start); d > 500*time.Millisecond {
			t.Errorf("Close took %v", d)
		}
	case <-time.After(5 * time.Second):
		t.Error("Close waits for a pick that entered after its first cancel")
		pickCancel <- struct{}{}
		<-closed
	}
	<-picked
	mu.Lock()
	defer mu.Unlock()
	if cancels < 2 || !shutdown {
		t.Errorf("%d cancels, shutdown %v: want a cancel after the pick entered, then im_shutdown", cancels, shutdown)
	}
}

// TestCloseConcurrent: a Close running beside the first returns only after the first's im_shutdown has finished,
// so that a caller may exit on either.
func TestCloseConcurrent(t *testing.T) {
	f := newFakeCaller(t)
	e := openFake(t, f, Options{Logger: slog.New(slog.DiscardHandler)})
	entered, release := make(chan struct{}), make(chan struct{})
	f.set(procShutdown, func([]uintptr) (uintptr, string) {
		close(entered)
		<-release
		return fakeGarbage, ""
	})
	done := make(chan struct{}, 2)
	go func() {
		_ = e.Close()
		done <- struct{}{}
	}()
	<-entered
	go func() {
		_ = e.Close()
		done <- struct{}{}
	}()
	select {
	case <-done:
		t.Fatal("a Close returned while im_shutdown was running")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-done
	<-done
	if n := f.count(procShutdown); n != 1 {
		t.Errorf("im_shutdown called %d times", n)
	}
}

func TestClock(t *testing.T) {
	f := newFakeCaller(t)
	e := openFake(t, f, Options{})
	before := f.qpcNS()
	s, err := e.Clock()
	after := f.qpcNS()
	if err != nil {
		t.Fatal(err)
	}
	if s.MonoNS < before || s.MonoNS > after || s.UncertaintyNS != 500 || s.UnixNS == 0 {
		t.Errorf("Clock() = %+v, outside [%d, %d]", s, before, after)
	}
	if args := f.callsOf(procClock); fakePtr[imClockSample](args[len(args)-1][0]).structSize != 32 {
		t.Error("im_clock_sample.struct_size is not 32")
	}
	f.set(procClock, fakeFails(imErrInternal, "QueryPerformanceCounter failed"))
	_, err = e.Clock()
	detail := wantError(t, err, media.CodeInternal, "", "clock")
	if detail != "im_clock: QueryPerformanceCounter failed" {
		t.Errorf("detail %q", detail)
	}
	at, err := AudioTestHooks(e)
	if err != nil {
		t.Fatal(err)
	}
	f.set(procClock, func(args []uintptr) (uintptr, string) {
		fakePtr[imClockSample](args[0]).monoNS = 1234
		return fakeResult(imOK), ""
	})
	if ns, err := at.EngineClockNS(); err != nil || ns != 1234 {
		t.Errorf("EngineClockNS() = %d, %v", ns, err)
	}
}

// TestBuildInfoRefresh: BuildInfo reads the DLL again (FFmpeg loads after Open) and keeps the last good one when a
// read fails.
func TestBuildInfoRefresh(t *testing.T) {
	f := newFakeCaller(t)
	e := openFake(t, f, Options{Logger: slog.New(slog.DiscardHandler)})
	f.setBuildJSON(strings.Replace(fakeBuildJSON, `"loaded":false,"dir"`, `"loaded":true,"dir"`, 1))
	if b := e.BuildInfo(); !b.FFmpeg.Loaded {
		t.Errorf("FFmpeg not loaded after the refresh: %+v", b.FFmpeg)
	}
	f.set(procBuildInfo, fakeFails(imErrInternal, "out of memory"))
	if b := e.BuildInfo(); !b.FFmpeg.Loaded || b.Engine != "0.2.0-test" {
		t.Errorf("BuildInfo after a failed read: %+v", b)
	}
	if e.Info().Build != "0.2.0-test" {
		t.Errorf("Info().Build = %q", e.Info().Build)
	}
}

func TestRelease(t *testing.T) {
	f := newFakeCaller(t)
	e := openFake(t, f, Options{})
	e.Release("pick:7")
	e.Release("")         // no call
	e.Release("pick\x00") // not a C string: no call
	f.mu.Lock()
	released := fmt.Sprint(f.released)
	f.mu.Unlock()
	if released != "[pick:7]" {
		t.Errorf("released %s", released)
	}
}

// TestNotImplementedYet: the parts later slices fill answer internal/not_implemented, with the Go method as Op.
func TestNotImplementedYet(t *testing.T) {
	f := newFakeCaller(t)
	e := openFake(t, f, Options{})
	ctx := context.Background()
	s := &session{c: f}
	calls := f.total()
	for op, err := range map[string]error{
		"probe":             second(e.Probe(ctx, false)),
		"sources":           second(e.Sources(ctx)),
		"pick":              second(e.Pick(ctx, media.PickRequest{})),
		"start":             second(e.Start(ctx, media.Config{})),
		"start_extra":       second(e.StartExtra(ctx, media.Config{})),
		"protect_window":    e.ProtectWindow(0),
		"set_openh264_path": e.SetOpenH264Path(""),
		"audio_watch":       e.WatchAudio(ctx, media.Exclusions{}),
		"audio_unwatch":     e.UnwatchAudio(ctx),
		"set_exclusions":    e.SetExclusions(ctx, media.Exclusions{}),
		"audio_set_mode":    e.SetAudioMode(ctx, media.AudioOff),
		"audio_set_muted":   e.SetAudioMuted(ctx, true),
		"list_audio_apps":   second(e.AudioReport(ctx)),
		"audio_levels_get":  second(e.AudioLevels(ctx)),
		"audio_sound_check": second(e.SoundCheck(ctx, 0, time.Second)),
		"audio_apps":        second(e.AudioApps(ctx)),
		"next":              second(s.Next(ctx)),
		"set_bitrate":       s.SetBitrate("f", 1e6),
		"reconfigure":       s.Reconfigure(media.Reconfig{}),
		"stop":              s.Stop(),
		"set_layer_active":  s.SetLayerActive("f", false),
		"set_paused":        s.SetPaused(true),
		"stats":             second(s.Stats()),
		"set_audio_bitrate": s.SetAudioBitrate(128000),
		"install_openh264":  second(InstallOpenH264(ctx, nil, t.TempDir())),
		"verify_openh264":   VerifyOpenH264("x"),
		"remove_openh264":   RemoveOpenH264(t.TempDir()),
	} {
		wantError(t, err, media.CodeInternal, reasonNotImplemented, op)
	}
	at, _ := AudioTestHooks(e)
	for op, err := range map[string]error{
		"test_render_tone":   at.RenderTone("", 440, 0.5, time.Second),
		"test_render_bursts": second(at.RenderBursts("", 2000, 0.5, 5*time.Millisecond, time.Second, 3)),
		"test_opus_decode":   second(at.OpusDecode(nil, nil)),
	} {
		wantError(t, err, media.CodeInternal, reasonNotImplemented, op)
	}
	if d, ok := e.SourceDetail("pick:1"); ok || d != (SourceDetail{}) {
		t.Errorf("SourceDetail = %+v, %v", d, ok)
	}
	if s.PCMTap() != nil {
		t.Error("PCMTap is not nil")
	}
	s.RequestKeyframe("f")
	if f.total() != calls {
		t.Errorf("the stubs made %d DLL calls", f.total()-calls)
	}
	if OpenH264.Version != "2.6.0" || !strings.HasSuffix(OpenH264.URL, "/"+OpenH264.FileName+".bz2") {
		t.Errorf("OpenH264 pin %+v", OpenH264)
	}
}

func second[T any](_ T, err error) error { return err }

// TestAudioTestHooks: only the engine native.Open returns has the test exports.
func TestAudioTestHooks(t *testing.T) {
	var other media.Engine
	if _, err := AudioTestHooks(other); !errors.Is(err, media.ErrUnsupportedOS) {
		t.Errorf("nil engine: %v", err)
	}
	if _, err := AudioTestHooks((*Engine)(nil)); !errors.Is(err, media.ErrUnsupportedOS) {
		t.Errorf("nil *Engine: %v", err)
	}
}

// TestSessionNow: Now is qpcNS − epoch_ns, the origin of every CaptureNS (04 §12.3).
func TestSessionNow(t *testing.T) {
	f := newFakeCaller(t)
	s := &session{c: f, epochNS: f.qpcNS()}
	if now := s.Now(); now < 0 || now > int64(time.Second) {
		t.Errorf("Now() = %d right after the epoch", now)
	}
	before := f.qpcNS() - s.epochNS
	now := s.Now()
	if now < before || now > f.qpcNS()-s.epochNS {
		t.Errorf("Now() = %d, not between the fake QPC readings", now)
	}
}
