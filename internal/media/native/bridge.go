package native

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
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
// Close); README WA23 the rest of the engine (Probe, Sources, Pick, Start, StartExtra, SourceDetail, Close stopping
// the sessions) and the session (§12.3: Next with the event table and EventLayer synthesis, the copy and its limits,
// ENDED, Stop, the controls, Reconfigure, Stats). README WA35 routed PCM through Next (media.KindPCM, 05 §13.2) and
// moved the start config's audio half (the single mode mapping, the exclusions) into audio.go, 05's file, which also
// holds the audio controls. OpenH264 is in openh264.go (WA51), ProtectWindow is WA70's. session_windows.go holds the
// one Windows-only part of a pick (the executable name of its process).

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
	return callJSONWith(c, op, p, maxCap, statusError, pre...)
}

// callJSONWith is callJSON with the status → error mapping of the export's area: statusError, or 05's
// audioStatusError for the audio exports (audio.go), which share this buffer rule (05 §13.2).
func callJSONWith(c caller, op string, p proc, maxCap int, mapErr func(op, export string, st int32,
	lastErr string) *media.Error, pre ...uintptr) ([]byte, error) {
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
			return nil, mapErr(op, p.String(), n, lastErr)
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

	// The sessions (04 §12.2): startMu orders Start and StartExtra (im_start runs on the engine's one worker anyway);
	// smu guards the rest. primary is the session Start started while it runs (Start is ErrBusy meanwhile); sessions
	// holds every live one, which Close stops before im_shutdown; details are the SourceDetail of the sources this
	// engine listed or picked.
	startMu   sync.Mutex
	smu       sync.Mutex
	primary   *session
	sessions  map[*session]struct{}
	details   map[string]SourceDetail
	pickOrder []string // the picks in details, oldest first (maxPickDetails)

	// probeMu orders Probe; probe is the last full probe (nil before the first).
	probeMu sync.Mutex
	probe   *media.Probe

	audio engineAudio // 05's audio calls (audio.go)
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
		c:        c,
		log:      log,
		logBuf:   make([]byte, logBufSize),
		logKick:  make(chan struct{}, 1),
		logStop:  make(chan struct{}),
		logDone:  make(chan struct{}),
		sessions: map[*session]struct{}{},
		details:  map[string]SourceDetail{},
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
	e.forgetDetail(sourceID)
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
	// The running sessions stop first (im_stop, ≤ 2 s each; a blocked Next returns ErrStopped). A Start still in
	// flight stops its own session when it sees closed (start), before life lets the shutdown run.
	for _, s := range e.liveSessions() {
		s.stopping.Store(true)
		if err := s.free("close"); err != nil {
			e.log.Warn("engine: stopping a session at close failed", logx.Err(err))
		}
	}
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

// ---------------------------------------------------------------------------------------------------- the probe

// Probe reports what the PC can do, decoded as 04 §12.2's "im_caps → media.Probe": im_probe(IM_PROBE_FULL) on the
// first call and with refresh, the cached result otherwise. A full probe takes up to 2 s and can't be interrupted,
// so ctx is checked before it starts (its error is returned as it is, as Next returns it).
func (e *Engine) Probe(ctx context.Context, refresh bool) (media.Probe, error) {
	const op = "probe"
	e.probeMu.Lock()
	defer e.probeMu.Unlock()
	if e.probe != nil && !refresh {
		return cloneProbe(*e.probe), nil
	}
	if err := ctx.Err(); err != nil {
		return media.Probe{}, err
	}
	if err := e.begin(op); err != nil {
		return media.Probe{}, err
	}
	caps := &imCaps{structSize: uint32(unsafe.Sizeof(imCaps{}))}
	var pn runtime.Pinner
	r, lastErr := e.c.call(procProbe, imProbeFull, addrOf(&pn, caps))
	pn.Unpin()
	e.end()
	if st := int32(r); st != imOK {
		return media.Probe{}, e.failed(statusError(op, procProbe.String(), st, lastErr))
	}
	p := decodeCaps(caps)
	e.probe = &p
	return cloneProbe(p), nil
}

// decodeCaps is im_caps → media.Probe (04 §12.2). Encoders are every encoders[] entry in the engine's order,
// unavailable ones included; an entry of a kind this binding has no token for (a later DLL's) is left out.
func decodeCaps(c *imCaps) media.Probe {
	p := media.Probe{
		OS: media.OSInfo{
			Name:      "windows",
			Version:   fmt.Sprintf("10.0.%d.%d", c.osBuild, c.osUBR),
			Build:     int(c.osBuild),
			Supported: c.osFlags&imOSSupported != 0,
		},
		Capture: media.CaptureCaps{
			Picker:             c.osFlags&imOSWGCPicker != 0,
			Borderless:         c.osFlags&imOSWGCBorderless != 0,
			DesktopDuplication: c.osFlags&imOSDesktopDuplication != 0,
			ExcludeFromCapture: c.osFlags&imOSExcludeFromCapture != 0,
			MaxFPS:             60,
		},
		Audio: media.AudioCapsFromFlags(c.audioFlags),
	}
	if c.osFlags&imOSWGCMinInterval != 0 {
		p.Capture.MaxFPS = 240
	}
	for i := range min(int(c.encoderCount), imMaxEncoders) {
		ei := &c.encoders[i]
		name, ok := encoderToken(ei.kind)
		if !ok {
			continue
		}
		info := media.EncoderInfo{
			Name:        name,
			Adapter:     -1,
			HW:          ei.flags&imEncfHardware != 0,
			Tested:      ei.flags&imEncfAvailable != 0,
			LiveBitrate: ei.flags&imEncfLiveBitrate != 0,
			ZeroCopy:    ei.flags&imEncfZeroCopy != 0,
			Reason:      cBytes(ei.reason[:]),
		}
		if ei.adapterIndex != 0xFFFFFFFF {
			info.Adapter = int(ei.adapterIndex)
		}
		if ei.flags&imEncfHigh != 0 {
			info.Profiles = append(info.Profiles, profileHigh)
		}
		if ei.flags&imEncfCB != 0 {
			info.Profiles = append(info.Profiles, profileCB)
		}
		if !info.Tested {
			info.Status = media.CodeInternal // not available, yet IM_OK: the engine didn't say why
			if ei.status != imOK {
				info.Status = statusCode(ei.status)
			}
		}
		p.Encoders = append(p.Encoders, info)
	}
	return p
}

// cloneProbe copies p's slices, so a caller can't change the cached probe.
func cloneProbe(p media.Probe) media.Probe {
	p.Encoders = slices.Clone(p.Encoders)
	for i := range p.Encoders {
		p.Encoders[i].Profiles = slices.Clone(p.Encoders[i].Profiles)
	}
	return p
}

// ---------------------------------------------------------------------------------------------------- sources

// The profile keys of 04 §12.2 (01 §8.6): the session profiles the engine runs, and High's SDP alias.
const (
	profileHigh      = "6400"
	profileHighAlias = "640c"
	profileCB        = "42e0"
)

// sourcesDoc is im_list_sources' JSON (04 §5.6): monitors only on Windows ("windows" stays empty).
type sourcesDoc struct {
	Monitors []struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
	} `json:"monitors"`
}

// Sources lists the monitors (im_list_sources) as media.Source{ID: "mon:…", Kind: screen, Name, Width, Height},
// with MonitorID their own id; starting one needs media.Config.Listed. Source names are local data: never logged.
func (e *Engine) Sources(ctx context.Context) ([]media.Source, error) {
	const op = "sources"
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := e.begin(op); err != nil {
		return nil, err
	}
	doc, err := callJSON(e.c, op, procListSources, jsonMaxCap, 0)
	e.end()
	if err != nil {
		return nil, e.failed(err)
	}
	var d sourcesDoc
	if err := json.Unmarshal(doc, &d); err != nil {
		return nil, codeError(media.CodeInternal, op, "", "im_list_sources: %v", err)
	}
	out := make([]media.Source, 0, len(d.Monitors))
	e.smu.Lock()
	defer e.smu.Unlock()
	for _, m := range d.Monitors {
		out = append(out, media.Source{ID: m.ID, Kind: media.SourceScreen, Name: m.Name, Width: m.Width,
			Height: m.Height, MonitorID: m.ID})
		e.details[m.ID] = SourceDetail{} // a listed monitor has nothing beyond its media.Source
	}
	return out, nil
}

// exeOfPID is the base name of a process's image (QueryFullProcessImageNameW, session_windows.go), "" when it can't
// be read or off Windows; media.Source.Exe of a window pick (01 §5.1). Local data: never logged.
var exeOfPID = func(pid uint32) string { return "" }

// maxPickTimeout is im_pick_params.timeout_ms's limit (04 §6.4).
const maxPickTimeout = 600 * time.Second

// Pick runs the OS picker (im_pick_source) on its own goroutine, never on the caller's (03 calls it from the Wails
// UI's handler, 04 §12.4); ctx's end calls im_cancel_pick, after which the pick returns cancelled. The result is
// media.Source{ID, Kind, Name, Width, Height, PID, Exe, MonitorID, Capture, OSBorder} (04 §12.2). A pick of an
// isshoni window (IM_SRCF_SELF) is released at once and returns *media.Error{Code: invalid, Reason: "self_window"}
// (README §3 item 17), so the app opens the picker again.
func (e *Engine) Pick(ctx context.Context, req media.PickRequest) (media.Source, error) {
	const op = "pick"
	if req.Timeout < 0 || req.Timeout > maxPickTimeout {
		return media.Source{}, codeError(media.CodeInvalid, op, "", "PickRequest.Timeout %v is outside 0..%v",
			req.Timeout, maxPickTimeout)
	}
	if err := ctx.Err(); err != nil {
		return media.Source{}, err
	}
	if err := e.begin(op); err != nil {
		return media.Source{}, err
	}
	defer e.end()
	params := &imPickParams{
		structSize:   uint32(unsafe.Sizeof(imPickParams{})),
		parentWindow: req.OwnerWindow,
		timeoutMS:    uint32(req.Timeout / time.Millisecond),
	}
	info := &imSourceInfo{structSize: uint32(unsafe.Sizeof(imSourceInfo{}))}
	type result struct {
		st      int32
		lastErr string
	}
	done := make(chan result, 1)
	go func() {
		var pn runtime.Pinner
		r, lastErr := e.c.call(procPickSource, addrOf(&pn, params), addrOf(&pn, info))
		pn.Unpin()
		done <- result{int32(r), lastErr}
	}()
	var res result
	select {
	case res = <-done:
	case <-ctx.Done():
		e.c.call(procCancelPick)
		res = <-done
	}
	if res.st != imOK {
		return media.Source{}, e.failed(statusError(op, procPickSource.String(), res.st, res.lastErr))
	}
	src, d := decodeSource(info)
	if info.flags&imSrcfSelf != 0 {
		if id, err := cString(op, src.ID); err == nil {
			var pn runtime.Pinner
			e.c.call(procReleaseSource, addrOf(&pn, id))
			pn.Unpin()
		}
		return media.Source{}, &media.Error{Code: media.CodeInvalid, Op: op, Reason: tokenSelfWindow,
			Detail: "the user picked an isshoni window; the pick was released"}
	}
	e.rememberPick(src.ID, d)
	return src, nil
}

// decodeSource is im_source_info → media.Source and the SourceDetail of 04 §12.2.
func decodeSource(info *imSourceInfo) (media.Source, SourceDetail) {
	kind := media.SourceScreen // IM_SOURCE_MONITOR and IM_SOURCE_TEST_PATTERN
	if info.kind == imSourceWindow {
		kind = media.SourceWindow
	}
	capture := captureToken(info.capturePath)
	src := media.Source{
		ID:        cBytes(info.id[:]),
		Kind:      kind,
		Name:      cBytes(info.name[:]),
		Width:     int(info.width),
		Height:    int(info.height),
		PID:       int(info.pid),
		MonitorID: cBytes(info.monitorID[:]),
		Capture:   capture,
		OSBorder:  info.flags&imSrcfOSBorder != 0,
	}
	if info.pid != 0 {
		src.Exe = exeOfPID(info.pid)
	}
	d := SourceDetail{
		Mapped: info.flags&imSrcfMapped != 0,
		Window: uintptr(info.window),
	}
	return src, d
}

// captureToken is IM_CAPTURE_* → "wgc", "dd", "test" (media.Source.Capture); "" for the other platforms' paths.
func captureToken(path uint32) string {
	switch path {
	case imCaptureWGC:
		return "wgc"
	case imCaptureDD:
		return "dd"
	case imCaptureTest:
		return "test"
	}
	return ""
}

// maxPickDetails bounds the picks SourceDetail remembers (unused picks live 5 min and at most 4 are kept, 04 §7.2).
const maxPickDetails = 16

// rememberPick keeps a pick's SourceDetail; the oldest picks go beyond maxPickDetails.
func (e *Engine) rememberPick(id string, d SourceDetail) {
	e.smu.Lock()
	defer e.smu.Unlock()
	e.details[id] = d
	e.pickOrder = append(e.pickOrder, id)
	for len(e.pickOrder) > maxPickDetails {
		delete(e.details, e.pickOrder[0])
		e.pickOrder = e.pickOrder[1:]
	}
}

// forgetDetail drops a released pick's SourceDetail.
func (e *Engine) forgetDetail(id string) {
	e.smu.Lock()
	defer e.smu.Unlock()
	if i := slices.Index(e.pickOrder, id); i >= 0 {
		e.pickOrder = slices.Delete(e.pickOrder, i, i+1)
		delete(e.details, id)
	}
}

// SourceDetail returns the im_source_info fields media.Source lacks for a source this engine listed (Sources) or
// picked (Pick, until its release); ok is false for any other id. 03 places the pill on media.Source.MonitorID.
func (e *Engine) SourceDetail(id string) (SourceDetail, bool) {
	e.smu.Lock()
	defer e.smu.Unlock()
	d, ok := e.details[id]
	return d, ok
}

// ---------------------------------------------------------------------------------------------------- start

// Start starts a session (im_start) with 04 §12.2's media.Config mapping. One session at a time: while a session
// Start started runs (until its Stop, or until Next returned its end), another Start is media.ErrBusy (01 §5.3,
// Conformance check 9); the DLL's own limits (4 sessions, 1 with audio) are busy too. im_start takes up to 3 s and
// can't be interrupted, so ctx is checked before it starts.
func (e *Engine) Start(ctx context.Context, cfg media.Config) (media.Session, error) {
	return e.start(ctx, "start", cfg, false)
}

// StartExtra is Start without the one-session check, for enginetest's sessions only (the app never calls it): up to
// the DLL's 4 sessions, video only (audio is one per process), so a cfg with Audio is invalid (04 §12.2).
func (e *Engine) StartExtra(ctx context.Context, cfg media.Config) (media.Session, error) {
	const op = "start_extra"
	if cfg.Audio != nil {
		return nil, codeError(media.CodeInvalid, op, "", "StartExtra runs video-only sessions (Audio must be nil)")
	}
	return e.start(ctx, op, cfg, true)
}

func (e *Engine) start(ctx context.Context, op string, cfg media.Config, extra bool) (media.Session, error) {
	args, err := buildStart(op, cfg)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.startMu.Lock()
	defer e.startMu.Unlock()
	if !extra {
		e.smu.Lock()
		busy := e.primary != nil
		e.smu.Unlock()
		if busy {
			return nil, &media.Error{Code: media.CodeBusy, Op: op,
				Detail: "a session started by Start still runs (one at a time, 01 §5.3)"}
		}
	}
	if err := e.begin(op); err != nil {
		return nil, err
	}
	defer e.end()
	var h uint64
	info := &imSessionInfo{structSize: uint32(unsafe.Sizeof(imSessionInfo{}))}
	var pn runtime.Pinner
	cfgAddr := args.pin(&pn)
	r, lastErr := e.c.call(procStart, cfgAddr, addrOf(&pn, &h), addrOf(&pn, info))
	pn.Unpin()
	runtime.KeepAlive(args)
	if st := int32(r); st != imOK {
		return nil, e.failed(statusError(op, procStart.String(), st, lastErr))
	}
	s := newSession(e, h, info, cfg, args.sc)
	e.smu.Lock()
	if e.closed.Load() {
		// Close began while im_start ran: it stopped the sessions it knew of, so this one stops itself, before life
		// lets im_shutdown run.
		e.smu.Unlock()
		s.stopping.Store(true)
		_ = s.free(op)
		return nil, errClosed(op)
	}
	e.sessions[s] = struct{}{}
	if !extra {
		e.primary = s
	}
	e.smu.Unlock()
	e.log.Info("engine session started", "layers", len(s.rids), "audio", s.hasAudio, "extra", extra)
	return s, nil
}

// liveSessions is every session that hasn't been stopped yet.
func (e *Engine) liveSessions() []*session {
	e.smu.Lock()
	defer e.smu.Unlock()
	out := make([]*session, 0, len(e.sessions))
	for s := range e.sessions {
		out = append(out, s)
	}
	return out
}

// forget drops a stopped session; a Start may follow once Start's own session is gone.
func (e *Engine) forget(s *session) {
	e.smu.Lock()
	defer e.smu.Unlock()
	delete(e.sessions, s)
	if e.primary == s {
		e.primary = nil
	}
}

// startArgs is im_start's input in Go memory: the struct and every string and array it points to (04 §12.4).
type startArgs struct {
	sc   *imStartConfig
	ex   *cExclusions // 05's exclusions (audio.go), nil without
	strs []*byte      // source_id
}

// pin pins everything the struct points to for the one im_start call and returns the struct's address.
func (a *startArgs) pin(pn *runtime.Pinner) uintptr {
	for _, p := range a.strs {
		pn.Pin(p)
	}
	if a.ex != nil {
		a.ex.pin(pn)
	}
	return addrOf(pn, a.sc)
}

func (a *startArgs) str(op, s string) (*byte, error) {
	p, err := cString(op, s)
	if err != nil {
		return nil, err
	}
	a.strs = append(a.strs, p)
	return p, nil
}

// maxSourceID is im_source_info.id's size: an id fits with its NUL.
const maxSourceID = 64

// buildStart is media.Config → im_start_config (04 §12.2's table). Anything the engine would refuse that Go can tell
// is CodeInvalid without a DLL call.
func buildStart(op string, cfg media.Config) (*startArgs, error) {
	a := &startArgs{sc: &imStartConfig{structSize: uint32(unsafe.Sizeof(imStartConfig{}))}}
	sc := a.sc
	if cfg.Source == "" {
		if len(cfg.Layers) != 0 || cfg.Audio == nil {
			return nil, codeError(media.CodeInvalid, op, "",
				"Config.Source \"\" is an audio-only session: no Layers, and Audio set")
		}
	} else {
		if len(cfg.Layers) == 0 {
			return nil, codeError(media.CodeInvalid, op, "", "Config.Layers is empty: a source needs a video layer")
		}
		if len(cfg.Source) >= maxSourceID {
			return nil, codeError(media.CodeInvalid, op, "", "Config.Source is longer than a source id")
		}
		p, err := a.str(op, cfg.Source)
		if err != nil {
			return nil, err
		}
		sc.sourceID = p
	}
	if cfg.Listed {
		sc.flags |= imStartAllowListedSource
	}
	if !cfg.Borderless {
		sc.flags |= imStartKeepBorder
	}
	// IM_START_HIDE_CURSOR is never set: shares show the pointer.
	kind, ok := encoderKind(cfg.Encoder)
	if !ok {
		return nil, codeError(media.CodeInvalid, op, "", "Config.Encoder %q is not an encoder token", cfg.Encoder)
	}
	sc.encoder = kind
	if len(cfg.Layers) > 0 {
		p, ok := profileValue(cfg.Profile)
		if !ok {
			return nil, codeError(media.CodeInvalid, op, "", "Config.Profile %q is not 6400 or 42e0", cfg.Profile)
		}
		sc.profile = p
	}
	hint, ok := contentHint(cfg.Preset)
	if !ok {
		return nil, codeError(media.CodeInvalid, op, "", "Config.Preset %q is not auto, game, movie or text", cfg.Preset)
	}
	sc.contentHint = hint
	if len(cfg.Layers) > imMaxLayers {
		return nil, codeError(media.CodeInvalid, op, "", "%d layers: at most %d", len(cfg.Layers), imMaxLayers)
	}
	for i, l := range cfg.Layers {
		lc, err := layerConfig(op, i, l)
		if err != nil {
			return nil, err
		}
		for k := range i {
			if cfg.Layers[k].RID == l.RID {
				return nil, codeError(media.CodeInvalid, op, "", "Config.Layers[%d] repeats rid %q", i, l.RID)
			}
		}
		sc.layers[i] = lc
	}
	sc.layerCount = uint32(len(cfg.Layers))
	if cfg.Audio != nil {
		// 05's half of the table: the single mode mapping and the exclusions (audio.go).
		ac, ex, err := buildAudioConfig(op, cfg.Audio)
		if err != nil {
			return nil, err
		}
		sc.audio = ac
		if ex != nil {
			a.ex = ex
			sc.exclusions = &ex.ex
		}
	}
	return a, nil
}

// layerConfig is one media.LayerConfig → im_layer_config: rid, max_pixels, fps = MaxFPS, bitrate_bps = Bitrate,
// gop_ms 0 (the engine's defaults, 01 §5.1), IM_LAYER_INACTIVE when Inactive; width and height 0.
func layerConfig(op string, i int, l media.LayerConfig) (imLayerConfig, error) {
	var lc imLayerConfig
	if l.RID == "" || len(l.RID) >= len(lc.rid) || strings.IndexByte(l.RID, 0) >= 0 {
		return lc, codeError(media.CodeInvalid, op, "", "Config.Layers[%d].RID %q: 1 to 3 bytes", i, l.RID)
	}
	if l.MaxFPS < 1 || l.MaxFPS > maxFPS {
		return lc, codeError(media.CodeInvalid, op, "", "Config.Layers[%d].MaxFPS %d is outside 1..%d", i, l.MaxFPS,
			maxFPS)
	}
	if l.MaxPixels < 0 || int64(l.MaxPixels) > math.MaxUint32 {
		return lc, codeError(media.CodeInvalid, op, "", "Config.Layers[%d].MaxPixels %d", i, l.MaxPixels)
	}
	if l.Bitrate != 0 && !validVideoBitrate(i, l.Bitrate) {
		return lc, codeError(media.CodeInvalid, op, "", "Config.Layers[%d].Bitrate %d is outside the layer's range "+
			"(04 §6.4)", i, l.Bitrate)
	}
	copy(lc.rid[:], l.RID)
	lc.maxPixels = uint32(l.MaxPixels)
	lc.fps = uint32(l.MaxFPS)
	lc.bitrateBPS = uint32(l.Bitrate)
	if l.Inactive {
		lc.flags |= imLayerInactive
	}
	return lc, nil
}

// The control limits of 04 §6.4.
const (
	maxFPS             = 240
	minBitrateLayer0   = 100_000
	maxBitrateLayer0   = 50_000_000
	minBitrateLayer    = 50_000
	maxBitrateLayer    = 2_000_000
	minAudioBitrateBPS = 32_000
	maxAudioBitrateBPS = 510_000
)

// validVideoBitrate is 04 §6.4's range for the layer at index i: "f" (index 0) 100 kbit/s–50 Mbit/s, the others
// 50 kbit/s–2 Mbit/s.
func validVideoBitrate(i, bps int) bool {
	if i == 0 {
		return bps >= minBitrateLayer0 && bps <= maxBitrateLayer0
	}
	return bps >= minBitrateLayer && bps <= maxBitrateLayer
}

// profileValue maps a profile key to IM_PROFILE_*: "6400" and its SDP alias "640c" are High (the engine never sets
// constraint_set4/5, so packets and EventLayer say 6400), "42e0" is Constrained Baseline.
func profileValue(key string) (uint32, bool) {
	switch key {
	case profileHigh, profileHighAlias:
		return imProfileHigh, true
	case profileCB:
		return imProfileCB, true
	}
	return 0, false
}

// profileKey is im_layer_info.profile → the key media.Event.Profile carries ("6400", "42e0").
func profileKey(p uint32) string { return fmt.Sprintf("%04x", p&0xFFFF) }

// contentHint maps a preset to IM_CONTENT_*: auto ("" too) → AUTO, game and movie → MOTION, text → DETAIL.
func contentHint(preset string) (uint32, bool) {
	switch preset {
	case "", "auto":
		return imContentAuto, true
	case "game", "movie":
		return imContentMotion, true
	case "text":
		return imContentDetail, true
	}
	return 0, false
}

// ---------------------------------------------------------------------------------------------------- the session

// session is a running im_session, returned as a media.Session (04 §12.3). Next is for one goroutine; every other
// method may be called from any goroutine, also while Next blocks.
//
// Locks: pull is held from each im_next_packet through the copy of its packet, and by im_stop, so the engine never
// frees a slot Go is still copying; ctl orders the control calls among themselves (01 §7.1 item 4), never
// im_next_packet; smu guards freed. Next's own state (queue, startPending, lastLayer, dropWarnAt) belongs to the one
// goroutine that calls Next.
type session struct {
	e       *Engine
	c       caller
	log     *slog.Logger
	h       uint64
	epochNS int64 // im_session_info.epoch_ns: the origin of Now and of every CaptureNS

	rids          []string // layer index → rid
	hasAudio      bool
	noOSIndicator bool

	ctl      sync.Mutex
	layerCfg []imLayerConfig // each layer's current config, for Reconfigure's im_update_layer

	pull     sync.Mutex
	stopping atomic.Bool // Stop, Close or the session's end: Next returns ErrStopped from here on

	queue        []media.Packet // Next's: ready to return (the start's EventLayers, a held Reinit packet)
	startPending []bool         // Next's: the layer's first packet hasn't come yet (its EventLayer went at start)
	lastLayer    []media.Event  // Next's: each layer's last EventLayer
	dropWarnAt   time.Time      // Next's: the last warning about a dropped packet

	tapPCM bool // AudioConfig.TapPCM: IM_PACKET_PCM chunks come out of Next as media.KindPCM (05 §13.2)

	smu   sync.Mutex
	freed bool // im_stop ran: the handle never reaches the DLL again
}

// The session's limits (04 §12.3): the pull timeout, and each packet kind's largest size (the f arena; one Opus
// packet with the Marker framing; one 10 ms PCM chunk; an event's JSON).
const (
	pullTimeoutMS = 50
	maxVideoBytes = 8 << 20
	maxAudioBytes = 4096
	pcmChunkBytes = 3840
	maxEventBytes = 1024
	dropWarnEvery = time.Second
)

// maxPacketBytes is the size limit of a packet kind (0: a kind Next doesn't take).
func maxPacketBytes(kind uint32) int {
	switch kind {
	case imPacketVideo, imPacketDebug:
		return maxVideoBytes
	case imPacketAudio:
		return maxAudioBytes
	case imPacketPCM:
		return pcmChunkBytes
	case imPacketEvent:
		return maxEventBytes
	}
	return 0
}

// newSession is the session of a successful im_start: one EventLayer per layer from info, queued for Next before any
// media (04 §12.3).
func newSession(e *Engine, h uint64, info *imSessionInfo, cfg media.Config, sc *imStartConfig) *session {
	n := len(cfg.Layers)
	s := &session{
		e:             e,
		c:             e.c,
		log:           e.log,
		h:             h,
		epochNS:       info.epochNS,
		rids:          make([]string, n),
		hasAudio:      cfg.Audio != nil,
		noOSIndicator: info.flags&imSessNoOSIndicator != 0,
		layerCfg:      slices.Clone(sc.layers[:n]),
		startPending:  make([]bool, n),
		lastLayer:     make([]media.Event, n),
	}
	for i, l := range cfg.Layers {
		s.rids[i] = l.RID
	}
	s.tapPCM = cfg.Audio != nil && cfg.Audio.TapPCM
	for i := range n {
		ev := s.layerEvent(info, i)
		s.lastLayer[i] = ev
		s.startPending[i] = true
		s.queue = append(s.queue, media.Packet{Kind: media.KindEvent, Event: &ev})
	}
	return s
}

// layerEvent is the EventLayer of layer i from a session info (04 §12.3's table).
func (s *session) layerEvent(info *imSessionInfo, i int) media.Event {
	li := &info.layers[i]
	ev := media.Event{
		Code:          media.EventLayer,
		Layer:         s.rids[i],
		Width:         int(li.width),
		Height:        int(li.height),
		FPS:           int(li.fps),
		HW:            li.flags&imEncfHardware != 0,
		Profile:       profileKey(li.profile),
		LiveBitrate:   li.flags&imEncfLiveBitrate != 0,
		NoOSIndicator: info.flags&imSessNoOSIndicator != 0,
	}
	ev.Encoder, _ = encoderToken(li.encoder)
	return ev
}

// errStopped is Next's (and the controls') answer once the session is stopped or ended.
func errStopped(op string) *media.Error { return &media.Error{Code: media.CodeStopped, Op: op} }

// Next returns the next packet or event (04 §12.3): it pulls with im_next_packet(s, 50 ms) in a loop, returns ctx's
// error once ctx is done (within one pull), copies every payload into fresh memory, maps events, synthesizes
// EventLayer before each Reinit packet (holding the packet for the next call), returns PCM chunks as media.KindPCM
// packets when the session was started with AudioConfig.TapPCM (05 §13.2), and turns the
// ENDED event into its error (the binding then frees the session with im_stop itself). After Stop, Close or the end
// it returns media.ErrStopped without calling the DLL.
func (s *session) Next(ctx context.Context) (media.Packet, error) {
	const op = "next"
	for {
		if s.stopping.Load() {
			s.queue = nil // what was held for the next call goes with the session
			return media.Packet{}, errStopped(op)
		}
		if len(s.queue) > 0 {
			p := s.queue[0]
			s.queue[0] = media.Packet{}
			s.queue = s.queue[1:]
			if len(s.queue) == 0 {
				s.queue = nil
			}
			return p, nil
		}
		if err := ctx.Err(); err != nil {
			return media.Packet{}, err
		}
		pk, ok := s.pullOne()
		if !ok {
			return media.Packet{}, errStopped(op)
		}
		switch pk.st {
		case imTimeout:
			continue
		case imOK:
			p, deliver, err := s.handle(&pk)
			if err != nil {
				return media.Packet{}, err
			}
			if deliver {
				return p, nil
			}
		case imEnded:
			// IM_ENDED without the ENDED event first (the engine always queues one, §6.3): internal, defensively.
			return media.Packet{}, s.end(&media.Error{Code: media.CodeInternal, Op: op,
				Detail: "im_next_packet: the session ended without an ENDED event"})
		default:
			if pk.st == imErrInvalidArg && s.stopping.Load() {
				return media.Packet{}, errStopped(op) // racing a Stop
			}
			// The handle is gone, or the engine refuses the session: it can't deliver anything any more.
			return media.Packet{}, s.end(statusError(op, procNextPacket.String(), pk.st, pk.lastErr))
		}
	}
}

// pulled is one im_next_packet result with its payload copied.
type pulled struct {
	p         imPacket
	data      []byte
	oversized bool // larger than its kind's limit: not copied
	st        int32
	lastErr   string
}

// pullOne runs one im_next_packet under the pull lock and copies the payload before the lock goes (the slot stays
// valid until the next call or im_stop, 04 §5.1). ok is false when the session is stopping or the engine closed.
func (s *session) pullOne() (pk pulled, ok bool) {
	if s.e.begin("next") != nil {
		return pk, false
	}
	defer s.e.end()
	s.pull.Lock()
	defer s.pull.Unlock()
	if s.stopping.Load() {
		return pk, false
	}
	pk.p.structSize = uint32(unsafe.Sizeof(imPacket{}))
	var pn runtime.Pinner
	r, lastErr := s.c.call(procNextPacket, uintptr(s.h), pullTimeoutMS, addrOf(&pn, &pk.p))
	pn.Unpin()
	pk.st, pk.lastErr = int32(r), lastErr
	if pk.st == imOK && pk.p.size > 0 {
		if int64(pk.p.size) <= int64(maxPacketBytes(pk.p.kind)) {
			pk.data = bytes.Clone(nativeBytes(pk.p.data, int(pk.p.size)))
		} else {
			pk.oversized = true
		}
	}
	return pk, true
}

// nativeBytes is the n bytes at the engine address a (valid until the next call on the session), aliased for the
// copy that follows at once. The address is reinterpreted rather than converted: a uintptr → unsafe.Pointer
// conversion is what vet's unsafeptr check and -race's checkptr flag, and neither concerns memory the Go heap doesn't
// own (04 §12.4: im_packet.data is a uintptr so the collector never sees a pointer into native memory).
func nativeBytes(a uintptr, n int) []byte {
	if a == 0 || n <= 0 {
		return nil
	}
	return unsafe.Slice((*byte)(*(*unsafe.Pointer)(unsafe.Pointer(&a))), n)
}

// handle turns one packet into what Next returns: deliver is false for a packet that is consumed here (a
// dropped or unmapped packet), err is set for the session's end.
func (s *session) handle(pk *pulled) (p media.Packet, deliver bool, err error) {
	switch pk.p.kind {
	case imPacketVideo:
		return s.video(pk)
	case imPacketAudio:
		return s.audio(pk)
	case imPacketPCM:
		return s.pcm(pk)
	case imPacketEvent:
		return s.event(pk)
	case imPacketDebug:
		// The luma tap of IM_TEST_HOOKS builds (README WA52 adds its hook): nobody listens yet.
		return media.Packet{}, false, nil
	}
	s.dropped("an unknown packet kind", pk, -1)
	return media.Packet{}, false, nil
}

// dropped logs a packet Next refuses (at warn, at most once a second, with its size; no payload) and, for a video
// layer (layer ≥ 0), asks for a keyframe: the engine's next IDR repairs the gap (04 §12.3).
func (s *session) dropped(why string, pk *pulled, layer int) {
	if now := time.Now(); now.Sub(s.dropWarnAt) >= dropWarnEvery {
		s.dropWarnAt = now
		s.log.Warn("engine packet dropped", "why", why, "kind", pk.p.kind, "layer", pk.p.layer, "bytes", pk.p.size)
	}
	if layer >= 0 {
		s.RequestKeyframe(s.rids[layer])
	}
}

// captureNS is the packet's CaptureNS: capture_ts_ns − epoch_ns (04 §12.3), ≥ 0 for every kind.
func (s *session) captureNS(pk *pulled) int64 { return pk.p.captureTSNS - s.epochNS }

func (s *session) video(pk *pulled) (media.Packet, bool, error) {
	idx := int(pk.p.layer)
	if pk.p.layer >= uint32(len(s.rids)) {
		s.dropped("a video packet of an unknown layer", pk, -1)
		return media.Packet{}, false, nil
	}
	switch {
	case pk.oversized:
		s.dropped("a video packet over 8 MiB", pk, idx)
		return media.Packet{}, false, nil
	case len(pk.data) == 0:
		s.dropped("an empty video packet", pk, idx)
		return media.Packet{}, false, nil
	case s.captureNS(pk) < 0:
		s.dropped("a video packet captured before the session's epoch", pk, idx)
		return media.Packet{}, false, nil
	}
	p := media.Packet{
		Kind:      media.KindVideo,
		Layer:     s.rids[idx],
		Keyframe:  pk.p.flags&imPktKeyframe != 0,
		Reinit:    pk.p.flags&imPktReinit != 0,
		CaptureNS: s.captureNS(pk),
		Data:      pk.data,
	}
	first := s.startPending[idx]
	s.startPending[idx] = false
	if !p.Reinit {
		return p, true, nil
	}
	// Before each Reinit packet, the layer's EventLayer from a fresh session info (the packet is held for the next
	// call). The layer's first packet needs none when nothing changed since the start's EventLayer.
	ev, err := s.readLayerEvent(idx)
	if err != nil {
		if s.stopping.Load() {
			return media.Packet{}, false, errStopped("next")
		}
		s.log.Warn("engine session info unreadable before a re-init", logx.Err(err))
		return p, true, nil
	}
	if first && ev == s.lastLayer[idx] {
		return p, true, nil
	}
	s.lastLayer[idx] = ev
	s.queue = append(s.queue, p)
	return media.Packet{Kind: media.KindEvent, Event: &ev}, true, nil
}

// readLayerEvent is layer i's EventLayer from im_get_session_info.
func (s *session) readLayerEvent(i int) (media.Event, error) {
	const op = "next"
	if err := s.e.begin(op); err != nil {
		return media.Event{}, err
	}
	defer s.e.end()
	info := &imSessionInfo{structSize: uint32(unsafe.Sizeof(imSessionInfo{}))}
	var pn runtime.Pinner
	r, lastErr := s.c.call(procGetSessionInfo, uintptr(s.h), addrOf(&pn, info))
	pn.Unpin()
	if st := int32(r); st != imOK {
		return media.Event{}, statusError(op, procGetSessionInfo.String(), st, lastErr)
	}
	if i >= int(info.layerCount) || i >= imMaxLayers {
		return media.Event{}, codeError(media.CodeInternal, op, "", "im_get_session_info has %d layers, not %d",
			info.layerCount, i+1)
	}
	return s.layerEvent(info, i), nil
}

func (s *session) audio(pk *pulled) (media.Packet, bool, error) {
	switch {
	case pk.oversized || len(pk.data) == 0:
		s.dropped("an audio packet that is empty or over 4096 bytes", pk, -1)
		return media.Packet{}, false, nil
	case s.captureNS(pk) < 0:
		s.dropped("an audio packet captured before the session's epoch", pk, -1)
		return media.Packet{}, false, nil
	}
	// DISCONTINUITY has no media.Packet field: the grid is kept, and Go's RTP timestamps keep the gap (04 §12.3).
	return media.Packet{Kind: media.KindAudio, Reinit: pk.p.flags&imPktReinit != 0, CaptureNS: s.captureNS(pk),
		Data: pk.data}, true, nil
}

// pcm turns an IM_PACKET_PCM chunk into a media.KindPCM packet (05 §13.2, 01 §5.1): 10 ms of the mix fed to Opus,
// 480 interleaved stereo float32 frames (3840 bytes, little-endian), CaptureNS its first sample. Only a session started
// with AudioConfig.TapPCM gets them (the core never sets it, so the pump and the publisher never see PCM); any other
// session, and a chunk of another size or captured before the epoch, drops it with a warning (at most once a second).
func (s *session) pcm(pk *pulled) (media.Packet, bool, error) {
	switch {
	case !s.tapPCM:
		s.dropped("a PCM chunk without AudioConfig.TapPCM", pk, -1)
		return media.Packet{}, false, nil
	case len(pk.data) != pcmChunkBytes || pk.oversized:
		s.dropped("a PCM chunk that is not 3840 bytes", pk, -1)
		return media.Packet{}, false, nil
	case s.captureNS(pk) < 0:
		s.dropped("a PCM chunk captured before the session's epoch", pk, -1)
		return media.Packet{}, false, nil
	}
	return media.Packet{Kind: media.KindPCM, CaptureNS: s.captureNS(pk), Data: pk.data}, true, nil
}

// eventDoc is the union of 04 §5.6's event details.
type eventDoc struct {
	Reason  string `json:"reason"`
	Detail  string `json:"detail"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	Cause   string `json:"cause"`
	Layer   string `json:"layer"`
	From    string `json:"from"`
	To      string `json:"to"`
	Limit   string `json:"limit"`
	Stream  string `json:"stream"`
	Dropped int64  `json:"dropped"`
}

// event maps an IM_PACKET_EVENT (04 §12.3's table, 01 §5.2's): ENDED becomes Next's error, RING_OVERFLOW and the
// audio range are logged, unknown events are logged and skipped.
func (s *session) event(pk *pulled) (media.Packet, bool, error) {
	if pk.oversized {
		s.dropped("an event over 1 KiB", pk, -1)
		return media.Packet{}, false, nil
	}
	var d eventDoc
	if len(pk.data) > 0 {
		if err := json.Unmarshal(pk.data, &d); err != nil {
			s.log.Warn("engine event detail unreadable", "event", pk.p.event, "bytes", len(pk.data))
		}
	}
	layer := d.Layer
	if layer == "" && pk.p.layer < uint32(len(s.rids)) {
		layer = s.rids[pk.p.layer]
	}
	var ev media.Event
	switch pk.p.event {
	case imEventEnded:
		return media.Packet{}, false, s.end(endedError(pk.p.status, d))
	case imEventSourceResized:
		ev = media.Event{Code: media.EventSourceResized, Width: d.Width, Height: d.Height}
	case imEventSourceMinimized:
		ev = media.Event{Code: media.EventSourceMinimized}
	case imEventSourceRestored:
		ev = media.Event{Code: media.EventSourceRestored}
	case imEventCaptureInterrupted:
		s.log.Info("engine capture interrupted", "cause", d.Cause)
		ev = media.Event{Code: media.EventCaptureInterrupted}
	case imEventCaptureResumed:
		s.log.Info("engine capture resumed", "cause", d.Cause)
		ev = media.Event{Code: media.EventCaptureResumed}
	case imEventEncoderChanged:
		s.log.Warn("engine encoder changed", "layer", layer, "from", d.From, "to", d.To, "reason", d.Reason)
		ev = media.Event{Code: media.EventEncoderChanged, Layer: layer, Encoder: d.To}
	case imEventDeviceReset:
		s.log.Warn("engine device reset", "reason", d.Reason)
		ev = media.Event{Code: media.EventDeviceReset}
	case imEventLimitChanged:
		ev = media.Event{Code: media.EventLimit, Layer: layer, Limit: d.Limit}
	case imEventRingOverflow:
		s.log.Warn("engine ring overflow: the consumer fell behind", "stream", d.Stream, "dropped", d.Dropped)
		return media.Packet{}, false, nil
	default:
		if pk.p.event >= 32 && pk.p.event <= 63 {
			s.log.Debug("engine audio event ignored", "event", pk.p.event) // 05 reports audio through its report
		} else {
			s.log.Warn("engine event unknown: skipped", "event", pk.p.event)
		}
		return media.Packet{}, false, nil
	}
	return media.Packet{Kind: media.KindEvent, Event: &ev}, true, nil
}

// endedError is the error of an ENDED event (04 §12.3): IM_OK → media.ErrStopped; else the status's code, with the
// detail's token (§5.7) as Reason.
func endedError(st int32, d eventDoc) error {
	if st == imOK {
		return errStopped("next")
	}
	e := statusError("next", "ENDED", st, d.Detail)
	if d.Reason != "" {
		e.Detail += " (" + d.Reason + ")"
	}
	return e
}

// end makes err the session's last word: Next answers ErrStopped from now on, and the binding frees the session with
// im_stop itself (≤ 2 s; the host's Stop then does nothing).
func (s *session) end(err error) error {
	s.stopping.Store(true)
	if ferr := s.free("next"); ferr != nil {
		s.log.Warn("engine session: im_stop after its end failed", logx.Err(ferr))
	}
	s.e.kickLogs()
	return err
}

// free runs im_stop once (it waits for a Next in im_next_packet, ≤ 50 ms, through the pull lock) and
// forgets the session. Later calls return nil.
func (s *session) free(op string) error {
	s.pull.Lock()
	defer s.pull.Unlock()
	s.smu.Lock()
	if s.freed {
		s.smu.Unlock()
		return nil
	}
	s.freed = true
	s.smu.Unlock()
	r, lastErr := s.c.call(procStop, uintptr(s.h))
	s.e.forget(s)
	if st := int32(r); st != imOK {
		return s.e.failed(statusError(op, procStop.String(), st, lastErr))
	}
	return nil
}

// Now is qpcNS − epoch_ns: the clock of Packet.CaptureNS (04 §12.3).
func (s *session) Now() int64 { return s.c.qpcNS() - s.epochNS }

// Stop ends the session: the first call sets stopping (a blocked Next returns ErrStopped within one 50 ms pull),
// waits for the pull lock and runs im_stop (≤ 2 s), and returns its result; later calls, and a Stop after the
// session's end, return nil. The handle never reaches the DLL twice.
func (s *session) Stop() error {
	s.stopping.Store(true)
	return s.free("stop")
}

// control runs one control export on the session (thread-safe in the engine, 04 §5.1). A Stop whose im_stop runs
// between the stopping check and the call leaves the engine a stale handle, which it answers with
// IM_ERR_INVALID_ARG: that is ErrStopped, as in Next (04 §12.3).
func (s *session) control(op string, p proc, args ...uintptr) error {
	if s.stopping.Load() {
		return errStopped(op)
	}
	if err := s.e.begin(op); err != nil {
		return err
	}
	defer s.e.end()
	r, lastErr := s.c.call(p, args...)
	if st := int32(r); st != imOK {
		if st == imErrInvalidArg && s.stopping.Load() {
			return errStopped(op)
		}
		return s.e.failed(statusError(op, p.String(), st, lastErr))
	}
	return nil
}

// layerIndex is the index of a layer's rid.
func (s *session) layerIndex(rid string) (int, bool) {
	i := slices.Index(s.rids, rid)
	return i, i >= 0
}

// RequestKeyframe is im_request_keyframe: unknown layers are ignored, and so is a failure (logged at debug).
func (s *session) RequestKeyframe(layer string) {
	const op = "request_keyframe"
	i, ok := s.layerIndex(layer)
	if !ok {
		return
	}
	if err := s.control(op, procRequestKeyframe, uintptr(s.h), uintptr(i)); err != nil {
		s.log.Debug("engine keyframe request failed", logx.Err(err))
	}
}

// SetBitrate: a video layer's target → im_set_bitrate (04 §6.4's range; 0 is CodeInvalid as 01 §5.1's contract and
// the fake have it: a layer pauses with SetLayerActive); media.LayerAudio → im_set_audio_bitrate.
func (s *session) SetBitrate(layer string, bps int) error {
	const op = "set_bitrate"
	if layer == media.LayerAudio {
		return s.setAudioBitrate(op, bps)
	}
	i, ok := s.layerIndex(layer)
	if !ok {
		return codeError(media.CodeInvalid, op, "", "unknown layer %q", layer)
	}
	if !validVideoBitrate(i, bps) {
		return codeError(media.CodeInvalid, op, "", "layer %q: %d bit/s is outside its range (04 §6.4)", layer, bps)
	}
	s.ctl.Lock()
	defer s.ctl.Unlock()
	if err := s.control(op, procSetBitrate, uintptr(s.h), uintptr(i), uintptr(bps)); err != nil {
		return err
	}
	s.layerCfg[i].bitrateBPS = uint32(bps)
	return nil
}

// SetLayerActive is im_set_layer_active: an inactive layer is not encoded; a resumed one starts with an IDR that is
// not a Reinit (04 §8.5).
func (s *session) SetLayerActive(layer string, active bool) error {
	const op = "set_layer_active"
	i, ok := s.layerIndex(layer)
	if !ok {
		return codeError(media.CodeInvalid, op, "", "unknown layer %q", layer)
	}
	s.ctl.Lock()
	defer s.ctl.Unlock()
	return s.control(op, procSetLayerActive, uintptr(s.h), uintptr(i), boolArg(active))
}

// SetPaused is im_set_paused: the video freezes from the next frame interval (04 §8.5), and the mixer's pause flag is
// set (the tone fades out).
func (s *session) SetPaused(paused bool) error {
	const op = "set_paused"
	s.ctl.Lock()
	defer s.ctl.Unlock()
	return s.control(op, procSetPaused, uintptr(s.h), boolArg(paused))
}

func boolArg(b bool) uintptr {
	if b {
		return 1
	}
	return 0
}

// Reconfigure applies r's non-zero fields in 04 §12.2's order and returns the first error: Profile →
// im_set_profile; Preset → im_set_content_hint; each of Layers (by RID) with MaxFPS or MaxPixels → im_update_layer
// with the layer's current config and those fields (and Bitrate, if set) replaced, with only Bitrate →
// im_set_bitrate. The engine coalesces the re-inits (04 §9.5).
func (s *session) Reconfigure(r media.Reconfig) error {
	const op = "reconfigure"
	s.ctl.Lock()
	defer s.ctl.Unlock()
	if r.Profile != "" {
		p, ok := profileValue(r.Profile)
		if !ok {
			return codeError(media.CodeInvalid, op, "", "Reconfig.Profile %q is not 6400 or 42e0", r.Profile)
		}
		if err := s.control(op, procSetProfile, uintptr(s.h), uintptr(p)); err != nil {
			return err
		}
	}
	if r.Preset != "" {
		hint, ok := contentHint(r.Preset)
		if !ok {
			return codeError(media.CodeInvalid, op, "", "Reconfig.Preset %q is not auto, game, movie or text",
				r.Preset)
		}
		if err := s.control(op, procSetContentHint, uintptr(s.h), uintptr(hint)); err != nil {
			return err
		}
	}
	for _, l := range r.Layers {
		if err := s.reconfigureLayer(op, l); err != nil {
			return err
		}
	}
	return nil
}

func (s *session) reconfigureLayer(op string, l media.LayerConfig) error {
	i, ok := s.layerIndex(l.RID)
	if !ok {
		return codeError(media.CodeInvalid, op, "", "Reconfig.Layers: unknown layer %q", l.RID)
	}
	switch {
	case l.MaxFPS < 0 || l.MaxFPS > maxFPS:
		return codeError(media.CodeInvalid, op, "", "layer %q: MaxFPS %d is outside 1..%d", l.RID, l.MaxFPS, maxFPS)
	case l.MaxPixels < 0 || int64(l.MaxPixels) > math.MaxUint32:
		return codeError(media.CodeInvalid, op, "", "layer %q: MaxPixels %d", l.RID, l.MaxPixels)
	case l.Bitrate != 0 && !validVideoBitrate(i, l.Bitrate):
		return codeError(media.CodeInvalid, op, "", "layer %q: %d bit/s is outside its range (04 §6.4)", l.RID,
			l.Bitrate)
	}
	if l.MaxFPS == 0 && l.MaxPixels == 0 {
		if l.Bitrate == 0 {
			return nil
		}
		if err := s.control(op, procSetBitrate, uintptr(s.h), uintptr(i), uintptr(l.Bitrate)); err != nil {
			return err
		}
		s.layerCfg[i].bitrateBPS = uint32(l.Bitrate)
		return nil
	}
	cfg := new(imLayerConfig)
	*cfg = s.layerCfg[i]
	if l.MaxFPS != 0 {
		cfg.fps = uint32(l.MaxFPS)
	}
	if l.MaxPixels != 0 {
		cfg.maxPixels = uint32(l.MaxPixels)
	}
	if l.Bitrate != 0 {
		cfg.bitrateBPS = uint32(l.Bitrate)
	}
	cfg.flags &^= imLayerInactive // a start flag; im_set_layer_active owns the state afterwards
	var pn runtime.Pinner
	err := s.control(op, procUpdateLayer, uintptr(s.h), uintptr(i), addrOf(&pn, cfg))
	pn.Unpin()
	if err != nil {
		return err
	}
	s.layerCfg[i] = *cfg
	return nil
}

// statsDoc is im_get_stats' JSON (04 §5.6), the fields media.EngineStats takes.
type statsDoc struct {
	Layers []struct {
		RID        string  `json:"rid"`
		Encoder    string  `json:"encoder"`
		HW         bool    `json:"hw"`
		FPSIn      float64 `json:"fpsIn"`
		FPSOut     float64 `json:"fpsOut"`
		BitrateBps int     `json:"bitrateBps"`
		TargetBps  int     `json:"targetBps"`
		Limit      string  `json:"limit"`
		Drops      struct {
			Pacer int `json:"pacer"`
			Ring  int `json:"ring"`
			Stale int `json:"stale"`
		} `json:"drops"`
		EncodeMs struct {
			P95 float64 `json:"p95"`
		} `json:"encodeMs"`
		LatencyMs struct {
			P50 float64 `json:"p50"`
			P95 float64 `json:"p95"`
		} `json:"latencyMs"`
	} `json:"layers"`
}

// Stats is im_get_stats (04 §5.6), decoded into media.EngineStats; Drops is pacer + ring + stale. A failure while
// a Stop runs is ErrStopped, as in control.
func (s *session) Stats() (media.EngineStats, error) {
	const op = "stats"
	if s.stopping.Load() {
		return media.EngineStats{}, errStopped(op)
	}
	if err := s.e.begin(op); err != nil {
		return media.EngineStats{}, err
	}
	doc, err := callJSON(s.c, op, procGetStats, jsonMaxCap, uintptr(s.h))
	s.e.end()
	if err != nil {
		if s.stopping.Load() {
			return media.EngineStats{}, errStopped(op)
		}
		return media.EngineStats{}, s.e.failed(err)
	}
	var d statsDoc
	if err := json.Unmarshal(doc, &d); err != nil {
		return media.EngineStats{}, codeError(media.CodeInternal, op, "", "im_get_stats: %v", err)
	}
	out := media.EngineStats{Layers: make([]media.EngineLayerStats, 0, len(d.Layers))}
	for _, l := range d.Layers {
		out.Layers = append(out.Layers, media.EngineLayerStats{
			RID: l.RID, Encoder: l.Encoder, HW: l.HW, FPSIn: l.FPSIn, FPSOut: l.FPSOut, BitrateBps: l.BitrateBps,
			TargetBps: l.TargetBps, Limit: l.Limit, EncodeP95: msDuration(l.EncodeMs.P95),
			LatencyP50: msDuration(l.LatencyMs.P50), LatencyP95: msDuration(l.LatencyMs.P95),
			Drops: l.Drops.Pacer + l.Drops.Ring + l.Drops.Stale,
		})
	}
	return out, nil
}

func msDuration(ms float64) time.Duration {
	return time.Duration(math.Round(ms * float64(time.Millisecond)))
}

// SetAudioBitrate is im_set_audio_bitrate (a Windows-only extra; SetBitrate(media.LayerAudio) is the contract's
// way): live, 32 000–510 000 bit/s.
func (s *session) SetAudioBitrate(bps int) error { return s.setAudioBitrate("set_audio_bitrate", bps) }

func (s *session) setAudioBitrate(op string, bps int) error {
	if !s.hasAudio {
		return codeError(media.CodeInvalid, op, "", "the session has no audio")
	}
	if bps < minAudioBitrateBPS || bps > maxAudioBitrateBPS {
		return codeError(media.CodeInvalid, op, "", "%d bit/s is outside %d..%d", bps, minAudioBitrateBPS,
			maxAudioBitrateBPS)
	}
	s.ctl.Lock()
	defer s.ctl.Unlock()
	return s.control(op, procSetAudioBitrate, uintptr(s.h), uintptr(bps))
}
