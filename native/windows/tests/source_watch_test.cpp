// source_watch_test (docs/m2/04-windows-engine.md §7.6, §7.7; README WF12): the session supervisor's plain parts for a
// running capture (capture/source_watch.h): SourceWatcher runs the resize debounce and the minimize polls as the
// supervisor does (one SOURCE_RESIZED when a resize is first seen, the settled size after 1 s of stability, IsIconic
// only every 500 ms and only for an identified window, the next deadline), ReinitForSize picks the layers whose output
// size changes, and PackSize carries a size through one atomic word. Plain C++: it builds and runs on the Mac too
// (window_match_test covers ResizeTracker and MinimizeTracker themselves).
#include <gtest/gtest.h>

#include <cstdint>
#include <optional>

#include "capture/source_watch.h"

namespace im::capture {
namespace {

constexpr int64_t kMs = 1'000'000;

using Change = MinimizeTracker::Change;

TEST(PackSize, RoundTrips) {
  for (const video::Size s : {video::Size{0, 0}, video::Size{1, 2}, video::Size{1920, 1080},
                              video::Size{0xFFFFFFFFu, 0xFFFFFFFFu}, video::Size{7680, 4320}}) {
    EXPECT_EQ(UnpackSize(PackSize(s)), s) << s.width << "x" << s.height;
  }
  EXPECT_NE(PackSize(video::Size{1, 0}), PackSize(video::Size{0, 1}));
  EXPECT_NE(PackSize(video::Size{1280, 720}), 0u) << "0 means no size";
}

TEST(ReinitForSize, OnlyWhenTheOutputSizeChanges) {
  const video::LayerBounds f{1920 * 1080, 0, 0}; // f: up to 1080p
  const video::LayerBounds q{640 * 360, 0, 0};   // q: 360p
  // 1280x720 → 1920x1080: f grows to 1080p, q stays 640x360.
  EXPECT_TRUE(ReinitForSize(video::Size{1920, 1080}, f, video::Size{1280, 720}));
  EXPECT_FALSE(ReinitForSize(video::Size{1920, 1080}, q, video::Size{640, 360}));
  // Larger than the bound in the same aspect: f stays at its bound.
  EXPECT_FALSE(ReinitForSize(video::Size{3840, 2160}, f, video::Size{1920, 1080}));
  // Another aspect changes both.
  EXPECT_TRUE(ReinitForSize(video::Size{1000, 1000}, q, video::Size{640, 360}));
  // No picture: never a re-init.
  EXPECT_FALSE(ReinitForSize(video::Size{0, 0}, f, video::Size{1280, 720}));
}

TEST(SourceWatcher, OneResizedEventThenTheSettledSizeAfterOneSecond) {
  int polls = 0;
  const auto iconic = [&] {
    ++polls;
    return false;
  };
  SourceWatcher w(video::Size{1280, 720}, false, 0);
  EXPECT_EQ(w.NextDueNs(), SourceWatcher::kNever) << "nothing to do: no window, no resize";

  SourceWatcher::Step s = w.Poll(std::nullopt, 10 * kMs, iconic);
  EXPECT_FALSE(s.resized);
  EXPECT_FALSE(s.settled);

  s = w.Poll(video::Size{1300, 730}, 100 * kMs, iconic);
  ASSERT_TRUE(s.resized);
  EXPECT_EQ(*s.resized, (video::Size{1300, 730})) << "the size first seen";
  EXPECT_FALSE(s.settled);
  EXPECT_EQ(w.NextDueNs(), 100 * kMs + kResizeSettleNs);

  // The drag goes on: no second event, the deadline moves.
  s = w.Poll(video::Size{1500, 800}, 600 * kMs, iconic);
  EXPECT_FALSE(s.resized);
  EXPECT_EQ(w.NextDueNs(), 600 * kMs + kResizeSettleNs);
  s = w.Poll(std::nullopt, 1500 * kMs, iconic);
  EXPECT_FALSE(s.settled) << "stable for 0.9 s only";
  s = w.Poll(std::nullopt, 1600 * kMs, iconic);
  ASSERT_TRUE(s.settled);
  EXPECT_EQ(*s.settled, (video::Size{1500, 800}));
  EXPECT_EQ(w.settled(), (video::Size{1500, 800}));
  EXPECT_EQ(w.NextDueNs(), SourceWatcher::kNever);
  EXPECT_EQ(polls, 0) << "IsIconic is never asked without a window";

  // The next resize is a new event.
  s = w.Poll(video::Size{800, 600}, 2000 * kMs, iconic);
  EXPECT_TRUE(s.resized);
}

TEST(SourceWatcher, ADragBackToTheSettledSizeSettlesWithoutAReinit) {
  const auto iconic = [] { return false; };
  SourceWatcher w(video::Size{1280, 720}, false, 0);
  EXPECT_TRUE(w.Poll(video::Size{1300, 720}, 0, iconic).resized);
  EXPECT_FALSE(w.Poll(video::Size{1280, 720}, 200 * kMs, iconic).resized);
  const SourceWatcher::Step s = w.Poll(std::nullopt, 200 * kMs + kResizeSettleNs, iconic);
  EXPECT_FALSE(s.settled);
  EXPECT_EQ(w.NextDueNs(), SourceWatcher::kNever) << "the resize is over";
}

TEST(SourceWatcher, AnEmptyContentSizeIsNoResize) {
  const auto iconic = [] { return false; };
  SourceWatcher w(video::Size{1280, 720}, false, 0);
  EXPECT_FALSE(w.Poll(video::Size{0, 0}, 0, iconic).resized) << "a window on its way to minimized";
  EXPECT_FALSE(w.Poll(video::Size{1280, 720}, 10 * kMs, iconic).resized) << "the same size again";
}

TEST(SourceWatcher, MinimizePollsEvery500msAndReportsEdges) {
  bool iconic_now = false;
  int polls = 0;
  const auto iconic = [&] {
    ++polls;
    return iconic_now;
  };
  SourceWatcher w(video::Size{1280, 720}, true, 1000 * kMs);
  EXPECT_EQ(w.NextDueNs(), 1000 * kMs) << "the first poll is due at once";

  EXPECT_EQ(w.Poll(std::nullopt, 1000 * kMs, iconic).minimize, Change::kNone);
  EXPECT_EQ(polls, 1);
  EXPECT_EQ(w.NextDueNs(), 1000 * kMs + kMinimizePollNs);

  iconic_now = true;
  EXPECT_EQ(w.Poll(std::nullopt, 1100 * kMs, iconic).minimize, Change::kNone) << "not due yet";
  EXPECT_EQ(polls, 1);
  EXPECT_EQ(w.Poll(std::nullopt, 1500 * kMs, iconic).minimize, Change::kMinimized);
  EXPECT_TRUE(w.minimized());
  EXPECT_EQ(w.Poll(std::nullopt, 2000 * kMs, iconic).minimize, Change::kNone) << "one event per edge";
  iconic_now = false;
  EXPECT_EQ(w.Poll(std::nullopt, 2500 * kMs, iconic).minimize, Change::kRestored);
  EXPECT_FALSE(w.minimized());
  EXPECT_EQ(polls, 4);
}

TEST(SourceWatcher, TheDeadlineIsTheEarlierOfTheSettleAndThePoll) {
  const auto iconic = [] { return false; };
  SourceWatcher w(video::Size{1280, 720}, true, 0);
  w.Poll(std::nullopt, 0, iconic); // next poll at 500 ms
  w.Poll(video::Size{1920, 1080}, 100 * kMs, iconic);
  EXPECT_EQ(w.NextDueNs(), kMinimizePollNs) << "the poll comes before the settle (1.1 s)";
  w.Poll(std::nullopt, kMinimizePollNs, iconic);     // next poll at 1 s
  w.Poll(std::nullopt, 2 * kMinimizePollNs, iconic); // next poll at 1.5 s
  EXPECT_EQ(w.NextDueNs(), 100 * kMs + kResizeSettleNs) << "now the settle comes first";
}

} // namespace
} // namespace im::capture
