#include "gpu/shader_convert.h"

#include <cstring>
#include <utility>

#include "api/guard.h"
#include "im/abi/status.h"

// Bytecode compiled by fxc at build time (native/windows/CMakeLists.txt): const BYTE g_<name>[].
#include "shaders/fullscreen_vs.h"
#include "shaders/nv12_uv_ps.h"
#include "shaders/nv12_y_ps.h"

namespace im::gpu {

namespace detail {

im_status CreateCommonStates(Device &device, ComPtr<ID3D11RasterizerState> *raster,
                             ComPtr<ID3D11DepthStencilState> *depth) noexcept {
  D3D11_RASTERIZER_DESC rd{};
  rd.FillMode = D3D11_FILL_SOLID;
  rd.CullMode = D3D11_CULL_NONE;
  rd.DepthClipEnable = TRUE;
  HRESULT hr = device.d3d()->CreateRasterizerState(&rd, raster->ReleaseAndGetAddressOf());
  if (FAILED(hr)) {
    return device.Fail(hr, "CreateRasterizerState");
  }
  D3D11_DEPTH_STENCIL_DESC dd{};
  dd.DepthEnable = FALSE;
  dd.DepthWriteMask = D3D11_DEPTH_WRITE_MASK_ZERO;
  dd.DepthFunc = D3D11_COMPARISON_ALWAYS;
  dd.StencilEnable = FALSE;
  hr = device.d3d()->CreateDepthStencilState(&dd, depth->ReleaseAndGetAddressOf());
  if (FAILED(hr)) {
    return device.Fail(hr, "CreateDepthStencilState");
  }
  return IM_OK;
}

im_status CreateConstantBuffer(Device &device, uint32_t bytes, ComPtr<ID3D11Buffer> *out) noexcept {
  D3D11_BUFFER_DESC bd{};
  bd.ByteWidth = (bytes + 15u) & ~15u;
  bd.Usage = D3D11_USAGE_DYNAMIC;
  bd.BindFlags = D3D11_BIND_CONSTANT_BUFFER;
  bd.CPUAccessFlags = D3D11_CPU_ACCESS_WRITE;
  const HRESULT hr = device.d3d()->CreateBuffer(&bd, nullptr, out->ReleaseAndGetAddressOf());
  if (FAILED(hr)) {
    return device.Fail(hr, "CreateBuffer (constants)");
  }
  return IM_OK;
}

im_status UpdateConstants(Device &device, ID3D11Buffer *buffer, const void *data, std::size_t bytes) noexcept {
  D3D11_MAPPED_SUBRESOURCE m{};
  const HRESULT hr = device.context()->Map(buffer, 0, D3D11_MAP_WRITE_DISCARD, 0, &m);
  if (FAILED(hr)) {
    return device.Fail(hr, "Map (constants)");
  }
  std::memcpy(m.pData, data, bytes);
  device.context()->Unmap(buffer, 0);
  return IM_OK;
}

void ResetUnusedStages(ID3D11DeviceContext *ctx) noexcept {
  ctx->IASetInputLayout(nullptr);
  ctx->HSSetShader(nullptr, nullptr, 0);
  ctx->DSSetShader(nullptr, nullptr, 0);
  ctx->GSSetShader(nullptr, nullptr, 0);
  ctx->OMSetBlendState(nullptr, nullptr, 0xFFFFFFFFu);
}

} // namespace detail

im_status ShaderConverter::Create(Device &device, std::unique_ptr<ShaderConverter> *out) noexcept {
  out->reset();
  return api::Guard("gpu shader converter", [&]() -> im_status {
    std::unique_ptr<ShaderConverter> c(new ShaderConverter(device));
    if (const im_status st = c->Init(); st != IM_OK) {
      return st;
    }
    c->views_.reserve(kMaxViews);
    *out = std::move(c);
    return IM_OK;
  });
}

im_status ShaderConverter::Init() noexcept {
  ID3D11Device *d = device_.d3d();
  HRESULT hr = d->CreateVertexShader(g_fullscreen_vs, sizeof g_fullscreen_vs, nullptr, vs_.GetAddressOf());
  if (FAILED(hr)) {
    return device_.Fail(hr, "CreateVertexShader (fullscreen)");
  }
  hr = d->CreatePixelShader(g_nv12_y_ps, sizeof g_nv12_y_ps, nullptr, ps_luma_.GetAddressOf());
  if (FAILED(hr)) {
    return device_.Fail(hr, "CreatePixelShader (nv12_y)");
  }
  hr = d->CreatePixelShader(g_nv12_uv_ps, sizeof g_nv12_uv_ps, nullptr, ps_chroma_.GetAddressOf());
  if (FAILED(hr)) {
    return device_.Fail(hr, "CreatePixelShader (nv12_uv)");
  }
  D3D11_SAMPLER_DESC sd{};
  sd.Filter = D3D11_FILTER_MIN_MAG_MIP_LINEAR;
  sd.AddressU = D3D11_TEXTURE_ADDRESS_CLAMP;
  sd.AddressV = D3D11_TEXTURE_ADDRESS_CLAMP;
  sd.AddressW = D3D11_TEXTURE_ADDRESS_CLAMP;
  sd.MaxAnisotropy = 1;
  sd.ComparisonFunc = D3D11_COMPARISON_NEVER;
  sd.MaxLOD = D3D11_FLOAT32_MAX;
  hr = d->CreateSamplerState(&sd, sampler_.GetAddressOf());
  if (FAILED(hr)) {
    return device_.Fail(hr, "CreateSamplerState (linear)");
  }
  if (const im_status st = detail::CreateCommonStates(device_, &raster_, &depth_); st != IM_OK) {
    return st;
  }
  return detail::CreateConstantBuffer(device_, sizeof(ConvertConstants), &constants_);
}

void ShaderConverter::ClearViews() noexcept { views_.clear(); }

im_status ShaderConverter::ViewsFor(const Nv12Target &dst, const Views **out) noexcept {
  ID3D11Texture2D *key = dst.planar() ? dst.luma : dst.texture;
  const uint32_t slice = dst.planar() ? 0 : dst.array_slice;
  for (std::size_t i = 0; i < views_.size(); ++i) {
    if (views_[i].texture == key && views_[i].slice == slice) {
      if (i + 1 != views_.size()) {
        Views v = std::move(views_[i]);
        views_.erase(views_.begin() + static_cast<std::ptrdiff_t>(i));
        views_.push_back(std::move(v));
      }
      *out = &views_.back();
      return IM_OK;
    }
  }
  if (key == nullptr || (dst.planar() && dst.chroma == nullptr)) {
    return abi::Fail(IM_ERR_INVALID_ARG, {}, "gpu: the NV12 target has no texture");
  }
  Views v;
  v.texture = key;
  v.slice = slice;
  D3D11_TEXTURE2D_DESC td{};
  key->GetDesc(&td);
  v.width = td.Width;
  v.height = td.Height;
  ID3D11Device *d = device_.d3d();
  HRESULT hr = S_OK;
  if (dst.planar()) {
    hr = d->CreateRenderTargetView(dst.luma, nullptr, v.luma.GetAddressOf());
    if (SUCCEEDED(hr)) {
      hr = d->CreateRenderTargetView(dst.chroma, nullptr, v.chroma.GetAddressOf());
    }
  } else {
    if (td.Format != DXGI_FORMAT_NV12 || slice >= td.ArraySize) {
      return abi::Failf(IM_ERR_INVALID_ARG, {}, "gpu: the target is not an NV12 texture with slice %u",
                        static_cast<unsigned>(slice));
    }
    D3D11_RENDER_TARGET_VIEW_DESC rd{};
    if (td.ArraySize > 1) {
      rd.ViewDimension = D3D11_RTV_DIMENSION_TEXTURE2DARRAY;
      rd.Texture2DArray.MipSlice = 0;
      rd.Texture2DArray.FirstArraySlice = slice;
      rd.Texture2DArray.ArraySize = 1;
    } else {
      rd.ViewDimension = D3D11_RTV_DIMENSION_TEXTURE2D;
      rd.Texture2D.MipSlice = 0;
    }
    rd.Format = DXGI_FORMAT_R8_UNORM; // the luma plane
    hr = d->CreateRenderTargetView(dst.texture, &rd, v.luma.GetAddressOf());
    if (SUCCEEDED(hr)) {
      rd.Format = DXGI_FORMAT_R8G8_UNORM; // the Cb/Cr plane
      hr = d->CreateRenderTargetView(dst.texture, &rd, v.chroma.GetAddressOf());
    }
  }
  if (FAILED(hr)) {
    return device_.Fail(hr, "CreateRenderTargetView (NV12 target)");
  }
  if (views_.size() >= kMaxViews) {
    views_.erase(views_.begin());
  }
  views_.push_back(std::move(v));
  *out = &views_.back();
  return IM_OK;
}

im_status ShaderConverter::Convert(const SourceFrame &src, const ConvertGeometry &g, const Nv12Target &dst) noexcept {
  if (src.srv == nullptr) {
    return abi::Fail(IM_ERR_INVALID_ARG, {}, "gpu: the source frame has no shader resource view");
  }
  return api::Guard("gpu shader converter", [&]() -> im_status {
    const Views *v = nullptr;
    if (const im_status st = ViewsFor(dst, &v); st != IM_OK) {
      return st;
    }
    if (v->width != g.output.width || v->height != g.output.height) {
      return abi::Failf(IM_ERR_INVALID_ARG, {}, "gpu: the NV12 target is %ux%u, the layer %ux%u",
                        static_cast<unsigned>(v->width), static_cast<unsigned>(v->height),
                        static_cast<unsigned>(g.output.width), static_cast<unsigned>(g.output.height));
    }
    const ConvertConstants k = MakeConvertConstants(g);
    ContextLock lock(device_);
    ID3D11DeviceContext *ctx = device_.context();
    if (const im_status st = detail::UpdateConstants(device_, constants_.Get(), &k, sizeof k); st != IM_OK) {
      return st;
    }
    detail::ResetUnusedStages(ctx);
    // The render target first: a slot still bound as one (by whoever drew into it last) would make the runtime refuse
    // it as a shader resource.
    ctx->OMSetRenderTargets(1, v->luma.GetAddressOf(), nullptr);
    ctx->IASetPrimitiveTopology(D3D11_PRIMITIVE_TOPOLOGY_TRIANGLELIST);
    ctx->VSSetShader(vs_.Get(), nullptr, 0);
    ctx->RSSetState(raster_.Get());
    ctx->OMSetDepthStencilState(depth_.Get(), 0);
    ID3D11ShaderResourceView *srv = src.srv;
    ctx->PSSetShaderResources(0, 1, &srv);
    ctx->PSSetSamplers(0, 1, sampler_.GetAddressOf());
    ctx->PSSetConstantBuffers(0, 1, constants_.GetAddressOf());

    D3D11_VIEWPORT vp{};
    vp.Width = static_cast<float>(g.output.width);
    vp.Height = static_cast<float>(g.output.height);
    vp.MaxDepth = 1.0f;
    ctx->RSSetViewports(1, &vp);
    ctx->PSSetShader(ps_luma_.Get(), nullptr, 0);
    ctx->Draw(3, 0);

    vp.Width = static_cast<float>(g.output.width / 2);
    vp.Height = static_cast<float>(g.output.height / 2);
    ctx->RSSetViewports(1, &vp);
    ctx->PSSetShader(ps_chroma_.Get(), nullptr, 0);
    ctx->OMSetRenderTargets(1, v->chroma.GetAddressOf(), nullptr);
    ctx->Draw(3, 0);

    // Unbind, so the slot can be a render target (the capture copy, the cursor) and the target an encoder's input.
    ID3D11ShaderResourceView *no_srv = nullptr;
    ctx->PSSetShaderResources(0, 1, &no_srv);
    ctx->OMSetRenderTargets(0, nullptr, nullptr);
    return IM_OK;
  });
}

} // namespace im::gpu
