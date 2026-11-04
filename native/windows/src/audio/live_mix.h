// A share's mixer as 05's engine holds it (docs/m2/05-windows-audio.md §9, §10.5; "05" below): the im_audiomix mixer
// thread with its Windows clock and its sink, shared between the share's audio::Mixer (live_mixer.cpp), which starts
// and stops the thread, and the watcher, which feeds its engine the include set's channels and reads its levels,
// counters and events. Shared ownership keeps it alive for a watcher that is late to detach.
#pragma once

#include <atomic>
#include <cstdint>
#include <memory>

#include "audio/mixer.h"
#include "im/audiomix/mixer.h"
#include "isshoni_media.h"

namespace im::audio {

struct LiveMix {
  // The sink and the clock first: the mixer, declared last, is destroyed (stopped) before them.
  std::unique_ptr<audiomix::ChunkSink> sink;  // forwards to the session's PcmSink
  std::unique_ptr<audiomix::TickClock> clock; // the high-resolution waitable timer on the QPC (05 §9.1)
  std::unique_ptr<audiomix::Mixer> mixer;
  uint32_t flags = 0; // IM_AUDIOF_* of im_start
  // IM_AUDIO_STATE_* for Mixer::State, published by the watcher (05 §12.1's state).
  std::atomic<uint32_t> state{IM_AUDIO_STATE_STARTING};

  audiomix::MixEngine &engine() noexcept { return mixer->engine(); }
};

// The PcmSink → audiomix::ChunkSink adapter (05 §10.5: im_audiomix can't include 04's pcm_sink.h): forwards both
// calls.
std::unique_ptr<audiomix::ChunkSink> NewPcmSinkAdapter(PcmSink *sink);

// The mixer clock of 05 §9.1: the engine clock (QPC ns) and a high-resolution waitable timer
// (CREATE_WAITABLE_TIMER_HIGH_RESOLUTION) for the waits.
std::unique_ptr<audiomix::TickClock> NewWindowsTickClock();

// MixerOptions::thread_hook: MMCSS "Pro Audio" and the thread's name on the mixer thread (05 §9.1).
void MixerThreadHook(void *ctx, bool starting);

} // namespace im::audio
