// A stream's resampler (docs/m2/05-windows-audio.md §9.3): speexdsp in production; a sample-and-hold stand-in with
// speexdsp's exact consumption (RateModel) for the long simulations, where speexdsp's filter would cost minutes in a
// debug build. Ratios are (kRatioDen + steps) / kRatioDen input frames per output frame (controller.h).
#pragma once

#include <cstdint>
#include <memory>

#include "im/audiomix/controller.h"
#include "isshoni_media.h"

namespace im::audiomix {

class Resampler {
public:
  virtual ~Resampler() = default;
  // The ratio; steps is never 0 (the controller guarantees it, controller.h). Allocates nothing.
  virtual void SetSteps(int32_t steps) noexcept = 0;
  // Forgets the filter history and phase (speex_resampler_reset_mem): the next output holds no input from before.
  virtual void Reset() noexcept = 0;
  // Makes up to out_frames interleaved stereo frames from up to in_frames: *used input frames consumed, *made output
  // frames written (fewer than out_frames only when the input ran out). Allocates nothing.
  virtual void Process(const float *in, uint32_t in_frames, float *out, uint32_t out_frames, uint32_t *used,
                       uint32_t *made) noexcept = 0;
  virtual int32_t steps() const noexcept = 0;
};

// speexdsp: speex_resampler_init_frac(2, kRatioDen + 1, kRatioDen, 48000, 48000, 5) on the calling thread (the
// watcher's, before the channel joins the mixer): quality 5, created at +1 step so the interpolated filter table
// (80 taps x 16 phases + 8) is allocated now and never on the mixer thread. IM_ERR_INTERNAL when speexdsp fails.
im_status NewSpeexResampler(std::unique_ptr<Resampler> *out);

// The stand-in: no filter, speexdsp's consumption to the frame. Tests and simulations only.
std::unique_ptr<Resampler> NewModelResampler();

} // namespace im::audiomix
