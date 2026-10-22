// The mix bus's sample rules (docs/m2/05-windows-audio.md §9.4-§9.6): the soft clip, the fade lengths and the level
// arithmetic.
#pragma once

#include <cstdint>

#include "im/audiomix/constants.h"

namespace im::audiomix {

inline constexpr uint32_t kFadeFrames = kChunkFrames;          // starts, stops, silence flags: 10 ms (§9.4, §9.6)
inline constexpr uint32_t kCrossfadeFrames = 2 * kChunkFrames; // between engines: 20 ms equal-gain (§9.4, §6.5)
inline constexpr uint32_t kCutFadeFrames = 5 * kFramesPerMs;   // the fast path's cut: 5 ms (§6.4)
inline constexpr uint32_t kTailRampFrames = 64;                // a chunk that ends early (§9.4)
// The onset fade (§9.5): sound after at least 50 ms of digital silence is ramped in over 5 ms. MixEngine applies it to
// the stream's input, before the resampler (mix_engine.cpp says why), where §9.5 puts it after.
inline constexpr uint32_t kOnsetSilenceFrames = 50 * kFramesPerMs;
inline constexpr uint32_t kOnsetRampFrames = 5 * kFramesPerMs;

// Soft clip above -1 dBFS (§9.6): for |x| > t (t = 0.891), y = sign(x) * (t + (1 - t) * tanh((|x| - t) / (1 - t))).
// Continuous with slope 1 at t; |y| <= 1 for every x; NaN becomes 0.
inline constexpr float kSoftClipKnee = 0.891f;
float SoftClip(float x) noexcept;

// 20 * log10(linear) with the -120 dBFS floor (§9.6); 0 and below give the floor.
float LinearToDb(double linear) noexcept;

// The backstop tail of a channel whose audio ended mid-chunk while audible (§9.4): 64 frames from its last value v to
// zero. S1 ramped linearly, which bends the waveform at once: a -6 dBFS 1 kHz tone cut mid-wave then shows a second
// difference of up to 0.07, a click by S1's own detector (threshold 0.03). This tail continues the last slope s and
// decays it: frame t (1..64) is (v + (s + w * v) * t) * exp(-w * t) * (1 - t / 64) with w = 0.1, a critically damped
// return to zero that the linear window ends at exactly zero; its second differences stay below 0.03 for such a tone
// at every phase (mix_engine_test checks it).
float TailFrame(float v, float s, uint32_t t) noexcept;

} // namespace im::audiomix
