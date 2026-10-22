#include "im/ring/packet_ring.h"

#include <bit>
#include <cstring>

namespace im::ring {

namespace {
uint32_t RoundUpPow2(uint32_t v) noexcept { return v <= 1 ? 1 : std::bit_ceil(v); }
} // namespace

PacketRing::PacketRing(const RingConfig &cfg, WakeSignal *signal)
    : config_(cfg), slot_mask_(RoundUpPow2(cfg.slots) - 1), slots_(new Slot[slot_mask_ + 1]),
      arena_(new uint8_t[cfg.arena_bytes == 0 ? 1 : cfg.arena_bytes]), signal_(signal) {}

// Whether a packet of size bytes fits now; on success *offset is where it goes and *need the arena bytes it takes
// (the skipped tail included).
bool PacketRing::Fits(uint64_t head_slot, uint64_t head_bytes, uint32_t size, uint32_t *offset,
                      uint64_t *need) const noexcept {
  if (head_slot - tail_.load(std::memory_order_acquire) > slot_mask_) {
    return false; // every slot is taken
  }
  const uint64_t arena = config_.arena_bytes;
  const uint64_t used = head_bytes - tail_bytes_.load(std::memory_order_acquire);
  const uint64_t free_bytes = arena - used;
  const uint64_t pos = arena == 0 ? 0 : head_bytes % arena;
  uint64_t pad = 0;
  if (pos + size > arena) {
    pad = arena - pos; // doesn't fit before the end: skip the tail and start at 0
  }
  if (pad + size > free_bytes) {
    return false;
  }
  *offset = pad != 0 ? 0 : static_cast<uint32_t>(pos);
  *need = pad + size;
  return true;
}

PushResult PacketRing::Drop(PushResult why) noexcept {
  discontinuity_ = true;
  if (config_.policy == OverflowPolicy::kVideo) {
    needs_keyframe_.store(true, std::memory_order_release);
  }
  ring_drops_.fetch_add(1, std::memory_order_relaxed);
  overflow_.fetch_add(1, std::memory_order_acq_rel);
  return why;
}

PushResult PacketRing::Push(const PacketMeta &meta, std::span<const uint8_t> data) noexcept {
  if (data.size() > config_.arena_bytes) {
    return Drop(PushResult::kTooLarge);
  }
  const bool keyframe = (meta.flags & IM_PKT_KEYFRAME) != 0;
  if (config_.policy == OverflowPolicy::kVideo && !keyframe && needs_keyframe_.load(std::memory_order_relaxed)) {
    return Drop(PushResult::kRefused);
  }
  const auto size = static_cast<uint32_t>(data.size());
  const uint64_t head = head_.load(std::memory_order_relaxed);
  uint32_t offset = 0;
  uint64_t need = 0;
  if (!Fits(head, head_bytes_, size, &offset, &need)) {
    return Drop(PushResult::kFull);
  }
  if (size != 0) {
    std::memcpy(arena_.get() + offset, data.data(), size);
  }
  Slot &slot = slots_[head & slot_mask_];
  head_bytes_ += need;
  slot.end = head_bytes_;
  slot.offset = offset;
  slot.size = size;
  slot.meta = meta;
  if (discontinuity_) {
    slot.meta.flags |= IM_PKT_DISCONTINUITY;
    discontinuity_ = false;
  }
  if (keyframe) {
    needs_keyframe_.store(false, std::memory_order_release);
  }
  head_.store(head + 1, std::memory_order_release);
  pushed_.fetch_add(1, std::memory_order_relaxed);
  if (signal_ != nullptr) {
    signal_->Notify();
  }
  return PushResult::kOk;
}

bool PacketRing::HasRoom(uint32_t bytes) const noexcept {
  if (bytes > config_.arena_bytes) {
    return false;
  }
  uint32_t offset = 0;
  uint64_t need = 0;
  return Fits(head_.load(std::memory_order_relaxed), head_bytes_, bytes, &offset, &need);
}

RingStats PacketRing::Stats() const noexcept {
  RingStats s;
  s.pushed = pushed_.load(std::memory_order_relaxed);
  s.ring_drops = ring_drops_.load(std::memory_order_relaxed);
  s.stale_drops = stale_drops_.load(std::memory_order_relaxed);
  const uint64_t head = head_.load(std::memory_order_acquire);
  const uint64_t tail = tail_.load(std::memory_order_acquire);
  s.queued = head >= tail ? static_cast<uint32_t>(head - tail) : 0;
  return s;
}

bool PacketRing::Front(PacketView *out) const noexcept {
  const uint64_t tail = tail_.load(std::memory_order_relaxed);
  if (tail == head_.load(std::memory_order_acquire)) {
    return false;
  }
  const Slot &slot = slots_[tail & slot_mask_];
  out->meta = slot.meta;
  out->data = arena_.get() + slot.offset;
  out->size = slot.size;
  return true;
}

void PacketRing::Pop() noexcept {
  const uint64_t tail = tail_.load(std::memory_order_relaxed);
  if (tail == head_.load(std::memory_order_acquire)) {
    return;
  }
  const Slot &slot = slots_[tail & slot_mask_];
  // Free the bytes, then the slot: the producer may reuse both as soon as it sees them.
  tail_bytes_.store(slot.end, std::memory_order_release);
  tail_.store(tail + 1, std::memory_order_release);
}

} // namespace im::ring
