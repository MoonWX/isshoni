// The PI controller of adaptive resampling (docs/m2/05-windows-audio.md §9.3): never ratio 1:1, the slew limit, the
// clamps, the hold of the silent bypass. The closed loop runs in mix_drift_test.
#include <gtest/gtest.h>

#include <cmath>

#include "im/audiomix/controller.h"

namespace im::audiomix {
namespace {

constexpr int64_t kTarget = 50 * kNsPerMs;

// Runs n ticks at a constant head age; returns how many updates ran.
int Feed(FillController &c, int64_t age_ns, int ticks) {
  int updates = 0;
  for (int i = 0; i < ticks; ++i) {
    updates += c.Tick(age_ns) ? 1 : 0;
  }
  return updates;
}

TEST(FillController, StartsAtPlusOneStepAndUpdatesEvery100ms) {
  FillController c;
  EXPECT_EQ(c.steps(), 1);
  EXPECT_EQ(c.ppm(), kPpmPerStep);
  EXPECT_EQ(Feed(c, kTarget, 100), 10); // 1 s of ticks
  EXPECT_EQ(c.steps(), 1);              // no error: it stays at +1 step, never 0
}

TEST(FillController, NeverZeroAroundZero) {
  FillController c;
  c.Seed(kTarget);
  // An error that hovers around zero in both directions: the command crosses 0 many times.
  int negative = 0, positive = 0;
  for (int t = 0; t < 60'000; ++t) { // 10 minutes of ticks
    const double phase = std::sin(t / 700.0);
    const int64_t age = kTarget + static_cast<int64_t>(phase * 300'000); // +-0.3 ms
    if (c.Tick(age)) {
      ASSERT_NE(c.steps(), 0) << "tick " << t;
      ASSERT_GE(std::abs(c.ppm()), 1);
      negative += c.steps() < 0;
      positive += c.steps() > 0;
    }
  }
  EXPECT_GT(negative, 0);
  EXPECT_GT(positive, 0);
}

TEST(FillController, ZeroBecomesMinusOneAfterANegativeRatio) {
  ControllerParams p;
  p.ki = 0;
  p.slew_ppm = 1000;
  FillController c(p);
  c.Seed(kTarget - 2 * kNsPerMs); // 2 ms short: -100 ppm
  Feed(c, kTarget - 2 * kNsPerMs, 10);
  EXPECT_LT(c.steps(), 0);
  // The error vanishes: the command is 0, which becomes -1 because the ratio was negative.
  c.Seed(kTarget);
  Feed(c, kTarget, 10);
  EXPECT_EQ(c.steps(), -1);
  // And +1 after a positive one.
  c.Seed(kTarget + 2 * kNsPerMs);
  Feed(c, kTarget + 2 * kNsPerMs, 10);
  EXPECT_GT(c.steps(), 0);
  c.Seed(kTarget);
  Feed(c, kTarget, 10);
  EXPECT_EQ(c.steps(), 1);
}

TEST(FillController, SlewLimitsEachUpdate) {
  FillController c;
  c.Seed(kTarget + 20 * kNsPerMs); // 20 ms too old: Kp alone asks for 1000 ppm
  double prev = c.command_ppm();
  for (int u = 0; u < 30; ++u) {
    Feed(c, kTarget + 20 * kNsPerMs, 10);
    EXPECT_LE(c.command_ppm() - prev, 20.0 + 1e-9);
    prev = c.command_ppm();
  }
  EXPECT_NEAR(c.command_ppm(), 16 + 30 * 20.0, 1e-6); // 30 updates of +20 from the initial +16
}

TEST(FillController, ClampsTheRatioAndTheIntegral) {
  FillController c;
  c.Seed(kTarget + 500 * kNsPerMs);
  Feed(c, kTarget + 500 * kNsPerMs, 10 * 600); // 10 minutes far off
  EXPECT_EQ(c.steps(), kMaxSteps);
  EXPECT_EQ(c.ppm(), 992);
  EXPECT_DOUBLE_EQ(c.integral(), 400.0);
  c.Seed(kTarget - 500 * kNsPerMs);
  Feed(c, kTarget - 500 * kNsPerMs, 10 * 600);
  EXPECT_EQ(c.steps(), -kMaxSteps);
  EXPECT_DOUBLE_EQ(c.integral(), -400.0);
}

TEST(FillController, HoldFreezesTheRatioNotTheAverage) {
  FillController c;
  c.Seed(kTarget);
  c.SetHold(true);
  EXPECT_EQ(Feed(c, kTarget + 10 * kNsPerMs, 300), 0);
  EXPECT_EQ(c.steps(), 1);
  EXPECT_NEAR(c.age_ns(), kTarget + 10 * kNsPerMs, 0.06 * 10 * kNsPerMs); // 3 s of a 1 s average: 95 % there
  c.SetHold(false);
  EXPECT_EQ(Feed(c, kTarget + 10 * kNsPerMs, 10), 1);
  EXPECT_GT(c.steps(), 1);
}

TEST(FillController, AverageIsOneSecond) {
  FillController c;
  c.Seed(0);
  Feed(c, 100 * kNsPerMs, 100); // one time constant
  EXPECT_NEAR(c.age_ns() / (100.0 * kNsPerMs), 1.0 - std::exp(-1.0), 0.01);
}

TEST(FillController, TargetFollowsSetTarget) {
  FillController c;
  c.Seed(45 * kNsPerMs);
  EXPECT_NEAR(c.error_ms(), -5.0, 1e-9);
  c.SetTarget(45 * kNsPerMs);
  EXPECT_NEAR(c.error_ms(), 0.0, 1e-9);
  EXPECT_EQ(c.target_ns(), 45 * kNsPerMs);
}

} // namespace
} // namespace im::audiomix
