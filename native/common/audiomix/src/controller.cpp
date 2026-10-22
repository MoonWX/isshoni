#include "im/audiomix/controller.h"

#include <cmath>

namespace im::audiomix {

namespace {

uint32_t Gcd(uint32_t a, uint32_t b) noexcept {
  while (b != 0) {
    const uint32_t t = a % b;
    a = b;
    b = t;
  }
  return a;
}

double Clamp(double v, double lo, double hi) noexcept { return v < lo ? lo : (v > hi ? hi : v); }

} // namespace

FillController::FillController(const ControllerParams &params) noexcept
    : p_(params), alpha_(params.ewma_seconds > 0
                             ? 1.0 - std::exp(-static_cast<double>(kChunkNs) / (params.ewma_seconds * kNsPerSecond))
                             : 1.0) {
  if (p_.update_ticks == 0) {
    p_.update_ticks = 1;
  }
  if (p_.max_steps < 1) {
    p_.max_steps = 1;
  }
}

void FillController::Seed(int64_t age) noexcept {
  avg_ns_ = static_cast<double>(age);
  seeded_ = true;
  ticks_ = 0;
}

double FillController::error_ms() const noexcept {
  return (avg_ns_ - static_cast<double>(p_.target_age_ns)) / static_cast<double>(kNsPerMs);
}

bool FillController::Tick(int64_t age) noexcept {
  if (!seeded_) {
    Seed(age);
  } else {
    avg_ns_ += alpha_ * (static_cast<double>(age) - avg_ns_);
  }
  if (hold_) {
    ticks_ = 0;
    return false;
  }
  if (++ticks_ < p_.update_ticks) {
    return false;
  }
  ticks_ = 0;
  Update();
  return true;
}

void FillController::Update() noexcept {
  const double dt = static_cast<double>(p_.update_ticks) * static_cast<double>(kChunkNs) / kNsPerSecond;
  const double e = error_ms();
  integral_ = Clamp(integral_ + e * dt, -p_.integral_limit, p_.integral_limit);
  const double max_ppm = static_cast<double>(p_.max_steps) * kPpmPerStep;
  const double raw = Clamp(p_.kp * e + p_.ki * integral_, -max_ppm, max_ppm);
  command_ += Clamp(raw - command_, -p_.slew_ppm, p_.slew_ppm);
  // The resampler takes whole steps; 0 is never used (§9.3): it becomes +1, or -1 after a negative ratio.
  auto steps = static_cast<int32_t>(std::lround(command_ / kPpmPerStep));
  if (steps == 0) {
    steps = steps_ < 0 ? -1 : 1;
  }
  if (steps > p_.max_steps) {
    steps = p_.max_steps;
  } else if (steps < -p_.max_steps) {
    steps = -p_.max_steps;
  }
  steps_ = steps;
}

RateModel::RateModel() noexcept {
  // speex_resampler_init_frac: the state starts zeroed (den 0, so no phase rescale), then set_rate_frac.
  SetSteps(1);
}

void RateModel::SetSteps(int32_t steps) noexcept {
  const auto n = static_cast<uint32_t>(static_cast<int64_t>(kRatioDen) + steps);
  const uint32_t d = kRatioDen;
  if (n == 0) {
    return; // speexdsp refuses it (RESAMPLER_ERR_INVALID_ARG)
  }
  if (num_ == n && den_ == d) {
    return; // speexdsp compares the stored (reduced) ratio with the new unreduced one
  }
  const uint32_t old_den = den_;
  const uint32_t g = Gcd(n, d);
  num_ = n / g;
  den_ = d / g;
  if (old_den > 0) {
    // multiply_frac(&frac, frac, den_, old_den): frac < old_den, so the major part is 0. No overflow: frac * den_ <
    // kRatioDen^2 < 2^32 (the reason for kRatioDen).
    frac_ = static_cast<uint32_t>(uint64_t{frac_} * den_ / old_den);
    if (frac_ >= den_) {
      frac_ = den_ - 1;
    }
  }
  int_adv_ = num_ / den_;
  frac_adv_ = num_ % den_;
}

void RateModel::Reset() noexcept {
  last_ = 0;
  frac_ = 0;
}

void RateModel::Run(const float *in, uint32_t in_frames, float *out, uint32_t out_frames, uint32_t *used,
                    uint32_t *made) noexcept {
  // speexdsp copies its input in pieces of kSpeexPiece frames (its buffer_size) and reports, for the piece it stopped
  // in, at most that piece as consumed: when the output fills, a position past the piece's end stays behind as
  // last_sample for the next call instead of being consumed now.
  constexpr int64_t kSpeexPiece = 160;
  uint32_t n = 0;
  int64_t last = last_;
  int64_t before = 0; // the position of the last output made
  uint32_t frac = frac_;
  const bool copy = in != nullptr && out != nullptr;
  const auto avail = static_cast<int64_t>(in_frames);
  while (last < avail && n < out_frames) {
    if (copy) {
      out[2 * static_cast<std::size_t>(n)] = in[2 * last];
      out[2 * static_cast<std::size_t>(n) + 1] = in[2 * last + 1];
    }
    ++n;
    before = last;
    last += int_adv_;
    frac += frac_adv_;
    if (frac >= den_) {
      frac -= den_;
      ++last;
    }
  }
  int64_t end = avail; // the input ran out: all of it is consumed
  if (n == out_frames && n > 0) {
    end = (before / kSpeexPiece + 1) * kSpeexPiece; // the output filled inside this piece
    end = end < avail ? end : avail;
  } else if (out_frames == 0) {
    end = 0;
  }
  const int64_t consumed = last < end ? last : end;
  last_ = last - consumed;
  frac_ = frac;
  *used = static_cast<uint32_t>(consumed);
  *made = n;
}

uint32_t RateModel::InputFor(uint32_t out_frames) const noexcept {
  if (out_frames == 0) {
    return 0;
  }
  // The last output needs its integer position to be an available frame: position after out_frames - 1 advances,
  // plus one.
  const uint64_t m = out_frames - 1;
  const uint64_t pos = static_cast<uint64_t>(last_) + m * int_adv_ + (uint64_t{frac_} + m * frac_adv_) / den_;
  return static_cast<uint32_t>(pos + 1);
}

} // namespace im::audiomix
