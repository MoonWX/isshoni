// Windows' capture streams (docs/m2/05-windows-audio.md §8.1-§8.3; "05" below): the process-loopback activation, the
// endpoint loopback of the endpoint engine (README WA53), the initialize attempts in S1's format order, and the client
// the capture thread drains (evented, or polled for the endpoint stream). Only native/windows/src/audio/ and its tests
// include it (win_audio.h's rule).
#pragma once

#include <cstdint>
#include <functional>
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

// How OpenCaptureStream waits for an asynchronous activation (the activator's ActivationContext::Wait, or a
// Completion::WaitFor with a timeout outside the pool) and whether it should give up between its steps.
using ActivationWaitFn = std::function<WaitResult(const std::shared_ptr<Completion> &)>;
using AbortFn = std::function<bool()>;

// Opens one capture stream on the calling thread (COM MTA):
// - kInclude, kExclude: activates a process-loopback stream on req.pid's tree and initializes it in shared, evented
//   loopback mode with a 100 ms buffer, trying float32 48 kHz stereo, PCM16, float32 with the engine's conversion, then
//   PCM16 with it, each on a freshly activated client (05 §8.2; GetMixFormat isn't supported by process loopback),
//   then sets its event and gets its IAudioCaptureClient;
// - kEndpoint: the default render endpoint's (eConsole) classic loopback, polled (the client has no event), with a
//   200 ms buffer, trying float32 with the conversion, PCM16 with it, float32, then PCM16 (05 §8.2).
// The formats tried before the one that worked are counted in the result (failed_attempts, last_failure).
OpenResult OpenCaptureStream(const OpenRequest &req, const ActivationWaitFn &wait, const AbortFn &aborted);

// The activator threads' opener (activator.h): OpenCaptureStream with the pool's wait and abort.
std::shared_ptr<StreamOpener> NewWasapiOpener();

// The activator threads' hook: COM's multithreaded apartment and the thread's name (ActivatorOptions::thread_hook).
void ActivatorThreadHook(void *ctx, bool starting);

} // namespace im::audio
