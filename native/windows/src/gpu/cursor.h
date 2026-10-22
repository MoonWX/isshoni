// The pointer blend for Desktop Duplication (docs/m2/04-windows-engine.md §7.8): DD's desktop image has no pointer,
// so the video thread draws it into the slot copy (never into the desktop texture) with a textured quad and straight
// alpha blending (precompiled shaders cursor_vs/cursor_ps), before the converters read the slot. The quad goes at the
// pointer's position turned for the slot's rotation, so it comes out upright. WGC draws its own pointer
// (IsCursorCaptureEnabled), and with IM_START_HIDE_CURSOR nothing is drawn at all.
//
// The shape comes from the DD thread, converted on the CPU (cursor_shape.h), into a 256×256 dynamic texture; a larger
// shape is cut to that size.
//
// Threads: Create on the engine worker; SetShape on any thread (the DD thread); Blend on the video thread (it takes the
// context lock).
#pragma once

#include <cstdint>
#include <memory>
#include <mutex>

#include "gpu/cursor_shape.h"
#include "gpu/device.h"
#include "gpu/slots.h"
#include "isshoni_media.h"

namespace im::gpu {

class CursorOverlay {
public:
  static constexpr uint32_t kMaxSide = 256; // the shape texture

  // start_flags: im_start_config.flags; IM_START_HIDE_CURSOR makes the overlay draw nothing (04 §7.8).
  static im_status Create(Device &device, uint32_t start_flags, std::unique_ptr<CursorOverlay> *out) noexcept;

  CursorOverlay(const CursorOverlay &) = delete;
  CursorOverlay &operator=(const CursorOverlay &) = delete;

  bool hidden() const noexcept { return hidden_; }

  // A new pointer shape (copied; the mutex is held for the copy only). Ignored when hidden.
  void SetShape(const CursorImage &image);

  // Draws slot.meta.cursor into the slot unless it is drawn already (slot.cursor_blended), the overlay is hidden, the
  // pointer is invisible or no shape came yet. Sets slot.cursor_blended when it drew.
  im_status Blend(Slot &slot) noexcept;

private:
  explicit CursorOverlay(Device &device, bool hidden) noexcept : device_(device), hidden_(hidden) {}
  im_status Init() noexcept;
  im_status Upload() noexcept; // the pending shape into the texture (under the context lock)

  Device &device_;
  const bool hidden_;
  ComPtr<ID3D11Texture2D> shape_;
  ComPtr<ID3D11ShaderResourceView> shape_srv_;
  ComPtr<ID3D11VertexShader> vs_;
  ComPtr<ID3D11PixelShader> ps_;
  ComPtr<ID3D11SamplerState> sampler_;
  ComPtr<ID3D11BlendState> blend_;
  ComPtr<ID3D11RasterizerState> raster_;
  ComPtr<ID3D11DepthStencilState> depth_;
  ComPtr<ID3D11Buffer> constants_;

  std::mutex mu_;
  CursorImage pending_;     // guarded by mu_
  uint64_t pending_id_ = 0; // guarded by mu_: bumped by every SetShape

  // The video thread's.
  uint64_t uploaded_id_ = 0;
  uint32_t width_ = 0; // the uploaded shape's size (0: none yet)
  uint32_t height_ = 0;
};

} // namespace im::gpu
