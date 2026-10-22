#include "gpu/pattern_layout.h"

#include <algorithm>

namespace im::gpu {
namespace {

// The bars' colors as R, G, B bit masks over the bar index (bit i = bar i): white, yellow, cyan, green, magenta, red,
// blue, black. pattern_ps.hlsl uses the same masks.
constexpr uint32_t kBarsRed = 0x33;   // bars 0, 1, 4, 5
constexpr uint32_t kBarsGreen = 0x0F; // bars 0, 1, 2, 3
constexpr uint32_t kBarsBlue = 0x55;  // bars 0, 2, 4, 6

constexpr uint32_t Grey(uint32_t v) noexcept { return 0xFF000000u | (v << 16) | (v << 8) | v; }

} // namespace

PatternLayout MakePatternLayout(uint32_t width, uint32_t height) noexcept {
  PatternLayout l;
  l.width = std::max<uint32_t>(width, 2);
  l.height = std::max<uint32_t>(height, 2);
  l.bars_end = l.height / 3;
  l.ramp_end = l.height / 2;
  l.mover_end = l.height * 3 / 4;
  l.counter_end = l.height * 7 / 8;
  l.flash_x = l.width - l.width / 8;
  l.mover_width = std::max<uint32_t>(2, l.width / 40);
  return l;
}

uint32_t PatternMoverX(const PatternLayout &l, int64_t elapsed_ns) noexcept {
  const int64_t e = std::max<int64_t>(elapsed_ns, 0);
  const auto phase = static_cast<uint64_t>(e % kPatternMoverPeriodNs);
  const uint64_t span = l.width > l.mover_width ? l.width - l.mover_width : 0;
  return static_cast<uint32_t>(phase * span / static_cast<uint64_t>(kPatternMoverPeriodNs));
}

bool PatternFlash(int64_t elapsed_ns) noexcept {
  return elapsed_ns >= 0 && elapsed_ns % kPatternFlashEveryNs < kPatternFlashNs;
}

uint32_t PatternPixel(const PatternLayout &l, const PatternFrame &f, uint32_t x, uint32_t y) noexcept {
  if (y < l.bars_end) {
    const uint32_t i = std::min<uint32_t>(x * 8 / l.width, 7);
    const uint32_t r = ((kBarsRed >> i) & 1) * 255;
    const uint32_t g = ((kBarsGreen >> i) & 1) * 255;
    const uint32_t b = ((kBarsBlue >> i) & 1) * 255;
    return 0xFF000000u | (r << 16) | (g << 8) | b;
  }
  if (y < l.ramp_end) {
    return Grey(x * 255 / (l.width - 1));
  }
  if (y < l.mover_end) {
    const uint32_t mx = PatternMoverX(l, f.elapsed_ns);
    return x >= mx && x < mx + l.mover_width ? Grey(255) : Grey(kPatternGrey);
  }
  if (y < l.counter_end) {
    const uint32_t i = std::min<uint32_t>(x * 16 / l.width, 15);
    const uint64_t bit = ((f.index & 0xFFFF) >> (15 - i)) & 1;
    return bit != 0 ? Grey(255) : Grey(0);
  }
  if (x >= l.flash_x) {
    return PatternFlash(f.elapsed_ns) ? Grey(255) : Grey(0);
  }
  return Grey(kPatternGrey);
}

PatternConstants MakePatternConstants(const PatternLayout &l, const PatternFrame &f) noexcept {
  PatternConstants c{};
  c.rows[0] = l.bars_end;
  c.rows[1] = l.ramp_end;
  c.rows[2] = l.mover_end;
  c.rows[3] = l.counter_end;
  c.size[0] = l.width;
  c.size[1] = l.height;
  c.size[2] = l.flash_x;
  c.size[3] = l.mover_width;
  c.frame[0] = PatternMoverX(l, f.elapsed_ns);
  c.frame[1] = static_cast<uint32_t>(f.index & 0xFFFF);
  c.frame[2] = PatternFlash(f.elapsed_ns) ? 1u : 0u;
  return c;
}

} // namespace im::gpu
