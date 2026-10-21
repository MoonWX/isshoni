// One stream's packet ring (docs/m2/04-windows-engine.md §6.2): a single-producer single-consumer slot array plus a
// byte arena. The producer is the stream's real-time thread (the video thread for a layer, the mixer or test-tone
// thread for Opus and the PCM tap); the consumer is the single im_next_packet caller, through PacketPump. No lock, no
// allocation after construction; a successful push wakes the consumer through the session's WakeSignal.
//
// - A packet's bytes are contiguous in the arena. One that doesn't fit before the arena's end starts again at offset
//   0 (the tail is skipped and freed with it), so a packet of up to half the arena always fits into an empty ring.
// - The consumer peeks the oldest packet with Front and frees it with Pop: the pump keeps the packet it handed out
//   until its next call ("valid until the next call", §6.2).
// - A full ring (no free slot or no room in the arena) drops the packet being pushed and sets DISCONTINUITY on the
//   next packet that gets in. A video ring also raises "needs keyframe": the next encoded frame of that layer must be
//   an IDR, and the ring refuses every non-keyframe access unit until one gets in. Drops are counted for the stats
//   and for the supervisor's RING_OVERFLOW event (TakeOverflowDrops).
// - The consumer drops stale packets (PacketPump) and asks the producer for a keyframe with RequestKeyframe.
#pragma once

#include <atomic>
#include <cstdint>
#include <memory>
#include <span>

#include "im/ring/signal.h"
#include "isshoni_media.h"

namespace im::ring {

enum class OverflowPolicy : uint8_t {
  kVideo, // drop the access unit, raise "needs keyframe", refuse non-keyframes until a keyframe gets in
  kAudio, // drop the newest packet
};

struct RingConfig {
  uint32_t slots;       // rounded up to a power of two
  uint32_t arena_bytes; // the byte arena
  OverflowPolicy policy;
};

// The sizes of 04 §6.2.
inline constexpr RingConfig kLayer0RingConfig{64, 8u << 20, OverflowPolicy::kVideo};  // "f": ~2 s at 30 Mbps
inline constexpr RingConfig kLayerRingConfig{64, 1u << 20, OverflowPolicy::kVideo};   // other layers
inline constexpr RingConfig kOpusRingConfig{128, 256u << 10, OverflowPolicy::kAudio}; // 2.56 s of 20 ms packets
inline constexpr RingConfig kPcmRingConfig{128, 512u << 10, OverflowPolicy::kAudio};  // 1.28 s of 3840-byte chunks

// The ring config of a video layer (layer 0 gets the 8 MiB arena).
constexpr RingConfig VideoRingConfig(uint32_t layer) noexcept {
  return layer == 0 ? kLayer0RingConfig : kLayerRingConfig;
}

// What a slot holds besides the bytes (§6.2: offset, size, flags, capture_ts, width, height, profile, duration).
struct PacketMeta {
  uint32_t flags = 0; // IM_PKT_*
  int64_t capture_ts_ns = 0;
  uint16_t width = 0; // video
  uint16_t height = 0;
  uint32_t profile = 0;     // video: the SPS's key
  uint32_t duration_us = 0; // audio 20000, PCM 10000
};

// A packet as the consumer sees it; data stays valid until Pop.
struct PacketView {
  PacketMeta meta;
  const uint8_t *data = nullptr;
  uint32_t size = 0;
};

enum class PushResult : uint8_t {
  kOk,
  kFull,     // dropped: no free slot or no room in the arena
  kRefused,  // video: dropped, a non-keyframe while the ring waits for a keyframe
  kTooLarge, // dropped: larger than the arena (counted like kFull)
};

struct RingStats {
  uint64_t pushed = 0;      // packets that got in
  uint64_t ring_drops = 0;  // packets the producer dropped (full, refused, too large): stats drops.ring
  uint64_t stale_drops = 0; // packets the consumer dropped as stale: stats drops.stale
  uint32_t queued = 0;      // packets in the ring now
};

class PacketRing {
public:
  // signal may be null (no wake-up, tests).
  explicit PacketRing(const RingConfig &cfg, WakeSignal *signal = nullptr);
  PacketRing(const PacketRing &) = delete;
  PacketRing &operator=(const PacketRing &) = delete;

  // ---- producer (one thread)
  // Copies the packet in. meta.flags gets DISCONTINUITY when packets were dropped since the last one that got in.
  PushResult Push(const PacketMeta &meta, std::span<const uint8_t> data) noexcept;
  // Whether Push of a packet of `bytes` bytes would find a slot and room now (the video thread waits for room before
  // it encodes the IDR a full ring asked for).
  bool HasRoom(uint32_t bytes) const noexcept;
  // Video: an access unit was dropped and no keyframe got in since; the next encoded frame must be an IDR.
  bool NeedsKeyframe() const noexcept { return needs_keyframe_.load(std::memory_order_acquire); }
  // Whether the consumer asked for a keyframe (stale drop) since the last call; clears the request.
  bool TakeKeyframeRequest() noexcept { return keyframe_request_.exchange(false, std::memory_order_acq_rel); }

  // ---- supervisor (any thread)
  // Packets the producer dropped since the last call: the RING_OVERFLOW event's "dropped".
  uint64_t TakeOverflowDrops() noexcept { return overflow_.exchange(0, std::memory_order_acq_rel); }
  RingStats Stats() const noexcept;

  // ---- consumer (one thread)
  // The oldest packet, if any (not removed).
  bool Front(PacketView *out) const noexcept;
  // Frees the oldest packet. No-op on an empty ring.
  void Pop() noexcept;
  // Asks the producer for a keyframe (TakeKeyframeRequest) and counts `dropped` stale packets.
  void RequestKeyframe() noexcept { keyframe_request_.store(true, std::memory_order_release); }
  void CountStale(uint64_t dropped) noexcept { stale_drops_.fetch_add(dropped, std::memory_order_relaxed); }

  const RingConfig &config() const noexcept { return config_; }
  uint32_t slot_count() const noexcept { return slot_mask_ + 1; }

private:
  struct Slot {
    uint64_t end = 0;    // the arena byte counter after this packet (skipped tail included): Pop frees up to it
    uint32_t offset = 0; // where the bytes start in the arena
    uint32_t size = 0;
    PacketMeta meta;
  };

  bool Fits(uint64_t head_slot, uint64_t head_bytes, uint32_t size, uint32_t *offset, uint64_t *need) const noexcept;
  PushResult Drop(PushResult why) noexcept;

  const RingConfig config_;
  const uint32_t slot_mask_;
  std::unique_ptr<Slot[]> slots_;
  std::unique_ptr<uint8_t[]> arena_;
  WakeSignal *const signal_;

  // Producer-owned.
  uint64_t head_bytes_ = 0;    // arena bytes handed out, ever
  bool discontinuity_ = false; // a drop since the last packet that got in
  std::atomic<uint64_t> head_{0};
  std::atomic<bool> needs_keyframe_{false};

  // Consumer-owned.
  std::atomic<uint64_t> tail_{0};
  std::atomic<uint64_t> tail_bytes_{0};
  std::atomic<bool> keyframe_request_{false};

  std::atomic<uint64_t> pushed_{0};
  std::atomic<uint64_t> ring_drops_{0};
  std::atomic<uint64_t> overflow_{0};
  std::atomic<uint64_t> stale_drops_{0};
};

} // namespace im::ring
