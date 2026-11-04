// Windows' held handles of include targets (process_hold.h, docs/m2/05-windows-audio.md §5.7).
#include "audio/process_hold.h"

#include "audio/win_audio.h"

#include <atomic>
#include <memory>
#include <utility>

#include "im/log/ring.h"

namespace im::audio {
namespace {

// What the wait's callback touches: owned by the hold, which unregisters the wait (and waits for a running callback)
// before it goes.
struct ExitState {
  std::atomic<bool> exited{false};
  void (*wake)(void *) = nullptr;
  void *ctx = nullptr;
};

void CALLBACK OnExit(PVOID param, BOOLEAN) {
  auto *s = static_cast<ExitState *>(param);
  s->exited.store(true, std::memory_order_release);
  if (s->wake != nullptr) {
    s->wake(s->ctx);
  }
}

class WinHold final : public ProcessHold {
public:
  WinHold(win::Handle process, void (*wake)(void *), void *ctx) : process_(std::move(process)) {
    state_.wake = wake;
    state_.ctx = ctx;
    // One-shot: the thread pool calls OnExit once the process ends (WT_EXECUTEONLYONCE).
    if (!RegisterWaitForSingleObject(&wait_, process_.get(), &OnExit, &state_, INFINITE, WT_EXECUTEONLYONCE)) {
      wait_ = nullptr;
      log::Warn("audio", "process exit wait not registered: the poll notices the exit");
    }
  }
  ~WinHold() override {
    if (wait_ != nullptr) {
      UnregisterWaitEx(wait_, INVALID_HANDLE_VALUE); // waits for a running OnExit
    }
  }
  WinHold(const WinHold &) = delete;
  WinHold &operator=(const WinHold &) = delete;

  bool exited() const noexcept override { return state_.exited.load(std::memory_order_acquire); }

private:
  win::Handle process_;
  ExitState state_;
  HANDLE wait_ = nullptr;
};

class WinGate final : public ProcessGate {
public:
  WinGate(void (*wake)(void *), void *ctx) : wake_(wake), ctx_(ctx) {}

  HoldStatus Hold(const audiopolicy::ProcessKey &key, std::unique_ptr<ProcessHold> *out) override {
    out->reset();
    win::Handle h(OpenProcess(SYNCHRONIZE | PROCESS_QUERY_LIMITED_INFORMATION, FALSE, key.pid));
    if (!h) {
      // No such process (any more): plan again. Anything else (access denied by a protected process or an anti-cheat
      // driver): activate anyway, checked by snapshot (05 §5.7).
      return GetLastError() == ERROR_INVALID_PARAMETER ? HoldStatus::kMismatch : HoldStatus::kNoHandle;
    }
    if (WaitForSingleObject(h.get(), 0) == WAIT_OBJECT_0) {
      return HoldStatus::kMismatch; // already exited
    }
    FILETIME created{}, exited{}, kernel{}, user{};
    if (key.created != 0 && GetProcessTimes(h.get(), &created, &exited, &kernel, &user) &&
        win::FiletimeValue(created) != key.created) {
      return HoldStatus::kMismatch; // the pid is another process now: the plan is recomputed
    }
    *out = std::make_unique<WinHold>(std::move(h), wake_, ctx_);
    return HoldStatus::kHeld;
  }

private:
  void (*wake_)(void *);
  void *ctx_;
};

} // namespace

std::unique_ptr<ProcessGate> NewWindowsProcessGate(void (*wake)(void *ctx), void *ctx) {
  return std::make_unique<WinGate>(wake, ctx);
}

} // namespace im::audio
