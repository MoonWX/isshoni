// One opened capture stream as the capture thread drives it (docs/m2/05-windows-audio.md §8.2-§8.4; "05" below), and
// the stream object the watcher and the capture thread share. Portable C++ (no OS header): the WASAPI client is
// wasapi_capture.cpp's; tests use fakes.
#pragma once

#include <atomic>
#include <cstdint>
#include <memory>

#include "im/audiomix/capture_ring.h"

namespace im::audio {

// An initialized loopback client (IAudioClient and IAudioCaptureClient on Windows). Created on an activator thread;
// started, drained and destroyed on the capture thread (05 §8.4: a stream's COM objects are released there).
class CaptureClient {
public:
  virtual ~CaptureClient() = default;
  // The event the OS signals when a packet is ready (a Windows HANDLE), or nullptr for a polled stream (the endpoint
  // engine's, every 5 ms; README WA53).
  virtual void *event() const noexcept = 0;
  // IAudioClient::Start. 0 or the failing HRESULT.
  virtual int32_t Start() noexcept = 0;
  // Moves every packet that is ready into ring (05 §8.3): PCM16 converted to float, AUDCLNT_BUFFERFLAGS_SILENT as
  // zeros, each packet with its QPC time (or the read time minus the frames already buffered when the position is
  // missing or flagged as a timestamp error). Returns 0 or the failing HRESULT (05 §8.5: the stream failed).
  virtual int32_t Drain(audiomix::CaptureRing &ring) noexcept = 0;
  // AUDCLNT_BUFFERFLAGS_DATA_DISCONTINUITY packets so far.
  virtual uint64_t discontinuities() const noexcept = 0;
};

// A capture stream: its client and ring, and what the capture thread tells the watcher. The watcher creates it once
// the commit check passed and hands it to the capture thread (CaptureHost::Add); the mixer's channel reads the ring.
struct CaptureStream {
  explicit CaptureStream(std::unique_ptr<CaptureClient> c) : client(std::move(c)) {}

  std::unique_ptr<CaptureClient> client; // the capture thread's from Add on; it destroys it after Remove
  audiomix::CaptureRing ring;            // 500 ms (05 §8.3)
  std::atomic<bool> failed{false};       // Start or Drain failed (05 §8.5); the capture thread stops draining it
  std::atomic<int32_t> failure{0};       // the HRESULT
  std::atomic<uint64_t> discontinuities{0};
};

// The capture thread as the watcher sees it (capture_thread.h; fakes in tests). Both calls return at once.
class CaptureHost {
public:
  virtual ~CaptureHost() = default;
  // The thread starts the stream's client and drains it from now on. False when the stream can't be taken (the
  // thread doesn't run, or 63 streams already wait on it): the stream is not drained and the caller drops it.
  virtual bool Add(std::shared_ptr<CaptureStream> s) = 0;
  // The thread stops draining it and destroys its client.
  virtual void Remove(const std::shared_ptr<CaptureStream> &s) = 0;
};

} // namespace im::audio
