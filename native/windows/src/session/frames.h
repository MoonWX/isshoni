// A session's frames between the capture side and the encoders (docs/m2/04-windows-engine.md §8.1–§8.3, §6.3, §8.5;
// README WF2).
//
// A session whose encoders take pixels (encoder.h, FrameInput::kNv12: every real encoder) runs the GPU path on its own
// gpu::Device (§8.1, created by the session on the encoder's adapter): gpu::FrameSlots at the source size, the GPU test
// pattern that "test:pattern" draws into them (gpu/test_pattern.h; it ships in release builds), one gpu::Converter per
// layer (the VideoProcessor, or the shader as fallback, §8.3) and a black texture for the black picture of the
// first-keyframe fallback (§6.3). A session whose encoders take nothing (the test hooks' IM_ENC_FAKE) has no device:
// its frames are capture times only, through the same triple-buffer index as the slots (frame_times.h), so
// session_fake_test and Go's Conformance never touch a GPU (WARP on CI runners) and keep their timing.
//
//   capture side   PublishPattern(frame k, capture time)   the supervisor's pattern timer: BeginWrite → TestPattern::
//                                                          Render → Publish (or the time alone without a device)
//                  slots()                                 README WA50's WGC callback and WA60's DD thread write here
//   video thread   Take()                                  the newest published frame (its capture time and the frames
//                                                          skipped since the last Take, for the pacer); the reader's
//                                                          slot keeps it as the last frame for refreshes and keyframes
//                  Convert(layer, black, target)           that frame, or black, into an encoder's NV12 target
//                  Configure(layer, output, fps)           after a layer's encoder (re)opened: its targets and
//                                                          geometry (a re-init, §7.7, §9.5; a fallback, §9.8)
//
// The session doesn't Take while it is paused (§8.5): the reader's slot keeps the frozen frame, so refreshes and
// keyframes of a paused session never show content captured after the pause took effect, while capture runs on and
// resume takes the newest frame at once.
//
// Threads: Create on the engine worker (im_start); PublishPattern on the one capture-side thread; Take, Convert and
// Configure on the video thread. The GPU calls take the device's context lock themselves, never across a wait (§6.1).
#pragma once

#include <array>
#include <cstdint>
#include <memory>
#include <span>

#include "gpu/converter.h"
#include "gpu/device.h"
#include "gpu/frame.h"
#include "gpu/pattern_layout.h"
#include "gpu/slots.h"
#include "gpu/test_pattern.h"
#include "im/video/scale.h"
#include "isshoni_media.h"
#include "session/frame_times.h"

namespace im::session {

class VideoFrames {
public:
  // One layer's geometry: its output size (im::video::ComputeOutputSize of the source) and frame rate.
  struct LayerSetup {
    video::OutputSize out;
    uint32_t fps = 60;
  };
  struct Config {
    video::Size source;                 // the source's size at start: the slots' and the pattern's (each side ≥ 2)
    bool pattern = false;               // draw the GPU test pattern (the test: capture path)
    std::span<const LayerSetup> layers; // at most IM_MAX_LAYERS
  };

  // The GPU path on device, or (device null) frame times only. Failures set the thread's detail: IM_ERR_ENCODER_LOST
  // (device_removed) on device loss, IM_ERR_INTERNAL or IM_ERR_INVALID_ARG otherwise.
  static im_status Create(gpu::Device *device, const Config &config, std::unique_ptr<VideoFrames> *out) noexcept;

  VideoFrames(const VideoFrames &) = delete;
  VideoFrames &operator=(const VideoFrames &) = delete;

  // Whether the session runs the GPU path (has a device).
  bool has_device() const noexcept { return device_ != nullptr; }
  gpu::Device *device() const noexcept { return device_; }

  // ---- the capture side (one writer thread)
  // Publishes test-pattern frame f, captured at capture_ts_ns: drawn into a slot by the GPU, or only its time without
  // a device. IM_ERR_ENCODER_LOST (device_removed) on device loss; IM_ERR_INTERNAL when the GPU path has no pattern.
  im_status PublishPattern(const gpu::PatternFrame &f, int64_t capture_ts_ns) noexcept;
  // The slots a real capture writes (null without a device).
  gpu::FrameSlots *slots() noexcept { return slots_.get(); }

  // ---- the video thread
  using Taken = TakenFrame;
  Taken Take() noexcept;

  // Converts the reader's frame (the newest one taken), or black when `black` is set or nothing was taken yet, into
  // target for the layer: letterboxed to the layer's output size, BT.709 limited range (§8.3). Device loss →
  // IM_ERR_ENCODER_LOST (device_removed); IM_ERR_INTERNAL without a device.
  im_status Convert(uint32_t layer, bool black, const gpu::Nv12Target &target) noexcept;

  // After the layer's encoder (re)opened: the layer's converter forgets the views of the previous encoder's targets
  // (its frame pool is gone, 04 §9.2; the views would keep its textures alive) and follows a new output size or frame
  // rate (gpu::Converter::Reconfigure). No-op without a device; fails only on device loss.
  im_status Configure(uint32_t layer, const video::OutputSize &out, uint32_t fps) noexcept;

private:
  VideoFrames() = default;
  im_status InitGpu(const Config &config) noexcept;

  gpu::Device *device_ = nullptr;
  video::Size source_;
  uint32_t layer_count_ = 0;

  // ---- the GPU path
  std::unique_ptr<gpu::FrameSlots> slots_;
  std::unique_ptr<gpu::TestPattern> pattern_;
  std::array<std::unique_ptr<gpu::Converter>, IM_MAX_LAYERS> converters_;
  gpu::ComPtr<ID3D11Texture2D> black_texture_;
  gpu::ComPtr<ID3D11ShaderResourceView> black_srv_;
  gpu::SourceFrame black_; // the black texture with no content: the converters write an all-black picture

  // ---- frame times only (no device)
  FrameTimes times_;
};

} // namespace im::session
