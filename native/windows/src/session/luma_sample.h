// The luma tap's reading and its IM_PACKET_DEBUG payload (docs/m2/04-windows-engine.md §5.8, §14 avsync; README
// WF16). Portable C++ without OS headers: luma_tap.h's GPU side (IM_TEST_HOOKS builds only) fills it, the tests and
// Go's binding (internal/media/native/luma.go) read the same bytes.
//
// A session started with kStartFlag in im_start_config.flags (IM_START_TEST_LUMA_TAP, an IM_TEST_HOOKS-only bit that
// the public header doesn't define, §5.8) gets one more stream: for every captured frame the video thread takes, the
// mean BT.709 luma of a fixed region (CenterBox: the centered square of up to kBoxSide pixels of the frame's content)
// with the frame's capture time. The payload is kPayloadBytes little-endian bytes:
//
//   offset  size  field
//   0       4     magic          kMagic ("LUMA" in byte order)
//   4       4     version        kVersion
//   8       8     capture_ts_ns  the frame's capture time (engine clock), the packet's capture_ts_ns too
//   16      4     mean           the region's mean luma × kMeanScale, rounded: 0 black, 65535 white
//   20      4     source_width   the frame's content in its slot (as captured, before any rotation)
//   24      4     source_height
//   28      4     region_x       the region within the content
//   32      4     region_y
//   36      4     region_width
//   40      4     region_height
//   44      4     missed         captured frames since the previous reading that have none (the slots replaced
//                                them before the video thread took one, or the tap's readback queue was full)
//
// Luma is BT.709's Y' = 0.2126 R' + 0.7152 G' + 0.0722 B' of the slot's 8-bit gamma-encoded values, full range (the
// encoders' limited-range conversion comes later, §8.3), so a white flash reads 65535 and black 0.
#pragma once

#include <cstddef>
#include <cstdint>
#include <span>

namespace im::session::luma {

// IM_START_TEST_LUMA_TAP: im_start_config.flags' bit that asks for the tap. IM_TEST_HOOKS builds only; a release build
// refuses it (IM_ERR_INVALID_ARG), so a shipped DLL never emits IM_PACKET_DEBUG.
inline constexpr uint32_t kStartFlag = 0x80000000u;
inline constexpr uint32_t kMagic = 0x414D554Cu; // 'L' 'U' 'M' 'A'
inline constexpr uint32_t kVersion = 1;
inline constexpr uint32_t kPayloadBytes = 48;
inline constexpr uint32_t kBoxSide = 256;
inline constexpr uint32_t kMeanScale = 65535;

struct Region {
  uint32_t x = 0;
  uint32_t y = 0;
  uint32_t width = 0;
  uint32_t height = 0;

  bool empty() const noexcept { return width == 0 || height == 0; }
};

// The fixed region of a frame whose content is width × height: the centered box of min(kBoxSide, side) on each side.
// A square centered box reads the same whatever the frame's rotation (§7.9), and avsync's 1280×720 flash window,
// centered on the primary monitor, covers it on a monitor capture as on a capture of the window itself (§14).
constexpr Region CenterBox(uint32_t width, uint32_t height) noexcept {
  Region r;
  r.width = width < kBoxSide ? width : kBoxSide;
  r.height = height < kBoxSide ? height : kBoxSide;
  r.x = (width - r.width) / 2;
  r.y = (height - r.height) / 2;
  return r;
}

// One pixel's luma in units of 1 / kPixelScale (BT.709's weights × 10 000, times the 8-bit value).
inline constexpr uint64_t kPixelScale = 10'000u * 255u;
constexpr uint32_t PixelLuma(uint8_t r, uint8_t g, uint8_t b) noexcept {
  return 2126u * r + 7152u * g + 722u * b;
}

// Sums the luma of BGRA pixels (DXGI_FORMAT_B8G8R8A8_UNORM's byte order: B, G, R, A).
class Accumulator {
public:
  void AddBgraRow(const uint8_t *row, uint32_t pixels) noexcept {
    for (uint32_t i = 0; i < pixels; ++i) {
      const uint8_t *p = row + static_cast<std::size_t>(i) * 4;
      sum_ += PixelLuma(p[2], p[1], p[0]);
    }
    count_ += pixels;
  }

  uint64_t count() const noexcept { return count_; }

  // The mean × kMeanScale, rounded to nearest; 0 without pixels. No overflow: a 256 × 256 box sums to below 2^38, and
  // × 65535 stays below 2^54.
  uint32_t Mean() const noexcept {
    if (count_ == 0) {
      return 0;
    }
    const uint64_t den = count_ * kPixelScale;
    return static_cast<uint32_t>((sum_ * kMeanScale + den / 2) / den);
  }

private:
  uint64_t sum_ = 0;
  uint64_t count_ = 0;
};

// One reading.
struct Sample {
  int64_t capture_ts_ns = 0;
  uint32_t mean = 0; // × kMeanScale
  uint32_t source_width = 0;
  uint32_t source_height = 0;
  Region region;
  uint32_t missed = 0;
};

namespace detail {
inline void Put32(uint8_t *p, uint32_t v) noexcept {
  for (int i = 0; i < 4; ++i) {
    p[i] = static_cast<uint8_t>(v >> (8 * i));
  }
}
inline uint32_t Get32(const uint8_t *p) noexcept {
  uint32_t v = 0;
  for (int i = 0; i < 4; ++i) {
    v |= static_cast<uint32_t>(p[i]) << (8 * i);
  }
  return v;
}
} // namespace detail

// The payload of s (the table above).
inline void Encode(const Sample &s, std::span<uint8_t, kPayloadBytes> out) noexcept {
  uint8_t *p = out.data();
  const auto ts = static_cast<uint64_t>(s.capture_ts_ns);
  detail::Put32(p + 0, kMagic);
  detail::Put32(p + 4, kVersion);
  detail::Put32(p + 8, static_cast<uint32_t>(ts));
  detail::Put32(p + 12, static_cast<uint32_t>(ts >> 32));
  detail::Put32(p + 16, s.mean);
  detail::Put32(p + 20, s.source_width);
  detail::Put32(p + 24, s.source_height);
  detail::Put32(p + 28, s.region.x);
  detail::Put32(p + 32, s.region.y);
  detail::Put32(p + 36, s.region.width);
  detail::Put32(p + 40, s.region.height);
  detail::Put32(p + 44, s.missed);
}

// Reads a payload: false unless it is kPayloadBytes long with kMagic, kVersion and a mean of at most kMeanScale.
inline bool Decode(std::span<const uint8_t> in, Sample *out) noexcept {
  if (in.size() != kPayloadBytes) {
    return false;
  }
  const uint8_t *p = in.data();
  if (detail::Get32(p) != kMagic || detail::Get32(p + 4) != kVersion || detail::Get32(p + 16) > kMeanScale) {
    return false;
  }
  Sample s;
  s.capture_ts_ns = static_cast<int64_t>(static_cast<uint64_t>(detail::Get32(p + 8)) |
                                         static_cast<uint64_t>(detail::Get32(p + 12)) << 32);
  s.mean = detail::Get32(p + 16);
  s.source_width = detail::Get32(p + 20);
  s.source_height = detail::Get32(p + 24);
  s.region.x = detail::Get32(p + 28);
  s.region.y = detail::Get32(p + 32);
  s.region.width = detail::Get32(p + 36);
  s.region.height = detail::Get32(p + 40);
  s.missed = detail::Get32(p + 44);
  *out = s;
  return true;
}

} // namespace im::session::luma
