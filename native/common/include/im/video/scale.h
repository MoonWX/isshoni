// A layer's output size (docs/m2/04-windows-engine.md §8.3, §7.9), computed at start and after every stable resize:
//
//   s = min(1, sqrt(max_pixels / (sw·sh)) if max_pixels, width/sw if width, height/sh if height)
//   out = (floor_even(sw·s), floor_even(sh·s)), then each side clamped to [64, 4096], with the content letterboxed
//   if clamping changed the aspect.
//
// Examples: 2560×1440 with 2 073 600 → 1920×1080; 3440×1440 → 2224×930; the preview's 230 400 of 1920×1080 → 640×360;
// a 120×40 window → 120×64 with bars. The arithmetic is exact (integers only): which factor is the smallest is
// decided by cross-multiplying, and a square root by an integer search, so 1920×1080 / 3 is 640, not 638.
#pragma once

#include <cstdint>

namespace im::video {

inline constexpr uint32_t kMinSide = 64; // 04 §6.4
inline constexpr uint32_t kMaxSide = 4096;

struct Size {
  uint32_t width = 0;
  uint32_t height = 0;
  friend constexpr bool operator==(const Size &, const Size &) = default;
};

struct Rect {
  uint32_t x = 0;
  uint32_t y = 0;
  uint32_t width = 0;
  uint32_t height = 0;
  friend constexpr bool operator==(const Rect &, const Rect &) = default;
};

// A layer's bounds from im_layer_config: 0 means none.
struct LayerBounds {
  uint32_t max_pixels = 0;
  uint32_t width = 0;
  uint32_t height = 0;
};

struct OutputSize {
  Size size;    // the encoder's size: even, each side in [64, 4096]
  Rect content; // where the source goes inside it (the whole output unless letterboxed)
  bool letterboxed = false;
};

// The output size for a source of sw×sh (physical pixels). A source with a zero side gives a zero size.
OutputSize ComputeOutputSize(Size source, const LayerBounds &bounds) noexcept;

// The largest rectangle with the source's aspect inside dst, centered (the letterbox fit, also used while a resize
// is debounced, §7.7). Even sizes and offsets where the box allows (NV12 chroma is 2×2), at least 2×2.
Rect FitRect(Size source, Size dst) noexcept;

} // namespace im::video
