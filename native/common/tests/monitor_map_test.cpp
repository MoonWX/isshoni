// Windows 10: mapping a picked monitor to a DXGI output (docs/m2/04-windows-engine.md §7.5, §18.1): steps 2, 3 and
// 5 with fake outputs: one size match, a name match among several, two identical monitors with the same name falling
// through to WGC with the border (frame-match cases come with step 4, §16).
#include <gtest/gtest.h>

#include <vector>

#include "im/capture/monitor_map.h"

namespace {

using im::capture::MapMonitor;
using im::capture::MapResult;
using im::capture::OutputDesc;
using Outcome = MapResult::Outcome;

OutputDesc Out(const char *device, const char *friendly, int32_t left, int32_t top, int32_t w, int32_t h) {
  OutputDesc o;
  o.device_name = device;
  o.friendly_name = friendly;
  o.left = left;
  o.top = top;
  o.right = left + w;
  o.bottom = top + h;
  return o;
}

TEST(MonitorMap, OneSizeMatch) {
  const std::vector<OutputDesc> outs = {
      Out(R"(\\.\DISPLAY1)", "DELL U2723QE", 0, 0, 3840, 2160),
      Out(R"(\\.\DISPLAY2)", "LG 24MK600", 3840, 0, 1920, 1080),
  };
  const MapResult r = MapMonitor(1920, 1080, "whatever the picker says", outs);
  EXPECT_EQ(r.outcome, Outcome::kBySize);
  EXPECT_EQ(r.output, 1);
  EXPECT_TRUE(r.mapped());
  EXPECT_EQ(r.size_matches, 1u);
}

TEST(MonitorMap, NameMatchAmongSeveral) {
  const std::vector<OutputDesc> outs = {
      Out(R"(\\.\DISPLAY1)", "DELL U2723QE", 0, 0, 2560, 1440),
      Out(R"(\\.\DISPLAY2)", "LG 27GL850", 2560, 0, 2560, 1440),
      Out(R"(\\.\DISPLAY3)", "LG 24MK600", 5120, 0, 1920, 1080),
  };
  // By friendly name.
  MapResult r = MapMonitor(2560, 1440, "LG 27GL850", outs);
  EXPECT_EQ(r.outcome, Outcome::kByName);
  EXPECT_EQ(r.output, 1);
  EXPECT_EQ(r.size_matches, 2u);
  EXPECT_EQ(r.name_matches, 1u);
  // By device name.
  r = MapMonitor(2560, 1440, R"(\\.\DISPLAY1)", outs);
  EXPECT_EQ(r.outcome, Outcome::kByName);
  EXPECT_EQ(r.output, 0);
  // A name that only matches an output of another size doesn't count.
  r = MapMonitor(2560, 1440, "LG 24MK600", outs);
  EXPECT_EQ(r.outcome, Outcome::kAmbiguous);
  EXPECT_FALSE(r.mapped());
}

TEST(MonitorMap, IdenticalMonitorsFallThroughToWgc) {
  // Two monitors of the same model: same size, same friendly name. DisplayName() returning the model name leaves
  // both, so there is no mapping (step 5: WGC with the border). Step 4's frame match would start here (§16).
  const std::vector<OutputDesc> outs = {
      Out(R"(\\.\DISPLAY1)", "DELL P2422H", 0, 0, 1920, 1080),
      Out(R"(\\.\DISPLAY2)", "DELL P2422H", 1920, 0, 1920, 1080),
  };
  const MapResult r = MapMonitor(1920, 1080, "DELL P2422H", outs);
  EXPECT_EQ(r.outcome, Outcome::kAmbiguous);
  EXPECT_EQ(r.output, -1);
  EXPECT_EQ(r.size_matches, 2u);
  EXPECT_EQ(r.name_matches, 2u);
  // If DisplayName() is the device name, step 3 resolves even identical monitors.
  const MapResult d = MapMonitor(1920, 1080, R"(\\.\DISPLAY2)", outs);
  EXPECT_EQ(d.outcome, Outcome::kByName);
  EXPECT_EQ(d.output, 1);
}

TEST(MonitorMap, NoSizeMatch) {
  const std::vector<OutputDesc> outs = {Out(R"(\\.\DISPLAY1)", "A", 0, 0, 1920, 1080)};
  const MapResult r = MapMonitor(2560, 1440, R"(\\.\DISPLAY1)", outs);
  EXPECT_EQ(r.outcome, Outcome::kNoSizeMatch);
  EXPECT_FALSE(r.mapped());
  EXPECT_EQ(MapMonitor(1920, 1080, "", {}).outcome, Outcome::kNoSizeMatch) << "no outputs";
}

TEST(MonitorMap, EmptyNamesNeverMatch) {
  const std::vector<OutputDesc> outs = {
      Out(R"(\\.\DISPLAY1)", "", 0, 0, 1920, 1080),
      Out(R"(\\.\DISPLAY2)", "", 1920, 0, 1920, 1080),
  };
  EXPECT_EQ(MapMonitor(1920, 1080, "", outs).outcome, Outcome::kAmbiguous);
  EXPECT_EQ(MapMonitor(1920, 1080, "", outs).name_matches, 0u);
}

TEST(MonitorMap, RotatedAndNegativeCoordinates) {
  // A portrait monitor left of the primary: desktop coordinates are after rotation and may be negative.
  const std::vector<OutputDesc> outs = {
      Out(R"(\\.\DISPLAY1)", "Main", 0, 0, 2560, 1440),
      Out(R"(\\.\DISPLAY2)", "Side", -1080, -240, 1080, 1920),
  };
  const MapResult r = MapMonitor(1080, 1920, "Side", outs);
  EXPECT_EQ(r.outcome, Outcome::kBySize);
  EXPECT_EQ(r.output, 1);
  EXPECT_EQ(MapMonitor(1920, 1080, "Side", outs).outcome, Outcome::kNoSizeMatch) << "unrotated size: no match";
}

} // namespace
