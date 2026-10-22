// The drift simulation's 2-hour runs (docs/m2/05-windows-audio.md §17, §19.1): device clocks at -500, 0 and +500 ppm
// against the QPC, 10 ms capture packets arriving 3 ms after their mix with +-3 ms of jitter, 2 simulated hours each
// (10 minutes under a sanitizer). Pass: from 60 s on the ring fill stays at 40 +- 15 ms, no underrun or trim in the
// whole run, and the resampling ratio's standard deviation is at most 30 ppm (mix_drift_support.h). A file of its own
// so ctest gives it its own time budget; the shorter runs are mix_drift_test's.
#include <gtest/gtest.h>

#include "mix_drift_support.h"

namespace im::audiomix {
namespace {

class TwoHourDrift : public ::testing::TestWithParam<double> {};

TEST_P(TwoHourDrift, MeetsSection17) {
  test::DriftRun run;
  run.drift_ppm = GetParam();
  run.duration = test::kTwoHours;
  run.seed = 1000 + static_cast<uint64_t>(run.drift_ppm + 1000);
  const test::DriftResult r = test::RunDrift(run);
  test::Report("model ", run, r);
  test::ExpectMeetsSection17(run, r);
  // The ratio tracks the drift (a fast device needs a ratio above 1 to drain).
  EXPECT_NEAR(r.ppm.mean(), run.drift_ppm, 8.0);
}

INSTANTIATE_TEST_SUITE_P(Drifts, TwoHourDrift, ::testing::Values(-500.0, 0.0, 500.0));

} // namespace
} // namespace im::audiomix
