#include "engine/engine.h"

#include <atomic>
#include <mutex>
#include <utility>

#include "api/guard.h"
#include "audio/mixer.h"
#include "audio_bridge/audio_bridge.h"
#include "capture/capture.h"
#include "engine/os.h"
#include "im/abi/status.h"
#include "im/log/ring.h"
#include "session/session.h"

namespace im::engine {
namespace {

struct State {
  std::mutex mu; // serializes Init and Shutdown (which must not race other calls anyway, 04 §5.1)
  std::atomic<bool> initialized{false};
  std::mutex options_mu; // a mutex of its own: jobs that Init and Shutdown wait for may read the options
  Options options;       // guarded by options_mu
  Worker worker;
};

State &TheState() noexcept {
  static State *const s = new State(); // never destroyed: see engine.h
  return *s;
}

} // namespace

im_status Init(Options options) {
  State &s = TheState();
  std::lock_guard<std::mutex> lock(s.mu);
  if (s.initialized.load(std::memory_order_acquire)) {
    return abi::Fail(IM_ERR_BAD_STATE, {}, "im_init: already initialized (call im_shutdown first)");
  }
  log::Default().SetLevel(options.log_level);
  os::ClearLibvplSearchPaths();
  const uint32_t flags = options.init_flags;
  const uint32_t caller_abi = options.caller_abi;
  const bool ffmpeg_dir_set = options.ffmpeg_dir.has_value();
  const bool openh264_set = options.openh264_path.has_value();
  {
    // Before the first job: whatever runs on the worker from here on reads this im_init's options.
    std::lock_guard<std::mutex> olock(s.options_mu);
    s.options = std::move(options);
  }

  im_status st = s.worker.Start();
  if (st == IM_OK) {
    st = s.worker.Post(
        "im_init: AudioEngine::Init", bound::kInit, [flags]() -> int32_t { return audio_bridge::Engine().Init(flags); },
        [](int32_t late) {
          // im_init gave up waiting (04 §6.1) and failed, so no im_shutdown will stop what this Init started: the
          // worker does, before Stop below lets it exit.
          if (late == IM_OK) {
            audio_bridge::Engine().Shutdown();
          }
        });
    if (st == IM_ERR_TIMEOUT) {
      // The worker is stuck in AudioEngine::Init: im_init fails, and the late handler above cleans up.
      s.worker.Stop(bound::kWorkerExit);
    } else if (st < 0) {
      // Audio never fails a call (04 §10.1, 05 §4.3): video sharing works without it. 05's report shows the audio
      // state, and im_shutdown still calls AudioEngine::Shutdown, which copes with a failed Init.
      log::Warn("engine", "audio init failed", {{"status", st}, {"detail", abi::LastErrorText()}});
      abi::ClearLastError(); // im_init succeeds: no detail is left behind
      st = IM_OK;
    }
  }
  if (st < 0) {
    std::lock_guard<std::mutex> olock(s.options_mu);
    s.options = Options{};
    return st;
  }

  if (caller_abi != IM_ABI_VERSION) {
    // Same major (im_init checked it): the struct sizes carry the real compatibility (04 §5.5).
    log::Warn("engine", "caller ABI minor differs", {{"engine_abi", IM_ABI_VERSION}, {"caller_abi", caller_abi}});
  }
  log::Info(
      "engine", "initialized",
      {{"abi", IM_ABI_VERSION}, {"flags", flags}, {"ffmpeg_dir_set", ffmpeg_dir_set}, {"openh264", openh264_set}});
  s.initialized.store(true, std::memory_order_release);
  return IM_OK;
}

void Shutdown() noexcept {
  State &s = TheState();
  std::lock_guard<std::mutex> lock(s.mu);
  if (!s.initialized.exchange(false, std::memory_order_acq_rel)) {
    return;
  }
  api::Guard("im_shutdown: sessions", [] { session::StopAll(); });
  api::Guard("im_shutdown: capture", [] { capture::Shutdown(); });
  api::Guard("im_shutdown: audio", [&] {
    s.worker.Post("im_shutdown: AudioEngine::Shutdown", bound::kShutdown, []() -> int32_t {
      audio_bridge::Engine().Shutdown();
      return IM_OK;
    });
  });
  s.worker.Stop(bound::kWorkerExit);
  api::Guard("im_shutdown: options", [&] {
    std::lock_guard<std::mutex> olock(s.options_mu);
    s.options = Options{};
  });
  log::Info("engine", "shut down");
}

bool Initialized() noexcept { return TheState().initialized.load(std::memory_order_acquire); }

im_status RequireInit(const char *fn) noexcept {
  if (Initialized()) {
    return IM_OK;
  }
  return abi::Failf(IM_ERR_BAD_STATE, abi::token::kNotInitialized, "%s: call im_init first", fn);
}

Options CurrentOptions() {
  State &s = TheState();
  std::lock_guard<std::mutex> lock(s.options_mu);
  return s.options;
}

Worker &TheWorker() noexcept { return TheState().worker; }

} // namespace im::engine
