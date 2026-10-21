// A layer's output size (docs/m2/04-windows-engine.md §8.3, §7.9, §18.1): §8.3's examples, odd and tiny sizes,
// clamping, and the letterbox fit.
#include <gtest/gtest.h>

#include <cmath>
#include <cstdint>

#include "im/video/scale.h"

namespace {

using im::video::ComputeOutputSize;
using im::video::FitRect;
using im::video::LayerBounds;
using im::video::OutputSize;
using im::video::Rect;
using im::video::Size;

LayerBounds MaxPixels(uint32_t mp) {
  LayerBounds b;
  b.max_pixels = mp;
  return b;
}

constexpr uint32_t kF = 2'073'600; // layer f: 1920×1080 worth of pixels (M1 02 §8.6)
constexpr uint32_t kQ = 230'400;   // layer q: 640×360

TEST(Scale, SpecExamples) {
  EXPECT_EQ(ComputeOutputSize({2560, 1440}, MaxPixels(kF)).size, (Size{1920, 1080}));
  EXPECT_EQ(ComputeOutputSize({3440, 1440}, MaxPixels(kF)).size, (Size{2224, 930}));
  EXPECT_EQ(ComputeOutputSize({1920, 1080}, MaxPixels(kQ)).size, (Size{640, 360})) << "exact: not 638×358";
  const OutputSize tiny = ComputeOutputSize({120, 40}, MaxPixels(kQ));
  EXPECT_EQ(tiny.size, (Size{120, 64}));
  EXPECT_TRUE(tiny.letterboxed);
  EXPECT_EQ(tiny.content, (Rect{0, 12, 120, 40})) << "bars above and below";
}

TEST(Scale, NoUpscaleAndEvenSizes) {
  // s is at most 1: a source within the budget keeps its size (rounded down to even).
  const OutputSize o = ComputeOutputSize({1280, 720}, MaxPixels(kF));
  EXPECT_EQ(o.size, (Size{1280, 720}));
  EXPECT_FALSE(o.letterboxed);
  EXPECT_EQ(o.content, (Rect{0, 0, 1280, 720}));
  EXPECT_EQ(ComputeOutputSize({1920, 1080}, MaxPixels(kF)).size, (Size{1920, 1080}));
  EXPECT_EQ(ComputeOutputSize({1001, 751}, MaxPixels(kF)).size, (Size{1000, 750})) << "odd window: floor to even";
  EXPECT_FALSE(ComputeOutputSize({1001, 751}, MaxPixels(kF)).letterboxed) << "rounding isn't letterboxing";
  EXPECT_EQ(ComputeOutputSize({1001, 751}, LayerBounds{}).size, (Size{1000, 750})) << "no bounds at all";
}

TEST(Scale, OddSourcesUnderABudget) {
  for (uint32_t w = 101; w < 4000; w += 97) {
    for (uint32_t h = 67; h < 2400; h += 89) {
      for (const uint32_t mp : {kF, kQ}) {
        const OutputSize o = ComputeOutputSize({w, h}, MaxPixels(mp));
        EXPECT_EQ(o.size.width % 2, 0u);
        EXPECT_EQ(o.size.height % 2, 0u);
        EXPECT_GE(o.size.width, 64u);
        EXPECT_GE(o.size.height, 64u);
        EXPECT_LE(o.size.width, 4096u);
        EXPECT_LE(o.size.height, 4096u);
        if (!o.letterboxed && o.size.width > 64 && o.size.height > 64) {
          // Within the budget, and as large as the formula allows: s = min(1, sqrt(mp / (w·h))).
          EXPECT_LE(uint64_t{o.size.width} * o.size.height, uint64_t{mp} + 0u) << w << "x" << h;
          const double s = std::min(1.0, std::sqrt(static_cast<double>(mp) / (static_cast<double>(w) * h)));
          EXPECT_NEAR(static_cast<double>(o.size.width), std::floor(w * s / 2) * 2, 2.0) << w << "x" << h;
          EXPECT_NEAR(static_cast<double>(o.size.height), std::floor(h * s / 2) * 2, 2.0) << w << "x" << h;
        }
      }
    }
  }
}

TEST(Scale, WidthAndHeightBounds) {
  LayerBounds b;
  b.width = 1280;
  EXPECT_EQ(ComputeOutputSize({1920, 1080}, b).size, (Size{1280, 720}));
  b.width = 0;
  b.height = 480;
  EXPECT_EQ(ComputeOutputSize({1920, 1080}, b).size, (Size{852, 480})) << "1920·480/1080 = 853.3 → 852";
  // The smallest factor wins: a 1280 box on a portrait source is beaten by the height.
  b.width = 1280;
  b.height = 720;
  EXPECT_EQ(ComputeOutputSize({1080, 1920}, b).size, (Size{404, 720}));
  // max_pixels against a box.
  b.max_pixels = kQ;
  EXPECT_EQ(ComputeOutputSize({1920, 1080}, b).size, (Size{640, 360})) << "the budget is smaller than the box";
  b.max_pixels = kF;
  EXPECT_EQ(ComputeOutputSize({3840, 2160}, b).size, (Size{1280, 720})) << "the box is smaller than the budget";
  // Bounds above the source change nothing.
  LayerBounds big;
  big.width = 8000;
  big.height = 8000;
  big.max_pixels = 0xFFFFFFFFu;
  EXPECT_EQ(ComputeOutputSize({1366, 768}, big).size, (Size{1366, 768}));
}

TEST(Scale, TinySourcesAreLetterboxed) {
  const OutputSize one = ComputeOutputSize({1, 1}, MaxPixels(kQ));
  EXPECT_EQ(one.size, (Size{64, 64}));
  EXPECT_FALSE(one.letterboxed) << "a square stays square: scaled up";
  const OutputSize strip = ComputeOutputSize({300, 10}, MaxPixels(kQ));
  EXPECT_EQ(strip.size, (Size{300, 64}));
  EXPECT_TRUE(strip.letterboxed);
  EXPECT_EQ(strip.content.width, 300u);
  EXPECT_EQ(strip.content.height, 10u);
  EXPECT_EQ(strip.content.y, 26u);
  const OutputSize col = ComputeOutputSize({1, 2000}, LayerBounds{});
  EXPECT_EQ(col.size, (Size{64, 2000}));
  EXPECT_TRUE(col.letterboxed);
  EXPECT_EQ(col.content.width, 2u) << "at least 2 pixels";
  EXPECT_EQ(col.content.x, 30u);
}

TEST(Scale, ClampingAt4096) {
  // Without a budget a huge source is clamped per side, the content letterboxed (04 §8.3 as written).
  const OutputSize o = ComputeOutputSize({8000, 1000}, LayerBounds{});
  EXPECT_EQ(o.size, (Size{4096, 1000}));
  EXPECT_TRUE(o.letterboxed);
  EXPECT_EQ(o.content, (Rect{0, 244, 4096, 512}));
  const OutputSize both = ComputeOutputSize({7680, 4320}, LayerBounds{});
  EXPECT_EQ(both.size, (Size{4096, 4096}));
  EXPECT_EQ(both.content, (Rect{0, 896, 4096, 2304}));
  // With layer f's budget the clamp is never reached.
  EXPECT_EQ(ComputeOutputSize({7680, 4320}, MaxPixels(kF)).size, (Size{1920, 1080}));
}

TEST(Scale, InvalidSource) {
  EXPECT_EQ(ComputeOutputSize({0, 1080}, MaxPixels(kF)).size, (Size{0, 0}));
  EXPECT_EQ(ComputeOutputSize({1920, 0}, MaxPixels(kF)).size, (Size{0, 0}));
  // Absurd sizes stay bounded.
  const OutputSize huge = ComputeOutputSize({0xFFFFFFFFu, 0xFFFFFFFFu}, MaxPixels(kF));
  EXPECT_LE(huge.size.width, 4096u);
  EXPECT_GE(huge.size.width, 64u);
}

TEST(Scale, FitRect) {
  EXPECT_EQ(FitRect({1920, 1080}, {1920, 1080}), (Rect{0, 0, 1920, 1080}));
  EXPECT_EQ(FitRect({1280, 1024}, {1920, 1080}), (Rect{284, 0, 1350, 1080})) << "pillarbox, even sizes and offsets";
  EXPECT_EQ(FitRect({2560, 1080}, {1920, 1080}), (Rect{0, 134, 1920, 810})) << "letterbox";
  EXPECT_EQ(FitRect({0, 10}, {640, 360}), (Rect{0, 0, 640, 360})) << "no source: the whole output";
  for (const Rect r : {FitRect({1001, 751}, {1920, 1080}), FitRect({333, 777}, {640, 360})}) {
    EXPECT_EQ(r.x % 2, 0u);
    EXPECT_EQ(r.y % 2, 0u);
    EXPECT_EQ(r.width % 2, 0u);
    EXPECT_EQ(r.height % 2, 0u);
  }
}

} // namespace
