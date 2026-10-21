#include "im/os/clock.h"

#include <chrono>

namespace im::os {

int64_t SteadyClock::NowNs() const noexcept {
  return std::chrono::duration_cast<std::chrono::nanoseconds>(std::chrono::steady_clock::now().time_since_epoch())
      .count();
}

const SteadyClock &SteadyClock::Get() noexcept {
  static const SteadyClock clock;
  return clock;
}

} // namespace im::os
