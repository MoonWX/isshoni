// The drift simulation of mix_drift_test and mix_drift_long_test (docs/m2/05-windows-audio.md §9.3, §17, §19.1): one
// stream playing a tone through a MixEngine on a simulated clock, its device clock drifting against the QPC, its
// packets arriving with jitter. Simulated time only (no wall clock): fast and the same on every runner.
//
// The long runs use the model resampler (speexdsp's consumption to the frame, mix_resampler_test, without its 80-tap
// filter, which costs about 0.3 ms per chunk in a debug build); shorter runs check the same with speexdsp.
#pragma once

#include <gtest/gtest.h>

#include <algorithm>
#include <cmath>
#include <cstdint>
#include <cstdio>
#include <memory>

#include "im/audiomix/channel.h"
#include "im/audiomix/mix_engine.h"
#include "mix_test_support.h"

#if defined(__SANITIZE_ADDRESS__) || defined(__SANITIZE_THREAD__)
#define IM_SANITIZED 1
#elif defined(__has_feature)
#if __has_feature(address_sanitizer) || __has_feature(thread_sanitizer)
#define IM_SANITIZED 1
#endif
#endif

namespace im::audiomix::test {

inline constexpr int64_t kMs = kNsPerMs;
inline constexpr int64_t kSecond = kNsPerSecond;

// Under a sanitizer the runs are shorter (the sanitizer build checks memory and threads, host-debug the numbers), so
// each test file stays well inside ctest's 300 s on CI's runners.
#if defined(IM_SANITIZED)
inline constexpr int64_t kTwoHours = 10 * 60 * kSecond;
inline constexpr int64_t kTwentyMinutes = 5 * 60 * kSecond;
inline constexpr int64_t kTenMinutes = 3 * 60 * kSecond;
inline constexpr int64_t kSpeexRun = 20 * kSecond;
#else
inline constexpr int64_t kTwoHours = 2 * 3600 * kSecond;
inline constexpr int64_t kTwentyMinutes = 20 * 60 * kSecond;
inline constexpr int64_t kTenMinutes = 10 * 60 * kSecond;
inline constexpr int64_t kSpeexRun = 100 * kSecond;
#endif

// A sink that checks the grid as the chunks come and keeps nothing.
class GridSink final : public ChunkSink {
public:
  void OnChunk(const float *, int64_t t) noexcept override {
    if (have_ && t - last_ != kChunkNs) {
      ++off_grid;
    }
    have_ = true;
    last_ = t;
    ++chunks;
  }
  void OnGap(int64_t) noexcept override { ++gaps; }
  uint64_t chunks = 0, gaps = 0, off_grid = 0;

private:
  bool have_ = false;
  int64_t last_ = 0;
};

struct Stats {
  double n = 0, sum = 0, sumsq = 0, min = 1e300, max = -1e300;
  void Add(double v) {
    n += 1;
    sum += v;
    sumsq += v * v;
    min = std::min(min, v);
    max = std::max(max, v);
  }
  double mean() const { return sum / n; }
  double sd() const { return std::sqrt(std::max(0.0, sumsq / n - mean() * mean())); }
};

struct DriftRun {
  double drift_ppm = 0;
  int64_t duration = kTwentyMinutes;
  bool speex = false;
  uint64_t seed = 1;
  int64_t latency_ns = 3 * kMs; // a packet reaches the capture thread this long after its mix, +-3 ms of jitter
  uint32_t packet_frames = kChunkFrames;
  int64_t settle_ns = 60 * kSecond; // the statistics start here
};

struct DriftResult {
  Stats fill_ms; // the ring fill at each tick, before the pop, from settle_ns on
  Stats ppm;     // the ratio at each tick from settle_ns on
  uint64_t underruns = 0, trims = 0, zero_ppm = 0, gaps = 0, off_grid = 0;
  double max_abs_ppm = 0;
  double target_ms = 0; // the head age the controller held at the end
};

inline DriftResult RunDrift(const DriftRun &run) {
  GridSink sink;
  MixEngine engine(&sink);
  CaptureRing ring;
  Tone tone(1000, 0.5f); // sound throughout: no silent bypass
  ProducerConfig pc;
  pc.drift_ppm = run.drift_ppm;
  pc.packet_frames = run.packet_frames;
  pc.start_ns = -1 * kSecond; // playing before the first chunk
  pc.latency_ns = run.latency_ns;
  pc.jitter_ns = 3 * kMs;
  pc.qpc_noise_ns = 200'000; // and +-0.2 ms on the reported QPC positions
  pc.seed = run.seed;
  Producer producer(&ring, &tone, pc);
  ChannelOptions o;
  o.ring = &ring;
  if (!run.speex) {
    o.resampler = NewModelResampler();
  }
  std::unique_ptr<Channel> ch;
  EXPECT_EQ(Channel::Create(std::move(o), &ch), IM_OK);
  EXPECT_TRUE(engine.Add(ch.get()));

  DriftResult res;
  engine.Begin(0);
  int64_t now = 0;
  while (now < run.duration) {
    now = std::min(engine.next_due_ns(), producer.next_arrival());
    producer.RunUntil(now);
    if (engine.next_due_ns() <= now) {
      if (now >= run.settle_ns) {
        res.fill_ms.Add(static_cast<double>(ring.Buffered()) / kFramesPerMs);
        res.ppm.Add(ch->ppm());
      }
      res.zero_ppm += ch->ppm() == 0;
      res.max_abs_ppm = std::max(res.max_abs_ppm, std::fabs(static_cast<double>(ch->ppm())));
      engine.Pump(now);
    }
  }
  res.underruns = ch->underruns();
  res.trims = ch->trims();
  res.gaps = sink.gaps;
  res.off_grid = sink.off_grid;
  res.target_ms = static_cast<double>(ch->target_age_ns()) / kMs;
  return res;
}

inline void Report(const char *what, const DriftRun &run, const DriftResult &r) {
  std::printf("%s drift %+5.0f ppm, lag %2lld ms, packets %2u ms: fill %.1f..%.1f ms (mean %.1f), head age %.0f ms, "
              "ratio %.1f +- %.1f ppm (|max| %.0f), underruns %llu, trims %llu\n",
              what, run.drift_ppm, static_cast<long long>(run.latency_ns / kMs), run.packet_frames / kFramesPerMs,
              r.fill_ms.min, r.fill_ms.max, r.fill_ms.mean(), r.target_ms, r.ppm.mean(), r.ppm.sd(), r.max_abs_ppm,
              static_cast<unsigned long long>(r.underruns), static_cast<unsigned long long>(r.trims));
}

// §17: from 60 s on (settle_ns) the fill stays at 40 +- 15 ms, no underrun or trim in the whole run, the ratio's
// standard deviation is at most 30 ppm and it is never 1:1 (§9.3); and the chunks stay on one grid (§10.4). Without
// settled, the statistics start before the controller has converged, and the ratio's spread isn't checked.
inline void ExpectMeetsSection17(const DriftRun &run, const DriftResult &r, bool settled = true) {
  const double drift = run.drift_ppm;
  ASSERT_GT(r.fill_ms.n, 0.0) << drift; // the run is longer than settle_ns
  EXPECT_GE(r.fill_ms.min, 25.0) << drift;
  EXPECT_LE(r.fill_ms.max, 55.0) << drift;
  EXPECT_EQ(r.underruns, 0u) << drift;
  EXPECT_EQ(r.trims, 0u) << drift;
  if (settled) {
    EXPECT_LE(r.ppm.sd(), 30.0) << drift;
  }
  EXPECT_EQ(r.zero_ppm, 0u) << drift;
  EXPECT_EQ(r.gaps, 1u) << drift; // the first chunk only
  EXPECT_EQ(r.off_grid, 0u) << drift;
}

} // namespace im::audiomix::test
