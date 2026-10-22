// The converters' geometry (docs/m2/04-windows-engine.md §8.3, §7.7, §7.9): where a frame's content goes in a layer's
// NV12 output, how an output pixel finds its source texels (rotation, scale, filter), and the constants the shader
// converter gets (shaders/convert.hlsli). Portable C++ without OS headers: the shader converter, the VideoProcessor
// and the CPU model of cpu_model.h all take their numbers from here, so they agree by construction.
//
// Coordinates. A frame slot (slots.h) is a BGRA texture of size `texture`; the frame's content is the region
// (0, 0, content) of it, in the orientation the capture delivered (Desktop Duplication: the panel's; WGC and the test
// pattern: upright). `rotation` turns that region upright. A layer's output (im::video::OutputSize) holds the upright
// content in its rectangle `dest`; the rest of the output is black (Y 16, Cb/Cr 128).
#pragma once

#include <cstdint>

#include "im/video/scale.h"

namespace im::gpu {

// How a captured region is turned to show the desktop upright: k90 turns it 90° clockwise, as
// D3D11_VIDEO_PROCESSOR_ROTATION_90 does and as DXGI_MODE_ROTATION_ROTATE90 asks (04 §7.9).
enum class Rotation : uint8_t { kIdentity = 0, k90 = 1, k180 = 2, k270 = 3 };

// DXGI_OUTDUPL_DESC.Rotation (DXGI_MODE_ROTATION_*): 2 → k90, 3 → k180, 4 → k270; 0 (unspecified), 1 (identity) and
// unknown values → kIdentity.
Rotation RotationFromDxgi(uint32_t dxgi_mode_rotation) noexcept;

// The upright size of a region of size `region` (in the capture's orientation): the sides swap for k90 and k270.
video::Size Upright(video::Size region, Rotation r) noexcept;

struct Point {
  double x = 0;
  double y = 0;
};

// A point of the upright picture, normalized to [0, 1]², to the same point of the captured region, normalized to
// [0, 1]². k90: (u, v) → (v, 1 − u); k180: (1 − u, 1 − v); k270: (1 − v, u).
Point UprightToRegion(Rotation r, Point upright) noexcept;

// The same in pixels: a point (continuous coordinates, pixel centers at +0.5) of the upright picture of a region of
// size `region` (capture orientation) to the region's pixel coordinates. The cursor's quad uses it (04 §7.8).
Point UprightToRegionPixels(Rotation r, video::Size region, Point upright_px) noexcept;

// How the converters filter (04 §8.3): bilinear, or for a layer downscaled by 2 or more on both axes (the preview) a
// 2×2 box over bilinear taps: the mean of four bilinear taps half a source texel to each side of the output pixel's
// center. At exactly 2:1 that is the 2×2 box; at 3:1 it is a (1 2 1)² tent over 3×3 texels.
enum class Filter : uint8_t { kBilinear, kBox };

// One conversion's geometry.
struct ConvertGeometry {
  video::Size output;  // the NV12 picture: even, each side 64..4096
  video::Rect dest;    // where the upright content goes (zero size: the output is all black)
  video::Size texture; // the slot texture
  video::Size content; // the content region (0, 0, content) of the texture, in the capture's orientation
  Rotation rotation = Rotation::kIdentity;
  Filter filter = Filter::kBilinear;
};

// The geometry of converting `content` (a region of a texture of size `texture`, turned upright by `rotation`) into
// a layer whose output size was computed for an upright source of size `layer_source` (im::video::ComputeOutputSize).
// While the upright content has that size, it goes into layer.content; a content of another size (a window resized
// and not yet re-initialized, 04 §7.7) is fitted into the whole output with its aspect kept
// (im::video::FitRect). content is clamped to texture.
ConvertGeometry ComputeGeometry(const video::OutputSize &layer, video::Size layer_source, video::Size texture,
                                video::Size content, Rotation rotation) noexcept;

// The constant buffer of the shader converter (shaders/convert.hlsli, register b0): 48 bytes, laid out as HLSL packs
// it (each float4 / float2 pair / uint group on 16-byte rows).
struct ConvertConstants {
  float dest[4];      // x, y, width, height of `dest` in output (luma) pixels
  float src_scale[2]; // content / texture: the content region in normalized texture coordinates
  float texel[2];     // 1 / texture: one texel in normalized texture coordinates
  uint32_t rotation;  // Rotation's value
  uint32_t box;       // 1 for Filter::kBox
  uint32_t pad[2];
};
static_assert(sizeof(ConvertConstants) == 48, "the cbuffer is three 16-byte rows");

ConvertConstants MakeConvertConstants(const ConvertGeometry &g) noexcept;

} // namespace im::gpu
