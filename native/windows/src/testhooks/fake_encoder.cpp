#include <cstdint> // keeps this file's translation unit non-empty in builds without test hooks

#include "testhooks/fake_encoder.h"

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS

#include <algorithm>
#include <array>
#include <cstring>
#include <string_view>
#include <utility>

#include "im/abi/status.h"
#include "im/abi/utf8.h"
#include "im/audio/opus_packet.h"
#include "im/encpolicy/encoders.h"
#include "im/pace/pacer.h"
#include "testhooks/testhooks.h"

namespace im::testhooks {
namespace {

// NAL unit header bytes (nal_ref_idc, nal_unit_type; H.264 7.3.1), as M1's fake writes them.
constexpr uint8_t kNalSps = 0x67;    // nal_ref_idc 3, type 7
constexpr uint8_t kNalPps = 0x68;    // nal_ref_idc 3, type 8
constexpr uint8_t kNalIdr = 0x65;    // nal_ref_idc 3, type 5
constexpr uint8_t kNalNonIdr = 0x41; // nal_ref_idc 2, type 1
constexpr uint8_t kStopBit = 0x80;   // rbsp_stop_one_bit and alignment zeros
constexpr std::array<uint8_t, 4> kStartCode = {0, 0, 0, 1};

constexpr uint32_t kIdrFactor = 10; // an IDR is about 10 P frames (04 §18.2)
constexpr uint32_t kMinBody = 1;    // filler bytes at least

// H.264 Table A-1 without level 1b: level_idc, MaxMBPS and MaxFS (as M1's fake).
struct Level {
  uint8_t idc;
  uint32_t max_mbps;
  uint32_t max_fs;
};
constexpr Level kLevels[] = {
    {10, 1485, 99},       {11, 3000, 396},       {12, 6000, 396},       {13, 11880, 396},       {20, 11880, 396},
    {21, 19800, 792},     {22, 20250, 1620},     {30, 40500, 1620},     {31, 108000, 3600},     {32, 216000, 5120},
    {40, 245760, 8192},   {41, 245760, 8192},    {42, 522240, 8704},    {50, 589824, 22080},    {51, 983040, 36864},
    {52, 2073600, 36864}, {60, 4177920, 139264}, {61, 8355840, 139264}, {62, 16711680, 139264},
};

// The lowest level whose frame size, macroblock rate and side limits (A.3.1: each side at most sqrt(8 * MaxFS)
// macroblocks) fit the layer; 0 above level 6.2.
uint8_t LevelFor(uint64_t w_mbs, uint64_t h_mbs, uint64_t fps) noexcept {
  const uint64_t fs = w_mbs * h_mbs;
  for (const Level &l : kLevels) {
    if (fs <= l.max_fs && fs * fps <= l.max_mbps && w_mbs * w_mbs <= 8ull * l.max_fs &&
        h_mbs * h_mbs <= 8ull * l.max_fs) {
      return l.idc;
    }
  }
  return 0;
}

// Writes an RBSP MSB first (u(n), ue(v), se(v); H.264 7.2, 9.1).
class BitWriter {
public:
  void U(uint32_t n, uint64_t v) {
    for (uint32_t i = n; i-- > 0;) {
      if (nbit_ == 0) {
        b_.push_back(0);
      }
      if (((v >> i) & 1u) != 0) {
        b_.back() = static_cast<uint8_t>(b_.back() | (0x80u >> nbit_));
      }
      nbit_ = (nbit_ + 1) % 8;
    }
  }
  void Ue(uint64_t v) {
    const uint64_t x = v + 1;
    uint32_t n = 0;
    while ((x >> n) > 1) {
      ++n;
    }
    U(n, 0);
    U(n + 1, x);
  }
  void Se(int64_t v) { Ue(v > 0 ? static_cast<uint64_t>(2 * v - 1) : static_cast<uint64_t>(-2 * v)); }
  // rbsp_trailing_bits: the stop bit and alignment zeros.
  std::vector<uint8_t> Trailing() {
    U(1, 1);
    if (nbit_ != 0) {
      U(8 - nbit_, 0);
    }
    return std::move(b_);
  }

private:
  std::vector<uint8_t> b_;
  uint32_t nbit_ = 0;
};

// Appends rbsp to out with emulation prevention (H.264 7.4.1): a 0x03 after every two zero bytes that a byte <= 0x03
// follows. The zero count starts fresh, so out should end with a NAL header byte.
void AppendEscaped(std::vector<uint8_t> &out, std::span<const uint8_t> rbsp) {
  uint32_t zeros = 0;
  for (const uint8_t c : rbsp) {
    if (zeros >= 2 && c <= 3) {
      out.push_back(3);
      zeros = 0;
    }
    out.push_back(c);
    zeros = c == 0 ? zeros + 1 : 0;
  }
}

// xorshift64*: the seeded filler (deterministic per layer).
class Rng {
public:
  explicit Rng(uint64_t seed) noexcept : s_(seed == 0 ? 0x9E3779B97F4A7C15ull : seed) {}
  uint64_t Next() noexcept {
    s_ ^= s_ >> 12;
    s_ ^= s_ << 25;
    s_ ^= s_ >> 27;
    return s_ * 0x2545F4914F6CDD1Dull;
  }

private:
  uint64_t s_;
};

class FakeEncoder final : public session::VideoEncoder {
public:
  explicit FakeEncoder(const session::EncoderParams &p) : p_(p), rng_(0x15540000ull + p.layer * 7919ull + p.kind) {
    sps_ = BuildSps(p.profile, p.width, p.height, p.fps);
    pps_ = BuildPps();
    sizes_ = FakeFrameSizes(p.bitrate_bps, p.fps, p.gop_ms, p.layer);
    Reserve();
  }

  bool ok() const noexcept { return !sps_.empty(); }

  im_status Encode(const session::EncodeInput &in, session::EncodedAu *out) noexcept override {
    try {
      return EncodeFrame(in, out);
    } catch (...) {
      return abi::Fail(IM_ERR_INTERNAL, {}, "fake encoder: out of memory");
    }
  }

  im_status SetBitrate(uint32_t bps) noexcept override {
    p_.bitrate_bps = bps;
    sizes_ = FakeFrameSizes(bps, p_.fps, p_.gop_ms, p_.layer);
    try {
      Reserve();
    } catch (...) {
      // Out of memory: Encode grows its buffers when it needs them.
    }
    return IM_OK;
  }

private:
  im_status EncodeFrame(const session::EncodeInput &in, session::EncodedAu *out) {
    const bool key = in.idr || first_;
    first_ = false;
    const uint32_t target = key ? sizes_.idr : sizes_.p;
    au_.clear();
    if (key) {
      au_.insert(au_.end(), kStartCode.begin(), kStartCode.end());
      au_.insert(au_.end(), sps_.begin(), sps_.end());
      au_.insert(au_.end(), kStartCode.begin(), kStartCode.end());
      au_.insert(au_.end(), pps_.begin(), pps_.end());
    }
    au_.insert(au_.end(), kStartCode.begin(), kStartCode.end());
    au_.push_back(key ? kNalIdr : kNalNonIdr);

    audio::Marker m;
    m.rid = p_.rid;
    m.keyframe = key;
    m.flash = in.flash;
    m.frame = in.index;
    m.capture_ns = in.ts_ns - p_.epoch_ns;
    rbsp_.resize(audio::kMarkerSize);
    audio::EncodeMarker(m, std::span<uint8_t, audio::kMarkerSize>(rbsp_.data(), audio::kMarkerSize));
    const std::size_t used = au_.size() + audio::kMarkerSize + 1;
    const std::size_t filler = std::max<std::size_t>(target > used ? target - used : 0, kMinBody);
    for (std::size_t i = 0; i < filler; i += 8) {
      const uint64_t r = rng_.Next();
      for (std::size_t k = 0; k < 8 && i + k < filler; ++k) {
        rbsp_.push_back(static_cast<uint8_t>(r >> (8 * k)));
      }
    }
    rbsp_.push_back(kStopBit);
    AppendEscaped(au_, rbsp_);
    out->data = std::span<const uint8_t>(au_.data(), au_.size());
    out->keyframe = key;
    return IM_OK;
  }

  void Reserve() {
    const std::size_t n = std::size_t{sizes_.idr} + sps_.size() + pps_.size() + 64;
    au_.reserve(n + n / 2);
    rbsp_.reserve(n);
  }

  session::EncoderParams p_;
  Rng rng_;
  std::vector<uint8_t> sps_;
  std::vector<uint8_t> pps_;
  FakeSizes sizes_;
  bool first_ = true;
  std::vector<uint8_t> au_;
  std::vector<uint8_t> rbsp_;
};

im_status OpenFake(const session::EncoderParams &p, std::unique_ptr<session::VideoEncoder> *out) {
  const std::string_view token = encpolicy::EncoderToken(p.kind);
  if (OpensFail(p.kind)) {
    return abi::Failf(IM_ERR_NO_ENCODER, abi::token::kOpenFailed, "test hooks: opening %.*s fails (FailOpens)",
                      static_cast<int>(token.size()), token.data());
  }
  if (p.profile != IM_PROFILE_HIGH && p.profile != IM_PROFILE_CB) {
    return abi::Failf(IM_ERR_NO_ENCODER, abi::token::kOpenFailed, "fake encoder: profile 0x%04X",
                      static_cast<unsigned>(p.profile));
  }
  auto enc = std::make_unique<FakeEncoder>(p);
  if (!enc->ok()) {
    return abi::Failf(IM_ERR_NO_ENCODER, abi::token::kOpenFailed, "fake encoder: %ux%u@%u is above level 6.2",
                      static_cast<unsigned>(p.width), static_cast<unsigned>(p.height), static_cast<unsigned>(p.fps));
  }
  *out = std::move(enc);
  return IM_OK;
}

} // namespace

std::vector<uint8_t> BuildSps(uint32_t profile, uint32_t width, uint32_t height, uint32_t fps) {
  const bool high = profile == IM_PROFILE_HIGH;
  const uint64_t w_mbs = (uint64_t{width} + 15) / 16;
  const uint64_t h_mbs = (uint64_t{height} + 15) / 16;
  const uint8_t level = LevelFor(w_mbs, h_mbs, std::max<uint32_t>(fps, 1));
  if (level == 0 || width == 0 || height == 0) {
    return {};
  }
  BitWriter w;
  w.U(8, high ? 100 : 66);    // profile_idc
  w.U(8, high ? 0x00 : 0xE0); // constraint_set0..5 and reserved_zero_2bits: CB is 0xE0 (04 §9.4)
  w.U(8, level);
  w.Ue(0); // seq_parameter_set_id
  if (high) {
    w.Ue(1);   // chroma_format_idc: 4:2:0
    w.Ue(0);   // bit_depth_luma_minus8
    w.Ue(0);   // bit_depth_chroma_minus8
    w.U(1, 0); // qpprime_y_zero_transform_bypass_flag
    w.U(1, 0); // seq_scaling_matrix_present_flag
  }
  w.Ue(4);   // log2_max_frame_num_minus4
  w.Ue(2);   // pic_order_cnt_type: output order is decoding order (no B-frames)
  w.Ue(1);   // max_num_ref_frames
  w.U(1, 0); // gaps_in_frame_num_value_allowed_flag
  w.Ue(w_mbs - 1);
  w.Ue(h_mbs - 1);
  w.U(1, 1); // frame_mbs_only_flag
  w.U(1, 1); // direct_8x8_inference_flag
  // 4:2:0 crop units are 2 samples.
  const uint64_t crop_r = (w_mbs * 16 - width) / 2;
  const uint64_t crop_b = (h_mbs * 16 - height) / 2;
  if (crop_r > 0 || crop_b > 0) {
    w.U(1, 1); // frame_cropping_flag
    w.Ue(0);
    w.Ue(crop_r);
    w.Ue(0);
    w.Ue(crop_b);
  } else {
    w.U(1, 0);
  }
  w.U(1, 0); // vui_parameters_present_flag
  const std::vector<uint8_t> rbsp = w.Trailing();
  std::vector<uint8_t> nal{kNalSps};
  AppendEscaped(nal, rbsp);
  return nal;
}

std::vector<uint8_t> BuildPps() {
  BitWriter w;
  w.Ue(0);   // pic_parameter_set_id
  w.Ue(0);   // seq_parameter_set_id
  w.U(1, 0); // entropy_coding_mode_flag: CAVLC
  w.U(1, 0); // bottom_field_pic_order_in_frame_present_flag
  w.Ue(0);   // num_slice_groups_minus1
  w.Ue(0);   // num_ref_idx_l0_default_active_minus1
  w.Ue(0);   // num_ref_idx_l1_default_active_minus1
  w.U(1, 0); // weighted_pred_flag
  w.U(2, 0); // weighted_bipred_idc
  w.Se(0);   // pic_init_qp_minus26
  w.Se(0);   // pic_init_qs_minus26
  w.Se(0);   // chroma_qp_index_offset
  w.U(1, 1); // deblocking_filter_control_present_flag
  w.U(1, 0); // constrained_intra_pred_flag
  w.U(1, 0); // redundant_pic_cnt_present_flag
  const std::vector<uint8_t> rbsp = w.Trailing();
  std::vector<uint8_t> nal{kNalPps};
  AppendEscaped(nal, rbsp);
  return nal;
}

FakeSizes FakeFrameSizes(uint32_t bitrate_bps, uint32_t fps, uint32_t gop_ms, uint32_t layer) {
  const double f = std::max<uint32_t>(fps, 1);
  const double avg = static_cast<double>(bitrate_bps) / 8.0 / f;
  const uint32_t gop = gop_ms != 0 ? gop_ms : pace::DefaultGopMs(layer);
  const double n = std::max(1.0, static_cast<double>(gop) * f / 1000.0);
  // Over a GOP of n frames, one IDR of 10 P: n * avg = (n - 1 + 10) * P.
  const double p = n * avg / (n - 1.0 + kIdrFactor);
  FakeSizes s;
  s.p = static_cast<uint32_t>(std::max(p, 32.0) + 0.5);
  s.idr = s.p * kIdrFactor;
  return s;
}

session::EncoderEntry FakeEncoderEntry(uint32_t kind) {
  const encpolicy::EncoderTraits &t = encpolicy::Traits(kind);
  session::EncoderEntry e;
  e.info.kind = kind;
  e.info.adapter_index = t.hardware ? 0u : 0xFFFFFFFFu;
  e.info.flags = IM_ENCF_AVAILABLE | IM_ENCF_FORCED_IDR | (t.hardware ? IM_ENCF_HARDWARE : 0u) |
                 (t.live_bitrate ? IM_ENCF_LIVE_BITRATE : 0u) | (t.high ? IM_ENCF_HIGH : 0u) | (t.cb ? IM_ENCF_CB : 0u);
  e.info.status = IM_OK;
  abi::CopyFixed(e.info.codec, sizeof e.info.codec, t.codec);
  e.open = &OpenFake;
  return e;
}

} // namespace im::testhooks

#endif // IM_TEST_HOOKS
