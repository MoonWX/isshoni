#include "im/encpolicy/reinit.h"

#include <algorithm>

namespace im::encpolicy {

BitrateDecision ReinitPlanner::SetBitrate(uint32_t bps) noexcept {
  if (live_) {
    if (bps == bitrate_) {
      return BitrateDecision::kNoop;
    }
    bitrate_ = bps;
    return BitrateDecision::kLive;
  }
  if (bps == bitrate_) {
    pending_bitrate_.reset(); // the latest value wins: back to the current one, nothing to do
    return BitrateDecision::kNoop;
  }
  pending_bitrate_ = bps;
  return BitrateDecision::kPending;
}

int64_t ReinitPlanner::NextDeadlineNs() const noexcept {
  const int64_t spaced = last_reinit_ + kReinitSpacingNs;
  int64_t at = kNever;
  if (pending_ != 0) {
    at = spaced;
  }
  if (pending_bitrate_) {
    at = std::min(at, std::max(spaced, last_bitrate_reinit_ + kBitrateReinitSpacingNs));
  }
  return at;
}

std::optional<ReinitAction> ReinitPlanner::Poll(int64_t now_ns) noexcept {
  const int64_t at = NextDeadlineNs();
  if (at == kNever || now_ns < at) {
    return std::nullopt;
  }
  ReinitAction a;
  a.reasons = pending_;
  a.bitrate_bps = bitrate_;
  if (pending_bitrate_) {
    a.reasons |= reinit::kBitrate;
    a.bitrate_bps = *pending_bitrate_;
    last_bitrate_reinit_ = now_ns;
  }
  last_reinit_ = now_ns;
  bitrate_ = a.bitrate_bps;
  pending_ = 0;
  pending_bitrate_.reset();
  return a;
}

void ReinitPlanner::OnReopened(int64_t now_ns, uint32_t bitrate_bps, bool is_live) noexcept {
  last_reinit_ = now_ns;
  if (bitrate_bps != bitrate_) {
    last_bitrate_reinit_ = now_ns;
  }
  bitrate_ = bitrate_bps;
  live_ = is_live;
  if (pending_bitrate_ && *pending_bitrate_ == bitrate_bps) {
    pending_bitrate_.reset();
  }
  if (live_ && pending_bitrate_) {
    bitrate_ = *pending_bitrate_;
    pending_bitrate_.reset();
  }
}

} // namespace im::encpolicy
