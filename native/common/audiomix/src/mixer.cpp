#include "im/audiomix/mixer.h"

#include <chrono>
#include <new>
#include <system_error>

#include "im/os/clock.h"

namespace im::audiomix {

int64_t SteadyTickClock::NowNs() noexcept { return os::SteadyClock::Get().NowNs(); }

void SteadyTickClock::WaitUntil(int64_t deadline_ns, const std::atomic<bool> &stop) noexcept {
  std::unique_lock<std::mutex> lock(mu_);
  while (!stop.load(std::memory_order_acquire)) {
    const int64_t now = NowNs();
    if (now >= deadline_ns) {
      return;
    }
    cv_.wait_for(lock, std::chrono::nanoseconds(deadline_ns - now));
  }
}

void SteadyTickClock::Interrupt() noexcept {
  {
    std::lock_guard<std::mutex> lock(mu_);
  }
  cv_.notify_all();
}

Mixer::Mixer(const MixerOptions &options, std::unique_ptr<TickClock> own_clock)
    : engine_(options.sink, options.params), own_clock_(std::move(own_clock)),
      clock_(options.clock != nullptr ? options.clock : own_clock_.get()), hook_(options.thread_hook),
      hook_ctx_(options.hook_ctx) {}

im_status Mixer::Create(const MixerOptions &options, std::unique_ptr<Mixer> *out) {
  if (out == nullptr || options.sink == nullptr) {
    return IM_ERR_INVALID_ARG;
  }
  std::unique_ptr<TickClock> own;
  if (options.clock == nullptr) {
    own.reset(new (std::nothrow) SteadyTickClock());
    if (!own) {
      return IM_ERR_INTERNAL;
    }
  }
  auto *m = new (std::nothrow) Mixer(options, std::move(own));
  if (m == nullptr) {
    return IM_ERR_INTERNAL;
  }
  out->reset(m);
  return IM_OK;
}

Mixer::~Mixer() { Stop(); }

im_status Mixer::Start() noexcept {
  if (running_) {
    return IM_ERR_BAD_STATE;
  }
  stop_.store(false, std::memory_order_release);
  started_.store(false, std::memory_order_release);
  try {
    thread_ = std::thread([this] { Run(); });
  } catch (const std::system_error &) {
    return IM_ERR_INTERNAL;
  } catch (const std::bad_alloc &) {
    return IM_ERR_INTERNAL;
  }
  running_ = true;
  std::unique_lock<std::mutex> lock(start_mu_);
  start_cv_.wait(lock, [this] { return started_.load(std::memory_order_acquire); });
  return IM_OK;
}

void Mixer::Stop() noexcept {
  if (!running_) {
    return;
  }
  stop_.store(true, std::memory_order_release);
  clock_->Interrupt();
  thread_.join();
  running_ = false;
}

void Mixer::Run() noexcept {
  if (hook_ != nullptr) {
    hook_(hook_ctx_, true);
  }
  engine_.Begin(clock_->NowNs());
  {
    std::lock_guard<std::mutex> lock(start_mu_);
    started_.store(true, std::memory_order_release);
  }
  start_cv_.notify_all();
  while (!stop_.load(std::memory_order_acquire)) {
    const int64_t t0 = clock_->NowNs();
    const int64_t next = engine_.Pump(t0);
    busy_ns_.store(busy_ns_.load(std::memory_order_relaxed) + (clock_->NowNs() - t0), std::memory_order_relaxed);
    if (stop_.load(std::memory_order_acquire)) {
      break;
    }
    clock_->WaitUntil(next, stop_);
  }
  if (hook_ != nullptr) {
    hook_(hook_ctx_, false);
  }
}

} // namespace im::audiomix
