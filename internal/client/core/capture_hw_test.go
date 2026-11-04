//go:build isshoni_hw && windows

package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
	"unsafe"

	"github.com/pion/webrtc/v4"
	"golang.org/x/sys/windows"

	"github.com/MoonWX/isshoni/internal/client/publish"
	"github.com/MoonWX/isshoni/internal/client/ratectl"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/mediatest"
	"github.com/MoonWX/isshoni/internal/media/native"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server"
	"github.com/MoonWX/isshoni/internal/server/ops"
	"github.com/MoonWX/isshoni/internal/server/servertest"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
	"github.com/MoonWX/isshoni/internal/version"
)

// The hardware checks of docs/m2/01-client-core.md §12.3 (C18, README WA69): the real engine on the owner's PC (both
// Win11 and Win10 22H2), or on the self-hosted runner once one is registered. They need a GPU, a monitor and the
// release engine, so they are built only with the isshoni_hw tag, on Windows, and skip unless
// ISSHONI_TEST_ENGINE_DLL names the engine (isshoni-media.dll or its folder, with the FFmpeg DLLs next to it, as
// the app ships them). Run them without -race (the detector slows the pump and the pacer, which these tests time),
// on a PC with nothing else busy, the primary monitor at 1080p or larger (16:9):
//
//	$env:ISSHONI_TEST_ENGINE_DLL = '<folder of isshoni-media.dll>'
//	go test -tags isshoni_hw -count=1 -timeout 30m -v -run '^(TestCaptureToSend|TestCaptureConformance)$' ./internal/client/core/
//
// Both tests draw an animated full-screen window on the primary monitor while they run (a helper of this file:
// GDI, every composition), so the capture delivers a frame per refresh, as for a game or a video; leave the PC
// alone until they end. Their output carries the encoder tokens and the numbers, never a source name or a path.

// The bounds of §12.3 and the load they hold for.
const (
	hwP95       = 35 * time.Millisecond // CaptureToSend p95, per encoder
	hwP50       = 20 * time.Millisecond // CaptureToSend p50, per encoder
	hwRun       = 30 * time.Second      // the measured span
	hwFPS       = 60                    // the full layer's frame rate at 1080p60
	hwBitrate   = 8_000_000             // the Auto preset's full layer at the end of the ramp (§9.3)
	hwMinFill   = 0.8                   // a window must hold at least this share of 60 fps worth of samples
	hwStartWait = 30 * time.Second      // the core online, the share live, the first frames at the viewer
)

// hwWindow is publish's CaptureToSend window (publish/stats.go's latencyWindow): the test reads the snapshot once
// per window, so the windows tile the run. Each window's p50 and p95 bound the run's: when every window has at least
// 95 % of its samples at or under x, so has their union.
const hwWindow = 10 * time.Second

// TestCaptureToSend is §12.3's first check: for each encoder the probe lists as Tested (forced with
// Options.Encoder), the real engine shares the primary monitor (a listed source from Engine.Sources, which the test
// may start because it sets the core's allowListed) at 1080p60 with the test tone, through the client core, to an
// in-process server, where an sfutest.Viewer of the same user watches the full layer with the audio. Once the ramp
// has taken the full layer to 8 Mbit/s, the core's own LayerSnapshot.CaptureToSend (capture → last packet written,
// over publish's 10 s window) is read at the end of three back-to-back windows: p95 ≤ 35 ms and p50 ≤ 20 ms in each
// (so over the 30 s), with at least 80 % of 60 fps worth of frames in each, the full layer at 1080p on the forced
// encoder throughout, and its packets at the viewer.
func TestCaptureToSend(t *testing.T) {
	if testing.Short() {
		t.Skip("the 30 s hardware run per encoder runs outside -short")
	}
	eng := openHWEngine(t)
	encoders := testedEncoders(t, eng)
	mon := primaryMonitor(t)
	src := monitorSource(t, eng, mon)
	animate(t, mon.rect)
	cs := hwServer(t)
	for _, enc := range encoders {
		t.Run(enc.Name, func(t *testing.T) { captureToSend(t, cs, eng, src, enc) })
	}
}

// TestCaptureConformance is §12.3's second check: 01 §6.4's suite (mediatest.Conformance) on the real engine with
// the primary monitor, once per encoder the probe lists as Tested ({Listed, TestTone, Encoder: <token>}; checks 5
// and 7 included), while the animated window runs. Each run opens the engine afresh: Conformance closes it.
func TestCaptureConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("the hardware conformance suite runs outside -short")
	}
	eng := openHWEngine(t)
	encoders := testedEncoders(t, eng)
	mon := primaryMonitor(t)
	src := monitorSource(t, eng, mon)
	if err := eng.Close(); err != nil {
		t.Fatalf("closing the probing engine: %v", err)
	}
	animate(t, mon.rect)
	for _, enc := range encoders {
		t.Run(enc.Name, func(t *testing.T) {
			mediatest.Conformance(t, func(t *testing.T) media.Engine { return openHWEngine(t) }, src,
				mediatest.Options{Listed: true, TestTone: true, Encoder: enc.Name})
		})
	}
}

// captureToSend is one encoder's run of TestCaptureToSend.
func captureToSend(t *testing.T, cs *compatServer, eng media.Engine, src media.Source, enc media.EncoderInfo) {
	app := hwApp(t, cs, eng, enc.Name)
	v := cs.viewer(t)
	ctx := ctxFor(t)
	sh, err := app.StartShare(ctx, ShareRequest{RoomID: v.room, Source: src, Preset: protocol.PresetAuto, Audio: true})
	if err != nil {
		t.Fatalf("StartShare of the primary monitor on %s: %v", enc.Name, err)
	}
	waitHW(t, hwStartWait, func() bool { return sh.Snapshot().State == StateLive },
		func() string {
			return fmt.Sprintf("the share is %s (%+v), want it live", sh.Snapshot().State, sh.Snapshot().End)
		})
	id := sh.Snapshot().ID
	v.subscribe(protocol.SubscriptionWant{ShareID: id, Video: protocol.VideoLayerHigh, Audio: protocol.AudioStateOn})
	video := v.recorder(id, webrtc.RTPCodecTypeVideo)
	waitHW(t, hwStartWait, func() bool { return len(video.Packets()) >= hwFPS },
		func() string {
			return fmt.Sprintf("%d video packets at the viewer, want the full layer", len(video.Packets()))
		})

	// The full layer on the forced encoder, active, at 1080p, and ramped to 8 Mbit/s: RampDelay and four RampEvery
	// from ratectl's start of 4 Mbit/s (§9.3), with slack for the engine's re-inits.
	liveAt := time.Now()
	waitHW(t, ratectl.RampDelay+4*ratectl.RampEvery+15*time.Second, func() bool {
		f := fullOf(sh)
		return f != nil && f.Active && f.Encoder == enc.Name && f.TargetBps >= hwBitrate
	}, func() string {
		return fmt.Sprintf("%v after live the full layer is %+v, want it active on %s at %d bit/s",
			time.Since(liveAt).Round(time.Millisecond), fullOf(sh), enc.Name, hwBitrate)
	})
	f := fullOf(sh)
	if !fills1080p(f.Width, f.Height) {
		t.Fatalf("the full layer is %d×%d, want 1080p (1920 wide or 1080 high): the primary monitor must be 1080p or "+
			"larger", f.Width, f.Height)
	}
	if !f.HW && enc.HW {
		t.Errorf("the full layer runs on %s without HW, which the probe lists as hardware", enc.Name)
	}

	// The run: three back-to-back windows of publish's 10 s, each read at its end.
	vmark := len(video.Packets())
	type reading struct {
		c2s, eng publish.LatencyStats
		fps      float64
		sent     int
	}
	var got []reading
	deadline := time.Now()
	for range hwRun / hwWindow {
		deadline = deadline.Add(hwWindow)
		select {
		case <-sh.Done():
			t.Fatalf("the share ended during the run: %+v", sh.Snapshot().End)
		case <-time.After(time.Until(deadline)):
		}
		f := fullOf(sh)
		if f == nil || !f.Active || f.Encoder != enc.Name {
			t.Fatalf("during the run the full layer became %+v, want it active on %s", f, enc.Name)
		}
		got = append(got, reading{c2s: f.CaptureToSend, eng: f.EngineLatency, fps: f.FPS, sent: f.SentBps})
	}
	frames := len(sfutest.Frames(sfutest.WithoutPartialTail(video.Packets()[vmark:])))

	minSamples := int(hwMinFill * hwFPS * hwWindow.Seconds())
	var worst publish.LatencyStats
	for i, r := range got {
		t.Logf("%s (hw %v, %d×%d, %s): window %d: capture-to-send p50 %v, p95 %v, max %v over %d frames; "+
			"engine p50 %v, p95 %v; sent %d bit/s at %.1f fps", enc.Name, enc.HW, f.Width, f.Height, runtime.GOOS,
			i+1, r.c2s.P50, r.c2s.P95, r.c2s.Max, r.c2s.Samples, r.eng.P50, r.eng.P95, r.sent, r.fps)
		if r.c2s.Samples < minSamples {
			t.Errorf("window %d holds %d frames of the full layer, want at least %d (%.0f %% of %d fps): not 1080p60",
				i+1, r.c2s.Samples, minSamples, hwMinFill*100, hwFPS)
		}
		worst.P50, worst.P95 = max(worst.P50, r.c2s.P50), max(worst.P95, r.c2s.P95)
		worst.Max, worst.Samples = max(worst.Max, r.c2s.Max), worst.Samples+r.c2s.Samples
	}
	t.Logf("%s: over %v, %d frames: capture-to-send p50 ≤ %v, p95 ≤ %v (the worst window's), max %v; the viewer "+
		"got %d whole frames", enc.Name, hwRun, worst.Samples, worst.P50, worst.P95, worst.Max, frames)
	if worst.P95 > hwP95 {
		t.Errorf("%s: capture-to-send p95 %v, want at most %v (§12.3)", enc.Name, worst.P95, hwP95)
	}
	if worst.P50 > hwP50 {
		t.Errorf("%s: capture-to-send p50 %v, want at most %v (§12.3)", enc.Name, worst.P50, hwP50)
	}
	if frames < minSamples*int(hwRun/hwWindow) {
		t.Errorf("%s: the viewer got %d whole frames of the full layer in %v, want what was sent", enc.Name, frames,
			hwRun)
	}
}

// fills1080p says whether a layer of w×h uses the full 1080p budget along one axis (a 16:9 monitor gives 1920×1080;
// a wider or taller one is scaled to 1920 wide or 1080 high).
func fills1080p(w, h int) bool { return w*h <= 1920*1080 && (w >= 1920-8 || h >= 1080-8) }

// openHWEngine opens the engine that ISSHONI_TEST_ENGINE_DLL names, with audio (the test tone needs no device, but
// the share's mixer runs as in the app), its log in the test's output at info level; it closes when the test ends.
// Without the variable the test skips.
func openHWEngine(t *testing.T) *native.Engine {
	t.Helper()
	p := os.Getenv("ISSHONI_TEST_ENGINE_DLL")
	if p == "" {
		t.Skip("ISSHONI_TEST_ENGINE_DLL is not set: no engine to run the hardware checks on (01 §12.3)")
	}
	if strings.EqualFold(filepath.Ext(p), ".dll") {
		p = filepath.Dir(p)
	}
	dir, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelInfo}))
	e, err := native.Open(native.Options{Dir: dir, Logger: logger, LogLevel: slog.LevelInfo})
	if err != nil {
		t.Fatalf("native.Open: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

// testWriter writes log lines into the test's output.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// testedEncoders returns the probe's Tested encoders, one per kind token (Options.Encoder forces a kind, not an
// adapter); it fails the test when there is none.
func testedEncoders(t *testing.T, eng media.Engine) []media.EncoderInfo {
	t.Helper()
	p, err := eng.Probe(ctxFor(t), true)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	var out []media.EncoderInfo
	for _, e := range p.Encoders {
		if !e.Tested {
			t.Logf("probe: %s (adapter %d) is not tested: %s %s", e.Name, e.Adapter, e.Status, e.Reason)
			continue
		}
		if slices.ContainsFunc(out, func(o media.EncoderInfo) bool { return o.Name == e.Name }) {
			continue
		}
		t.Logf("probe: %s (adapter %d, hw %v, live bitrate %v, profiles %v) is tested", e.Name, e.Adapter, e.HW,
			e.LiveBitrate, e.Profiles)
		out = append(out, e)
	}
	if len(out) == 0 {
		t.Fatalf("the probe lists no Tested encoder among %d (OS %s)", len(p.Encoders), p.OS.Version)
	}
	return out
}

// monitorSource returns the engine's listed source of the monitor (Engine.Sources' "mon:<device>"). Without such a
// source it falls back to 04's test:pattern, as §12.3 allows, with a note.
func monitorSource(t *testing.T, eng media.Engine, mon monitor) media.Source {
	t.Helper()
	srcs, err := eng.Sources(ctxFor(t))
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	for _, s := range srcs {
		if strings.EqualFold(s.ID, "mon:"+mon.device) {
			t.Logf("sharing the primary monitor (%s, %d×%d)", s.ID, s.Width, s.Height)
			return s
		}
	}
	t.Logf("Sources lists %d monitors, none of them the primary one: sharing test:pattern instead", len(srcs))
	return media.Source{ID: "test:pattern", Kind: media.SourceScreen, Capture: "test"}
}

// hwServer starts an in-process server, creates its admin the way an operator does (a setup link from the admin
// socket, then the setup form) and signs the admin in: the returned compatServer has the admin's cookie, so its
// viewer helper works as in TestCompatServer.
func hwServer(t *testing.T) *compatServer {
	t.Helper()
	srv := servertest.Start(t, servertest.Options{Deps: server.Deps{SPA: fstest.MapFS{
		"index.html":   &fstest.MapFile{Data: []byte("<!doctype html><title>isshoni</title><div id=root></div>\n")},
		"version.json": &fstest.MapFile{Data: []byte(`{"version":"` + version.Version() + `"}`)},
	}}})
	cs := &compatServer{t: t, origin: srv.URL, loopback: true, http: newCompatHTTP(t)}
	link, err := ops.DialAdmin(srv.AdminSocket).SetupURL(ctxFor(t), 0)
	if err != nil {
		t.Fatalf("setup-url: %v", err)
	}
	token, ok := strings.CutPrefix(link.URL, srv.URL+"/setup#")
	if !ok || token == "" {
		t.Fatalf("the setup link is not %s/setup#<token>", srv.URL)
	}
	admin := compatAdmin{Username: "Alex", Password: "correct horse battery"}
	cs.do(http.MethodPost, "/api/v1/auth/setup/complete",
		api.SetupCompleteRequest{Token: token, Username: admin.Username, Password: admin.Password},
		http.StatusCreated, nil)
	cs.cookie = cs.login(admin)
	return cs
}

// hwApp runs the core on eng with the admin's cookie, the encoder forced, listed sources allowed and the test tone
// as the share's audio, until the test ends; it waits until the core is online. The engine outlives the core: the
// core never closes it, and after Close no session of it runs.
func hwApp(t *testing.T, cs *compatServer, eng media.Engine, encoder string) *Client {
	t.Helper()
	logs := &logBuffer{}
	se := sfutest.LoopbackSettings()
	tr := &http.Transport{}
	c, err := New(Options{
		ServerURL:   cs.origin,
		Header:      http.Header{"Cookie": {cs.cookie}},
		Engine:      eng,
		AudioConfig: func() media.AudioConfig { return media.AudioConfig{Mode: media.AudioTestTone} },
		Encoder:     encoder,
		Settings:    &se,
		HTTPClient:  &http.Client{Transport: tr},
		Logger:      slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		allowListed: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ran := make(chan error, 1)
	go func() { ran <- c.Run(context.Background()) }()
	t.Cleanup(func() {
		_ = c.Close()
		select {
		case err := <-ran:
			if err != nil {
				t.Errorf("Run = %v, want nil after Close", err)
			}
		case <-time.After(waitTimeout):
			t.Error("Run did not return after Close")
		}
		tr.CloseIdleConnections()
		if t.Failed() {
			t.Logf("the core's log:\n%s", logs.String())
		}
	})
	waitHW(t, hwStartWait, func() bool { st := c.Status(); return st.Conn == ConnOnline || st.Conn == ConnStopped },
		func() string { return fmt.Sprintf("the core is %+v, want online", c.Status()) })
	if st := c.Status(); st.Conn != ConnOnline {
		t.Fatalf("the core stopped: %v", st.Stop)
	}
	return c
}

// waitHW waits up to d for cond, polling every 20 ms.
func waitHW(t *testing.T, d time.Duration, cond func() bool, what func() string) {
	t.Helper()
	for deadline := time.Now().Add(d); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v: %s", d, what())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The helper window: a borderless, topmost, non-activating window over the whole monitor that draws new content at
// every composition (DwmFlush), through GDI, from a goroutine locked to its OS thread that also pumps the window's
// messages. The content is a band of 64 colored stripes that scrolls 8 px a frame, with a 16×9 grid of blocks of
// which one row changes color each frame, so every refresh brings a new frame and the encoder has real work.

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	dwmapi   = windows.NewLazySystemDLL("dwmapi.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procRegisterClassExW              = user32.NewProc("RegisterClassExW")
	procCreateWindowExW               = user32.NewProc("CreateWindowExW")
	procDefWindowProcW                = user32.NewProc("DefWindowProcW")
	procDestroyWindow                 = user32.NewProc("DestroyWindow")
	procPeekMessageW                  = user32.NewProc("PeekMessageW")
	procTranslateMessage              = user32.NewProc("TranslateMessage")
	procDispatchMessageW              = user32.NewProc("DispatchMessageW")
	procGetDC                         = user32.NewProc("GetDC")
	procReleaseDC                     = user32.NewProc("ReleaseDC")
	procFillRect                      = user32.NewProc("FillRect")
	procMonitorFromPoint              = user32.NewProc("MonitorFromPoint")
	procGetMonitorInfoW               = user32.NewProc("GetMonitorInfoW")
	procSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	procCreateSolidBrush              = gdi32.NewProc("CreateSolidBrush")
	procDeleteObject                  = gdi32.NewProc("DeleteObject")
	procDwmFlush                      = dwmapi.NewProc("DwmFlush")
	procGetModuleHandleW              = kernel32.NewProc("GetModuleHandleW")
	animClassName                     = windows.StringToUTF16Ptr("isshoniHWTestAnimation")
	registerAnimClass                 = sync.OnceValue(registerClass)
	dpiAware                          = sync.OnceFunc(setDPIAware)
	errAnimWindow                     = errors.New("the animation window could not be created")
	animWndProc                       = sync.OnceValue(func() uintptr { return windows.NewCallback(defWindowProc) })
	dpiAwarenessPerMonitorV2          = ^uintptr(3) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = (HANDLE)-4
)

// Win32 constants (winuser.h).
const (
	wsPopup                 = 0x80000000
	wsVisible               = 0x10000000
	wsExTopmost             = 0x00000008
	wsExToolWindow          = 0x00000080
	wsExNoActivate          = 0x08000000
	pmRemove                = 0x0001
	monitorDefaultToPrimary = 1
	wmQuit                  = 0x0012
	monitorInfoSize         = 104 // sizeof(MONITORINFOEXW)
)

type rect struct{ left, top, right, bottom int32 }

type monitorInfoEx struct {
	size    uint32
	monitor rect
	work    rect
	flags   uint32
	device  [32]uint16
}

type wndClassEx struct {
	size       uint32
	style      uint32
	wndProc    uintptr
	clsExtra   int32
	wndExtra   int32
	instance   uintptr
	icon       uintptr
	cursor     uintptr
	background uintptr
	menuName   *uint16
	className  *uint16
	iconSm     uintptr
}

type msg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      struct{ x, y int32 }
	private uint32
}

// monitor is the primary monitor: its device name ("\\.\DISPLAY1", the engine's "mon:" id without the prefix) and
// its rectangle in physical pixels.
type monitor struct {
	device string
	rect   rect
}

func setDPIAware() {
	// Physical pixels, so the window covers the monitor whatever its scale; an error means the process already has
	// an awareness, which is as good.
	_, _, _ = procSetProcessDpiAwarenessContext.Call(dpiAwarenessPerMonitorV2)
}

// primaryMonitor returns the primary monitor (MonitorFromPoint(0, 0, MONITOR_DEFAULTTOPRIMARY), GetMonitorInfoW).
func primaryMonitor(t *testing.T) monitor {
	t.Helper()
	dpiAware()
	h, _, _ := procMonitorFromPoint.Call(0, monitorDefaultToPrimary) // POINT{0, 0} by value
	if h == 0 {
		t.Fatal("MonitorFromPoint found no primary monitor")
	}
	mi := monitorInfoEx{size: monitorInfoSize}
	if r, _, err := procGetMonitorInfoW.Call(h, uintptr(unsafe.Pointer(&mi))); r == 0 {
		t.Fatalf("GetMonitorInfoW: %v", err)
	}
	return monitor{device: windows.UTF16ToString(mi.device[:]), rect: mi.monitor}
}

func defWindowProc(hwnd, m, w, l uintptr) uintptr {
	r, _, _ := procDefWindowProcW.Call(hwnd, m, w, l)
	return r
}

func registerClass() error {
	inst, _, _ := procGetModuleHandleW.Call(0)
	wc := wndClassEx{size: uint32(unsafe.Sizeof(wndClassEx{})), wndProc: animWndProc(), instance: inst,
		className: animClassName}
	if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return fmt.Errorf("RegisterClassExW: %w", err)
	}
	return nil
}

// animate shows the helper window over r until the test ends.
func animate(t *testing.T, r rect) {
	t.Helper()
	dpiAware()
	if err := registerAnimClass(); err != nil {
		t.Fatal(err)
	}
	stop, done, made := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		defer close(done)
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		inst, _, _ := procGetModuleHandleW.Call(0)
		w, h := r.right-r.left, r.bottom-r.top
		hwnd, _, err := procCreateWindowExW.Call(wsExTopmost|wsExToolWindow|wsExNoActivate,
			uintptr(unsafe.Pointer(animClassName)), 0, wsPopup|wsVisible,
			winInt(r.left), winInt(r.top), winInt(w), winInt(h), 0, 0, inst, 0)
		if hwnd == 0 {
			made <- fmt.Errorf("%w: CreateWindowExW: %w", errAnimWindow, err)
			return
		}
		made <- nil
		defer func() {
			_, _, _ = procDestroyWindow.Call(hwnd)
			pumpMessages()
		}()
		for frame := int32(0); ; frame++ {
			select {
			case <-stop:
				return
			default:
			}
			pumpMessages()
			drawFrame(hwnd, w, h, frame)
			if r, _, _ := procDwmFlush.Call(); r != 0 { // S_OK is 0: else no composition to wait for
				time.Sleep(time.Second / hwFPS)
			}
		}
	}()
	if err := <-made; err != nil {
		<-done
		t.Fatal(err)
	}
	t.Cleanup(func() {
		close(stop)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the animation window did not close within 5 s")
		}
	})
	time.Sleep(500 * time.Millisecond) // the window composed before capture starts
}

// pumpMessages dispatches the thread's pending messages.
func pumpMessages() {
	var m msg
	for {
		r, _, _ := procPeekMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0, pmRemove)
		if r == 0 || m.message == wmQuit {
			return
		}
		_, _, _ = procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		_, _, _ = procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// drawFrame draws frame n: the scrolling stripes over the upper two thirds, the block grid below, one row of it
// recolored.
func drawFrame(hwnd uintptr, w, h int32, n int32) {
	dc, _, _ := procGetDC.Call(hwnd)
	if dc == 0 {
		return
	}
	defer func() { _, _, _ = procReleaseDC.Call(hwnd, dc) }()
	fill := func(x0, y0, x1, y1 int32, rgb uint32) {
		b, _, _ := procCreateSolidBrush.Call(uintptr(rgb))
		if b == 0 {
			return
		}
		rc := rect{x0, y0, x1, y1}
		_, _, _ = procFillRect.Call(dc, uintptr(unsafe.Pointer(&rc)), b)
		_, _, _ = procDeleteObject.Call(b)
	}
	band := h * 2 / 3
	const stripes = 64
	sw := max(w/stripes, 1)
	off := n * 8 % (sw * 2)
	for i := int32(-2); i < stripes+2; i++ {
		x := i*sw + off
		fill(x, 0, x+sw, band, hue(int(i*23+n)))
	}
	const cols, rows = 16, 9
	bw, bh := max(w/cols, 1), max((h-band)/rows, 1)
	row := n % rows
	for c := range int32(cols) {
		fill(c*bw, band+row*bh, (c+1)*bw, band+(row+1)*bh, hue(int(c*41+n*7+row*13)))
	}
}

// hue is a saturated color (COLORREF 0x00BBGGRR) on a 192-step wheel.
func hue(i int) uint32 {
	i = ((i % 192) + 192) % 192
	ramp := uint32(i%64) * 4
	var r, g, b uint32
	switch i / 64 {
	case 0:
		r, g, b = 255-ramp, ramp, 0
	case 1:
		r, g, b = 0, 255-ramp, ramp
	default:
		r, g, b = ramp, 0, 255-ramp
	}
	return r | g<<8 | b<<16
}

// winInt passes a Win32 int argument, sign-extended to the register's width as the ABI wants.
func winInt(v int32) uintptr { return uintptr(v) } //nolint:gosec // G115: the sign extension is the point
