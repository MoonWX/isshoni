#include "im/ring/pump.h"

#include <chrono>

namespace im::ring {

PacketPump::PacketPump(EventQueue *events, WakeSignal *signal, const os::Clock *clock) noexcept
    : events_(events), signal_(signal), clock_(clock) {}

bool PacketPump::AddStream(PacketRing *ring, uint32_t kind, uint32_t layer) noexcept {
  if (ring == nullptr || stream_count_ >= kMaxStreams) {
    return false;
  }
  Stream &s = streams_[stream_count_++];
  s.ring = ring;
  s.kind = kind;
  s.layer = kind == IM_PACKET_VIDEO ? layer : IM_NO_LAYER;
  return true;
}

void PacketPump::Stop() noexcept {
  stopped_.store(true, std::memory_order_release);
  if (signal_ != nullptr) {
    signal_->Notify();
  }
}

void PacketPump::Release() noexcept {
  if (current_ >= 0) {
    streams_[static_cast<uint32_t>(current_)].ring->Pop();
    current_ = -1;
  }
}

bool PacketPump::ReadyFront(Stream &s, int64_t now, PacketView *v) noexcept {
  const bool video = s.kind == IM_PACKET_VIDEO;
  uint64_t dropped = 0;
  bool ready = false;
  while (s.ring->Front(v)) {
    if (now - v->meta.capture_ts_ns > kStaleNs) {
      // More than 1 s old: drop it; video also drops its layer until the next keyframe and asks for one.
      s.ring->Pop();
      ++dropped;
      s.discontinuity = true;
      if (video && !s.await_keyframe) {
        s.await_keyframe = true;
        s.ring->RequestKeyframe();
      }
      continue;
    }
    if (video && s.await_keyframe && (v->meta.flags & IM_PKT_KEYFRAME) == 0) {
      s.ring->Pop();
      ++dropped;
      continue;
    }
    ready = true;
    break;
  }
  if (dropped != 0) {
    s.ring->CountStale(dropped);
  }
  return ready;
}

bool PacketPump::TryNext(Packet *out) noexcept {
  if (events_ != nullptr && events_->Pop(&event_)) {
    *out = Packet{};
    out->kind = IM_PACKET_EVENT;
    out->layer = event_.layer;
    out->capture_ts_ns = event_.ts_ns;
    out->data = reinterpret_cast<const uint8_t *>(event_.json);
    out->size = event_.size;
    out->event = event_.event;
    out->status = event_.status;
    if (event_.event == IM_EVENT_ENDED) {
      ended_ = true;
    }
    return true;
  }
  const int64_t now = clock_ != nullptr ? clock_->NowNs() : 0;
  int32_t best = -1;
  PacketView best_view;
  for (uint32_t i = 0; i < stream_count_; ++i) {
    PacketView v;
    if (!ReadyFront(streams_[i], now, &v)) {
      continue;
    }
    if (best < 0 || v.meta.capture_ts_ns < best_view.meta.capture_ts_ns) {
      best = static_cast<int32_t>(i);
      best_view = v;
    }
  }
  if (best < 0) {
    return false;
  }
  Stream &s = streams_[static_cast<uint32_t>(best)];
  *out = Packet{};
  out->kind = s.kind;
  out->layer = s.layer;
  out->flags = best_view.meta.flags;
  if (s.discontinuity) {
    out->flags |= IM_PKT_DISCONTINUITY;
    s.discontinuity = false;
  }
  if ((best_view.meta.flags & IM_PKT_KEYFRAME) != 0) {
    s.await_keyframe = false;
  }
  out->capture_ts_ns = best_view.meta.capture_ts_ns;
  out->data = best_view.data;
  out->size = best_view.size;
  out->duration_us = best_view.meta.duration_us;
  out->width = best_view.meta.width;
  out->height = best_view.meta.height;
  out->profile = best_view.meta.profile;
  current_ = best;
  return true;
}

im_status PacketPump::Next(uint32_t timeout_ms, Packet *out) noexcept {
  Release();
  if (ended_ || stopped()) {
    return IM_ENDED;
  }
  using Steady = std::chrono::steady_clock;
  const auto deadline = Steady::now() + std::chrono::milliseconds(timeout_ms);
  for (;;) {
    // Read the sequence before looking, so a push between the look and the wait wakes the wait.
    const uint32_t seen = signal_ != nullptr ? signal_->Snapshot() : 0;
    if (stopped()) {
      return IM_ENDED;
    }
    if (TryNext(out)) {
      return IM_OK;
    }
    const auto now = Steady::now();
    if (now >= deadline) {
      return IM_TIMEOUT;
    }
    const auto left_us = std::chrono::duration_cast<std::chrono::microseconds>(deadline - now).count();
    const auto left_ms = static_cast<uint32_t>((left_us + 999) / 1000);
    if (signal_ == nullptr) {
      return IM_TIMEOUT; // nothing can wake us: don't spin
    }
    signal_->WaitFor(seen, left_ms);
  }
}

} // namespace im::ring
