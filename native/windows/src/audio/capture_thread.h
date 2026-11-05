// The capture thread (docs/m2/05-windows-audio.md §8.4; "05" below): one thread, MMCSS "Pro Audio", COM MTA, that waits
// on up to 63 stream events plus a control event (20 ms timeout) and on each wake drains every stream with a packet
// ready into its ring (capture_client.h). 48 include streams + 1 EXCLUDE or endpoint stream + the control event are
// 50 handles, so one thread suffices. A stream's COM objects are started and released on this thread (S1).
#pragma once

#include <atomic>
#include <cstdint>
#include <memory>
#include <thread>

#include "audio/capture_client.h"
#include "isshoni_media.h"

namespace im::audio {

inline constexpr uint32_t kMaxCaptureStreams = 63; // MAXIMUM_WAIT_OBJECTS minus the control event

class CaptureThread final : public CaptureHost {
public:
  CaptureThread();
  ~CaptureThread() override; // Stop
  CaptureThread(const CaptureThread &) = delete;
  CaptureThread &operator=(const CaptureThread &) = delete;

  // Starts the thread (idempotent). IM_ERR_INTERNAL when it can't start.
  im_status Start();
  // Releases every stream's client on the thread and joins it within 2 s; a thread stuck in a driver call is
  // abandoned with a log record, never killed (05 §10.5). Start may follow.
  void Stop() noexcept;
  bool running() const noexcept { return thread_.joinable(); }

  bool Add(std::shared_ptr<CaptureStream> s) override;
  void Remove(const std::shared_ptr<CaptureStream> &s) override;

  // Time spent draining since the thread started (the capture's share of audioCpuMsPerS, 05 §12.1).
  int64_t busy_ns() const noexcept;

  struct Shared;

private:
  std::shared_ptr<Shared> sh_;
  std::thread thread_;
};

} // namespace im::audio
