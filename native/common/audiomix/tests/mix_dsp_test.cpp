// The mix bus's sample rules (docs/m2/05-windows-audio.md §9.6): the soft clip above -1 dBFS and the level floor.
#include <gtest/gtest.h>

#include <cmath>
#include <limits>

#include "im/audiomix/dsp.h"
#include "im/audiomix/mix_engine.h"

namespace im::audiomix {
namespace {

TEST(SoftClip, IdentityUpToTheKnee) {
  for (float x = -kSoftClipKnee; x <= kSoftClipKnee; x += 0.001f) {
    EXPECT_EQ(SoftClip(x), x);
  }
  EXPECT_EQ(SoftClip(0.0f), 0.0f);
}

TEST(SoftClip, ContinuousWithSlopeOneAtTheKnee) {
  constexpr float kEps = 1e-4f;
  for (const float sign : {1.0f, -1.0f}) {
    const float t = sign * kSoftClipKnee;
    // Value continuity.
    EXPECT_NEAR(SoftClip(t + sign * 1e-6f), t, 2e-6f);
    // Slope: the one-sided derivatives on both sides are 1.
    const float below = (SoftClip(t) - SoftClip(t - sign * kEps)) / (sign * kEps);
    const float above = (SoftClip(t + sign * kEps) - SoftClip(t)) / (sign * kEps);
    EXPECT_NEAR(below, 1.0f, 1e-2f);
    EXPECT_NEAR(above, 1.0f, 1e-2f);
  }
}

TEST(SoftClip, BoundedMonotonicOddAboveTheKnee) {
  float prev = SoftClip(kSoftClipKnee);
  for (float x = kSoftClipKnee; x < 20.0f; x += 0.01f) {
    const float y = SoftClip(x);
    EXPECT_GE(y, prev) << x;
    EXPECT_LE(y, 1.0f) << x;
    EXPECT_EQ(SoftClip(-x), -y) << x;
    prev = y;
  }
  EXPECT_NEAR(SoftClip(1.0f), 0.891f + 0.109f * std::tanh(0.109f / 0.109f), 1e-6f);
  EXPECT_LE(SoftClip(std::numeric_limits<float>::infinity()), 1.0f);
  EXPECT_GE(SoftClip(-std::numeric_limits<float>::infinity()), -1.0f);
  EXPECT_EQ(SoftClip(std::numeric_limits<float>::quiet_NaN()), 0.0f);
}

TEST(Levels, DbWithTheFloor) {
  EXPECT_EQ(LinearToDb(0.0), kLevelFloorDb);
  EXPECT_EQ(LinearToDb(-1.0), kLevelFloorDb);
  EXPECT_EQ(LinearToDb(1e-7), kLevelFloorDb);
  EXPECT_NEAR(LinearToDb(1.0), 0.0f, 1e-6f);
  EXPECT_NEAR(LinearToDb(0.5), -6.0206f, 1e-3f);
  EXPECT_EQ(LinearToDb(std::numeric_limits<double>::quiet_NaN()), kLevelFloorDb);
}

TEST(Levels, PerAppSumAddsPeaksAndPowers) {
  LevelSum s;
  EXPECT_EQ(s.peak_db(), kLevelFloorDb);
  EXPECT_EQ(s.rms_db(), kLevelFloorDb);
  s.Add(-6.0206f, -9.0309f); // 0.5 peak, 0.3536 RMS (a -6 dBFS sine)
  s.Add(-6.0206f, -9.0309f);
  EXPECT_NEAR(s.peak_db(), 0.0f, 1e-3f);              // peaks add linearly
  EXPECT_NEAR(s.rms_db(), -9.0309f + 3.0103f, 1e-3f); // powers add: +3 dB
  s.Add(kLevelFloorDb, kLevelFloorDb);                // a silent channel changes nothing
  EXPECT_NEAR(s.peak_db(), 0.0f, 1e-3f);
}

} // namespace
} // namespace im::audiomix
