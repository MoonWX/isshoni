package audio

import (
	"os"
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the run when a goroutine outlives the tests (the Run loops, the sound-check helper's process
// watchers). Started as `<test binary> --sound-check-helper`, the binary is the sound-check helper itself, so
// SoundCheck's tests start a real helper process.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == SoundCheckHelperFlag {
		os.Exit(RunSoundCheckHelper())
	}
	goleak.VerifyTestMain(m)
}
