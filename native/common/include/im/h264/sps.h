// The H.264 sequence parameter set parser (docs/m2/04-windows-engine.md §9.4): profile_idc, the constraint byte,
// level_idc and the picture size after cropping, which fill im_packet.profile, width and height. It reads
// seq_parameter_set_data (H.264 §7.3.2.1.1) up to frame_cropping; VUI is not needed. Bounded and allocation-free:
// the RBSP is unescaped into a fixed buffer, and every Exp-Golomb value is range-checked.
#pragma once

#include <cstddef>
#include <cstdint>
#include <span>

namespace im::h264 {

// The largest SPS NAL unit the parser (and the normalizer's cache) takes.
inline constexpr std::size_t kMaxSpsBytes = 4096;

// profile_idc values.
inline constexpr uint8_t kProfileBaseline = 66;
inline constexpr uint8_t kProfileMain = 77;
inline constexpr uint8_t kProfileHigh = 100;

struct Sps {
  uint8_t profile_idc = 0;
  uint8_t constraint_flags = 0; // the byte after profile_idc: constraint_set0..5 flags and reserved_zero_2bits
  uint8_t level_idc = 0;
  uint32_t sps_id = 0;
  uint32_t chroma_format_idc = 1;
  bool separate_colour_plane = false;
  uint32_t bit_depth_luma = 8;
  uint32_t bit_depth_chroma = 8;
  bool frame_mbs_only = true;
  uint32_t coded_width = 0; // macroblocks × 16
  uint32_t coded_height = 0;
  uint32_t crop_left = 0, crop_right = 0, crop_top = 0, crop_bottom = 0; // frame_crop_*_offset
  uint32_t width = 0;                                                    // after cropping
  uint32_t height = 0;
  bool vui_present = false;

  // The key im_packet.profile and the SFU's table use: profile_idc << 8 | constraint byte (0x6400, 0x42E0).
  uint32_t profile_key() const noexcept { return (uint32_t{profile_idc} << 8) | constraint_flags; }
};

enum class SpsError : uint8_t {
  kOk,
  kNotSps,    // not a NAL unit of type 7, or forbidden_zero_bit set
  kTooLarge,  // more than kMaxSpsBytes
  kTruncated, // the data ends before frame_cropping
  kBadValue,  // a value outside its range (ids, bit depths, sizes, a crop larger than the picture)
};

// Parses an SPS NAL unit (header byte included, emulation-prevention bytes in place).
SpsError ParseSps(std::span<const uint8_t> unit, Sps *out) noexcept;

// Reads RBSP bits MSB first, with Exp-Golomb codes (H.264 §9.1).
class BitReader {
public:
  explicit BitReader(std::span<const uint8_t> rbsp) noexcept : data_(rbsp) {}
  // n <= 32. False (and nothing read) when fewer than n bits are left (then overrun() is true).
  bool Bits(uint32_t n, uint32_t *v) noexcept;
  bool Flag(bool *v) noexcept;
  // ue(v): values 0 .. 2^32 - 2; false for a code longer than 32 bits or past the end.
  bool Ue(uint32_t *v) noexcept;
  // se(v): values -2^31 + 1 .. 2^31 - 1.
  bool Se(int32_t *v) noexcept;
  std::size_t bits_left() const noexcept { return data_.size() * 8 - pos_; }
  // Whether a read failed because the data ended.
  bool overrun() const noexcept { return overrun_; }

private:
  std::span<const uint8_t> data_;
  std::size_t pos_ = 0;
  bool overrun_ = false;
};

} // namespace im::h264
