#include "gpu/video_processor.h"

#include <utility>

#include "api/guard.h"
#include "im/abi/status.h"

namespace im::gpu {
namespace {

D3D11_VIDEO_PROCESSOR_ROTATION ToD3d(Rotation r) noexcept {
  switch (r) {
  case Rotation::k90:
    return D3D11_VIDEO_PROCESSOR_ROTATION_90;
  case Rotation::k180:
    return D3D11_VIDEO_PROCESSOR_ROTATION_180;
  case Rotation::k270:
    return D3D11_VIDEO_PROCESSOR_ROTATION_270;
  case Rotation::kIdentity:
    break;
  }
  return D3D11_VIDEO_PROCESSOR_ROTATION_IDENTITY;
}

im_status Unavailable(const char *why) noexcept {
  return abi::Failf(IM_ERR_INTERNAL, {}, "gpu: no VideoProcessor: %s", why);
}

} // namespace

im_status VideoProcessorConverter::Create(Device &device, video::Size input, video::Size output, uint32_t fps,
                                          std::unique_ptr<VideoProcessorConverter> *out) noexcept {
  out->reset();
  if (device.video_device() == nullptr || device.video_context() == nullptr) {
    return Unavailable("the device has no video support");
  }
  if (!device.nv12_render_target()) {
    return Unavailable("the device can't render to NV12");
  }
  if (input.width == 0 || input.height == 0 || output.width == 0 || output.height == 0) {
    return abi::Fail(IM_ERR_INVALID_ARG, {}, "gpu: the VideoProcessor needs input and output sizes");
  }
  return api::Guard("gpu video processor", [&]() -> im_status {
    std::unique_ptr<VideoProcessorConverter> c(new VideoProcessorConverter(device));
    if (const im_status st = c->Init(input, output, fps); st != IM_OK) {
      return st;
    }
    c->inputs_.reserve(kMaxInputViews);
    c->outputs_.reserve(kMaxOutputViews);
    *out = std::move(c);
    return IM_OK;
  });
}

im_status VideoProcessorConverter::Init(video::Size input, video::Size output, uint32_t fps) noexcept {
  input_ = input;
  output_ = output;
  ID3D11VideoDevice *vd = device_.video_device();
  D3D11_VIDEO_PROCESSOR_CONTENT_DESC cd{};
  cd.InputFrameFormat = D3D11_VIDEO_FRAME_FORMAT_PROGRESSIVE;
  cd.InputFrameRate = DXGI_RATIONAL{fps == 0 ? 60u : fps, 1};
  cd.InputWidth = input.width;
  cd.InputHeight = input.height;
  cd.OutputFrameRate = cd.InputFrameRate;
  cd.OutputWidth = output.width;
  cd.OutputHeight = output.height;
  cd.Usage = D3D11_VIDEO_USAGE_OPTIMAL_SPEED;
  HRESULT hr = vd->CreateVideoProcessorEnumerator(&cd, enumerator_.GetAddressOf());
  if (FAILED(hr)) {
    if (device_.Removed()) {
      return device_.Fail(hr, "CreateVideoProcessorEnumerator");
    }
    return Unavailable("CreateVideoProcessorEnumerator failed");
  }
  UINT flags = 0;
  if (FAILED(enumerator_->CheckVideoProcessorFormat(DXGI_FORMAT_B8G8R8A8_UNORM, &flags)) ||
      (flags & D3D11_VIDEO_PROCESSOR_FORMAT_SUPPORT_INPUT) == 0) {
    return Unavailable("BGRA input is not supported");
  }
  flags = 0;
  if (FAILED(enumerator_->CheckVideoProcessorFormat(DXGI_FORMAT_NV12, &flags)) ||
      (flags & D3D11_VIDEO_PROCESSOR_FORMAT_SUPPORT_OUTPUT) == 0) {
    return Unavailable("NV12 output is not supported");
  }
  ComPtr<ID3D11VideoProcessorEnumerator1> enumerator1;
  BOOL conversion = FALSE;
  if (FAILED(enumerator_.As(&enumerator1)) ||
      FAILED(enumerator1->CheckVideoProcessorFormatConversion(
          DXGI_FORMAT_B8G8R8A8_UNORM, DXGI_COLOR_SPACE_RGB_FULL_G22_NONE_P709, DXGI_FORMAT_NV12,
          DXGI_COLOR_SPACE_YCBCR_STUDIO_G22_LEFT_P709, &conversion)) ||
      !conversion) {
    return Unavailable("RGB full range to BT.709 limited NV12 is not supported");
  }
  D3D11_VIDEO_PROCESSOR_CAPS caps{};
  if (SUCCEEDED(enumerator_->GetVideoProcessorCaps(&caps))) {
    rotates_ = (caps.FeatureCaps & D3D11_VIDEO_PROCESSOR_FEATURE_CAPS_ROTATION) != 0;
  }
  hr = vd->CreateVideoProcessor(enumerator_.Get(), 0, processor_.GetAddressOf());
  if (FAILED(hr)) {
    if (device_.Removed()) {
      return device_.Fail(hr, "CreateVideoProcessor");
    }
    return Unavailable("CreateVideoProcessor failed");
  }

  ContextLock lock(device_);
  ID3D11VideoContext1 *vc = device_.video_context();
  ID3D11VideoProcessor *vp = processor_.Get();
  vc->VideoProcessorSetStreamFrameFormat(vp, 0, D3D11_VIDEO_FRAME_FORMAT_PROGRESSIVE);
  vc->VideoProcessorSetStreamOutputRate(vp, 0, D3D11_VIDEO_PROCESSOR_OUTPUT_RATE_NORMAL, FALSE, nullptr);
  vc->VideoProcessorSetStreamAutoProcessingMode(vp, 0, FALSE); // no driver "enhancement"
  vc->VideoProcessorSetStreamColorSpace1(vp, 0, DXGI_COLOR_SPACE_RGB_FULL_G22_NONE_P709);
  vc->VideoProcessorSetOutputColorSpace1(vp, DXGI_COLOR_SPACE_YCBCR_STUDIO_G22_LEFT_P709);
  D3D11_VIDEO_COLOR black{};
  black.RGBA.R = 0.0f;
  black.RGBA.G = 0.0f;
  black.RGBA.B = 0.0f;
  black.RGBA.A = 1.0f;
  vc->VideoProcessorSetOutputBackgroundColor(vp, FALSE, &black); // RGB black: Y 16, Cb/Cr 128 in the output space
  vc->VideoProcessorSetOutputTargetRect(vp, FALSE, nullptr);     // the whole output
  return IM_OK;
}

void VideoProcessorConverter::ClearViews() noexcept {
  inputs_.clear();
  outputs_.clear();
}

im_status VideoProcessorConverter::InputFor(const SourceFrame &src, ID3D11VideoProcessorInputView **out) noexcept {
  for (const InputView &v : inputs_) {
    if (v.texture_id == src.texture_id) {
      *out = v.view.Get();
      return IM_OK;
    }
  }
  D3D11_VIDEO_PROCESSOR_INPUT_VIEW_DESC d{};
  d.FourCC = 0;
  d.ViewDimension = D3D11_VPIV_DIMENSION_TEXTURE2D;
  d.Texture2D.MipSlice = 0;
  d.Texture2D.ArraySlice = 0;
  InputView v;
  v.texture_id = src.texture_id;
  const HRESULT hr =
      device_.video_device()->CreateVideoProcessorInputView(src.texture, enumerator_.Get(), &d, v.view.GetAddressOf());
  if (FAILED(hr)) {
    return device_.Fail(hr, "CreateVideoProcessorInputView");
  }
  if (inputs_.size() >= kMaxInputViews) {
    inputs_.erase(inputs_.begin());
  }
  inputs_.push_back(std::move(v));
  *out = inputs_.back().view.Get();
  return IM_OK;
}

im_status VideoProcessorConverter::OutputFor(const Nv12Target &dst, ID3D11VideoProcessorOutputView **out) noexcept {
  for (const OutputView &v : outputs_) {
    if (v.texture == dst.texture && v.slice == dst.array_slice) {
      *out = v.view.Get();
      return IM_OK;
    }
  }
  D3D11_TEXTURE2D_DESC td{};
  dst.texture->GetDesc(&td);
  if (td.Format != DXGI_FORMAT_NV12 || td.Width != output_.width || td.Height != output_.height ||
      dst.array_slice >= td.ArraySize) {
    return abi::Failf(IM_ERR_INVALID_ARG, {}, "gpu: the VideoProcessor's target is not a %ux%u NV12 texture",
                      static_cast<unsigned>(output_.width), static_cast<unsigned>(output_.height));
  }
  D3D11_VIDEO_PROCESSOR_OUTPUT_VIEW_DESC d{};
  if (td.ArraySize > 1) {
    d.ViewDimension = D3D11_VPOV_DIMENSION_TEXTURE2DARRAY;
    d.Texture2DArray.MipSlice = 0;
    d.Texture2DArray.FirstArraySlice = dst.array_slice;
    d.Texture2DArray.ArraySize = 1;
  } else {
    d.ViewDimension = D3D11_VPOV_DIMENSION_TEXTURE2D;
    d.Texture2D.MipSlice = 0;
  }
  OutputView v;
  v.texture = dst.texture;
  v.slice = dst.array_slice;
  const HRESULT hr =
      device_.video_device()->CreateVideoProcessorOutputView(dst.texture, enumerator_.Get(), &d, v.view.GetAddressOf());
  if (FAILED(hr)) {
    return device_.Fail(hr, "CreateVideoProcessorOutputView");
  }
  if (outputs_.size() >= kMaxOutputViews) {
    outputs_.erase(outputs_.begin());
  }
  outputs_.push_back(std::move(v));
  *out = outputs_.back().view.Get();
  return IM_OK;
}

im_status VideoProcessorConverter::Convert(const SourceFrame &src, const ConvertGeometry &g,
                                           const Nv12Target &dst) noexcept {
  if (dst.planar() || src.texture == nullptr) {
    return abi::Fail(IM_ERR_INVALID_ARG, {}, "gpu: the VideoProcessor needs a source texture and an NV12 target");
  }
  if (g.dest.width == 0 || g.dest.height == 0) {
    return abi::Fail(IM_ERR_INVALID_ARG, {}, "gpu: the VideoProcessor needs a destination rectangle");
  }
  return api::Guard("gpu video processor", [&]() -> im_status {
    ID3D11VideoProcessorInputView *in = nullptr;
    ID3D11VideoProcessorOutputView *ov = nullptr;
    if (const im_status st = InputFor(src, &in); st != IM_OK) {
      return st;
    }
    if (const im_status st = OutputFor(dst, &ov); st != IM_OK) {
      return st;
    }
    RECT source{0, 0, static_cast<LONG>(g.content.width), static_cast<LONG>(g.content.height)};
    RECT dest{static_cast<LONG>(g.dest.x), static_cast<LONG>(g.dest.y), static_cast<LONG>(g.dest.x + g.dest.width),
              static_cast<LONG>(g.dest.y + g.dest.height)};
    ContextLock lock(device_);
    ID3D11VideoContext1 *vc = device_.video_context();
    ID3D11VideoProcessor *vp = processor_.Get();
    vc->VideoProcessorSetStreamSourceRect(vp, 0, TRUE, &source);
    vc->VideoProcessorSetStreamDestRect(vp, 0, TRUE, &dest);
    vc->VideoProcessorSetStreamRotation(vp, 0, g.rotation != Rotation::kIdentity ? TRUE : FALSE, ToD3d(g.rotation));
    D3D11_VIDEO_PROCESSOR_STREAM stream{};
    stream.Enable = TRUE;
    stream.pInputSurface = in;
    HRESULT hr = vc->VideoProcessorBlt(vp, ov, 0, 1, &stream);
#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
    if (fail_next_blt_) {
      fail_next_blt_ = false;
      hr = E_FAIL;
    }
#endif
    if (FAILED(hr)) {
      return device_.Fail(hr, "VideoProcessorBlt");
    }
    return IM_OK;
  });
}

} // namespace im::gpu
