//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Fake apps for the self-test: play a tone through the normal Windows audio
// stack (PlaySound → a per-process audio session), optionally hold the
// microphone open (like a voice app), and launch processes as children of
// explorer.exe so they look like user-started apps rather than children of s1.

var (
	winmm              = windows.NewLazySystemDLL("winmm.dll")
	procPlaySound      = winmm.NewProc("PlaySoundW")
	procWaveInOpen     = winmm.NewProc("waveInOpen")
	procWaveInPrepare  = winmm.NewProc("waveInPrepareHeader")
	procWaveInAddBuf   = winmm.NewProc("waveInAddBuffer")
	procWaveInStart    = winmm.NewProc("waveInStart")
	procWaveInReset    = winmm.NewProc("waveInReset")
	procWaveInClose    = winmm.NewProc("waveInClose")
	procWaveInUnprep   = winmm.NewProc("waveInUnprepareHeader")
	activeSoundBuffers [][]byte // PlaySound(SND_MEMORY|SND_ASYNC) reads from here while playing
)

const (
	sndAsync     = 0x0001
	sndNoDefault = 0x0002
	sndMemory    = 0x0004
	waveMapper   = 0xFFFFFFFF
)

// playTone starts playing asynchronously; stopSound() stops it.
func playTone(freq, amp, seconds float64) error {
	wav := toneWAV(freq, amp, seconds)
	activeSoundBuffers = append(activeSoundBuffers, wav)
	r, _, err := procPlaySound.Call(uintptr(unsafe.Pointer(&wav[0])), 0, sndMemory|sndAsync|sndNoDefault)
	if r == 0 {
		return fmt.Errorf("PlaySound: %v", err)
	}
	return nil
}

func stopSound() { procPlaySound.Call(0, 0, 0) }

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

var micKeepAlive []any

// openMic opens the default microphone and starts recording into a few
// buffers, which creates a capture audio session for this process.
func openMic() (func(), error) {
	var h uintptr
	format := &waveFormatEx{FormatTag: 1, Channels: 1, SamplesPerSec: 48000, AvgBytesPerSec: 96000, BlockAlign: 2, BitsPerSample: 16}
	if r, _, _ := procWaveInOpen.Call(uintptr(unsafe.Pointer(&h)), waveMapper, uintptr(unsafe.Pointer(format)), 0, 0, 0); r != 0 {
		return nil, fmt.Errorf("waveInOpen: MMRESULT %d (no microphone, or microphone access disabled for desktop apps)", r)
	}
	var hdrs []*waveHdr
	for i := 0; i < 8; i++ {
		buf := make([]byte, 96000*2) // 2 s each
		hdr := &waveHdr{Data: uintptr(unsafe.Pointer(&buf[0])), BufferLength: uint32(len(buf))}
		micKeepAlive = append(micKeepAlive, buf, hdr)
		procWaveInPrepare.Call(h, uintptr(unsafe.Pointer(hdr)), unsafe.Sizeof(*hdr))
		procWaveInAddBuf.Call(h, uintptr(unsafe.Pointer(hdr)), unsafe.Sizeof(*hdr))
		hdrs = append(hdrs, hdr)
	}
	if r, _, _ := procWaveInStart.Call(h); r != 0 {
		procWaveInClose.Call(h)
		return nil, fmt.Errorf("waveInStart: MMRESULT %d", r)
	}
	return func() {
		procWaveInReset.Call(h)
		for _, hdr := range hdrs {
			procWaveInUnprep.Call(h, uintptr(unsafe.Pointer(hdr)), unsafe.Sizeof(*hdr))
		}
		procWaveInClose.Call(h)
	}, nil
}

// startUnderExplorer launches exe as a child of the user's explorer.exe (like
// an app started from the Start menu). Falls back to a normal child process.
func startUnderExplorer(exe string, args ...string) (*os.Process, bool, error) {
	cmd := exec.Command(exe, args...)
	attr := &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	reparented := false
	if h, err := openExplorer(); err == nil {
		defer windows.CloseHandle(h)
		attr.ParentProcess = syscall.Handle(h)
		reparented = true
	}
	cmd.SysProcAttr = attr
	if err := cmd.Start(); err != nil {
		if !reparented {
			return nil, false, err
		}
		cmd = exec.Command(exe, args...) // retry without reparenting
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
		if err := cmd.Start(); err != nil {
			return nil, false, err
		}
		reparented = false
	}
	return cmd.Process, reparented, nil
}

func openExplorer() (windows.Handle, error) {
	var mySession uint32
	windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &mySession)
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(snap)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		if !strings.EqualFold(windows.UTF16ToString(pe.ExeFile[:]), "explorer.exe") {
			continue
		}
		var s uint32
		if windows.ProcessIdToSessionId(pe.ProcessID, &s) != nil || s != mySession {
			continue
		}
		// PROCESS_DUP_HANDLE: Go duplicates the child's stdio handles into the new parent.
		return windows.OpenProcess(windows.PROCESS_CREATE_PROCESS|windows.PROCESS_DUP_HANDLE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pe.ProcessID)
	}
	return 0, fmt.Errorf("explorer.exe not found in session %d", mySession)
}
