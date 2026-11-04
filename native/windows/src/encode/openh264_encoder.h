// The OpenH264 encoder behind the session's VideoEncoder (docs/m2/04-windows-engine.md §9.1, §9.3, §9.5, §9.9, §9.10;
// "04" below; README WA51), registered through the selector's table (selector.cpp: IM_ENC_OPENH264 → OpenOpenH264).
//
// - Input (04 §9.1): I420 in system memory. The session converts each frame into the encoder's own gpu::Nv12Surface
//   (NextTarget), Encode reads it back (gpu::Nv12Readback) and splits the interleaved chroma on the CPU (about 1 ms at
//   1080p, 04 §9.9), then EncodeFrame. The catalog entry takes pixels on the default adapter (a software encoder).
// - Settings (04 §9.3, openh264_support.h): screen content in real time, bitrate rate control, no periodic IDR,
//   CAVLC; Constrained Baseline only, so a High session never opens it (encpolicy makes the whole session CB when
//   OpenH264 is the only encoder for some layer, 04 §9.6), and Open refuses High. Capped at 1080p30 (04 §9.1).
// - IDRs: ForceIntraFrame(true) before the frame (04 §9.4); the first frame after an open is an IDR. SPS and PPS come
//   with every IDR (the normalizer inserts them from its cache anyway, 04 §9.4).
// - Live bitrate (04 §9.5): SetOption(ENCODER_OPTION_BITRATE) and ENCODER_OPTION_MAX_BITRATE (target = max), before
//   the next frame, no IDR; the probe checks the "no IDR" (04 §9.10's live-bitrate check).
// - No frame delay: each AU is the frame just given (EncodedAu::capture_ts_ns 0). A skipped frame (OpenH264's rate
//   control, bEnableFrameSkip) gives an empty AU.
// - OpenH264's own log at warning and above goes into the engine's log ring (component "openh264").
//
// Threads: Open on the engine worker or the video thread; NextTarget, Encode and SetBitrate on the video thread.
#pragma once

#include <cstddef>
#include <cstdint>
#include <memory>
#include <span>
#include <vector>

#include "encode/openh264_api.h"
#include "gpu/device.h"
#include "gpu/frame.h"
#include "gpu/nv12.h"
#include "isshoni_media.h"
#include "session/encoder.h"

namespace im::encode {

// One OpenH264 encoder session without a device: I420 in, one Annex B access unit out. OpenH264Encoder wraps it for
// sessions; the probe's test encode (04 §9.10) and openh264_test use it directly.
class OpenH264Core {
public:
  // Creates and initializes the encoder (WelsCreateSVCEncoder, GetDefaultParams, openh264_support.h's settings,
  // InitializeExt). IM_ERR_NO_ENCODER open_failed when OpenH264 refuses the settings.
  static im_status Create(std::shared_ptr<const openh264::Library> lib, const openh264::Settings &settings,
                          std::unique_ptr<OpenH264Core> *out) noexcept;

  ~OpenH264Core();
  OpenH264Core(const OpenH264Core &) = delete;
  OpenH264Core &operator=(const OpenH264Core &) = delete;

  // One I420 picture of the settings' size: y rows y_stride apart, u and v rows c_stride apart. ts_ms increases from
  // frame to frame (Encode makes it so). *au stays valid until the next call; empty for a skipped frame. *idr is
  // OpenH264's own key flag. IM_ERR_INTERNAL when EncodeFrame fails.
  im_status Encode(const uint8_t *y, std::size_t y_stride, const uint8_t *u, const uint8_t *v, std::size_t c_stride,
                   int64_t ts_ms, bool force_idr, std::span<const uint8_t> *au, bool *idr) noexcept;
  // A live change of the target (= maximum) bitrate, applied before the next frame (04 §9.5).
  im_status SetBitrate(uint32_t bps) noexcept;

  const openh264::Settings &settings() const noexcept { return settings_; }

private:
  OpenH264Core(std::shared_ptr<const openh264::Library> lib, const openh264::Settings &settings) noexcept;

  std::shared_ptr<const openh264::Library> lib_; // outlives encoder_: the DLL is unloaded only after it
  ISVCEncoder *encoder_ = nullptr;
  openh264::Settings settings_;
  int64_t last_ts_ms_ = -1;
  std::unique_ptr<SFrameBSInfo> info_; // about 4 KB: not on the video thread's stack
  std::vector<uint8_t> au_;
};

class OpenH264Encoder final : public session::VideoEncoder {
public:
  // The session's OpenEncoderFn for IM_ENC_OPENH264 (OpenOpenH264): OpenH264 must be turned on (Acquire), the
  // session profile Constrained Baseline, the size within 1080p; params.device must be set.
  static im_status Open(const session::EncoderParams &params, std::unique_ptr<session::VideoEncoder> *out) noexcept;

  ~OpenH264Encoder() override = default;
  OpenH264Encoder(const OpenH264Encoder &) = delete;
  OpenH264Encoder &operator=(const OpenH264Encoder &) = delete;

  im_status NextTarget(gpu::Nv12Target *out) noexcept override;
  im_status Encode(const session::EncodeInput &in, session::EncodedAu *out) noexcept override;
  im_status SetBitrate(uint32_t bps) noexcept override;

private:
  explicit OpenH264Encoder(const session::EncoderParams &params) noexcept : params_(params) {}

  session::EncoderParams params_;
  std::unique_ptr<OpenH264Core> core_;
  std::unique_ptr<gpu::Nv12Surface> surface_;
  std::unique_ptr<gpu::Nv12Readback> readback_;
  std::vector<uint8_t> y_;  // width × height
  std::vector<uint8_t> uv_; // NV12's interleaved chroma, width × height/2
  std::vector<uint8_t> u_;  // width/2 × height/2
  std::vector<uint8_t> v_;
};

} // namespace im::encode
