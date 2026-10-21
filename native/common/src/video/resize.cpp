#include "im/video/resize.h"

namespace im::video {

bool ResizeDebouncer::OnSourceSize(Size size, int64_t now_ns) noexcept {
  if (size == current_) {
    return false;
  }
  current_ = size;
  stable_since_ = now_ns;
  if (pending_) {
    return false; // the same episode: the clock restarts, no new event
  }
  pending_ = true;
  return true;
}

std::optional<Size> ResizeDebouncer::Poll(int64_t now_ns) noexcept {
  if (!pending_ || now_ns < stable_since_ + kResizeDebounceNs) {
    return std::nullopt;
  }
  pending_ = false;
  encoded_ = current_;
  return current_;
}

LayerResize PlanLayerResize(Size new_source, const LayerBounds &bounds, Size current_output) noexcept {
  LayerResize r;
  r.output = ComputeOutputSize(new_source, bounds);
  r.reinit = !(r.output.size == current_output);
  return r;
}

} // namespace im::video
