// The skeleton's audio test exports (README WA11): not implemented yet. 05 replaces this file (README WA53).
#include "audio/test_exports.h"

#include "im/abi/status.h"

namespace im::audio {

im_status TestRenderTone(const char *, float, float, int32_t) {
  return abi::Fail(IM_ERR_NOT_IMPLEMENTED, {}, "im_test_render_tone: not implemented yet (README WA53)");
}

int32_t TestRenderBursts(const char *, float, float, int32_t, int32_t, int32_t, int64_t *, int32_t) {
  return abi::Fail(IM_ERR_NOT_IMPLEMENTED, {}, "im_test_render_bursts: not implemented yet (README WA53)");
}

} // namespace im::audio
