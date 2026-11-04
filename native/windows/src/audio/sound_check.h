// The first-run sound check (docs/m2/05-windows-audio.md §11.3; "05" below): it captures the chime that
// `isshoni.exe --sound-check-helper` plays (helper_pid), or that the DLL plays itself from a private render session
// (helper_pid 0), with an INCLUDE stream on that process when process loopback works, else with the default render
// endpoint's loopback, and passes when both partials (880 Hz, 1320 Hz) reach -40 dBFS within the timeout.
//
// Portable C++ (no OS header): the logic runs against a LoopbackIo (loopback_io.h); audio_engine.cpp runs it on a DLL
// thread in COM's multithreaded apartment with Windows' io. Tests use fakes
// (native/windows/tests/audio_engine_control_test.cpp).
#pragma once

#include <atomic>
#include <cstdint>
#include <string>

#include "audio/engine_gate.h"
#include "audio/loopback_io.h"
#include "isshoni_media.h"

namespace im::audio {

inline constexpr double kSoundCheckPassDb = -40.0;            // 05 §11.3 step 4, per partial
inline constexpr uint32_t kSoundCheckActivationMs = 3000;     // one capture open's activations (05 §8.1)
inline constexpr uint32_t kSoundCheckWindowFrames = 100 * 48; // Goertzel windows of 100 ms
inline constexpr uint32_t kSoundCheckHopFrames = 20 * 48;
inline constexpr uint32_t kSoundCheckChimeLeadFrames = 100 * 48; // the DLL's own chime starts 100 ms after its stream

struct SoundCheckParams {
  uint32_t helper_pid = 0;   // 0: the DLL plays the chime itself and captures its own process
  int32_t timeout_ms = 3000; // 1000..10000 (the export checks it)
  uint32_t audio_flags = 0;  // im_caps.audio_flags (05 §11.1): which capture path to take
};

// 05 §11.3 step 6.
struct SoundCheckResult {
  bool captured = false;
  double level_db = -120.0;     // captured: the weaker partial's level
  uint32_t ms = 0;              // captured: how long it took
  std::string backend = "none"; // "process-loopback", "endpoint", "none"
  std::string reason;           // not captured: silent, activation_failed, no_device, device_busy, timeout
};

// The capture path's flags for this PC: im_caps.audio_flags without IM_AUDIO_PROCESS_LOOPBACK on a Windows 10 build
// (os_build below kMinIncludeSetBuild; 0: unknown, not gated). Every share there runs the endpoint engine (05 §6.1), so
// the wizard must capture through the endpoint and show the degraded case ("backend":"endpoint", 05 §11.3 step 3).
constexpr uint32_t SoundCheckFlags(uint32_t os_build, uint32_t audio_flags) noexcept {
  return os_build != 0 && os_build < kMinIncludeSetBuild ? audio_flags & ~uint32_t{IM_AUDIO_PROCESS_LOOPBACK}
                                                         : audio_flags;
}

// Runs the check on the calling thread; returns within about timeout_ms (abort ends it early: reason timeout).
SoundCheckResult RunSoundCheck(LoopbackIo &io, const SoundCheckParams &p, const std::atomic<bool> *abort);

// 05 §11.3 step 6's JSON: {"captured":true,"levelDb":-14.3,"ms":1460,"backend":"process-loopback"} or
// {"captured":false,"reason":"silent","backend":"endpoint"}.
std::string SoundCheckJson(const SoundCheckResult &r);

} // namespace im::audio
