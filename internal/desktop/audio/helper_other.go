//go:build !windows

package audio

import "os/exec"

// RunSoundCheckHelper is the process that SoundCheck starts as `<Exe> --sound-check-helper` (05 §11.3). Off Windows
// it plays nothing and returns 2 (M3 gives darwin its own file, 05 §20).
func RunSoundCheckHelper() int { return 2 }

// hideWindow does nothing off Windows.
func hideWindow(*exec.Cmd) {}
