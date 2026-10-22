#include "im/audiomix/resampler.h"

#include <new>

#include <speex/speex_resampler.h>

namespace im::audiomix {

namespace {

constexpr int kQuality = 5; // §9.3

class SpeexResampler final : public Resampler {
public:
  explicit SpeexResampler(SpeexResamplerState *st) noexcept : st_(st) {}
  ~SpeexResampler() override { speex_resampler_destroy(st_); }
  SpeexResampler(const SpeexResampler &) = delete;
  SpeexResampler &operator=(const SpeexResampler &) = delete;

  void SetSteps(int32_t steps) noexcept override {
    if (steps == steps_ || steps == 0) {
      return;
    }
    const auto num = static_cast<spx_uint32_t>(static_cast<int64_t>(kRatioDen) + steps);
    (void)speex_resampler_set_rate_frac(st_, num, kRatioDen, kRate, kRate);
    steps_ = steps;
  }

  void Reset() noexcept override { (void)speex_resampler_reset_mem(st_); }

  void Process(const float *in, uint32_t in_frames, float *out, uint32_t out_frames, uint32_t *used,
               uint32_t *made) noexcept override {
    spx_uint32_t il = in_frames, ol = out_frames;
    (void)speex_resampler_process_interleaved_float(st_, in, &il, out, &ol);
    *used = il;
    *made = ol;
  }

  int32_t steps() const noexcept override { return steps_; }

private:
  SpeexResamplerState *st_;
  int32_t steps_ = 1;
};

class ModelResampler final : public Resampler {
public:
  void SetSteps(int32_t steps) noexcept override {
    if (steps == steps_ || steps == 0) {
      return;
    }
    model_.SetSteps(steps);
    steps_ = steps;
  }
  void Reset() noexcept override { model_.Reset(); }
  void Process(const float *in, uint32_t in_frames, float *out, uint32_t out_frames, uint32_t *used,
               uint32_t *made) noexcept override {
    model_.Run(in, in_frames, out, out_frames, used, made);
  }
  int32_t steps() const noexcept override { return steps_; }

private:
  RateModel model_;
  int32_t steps_ = 1;
};

} // namespace

im_status NewSpeexResampler(std::unique_ptr<Resampler> *out) {
  int err = 0;
  SpeexResamplerState *st =
      speex_resampler_init_frac(kChannels, kRatioDen + 1, kRatioDen, kRate, kRate, kQuality, &err);
  if (st == nullptr || err != RESAMPLER_ERR_SUCCESS) {
    if (st != nullptr) {
      speex_resampler_destroy(st);
    }
    return IM_ERR_INTERNAL;
  }
  auto *r = new (std::nothrow) SpeexResampler(st);
  if (r == nullptr) {
    speex_resampler_destroy(st);
    return IM_ERR_INTERNAL;
  }
  out->reset(r);
  return IM_OK;
}

std::unique_ptr<Resampler> NewModelResampler() { return std::make_unique<ModelResampler>(); }

} // namespace im::audiomix
