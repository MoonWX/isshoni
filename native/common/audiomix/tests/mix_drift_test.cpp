// Adaptive resampling in simulation (docs/m2/05-windows-audio.md §9.3, §17, §19.1): device clocks drifting -500 ..
// +500 ppm against the QPC, capture packets arriving with +-3 ms of jitter. Pass: from 60 s on the ring fill stays at
// 40 +- 15 ms, no underrun or trim in the whole run, and the resampling ratio's standard deviation is at most 30 ppm
// (mix_drift_support.h). This file has the 20-minute runs (drifts between the ends; packets that come late or are
// long), speexdsp itself and the controller's own checks; the 2-hour runs at -500, 0 and +500 ppm are
// mix_drift_long_test's, a ctest entry of their own with its own time budget.
#include <gtest/gtest.h>

#include <cmath>
#include <cstdio>

#include "im/audiomix/controller.h"
#include "mix_drift_support.h"

namespace im::audiomix {
namespace {

using test::DriftResult;
using test::DriftRun;
using test::kMs;
using test::kSecond;
using test::Stats;

class TwentyMinuteDrift : public ::testing::TestWithParam<double> {};

TEST_P(TwentyMinuteDrift, MeetsSection17) {
  DriftRun run;
  run.drift_ppm = GetParam();
  run.seed = 2000 + static_cast<uint64_t>(run.drift_ppm + 1000);
  const DriftResult r = test::RunDrift(run);
  test::Report("model ", run, r);
  test::ExpectMeetsSection17(run, r);
  EXPECT_NEAR(r.ppm.mean(), run.drift_ppm, 8.0); // a fast device needs a ratio above 1 to drain
}

INSTANTIATE_TEST_SUITE_P(Drifts, TwentyMinuteDrift, ::testing::Values(-250.0, -40.0, 40.0, 250.0));

// Packets that reach the capture thread later than the 3 ms the other runs assume (a capture offset S1 never measured
// on hardware), or that are 20 ms long: the fill floor (mix_engine.h) holds the fill at 40 ms on average, so §17 holds
// at both ends of the drift range. The head age then sits above L0 (40 ms + the lag + half a packet).
struct LatePackets {
  double drift_ppm;
  int64_t latency_ms;
  uint32_t packet_ms;
};

class LateOrLongPackets : public ::testing::TestWithParam<LatePackets> {};

TEST_P(LateOrLongPackets, MeetSection17) {
  const LatePackets c = GetParam();
  DriftRun run;
  run.drift_ppm = c.drift_ppm;
  run.latency_ns = c.latency_ms * kMs;
  run.packet_frames = c.packet_ms * kFramesPerMs;
  run.seed = 3000 + static_cast<uint64_t>(c.drift_ppm + 1000) + static_cast<uint64_t>(c.latency_ms * 10 + c.packet_ms);
  const DriftResult r = test::RunDrift(run);
  test::Report("model ", run, r);
  test::ExpectMeetsSection17(run, r);
  EXPECT_NEAR(r.ppm.mean(), c.drift_ppm, 8.0);
  EXPECT_NEAR(r.fill_ms.mean(), 40.0, 2.0);
  // The floor: 40 ms + the lag + half a packet, or L0 when that is higher.
  const double floor_ms = 40.0 + static_cast<double>(c.latency_ms) + c.packet_ms / 2.0;
  EXPECT_NEAR(r.target_ms, std::max(50.0, floor_ms), 1.0);
}

INSTANTIATE_TEST_SUITE_P(Packets, LateOrLongPackets,
                         ::testing::Values(LatePackets{-500.0, 15, 10}, LatePackets{500.0, 15, 10},
                                           LatePackets{-500.0, 25, 10}, LatePackets{500.0, 25, 10},
                                           LatePackets{-500.0, 3, 20}, LatePackets{500.0, 3, 20}));

// The same with speexdsp itself at both ends of the range: 100 simulated seconds each, the statistics from 60 s on.
// Under a sanitizer 20 s, the statistics from 10 s on, while the ratio still converges: everything but its spread.
TEST(Drift, SpeexdspMeetsSection17AtTheEnds) {
  constexpr bool kSettled = test::kSpeexRun > 60 * kSecond;
  if (!kSettled) {
    std::printf("note: a short run under a sanitizer; host-debug checks the ratio's spread with speexdsp\n");
  }
  for (const double drift : {-500.0, 500.0}) {
    DriftRun run;
    run.drift_ppm = drift;
    run.duration = test::kSpeexRun;
    run.settle_ns = kSettled ? 60 * kSecond : run.duration / 2;
    run.speex = true;
    run.seed = 7;
    const DriftResult r = test::RunDrift(run);
    test::Report("speex ", run, r);
    test::ExpectMeetsSection17(run, r, kSettled);
  }
}

// A drift of exactly 0 for 10 simulated minutes: the ratio is never 1:1 (§9.3, §19.1).
TEST(Drift, ZeroDriftNeverGivesPpmZero) {
  DriftRun run;
  run.duration = test::kTenMinutes;
  run.seed = 3;
  const DriftResult r = test::RunDrift(run);
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
