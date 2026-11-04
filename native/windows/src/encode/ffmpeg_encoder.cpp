#include "encode/ffmpeg_encoder.h"

#include <windows.h>

#include <d3d11.h>

#include <algorithm>
#include <cerrno>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <iterator>
#include <new>
#include <string>
#include <utility>

#include "engine/os.h"
#include "im/abi/status.h"
#include "im/encpolicy/encoders.h"
#include "im/log/ring.h"

#if IM_ENCODE_FFMPEG
extern "C" {
#include <libavutil/hwcontext_d3d11va.h>
}
#endif

namespace im::encode {
namespace {

#if IM_ENCODE_FFMPEG

bool IsFfmpegKind(uint32_t kind) noexcept {
  return kind == IM_ENC_NVENC || kind == IM_ENC_QSV || kind == IM_ENC_AMF || kind == IM_ENC_MF_HW ||
         kind == IM_ENC_MF_SW;
}

const AVCodec *FindCodec(const ffmpeg::Api *api, uint32_t kind) noexcept {
  if (api == nullptr || !IsFfmpegKind(kind)) {
    return nullptr;
  }
  const std::string name(encpolicy::EncoderCodec(kind));
  return api->avcodec_find_encoder_by_name(name.c_str());
}

bool ListsHwFrames(const ffmpeg::Api *api, const AVCodec *codec, AVPixelFormat fmt) noexcept {
  for (int i = 0;; ++i) {
    const AVCodecHWConfig *h = api->avcodec_get_hw_config(codec, i);
    if (h == nullptr) {
      return false;
    }
    if (h->pix_fmt == fmt && (h->methods & AV_CODEC_HW_CONFIG_METHOD_HW_FRAMES_CTX) != 0) {
      return true;
    }
  }
}

AVPixelFormat HwFormatOf(uint32_t kind) noexcept { return kind == IM_ENC_QSV ? AV_PIX_FMT_QSV : AV_PIX_FMT_D3D11; }

// One private option of 04 §9.3's table.
struct Option {
  const char *name;
  const char *value;
  bool required; // R: the open fails without it
};

#endif // IM_ENCODE_FFMPEG

} // namespace

FfmpegEncoder::FfmpegEncoder(const session::EncoderParams &params) noexcept : params_(params) {}

FfmpegEncoder::~FfmpegEncoder() { Release(); }

im_status FfmpegEncoder::Open(const session::EncoderParams &params,
                              std::unique_ptr<session::VideoEncoder> *out) noexcept {
  std::unique_ptr<FfmpegEncoder> enc;
  const im_status st = Create(params, &enc, nullptr);
  if (st == IM_OK) {
    *out = std::move(enc);
  }
  return st;
}

#if IM_ENCODE_FFMPEG

bool FfmpegEncoder::BuildHasEncoder(uint32_t kind) noexcept { return FindCodec(ffmpeg::Loaded(), kind) != nullptr; }

bool FfmpegEncoder::BuildListsZeroCopy(uint32_t kind) noexcept {
  const ffmpeg::Api *api = ffmpeg::Loaded();
  const AVCodec *codec = FindCodec(api, kind);
  if (codec == nullptr || kind == IM_ENC_MF_SW) {
    return false; // MF software takes NV12 in system memory (04 §9.1)
  }
  return ListsHwFrames(api, codec, HwFormatOf(kind));
}

im_status FfmpegEncoder::Create(const session::EncoderParams &params, std::unique_ptr<FfmpegEncoder> *out,
                                OpenFacts *facts) noexcept {
  if (!IsFfmpegKind(params.kind)) {
    return abi::Failf(IM_ERR_NO_ENCODER, abi::token::kOpenFailed, "encode: kind %u is not a libavcodec encoder",
                      static_cast<unsigned>(params.kind));
  }
  if (params.device == nullptr) {
    return abi::Fail(IM_ERR_NO_ENCODER, abi::token::kOpenFailed, "encode: a libavcodec encoder needs the device");
  }
  if (params.width == 0 || params.height == 0 || params.fps == 0 || params.bitrate_bps == 0) {
    return abi::Failf(IM_ERR_INVALID_ARG, {}, "encode: %ux%u at %u fps and %u bit/s", params.width, params.height,
                      params.fps, params.bitrate_bps);
  }
  ffmpeg::LoadResult why;
  const ffmpeg::Api *api = ffmpeg::Load(ffmpeg::DefaultDir(), &why);
  if (api == nullptr) {
    return abi::Fail(why.status, why.token, why.detail);
  }
  std::unique_ptr<FfmpegEncoder> enc(new (std::nothrow) FfmpegEncoder(params));
  if (!enc) {
    return abi::Fail(IM_ERR_INTERNAL, {}, "encode: out of memory");
  }
  enc->api_ = api;
  enc->device_ = params.device;
  enc->codec_ = FindCodec(api, params.kind);
  if (enc->codec_ == nullptr) {
    return abi::Failf(IM_ERR_NO_ENCODER, abi::token::kOpenFailed, "encode: the FFmpeg build has no %s",
                      std::string(encpolicy::EncoderCodec(params.kind)).c_str());
  }
  enc->qsv_ = params.kind == IM_ENC_QSV;
  enc->bitrate_ = params.bitrate_bps;
  // 04 §9.3: layer 0's VBV is 0.5 s (1.0 s for DETAIL), other layers' 1.0 s.
  enc->vbv_s_ = (params.layer == 0 && params.content_hint != IM_CONTENT_DETAIL) ? 0.5 : 1.0;
  enc->zero_copy_ = params.kind != IM_ENC_MF_SW && ListsHwFrames(api, enc->codec_, HwFormatOf(params.kind)) &&
                    params.device->nv12_render_target();
  if (const im_status st = enc->OpenCodec(facts); st != IM_OK) {
    return st;
  }
  *out = std::move(enc);
  return IM_OK;
}

im_status FfmpegEncoder::SetupDevice() noexcept {
  if (params_.kind == IM_ENC_MF_SW) {
    return IM_OK; // Microsoft's software MFT takes system memory only
  }
  device_ref_ = api_->av_hwdevice_ctx_alloc(AV_HWDEVICE_TYPE_D3D11VA);
  if (device_ref_ == nullptr) {
    return abi::Fail(IM_ERR_INTERNAL, {}, "encode: av_hwdevice_ctx_alloc(D3D11VA) failed");
  }
  auto *hw = reinterpret_cast<AVHWDeviceContext *>(device_ref_->data);
  auto *d3d = static_cast<AVD3D11VADeviceContext *>(hw->hwctx);
  // FFmpeg releases the device when the context goes away: it gets its own reference. The immediate context and the
  // video interfaces it takes from the device itself; every use of them goes through our context lock (04 §8.1).
  device_->d3d()->AddRef();
  d3d->device = device_->d3d();
  d3d->lock = &gpu::Device::LockCallback;
  d3d->unlock = &gpu::Device::UnlockCallback;
  d3d->lock_ctx = device_->lock_ctx();
  if (const int r = api_->av_hwdevice_ctx_init(device_ref_); r < 0) {
    return EncodeFail(r, "av_hwdevice_ctx_init(D3D11VA)");
  }
  if (qsv_) {
    if (const int r = api_->av_hwdevice_ctx_create_derived(&qsv_device_ref_, AV_HWDEVICE_TYPE_QSV, device_ref_, 0);
        r < 0) {
      return abi::Failf(IM_ERR_NO_ENCODER, abi::token::kOpenFailed, "encode: deriving the QSV device failed: %s",
                        api_->ErrorText(r).c_str());
    }
  }
  return IM_OK;
}

im_status FfmpegEncoder::SetupFrames() noexcept {
  if (!zero_copy_) {
    // Our own NV12 surface and its readback into an NV12 frame in system memory (04 §8.3's last point).
    const video::Size size{params_.width, params_.height};
    if (const im_status st = gpu::Nv12Surface::Create(*device_, size, &surface_); st != IM_OK) {
      return st;
    }
    if (const im_status st = gpu::Nv12Readback::Create(*device_, size, surface_->planar(), &readback_); st != IM_OK) {
      return st;
    }
    sw_frame_ = api_->av_frame_alloc();
    if (sw_frame_ == nullptr) {
      return abi::Fail(IM_ERR_INTERNAL, {}, "encode: av_frame_alloc failed");
    }
    sw_frame_->format = AV_PIX_FMT_NV12;
    sw_frame_->width = static_cast<int>(params_.width);
    sw_frame_->height = static_cast<int>(params_.height);
    if (const int r = api_->av_frame_get_buffer(sw_frame_, 0); r < 0) {
      return abi::Failf(IM_ERR_INTERNAL, {}, "encode: av_frame_get_buffer failed: %s", api_->ErrorText(r).c_str());
    }
    return IM_OK;
  }
  frames_ref_ = api_->av_hwframe_ctx_alloc(device_ref_);
  if (frames_ref_ == nullptr) {
    return abi::Fail(IM_ERR_INTERNAL, {}, "encode: av_hwframe_ctx_alloc failed");
  }
  auto *fc = reinterpret_cast<AVHWFramesContext *>(frames_ref_->data);
  fc->format = AV_PIX_FMT_D3D11;
  fc->sw_format = AV_PIX_FMT_NV12;
  fc->width = static_cast<int>(params_.width);
  fc->height = static_cast<int>(params_.height);
  fc->initial_pool_size = params_.layer == 0 ? 8 : 6; // 04 §9.2: 8 for f, 6 for q
  auto *d3dfc = static_cast<AVD3D11VAFramesContext *>(fc->hwctx);
  d3dfc->BindFlags = D3D11_BIND_RENDER_TARGET;
  if (params_.kind == IM_ENC_MF_HW) {
    d3dfc->BindFlags |= D3D11_BIND_VIDEO_ENCODER; // Media Foundation's MFTs need it on their input
  }
  if (const int r = api_->av_hwframe_ctx_init(frames_ref_); r < 0) {
    return EncodeFail(r, "av_hwframe_ctx_init(D3D11)");
  }
  if (qsv_) {
    if (const int r = api_->av_hwframe_ctx_create_derived(&qsv_frames_ref_, AV_PIX_FMT_QSV, qsv_device_ref_,
                                                          frames_ref_, AV_HWFRAME_MAP_DIRECT);
        r < 0) {
      return abi::Failf(IM_ERR_NO_ENCODER, abi::token::kOpenFailed, "encode: deriving the QSV frame pool failed: %s",
                        api_->ErrorText(r).c_str());
    }
    mapped_ = api_->av_frame_alloc();
    if (mapped_ == nullptr) {
      return abi::Fail(IM_ERR_INTERNAL, {}, "encode: av_frame_alloc failed");
    }
  }
  pending_ = api_->av_frame_alloc();
  if (pending_ == nullptr) {
    return abi::Fail(IM_ERR_INTERNAL, {}, "encode: av_frame_alloc failed");
  }
  return IM_OK;
}

im_status FfmpegEncoder::ApplyOptions() noexcept {
  const bool high = params_.profile != IM_PROFILE_CB;
  const bool detail = params_.content_hint == IM_CONTENT_DETAIL;
  Option opts[16];
  std::size_t n = 0;
  const auto add = [&](const char *name, const char *value, bool required) {
    if (n < std::size(opts)) {
      opts[n++] = Option{name, value, required};
    }
  };
  switch (params_.kind) {
  case IM_ENC_NVENC:
    add("preset", "p4", false);
    add("tune", "ll", false);
    add("rc", "cbr", false);
    add("zerolatency", "1", true);
    add("delay", "0", false);
    add("rc-lookahead", "0", false);
    add("forced-idr", "1", true);
    add("no-scenecut", "1", false);
    add("multipass", "disabled", false);
    add("b_ref_mode", "disabled", false);
    add("aud", "0", false);
    if (detail) {
      add("spatial-aq", "1", false);
    }
    add("profile", high ? "high" : "baseline", true);
    break;
  case IM_ENC_QSV:
    add("preset", "veryfast", false);
    add("look_ahead", "0", false);
    add("async_depth", "1", true);
    add("forced_idr", "1", true);
    add("scenario", "displayremoting", false);
    add("adaptive_i", "0", false);
    add("adaptive_b", "0", false);
    add("idr_interval", "0", false);
    add("profile", high ? "high" : "baseline", false);
    break;
  case IM_ENC_AMF:
    add("usage", "ultralowlatency", true);
    add("quality", detail ? "balanced" : "speed", false);
    add("rc", "cbr", false);
    add("enforce_hrd", "1", false);
    add("filler_data", "0", false);
    add("frame_skipping", "0", false);
    add("preencode", "0", false);
    add("forced_idr", "1", false);
    add("header_insertion_mode", "idr", false);
    if (detail) {
      add("vbaq", "1", false);
    }
    add("profile", high ? "high" : "constrained_baseline", false);
    break;
  case IM_ENC_MF_HW:
  case IM_ENC_MF_SW:
    add("hw_encoding", params_.kind == IM_ENC_MF_HW ? "1" : "0", true);
    add("scenario", "display_remoting", true);
    add("rate_control", "cbr", false);
    break;
  default:
    break;
  }
  for (std::size_t i = 0; i < n; ++i) {
    const Option &o = opts[i];
    const int r = api_->av_opt_set(ctx_->priv_data, o.name, o.value, 0);
    if (r >= 0) {
      continue;
    }
    if (o.required) {
      return abi::Failf(IM_ERR_NO_ENCODER, abi::token::kOpenFailed, "encode: %s refused the required option %s=%s: %s",
                        codec_->name, o.name, o.value, api_->ErrorText(r).c_str());
    }
    // 04 §9.3: an option an FFmpeg patch release renamed degrades instead of breaking.
    log::Debug("encode", "option skipped",
               {{"encoder", encpolicy::EncoderToken(params_.kind)},
                {"option", o.name},
                {"value", o.value},
                {"unknown", r == AVERROR_OPTION_NOT_FOUND}});
  }
  return IM_OK;
}

im_status FfmpegEncoder::OpenCodec(OpenFacts *facts) noexcept {
  if (const im_status st = SetupDevice(); st != IM_OK) {
    Release();
    return st;
  }
  if (const im_status st = SetupFrames(); st != IM_OK) {
    Release();
    return st;
  }
  ctx_ = api_->avcodec_alloc_context3(codec_);
  pkt_ = api_->av_packet_alloc();
  if (ctx_ == nullptr || pkt_ == nullptr) {
    Release();
    return abi::Fail(IM_ERR_INTERNAL, {}, "encode: out of memory allocating the codec context");
  }
  const encpolicy::EncoderTraits &traits = encpolicy::Traits(params_.kind);
  ctx_->width = static_cast<int>(params_.width);
  ctx_->height = static_cast<int>(params_.height);
  ctx_->time_base = AVRational{1, 1000000};
  ctx_->framerate = AVRational{static_cast<int>(params_.fps), 1};
  ctx_->max_b_frames = 0;                                // required (04 §9.3)
  ctx_->gop_size = static_cast<int>(params_.fps) * 3600; // the session forces IDRs itself (04 §9.4)
  ctx_->bit_rate = static_cast<int64_t>(bitrate_);
  ctx_->rc_max_rate = static_cast<int64_t>(bitrate_);
  ctx_->rc_buffer_size = static_cast<int>(static_cast<double>(bitrate_) * vbv_s_);
  ctx_->flags |= AV_CODEC_FLAG_LOW_DELAY; // and no AV_CODEC_FLAG_GLOBAL_HEADER: headers stay in-band
  ctx_->color_range = AVCOL_RANGE_MPEG;
  ctx_->colorspace = AVCOL_SPC_BT709;
  ctx_->color_primaries = AVCOL_PRI_BT709;
  ctx_->color_trc = AVCOL_TRC_BT709;
  ctx_->profile = params_.profile == IM_PROFILE_CB ? AV_PROFILE_H264_CONSTRAINED_BASELINE : AV_PROFILE_H264_HIGH;
  if (traits.hardware) {
    ctx_->thread_count = 1;
  }
  ctx_->sw_pix_fmt = AV_PIX_FMT_NV12;
  if (zero_copy_) {
    ctx_->pix_fmt = qsv_ ? AV_PIX_FMT_QSV : AV_PIX_FMT_D3D11;
    ctx_->hw_frames_ctx = api_->av_buffer_ref(qsv_ ? qsv_frames_ref_ : frames_ref_);
    if (ctx_->hw_frames_ctx == nullptr) {
      Release();
      return abi::Fail(IM_ERR_INTERNAL, {}, "encode: av_buffer_ref failed");
    }
  } else {
    ctx_->pix_fmt = AV_PIX_FMT_NV12;
  }
  // The device the encoder runs on (its adapter): the D3D11 device, QSV's derived one.
  if (AVBufferRef *dev = qsv_ ? qsv_device_ref_ : device_ref_; dev != nullptr) {
    ctx_->hw_device_ctx = api_->av_buffer_ref(dev);
  }
  if (const im_status st = ApplyOptions(); st != IM_OK) {
    Release();
    return st;
  }

  ffmpeg::LogCapture capture;
  const int64_t t0 = engine::os::QpcNs();
  const int r = api_->avcodec_open2(ctx_, codec_, nullptr);
  const int64_t open_ns = engine::os::QpcNs() - t0;
  if (facts != nullptr) {
    facts->zero_copy = zero_copy_;
    facts->open_us = static_cast<uint32_t>(std::clamp<int64_t>(open_ns / 1000, 0, UINT32_MAX));
  }
  if (r < 0) {
    const std::string err = api_->ErrorText(r);
    im_status st;
    if (device_->Removed()) {
      st = abi::Failf(IM_ERR_ENCODER_LOST, abi::token::kDeviceRemoved, "encode: %s: the device was removed",
                      codec_->name);
    } else if (params_.kind == IM_ENC_NVENC &&
               (capture.Contains("required nvenc api version") || capture.Contains("minimum required nvidia driver"))) {
      // nvenc.c: "Driver does not support the required nvenc API version. Required: x Found: y" and "The minimum
      // required Nvidia driver for nvenc is …" (04 §9.10; 03 names the driver version 04 §13.2 needs).
      st = abi::Failf(IM_ERR_DEPENDENCY, abi::token::kDriverTooOld, "encode: %s: the driver is too old for NVENC (%s)",
                      codec_->name, err.c_str());
    } else if (params_.kind == IM_ENC_NVENC &&
               (capture.Contains("out of memory (10)") || capture.Contains("incompatible client key (21)"))) {
      // NV_ENC_ERR_OUT_OF_MEMORY from OpenEncodeSessionEx: GeForce's per-system session limit (04 §9.7).
      st = abi::Failf(IM_ERR_LIMIT, abi::token::kSessionLimit, "encode: %s: NVENC refused another session",
                      codec_->name);
    } else {
      st = abi::Failf(IM_ERR_NO_ENCODER, abi::token::kOpenFailed, "encode: avcodec_open2(%s) failed: %s", codec_->name,
                      err.c_str());
    }
    Release();
    return st;
  }
  try {
    au_.reserve(std::max<std::size_t>(64 * 1024, bitrate_ / 8));
  } catch (...) {
    Release();
    return abi::Fail(IM_ERR_INTERNAL, {}, "encode: out of memory");
  }
  return IM_OK;
}

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
int ffmpeg::OpenEncoderWithoutDeviceForTest(uint32_t kind, std::string *error) noexcept {
  error->clear();
  const ffmpeg::Api *api = ffmpeg::Loaded();
  const AVCodec *codec = FindCodec(api, kind);
  if (codec == nullptr) {
    *error = api == nullptr ? "FFmpeg is not loaded" : "the FFmpeg build has no such encoder";
    return AVERROR_ENCODER_NOT_FOUND;
  }
  AVCodecContext *ctx = api->avcodec_alloc_context3(codec);
  if (ctx == nullptr) {
    *error = "out of memory";
    return AVERROR(ENOMEM);
  }
  ctx->width = 1920;
  ctx->height = 1080;
  ctx->time_base = AVRational{1, 1000000}; // avcodec_open2 refuses an encoder without one
  ctx->framerate = AVRational{60, 1};
  ctx->pix_fmt = AV_PIX_FMT_NV12; // in all three encoders' pixel-format lists
  ctx->bit_rate = 8'000'000;
  ctx->max_b_frames = 0;
  ctx->thread_count = 1;
  const int r = api->avcodec_open2(ctx, codec, nullptr);
  if (r < 0) {
    *error = api->ErrorText(r);
  }
  api->avcodec_free_context(&ctx);
  return r;
}
#endif

im_status FfmpegEncoder::EncodeFail(int err, const char *what) noexcept {
  if (device_ != nullptr && device_->Removed()) {
    return abi::Failf(IM_ERR_ENCODER_LOST, abi::token::kDeviceRemoved, "encode: %s: the device was removed", what);
  }
  return abi::Failf(IM_ERR_INTERNAL, {}, "encode: %s failed: %s", what,
                    api_ != nullptr ? api_->ErrorText(err).c_str() : "");
}

void FfmpegEncoder::Release() noexcept {
  if (api_ == nullptr) {
    return;
  }
  api_->av_frame_free(&mapped_);
  api_->av_frame_free(&pending_);
  api_->av_frame_free(&sw_frame_);
  api_->av_packet_free(&pkt_);
  api_->avcodec_free_context(&ctx_);
  api_->av_buffer_unref(&qsv_frames_ref_);
  api_->av_buffer_unref(&frames_ref_);
  api_->av_buffer_unref(&qsv_device_ref_);
  api_->av_buffer_unref(&device_ref_);
  readback_.reset();
  surface_.reset();
  gray_.Reset();
}

im_status FfmpegEncoder::NextTarget(gpu::Nv12Target *out) noexcept {
  *out = gpu::Nv12Target{};
  if (ctx_ == nullptr) {
    return abi::Fail(IM_ERR_BAD_STATE, {}, "encode: the encoder is closed");
  }
  if (!zero_copy_) {
    *out = surface_->target();
    return IM_OK;
  }
  api_->av_frame_unref(pending_);
  if (const int r = api_->av_hwframe_get_buffer(frames_ref_, pending_, 0); r < 0) {
    return EncodeFail(r, "av_hwframe_get_buffer");
  }
  // AVD3D11FrameDescriptor's convention: data[0] the texture, data[1] the array index.
  out->texture = reinterpret_cast<ID3D11Texture2D *>(pending_->data[0]);
  out->array_slice = static_cast<uint32_t>(reinterpret_cast<intptr_t>(pending_->data[1]));
  return IM_OK;
}

im_status FfmpegEncoder::FillGray(const gpu::Nv12Target &target) noexcept {
  if (!zero_copy_) {
    gray_next_ = true; // Encode writes the gray planes into the system-memory frame instead of reading back
    return IM_OK;
  }
  if (target.texture == nullptr) {
    return abi::Fail(IM_ERR_INVALID_ARG, {}, "encode: FillGray needs the target NextTarget handed out");
  }
  if (!gray_) {
    const std::size_t w = params_.width;
    const std::size_t h = params_.height;
    std::vector<uint8_t> pixels;
    try {
      pixels.assign(w * h * 3 / 2, 128);
    } catch (...) {
      return abi::Fail(IM_ERR_INTERNAL, {}, "encode: out of memory");
    }
    D3D11_TEXTURE2D_DESC desc{};
    desc.Width = params_.width;
    desc.Height = params_.height;
    desc.MipLevels = 1;
    desc.ArraySize = 1;
    desc.Format = DXGI_FORMAT_NV12;
    desc.SampleDesc.Count = 1;
    desc.Usage = D3D11_USAGE_DEFAULT;
    D3D11_SUBRESOURCE_DATA init{};
    init.pSysMem = pixels.data();
    init.SysMemPitch = static_cast<UINT>(w);
    if (const HRESULT hr = device_->d3d()->CreateTexture2D(&desc, &init, &gray_); FAILED(hr)) {
      return device_->Fail(hr, "encode: the gray test picture");
    }
  }
  gpu::ContextLock lock(*device_);
  device_->context()->CopySubresourceRegion(target.texture, D3D11CalcSubresource(0, target.array_slice, 1), 0, 0, 0,
                                            gray_.Get(), 0, nullptr);
  return IM_OK;
}

im_status FfmpegEncoder::Encode(const session::EncodeInput &in, session::EncodedAu *out) noexcept {
  *out = session::EncodedAu{};
  if (ctx_ == nullptr) {
    return abi::Fail(IM_ERR_BAD_STATE, {}, "encode: the encoder is closed");
  }
  au_.clear();
  AVFrame *frame = nullptr;
  if (zero_copy_) {
    if (pending_->buf[0] == nullptr) {
      return abi::Fail(IM_ERR_INTERNAL, {}, "encode: Encode without a target from NextTarget");
    }
    frame = pending_;
    if (qsv_) {
      api_->av_frame_unref(mapped_);
      mapped_->format = AV_PIX_FMT_QSV;
      mapped_->hw_frames_ctx = api_->av_buffer_ref(qsv_frames_ref_);
      if (const int r = api_->av_hwframe_map(mapped_, pending_, AV_HWFRAME_MAP_READ); r < 0) {
        api_->av_frame_unref(pending_);
        return EncodeFail(r, "av_hwframe_map(QSV)");
      }
      frame = mapped_;
    }
  } else {
    if (const int r = api_->av_frame_make_writable(sw_frame_); r < 0) {
      return EncodeFail(r, "av_frame_make_writable");
    }
    if (gray_next_) {
      gray_next_ = false;
      for (uint32_t y = 0; y < params_.height; ++y) {
        std::memset(sw_frame_->data[0] + static_cast<std::ptrdiff_t>(y) * sw_frame_->linesize[0], 128, params_.width);
      }
      for (uint32_t y = 0; y < params_.height / 2; ++y) {
        std::memset(sw_frame_->data[1] + static_cast<std::ptrdiff_t>(y) * sw_frame_->linesize[1], 128, params_.width);
      }
    } else {
      if (in.picture == nullptr) {
        return abi::Fail(IM_ERR_INTERNAL, {}, "encode: Encode without a picture");
      }
      gpu::Nv12Planes planes;
      planes.y = sw_frame_->data[0];
      planes.y_stride = static_cast<std::size_t>(sw_frame_->linesize[0]);
      planes.uv = sw_frame_->data[1];
      planes.uv_stride = static_cast<std::size_t>(sw_frame_->linesize[1]);
      if (const im_status st = readback_->Read(*in.picture, planes); st != IM_OK) {
        return st;
      }
    }
    frame = sw_frame_;
  }
  int64_t pts = (in.ts_ns - params_.epoch_ns) / 1000;
  if (pts <= last_pts_) {
    pts = last_pts_ + 1; // encoders want increasing timestamps; a refresh may share a microsecond
  }
  last_pts_ = pts;
  frame->pts = pts;
  if (params_.kind == IM_ENC_MF_HW || params_.kind == IM_ENC_MF_SW) {
    // One nominal frame interval in time_base (1/1e6): h264_mf passes it on as the sample's duration, which is 0
    // otherwise. MF only: AMF would take it as the surface duration for rate control, and the real spacing can be
    // wider.
    frame->duration = static_cast<int64_t>(1'000'000 / params_.fps);
  }
  pts_ring_[pts_next_] = PtsTs{pts, in.ts_ns};
  pts_next_ = (pts_next_ + 1) % kPtsRing;
  au_ts_ns_ = 0;
  bool au_ts_set = false;
  const auto capture_time = [&](int64_t packet_pts) noexcept {
    for (const PtsTs &e : pts_ring_) {
      if (e.pts == packet_pts) {
        return e.ts_ns;
      }
    }
    return in.ts_ns; // no pts (AV_NOPTS_VALUE) or one older than the ring: the frame just sent
  };
  frame->pict_type = in.idr ? AV_PICTURE_TYPE_I : AV_PICTURE_TYPE_NONE;
  if (in.idr) {
    frame->flags |= AV_FRAME_FLAG_KEY;
  } else {
    frame->flags &= ~AV_FRAME_FLAG_KEY;
  }

  bool keyframe = false;
  const auto drain = [&]() -> int {
    for (;;) {
      const int r = api_->avcodec_receive_packet(ctx_, pkt_);
      if (r == AVERROR(EAGAIN) || r == AVERROR_EOF) {
        return 0;
      }
      if (r < 0) {
        return r;
      }
      try {
        au_.insert(au_.end(), pkt_->data, pkt_->data + pkt_->size);
      } catch (...) {
        api_->av_packet_unref(pkt_);
        return AVERROR(ENOMEM);
      }
      keyframe = keyframe || (pkt_->flags & AV_PKT_FLAG_KEY) != 0;
      if (!au_ts_set) {
        au_ts_ns_ = capture_time(pkt_->pts); // the AU's first packet
        au_ts_set = true;
      }
      api_->av_packet_unref(pkt_);
    }
  };
  int r = api_->avcodec_send_frame(ctx_, frame);
  if (r == AVERROR(EAGAIN)) {
    // The encoder's queue is full: take what it has, then the frame goes in.
    if (const int d = drain(); d < 0) {
      r = d;
    } else {
      r = api_->avcodec_send_frame(ctx_, frame);
    }
  }
  if (zero_copy_) {
    // The encoder holds its own reference to a frame it took; the pool slot returns when it lets go.
    if (qsv_) {
      api_->av_frame_unref(mapped_);
    }
    api_->av_frame_unref(pending_);
  }
  if (r < 0) {
    return EncodeFail(r, "avcodec_send_frame");
  }
  if (const int d = drain(); d < 0) {
    return EncodeFail(d, "avcodec_receive_packet");
  }
  out->data = std::span<const uint8_t>(au_.data(), au_.size());
  out->keyframe = keyframe;
  out->capture_ts_ns = au_ts_set ? au_ts_ns_ : 0; // 04 §9.2: the frame the AU came from (0 without an AU)
  return IM_OK;
}

im_status FfmpegEncoder::Drain(std::vector<std::vector<uint8_t>> *aus) noexcept {
  if (ctx_ == nullptr) {
    return abi::Fail(IM_ERR_BAD_STATE, {}, "encode: the encoder is closed");
  }
  // h264_mf: the NULL frame sends MFT_MESSAGE_COMMAND_DRAIN, and from then on every receive calls ProcessOutput
  // until the MFT says MF_E_TRANSFORM_NEED_MORE_INPUT (then AVERROR_EOF).
  if (const int r = api_->avcodec_send_frame(ctx_, nullptr); r < 0 && r != AVERROR_EOF) {
    return EncodeFail(r, "avcodec_send_frame(NULL)");
  }
  for (;;) {
    const int r = api_->avcodec_receive_packet(ctx_, pkt_);
    if (r == AVERROR_EOF) {
      return IM_OK;
    }
    if (r == AVERROR(EAGAIN)) {
      // Not EOF after a drain request: an encoder that won't finish draining (libavcodec says it can't happen).
      return abi::Fail(IM_ERR_INTERNAL, {}, "encode: avcodec_receive_packet said EAGAIN while draining");
    }
    if (r < 0) {
      return EncodeFail(r, "avcodec_receive_packet(drain)");
    }
    try {
      aus->emplace_back(pkt_->data, pkt_->data + pkt_->size);
    } catch (...) {
      api_->av_packet_unref(pkt_);
      return abi::Fail(IM_ERR_INTERNAL, {}, "encode: out of memory draining");
    }
    api_->av_packet_unref(pkt_);
  }
}

im_status FfmpegEncoder::SetBitrate(uint32_t bps) noexcept {
  if (ctx_ == nullptr) {
    return abi::Fail(IM_ERR_BAD_STATE, {}, "encode: the encoder is closed");
  }
  if (!encpolicy::Traits(params_.kind).live_bitrate) {
    return abi::Failf(IM_ERR_BAD_STATE, {}, "encode: %s changes its bitrate by a re-init (04 §9.5)",
                      std::string(encpolicy::EncoderToken(params_.kind)).c_str());
  }
  if (bps == 0) {
    return abi::Fail(IM_ERR_INVALID_ARG, {}, "encode: bitrate 0");
  }
  // 04 §9.5: set before the next avcodec_send_frame; FFmpeg reconfigures NVENC (NvEncReconfigureEncoder) and resets
  // QSV (MFXVideoENCODE_Reset) when it sees the change.
  bitrate_ = bps;
  ctx_->bit_rate = static_cast<int64_t>(bps);
  ctx_->rc_max_rate = static_cast<int64_t>(bps);
  ctx_->rc_buffer_size = static_cast<int>(static_cast<double>(bps) * vbv_s_);
  return IM_OK;
}

#else // !IM_ENCODE_FFMPEG

bool FfmpegEncoder::BuildHasEncoder(uint32_t) noexcept { return false; }
bool FfmpegEncoder::BuildListsZeroCopy(uint32_t) noexcept { return false; }

im_status FfmpegEncoder::Create(const session::EncoderParams &, std::unique_ptr<FfmpegEncoder> *,
                                OpenFacts *) noexcept {
  ffmpeg::LoadResult why;
  ffmpeg::Load({}, &why);
  return abi::Fail(why.status, why.token, why.detail);
}

void FfmpegEncoder::Release() noexcept {}

im_status FfmpegEncoder::NextTarget(gpu::Nv12Target *out) noexcept {
  *out = gpu::Nv12Target{};
  return abi::Fail(IM_ERR_BAD_STATE, {}, "encode: built without FFmpeg");
}

im_status FfmpegEncoder::FillGray(const gpu::Nv12Target &) noexcept {
  return abi::Fail(IM_ERR_BAD_STATE, {}, "encode: built without FFmpeg");
}

im_status FfmpegEncoder::Encode(const session::EncodeInput &, session::EncodedAu *out) noexcept {
  *out = session::EncodedAu{};
  return abi::Fail(IM_ERR_BAD_STATE, {}, "encode: built without FFmpeg");
}

im_status FfmpegEncoder::SetBitrate(uint32_t) noexcept {
  return abi::Fail(IM_ERR_BAD_STATE, {}, "encode: built without FFmpeg");
}

im_status FfmpegEncoder::Drain(std::vector<std::vector<uint8_t>> *) noexcept {
  return abi::Fail(IM_ERR_BAD_STATE, {}, "encode: built without FFmpeg");
}

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
int ffmpeg::OpenEncoderWithoutDeviceForTest(uint32_t, std::string *error) noexcept {
  *error = "built without FFmpeg";
  return -1;
}
#endif

#endif // IM_ENCODE_FFMPEG

} // namespace im::encode
