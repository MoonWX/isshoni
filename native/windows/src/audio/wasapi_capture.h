// Windows' capture streams (docs/m2/05-windows-audio.md §8.1-§8.3; "05" below): the process-loopback activation, the
// initialize attempts in S1's format order, and the client the capture thread drains. Only native/windows/src/audio/
// and its tests include it (win_audio.h's rule).
#pragma once

#include <cstdint>
#include <memory>

#include "audio/activator.h"
#include "audio/capture_client.h"
#include "audio/win_audio.h"

namespace im::audio {

// One ActivateAudioInterfaceAsync call on the process-loopback virtual device (VAD\Process_Loopback) for pid's
// process tree, INCLUDE or EXCLUDE (05 §8.1). The completion handler is agile and holds its own reference, so a
// completion after the caller gave up (a timeout, a stop) touches only the handler and the Completion.
class LoopbackActivation {
public:
  // Starts the activation. E_NOTIMPL when mmdevapi.dll has no ActivateAudioInterfaceAsync (Windows too old).
  static HRESULT Start(uint32_t pid, bool include_tree, std::unique_ptr<LoopbackActivation> *out);
  ~LoopbackActivation();
  LoopbackActivation(const LoopbackActivation &) = delete;
  LoopbackActivation &operator=(const LoopbackActivation &) = delete;

  // Completed by the handler.
  const std::shared_ptr<Completion> &completion() const noexcept { return done_; }
  // Once completion() is done: the activation's HRESULT and, on success, the client.
  HRESULT Result(win::ComPtr<IAudioClient> &out);

private:
  class Handler;
  LoopbackActivation() = default;
  std::shared_ptr<Completion> done_;
  win::ComPtr<IActivateAudioInterfaceCompletionHandler> handler_;
  win::ComPtr<IActivateAudioInterfaceAsyncOperation> op_;
};

// The activator threads' opener (activator.h): activates and initializes a process-loopback stream (kInclude,
// kExclude) in shared, evented loopback mode with a 100 ms buffer, trying float32 48 kHz stereo, PCM16, float32 with
// the engine's conversion, then PCM16 with it, each on a freshly activated client (05 §8.2; GetMixFormat isn't
// supported by process loopback), then sets its event and gets its IAudioCaptureClient. The endpoint stream is README
// WA53's (E_NOTIMPL here).
std::shared_ptr<StreamOpener> NewWasapiOpener();

// The activator threads' hook: COM's multithreaded apartment and the thread's name (ActivatorOptions::thread_hook).
void ActivatorThreadHook(void *ctx, bool starting);

} // namespace im::audio
