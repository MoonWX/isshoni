// The frames of a session without a device (session/frames.h; README WF2): capture times only, for encoders that take
// no pixels (the test hooks' IM_ENC_FAKE). They go through the frame slots' triple-buffer index (gpu/triple_buffer.h),
// so the newest frame wins and the skipped count is the GPU path's: the writer fills its own entry and publishes it,
// the reader takes the latest when it is newer and keeps its entry otherwise. One writer thread, one reader thread;
// Publish and Take never block or allocate. Portable C++ without OS headers.
#pragma once

#include <array>
#include <cstdint>

#include "gpu/triple_buffer.h"

namespace im::session {

// What the video thread takes (VideoFrames::Take, FrameTimes::Take).
struct TakenFrame {
  bool fresh = false;        // a newer frame than the last one taken
  uint64_t skipped = 0;      // frames published since the last Take that were never taken
  int64_t capture_ts_ns = 0; // the newest taken frame's capture time (0 before the first one)
};

class FrameTimes {
public:
  // The writer: publishes a frame captured at capture_ts_ns.
  void Publish(int64_t capture_ts_ns) noexcept {
    times_[index_.write_index()] = capture_ts_ns; // the writer's own entry: the swap below publishes it
    index_.Publish();
  }

  // The reader: the newest published frame, when there is a newer one than the last Take's.
  TakenFrame Take() noexcept {
    const gpu::TripleIndex::Taken t = index_.Take();
    TakenFrame out;
    out.fresh = t.fresh;
    out.skipped = t.skipped;
    out.capture_ts_ns = t.seq == 0 ? 0 : times_[t.index]; // the reader's own entry
    return out;
  }

private:
  gpu::TripleIndex index_;
  std::array<int64_t, gpu::TripleIndex::kSlots> times_{};
};

} // namespace im::session
