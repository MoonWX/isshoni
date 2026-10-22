// The pictures the GPU path passes around (docs/m2/04-windows-engine.md §8.2, §8.3): a captured frame in a BGRA slot
// texture (what the converters read) and an NV12 picture (what they write, and what encoders take).
#pragma once

#include <windows.h>

#include <d3d11.h>

#include <cstdint>

#include "gpu/geometry.h"
#include "im/video/scale.h"

namespace im::gpu {

// A frame as the converters read it: the region (0, 0, content) of a BGRA texture (DXGI_FORMAT_B8G8R8A8_UNORM,
// D3D11_BIND_SHADER_RESOURCE), turned upright by rotation. texture_id is unique per texture for the process's
// lifetime (a cache key that a new texture at a freed address can't alias).
struct SourceFrame {
  ID3D11Texture2D *texture = nullptr;
  ID3D11ShaderResourceView *srv = nullptr;
  uint64_t texture_id = 0;
  video::Size texture_size;
  video::Size content;
  Rotation rotation = Rotation::kIdentity;
};

// A new texture_id (thread-safe, never 0).
uint64_t NewTextureId() noexcept;

// Where a converter writes a layer's NV12 picture (its size: the texture's, which must be the layer's output size):
// - an NV12 texture (DXGI_FORMAT_NV12 with D3D11_BIND_RENDER_TARGET), or one array slice of one (FFmpeg's hardware
//   frame pool, 04 §9.2); or
// - the two planes as an R8_UNORM texture (luma, the full size) and an R8G8_UNORM texture (Cb/Cr, half size), on a
//   device that can't render to NV12 (Device::nv12_render_target(); our own surfaces only, nv12.h).
struct Nv12Target {
  ID3D11Texture2D *texture = nullptr;
  uint32_t array_slice = 0;
  ID3D11Texture2D *luma = nullptr;
  ID3D11Texture2D *chroma = nullptr;

  bool planar() const noexcept { return texture == nullptr; }
};

} // namespace im::gpu
