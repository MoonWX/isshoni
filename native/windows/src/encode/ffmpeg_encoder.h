// The libavcodec encoders: NVENC, QSV, AMF and Media Foundation, hardware and software (docs/m2/04-windows-engine.md
// §9.1-§9.5; "04" below), behind the session's VideoEncoder (session/encoder.h).
//
// One FfmpegEncoder is one layer's encoder session on the session's device:
// - Hardware device: an AV_HWDEVICE_TYPE_D3D11VA context wrapping the session's ID3D11Device, its context lock as
//   AVD3D11VADeviceContext.lock/unlock (gpu::Device::LockCallback), then av_hwdevice_ctx_init; QSV derives an
//   AV_HWDEVICE_TYPE_QSV device from it. MF software takes none.
// - Zero copy (IM_ENCF_ZERO_COPY) when avcodec_get_hw_config lists AV_PIX_FMT_D3D11 (QSV: AV_PIX_FMT_QSV) with
//   AV_CODEC_HW_CONFIG_METHOD_HW_FRAMES_CTX and the device can render to NV12: an AV_PIX_FMT_D3D11 frame pool
//   (sw_format NV12, the layer's size, 8 frames for layer 0 and 6 for the others, BindFlags RENDER_TARGET plus
//   VIDEO_ENCODER for MF) whose array slices NextTarget hands to the converter; QSV maps each frame into a derived
//   AV_PIX_FMT_QSV pool (av_hwframe_ctx_create_derived, AV_HWFRAME_MAP_DIRECT; av_hwframe_map per frame). Otherwise
//   NextTarget hands out our own gpu::Nv12Surface and Encode reads it back (gpu::Nv12Readback) into an NV12 AVFrame
//   in system memory: MF software always, and a hardware encoder whose build or device doesn't allow zero copy.
// - Settings (04 §9.3): the common AVCodecContext fields (no B-frames, gop_size fps × 3600 since the session forces
//   IDRs itself, CBR at the target with rc_max_rate = target and rc_buffer_size = target × VBV, LOW_DELAY, headers
//   in-band, BT.709 limited range, the profile) and each encoder's private options. A required option (R in §9.3's
//   table) that the build refuses fails the open; any other unknown option is skipped with a debug log.
// - Encode (04 §9.2): avcodec_send_frame, then avcodec_receive_packet until EAGAIN; AVFrame.pts is the capture time in
//   µs since epoch_ns; a forced IDR is pict_type I with the encoder's forced-IDR option on. The access unit is the
//   packets' bytes as the encoder wrote them, with the capture time of the frame they came from (the first packet's
//   pts looked up among the last frames sent: EncodedAu::capture_ts_ns); the session's normalizer (04 §9.4) does the
//   rest.
// - SetBitrate (live encoders, 04 §9.5): bit_rate, rc_max_rate and rc_buffer_size change before the next
//   avcodec_send_frame (FFmpeg reconfigures NVENC and resets QSV). Re-init encoders (AMF, MF) are closed and opened
//   again by the session.
//
// Errors: an open fails with IM_ERR_NO_ENCODER open_failed, IM_ERR_DEPENDENCY driver_too_old (NVENC's API version
// is newer than the driver), IM_ERR_LIMIT session_limit (NVENC refused another session), or IM_ERR_ENCODER_LOST
// device_removed (the device is gone). An encode error is IM_ERR_INTERNAL, or IM_ERR_ENCODER_LOST device_removed when
// the device reports removal (04 §9.8).
//
// Threads: Create on the engine worker or the video thread; NextTarget, Encode and SetBitrate on the video thread,
// without the context lock held (FFmpeg and the readback take it themselves).
#pragma once

#include <cstddef>
#include <cstdint>
#include <memory>
#include <vector>

#include "encode/ffmpeg_api.h"
#include "gpu/device.h"
#include "gpu/frame.h"
#include "gpu/nv12.h"
#include "isshoni_media.h"
#include "session/encoder.h"

namespace im::encode {

// What an open learned, for the probe (04 §9.10).
struct OpenFacts {
  bool zero_copy = false;
  uint32_t open_us = 0; // avcodec_open2's wall time
};

class FfmpegEncoder final : public session::VideoEncoder {
public:
  // The session's OpenEncoderFn for the FFmpeg kinds (IM_ENC_NVENC, _QSV, _AMF, _MF_HW, _MF_SW). FFmpeg must load
  // (ffmpeg::Load with ffmpeg::DefaultDir()); params.device must be set.
  static im_status Open(const session::EncoderParams &params, std::unique_ptr<session::VideoEncoder> *out) noexcept;
  // The same, typed, for the probe; facts (optional) says how it opened.
  static im_status Create(const session::EncoderParams &params, std::unique_ptr<FfmpegEncoder> *out,
                          OpenFacts *facts) noexcept;

  // Whether the build's encoder of this kind takes D3D11 (QSV: QSV) frames from a frame pool (04 §9.2's zero-copy
  // decision, before the device's own NV12 capability is known). False when FFmpeg isn't loaded or lacks the encoder.
  static bool BuildListsZeroCopy(uint32_t kind) noexcept;
  // Whether the loaded FFmpeg has the kind's encoder at all (avcodec_find_encoder_by_name).
  static bool BuildHasEncoder(uint32_t kind) noexcept;

  ~FfmpegEncoder() override;
  FfmpegEncoder(const FfmpegEncoder &) = delete;
  FfmpegEncoder &operator=(const FfmpegEncoder &) = delete;

  im_status NextTarget(gpu::Nv12Target *out) noexcept override;
  im_status Encode(const session::EncodeInput &in, session::EncodedAu *out) noexcept override;
  im_status SetBitrate(uint32_t bps) noexcept override;

  bool zero_copy() const noexcept { return zero_copy_; }

  // For the probe (04 §9.10; encpolicy::TestEncodePlan.drain): end of stream (avcodec_send_frame(NULL)), then every
  // packet the encoder still held, each appended to aus as one AU, until AVERROR_EOF. The encoder takes no frame
  // after it.
  im_status Drain(std::vector<std::vector<uint8_t>> *aus) noexcept;

  // 04 §9.2's pts → capture-time table: the capture time (EncodeInput.ts_ns) of the frame the last Encode's AU came
  // from, found by the first packet's pts among the last kPtsRing frames sent; 0 when that Encode produced no AU.
  // An encoder with frames of delay emits an AU for an earlier frame than the one just sent: Encode also returns this
  // value as EncodedAu::capture_ts_ns, and the session stamps the packet with it (session/au_times.h).
  int64_t au_capture_ts_ns() const noexcept { return au_ts_ns_; }

  // The probe's test picture (04 §9.10): mid-gray (Y 128, Cb/Cr 128) written into target, which NextTarget handed
  // out. Takes the context lock itself.
  im_status FillGray(const gpu::Nv12Target &target) noexcept;

private:
  explicit FfmpegEncoder(const session::EncoderParams &params) noexcept;
  im_status OpenCodec(OpenFacts *facts) noexcept;
  im_status SetupDevice() noexcept;
  im_status SetupFrames() noexcept;
  im_status ApplyOptions() noexcept;
  // The status for a failed FFmpeg call `what` with AVERROR err (device loss → IM_ERR_ENCODER_LOST device_removed).
  im_status EncodeFail(int err, const char *what) noexcept;
  void Release() noexcept;

  session::EncoderParams params_;
  gpu::Device *device_ = nullptr;
  bool zero_copy_ = false;
  bool qsv_ = false;
  uint32_t bitrate_ = 0;
  int64_t last_pts_ = INT64_MIN; // the last AVFrame.pts sent (they must increase)
  double vbv_s_ = 1.0;
  // The pts → capture-time ring (au_capture_ts_ns).
  static constexpr std::size_t kPtsRing = 8;
  struct PtsTs {
    int64_t pts = INT64_MIN;
    int64_t ts_ns = 0;
  };
  PtsTs pts_ring_[kPtsRing];
  std::size_t pts_next_ = 0;
  int64_t au_ts_ns_ = 0;
#if IM_ENCODE_FFMPEG
  const ffmpeg::Api *api_ = nullptr;
  const AVCodec *codec_ = nullptr;
  AVCodecContext *ctx_ = nullptr;
  AVBufferRef *device_ref_ = nullptr;     // D3D11VA
  AVBufferRef *qsv_device_ref_ = nullptr; // QSV, derived
  AVBufferRef *frames_ref_ = nullptr;     // D3D11 pool (zero copy)
  AVBufferRef *qsv_frames_ref_ = nullptr; // QSV pool derived from it
  AVFrame *pending_ = nullptr;            // zero copy: the pool frame NextTarget handed out
  AVFrame *mapped_ = nullptr;             // QSV: pending_ mapped
  AVFrame *sw_frame_ = nullptr;           // CPU path: the NV12 frame in system memory
  AVPacket *pkt_ = nullptr;
#endif
  std::unique_ptr<gpu::Nv12Surface> surface_;   // CPU path
  std::unique_ptr<gpu::Nv12Readback> readback_; // CPU path
  gpu::ComPtr<ID3D11Texture2D> gray_;           // FillGray's source for a zero-copy target
  bool gray_next_ = false;                      // CPU path: the next Encode sends a mid-gray frame
  std::vector<uint8_t> au_;
};

} // namespace im::encode
