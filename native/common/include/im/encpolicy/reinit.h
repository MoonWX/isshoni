// Live bitrate changes versus re-inits, and the re-init spacing (docs/m2/04-windows-engine.md §9.5, §6.4), per layer.
// Go owns the rate policy (hysteresis, 15 % steps, every 2 s); the engine applies what it is told and guards the
// expensive case:
// - A live encoder (IM_ENCF_LIVE_BITRATE) takes a new bitrate before the next frame; the same value is a no-op.
// - A re-init encoder (AMF, MF, or a live one whose probe saw an IDR) re-opens with an IDR at most once per 10 s per
//   layer: a request inside the window is stored and applied when it ends, the latest value winning (a request back
//   to the current value cancels it).
// - Size, fps, profile and content-hint changes always re-init, at least 1 s apart per layer, coalesced: requests
//   inside the second become one re-init with every reason. A pending bitrate rides along with any re-init (which
//   then counts as the layer's bitrate re-init too).
// Pure logic on a caller-supplied clock; no allocation. The video thread owns it.
#pragma once

#include <cstdint>
#include <limits>
#include <optional>

namespace im::encpolicy {

inline constexpr int64_t kBitrateReinitSpacingNs = 10'000'000'000; // 04 §6.4
inline constexpr int64_t kReinitSpacingNs = 1'000'000'000;

// Why a layer re-inits; a re-init may carry several.
namespace reinit {
inline constexpr uint32_t kSize = 0x01;        // im_update_layer width/height/max_pixels, a stable resize (§7.7)
inline constexpr uint32_t kFps = 0x02;         // im_update_layer fps
inline constexpr uint32_t kProfile = 0x04;     // im_set_profile
inline constexpr uint32_t kContentHint = 0x08; // im_set_content_hint
inline constexpr uint32_t kBitrate = 0x10;     // a re-init encoder's new bitrate
} // namespace reinit

struct ReinitAction {
  uint32_t reasons = 0;     // reinit::*
  uint32_t bitrate_bps = 0; // the bitrate to open with (the newest requested)
};

enum class BitrateDecision : uint8_t {
  kNoop,    // the current value, nothing pending
  kLive,    // apply now (live encoder): bitrate() is the new value
  kPending, // stored for a re-init (Poll returns it when allowed)
};

class ReinitPlanner {
public:
  static constexpr int64_t kNever = std::numeric_limits<int64_t>::max();

  // bitrate_bps: what the encoder was opened with; is_live: IM_ENCF_LIVE_BITRATE of its encoder.
  ReinitPlanner(uint32_t bitrate_bps, bool is_live) noexcept : bitrate_(bitrate_bps), live_(is_live) {}

  // im_set_bitrate (validated by the caller).
  BitrateDecision SetBitrate(uint32_t bps) noexcept;
  // A size, fps, profile or content-hint change (reinit::* other than kBitrate).
  void Request(uint32_t reasons) noexcept { pending_ |= reasons & ~reinit::kBitrate; }
  // The re-init due now, if any; the caller re-opens the encoder with it (and then the planner counts it as done).
  std::optional<ReinitAction> Poll(int64_t now_ns) noexcept;
  // When Poll has something to return, or kNever.
  int64_t NextDeadlineNs() const noexcept;
  // The encoder was re-opened outside Poll (a fallback to another encoder, §9.8) with bitrate_bps (normally
  // target_bitrate(), which applies a pending bitrate) and the new encoder's live flag. It counts for the spacing;
  // other pending reasons stay. A new live encoder takes a still-pending bitrate at once (the caller applies
  // bitrate()).
  void OnReopened(int64_t now_ns, uint32_t bitrate_bps, bool is_live) noexcept;

  uint32_t bitrate() const noexcept { return bitrate_; }
  // The bitrate a re-open should use: the pending one, else the current.
  uint32_t target_bitrate() const noexcept { return pending_bitrate_.value_or(bitrate_); }
  bool live() const noexcept { return live_; }
  uint32_t pending_reasons() const noexcept { return pending_ | (pending_bitrate_ ? reinit::kBitrate : 0u); }

private:
  static constexpr int64_t kMinusInf = std::numeric_limits<int64_t>::min() / 4;

  uint32_t bitrate_;
  bool live_;
  uint32_t pending_ = 0;
  std::optional<uint32_t> pending_bitrate_;
  int64_t last_reinit_ = kMinusInf;
  int64_t last_bitrate_reinit_ = kMinusInf;
};

} // namespace im::encpolicy
