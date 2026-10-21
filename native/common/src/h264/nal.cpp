#include "im/h264/nal.h"

namespace im::h264 {

std::size_t UnescapeRbsp(std::span<const uint8_t> in, std::span<uint8_t> out) noexcept {
  std::size_t w = 0;
  int zeros = 0;
  for (const uint8_t b : in) {
    if (zeros >= 2 && b == 3) {
      zeros = 0; // emulation_prevention_three_byte (H.264 §7.3.1): not data
      continue;
    }
    out[w++] = b;
    zeros = b == 0 ? zeros + 1 : 0;
  }
  return w;
}

std::size_t EscapeRbsp(std::span<const uint8_t> in, std::span<uint8_t> out) noexcept {
  std::size_t w = 0;
  int zeros = 0;
  for (const uint8_t b : in) {
    if (zeros >= 2 && b <= 3) {
      out[w++] = 3;
      zeros = 0;
    }
    out[w++] = b;
    zeros = b == 0 ? zeros + 1 : 0;
  }
  if (w > 0 && out[w - 1] == 0) {
    out[w++] = 3; // H.264 §7.4.1: data that ends in 00 (a cabac_zero_word) gets a final 03
  }
  return w;
}

} // namespace im::h264
