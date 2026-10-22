#include "gpu/cursor_shape.h"

#include <cstddef>

namespace im::gpu {
namespace {

constexpr uint32_t kOpaqueBlack = 0xFF000000u;
constexpr uint32_t kOpaqueWhite = 0xFFFFFFFFu;
constexpr uint32_t kTransparent = 0x00000000u;

uint32_t ReadBgra(const uint8_t *p) noexcept {
  return uint32_t{p[0]} | (uint32_t{p[1]} << 8) | (uint32_t{p[2]} << 16) | (uint32_t{p[3]} << 24);
}

bool Bit(const uint8_t *row, uint32_t x) noexcept { return (row[x / 8] & (0x80u >> (x % 8))) != 0; }

// BT.709 luma (0..255, rounded) of an 0x..RRGGBB value.
uint32_t Luma(uint32_t rgb) noexcept {
  const uint32_t r = (rgb >> 16) & 0xFF;
  const uint32_t g = (rgb >> 8) & 0xFF;
  const uint32_t b = rgb & 0xFF;
  return (2126 * r + 7152 * g + 722 * b + 5000) / 10000;
}

void ConvertMonochrome(const PointerShape &s, std::span<const uint8_t> buf, CursorImage *out) {
  const uint32_t w = s.width;
  const uint32_t h = s.height / 2;
  out->width = w;
  out->height = h;
  out->pixels.assign(static_cast<std::size_t>(w) * h, kTransparent);
  std::vector<uint8_t> invert(static_cast<std::size_t>(w) * h, 0);
  for (uint32_t y = 0; y < h; ++y) {
    const uint8_t *and_row = buf.data() + static_cast<std::size_t>(y) * s.pitch;
    const uint8_t *xor_row = buf.data() + static_cast<std::size_t>(y + h) * s.pitch;
    for (uint32_t x = 0; x < w; ++x) {
      const bool a = Bit(and_row, x);
      const bool v = Bit(xor_row, x);
      uint32_t &px = out->pixels[static_cast<std::size_t>(y) * w + x];
      if (!a) {
        px = v ? kOpaqueWhite : kOpaqueBlack;
      } else if (v) {
        px = kOpaqueBlack;
        invert[static_cast<std::size_t>(y) * w + x] = 1;
      }
    }
  }
  // The outline: transparent pixels next to (8-neighborhood) an inverting one turn white.
  for (uint32_t y = 0; y < h; ++y) {
    for (uint32_t x = 0; x < w; ++x) {
      uint32_t &px = out->pixels[static_cast<std::size_t>(y) * w + x];
      if (px != kTransparent) {
        continue;
      }
      bool adjacent = false;
      for (uint32_t ny = (y == 0 ? 0u : y - 1); ny <= y + 1 && ny < h && !adjacent; ++ny) {
        for (uint32_t nx = (x == 0 ? 0u : x - 1); nx <= x + 1 && nx < w; ++nx) {
          if (invert[static_cast<std::size_t>(ny) * w + nx] != 0) {
            adjacent = true;
            break;
          }
        }
      }
      if (adjacent) {
        px = kOpaqueWhite;
      }
    }
  }
}

void ConvertColor(const PointerShape &s, std::span<const uint8_t> buf, bool masked, CursorImage *out) {
  out->width = s.width;
  out->height = s.height;
  out->pixels.assign(static_cast<std::size_t>(s.width) * s.height, kTransparent);
  for (uint32_t y = 0; y < s.height; ++y) {
    const uint8_t *row = buf.data() + static_cast<std::size_t>(y) * s.pitch;
    for (uint32_t x = 0; x < s.width; ++x) {
      const uint32_t v = ReadBgra(row + static_cast<std::size_t>(x) * 4);
      uint32_t &px = out->pixels[static_cast<std::size_t>(y) * s.width + x];
      if (!masked) {
        px = v;
        continue;
      }
      const uint32_t rgb = v & 0x00FFFFFFu;
      if ((v >> 24) == 0) {
        px = 0xFF000000u | rgb; // replaces the screen
      } else if (rgb == 0) {
        px = kTransparent; // XOR with 0: the screen as it is
      } else {
        px = Luma(rgb ^ 0x00FFFFFFu) < 128 ? kOpaqueBlack : kOpaqueWhite; // inverts: as it would look over white
      }
    }
  }
}

} // namespace

bool ConvertPointerShape(const PointerShape &shape, std::span<const uint8_t> buffer, CursorImage *out) {
  out->width = 0;
  out->height = 0;
  out->pixels.clear();
  if (shape.width == 0 || shape.height == 0) {
    return false;
  }
  if (uint64_t{shape.height} * shape.pitch > buffer.size()) {
    return false;
  }
  switch (shape.type) {
  case kPointerMonochrome:
    if (shape.height % 2 != 0 || uint64_t{shape.pitch} * 8 < shape.width) {
      return false;
    }
    ConvertMonochrome(shape, buffer, out);
    return true;
  case kPointerColor:
  case kPointerMaskedColor:
    if (uint64_t{shape.pitch} < uint64_t{shape.width} * 4) {
      return false;
    }
    ConvertColor(shape, buffer, shape.type == kPointerMaskedColor, out);
    return true;
  default:
    return false;
  }
}

} // namespace im::gpu
