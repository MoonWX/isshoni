// What the engine knows about each encoder kind (docs/m2/04-windows-engine.md §5.6, §9.1, §9.5) and the control
// limits of §6.4. The token names an encoder everywhere (stats, ENCODER_CHANGED, Go's media.EncoderInfo.Name,
// enginetest's -encoder flag); the FFmpeg codec name is diagnostic only.
#pragma once

#include <cstdint>
#include <string_view>

#include "isshoni_media.h"

namespace im::encpolicy {

struct EncoderTraits {
  uint32_t kind = IM_ENC_AUTO;
  std::string_view token; // "nvenc", "mf-hw", ...
  std::string_view codec; // "h264_nvenc", ...
  bool hardware = false;
  bool vendor = false;       // NVENC, QSV, AMF: the capture adapter's own encoder, first in the order
  bool live_bitrate = false; // §9.1's table: bitrate changes apply without a re-init (the probe may clear it)
  bool high = false;         // can encode High (6400)
  bool cb = false;           // can encode Constrained Baseline (42e0)
};

// The traits of a kind; an unknown kind gives traits with an empty token.
const EncoderTraits &Traits(uint32_t kind) noexcept;

// The §5.6 token of a kind ("auto" for IM_ENC_AUTO), or "" for an unknown kind.
std::string_view EncoderToken(uint32_t kind) noexcept;
// The diagnostic codec name ("h264_nvenc"), or "".
std::string_view EncoderCodec(uint32_t kind) noexcept;
// The kind of a token, IM_ENC_AUTO for "auto", or kUnknownKind.
inline constexpr uint32_t kUnknownKind = 0xFFFFFFFFu;
uint32_t EncoderKindFromToken(std::string_view token) noexcept;

// The probe flags after §9.10's live-bitrate check: an encoder the table marks live that emitted an IDR after a
// bitrate change loses IM_ENCF_LIVE_BITRATE, which makes it a re-init encoder for 01 and the engine.
constexpr uint32_t ApplyLiveBitrateCheck(uint32_t flags, bool idr_after_change) noexcept {
  return idr_after_change ? (flags & ~IM_ENCF_LIVE_BITRATE) : flags;
}

// How a bitrate change reaches an encoder with these probe flags (§9.5).
enum class BitrateMode : uint8_t {
  kLive,   // set before the next frame, no IDR
  kReinit, // close and re-open with an IDR and REINIT, at most once per 10 s per layer
};
constexpr BitrateMode BitrateModeFor(uint32_t flags) noexcept {
  return (flags & IM_ENCF_LIVE_BITRATE) != 0 ? BitrateMode::kLive : BitrateMode::kReinit;
}

// ------------------------------------------------------------------------------------------- §6.4's control limits
inline constexpr uint32_t kMinBitrateLayer0 = 100'000;
inline constexpr uint32_t kMaxBitrateLayer0 = 50'000'000;
inline constexpr uint32_t kMinBitrateLayer = 50'000;
inline constexpr uint32_t kMaxBitrateLayer = 2'000'000;
inline constexpr uint32_t kMinOpusBitrate = 32'000;
inline constexpr uint32_t kMaxOpusBitrate = 510'000;
inline constexpr uint32_t kMinFps = 1;
inline constexpr uint32_t kMaxFps = 240;
inline constexpr uint32_t kMinGopMs = 1000;
inline constexpr uint32_t kMaxGopMs = 10000;

// Whether bps is a valid video bitrate for the layer (layer 0: 100 kbps .. 50 Mbps; others: 50 kbps .. 2 Mbps).
constexpr bool ValidVideoBitrate(uint32_t layer, uint32_t bps) noexcept {
  return layer == 0 ? (bps >= kMinBitrateLayer0 && bps <= kMaxBitrateLayer0)
                    : (bps >= kMinBitrateLayer && bps <= kMaxBitrateLayer);
}
constexpr bool ValidOpusBitrate(uint32_t bps) noexcept { return bps >= kMinOpusBitrate && bps <= kMaxOpusBitrate; }
constexpr bool ValidFps(uint32_t fps) noexcept { return fps >= kMinFps && fps <= kMaxFps; }
// 0 (the default) or 1000..10000.
constexpr bool ValidGopMs(uint32_t gop_ms) noexcept {
  return gop_ms == 0 || (gop_ms >= kMinGopMs && gop_ms <= kMaxGopMs);
}

} // namespace im::encpolicy
