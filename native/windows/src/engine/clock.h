// The engine clock and im_clock (docs/m2/04-windows-engine.md §5.3, §12.3).
#pragma once

#include <cstdint>

#include "isshoni_media.h"

namespace im::engine {

// 04 §12.3's conversion of a QPC reading q at frequency f to ns: q / f * 1e9 + (q % f) * 1e9 / f, in integer
// arithmetic without overflow (for f below 9.2 GHz; Windows reports 10 MHz). Go's qpcNS() uses the same formula, so
// both read the same nanosecond.
constexpr int64_t QpcToNs(int64_t q, int64_t f) noexcept {
  return (q / f) * 1'000'000'000 + (q % f) * 1'000'000'000 / f;
}

// im_clock: three brackets of the engine clock around the wall clock, the tightest kept. mono_ns is the bracket's
// middle, unix_ns the wall clock read inside it and uncertainty_ns half its width. out.struct_size is left to the
// caller (WriteOut sets it).
void SampleClock(im_clock_sample &out) noexcept;

} // namespace im::engine
