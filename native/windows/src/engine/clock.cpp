#include "engine/clock.h"

#include <limits>

#include "engine/os.h"

namespace im::engine {

void SampleClock(im_clock_sample &out) noexcept {
  int64_t best_width = std::numeric_limits<int64_t>::max();
  for (int i = 0; i < 3; ++i) {
    const int64_t before = os::QpcNs();
    const int64_t wall = os::UnixNs();
    const int64_t after = os::QpcNs();
    const int64_t width = after - before;
    if (width < best_width) {
      best_width = width;
      out.mono_ns = before + width / 2;
      out.unix_ns = wall;
    }
  }
  out.uncertainty_ns = best_width / 2;
  out.reserved = 0;
}

} // namespace im::engine
