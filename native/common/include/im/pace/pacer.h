// The pacer (docs/m2/04-windows-engine.md §8.4, §8.5, §9.4): per session, on the video thread, it decides when each
// layer encodes which frame. Event-driven on a nominal schedule per layer; layer 0 sets the cadence, the other layers
// follow their own interval from the same captured frames.
//
//   interval[l] = 1e9 / fps[l]; next_due[l] = -inf until the layer's first encode (and after a re-open or reactivation)
//   for each active layer l:
//     due = now >= next_due[l] - 2 ms
//     if new frame since last_encode[l] and due:                    encode(latest, l); advance(l)   (IDR if wanted)
//     elif IDR wanted and no new frame for one interval[0]:          encode(latest or last, l, IDR); advance(l)
//     elif no new frame for 200 ms and refresh due[l]:               encode(last, l, REFRESH); advance(l)
//   advance(l): next_due[l] = now - next_due[l] > interval[l] ? now + interval[l] : next_due[l] + interval[l]
//
// So a source at or below the target is encoded the moment a frame arrives, a faster source averages exactly the
// target (the schedule advances one interval per encode, whenever it happened), and a source that resumes after a
// static stretch gets no catch-up burst (the resync). Also here:
// - IDRs: the first frame of a layer, after a re-open, after reactivation and after a ring overflow are required at
//   once (RequireKeyframe); requested ones (PLI, a stale drop) coalesce and wait for the 250 ms mark after the last
//   IDR; the time-based GOP forces one when gop_ms passed since the last IDR and the content changed (a new frame);
//   a static screen gets no periodic IDR. A wanted IDR rides on the next new frame when the layer is due, so a
//   moving source never gets a duplicate picture as its IDR; on a static screen (no new frame for one interval of
//   layer 0) it re-encodes the last frame within one interval, and while paused as soon as the layer is due. (04
//   §8.4's "due or no new frame for one interval" with "due" applied to new frames: re-encoding the last frame at
//   the due instant would often beat a 60 Hz source's next frame by the 2 ms tolerance.)
// - Refresh: 200 ms after the last new frame, then every 1 s, the last frame is re-encoded (REFRESH).
// - First keyframe: the first captured frame is encoded at once; if none landed 300 ms after the start, the IDR is
//   encoded from frame 0 (black) and the first real frame follows when it arrives.
// - Pause: captured frames are ignored; only refreshes and IDRs of the frozen last frame are encoded. Resume encodes
//   the newest captured frame at once.
// - Inactive layers encode nothing; reactivation starts with an IDR.
// - Timestamps: a frame keeps its capture time (bumped by 1 µs when not after the layer's previous packet, and never
//   before epoch_ns); a refresh, a static IDR and a frame captured more than 500 ms before its encode are stamped now.
// - Overload: when fpsOut stays below 90 % of min(target, fpsIn) for 3 s, the layer's limit becomes cpu (software
//   encoder) or gpu; it returns to none after 10 s above 95 %. Every edge is reported (TakeLimitChanges).
//
// Pure logic on a caller-supplied clock (now_ns): no OS calls, no allocation after construction. Single-threaded.
#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <limits>
#include <span>
#include <string_view>

#include "im/pace/rate_meter.h"
#include "isshoni_media.h"

namespace im::pace {

inline constexpr int64_t kNever = std::numeric_limits<int64_t>::max();
inline constexpr int64_t kToleranceNs = 2'000'000;            // timer and vsync jitter
inline constexpr int64_t kRefreshAfterNs = 200'000'000;       // first refresh after the last new frame
inline constexpr int64_t kRefreshEveryNs = 1'000'000'000;     // then every second
inline constexpr int64_t kIdrSpacingNs = 250'000'000;         // requested IDRs per layer
inline constexpr int64_t kFirstFrameFallbackNs = 300'000'000; // black IDR when nothing was captured by then
inline constexpr int64_t kStaticFrameNs = 500'000'000;        // older frames are stamped now
inline constexpr int64_t kOverloadAfterNs = 3'000'000'000;    // below 90 % this long: cpu/gpu
inline constexpr int64_t kRecoverAfterNs = 10'000'000'000;    // above 95 % this long: none
inline constexpr int64_t kRateWindowNs = 2'000'000'000;       // fpsIn/fpsOut window (04 §5.6)
inline constexpr int64_t kMinRateWindowNs = 1'000'000'000;    // the overload rule waits for this much data
inline constexpr int64_t kStampStepNs = 1'000;                // the 1 µs bump
inline constexpr double kOverloadRatio = 0.90;
inline constexpr double kRecoverRatio = 0.95;

// The default GOP (04 §6.4): layer 0 4 s, the others 2 s.
constexpr uint32_t DefaultGopMs(uint32_t layer) noexcept { return layer == 0 ? 4000u : 2000u; }

enum class Limit : uint8_t { kNone, kCpu, kGpu };

// "none", "cpu", "gpu" (stats and LIMIT_CHANGED, 04 §5.6).
std::string_view LimitToken(Limit limit) noexcept;

enum class Input : uint8_t {
  kLatest, // the newest captured frame: convert it, then encode
  kLast,   // the frame this layer encoded last, again (its NV12 while still valid for the encoder). Frame 0 means
           // nothing may be used: encode black (the first-keyframe fallback, a paused start)
};

struct Encode {
  uint32_t layer = 0;
  Input input = Input::kLatest;
  uint64_t frame = 0;   // which frame: the pacer's count of frames seen (1, 2, ... in OnFrame order); 0 = black
  bool idr = false;     // force an IDR
  bool refresh = false; // IM_PKT_REFRESH
  int64_t ts_ns = 0;    // the capture_ts_ns to stamp
};

struct LayerConfig {
  uint32_t fps = 60;    // 1..240
  uint32_t gop_ms = 0;  // 0 = DefaultGopMs
  bool active = true;   // false: IM_LAYER_INACTIVE
  bool hardware = true; // the layer's encoder: LIMIT_CHANGED says gpu (true) or cpu
};

struct LimitChange {
  uint32_t layer = 0;
  Limit limit = Limit::kNone;
  double fps = 0; // fpsOut when it changed
};

struct LayerStats {
  double fps_in = 0;      // captured frames per second over the last 2 s
  double fps_out = 0;     // encodes of new frames per second over the last 2 s
  uint64_t encodes = 0;   // every encode (new frames, IDRs, refreshes)
  uint64_t keyframes = 0; // forced IDRs
  uint64_t refreshes = 0;
  uint64_t pacer_drops = 0; // frames superseded while the layer was due (the encoder fell behind); thinning a faster
                            // source to the target isn't counted
  Limit limit = Limit::kNone;
  bool active = false;
};

class Pacer {
public:
  static constexpr uint32_t kMaxLayers = IM_MAX_LAYERS;
  static constexpr std::size_t kMaxLimitChanges = 8;

  // epoch_ns: im_session_info.epoch_ns (no stamp is earlier; the first-keyframe fallback counts from it). At most
  // kMaxLayers layers (more are ignored).
  Pacer(int64_t epoch_ns, std::span<const LayerConfig> layers) noexcept;

  uint32_t layer_count() const noexcept { return layer_count_; }
  bool paused() const noexcept { return paused_; }

  // ---- input
  // The video thread saw a newly published frame. skipped: frames published since the previous OnFrame that it never
  // saw (the triple buffer kept only the newest while the thread was busy encoding).
  void OnFrame(int64_t capture_ts_ns, int64_t now_ns, uint32_t skipped = 0) noexcept;
  // im_request_keyframe, a stale drop (layer or IM_ALL_LAYERS): coalesced, 250 ms after the last IDR at the earliest.
  void RequestKeyframe(uint32_t layer) noexcept;
  // The next encode of the layer must be an IDR, now (a ring overflow; layer or IM_ALL_LAYERS).
  void RequireKeyframe(uint32_t layer) noexcept;
  // The layer's encoder was (re)opened (re-init, fallback, profile or fps change): like a first frame.
  void Reopen(uint32_t layer, int64_t now_ns) noexcept;
  // A new frame rate or GOP (an fps re-init; call Reopen with it).
  void SetFps(uint32_t layer, uint32_t fps, int64_t now_ns) noexcept;
  void SetGop(uint32_t layer, uint32_t gop_ms) noexcept;
  // im_set_layer_active.
  void SetLayerActive(uint32_t layer, bool active, int64_t now_ns) noexcept;
  // im_set_paused.
  void SetPaused(bool on, int64_t now_ns) noexcept;
  // The layer's encoder kind changed between hardware and software.
  void SetHardware(uint32_t layer, bool hardware) noexcept;

  // ---- decisions
  // What to encode now, at most one entry per layer in layer order; the schedule advances as if each encode started
  // now. Returns the number of entries written (out.size() bounds it).
  std::size_t Poll(int64_t now_ns, std::span<Encode> out) noexcept;
  // When Poll must run next if no input arrives, or kNever. After Poll(now) it is later than now.
  int64_t NextWakeNs() const noexcept;
  // Overload edges since the last call, oldest first.
  std::size_t TakeLimitChanges(std::span<LimitChange> out) noexcept;

  LayerStats Stats(uint32_t layer, int64_t now_ns) const noexcept;

private:
  static constexpr int64_t kMinusInf = std::numeric_limits<int64_t>::min() / 4;

  struct Layer {
    LayerConfig cfg;
    int64_t interval = 16'666'667;
    int64_t gop_ns = 4'000'000'000;
    int64_t next_due = kMinusInf;
    uint64_t encoded_frame = 0; // the last captured frame this layer encoded
    bool encoded_any = false;
    bool must_idr = true;
    bool idr_requested = false;
    int64_t last_idr = kMinusInf;
    int64_t last_ts = kMinusInf;
    int64_t refresh_at = kNever;
    RateMeter<512> out_meter;     // encodes of new frames
    int64_t measure_since = 0;    // the overload rule's data starts here
    int64_t below_since = kNever; // ratio below 90 % since
    int64_t above_since = kNever; // ratio above 95 % since
    LayerStats stats;
  };

  void Configure(Layer &l, const LayerConfig &cfg, uint32_t index) noexcept;
  bool Decide(uint32_t index, int64_t now, Encode *e) noexcept;
  void Apply(Layer &l, const Encode &e, int64_t now) noexcept;
  int64_t Stamp(Layer &l, int64_t ts) const noexcept;
  void EvaluateLimit(uint32_t index, int64_t now) noexcept;
  void ResetMeasure(Layer &l, int64_t now) noexcept;
  void PushLimitChange(const LimitChange &c) noexcept;
  template <class Fn> void ForLayers(uint32_t layer, Fn &&fn) noexcept;

  int64_t epoch_;
  uint32_t layer_count_ = 0;
  std::array<Layer, kMaxLayers> layers_{};
  bool paused_ = false;
  uint64_t latest_frame_ = 0; // sequence number of the newest captured frame (0 = none yet)
  int64_t latest_ts_ = 0;     // its capture time
  int64_t latest_arrival_ = kMinusInf;
  RateMeter<1024> in_meter_; // captured frames (up to 2 x 240 deliveries per second, §7.3)
  std::array<LimitChange, kMaxLimitChanges> changes_{};
  std::size_t change_count_ = 0;
};

} // namespace im::pace
