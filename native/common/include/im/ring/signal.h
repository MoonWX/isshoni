// The session's wake-up (docs/m2/04-windows-engine.md §6.2): every producer of a session (the rings, the event queue,
// a stop) bumps one 32-bit sequence and wakes the consumer, which waits on it with im_next_packet's timeout. Lock-free
// and allocation-free on the producer side, so real-time threads may notify. The wake system call is skipped while no
// consumer waits.
#pragma once

#include <atomic>
#include <cstdint>

namespace im::ring {

class WakeSignal {
public:
  WakeSignal() = default;
  WakeSignal(const WakeSignal &) = delete;
  WakeSignal &operator=(const WakeSignal &) = delete;

  // The current sequence. A consumer reads it before it looks for work, then waits with WaitFor(seen, ...), so a
  // notification between the look and the wait is never lost.
  uint32_t Snapshot() const noexcept { return seq_.load(std::memory_order_seq_cst); }

  // Bumps the sequence and wakes the waiting consumer, if any. Any thread, never blocks.
  void Notify() noexcept;

  // Waits until the sequence differs from seen, at most timeout_ms. Returns true when it moved, false on timeout.
  bool WaitFor(uint32_t seen, uint32_t timeout_ms) const noexcept;

private:
  std::atomic<uint32_t> seq_{0};
  mutable std::atomic<uint32_t> waiters_{0};
};

} // namespace im::ring
