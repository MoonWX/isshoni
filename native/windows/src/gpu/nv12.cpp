#include "gpu/nv12.h"

#include <chrono>
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

// Whether t is a single plane texture of width × height in format (what CopyResource into our staging plane needs).
bool IsPlane(ID3D11Texture2D *t, uint32_t width, uint32_t height, DXGI_FORMAT format) noexcept {
  D3D11_TEXTURE2D_DESC d{};
  t->GetDesc(&d);
  return d.Width == width && d.Height == height && d.Format == format && d.MipLevels == 1 && d.ArraySize == 1 &&
         d.SampleDesc.Count == 1;
}

// Calls step(context) under the context lock until it returns something other than S_FALSE or
// DXGI_ERROR_WAS_STILL_DRAWING (the GPU isn't done yet), releasing the lock and yielding between tries, so the lock is
// never held across the wait (04 §6.1, §8.1). Returns step's last result, or DXGI_ERROR_WAS_STILL_DRAWING when the GPU
// isn't done after Nv12Readback::kGpuWaitLimitMs.
template <typename Step> HRESULT PollUnlocked(Device &device, Step step) noexcept {
  const auto deadline = std::chrono::steady_clock::now() + std::chrono::milliseconds(Nv12Readback::kGpuWaitLimitMs);
  for (;;) {
    HRESULT hr = S_OK;
    {
      ContextLock lock(device);
      hr = step(device.context());
    }
    if (hr != S_FALSE && hr != DXGI_ERROR_WAS_STILL_DRAWING) {
      return hr;
    }
    if (std::chrono::steady_clock::now() >= deadline) {
      return DXGI_ERROR_WAS_STILL_DRAWING;
    }
    // A few microseconds for a thread waiting on the lock (the capture copy, FFmpeg) to take it, then the rest of the
    // time slice to any thread ready on this processor; no timer sleep, whose tick can be 15.6 ms.
    for (int i = 0; i < 32; ++i) {
      YieldProcessor();
    }
    SwitchToThread();
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
    D3D11_QUERY_DESC qd{};
    qd.Query = D3D11_QUERY_EVENT;
    const HRESULT hr = device.d3d()->CreateQuery(&qd, r->query_.GetAddressOf());
    if (FAILED(hr)) {
      return device.Fail(hr, "CreateQuery (readback)");
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
  // GetDesc is a method of the resource, not of the context: no lock.
  D3D11_TEXTURE2D_DESC sd{};
  if (!src.planar()) {
    src.texture->GetDesc(&sd);
    if (sd.Width != w || sd.Height != h || sd.Format != DXGI_FORMAT_NV12 || src.array_slice >= sd.ArraySize) {
      return abi::Fail(IM_ERR_INVALID_ARG, {}, "gpu: the readback's source is not an NV12 picture of its size");
    }
  } else if (!IsPlane(src.luma, w, h, DXGI_FORMAT_R8_UNORM) ||
             !IsPlane(src.chroma, w / 2, h / 2, DXGI_FORMAT_R8G8_UNORM)) {
    // CopyResource does nothing (a debug-layer message only) between textures of different sizes or formats, which
    // would hand the encoder the previous frame's planes.
    return abi::Fail(IM_ERR_INVALID_ARG, {}, "gpu: the readback's planar source is not a picture of its size");
  }

  // Submit the copies and the query behind them; the GPU runs them while nobody holds the lock.
  {
    ContextLock lock(device_);
    ID3D11DeviceContext *ctx = device_.context();
    if (!src.planar()) {
      // One subresource holds both planes; the Cb/Cr plane follows the Y plane at RowPitch × height (as FFmpeg's
      // hwcontext_d3d11va reads it).
      ctx->CopySubresourceRegion(nv12_.Get(), 0, 0, 0, 0, src.texture,
                                 D3D11CalcSubresource(0, src.array_slice, sd.MipLevels), nullptr);
    } else {
      ctx->CopyResource(luma_.Get(), src.luma);
      ctx->CopyResource(chroma_.Get(), src.chroma);
    }
    ctx->End(query_.Get());
    ctx->Flush();
  }
#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
  if (wait_hook_ != nullptr) {
    wait_hook_(wait_hook_ctx_);
  }
#endif
  if (const im_status st = WaitForCopies(); st != IM_OK) {
    return st;
  }
  if (!src.planar()) {
    return ReadStaging(Plane::kNv12, dst);
  }
  if (const im_status st = ReadStaging(Plane::kLuma, dst); st != IM_OK) {
    return st;
  }
  return ReadStaging(Plane::kChroma, dst);
}

im_status Nv12Readback::WaitForCopies() noexcept {
  const HRESULT hr = PollUnlocked(device_, [this](ID3D11DeviceContext *ctx) noexcept {
    BOOL done = FALSE;
    const HRESULT r = ctx->GetData(query_.Get(), &done, sizeof done, 0);
    return r == S_OK && !done ? S_FALSE : r;
  });
  if (hr == DXGI_ERROR_WAS_STILL_DRAWING) {
    return device_.Fail(hr, "readback: the GPU did not finish the copy in time");
  }
  return FAILED(hr) ? device_.Fail(hr, "GetData (readback query)") : IM_OK;
}

im_status Nv12Readback::ReadStaging(Plane plane, const Nv12Planes &dst) noexcept {
  ID3D11Texture2D *staging = plane == Plane::kNv12 ? nv12_.Get() : plane == Plane::kLuma ? luma_.Get() : chroma_.Get();
  const char *what = plane == Plane::kNv12   ? "Map (NV12 readback)"
                     : plane == Plane::kLuma ? "Map (luma readback)"
                                             : "Map (chroma readback)";
  D3D11_MAPPED_SUBRESOURCE m{};
  // The copy has finished, so this Map doesn't wait; DO_NOT_WAIT makes sure of it on any driver.
  const HRESULT hr = PollUnlocked(device_, [&](ID3D11DeviceContext *ctx) noexcept {
    return ctx->Map(staging, 0, D3D11_MAP_READ, D3D11_MAP_FLAG_DO_NOT_WAIT, &m);
  });
  if (FAILED(hr)) {
    return device_.Fail(hr, what);
  }
  // The mapped memory stays valid until Unmap, and nobody else uses our staging textures: copy without the lock.
  const uint32_t w = size_.width;
  const uint32_t h = size_.height;
  switch (plane) {
  case Plane::kNv12:
    CopyRows(m, 0, h, w, dst.y, dst.y_stride);
    CopyRows(m, static_cast<std::size_t>(m.RowPitch) * h, h / 2, w, dst.uv, dst.uv_stride);
    break;
  case Plane::kLuma:
    CopyRows(m, 0, h, w, dst.y, dst.y_stride);
    break;
  case Plane::kChroma:
    CopyRows(m, 0, h / 2, w, dst.uv, dst.uv_stride); // w / 2 pairs of 2 bytes
    break;
  }
  ContextLock lock(device_);
  device_.context()->Unmap(staging, 0);
  return IM_OK;
}

} // namespace im::gpu
