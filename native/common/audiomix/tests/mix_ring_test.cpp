// The capture ring and its QPC side ring (docs/m2/05-windows-audio.md §8.3, §19.1 "ring wraparound and QPC side
// ring").
#include <gtest/gtest.h>

#include <atomic>
#include <cmath>
#include <thread>
#include <vector>

#include "im/audiomix/capture_ring.h"
#include "mix_test_support.h"

namespace im::audiomix {
namespace {

// Frames whose left sample is their index and right sample its negative.
std::vector<float> Counting(uint64_t first, uint32_t n) {
  std::vector<float> v(2 * static_cast<std::size_t>(n));
  for (uint32_t i = 0; i < n; ++i) {
    v[2 * i] = static_cast<float>(first + i);
    v[2 * i + 1] = -static_cast<float>(first + i);
  }
  return v;
}

int64_t FrameNs(uint64_t frames) { return static_cast<int64_t>(frames * static_cast<uint64_t>(kNsPerSecond) / kRate); }

// When a packet of n frames whose first frame was mixed at qpc_ns ends: its arrival when it comes without lag.
int64_t Ends(int64_t qpc_ns, uint32_t n) { return qpc_ns + FrameNs(n); }

TEST(CaptureRing, WrapsAroundFrameExact) {
  CaptureRing ring(1000);
  EXPECT_EQ(ring.capacity(), 1000u);
  std::vector<float> got(2 * 1000);
  uint64_t written = 0, read = 0;
  for (int round = 0; round < 50; ++round) {
    const uint32_t n = 300 + static_cast<uint32_t>(round % 7) * 17; // packets that straddle the end at changing points
    auto packet = Counting(written, n);
    ring.Write(packet.data(), n, FrameNs(written), Ends(FrameNs(written), n));
    written += n;
    // Read back in pieces of another size.
    while (ring.Buffered() >= 230) {
      ASSERT_EQ(ring.Peek(got.data(), 230), 230u);
      for (uint32_t i = 0; i < 230; ++i) {
        ASSERT_EQ(got[2 * i], static_cast<float>(read + i)) << "round " << round;
        ASSERT_EQ(got[2 * i + 1], -static_cast<float>(read + i));
      }
      int64_t qpc = 0;
      ASSERT_TRUE(ring.HeadQpc(&qpc));
      // The frame's own time from its packet's entry (to the nanosecond the two roundings of a packet's time plus an
      // offset allow).
      EXPECT_NEAR(static_cast<double>(qpc), static_cast<double>(FrameNs(read)), 1.0);
      ASSERT_TRUE(ring.Consume(230));
      read += 230;
    }
  }
  EXPECT_EQ(ring.overflows(), 0u);
  EXPECT_EQ(ring.written_frames(), written);
}

TEST(CaptureRing, SideRingGivesEachFrameItsPacketsTime) {
  CaptureRing ring;
  // Three packets with gaps between their capture times (a glitch: Windows dropped audio in between).
  auto a = Counting(0, 480), b = Counting(480, 480), c = Counting(960, 100);
  ring.Write(a.data(), 480, 1'000'000'000, Ends(1'000'000'000, 480));
  ring.Write(b.data(), 480, 1'030'000'000, Ends(1'030'000'000, 480)); // 20 ms later than contiguous
  ring.Write(c.data(), 100, 1'040'000'000, Ends(1'040'000'000, 100));
  int64_t qpc = 0;
  ASSERT_TRUE(ring.HeadQpc(&qpc));
  EXPECT_EQ(qpc, 1'000'000'000);
  ASSERT_TRUE(ring.Consume(479));
  ASSERT_TRUE(ring.HeadQpc(&qpc));
  EXPECT_EQ(qpc, 1'000'000'000 + FrameNs(479));
  ASSERT_TRUE(ring.Consume(1));
  ASSERT_TRUE(ring.HeadQpc(&qpc));
  EXPECT_EQ(qpc, 1'030'000'000); // the next packet's own time, not an extrapolation
  ASSERT_TRUE(ring.Consume(500));
  ASSERT_TRUE(ring.HeadQpc(&qpc));
  EXPECT_EQ(qpc, 1'040'000'000 + FrameNs(20));
  // Empty: the head is the next frame to come, extrapolated from the last packet.
  ring.DiscardAll();
  EXPECT_EQ(ring.Buffered(), 0u);
  ASSERT_TRUE(ring.HeadQpc(&qpc));
  EXPECT_EQ(qpc, 1'040'000'000 + FrameNs(100));
}

TEST(CaptureRing, NoTimeBeforeTheFirstPacket) {
  CaptureRing ring;
  int64_t qpc = 0;
  EXPECT_FALSE(ring.HeadQpc(&qpc));
  EXPECT_EQ(ring.FramesBefore(123), 0u);
  float f[2];
  EXPECT_EQ(ring.Peek(f, 1), 0u);
  EXPECT_TRUE(ring.Consume(1)); // nothing to consume is not a race
}

TEST(CaptureRing, OverflowDropsTheOldest) {
  CaptureRing ring(1000);
  auto p = Counting(0, 600);
  ring.Write(p.data(), 600, 0, Ends(0, 600));
  auto q = Counting(600, 600);
  ring.Write(q.data(), 600, FrameNs(600), Ends(FrameNs(600), 600));
  EXPECT_EQ(ring.Buffered(), 1000u);
  EXPECT_EQ(ring.overflows(), 1u);
  EXPECT_EQ(ring.overflow_frames(), 200u);
  std::vector<float> got(2);
  ASSERT_EQ(ring.Peek(got.data(), 1), 1u);
  EXPECT_EQ(got[0], 200.0f); // frames 0..199 went
  int64_t qpc = 0;
  ASSERT_TRUE(ring.HeadQpc(&qpc));
  EXPECT_EQ(qpc, FrameNs(200));
}

TEST(CaptureRing, APacketLongerThanTheRingKeepsItsEnd) {
  CaptureRing ring(100);
  auto p = Counting(0, 250);
  ring.Write(p.data(), 250, 5'000'000, Ends(5'000'000, 250));
  EXPECT_EQ(ring.Buffered(), 100u);
  std::vector<float> got(2 * 100);
  ASSERT_EQ(ring.Peek(got.data(), 100), 100u);
  EXPECT_EQ(got[0], 150.0f);
  EXPECT_EQ(got[2 * 99], 249.0f);
  int64_t qpc = 0;
  ASSERT_TRUE(ring.HeadQpc(&qpc));
  EXPECT_EQ(qpc, 5'000'000 + FrameNs(150));
  EXPECT_EQ(ring.overflow_frames(), 150u);
}

TEST(CaptureRing, SilentPacketsAreZeros) {
  CaptureRing ring(100);
  auto p = Counting(1, 10);
  ring.Write(p.data(), 10, 0, Ends(0, 10));
  ring.Write(nullptr, 10, FrameNs(10), Ends(FrameNs(10), 10)); // AUDCLNT_BUFFERFLAGS_SILENT
  std::vector<float> got(2 * 20);
  ASSERT_EQ(ring.Peek(got.data(), 20), 20u);
  for (uint32_t i = 10; i < 20; ++i) {
    EXPECT_EQ(got[2 * i], 0.0f);
    EXPECT_EQ(got[2 * i + 1], 0.0f);
  }
}

TEST(CaptureRing, ConsumeAfterARacedOverflowSaysSo) {
  CaptureRing ring(1000);
  auto p = Counting(0, 900);
  ring.Write(p.data(), 900, 0, Ends(0, 900));
  std::vector<float> got(2 * 500);
  ASSERT_EQ(ring.Peek(got.data(), 500), 500u);
  auto q = Counting(900, 300); // the producer drops 200 of the peeked frames meanwhile
  ring.Write(q.data(), 300, FrameNs(900), Ends(FrameNs(900), 300));
  EXPECT_FALSE(ring.Consume(500));
  ASSERT_EQ(ring.Peek(got.data(), 1), 1u);
  EXPECT_EQ(got[0], 500.0f); // past both: the raced 200 and the 500
  EXPECT_TRUE(ring.Consume(1));
}

TEST(CaptureRing, FramesBeforeAQpcTime) {
  CaptureRing ring;
  auto a = Counting(0, 480), b = Counting(480, 480);
  ring.Write(a.data(), 480, 100'000'000, Ends(100'000'000, 480));
  ring.Write(b.data(), 480, 110'000'000, Ends(110'000'000, 480));
  EXPECT_EQ(ring.FramesBefore(50'000'000), 0u);  // before the head
  EXPECT_EQ(ring.FramesBefore(100'000'000), 0u); // the head itself
  EXPECT_EQ(ring.FramesBefore(100'000'001), 1u); // the first frame at or after it is frame 1
  EXPECT_EQ(ring.FramesBefore(105'000'000), 240u);
  EXPECT_EQ(ring.FramesBefore(110'000'000), 480u);
  EXPECT_EQ(ring.FramesBefore(115'000'000), 720u);
  EXPECT_EQ(ring.FramesBefore(500'000'000), 960u); // later than anything buffered
  ASSERT_TRUE(ring.Consume(300));
  EXPECT_EQ(ring.FramesBefore(105'000'000), 0u);
  EXPECT_EQ(ring.FramesBefore(110'000'000), 180u);
}

TEST(CaptureRing, ManyTinyPacketsOverflowTheSideRingNotTheTimes) {
  CaptureRing ring(24000);
  // 3000 packets of 2 frames: the side ring keeps the newest 1024 entries.
  for (uint64_t k = 0; k < 3000; ++k) {
    auto p = Counting(2 * k, 2);
    ring.Write(p.data(), 2, 1'000'000 + FrameNs(2 * k), Ends(1'000'000 + FrameNs(2 * k), 2));
  }
  int64_t qpc = 0;
  ASSERT_TRUE(ring.HeadQpc(&qpc)); // the head's entry is gone: extrapolated back from the oldest one left
  EXPECT_EQ(qpc, 1'000'000 + FrameNs(0) - 0);
  ASSERT_TRUE(ring.Consume(5000));
  ASSERT_TRUE(ring.HeadQpc(&qpc));
  EXPECT_NEAR(static_cast<double>(qpc), static_cast<double>(1'000'000 + FrameNs(5000)), 1.0);
}

// How late the packets reach the capture thread and how long they are, averaged over ~10 s of audio (the mixer's fill
// floor, mix_engine.h).
TEST(CaptureRing, PacketTimingAveragesLagAndLength) {
  constexpr int64_t kMs = kNsPerMs;
  CaptureRing ring;
  EXPECT_EQ(ring.arrival_lag_ns(), 0);
  EXPECT_EQ(ring.packet_ns(), 0);
  int64_t qpc = 0;
  auto write = [&](uint32_t n, int64_t lag) {
    ring.Write(nullptr, n, qpc, Ends(qpc, n) + lag);
    qpc += FrameNs(n);
  };
  // While far less than 10 s has come, about a plain mean: one packet 200 ms late (counted as 50 ms), then nine
  // without lag (the first one's weight has decayed by 1 % meanwhile).
  write(480, 200 * kMs);
  EXPECT_EQ(ring.arrival_lag_ns(), 50 * kMs);
  EXPECT_EQ(ring.packet_ns(), 10 * kMs);
  for (int i = 0; i < 9; ++i) {
    write(480, 0);
  }
  EXPECT_NEAR(static_cast<double>(ring.arrival_lag_ns()), 5.0 * kMs, 0.05 * kMs);
  // 100 s at 7 ms: the start is forgotten.
  for (int i = 0; i < 10'000; ++i) {
    write(480, 7 * kMs);
  }
  EXPECT_NEAR(static_cast<double>(ring.arrival_lag_ns()), 7.0 * kMs, 1000.0);
  // 10 s at 17 ms: about 1 - 1/e of the way.
  for (int i = 0; i < 1000; ++i) {
    write(480, 17 * kMs);
  }
  EXPECT_NEAR(static_cast<double>(ring.arrival_lag_ns()), (7.0 + 10.0 * (1 - std::exp(-1.0))) * kMs, 0.05 * kMs);
  // A packet that seems to arrive before its end (a bad position) counts as no lag.
  for (int i = 0; i < 10'000; ++i) {
    write(480, -5 * kMs);
  }
  EXPECT_LT(ring.arrival_lag_ns(), 1000);
  // Lengths are weighted by length: 10 and 20 ms packets in turn average (10^2 + 20^2) / (10 + 20) ms.
  for (int i = 0; i < 5000; ++i) {
    write(480, 0);
    write(960, 0);
  }
  EXPECT_NEAR(static_cast<double>(ring.packet_ns()), 500.0 / 30.0 * kMs, 0.2 * kMs);
}

// One producer thread, one consumer thread (clean under host-tsan): frames arrive in order, gaps only where an
// overflow was counted, and the head's time always matches its frame.
TEST(CaptureRing, ProducerAndConsumerThreads) {
  CaptureRing ring(4800);
  constexpr uint64_t kFrames = 1'000'000;
  std::atomic<bool> done{false};
  std::thread producer([&] {
    test::Rng rng(7);
    std::vector<float> buf(2 * 1000);
    uint64_t next = 0;
    while (next < kFrames) {
      const auto n = static_cast<uint32_t>(rng.UniformInt(1, 1000));
      for (uint32_t i = 0; i < n; ++i) {
        buf[2 * i] = static_cast<float>(next + i);
        buf[2 * i + 1] = 1.0f;
      }
      ring.Write(buf.data(), n, FrameNs(next), Ends(FrameNs(next), n));
      next += n;
      if (rng.UniformInt(0, 3) == 0) {
        std::this_thread::yield();
      }
    }
    done.store(true, std::memory_order_release);
  });
  std::vector<float> got(2 * 700);
  double last = -1;
  uint64_t gaps = 0, raced = 0, read = 0, bad = 0;
  test::Rng rng(9);
  while (!done.load(std::memory_order_acquire) || ring.Buffered() > 0) {
    const auto want = static_cast<uint32_t>(rng.UniformInt(1, 700));
    const uint32_t k = ring.Peek(got.data(), want);
    if (k == 0) {
      std::this_thread::yield();
      continue;
    }
    int64_t qpc = 0;
    const bool have = ring.HeadQpc(&qpc);
    if (!ring.Consume(k)) {
      ++raced; // an overflow raced with the read: the frames may be stale, skip them
      continue;
    }
    for (uint32_t i = 0; i < k; ++i) {
      const double v = got[2 * i];
      bad += got[2 * i + 1] != 1.0f || v <= last;
      if (v != last + 1) {
        ++gaps;
      }
      last = v;
    }
    read += k;
    // Clean: the head didn't move between the peek and the consume, so its time is the first peeked frame's (to the
    // nanosecond the two roundings of a packet's time plus an offset allow).
    const int64_t want_ns = FrameNs(static_cast<uint64_t>(got[0]));
    bad += !have || qpc < want_ns - 1 || qpc > want_ns;
  }
  producer.join();
  EXPECT_EQ(bad, 0u);
  EXPECT_GT(read, 0u);
  if (ring.overflows() == 0) {
    EXPECT_EQ(raced, 0u);
    EXPECT_EQ(gaps, 0u); // without an overflow nothing is lost
  } else {
    // Every lost frame was dropped by the producer or skipped after a raced read.
    EXPECT_GE(ring.overflow_frames() + 700 * raced, ring.written_frames() - read);
  }
  EXPECT_EQ(last, static_cast<double>(ring.written_frames() - 1)); // the newest frame always arrives
}

} // namespace
} // namespace im::audiomix
