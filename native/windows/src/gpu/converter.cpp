#include "gpu/converter.h"

#include <algorithm>
#include <utility>

#include "api/guard.h"
#include "im/abi/status.h"
#include "im/log/ring.h"

namespace im::gpu {
namespace {

// The slot texture size a converter makes its first VideoProcessor for.
video::Size ProbeInput(const ConverterConfig &config) noexcept {
  return config.input.width != 0 && config.input.height != 0 ? config.input : config.source;
}

} // namespace

im_status Converter::Create(Device &device, const ConverterConfig &config, std::unique_ptr<Converter> *out) noexcept {
  out->reset();
  return api::Guard("gpu converter", [&]() -> im_status {
    std::unique_ptr<Converter> c(new Converter(device, config));
    if (const im_status st = ShaderConverter::Create(device, &c->shader_); st != IM_OK) {
      return st;
    }
    if (config.video_processor) {
      c->vps_.reserve(kMaxVideoProcessors);
      // The slot textures start at config.input: make the VideoProcessor for it now, on this thread (Convert makes
      // another one only for a slot that grew).
      VideoProcessorConverter *vp = nullptr;
      if (const im_status st = c->VideoProcessorFor(ProbeInput(config), &vp); st != IM_OK) {
        return st;
      }
    } else {
      c->vp_disabled_ = true;
    }
    log::Info("gpu", "converter",
              {{"path", c->path_ == Path::kVideoProcessor ? "video_processor" : "shader"},
               {"w", config.output.size.width},
               {"h", config.output.size.height},
               {"letterboxed", config.output.letterboxed}});
    *out = std::move(c);
    return IM_OK;
  });
}

void Converter::UseShader(std::string_view why) noexcept {
  if (!vp_disabled_) {
    log::Warn("gpu", "converter uses the shader", {{"why", why}});
  }
  vp_disabled_ = true;
  vps_.clear();
  path_ = Path::kShader;
}

im_status Converter::VideoProcessorFor(video::Size input, VideoProcessorConverter **out) noexcept {
  *out = nullptr;
  if (vp_disabled_) {
    return IM_OK;
  }
  for (auto it = vps_.begin(); it != vps_.end(); ++it) {
    if ((*it)->input() == input && (*it)->output() == config_.output.size) {
      std::rotate(it, it + 1, vps_.end()); // the most recently used last
      *out = vps_.back().get();
      return IM_OK;
    }
  }
  std::unique_ptr<VideoProcessorConverter> vp;
  const im_status st = VideoProcessorConverter::Create(device_, input, config_.output.size, config_.fps, &vp);
  if (st == IM_ERR_ENCODER_LOST) {
    return st;
  }
  if (st != IM_OK) {
    UseShader(abi::LastErrorText());
    abi::ClearLastError();
    return IM_OK;
  }
  if (vps_.size() >= kMaxVideoProcessors) {
    vps_.erase(vps_.begin()); // the least recently used
  }
  vps_.push_back(std::move(vp));
  path_ = Path::kVideoProcessor;
#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
  ++vp_created_;
#endif
  *out = vps_.back().get();
  return IM_OK;
}

ConvertGeometry Converter::Geometry(const SourceFrame &src) const noexcept {
  return ComputeGeometry(config_.output, config_.source, src.texture_size, src.content, src.rotation);
}

im_status Converter::Convert(const SourceFrame &src, const Nv12Target &dst) noexcept {
  const ConvertGeometry g = Geometry(src);
  const bool empty = g.dest.width == 0 || g.dest.height == 0;
  if (!vp_disabled_ && !dst.planar() && !empty) {
    VideoProcessorConverter *vp = nullptr;
    if (const im_status st = VideoProcessorFor(src.texture_size, &vp); st != IM_OK) {
      return st;
    }
    if (vp != nullptr && (g.rotation == Rotation::kIdentity || vp->rotates())) {
#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
      if (fail_next_blt_) {
        vp->FailNextBltForTest();
        fail_next_blt_ = false;
      }
#endif
      const im_status st = vp->Convert(src, g, dst);
      if (st == IM_OK || st == IM_ERR_ENCODER_LOST || st == IM_ERR_INVALID_ARG) {
        return st;
      }
      // The first failed Blt on a healthy device: the shader from now on (04 §8.3).
      UseShader(abi::LastErrorText());
      abi::ClearLastError();
    }
  }
  return shader_->Convert(src, g, dst);
}

im_status Converter::Reconfigure(const ConverterConfig &config) noexcept {
  const bool keep_vp = !vp_disabled_ && config.video_processor;
  config_ = config;
  if (!keep_vp) {
    if (!vp_disabled_) {
      UseShader("the shader was asked for");
    }
    return IM_OK;
  }
  // VideoProcessors for the old output size can't write the new one; the one for the slots' size is made here, not on
  // the next frame.
  std::erase_if(
      vps_, [&](const std::unique_ptr<VideoProcessorConverter> &vp) { return !(vp->output() == config_.output.size); });
  VideoProcessorConverter *vp = nullptr;
  return VideoProcessorFor(ProbeInput(config_), &vp);
}

void Converter::ClearViews() noexcept {
  if (shader_) {
    shader_->ClearViews();
  }
  for (const std::unique_ptr<VideoProcessorConverter> &vp : vps_) {
    vp->ClearViews();
  }
}

} // namespace im::gpu
