#include "audio/activator.h"

#include <algorithm>
#include <deque>
#include <exception>
#include <system_error>
#include <utility>

#include "im/log/ring.h"

namespace im::audio {

namespace detail {

// The pool's state. Its threads hold a reference each, so an abandoned thread (Stop) never touches freed memory.
struct ActivatorShared {
  ActivatorShared(std::shared_ptr<StreamOpener> o, ActivatorOptions opt) : opener(std::move(o)), options(opt) {}

  // One thread's slot: the request it runs and the completion it waits for.
  struct Slot {
    bool busy = false;
    uint64_t id = 0;
    bool cancelled = false;
    std::shared_ptr<Completion> waiting;
  };

  const std::shared_ptr<StreamOpener> opener;
  const ActivatorOptions options;

  std::mutex mu; // guards what follows
  std::condition_variable cv;
  bool stop = false;
  std::deque<OpenRequest> queue;
  std::vector<Slot> slots;
  std::vector<OpenResult> results;
  uint32_t running_threads = 0; // threads that have not left Run yet
  std::condition_variable exited;

  // Wakes slot's wait (stop or cancel). mu held.
  void AbortSlot(Slot &s) {
    s.cancelled = true;
    if (s.waiting) {
      std::lock_guard<std::mutex> l(s.waiting->mu_);
      s.waiting->aborted_ = true;
      s.waiting->cv_.notify_all();
    }
  }

  WaitResult Wait(uint32_t slot, const std::shared_ptr<Completion> &c) {
    {
      std::lock_guard<std::mutex> l(mu);
      if (stop || slots[slot].cancelled) {
        return WaitResult::kAborted;
      }
      slots[slot].waiting = c;
    }
    WaitResult r = WaitResult::kTimeout;
    {
      std::unique_lock<std::mutex> l(c->mu_);
      if (c->cv_.wait_for(l, options.timeout, [&] { return c->done_ || c->aborted_; })) {
        r = c->done_ ? WaitResult::kDone : WaitResult::kAborted;
      }
    }
    std::lock_guard<std::mutex> l(mu);
    slots[slot].waiting.reset();
    if (r != WaitResult::kDone && (stop || slots[slot].cancelled)) {
      r = WaitResult::kAborted;
    }
    return r;
  }

  bool Aborted(uint32_t slot) {
    std::lock_guard<std::mutex> l(mu);
    return stop || slots[slot].cancelled;
  }

  void Run(uint32_t slot) noexcept {
    if (options.thread_hook != nullptr) {
      options.thread_hook(options.hook_ctx, true);
    }
    for (;;) {
      OpenRequest req;
      {
        std::unique_lock<std::mutex> l(mu);
        cv.wait(l, [&] { return stop || !queue.empty(); });
        if (stop) {
          break;
        }
        req = queue.front();
        queue.pop_front();
        slots[slot] = Slot{true, req.id, false, {}};
      }
      OpenResult res;
      try {
        ActivationContext ctx(this, slot);
        res = opener->Open(req, ctx);
      } catch (const std::exception &) {
        res = OpenResult{};
        res.hr = static_cast<int32_t>(0x8000FFFFu); // E_UNEXPECTED
        log::Error("audio", "audio activation threw");
      }
      res.id = req.id;
      bool deliver = true;
      {
        std::lock_guard<std::mutex> l(mu);
        if (slots[slot].cancelled || stop) {
          res.aborted = true;
          deliver = !stop; // a stopped pool has no reader; a cancelled request's result says aborted
        }
        if (deliver) {
          results.push_back(std::move(res));
        }
        slots[slot] = Slot{};
      }
      if (!deliver) {
        res = OpenResult{}; // releases the client here, on the activator thread (COM MTA)
      }
      if (deliver && options.on_result != nullptr) {
        options.on_result(options.result_ctx);
      }
    }
    if (options.thread_hook != nullptr) {
      options.thread_hook(options.hook_ctx, false);
    }
    std::lock_guard<std::mutex> l(mu);
    --running_threads;
    exited.notify_all();
  }
};

} // namespace detail

void Completion::Complete() noexcept {
  std::lock_guard<std::mutex> l(mu_);
  done_ = true;
  cv_.notify_all();
}

bool Completion::done() const noexcept {
  std::lock_guard<std::mutex> l(mu_);
  return done_;
}

bool Completion::WaitFor(std::chrono::milliseconds timeout) const {
  std::unique_lock<std::mutex> l(mu_);
  return cv_.wait_for(l, timeout, [&] { return done_; });
}

WaitResult ActivationContext::Wait(const std::shared_ptr<Completion> &c) {
  if (!c) {
    return WaitResult::kAborted;
  }
  return sh_->Wait(slot_, c);
}

bool ActivationContext::aborted() const noexcept { return sh_->Aborted(slot_); }

ActivatorPool::ActivatorPool(std::shared_ptr<StreamOpener> opener, ActivatorOptions options)
    : sh_(std::make_shared<detail::ActivatorShared>(std::move(opener), options)) {}

ActivatorPool::~ActivatorPool() { Stop(); }

im_status ActivatorPool::Start() {
  if (!threads_.empty()) {
    return IM_OK;
  }
  bool abandoned = false;
  {
    std::lock_guard<std::mutex> l(sh_->mu);
    abandoned = sh_->running_threads != 0;
  }
  if (abandoned) {
    // A previous Stop abandoned a thread: start over with fresh state, which the old thread no longer shares.
    sh_ = std::make_shared<detail::ActivatorShared>(sh_->opener, sh_->options);
  }
  const uint32_t n = std::max<uint32_t>(1, sh_->options.threads);
  {
    std::lock_guard<std::mutex> l(sh_->mu);
    sh_->stop = false;
    sh_->slots.assign(n, {});
  }
  for (uint32_t i = 0; i < n; ++i) {
    try {
      {
        std::lock_guard<std::mutex> l(sh_->mu);
        ++sh_->running_threads;
      }
      threads_.emplace_back([sh = sh_, i] { sh->Run(i); });
    } catch (const std::system_error &) {
      {
        std::lock_guard<std::mutex> l(sh_->mu);
        --sh_->running_threads;
      }
      Stop();
      return IM_ERR_INTERNAL;
    }
  }
  return IM_OK;
}

void ActivatorPool::Stop() noexcept {
  if (threads_.empty()) {
    return;
  }
  bool all = false;
  {
    std::unique_lock<std::mutex> l(sh_->mu);
    sh_->stop = true;
    sh_->queue.clear();
    for (auto &s : sh_->slots) {
      if (s.busy) {
        sh_->AbortSlot(s);
      }
    }
    sh_->cv.notify_all();
    all = sh_->exited.wait_for(l, kActivatorJoin, [&] { return sh_->running_threads == 0; });
  }
  for (std::thread &t : threads_) {
    try {
      if (all) {
        t.join();
      } else {
        t.detach(); // stuck in a driver call: abandoned, never killed; it holds its own reference to the state
      }
    } catch (const std::system_error &) {
      log::Error("audio", "audio activator thread: join failed");
    }
  }
  if (!all) {
    log::Error("audio", "audio activator thread abandoned after 2 s");
  }
  threads_.clear();
  std::lock_guard<std::mutex> l(sh_->mu);
  sh_->results.clear(); // releases the clients of results nobody took (the caller's thread: the watcher, COM MTA)
}

bool ActivatorPool::Submit(const OpenRequest &req) {
  {
    std::lock_guard<std::mutex> l(sh_->mu);
    if (threads_.empty() || sh_->stop) {
      return false;
    }
    uint32_t busy = 0;
    for (const auto &s : sh_->slots) {
      busy += s.busy ? 1u : 0u;
    }
    if (busy + sh_->queue.size() >= kMaxActivationsInFlight) {
      return false;
    }
    sh_->queue.push_back(req);
  }
  sh_->cv.notify_one();
  return true;
}

void ActivatorPool::Cancel(uint64_t id) {
  std::lock_guard<std::mutex> l(sh_->mu);
  sh_->queue.erase(
      std::remove_if(sh_->queue.begin(), sh_->queue.end(), [&](const OpenRequest &r) { return r.id == id; }),
      sh_->queue.end());
  for (auto &s : sh_->slots) {
    if (s.busy && s.id == id) {
      sh_->AbortSlot(s);
    }
  }
}

uint32_t ActivatorPool::in_flight() const {
  std::lock_guard<std::mutex> l(sh_->mu);
  uint32_t n = static_cast<uint32_t>(sh_->queue.size());
  for (const auto &s : sh_->slots) {
    n += s.busy ? 1u : 0u;
  }
  return n;
}

std::vector<OpenResult> ActivatorPool::TakeResults() {
  std::lock_guard<std::mutex> l(sh_->mu);
  return std::exchange(sh_->results, {});
}

} // namespace im::audio
