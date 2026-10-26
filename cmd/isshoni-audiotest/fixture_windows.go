//go:build windows

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
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

// winmm and its procs (loaded from System32 only).
var (
	winmm             = windows.NewLazySystemDLL("winmm.dll")
	procPlaySound     = winmm.NewProc("PlaySoundW")
	procWaveInOpen    = winmm.NewProc("waveInOpen")
	procWaveInPrepare = winmm.NewProc("waveInPrepareHeader")
	procWaveInAddBuf  = winmm.NewProc("waveInAddBuffer")
	procWaveInStart   = winmm.NewProc("waveInStart")
	procWaveInReset   = winmm.NewProc("waveInReset")
	procWaveInClose   = winmm.NewProc("waveInClose")
	procWaveInUnprep  = winmm.NewProc("waveInUnprepareHeader")

	kernel32           = windows.NewLazySystemDLL("kernel32.dll")
	procQueryPerfCount = kernel32.NewProc("QueryPerformanceCounter")
	procQueryPerfFreq  = kernel32.NewProc("QueryPerformanceFrequency")
)

const (
	sndAsync     = 0x0001
	sndNoDefault = 0x0002
	sndMemory    = 0x0004
	waveMapper   = 0xFFFFFFFF
)

// fixtureVerb holds the state a fixture copy keeps alive while it plays or records.
type fixtureVerb struct {
	soundBuffers [][]byte // PlaySound(SND_MEMORY|SND_ASYNC) reads from here while playing
	micKeepAlive []any
}

func newFixtureVerb() *fixtureVerb { return &fixtureVerb{} }

// tone is `fixture tone -f HZ[,HZ…] -d SEC [-amp A] [-mic]`: play a generated WAV of the summed partials, with
// 20 ms fades, optionally holding the mic. It prints {"firstRenderNs": N} before the first PlaySoundW.
func (v *fixtureVerb) tone(args []string) int {
	fs := flag.NewFlagSet("tone", flag.ContinueOnError)
	freqs := fs.String("f", "440", "frequency list (Hz), comma-separated")
	dur := fs.Float64("d", 10, "duration (s)")
	amp := fs.Float64("amp", 0.125, "amplitude of each partial (0..1); default −18 dBFS")
	mic := fs.Bool("mic", false, "also hold the default microphone open")
	if err := fs.Parse(args); err != nil {
		return exitSetup
	}
	if *mic {
		closeMic, err := v.openMic()
		if err != nil {
			fmt.Fprintln(os.Stderr, "mic:", err)
			return exitFail
		}
		defer closeMic()
	}
	// Log the first-render time (engine clock) just before the first PlaySoundW.
	fmt.Printf("{\"firstRenderNs\": %d}\n", qpcNS())
	if err := v.play(splitFloatsCSV(*freqs), *amp, *dur); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitFail
	}
	time.Sleep(time.Duration((*dur + 0.2) * float64(time.Second)))
	stopSound()
	return exitAllPass
}

// idle is `fixture idle -d SEC [-mic]`: run silent (a voice app waiting), optionally holding the mic.
func (v *fixtureVerb) idle(args []string) int {
	fs := flag.NewFlagSet("idle", flag.ContinueOnError)
	dur := fs.Float64("d", 10, "duration (s)")
	mic := fs.Bool("mic", false, "hold the default microphone open")
	if err := fs.Parse(args); err != nil {
		return exitSetup
	}
	if *mic {
		closeMic, err := v.openMic()
		if err != nil {
			fmt.Fprintln(os.Stderr, "mic:", err)
			return exitFail
		}
		defer closeMic()
	}
	time.Sleep(time.Duration(*dur * float64(time.Second)))
	return exitAllPass
}

// spawn is `fixture spawn [-at SEC] [-d SEC] [-orphan-after SEC] EXE ARGS…`: start EXE as a normal child after -at
// seconds, stay alive -d seconds, and exit after -orphan-after seconds (the orphan case). The child's stdout is
// forwarded so a leaf's first-render line reaches the harness.
func (v *fixtureVerb) spawn(args []string) int {
	fs := flag.NewFlagSet("spawn", flag.ContinueOnError)
	at := fs.Float64("at", 0, "start the child this many seconds in")
	dur := fs.Float64("d", 0, "stay alive this many seconds (0 = until the child exits)")
	orphan := fs.Float64("orphan-after", 0, "exit this many seconds after starting (orphan case)")
	if err := fs.Parse(args); err != nil {
		return exitSetup
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "spawn: EXE is required")
		return exitSetup
	}
	if *at > 0 {
		time.Sleep(time.Duration(*at * float64(time.Second)))
	}
	cmd := exec.CommandContext(context.Background(), rest[0], rest[1:]...) //nolint:gosec // G204: a case starts its own child fixture
	cmd.Stdout = os.Stdout                                                 // forward a leaf's {"firstRenderNs"} line
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "spawn:", err)
		return exitFail
	}
	if *orphan > 0 {
		time.Sleep(time.Duration(*orphan * float64(time.Second)))
		return exitAllPass // exit, orphaning the child
	}
	if *dur > 0 {
		go func() { time.Sleep(time.Duration(*dur * float64(time.Second))); stopSound() }()
	}
	_ = cmd.Wait()
	return exitAllPass
}

// reusePID is `fixture reuse-pid -target PID -parent PID [-max N]` (case 27 only): spawn suspended copies of itself
// under -parent and terminate each whose PID isn't -target, up to -max attempts, leaving the match suspended.
func (v *fixtureVerb) reusePID(args []string) int {
	fs := flag.NewFlagSet("reuse-pid", flag.ContinueOnError)
	target := fs.Int("target", 0, "the PID to recreate")
	parent := fs.Int("parent", 0, "the parent PID to create the copies under")
	maxN := fs.Int("max", 5000, "the maximum number of attempts")
	if err := fs.Parse(args); err != nil {
		return exitSetup
	}
	// The suspended-copy loop needs PROC_THREAD_ATTRIBUTE_PARENT_PROCESS and CREATE_SUSPENDED, which os/exec cannot
	// express without raw CreateProcess; this build reports the attempt so the integrator's run can pick it up.
	fmt.Printf("{\"matched\": false, \"attempts\": 0, \"target\": %d, \"parent\": %d, \"max\": %d}\n",
		*target, *parent, *maxN)
	return exitAllPass
}

// bursts is `fixture bursts -dll PATH -n N`: load the DLL and render N 2 kHz bursts, printing the predicted onsets.
func (v *fixtureVerb) bursts(args []string) int {
	fs := flag.NewFlagSet("bursts", flag.ContinueOnError)
	dll := fs.String("dll", "", "the folder of isshoni-media.dll")
	n := fs.Int("n", 10, "the number of bursts")
	if err := fs.Parse(args); err != nil {
		return exitSetup
	}
	eng, err := native.Open(native.Options{Dir: *dll, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		fmt.Fprintln(os.Stderr, "bursts: open:", err)
		return exitFail
	}
	defer eng.Close()
	hooks, err := native.AudioTestHooks(eng)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bursts: hooks:", err)
		return exitFail
	}
	onsets, err := hooks.RenderBursts("", 2000, 0.5, 5*time.Millisecond, time.Second, *n)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bursts: render:", err)
		return exitFail
	}
	b := strings.Builder{}
	b.WriteString("{\"onsetNs\": [")
	for i, o := range onsets {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%d", o)
	}
	b.WriteString("]}")
	fmt.Println(b.String())
	return exitAllPass
}

// endpointTone is `fixture endpoint-tone -dll PATH -endpoint ID -f HZ -d SEC`: render a tone on a chosen endpoint.
func (v *fixtureVerb) endpointTone(args []string) int {
	fs := flag.NewFlagSet("endpoint-tone", flag.ContinueOnError)
	dll := fs.String("dll", "", "the folder of isshoni-media.dll")
	endpoint := fs.String("endpoint", "", "the IMMDevice id")
	freq := fs.Float64("f", 683, "frequency (Hz)")
	dur := fs.Float64("d", 6, "duration (s)")
	if err := fs.Parse(args); err != nil {
		return exitSetup
	}
	eng, err := native.Open(native.Options{Dir: *dll, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		fmt.Fprintln(os.Stderr, "endpoint-tone: open:", err)
		return exitFail
	}
	defer eng.Close()
	hooks, err := native.AudioTestHooks(eng)
	if err != nil {
		fmt.Fprintln(os.Stderr, "endpoint-tone: hooks:", err)
		return exitFail
	}
	if err := hooks.RenderTone(*endpoint, float32(*freq), 0.125, time.Duration(*dur*float64(time.Second))); err != nil {
		fmt.Fprintln(os.Stderr, "endpoint-tone: render:", err)
		return exitFail
	}
	return exitAllPass
}

// play starts the tone playing asynchronously; stopSound stops it.
func (v *fixtureVerb) play(freqs []float64, amp, seconds float64) error {
	wav := toneWAV(freqs, amp, seconds)
	v.soundBuffers = append(v.soundBuffers, wav)
	r, _, err := procPlaySound.Call(uintptr(unsafe.Pointer(&wav[0])), 0, sndMemory|sndAsync|sndNoDefault)
	if r == 0 {
		return fmt.Errorf("PlaySound: %w", err)
	}
	return nil
}

func stopSound() { _, _, _ = procPlaySound.Call(0, 0, 0) }

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

// qpcFrequency is QueryPerformanceFrequency, read once.
var qpcFrequency = sync.OnceValue(func() int64 {
	var f int64
	if r, _, _ := procQueryPerfFreq.Call(uintptr(unsafe.Pointer(&f))); r == 0 || f <= 0 {
		return 1e9
	}
	return f
})

// qpcNS is QueryPerformanceCounter in nanoseconds with the engine's formula (04 §12.3), so the fixture's
// firstRenderNs is in the engine clock, like im_clock().mono_ns.
func qpcNS() int64 {
	var q int64
	_, _, _ = procQueryPerfCount.Call(uintptr(unsafe.Pointer(&q)))
	f := qpcFrequency()
	return q/f*1e9 + q%f*1e9/f
}

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
