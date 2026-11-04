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
// - SourceWatcher: both trackers as the supervisor runs them (README WF12): the newest content size the capture
//   reported since the last pass, the settle deadline and the minimize poll's schedule; what the pass sends.
// - ReinitForSize: whether a settled size changes a layer's output size (§7.7: only those layers re-init).
#pragma once

#include <algorithm>
#include <cstdint>
#include <limits>
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

// A size in one 64-bit word (width high, height low), for the atomics that carry a size between threads.
constexpr uint64_t PackSize(video::Size s) noexcept { return (static_cast<uint64_t>(s.width) << 32) | s.height; }
constexpr video::Size UnpackSize(uint64_t v) noexcept {
  return video::Size{static_cast<uint32_t>(v >> 32), static_cast<uint32_t>(v & 0xFFFFFFFFu)};
}

// Whether a source of `settled` changes the output size of a layer with `bounds` whose encoder runs at `current`
// (§7.7, §8.3): only then does the layer re-init (REINIT|KEYFRAME); otherwise the converter keeps fitting the content.
inline bool ReinitForSize(video::Size settled, const video::LayerBounds &bounds, video::Size current) noexcept {
  const video::OutputSize out = video::ComputeOutputSize(settled, bounds);
  return out.size.width != 0 && out.size.height != 0 && !(out.size == current);
}

// The session supervisor's view of a running capture (§7.6, §7.7): one Poll per supervisor pass.
class SourceWatcher {
public:
  static constexpr int64_t kNever = std::numeric_limits<int64_t>::max();

  struct Step {
    std::optional<video::Size> resized; // SOURCE_RESIZED {"width","height"} goes out now
    std::optional<video::Size> settled; // the size the layers re-init for (ReinitForSize), stable for 1 s
    MinimizeTracker::Change minimize = MinimizeTracker::Change::kNone; // SOURCE_MINIMIZED / SOURCE_RESTORED
  };

  // initial: the size the encoders were set up for (the pick's); watch_minimize: the capture has an identified window
  // to poll (§7.6); now: the first poll is due at once.
  SourceWatcher(video::Size initial, bool watch_minimize, int64_t now) noexcept
      : resize_(initial), watch_minimize_(watch_minimize), next_poll_(now) {}

  // One pass at now. reported: the newest content size the capture reported since the last pass (none: no change);
  // iconic() is called only when a minimize poll is due (IsIconic on the window).
  template <class Iconic> Step Poll(std::optional<video::Size> reported, int64_t now, Iconic &&iconic) {
    Step s;
    if (reported && resize_.Observe(*reported, now)) {
      s.resized = *reported;
    }
    s.settled = resize_.Settle(now);
    if (watch_minimize_ && now >= next_poll_) {
      s.minimize = minimize_.Update(static_cast<bool>(iconic()));
      next_poll_ = now + kMinimizePollNs;
    }
    return s;
  }

  // When Poll next has work of its own (a settle or a minimize poll); kNever when none.
  int64_t NextDueNs() const noexcept {
    int64_t at = watch_minimize_ ? next_poll_ : kNever;
    if (const std::optional<int64_t> due = resize_.SettleDueNs()) {
      at = std::min(at, *due);
    }
    return at;
  }

  bool minimized() const noexcept { return minimize_.minimized(); }
  video::Size settled() const noexcept { return resize_.settled(); }

private:
  ResizeTracker resize_;
  MinimizeTracker minimize_;
  bool watch_minimize_ = false;
  int64_t next_poll_ = 0;
};

} // namespace im::capture
