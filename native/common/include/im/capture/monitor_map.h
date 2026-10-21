// Windows 10: mapping a picked monitor to a DXGI output (docs/m2/04-windows-engine.md §7.5). The picker returns a
// GraphicsCaptureItem with no HMONITOR; to capture it borderless with Desktop Duplication the engine finds its output:
//   1. enumerate the outputs (native/windows: EnumAdapters1 -> EnumOutputs -> DXGI_OUTPUT_DESC, plus the
//      DISPLAYCONFIG_TARGET_DEVICE_NAME friendly name);
//   2. candidates: outputs whose desktop size equals item.Size();
//   3. one candidate: done. Several: keep those whose DeviceName or friendly name equals item.DisplayName(); one
//      left: done;
//   4. (not built in M2: a frame match for identical monitors, §16; MapResult::kAmbiguous is where it would start);
//   5. otherwise no mapping: capture with WGC (border shown) and log at info.
// This is steps 2, 3 and 5 over plain data; native/windows does step 1 and the capture.
#pragma once

#include <cstdint>
#include <span>
#include <string_view>

namespace im::capture {

struct OutputDesc {
  std::string_view device_name;   // DXGI_OUTPUT_DESC.DeviceName in UTF-8 ("\\.\DISPLAY1")
  std::string_view friendly_name; // the monitor's friendly name in UTF-8 ("" when unknown)
  int32_t left = 0;               // DesktopCoordinates (physical pixels, after rotation)
  int32_t top = 0;
  int32_t right = 0;
  int32_t bottom = 0;
};

struct MapResult {
  enum class Outcome : uint8_t {
    kBySize,      // step 3: the only output of that size
    kByName,      // step 3: the only one of several of that size whose name matched
    kNoSizeMatch, // step 5: no output has the item's size
    kAmbiguous,   // step 5: several outputs of that size and the name didn't single one out (step 4's case)
  };
  Outcome outcome = Outcome::kNoSizeMatch;
  int32_t output = -1;       // index into the outputs, or -1 (capture with WGC)
  uint32_t size_matches = 0; // outputs with the item's size
  uint32_t name_matches = 0; // of those, outputs whose name matched

  bool mapped() const noexcept { return output >= 0; }
};

// Maps a picked monitor (item_width × item_height, item.DisplayName() in UTF-8) to one of outputs. Names compare
// exactly; an empty name never matches.
MapResult MapMonitor(uint32_t item_width, uint32_t item_height, std::string_view item_name,
                     std::span<const OutputDesc> outputs) noexcept;

} // namespace im::capture
