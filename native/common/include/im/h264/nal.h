// H.264 NAL unit basics for the access-unit normalizer and the SPS parser (docs/m2/04-windows-engine.md §9.4): NAL
// types, Annex B splitting and the emulation-prevention (RBSP) transforms. Pure functions over caller memory; nothing
// allocates.
#pragma once

#include <cstddef>
#include <cstdint>
#include <span>

namespace im::h264 {

// nal_unit_type (ITU-T H.264 Table 7-1), the ones the engine names.
namespace nal {
inline constexpr uint8_t kSlice = 1;    // non-IDR slice
inline constexpr uint8_t kSliceDpa = 2; // data partitions A..C
inline constexpr uint8_t kSliceDpb = 3;
inline constexpr uint8_t kSliceDpc = 4;
inline constexpr uint8_t kIdr = 5; // IDR slice: KEYFRAME
inline constexpr uint8_t kSei = 6; // dropped
inline constexpr uint8_t kSps = 7;
inline constexpr uint8_t kPps = 8;
inline constexpr uint8_t kAud = 9;          // dropped
inline constexpr uint8_t kEndOfSeq = 10;    // dropped
inline constexpr uint8_t kEndOfStream = 11; // dropped
inline constexpr uint8_t kFiller = 12;      // dropped
inline constexpr uint8_t kSpsExt = 13;
inline constexpr uint8_t kPrefix = 14;
inline constexpr uint8_t kSubsetSps = 15;
inline constexpr uint8_t kAuxSlice = 19;
inline constexpr uint8_t kSliceExt = 20;
} // namespace nal

// The type of a NAL unit (its first byte's low 5 bits); 0 for an empty one.
constexpr uint8_t NalType(std::span<const uint8_t> nal) noexcept { return nal.empty() ? 0 : (nal[0] & 0x1F); }

// Whether a NAL unit of this type is a slice of the primary coded picture (1..5).
constexpr bool IsVcl(uint8_t type) noexcept { return type >= nal::kSlice && type <= nal::kIdr; }

// Splits an Annex B byte stream into NAL units: units start after a 3-byte (00 00 01) or 4-byte (00 00 00 01) start
// code and end at the next start code; trailing zero bytes (a 4-byte code's leading zero, trailing_zero_8bits) are not
// part of a unit, and empty units are skipped. Bytes before the first start code are not a unit. Calls fn(span) for
// each unit, in order, until fn returns false; returns the number of bytes before the first start code that aren't
// zero bytes in front of it (0 when the stream starts with a start code; the whole size when it has none).
template <class Fn> std::size_t ForEachNal(std::span<const uint8_t> stream, Fn &&fn);

// Removes emulation-prevention bytes (the 03 of every 00 00 03 sequence) from a NAL unit. out must hold in.size()
// bytes; returns the bytes written.
std::size_t UnescapeRbsp(std::span<const uint8_t> in, std::span<uint8_t> out) noexcept;

// The worst-case size EscapeRbsp can produce for n bytes.
constexpr std::size_t MaxEscapedSize(std::size_t n) noexcept { return n + n / 2 + 1; }

// Inserts emulation-prevention bytes so that no 00 00 0x (x <= 3) sequence appears and the unit doesn't end in 00.
// out must hold MaxEscapedSize(in.size()) bytes; returns the bytes written. EscapeRbsp(UnescapeRbsp(x)) == x for a
// canonically escaped unit.
std::size_t EscapeRbsp(std::span<const uint8_t> in, std::span<uint8_t> out) noexcept;

// --------------------------------------------------------------------------------------------------- template body

namespace detail {
// The position of the next 00 00 01 at or after from, or stream.size().
inline std::size_t FindStartCode(std::span<const uint8_t> s, std::size_t from) noexcept {
  const std::size_t n = s.size();
  std::size_t i = from;
  while (i + 2 < n) {
    if (s[i + 2] > 1) {
      i += 3; // s[i + 2] can't be part of a start code ending at i + 2, nor can the two before it
    } else if (s[i + 2] == 1 && s[i] == 0 && s[i + 1] == 0) {
      return i;
    } else {
      ++i;
    }
  }
  return n;
}
} // namespace detail

template <class Fn> std::size_t ForEachNal(std::span<const uint8_t> stream, Fn &&fn) {
  const std::size_t n = stream.size();
  std::size_t sc = detail::FindStartCode(stream, 0);
  std::size_t leading = sc;
  if (sc < n) {
    while (leading > 0 && stream[leading - 1] == 0) {
      --leading; // a 4-byte start code's zero_byte, or leading_zero_8bits
    }
  }
  while (sc < n) {
    const std::size_t begin = sc + 3;
    const std::size_t next = detail::FindStartCode(stream, begin);
    std::size_t end = next;
    while (end > begin && stream[end - 1] == 0) {
      --end;
    }
    if (end > begin && !fn(stream.subspan(begin, end - begin))) {
      break;
    }
    sc = next;
  }
  return leading;
}

} // namespace im::h264
