// The monitors as DXGI outputs (docs/m2/04-windows-engine.md §5.6, §7.1, §7.5, §7.9; README WA60): what
// im_list_sources reports, the "mon:" ids, the Windows half of §7.5's mapping of a picked monitor to an output, and
// how a pick is captured on each Windows version.
//
// §7.5's steps:
//   1. enumerate the outputs (outputs_win.h: EnumAdapters1 → EnumOutputs → DXGI_OUTPUT_DESC, with the
//      DISPLAYCONFIG_TARGET_DEVICE_NAME friendly name of each);
//   2.–3. candidates by size, then by name (im/capture/monitor_map.h, native/common's MapMonitor; MatchPickedMonitor
//      runs it over these outputs);
//   4. not built in M2 (a frame match for identical monitors; MapResult::Outcome::kAmbiguous is where it would start);
//   5. otherwise no mapping: WGC with the border, logged at info (PlanPick).
//
// Plain C++ over an abstract output list, so dd_outputs_test runs the rules over fake outputs; the Win32 side is
// outputs_win.cpp. Nothing here logs a monitor name.
#pragma once

#include <cstddef>
#include <cstdint>
#include <optional>
#include <span>
#include <string>
#include <string_view>
#include <vector>

#include "capture/window_match.h"
#include "im/capture/monitor_map.h"
#include "isshoni_media.h"

namespace im::capture {

// The prefix of a listed monitor's id: "mon:" + the output's DeviceName ("mon:\\.\DISPLAY1", 04 §5.6).
inline constexpr std::string_view kMonitorIdPrefix = "mon:";

// The first Windows 11 build (04 §7.1: Windows 11 captures monitors with WGC, Windows 10 with Desktop Duplication).
inline constexpr uint32_t kWindows11Build = 22000;

// DXGI_MODE_ROTATION_* (dxgitype.h), for the portable code.
inline constexpr uint32_t kDxgiRotationUnspecified = 0;
inline constexpr uint32_t kDxgiRotationIdentity = 1;
inline constexpr uint32_t kDxgiRotation90 = 2;
inline constexpr uint32_t kDxgiRotation180 = 3;
inline constexpr uint32_t kDxgiRotation270 = 4;

// One DXGI output attached to the desktop (§7.5 step 1).
struct DisplayOutput {
  std::string device_name;   // DXGI_OUTPUT_DESC.DeviceName in UTF-8 ("\\.\DISPLAY1")
  std::string friendly_name; // DISPLAYCONFIG_TARGET_DEVICE_NAME.monitorFriendlyDeviceName in UTF-8, "" when unknown
  int32_t left = 0;          // DesktopCoordinates: physical pixels in the virtual desktop, already turned upright
  int32_t top = 0;
  int32_t right = 0;
  int32_t bottom = 0;
  uint32_t rotation = kDxgiRotationIdentity; // DXGI_OUTPUT_DESC.Rotation
  uint64_t monitor = 0;                      // the HMONITOR as an integer
  uint64_t adapter_luid = 0;                 // the adapter that owns the output (DD runs only on its device, §7.4)
  uint32_t adapter_index = 0;                // into im_caps.adapters (the probe's order)
  bool primary = false;                      // MONITORINFOF_PRIMARY
  uint32_t dpi = 96;                         // GetDpiForMonitor(MDT_EFFECTIVE_DPI)
  uint32_t refresh_hz = 0;                   // the current mode's refresh rate, 0 when unknown
  bool hdr = false;                          // the output runs in an HDR color space (§7.9)

  // The upright desktop size (DesktopCoordinates' width and height).
  PixelSize size() const noexcept { return PixelSize{right - left, bottom - top}; }
};

// The outputs (step 1). Every call may fail; an empty list says so.
class OutputSystem {
public:
  virtual ~OutputSystem() = default;
  // The outputs attached to the desktop, adapter by adapter in the probe's adapter order, each adapter's in
  // EnumOutputs order.
  virtual std::vector<DisplayOutput> Outputs() = 0;
};

// "mon:" + the output's DeviceName.
std::string MonitorIdOf(const DisplayOutput &output);

// The index of the output whose MonitorIdOf is id, or nullopt (another prefix, an unknown or a detached monitor).
std::optional<std::size_t> FindMonitor(std::span<const DisplayOutput> outputs, std::string_view id) noexcept;

// DXGI_MODE_ROTATION_* → 0, 90, 180 or 270 (unspecified and unknown values: 0).
uint32_t RotationDegrees(uint32_t dxgi_rotation) noexcept;

// The name im_list_sources reports for an output: its friendly name, or its DeviceName when it has none.
std::string_view MonitorName(const DisplayOutput &output) noexcept;

// im_list_sources' document (04 §5.6): {"monitors":[{"id","name","primary","x","y","width","height","dpi",
// "refreshHz","rotation","hdr","adapter"},...],"windows":[]} (Windows lists monitors only).
std::string SourcesJson(std::span<const DisplayOutput> outputs);

// §7.5 steps 2, 3 and 5 for a picked monitor of item_size whose DisplayName() is display_name (UTF-8).
struct MonitorMatch {
  MapResult result;                    // the outcome of MapMonitor
  std::optional<DisplayOutput> output; // the mapped output (result.mapped())
};
MonitorMatch MatchPickedMonitor(std::span<const DisplayOutput> outputs, PixelSize item_size,
                                std::string_view display_name);

// The outcome's name for the log ("size", "name", "no_size_match", "ambiguous").
std::string_view MapOutcomeName(MapResult::Outcome outcome) noexcept;

// How a pick is captured (§7.1, §7.5) and what im_pick_source reports about it.
struct PickPlan {
  uint32_t capture_path = IM_CAPTURE_WGC;
  uint32_t flags = 0;     // IM_SRCF_OS_BORDER, IM_SRCF_SELF, IM_SRCF_MAPPED
  std::string monitor_id; // im_source_info.monitor_id: a monitor pick that mapped to an output, else ""
};

// What the picker learned about a pick.
struct PickFacts {
  uint32_t kind = IM_SOURCE_WINDOW;    // window_match.h's ClassifyPick
  bool windows11 = false;              // build ≥ kWindows11Build
  bool borderless = false;             // BorderlessAllowed() (Windows 11 24H2+)
  bool self = false;                   // IM_SRCF_SELF
  const MonitorMatch *match = nullptr; // monitors: the mapping (null when it wasn't run)
};

// - A window: WGC, IM_SRCF_OS_BORDER unless borderless; IM_SRCF_SELF when it is isshoni's.
// - A monitor on Windows 10 that mapped: Desktop Duplication, IM_SRCF_MAPPED, no border (DD never shows one).
// - A monitor on Windows 10 that didn't (step 5): WGC with the border.
// - A monitor on Windows 11: WGC, border unless borderless; it keeps its output (when mapped) for the DD fallback
//   of §7.1 ("if WGC fails to start on a monitor, DD on the same output") and for monitor_id.
PickPlan PlanPick(const PickFacts &facts);

} // namespace im::capture
