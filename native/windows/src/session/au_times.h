// The capture time of each access unit of a layer (docs/m2/04-windows-engine.md §9.2: "a small table maps packet pts
// back to the capture time, so encoders with one frame of delay keep correct timestamps"). The encoder finds the frame
// an AU came from (FfmpegEncoder's pts → capture-time ring; EncodedAu::capture_ts_ns); this keeps the session's side:
// the last frames the encoder took, so the packet carries that frame's IM_PKT_REFRESH too, and the rule that a
// layer's packets are at least one RTP tick apart (kMinSpacingNs) in capture order, whatever the encoder reports.
// One per layer, used on the video thread only. Portable C++ without OS headers.
#pragma once

#include <array>
#include <cstddef>
#include <cstdint>

namespace im::session {

class AuTimes {
public:
  // Two AUs of a layer are at least one 90 kHz tick apart (session_object.h's kMinAuSpacingNs).
  static constexpr int64_t kMinSpacingNs = 11'112;
  // How many frames back an AU may come from (FfmpegEncoder keeps as many pts).
  static constexpr std::size_t kFrames = 8;

  struct Stamp {
    int64_t capture_ts_ns = 0;
    bool refresh = false;
  };

  // The encoder took a frame captured at ts_ns (a re-encode of an unchanged frame when refresh).
  void Sent(int64_t ts_ns, bool refresh) noexcept {
    sent_[next_] = Frame{ts_ns, refresh};
    next_ = (next_ + 1) % kFrames;
  }

  // The stamp of an AU that the encoder returned right after it took the frame sent_ts_ns (sent_refresh): its own
  // frame's capture time (au_ts_ns, EncodedAu::capture_ts_ns; 0 means the frame just sent) and that frame's refresh
  // flag, moved past the last pushed AU's time by at least kMinSpacingNs if an encoder ever reports an older one.
  Stamp ForAu(int64_t au_ts_ns, int64_t sent_ts_ns, bool sent_refresh) const noexcept {
    Stamp s{sent_ts_ns, sent_refresh};
    if (au_ts_ns != 0 && au_ts_ns != sent_ts_ns) {
      s.capture_ts_ns = au_ts_ns;
      s.refresh = false; // a frame the table no longer holds: a new frame is what an AU usually is
      for (const Frame &f : sent_) {
        if (f.ts_ns == au_ts_ns) {
          s.refresh = f.refresh;
          break;
        }
      }
    }
    if (last_pushed_ns_ != 0 && s.capture_ts_ns < last_pushed_ns_ + kMinSpacingNs) {
      s.capture_ts_ns = last_pushed_ns_ + kMinSpacingNs;
    }
    return s;
  }

  // An AU stamped capture_ts_ns went into the layer's ring.
  void Pushed(int64_t capture_ts_ns) noexcept { last_pushed_ns_ = capture_ts_ns; }

private:
  struct Frame {
    int64_t ts_ns = 0;
    bool refresh = false;
  };
  std::array<Frame, kFrames> sent_{};
  std::size_t next_ = 0;
  int64_t last_pushed_ns_ = 0; // 0: none yet
};

} // namespace im::session
