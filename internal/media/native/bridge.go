package native

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/media"
)

// This file is everything behind the caller interface (04 §12.1, §12.4), so it builds and is tested on every OS
// against fakecaller_test.go's fake: the Engine and session logic, the snprintf retry, JSON decoding, the clock
// cross-check, the log drain. README WA10 wrote the loader half (Open's checks, build info, clock, log drain,
// Close); the parts marked "not implemented yet" are README WA23's (Probe, Sources, Pick, Start and the session),
// with 05's audio in audio.go (WA35), OpenH264 in openh264.go (WA51) and ProtectWindow and the self-window pick in
// WA70.

// The engine and its sessions implement the contract (04 §12.2).
var (
	_ media.Engine          = (*Engine)(nil)
	_ media.AudioController = (*Engine)(nil) // audio.go (05 §13.1)
	_ media.Session         = (*session)(nil)
)

// proc is an index into the export table Open resolves (04 §12.4).
type proc uint8

// The exports, in header order: every IM_API function of isshoni_media.h v1.0, 05's audio exports and the shipped
// test exports included (abi_test.go checks the list against the header).
const (
	procVersion proc = iota
	procInit
	procShutdown
	procBuildInfo
	procSetOpenH264Path
	procClock
	procProbe
	procListSources
	procPickSource
	procCancelPick
	procReleaseSource
	procListAudioApps
	procAudioWatch
	procAudioUnwatch
	procSetExclusions
	procAudioSetMode
	procAudioSetMuted
	procAudioLevelsGet
	procAudioSoundCheck
	procStart
	procGetSessionInfo
	procNextPacket
	procRequestKeyframe
	procSetBitrate
	procSetLayerActive
	procUpdateLayer
	procSetProfile
	procSetContentHint
	procSetPaused
	procSetAudioBitrate
	procGetStats
	procStop
	procProtectWindow
	procLastError
	procReadLogs
	procABILayout
	procTestRenderTone
	procTestRenderBursts
	procTestOpusDecode

	procCount // the number of exports
)

// procNames are the exports' C names, by proc. Every one is required: all are v1.0 (minor 0 = goMinMinor). An
// export of a later minor would be resolved as optional and gate its feature (04 §5.5).
var procNames = [procCount]string{
	procVersion:          "im_version",
	procInit:             "im_init",
	procShutdown:         "im_shutdown",
	procBuildInfo:        "im_build_info",
	procSetOpenH264Path:  "im_set_openh264_path",
	procClock:            "im_clock",
	procProbe:            "im_probe",
	procListSources:      "im_list_sources",
	procPickSource:       "im_pick_source",
	procCancelPick:       "im_cancel_pick",
	procReleaseSource:    "im_release_source",
	procListAudioApps:    "im_list_audio_apps",
	procAudioWatch:       "im_audio_watch",
	procAudioUnwatch:     "im_audio_unwatch",
	procSetExclusions:    "im_set_exclusions",
	procAudioSetMode:     "im_audio_set_mode",
	procAudioSetMuted:    "im_audio_set_muted",
	procAudioLevelsGet:   "im_audio_levels_get",
	procAudioSoundCheck:  "im_audio_sound_check",
	procStart:            "im_start",
	procGetSessionInfo:   "im_get_session_info",
	procNextPacket:       "im_next_packet",
	procRequestKeyframe:  "im_request_keyframe",
	procSetBitrate:       "im_set_bitrate",
	procSetLayerActive:   "im_set_layer_active",
	procUpdateLayer:      "im_update_layer",
	procSetProfile:       "im_set_profile",
	procSetContentHint:   "im_set_content_hint",
	procSetPaused:        "im_set_paused",
	procSetAudioBitrate:  "im_set_audio_bitrate",
	procGetStats:         "im_get_stats",
	procStop:             "im_stop",
	procProtectWindow:    "im_protect_window",
	procLastError:        "im_last_error",
	procReadLogs:         "im_read_logs",
	procABILayout:        "im_abi_layout",
	procTestRenderTone:   "im_test_render_tone",
	procTestRenderBursts: "im_test_render_bursts",
	procTestOpusDecode:   "im_test_opus_decode",
}

func (p proc) String() string {
	if p < procCount {
		return procNames[p]
	}
	return fmt.Sprintf("proc(%d)", uint8(p))
}

// fallible reports whether the export's result is a status or a length whose negative values are statuses, after
// which im_last_error holds the detail. im_version returns a version, im_last_error a length, and im_shutdown and
// im_release_source return nothing: their result register is meaningless.
func (p proc) fallible() bool {
	switch p {
	case procVersion, procShutdown, procReleaseSource, procLastError:
		return false
	}
	return true
}

// caller runs the engine's exports (04 §12.4). call_windows.go implements it over the DLL; the tests use
// fakecaller_test.go's fake, which reads and writes the same memory the DLL would.
type caller interface {
	// call runs one export with syscall.SyscallN semantics. args are its C arguments: integers, or addresses of memory
	// that the binding pinned for the call (addrOf). r is the C result register, of which only the export's own width
	// counts: int32(r) for a status or length, uint32(r) for im_version. When the export is fallible and int32(r) is
	// negative, lastErr is im_last_error's text, read on the same locked OS thread (it is thread-local).
	call(p proc, args ...uintptr) (r uintptr, lastErr string)
	// qpcNS is QueryPerformanceCounter in nanoseconds (qpcToNS), the engine clock's domain (04 §12.3).
	qpcNS() int64
}

// addrOf returns the address of *v for one DLL call and pins it in pn until the caller unpins pn after the call. A
// uintptr is not a reference: without the pin the collector could free the memory, and a value the compiler kept on
// the goroutine's stack could move with it during the call. Pinning makes the value escape to the heap, where Go never
// moves memory, and keeps it alive. Objects that *v points to are heap objects reachable from it, so they stay too.
// The engine keeps no pointer after a call returns (04 §5.1): a pin lasts one call. A nil v is address 0.
func addrOf[T any](pn *runtime.Pinner, v *T) uintptr {
	if v == nil {
		return 0
	}
	pn.Pin(v)
	return uintptr(unsafe.Pointer(v))
}

// cString returns s as a NUL-terminated byte string in fresh Go memory, for a const char* field or argument. A
// string with a NUL in it is CodeInvalid: C would cut it short.
func cString(op, s string) (*byte, error) {
	if strings.IndexByte(s, 0) >= 0 {
		return nil, codeError(media.CodeInvalid, op, "", "a string argument contains a NUL byte")
	}
	b := make([]byte, len(s)+1)
	copy(b, s)
	return &b[0], nil
}

// cPath is cString of a path option made absolute (the engine takes absolute paths only); "" is nil, the engine's
// default.
func cPath(op, option, path string) (*byte, error) {
	if path == "" {
		return nil, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, codeError(media.CodeInvalid, op, "", "Options.%s: %v", option, err)
	}
	return cString(op, abs)
}

// qpcToNS converts a QueryPerformanceCounter reading q at frequency f to nanoseconds, exactly as the engine does
// (04 §12.3): q / f × 1e9 + (q % f) × 1e9 / f, in integers, without overflow for any q while f < 9.2 GHz (Windows'
// QPC runs at 10 MHz).
func qpcToNS(q, f int64) int64 {
	return q/f*1e9 + q%f*1e9/f
}

// The snprintf rule's buffers (04 §12.4): the first try, and the largest retry Open's and the session's JSON
// exports accept. 05's report has its own, larger limit (05 §13.2).
const (
	jsonFirstCap = 64 << 10
	jsonMaxCap   = 1 << 20
)

// callJSON runs an export that writes JSON under the snprintf rule (04 §5.1): pre are the arguments in front of its
// (buf, cap) pair. The first try has a 64 KiB buffer (at most maxCap); when the returned length n is ≥ cap it retries
// once with n + 1 bytes. A document of maxCap bytes or more, or one that grew again between the two calls, is
// internal, and so is any other unexpected answer; a negative result is the status's error.
func callJSON(c caller, op string, p proc, maxCap int, pre ...uintptr) ([]byte, error) {
	size := min(jsonFirstCap, maxCap)
	for retried := false; ; retried = true {
		buf := make([]byte, size)
		var pn runtime.Pinner
		args := append(slices.Clip(pre), addrOf(&pn, &buf[0]), uintptr(size))
		r, lastErr := c.call(p, args...)
		pn.Unpin()
		n := int32(r)
		switch {
		case n < 0:
			return nil, statusError(op, p.String(), n, lastErr)
		case int(n) < size:
			return buf[:n], nil
		case retried:
			return nil, codeError(media.CodeInternal, op, "", "%s: the document grew to %d bytes between two calls",
				p, n)
		case int(n) >= maxCap:
			return nil, codeError(media.CodeInternal, op, "", "%s: a %d-byte document is over the %d-byte limit",
				p, n, maxCap)
		}
		size = int(n) + 1
	}
}

// readClock is im_clock.
func readClock(c caller, op string) (ClockSample, error) {
	s := &imClockSample{structSize: uint32(unsafe.Sizeof(imClockSample{}))}
	var pn runtime.Pinner
	r, lastErr := c.call(procClock, addrOf(&pn, s))
	pn.Unpin()
	if st := int32(r); st != imOK {
		return ClockSample{}, statusError(op, procClock.String(), st, lastErr)
	}
	return ClockSample{MonoNS: s.monoNS, UnixNS: s.unixNS, UncertaintyNS: s.uncertaintyNS}, nil
}

// clockSlack is how far im_clock's mono_ns may lie outside Go's QPC readings around the call (04 §12.3).
const clockSlack = int64(time.Millisecond)

// checkClock is Open's clock cross-check (04 §12.3): Go reads qpcNS before and after im_clock, and the engine's
// mono_ns must lie in [before − 1 ms, after + 1 ms]. Go's Session.Now and the engine's capture times would otherwise
// disagree, which breaks A/V sync and 01's sender-report anchor; that is internal, naming both readings.
func checkClock(c caller) error {
	before := c.qpcNS()
	s, err := readClock(c, "open")
	after := c.qpcNS()
	if err != nil {
		return err
	}
	if s.MonoNS < before-clockSlack || s.MonoNS > after+clockSlack {
		return codeError(media.CodeInternal, "open", "",
			"clock cross-check: im_clock mono_ns %d is outside Go's QPC readings [%d, %d] by more than 1 ms", s.MonoNS,
			before, after)
	}
	return nil
}

// readBuildInfo is im_build_info, decoded (04 §5.6).
func readBuildInfo(c caller, op string) (BuildInfo, error) {
	doc, err := callJSON(c, op, procBuildInfo, jsonMaxCap)
	if err != nil {
		return BuildInfo{}, err
	}
	var b BuildInfo
	if err := json.Unmarshal(doc, &b); err != nil {
		return BuildInfo{}, codeError(media.CodeInternal, op, "", "im_build_info: %v", err)
	}
	return b, nil
}

// checkVersion is Open's ABI check (04 §5.5): the DLL's major must be media.ABIVersion and its minor at least
// goMinMinor. A mismatch is dependency with Reason abi (01 §5.2, README §3 item 17: 03 shows "Reinstall isshoni"),
// naming both versions.
func checkVersion(v uint32) error {
	major, minor := v>>16, v&0xFFFF
	if major != media.ABIVersion || int64(minor) < goMinMinor {
		return codeError(media.CodeDependency, "open", tokenABI,
			"isshoni-media.dll speaks ABI %d.%d; this build needs %d.%d or a later minor (stale build?)", major, minor,
			media.ABIVersion, goMinMinor)
	}
	return nil
}

// resolveProcs looks every export up with lookup (GetProcAddress on Windows). Any missing one fails Open with
// dependency and Reason abi, naming each missing export ("stale build?", as S1 did): the DLL does not match this
// binding.
func resolveProcs(lookup func(name string) (uintptr, bool)) ([procCount]uintptr, error) {
	var addrs [procCount]uintptr
	var missing []string
	for p := range procCount {
		a, ok := lookup(procNames[p])
		if !ok || a == 0 {
			missing = append(missing, procNames[p])
			continue
		}
		addrs[p] = a
	}
	if len(missing) > 0 {
		return addrs, codeError(media.CodeDependency, "open", tokenABI, "isshoni-media.dll lacks %s (stale build?)",
			strings.Join(missing, ", "))
	}
	return addrs, nil
}

// engineDir is the absolute directory Open loads isshoni-media.dll from: dir, or the running executable's directory
// when dir is "". LoadLibraryEx's LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR needs a full path, so a relative dir is made
// absolute.
func engineDir(dir string, executable func() (string, error)) (string, error) {
	if dir == "" {
		exe, err := executable()
		if err != nil {
			return "", codeError(media.CodeInternal, "open", "", "os.Executable: %v", err)
		}
		dir = filepath.Dir(exe)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", codeError(media.CodeInternal, "open", "", "Options.Dir: %v", err)
	}
	return abs, nil
}

// logLevel maps Options.LogLevel to IM_LOG_*: the engine records everything at or above it.
func logLevel(l slog.Level) uint32 {
	switch {
	case l >= slog.LevelError:
		return imLogError
	case l >= slog.LevelWarn:
		return imLogWarn
	case l >= slog.LevelInfo:
		return imLogInfo
	}
	return imLogDebug
}

// Options configures Open (04 §12.2).
type Options struct {
	Dir          string       // directory of isshoni-media.dll; "" = the directory of os.Executable()
	FFmpegDir    string       // "" = Dir (im_init_params.ffmpeg_dir = NULL); tests and tools only (04 §13.3)
	OpenH264Path string       // "" = none; must have passed VerifyOpenH264
	Logger       *slog.Logger // engine logs with component=engine; nil = slog.Default()
	LogLevel     slog.Level   // mapped to IM_LOG_*
	NoAudio      bool         // IM_INIT_NO_AUDIO: the engine touches no audio API (video-only tools)
}

// Engine is the Windows media engine, opened by Open: a media.Engine and 05's media.AudioController (audio.go), with
// the Windows-only extras BuildInfo, Clock, SetOpenH264Path, ProtectWindow, SourceDetail and StartExtra, which 03
// and the tools reach by type assertion (04 §12.2). It is safe for concurrent use. There is one per process: the
// engine's state (im_init) is process-wide.
type Engine struct {
	c    caller
	log  *slog.Logger // component=engine
	info media.Info

	// life orders im_shutdown after every other call (04 §5.1: im_init and im_shutdown must not race other calls):
	// each call holds it for reading (begin, end), Close for writing. closed fails every begin once Close has
	// begun; closeOnce runs Close's body once, and every other Close waits for it.
	life      sync.RWMutex
	closed    atomic.Bool
	closeOnce sync.Once

	mu    sync.Mutex
	build BuildInfo // the last im_build_info read

	// The log drain (04 §12.3): drainLoop reads im_read_logs into log every second and right after an error; Close
	// stops it and drains once more after im_shutdown. logBuf and logErrAt belong to whichever of the two runs.
	logBuf   []byte
	logErrAt time.Time
	logKick  chan struct{}
	logStop  chan struct{}
	logDone  chan struct{}
}

// openEngine is Open's portable half, on a resolved caller: the ABI check, im_init, the clock cross-check
// (04 §12.3) and the build info; then it starts the log drain. A failure of im_init or after it drains the engine's
// log ring first (04 §12.3: right after any error), so its records about the failure reach the log. Any failure
// after im_init shuts the engine down again, so that a later Open can call im_init; a failed im_init is not shut
// down (nothing was initialized, and the engine of another Open may be running).
func openEngine(c caller, o Options) (*Engine, error) {
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "engine")

	r, _ := c.call(procVersion)
	v := uint32(r)
	if err := checkVersion(v); err != nil {
		return nil, err
	}

	params := &imInitParams{
		structSize: uint32(unsafe.Sizeof(imInitParams{})),
		abiVersion: imABIVersion,
		logLevel:   logLevel(o.LogLevel),
	}
	if o.NoAudio {
		params.flags |= imInitNoAudio
	}
	var err error
	if params.ffmpegDir, err = cPath("open", "FFmpegDir", o.FFmpegDir); err != nil {
		return nil, err
	}
	if params.openh264Path, err = cPath("open", "OpenH264Path", o.OpenH264Path); err != nil {
		return nil, err
	}
	e := &Engine{
		c:       c,
		log:     log,
		logBuf:  make([]byte, logBufSize),
		logKick: make(chan struct{}, 1),
		logStop: make(chan struct{}),
		logDone: make(chan struct{}),
	}
	var pn runtime.Pinner
	r, lastErr := c.call(procInit, addrOf(&pn, params))
	pn.Unpin()
	if st := int32(r); st != imOK {
		e.drainLogs()
		return nil, statusError("open", procInit.String(), st, lastErr)
	}

	err = checkClock(c)
	var b BuildInfo
	if err == nil {
		b, err = readBuildInfo(c, "open")
	}
	if err != nil {
		// The engine's own records say more about the failure; drain them around the shutdown.
		e.drainLogs()
		c.call(procShutdown)
		e.drainLogs()
		return nil, err
	}
	e.build = b
	e.info = media.Info{Name: "windows", ABI: int(v >> 16), Build: b.Engine}
	log.Info("engine opened", "abi", fmt.Sprintf("%d.%d", v>>16, v&0xFFFF), "build", b.Engine, "commit", b.Commit,
		"test_hooks", b.TestHooks)
	go e.drainLoop()
	return e, nil
}

// begin starts a DLL call on behalf of op; it fails without a call once Close has begun. end must follow a nil
// begin. Calls never nest begin.
func (e *Engine) begin(op string) error {
	e.life.RLock()
	if e.closed.Load() {
		e.life.RUnlock()
		return errClosed(op)
	}
	return nil
}

func (e *Engine) end() { e.life.RUnlock() }

// failed returns err and, when it is an error, has the log drain run now, so the engine's records about the failure
// reach the log right away (04 §12.3).
func (e *Engine) failed(err error) error {
	if err != nil {
		e.kickLogs()
	}
	return err
}

func (e *Engine) kickLogs() {
	select {
	case e.logKick <- struct{}{}:
	default:
	}
}

// Info describes the engine: {"windows", the ABI major, im_build_info().engine} (01 §5.1). It does no I/O.
func (e *Engine) Info() media.Info { return e.info }

// BuildInfo is im_build_info (04 §5.6), read again on every call: FFmpeg and OpenH264 load after Open. When the read
// fails it logs and returns the last good one, and after Close it returns that without a call.
func (e *Engine) BuildInfo() BuildInfo {
	if e.begin("build_info") != nil {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.build
	}
	b, err := readBuildInfo(e.c, "build_info")
	e.end()
	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil {
		e.log.Warn("engine build info unreadable; returning the last one", logx.Err(e.failed(err)))
		return e.build
	}
	e.build = b
	return b
}

// Clock is im_clock: the engine clock and the wall clock at one instant.
func (e *Engine) Clock() (ClockSample, error) {
	if err := e.begin("clock"); err != nil {
		return ClockSample{}, err
	}
	defer e.end()
	s, err := readClock(e.c, "clock")
	return s, e.failed(err)
}

// Release forgets an unused pick (im_release_source); other ids are a no-op (01 §5.1). Source ids are local data:
// never logged.
func (e *Engine) Release(sourceID string) {
	if sourceID == "" {
		return
	}
	id, err := cString("release", sourceID)
	if err != nil || e.begin("release") != nil {
		return
	}
	defer e.end()
	var pn runtime.Pinner
	defer pn.Unpin()
	e.c.call(procReleaseSource, addrOf(&pn, id))
}

// Close stops every running session, then im_shutdown (04 §5.3: ≤ 3 s); the DLL stays loaded (no FreeLibrary,
// ever). Later calls fail with internal/not_initialized without reaching the DLL. Idempotent: every Close, also one
// running concurrently with the first, returns only after the shutdown has finished, so a caller may exit then.
func (e *Engine) Close() error {
	e.closeOnce.Do(e.close)
	return nil
}

// cancelPickEvery is how often Close repeats im_cancel_pick while it waits for the calls in flight.
const cancelPickEvery = 20 * time.Millisecond

func (e *Engine) close() {
	e.closed.Store(true)
	// A blocked im_pick_source would hold the shutdown back for up to its timeout (120 s), and a Pick that passed
	// begin just before closed was set may enter it only after a first im_cancel_pick has run as a no-op: Close
	// cancels every cancelPickEvery until it holds life, and stops before im_shutdown.
	locked, cancelDone := make(chan struct{}), make(chan struct{})
	go e.cancelPicks(locked, cancelDone)
	// README WA23 stops the running sessions here (im_stop, ≤ 2 s each), before the shutdown.
	close(e.logStop)
	<-e.logDone
	e.life.Lock() // the calls in flight end first
	defer e.life.Unlock()
	close(locked)
	<-cancelDone
	e.drainLogs()
	e.c.call(procShutdown)
	e.drainLogs()
}

// cancelPicks calls im_cancel_pick (a no-op without a pick) at once and every cancelPickEvery until locked is
// closed; it closes done after its last call.
func (e *Engine) cancelPicks(locked <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	t := time.NewTicker(cancelPickEvery)
	defer t.Stop()
	for {
		e.c.call(procCancelPick)
		select {
		case <-locked:
			return
		case <-t.C:
		}
	}
}

// Probe reports what the PC can do: im_probe(FULL) on the first call and with refresh, cached otherwise, decoded
// as 04 §12.2's "im_caps → media.Probe". Not implemented yet (README WA23).
func (e *Engine) Probe(ctx context.Context, refresh bool) (media.Probe, error) {
	return media.Probe{}, notImplemented("probe")
}

// Sources lists the monitors (im_list_sources) as media.Source{ID: "mon:…", Kind: screen, Name, Width, Height}.
// Not implemented yet (README WA23).
func (e *Engine) Sources(ctx context.Context) ([]media.Source, error) {
	return nil, notImplemented("sources")
}

// Pick runs the OS picker (im_pick_source, on its own goroutine; ctx's end calls im_cancel_pick). A pick of an
// isshoni window is released and returns *media.Error{Code: invalid, Reason: "self_window"}. Not implemented yet
// (README WA23; the self-window rule WA70).
func (e *Engine) Pick(ctx context.Context, r media.PickRequest) (media.Source, error) {
	return media.Source{}, notImplemented("pick")
}

// Start starts a session (im_start) with 04 §12.2's media.Config mapping; media.ErrBusy while a session it started
// runs. Not implemented yet (README WA23).
func (e *Engine) Start(ctx context.Context, cfg media.Config) (media.Session, error) {
	return nil, notImplemented("start")
}

// StartExtra is Start without the one-session check (up to the DLL's 4 sessions, video only), for enginetest
// sessions only; the app never calls it. Not implemented yet (README WA23).
func (e *Engine) StartExtra(ctx context.Context, cfg media.Config) (media.Session, error) {
	return nil, notImplemented("start_extra")
}

// SourceDetail returns the im_source_info fields media.Source lacks for a source this engine listed or picked; ok
// is false for an unknown id. Until README WA23 keeps sources, every id is unknown.
func (e *Engine) SourceDetail(id string) (SourceDetail, bool) {
	return SourceDetail{}, false
}

// ProtectWindow is im_protect_window (04 §11): enginetest's protect-check only (the app's WDA is 03's winos). Not
// implemented yet (README WA70).
func (e *Engine) ProtectWindow(hwnd uintptr) error {
	return notImplemented("protect_window")
}

// The log drain (04 §5.6, §12.3).
const (
	logDrainInterval = time.Second
	logBufSize       = 64 << 10 // im_read_logs needs more than 8192 bytes
	logReadsPerDrain = 8        // one drain reads at most 8 buffers; the rest waits for the next one
	logErrEvery      = time.Minute
)

func (e *Engine) drainLoop() {
	defer close(e.logDone)
	t := time.NewTicker(logDrainInterval)
	defer t.Stop()
	for {
		select {
		case <-e.logStop:
			return
		case <-t.C:
		case <-e.logKick:
		}
		if e.begin("read_logs") != nil {
			return
		}
		e.drainLogs()
		e.end()
	}
}

// drainLogs moves the engine's log ring into the logger. Only drainLoop and, after it ended, Close and a failing
// openEngine call it.
func (e *Engine) drainLogs() {
	for range logReadsPerDrain {
		var pn runtime.Pinner
		r, lastErr := e.c.call(procReadLogs, addrOf(&pn, &e.logBuf[0]), uintptr(len(e.logBuf)))
		pn.Unpin()
		n := int32(r)
		if n < 0 {
			if now := time.Now(); now.Sub(e.logErrAt) >= logErrEvery {
				e.logErrAt = now
				e.log.Warn("engine logs unreadable", logx.Err(statusError("read_logs", procReadLogs.String(), n,
					lastErr)))
			}
			return
		}
		if n == 0 {
			return
		}
		e.logRecords(e.logBuf[:min(int(n), len(e.logBuf)-1)])
	}
}

// logRecords logs im_read_logs' NDJSON records through the engine logger's handler, with the engine's own time,
// level and message, its component as "area" and its attrs as they are. The records are private by the engine's
// rule (04 §5.6: no window title, no executable, process or app name at any level).
func (e *Engine) logRecords(b []byte) {
	h := e.log.Handler()
	ctx := context.Background()
	for len(b) > 0 {
		line := b
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			line, b = b[:i], b[i+1:]
		} else {
			b = nil
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		rec, ok := engineRecord(line)
		if !ok {
			e.log.Warn("engine log record unreadable", "bytes", len(line))
			continue
		}
		if h.Enabled(ctx, rec.Level) {
			_ = h.Handle(ctx, rec)
		}
	}
}

// engineLogLine is one im_read_logs record (04 §5.6).
type engineLogLine struct {
	T         string                     `json:"t"`
	Level     string                     `json:"level"`
	Component string                     `json:"component"`
	Msg       string                     `json:"msg"`
	Attrs     map[string]json.RawMessage `json:"attrs"`
	Dropped   int64                      `json:"dropped"` // records lost before this one (the ring was full)
}

// engineRecord turns one record into a slog.Record; ok is false when it is not a JSON object.
func engineRecord(line []byte) (rec slog.Record, ok bool) {
	var l engineLogLine
	if err := json.Unmarshal(line, &l); err != nil {
		return slog.Record{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, l.T)
	if err != nil {
		t = time.Now()
	}
	rec = slog.NewRecord(t, engineLevel(l.Level), l.Msg, 0)
	if l.Component != "" {
		rec.AddAttrs(slog.String("area", l.Component))
	}
	keys := make([]string, 0, len(l.Attrs))
	for k := range l.Attrs {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		rec.AddAttrs(slog.Any(k, attrValue(l.Attrs[k])))
	}
	if l.Dropped > 0 {
		rec.AddAttrs(slog.Int64("dropped", l.Dropped))
	}
	return rec, true
}

func engineLevel(s string) slog.Level {
	switch s {
	case "error":
		return slog.LevelError
	case "warn":
		return slog.LevelWarn
	case "debug":
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

// attrValue decodes one attr value: integers stay int64, or uint64 above math.MaxInt64 (the log ring's unsigned
// attrs), other numbers float64, the rest as encoding/json decodes it.
func attrValue(raw json.RawMessage) any {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return string(raw)
	}
	if n, ok := v.(json.Number); ok {
		if i, err := n.Int64(); err == nil {
			return i
		}
		if u, err := strconv.ParseUint(n.String(), 10, 64); err == nil {
			return u
		}
		if f, err := n.Float64(); err == nil {
			return f
		}
	}
	return v
}

// session is a running im_session, returned as a media.Session (04 §12.3). Next is for one goroutine; every other
// method may be called from any goroutine, also while Next blocks. README WA23 implements it; until then Start
// returns none.
type session struct {
	c       caller
	epochNS int64 // im_session_info.epoch_ns: the origin of Now and of every CaptureNS
}

// Next returns the next packet or event (04 §12.3). Not implemented yet (README WA23).
func (s *session) Next(ctx context.Context) (media.Packet, error) {
	return media.Packet{}, notImplemented("next")
}

// Now is qpcNS − epoch_ns: the clock of Packet.CaptureNS (04 §12.3).
func (s *session) Now() int64 { return s.c.qpcNS() - s.epochNS }

// RequestKeyframe is im_request_keyframe; unknown layers are ignored. Not implemented yet (README WA23): a no-op.
func (s *session) RequestKeyframe(layer string) {}

// SetBitrate: video → im_set_bitrate; 0 → im_set_layer_active(l, 0), the next non-zero value reactivating the layer;
// media.LayerAudio → im_set_audio_bitrate. Not implemented yet (README WA23).
func (s *session) SetBitrate(layer string, bps int) error { return notImplemented("set_bitrate") }

// Reconfigure applies 04 §12.2's order (im_set_profile, im_set_content_hint, im_update_layer or im_set_bitrate per
// layer). Not implemented yet (README WA23).
func (s *session) Reconfigure(r media.Reconfig) error { return notImplemented("reconfigure") }

// Stop runs im_stop once (≤ 2 s) and closes PCMTap; later calls return nil. Not implemented yet (README WA23).
func (s *session) Stop() error { return notImplemented("stop") }

// SetLayerActive is im_set_layer_active. Not implemented yet (README WA23).
func (s *session) SetLayerActive(layer string, active bool) error {
	return notImplemented("set_layer_active")
}

// SetPaused is im_set_paused (the video freezes; the mixer's pause flag). Not implemented yet (README WA23).
func (s *session) SetPaused(paused bool) error { return notImplemented("set_paused") }

// Stats is im_get_stats (04 §5.6), decoded. Not implemented yet (README WA23).
func (s *session) Stats() (media.EngineStats, error) {
	return media.EngineStats{}, notImplemented("stats")
}

// SetAudioBitrate is im_set_audio_bitrate (a Windows-only extra, by type assertion). Not implemented yet
// (README WA23).
func (s *session) SetAudioBitrate(bps int) error { return notImplemented("set_audio_bitrate") }

// PCMTap returns the session's PCM chunks (buffered 128, closed when the session stops) when it was started with
// media.AudioConfig.TapPCM, else nil (a Windows-only extra; cmd/isshoni-audiotest type-asserts
// interface{ PCMTap() <-chan native.PCMChunk }). Until README WA23 it is always nil.
func (s *session) PCMTap() <-chan PCMChunk { return nil }
