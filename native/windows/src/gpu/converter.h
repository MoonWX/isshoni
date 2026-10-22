// A layer's converter (docs/m2/04-windows-engine.md §8.3): the VideoProcessor first, the shader converter as the
// fallback. The VideoProcessor is used when it can be created for the device, the formats and the color spaces; when
// it can't, or the first time a VideoProcessorBlt fails on a healthy device, the layer switches to the shader for
// good (logged). Per frame the shader also takes what the VideoProcessor can't do: a rotated frame on a driver
// without rotation, and an empty destination (all black). Device loss is never a fallback: it is
// IM_ERR_ENCODER_LOST (device_removed), which ends the session (§9.8).
//
// The session (README WA44's encoders, the GPU test pattern) uses one per layer: Create at start with the layer's
// output size, Reconfigure after a stable resize or an fps re-init (§7.7, §9.5), Convert for every encode of the
// layer, into the encoder's hardware frame (an FFmpeg pool slice) or our own Nv12Surface for a CPU encoder (nv12.h).
//
// A VideoProcessor is made for one input texture size. The frame slots' textures can differ in size (only the writer's
// slot grows, slots.h), so the converter keeps one per size, up to one per slot (the least recently used one goes),
// and alternating slot sizes reuse them. Create and Reconfigure make the one for ConverterConfig::input, the slots'
// starting size, so a session's first frames create none on the video thread.
//
// Threads: Create and Reconfigure on the engine worker or the video thread; Convert on the video thread.
#pragma once

#include <cstddef>
#include <cstdint>
#include <memory>
#include <string_view>
#include <vector>

#include "gpu/device.h"
#include "gpu/frame.h"
#include "gpu/geometry.h"
#include "gpu/shader_convert.h"
#include "gpu/triple_buffer.h"
#include "gpu/video_processor.h"
#include "im/video/scale.h"
#include "isshoni_media.h"

namespace im::gpu {

struct ConverterConfig {
  video::OutputSize output; // the layer's (im::video::ComputeOutputSize of `source`)
  video::Size source;       // the upright source size `output` was computed for
  // The frame slots' texture size, in the capture's orientation (FrameSlots::Create's size: for a Desktop Duplication
  // output turned 90 or 270 degrees, `source` with its sides swapped). Empty: `source`.
  video::Size input;
  uint32_t fps = 60;
  bool video_processor = true; // false: the shader only
};

class Converter {
public:
  enum class Path : uint8_t { kVideoProcessor, kShader };

  // Fails only when the shader converter can't be created (the thread's detail set); a missing VideoProcessor is
  // logged and leaves the path at kShader.
  static im_status Create(Device &device, const ConverterConfig &config, std::unique_ptr<Converter> *out) noexcept;

  Converter(const Converter &) = delete;
  Converter &operator=(const Converter &) = delete;

  // Converts src into dst, an NV12 picture of the layer's output size.
  im_status Convert(const SourceFrame &src, const Nv12Target &dst) noexcept;

  // A new output geometry: the shader's views stay; VideoProcessors for another output size go, and the one for
  // config.input is made now. Fails only on device loss (IM_ERR_ENCODER_LOST); a VideoProcessor that can't be made
  // switches the layer to the shader for good.
  im_status Reconfigure(const ConverterConfig &config) noexcept;

  // The geometry Convert uses for src.
  ConvertGeometry Geometry(const SourceFrame &src) const noexcept;

  Path path() const noexcept { return path_; }
  const ConverterConfig &config() const noexcept { return config_; }

  // After the encoder's frame pool was replaced: forget the views of its textures.
  void ClearViews() noexcept;

  // The most VideoProcessors a converter keeps: one per frame slot texture size.
  static constexpr std::size_t kMaxVideoProcessors = TripleIndex::kSlots;

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
  // gpu_warp_test: the VideoProcessor's next Blt fails (04 §8.3's fallback). No effect on the shader path.
  void FailNextVideoProcessorBltForTest() noexcept { fail_next_blt_ = true; }
  // gpu_warp_test: how many VideoProcessors this converter has created.
  uint32_t video_processors_created_for_test() const noexcept { return vp_created_; }
#endif

private:
  Converter(Device &device, const ConverterConfig &config) noexcept : device_(device), config_(config) {}
  // The VideoProcessor for frames in textures of `input` size: a kept one (now the most recently used), or a new one
  // that replaces the least recently used when kMaxVideoProcessors are kept. *out is null when the VideoProcessor is
  // off for good, or was turned off now because none can be created for input (UseShader). Fails only on device loss.
  im_status VideoProcessorFor(video::Size input, VideoProcessorConverter **out) noexcept;
  // Turns the VideoProcessor off for good (logged once with why).
  void UseShader(std::string_view why) noexcept;

  Device &device_;
  ConverterConfig config_;
  Path path_ = Path::kShader;
  bool vp_disabled_ = false;
  std::unique_ptr<ShaderConverter> shader_;
  std::vector<std::unique_ptr<VideoProcessorConverter>> vps_; // the most recently used last; capacity reserved
#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
  bool fail_next_blt_ = false;
  uint32_t vp_created_ = 0;
#endif
};

} // namespace im::gpu
