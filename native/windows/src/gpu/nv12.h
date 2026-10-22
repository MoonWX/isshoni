// Our own NV12 pictures and their readback (docs/m2/04-windows-engine.md §8.3's last point, §9.2): a CPU encoder
// (MF software, OpenH264, or a hardware one without zero copy) gets its frame from our own NV12 surface, read back
// through a staging texture (CopyResource, Map with D3D11_MAP_READ) into the encoder's planes. Hardware encoders
// write nothing of this: the converters render into their FFmpeg pool's textures directly.
//
// On a device that can't render to NV12 (Device::nv12_render_target(): an optional D3D11 capability that hardware
// drivers report, checked rather than assumed for WARP) the surface is two plane textures, R8_UNORM and R8G8_UNORM,
// which the shader converter writes and the readback copies the same way.
//
// The readback never holds the context lock across a wait for the GPU (04 §6.1, §8.1): it submits the copies and an
// event query under the lock, then polls the query, taking the lock only for each GetData and yielding between polls,
// so the capture copy and FFmpeg's submissions go on while the GPU works through its queue. Map then runs with
// D3D11_MAP_FLAG_DO_NOT_WAIT (polled the same way), and the rows are copied out with the lock released.
//
// Threads: Create on any thread; Read on the video thread, without the context lock held (Read takes it for each D3D
// call, never while it waits for the GPU; a caller holding it would make everyone else wait for the GPU with it).
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

  // How long Read waits for the GPU at most: longer than Windows' GPU timeout (TDR, 2 s by default), after which a hung
  // device is removed and the wait ends as device loss instead.
  static constexpr uint32_t kGpuWaitLimitMs = 3000;

  // Copies src (an NV12 texture or slice, or a planar target: R8_UNORM of this size and R8G8_UNORM of half of it) into
  // dst, once the GPU has finished the copy. IM_ERR_INVALID_ARG for a source of another size, format or layout;
  // device loss → IM_ERR_ENCODER_LOST (device_removed); a GPU that doesn't finish within kGpuWaitLimitMs →
  // IM_ERR_INTERNAL.
  im_status Read(const Nv12Target &src, const Nv12Planes &dst) noexcept;

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
  // gpu_warp_test: hook(ctx) runs once per Read on its thread, after the copies were submitted and before it waits for
  // them, with the context lock released. Null removes it.
  void SetWaitHookForTest(void (*hook)(void *), void *ctx) noexcept {
    wait_hook_ = hook;
    wait_hook_ctx_ = ctx;
  }
#endif

private:
  explicit Nv12Readback(Device &device) noexcept : device_(device) {}
  enum class Plane : uint8_t {
    kNv12,   // nv12_: the Y rows, then the Cb/Cr rows at RowPitch × height
    kLuma,   // luma_: the Y rows
    kChroma, // chroma_: the Cb/Cr rows
  };
  // Waits until the GPU has finished every command up to query_ (ended and flushed before).
  im_status WaitForCopies() noexcept;
  // Maps a staging texture whose copy has finished, copies its rows into dst with the lock released, and unmaps it.
  im_status ReadStaging(Plane plane, const Nv12Planes &dst) noexcept;

  Device &device_;
  video::Size size_;
  ComPtr<ID3D11Texture2D> nv12_;
  ComPtr<ID3D11Texture2D> luma_;
  ComPtr<ID3D11Texture2D> chroma_;
  ComPtr<ID3D11Query> query_; // D3D11_QUERY_EVENT: signaled when the GPU has finished the copies before it
#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
  void (*wait_hook_)(void *) = nullptr;
  void *wait_hook_ctx_ = nullptr;
#endif
};

} // namespace im::gpu
