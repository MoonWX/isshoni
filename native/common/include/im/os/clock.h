// The engine clock behind an interface (docs/m2/04-windows-engine.md §12.3), so the portable parts that need "now"
// (the packet pump's stale check, §6.2) run on a virtual clock in tests. Capture timestamps, packet times and this
// clock are one time base: QPC nanoseconds on Windows, the monotonic clock elsewhere.
#pragma once

#include <cstdint>

namespace im::os {

class Clock {
public:
  virtual ~Clock() = default;
  // Monotonic nanoseconds.
  virtual int64_t NowNs() const noexcept = 0;
};

// std::chrono::steady_clock in nanoseconds. MSVC's steady_clock is QueryPerformanceCounter converted with 04 §12.3's
// formula (q / f * 1e9 + (q % f) * 1e9 / f), the clock of every capture timestamp on Windows; libstdc++ and libc++
// read the monotonic clock.
class SteadyClock final : public Clock {
public:
  int64_t NowNs() const noexcept override;
  // A process-wide instance (stateless).
  static const SteadyClock &Get() noexcept;
};

} // namespace im::os
