#include "gpu/cursor.h"

#include <algorithm>
#include <cstring>
#include <utility>

#include "api/guard.h"
#include "gpu/geometry.h"
#include "gpu/shader_convert.h"
#include "im/abi/status.h"
#include "im/log/ring.h"

// Bytecode compiled by fxc at build time (native/windows/CMakeLists.txt): const BYTE g_<name>[].
#include "shaders/cursor_ps.h"
#include "shaders/cursor_vs.h"

namespace im::gpu {

im_status CursorOverlay::Create(Device &device, uint32_t start_flags, std::unique_ptr<CursorOverlay> *out) noexcept {
  out->reset();
  return api::Guard("gpu cursor", [&]() -> im_status {
    std::unique_ptr<CursorOverlay> c(new CursorOverlay(device, (start_flags & IM_START_HIDE_CURSOR) != 0));
    if (!c->hidden_) {
      if (const im_status st = c->Init(); st != IM_OK) {
        return st;
      }
      c->pending_.pixels.reserve(static_cast<std::size_t>(kMaxSide) * kMaxSide);
    }
    *out = std::move(c);
    return IM_OK;
  });
}

im_status CursorOverlay::Init() noexcept {
  ID3D11Device *d = device_.d3d();
  D3D11_TEXTURE2D_DESC td{};
  td.Width = kMaxSide;
  td.Height = kMaxSide;
  td.MipLevels = 1;
  td.ArraySize = 1;
  td.Format = DXGI_FORMAT_B8G8R8A8_UNORM;
  td.SampleDesc.Count = 1;
  td.Usage = D3D11_USAGE_DYNAMIC;
  td.BindFlags = D3D11_BIND_SHADER_RESOURCE;
  td.CPUAccessFlags = D3D11_CPU_ACCESS_WRITE;
  HRESULT hr = d->CreateTexture2D(&td, nullptr, shape_.GetAddressOf());
  if (FAILED(hr)) {
    return device_.Fail(hr, "CreateTexture2D (pointer)");
  }
  hr = d->CreateShaderResourceView(shape_.Get(), nullptr, shape_srv_.GetAddressOf());
  if (FAILED(hr)) {
    return device_.Fail(hr, "CreateShaderResourceView (pointer)");
  }
  hr = d->CreateVertexShader(g_cursor_vs, sizeof g_cursor_vs, nullptr, vs_.GetAddressOf());
  if (FAILED(hr)) {
    return device_.Fail(hr, "CreateVertexShader (cursor)");
  }
  hr = d->CreatePixelShader(g_cursor_ps, sizeof g_cursor_ps, nullptr, ps_.GetAddressOf());
  if (FAILED(hr)) {
    return device_.Fail(hr, "CreatePixelShader (cursor)");
  }
  D3D11_SAMPLER_DESC sd{};
  sd.Filter = D3D11_FILTER_MIN_MAG_MIP_POINT;
  sd.AddressU = D3D11_TEXTURE_ADDRESS_CLAMP;
  sd.AddressV = D3D11_TEXTURE_ADDRESS_CLAMP;
  sd.AddressW = D3D11_TEXTURE_ADDRESS_CLAMP;
  sd.MaxAnisotropy = 1;
  sd.ComparisonFunc = D3D11_COMPARISON_NEVER;
  sd.MaxLOD = D3D11_FLOAT32_MAX;
  hr = d->CreateSamplerState(&sd, sampler_.GetAddressOf());
  if (FAILED(hr)) {
    return device_.Fail(hr, "CreateSamplerState (point)");
  }
  // Straight alpha over the desktop; the slot's own alpha stays (the converters ignore it).
  D3D11_BLEND_DESC bd{};
  D3D11_RENDER_TARGET_BLEND_DESC &rt = bd.RenderTarget[0];
  rt.BlendEnable = TRUE;
  rt.SrcBlend = D3D11_BLEND_SRC_ALPHA;
  rt.DestBlend = D3D11_BLEND_INV_SRC_ALPHA;
  rt.BlendOp = D3D11_BLEND_OP_ADD;
  rt.SrcBlendAlpha = D3D11_BLEND_ZERO;
  rt.DestBlendAlpha = D3D11_BLEND_ONE;
  rt.BlendOpAlpha = D3D11_BLEND_OP_ADD;
  rt.RenderTargetWriteMask =
      D3D11_COLOR_WRITE_ENABLE_RED | D3D11_COLOR_WRITE_ENABLE_GREEN | D3D11_COLOR_WRITE_ENABLE_BLUE;
  hr = d->CreateBlendState(&bd, blend_.GetAddressOf());
  if (FAILED(hr)) {
    return device_.Fail(hr, "CreateBlendState (cursor)");
  }
  if (const im_status st = detail::CreateCommonStates(device_, &raster_, &depth_); st != IM_OK) {
    return st;
  }
  return detail::CreateConstantBuffer(device_, 16 * sizeof(float), &constants_);
}

void CursorOverlay::SetShape(const CursorImage &image) {
  if (hidden_) {
    return;
  }
  std::lock_guard<std::mutex> lock(mu_);
  pending_.width = image.width;
  pending_.height = image.height;
  pending_.pixels.assign(image.pixels.begin(), image.pixels.end());
  ++pending_id_;
}

im_status CursorOverlay::Upload() noexcept {
  std::lock_guard<std::mutex> lock(mu_);
  if (pending_id_ == uploaded_id_) {
    return IM_OK;
  }
  const uint32_t w = std::min(pending_.width, kMaxSide);
  const uint32_t h = std::min(pending_.height, kMaxSide);
  if (pending_.width > kMaxSide || pending_.height > kMaxSide) {
    log::Info("gpu", "pointer shape cut", {{"w", pending_.width}, {"h", pending_.height}});
  }
  if (w != 0 && h != 0 && pending_.pixels.size() >= static_cast<std::size_t>(pending_.width) * pending_.height) {
    D3D11_MAPPED_SUBRESOURCE m{};
    const HRESULT hr = device_.context()->Map(shape_.Get(), 0, D3D11_MAP_WRITE_DISCARD, 0, &m);
    if (FAILED(hr)) {
      return device_.Fail(hr, "Map (pointer)");
    }
    for (uint32_t y = 0; y < h; ++y) {
      std::memcpy(static_cast<uint8_t *>(m.pData) + static_cast<std::size_t>(y) * m.RowPitch,
                  pending_.pixels.data() + static_cast<std::size_t>(y) * pending_.width,
                  static_cast<std::size_t>(w) * sizeof(uint32_t));
    }
    device_.context()->Unmap(shape_.Get(), 0);
    width_ = w;
    height_ = h;
  } else {
    width_ = 0;
    height_ = 0;
  }
  uploaded_id_ = pending_id_;
  return IM_OK;
}

im_status CursorOverlay::Blend(Slot &slot) noexcept {
  if (hidden_ || slot.cursor_blended || !slot.meta.cursor.visible || slot.size.width == 0 || slot.size.height == 0) {
    return IM_OK;
  }
  return api::Guard("gpu cursor", [&]() -> im_status {
    ContextLock lock(device_);
    if (const im_status st = Upload(); st != IM_OK) {
      return st;
    }
    if (width_ == 0 || height_ == 0) {
      return IM_OK;
    }
    // The pointer image's corners in the frame's upright pixels, turned into the slot's (region) pixels, then into clip
    // space over the whole slot texture. TL, TR, BL, BR: a triangle strip whatever the rotation.
    const auto x0 = static_cast<double>(slot.meta.cursor.x);
    const auto y0 = static_cast<double>(slot.meta.cursor.y);
    const double x1 = x0 + width_;
    const double y1 = y0 + height_;
    const Point upright[4] = {{x0, y0}, {x1, y0}, {x0, y1}, {x1, y1}};
    const float u1 = static_cast<float>(width_) / static_cast<float>(kMaxSide);
    const float v1 = static_cast<float>(height_) / static_cast<float>(kMaxSide);
    const float uv[4][2] = {{0.0f, 0.0f}, {u1, 0.0f}, {0.0f, v1}, {u1, v1}};
    const auto tw = static_cast<double>(slot.size.width);
    const auto th = static_cast<double>(slot.size.height);
    float quad[16];
    for (int i = 0; i < 4; ++i) {
      const Point p = UprightToRegionPixels(slot.meta.rotation, slot.meta.content, upright[i]);
      quad[i * 4 + 0] = static_cast<float>(p.x / tw * 2.0 - 1.0);
      quad[i * 4 + 1] = static_cast<float>(1.0 - p.y / th * 2.0);
      quad[i * 4 + 2] = uv[i][0];
      quad[i * 4 + 3] = uv[i][1];
    }
    ID3D11DeviceContext *ctx = device_.context();
    if (const im_status st = detail::UpdateConstants(device_, constants_.Get(), quad, sizeof quad); st != IM_OK) {
      return st;
    }
    detail::ResetUnusedStages(ctx);
    ctx->IASetPrimitiveTopology(D3D11_PRIMITIVE_TOPOLOGY_TRIANGLESTRIP);
    ctx->VSSetShader(vs_.Get(), nullptr, 0);
    ctx->VSSetConstantBuffers(0, 1, constants_.GetAddressOf());
    ctx->PSSetShader(ps_.Get(), nullptr, 0);
    ctx->PSSetShaderResources(0, 1, shape_srv_.GetAddressOf());
    ctx->PSSetSamplers(0, 1, sampler_.GetAddressOf());
    ctx->RSSetState(raster_.Get());
    ctx->OMSetDepthStencilState(depth_.Get(), 0);
    ctx->OMSetBlendState(blend_.Get(), nullptr, 0xFFFFFFFFu);
    D3D11_VIEWPORT vp{};
    vp.Width = static_cast<float>(slot.size.width);
    vp.Height = static_cast<float>(slot.size.height);
    vp.MaxDepth = 1.0f;
    ctx->RSSetViewports(1, &vp);
    ctx->OMSetRenderTargets(1, slot.rtv.GetAddressOf(), nullptr);
    ctx->Draw(4, 0);
    ID3D11ShaderResourceView *no_srv = nullptr;
    ctx->PSSetShaderResources(0, 1, &no_srv);
    ctx->OMSetRenderTargets(0, nullptr, nullptr);
    ctx->OMSetBlendState(nullptr, nullptr, 0xFFFFFFFFu);
    slot.cursor_blended = true;
    return IM_OK;
  });
}

} // namespace im::gpu
