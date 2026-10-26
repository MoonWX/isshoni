// A session's D3D11 device and its context lock (docs/m2/04-windows-engine.md §8.1).
//
// - D3D11CreateDevice on the requested adapter (the chosen encoder's, §9.1; WARP for tests) with BGRA and video
//   support and feature levels {11_1, 11_0}. A device that refuses D3D11_CREATE_DEVICE_VIDEO_SUPPORT is created
//   without it: it has no VideoProcessor, so the converters use the shader (§8.3). ID3D11Multithread's protection is
//   on as a safety net.
// - One device per session. The immediate context is shared by the capture copy, the video thread and FFmpeg, so
//   every use of it holds the context lock: a recursive CRITICAL_SECTION (FFmpeg may take it again while holding it),
//   held only around D3D calls and never across a wait (§6.1). FFmpeg gets it through AVD3D11VADeviceContext.lock /
//   unlock / lock_ctx = LockCallback / UnlockCallback / lock_ctx() (§9.2). The device outlives everything that uses
//   the lock (encoders, frame pools, converters).
// - Device loss: Fail maps DXGI_ERROR_DEVICE_REMOVED / _RESET, or any failure while GetDeviceRemovedReason() is not
//   S_OK, to IM_ERR_ENCODER_LOST with token device_removed, which ends the session (§9.8); other failures are
//   IM_ERR_INTERNAL.
//
// Threads: Create runs on the engine worker (im_start) or a test's thread; the ID3D11Device is free-threaded; the
// context, and the video interfaces on it, only under the lock.
#pragma once

#include <windows.h>

#include <d3d11_4.h>
#include <dxgi1_4.h>
#include <wrl/client.h>

#include <cstdint>
#include <memory>

#include "isshoni_media.h"

namespace im::gpu {

using Microsoft::WRL::ComPtr;

inline constexpr uint32_t kVendorMicrosoft = 0x1414;
inline constexpr uint32_t kBasicRenderDriverDevice = 0x8C; // WARP as a display adapter (VMs without a GPU driver)

struct DeviceRequest {
  enum class Adapter : uint8_t {
    kDefault, // the first adapter DXGI lists (the one with the primary output)
    kLuid,    // the adapter with luid (im_adapter_info.luid: (HighPart << 32) | LowPart)
    kWarp,    // D3D_DRIVER_TYPE_WARP, the software rasterizer (CI, gpu_warp_test)
  };
  Adapter adapter = Adapter::kDefault;
  uint64_t luid = 0;
  bool video_support = true; // ask for D3D11_CREATE_DEVICE_VIDEO_SUPPORT (retried without it when refused)
};

class Device {
public:
  // Creates the device. Failures set the thread's detail: IM_ERR_INTERNAL (no such adapter, or creation failed).
  static im_status Create(const DeviceRequest &request, std::unique_ptr<Device> *out) noexcept;

  ~Device();
  Device(const Device &) = delete;
  Device &operator=(const Device &) = delete;

  ID3D11Device *d3d() const noexcept { return device_.Get(); }
  // The immediate context: only while holding the lock.
  ID3D11DeviceContext *context() const noexcept { return context_.Get(); }
  // The video interfaces (null when the device has no video support); the context one only under the lock.
  ID3D11VideoDevice *video_device() const noexcept { return video_device_.Get(); }
  ID3D11VideoContext1 *video_context() const noexcept { return video_context_.Get(); }

  uint64_t luid() const noexcept { return luid_; }
  uint32_t vendor_id() const noexcept { return vendor_id_; }
  uint32_t device_id() const noexcept { return device_id_; }
  D3D_FEATURE_LEVEL feature_level() const noexcept { return level_; }
  // WARP: created as D3D_DRIVER_TYPE_WARP, or the Basic Render Driver adapter.
  bool warp() const noexcept { return warp_; }
  // Whether NV12 textures can be render targets (R8 / R8G8 views of the planes): the shader converter and the
  // VideoProcessor write NV12 only then; our own surfaces fall back to two plane textures (nv12.h).
  bool nv12_render_target() const noexcept { return nv12_render_target_; }

  // The context lock (recursive).
  void Lock() noexcept;
  void Unlock() noexcept;
  // FFmpeg's AVD3D11VADeviceContext.lock / unlock, with lock_ctx = lock_ctx().
  static void LockCallback(void *ctx) noexcept;
  static void UnlockCallback(void *ctx) noexcept;
  void *lock_ctx() noexcept { return this; }

  // Whether the device was removed or reset (GetDeviceRemovedReason() != S_OK).
  bool Removed() const noexcept;
  // The status for a failed D3D call `what`: IM_ERR_ENCODER_LOST (device_removed) on device loss, else IM_ERR_INTERNAL;
  // sets the thread's detail.
  im_status Fail(HRESULT hr, const char *what) const noexcept;

private:
  Device() noexcept;

  CRITICAL_SECTION lock_;
  ComPtr<ID3D11Device> device_;
  ComPtr<ID3D11DeviceContext> context_;
  ComPtr<ID3D11VideoDevice> video_device_;
  ComPtr<ID3D11VideoContext1> video_context_;
  D3D_FEATURE_LEVEL level_ = D3D_FEATURE_LEVEL_11_0;
  uint64_t luid_ = 0;
  uint32_t vendor_id_ = 0;
  uint32_t device_id_ = 0;
  bool warp_ = false;
  bool nv12_render_target_ = false;
};

// Holds the context lock for a scope.
class ContextLock {
public:
  explicit ContextLock(Device &device) noexcept : device_(device) { device_.Lock(); }
  ~ContextLock() { device_.Unlock(); }
  ContextLock(const ContextLock &) = delete;
  ContextLock &operator=(const ContextLock &) = delete;

private:
  Device &device_;
};

} // namespace im::gpu
