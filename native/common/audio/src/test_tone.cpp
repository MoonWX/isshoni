// The test tone (test_tone.h; docs/m2/04-windows-engine.md §10.3).
#include "im/audio/test_tone.h"

#include <cmath>
#include <numbers>

#include "im/audio/opus_stage.h"

namespace im::audio {

float TestTone::PeriodSample(uint32_t n) noexcept {
  // internal/media/fake/testdata/gentone.go's sample(), in float instead of 16-bit.
  const double t = static_cast<double>(n % kRate);
  constexpr double kTwoPi = 2 * std::numbers::pi;
  double v = kToneBedLevel * std::sin(kTwoPi * kToneBedHz * t / kRate);
  if (n < kToneBeepFrames) {
    double env = 1.0;
    if (n < kToneEdgeFrames) {
      env = 0.5 - 0.5 * std::cos(std::numbers::pi * t / kToneEdgeFrames);
    } else if (n >= kToneBeepFrames - kToneEdgeFrames) {
      env = 0.5 - 0.5 * std::cos(std::numbers::pi * static_cast<double>(kToneBeepFrames - n) / kToneEdgeFrames);
    }
    v += kToneBeepLevel * env * std::sin(kTwoPi * kToneBeepHz * t / kRate);
  }
  return static_cast<float>(v);
}

uint32_t TestTone::PeriodIndex(int64_t captureNs) const noexcept {
  // Only the position within the second matters (the period is exactly one second), which also keeps the product
  // below in range for any clock value.
  int64_t r = (captureNs - epoch_ns_) % kNsPerSecond;
  if (r < 0) {
    r += kNsPerSecond;
  }
  const int64_t n = (r * int64_t{kRate} + kNsPerSecond / 2) / kNsPerSecond; // nearest sample, 0..kRate
  return static_cast<uint32_t>(n % int64_t{kRate});
}

void TestTone::Fill(float *frames, int64_t captureNs) noexcept {
  if (frames == nullptr) {
    return;
  }
  const bool silent = paused() || muted();
  uint32_t n = PeriodIndex(captureNs);
  for (uint32_t i = 0; i < kChunkFrames; i++) {
    if (silent && gain_steps_ > 0) {
      gain_steps_--;
    } else if (!silent && gain_steps_ < kToneFadeFrames) {
      gain_steps_++;
    }
    float s = 0.0f; // digital zero once faded out
    if (gain_steps_ == kToneFadeFrames) {
      s = PeriodSample(n);
    } else if (gain_steps_ > 0) {
      s = PeriodSample(n) * static_cast<float>(gain_steps_) / static_cast<float>(kToneFadeFrames);
    }
    frames[2 * i] = s;
    frames[2 * i + 1] = s;
    n = n + 1 == kRate ? 0 : n + 1;
  }
}

int64_t TestToneSource::Pump(int64_t now_ns) noexcept {
  if (sink_ == nullptr) {
    return now_ns + kChunkNs;
  }
  if (!started_) {
    started_ = true;
    sink_->OnGap(next_ns_); // the first chunk starts a grid
  }
  if (now_ns < next_ns_ + kDueAfterNs) {
    return next_ns_ + kDueAfterNs;
  }
  int64_t due = (now_ns - next_ns_ - kDueAfterNs) / kChunkNs + 1;
  if (due > kMaxLagChunks) {
    // Too far behind to catch up: a new grid at the newest due chunk, skipping a whole number of 20 ms frames so that
    // the packets stay on epoch_ns + k * 20 ms.
    int64_t skip = due - 1;
    skip -= skip % int64_t{kFrameChunks};
    next_ns_ += skip * kChunkNs;
    due -= skip;
    skips_++;
    sink_->OnGap(next_ns_);
  }
  for (; due > 0; due--) {
    tone_.Fill(buf_, next_ns_);
    sink_->OnChunk(buf_, next_ns_);
    next_ns_ += kChunkNs;
    chunks_++;
  }
  return next_ns_ + kDueAfterNs;
}

} // namespace im::audio
