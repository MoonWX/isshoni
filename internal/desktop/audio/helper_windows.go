package audio

import (
	"os/exec"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// PlaySoundW's flags (mmsystem.h).
const (
	sndSync      = 0x0000
	sndNoDefault = 0x0002 // no default beep when the sound can't play
	sndMemory    = 0x0004 // the first argument points to a WAVE file in memory
)

// procPlaySoundW is winmm's PlaySoundW, loaded from System32 only.
var procPlaySoundW = windows.NewLazySystemDLL("winmm.dll").NewProc("PlaySoundW")

// RunSoundCheckHelper is the process that SoundCheck starts as `<Exe> --sound-check-helper` (05 §11.3): 03's main
// runs it, before Wails starts, when os.Args[1] is SoundCheckHelperFlag, and exits with its result. It waits 400 ms
// (so the engine is listening), plays the chime built into the binary (chimeWAV) with PlaySoundW(SND_MEMORY |
// SND_SYNC) and returns 0, or 1 when Windows couldn't play it. The helper is an isshoni descendant, so it is never
// heard in a live share; the sound check opens its own stream on the helper's pid, outside the policy.
func RunSoundCheckHelper() int {
	time.Sleep(helperDelay)
	if procPlaySoundW.Find() != nil {
		return 1
	}
	wav := chimeWAV()
	ok, _, _ := procPlaySoundW.Call(uintptr(unsafe.Pointer(&wav[0])), 0, sndMemory|sndSync|sndNoDefault)
	runtime.KeepAlive(wav)
	if ok == 0 {
		return 1
	}
	return 0
}

// hideWindow starts the helper without a console window (CREATE_NO_WINDOW).
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}
