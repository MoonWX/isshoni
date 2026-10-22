// Adaptive resampling in simulation (docs/m2/05-windows-audio.md §9.3, §17, §19.1): device clocks drifting -500 ..
// +500 ppm against the QPC, 10 ms capture packets arriving with +-3 ms of jitter, 2 simulated hours. Pass: from 60 s
// on the ring fill stays at 40 +- 15 ms, no underrun or trim in the whole run, and the resampling ratio's standard
// deviation is at most 30 ppm. Simulated time only (no wall clock): fast and the same on every runner.
//
// The two-hour runs (at -500, 0 and +500 ppm; the drifts in between run 20 simulated minutes) use the model resampler
// (speexdsp's consumption to the frame, mix_resampler_test, without its 80-tap filter, which costs about 0.3 ms per
// chunk in a debug build); a shorter run checks the same with speexdsp.
#include <gtest/gtest.h>

#include <cmath>
#include <cstdio>

#include "im/audiomix/controller.h"
#include "im/audiomix/mix_engine.h"
#include "mix_test_support.h"

namespace im::audiomix {
namespace {

constexpr int64_t kMs = kNsPerMs;
constexpr int64_t kSecond = kNsPerSecond;

#if defined(__SANITIZE_ADDRESS__) || defined(__SANITIZE_THREAD__)
#define IM_SANITIZED 1
#elif defined(__has_feature)
#if __has_feature(address_sanitizer) || __has_feature(thread_sanitizer)
#define IM_SANITIZED 1
#endif
#endif

// Under a sanitizer the runs are shorter (the sanitizer build checks memory and threads, host-debug the numbers).
#if defined(IM_SANITIZED)
constexpr int64_t kTwoHours = 10 * 60 * kSecond;
constexpr int64_t kTwentyMinutes = 5 * 60 * kSecond;
#else
constexpr int64_t kTwoHours = 2 * 3600 * kSecond;
constexpr int64_t kTwentyMinutes = 20 * 60 * kSecond;
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

struct DriftResult {
  Stats fill_ms; // the ring fill at each tick, before the pop, from 60 s on
  Stats ppm;     // the ratio at each tick from 60 s on
  uint64_t underruns = 0, trims = 0, zero_ppm = 0, gaps = 0, off_grid = 0;
  double max_abs_ppm = 0;
};

DriftResult RunDrift(double drift_ppm, int64_t duration, bool speex, uint64_t seed) {
  GridSink sink;
  MixEngine engine(&sink);
  CaptureRing ring;
  test::Tone tone(1000, 0.5f); // sound throughout: no silent bypass
  test::ProducerConfig pc;
  pc.drift_ppm = drift_ppm;
  pc.start_ns = -1 * kSecond; // playing before the first chunk
  pc.latency_ns = 3 * kMs;
  pc.jitter_ns = 3 * kMs;
  pc.qpc_noise_ns = 200'000; // and +-0.2 ms on the reported QPC positions
  pc.seed = seed;
  test::Producer producer(&ring, &tone, pc);
  ChannelOptions o;
  o.ring = &ring;
  if (!speex) {
    o.resampler = NewModelResampler();
  }
  std::unique_ptr<Channel> ch;
  EXPECT_EQ(Channel::Create(std::move(o), &ch), IM_OK);
  EXPECT_TRUE(engine.Add(ch.get()));

  DriftResult res;
  engine.Begin(0);
  int64_t now = 0;
  while (now < duration) {
    now = std::min(engine.next_due_ns(), producer.next_arrival());
    producer.RunUntil(now);
    if (engine.next_due_ns() <= now) {
      if (now >= 60 * kSecond) {
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
  return res;
}

void Report(const char *what, double drift, const DriftResult &r) {
  std::printf("%s drift %+5.0f ppm: fill %.1f..%.1f ms (mean %.1f), ratio %.1f +- %.1f ppm (|max| %.0f), "
              "underruns %llu, trims %llu\n",
              what, drift, r.fill_ms.min, r.fill_ms.max, r.fill_ms.mean(), r.ppm.mean(), r.ppm.sd(), r.max_abs_ppm,
              static_cast<unsigned long long>(r.underruns), static_cast<unsigned long long>(r.trims));
}

void ExpectMeetsSection17(double drift, const DriftResult &r) {
  EXPECT_GE(r.fill_ms.min, 25.0) << drift;
  EXPECT_LE(r.fill_ms.max, 55.0) << drift;
  EXPECT_EQ(r.underruns, 0u) << drift;
  EXPECT_EQ(r.trims, 0u) << drift;
  EXPECT_LE(r.ppm.sd(), 30.0) << drift;
  EXPECT_EQ(r.zero_ppm, 0u) << drift;
  EXPECT_EQ(r.gaps, 1u) << drift; // the first chunk only
  EXPECT_EQ(r.off_grid, 0u) << drift;
}

class TwoHourDrift : public ::testing::TestWithParam<double> {};

TEST_P(TwoHourDrift, MeetsSection17) {
  const double drift = GetParam();
  const DriftResult r = RunDrift(drift, kTwoHours, /*speex=*/false, 1000 + static_cast<uint64_t>(drift + 1000));
  Report("model ", drift, r);
  ExpectMeetsSection17(drift, r);
  // The ratio tracks the drift (a fast device needs a ratio above 1 to drain).
  EXPECT_NEAR(r.ppm.mean(), drift, 8.0);
}

INSTANTIATE_TEST_SUITE_P(Drifts, TwoHourDrift, ::testing::Values(-500.0, 0.0, 500.0));

class TwentyMinuteDrift : public ::testing::TestWithParam<double> {};

TEST_P(TwentyMinuteDrift, MeetsSection17) {
  const double drift = GetParam();
  const DriftResult r = RunDrift(drift, kTwentyMinutes, /*speex=*/false, 2000 + static_cast<uint64_t>(drift + 1000));
  Report("model ", drift, r);
  ExpectMeetsSection17(drift, r);
  EXPECT_NEAR(r.ppm.mean(), drift, 8.0);
}

INSTANTIATE_TEST_SUITE_P(Drifts, TwentyMinuteDrift, ::testing::Values(-250.0, -40.0, 40.0, 250.0));

// The same with speexdsp itself, for 100 simulated seconds at both ends of the range.
TEST(Drift, SpeexdspMeetsSection17AtTheEnds) {
  for (const double drift : {-500.0, 500.0}) {
    const DriftResult r = RunDrift(drift, 100 * kSecond, /*speex=*/true, 7);
    Report("speex ", drift, r);
    ExpectMeetsSection17(drift, r);
  }
}

// A drift of exactly 0 for 10 simulated minutes: the ratio is never 1:1 (§9.3, §19.1).
TEST(Drift, ZeroDriftNeverGivesPpmZero) {
  const DriftResult r = RunDrift(0.0, 10 * 60 * kSecond, false, 3);
  EXPECT_EQ(r.zero_ppm, 0u);
  EXPECT_GE(r.max_abs_ppm, 1.0);
  EXPECT_LE(r.ppm.sd(), 30.0);
}

// Why the controller holds the head's age and not, as §9.3 writes, the fill sampled at each tick (controller.h): a
// control-level simulation of the same stream (exact ring counts, speexdsp's consumption from RateModel) shows the
// sampled fill's packet-phase sawtooth driving the ratio far past §17's 30 ppm spread, while the head age, which
// doesn't depend on when packets arrive, stays well inside it.
TEST(Drift, SampledFillControlMissesTheRatioBound) {
  auto run = [](bool sampled_fill, double drift) {
    test::Rng rng(5);
    RateModel model;
    FillController ctl;
    const double fps = kRate * (1 + drift * 1e-6); // device frames per QPC second
    uint64_t written = 0, consumed = 0;
    int64_t next_arrival = static_cast<int64_t>(480 / fps * kSecond) + 3 * kMs;
    Stats ppm;
    for (int64_t tick = 0; tick < 30 * 60 * kSecond; tick += kChunkNs) {
      while (next_arrival <= tick) {
        written += 480;
        next_arrival = static_cast<int64_t>(static_cast<double>(written + 480) / fps * kSecond) + 3 * kMs +
                       rng.UniformInt(-3 * kMs, 3 * kMs);
      }
      const double fill_ms = static_cast<double>(written - consumed) / kFramesPerMs;
      const double head_age_ns = static_cast<double>(tick) - static_cast<double>(consumed) / fps * kSecond;
      // The sampled fill enters as fill + 10 ms, so the controller's 50 ms target is §9.3's 40 ms fill.
      const auto measured = static_cast<int64_t>(sampled_fill ? (fill_ms + 10.0) * kMs : head_age_ns);
      if (tick == 40 * kMs) {
        ctl.Seed(measured);
      }
      if (tick >= 40 * kMs) {
        if (ctl.Tick(measured)) {
          model.SetSteps(ctl.steps());
        }
        uint32_t used = 0, made = 0;
        model.Run(nullptr, static_cast<uint32_t>(written - consumed), nullptr, kChunkFrames, &used, &made);
        consumed += used;
        if (tick >= 60 * kSecond) {
          ppm.Add(ctl.ppm());
        }
      }
    }
    return ppm.sd();
  };
  for (const double drift : {500.0, -250.0}) {
    const double sampled = run(true, drift), age = run(false, drift);
    std::printf("drift %+.0f ppm: ratio sd %.1f ppm with the sampled fill, %.1f ppm with the head age\n", drift,
                sampled, age);
    EXPECT_GT(sampled, 30.0) << drift;
    EXPECT_LE(age, 30.0) << drift;
  }
}

} // namespace
} // namespace im::audiomix
