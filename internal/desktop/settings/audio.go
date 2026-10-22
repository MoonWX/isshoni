package settings

import (
	"encoding/json"
	"log/slog"
	"maps"
)

// AudioStore is 05's audio.SettingsStore over the store's `audio` section (05 §14.1–§14.2, 03 §14.2):
// AudioSettings unmarshals the section into T, 05's audio.Settings, on top of 05's defaults, and SaveAudioSettings
// marshals it back through the store's debounced atomic write. 05 owns the defaults and the validation; keys of the
// section that T doesn't have are kept and written back unchanged.
//
// It is generic so that this package needn't import internal/desktop/audio: main builds
// settings.NewAudioStore(store, audio.DefaultSettings) (or a func literal returning 05's defaults), and its methods
// AudioSettings() audio.Settings and SaveAudioSettings(audio.Settings) error are audio.SettingsStore's.
type AudioStore[T any] struct {
	s        *Store
	defaults func() T
}

// NewAudioStore returns the adapter. defaults returns 05's default settings; nil means T's zero value.
func NewAudioStore[T any](s *Store, defaults func() T) *AudioStore[T] {
	if defaults == nil {
		defaults = func() T { var zero T; return zero }
	}
	return &AudioStore[T]{s: s, defaults: defaults}
}

// AudioSettings returns the saved section as T: the defaults, overwritten by what the section has. A section that
// doesn't decode into T gives the defaults for what failed (one WARN).
func (a *AudioStore[T]) AudioSettings() T {
	v := a.defaults()
	if err := json.Unmarshal(a.s.AudioSection(), &v); err != nil {
		a.s.log.Warn("settings: the audio section doesn't fit; using 05's defaults for what failed", slog.Any("err", err))
	}
	return v
}

// SaveAudioSettings writes v as the section, keeping the section's keys that v doesn't have.
func (a *AudioStore[T]) SaveAudioSettings(v T) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var next map[string]json.RawMessage
	if err := json.Unmarshal(data, &next); err != nil || next == nil {
		return fieldError("audio")
	}
	var old map[string]json.RawMessage
	_ = json.Unmarshal(a.s.AudioSection(), &old)
	merged := make(map[string]json.RawMessage, len(old)+len(next))
	maps.Copy(merged, old)
	maps.Copy(merged, next)
	out, err := json.Marshal(merged)
	if err != nil {
		return err
	}
	return a.s.SetAudioSection(out)
}
