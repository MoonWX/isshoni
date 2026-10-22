// Source resizes (docs/m2/04-windows-engine.md §7.7): a window resized or a monitor's mode changed. Drag-resizing
// would otherwise re-init dozens of times a second, so:
// - SOURCE_RESIZED goes out when a change is first seen (OnSourceSize returns true once per episode);
// - until the new size has been stable for 1 s, every layer keeps its output size and fits the new content into it
//   (aspect kept, black bars: Letterbox);
// - then each layer's output size is computed for the new source (§8.3); a layer whose size changed re-inits its
//   encoder (REINIT|KEYFRAME), one whose size is unchanged doesn't (its content rectangle is just updated).
// Pure logic on a caller-supplied clock; the supervisor owns it.
#pragma once

#include <cstdint>
#include <limits>
#include <optional>

#include "im/video/scale.h"

namespace im::video {

inline constexpr int64_t kResizeDebounceNs = 1'000'000'000; // 04 §6.4

class ResizeDebouncer {
public:
  static constexpr int64_t kNever = std::numeric_limits<int64_t>::max();

  // source: the size the encoders were opened for.
  explicit ResizeDebouncer(Size source) noexcept : encoded_(source), current_(source) {}

  // A captured frame's content size. Returns true when a change is first seen (send SOURCE_RESIZED with this size).
  bool OnSourceSize(Size size, int64_t now_ns) noexcept;

  // The new source size once it has been stable for 1 s (it then becomes the encoders' size), else nothing. May
  // equal the old size when the source came back to it.
  std::optional<Size> Poll(int64_t now_ns) noexcept;

  // When Poll has something to return, or kNever.
  int64_t NextDeadlineNs() const noexcept { return pending_ ? stable_since_ + kResizeDebounceNs : kNever; }

  bool pending() const noexcept { return pending_; }
  Size encoded_source() const noexcept { return encoded_; } // what the encoders' output sizes were computed for
  Size current_source() const noexcept { return current_; } // the latest content size

private:
  Size encoded_;
  Size current_;
  bool pending_ = false;
  int64_t stable_since_ = 0;
};

// While a resize is pending: where the current content goes inside a layer's unchanged output.
inline Rect Letterbox(Size source, Size output) noexcept { return FitRect(source, output); }

struct LayerResize {
  bool reinit = false; // the output size changed: re-open the encoder (REINIT|KEYFRAME)
  OutputSize output;   // the layer's output size and content rectangle for the new source
};

// What a stable resize means for one layer whose encoder runs at current_output.
LayerResize PlanLayerResize(Size new_source, const LayerBounds &bounds, Size current_output) noexcept;

} // namespace im::video
