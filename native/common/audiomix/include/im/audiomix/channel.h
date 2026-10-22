// One capture stream in the mixer (docs/m2/05-windows-audio.md §9.2-§9.5): its ring, its fill controller and
// resampler, its state (Preroll, Running, Fading), its gain and fades, the onset fade and the silent bypass.
//
// The watcher creates a channel (Create allocates the resampler: §9.3 wants it made before the channel joins), hands
// it to MixEngine::Add and keeps owning it. The mixer thread does everything else; the watcher's calls below only set
// requests the mixer picks up at its next tick. Once released() is true the mixer has let go of the channel for good
// and the watcher may destroy it (and its ring).
#pragma once

#include <atomic>
#include <cstdint>
#include <memory>

#include "im/audiomix/capture_ring.h"
#include "im/audiomix/controller.h"
#include "im/audiomix/resampler.h"
#include "isshoni_media.h"

namespace im::audiomix {

class MixEngine;

// A channel's state as the report's sessions[].stream shows it (05 §12.1): kIdle before the mixer took it.
enum class StreamState : uint8_t {
  kIdle,     // created or added; the mixer hasn't taken it yet
  kPreroll,  // filling up to 40 ms (§9.2)
  kRunning,  // mixed
  kFading,   // fading out before it leaves (removal, failure, a crossfade, a cut)
  kReleased, // the mixer let go of it
};

// The engines whose streams crossfade (§6.5), one group each: the include set's streams, the EXCLUDE stream of the
// fast path (exclude-self, §6.4) and the endpoint stream. A crossfading channel takes over from every channel of every
// other group, so each engine needs its own group: "any -> endpoint" includes exclude-self -> endpoint.
inline constexpr uint8_t kGroupInclude = 0;
inline constexpr uint8_t kGroupExcludeSelf = 1;
inline constexpr uint8_t kGroupEndpoint = 2;

struct ChannelOptions {
  // The stream's ring (required). It must outlive the channel's time in the mixer, i.e. until released().
  CaptureRing *ring = nullptr;
  // The caller's id for this stream: levels and events carry it.
  uint64_t tag = 0;
  // The engine the stream belongs to (kGroupInclude, kGroupExcludeSelf, kGroupEndpoint; any value works).
  uint8_t group = kGroupInclude;
  // An engine switch (§6.5): when the first crossfading channel of its group starts running, the channels of every
  // other group fade out over the same 20 ms in which it fades in (an equal-gain crossfade between engines) and then
  // leave the mixer (those not audible yet leave at once). That takeover happens once per group: from then on every
  // channel of the group that was in the mixer at that tick counts as taken over, so a re-entry after an underrun or
  // a trim fades in over 10 ms and leaves the other groups alone (§9.2). Set it only on the streams that open for a
  // switch; a stream reopened inside the running engine (a device change, §7.5) fades in on its own.
  bool crossfade = false;
  // nullptr: speexdsp (NewSpeexResampler), the production resampler. Tests and simulations pass NewModelResampler().
  std::unique_ptr<Resampler> resampler;
  ControllerParams controller;
};

class Channel {
public:
  // Allocates the channel and, unless options.resampler is set, its speexdsp resampler. IM_ERR_INVALID_ARG without a
  // ring, IM_ERR_INTERNAL when speexdsp fails.
  static im_status Create(ChannelOptions options, std::unique_ptr<Channel> *out);
  ~Channel();
  Channel(const Channel &) = delete;
  Channel &operator=(const Channel &) = delete;

  // ---- the watcher's requests (any thread)

  // The stream goes: it fades out over 10 ms of its buffered audio, then leaves the mixer (§9.2, §9.4). A channel
  // that isn't audible yet (Preroll) leaves at once.
  void Remove() noexcept { remove_.store(true, std::memory_order_release); }
  // The stream failed (§8.5): the same fade, then it leaves.
  void Fail() noexcept { fail_.store(true, std::memory_order_release); }
  // The fast path's exit (§6.4): the ring is cut at the frame captured at qpc_ns, with a 5 ms fade that ends there,
  // and the channel leaves. If that frame was already mixed, it fades out over 5 ms at once (a late cut, counted in
  // MixCounters::fastpath_late_cuts and the event kFastpathLateCut).
  void CutAt(int64_t qpc_ns) noexcept {
    cut_qpc_.store(qpc_ns, std::memory_order_relaxed);
    cut_.store(true, std::memory_order_release);
  }

  // ---- state (any thread)
  bool released() const noexcept { return released_.load(std::memory_order_acquire); }
  StreamState state() const noexcept { return state_.load(std::memory_order_acquire); }
  uint64_t tag() const noexcept { return tag_; }
  uint8_t group() const noexcept { return group_; }
  CaptureRing *ring() const noexcept { return ring_; }
  int32_t ppm() const noexcept { return ppm_.load(std::memory_order_relaxed); }
  int64_t age_ns() const noexcept { return age_ns_.load(std::memory_order_relaxed); } // smoothed head age
  // The head age its controller holds now: L, or more when the ring's fill would average less than 40 ms there
  // (controller.h). 0 before it first ran.
  int64_t target_age_ns() const noexcept { return target_pub_.load(std::memory_order_relaxed); }
  uint64_t underruns() const noexcept { return underruns_.load(std::memory_order_relaxed); }
  uint64_t trims() const noexcept { return trims_.load(std::memory_order_relaxed); }
  bool bypassed() const noexcept { return bypass_flag_.load(std::memory_order_relaxed); } // in the silent bypass

private:
  friend class MixEngine;
  explicit Channel(ChannelOptions &&options) noexcept;

  // Set once by the constructor.
  CaptureRing *const ring_;
  const uint64_t tag_;
  const uint8_t group_;
  const bool crossfade_;
  std::unique_ptr<Resampler> resampler_;

  // Requests and published state.
  std::atomic<bool> remove_{false};
  std::atomic<bool> fail_{false};
  std::atomic<bool> cut_{false};
  std::atomic<int64_t> cut_qpc_{0};
  std::atomic<bool> released_{false};
  std::atomic<StreamState> state_{StreamState::kIdle};
  std::atomic<int32_t> ppm_{kPpmPerStep};
  std::atomic<int64_t> age_ns_{0};
  std::atomic<int64_t> target_pub_{0};
  std::atomic<uint64_t> underruns_{0};
  std::atomic<uint64_t> trims_{0};
  std::atomic<bool> bypass_flag_{false};

  // The mixer thread's own state.
  FillController ctl_;
  bool joined_ = false;
  StreamState st_ = StreamState::kIdle;
  float gain_ = 0.0f;
  float gain_target_ = 0.0f;
  float gain_step_ = 1.0f / kChunkFrames;
  uint32_t silent_frames_ = 0; // input frames of digital silence before the next one, saturating at 50 ms (§9.5)
  uint32_t onset_pos_ = 0;     // position in the onset ramp; >= its length: no ramp
  uint32_t silent_input_ = 0;  // consumed input frames of digital silence in a row (the bypass rule, §9.3)
  bool bypass_ = false;
  bool cut_active_ = false; // a cut is scheduled
  int64_t cut_left_ = 0;    // input frames before the cut point
  uint32_t cut_fade_ = 1;   // the cut's fade: 5 ms, or what is left when the cut point is nearer
  bool stop_after_ = false; // fade out this chunk, then back to Preroll (an underrun or trim)
  bool trim_after_ = false;
  bool entered_ = false;   // entered Running at this tick
  bool took_over_ = false; // its group took over from the other groups (ChannelOptions::crossfade): no second time
  int64_t floor_ns_ = 0;   // the head age at which its fill averages 40 ms (0: no packet yet), set at each Render
  bool skip_ = false;      // nothing to mix at this tick
  uint64_t seen_overflows_ = 0;
  float last_l_ = 0.0f, last_r_ = 0.0f; // the last two frames this channel added to the bus
  float prev_l_ = 0.0f, prev_r_ = 0.0f;
  uint32_t tail_pos_ = 0;                 // the backstop tail in progress: next frame 1..64, 0 = none
  float tail_v_[2] = {}, tail_s_[2] = {}; // its start value and slope per channel
  float peak_[10] = {};                   // the last 10 ticks' peak and sum of squares (levels over 100 ms, §9.6)
  double sumsq_[10] = {};
  uint32_t level_pos_ = 0;
};

} // namespace im::audiomix
