// What a source holds for the capture area (session::SourceSpec::item, docs/m2/04-windows-engine.md §7.2, §7.5): the
// picked GraphicsCaptureItem and what the engine learned about it at pick time, or a listed monitor's output. Every
// pick of this area's picker stores one, with the DXGI output a monitor pick mapped to (README WA60); a listed monitor
// ("mon:…", capture.h's ResolveMonitor) stores its output and no item; the test hooks' fake picks store none.
// GraphicsCaptureItem is agile: the picker's STA creates it, the engine worker (im_start) and WGC's threads use it, and
// whichever thread drops the token releases it.
#pragma once

#include <memory>
#include <optional>

#include "capture/outputs.h"
#include "capture/window_match.h"
#include "capture/winrt_support.h"
#include "session/sources.h"

namespace im::capture {

struct PickedItem {
  wgc::GraphicsCaptureItem item{nullptr}; // null for a listed monitor
  WindowIdentity identity;                // §7.6 (0s when not identified, and for monitors)
  // A monitor's DXGI output (§7.5: what a pick mapped to, or the listed monitor): Desktop Duplication captures it on
  // Windows 10, and on Windows 11 when WGC fails to start on the monitor (§7.1).
  std::optional<DisplayOutput> output;
};

// The PickedItem of a source, or null (the test pattern, a fake pick).
inline std::shared_ptr<PickedItem> PickedItemOf(const session::SourceSpec &spec) noexcept {
  if ((spec.capture_path != IM_CAPTURE_WGC && spec.capture_path != IM_CAPTURE_DD) || spec.test) {
    return nullptr;
  }
  return std::static_pointer_cast<PickedItem>(spec.item);
}

} // namespace im::capture
