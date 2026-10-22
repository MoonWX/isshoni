#include "im/encpolicy/encoders.h"

#include <array>

namespace im::encpolicy {

namespace {

// kind, token, codec, hardware, vendor, live bitrate, High, CB (04 §5.6, §9.1)
constexpr std::array<EncoderTraits, 10> kTable{{
    {IM_ENC_AUTO, "auto", "", false, false, false, false, false},
    {IM_ENC_NVENC, "nvenc", "h264_nvenc", true, true, true, true, true},
    {IM_ENC_QSV, "qsv", "h264_qsv", true, true, true, true, true},
    {IM_ENC_AMF, "amf", "h264_amf", true, true, false, true, true},
    {IM_ENC_MF_HW, "mf-hw", "h264_mf", true, false, false, true, true},
    {IM_ENC_MF_SW, "mf-sw", "h264_mf", false, false, false, true, true},
    {IM_ENC_OPENH264, "openh264", "openh264", false, false, true, false, true},
    {IM_ENC_VIDEOTOOLBOX, "videotoolbox", "videotoolbox", true, true, true, true, true},
    {IM_ENC_GSTREAMER, "gstreamer", "gstreamer", false, false, false, true, true},
    {IM_ENC_FAKE, "fake", "fake", false, false, true, true, true},
}};

constexpr EncoderTraits kUnknown{kUnknownKind, "", "", false, false, false, false, false};

} // namespace

const EncoderTraits &Traits(uint32_t kind) noexcept {
  for (const auto &t : kTable) {
    if (t.kind == kind) {
      return t;
    }
  }
  return kUnknown;
}

std::string_view EncoderToken(uint32_t kind) noexcept { return Traits(kind).token; }

std::string_view EncoderCodec(uint32_t kind) noexcept { return Traits(kind).codec; }

uint32_t EncoderKindFromToken(std::string_view token) noexcept {
  if (token.empty()) {
    return kUnknownKind;
  }
  for (const auto &t : kTable) {
    if (t.token == token) {
      return t.kind;
    }
  }
  return kUnknownKind;
}

} // namespace im::encpolicy
