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

#include "audio/loopback_io.h"

namespace im::audio {

inline constexpr double kSoundCheckPassDb = -40.0;            // 05 §11.3 step 4, per partial
inline constexpr uint32_t kSoundCheckActivationMs = 3000;     // one INCLUDE activation (05 §8.1)
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

// Runs the check on the calling thread; returns within about timeout_ms (abort ends it early: reason timeout).
SoundCheckResult RunSoundCheck(LoopbackIo &io, const SoundCheckParams &p, const std::atomic<bool> *abort);

// 05 §11.3 step 6's JSON: {"captured":true,"levelDb":-14.3,"ms":1460,"backend":"process-loopback"} or
// {"captured":false,"reason":"silent","backend":"endpoint"}.
std::string SoundCheckJson(const SoundCheckResult &r);

} // namespace im::audio
