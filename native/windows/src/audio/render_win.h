// The DLL's own render client (docs/m2/05-windows-audio.md §11.2, §11.3, §13.2; "05" below): a shared-mode WASAPI
// render client, float32 48 kHz stereo with the engine's conversion, a fixed private session GUID and its
// ISimpleAudioVolume at 1.0 unmuted (so a user's Volume Mixer setting for isshoni can't fail the probe or the test
// signals), with its IAudioClock for the device position. The startup probe's tone, the sound check's own chime and the
// test exports play through it (signal.h). Also the DLL's way to run COM work off a caller's thread. Only
// native/windows/src/audio/ includes it (win_audio.h's rule).
#pragma once

#include <cstdint>
#include <functional>
#include <memory>
#include <string>

#include "audio/signal.h"
#include "audio/win_audio.h"

namespace im::audio {

class WasapiRender final : public RenderTarget {
public:
  // Opens and initializes the client on endpoint_id (an IMMDevice id; empty: the default render endpoint, eConsole) on
  // the calling thread (COM MTA). Not started. HRESULT_FROM_WIN32(ERROR_NOT_FOUND) or E_NOTFOUND: no such (or no)
  // endpoint; AUDCLNT_E_DEVICE_IN_USE: exclusive mode elsewhere.
  static HRESULT Open(const std::wstring &endpoint_id, std::unique_ptr<WasapiRender> *out);
  ~WasapiRender() override;
  WasapiRender(const WasapiRender &) = delete;
  WasapiRender &operator=(const WasapiRender &) = delete;

  int32_t Writable(uint32_t *frames) noexcept override;
  int32_t Write(const float *frames, uint32_t n) noexcept override;
  int32_t Start() noexcept override;
  bool Position(uint64_t *frames, int64_t *qpc_ns) noexcept override;
  void Wait(uint32_t ms) noexcept override;

  const std::string &endpoint_id() const noexcept { return id_; } // UTF-8

private:
  WasapiRender() = default;
  win::ComPtr<IAudioClient> client_;
  win::ComPtr<IAudioRenderClient> render_;
  win::ComPtr<IAudioClock> clock_;
  UINT32 buffer_frames_ = 0;
  UINT64 clock_freq_ = 0;
  bool started_ = false;
  std::string id_;
};

// Runs fn on a new DLL thread in COM's multithreaded apartment and waits for it: the DLL never calls CoInitializeEx on
// a caller's thread (05 §4.3). fn returns a status and, on failure, a detail; RunOnMta sets the calling thread's detail
// from it. IM_ERR_INTERNAL when the thread can't start.
im_status RunOnMta(const char *name, const std::function<im_status(std::string *detail)> &fn);

} // namespace im::audio
