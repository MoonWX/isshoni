#include "im/ring/event_queue.h"

#include <cstring>

namespace im::ring {

EventQueue::EventQueue(WakeSignal *signal, uint32_t slots)
    : signal_(signal), capacity_(slots == 0 ? 1 : slots), records_(new EventRecord[capacity_]) {}

EventPush EventQueue::Push(int32_t event, im_status status, uint32_t layer, int64_t ts_ns,
                           std::string_view json) noexcept {
  const bool is_ended = event == IM_EVENT_ENDED;
  EventPush result = EventPush::kOk;
  if (json.size() > kMaxEventJson) {
    if (!is_ended) {
      return EventPush::kTooLarge;
    }
    json = "{}"; // the status still carries the cause
    result = EventPush::kTooLarge;
  }
  {
    std::lock_guard<std::mutex> lock(mu_);
    if (ended_) {
      return EventPush::kAfterEnded;
    }
    if (count_ == capacity_) {
      // ENDED is never in a full queue here (nothing follows it), so the oldest is a non-ENDED event.
      head_ = (head_ + 1) % capacity_;
      --count_;
      ++dropped_;
      if (result == EventPush::kOk) {
        result = EventPush::kDroppedOldest;
      }
    }
    EventRecord &r = records_[(head_ + count_) % capacity_];
    r.event = event;
    r.status = status;
    r.layer = layer;
    r.ts_ns = ts_ns;
    r.size = static_cast<uint32_t>(json.size());
    if (!json.empty()) {
      std::memcpy(r.json, json.data(), json.size());
    }
    ++count_;
    queued_.store(count_, std::memory_order_release);
    if (is_ended) {
      ended_ = true;
    }
  }
  if (signal_ != nullptr) {
    signal_->Notify();
  }
  return result;
}

bool EventQueue::Pop(EventRecord *out) noexcept {
  // A push stores queued_ before it notifies the session's signal, so a consumer that saw the notification sees it.
  if (queued_.load(std::memory_order_acquire) == 0) {
    return false;
  }
  std::lock_guard<std::mutex> lock(mu_);
  if (count_ == 0) {
    return false;
  }
  const EventRecord &r = records_[head_];
  out->event = r.event;
  out->status = r.status;
  out->layer = r.layer;
  out->ts_ns = r.ts_ns;
  out->size = r.size;
  std::memcpy(out->json, r.json, r.size);
  head_ = (head_ + 1) % capacity_;
  --count_;
  queued_.store(count_, std::memory_order_release);
  if (r.event == IM_EVENT_ENDED) {
    ended_delivered_ = true;
  }
  return true;
}

bool EventQueue::ended() const noexcept {
  std::lock_guard<std::mutex> lock(mu_);
  return ended_;
}

bool EventQueue::ended_delivered() const noexcept {
  std::lock_guard<std::mutex> lock(mu_);
  return ended_delivered_;
}

uint64_t EventQueue::TakeDropped() noexcept {
  std::lock_guard<std::mutex> lock(mu_);
  const uint64_t d = dropped_;
  dropped_ = 0;
  return d;
}

uint32_t EventQueue::size() const noexcept {
  std::lock_guard<std::mutex> lock(mu_);
  return count_;
}

} // namespace im::ring
