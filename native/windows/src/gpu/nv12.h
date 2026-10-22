// Our own NV12 pictures and their readback (docs/m2/04-windows-engine.md §8.3's last point, §9.2): a CPU encoder
// (MF software, OpenH264, or a hardware one without zero copy) gets its frame from our own NV12 surface, read back
// through a staging texture (CopyResource, Map with D3D11_MAP_READ) into the encoder's planes. Hardware encoders
// write nothing of this: the converters render into their FFmpeg pool's textures directly.
//
// On a device that can't render to NV12 (Device::nv12_render_target(): an optional D3D11 capability that hardware
// drivers report, checked rather than assumed for WARP) the surface is two plane textures, R8_UNORM and R8G8_UNORM,
// which the shader converter writes and the readback copies the same way.
//
// Threads: Create on any thread; Read on the video thread (it takes the context lock, and Map waits for the GPU).
#pragma once

#include <cstddef>
#include <cstdint>
#include <memory>

#include "gpu/device.h"
#include "gpu/frame.h"
#include "im/video/scale.h"
#include "isshoni_media.h"

namespace im::gpu {

class Nv12Surface {
public:
  // An NV12 picture of size (even sides) the converters can render to.
  static im_status Create(Device &device, video::Size size, std::unique_ptr<Nv12Surface> *out) noexcept;

  Nv12Surface(const Nv12Surface &) = delete;
  Nv12Surface &operator=(const Nv12Surface &) = delete;

  Nv12Target target() const noexcept { return Nv12Target{nv12_.Get(), 0, luma_.Get(), chroma_.Get()}; }
  video::Size size() const noexcept { return size_; }
  bool planar() const noexcept { return nv12_.Get() == nullptr; }

private:
  Nv12Surface() = default;

  video::Size size_;
  ComPtr<ID3D11Texture2D> nv12_;
  ComPtr<ID3D11Texture2D> luma_;
  ComPtr<ID3D11Texture2D> chroma_;
};

// Where a readback writes: the Y plane (width × height bytes) and the interleaved Cb/Cr plane (width × height/2 bytes),
// each row `stride` bytes apart (an AVFrame's data[0], data[1] and linesize[0], linesize[1]).
struct Nv12Planes {
  uint8_t *y = nullptr;
  std::size_t y_stride = 0;
  uint8_t *uv = nullptr;
  std::size_t uv_stride = 0;
};

class Nv12Readback {
public:
  // Staging textures for pictures of size, NV12 or (planar) two planes like Nv12Surface's.
  static im_status Create(Device &device, video::Size size, bool planar, std::unique_ptr<Nv12Readback> *out) noexcept;

  Nv12Readback(const Nv12Readback &) = delete;
  Nv12Readback &operator=(const Nv12Readback &) = delete;

  // Copies src (an NV12 texture or slice, or a planar target, of this size) into dst. Waits for the GPU.
  im_status Read(const Nv12Target &src, const Nv12Planes &dst) noexcept;

private:
  explicit Nv12Readback(Device &device) noexcept : device_(device) {}

  Device &device_;
  video::Size size_;
  ComPtr<ID3D11Texture2D> nv12_;
  ComPtr<ID3D11Texture2D> luma_;
  ComPtr<ID3D11Texture2D> chroma_;
};

} // namespace im::gpu
