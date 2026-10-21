// The skeleton's Opus side of the audio bridge (README WA11): libopus isn't linked into the DLL yet. README WA34
// replaces this file once the Opus stage (native/common/audio, README WA13) is wired to sessions.
#include "audio_bridge/audio_bridge.h"

#include "im/abi/status.h"

namespace im::audio_bridge {

int32_t TestOpusDecode(const uint8_t *, int32_t, float *, int32_t) {
  return abi::Fail(IM_ERR_NOT_IMPLEMENTED, {}, "im_test_opus_decode: not implemented yet (README WA34)");
}

std::string_view OpusVersion() noexcept { return {}; }

} // namespace im::audio_bridge
