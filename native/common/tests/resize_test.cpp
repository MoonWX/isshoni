// Source resizes (docs/m2/04-windows-engine.md §7.7, §18.1): the 1 s debounce; the letterbox meanwhile; no re-init
// when the computed output size is unchanged.
#include <gtest/gtest.h>

#include <optional>

#include "im/video/resize.h"
#include "im/video/scale.h"

namespace {

using im::video::LayerBounds;
using im::video::Letterbox;
using im::video::PlanLayerResize;
using im::video::Rect;
using im::video::ResizeDebouncer;
using im::video::Size;

constexpr int64_t kMs = 1'000'000;
constexpr int64_t kT0 = 5'000'000'000'000;

LayerBounds MaxPixels(uint32_t mp) {
  LayerBounds b;
  b.max_pixels = mp;
  return b;
}

TEST(Resize, OneSecondDebounce) {
  ResizeDebouncer d({1280, 720});
  EXPECT_FALSE(d.pending());
  EXPECT_EQ(d.NextDeadlineNs(), ResizeDebouncer::kNever);
  EXPECT_FALSE(d.OnSourceSize({1280, 720}, kT0)) << "unchanged";
  // A drag: sizes change every 16 ms for half a second. SOURCE_RESIZED once, when the change is first seen.
  int events = 0;
  int64_t t = kT0;
  for (uint32_t i = 1; i <= 30; ++i) {
    events += d.OnSourceSize({1280 + i * 4, 720 + i * 2}, t) ? 1 : 0;
    EXPECT_FALSE(d.Poll(t).has_value());
    t += 16 * kMs;
  }
  EXPECT_EQ(events, 1);
  EXPECT_TRUE(d.pending());
  const int64_t last_change = t - 16 * kMs;
  EXPECT_EQ(d.NextDeadlineNs(), last_change + 1000 * kMs);
  EXPECT_FALSE(d.Poll(last_change + 999 * kMs).has_value()) << "stable for less than 1 s";
  const std::optional<Size> stable = d.Poll(last_change + 1000 * kMs);
  ASSERT_TRUE(stable.has_value());
  EXPECT_EQ(*stable, (Size{1400, 780}));
  EXPECT_EQ(d.encoded_source(), (Size{1400, 780}));
  EXPECT_FALSE(d.pending());
  EXPECT_FALSE(d.Poll(last_change + 5000 * kMs).has_value()) << "once";
  // A later change is a new episode with its own event.
  EXPECT_TRUE(d.OnSourceSize({1920, 1080}, last_change + 6000 * kMs));
}

TEST(Resize, LetterboxMeanwhile) {
  // Layer f runs at 1280×720 for a 1280×720 window; the window becomes 1000×1000 while the debounce runs: the new
  // content is fitted into the unchanged output with bars.
  ResizeDebouncer d({1280, 720});
  ASSERT_TRUE(d.OnSourceSize({1000, 1000}, kT0));
  EXPECT_EQ(Letterbox(d.current_source(), {1280, 720}), (Rect{280, 0, 720, 720}));
  EXPECT_EQ(Letterbox({2000, 500}, {1280, 720}), (Rect{0, 200, 1280, 320}));
  EXPECT_EQ(d.encoded_source(), (Size{1280, 720})) << "the encoders keep their size meanwhile";
}

TEST(Resize, ReinitOnlyWhenTheOutputSizeChanges) {
  LayerBounds f = MaxPixels(2'073'600);
  LayerBounds box;
  box.width = 1280;
  // A 4K monitor's mode goes from 3840×2160 to 2560×1440: layer f stays 1920×1080, no re-init.
  auto r = PlanLayerResize({2560, 1440}, f, {1920, 1080});
  EXPECT_FALSE(r.reinit);
  EXPECT_EQ(r.output.size, (Size{1920, 1080}));
  // A one-pixel window change within a 1280 box: still 1280×720.
  r = PlanLayerResize({1921, 1081}, box, {1280, 720});
  EXPECT_FALSE(r.reinit);
  EXPECT_EQ(r.output.content, (Rect{0, 0, 1280, 720})) << "the letterbox ends";
  // A real change re-inits (REINIT|KEYFRAME) at the new size.
  r = PlanLayerResize({1000, 1000}, f, {1920, 1080});
  EXPECT_TRUE(r.reinit);
  EXPECT_EQ(r.output.size, (Size{1000, 1000}));
  // Back to the old size before the debounce ended: no re-init.
  ResizeDebouncer d({1920, 1080});
  ASSERT_TRUE(d.OnSourceSize({1600, 900}, kT0));
  EXPECT_FALSE(d.OnSourceSize({1920, 1080}, kT0 + 300 * kMs));
  const auto stable = d.Poll(kT0 + 1300 * kMs);
  ASSERT_TRUE(stable.has_value());
  EXPECT_EQ(*stable, (Size{1920, 1080}));
  EXPECT_FALSE(PlanLayerResize(*stable, f, {1920, 1080}).reinit);
}

TEST(Resize, EachLayerDecidesForItself) {
  // A window grows from 1920×1080 to 1920×1088: both budgets give new sizes, so both layers re-init.
  const auto q = PlanLayerResize({1920, 1088}, MaxPixels(230'400), {640, 360});
  EXPECT_EQ(q.output.size, (Size{636, 360}));
  EXPECT_TRUE(q.reinit);
  const auto f = PlanLayerResize({1920, 1088}, MaxPixels(2'073'600), {1920, 1080});
  EXPECT_EQ(f.output.size, (Size{1912, 1082}));
  EXPECT_TRUE(f.reinit);
  // A layer bounded by a width whose computed height rounds the same keeps its size.
  LayerBounds box;
  box.width = 640;
  EXPECT_FALSE(PlanLayerResize({1920, 1081}, box, {640, 360}).reinit);
  EXPECT_TRUE(PlanLayerResize({1920, 1088}, box, {640, 360}).reinit) << "640×362";
}

} // namespace
