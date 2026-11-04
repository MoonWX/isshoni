// Windows.Graphics.Capture for one session (docs/m2/04-windows-engine.md §7.1, §7.3, §7.6–§7.8), writing into the
// session's frame slots (gpu/slots.h).
//
// Start (on the engine worker, im_start, COM MTA), on the session's own D3D11 device (§8.1):
//   CreateDirect3D11DeviceFromDXGIDevice → Direct3D11CaptureFramePool::CreateFreeThreaded(device,
//   B8G8R8A8UIntNormalized, 2, item.Size()) → pool.CreateCaptureSession(item), then on the session, each where
//   ApiInformation says the property exists:
//   - IsCursorCaptureEnabled(!hide_cursor) (§7.8: the pointer is drawn unless IM_START_HIDE_CURSOR);
//   - IsBorderRequired(false) when BorderlessAllowed() and not keep_border (IM_START_KEEP_BORDER), read back:
//     borderless() is true only when the session then reports IsBorderRequired() == false (§7.1), and the session
//     sets IM_SESS_NO_OS_INDICATOR for it;
//   - MinUpdateInterval(10'000'000 / (2 × fps) × 100 ns), half layer 0's frame interval (24H2+, §7.3), re-set by
//     SetFps on an fps re-init;
//   - IncludeSecondaryWindows(true) (24H2+: the shared app's menus and popups show);
//   and StartCapture().
//
// FrameArrived (free-threaded, WGC's thread pool; serialized here): TryGetNextFrame; a ContentSize that differs from
// the pool's size recreates the pool at once (Recreate) and reports CaptureSink::OnResized; the content rectangle is
// copied into the slot FrameSlots::BeginWrite returns (grown when the content is larger) under the context lock and
// published with capture_ts = SystemRelativeTime() × 100 (QPC-based, the engine clock); the frame is closed and
// CaptureSink::OnFrame signals the session. item.Closed reports CaptureSink::OnClosed (window closed, monitor
// removed: the session ends with SOURCE_GONE, §7.7). Minimized windows deliver no frames: the session's supervisor
// polls window() with WindowMinimized every kMinimizePollNs (source_watch.h).
//
// Stop (any thread but WGC's callbacks; also from the destructor) revokes the handlers and closes the session and the
// pool; once it returns no CaptureSink call is made and the slots are not touched. Protected content captures as black
// (plan); nothing to do.
#pragma once

#include <cstdint>
#include <memory>
#include <string_view>

#include "capture/source_watch.h"
#include "capture/window_match.h"
#include "gpu/slots.h"
#include "im/video/scale.h"
#include "isshoni_media.h"
#include "session/sources.h"

namespace im::capture {

// What a running capture tells its session. Called on WGC's threads, one call at a time, never after Stop returns:
// each call must return quickly without blocking on the session (set a flag, wake a thread).
class CaptureSink {
public:
  virtual ~CaptureSink() = default;
  // A frame captured at capture_ts_ns (engine clock) is published in the slots.
  virtual void OnFrame(int64_t capture_ts_ns) noexcept = 0;
  // The source's content size changed to content (seen on the frame that carries it; ResizeTracker debounces).
  virtual void OnResized(video::Size content) noexcept = 0;
  // The item closed: the window was closed or the monitor removed (SOURCE_GONE).
  virtual void OnClosed() noexcept = 0;
  // Capture failed for good (the copy or the pool: IM_ERR_ENCODER_LOST with device_removed on device loss, else
  // IM_ERR_INTERNAL); detail is im_last_error's text. No frame follows.
  virtual void OnFailed(im_status status, std::string_view detail) noexcept = 0;
};

struct CaptureOptions {
  bool hide_cursor = false; // IM_START_HIDE_CURSOR
  bool keep_border = false; // IM_START_KEEP_BORDER: don't ask for borderless capture
  uint32_t fps = 60;        // layer 0's frame rate (MinUpdateInterval)
};

class WgcCapture {
public:
  // Whether source is a pick this area captures with WGC (capture_path IM_CAPTURE_WGC with a picked item).
  static bool Handles(const session::SourceSpec &source) noexcept;

  // Starts capturing source into slots (whose device is the session's). On failure nothing runs and the thread's
  // detail is set: IM_ERR_SOURCE_GONE (the item closed since the pick), IM_ERR_PERMISSION_DENIED (capture_denied),
  // IM_ERR_ENCODER_LOST (device_removed), IM_ERR_UNSUPPORTED_OS (os_too_old), IM_ERR_INTERNAL.
  static im_status Start(const session::SourceSpec &source, gpu::FrameSlots &slots, const CaptureOptions &options,
                         CaptureSink *sink, std::unique_ptr<WgcCapture> *out) noexcept;

  ~WgcCapture();
  WgcCapture(const WgcCapture &) = delete;
  WgcCapture &operator=(const WgcCapture &) = delete;

  // The session runs without Windows' capture border (IM_SESS_NO_OS_INDICATOR).
  bool borderless() const noexcept { return borderless_; }
  // The identified window (§7.6) for minimize polling; 0 for monitors and unidentified windows.
  WindowHandle window() const noexcept { return window_; }

  // Re-sets MinUpdateInterval for a new layer-0 frame rate (an fps re-init). No-op where the property is absent or
  // after Stop. Any thread.
  void SetFps(uint32_t fps) noexcept;

  // Stops capture; idempotent. After it returns no CaptureSink call is made.
  void Stop() noexcept;

private:
  struct Impl;
  explicit WgcCapture(std::shared_ptr<Impl> impl) noexcept;

  std::shared_ptr<Impl> impl_;
  bool borderless_ = false;
  WindowHandle window_ = 0;
};

} // namespace im::capture
