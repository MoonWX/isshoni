// The C++ boundary between the engine (04) and Windows audio (05) inside isshoni-media.dll:
// docs/m2/04-windows-engine.md §10.2 in the form of docs/m2/05-windows-audio.md §10.5, which 05 implements in this
// folder. 04 calls it from src/api/exports.cpp (the audio exports, after their argument checks), src/engine/ (Init and
// Shutdown on the worker) and src/audio_bridge/ (a session's Mixer, wired to its Opus stage as the PcmSink).
//
// README WA11 declared this boundary with a stub engine; 05's engine is audio_engine.cpp: README WA35 the watcher half
// (watcher.h: the process table, endpoints, session and device notifications, meters, mic detection, the report, the
// levels and the probe flags), WA45 and WA53 the capture, the mixer and the sound check.
//
// Rules for the implementation (05 §4.3, §10.5): every audio call may come from any thread; COM work happens only on
// 05's own MTA threads; rules and strings are copied during the call; audio never fails a share and never makes
// im_next_packet fail; there are no audio events (Go polls ListAppsJson and LevelsGet).
#pragma once

#include <cstdint>
#include <memory>

#include "isshoni_media.h"

namespace im::audio {

// The sink a Mixer feeds every 10 ms: 04's Opus stage and PCM tap (native/common/audio/include/im/audio/pcm_sink.h,
// 04 §10.2). Declared here only as far as a pointer to it needs.
class PcmSink;

struct MixerConfig {
  uint32_t mode;  // IM_AUDIO_MODE_EXCLUDE_APPS (EVERYTHING never gets here in M2: the exports refuse it; OFF makes no
                  // mixer; TEST_TONE uses 04's tone instead)
  uint32_t flags; // IM_AUDIOF_*
  PcmSink *sink;  // owned by the session, outlives the mixer
};

// A share's mixer (05 §9, §10.5).
class Mixer {
public:
  virtual ~Mixer() = default;
  // On the worker inside im_start: returns once the mixer clock runs (<= 100 ms); 05's startup probe goes on
  // asynchronously (05 §11.2).
  virtual im_status Start(const MixerConfig &cfg) = 0;
  // <= 500 ms; no OnChunk or OnGap after it returns.
  virtual void Stop() = 0;
  // From im_set_paused: the pause flag of 05 §9.6 (effective mute = user mute OR paused OR mode off).
  virtual void SetPaused(bool paused) = 0;
  // IM_AUDIO_STATE_*: im_session_info.audio_state and the stats' audio.state.
  virtual uint32_t State() const = 0;
};

// The process-wide audio engine (05 §7, §10.5): the watcher, the report, the levels and the sound check.
class AudioEngine {
public:
  virtual ~AudioEngine() = default;

  // The process-wide instance (05's; the stub's in this skeleton). 04's code reaches it through
  // im::audio_bridge::Engine(), which tests can point at a fake (04 §18.2).
  static AudioEngine &Get();

  // im_init, on 04's worker: starts the watcher thread unless IM_INIT_NO_AUDIO; <= 100 ms. A failure doesn't fail
  // im_init (audio never fails a call): 04 logs it, and the audio calls and the report go on against this engine.
  virtual im_status Init(uint32_t init_flags) = 0;
  // im_shutdown: Mixer::Stop, unwatch, joins every audio thread (<= 2 s); Init may follow. Also called after an Init
  // that failed, and on the worker after an Init that succeeded too late for im_init (04 §6.1).
  virtual void Shutdown() = 0;
  // im_caps.audio_flags (05 §11.1); cached, <= 50 ms the first time. Called by the probe on 04's worker.
  virtual uint32_t ProbeFlags() = 0;
  // im_list_audio_apps (05 §12.1), snprintf rule (04 §5.1).
  virtual int32_t ListAppsJson(uint32_t flags, char *buf, int32_t cap) = 0;
  // im_set_exclusions and im_start_config.exclusions (copied).
  virtual im_status SetRules(const im_exclusions *ex) = 0;
  // im_audio_watch, im_audio_unwatch.
  virtual im_status Watch(const im_exclusions *ex) = 0;
  virtual im_status Unwatch() = 0;
  // im_audio_set_mode (IM_AUDIO_MODE_OFF or IM_AUDIO_MODE_EXCLUDE_APPS; the export refuses the rest).
  virtual im_status SetMode(uint32_t mode) = 0;
  // im_audio_set_muted: the user's (pause) mute.
  virtual im_status SetMuted(bool muted) = 0;
  // im_audio_levels_get (05 §12.2): lock-free; writes out up to its struct_size.
  virtual im_status LevelsGet(im_audio_levels *out) = 0;
  // im_audio_sound_check (05 §11.3): timeout_ms 1000..10000; snprintf rule.
  virtual int32_t SoundCheck(uint32_t helper_pid, int32_t timeout_ms, char *buf, int32_t cap) = 0;
  // A share's mixer (im_start with IM_AUDIO_MODE_EXCLUDE_APPS); nullptr when audio can't be captured.
  virtual std::unique_ptr<Mixer> NewMixer() = 0;
};

} // namespace im::audio
