#include "session/frames.h"

#include <utility>

#include "api/guard.h"
#include "im/abi/status.h"

namespace im::session {
namespace {

constexpr uint32_t kBlackSide = 2; // the black texture: 2×2 BGRA zeros, sampled with no content (all black out)

} // namespace

im_status VideoFrames::Create(gpu::Device *device, const Config &config, std::unique_ptr<VideoFrames> *out) noexcept {
  out->reset();
  if (config.layers.size() > IM_MAX_LAYERS) {
    return abi::Failf(IM_ERR_INVALID_ARG, {}, "session: %zu layers (at most %u)", config.layers.size(),
                      static_cast<unsigned>(IM_MAX_LAYERS));
  }
  return api::Guard("session frames", [&]() -> im_status {
    std::unique_ptr<VideoFrames> f(new VideoFrames());
    f->device_ = device;
    f->source_ = config.source;
    f->layer_count_ = static_cast<uint32_t>(config.layers.size());
    if (device != nullptr) {
      if (const im_status st = f->InitGpu(config); st != IM_OK) {
        return st;
      }
    }
    *out = std::move(f);
    return IM_OK;
  });
}

im_status VideoFrames::InitGpu(const Config &config) noexcept {
  if (source_.width < 2 || source_.height < 2) {
    return abi::Failf(IM_ERR_INVALID_ARG, {}, "session: a %ux%u source has no picture",
                      static_cast<unsigned>(source_.width), static_cast<unsigned>(source_.height));
  }
  if (const im_status st = gpu::FrameSlots::Create(*device_, source_, &slots_); st != IM_OK) {
    return st;
  }
  if (config.pattern) {
    if (const im_status st = gpu::TestPattern::Create(*device_, &pattern_); st != IM_OK) {
      return st;
    }
  }
  for (uint32_t l = 0; l < layer_count_; ++l) {
    gpu::ConverterConfig cc;
    cc.output = config.layers[l].out;
    cc.source = source_;
    cc.input = source_; // the slots' size: every capture path delivers the source upright until README WA60's DD
    cc.fps = config.layers[l].fps;
    if (const im_status st = gpu::Converter::Create(*device_, cc, &converters_[l]); st != IM_OK) {
      return st;
    }
  }

  // The black picture (§6.3): any texture works, since a source with no content leaves the whole output black; an
  // immutable one of zeros never changes.
  static constexpr uint32_t kZeros[kBlackSide * kBlackSide] = {};
  D3D11_TEXTURE2D_DESC td{};
  td.Width = kBlackSide;
  td.Height = kBlackSide;
  td.MipLevels = 1;
  td.ArraySize = 1;
  td.Format = DXGI_FORMAT_B8G8R8A8_UNORM;
  td.SampleDesc.Count = 1;
  td.Usage = D3D11_USAGE_IMMUTABLE;
  td.BindFlags = D3D11_BIND_SHADER_RESOURCE;
  D3D11_SUBRESOURCE_DATA init{};
  init.pSysMem = kZeros;
  init.SysMemPitch = static_cast<UINT>(kBlackSide * sizeof(uint32_t));
  HRESULT hr = device_->d3d()->CreateTexture2D(&td, &init, black_texture_.GetAddressOf());
  if (FAILED(hr)) {
    return device_->Fail(hr, "CreateTexture2D (black)");
  }
  hr = device_->d3d()->CreateShaderResourceView(black_texture_.Get(), nullptr, black_srv_.GetAddressOf());
  if (FAILED(hr)) {
    return device_->Fail(hr, "CreateShaderResourceView (black)");
  }
  black_.texture = black_texture_.Get();
  black_.srv = black_srv_.Get();
  black_.texture_id = gpu::NewTextureId();
  black_.texture_size = video::Size{kBlackSide, kBlackSide};
  black_.content = video::Size{}; // no content: the converters write Y 16, Cb/Cr 128 everywhere
  return IM_OK;
}

im_status VideoFrames::PublishPattern(const gpu::PatternFrame &f, int64_t capture_ts_ns) noexcept {
  if (device_ == nullptr) {
    times_.Publish(capture_ts_ns);
    return IM_OK;
  }
  if (!pattern_) {
    return abi::Fail(IM_ERR_INTERNAL, {}, "session: the GPU path has no test pattern (not a test: source)");
  }
  gpu::Slot *slot = nullptr;
  if (const im_status st = slots_->BeginWrite(source_, &slot); st != IM_OK) {
    return st;
  }
  if (const im_status st = pattern_->Render(*slot, source_, f); st != IM_OK) {
    return st; // the slot isn't published; the next frame begins it again
  }
  gpu::FrameMeta meta;
  meta.capture_ts_ns = capture_ts_ns;
  meta.content = source_;
  meta.rotation = gpu::Rotation::kIdentity;
  slots_->Publish(meta);
  return IM_OK;
}

VideoFrames::Taken VideoFrames::Take() noexcept {
  if (device_ == nullptr) {
    return times_.Take();
  }
  Taken out;
  const gpu::FrameSlots::Taken t = slots_->TakeLatest();
  out.fresh = t.fresh;
  out.skipped = t.skipped;
  out.capture_ts_ns = t.slot != nullptr ? t.slot->meta.capture_ts_ns : 0;
  return out;
}

im_status VideoFrames::Convert(uint32_t layer, bool black, const gpu::Nv12Target &target) noexcept {
  if (device_ == nullptr || layer >= layer_count_ || !converters_[layer]) {
    return abi::Failf(IM_ERR_INTERNAL, {}, "session: no converter for layer %u (no GPU path)",
                      static_cast<unsigned>(layer));
  }
  const gpu::Slot *slot = black ? nullptr : slots_->current();
  return converters_[layer]->Convert(slot != nullptr ? slot->source() : black_, target);
}

im_status VideoFrames::Configure(uint32_t layer, const video::OutputSize &out, uint32_t fps) noexcept {
  if (device_ == nullptr || layer >= layer_count_ || !converters_[layer]) {
    return IM_OK;
  }
  gpu::Converter &c = *converters_[layer];
  c.ClearViews();
  const gpu::ConverterConfig &have = c.config();
  if (have.output.size == out.size && have.output.content == out.content && have.fps == fps) {
    return IM_OK;
  }
  gpu::ConverterConfig cc = have;
  cc.output = out;
  cc.fps = fps;
  return c.Reconfigure(cc);
}

} // namespace im::session
