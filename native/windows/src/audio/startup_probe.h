// The startup probe (docs/m2/05-windows-audio.md §11.2; "05" below), run at each audio start in parallel with the
// video start (<= 600 ms): an INCLUDE stream on the DLL's own process, a private render session on the default render
// endpoint playing 200 ms of 997 Hz at -60 dBFS (barely audible: the loopback is digital and pre-volume), and a
// Goertzel level of what came back. The watcher starts it through a ProbeRunner (its own thread) and hands the result
// to the EngineGate (engine_gate.h), which picks the engine.
//
// Portable C++ (no OS header): the probe's logic runs against a LoopbackIo (loopback_io.h); the Windows backend gives
// it the WASAPI io on a COM MTA thread. Tests use fakes (native/windows/tests/audio_engine_control_test.cpp).
#pragma once

#include <atomic>
#include <condition_variable>
#include <cstdint>
#include <functional>
#include <memory>
#include <mutex>
#include <optional>
#include <thread>

#include "audio/engine_gate.h"
#include "audio/loopback_io.h"

namespace im::audio {

inline constexpr uint32_t kProbeActivationMs = 500;      // 05 §11.2 step 1
inline constexpr uint32_t kProbeLeadFrames = 30 * 48;    // zeros before the tone
inline constexpr uint32_t kProbeTailFrames = 60 * 48;    // and after it, so its end is captured
inline constexpr uint32_t kProbeWindowFrames = 160 * 48; // the measured window: the tone minus 20 ms at each end
inline constexpr uint32_t kProbeHopFrames = 10 * 48;

// One probe on io (05 §11.2 steps 1-5): no render client → skipped; the INCLUDE stream fails → unsupported; else the
// tone is played and measured. abort ends it early (skipped).
ProbeReport RunStartupProbe(LoopbackIo &io, const std::atomic<bool> *abort);

// Where a ProbeRunner gets its probe from.
class ProbeBackend {
public:
  virtual ~ProbeBackend() = default;
  // Runs one probe on the calling thread (the runner's).
  virtual ProbeReport Run(const std::atomic<bool> &abort) noexcept = 0;
};

// Windows' backend: COM MTA on the runner's thread, NewWindowsLoopbackIo, RunStartupProbe.
std::shared_ptr<ProbeBackend> NewWindowsProbeBackend();

// Runs one probe at a time on a thread of its own and hands the result back to the watcher.
class ProbeRunner {
public:
  // wake: called on the runner's thread once a result is ready (must not block; it may outlive the runner's owner,
  // so it holds what it needs).
  ProbeRunner(std::shared_ptr<ProbeBackend> backend, std::function<void()> wake);
  ~ProbeRunner(); // Stop
  ProbeRunner(const ProbeRunner &) = delete;
  ProbeRunner &operator=(const ProbeRunner &) = delete;

  // Starts a probe. False while one runs (or the thread can't start).
  bool Start();
  // The result of the last probe, once (false while none is ready).
  bool TakeResult(ProbeReport *out);
  // Aborts a running probe and joins it within 2 s; one stuck in a driver call is abandoned (05 §10.5). Its result is
  // dropped.
  void Stop() noexcept;
  bool running() const;

private:
  struct Shared {
    std::mutex mu;
    std::condition_variable cv;
    bool done = false;
    std::optional<ProbeReport> result;
    std::atomic<bool> abort{false};
  };
  void Reap();

  std::shared_ptr<ProbeBackend> backend_;
  std::function<void()> wake_;
  std::shared_ptr<Shared> sh_;
  std::thread thread_;
};

} // namespace im::audio
