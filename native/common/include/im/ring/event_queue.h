// A session's event queue (docs/m2/04-windows-engine.md §6.2): a bounded multi-producer single-consumer queue of 64
// events with at most 1 KiB of JSON each, delivered through im_next_packet before any media. Producers are the
// supervisor, the worker and the video thread's fallback path (§9.8); real-time threads never push (they set atomic
// flags the supervisor turns into events). A mutex is held for the copy of one event only; the storage is fixed, so
// nothing allocates after construction.
//
// - Overflow drops the oldest event that isn't ENDED; the drops are counted (TakeDropped) for the next RING_OVERFLOW.
// - ENDED is final: the first one is always accepted (making room if needed), and every push after it is refused, so
//   ENDED is always the last event delivered. A second ENDED is refused too (the first cause wins).
// - Events never expire.
#pragma once

#include <atomic>
#include <cstddef>
#include <cstdint>
#include <memory>
#include <mutex>
#include <string_view>

#include "im/ring/signal.h"
#include "isshoni_media.h"

namespace im::ring {

inline constexpr uint32_t kEventCapacity = 64;     // 04 §6.2
inline constexpr std::size_t kMaxEventJson = 1024; // 04 §6.4: event JSON <= 1 KiB

struct EventRecord {
  int32_t event = 0; // IM_EVENT_*
  im_status status = IM_OK;
  uint32_t layer = IM_NO_LAYER;
  int64_t ts_ns = 0; // when it happened (engine clock)
  uint32_t size = 0; // bytes of json
  char json[kMaxEventJson] = {};

  std::string_view json_view() const noexcept { return std::string_view(json, size); }
};

enum class EventPush : uint8_t {
  kOk,
  kDroppedOldest, // queued; the oldest non-ENDED event was dropped to make room
  kAfterEnded,    // refused: ENDED is already queued or delivered
  kTooLarge,      // refused: JSON over kMaxEventJson (an ENDED is queued with "{}" instead)
};

class EventQueue {
public:
  // signal may be null (no wake-up, tests). slots: kEventCapacity in the engine (at least 1).
  explicit EventQueue(WakeSignal *signal = nullptr, uint32_t slots = kEventCapacity);
  EventQueue(const EventQueue &) = delete;
  EventQueue &operator=(const EventQueue &) = delete;

  // Any thread. Copies json.
  EventPush Push(int32_t event, im_status status, uint32_t layer, int64_t ts_ns, std::string_view json) noexcept;

  // The consumer: the oldest event, if any. Popping ENDED marks the queue delivered. An empty queue costs one atomic
  // load (im_next_packet's hot path takes no lock).
  bool Pop(EventRecord *out) noexcept;

  // ENDED has been queued (no more events are accepted).
  bool ended() const noexcept;
  // The consumer has popped ENDED.
  bool ended_delivered() const noexcept;
  // Events dropped by overflow since the last call.
  uint64_t TakeDropped() noexcept;
  uint32_t size() const noexcept;
  uint32_t capacity() const noexcept { return capacity_; }

private:
  mutable std::mutex mu_;
  WakeSignal *const signal_;
  const uint32_t capacity_;
  std::unique_ptr<EventRecord[]> records_;
  uint32_t head_ = 0; // the oldest
  uint32_t count_ = 0;
  std::atomic<uint32_t> queued_{0}; // count_, readable without the lock: an empty Pop takes no lock
  bool ended_ = false;
  bool ended_delivered_ = false;
  uint64_t dropped_ = 0;
};

} // namespace im::ring
