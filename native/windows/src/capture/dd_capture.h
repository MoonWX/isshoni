// DXGI Desktop Duplication for one session (docs/m2/04-windows-engine.md §7.1, §7.4, §7.7–§7.9; README WA60): a
// monitor's output duplicated into the session's frame slots (gpu/slots.h), with the pointer drawn in.
//
// Start (on the engine worker, im_start), on the session's own D3D11 device (§8.1), on a per-monitor DPI-aware thread:
//   the output is looked up by its DeviceName among the outputs of the device's adapter (DD fails across adapters,
//   §7.4), then IDXGIOutput5::DuplicateOutput1(device, 0, 1, {DXGI_FORMAT_B8G8R8A8_UNORM}) (IDXGIOutput1::
//   DuplicateOutput where Output5 is missing): asking for 8-bit makes DXGI hand an HDR desktop over as SDR (§7.9).
//   A secure desktop or a mode change at that moment starts the capture interrupted (the thread re-creates it).
//
// The DD thread ("isshoni dd", MMCSS Capture, per-monitor DPI aware) loops on AcquireNextFrame(≤ kDdMaxWaitMs):
//   - LastPresentTime != 0: the desktop texture is copied into the slot FrameSlots::BeginWrite returns (in the panel's
//     orientation; FrameMeta::rotation = DXGI_OUTDUPL_DESC.Rotation turns it upright, §7.9) and, unless the pointer is
//     hidden, into a copy of the last desktop image of its own; capture_ts = LastPresentTime (QPC ticks) in ns by
//     §12.3's formula; ReleaseFrame right after; CaptureSink::OnFrame.
//   - The pointer (§7.8, unless IM_START_HIDE_CURSOR): its position and visibility from LastMouseUpdateTime, its shape
//     from GetFramePointerShape when PointerShapeBufferSize > 0, converted on the CPU (gpu/cursor_shape.h) into a
//     gpu::CursorOverlay. The overlay blends it into the slot copy, never into the desktop texture, before the slot is
//     published; the published FrameMeta::cursor is then invisible (already drawn). A pointer that moves over an
//     unchanged desktop is drawn into a fresh copy of the last desktop image and published at most every half frame
//     interval (dd_policy.h), with LastMouseUpdateTime as its capture time.
//     As built, the blend runs on the DD thread rather than the video thread that §7.8 names: the session's video
//     thread doesn't call CursorOverlay::Blend (session/ is outside README WA60's paths), and drawing into the slot
//     before it is published keeps the rule that matters (into the copy, never the desktop texture) without it.
//   - DXGI_ERROR_ACCESS_LOST (and E_ACCESSDENIED, DXGI_ERROR_SESSION_DISCONNECTED): CaptureSink::OnInterrupted with
//     the cause (secure_desktop, mode_change or access_lost); the duplication is re-created every kDdRetryNs, and on
//     success CaptureSink::OnResumed, then OnResized when the upright size changed. An output missing from the
//     adapter's EnumOutputs for kDdGoneNs: CaptureSink::OnClosed (SOURCE_GONE), and the thread ends.
//   - DXGI_ERROR_WAIT_TIMEOUT is a static screen. A removed device is CaptureSink::OnFailed with IM_ERR_ENCODER_LOST
//     (device_removed); any other failure OnFailed with IM_ERR_INTERNAL. No frame follows a failure.
//
// Stop (any thread but the DD thread; also from the destructor) ends the thread within kDdMaxWaitMs and joins it;
// once it returns no CaptureSink call is made and the slots are not touched. DD never shows a border: borderless() is
// true and the session sets IM_SESS_NO_OS_INDICATOR (§7.1).
#pragma once

#include <cstdint>
#include <memory>

#include "capture/capture_source.h"
#include "capture/outputs.h"
#include "capture/window_match.h"
#include "gpu/slots.h"
#include "im/video/scale.h"
#include "isshoni_media.h"
#include "session/sources.h"

namespace im::capture {

class DdCapture final : public CaptureSource {
public:
  // Whether source is a monitor this area captures with Desktop Duplication (capture_path IM_CAPTURE_DD with an
  // output: a Windows 10 monitor pick that mapped, §7.5, or a listed "mon:" id).
  static bool Handles(const session::SourceSpec &source) noexcept;

  // Starts duplicating output into slots (whose device is the session's). On failure nothing runs and the thread's
  // detail is set: IM_ERR_INTERNAL with token dpi_unaware (the process isn't per-monitor DPI aware: a packaging bug,
  // §7.4), IM_ERR_SOURCE_GONE (the output is no longer attached), IM_ERR_LIMIT (DXGI_ERROR_NOT_CURRENTLY_AVAILABLE:
  // too many duplications of the output), IM_ERR_ENCODER_LOST (device_removed), IM_ERR_INTERNAL (else).
  // *cross_adapter (optional) is set when the output belongs to another adapter than the device's, or DXGI refused
  // the duplication as unsupported: the caller may capture the monitor with WGC instead.
  static im_status Start(const DisplayOutput &output, gpu::FrameSlots &slots, const CaptureOptions &options,
                         CaptureSink *sink, std::unique_ptr<DdCapture> *out, bool *cross_adapter) noexcept;

  ~DdCapture() override;
  DdCapture(const DdCapture &) = delete;
  DdCapture &operator=(const DdCapture &) = delete;

  bool borderless() const noexcept override { return true; } // DD never shows a border (§7.1)
  WindowHandle window() const noexcept override { return 0; }
  bool minimized() const noexcept override { return false; }
  // The output's upright desktop size at Start.
  video::Size size() const noexcept override { return size_; }
  // The pointer-only frames' spacing follows layer 0's frame rate. Any thread.
  void SetFps(uint32_t fps) noexcept override;
  // Ends the DD thread and joins it; idempotent.
  void Stop() noexcept override;

private:
  struct Impl;
  explicit DdCapture(std::unique_ptr<Impl> impl) noexcept;

  std::unique_ptr<Impl> impl_;
  video::Size size_;
};

} // namespace im::capture
