// The luma tap's producer (luma_tap.h; docs/m2/04-windows-engine.md §5.8): IM_TEST_HOOKS builds only.
#include <cstdint> // keeps this file's translation unit non-empty in builds without test hooks

#include "session/luma_tap.h"

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS

#include <algorithm>
#include <array>
#include <cstddef>
#include <span>
#include <utility>

#include "api/guard.h"
#include "im/abi/status.h"
#include "im/log/ring.h"

namespace im::session {

im_status LumaTap::Create(gpu::Device &device, ring::PacketRing *ring, std::unique_ptr<LumaTap> *out) noexcept {
  out->reset();
  if (ring == nullptr) {
    return abi::Fail(IM_ERR_INTERNAL, {}, "session: the luma tap has no ring");
  }
  return api::Guard("session luma tap", [&]() -> im_status {
    std::unique_ptr<LumaTap> t(new LumaTap(device, ring));
    D3D11_TEXTURE2D_DESC td{};
    td.Width = luma::kBoxSide;
    td.Height = luma::kBoxSide;
    td.MipLevels = 1;
    td.ArraySize = 1;
    td.Format = DXGI_FORMAT_B8G8R8A8_UNORM; // the slots' format (gpu/frame.h): a copy between them is exact
    td.SampleDesc.Count = 1;
    td.Usage = D3D11_USAGE_STAGING;
    td.CPUAccessFlags = D3D11_CPU_ACCESS_READ;
    D3D11_QUERY_DESC qd{};
    qd.Query = D3D11_QUERY_EVENT;
    for (Entry &e : t->entries_) {
      HRESULT hr = device.d3d()->CreateTexture2D(&td, nullptr, e.staging.GetAddressOf());
      if (FAILED(hr)) {
        return device.Fail(hr, "CreateTexture2D (luma tap)");
      }
      hr = device.d3d()->CreateQuery(&qd, e.query.GetAddressOf());
      if (FAILED(hr)) {
        return device.Fail(hr, "CreateQuery (luma tap)");
      }
    }
    *out = std::move(t);
    return IM_OK;
  });
}

void LumaTap::Stop(HRESULT hr, const char *what) noexcept {
  if (stopped_) {
    return;
  }
  stopped_ = true;
  count_ = 0;
  log::Warn("session", "luma tap stopped",
            {{"call", what}, {"hr", static_cast<uint32_t>(hr)}, {"removed", device_.Removed()}});
}

void LumaTap::OnFrame(const gpu::Slot &slot, uint64_t skipped) noexcept {
  if (stopped_) {
    return;
  }
  missed_ += skipped;
  // The frame fills (0, 0, content) of its texture (gpu/slots.h), never more than the texture.
  const uint32_t width = std::min(slot.meta.content.width, slot.size.width);
  const uint32_t height = std::min(slot.meta.content.height, slot.size.height);
  const luma::Region r = luma::CenterBox(width, height);
  if (count_ == kDepth || r.empty() || slot.texture == nullptr) {
    ++missed_; // every staging texture is busy (the GPU is behind), or the frame has no content
    return;
  }
  Entry &e = entries_[(head_ + count_) % kDepth];
  D3D11_BOX box{};
  box.left = r.x;
  box.top = r.y;
  box.front = 0;
  box.right = r.x + r.width;
  box.bottom = r.y + r.height;
  box.back = 1;
  {
    gpu::ContextLock lock(device_);
    ID3D11DeviceContext *ctx = device_.context();
    ctx->CopySubresourceRegion(e.staging.Get(), 0, 0, 0, 0, slot.texture.Get(), 0, &box);
    ctx->End(e.query.Get());
    ctx->Flush(); // a static screen gets no other submission that would start the copy
  }
  e.sample = luma::Sample{};
  e.sample.capture_ts_ns = slot.meta.capture_ts_ns;
  e.sample.source_width = width;
  e.sample.source_height = height;
  e.sample.region = r;
  e.sample.missed = static_cast<uint32_t>(std::min<uint64_t>(missed_, 0xFFFFFFFFu));
  missed_ = 0;
  ++count_;
}

void LumaTap::Poll() noexcept {
  while (count_ != 0 && !stopped_) {
    Entry &e = entries_[head_];
    BOOL done = FALSE;
    HRESULT hr = S_OK;
    {
      gpu::ContextLock lock(device_);
      hr = device_.context()->GetData(e.query.Get(), &done, sizeof done, D3D11_ASYNC_GETDATA_DONOTFLUSH);
    }
    if (hr == S_FALSE || (hr == S_OK && !done)) {
      return; // the GPU is still on it: the next wake-up looks again
    }
    if (FAILED(hr)) {
      Stop(hr, "GetData");
      return;
    }
    D3D11_MAPPED_SUBRESOURCE m{};
    {
      gpu::ContextLock lock(device_);
      hr = device_.context()->Map(e.staging.Get(), 0, D3D11_MAP_READ, D3D11_MAP_FLAG_DO_NOT_WAIT, &m);
    }
    if (hr == DXGI_ERROR_WAS_STILL_DRAWING) {
      return;
    }
    if (FAILED(hr)) {
      Stop(hr, "Map");
      return;
    }
    // Nobody else touches the staging texture: sum without the lock (as gpu/nv12.cpp's readback copies).
    luma::Accumulator acc;
    const auto *base = static_cast<const uint8_t *>(m.pData);
    for (uint32_t y = 0; y < e.sample.region.height; ++y) {
      acc.AddBgraRow(base + static_cast<std::size_t>(y) * m.RowPitch, e.sample.region.width);
    }
    {
      gpu::ContextLock lock(device_);
      device_.context()->Unmap(e.staging.Get(), 0);
    }
    e.sample.mean = acc.Mean();
    std::array<uint8_t, luma::kPayloadBytes> payload{};
    luma::Encode(e.sample, payload);
    ring::PacketMeta meta;
    meta.capture_ts_ns = e.sample.capture_ts_ns;
    // A full ring drops the reading; the next one that gets in carries DISCONTINUITY (§6.2).
    if (ring_->Push(meta, std::span<const uint8_t>(payload.data(), payload.size())) == ring::PushResult::kOk) {
      ++readings_;
    }
    head_ = (head_ + 1) % kDepth;
    --count_;
  }
}

} // namespace im::session

#endif // IM_TEST_HOOKS
