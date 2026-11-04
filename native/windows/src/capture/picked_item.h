// What a pick token holds for the capture area (session::SourceSpec::item, docs/m2/04-windows-engine.md §7.2): the
// picked GraphicsCaptureItem and what the engine learned about it at pick time. Every pick of this area's picker
// stores one (README WA60's monitor mapping adds its output here); the test hooks' fake picks store none.
// GraphicsCaptureItem is agile: the picker's STA creates it, the engine worker (im_start) and WGC's threads use it, and
// whichever thread drops the token releases it.
#pragma once

#include <memory>

#include "capture/window_match.h"
#include "capture/winrt_support.h"
#include "session/sources.h"

namespace im::capture {

struct PickedItem {
  wgc::GraphicsCaptureItem item{nullptr};
  WindowIdentity identity; // §7.6 (0s when not identified, and for monitors)
};

// The PickedItem of a source, or null (a listed source, a fake pick).
inline std::shared_ptr<PickedItem> PickedItemOf(const session::SourceSpec &spec) noexcept {
  if (spec.capture_path != IM_CAPTURE_WGC || spec.test) {
    return nullptr;
  }
  return std::static_pointer_cast<PickedItem>(spec.item);
}

} // namespace im::capture
