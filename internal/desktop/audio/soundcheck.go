package audio

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
)

// SoundCheckHelperFlag is the argument that starts the binary as the sound-check helper: 03's main checks os.Args[1]
// for it first and exits with RunSoundCheckHelper() (05 §11.3).
const SoundCheckHelperFlag = "--sound-check-helper"

// The sound check's times (05 §11.3).
const (
	soundCheckTimeout = 3 * time.Second        // the engine's capture window (im_audio_sound_check's timeout_ms)
	helperLimit       = 5 * time.Second        // the helper is killed after this
	helperDelay       = 400 * time.Millisecond // the helper waits this long before it plays the chime
)

// SoundCheck is the first-run wizard's sound check (05 §11.3). With SelfInfo.Exe it starts `<Exe>
// --sound-check-helper` (on Windows without a console window), asks the engine to capture the helper's chime for up
// to 3 s, and waits for the helper, which is killed after 5 s. With Exe "" it starts nothing and the engine plays the
// chime itself (helper pid 0). The result is the engine's (captured, level, backend, reason); errors are the
// engine's, wrapped, or the helper's start failing (wrapped without the binary's path, 05 §16).
func (s *Service) SoundCheck(ctx context.Context) (media.SoundCheckResult, error) {
	if s.isClosed() {
		return media.SoundCheckResult{}, ErrClosed
	}
	if s.self.Exe == "" {
		res, err := s.eng.SoundCheck(ctx, 0, soundCheckTimeout)
		if err != nil {
			return media.SoundCheckResult{}, fmt.Errorf("audio: sound check: %w", err)
		}
		return res, nil
	}
	hctx, cancel := context.WithTimeout(ctx, helperLimit)
	defer cancel()
	cmd := exec.CommandContext(hctx, s.self.Exe, SoundCheckHelperFlag) //nolint:gosec // G204: the app's own binary (03: os.Executable())
	hideWindow(cmd)
	if err := cmd.Start(); err != nil {
		return media.SoundCheckResult{}, fmt.Errorf("audio: sound check: start the helper: %w", withoutPath(err))
	}
	res, err := s.eng.SoundCheck(ctx, uint32(cmd.Process.Pid), soundCheckTimeout) //nolint:gosec // G115: a pid is a DWORD
	// The helper's exit code doesn't matter; hctx kills it after 5 s at the latest.
	_ = cmd.Wait()
	if err != nil {
		return media.SoundCheckResult{}, fmt.Errorf("audio: sound check: %w", err)
	}
	return res, nil
}

// withoutPath returns err's cause without the path that exec and os errors quote (it names the user's folder).
func withoutPath(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	var ee *exec.Error
	if errors.As(err, &ee) {
		return ee.Err
	}
	return errors.New("the helper did not start")
}
