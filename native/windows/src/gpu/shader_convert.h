// The shader converter (docs/m2/04-windows-engine.md §8.3): BGRA → NV12 in two pixel-shader passes, the Y plane through
// an R8_UNORM render target view and the half-size Cb/Cr plane through an R8G8_UNORM one, with the filter, rotation
// and letterbox of geometry.h and the color of cpu_model.h (shaders/convert.hlsli). It is the fallback when the
// VideoProcessor can't be created or fails, and the reference CI compares the VideoProcessor with on WARP.
//
// Threads: Create on any thread; Convert on the video thread (it takes the context lock).
#pragma once

#include <cstddef>
#include <cstdint>
#include <memory>
#include <vector>

#include "gpu/device.h"
#include "gpu/frame.h"
#include "gpu/geometry.h"
#include "isshoni_media.h"

namespace im::gpu {

class ShaderConverter {
public:
  static im_status Create(Device &device, std::unique_ptr<ShaderConverter> *out) noexcept;

  ShaderConverter(const ShaderConverter &) = delete;
  ShaderConverter &operator=(const ShaderConverter &) = delete;

  // Converts src into dst by g (g.output must be dst's size). Device loss → IM_ERR_ENCODER_LOST (device_removed).
  im_status Convert(const SourceFrame &src, const ConvertGeometry &g, const Nv12Target &dst) noexcept;

  // Forgets the cached render target views (and the references they hold on their textures): after the encoder's
  // frame pool was replaced.
  void ClearViews() noexcept;

private:
  // The render target views of one target (an NV12 texture's slice, or a pair of plane textures).
  struct Views {
    ID3D11Texture2D *texture = nullptr; // the key: the NV12 texture, or the luma texture of a planar target
    uint32_t slice = 0;
    ComPtr<ID3D11RenderTargetView> luma;
    ComPtr<ID3D11RenderTargetView> chroma;
    uint32_t width = 0; // the luma plane's size
    uint32_t height = 0;
  };
  static constexpr std::size_t kMaxViews = 16; // FFmpeg's pools hold 6–8 frames per layer (04 §9.2)

  explicit ShaderConverter(Device &device) noexcept : device_(device) {}
  im_status Init() noexcept;
  im_status ViewsFor(const Nv12Target &dst, const Views **out) noexcept;

  Device &device_;
  ComPtr<ID3D11VertexShader> vs_;
  ComPtr<ID3D11PixelShader> ps_luma_;
  ComPtr<ID3D11PixelShader> ps_chroma_;
  ComPtr<ID3D11SamplerState> sampler_;
  ComPtr<ID3D11RasterizerState> raster_;
  ComPtr<ID3D11DepthStencilState> depth_;
  ComPtr<ID3D11Buffer> constants_;
  std::vector<Views> views_; // the most recently used last
};

// Pipeline state both drawing parts of the GPU path set up the same way (shader_convert.cpp).
namespace detail {
im_status CreateCommonStates(Device &device, ComPtr<ID3D11RasterizerState> *raster,
                             ComPtr<ID3D11DepthStencilState> *depth) noexcept;
im_status CreateConstantBuffer(Device &device, uint32_t bytes, ComPtr<ID3D11Buffer> *out) noexcept;
// Writes bytes into a D3D11_USAGE_DYNAMIC constant buffer (under the context lock).
im_status UpdateConstants(Device &device, ID3D11Buffer *buffer, const void *data, std::size_t bytes) noexcept;
// Clears the stages the GPU path doesn't use, so state another user of the context left can't break a draw.
void ResetUnusedStages(ID3D11DeviceContext *ctx) noexcept;
} // namespace detail

} // namespace im::gpu
