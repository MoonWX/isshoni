// The test tone (docs/m2/04-windows-engine.md §10.3): M1's fake signal locked to epoch_ns, the 10 ms pause and mute
// fade, and the source's exact 10 ms grid, its catch-up and its skip after a stall.
#include <gtest/gtest.h>

#include <cmath>
#include <numbers>
#include <vector>

#include "im/audio/test_tone.h"

namespace {

using im::audio::kChunkFrames;
using im::audio::kChunkNs;
using im::audio::kChunkSamples;
using im::audio::TestTone;
using im::audio::TestToneSource;

constexpr int64_t kSecond = 1'000'000'000;

// gentone.go's sample() before its 16-bit rounding.
double Gentone(int t) {
  double v = 0.1 * std::sin(2 * std::numbers::pi * 440 * t / 48000.0);
  if (t < 4800) {
    double env = 1.0;
    if (t < 48) {
      env = 0.5 - 0.5 * std::cos(std::numbers::pi * t / 48.0);
    } else if (t >= 4800 - 48) {
      env = 0.5 - 0.5 * std::cos(std::numbers::pi * (4800 - t) / 48.0);
    }
    v += 0.32 * env * std::sin(2 * std::numbers::pi * 1000 * t / 48000.0);
  }
  return v;
}

TEST(TestTone, IsM1sFakeSignal) {
  for (int t = 0; t < 48000; t++) {
    ASSERT_NEAR(TestTone::PeriodSample(static_cast<uint32_t>(t)), Gentone(t), 1e-6) << t;
  }
  EXPECT_EQ(TestTone::PeriodSample(0), 0.0f);
  // Periodic: whole cycles of both tones per second, so the period's end runs into its start.
  EXPECT_NEAR(TestTone::PeriodSample(47999) - TestTone::PeriodSample(0),
              0.1 * std::sin(-2 * std::numbers::pi * 440 / 48000.0), 1e-6);
}

TEST(TestTone, PhaseIsLockedToEpoch) {
  constexpr int64_t kEpoch = 5 * kSecond + 123;
  const TestTone tone(kEpoch);
  EXPECT_EQ(tone.PeriodIndex(kEpoch), 0u);
  EXPECT_EQ(tone.PeriodIndex(kEpoch + 6'500'000), 312u); // the lookahead
  EXPECT_EQ(tone.PeriodIndex(kEpoch + 10'000'000), 480u);
  EXPECT_EQ(tone.PeriodIndex(kEpoch + 7 * kSecond), 0u); // every beep starts at epoch_ns + k * 1 s
  EXPECT_EQ(tone.PeriodIndex(kEpoch + kSecond - 1), 0u); // rounded to the nearest sample
  EXPECT_EQ(tone.PeriodIndex(kEpoch - 10'000'000), 47520u);
  EXPECT_EQ(tone.PeriodIndex(kEpoch + 20'834), 1u);                               // 20 833.3 ns per sample
  EXPECT_EQ(tone.PeriodIndex(kEpoch + 1'000'000LL * kSecond + 10'000'000), 480u); // far from epoch_ns
}

TEST(TestTone, ChunksAreContinuous) {
  TestTone tone(0);
  std::vector<float> all;
  float buf[kChunkSamples];
  for (int k = 0; k < 250; k++) { // 2.5 s from epoch_ns + 6.5 ms
    tone.Fill(buf, 6'500'000 + k * kChunkNs);
    all.insert(all.end(), buf, buf + kChunkSamples);
  }
  for (std::size_t n = 0; n < all.size() / 2; n++) {
    ASSERT_EQ(all[2 * n], all[2 * n + 1]) << "both channels equal";
    ASSERT_EQ(all[2 * n], TestTone::PeriodSample(static_cast<uint32_t>((n + 312) % 48000))) << n;
  }
}

TEST(TestTone, PauseAndMuteFadeIn10ms) {
  TestTone tone(0);
  float buf[kChunkSamples];
  int64_t t = 0;
  auto next = [&] {
    tone.Fill(buf, t);
    t += kChunkNs;
  };
  next();
  tone.SetPaused(true);
  next(); // the fade: a linear ramp to exactly zero within this chunk
  for (uint32_t i = 0; i < kChunkFrames; i++) {
    const float want = TestTone::PeriodSample(480 + i) * static_cast<float>(kChunkFrames - 1 - i) / 480.0f;
    ASSERT_FLOAT_EQ(buf[2 * i], want) << i;
  }
  EXPECT_EQ(buf[2 * (kChunkFrames - 1)], 0.0f);
  for (int k = 0; k < 10; k++) {
    next();
    for (const float v : buf) {
      ASSERT_EQ(v, 0.0f) << "digital zero while paused";
    }
  }
  // The user's mute holds the silence through an un-pause (effective mute = mute OR pause).
  tone.SetMuted(true);
  tone.SetPaused(false);
  next();
  for (const float v : buf) {
    ASSERT_EQ(v, 0.0f);
  }
  tone.SetMuted(false);
  const int64_t back = t;
  next(); // fades back in over 10 ms
  const uint32_t n0 = tone.PeriodIndex(back);
  for (uint32_t i = 0; i < kChunkFrames; i++) {
    const float want = TestTone::PeriodSample((n0 + i) % 48000) * static_cast<float>(i + 1) / 480.0f;
    ASSERT_FLOAT_EQ(buf[2 * i], want) << i;
  }
  next();
  EXPECT_EQ(buf[0], TestTone::PeriodSample(tone.PeriodIndex(t - kChunkNs)));
}

// Records what a PcmSink sees.
struct Sink final : im::audio::PcmSink {
  struct Call {
    bool gap;
    int64_t ns;
  };
  void OnChunk(const float *frames, int64_t ns) noexcept override {
    calls.push_back({false, ns});
    first.push_back(frames[0]);
  }
  void OnGap(int64_t ns) noexcept override { calls.push_back({true, ns}); }
  std::vector<Call> calls;
  std::vector<float> first;
};

TEST(TestToneSource, PacesChunksOnAnExact10msGrid) {
  constexpr int64_t kEpoch = kSecond;
  Sink sink;
  TestToneSource src(kEpoch, kEpoch + 6'500'000, &sink);
  // Nothing is due before the first chunk's last frame; the first Pump starts the grid.
  EXPECT_EQ(src.Pump(kEpoch), kEpoch + 16'500'000);
  ASSERT_EQ(sink.calls.size(), 1u);
  EXPECT_TRUE(sink.calls[0].gap);
  EXPECT_EQ(sink.calls[0].ns, kEpoch + 6'500'000);
  // A timer that fires late or early: every due chunk comes out once, in order, exactly 10 ms apart.
  int64_t deadline = src.Pump(kEpoch + 16'499'999);
  EXPECT_EQ(sink.calls.size(), 1u);
  for (const int64_t jitter : {0, 3'000'000, -1, 1'500'000, 9'999'999, 0, 0, 25'000'000, 0}) {
    deadline = src.Pump(deadline + jitter);
  }
  ASSERT_GT(sink.calls.size(), 10u);
  for (std::size_t i = 1; i < sink.calls.size(); i++) {
    EXPECT_FALSE(sink.calls[i].gap);
    EXPECT_EQ(sink.calls[i].ns, kEpoch + 6'500'000 + static_cast<int64_t>(i - 1) * kChunkNs);
  }
  EXPECT_EQ(src.chunks(), sink.calls.size() - 1);
  EXPECT_EQ(deadline, src.next_chunk_ns() + kChunkNs);
  EXPECT_EQ(src.skips(), 0u);
}

TEST(TestToneSource, CatchesUpShortStallsAndSkipsLongOnes) {
  constexpr int64_t kEpoch = 0;
  const int64_t first = kEpoch + 6'500'000;
  Sink sink;
  TestToneSource src(kEpoch, first, &sink);
  (void)src.Pump(first + kChunkNs); // chunk 0
  // A 150 ms stall: the 15 due chunks come out at once, the grid unbroken.
  (void)src.Pump(first + 16 * kChunkNs);
  ASSERT_EQ(src.chunks(), 16u);
  EXPECT_EQ(src.skips(), 0u);
  // A 10 s stall: no burst of 1000 chunks, but a new grid at the newest due chunk, a whole number of 20 ms frames on.
  const std::size_t before = sink.calls.size();
  (void)src.Pump(first + 1016 * kChunkNs + 3'000'000);
  EXPECT_EQ(src.skips(), 1u);
  ASSERT_GT(sink.calls.size(), before);
  EXPECT_TRUE(sink.calls[before].gap);
  const int64_t restart = sink.calls[before].ns;
  EXPECT_EQ((restart - first) % (2 * kChunkNs), 0) << "the 20 ms frame phase is kept";
  EXPECT_LE(sink.calls.size() - before - 1, 2u);
  EXPECT_FALSE(sink.calls.back().gap);
  EXPECT_EQ(sink.calls.back().ns, first + 1015 * kChunkNs);
  EXPECT_EQ(sink.calls[before + 1].ns, restart);
  // The tone after the skip is still the epoch-locked signal.
  TestTone ref(kEpoch);
  EXPECT_EQ(sink.first.back(), TestTone::PeriodSample(ref.PeriodIndex(sink.calls.back().ns)));
}

} // namespace
