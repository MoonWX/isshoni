// The GPU test pattern (docs/m2/04-windows-engine.md §2 "Test builds", E6; README WA33): "test:pattern" as real
// pixels, drawn by a pixel shader straight into a frame slot, so the whole video path (slots, converters, encoders)
// runs on it without a capture source. It ships in release builds (the smoke tool and isshoni-enginetest need it;
// it captures nothing). The look is pattern_layout.h's.
//
// A session uses it in place of a capture: on the pattern's frame grid (frame k at epoch_ns + k / hz, the instants
// the session's supervisor schedules), FrameSlots::BeginWrite, Render, then FrameSlots::Publish with the frame's
// time, its size and Rotation::kIdentity.
//
// Threads: Create on the engine worker; Render on the thread that writes the slots (it takes the context lock).
#pragma once

#include <memory>

#include "gpu/device.h"
#include "gpu/pattern_layout.h"
#include "gpu/slots.h"
#include "im/video/scale.h"
#include "isshoni_media.h"

namespace im::gpu {

class TestPattern {
public:
  static im_status Create(Device &device, std::unique_ptr<TestPattern> *out) noexcept;

  TestPattern(const TestPattern &) = delete;
  TestPattern &operator=(const TestPattern &) = delete;

  // Draws frame f of a pattern of `size` into the region (0, 0, size) of slot (size ≤ the slot's, each side ≥ 2).
  im_status Render(Slot &slot, video::Size size, const PatternFrame &f) noexcept;

private:
  explicit TestPattern(Device &device) noexcept : device_(device) {}
  im_status Init() noexcept;

  Device &device_;
  ComPtr<ID3D11VertexShader> vs_;
  ComPtr<ID3D11PixelShader> ps_;
  ComPtr<ID3D11RasterizerState> raster_;
  ComPtr<ID3D11DepthStencilState> depth_;
  ComPtr<ID3D11Buffer> constants_;
};

} // namespace im::gpu
