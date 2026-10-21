// The encode area's entry points behind the exports (docs/m2/04-windows-engine.md §9; README WA44 the probe and
// libavcodec, WA51 OpenH264). encode_stub.cpp is the skeleton's version, replaced by those slices. Arguments arrive
// checked (src/api/validate.h); failures set the thread's detail (im::abi::Fail).
#pragma once

#include <cstdint>
#include <optional>
#include <string>

#include "isshoni_media.h"

namespace im::encode {

// im_probe, on the engine worker (bound::kProbeQuick or kProbeFull): fills caps (zeroed, struct_size =
// sizeof(im_caps)), the audio flags from 05's AudioEngine::ProbeFlags included (04 §6.1, §9.10).
im_status Probe(uint32_t flags, im_caps &caps);

// im_set_openh264_path, on the engine worker (bound::kSetOpenH264Path): an absolute path to check and use from the
// next probe and start (04 §9.9), or nothing to turn OpenH264 off.
im_status SetOpenH264Path(const std::optional<std::string> &path);

// What im_build_info reports about FFmpeg (04 §5.6): loaded, and then the libraries' versions and license.
struct FfmpegInfo {
  bool loaded = false;
  std::string avcodec; // "62.3.100"; "" when not loaded
  std::string avutil;
  std::string license; // avcodec_license(); "" when not loaded
};
FfmpegInfo GetFfmpegInfo();

// What im_build_info reports about OpenH264.
struct OpenH264Info {
  bool loaded = false;
  std::string version; // "2.6.0"; "" when not loaded
};
OpenH264Info GetOpenH264Info();

} // namespace im::encode
