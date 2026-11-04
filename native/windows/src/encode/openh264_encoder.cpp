#include "encode/openh264_encoder.h"

#include <algorithm>
#include <chrono>
#include <cstring>
#include <string_view>
#include <thread>
#include <utility>

#include "encode/selector.h"
#include "im/abi/status.h"
#include "im/abi/utf8.h"
#include "im/encpolicy/candidates.h"
#include "im/h264/nal.h"
#include "im/log/ring.h"

namespace im::encode {
namespace {

// OpenH264's trace callback: its messages at warning and above into the engine's log ring (it calls this from its
// encoder threads too; the ring never blocks).
void Trace(void *, int level, const char *text) {
  if (text == nullptr) {
    return;
  }
  std::string_view t(text);
  while (!t.empty() && (t.back() == '\n' || t.back() == '\r' || t.back() == ' ')) {
    t.remove_suffix(1);
  }
  if (t.empty()) {
    return;
  }
  if (level <= WELS_LOG_ERROR) {
    log::Error("openh264", t);
  } else {
    log::Warn("openh264", t);
  }
}

int64_t NowUs() noexcept {
  return std::chrono::duration_cast<std::chrono::microseconds>(std::chrono::steady_clock::now().time_since_epoch())
      .count();
}

uint32_t Cores() noexcept { return std::max(1u, std::thread::hardware_concurrency()); }

// The NAL types of an access unit.
struct AuTypes {
  bool sps = false;
  bool pps = false;
  bool idr = false;
};
AuTypes TypesOf(std::span<const uint8_t> au) noexcept {
  AuTypes t;
  h264::ForEachNal(au, [&](std::span<const uint8_t> nal) {
    switch (h264::NalType(nal)) {
    case h264::nal::kSps:
      t.sps = true;
      break;
    case h264::nal::kPps:
      t.pps = true;
      break;
    case h264::nal::kIdr:
      t.idr = true;
      break;
    default:
      break;
    }
    return true;
  });
  return t;
}

// 04 §9.10 step 3 for OpenH264: open at 1920×1080 (30 fps, the cap), one mid-gray IDR that must carry SPS, PPS and an
// IDR slice, then the live-bitrate check (two P frames, a 50 % bitrate change, two more frames: an IDR among those
// clears the live flag). The record goes with the library (openh264_api.h).
openh264::TestRecord TestEncode(const std::shared_ptr<const openh264::Library> &lib) noexcept {
  openh264::TestRecord r;
  r.tested = true;
  const int64_t t0 = NowUs();
  const auto done = [&](im_status st, std::string_view token) {
    r.status = st;
    r.token = token;
    r.test_encode_us = static_cast<uint32_t>(std::clamp<int64_t>(NowUs() - t0, 0, UINT32_MAX));
    abi::ClearLastError();
    return r;
  };
  openh264::Settings s;
  s.width = 1920;
  s.height = 1080;
  s.fps = openh264::kMaxFps;
  s.bitrate_bps = 8'000'000;
  s.cores = Cores();
  std::unique_ptr<OpenH264Core> core;
  if (OpenH264Core::Create(lib, s, &core) != IM_OK) {
    return done(IM_ERR_NO_ENCODER, abi::token::kOpenFailed);
  }
  if (NowUs() - t0 > 1'000'000) {
    return done(IM_ERR_NO_ENCODER, abi::token::kSlowOpen);
  }
  std::vector<uint8_t> gray;
  try {
    gray.assign(static_cast<std::size_t>(s.width) * s.height, 128);
  } catch (...) {
    return done(IM_ERR_INTERNAL, {});
  }
  const uint8_t *y = gray.data();
  const uint8_t *c = gray.data(); // the chroma planes are mid-gray too (128), half the size: the same buffer
  std::span<const uint8_t> au;
  bool key = false;
  if (core->Encode(y, s.width, c, c, s.width / 2, 0, true, &au, &key) != IM_OK) {
    return done(IM_ERR_NO_ENCODER, abi::token::kTestEncodeFailed);
  }
  const AuTypes first = TypesOf(au);
  if (!first.sps || !first.pps || !first.idr) {
    log::Warn(
        "encode", "probe: OpenH264's test IDR lacks SPS, PPS or an IDR slice",
        {{"sps", first.sps}, {"pps", first.pps}, {"idr", first.idr}, {"bytes", static_cast<uint64_t>(au.size())}});
    return done(IM_ERR_NO_ENCODER, abi::token::kTestEncodeFailed);
  }
  for (int i = 1; i <= 4; ++i) {
    if (i == 3 && core->SetBitrate(s.bitrate_bps / 2) != IM_OK) {
      return done(IM_ERR_NO_ENCODER, abi::token::kTestEncodeFailed);
    }
    if (core->Encode(y, s.width, c, c, s.width / 2, int64_t{i} * 1000 / s.fps, false, &au, &key) != IM_OK) {
      return done(IM_ERR_NO_ENCODER, abi::token::kTestEncodeFailed);
    }
    if (i >= 3 && TypesOf(au).idr) {
      r.live_bitrate = false;
      log::Info("encode", "probe: an IDR after a live bitrate change",
                {{"encoder", "openh264"}, {"live_bitrate_idr", true}});
    }
  }
  return done(IM_OK, {});
}

} // namespace

// ---------------------------------------------------------------------------------------------- OpenH264Core

OpenH264Core::OpenH264Core(std::shared_ptr<const openh264::Library> lib, const openh264::Settings &settings) noexcept
    : lib_(std::move(lib)), settings_(settings) {}

OpenH264Core::~OpenH264Core() {
  if (encoder_ != nullptr) {
    encoder_->Uninitialize();
    lib_->destroy_encoder(encoder_); // joins OpenH264's threads before lib_ may unload the DLL
  }
}

im_status OpenH264Core::Create(std::shared_ptr<const openh264::Library> lib, const openh264::Settings &settings,
                               std::unique_ptr<OpenH264Core> *out) noexcept {
  out->reset();
  if (!lib || lib->create_encoder == nullptr) {
    return abi::Fail(IM_ERR_INTERNAL, {}, "openh264: no library");
  }
  if (!openh264::SizeSupported(settings.width, settings.height)) {
    return abi::Failf(IM_ERR_NO_ENCODER, abi::token::kOpenFailed, "openh264: %ux%u is beyond OpenH264's 1080p cap",
                      settings.width, settings.height);
  }
  std::unique_ptr<OpenH264Core> c;
  try {
    c.reset(new OpenH264Core(std::move(lib), settings));
    c->info_ = std::make_unique<SFrameBSInfo>();
    c->au_.reserve(static_cast<std::size_t>(settings.width) * settings.height * 3 / 2);
  } catch (...) {
    return abi::Fail(IM_ERR_INTERNAL, {}, "openh264: out of memory");
  }
  if (c->lib_->create_encoder(&c->encoder_) != 0 || c->encoder_ == nullptr) {
    c->encoder_ = nullptr;
    return abi::Fail(IM_ERR_NO_ENCODER, abi::token::kOpenFailed, "openh264: WelsCreateSVCEncoder failed");
  }
  int trace_level = WELS_LOG_WARNING;
  c->encoder_->SetOption(ENCODER_OPTION_TRACE_LEVEL, &trace_level);
  WelsTraceCallback trace = &Trace;
  c->encoder_->SetOption(ENCODER_OPTION_TRACE_CALLBACK, &trace);
  SEncParamExt p{};
  if (c->encoder_->GetDefaultParams(&p) != cmResultSuccess) {
    return abi::Fail(IM_ERR_NO_ENCODER, abi::token::kOpenFailed, "openh264: GetDefaultParams failed");
  }
  openh264::ApplySettings(settings, &p);
  if (const int r = c->encoder_->InitializeExt(&p); r != cmResultSuccess) {
    return abi::Failf(IM_ERR_NO_ENCODER, abi::token::kOpenFailed, "openh264: InitializeExt failed (%d)", r);
  }
  int format = videoFormatI420;
  c->encoder_->SetOption(ENCODER_OPTION_DATAFORMAT, &format);
  *out = std::move(c);
  return IM_OK;
}

im_status OpenH264Core::Encode(const uint8_t *y, std::size_t y_stride, const uint8_t *u, const uint8_t *v,
                               std::size_t c_stride, int64_t ts_ms, bool force_idr, std::span<const uint8_t> *au,
                               bool *idr) noexcept {
  *au = {};
  *idr = false;
  if (ts_ms <= last_ts_ms_) {
    ts_ms = last_ts_ms_ + 1; // OpenH264's rate control wants increasing times; a refresh may share a millisecond
  }
  last_ts_ms_ = ts_ms;
  SSourcePicture pic{};
  pic.iColorFormat = videoFormatI420;
  pic.iPicWidth = static_cast<int>(settings_.width);
  pic.iPicHeight = static_cast<int>(settings_.height);
  pic.iStride[0] = static_cast<int>(y_stride);
  pic.iStride[1] = static_cast<int>(c_stride);
  pic.iStride[2] = static_cast<int>(c_stride);
  // OpenH264 takes non-const planes but only reads them.
  pic.pData[0] = const_cast<uint8_t *>(y);
  pic.pData[1] = const_cast<uint8_t *>(u);
  pic.pData[2] = const_cast<uint8_t *>(v);
  pic.uiTimeStamp = ts_ms;
  if (force_idr) {
    encoder_->ForceIntraFrame(true);
  }
  *info_ = SFrameBSInfo{};
  if (const int r = encoder_->EncodeFrame(&pic, info_.get()); r != cmResultSuccess) {
    return abi::Failf(IM_ERR_INTERNAL, {}, "openh264: EncodeFrame failed (%d)", r);
  }
  try {
    openh264::CollectAu(*info_, &au_);
  } catch (...) {
    return abi::Fail(IM_ERR_INTERNAL, {}, "openh264: out of memory");
  }
  *au = std::span<const uint8_t>(au_);
  *idr = openh264::IsIdr(*info_);
  return IM_OK;
}

im_status OpenH264Core::SetBitrate(uint32_t bps) noexcept {
  SBitrateInfo b{};
  b.iLayer = SPATIAL_LAYER_ALL;
  b.iBitrate = static_cast<int>(std::min<uint32_t>(bps, 0x7FFFFFFF));
  // The maximum first when the target rises, last when it falls: OpenH264 keeps the target at or below the maximum.
  const bool up = bps > settings_.bitrate_bps;
  int r1 = 0;
  int r2 = 0;
  if (up) {
    r1 = encoder_->SetOption(ENCODER_OPTION_MAX_BITRATE, &b);
    r2 = encoder_->SetOption(ENCODER_OPTION_BITRATE, &b);
  } else {
    r1 = encoder_->SetOption(ENCODER_OPTION_BITRATE, &b);
    r2 = encoder_->SetOption(ENCODER_OPTION_MAX_BITRATE, &b);
  }
  if (r1 != cmResultSuccess || r2 != cmResultSuccess) {
    return abi::Failf(IM_ERR_INTERNAL, {}, "openh264: the bitrate change failed (%d, %d)", r1, r2);
  }
  settings_.bitrate_bps = bps;
  return IM_OK;
}

// ---------------------------------------------------------------------------------------------- OpenH264Encoder

im_status OpenH264Encoder::Open(const session::EncoderParams &params,
                                std::unique_ptr<session::VideoEncoder> *out) noexcept {
  out->reset();
  if (params.profile != IM_PROFILE_CB) {
    return abi::Fail(IM_ERR_NO_ENCODER, abi::token::kOpenFailed, "openh264: Constrained Baseline only");
  }
  if (params.device == nullptr) {
    return abi::Fail(IM_ERR_INTERNAL, {}, "openh264: the session gave no device");
  }
  std::shared_ptr<const openh264::Library> lib;
  if (const im_status st = openh264::Acquire(&lib); st != IM_OK) {
    return st;
  }
  openh264::Settings s;
  s.width = params.width;
  s.height = params.height;
  s.fps = params.fps;
  s.bitrate_bps = params.bitrate_bps;
  s.cores = Cores();
  std::unique_ptr<OpenH264Encoder> e;
  try {
    e.reset(new OpenH264Encoder(params));
    const std::size_t luma = static_cast<std::size_t>(params.width) * params.height;
    e->y_.resize(luma);
    e->uv_.resize(luma / 2);
    e->u_.resize(luma / 4);
    e->v_.resize(luma / 4);
  } catch (...) {
    return abi::Fail(IM_ERR_INTERNAL, {}, "openh264: out of memory");
  }
  if (const im_status st = OpenH264Core::Create(std::move(lib), s, &e->core_); st != IM_OK) {
    return st;
  }
  const video::Size size{params.width, params.height};
  if (const im_status st = gpu::Nv12Surface::Create(*params.device, size, &e->surface_); st != IM_OK) {
    return st;
  }
  if (const im_status st = gpu::Nv12Readback::Create(*params.device, size, e->surface_->planar(), &e->readback_);
      st != IM_OK) {
    return st;
  }
  *out = std::move(e);
  return IM_OK;
}

im_status OpenH264Encoder::NextTarget(gpu::Nv12Target *out) noexcept {
  *out = surface_->target();
  return IM_OK;
}

im_status OpenH264Encoder::Encode(const session::EncodeInput &in, session::EncodedAu *out) noexcept {
  *out = session::EncodedAu{};
  if (in.picture == nullptr) {
    return abi::Fail(IM_ERR_INTERNAL, {}, "openh264: Encode without a picture");
  }
  const uint32_t w = params_.width;
  const uint32_t h = params_.height;
  gpu::Nv12Planes planes;
  planes.y = y_.data();
  planes.y_stride = w;
  planes.uv = uv_.data();
  planes.uv_stride = w;
  if (const im_status st = readback_->Read(*in.picture, planes); st != IM_OK) {
    return st;
  }
  openh264::SplitChroma(uv_.data(), w, w, h, u_.data(), v_.data());
  const int64_t ts_ms = std::max<int64_t>(in.ts_ns - params_.epoch_ns, 0) / 1'000'000;
  std::span<const uint8_t> au;
  bool idr = false;
  if (const im_status st = core_->Encode(y_.data(), w, u_.data(), v_.data(), w / 2, ts_ms, in.idr, &au, &idr);
      st != IM_OK) {
    return st;
  }
  out->data = au;
  out->keyframe = idr;
  return IM_OK;
}

im_status OpenH264Encoder::SetBitrate(uint32_t bps) noexcept { return core_->SetBitrate(bps); }

// ---------------------------------------------------------------------------------------------- the selector's entry

void ProbeOpenH264(bool full, im_encoder_info &info) noexcept {
  info.adapter_index = encpolicy::kNoAdapter;
  abi::CopyFixed(info.codec, "openh264");
  info.flags = IM_ENCF_FORCED_IDR | IM_ENCF_LIVE_BITRATE | IM_ENCF_CB; // 04 §9.1: software, live, CB only
  info.test_encode_us = 0;
  abi::CopyFixed(info.reason, std::string_view{});
  std::shared_ptr<const openh264::Library> lib;
  if (const im_status st = openh264::Acquire(&lib); st != IM_OK) {
    info.status = st;
    std::string_view token = abi::LastErrorText();
    token = token.substr(0, token.find(':'));
    abi::CopyFixed(info.reason, abi::IsToken(token) ? token : abi::token::kMissing);
    abi::ClearLastError();
    return;
  }
  openh264::TestRecord rec = openh264::TestRecordOf(lib.get());
  if (full) {
    rec = TestEncode(lib);
    openh264::StoreTestRecord(lib.get(), rec);
    log::Info("encode", "encoder probed",
              {{"encoder", "openh264"},
               {"adapter", int64_t{-1}},
               {"status", rec.status},
               {"reason", rec.token},
               {"flags", info.flags},
               {"us", rec.test_encode_us}});
  }
  if (!rec.tested) {
    info.status = IM_OK; // turned on and loaded; untested until a full probe (like a quick probe's FFmpeg entries)
    return;
  }
  info.test_encode_us = rec.test_encode_us;
  if (rec.status != IM_OK) {
    info.status = rec.status;
    abi::CopyFixed(info.reason, rec.token);
    return;
  }
  if (!rec.live_bitrate) {
    info.flags &= ~IM_ENCF_LIVE_BITRATE;
  }
  info.status = IM_OK;
  info.flags |= IM_ENCF_AVAILABLE;
}

im_status OpenOpenH264(const session::EncoderParams &params, std::unique_ptr<session::VideoEncoder> *out) noexcept {
  return OpenH264Encoder::Open(params, out);
}

} // namespace im::encode
