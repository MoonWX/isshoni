// The skeleton's capture area (README WA11): not implemented yet. README WA50 (the picker, WGC) and WA60 (Desktop
// Duplication, im_list_sources) replace it.
#include "capture/capture.h"

#include "im/abi/status.h"

namespace im::capture {

im_status ListSources(uint32_t, std::string &) {
  return abi::Fail(IM_ERR_NOT_IMPLEMENTED, {}, "im_list_sources: not implemented yet (README WA60)");
}

im_status PickSource(const im_pick_params &, im_source_info &) {
  return abi::Fail(IM_ERR_NOT_IMPLEMENTED, {}, "im_pick_source: not implemented yet (README WA50)");
}

im_status CancelPick() {
  return abi::Fail(IM_ERR_NOT_IMPLEMENTED, {}, "im_cancel_pick: not implemented yet (README WA50)");
}

void ReleaseSource(std::string_view) noexcept {}

void Shutdown() noexcept {}

} // namespace im::capture
