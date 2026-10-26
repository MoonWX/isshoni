//go:build windows

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/MoonWX/isshoni/internal/media/native"
)

// This file is the fixture side of the audiotest (docs/m2/05-windows-audio.md §18.3): what a renamed copy of the
// binary runs. A fixture plays a tone through the normal Windows audio stack (PlaySoundW → a per-process audio
// session), optionally holds the microphone (a voice app), spawns a child (a helper tree), reuses a PID, renders
// bursts or an endpoint tone. It is ported from spike S1 (spikes/s1-win-audio/winapi_windows.go). Windows only.
//
// Every fixture prints JSON lines on stdout that the harness reads (fixtureplan.go): {"role","pid","startNs"} when
// it starts, {"role","firstRenderNs"} right before its first PlaySoundW, and {"role","micError"} when -mic could not
// open the microphone (the mic cases then SKIP, 05 §18.4). A spawn's children inherit its stdout, so their lines
// reach the harness through it, also after the spawn exits (the orphan cases).

// winmm's waveIn procs (loaded from System32 only).
var (
	procWaveInOpen    = winmm.NewProc("waveInOpen")
	procWaveInPrepare = winmm.NewProc("waveInPrepareHeader")
	procWaveInAddBuf  = winmm.NewProc("waveInAddBuffer")
	procWaveInStart   = winmm.NewProc("waveInStart")
	procWaveInReset   = winmm.NewProc("waveInReset")
	procWaveInClose   = winmm.NewProc("waveInClose")
	procWaveInUnprep  = winmm.NewProc("waveInUnprepareHeader")
)

const waveMapper = 0xFFFFFFFF

// fixtureLine is one JSON line a fixture prints on stdout; the harness reads every field it knows (driver_windows.go).
type fixtureLine struct {
	Role          string  `json:"role,omitempty"`
	PID           uint32  `json:"pid,omitempty"`
	StartNS       int64   `json:"startNs,omitempty"`
	FirstRenderNS int64   `json:"firstRenderNs,omitempty"`
	OnsetNS       []int64 `json:"onsetNs,omitempty"`
	Matched       *bool   `json:"matched,omitempty"`
	Attempts      int     `json:"attempts,omitempty"`
	Error         string  `json:"error,omitempty"`
	MicError      string  `json:"micError,omitempty"`
}

// holdMic opens the microphone for a -mic fixture; on failure it tells the harness ({"role","micError"}) and
// stderr, and the fixture exits.
func (v *fixtureVerb) holdMic(role string) (func(), bool) {
	closeMic, err := v.openMic()
	if err != nil {
		emit(fixtureLine{Role: role, MicError: err.Error()})
		fmt.Fprintln(os.Stderr, "mic:", err)
		return nil, false
	}
	return closeMic, true
}

// emit prints one fixture line (a single write, so lines of processes sharing the pipe don't interleave).
func emit(l fixtureLine) {
	b, err := json.Marshal(l)
	if err != nil {
		return
	}
	_, _ = os.Stdout.Write(append(b, '\n'))
}

// hello announces this fixture process: its role, pid and start time on the engine clock.
func hello(role string) {
	emit(fixtureLine{Role: role, PID: windows.GetCurrentProcessId(), StartNS: qpcNS()})
}

// fixtureVerb holds the state a fixture copy keeps alive while it records.
type fixtureVerb struct {
	micKeepAlive []any
}

func newFixtureVerb() *fixtureVerb { return &fixtureVerb{} }

// startTone logs the first-render time (engine clock) and starts the tone right after (05 §18.3).
func startTone(role string, freqs []float64, amp, seconds float64) error {
	emit(fixtureLine{Role: role, FirstRenderNS: qpcNS()})
	return playTone(freqs, amp, seconds)
}

// tone is `fixture tone -role R -f HZ[,HZ…] -d SEC [-amp A] [-am HZ] [-mic]`: play a generated WAV of the summed
// partials, with 20 ms fades, optionally holding the mic, then exit. With -am (the soak's music, 05 §18.5 case 23)
// it loops a seamless WAV of one slow-AM period (loopToneWAV) for -d seconds instead.
func (v *fixtureVerb) tone(args []string) int {
	fs := flag.NewFlagSet("tone", flag.ContinueOnError)
	role := fs.String("role", "", "the fixture's role (harness-internal)")
	freqs := fs.String("f", "440", "frequency list (Hz), comma-separated")
	dur := fs.Float64("d", 10, "duration (s)")
	amp := fs.Float64("amp", 0.125, "amplitude of each partial (0..1); default −18 dBFS")
	am := fs.Float64("am", 0, "loop the tone with this slow amplitude modulation (Hz); 0 = a plain tone")
	mic := fs.Bool("mic", false, "also hold the default microphone open")
	if err := fs.Parse(args); err != nil {
		return exitSetup
	}
	hello(*role)
	if *mic {
		closeMic, ok := v.holdMic(*role)
		if !ok {
			return exitFail
		}
		defer closeMic()
	}
	var err error
	if *am > 0 {
		err = startLoopTone(*role, splitFloatsCSV(*freqs), *amp, *am)
	} else {
		err = startTone(*role, splitFloatsCSV(*freqs), *amp, *dur)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitFail
	}
	time.Sleep(time.Duration((*dur + 0.2) * float64(time.Second)))
	stopSound()
	return exitAllPass
}

// startLoopTone logs the first-render time and starts the soak's looped music (05 §18.5 case 23).
func startLoopTone(role string, freqs []float64, amp, am float64) error {
	wav, err := loopToneWAV(freqs, amp, am)
	if err != nil {
		return err
	}
	emit(fixtureLine{Role: role, FirstRenderNS: qpcNS()})
	return playLoopWAV(wav)
}

// idle is `fixture idle -role R -d SEC [-mic]`: run silent (a voice app waiting), optionally holding the mic.
func (v *fixtureVerb) idle(args []string) int {
	fs := flag.NewFlagSet("idle", flag.ContinueOnError)
	role := fs.String("role", "", "the fixture's role (harness-internal)")
	dur := fs.Float64("d", 10, "duration (s)")
	mic := fs.Bool("mic", false, "hold the default microphone open")
	if err := fs.Parse(args); err != nil {
		return exitSetup
	}
	hello(*role)
	if *mic {
		closeMic, ok := v.holdMic(*role)
		if !ok {
			return exitFail
		}
		defer closeMic()
	}
	time.Sleep(time.Duration(*dur * float64(time.Second)))
	return exitAllPass
}

// spawn is `fixture spawn -role R [-at SEC] [-tone HZ[,HZ…]] [-amp A] [-mic] [-d SEC] [-orphan-after SEC] EXE ARGS…`
// (05 §18.3): optionally play a tone itself (logging firstRenderNs as tone does) and hold the mic, start EXE as a
// normal child -at seconds after its own start, then stay alive -d seconds from its start, or exit -orphan-after
// seconds after its start, orphaning the child. The child inherits stdout, so its lines reach the harness.
func (v *fixtureVerb) spawn(args []string) int {
	fs := flag.NewFlagSet("spawn", flag.ContinueOnError)
	role := fs.String("role", "", "the fixture's role (harness-internal)")
	at := fs.Float64("at", 0, "start the child this many seconds in")
	tone := fs.String("tone", "", "also play these frequencies (Hz, comma-separated)")
	amp := fs.Float64("amp", 0.125, "amplitude of each partial of -tone")
	mic := fs.Bool("mic", false, "also hold the default microphone open")
	dur := fs.Float64("d", 10, "stay alive this many seconds")
	orphan := fs.Float64("orphan-after", 0, "exit this many seconds after starting (orphan case)")
	if err := fs.Parse(args); err != nil {
		return exitSetup
	}
	start := time.Now()
	hello(*role)
	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "spawn: EXE is required")
		return exitSetup
	}
	if *mic {
		closeMic, ok := v.holdMic(*role)
		if !ok {
			return exitFail
		}
		defer closeMic()
	}
	if *tone != "" {
		if err := startTone(*role, splitFloatsCSV(*tone), *amp, *dur); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return exitFail
		}
		defer stopSound()
	}
	time.Sleep(time.Until(start.Add(secs(*at))))
	cmd := exec.CommandContext(context.Background(), rest[0], rest[1:]...) //nolint:gosec // G204: a case starts its own child fixture
	cmd.Stdout = os.Stdout                                                 // the child's lines reach the harness
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "spawn:", err)
		return exitFail
	}
	go func() { _ = cmd.Wait() }() // reap the child if it exits first; never kill it
	life := *dur
	if *orphan > 0 {
		life = *orphan // exit, orphaning the child
	}
	time.Sleep(time.Until(start.Add(secs(life))))
	return exitAllPass
}

// reusePID is `fixture reuse-pid -target PID -parent PID [-max N]` (case 27 only, 05 §18.3): create suspended copies
// of itself under -parent and terminate each whose PID isn't -target, up to -max attempts. A match is left
// suspended (alive) until the harness kills it. It prints {"matched": true|false, "attempts": n} (and "pid" on a
// match).
//
// The copies come from suspendedSpawner, a direct CreateProcessW with CREATE_SUSPENDED and
// PROC_THREAD_ATTRIBUTE_PARENT_PROCESS that inherits no handle, so the parent needs only PROCESS_CREATE_PROCESS, as
// 05 §18.3 says (os/exec would also duplicate the standard handles into the parent, which needs PROCESS_DUP_HANDLE,
// and opens three NUL handles per attempt).
func (v *fixtureVerb) reusePID(args []string) int {
	fs := flag.NewFlagSet("reuse-pid", flag.ContinueOnError)
	target := fs.Int("target", 0, "the PID to recreate")
	parent := fs.Int("parent", 0, "the parent PID to create the copies under")
	maxN := fs.Int("max", 5000, "the maximum number of attempts")
	if err := fs.Parse(args); err != nil {
		return exitSetup
	}
	matched := false
	if *target <= 0 || *target > math.MaxUint32 || *parent <= 0 || *parent > math.MaxUint32 {
		emit(fixtureLine{Matched: &matched, Error: "target and parent are required"})
		return exitSetup
	}
	ph, err := windows.OpenProcess(windows.PROCESS_CREATE_PROCESS, false, uint32(*parent))
	if err != nil {
		emit(fixtureLine{Matched: &matched, Error: "open the parent: " + err.Error()})
		return exitFail
	}
	defer func() { _ = windows.CloseHandle(ph) }()
	self, err := os.Executable()
	if err != nil {
		emit(fixtureLine{Matched: &matched, Error: err.Error()})
		return exitFail
	}
	spawner, err := newSuspendedSpawner(ph, self, []string{"fixture", verbIdle, "-d", "1"})
	if err != nil {
		emit(fixtureLine{Matched: &matched, Error: err.Error()})
		return exitFail
	}
	defer spawner.close()
	for n := 1; n <= *maxN; n++ {
		pi, err := spawner.create()
		if err != nil {
			emit(fixtureLine{Matched: &matched, Attempts: n, Error: "create: " + err.Error()})
			return exitFail
		}
		if pi.ProcessId == uint32(*target) {
			matched = true
			emit(fixtureLine{Matched: &matched, Attempts: n, PID: pi.ProcessId})
			_ = windows.CloseHandle(pi.Thread) // leave it suspended and alive: the harness kills it with the case
			_ = windows.CloseHandle(pi.Process)
			return exitAllPass
		}
		_ = windows.TerminateProcess(pi.Process, 1)
		_, _ = windows.WaitForSingleObject(pi.Process, 2000) // its PID is free once it is gone and unreferenced
		_ = windows.CloseHandle(pi.Thread)
		_ = windows.CloseHandle(pi.Process)
	}
	emit(fixtureLine{Matched: &matched, Attempts: *maxN})
	return exitAllPass
}

// suspendedSpawner creates suspended copies of a program as children of another process (case 27's PID-reuse loop):
// CreateProcessW with CREATE_SUSPENDED and EXTENDED_STARTUPINFO_PRESENT, an attribute list holding only
// PROC_THREAD_ATTRIBUTE_PARENT_PROCESS, and no inherited handle.
type suspendedSpawner struct {
	parent  windows.Handle // kept alive for the attribute list, which points at it
	attrs   *windows.ProcThreadAttributeListContainer
	exe     *uint16
	cmdline string
}

// newSuspendedSpawner prepares the attribute list for parent (opened with PROCESS_CREATE_PROCESS) and exe's command
// line.
func newSuspendedSpawner(parent windows.Handle, exe string, args []string) (*suspendedSpawner, error) {
	s := &suspendedSpawner{parent: parent, cmdline: windows.ComposeCommandLine(append([]string{exe}, args...))}
	var err error
	if s.exe, err = windows.UTF16PtrFromString(exe); err != nil {
		return nil, err
	}
	if s.attrs, err = windows.NewProcThreadAttributeList(1); err != nil {
		return nil, fmt.Errorf("attribute list: %w", err)
	}
	if err := s.attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PARENT_PROCESS, unsafe.Pointer(&s.parent),
		unsafe.Sizeof(s.parent)); err != nil {
		s.attrs.Delete()
		return nil, fmt.Errorf("parent attribute: %w", err)
	}
	return s, nil
}

// create starts one suspended copy. The caller closes both handles of the result.
func (s *suspendedSpawner) create() (windows.ProcessInformation, error) {
	var pi windows.ProcessInformation
	cmd, err := windows.UTF16FromString(s.cmdline) // CreateProcessW may write into the command line
	if err != nil {
		return pi, err
	}
	si := windows.StartupInfoEx{ProcThreadAttributeList: s.attrs.List()}
	si.Cb = uint32(unsafe.Sizeof(si))
	err = windows.CreateProcess(s.exe, &cmd[0], nil, nil, false,
		windows.CREATE_SUSPENDED|windows.CREATE_NO_WINDOW|windows.EXTENDED_STARTUPINFO_PRESENT, nil, nil,
		&si.StartupInfo, &pi)
	return pi, err
}

func (s *suspendedSpawner) close() { s.attrs.Delete() }

// bursts is `fixture bursts -dll PATH -n N`: load the DLL and render N 2 kHz bursts, printing the predicted onsets.
func (v *fixtureVerb) bursts(args []string) int {
	fs := flag.NewFlagSet("bursts", flag.ContinueOnError)
	role := fs.String("role", "bursts", "the fixture's role (harness-internal)")
	dll := fs.String("dll", "", "the folder of isshoni-media.dll")
	n := fs.Int("n", 10, "the number of bursts")
	if err := fs.Parse(args); err != nil {
		return exitSetup
	}
	hello(*role)
	eng, err := native.Open(native.Options{Dir: *dll, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		fmt.Fprintln(os.Stderr, "bursts: open:", err)
		return exitFail
	}
	defer func() { _ = eng.Close() }()
	hooks, err := native.AudioTestHooks(eng)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bursts: hooks:", err)
		return exitFail
	}
	onsets, err := hooks.RenderBursts("", freqBursts, 0.5, 5*time.Millisecond, time.Second, *n)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bursts: render:", err)
		return exitFail
	}
	emit(fixtureLine{Role: *role, OnsetNS: onsets})
	return exitAllPass
}

// endpointTone is `fixture endpoint-tone -role R -dll PATH -endpoint ID -f HZ -d SEC` (case 22): render a tone on a
// chosen endpoint with im_test_render_tone, logging firstRenderNs right before.
func (v *fixtureVerb) endpointTone(args []string) int {
	fs := flag.NewFlagSet("endpoint-tone", flag.ContinueOnError)
	role := fs.String("role", "", "the fixture's role (harness-internal)")
	dll := fs.String("dll", "", "the folder of isshoni-media.dll")
	endpoint := fs.String("endpoint", "", "the IMMDevice id")
	freq := fs.Float64("f", 683, "frequency (Hz)")
	dur := fs.Float64("d", 6, "duration (s)")
	if err := fs.Parse(args); err != nil {
		return exitSetup
	}
	hello(*role)
	eng, err := native.Open(native.Options{Dir: *dll, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		fmt.Fprintln(os.Stderr, "endpoint-tone: open:", err)
		return exitFail
	}
	defer func() { _ = eng.Close() }()
	hooks, err := native.AudioTestHooks(eng)
	if err != nil {
		fmt.Fprintln(os.Stderr, "endpoint-tone: hooks:", err)
		return exitFail
	}
	emit(fixtureLine{Role: *role, FirstRenderNS: qpcNS()})
	if err := hooks.RenderTone(*endpoint, float32(*freq), 0.125, secs(*dur)); err != nil {
		fmt.Fprintln(os.Stderr, "endpoint-tone: render:", err)
		return exitFail
	}
	return exitAllPass
}

type waveFormatEx struct {
	FormatTag      uint16
	Channels       uint16
	SamplesPerSec  uint32
	AvgBytesPerSec uint32
	BlockAlign     uint16
	BitsPerSample  uint16
	CbSize         uint16
}

type waveHdr struct {
	Data          uintptr
	BufferLength  uint32
	BytesRecorded uint32
	User          uintptr
	Flags         uint32
	Loops         uint32
	Next          uintptr
	Reserved      uintptr
}

// openMic opens the default microphone (a capture session for this process), like a voice app (S1's openMic).
func (v *fixtureVerb) openMic() (func(), error) {
	var h uintptr
	format := &waveFormatEx{FormatTag: 1, Channels: 1, SamplesPerSec: 48000, AvgBytesPerSec: 96000, BlockAlign: 2,
		BitsPerSample: 16}
	if r, _, _ := procWaveInOpen.Call(uintptr(unsafe.Pointer(&h)), waveMapper, uintptr(unsafe.Pointer(format)), 0, 0,
		0); r != 0 {
		return nil, fmt.Errorf("waveInOpen: MMRESULT %d (no microphone or access disabled)", r)
	}
	var hdrs []*waveHdr
	for range 8 {
		buf := make([]byte, 96000*2)                                                            // 2 s each
		hdr := &waveHdr{Data: uintptr(unsafe.Pointer(&buf[0])), BufferLength: uint32(len(buf))} //nolint:gosec // G115: a 192 KiB buffer fits in uint32
		v.micKeepAlive = append(v.micKeepAlive, buf, hdr)
		_, _, _ = procWaveInPrepare.Call(h, uintptr(unsafe.Pointer(hdr)), unsafe.Sizeof(*hdr))
		_, _, _ = procWaveInAddBuf.Call(h, uintptr(unsafe.Pointer(hdr)), unsafe.Sizeof(*hdr))
		hdrs = append(hdrs, hdr)
	}
	if r, _, _ := procWaveInStart.Call(h); r != 0 {
		_, _, _ = procWaveInClose.Call(h)
		return nil, fmt.Errorf("waveInStart: MMRESULT %d", r)
	}
	return func() {
		_, _, _ = procWaveInReset.Call(h)
		for _, hdr := range hdrs {
			_, _, _ = procWaveInUnprep.Call(h, uintptr(unsafe.Pointer(hdr)), unsafe.Sizeof(*hdr))
		}
		_, _, _ = procWaveInClose.Call(h)
	}, nil
}

// secs converts seconds to a time.Duration.
func secs(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// splitFloatsCSV parses a comma-separated frequency list.
func splitFloatsCSV(s string) []float64 {
	var out []float64
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			var f float64
			if _, err := fmt.Sscanf(p, "%g", &f); err == nil {
				out = append(out, f)
			}
		}
	}
	return out
}
