//go:build windows

package main

import (
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// This file holds the Windows sound helpers that both the fixtures (fixture_windows.go) and the harness itself
// (driver_windows.go: cases 4 and 10 play isshoni's own tone, 05 §18.5) use: PlaySoundW of a generated WAV, and the
// engine clock from QueryPerformanceCounter. Ported from spike S1 (spikes/s1-win-audio/winapi_windows.go).

// winmm and kernel32 procs (loaded from System32 only).
var (
	winmm         = windows.NewLazySystemDLL("winmm.dll")
	procPlaySound = winmm.NewProc("PlaySoundW")

	kernel32           = windows.NewLazySystemDLL("kernel32.dll")
	procQueryPerfCount = kernel32.NewProc("QueryPerformanceCounter")
	procQueryPerfFreq  = kernel32.NewProc("QueryPerformanceFrequency")
)

const (
	sndAsync     = 0x0001
	sndNoDefault = 0x0002
	sndMemory    = 0x0004
	sndLoop      = 0x0008
)

// soundMu guards soundBuf: PlaySoundW(SND_MEMORY|SND_ASYNC) reads the WAV while it plays, so the buffer must stay
// alive until the next PlaySoundW call replaces or stops the sound (one sound per process).
var (
	soundMu  sync.Mutex
	soundBuf []byte //nolint:unused // written only: holding it keeps the WAV alive while PlaySoundW plays it
)

// playTone starts playing a generated WAV of freqs (each partial at amp, 20 ms fades, 05 §18.3) for seconds,
// asynchronously, from this process's audio session.
func playTone(freqs []float64, amp, seconds float64) error {
	wav := toneWAV(freqs, amp, seconds)
	soundMu.Lock()
	defer soundMu.Unlock()
	r, _, err := procPlaySound.Call(uintptr(unsafe.Pointer(&wav[0])), 0, sndMemory|sndAsync|sndNoDefault)
	if r == 0 {
		return fmt.Errorf("PlaySound: %w", err)
	}
	soundBuf = wav
	return nil
}

// playLoopWAV plays an in-memory WAV over and over (SND_LOOP, asynchronously) until stopSound: the soak's music
// (loopToneWAV), 30 minutes from a 10 s loop.
func playLoopWAV(wav []byte) error {
	if len(wav) == 0 {
		return fmt.Errorf("PlaySound: an empty WAV")
	}
	soundMu.Lock()
	defer soundMu.Unlock()
	r, _, err := procPlaySound.Call(uintptr(unsafe.Pointer(&wav[0])), 0, sndMemory|sndAsync|sndNoDefault|sndLoop)
	if r == 0 {
		return fmt.Errorf("PlaySound: %w", err)
	}
	soundBuf = wav
	return nil
}

// stopSound stops this process's sound.
func stopSound() {
	soundMu.Lock()
	defer soundMu.Unlock()
	_, _, _ = procPlaySound.Call(0, 0, 0)
	soundBuf = nil
}

// qpcFrequency is QueryPerformanceFrequency, read once.
var qpcFrequency = sync.OnceValue(func() int64 {
	var f int64
	if r, _, _ := procQueryPerfFreq.Call(uintptr(unsafe.Pointer(&f))); r == 0 || f <= 0 {
		return 1e9
	}
	return f
})

// qpcNS is QueryPerformanceCounter in nanoseconds with the engine's formula (04 §12.3), so a fixture's logged times
// are in the engine clock, like im_clock().mono_ns.
func qpcNS() int64 {
	var q int64
	_, _, _ = procQueryPerfCount.Call(uintptr(unsafe.Pointer(&q)))
	f := qpcFrequency()
	return q/f*1e9 + q%f*1e9/f
}
