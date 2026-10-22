#include "gpu/nv12.h"

#include <cstring>
#include <utility>

#include "api/guard.h"
#include "im/abi/status.h"

namespace im::gpu {
namespace {

im_status CheckSize(video::Size size, const char *what) noexcept {
  if (size.width == 0 || size.height == 0 || size.width % 2 != 0 || size.height % 2 != 0) {
    return abi::Failf(IM_ERR_INVALID_ARG, {}, "gpu: %s: NV12 needs even sides (%ux%u)", what,
                      static_cast<unsigned>(size.width), static_cast<unsigned>(size.height));
  }
  return IM_OK;
}

D3D11_TEXTURE2D_DESC TextureDesc(uint32_t width, uint32_t height, DXGI_FORMAT format, bool staging) noexcept {
  D3D11_TEXTURE2D_DESC td{};
  td.Width = width;
  td.Height = height;
  td.MipLevels = 1;
  td.ArraySize = 1;
  td.Format = format;
  td.SampleDesc.Count = 1;
  if (staging) {
    td.Usage = D3D11_USAGE_STAGING;
    td.CPUAccessFlags = D3D11_CPU_ACCESS_READ;
  } else {
    td.Usage = D3D11_USAGE_DEFAULT;
    td.BindFlags = D3D11_BIND_RENDER_TARGET;
  }
  return td;
}

// Creates the NV12 texture, or the two plane textures when nv12 is false.
im_status CreatePlanes(Device &device, video::Size size, bool nv12, bool staging, ComPtr<ID3D11Texture2D> *nv12_tex,
                       ComPtr<ID3D11Texture2D> *luma, ComPtr<ID3D11Texture2D> *chroma) noexcept {
  ID3D11Device *d = device.d3d();
  if (nv12) {
    const D3D11_TEXTURE2D_DESC td = TextureDesc(size.width, size.height, DXGI_FORMAT_NV12, staging);
    const HRESULT hr = d->CreateTexture2D(&td, nullptr, nv12_tex->ReleaseAndGetAddressOf());
    return FAILED(hr) ? device.Fail(hr, "CreateTexture2D (NV12)") : IM_OK;
  }
  D3D11_TEXTURE2D_DESC td = TextureDesc(size.width, size.height, DXGI_FORMAT_R8_UNORM, staging);
  HRESULT hr = d->CreateTexture2D(&td, nullptr, luma->ReleaseAndGetAddressOf());
  if (FAILED(hr)) {
    return device.Fail(hr, "CreateTexture2D (luma plane)");
  }
  td = TextureDesc(size.width / 2, size.height / 2, DXGI_FORMAT_R8G8_UNORM, staging);
  hr = d->CreateTexture2D(&td, nullptr, chroma->ReleaseAndGetAddressOf());
  return FAILED(hr) ? device.Fail(hr, "CreateTexture2D (chroma plane)") : IM_OK;
}

// Copies rows bytes wide from a mapped staging texture.
void CopyRows(const D3D11_MAPPED_SUBRESOURCE &m, std::size_t offset, uint32_t rows, uint32_t bytes, uint8_t *dst,
              std::size_t stride) noexcept {
  const auto *src = static_cast<const uint8_t *>(m.pData) + offset;
  for (uint32_t r = 0; r < rows; ++r) {
    std::memcpy(dst + r * stride, src + static_cast<std::size_t>(r) * m.RowPitch, bytes);
  }
}

} // namespace

im_status Nv12Surface::Create(Device &device, video::Size size, std::unique_ptr<Nv12Surface> *out) noexcept {
  out->reset();
  if (const im_status st = CheckSize(size, "surface"); st != IM_OK) {
    return st;
  }
  return api::Guard("gpu nv12 surface", [&]() -> im_status {
    std::unique_ptr<Nv12Surface> s(new Nv12Surface());
    s->size_ = size;
    if (const im_status st =
            CreatePlanes(device, size, device.nv12_render_target(), false, &s->nv12_, &s->luma_, &s->chroma_);
        st != IM_OK) {
      return st;
    }
    *out = std::move(s);
    return IM_OK;
  });
}

im_status Nv12Readback::Create(Device &device, video::Size size, bool planar,
                               std::unique_ptr<Nv12Readback> *out) noexcept {
  out->reset();
  if (const im_status st = CheckSize(size, "readback"); st != IM_OK) {
    return st;
  }
  return api::Guard("gpu nv12 readback", [&]() -> im_status {
    std::unique_ptr<Nv12Readback> r(new Nv12Readback(device));
    r->size_ = size;
    if (const im_status st = CreatePlanes(device, size, !planar, true, &r->nv12_, &r->luma_, &r->chroma_);
        st != IM_OK) {
      return st;
    }
    *out = std::move(r);
    return IM_OK;
  });
}

im_status Nv12Readback::Read(const Nv12Target &src, const Nv12Planes &dst) noexcept {
  if (dst.y == nullptr || dst.uv == nullptr || dst.y_stride < size_.width || dst.uv_stride < size_.width) {
    return abi::Fail(IM_ERR_INVALID_ARG, {}, "gpu: readback planes too small");
  }
  if (src.planar() != (nv12_.Get() == nullptr) || (src.planar() && (src.luma == nullptr || src.chroma == nullptr))) {
    return abi::Fail(IM_ERR_INVALID_ARG, {}, "gpu: the readback and its source differ in layout");
  }
  const uint32_t w = size_.width;
  const uint32_t h = size_.height;
  ContextLock lock(device_);
  ID3D11DeviceContext *ctx = device_.context();
  D3D11_MAPPED_SUBRESOURCE m{};
  if (!src.planar()) {
    D3D11_TEXTURE2D_DESC sd{};
    src.texture->GetDesc(&sd);
    if (sd.Width != w || sd.Height != h || sd.Format != DXGI_FORMAT_NV12 || src.array_slice >= sd.ArraySize) {
      return abi::Fail(IM_ERR_INVALID_ARG, {}, "gpu: the readback's source is not an NV12 picture of its size");
    }
    // One subresource holds both planes; the Cb/Cr plane follows the Y plane at RowPitch × height (as FFmpeg's
    // hwcontext_d3d11va reads it).
    ctx->CopySubresourceRegion(nv12_.Get(), 0, 0, 0, 0, src.texture,
                               D3D11CalcSubresource(0, src.array_slice, sd.MipLevels), nullptr);
    const HRESULT hr = ctx->Map(nv12_.Get(), 0, D3D11_MAP_READ, 0, &m);
    if (FAILED(hr)) {
      return device_.Fail(hr, "Map (NV12 readback)");
    }
    CopyRows(m, 0, h, w, dst.y, dst.y_stride);
    CopyRows(m, static_cast<std::size_t>(m.RowPitch) * h, h / 2, w, dst.uv, dst.uv_stride);
    ctx->Unmap(nv12_.Get(), 0);
    return IM_OK;
  }
  ctx->CopyResource(luma_.Get(), src.luma);
  HRESULT hr = ctx->Map(luma_.Get(), 0, D3D11_MAP_READ, 0, &m);
  if (FAILED(hr)) {
    return device_.Fail(hr, "Map (luma readback)");
  }
  CopyRows(m, 0, h, w, dst.y, dst.y_stride);
  ctx->Unmap(luma_.Get(), 0);
  ctx->CopyResource(chroma_.Get(), src.chroma);
  hr = ctx->Map(chroma_.Get(), 0, D3D11_MAP_READ, 0, &m);
  if (FAILED(hr)) {
    return device_.Fail(hr, "Map (chroma readback)");
  }
  CopyRows(m, 0, h / 2, w, dst.uv, dst.uv_stride); // w / 2 pairs of 2 bytes
  ctx->Unmap(chroma_.Get(), 0);
  return IM_OK;
}

} // namespace im::gpu
