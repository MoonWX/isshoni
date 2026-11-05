// What a session sees of a running capture (docs/m2/04-windows-engine.md §7.3, §7.6–§7.8; README WF12), and which
// capture runs a source.
//
// A session whose source a capture area captures (a WGC pick, wgc_capture.h; a Desktop Duplication monitor, a Windows
// 10 monitor pick or a listed "mon:" id, dd_capture.h, README WA60) starts it on the engine worker with
// StartSourceCapture, after its frame slots exist and its encoders opened:
//   - the capture writes each frame into the session's slots (gpu/slots.h) and tells the session through CaptureSink;
//   - the session's supervisor polls minimized() for an identified window and turns the sink's reports into events;
//   - the video thread calls SetFps after a layer-0 fps re-init;
//   - im_stop calls Stop first, before the slots and the device go (04 §6.1: "Stop stops capture first").
// Sources no capture area handles (test:pattern, the test hooks' fake picks) run the session's own test pattern.
//
// Test hooks (IM_TEST_HOOKS builds): SetCaptureFactoryForTest installs a factory that the session asks before the real
// ones, so session_capture_test runs the session's capture rules with a fake capture and no GPU.
#pragma once

#include <cstdint>
#include <memory>
#include <string_view>

#include "capture/window_match.h"
#include "gpu/slots.h"
#include "im/video/scale.h"
#include "isshoni_media.h"
#include "session/sources.h"

namespace im::capture {

// What a running capture tells its session. Called on the capture's threads, one call at a time, never after Stop
// returns: each call must return quickly without blocking on the session (set a flag, wake a thread).
class CaptureSink {
public:
  virtual ~CaptureSink() = default;
  // A frame captured at capture_ts_ns (engine clock) is published in the slots.
  virtual void OnFrame(int64_t capture_ts_ns) noexcept = 0;
  // The source's content size changed to content (seen on the frame that carries it; ResizeTracker debounces).
  virtual void OnResized(video::Size content) noexcept = 0;
  // The item closed: the window was closed or the monitor removed (SOURCE_GONE).
  virtual void OnClosed() noexcept = 0;
  // Capture failed for good (the copy or the pool: IM_ERR_ENCODER_LOST with device_removed on device loss,
  // IM_ERR_SOURCE_GONE when the source went away mid-frame, else IM_ERR_INTERNAL); detail is im_last_error's text.
  // No frame follows.
  virtual void OnFailed(im_status status, std::string_view detail) noexcept = 0;
  // Desktop Duplication lost its duplication (§7.4: a mode change, the secure desktop, the lock screen): no frame
  // comes until OnResumed, and the session keeps repeating the last one (CAPTURE_INTERRUPTED). cause is one of
  // dd_policy.h's kCause* ("access_lost", "secure_desktop", "mode_change"). Called once per interruption.
  // README WA60 adds these with empty bodies: the session's sink (session_object.cpp) turns them into
  // CAPTURE_INTERRUPTED / CAPTURE_RESUMED once a session slice wires them.
  virtual void OnInterrupted(std::string_view cause) noexcept { (void)cause; }
  // The duplication is back (CAPTURE_RESUMED, with the interruption's cause); OnResized follows when the size changed.
  virtual void OnResumed(std::string_view cause) noexcept { (void)cause; }
};

struct CaptureOptions {
  bool hide_cursor = false; // IM_START_HIDE_CURSOR
  bool keep_border = false; // IM_START_KEEP_BORDER: don't ask for borderless capture
  uint32_t fps = 60;        // layer 0's frame rate (MinUpdateInterval)
};

// A running capture.
class CaptureSource {
public:
  virtual ~CaptureSource() = default;

  // The session runs without Windows' capture border (IM_SESS_NO_OS_INDICATOR, §7.1).
  virtual bool borderless() const noexcept = 0;
  // The identified window (§7.6) the supervisor polls; 0 for monitors and unidentified windows.
  virtual WindowHandle window() const noexcept = 0;
  // Whether window() is minimized now (IsIconic); false without a window. The supervisor's thread.
  virtual bool minimized() const noexcept = 0;
  // The content size capture started at (the source may have changed size since the pick).
  virtual video::Size size() const noexcept = 0;
  // Re-sets the delivery interval for a new layer-0 frame rate (an fps re-init). No-op after Stop. Any thread.
  virtual void SetFps(uint32_t fps) noexcept = 0;
  // Stops capture; idempotent. After it returns no CaptureSink call is made and the slots are not touched. Any thread
  // but the capture's own callbacks.
  virtual void Stop() noexcept = 0;
};

// Starts the captures of one kind of source.
class CaptureFactory {
public:
  virtual ~CaptureFactory() = default;
  // Whether this factory captures source.
  virtual bool Handles(const session::SourceSpec &source) const noexcept = 0;
  // Whether its capture writes into frame slots (the session then keeps the GPU path, whatever its encoders take).
  virtual bool NeedsSlots(const session::SourceSpec &source) const noexcept = 0;
  // Starts capturing source (slots: the session's, null when the session has no GPU path). On failure nothing runs
  // and the thread's detail is set.
  virtual im_status Start(const session::SourceSpec &source, gpu::FrameSlots *slots, const CaptureOptions &options,
                          CaptureSink *sink, std::unique_ptr<CaptureSource> *out) noexcept = 0;
};

// The LUID of the adapter that owns a Desktop Duplication source's output (0 for every other source): DD runs only
// on a device of that adapter (§7.4, §8.1), so the session's device should be created there.
uint64_t OutputAdapterLuid(const session::SourceSpec &source) noexcept;

// Whether a capture area captures source (otherwise the session draws its own test pattern).
bool Captures(const session::SourceSpec &source) noexcept;
// Whether that capture needs frame slots (WGC and Desktop Duplication: yes).
bool NeedsSlots(const session::SourceSpec &source) noexcept;
// Starts the capture of source; IM_ERR_INTERNAL when no capture area handles it, or when it needs slots and slots is
// null. Failures set the thread's detail (wgc_capture.h lists WGC's).
im_status StartSourceCapture(const session::SourceSpec &source, gpu::FrameSlots *slots, const CaptureOptions &options,
                             CaptureSink *sink, std::unique_ptr<CaptureSource> *out) noexcept;

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
// Test hooks: factory is asked before the real ones for sessions started from now on (null: none). The caller keeps it
// alive until it resets the hook and the sessions it started have stopped.
void SetCaptureFactoryForTest(CaptureFactory *factory) noexcept;
#endif

} // namespace im::capture
