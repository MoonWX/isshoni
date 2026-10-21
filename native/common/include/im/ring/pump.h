// The consumer side of a session (docs/m2/04-windows-engine.md §6.2, §6.3): what im_next_packet does once its
// arguments are checked. One caller at a time.
//
// - Order: events first; then, among the streams with a ready packet, the one with the smallest capture_ts_ns (ties:
//   the stream added first). Order is strict within a stream, not across streams; nothing is held back.
// - Release: the packet handed out by one call stays valid until the next call (or Release, from im_stop), which
//   frees its slot.
// - Stale packets: a video access unit more than 1 s old at dequeue is dropped, then that layer is dropped until its
//   next keyframe and the producer is asked for one; audio and PCM older than 1 s are dropped too. The next packet
//   delivered from such a stream carries DISCONTINUITY. Events never expire.
// - ENDED: once the ENDED event has been delivered, Next returns IM_ENDED at once. Stop (im_stop, from any thread)
//   makes a blocked or later Next return IM_ENDED within one wake-up.
#pragma once

#include <array>
#include <atomic>
#include <cstdint>

#include "im/os/clock.h"
#include "im/ring/event_queue.h"
#include "im/ring/packet_ring.h"
#include "im/ring/signal.h"
#include "isshoni_media.h"

namespace im::ring {

inline constexpr int64_t kStaleNs = 1'000'000'000;         // 04 §6.2, §6.4
inline constexpr uint32_t kMaxStreams = IM_MAX_LAYERS + 2; // the video layers, Opus and the PCM tap
inline constexpr uint32_t kMaxNextTimeoutMs = 1000;        // im_next_packet's timeout bound (04 §6.4)

// One delivered packet: im_packet's fields without struct_size. data is valid until the next Next or Release.
struct Packet {
  uint32_t kind = 0; // IM_PACKET_*
  uint32_t layer = IM_NO_LAYER;
  uint32_t flags = 0; // IM_PKT_*
  int64_t capture_ts_ns = 0;
  const uint8_t *data = nullptr;
  uint32_t size = 0;
  uint32_t duration_us = 0;
  uint16_t width = 0;
  uint16_t height = 0;
  uint32_t profile = 0;
  int32_t event = 0; // IM_EVENT_*; 0 for media
  im_status status = IM_OK;
};

class PacketPump {
public:
  // Every pointer must outlive the pump. clock is the engine clock the capture timestamps use.
  PacketPump(EventQueue *events, WakeSignal *signal, const os::Clock *clock) noexcept;
  PacketPump(const PacketPump &) = delete;
  PacketPump &operator=(const PacketPump &) = delete;

  // Adds a stream before the first Next: kind IM_PACKET_VIDEO (layer = its index; video rules) or IM_PACKET_AUDIO /
  // IM_PACKET_PCM (layer IM_NO_LAYER). Returns false when kMaxStreams are added or ring is null.
  bool AddStream(PacketRing *ring, uint32_t kind, uint32_t layer) noexcept;

  // Frees the previous packet, then waits up to timeout_ms (callers bound it by kMaxNextTimeoutMs) for the next one:
  // IM_OK (*out filled), IM_TIMEOUT or IM_ENDED.
  im_status Next(uint32_t timeout_ms, Packet *out) noexcept;

  // Any thread: Next returns IM_ENDED from now on (a blocked call within one wake-up).
  void Stop() noexcept;
  bool stopped() const noexcept { return stopped_.load(std::memory_order_acquire); }

  // Frees the packet handed out last (im_stop, after the consumer has returned).
  void Release() noexcept;

private:
  struct Stream {
    PacketRing *ring = nullptr;
    uint32_t kind = 0;
    uint32_t layer = IM_NO_LAYER;
    bool await_keyframe = false; // video: dropping until the next keyframe after a stale drop
    bool discontinuity = false;  // the consumer dropped packets of this stream since the last delivered one
  };

  // Drops what is stale at the front of s; returns whether a packet is ready (in *v).
  bool ReadyFront(Stream &s, int64_t now, PacketView *v) noexcept;
  bool TryNext(Packet *out) noexcept;

  EventQueue *const events_;
  WakeSignal *const signal_;
  const os::Clock *const clock_;
  std::array<Stream, kMaxStreams> streams_{};
  uint32_t stream_count_ = 0;
  int32_t current_ = -1; // the stream whose front was handed out, or -1
  bool ended_ = false;   // ENDED delivered
  std::atomic<bool> stopped_{false};
  EventRecord event_; // the event handed out last (its JSON is the packet's data)
};

} // namespace im::ring
