// Desktop Duplication pointer shapes → a BGRA image with straight alpha (docs/m2/04-windows-engine.md §7.8), on the
// CPU, so the video thread only blends a texture (cursor.h). Portable C++; README WA60's DD thread calls it with what
// IDXGIOutputDuplication::GetFramePointerShape returned.
//
// - Color shapes (DXGI_OUTDUPL_POINTER_SHAPE_TYPE_COLOR, 32 bpp BGRA): as they are, alpha straight.
// - Masked-color shapes (…_MASKED_COLOR, 32 bpp, the alpha byte a mask): mask 0 → the RGB replaces the screen (opaque);
//   mask 0xFF → the RGB is XORed onto the screen. XOR with 0 changes nothing (transparent); any other value inverts,
//   which a blend can't do: the pixel is drawn opaque, black or white by luminance as it would look over white (the
//   inverted value's BT.709 luma below 128 → black, else white).
// - Monochrome shapes (…_MONOCHROME, 1 bpp, an AND mask over an XOR mask, so the reported height is twice the
//   pointer's): AND 0 XOR 0 → black, AND 0 XOR 1 → white, AND 1 XOR 0 → transparent, AND 1 XOR 1 ("invert", the
//   I-beam's) → black with a one-pixel white outline drawn on the transparent pixels around it.
#pragma once

#include <cstdint>
#include <span>
#include <vector>

namespace im::gpu {

// DXGI_OUTDUPL_POINTER_SHAPE_TYPE_*.
inline constexpr uint32_t kPointerMonochrome = 1;
inline constexpr uint32_t kPointerColor = 2;
inline constexpr uint32_t kPointerMaskedColor = 4;

// What DXGI_OUTDUPL_POINTER_SHAPE_INFO says about a shape buffer.
struct PointerShape {
  uint32_t type = 0;   // kPointer*
  uint32_t width = 0;  // pixels
  uint32_t height = 0; // rows in the buffer (monochrome: AND rows + XOR rows)
  uint32_t pitch = 0;  // bytes per row
};

// A pointer image: width × height pixels as 0xAARRGGBB (B, G, R, A in memory), alpha straight.
struct CursorImage {
  uint32_t width = 0;
  uint32_t height = 0;
  std::vector<uint32_t> pixels;
};

// Converts a shape. False, with out emptied, for an unknown type, a zero size, an odd monochrome height, a pitch too
// small for the width or a buffer shorter than height × pitch.
bool ConvertPointerShape(const PointerShape &shape, std::span<const uint8_t> buffer, CursorImage *out);

} // namespace im::gpu
