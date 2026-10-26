// session_frames_test (docs/m2/04-windows-engine.md §10.3, §8.2, §18.2; README WF2): the session's two portable frame
// pieces, without a GPU or the test hooks, so every test build runs it (win-x64-ci, win-x64-asan).
//
// - PatternGrid (session/pattern_grid.h), the supervisor's test-pattern clock: frame k at epoch_ns + k / hz s, exact
//   at whole seconds (the tone's beeps); the test hooks' late frames stay below half an interval and every third frame
//   stays on time; NewestDue picks the newest frame due, never one that isn't due yet and never one before `next`.
// - FrameTimes (session/frame_times.h), the frames of a session without a device (IM_ENC_FAKE's): Take reports fresh,
//   skipped and the newest capture time; across two threads every frame published is either taken or skipped, and
//   the times taken only grow.
#include <gtest/gtest.h>

#include <algorithm>
#include <atomic>
#include <chrono>
#include <cstdint>
#include <cstdio>
#include <random>
#include <string>
#include <thread>

#include "session/frame_times.h"
#include "session/pattern_grid.h"

namespace {

using im::session::FrameTimes;
using im::session::PatternGrid;
using im::session::TakenFrame;

constexpr int64_t kSecond = 1'000'000'000;
constexpr int64_t kEpoch = 123'456'789'012; // an engine clock reading (QPC ns) some time after boot
constexpr uint32_t kRates[] = {1, 15, 30, 60, 144, 240};

// ---------------------------------------------------------------------------------------------- PatternGrid

TEST(PatternGrid, FrameTimesAreExactAtWholeSecondsAndRoundedDownBetween) {
  for (const uint32_t hz : kRates) {
    SCOPED_TRACE("hz " + std::to_string(hz));
    const PatternGrid g{kEpoch, hz, 0};
    // Every frame of the first minute: floor(k × 1e9 / hz) after the epoch (k × 1e9 fits in 64 bits here).
    for (uint64_t k = 0; k <= 60ull * hz; ++k) {
      ASSERT_EQ(g.FrameNs(k), kEpoch + static_cast<int64_t>(k * 1'000'000'000ull / hz)) << "frame " << k;
    }
    // Whole seconds stay exact far out (a share of a few days), and the formula is WA23's in between.
    for (const uint64_t sec : {1ull, 59ull, 3'600ull, 86'399ull, 400'000ull}) {
      EXPECT_EQ(g.FrameNs(sec * hz), kEpoch + static_cast<int64_t>(sec) * kSecond) << "second " << sec;
      for (const uint64_t k : {sec * hz + 1, sec * hz + hz / 2, sec * hz + hz - 1}) {
        const uint64_t want = k / hz * 1'000'000'000ull + (k % hz) * 1'000'000'000ull / hz;
        EXPECT_EQ(g.FrameNs(k), kEpoch + static_cast<int64_t>(want)) << "frame " << k;
      }
    }
  }
  EXPECT_EQ((PatternGrid{kEpoch, 0, 0}).FrameNs(3), kEpoch + 3 * kSecond) << "hz 0 counts as 1";
}

TEST(PatternGrid, LateFramesStayBelowHalfAnIntervalAndEveryThirdIsOnTime) {
  for (const uint32_t hz : kRates) {
    SCOPED_TRACE("hz " + std::to_string(hz));
    const int64_t half = kSecond / 2 / hz;
    for (const int64_t late : {int64_t{-5}, int64_t{0}, int64_t{1'000}, half - 1, half, int64_t{10} * kSecond}) {
      SCOPED_TRACE("late_ns " + std::to_string(late));
      const PatternGrid g{kEpoch, hz, late};
      for (uint64_t k = 0; k <= 3ull * hz + 3; ++k) {
        const int64_t want = k % 3 == 0 || late <= 0 ? g.FrameNs(k) : g.FrameNs(k) + std::min(late, half - 1);
        ASSERT_EQ(g.DueNs(k), want) << "frame " << k;
        ASSERT_LT(g.DueNs(k), g.FrameNs(k + 1)) << "frame " << k << " is due before the next one is captured";
      }
    }
  }
}

TEST(PatternGrid, NewestDueIsTheNewestFrameThatIsDueAndNeverBeforeNext) {
  for (const uint32_t hz : kRates) {
    for (const int64_t late : {int64_t{0}, int64_t{3'000'000}}) {
      SCOPED_TRACE("hz " + std::to_string(hz) + ", late_ns " + std::to_string(late));
      const PatternGrid g{kEpoch, hz, late};
      // Before or at the epoch: nothing newer than next.
      EXPECT_EQ(g.NewestDue(kEpoch - kSecond, 0), 0u);
      EXPECT_EQ(g.NewestDue(kEpoch, 0), 0u);
      EXPECT_EQ(g.NewestDue(kEpoch, 7), 7u);
      // Every frame's capture and due instants, and the nanoseconds around them.
      for (uint64_t k = 0; k <= 2ull * hz + 2; ++k) {
        for (const int64_t instant : {g.FrameNs(k), g.DueNs(k)}) {
          for (int64_t at = std::max(instant - 1, kEpoch + 1); at <= instant + 1; ++at) {
            // DueNs grows with k (a late frame is due before the next one is captured), so r is the newest due.
            const uint64_t r = g.NewestDue(at, 0);
            ASSERT_LE(g.DueNs(r), at) << "at +" << at - kEpoch << " ns: frame " << r << " isn't due yet";
            ASSERT_GT(g.DueNs(r + 1), at) << "at +" << at - kEpoch << " ns: frame " << r + 1 << " is due too";
          }
        }
      }
      // Random instants over a minute, with next anywhere from far behind to ahead of them.
      std::mt19937_64 rng(hz * 1'000 + static_cast<uint64_t>(late));
      std::uniform_int_distribution<int64_t> when(1, 60 * kSecond);
      for (int i = 0; i < 20'000; ++i) {
        const int64_t at = kEpoch + when(rng);
        const uint64_t newest = g.NewestDue(at, 0);
        ASSERT_LE(g.DueNs(newest), at);
        for (uint64_t k = newest + 1; k <= newest + 3; ++k) {
          ASSERT_GT(g.DueNs(k), at) << "frame " << k << " is due at +" << at - kEpoch << " ns";
        }
        const uint64_t next = std::uniform_int_distribution<uint64_t>(0, newest + 2)(rng);
        EXPECT_EQ(g.NewestDue(at, next), std::max(newest, next)) << "next " << next;
      }
    }
  }
}

// ---------------------------------------------------------------------------------------------- FrameTimes

TEST(FrameTimes, TakeReportsFreshSkippedAndTheNewestTime) {
  FrameTimes f;
  TakenFrame t = f.Take();
  EXPECT_FALSE(t.fresh) << "nothing published";
  EXPECT_EQ(t.skipped, 0u);
  EXPECT_EQ(t.capture_ts_ns, 0) << "no frame taken yet";

  f.Publish(1'000);
  t = f.Take();
  EXPECT_TRUE(t.fresh);
  EXPECT_EQ(t.skipped, 0u);
  EXPECT_EQ(t.capture_ts_ns, 1'000);
  t = f.Take();
  EXPECT_FALSE(t.fresh) << "nothing newer";
  EXPECT_EQ(t.skipped, 0u);
  EXPECT_EQ(t.capture_ts_ns, 1'000) << "the reader keeps its frame (refreshes, keyframes, pause)";

  for (int64_t i = 2; i <= 6; ++i) {
    f.Publish(i * 1'000);
  }
  t = f.Take();
  EXPECT_TRUE(t.fresh);
  EXPECT_EQ(t.skipped, 4u) << "5 published, the newest taken";
  EXPECT_EQ(t.capture_ts_ns, 6'000);
  t = f.Take();
  EXPECT_FALSE(t.fresh);
  EXPECT_EQ(t.capture_ts_ns, 6'000);
}

TEST(FrameTimes, TwoThreadsTakeOrSkipEveryFrameNewestFirst) {
  // Frame n (1..kFrames) is published with capture time kEpoch + n × kStep, so a time names its frame.
  constexpr uint64_t kFrames = 200'000;
  constexpr int64_t kStep = 16'666'667;
  using Clock = std::chrono::steady_clock;
  const auto deadline = Clock::now() + std::chrono::seconds(10); // a loop that doesn't finish fails, never hangs
  FrameTimes f;
  std::atomic<bool> go{false};
  std::atomic<bool> stop{false};
  std::thread writer([&] {
    while (!go.load(std::memory_order_acquire) && !stop.load(std::memory_order_relaxed)) {
      std::this_thread::yield();
    }
    for (uint64_t n = 1; n <= kFrames && !stop.load(std::memory_order_relaxed); ++n) {
      f.Publish(kEpoch + static_cast<int64_t>(n) * kStep);
      if (n % 256 == 0) {
        std::this_thread::yield(); // let the reader in between, also on a runner with few cores
      }
    }
  });
  go.store(true, std::memory_order_release);

  uint64_t takes = 0, skipped = 0, last = 0, wrong = 0;
  while (last < kFrames && Clock::now() < deadline) {
    const TakenFrame t = f.Take();
    if (!t.fresh) {
      std::this_thread::yield();
      continue;
    }
    const int64_t d = t.capture_ts_ns - kEpoch;
    const uint64_t n = d > 0 && d % kStep == 0 ? static_cast<uint64_t>(d / kStep) : 0;
    ++takes;
    skipped += t.skipped;
    // A taken frame is newer than the last, and every frame before it was taken or skipped exactly once.
    if (n <= last || n != takes + skipped) {
      if (wrong++ == 0) {
        ADD_FAILURE() << "take " << takes << ": frame " << n << " (time " << t.capture_ts_ns << ") after frame " << last
                      << ", " << takes + skipped << " taken or skipped";
      }
    }
    last = n > last ? n : last;
  }
  stop.store(true, std::memory_order_relaxed);
  writer.join();

  ASSERT_EQ(last, kFrames) << "the reader saw the last frame within 10 s";
  EXPECT_EQ(wrong, 0u);
  EXPECT_EQ(takes + skipped, kFrames) << "takes + skipped == published";
  std::printf("[ info     ] %llu frames taken, %llu skipped\n", static_cast<unsigned long long>(takes),
              static_cast<unsigned long long>(skipped));
  const TakenFrame again = f.Take();
  EXPECT_FALSE(again.fresh);
  EXPECT_EQ(again.capture_ts_ns, kEpoch + static_cast<int64_t>(kFrames) * kStep);
}

} // namespace
