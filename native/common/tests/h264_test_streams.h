// Synthetic H.264 streams for annexb_test, sps_test and the fuzz seeds: a bit writer with Exp-Golomb codes, an SPS
// writer (H.264 §7.3.2.1.1), stand-in PPS and slice units, and Annex B framing with 3- or 4-byte start codes.
#pragma once

#include <cstdint>
#include <initializer_list>
#include <span>
#include <vector>

#include "im/h264/nal.h"

namespace im::h264::testing {

using Bytes = std::vector<uint8_t>;

class BitWriter {
public:
  void Bit(uint32_t b) {
    cur_ = static_cast<uint8_t>((cur_ << 1) | (b & 1u));
    if (++n_ == 8) {
      out_.push_back(cur_);
      cur_ = 0;
      n_ = 0;
    }
  }
  void Bits(uint32_t v, uint32_t count) {
    for (uint32_t i = count; i > 0; --i) {
      Bit((v >> (i - 1)) & 1u);
    }
  }
  void Ue(uint32_t v) {
    const uint64_t x = uint64_t{v} + 1;
    uint32_t len = 0;
    while ((x >> len) > 1) {
      ++len;
    }
    Bits(0, len);
    for (uint32_t i = len + 1; i > 0; --i) {
      Bit(static_cast<uint32_t>((x >> (i - 1)) & 1u));
    }
  }
  void Se(int32_t v) { Ue(v > 0 ? static_cast<uint32_t>(2 * int64_t{v} - 1) : static_cast<uint32_t>(-2 * int64_t{v})); }
  // rbsp_trailing_bits: a stop bit, then zeros to the byte boundary.
  Bytes Finish() {
    Bit(1);
    while (n_ != 0) {
      Bit(0);
    }
    return out_;
  }

private:
  Bytes out_;
  uint8_t cur_ = 0;
  uint32_t n_ = 0;
};

struct SpsParams {
  uint8_t profile_idc = 100;
  uint8_t constraint = 0x00;
  uint8_t level_idc = 40;
  uint32_t sps_id = 0;
  uint32_t chroma_format_idc = 1; // written for High-family profiles
  bool scaling_matrix = false;    // High-family: one 4x4 list present
  uint32_t poc_type = 0;
  uint32_t width = 1920; // the picture size; coded as macroblocks with cropping
  uint32_t height = 1080;
  bool frame_mbs_only = true;
  bool vui = false;
};

// The escaped SPS NAL unit (header byte included).
inline Bytes MakeSps(const SpsParams &p) {
  BitWriter w;
  w.Bits(p.profile_idc, 8);
  w.Bits(p.constraint, 8);
  w.Bits(p.level_idc, 8);
  w.Ue(p.sps_id);
  const bool high = p.profile_idc == 100 || p.profile_idc == 110 || p.profile_idc == 122 || p.profile_idc == 244;
  if (high) {
    w.Ue(p.chroma_format_idc);
    if (p.chroma_format_idc == 3) {
      w.Bit(0); // separate_colour_plane_flag
    }
    w.Ue(0);  // bit_depth_luma_minus8
    w.Ue(0);  // bit_depth_chroma_minus8
    w.Bit(0); // qpprime_y_zero_transform_bypass_flag
    w.Bit(p.scaling_matrix ? 1 : 0);
    if (p.scaling_matrix) {
      const uint32_t lists = p.chroma_format_idc != 3 ? 8 : 12;
      for (uint32_t i = 0; i < lists; ++i) {
        w.Bit(i == 0 ? 1 : 0);
        if (i == 0) {
          for (int j = 0; j < 16; ++j) {
            w.Se(j == 0 ? 8 : 0); // first delta 8 (16), then flat
          }
        }
      }
    }
  }
  w.Ue(0); // log2_max_frame_num_minus4
  w.Ue(p.poc_type);
  if (p.poc_type == 0) {
    w.Ue(2); // log2_max_pic_order_cnt_lsb_minus4
  } else if (p.poc_type == 1) {
    w.Bit(0); // delta_pic_order_always_zero_flag
    w.Se(-1); // offset_for_non_ref_pic
    w.Se(2);  // offset_for_top_to_bottom_field
    w.Ue(2);  // num_ref_frames_in_pic_order_cnt_cycle
    w.Se(3);
    w.Se(-4);
  }
  w.Ue(1);  // max_num_ref_frames
  w.Bit(0); // gaps_in_frame_num_value_allowed_flag
  const uint32_t mbs_w = (p.width + 15) / 16;
  const uint32_t unit_rows = p.frame_mbs_only ? 16 : 32;
  const uint32_t map_units = (p.height + unit_rows - 1) / unit_rows;
  w.Ue(mbs_w - 1);
  w.Ue(map_units - 1);
  w.Bit(p.frame_mbs_only ? 1 : 0);
  if (!p.frame_mbs_only) {
    w.Bit(0); // mb_adaptive_frame_field_flag
  }
  w.Bit(1); // direct_8x8_inference_flag
  const uint32_t crop_w = mbs_w * 16 - p.width;
  const uint32_t crop_h = map_units * unit_rows - p.height;
  const bool chroma = !(high && p.chroma_format_idc == 0);
  const uint32_t unit_x = (chroma && p.chroma_format_idc != 3) ? 2 : 1;
  const uint32_t unit_y = ((chroma && p.chroma_format_idc == 1) ? 2 : 1) * (p.frame_mbs_only ? 1 : 2);
  if (crop_w != 0 || crop_h != 0) {
    w.Bit(1);
    w.Ue(0);
    w.Ue(crop_w / unit_x);
    w.Ue(0);
    w.Ue(crop_h / unit_y);
  } else {
    w.Bit(0);
  }
  w.Bit(p.vui ? 1 : 0);
  if (p.vui) {
    w.Bit(0); // aspect_ratio_info_present_flag ... (the parser stops before VUI)
  }
  Bytes rbsp = w.Finish();
  Bytes nal(1 + MaxEscapedSize(rbsp.size()));
  nal[0] = 0x67;
  nal.resize(1 + EscapeRbsp(rbsp, std::span<uint8_t>(nal).subspan(1)));
  return nal;
}

// A stand-in PPS (only cached and passed through by the normalizer).
inline Bytes MakePps(uint8_t tag = 0xCE) { return Bytes{0x68, tag, 0x3C, 0x80}; }

// A stand-in slice of the given NAL type with a body.
inline Bytes MakeSlice(uint8_t type, std::initializer_list<uint8_t> body = {0x88, 0x84, 0x21, 0xA0}) {
  // nal_ref_idc 3 for slices that are references (IDR, P), 0 for the rest.
  Bytes unit{static_cast<uint8_t>(((type == nal::kIdr || type == nal::kSlice) ? 0x60 : 0x00) | type)};
  unit.insert(unit.end(), body.begin(), body.end());
  return unit;
}

inline Bytes Unit(std::initializer_list<uint8_t> bytes) { return Bytes(bytes); }

// Annex B: each unit after a 4-byte (or, with short, 3-byte) start code.
inline Bytes AnnexB(std::initializer_list<Bytes> units, bool short_codes = false) {
  Bytes out;
  for (const Bytes &u : units) {
    if (!short_codes) {
      out.push_back(0);
    }
    out.insert(out.end(), {0, 0, 1});
    out.insert(out.end(), u.begin(), u.end());
  }
  return out;
}

// The NAL units of an Annex B stream.
inline std::vector<Bytes> Units(std::span<const uint8_t> stream) {
  std::vector<Bytes> units;
  ForEachNal(stream, [&](std::span<const uint8_t> nal) {
    units.emplace_back(nal.begin(), nal.end());
    return true;
  });
  return units;
}

} // namespace im::h264::testing
