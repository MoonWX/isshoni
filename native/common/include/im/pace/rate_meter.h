// Event counting over a sliding time window, for the pacer's fpsIn/fpsOut and its overload rule (docs/m2/04-windows-
// engine.md §5.6, §8.4). A fixed ring of timestamps: no allocation, O(log N) counting. Single-threaded.
#pragma once

#include <array>
#include <cstddef>
#include <cstdint>

namespace im::pace {

template <std::size_t N> class RateMeter {
  static_assert(N >= 2);

public:
  // Records an event at t (times are non-decreasing). Beyond N events the oldest is forgotten, so a window must hold
  // fewer than N events to count exactly (N covers 2 s at the rates the engine sees).
  void Add(int64_t t) noexcept {
    if (count_ == N) {
      head_ = (head_ + 1) % N;
      --count_;
    }
    times_[(head_ + count_) % N] = t;
    ++count_;
  }

  // Events with time > from.
  uint32_t CountSince(int64_t from) const noexcept {
    // Binary search for the first logical index whose time is > from.
    std::size_t lo = 0;
    std::size_t hi = count_;
    while (lo < hi) {
      const std::size_t mid = lo + (hi - lo) / 2;
      if (times_[(head_ + mid) % N] > from) {
        hi = mid;
      } else {
        lo = mid + 1;
      }
    }
    return static_cast<uint32_t>(count_ - lo);
  }

  // The newest event's time, or `none` when there is none.
  int64_t Newest(int64_t none) const noexcept { return count_ == 0 ? none : times_[(head_ + count_ - 1) % N]; }

  void Clear() noexcept {
    head_ = 0;
    count_ = 0;
  }

private:
  std::array<int64_t, N> times_{};
  std::size_t head_ = 0;
  std::size_t count_ = 0;
};

} // namespace im::pace
