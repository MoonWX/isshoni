#include "im/h264/sps.h"

#include <algorithm>
#include <array>

#include "im/h264/nal.h"

namespace im::h264 {

bool BitReader::Bits(uint32_t n, uint32_t *v) noexcept {
  if (n > 32) {
    return false;
  }
  if (bits_left() < n) {
    overrun_ = true;
    return false;
  }
  uint64_t r = 0;
  for (uint32_t i = 0; i < n; ++i) {
    const uint8_t byte = data_[pos_ >> 3];
    const uint32_t bit = (byte >> (7 - (pos_ & 7))) & 1u;
    r = (r << 1) | bit;
    ++pos_;
  }
  *v = static_cast<uint32_t>(r);
  return true;
}

bool BitReader::Flag(bool *v) noexcept {
  uint32_t b = 0;
  if (!Bits(1, &b)) {
    return false;
  }
  *v = b != 0;
  return true;
}

bool BitReader::Ue(uint32_t *v) noexcept {
  uint32_t zeros = 0;
  for (;;) {
    uint32_t b = 0;
    if (!Bits(1, &b)) {
      return false;
    }
    if (b != 0) {
      break;
    }
    if (++zeros > 31) {
      return false; // longer than any 32-bit value
    }
  }
  uint32_t suffix = 0;
  if (!Bits(zeros, &suffix)) {
    return false;
  }
  *v = static_cast<uint32_t>((uint64_t{1} << zeros) - 1 + suffix);
  return true;
}

bool BitReader::Se(int32_t *v) noexcept {
  uint32_t k = 0;
  if (!Ue(&k)) {
    return false;
  }
  const int64_t mag = (static_cast<int64_t>(k) + 1) / 2;
  *v = static_cast<int32_t>((k & 1u) != 0 ? mag : -mag);
  return true;
}

namespace {

constexpr uint32_t kMaxSide = 32768; // pixels; im_packet carries uint16 sizes, encoders stop at 4096

bool HasChromaInfo(uint8_t profile_idc) noexcept {
  switch (profile_idc) {
  case 100:
  case 110:
  case 122:
  case 244:
  case 44:
  case 83:
  case 86:
  case 118:
  case 128:
  case 138:
  case 139:
  case 134:
  case 135:
    return true;
  default:
    return false;
  }
}

// scaling_list() of H.264 §7.3.2.1.1.1: only parsed to get past it.
bool SkipScalingList(BitReader &br, uint32_t size) noexcept {
  int32_t last = 8;
  int32_t next = 8;
  for (uint32_t j = 0; j < size; ++j) {
    if (next != 0) {
      int32_t delta = 0;
      if (!br.Se(&delta) || delta < -128 || delta > 127) {
        return false;
      }
      next = (last + delta + 256) % 256;
    }
    last = next == 0 ? last : next;
  }
  return true;
}

// Reads ue(v) into *v and checks it is at most max.
bool UeMax(BitReader &br, uint32_t max, uint32_t *v) noexcept { return br.Ue(v) && *v <= max; }

} // namespace

SpsError ParseSps(std::span<const uint8_t> unit, Sps *out) noexcept {
  if (unit.empty() || (unit[0] & 0x80) != 0 || NalType(unit) != nal::kSps) {
    return SpsError::kNotSps;
  }
  if (unit.size() > kMaxSpsBytes) {
    return SpsError::kTooLarge;
  }
  std::array<uint8_t, kMaxSpsBytes> buf;
  const std::size_t n = UnescapeRbsp(unit.subspan(1), std::span<uint8_t>(buf));
  if (n > kMaxSpsRbspBytes) {
    return SpsError::kTooLarge;
  }
  BitReader br(std::span<const uint8_t>(buf.data(), n));

  Sps s;
  uint32_t v = 0;
  bool f = false;
  if (!br.Bits(8, &v)) {
    return SpsError::kTruncated;
  }
  s.profile_idc = static_cast<uint8_t>(v);
  if (!br.Bits(8, &v)) {
    return SpsError::kTruncated;
  }
  s.constraint_flags = static_cast<uint8_t>(v);
  if (!br.Bits(8, &v)) {
    return SpsError::kTruncated;
  }
  s.level_idc = static_cast<uint8_t>(v);

  // From here on a read fails at the end of the data (truncated) or on a value out of range.
  const auto fail = [&br]() noexcept { return br.overrun() ? SpsError::kTruncated : SpsError::kBadValue; };

  if (!UeMax(br, 31, &s.sps_id)) {
    return fail();
  }
  if (HasChromaInfo(s.profile_idc)) {
    if (!UeMax(br, 3, &s.chroma_format_idc)) {
      return fail();
    }
    if (s.chroma_format_idc == 3) {
      if (!br.Flag(&s.separate_colour_plane)) {
        return fail();
      }
    }
    if (!UeMax(br, 6, &v)) {
      return fail();
    }
    s.bit_depth_luma = v + 8;
    if (!UeMax(br, 6, &v)) {
      return fail();
    }
    s.bit_depth_chroma = v + 8;
    if (!br.Flag(&f)) { // qpprime_y_zero_transform_bypass_flag
      return fail();
    }
    bool scaling = false;
    if (!br.Flag(&scaling)) {
      return fail();
    }
    if (scaling) {
      const uint32_t lists = s.chroma_format_idc != 3 ? 8 : 12;
      for (uint32_t i = 0; i < lists; ++i) {
        bool present = false;
        if (!br.Flag(&present)) {
          return fail();
        }
        if (present && !SkipScalingList(br, i < 6 ? 16 : 64)) {
          return fail();
        }
      }
    }
  }
  if (!UeMax(br, 12, &v)) { // log2_max_frame_num_minus4
    return fail();
  }
  uint32_t poc_type = 0;
  if (!UeMax(br, 2, &poc_type)) {
    return fail();
  }
  if (poc_type == 0) {
    if (!UeMax(br, 12, &v)) { // log2_max_pic_order_cnt_lsb_minus4
      return fail();
    }
  } else if (poc_type == 1) {
    int32_t offset = 0;
    if (!br.Flag(&f) || !br.Se(&offset) || !br.Se(&offset)) {
      return fail();
    }
    uint32_t cycle = 0;
    if (!UeMax(br, 255, &cycle)) {
      return fail();
    }
    for (uint32_t i = 0; i < cycle; ++i) {
      if (!br.Se(&offset)) {
        return fail();
      }
    }
  }
  if (!UeMax(br, 16, &v)) { // max_num_ref_frames
    return fail();
  }
  if (!br.Flag(&f)) { // gaps_in_frame_num_value_allowed_flag
    return fail();
  }
  uint32_t width_mbs = 0;
  uint32_t height_units = 0;
  if (!UeMax(br, kMaxSide / 16 - 1, &width_mbs) || !UeMax(br, kMaxSide / 16 - 1, &height_units)) {
    return fail();
  }
  if (!br.Flag(&s.frame_mbs_only)) {
    return fail();
  }
  if (!s.frame_mbs_only && !br.Flag(&f)) { // mb_adaptive_frame_field_flag
    return fail();
  }
  if (!br.Flag(&f)) { // direct_8x8_inference_flag
    return fail();
  }
  bool cropping = false;
  if (!br.Flag(&cropping)) {
    return fail();
  }
  if (cropping) {
    if (!UeMax(br, kMaxSide, &s.crop_left) || !UeMax(br, kMaxSide, &s.crop_right) ||
        !UeMax(br, kMaxSide, &s.crop_top) || !UeMax(br, kMaxSide, &s.crop_bottom)) {
      return fail();
    }
  }
  if (!br.Flag(&s.vui_present)) {
    return SpsError::kTruncated;
  }

  const uint32_t field_factor = s.frame_mbs_only ? 1 : 2;
  s.coded_width = (width_mbs + 1) * 16;
  s.coded_height = field_factor * (height_units + 1) * 16;
  if (s.coded_height > kMaxSide) {
    return SpsError::kBadValue;
  }
  // H.264 §7.4.2.1.1: CropUnitX/Y from ChromaArrayType.
  const uint32_t chroma_array_type = s.separate_colour_plane ? 0 : s.chroma_format_idc;
  uint32_t unit_x = 1;
  uint32_t unit_y = field_factor;
  if (chroma_array_type != 0) {
    const uint32_t sub_w = (s.chroma_format_idc == 1 || s.chroma_format_idc == 2) ? 2 : 1;
    const uint32_t sub_h = s.chroma_format_idc == 1 ? 2 : 1;
    unit_x = sub_w;
    unit_y = sub_h * field_factor;
  }
  const uint64_t crop_x = uint64_t{unit_x} * (uint64_t{s.crop_left} + s.crop_right);
  const uint64_t crop_y = uint64_t{unit_y} * (uint64_t{s.crop_top} + s.crop_bottom);
  if (crop_x >= s.coded_width || crop_y >= s.coded_height) {
    return SpsError::kBadValue;
  }
  s.width = s.coded_width - static_cast<uint32_t>(crop_x);
  s.height = s.coded_height - static_cast<uint32_t>(crop_y);
  *out = s;
  return SpsError::kOk;
}

std::size_t SetConstraintByte(std::span<const uint8_t> unit, uint8_t value, std::span<uint8_t> out) noexcept {
  // The head: the RBSP (header byte included) up to the first non-zero byte after the constraint byte (index 2),
  // unescaped as UnescapeRbsp does; split is where the rest of the unit starts.
  constexpr std::size_t kHeadMax = 16;
  std::array<uint8_t, kHeadMax> head{};
  std::size_t w = 0;
  std::size_t split = 0;
  int zeros = 0;
  for (std::size_t i = 0; i < unit.size() && split == 0; ++i) {
    const uint8_t b = unit[i];
    if (zeros >= 2 && b == 3) {
      zeros = 0; // emulation_prevention_three_byte
      continue;
    }
    if (w == kHeadMax) {
      return 0;
    }
    head[w++] = b;
    zeros = b == 0 ? zeros + 1 : 0;
    if (w > 3 && b != 0) {
      split = i + 1;
    }
  }
  if (split == 0) {
    return 0;
  }
  head[2] = value;
  // The new head escaped again. It ends in a non-zero byte, as the old head did, and after a non-zero data byte both
  // EscapeRbsp and UnescapeRbsp are back in their first state: the rest of the unit unescapes after the new head
  // exactly as it did after the old one, and no 00 00 0x sequence spans the join.
  std::array<uint8_t, MaxEscapedSize(kHeadMax)> esc{};
  const std::size_t esc_size = EscapeRbsp(std::span<const uint8_t>(head.data(), w), std::span<uint8_t>(esc));
  const std::size_t rest = unit.size() - split;
  if (esc_size + rest > out.size()) {
    return 0;
  }
  std::copy_n(esc.begin(), esc_size, out.begin());
  std::copy(unit.begin() + static_cast<std::ptrdiff_t>(split), unit.end(),
            out.begin() + static_cast<std::ptrdiff_t>(esc_size));
  return esc_size + rest;
}

} // namespace im::h264
