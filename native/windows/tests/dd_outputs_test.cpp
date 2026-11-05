// dd_outputs_test (docs/m2/04-windows-engine.md §18.2, README WA60): the Windows half of §7.5's monitor mapping over
// fake outputs (common's monitor_map_test checks MapMonitor itself): one size match, a name match among several (by
// DeviceName and by friendly name), two identical monitors falling through to WGC with the border (step 5), a rotated
// monitor's upright size; how a pick is captured on Windows 10 and 11 (PlanPick); the "mon:" ids and
// im_list_sources' document (§5.6); and Desktop Duplication's timing rules (§7.4, §7.8: dd_policy.h).
#include <gtest/gtest.h>

#include <cstdint>
#include <limits>
#include <string>
#include <vector>

#include "capture/dd_policy.h"
#include "capture/outputs.h"
#include "isshoni_media.h"

namespace im::capture {
namespace {

constexpr int64_t kMs = 1'000'000;

DisplayOutput Out(const char *device, const char *friendly, int32_t left, int32_t top, int32_t w, int32_t h) {
  DisplayOutput o;
  o.device_name = device;
  o.friendly_name = friendly;
  o.left = left;
  o.top = top;
  o.right = left + w;
  o.bottom = top + h;
  return o;
}

// A fake OutputSystem: the code under test takes its list, as the picker takes Win32Outputs'.
class FakeOutputs final : public OutputSystem {
public:
  explicit FakeOutputs(std::vector<DisplayOutput> outputs) : outputs_(std::move(outputs)) {}
  std::vector<DisplayOutput> Outputs() override { return outputs_; }

private:
  std::vector<DisplayOutput> outputs_;
};

MonitorMatch MatchOn(OutputSystem &os, int32_t w, int32_t h, const char *name) {
  const std::vector<DisplayOutput> list = os.Outputs();
  return MatchPickedMonitor(list, PixelSize{w, h}, name);
}

PickPlan Plan(uint32_t kind, bool windows11, const MonitorMatch *match, bool borderless = false, bool self = false) {
  PickFacts f;
  f.kind = kind;
  f.windows11 = windows11;
  f.borderless = borderless;
  f.self = self;
  f.match = match;
  return PlanPick(f);
}

// ------------------------------------------------------------------------------------------- §7.5 steps 2, 3, 5

TEST(DdOutputs, OneSizeMatchMapsToDesktopDuplicationOnWindows10) {
  FakeOutputs os({Out(R"(\\.\DISPLAY1)", "DELL U2723QE", 0, 0, 3840, 2160),
                  Out(R"(\\.\DISPLAY2)", "DELL P2419H", 3840, 0, 1920, 1080)});
  const MonitorMatch m = MatchOn(os, 1920, 1080, "whatever Windows calls it");
  ASSERT_TRUE(m.output.has_value());
  EXPECT_EQ(m.result.outcome, MapResult::Outcome::kBySize);
  EXPECT_EQ(m.output->device_name, R"(\\.\DISPLAY2)");
  EXPECT_EQ(MapOutcomeName(m.result.outcome), "size");

  const PickPlan p = Plan(IM_SOURCE_MONITOR, false, &m);
  EXPECT_EQ(p.capture_path, IM_CAPTURE_DD);
  EXPECT_EQ(p.flags, IM_SRCF_MAPPED) << "DD never shows a border";
  EXPECT_EQ(p.monitor_id, R"(mon:\\.\DISPLAY2)");
}

TEST(DdOutputs, NameMatchAmongSeveralOfTheSameSize) {
  FakeOutputs os({Out(R"(\\.\DISPLAY1)", "LG 27GL850", 0, 0, 2560, 1440),
                  Out(R"(\\.\DISPLAY2)", "DELL S2721DGF", 2560, 0, 2560, 1440),
                  Out(R"(\\.\DISPLAY3)", "DELL P2419H", 5120, 0, 1920, 1080)});
  // By DeviceName (what step 3 hopes DisplayName() returns) ...
  MonitorMatch m = MatchOn(os, 2560, 1440, R"(\\.\DISPLAY2)");
  ASSERT_TRUE(m.output.has_value());
  EXPECT_EQ(m.result.outcome, MapResult::Outcome::kByName);
  EXPECT_EQ(m.result.size_matches, 2u);
  EXPECT_EQ(m.output->device_name, R"(\\.\DISPLAY2)");
  // ... or by the friendly name.
  m = MatchOn(os, 2560, 1440, "LG 27GL850");
  ASSERT_TRUE(m.output.has_value());
  EXPECT_EQ(m.output->device_name, R"(\\.\DISPLAY1)");
  EXPECT_EQ(Plan(IM_SOURCE_MONITOR, false, &m).capture_path, IM_CAPTURE_DD);
}

TEST(DdOutputs, TwoIdenticalMonitorsFallThroughToWgcWithTheBorder) {
  FakeOutputs os({Out(R"(\\.\DISPLAY1)", "DELL U2723QE", 0, 0, 3840, 2160),
                  Out(R"(\\.\DISPLAY2)", "DELL U2723QE", 3840, 0, 3840, 2160)});
  const MonitorMatch m = MatchOn(os, 3840, 2160, "DELL U2723QE");
  EXPECT_FALSE(m.output.has_value());
  EXPECT_EQ(m.result.outcome, MapResult::Outcome::kAmbiguous) << "step 4's case, not built in M2";
  EXPECT_EQ(m.result.size_matches, 2u);
  EXPECT_EQ(m.result.name_matches, 2u);
  EXPECT_EQ(MapOutcomeName(m.result.outcome), "ambiguous");

  const PickPlan p = Plan(IM_SOURCE_MONITOR, false, &m);
  EXPECT_EQ(p.capture_path, IM_CAPTURE_WGC);
  EXPECT_EQ(p.flags, IM_SRCF_OS_BORDER);
  EXPECT_EQ(p.monitor_id, "");
}

TEST(DdOutputs, NoOutputOfThatSizeIsStepFive) {
  FakeOutputs os({Out(R"(\\.\DISPLAY1)", "", 0, 0, 1920, 1080)});
  const MonitorMatch m = MatchOn(os, 1280, 720, R"(\\.\DISPLAY1)");
  EXPECT_FALSE(m.output.has_value());
  EXPECT_EQ(m.result.outcome, MapResult::Outcome::kNoSizeMatch);
  EXPECT_EQ(Plan(IM_SOURCE_MONITOR, false, &m).capture_path, IM_CAPTURE_WGC);

  FakeOutputs none({});
  EXPECT_FALSE(MatchOn(none, 1920, 1080, "x").output.has_value()) << "no outputs at all (EnumOutputs failed)";
  EXPECT_FALSE(MatchOn(os, 0, 0, "").output.has_value());
}

TEST(DdOutputs, ARotatedMonitorMapsByItsUprightSize) {
  // DesktopCoordinates are already upright: a portrait 1080×1920 monitor is 1080 wide, whatever the panel is.
  DisplayOutput portrait = Out(R"(\\.\DISPLAY2)", "", 1920, 0, 1080, 1920);
  portrait.rotation = kDxgiRotation90;
  FakeOutputs os({Out(R"(\\.\DISPLAY1)", "", 0, 0, 1920, 1080), portrait});
  const MonitorMatch m = MatchOn(os, 1080, 1920, "");
  ASSERT_TRUE(m.output.has_value());
  EXPECT_EQ(m.output->device_name, R"(\\.\DISPLAY2)");
  EXPECT_EQ(m.output->rotation, kDxgiRotation90);
}

// ------------------------------------------------------------------------------------------- §7.1: the paths

TEST(DdOutputs, PlanPickPerWindowsVersion) {
  FakeOutputs os({Out(R"(\\.\DISPLAY1)", "", 0, 0, 1920, 1080)});
  const MonitorMatch m = MatchOn(os, 1920, 1080, "");

  // Windows 11: WGC for monitors, borderless where allowed, the output kept for monitor_id (and the DD fallback).
  PickPlan p = Plan(IM_SOURCE_MONITOR, true, &m, /*borderless=*/true);
  EXPECT_EQ(p.capture_path, IM_CAPTURE_WGC);
  EXPECT_EQ(p.flags, 0u);
  EXPECT_EQ(p.monitor_id, R"(mon:\\.\DISPLAY1)");
  p = Plan(IM_SOURCE_MONITOR, true, &m, /*borderless=*/false);
  EXPECT_EQ(p.flags, IM_SRCF_OS_BORDER);

  // Windows: WGC on both versions, with the border unless borderless; self is kept; never a monitor_id.
  for (const bool win11 : {false, true}) {
    p = Plan(IM_SOURCE_WINDOW, win11, nullptr, false, /*self=*/true);
    EXPECT_EQ(p.capture_path, IM_CAPTURE_WGC);
    EXPECT_EQ(p.flags, IM_SRCF_OS_BORDER | IM_SRCF_SELF);
    EXPECT_EQ(p.monitor_id, "");
  }
  EXPECT_EQ(Plan(IM_SOURCE_WINDOW, true, nullptr, true).flags, 0u);

  // A Windows 10 monitor whose mapping didn't run (no outputs at all).
  p = Plan(IM_SOURCE_MONITOR, false, nullptr);
  EXPECT_EQ(p.capture_path, IM_CAPTURE_WGC);
  EXPECT_EQ(p.flags, IM_SRCF_OS_BORDER);
}

// ------------------------------------------------------------------------------- ids and im_list_sources (§5.6)

TEST(DdOutputs, MonitorIds) {
  const std::vector<DisplayOutput> list = {Out(R"(\\.\DISPLAY1)", "", 0, 0, 1920, 1080),
                                           Out(R"(\\.\DISPLAY12)", "", 1920, 0, 1920, 1080)};
  EXPECT_EQ(MonitorIdOf(list[0]), R"(mon:\\.\DISPLAY1)");
  EXPECT_EQ(FindMonitor(list, R"(mon:\\.\DISPLAY12)"), 1u);
  EXPECT_EQ(FindMonitor(list, R"(mon:\\.\DISPLAY1)"), 0u) << "a prefix of another id is not that id";
  EXPECT_FALSE(FindMonitor(list, R"(mon:\\.\DISPLAY3)").has_value());
  EXPECT_FALSE(FindMonitor(list, R"(\\.\DISPLAY1)").has_value());
  EXPECT_FALSE(FindMonitor(list, "mon:").has_value());
  EXPECT_FALSE(FindMonitor(list, "pick:1").has_value());
}

TEST(DdOutputs, RotationDegrees) {
  EXPECT_EQ(RotationDegrees(kDxgiRotationUnspecified), 0u);
  EXPECT_EQ(RotationDegrees(kDxgiRotationIdentity), 0u);
  EXPECT_EQ(RotationDegrees(kDxgiRotation90), 90u);
  EXPECT_EQ(RotationDegrees(kDxgiRotation180), 180u);
  EXPECT_EQ(RotationDegrees(kDxgiRotation270), 270u);
  EXPECT_EQ(RotationDegrees(99), 0u);
}

TEST(DdOutputs, SourcesJsonIsSection56sDocument) {
  DisplayOutput a = Out(R"(\\.\DISPLAY1)", "DELL U2723QE", 0, 0, 3840, 2160);
  a.primary = true;
  a.dpi = 144;
  a.refresh_hz = 60;
  DisplayOutput b = Out(R"(\\.\DISPLAY2)", "", -1080, -200, 1080, 1920);
  b.rotation = kDxgiRotation270;
  b.hdr = true;
  b.adapter_index = 1;
  b.refresh_hz = 0;
  const std::vector<DisplayOutput> list = {a, b};
  EXPECT_EQ(SourcesJson(list),
            R"({"monitors":[{"id":"mon:\\\\.\\DISPLAY1","name":"DELL U2723QE","primary":true,"x":0,"y":0,)"
            R"("width":3840,"height":2160,"dpi":144,"refreshHz":60,"rotation":0,"hdr":false,"adapter":0},)"
            R"({"id":"mon:\\\\.\\DISPLAY2","name":"\\\\.\\DISPLAY2","primary":false,"x":-1080,"y":-200,)"
            R"("width":1080,"height":1920,"dpi":96,"refreshHz":0,"rotation":270,"hdr":true,"adapter":1}],)"
            R"("windows":[]})");
  EXPECT_EQ(SourcesJson({}), R"({"monitors":[],"windows":[]})");
}

// ------------------------------------------------------------------------------------- §7.4, §7.8: dd_policy.h

TEST(DdPolicy, InterruptCause) {
  EXPECT_EQ(InterruptCause(true, true), "secure_desktop");
  EXPECT_EQ(InterruptCause(true, false), "secure_desktop");
  EXPECT_EQ(InterruptCause(false, true), "mode_change");
  EXPECT_EQ(InterruptCause(false, false), "access_lost");
}

TEST(DdPolicy, RecoveryRetriesEvery250msAndResumes) {
  DuplicationRecovery r;
  EXPECT_FALSE(r.interrupted());
  EXPECT_EQ(r.NextRetryNs(), kDdNever);
  const int64_t t0 = 1'000 * kMs;
  EXPECT_TRUE(r.Lost(t0)) << "the first loss starts an interruption";
  EXPECT_FALSE(r.Lost(t0 + 10 * kMs)) << "CAPTURE_INTERRUPTED goes out once per interruption";
  EXPECT_TRUE(r.interrupted());
  EXPECT_EQ(r.NextRetryNs(), t0 + 250 * kMs);
  // The secure desktop is still up: the output is there, the duplication isn't.
  EXPECT_EQ(r.Attempt(t0 + 250 * kMs, true, false), DuplicationRecovery::Outcome::kWaiting);
  EXPECT_EQ(r.NextRetryNs(), t0 + 500 * kMs);
  EXPECT_EQ(r.Attempt(t0 + 500 * kMs, true, true), DuplicationRecovery::Outcome::kResumed);
  EXPECT_FALSE(r.interrupted());
  EXPECT_EQ(r.NextRetryNs(), kDdNever);
  EXPECT_TRUE(r.Lost(t0 + 900 * kMs)) << "a later loss is a new interruption";
}

TEST(DdPolicy, AnOutputMissingFor5sIsGone) {
  DuplicationRecovery r;
  const int64_t t0 = 0;
  r.Lost(t0);
  int64_t t = t0 + kDdRetryNs;
  // Missing from EnumOutputs from the first attempt on: gone once 5 s passed since that attempt.
  const int64_t first_missing = t;
  int attempts = 0;
  DuplicationRecovery::Outcome o = DuplicationRecovery::Outcome::kWaiting;
  for (; attempts < 100 && o == DuplicationRecovery::Outcome::kWaiting; ++attempts, t += kDdRetryNs) {
    o = r.Attempt(t, false, false);
    if (o == DuplicationRecovery::Outcome::kWaiting) {
      EXPECT_LT(t - first_missing, kDdGoneNs);
    }
  }
  EXPECT_EQ(o, DuplicationRecovery::Outcome::kGone);
  EXPECT_EQ(t - kDdRetryNs - first_missing, kDdGoneNs);

  // An output that comes back resets the 5 s.
  DuplicationRecovery back;
  back.Lost(0);
  EXPECT_EQ(back.Attempt(250 * kMs, false, false), DuplicationRecovery::Outcome::kWaiting);
  EXPECT_EQ(back.Attempt(4'000 * kMs, true, false), DuplicationRecovery::Outcome::kWaiting);
  EXPECT_EQ(back.Attempt(5'500 * kMs, false, false), DuplicationRecovery::Outcome::kWaiting)
      << "missing again since 5.5 s, not since 0.25 s";
  EXPECT_EQ(back.Attempt(10'250 * kMs, false, false), DuplicationRecovery::Outcome::kWaiting);
  EXPECT_EQ(back.Attempt(10'500 * kMs, false, false), DuplicationRecovery::Outcome::kGone);
}

TEST(DdPolicy, PointerFramesAndTimeouts) {
  EXPECT_EQ(PointerSpacingNs(60), 8'333'333);
  EXPECT_EQ(PointerSpacingNs(30), 16'666'666);
  EXPECT_EQ(PointerSpacingNs(0), PointerSpacingNs(60));
  constexpr int64_t kNone = std::numeric_limits<int64_t>::min();
  EXPECT_EQ(PointerDueNs(kNone, 60), kNone) << "nothing published yet: at once";
  EXPECT_EQ(PointerDueNs(100 * kMs, 60), 100 * kMs + 8'333'333);

  EXPECT_EQ(AcquireTimeoutMs(0, kDdNever), kDdMaxWaitMs);
  EXPECT_EQ(AcquireTimeoutMs(0, 500 * kMs), kDdMaxWaitMs) << "never more than 100 ms (Stop is seen within that)";
  EXPECT_EQ(AcquireTimeoutMs(0, 8 * kMs + 1), 9u) << "rounded up: never wake before the deadline";
  EXPECT_EQ(AcquireTimeoutMs(0, 8 * kMs), 8u);
  EXPECT_EQ(AcquireTimeoutMs(10 * kMs, 10 * kMs), 0u);
  EXPECT_EQ(AcquireTimeoutMs(10 * kMs, 5 * kMs), 0u);

  EXPECT_EQ(MonotonicTs(kNone, 5), 5);
  EXPECT_EQ(MonotonicTs(10, 20), 20);
  EXPECT_EQ(MonotonicTs(20, 20), 21) << "a pointer frame at the same QPC tick as the last frame";
  EXPECT_EQ(MonotonicTs(20, 3), 21);
}

} // namespace
} // namespace im::capture
