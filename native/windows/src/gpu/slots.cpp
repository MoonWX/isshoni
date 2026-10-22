#include "gpu/slots.h"

#include <algorithm>
#include <atomic>
#include <utility>

#include "api/guard.h"
#include "im/abi/status.h"

namespace im::gpu {
namespace {

// A side of a grown slot texture that holds `want`: rounded up to a multiple of kGrowStep (at most D3D11's largest
// texture, unless `want` itself is larger, which CreateTexture2D then refuses), so a window dragged larger reallocates
// a slot every 256 pixels of growth instead of on every frame (04 §6.1: the capture side doesn't allocate per frame),
// and the converters see few distinct texture sizes (converter.h keeps a VideoProcessor per size).
constexpr uint64_t kGrowStep = 256;

uint32_t GrownSide(uint32_t have, uint32_t want) noexcept {
  if (want <= have) {
    return have;
  }
  const uint64_t rounded = (uint64_t{want} + kGrowStep - 1) / kGrowStep * kGrowStep;
  return std::max(want, static_cast<uint32_t>(std::min<uint64_t>(rounded, D3D11_REQ_TEXTURE2D_U_OR_V_DIMENSION)));
}

} // namespace

uint64_t NewTextureId() noexcept {
  static std::atomic<uint64_t> next{0};
  return next.fetch_add(1, std::memory_order_relaxed) + 1;
}

im_status FrameSlots::Create(Device &device, video::Size size, std::unique_ptr<FrameSlots> *out) noexcept {
  out->reset();
  if (size.width == 0 || size.height == 0) {
    return abi::Fail(IM_ERR_INVALID_ARG, {}, "gpu: frame slots need a size");
  }
  return api::Guard("gpu slots", [&]() -> im_status {
    std::unique_ptr<FrameSlots> s(new FrameSlots(device));
    for (Slot &slot : s->slots_) {
      if (const im_status st = s->Allocate(slot, size); st != IM_OK) {
        return st;
      }
    }
    *out = std::move(s);
    return IM_OK;
  });
}

im_status FrameSlots::Allocate(Slot &slot, video::Size size) noexcept {
  D3D11_TEXTURE2D_DESC td{};
  td.Width = size.width;
  td.Height = size.height;
  td.MipLevels = 1;
  td.ArraySize = 1;
  td.Format = DXGI_FORMAT_B8G8R8A8_UNORM;
  td.SampleDesc.Count = 1;
  td.Usage = D3D11_USAGE_DEFAULT;
  td.BindFlags = D3D11_BIND_SHADER_RESOURCE | D3D11_BIND_RENDER_TARGET;
  ComPtr<ID3D11Texture2D> texture;
  ComPtr<ID3D11RenderTargetView> rtv;
  ComPtr<ID3D11ShaderResourceView> srv;
  ID3D11Device *d = device_.d3d();
  HRESULT hr = d->CreateTexture2D(&td, nullptr, texture.GetAddressOf());
  if (FAILED(hr)) {
    return device_.Fail(hr, "CreateTexture2D (frame slot)");
  }
  hr = d->CreateRenderTargetView(texture.Get(), nullptr, rtv.GetAddressOf());
  if (FAILED(hr)) {
    return device_.Fail(hr, "CreateRenderTargetView (frame slot)");
  }
  hr = d->CreateShaderResourceView(texture.Get(), nullptr, srv.GetAddressOf());
  if (FAILED(hr)) {
    return device_.Fail(hr, "CreateShaderResourceView (frame slot)");
  }
  slot.texture = std::move(texture);
  slot.rtv = std::move(rtv);
  slot.srv = std::move(srv);
  slot.size = size;
  slot.texture_id = NewTextureId();
  return IM_OK;
}

im_status FrameSlots::BeginWrite(video::Size content, Slot **out) noexcept {
  Slot &slot = slots_[index_.write_index()];
  *out = &slot;
  if (content.width > slot.size.width || content.height > slot.size.height) {
    const video::Size grown{GrownSide(slot.size.width, content.width), GrownSide(slot.size.height, content.height)};
    if (const im_status st = Allocate(slot, grown); st != IM_OK) {
      *out = nullptr;
      return st;
    }
  }
  slot.cursor_blended = false;
  return IM_OK;
}

uint64_t FrameSlots::Publish(const FrameMeta &meta) noexcept {
  Slot &slot = slots_[index_.write_index()];
  slot.meta = meta;
  slot.meta.content =
      video::Size{std::min(meta.content.width, slot.size.width), std::min(meta.content.height, slot.size.height)};
  slot.cursor_blended = false;
  // The number Publish will give it: the triple buffer counts from 1 (the slot's fields must be set before the swap).
  slot.seq = index_.published() + 1;
  return index_.Publish();
}

FrameSlots::Taken FrameSlots::TakeLatest() noexcept {
  const TripleIndex::Taken t = index_.Take();
  Taken out;
  out.fresh = t.fresh;
  out.skipped = t.skipped;
  out.slot = t.seq == 0 ? nullptr : &slots_[t.index];
  return out;
}

Slot *FrameSlots::current() noexcept { return index_.read_seq() == 0 ? nullptr : &slots_[index_.read_index()]; }

} // namespace im::gpu
