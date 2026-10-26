#include "im/audiomix/dsp.h"

#include <cmath>

namespace im::audiomix {

float SoftClip(float x) noexcept {
  if (x <= kSoftClipKnee && x >= -kSoftClipKnee) {
    return x;
  }
  if (std::isnan(x)) {
    return 0.0f;
  }
  constexpr float kHead = 1.0f - kSoftClipKnee;
  const float a = std::fabs(x);
  const float y = kSoftClipKnee + kHead * std::tanh((a - kSoftClipKnee) / kHead);
  return x < 0 ? -y : y;
}

float TailFrame(float v, float s, uint32_t t) noexcept {
  constexpr float kW = 0.1f;
  if (t >= kTailRampFrames) {
    return 0.0f;
  }
  const auto tf = static_cast<float>(t);
  return (v + (s + kW * v) * tf) * std::exp(-kW * tf) * (1.0f - tf / static_cast<float>(kTailRampFrames));
}

float LinearToDb(double linear) noexcept {
  constexpr double kFloorLinear = 1e-6; // -120 dBFS
  if (!(linear > kFloorLinear)) {
    return kLevelFloorDb;
  }
  return static_cast<float>(20.0 * std::log10(linear));
}

} // namespace im::audiomix
