// The mixer thread (docs/m2/05-windows-audio.md §9.1, §10.5): one thread that runs a MixEngine on a clock, a chunk
// every 10 ms. On Windows the clock is a high-resolution waitable timer on the QPC and the thread runs under MMCSS
// "Pro Audio" (native/windows/src/audio/, README WA45, through TickClock and MixerOptions::thread_hook); elsewhere
// SteadyTickClock stands in. After Start the thread never allocates, never takes a lock the engine shares and never
// blocks except in the clock's wait.
#pragma once

#include <atomic>
#include <condition_variable>
#include <cstdint>
#include <memory>
#include <mutex>
#include <thread>

#include "im/audiomix/mix_engine.h"
#include "isshoni_media.h"

namespace im::audiomix {

// The mixer's clock: the engine clock (QPC ns on Windows) and a wait for a deadline on it.
class TickClock {
public:
  virtual ~TickClock() = default;
  virtual int64_t NowNs() noexcept = 0;
  // Called on the mixer thread only. Blocks until NowNs() >= deadline_ns or stop is true; may return early. Must not
  // allocate.
  virtual void WaitUntil(int64_t deadline_ns, const std::atomic<bool> &stop) noexcept = 0;
  // Wakes a WaitUntil (Stop calls it after setting stop).
  virtual void Interrupt() noexcept = 0;
};

// The monotonic clock (im::os::SteadyClock's time base) with a condition-variable wait: hosts and tests.
class SteadyTickClock final : public TickClock {
public:
  int64_t NowNs() noexcept override;
  void WaitUntil(int64_t deadline_ns, const std::atomic<bool> &stop) noexcept override;
  void Interrupt() noexcept override;

private:
  std::mutex mu_;
  std::condition_variable cv_;
};

struct MixerOptions {
  ChunkSink *sink = nullptr;  // required; must outlive the mixer
  TickClock *clock = nullptr; // nullptr: a SteadyTickClock of the mixer's own; else must outlive the mixer
  MixParams params;
  // Called on the mixer thread with starting = true before its first chunk (before Start returns) and with false
  // after its last one (before the thread ends): MMCSS on Windows, the allocation count in tests. It may allocate.
  void (*thread_hook)(void *ctx, bool starting) = nullptr;
  void *hook_ctx = nullptr;
};

class Mixer {
public:
  // Allocates the engine (and the clock when none is given). IM_ERR_INVALID_ARG without a sink.
  static im_status Create(const MixerOptions &options, std::unique_ptr<Mixer> *out);
  ~Mixer(); // Stop
  Mixer(const Mixer &) = delete;
  Mixer &operator=(const Mixer &) = delete;

  // Starts the mixer thread: the first chunk is due at once and starts a grid (MixEngine::Begin). Returns once the
  // thread runs. IM_ERR_BAD_STATE while running; IM_ERR_INTERNAL when the thread can't be created.
  im_status Start() noexcept;
  // Stops and joins the thread: no OnChunk or OnGap after it returns. The channels stay in the engine; ReleaseAll
  // hands them back. Start may follow (a new grid).
  void Stop() noexcept;
  bool running() const noexcept { return running_; }

  MixEngine &engine() noexcept { return engine_; }
  const MixEngine &engine() const noexcept { return engine_; }
  // Engine-clock time spent making chunks since Create (the mixer's share of audioCpuMsPerS, 05 §12.1).
  int64_t busy_ns() const noexcept { return busy_ns_.load(std::memory_order_relaxed); }

private:
  explicit Mixer(const MixerOptions &options, std::unique_ptr<TickClock> own_clock);
  void Run() noexcept;

  MixEngine engine_;
  std::unique_ptr<TickClock> own_clock_;
  TickClock *clock_;
  void (*hook_)(void *, bool);
  void *hook_ctx_;
  std::thread thread_;
  std::atomic<bool> stop_{false};
  std::atomic<bool> started_{false};
  std::mutex start_mu_;
  std::condition_variable start_cv_;
  bool running_ = false;
  std::atomic<int64_t> busy_ns_{0};
};

} // namespace im::audiomix
