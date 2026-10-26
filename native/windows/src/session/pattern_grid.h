// The test pattern's frame grid (docs/m2/04-windows-engine.md §10.3, §18.2; README WF2): frame k of "test:pattern" at
// hz frames per second is captured at epoch_ns + k / hz seconds, so the frames at whole seconds are the instants the
// test tone's beeps start (04 §10.3) and the GPU pattern's flash box lights on them (gpu/pattern_layout.h). The
// session's supervisor publishes each frame when it is due; under test hooks two of every three frames may be due
// late (a capture whose frames arrive with jitter), their capture times unchanged, as a capture's time precedes its
// delivery. Portable C++ without OS headers: the supervisor (session_object.cpp) runs on it.
#pragma once

#include <algorithm>
#include <cstdint>

namespace im::session {

struct PatternGrid {
  static constexpr int64_t kSecondNs = 1'000'000'000;

  int64_t epoch_ns = 0; // im_session_info.epoch_ns: frame 0's capture time
  uint32_t hz = 60;     // frames per second (0 counts as 1)
  // Frames k with k mod 3 ≠ 0 are due this long after their capture times, clamped below half an interval (test
  // hooks; 0 = every frame is due at its capture time).
  int64_t late_ns = 0;

  uint64_t rate() const noexcept { return std::max<uint32_t>(hz, 1); }

  // Frame k's capture time: exact at whole seconds (k a multiple of hz), nanoseconds rounded down in between.
  int64_t FrameNs(uint64_t k) const noexcept {
    const uint64_t r = rate();
    const uint64_t s = static_cast<uint64_t>(kSecondNs);
    return epoch_ns + static_cast<int64_t>(k / r * s + (k % r) * s / r);
  }

  // When frame k is due at the session: its capture time, or later for the test hooks' late frames (the frames at
  // whole seconds are on time when hz is a multiple of 3).
  int64_t DueNs(uint64_t k) const noexcept {
    if (k % 3 == 0 || late_ns <= 0) {
      return FrameNs(k);
    }
    const int64_t half = kSecondNs / 2 / static_cast<int64_t>(rate());
    return FrameNs(k) + std::min(late_ns, half - 1);
  }

  // The newest frame due at now, but never one before `next` (the next frame not published yet): the frames a late
  // timer missed are skipped, as a capture source keeps only the newest. The supervisor calls it once
  // DueNs(next) <= now.
  uint64_t NewestDue(int64_t now, uint64_t next) const noexcept {
    if (now <= epoch_ns) {
      return next;
    }
    const auto elapsed = static_cast<uint64_t>(now - epoch_ns);
    const uint64_t s = static_cast<uint64_t>(kSecondNs);
    uint64_t k = elapsed / s * rate() + (elapsed % s) * rate() / s;
    while (k > next && DueNs(k) > now) {
      --k;
    }
    return std::max(k, next);
  }
};

} // namespace im::session
