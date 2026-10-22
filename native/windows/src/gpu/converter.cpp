#include "gpu/converter.h"

#include <utility>

#include "api/guard.h"
#include "im/abi/status.h"
#include "im/log/ring.h"

namespace im::gpu {

im_status Converter::Create(Device &device, const ConverterConfig &config, std::unique_ptr<Converter> *out) noexcept {
  out->reset();
  return api::Guard("gpu converter", [&]() -> im_status {
    std::unique_ptr<Converter> c(new Converter(device, config));
    if (const im_status st = ShaderConverter::Create(device, &c->shader_); st != IM_OK) {
      return st;
    }
    if (config.video_processor) {
      // The slot textures start at the source's size: probe the VideoProcessor for it now (Convert creates it again
      // when a frame's texture has another size, e.g. a rotated Desktop Duplication output).
      if (const im_status st = c->EnsureVideoProcessor(config.source); st != IM_OK) {
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
  vp_.reset();
  path_ = Path::kShader;
}

im_status Converter::EnsureVideoProcessor(video::Size input) noexcept {
  if (vp_disabled_) {
    return IM_OK;
  }
  if (vp_ && vp_->input() == input && vp_->output() == config_.output.size) {
    return IM_OK;
  }
  vp_.reset();
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
  vp_ = std::move(vp);
  path_ = Path::kVideoProcessor;
#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
  if (fail_next_blt_) {
    vp_->FailNextBltForTest();
    fail_next_blt_ = false;
  }
#endif
  return IM_OK;
}

ConvertGeometry Converter::Geometry(const SourceFrame &src) const noexcept {
  return ComputeGeometry(config_.output, config_.source, src.texture_size, src.content, src.rotation);
}

im_status Converter::Convert(const SourceFrame &src, const Nv12Target &dst) noexcept {
  const ConvertGeometry g = Geometry(src);
  const bool empty = g.dest.width == 0 || g.dest.height == 0;
  if (!vp_disabled_ && !dst.planar() && !empty) {
    if (const im_status st = EnsureVideoProcessor(src.texture_size); st != IM_OK) {
      return st;
    }
    if (vp_ && (g.rotation == Rotation::kIdentity || vp_->rotates())) {
      const im_status st = vp_->Convert(src, g, dst);
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
  // The VideoProcessor's output size changed: Convert creates it again for the next frame.
  if (vp_ && !(vp_->output() == config_.output.size)) {
    vp_.reset();
  }
  return IM_OK;
}

void Converter::ClearViews() noexcept {
  if (shader_) {
    shader_->ClearViews();
  }
  if (vp_) {
    vp_->ClearViews();
  }
}

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
void Converter::FailNextVideoProcessorBltForTest() noexcept {
  if (vp_) {
    vp_->FailNextBltForTest();
  } else {
    fail_next_blt_ = true;
  }
}
#endif

} // namespace im::gpu
