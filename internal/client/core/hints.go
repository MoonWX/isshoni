package core

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/MoonWX/isshoni/internal/protocol"
)

// Hints, the codec switch, presets and pause (docs/m2/01-client-core.md D13–D15, §11.7's reconcile, §11.8) are
// README WA46's. Until then a share runs at the first targets of ratectl.Initial and in the profile its session
// started with (or the one an answer chose, which the Publisher follows by itself), and these calls are refused.

// SetPreset changes the share's preset: share.update{preset}, then the reply's ShareParams to the rate controller and
// Session.Reconfigure (§11.7). Not implemented yet (README WA46).
func (s *Share) SetPreset(ctx context.Context, p protocol.Preset) error {
	return fmt.Errorf("core: Share.SetPreset: %w", errNotImplemented)
}

// SetPaused pauses or resumes the share (D14, §11.8): the pump drops media at once, then Session.SetPaused,
// AudioController.SetAudioMuted and share.update{paused}; "Share sound" off is 05's Service.SetShareSound(false).
// Not implemented yet (README WA46).
func (s *Share) SetPaused(ctx context.Context, paused bool) error {
	return fmt.Errorf("core: Share.SetPaused: %w", errNotImplemented)
}

// onHint handles a quality.hint (§11.8). Applying it is README WA46's: it is only logged.
func (s *Share) onHint(h protocol.QualityHint) {
	if h.ShareID != s.params.ShareID {
		return
	}
	s.log.Debug("quality hint not applied", slog.String("reason", string(h.Reason)),
		slog.String("codec", string(h.Codec)), slog.Int64("max_bitrate", h.MaxBitrate))
}
