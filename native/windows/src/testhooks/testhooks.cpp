#include <cstdint> // keeps this file's translation unit non-empty in builds without test hooks

#include "testhooks/testhooks.h"

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS

#include <algorithm>
#include <atomic>
#include <mutex>
#include <utility>

#include "session/session.h"

namespace im::testhooks {
namespace {

struct State {
  std::mutex mu;
  Options options;                  // guarded by mu
  std::vector<uint32_t> fail_opens; // guarded by mu
  std::atomic<bool> any_fail_opens{false};
  std::atomic<int64_t> pick_clock_offset{0};
};

State &TheState() noexcept {
  static State *const s = new State(); // never destroyed, like the engine
  return *s;
}

} // namespace

void Configure(const Options &options) {
  State &s = TheState();
  std::lock_guard<std::mutex> lock(s.mu);
  s.options = options;
}

Options Current() {
  State &s = TheState();
  std::lock_guard<std::mutex> lock(s.mu);
  return s.options;
}

void Reset() {
  State &s = TheState();
  {
    std::lock_guard<std::mutex> lock(s.mu);
    s.options = Options{};
    s.fail_opens.clear();
    s.any_fail_opens.store(false, std::memory_order_release);
  }
  s.pick_clock_offset.store(0, std::memory_order_release);
  session::ResetProbeCacheStale();
}

void FailOpens(uint32_t kind, bool fail) {
  State &s = TheState();
  std::lock_guard<std::mutex> lock(s.mu);
  auto it = std::find(s.fail_opens.begin(), s.fail_opens.end(), kind);
  if (fail && it == s.fail_opens.end()) {
    s.fail_opens.push_back(kind);
  } else if (!fail && it != s.fail_opens.end()) {
    s.fail_opens.erase(it);
  }
  s.any_fail_opens.store(!s.fail_opens.empty(), std::memory_order_release);
}

bool OpensFail(uint32_t kind) noexcept {
  State &s = TheState();
  if (!s.any_fail_opens.load(std::memory_order_acquire)) {
    return false;
  }
  std::lock_guard<std::mutex> lock(s.mu);
  return std::find(s.fail_opens.begin(), s.fail_opens.end(), kind) != s.fail_opens.end();
}

std::string FakePick(uint32_t kind, uint32_t width, uint32_t height) {
  const Options o = Current();
  session::SourceSpec spec;
  spec.kind = kind == IM_SOURCE_MONITOR ? IM_SOURCE_MONITOR : IM_SOURCE_WINDOW;
  spec.capture_path = IM_CAPTURE_TEST;
  spec.width = width;
  spec.height = height;
  spec.test = true;
  spec.pattern = o.pattern;
  spec.pattern_hz = o.pattern_hz;
  return session::Sources().AddPick(std::move(spec));
}

void AdvancePickClock(int64_t ns) noexcept { TheState().pick_clock_offset.fetch_add(ns, std::memory_order_acq_rel); }

int64_t PickClockOffsetNs() noexcept { return TheState().pick_clock_offset.load(std::memory_order_acquire); }

im_status Inject(im_session s, uint32_t layer, Fault fault) {
  return session::InjectFault(s, layer,
                              fault == Fault::kDeviceRemoved ? session::InjectedFault::kDeviceRemoved
                                                             : session::InjectedFault::kEncodeError);
}

uint64_t SessionDevice(im_session s) { return session::DeviceOf(s); }

bool ProbeCacheStale() noexcept { return session::ProbeCacheStale(); }

} // namespace im::testhooks

#endif // IM_TEST_HOOKS
