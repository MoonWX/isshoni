#include "im/video/scale.h"

#include <algorithm>
#include <cmath>

namespace im::video {

namespace {

// Source sides are clamped to this before the arithmetic, which keeps every product below 2^62 (real sources are far
// smaller).
constexpr uint64_t kMaxSource = uint64_t{1} << 20;

constexpr uint32_t FloorEven(uint64_t v) noexcept { return static_cast<uint32_t>(v & ~uint64_t{1}); }

// The largest x with x * x * d <= n (d > 0).
uint64_t IsqrtRatio(uint64_t n, uint64_t d) noexcept {
  auto x = static_cast<uint64_t>(std::sqrt(static_cast<double>(n) / static_cast<double>(d)));
  while (x > 0 && x * x * d > n) {
    --x;
  }
  while ((x + 1) * (x + 1) * d <= n) {
    ++x;
  }
  return x;
}

enum class Factor : uint8_t { kOne, kMaxPixels, kWidth, kHeight };

} // namespace

Rect FitRect(Size source, Size dst) noexcept {
  Rect r{0, 0, dst.width, dst.height};
  if (source.width == 0 || source.height == 0 || dst.width == 0 || dst.height == 0) {
    return r;
  }
  const uint64_t sw = source.width;
  const uint64_t sh = source.height;
  const uint64_t dw = dst.width;
  const uint64_t dh = dst.height;
  uint64_t w = dw;
  uint64_t h = dh;
  if (sw * dh >= sh * dw) {
    h = sh * dw / sw; // the source is wider: full width
  } else {
    w = sw * dh / sh; // taller: full height
  }
  w = std::min<uint64_t>(std::max<uint64_t>(FloorEven(w), 2), dw);
  h = std::min<uint64_t>(std::max<uint64_t>(FloorEven(h), 2), dh);
  r.width = static_cast<uint32_t>(w);
  r.height = static_cast<uint32_t>(h);
  r.x = FloorEven((dw - w) / 2);
  r.y = FloorEven((dh - h) / 2);
  return r;
}

OutputSize ComputeOutputSize(Size source, const LayerBounds &bounds) noexcept {
  OutputSize out;
  if (source.width == 0 || source.height == 0) {
    return out;
  }
  const uint64_t sw = std::min<uint64_t>(source.width, kMaxSource);
  const uint64_t sh = std::min<uint64_t>(source.height, kMaxSource);
  // A bound at or above the source can't win over 1: clamp it there, which also bounds the products below.
  const uint64_t mp = bounds.max_pixels == 0 ? 0 : std::min<uint64_t>(bounds.max_pixels, sw * sh);
  const uint64_t bw = bounds.width == 0 ? 0 : std::min<uint64_t>(bounds.width, sw);
  const uint64_t bh = bounds.height == 0 ? 0 : std::min<uint64_t>(bounds.height, sh);

  // s = min(1, s_mp, s_w, s_h), compared exactly: s_mp = sqrt(mp / (sw·sh)), s_w = bw / sw, s_h = bh / sh.
  Factor f = Factor::kOne;
  if (mp != 0 && mp < sw * sh) {
    f = Factor::kMaxPixels;
  }
  if (bw != 0 && bw < sw) {
    // s_w < current? vs 1: bw < sw (true here); vs s_mp: bw² · sh < mp · sw.
    if (f == Factor::kOne || bw * bw * sh < mp * sw) {
      f = Factor::kWidth;
    }
  }
  if (bh != 0 && bh < sh) {
    bool smaller = false;
    switch (f) {
    case Factor::kOne:
      smaller = true;
      break;
    case Factor::kMaxPixels:
      smaller = bh * bh * sw < mp * sh;
      break;
    case Factor::kWidth:
      smaller = bh * sw < bw * sh;
      break;
    case Factor::kHeight:
      break;
    }
    if (smaller) {
      f = Factor::kHeight;
    }
  }

  uint64_t w = sw;
  uint64_t h = sh;
  switch (f) {
  case Factor::kOne:
    break;
  case Factor::kMaxPixels:
    w = IsqrtRatio(sw * mp, sh); // floor(sw · s) = floor(sqrt(sw · mp / sh))
    h = IsqrtRatio(sh * mp, sw);
    break;
  case Factor::kWidth:
    w = bw;
    h = sh * bw / sw;
    break;
  case Factor::kHeight:
    w = sw * bh / sh;
    h = bh;
    break;
  }
  const uint32_t ew = FloorEven(w);
  const uint32_t eh = FloorEven(h);
  out.size.width = std::clamp(ew, kMinSide, kMaxSide);
  out.size.height = std::clamp(eh, kMinSide, kMaxSide);
  out.content = Rect{0, 0, out.size.width, out.size.height};
  if (out.size.width != ew || out.size.height != eh) {
    // Clamping changed a side: fit the content with its aspect, black bars around it.
    out.content = FitRect(source, out.size);
    out.letterboxed = !(out.content == Rect{0, 0, out.size.width, out.size.height});
  }
  return out;
}

} // namespace im::video
