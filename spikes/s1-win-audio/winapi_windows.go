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

// launcher picks the parent of the fake apps (selftest -launcher):
//   - "explorer" (default): the user's explorer.exe in s1's session, like an app
//     started from the Start menu;
//   - "parent": s1's own parent process (the shell that started s1, such as
//     pwsh.exe on a CI runner with no desktop), so the fake apps are siblings of
//     s1, not descendants; the shell is a launcher where the engine's app walk
//     stops, so they are classified as under Explorer;
//   - "auto": explorer, else parent.
var launcher = "explorer"

// startUnderExplorer launches exe as a child of the launcher process (see
// launcher; explorer.exe by default). Falls back to a normal child process,
// reported as reparented=false.
func startUnderExplorer(exe string, args ...string) (*os.Process, bool, error) {
	cmd := exec.Command(exe, args...)
	attr := &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	reparented := false
	if h, _, err := openLauncher(); err == nil {
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

// openLauncher opens the process the fake apps start under, and describes it.
func openLauncher() (windows.Handle, string, error) {
	switch launcher {
	case "explorer":
		return openExplorer()
	case "parent":
		return openOwnParent()
	case "auto":
		if h, desc, err := openExplorer(); err == nil {
			return h, desc, nil
		}
		return openOwnParent()
	}
	return 0, "", fmt.Errorf("unknown launcher %q (explorer, parent or auto)", launcher)
}

// launcherAccess: PROCESS_CREATE_PROCESS to be the parent; PROCESS_DUP_HANDLE
// because Go duplicates the child's stdio handles into the new parent.
const launcherAccess = windows.PROCESS_CREATE_PROCESS | windows.PROCESS_DUP_HANDLE | windows.PROCESS_QUERY_LIMITED_INFORMATION

// processes lists every process: PID → (parent PID, exe name).
func processes() (map[uint32]windows.ProcessEntry32, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)
	out := map[uint32]windows.ProcessEntry32{}
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		out[pe.ProcessID] = pe
	}
	return out, nil
}

func openExplorer() (windows.Handle, string, error) {
	var mySession uint32
	windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &mySession)
	procs, err := processes()
	if err != nil {
		return 0, "", err
	}
	for pid, pe := range procs {
		if !strings.EqualFold(windows.UTF16ToString(pe.ExeFile[:]), "explorer.exe") {
			continue
		}
		var s uint32
		if windows.ProcessIdToSessionId(pid, &s) != nil || s != mySession {
			continue
		}
		h, err := windows.OpenProcess(launcherAccess, false, pid)
		return h, fmt.Sprintf("explorer.exe (session %d)", mySession), err
	}
	return 0, "", fmt.Errorf("explorer.exe not found in session %d", mySession)
}

// openOwnParent opens the process that started s1 (selftest -launcher parent).
func openOwnParent() (windows.Handle, string, error) {
	procs, err := processes()
	if err != nil {
		return 0, "", err
	}
	me, ok := procs[windows.GetCurrentProcessId()]
	if !ok {
		return 0, "", fmt.Errorf("s1 is not in the process list")
	}
	parent, ok := procs[me.ParentProcessID]
	if !ok {
		return 0, "", fmt.Errorf("s1's parent process %d has exited", me.ParentProcessID)
	}
	name := windows.UTF16ToString(parent.ExeFile[:])
	h, err := windows.OpenProcess(launcherAccess, false, me.ParentProcessID)
	return h, "s1's parent " + name, err
}
