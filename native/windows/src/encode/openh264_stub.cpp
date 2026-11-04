// OpenH264 until README WA51 (docs/m2/04-windows-engine.md §9.9): never loaded, so the probe lists it as off
// (IM_ERR_DEPENDENCY "disabled") and no session can open it. README WA51 replaces this file with the runtime-loaded
// backend (its probe entry, its open function, im_set_openh264_path and im_build_info's "openh264").
#include "encode/encode.h"
#include "encode/selector.h"
#include "im/abi/status.h"
#include "im/abi/utf8.h"

namespace im::encode {

im_status SetOpenH264Path(const std::optional<std::string> &) {
  return abi::Fail(IM_ERR_NOT_IMPLEMENTED, {}, "im_set_openh264_path: not implemented yet (README WA51)");
}

OpenH264Info GetOpenH264Info() { return {}; }

void ProbeOpenH264(bool, im_encoder_info &info) noexcept {
  info.status = IM_ERR_DEPENDENCY;
  info.flags &= ~IM_ENCF_AVAILABLE;
  abi::CopyFixed(info.reason, abi::token::kDisabled);
}

im_status OpenOpenH264(const session::EncoderParams &, std::unique_ptr<session::VideoEncoder> *) noexcept {
  return abi::Fail(IM_ERR_DEPENDENCY, abi::token::kDisabled, "encode: OpenH264 is not turned on (README WA51)");
}

} // namespace im::encode
