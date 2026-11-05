// Desktop Duplication's timing rules (docs/m2/04-windows-engine.md §7.4, §7.7, §7.8; README WA60), as plain C++ so
// dd_outputs_test checks them without Windows headers; dd_capture.cpp's thread follows them.
//
// - DXGI_ERROR_ACCESS_LOST (a mode change, the secure desktop of UAC or Ctrl+Alt+Del, the lock screen, a full-screen
//   exclusive app switching modes) interrupts the capture: CAPTURE_INTERRUPTED, the session keeps repeating the last
//   frame, and the duplication is re-created every kDdRetryNs. A re-creation that works resumes it
//   (CAPTURE_RESUMED, then the resize path when the size changed). An output missing from EnumOutputs for kDdGoneNs
//   in a row ends it (SOURCE_GONE).
// - AcquireNextFrame waits at most kDdMaxWaitMs, so Stop is seen within that.
// - A pointer that moved or changed shape over an unchanged desktop is drawn into a copy of the last desktop image
//   and published as a frame of its own, at most once per half a frame interval (the pacer thins frames to fps
//   anyway; a mouse reports up to 1000 moves a second).
#pragma once

#include <algorithm>
#include <cstdint>
#include <limits>
#include <string_view>

namespace im::capture {

inline constexpr int64_t kDdRetryNs = 250'000'000;  // §7.4: re-create the duplication every 250 ms
inline constexpr int64_t kDdGoneNs = 5'000'000'000; // §7.4: the output gone from EnumOutputs for 5 s
inline constexpr uint32_t kDdMaxWaitMs = 100;       // §7.4: AcquireNextFrame's timeout, at most 100 ms
inline constexpr int64_t kDdNever = std::numeric_limits<int64_t>::max();

// CAPTURE_INTERRUPTED / CAPTURE_RESUMED's cause (04 §5.6).
inline constexpr std::string_view kCauseAccessLost = "access_lost";
inline constexpr std::string_view kCauseSecureDesktop = "secure_desktop";
inline constexpr std::string_view kCauseModeChange = "mode_change";

// The cause of an interruption: the secure desktop (the input desktop isn't the user's: UAC, Ctrl+Alt+Del, the lock
// screen) first, then a mode change (the output's size or rotation differs from the duplication's), else access_lost.
constexpr std::string_view InterruptCause(bool secure_desktop, bool mode_changed) noexcept {
  if (secure_desktop) {
    return kCauseSecureDesktop;
  }
  return mode_changed ? kCauseModeChange : kCauseAccessLost;
}

// The interrupted state of one duplication.
class DuplicationRecovery {
public:
  enum class Outcome : uint8_t {
    kWaiting, // still interrupted: try again at NextRetryNs()
    kResumed, // the duplication is back
    kGone,    // the output was missing for kDdGoneNs: the source is gone
  };

  // The duplication was lost at now (or couldn't be created at the start). True when this begins an interruption
  // (CAPTURE_INTERRUPTED goes out once per interruption).
  bool Lost(int64_t now) noexcept {
    if (interrupted_) {
      return false;
    }
    interrupted_ = true;
    next_retry_ = now + kDdRetryNs;
    missing_since_ = kDdNever;
    return true;
  }

  bool interrupted() const noexcept { return interrupted_; }

  // When the next re-creation is due (kDdNever while not interrupted).
  int64_t NextRetryNs() const noexcept { return interrupted_ ? next_retry_ : kDdNever; }

  // One re-creation attempt at now: output_present says whether EnumOutputs still lists the output, recreated
  // whether the duplication could be created again.
  Outcome Attempt(int64_t now, bool output_present, bool recreated) noexcept {
    if (!interrupted_) {
      return Outcome::kResumed;
    }
    if (output_present) {
      missing_since_ = kDdNever;
      if (recreated) {
        interrupted_ = false;
        next_retry_ = kDdNever;
        return Outcome::kResumed;
      }
    } else {
      if (missing_since_ == kDdNever) {
        missing_since_ = now;
      }
      if (now - missing_since_ >= kDdGoneNs) {
        return Outcome::kGone;
      }
    }
    next_retry_ = now + kDdRetryNs;
    return Outcome::kWaiting;
  }

private:
  bool interrupted_ = false;
  int64_t next_retry_ = kDdNever;
  int64_t missing_since_ = kDdNever; // the first attempt that didn't find the output
};

// The pointer-only frames' spacing: half a frame interval at fps (0: a frame interval of 60 fps).
constexpr int64_t PointerSpacingNs(uint32_t fps) noexcept {
  return 1'000'000'000 / (2 * static_cast<int64_t>(fps == 0 ? 60 : fps));
}

// When a pointer change seen at or before now may be published: never before last_publish + PointerSpacingNs(fps).
constexpr int64_t PointerDueNs(int64_t last_publish_ns, uint32_t fps) noexcept {
  return last_publish_ns == std::numeric_limits<int64_t>::min() ? std::numeric_limits<int64_t>::min()
                                                                : last_publish_ns + PointerSpacingNs(fps);
}

// AcquireNextFrame's timeout at now: until due_ns (a pending pointer frame, or kDdNever), at most kDdMaxWaitMs.
constexpr uint32_t AcquireTimeoutMs(int64_t now, int64_t due_ns) noexcept {
  if (due_ns <= now) {
    return 0;
  }
  constexpr int64_t kMaxWaitNs = int64_t{kDdMaxWaitMs} * 1'000'000;
  if (due_ns - now >= kMaxWaitNs) { // also keeps kDdNever's subtraction from overflowing below
    return kDdMaxWaitMs;
  }
  return static_cast<uint32_t>((due_ns - now + 999'999) / 1'000'000); // rounded up: never wake before the deadline
}

// A capture time that keeps a stream's times increasing: ts, or one past the last one.
constexpr int64_t MonotonicTs(int64_t last_ns, int64_t ts_ns) noexcept { return ts_ns > last_ns ? ts_ns : last_ns + 1; }

} // namespace im::capture
