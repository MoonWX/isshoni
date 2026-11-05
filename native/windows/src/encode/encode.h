// The encode area's entry points behind the exports (docs/m2/04-windows-engine.md §9). Arguments arrive checked
// (src/api/validate.h); failures set the thread's detail (im::abi::Fail).
//
// The area (README WA44; OpenH264 README WA51):
//   ffmpeg_api      libavutil and libavcodec loaded at run time, the version check, FFmpeg's log (§9.2)
//   ffmpeg_encoder  NVENC, QSV, AMF and Media Foundation through libavcodec behind session::VideoEncoder (§9.1-§9.5)
//   selector        the kind → open function table and the probe's encoders as the sessions' catalog entries
//   probe           im_probe (§9.10), with the live-bitrate check
//   openh264_*      Cisco's OpenH264, loaded at run time once the user turned it on (README WA51, 04 §9.9):
//                   openh264_api (the DLL and the setting), openh264_encoder (the encoder and its probe entry),
//                   openh264_support (portable settings and helpers), openh264_pin (the pinned release)
#pragma once

#include <cstdint>
#include <optional>
#include <string>

#include "isshoni_media.h"

namespace im::encode {

// im_probe, on the engine worker (bound::kProbeQuick or kProbeFull): fills caps (zeroed, struct_size =
// sizeof(im_caps)), the audio flags from 05's AudioEngine::ProbeFlags included (04 §6.1, §9.10). A full probe's
// encoders become the sessions' catalog (selector.h). Missing or refused FFmpeg DLLs never fail the probe: the
// encoders that need them carry IM_ERR_DEPENDENCY and the token in their entry.
im_status Probe(uint32_t flags, im_caps &caps);

// im_set_openh264_path, on the engine worker (bound::kSetOpenH264Path): an absolute path to check and use from the
// next probe and start (04 §9.9), or nothing to turn OpenH264 off.
im_status SetOpenH264Path(const std::optional<std::string> &path);

// im_shutdown, after the sessions stopped and the worker exited: forgets im_set_openh264_path's setting and drops the
// loaded OpenH264 library (unloaded now, no encoder holds it any more), so each im_init starts from its own
// openh264_path and 03 can delete the file after an im_shutdown (README WA51).
void ForgetOpenH264Setting() noexcept;

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
  std::string version; // "2.7.0"; "" when not loaded
};
OpenH264Info GetOpenH264Info();

} // namespace im::encode
