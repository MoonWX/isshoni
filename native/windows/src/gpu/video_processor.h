// The VideoProcessor converter (docs/m2/04-windows-engine.md §8.3, §7.9): one ID3D11VideoProcessor per layer, input
// BGRA at the slot texture's size, output NV12 at the layer's size. Color: stream
// DXGI_COLOR_SPACE_RGB_FULL_G22_NONE_P709, output DXGI_COLOR_SPACE_YCBCR_STUDIO_G22_LEFT_P709 (BT.709, limited range);
// auto processing off, so drivers add no "enhancement"; source rectangle = the content, destination rectangle = the
// letterboxed fit, background black (RGB black: Y 16, Cb/Cr 128). Rotation through VideoProcessorSetStreamRotation
// where the driver offers it.
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
#include "im/video/scale.h"
#include "isshoni_media.h"

namespace im::gpu {

class VideoProcessorConverter {
public:
  // A VideoProcessor for frames in textures of `input` size and NV12 output of `output` size at fps. IM_ERR_INTERNAL
  // when the device offers none for these formats and color spaces (no video support, no NV12 render targets, the
  // enumerator, a format or the conversion refused), and the caller converts with the shader; IM_ERR_ENCODER_LOST
  // (device_removed) on device loss. The thread's detail is set either way.
  static im_status Create(Device &device, video::Size input, video::Size output, uint32_t fps,
                          std::unique_ptr<VideoProcessorConverter> *out) noexcept;

  VideoProcessorConverter(const VideoProcessorConverter &) = delete;
  VideoProcessorConverter &operator=(const VideoProcessorConverter &) = delete;

  video::Size input() const noexcept { return input_; }
  video::Size output() const noexcept { return output_; }
  // Whether the driver rotates (D3D11_VIDEO_PROCESSOR_FEATURE_CAPS_ROTATION); without it rotated frames take the
  // shader.
  bool rotates() const noexcept { return rotates_; }

  // Converts src (a texture of input() size) into dst (an NV12 texture or slice of output() size) by g; g.dest must
  // not be empty. A failed VideoProcessorBlt returns its Device::Fail status.
  im_status Convert(const SourceFrame &src, const ConvertGeometry &g, const Nv12Target &dst) noexcept;

  // Forgets the cached input and output views (and the texture references they hold).
  void ClearViews() noexcept;

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
  // gpu_warp_test: the next VideoProcessorBlt fails with E_FAIL, as a driver's would (04 §8.3's fallback).
  void FailNextBltForTest() noexcept { fail_next_blt_ = true; }
#endif

private:
  struct InputView {
    uint64_t texture_id = 0;
    ComPtr<ID3D11VideoProcessorInputView> view;
  };
  struct OutputView {
    ID3D11Texture2D *texture = nullptr;
    uint32_t slice = 0;
    ComPtr<ID3D11VideoProcessorOutputView> view;
  };
  static constexpr std::size_t kMaxInputViews = 8;
  static constexpr std::size_t kMaxOutputViews = 16;

  explicit VideoProcessorConverter(Device &device) noexcept : device_(device) {}
  im_status Init(video::Size input, video::Size output, uint32_t fps) noexcept;
  im_status InputFor(const SourceFrame &src, ID3D11VideoProcessorInputView **out) noexcept;
  im_status OutputFor(const Nv12Target &dst, ID3D11VideoProcessorOutputView **out) noexcept;

  Device &device_;
  video::Size input_;
  video::Size output_;
  bool rotates_ = false;
  ComPtr<ID3D11VideoProcessorEnumerator> enumerator_;
  ComPtr<ID3D11VideoProcessor> processor_;
  std::vector<InputView> inputs_;
  std::vector<OutputView> outputs_;
#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
  bool fail_next_blt_ = false;
#endif
};

} // namespace im::gpu
