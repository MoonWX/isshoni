#include "gpu/test_pattern.h"

#include <utility>

#include "api/guard.h"
#include "gpu/shader_convert.h"
#include "im/abi/status.h"

// Bytecode compiled by fxc at build time (native/windows/CMakeLists.txt): const BYTE g_<name>[]. fullscreen_vs.h is
// included by shader_convert.cpp too; a namespace-scope const array has internal linkage, so each file has its copy.
#include "shaders/fullscreen_vs.h"
#include "shaders/pattern_ps.h"

namespace im::gpu {

im_status TestPattern::Create(Device &device, std::unique_ptr<TestPattern> *out) noexcept {
  out->reset();
  return api::Guard("gpu test pattern", [&]() -> im_status {
    std::unique_ptr<TestPattern> p(new TestPattern(device));
    if (const im_status st = p->Init(); st != IM_OK) {
      return st;
    }
    *out = std::move(p);
    return IM_OK;
  });
}

im_status TestPattern::Init() noexcept {
  ID3D11Device *d = device_.d3d();
  HRESULT hr = d->CreateVertexShader(g_fullscreen_vs, sizeof g_fullscreen_vs, nullptr, vs_.GetAddressOf());
  if (FAILED(hr)) {
    return device_.Fail(hr, "CreateVertexShader (fullscreen)");
  }
  hr = d->CreatePixelShader(g_pattern_ps, sizeof g_pattern_ps, nullptr, ps_.GetAddressOf());
  if (FAILED(hr)) {
    return device_.Fail(hr, "CreatePixelShader (pattern)");
  }
  if (const im_status st = detail::CreateCommonStates(device_, &raster_, &depth_); st != IM_OK) {
    return st;
  }
  return detail::CreateConstantBuffer(device_, sizeof(PatternConstants), &constants_);
}

im_status TestPattern::Render(Slot &slot, video::Size size, const PatternFrame &f) noexcept {
  if (size.width < 2 || size.height < 2 || size.width > slot.size.width || size.height > slot.size.height ||
      slot.rtv.Get() == nullptr) {
    return abi::Failf(IM_ERR_INVALID_ARG, {}, "gpu: a %ux%u pattern doesn't fit the %ux%u slot",
                      static_cast<unsigned>(size.width), static_cast<unsigned>(size.height),
                      static_cast<unsigned>(slot.size.width), static_cast<unsigned>(slot.size.height));
  }
  const PatternConstants k = MakePatternConstants(MakePatternLayout(size.width, size.height), f);
  ContextLock lock(device_);
  ID3D11DeviceContext *ctx = device_.context();
  if (const im_status st = detail::UpdateConstants(device_, constants_.Get(), &k, sizeof k); st != IM_OK) {
    return st;
  }
  detail::ResetUnusedStages(ctx);
  ctx->IASetPrimitiveTopology(D3D11_PRIMITIVE_TOPOLOGY_TRIANGLELIST);
  ctx->VSSetShader(vs_.Get(), nullptr, 0);
  ctx->PSSetShader(ps_.Get(), nullptr, 0);
  ctx->PSSetConstantBuffers(0, 1, constants_.GetAddressOf());
  ctx->RSSetState(raster_.Get());
  ctx->OMSetDepthStencilState(depth_.Get(), 0);
  D3D11_VIEWPORT vp{};
  vp.Width = static_cast<float>(size.width);
  vp.Height = static_cast<float>(size.height);
  vp.MaxDepth = 1.0f;
  ctx->RSSetViewports(1, &vp);
  ctx->OMSetRenderTargets(1, slot.rtv.GetAddressOf(), nullptr);
  ctx->Draw(3, 0);
  ctx->OMSetRenderTargets(0, nullptr, nullptr);
  return IM_OK;
}

} // namespace im::gpu
