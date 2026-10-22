// The mixer's tick (docs/m2/05-windows-audio.md §9, §10.3, §10.4): every 10 ms it pulls one chunk from each channel,
// fades, sums, soft-clips, applies the silence flags, publishes the levels and hands the chunk to the sink with its
// capture time on the exact grid.
//
// Timestamps (§10.3). A chunk's time is tick_k - L, where tick_k = tick0 + k * 10 ms is computed, never measured, and L
// is the delay from the audio engine's mix to the mixer's pop. L starts at L0 = 50 ms with the first chunk and changes
// only at a new grid:
//   - a silence re-anchor: a channel entered Running at this tick, every chunk since the last grid started, this one
//     included, is digital silence, and the median over running channels of their smoothed head ages, L', differs
//     from L by more than 5 ms. The new grid starts at max(tick - L', previous chunk + 10 ms), and L becomes tick
//     minus that. "Every chunk since the grid started" includes the one being made, so a stream that is already
//     playing when it starts running (the common case of a game playing at share start) never moves the grid, and a
//     jump never lands in sound. Because the new grid can't start before the previous chunk + 10 ms, between two
//     consecutive ticks a re-anchor can only lower L (a later L' leaves the grid where it is).
//   - a resync: the mixer fell more than 100 ms behind (§9.1): the grid restarts at now, at least 10 ms after the
//     previous chunk.
// The sink gets OnGap exactly at the first chunk after Begin, at a resync and at a re-anchor (§10.4), and chunk times
// never decrease. Device changes, engine crossfades, sound off and on and pause keep the grid. Every channel's
// controller holds its head age at L (kept within 40..60 ms, controller.h), so a chunk's time is the QPC time at which
// the audio engine mixed its first frame, also after a re-anchor changed L.
//
// Threads. Begin and Pump run on the mixer thread only (Mixer, or a test driving simulated time); they never block or
// allocate. Add, the silence flags, ReadLevels, counters and DrainEvents are for any other thread and are lock-free.
// Construction allocates.
#pragma once

#include <atomic>
#include <cstdint>

#include "im/audiomix/channel.h"
#include "im/audiomix/constants.h"

namespace im::audiomix {

struct MixParams {
  int64_t initial_delay_ns = 50 * kNsPerMs;  // L0 (§10.3)
  int64_t resync_after_ns = 100 * kNsPerMs;  // behind by more than this: resync (§9.1)
  int64_t reanchor_change_ns = 5 * kNsPerMs; // a re-anchor needs |L' - L| above this (§10.3)
  // The channels' controllers hold their head age at L, kept within these bounds (controller.h): the fill stays
  // above the 20 ms low water and near §9.3's 40 ms whatever L a re-anchor measured.
  int64_t min_target_age_ns = 40 * kNsPerMs;
  int64_t max_target_age_ns = 60 * kNsPerMs;
};

// What the mixer counts (the report's counters, 05 §12.1, as far as they are the mixer's). Monotonic.
struct MixCounters {
  uint64_t chunks = 0;
  uint64_t gaps = 0; // OnGap calls
  uint64_t underruns = 0;
  uint64_t trims = 0;
  uint64_t overflows = 0; // ring overflows of the channels while in the mixer
  uint64_t mixer_resyncs = 0;
  uint64_t reanchors = 0;
  uint64_t fastpath_late_cuts = 0;
  int32_t max_resample_ppm = 0; // the largest |ppm| a channel used
  int64_t delay_ns = 0;         // L now
};

// Events the mixer raises for the report's events[] (05 §12.1 spells them as EventCodeName gives).
enum class MixEventCode : uint8_t {
  kStreamRunning,   // stream_running: a channel entered Running
  kStreamRemoved,   // stream_removed: a channel left the mixer
  kUnderrun,        // underrun
  kTrim,            // trim
  kMixerResync,     // mixer_resync (tag 0)
  kReanchor,        // reanchor (tag 0)
  kFastpathLateCut, // fastpath_late_cut
};
const char *MixEventName(MixEventCode code) noexcept;

struct MixEvent {
  int64_t t_ns = 0; // the tick time
  MixEventCode code = MixEventCode::kStreamRunning;
  uint64_t tag = 0; // the channel's tag; 0 for the mixer's own events
};

// One channel's levels in a snapshot.
struct ChannelLevel {
  uint64_t tag = 0;
  float peak_db = kLevelFloorDb; // max over the last 100 ms of what friends hear from it (post-fade, after the bus's
  float rms_db = kLevelFloorDb;  // silence flags); RMS over the same 100 ms
  bool running = false;          // Running or Fading (else Preroll: nothing heard yet)
};

inline constexpr uint32_t kMaxMixChannels = 64; // 48 include streams + 1 EXCLUDE or endpoint stream, with room for
                                                // handovers (§6.2 step 4, §6.5)

// The levels the mixer published at its last tick (§9.6, for im_audio_levels_get).
struct LevelsSnapshot {
  uint64_t seq = 0; // ticks published so far (0: none yet)
  int64_t t_ns = 0; // the tick time of the sample
  float mix_peak_db = kLevelFloorDb;
  float mix_rms_db = kLevelFloorDb;
  uint32_t count = 0; // entries in channels
  ChannelLevel channels[kMaxMixChannels];
};

// Per-app levels sum the app's channels (§9.6): linear peaks add up (the bound of the sum), powers add up.
struct LevelSum {
  double peak = 0;
  double power = 0;
  void Add(float peak_dbfs, float rms_dbfs) noexcept;
  float peak_db() const noexcept;
  float rms_db() const noexcept;
};

class MixEngine {
public:
  static constexpr uint32_t kEventCapacity = 256;

  // sink must outlive the engine.
  explicit MixEngine(ChunkSink *sink, const MixParams &params = {});
  MixEngine(const MixEngine &) = delete;
  MixEngine &operator=(const MixEngine &) = delete;

  // ---- any thread but the mixer's

  // Hands a channel to the mixer (it starts in Preroll at the next tick, its ring emptied first, so nothing captured
  // before the join is mixed: §6.2 step 5's 40 ms window). False when all kMaxMixChannels slots are taken. A
  // channel is added once.
  bool Add(Channel *ch) noexcept;
  // The silence flags of §9.6: the bus fades to 0 in 10 ms while any is set. Each is set and cleared only by its own
  // call, so resuming a pause never brings back sound that was switched off.
  void SetPaused(bool on) noexcept { paused_.store(on, std::memory_order_relaxed); }
  void SetMuted(bool on) noexcept { muted_.store(on, std::memory_order_relaxed); }
  void SetSoundOff(bool on) noexcept { sound_off_.store(on, std::memory_order_relaxed); }
  bool paused() const noexcept { return paused_.load(std::memory_order_relaxed); }
  bool muted() const noexcept { return muted_.load(std::memory_order_relaxed); }
  bool sound_off() const noexcept { return sound_off_.load(std::memory_order_relaxed); }
  // The last published levels (a seqlock: lock-free, retried while a tick writes). False if no consistent copy was
  // read in a few tries (a tick was writing each time) or nothing was published yet.
  bool ReadLevels(LevelsSnapshot *out) const noexcept;
  MixCounters counters() const noexcept;
  // Moves up to cap events out (oldest first); returns how many. One reader at a time.
  uint32_t DrainEvents(MixEvent *out, uint32_t cap) noexcept;
  uint64_t dropped_events() const noexcept { return events_dropped_.load(std::memory_order_relaxed); }

  // ---- the mixer thread

  // A run starts: the first chunk is due at now_ns and starts a grid with L = L0.
  void Begin(int64_t now_ns) noexcept;
  // Makes every chunk due at now_ns (one per 10 ms of the grid since Begin; a resync when more than 100 ms behind)
  // and returns the time the next chunk is due.
  int64_t Pump(int64_t now_ns) noexcept;
  int64_t next_due_ns() const noexcept { return next_due_; }
  int64_t delay_ns() const noexcept { return delay_; } // L

  // ---- only while no mixer thread runs

  // Lets go of every channel (each becomes released()).
  void ReleaseAll() noexcept;

private:
  void Tick(int64_t tick_ns) noexcept;
  void Prepare(uint32_t slot, Channel &ch, int64_t tick_ns) noexcept;
  void Render(uint32_t slot, Channel &ch, int64_t tick_ns) noexcept;
  void Release(uint32_t slot, Channel &ch, int64_t tick_ns) noexcept;
  void StartFade(Channel &ch, uint32_t frames) noexcept;
  void EmitTail(Channel &ch, uint32_t from) noexcept;
  void PushEvent(int64_t t_ns, MixEventCode code, uint64_t tag) noexcept;
  void PublishLevels(int64_t tick_ns, float bus_gain) noexcept;
  void Count(std::atomic<uint64_t> &c) noexcept {
    c.store(c.load(std::memory_order_relaxed) + 1, std::memory_order_relaxed);
  }

  ChunkSink *const sink_;
  const MixParams params_;

  std::atomic<Channel *> slots_[kMaxMixChannels] = {};

  std::atomic<bool> paused_{false};
  std::atomic<bool> muted_{false};
  std::atomic<bool> sound_off_{false};

  // Counters (written by the mixer thread only).
  std::atomic<uint64_t> chunks_{0}, gaps_{0}, underruns_{0}, trims_{0}, overflows_{0}, resyncs_{0}, reanchors_{0},
      late_cuts_{0};
  std::atomic<int32_t> max_ppm_{0};
  std::atomic<int64_t> delay_pub_{0};

  // Events: a single-producer single-consumer ring.
  MixEvent events_[kEventCapacity] = {};
  std::atomic<uint32_t> ev_head_{0}, ev_tail_{0};
  std::atomic<uint64_t> events_dropped_{0};

  // Levels: a seqlock over atomics.
  struct LevelSlot {
    std::atomic<uint64_t> tag{0};
    std::atomic<uint32_t> peak{0}, rms{0}; // float bits
    std::atomic<uint32_t> flags{0};        // bit 0: present, bit 1: running
  };
  std::atomic<uint64_t> lv_seq_{0};
  std::atomic<int64_t> lv_t_{0};
  std::atomic<uint32_t> lv_mix_peak_{0}, lv_mix_rms_{0};
  LevelSlot lv_[kMaxMixChannels];

  // The mixer thread's state.
  Channel *cur_[kMaxMixChannels] = {}; // this tick's channels
  int64_t tick0_ = 0;
  int64_t k_ = 0;
  int64_t next_due_ = 0;
  int64_t delay_ = 0;      // L
  int64_t target_age_ = 0; // the controllers' target: L within the bounds
  int64_t prev_chunk_ = 0;
  bool have_prev_ = false;
  bool need_gap_ = false;
  bool resync_ = false;
  bool silent_since_anchor_ = true;
  bool any_entered_ = false;
  float bus_gain_ = 1.0f;
  float bus_peak_[10] = {};
  double bus_sumsq_[10] = {};
  uint32_t bus_level_pos_ = 0;
  float bus_[kChunkSamples] = {};
  float in_[2 * 600] = {};      // a channel's peeked input (up to 600 frames)
  bool in_silent_[600] = {};    // per peeked frame: digital silence (before the onset ramp)
  uint32_t onset_sf_[600] = {}; // the onset state after each peeked frame
  uint32_t onset_op_[600] = {};
  float out_[kChunkSamples] = {}; // a channel's resampled chunk
};

} // namespace im::audiomix
