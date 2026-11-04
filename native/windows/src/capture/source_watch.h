// The source changes a session's supervisor turns into events (docs/m2/04-windows-engine.md §7.6, §7.7), as plain
// state machines over the engine clock (window_match_test covers them):
//
// - ResizeTracker: WGC reports each frame's ContentSize (§7.3). The first frame whose size differs from the settled
//   size starts a resize: SOURCE_RESIZED {"width","height"} goes out at once (Observe returns true), and the converters
//   letterbox the new content into the current output size meanwhile (the slots carry each frame's content size,
//   gpu/slots.h). Once the size has been stable for kResizeSettleNs, Settle returns it: the session re-inits the
//   encoders (REINIT|KEYFRAME) when the output size computed for it (§8.3) differs. A drag that ends at the settled
//   size again settles without a re-init (Settle returns nothing).
// - MinimizeTracker: the supervisor polls IsIconic(window) every kMinimizePollNs for an identified window (§7.6) and
//   sends SOURCE_MINIMIZED / SOURCE_RESTORED on each edge; WGC delivers no frames meanwhile, so viewers keep the last
//   picture.
#pragma once

#include <cstdint>
#include <optional>

#include "im/video/scale.h"

namespace im::capture {

inline constexpr int64_t kResizeSettleNs = 1'000'000'000; // §7.7: "until the size has been stable for 1 s"
inline constexpr int64_t kMinimizePollNs = 500'000'000;   // §7.6: IsIconic every 500 ms

// WGC's MinUpdateInterval for layer 0's fps, in 100 ns units (§7.3): half the frame interval, so DWM delivers between
// fps and 2 × fps frames on any refresh rate and the pacer thins them to fps (83 333 = 8.33 ms at 60 fps). 0 for 0.
constexpr int64_t MinUpdateInterval100ns(uint32_t fps) noexcept {
  return fps == 0 ? 0 : 10'000'000 / (2 * static_cast<int64_t>(fps));
}

class ResizeTracker {
public:
  // initial: the size the session's encoders were set up for (the pick's size).
  explicit ResizeTracker(video::Size initial) noexcept : settled_(initial), last_(initial) {}

  // A frame of `content` arrived at now. True when it starts a resize (SOURCE_RESIZED goes out now).
  bool Observe(video::Size content, int64_t now) noexcept {
    if (content.width == 0 || content.height == 0) {
      return false; // no picture (a window on its way to minimized)
    }
    if (content == last_) {
      return false;
    }
    last_ = content;
    changed_at_ = now;
    if (resizing_) {
      return false; // one event per resize
    }
    resizing_ = true;
    return true;
  }

  // The new size once it has been stable for kResizeSettleNs and differs from the settled one (it becomes the
  // settled size); nothing otherwise.
  std::optional<video::Size> Settle(int64_t now) noexcept {
    if (!resizing_ || now - changed_at_ < kResizeSettleNs) {
      return std::nullopt;
    }
    resizing_ = false;
    if (last_ == settled_) {
      return std::nullopt;
    }
    settled_ = last_;
    return settled_;
  }

  // When Settle can next return a size (for the supervisor's deadline), or nothing while no resize runs.
  std::optional<int64_t> SettleDueNs() const noexcept {
    if (!resizing_) {
      return std::nullopt;
    }
    return changed_at_ + kResizeSettleNs;
  }

  bool resizing() const noexcept { return resizing_; }
  video::Size settled() const noexcept { return settled_; }

private:
  video::Size settled_;
  video::Size last_;
  int64_t changed_at_ = 0;
  bool resizing_ = false;
};

class MinimizeTracker {
public:
  enum class Change : uint8_t { kNone, kMinimized, kRestored };

  // The poll's IsIconic result; the edge, if any.
  Change Update(bool iconic) noexcept {
    if (iconic == minimized_) {
      return Change::kNone;
    }
    minimized_ = iconic;
    return iconic ? Change::kMinimized : Change::kRestored;
  }

  bool minimized() const noexcept { return minimized_; }

private:
  bool minimized_ = false;
};

} // namespace im::capture
