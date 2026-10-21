#include "im/ring/signal.h"

#include <chrono>

#include "im/os/wake.h"

namespace im::ring {

void WakeSignal::Notify() noexcept {
  // seq_cst on both sides (here: bump, then read waiters_; WaitFor: count itself in, then read seq_) means at least one
  // side sees the other: either the waiter sees the new sequence and doesn't sleep, or this sees the waiter and wakes
  // it.
  seq_.fetch_add(1, std::memory_order_seq_cst);
  if (waiters_.load(std::memory_order_seq_cst) != 0) {
    os::WakeWordAll(seq_);
  }
}

bool WakeSignal::WaitFor(uint32_t seen, uint32_t timeout_ms) const noexcept {
  if (seq_.load(std::memory_order_seq_cst) != seen) {
    return true;
  }
  if (timeout_ms == 0) {
    return false;
  }
  using Clock = std::chrono::steady_clock;
  const auto deadline = Clock::now() + std::chrono::milliseconds(timeout_ms);
  waiters_.fetch_add(1, std::memory_order_seq_cst);
  bool moved = false;
  for (;;) {
    if (seq_.load(std::memory_order_seq_cst) != seen) {
      moved = true;
      break;
    }
    const auto now = Clock::now();
    if (now >= deadline) {
      break;
    }
    // Round the remaining time up to whole milliseconds, so a wait never ends just before its deadline.
    const auto left = std::chrono::duration_cast<std::chrono::microseconds>(deadline - now).count();
    const auto ms = static_cast<uint32_t>((left + 999) / 1000);
    os::WaitWord(seq_, seen, ms);
  }
  waiters_.fetch_sub(1, std::memory_order_seq_cst);
  return moved;
}

} // namespace im::ring
