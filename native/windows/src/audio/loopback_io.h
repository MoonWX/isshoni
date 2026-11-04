// The OS side of the startup probe and the sound check (docs/m2/05-windows-audio.md §11.2, §11.3; "05" below): one
// loopback capture (an INCLUDE stream on a process tree, or the default render endpoint's loopback) read into a
// buffer, one render client on the default render endpoint with a private session, and the session list. Their logic
// (startup_probe.h, sound_check.h) is portable and runs against this interface; loopback_io_win.cpp is Windows' and
// tests use fakes. One thread drives an instance (a DLL-owned MTA thread on Windows).
#pragma once

#include <atomic>
#include <cstdint>
#include <memory>
#include <string>
#include <vector>

#include "audio/signal.h"

namespace im::audio {

// How opening a capture or the render client went.
enum class OpenOutcome : uint8_t {
  kOk,
  kUnsupported, // process loopback: E_NOTIMPL from the activation, or no format initializes (05 §11.3 step 3)
  kFailed,      // anything else (the activation's own failure, a timeout)
  kNoDevice,    // no active render endpoint
  kBusy,        // another app holds the default render device in exclusive mode (AUDCLNT_E_DEVICE_IN_USE)
};

class LoopbackIo {
public:
  virtual ~LoopbackIo() = default;
  // An INCLUDE stream on pid's process tree (05 §8.1, §8.2's formats; activation <= 500 ms for the probe, 3 s for the
  // sound check: the implementation's), started. format: "float32" or "pcm16".
  virtual OpenOutcome OpenProcessCapture(uint32_t pid, std::string *format) noexcept = 0;
  // The default render endpoint's loopback (05 §8.2's endpoint formats), started.
  virtual OpenOutcome OpenEndpointCapture(std::string *format) noexcept = 0;
  // Appends what the open capture has captured since the last call (interleaved stereo float, 48 kHz). 0 or the
  // failing HRESULT.
  virtual int32_t ReadCapture(std::vector<float> &stereo) noexcept = 0;
  virtual void CloseCapture() noexcept = 0;
  // A shared-mode render client on the default render endpoint (float32 48 kHz stereo, AUTOCONVERTPCM, a fixed private
  // session GUID, its ISimpleAudioVolume at 1.0 unmuted: 05 §11.2 step 2), not started; it stays the io's. endpoint_id:
  // the endpoint's IMMDevice id (UTF-8).
  virtual OpenOutcome OpenRender(RenderTarget **out, std::string *endpoint_id) noexcept = 0;
  virtual void CloseRender() noexcept = 0;
  // Whether pid has an audio session on an active render endpoint now (05 §11.3 step 6: "timeout").
  virtual bool SessionSeen(uint32_t pid) noexcept = 0;
  virtual uint32_t SelfPid() const noexcept = 0;
  virtual int64_t NowNs() noexcept = 0; // the engine clock
  virtual void Wait(uint32_t ms) noexcept = 0;
};

// Windows' io (loopback_io_win.cpp). activation_timeout_ms: how long the activations of one capture open may take in
// all (one per format tried); abort (may be null; it must outlive the io) cuts a wait short (the open fails). Call on a
// thread in COM's multithreaded apartment.
std::unique_ptr<LoopbackIo> NewWindowsLoopbackIo(uint32_t activation_timeout_ms, const std::atomic<bool> *abort);

} // namespace im::audio
