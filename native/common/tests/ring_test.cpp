// Rings and packet memory (docs/m2/04-windows-engine.md §6.2, §18.1): wrap-around of slots and arena; an AU that
// doesn't fit at the arena's end wraps; release on the next call; full ring → drop, DISCONTINUITY, needs-IDR flag;
// stale drops; the im_next_packet order (events first, then the smallest capture time); stop and timeouts; stress:
// 3 producer streams and 1 consumer, 10^6 packets, strict order within each stream, no loss unless full. Clean under
// ThreadSanitizer (host-tsan).
#include <gtest/gtest.h>

#include <array>
#include <atomic>
#include <chrono>
#include <cstring>
#include <string>
#include <thread>
#include <vector>

#include "im/os/clock.h"
#include "im/os/wake.h"
#include "im/ring/event_queue.h"
#include "im/ring/packet_ring.h"
#include "im/ring/pump.h"
#include "im/ring/signal.h"
#include "isshoni_media.h"

namespace {

using im::ring::EventQueue;
using im::ring::OverflowPolicy;
using im::ring::Packet;
using im::ring::PacketMeta;
using im::ring::PacketPump;
using im::ring::PacketRing;
using im::ring::PacketView;
using im::ring::PushResult;
using im::ring::RingConfig;
using im::ring::WakeSignal;

using Bytes = std::vector<uint8_t>;
using Steady = std::chrono::steady_clock;

class FakeClock final : public im::os::Clock {
public:
  int64_t NowNs() const noexcept override { return now.load(std::memory_order_relaxed); }
  std::atomic<int64_t> now{0};
};

Bytes Payload(uint32_t size, uint8_t seed) {
  Bytes b(size);
  for (uint32_t i = 0; i < size; ++i) {
    b[i] = static_cast<uint8_t>(seed + i);
  }
  return b;
}

PacketMeta Meta(int64_t ts, uint32_t flags = 0) {
  PacketMeta m;
  m.capture_ts_ns = ts;
  m.flags = flags;
  return m;
}

Bytes Copy(const PacketView &v) { return Bytes(v.data, v.data + v.size); }

TEST(Ring, PresetsFollowTheSpec) {
  EXPECT_EQ(im::ring::kLayer0RingConfig.slots, 64u);
  EXPECT_EQ(im::ring::kLayer0RingConfig.arena_bytes, 8u << 20);
  EXPECT_EQ(im::ring::kLayerRingConfig.arena_bytes, 1u << 20);
  EXPECT_EQ(im::ring::kOpusRingConfig.slots, 128u);
  EXPECT_EQ(im::ring::kOpusRingConfig.arena_bytes, 256u << 10);
  EXPECT_EQ(im::ring::kPcmRingConfig.slots, 128u);
  EXPECT_EQ(im::ring::kPcmRingConfig.arena_bytes, 512u << 10);
  EXPECT_EQ(im::ring::kPcmRingConfig.arena_bytes / 3840u, 136u) << "1.28 s of 10 ms chunks fit";
  EXPECT_EQ(im::ring::VideoRingConfig(0).arena_bytes, 8u << 20);
  EXPECT_EQ(im::ring::VideoRingConfig(1).arena_bytes, 1u << 20);
  PacketRing r(RingConfig{5, 64, OverflowPolicy::kAudio});
  EXPECT_EQ(r.slot_count(), 8u) << "rounded up to a power of two";
}

TEST(Ring, SlotWrapAround) {
  PacketRing r(RingConfig{4, 4096, OverflowPolicy::kAudio});
  for (int i = 0; i < 100; ++i) {
    const Bytes p = Payload(10, static_cast<uint8_t>(i));
    ASSERT_EQ(r.Push(Meta(i), p), PushResult::kOk) << i;
    PacketView v;
    ASSERT_TRUE(r.Front(&v));
    EXPECT_EQ(v.meta.capture_ts_ns, i);
    EXPECT_EQ(Copy(v), p);
    r.Pop();
    EXPECT_FALSE(r.Front(&v));
  }
  // Fill all 4 slots, then a fifth is full by slots although the arena has room.
  for (int i = 0; i < 4; ++i) {
    ASSERT_EQ(r.Push(Meta(i), Payload(1, 0)), PushResult::kOk);
  }
  EXPECT_EQ(r.Push(Meta(4), Payload(1, 0)), PushResult::kFull);
  EXPECT_EQ(r.Stats().queued, 4u);
}

TEST(Ring, ArenaWrapAround) {
  // 100-byte arena: 40 + 40, free the first, then 30 doesn't fit in the last 20 bytes: it starts again at offset 0.
  PacketRing r(RingConfig{8, 100, OverflowPolicy::kAudio});
  ASSERT_EQ(r.Push(Meta(1), Payload(40, 1)), PushResult::kOk);
  PacketView first;
  ASSERT_TRUE(r.Front(&first));
  const uint8_t *base = first.data;
  ASSERT_EQ(r.Push(Meta(2), Payload(40, 2)), PushResult::kOk);
  r.Pop();
  EXPECT_FALSE(r.HasRoom(61)) << "40 bytes are in use: 60 left, not contiguous past the end";
  EXPECT_TRUE(r.HasRoom(30));
  ASSERT_EQ(r.Push(Meta(3), Payload(30, 3)), PushResult::kOk);
  PacketView v;
  ASSERT_TRUE(r.Front(&v));
  EXPECT_EQ(Copy(v), Payload(40, 2));
  r.Pop();
  ASSERT_TRUE(r.Front(&v));
  EXPECT_EQ(v.data, base) << "the AU that didn't fit at the end wrapped to offset 0";
  EXPECT_EQ(Copy(v), Payload(30, 3));
  // The skipped tail is freed with it: after the pop the whole arena is free again.
  r.Pop();
  EXPECT_TRUE(r.HasRoom(50));
  // Many rounds of odd sizes keep every byte intact. Two packets of up to 49 bytes need up to 194 bytes with the
  // skipped tails (a packet that doesn't fit at the end may waste up to 48), so a 256-byte arena always takes them.
  PacketRing odd(RingConfig{8, 256, OverflowPolicy::kAudio});
  std::vector<Bytes> pending;
  for (int i = 0; i < 1000; ++i) {
    const uint32_t size = static_cast<uint32_t>(1 + (i * 37) % 49);
    const Bytes p = Payload(size, static_cast<uint8_t>(i));
    ASSERT_EQ(odd.Push(Meta(i), p), PushResult::kOk) << i;
    pending.push_back(p);
    if (i % 2 == 1) {
      for (const Bytes &want : pending) {
        ASSERT_TRUE(odd.Front(&v));
        EXPECT_EQ(Copy(v), want) << i;
        odd.Pop();
      }
      pending.clear();
      EXPECT_FALSE(odd.Front(&v));
    }
  }
}

TEST(Ring, FullByBytesAndTooLarge) {
  PacketRing r(RingConfig{8, 100, OverflowPolicy::kAudio});
  ASSERT_EQ(r.Push(Meta(1), Payload(60, 1)), PushResult::kOk);
  EXPECT_EQ(r.Push(Meta(2), Payload(50, 2)), PushResult::kFull);
  EXPECT_EQ(r.Push(Meta(3), Payload(101, 3)), PushResult::kTooLarge);
  EXPECT_FALSE(r.HasRoom(101));
  EXPECT_EQ(r.Push(Meta(4), {}), PushResult::kOk) << "an empty packet needs a slot only";
  EXPECT_EQ(r.Stats().ring_drops, 2u);
}

TEST(Ring, VideoFullDropsAndNeedsKeyframe) {
  WakeSignal signal;
  PacketRing r(RingConfig{4, 1 << 16, OverflowPolicy::kVideo}, &signal);
  ASSERT_EQ(r.Push(Meta(0, IM_PKT_KEYFRAME | IM_PKT_REINIT), Payload(100, 0)), PushResult::kOk);
  for (int i = 1; i < 4; ++i) {
    ASSERT_EQ(r.Push(Meta(i), Payload(10, 0)), PushResult::kOk);
  }
  EXPECT_FALSE(r.NeedsKeyframe());
  EXPECT_EQ(r.Push(Meta(4), Payload(10, 0)), PushResult::kFull) << "the AU is dropped";
  EXPECT_TRUE(r.NeedsKeyframe());
  // Room again, but a P frame can't follow the gap: refused until a keyframe gets in.
  r.Pop();
  r.Pop();
  EXPECT_EQ(r.Push(Meta(5), Payload(10, 0)), PushResult::kRefused);
  EXPECT_TRUE(r.NeedsKeyframe());
  EXPECT_TRUE(r.HasRoom(1000));
  ASSERT_EQ(r.Push(Meta(6, IM_PKT_KEYFRAME), Payload(100, 6)), PushResult::kOk);
  EXPECT_FALSE(r.NeedsKeyframe());
  ASSERT_EQ(r.Push(Meta(7), Payload(10, 7)), PushResult::kOk);
  PacketView v;
  std::vector<std::pair<int64_t, uint32_t>> seen;
  while (r.Front(&v)) {
    seen.emplace_back(v.meta.capture_ts_ns, v.meta.flags);
    r.Pop();
  }
  ASSERT_EQ(seen.size(), 4u);
  EXPECT_EQ(seen[0], (std::pair<int64_t, uint32_t>{2, 0u}));
  EXPECT_EQ(seen[1], (std::pair<int64_t, uint32_t>{3, 0u}));
  EXPECT_EQ(seen[2], (std::pair<int64_t, uint32_t>{6, IM_PKT_KEYFRAME | IM_PKT_DISCONTINUITY}))
      << "the first packet after the drops carries DISCONTINUITY";
  EXPECT_EQ(seen[3], (std::pair<int64_t, uint32_t>{7, 0u}));
  EXPECT_EQ(r.TakeOverflowDrops(), 2u) << "the drop and the refusal (RING_OVERFLOW's count)";
  EXPECT_EQ(r.TakeOverflowDrops(), 0u);
  const auto stats = r.Stats();
  EXPECT_EQ(stats.ring_drops, 2u);
  EXPECT_EQ(stats.pushed, 6u);
}

TEST(Ring, AudioFullDropsNewest) {
  PacketRing r(RingConfig{2, 1024, OverflowPolicy::kAudio});
  ASSERT_EQ(r.Push(Meta(0), Payload(10, 0)), PushResult::kOk);
  ASSERT_EQ(r.Push(Meta(1), Payload(10, 1)), PushResult::kOk);
  EXPECT_EQ(r.Push(Meta(2), Payload(10, 2)), PushResult::kFull) << "the newest is dropped";
  EXPECT_FALSE(r.NeedsKeyframe());
  r.Pop();
  ASSERT_EQ(r.Push(Meta(3), Payload(10, 3)), PushResult::kOk) << "audio needs no keyframe";
  PacketView v;
  ASSERT_TRUE(r.Front(&v));
  EXPECT_EQ(v.meta.capture_ts_ns, 1);
  EXPECT_EQ(v.meta.flags, 0u);
  r.Pop();
  ASSERT_TRUE(r.Front(&v));
  EXPECT_EQ(v.meta.capture_ts_ns, 3);
  EXPECT_EQ(v.meta.flags, static_cast<uint32_t>(IM_PKT_DISCONTINUITY));
  EXPECT_EQ(r.TakeOverflowDrops(), 1u);
}

TEST(Ring, KeyframeRequestFromConsumer) {
  PacketRing r(RingConfig{4, 1024, OverflowPolicy::kVideo});
  EXPECT_FALSE(r.TakeKeyframeRequest());
  r.RequestKeyframe();
  EXPECT_TRUE(r.TakeKeyframeRequest());
  EXPECT_FALSE(r.TakeKeyframeRequest()) << "taken";
}

// ------------------------------------------------------------------------------------------------- the pump

struct Session {
  WakeSignal signal;
  FakeClock clock;
  EventQueue events{&signal};
  PacketRing f{RingConfig{16, 1 << 16, OverflowPolicy::kVideo}, &signal};
  PacketRing q{RingConfig{16, 1 << 16, OverflowPolicy::kVideo}, &signal};
  PacketRing audio{RingConfig{16, 1 << 14, OverflowPolicy::kAudio}, &signal};
  PacketPump pump{&events, &signal, &clock};

  Session() {
    EXPECT_TRUE(pump.AddStream(&f, IM_PACKET_VIDEO, 0));
    EXPECT_TRUE(pump.AddStream(&q, IM_PACKET_VIDEO, 1));
    EXPECT_TRUE(pump.AddStream(&audio, IM_PACKET_AUDIO, 0));
  }
};

TEST(Pump, EventsFirstThenSmallestCaptureTime) {
  Session s;
  s.clock.now = 1'000'000;
  s.f.Push(Meta(300, IM_PKT_KEYFRAME), Payload(4, 1));
  s.q.Push(Meta(100, IM_PKT_KEYFRAME), Payload(4, 2));
  s.audio.Push(Meta(200), Payload(4, 3));
  s.f.Push(Meta(400), Payload(4, 4));
  s.events.Push(IM_EVENT_SOURCE_RESIZED, IM_OK, IM_NO_LAYER, 500, R"({"width":1280,"height":720})");
  Packet p;
  ASSERT_EQ(s.pump.Next(0, &p), IM_OK);
  EXPECT_EQ(p.kind, static_cast<uint32_t>(IM_PACKET_EVENT)) << "events first, though stamped later";
  EXPECT_EQ(p.event, IM_EVENT_SOURCE_RESIZED);
  EXPECT_EQ(std::string(reinterpret_cast<const char *>(p.data), p.size), R"({"width":1280,"height":720})");
  std::vector<std::pair<uint32_t, int64_t>> order;
  for (int i = 0; i < 4; ++i) {
    ASSERT_EQ(s.pump.Next(0, &p), IM_OK);
    order.emplace_back(p.kind == IM_PACKET_VIDEO ? p.layer : 99u, p.capture_ts_ns);
  }
  const std::vector<std::pair<uint32_t, int64_t>> want = {{1, 100}, {99, 200}, {0, 300}, {0, 400}};
  EXPECT_EQ(order, want);
  EXPECT_EQ(s.pump.Next(0, &p), IM_TIMEOUT);
}

TEST(Pump, ReleaseOnTheNextCall) {
  Session s;
  s.audio.Push(Meta(1), Payload(8, 1));
  s.audio.Push(Meta(2), Payload(8, 2));
  Packet p;
  ASSERT_EQ(s.pump.Next(0, &p), IM_OK);
  const Bytes first(p.data, p.data + p.size);
  EXPECT_EQ(first, Payload(8, 1));
  EXPECT_EQ(s.audio.Stats().queued, 2u) << "the packet handed out stays until the next call";
  // Its bytes stay intact even when the producer pushes meanwhile.
  s.audio.Push(Meta(3), Payload(8, 3));
  EXPECT_EQ(Bytes(p.data, p.data + p.size), first);
  ASSERT_EQ(s.pump.Next(0, &p), IM_OK);
  EXPECT_EQ(p.capture_ts_ns, 2);
  EXPECT_EQ(s.audio.Stats().queued, 2u) << "the first is freed, the second handed out, the third queued";
  s.pump.Release();
  EXPECT_EQ(s.audio.Stats().queued, 1u) << "Release (im_stop) frees the one handed out";
  s.pump.Release();
  EXPECT_EQ(s.audio.Stats().queued, 1u) << "idempotent";
}

TEST(Pump, PacketFieldsComeThrough) {
  Session s;
  PacketMeta m = Meta(42, IM_PKT_KEYFRAME | IM_PKT_REINIT);
  m.width = 1920;
  m.height = 1080;
  m.profile = IM_PROFILE_HIGH;
  s.f.Push(m, Payload(5, 9));
  PacketMeta a = Meta(50);
  a.duration_us = 20000;
  s.audio.Push(a, Payload(3, 1));
  Packet p;
  ASSERT_EQ(s.pump.Next(0, &p), IM_OK);
  EXPECT_EQ(p.kind, static_cast<uint32_t>(IM_PACKET_VIDEO));
  EXPECT_EQ(p.layer, 0u);
  EXPECT_EQ(p.flags, static_cast<uint32_t>(IM_PKT_KEYFRAME | IM_PKT_REINIT));
  EXPECT_EQ(p.width, 1920u);
  EXPECT_EQ(p.height, 1080u);
  EXPECT_EQ(p.profile, static_cast<uint32_t>(IM_PROFILE_HIGH));
  EXPECT_EQ(p.size, 5u);
  EXPECT_EQ(p.event, 0);
  ASSERT_EQ(s.pump.Next(0, &p), IM_OK);
  EXPECT_EQ(p.kind, static_cast<uint32_t>(IM_PACKET_AUDIO));
  EXPECT_EQ(p.layer, static_cast<uint32_t>(IM_NO_LAYER));
  EXPECT_EQ(p.duration_us, 20000u);
}

TEST(Pump, StaleVideoDropsUntilKeyframeAndAsksForOne) {
  Session s;
  const int64_t second = 1'000'000'000;
  s.clock.now = 10 * second;
  // Old AUs (more than 1 s before now), then a fresh P frame, then a fresh keyframe.
  s.f.Push(Meta(8 * second, IM_PKT_KEYFRAME), Payload(4, 0));
  s.f.Push(Meta(9 * second - 1), Payload(4, 0));
  s.f.Push(Meta(10 * second - 10), Payload(4, 0));
  s.f.Push(Meta(10 * second - 5, IM_PKT_KEYFRAME), Payload(4, 0));
  s.f.Push(Meta(10 * second - 4), Payload(4, 0));
  Packet p;
  ASSERT_EQ(s.pump.Next(0, &p), IM_OK);
  EXPECT_EQ(p.capture_ts_ns, 10 * second - 5) << "stale AUs, then the fresh P frame until the keyframe, dropped";
  EXPECT_EQ(p.flags, static_cast<uint32_t>(IM_PKT_KEYFRAME | IM_PKT_DISCONTINUITY));
  EXPECT_TRUE(s.f.TakeKeyframeRequest()) << "the producer is asked for a keyframe";
  EXPECT_EQ(s.f.Stats().stale_drops, 3u);
  ASSERT_EQ(s.pump.Next(0, &p), IM_OK);
  EXPECT_EQ(p.flags, 0u);
  // Exactly 1 s old is not stale.
  s.f.Push(Meta(9 * second), Payload(4, 0));
  ASSERT_EQ(s.pump.Next(0, &p), IM_OK);
  EXPECT_EQ(p.capture_ts_ns, 9 * second);
}

TEST(Pump, StaleAudioDropped) {
  Session s;
  const int64_t second = 1'000'000'000;
  s.clock.now = 5 * second;
  s.audio.Push(Meta(3 * second), Payload(4, 0));
  s.audio.Push(Meta(4 * second - 1), Payload(4, 0));
  s.audio.Push(Meta(4 * second + 20'000'000), Payload(4, 0));
  Packet p;
  ASSERT_EQ(s.pump.Next(0, &p), IM_OK);
  EXPECT_EQ(p.capture_ts_ns, 4 * second + 20'000'000);
  EXPECT_EQ(p.flags, static_cast<uint32_t>(IM_PKT_DISCONTINUITY));
  EXPECT_EQ(s.audio.Stats().stale_drops, 2u);
  EXPECT_FALSE(s.audio.TakeKeyframeRequest());
  // Events never expire.
  s.events.Push(IM_EVENT_CAPTURE_RESUMED, IM_OK, IM_NO_LAYER, 0, "{}");
  ASSERT_EQ(s.pump.Next(0, &p), IM_OK);
  EXPECT_EQ(p.event, IM_EVENT_CAPTURE_RESUMED);
}

TEST(Pump, EndedIsFinal) {
  Session s;
  s.f.Push(Meta(1, IM_PKT_KEYFRAME), Payload(4, 0));
  s.events.Push(IM_EVENT_ENDED, IM_ERR_SOURCE_GONE, IM_NO_LAYER, 2, R"({"reason":"source_gone"})");
  Packet p;
  ASSERT_EQ(s.pump.Next(0, &p), IM_OK);
  EXPECT_EQ(p.event, IM_EVENT_ENDED);
  EXPECT_EQ(p.status, IM_ERR_SOURCE_GONE);
  EXPECT_EQ(s.pump.Next(0, &p), IM_ENDED) << "media left in the rings is not delivered after ENDED";
  EXPECT_EQ(s.pump.Next(100, &p), IM_ENDED);
}

TEST(Pump, TimeoutAndWakeUp) {
  Session s;
  Packet p;
  const auto t0 = Steady::now();
  EXPECT_EQ(s.pump.Next(30, &p), IM_TIMEOUT);
  const auto waited = std::chrono::duration_cast<std::chrono::milliseconds>(Steady::now() - t0).count();
  EXPECT_GE(waited, 29);
  EXPECT_LT(waited, 1000);
  // A push from another thread wakes a blocked Next.
  std::thread producer([&] {
    std::this_thread::sleep_for(std::chrono::milliseconds(20));
    s.audio.Push(Meta(7), Payload(4, 0));
  });
  const auto t1 = Steady::now();
  EXPECT_EQ(s.pump.Next(1000, &p), IM_OK);
  EXPECT_LT(std::chrono::duration_cast<std::chrono::milliseconds>(Steady::now() - t1).count(), 500);
  EXPECT_EQ(p.capture_ts_ns, 7);
  producer.join();
}

TEST(Pump, StopWakesABlockedConsumerWithin50ms) {
  Session s;
  Packet p;
  std::atomic<im_status> status{IM_OK};
  std::atomic<bool> started{false};
  std::atomic<bool> returned{false};
  std::thread consumer([&] {
    started = true;
    status = s.pump.Next(1000, &p);
    returned = true;
  });
  while (!started) {
    std::this_thread::yield();
  }
  std::this_thread::sleep_for(std::chrono::milliseconds(50));
  EXPECT_FALSE(returned.load()) << "Next blocks until its timeout";
  const auto stop_at = Steady::now();
  s.pump.Stop();
  consumer.join();
  const auto after_stop = std::chrono::duration_cast<std::chrono::milliseconds>(Steady::now() - stop_at).count();
  EXPECT_EQ(status.load(), IM_ENDED);
  EXPECT_LE(after_stop, 50) << "a blocked consumer returns IM_ENDED within 50 ms of im_stop (04 §6.3)";
  EXPECT_EQ(s.pump.Next(0, &p), IM_ENDED);
}

TEST(Pump, StreamLimit) {
  WakeSignal signal;
  FakeClock clock;
  PacketPump pump(nullptr, &signal, &clock);
  std::vector<std::unique_ptr<PacketRing>> rings;
  for (uint32_t i = 0; i < im::ring::kMaxStreams; ++i) {
    rings.push_back(std::make_unique<PacketRing>(RingConfig{4, 64, OverflowPolicy::kAudio}));
    EXPECT_TRUE(pump.AddStream(rings.back().get(), IM_PACKET_AUDIO, 0));
  }
  PacketRing extra(RingConfig{4, 64, OverflowPolicy::kAudio});
  EXPECT_FALSE(pump.AddStream(&extra, IM_PACKET_AUDIO, 0));
  EXPECT_FALSE(pump.AddStream(nullptr, IM_PACKET_AUDIO, 0));
}

// ------------------------------------------------------------------------------------------------- wake-up

TEST(Wake, SignalWaitAndNotify) {
  WakeSignal sig;
  const uint32_t seen = sig.Snapshot();
  EXPECT_FALSE(sig.WaitFor(seen, 0));
  EXPECT_FALSE(sig.WaitFor(seen, 10));
  std::thread t([&] {
    std::this_thread::sleep_for(std::chrono::milliseconds(10));
    sig.Notify();
  });
  EXPECT_TRUE(sig.WaitFor(seen, 2000));
  t.join();
  EXPECT_NE(sig.Snapshot(), seen);
  EXPECT_TRUE(sig.WaitFor(seen, 0)) << "already moved";
}

TEST(Wake, OsWordWaitAndFallback) {
  std::atomic<uint32_t> word{5};
  EXPECT_TRUE(im::os::WaitWord(word, 4, 100)) << "word != expected: no wait";
  // Nobody wakes the word: the wait ends by its timeout (a spurious early return is allowed: retry).
  const auto t0 = Steady::now();
  bool timed_out = false;
  for (int i = 0; i < 100 && !timed_out; ++i) {
    timed_out = !im::os::WaitWord(word, 5, 20);
  }
  EXPECT_TRUE(timed_out);
  EXPECT_GE(std::chrono::duration_cast<std::chrono::milliseconds>(Steady::now() - t0).count(), 15);
  EXPECT_FALSE(im::os::detail::FallbackWaitWord(word, 5, 10));
  EXPECT_TRUE(im::os::detail::FallbackWaitWord(word, 6, 10));
  for (const bool fallback : {false, true}) {
    std::atomic<bool> done{false};
    std::thread waiter([&] {
      while (word.load() == 5) {
        if (fallback) {
          im::os::detail::FallbackWaitWord(word, 5, 1000);
        } else {
          im::os::WaitWord(word, 5, 1000);
        }
      }
      done = true;
    });
    std::this_thread::sleep_for(std::chrono::milliseconds(10));
    word.store(6);
    if (fallback) {
      im::os::detail::FallbackWakeWordAll(word);
    } else {
      im::os::WakeWordOne(word);
    }
    waiter.join();
    EXPECT_TRUE(done.load());
    word.store(5);
  }
  im::os::WakeWordAll(word); // nobody waits: harmless
}

// ------------------------------------------------------------------------------------------------- stress

struct StreamCheck {
  uint64_t delivered = 0;
  uint64_t last_seq = 0;
  bool any = false;
  uint64_t gaps = 0;          // packets missing between delivered ones
  uint64_t bad_gap_flags = 0; // a gap without DISCONTINUITY, or a video gap not ended by a keyframe
  uint64_t out_of_order = 0;
};

// Producers push `per_stream` packets each; each payload holds (stream, seq). wait_for_room: a producer waits while
// its ring is full (then nothing may be lost); otherwise drops must account for every gap.
void Stress(uint64_t per_stream, bool wait_for_room) {
  WakeSignal signal;
  FakeClock clock; // stays at 0: nothing is stale (capture times are small positive numbers)
  EventQueue events(&signal);
  PacketRing f(RingConfig{64, 1 << 16, OverflowPolicy::kVideo}, &signal);
  PacketRing q(RingConfig{64, 1 << 14, OverflowPolicy::kVideo}, &signal);
  PacketRing a(RingConfig{128, 1 << 14, OverflowPolicy::kAudio}, &signal);
  PacketPump pump(&events, &signal, &clock);
  ASSERT_TRUE(pump.AddStream(&f, IM_PACKET_VIDEO, 0));
  ASSERT_TRUE(pump.AddStream(&q, IM_PACKET_VIDEO, 1));
  ASSERT_TRUE(pump.AddStream(&a, IM_PACKET_AUDIO, 0));
  std::array<PacketRing *, 3> rings{&f, &q, &a};
  std::array<std::atomic<uint64_t>, 3> attempts{};
  std::array<std::atomic<uint64_t>, 3> refused_or_full{};
  std::atomic<int> producers_done{0};

  auto produce = [&](uint32_t stream) {
    PacketRing &r = *rings[stream];
    const bool video = stream < 2;
    uint8_t buf[24] = {};
    for (uint64_t seq = 1; seq <= per_stream; ++seq) {
      // Video: a keyframe every 30 AUs, or whenever the ring asks for one.
      const bool key = video && (seq % 30 == 1 || r.NeedsKeyframe() || r.TakeKeyframeRequest());
      PacketMeta m;
      m.flags = key ? IM_PKT_KEYFRAME : 0u;
      m.capture_ts_ns = static_cast<int64_t>(seq * 3 + stream); // strictly increasing per stream
      std::memcpy(buf, &stream, sizeof(stream));
      std::memcpy(buf + 8, &seq, sizeof(seq));
      const uint32_t size = 16 + static_cast<uint32_t>(seq % 8);
      if (wait_for_room) {
        while (!r.HasRoom(size)) {
          std::this_thread::yield();
        }
      }
      attempts[stream].fetch_add(1);
      if (r.Push(m, std::span<const uint8_t>(buf, size)) != PushResult::kOk) {
        refused_or_full[stream].fetch_add(1);
      }
    }
    producers_done.fetch_add(1);
  };

  std::array<StreamCheck, 3> checks{};
  std::thread p0(produce, 0u), p1(produce, 1u), p2(produce, 2u);
  Packet pkt;
  for (;;) {
    const im_status st = pump.Next(5, &pkt);
    if (st == IM_TIMEOUT) {
      if (producers_done.load() == 3) {
        // Drain what is left; a timeout now means every ring is empty.
        if (pump.Next(0, &pkt) == IM_TIMEOUT) {
          break;
        }
      } else {
        continue;
      }
    }
    ASSERT_EQ(pkt.kind == IM_PACKET_EVENT, false);
    uint32_t stream = 0;
    uint64_t seq = 0;
    ASSERT_GE(pkt.size, 16u);
    std::memcpy(&stream, pkt.data, sizeof(stream));
    std::memcpy(&seq, pkt.data + 8, sizeof(seq));
    ASSERT_LT(stream, 3u);
    StreamCheck &c = checks[stream];
    if (c.any && seq <= c.last_seq) {
      ++c.out_of_order;
    }
    const uint64_t expected_next = c.any ? c.last_seq + 1 : 1;
    if (seq > expected_next) {
      c.gaps += seq - expected_next;
      const bool video = stream < 2;
      if ((pkt.flags & IM_PKT_DISCONTINUITY) == 0 || (video && (pkt.flags & IM_PKT_KEYFRAME) == 0)) {
        ++c.bad_gap_flags;
      }
    }
    c.last_seq = seq;
    c.any = true;
    ++c.delivered;
  }
  p0.join();
  p1.join();
  p2.join();
  for (uint32_t s = 0; s < 3; ++s) {
    const StreamCheck &c = checks[s];
    EXPECT_EQ(c.out_of_order, 0u) << "stream " << s << ": strict order within a stream";
    EXPECT_EQ(c.bad_gap_flags, 0u) << "stream " << s;
    EXPECT_EQ(attempts[s].load(), per_stream);
    const uint64_t drops = rings[s]->Stats().ring_drops;
    EXPECT_EQ(drops, refused_or_full[s].load());
    EXPECT_EQ(c.delivered + drops, per_stream) << "stream " << s << ": no loss beyond the counted drops";
    // Drops at the very end leave no gap to see; every gap the consumer saw is a counted drop.
    EXPECT_LE(c.gaps, drops) << "stream " << s;
    if (wait_for_room) {
      EXPECT_EQ(drops, 0u) << "stream " << s << ": no loss unless full";
      EXPECT_EQ(c.delivered, per_stream);
    }
  }
}

TEST(RingStress, ThreeProducersOneConsumerMillionPacketsNoLoss) {
  Stress(1'000'000 / 3 + 1, /*wait_for_room=*/true); // 1 000 002 packets
}

TEST(RingStress, OverflowingProducersLoseOnlyCountedPackets) { Stress(100'000, /*wait_for_room=*/false); }

} // namespace
