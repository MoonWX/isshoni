// im_caps.audio_flags (docs/m2/05-windows-audio.md §11.1; "05" below): what the PC's audio can do, found without
// playing a sound, and the process-loopback activation it shares with the capture streams of README WA45 (05 §8.1).
// Runs on the watcher thread (COM MTA).
#pragma once

#include <cstdint>

#include "audio/win_audio.h"

namespace im::audio {

// How long one activation may take before the probe gives up on it (05 §8.1 allows 3 s to a stream; the probe has
// im_probe's budget, 04 §5.3).
inline constexpr uint32_t kProbeActivationTimeoutMs = 1000;

// Activates an IAudioClient on the process-loopback virtual device (VAD\Process_Loopback) for pid's process tree,
// INCLUDE or EXCLUDE (05 §8.1), and waits at most timeout_ms for ActivateAudioInterfaceAsync's completion. E_NOTIMPL
// when mmdevapi.dll has no ActivateAudioInterfaceAsync (Windows too old); HRESULT_FROM_WIN32(ERROR_TIMEOUT) when the
// completion didn't come in time (the handler stays alive until it does).
HRESULT ActivateProcessLoopback(uint32_t pid, bool include_tree, uint32_t timeout_ms, win::ComPtr<IAudioClient> &out);

// Whether an INCLUDE stream on pid's tree activates and initializes in shared loopback mode, float32 48 kHz stereo
// first, then with the engine's conversion (S1's format order, 05 §8.2). Nothing is started.
bool ProcessLoopbackWorks(uint32_t pid, uint32_t timeout_ms);

// Whether classic loopback of the default render endpoint (eConsole) initializes in its mix format.
bool EndpointLoopbackWorks(IMMDeviceEnumerator *en);

// Whether a session notification registers (and unregisters again) on every active render endpoint, at least one.
bool SessionEventsWork(IMMDeviceEnumerator *en);

// The number of active endpoints of a flow, 0 when the listing fails.
uint32_t ActiveEndpoints(IMMDeviceEnumerator *en, EDataFlow flow);

} // namespace im::audio
