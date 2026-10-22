// The skeleton's encode area (README WA11): no probe, no encoder, nothing loaded. README WA44 (libavcodec, the probe)
// and WA51 (OpenH264) replace it.
#include "encode/encode.h"

#include "im/abi/status.h"

namespace im::encode {

im_status Probe(uint32_t, im_caps &) {
  return abi::Fail(IM_ERR_NOT_IMPLEMENTED, {}, "im_probe: not implemented yet (README WA44)");
}

im_status SetOpenH264Path(const std::optional<std::string> &) {
  return abi::Fail(IM_ERR_NOT_IMPLEMENTED, {}, "im_set_openh264_path: not implemented yet (README WA51)");
}

FfmpegInfo GetFfmpegInfo() { return {}; }

OpenH264Info GetOpenH264Info() { return {}; }

} // namespace im::encode
