// The look of the GPU test pattern ("test:pattern", docs/m2/04-windows-engine.md §2 "Test builds", §14; README WA33),
// as integers: test_pattern.cpp's shader draws it from these numbers, and PatternPixel is its CPU model, so a readback
// can be compared exactly. Portable C++.
//
// Bands from top to bottom (H = height, W = width; integer division throughout):
//   rows [0, H/3)          8 color bars (white, yellow, cyan, green, magenta, red, blue, black), bar i = x·8/W  static
//   rows [H/3, H/2)        a grey ramp, value x·255/(W−1)                                                       static
//   rows [H/2, 3H/4)       grey 32 with a white bar max(2, W/40) wide that crosses the band every 2 s
//   rows [3H/4, 7H/8)      the frame counter: 16 blocks (block i = x·16/W), white where bit 15−i of the frame
//                          index is set, else black
//   rows [7H/8, H)         grey 32, and at x ≥ W − W/8 the flash box: white during the first 100 ms of every
//                          second since epoch_ns (when the test tone beeps, 04 §10.3), else black
// The top half is static, so encoders converge on it (openh264_test's PSNR, 04 §18.2); the moving bar and the counter
// make every frame new; the flash box lasts 100 ms so every layer (the preview at 15 fps too) shows each flash.
#pragma once

#include <cstdint>

namespace im::gpu {

inline constexpr uint32_t kPatternGrey = 32;                    // the background
inline constexpr int64_t kPatternFlashNs = 100'000'000;         // the flash box's 100 ms
inline constexpr int64_t kPatternFlashEveryNs = 1'000'000'000;  // once a second
inline constexpr int64_t kPatternMoverPeriodNs = 2'000'000'000; // the bar crosses its band every 2 s

struct PatternLayout {
  uint32_t width = 0;
  uint32_t height = 0;
  uint32_t bars_end = 0;    // H/3
  uint32_t ramp_end = 0;    // H/2
  uint32_t mover_end = 0;   // 3H/4
  uint32_t counter_end = 0; // 7H/8
  uint32_t flash_x = 0;     // W − W/8
  uint32_t mover_width = 0; // max(2, W/40)
};

// The layout for a frame of width × height (each at least 2).
PatternLayout MakePatternLayout(uint32_t width, uint32_t height) noexcept;

// One frame: its index (the counter shows the low 16 bits) and its time since epoch_ns.
struct PatternFrame {
  uint64_t index = 0;
  int64_t elapsed_ns = 0;
};

// The moving bar's left edge, and whether the flash box is white.
uint32_t PatternMoverX(const PatternLayout &l, int64_t elapsed_ns) noexcept;
bool PatternFlash(int64_t elapsed_ns) noexcept;

// The pixel at (x, y) as 0xAARRGGBB (alpha 0xFF).
uint32_t PatternPixel(const PatternLayout &l, const PatternFrame &f, uint32_t x, uint32_t y) noexcept;

// The pattern shader's constant buffer (shaders/pattern_ps.hlsl, register b0): three uint4 rows.
struct PatternConstants {
  uint32_t rows[4];  // bars_end, ramp_end, mover_end, counter_end
  uint32_t size[4];  // width, height, flash_x, mover_width
  uint32_t frame[4]; // mover x, frame index & 0xFFFF, flash (0/1), 0
};
static_assert(sizeof(PatternConstants) == 48, "three uint4 rows");

PatternConstants MakePatternConstants(const PatternLayout &l, const PatternFrame &f) noexcept;

} // namespace im::gpu
