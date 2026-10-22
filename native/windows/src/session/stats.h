// A session's numbers and im_get_stats' document (docs/m2/04-windows-engine.md §5.6). The video thread keeps the
// counters and the windows below (fixed arrays: nothing allocates on it) and copies a StatsSnapshot for the caller of
// im_get_stats now and then; StatsJson writes the document from a snapshot.
#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <string>
#include <string_view>

#include "isshoni_media.h"

namespace im::session {

inline constexpr int64_t kStatsWindowNs = 2'000'000'000; // fpsIn, fpsOut and bitrateBps: the last 2 s (§5.6)

// Bytes over the last 2 s: a ring of (time, bytes).
class ByteWindow {
public:
  void Add(int64_t t_ns, uint32_t bytes) noexcept;
  // Bits per second over the window ending at now_ns.
  uint64_t BitsPerSecond(int64_t now_ns) const noexcept;
  void Clear() noexcept { count_ = 0; }

private:
  static constexpr std::size_t kCap = 1024; // 2 s at up to 240 fps, twice over
  struct Entry {
    int64_t t = 0;
    uint32_t bytes = 0;
  };
  std::array<Entry, kCap> e_{};
  std::size_t head_ = 0; // the oldest
  std::size_t count_ = 0;
};

// The latest samples of a duration, for percentiles.
class Samples {
public:
  void Add(int64_t ns) noexcept;
  // The q-th percentile (0..100) in milliseconds; 0 without samples.
  double PercentileMs(double q) const noexcept;

private:
  static constexpr std::size_t kCap = 256;
  std::array<int64_t, kCap> v_{};
  std::size_t next_ = 0;
  std::size_t count_ = 0;
};

struct LayerStatsSnapshot {
  char rid[4] = {};
  std::string_view encoder; // token (§5.6), "" while none is open
  std::string_view codec;
  bool hw = false;
  uint32_t width = 0;
  uint32_t height = 0;
  bool active = false;
  double fps_in = 0;
  double fps_out = 0;
  uint64_t bitrate_bps = 0;
  uint32_t target_bps = 0;
  uint64_t keyframes = 0;
  uint64_t refreshes = 0;
  uint64_t reinits = 0;
  uint64_t drops_pacer = 0;
  uint64_t drops_ring = 0;
  uint64_t drops_stale = 0;
  double encode_p50 = 0, encode_p95 = 0, encode_p99 = 0; // ms
  double latency_p50 = 0, latency_p95 = 0;               // ms: capture time to the packet in the ring
  std::string_view limit = "none";                       // none, cpu, gpu, source
};

struct StatsSnapshot {
  uint32_t layer_count = 0;
  std::array<LayerStatsSnapshot, IM_MAX_LAYERS> layers{};
  std::string_view capture_path; // wgc, dd, test; "" for an audio-only session
  uint32_t source_width = 0;
  uint32_t source_height = 0;
  uint64_t frames = 0; // captured frames
  bool interrupted = false;
  std::string_view audio_state = "off"; // off, starting, ok, degraded, no-device
  uint64_t audio_packets = 0;
  uint64_t audio_drops = 0;
  uint32_t audio_bitrate_bps = 0;
  bool paused = false;
};

// The §5.6 document.
std::string StatsJson(const StatsSnapshot &s);

// IM_AUDIO_STATE_* → its string (05's report "state").
std::string_view AudioStateName(uint32_t state) noexcept;
// IM_CAPTURE_* → "wgc", "dd", "test" (and the later platforms' names), "" for none.
std::string_view CapturePathName(uint32_t path) noexcept;

} // namespace im::session
