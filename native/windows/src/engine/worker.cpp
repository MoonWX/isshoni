#include "engine/worker.h"

#include <exception>
#include <system_error>
#include <utility>

#include "api/guard.h"
#include "engine/os.h"
#include "im/abi/status.h"
#include "im/log/ring.h"

namespace im::engine {

// One posted job and its result, shared by the poster and the worker (the poster may give up first).
struct Worker::Task {
  Job job;
  LateHandler late;
  std::mutex mu;
  std::condition_variable cv;
  bool done = false;      // guarded by mu
  bool abandoned = false; // guarded by mu: the poster timed out
  int32_t result = IM_ERR_INTERNAL;
  std::string detail; // the job's im_last_error text
};

// The thread's side, kept alive by the thread itself: an abandoned thread never touches a newer worker's state.
struct Worker::State {
  std::mutex mu;
  std::condition_variable cv; // a job was queued, stopping was set, the thread started or exited
  std::deque<std::shared_ptr<Task>> queue;
  bool stopping = false;
  bool started = false;
  bool exited = false;
  int32_t start_result = 0; // EnterWorkerThread's HRESULT
  std::thread::id id;
};

Worker::~Worker() { Stop(std::chrono::seconds(2)); }

im_status Worker::Start() {
  std::lock_guard<std::mutex> lock(mu_);
  if (state_) {
    return abi::Fail(IM_ERR_BAD_STATE, {}, "engine worker: already running");
  }
  auto st = std::make_shared<State>();
  std::thread th;
  try {
    th = std::thread(&Worker::Run, st);
  } catch (const std::system_error &e) {
    return abi::Failf(IM_ERR_INTERNAL, {}, "engine worker: cannot start the thread: %s", e.what());
  }
  std::unique_lock<std::mutex> lk(st->mu);
  st->cv.wait(lk, [&] { return st->started; });
  if (st->start_result != 0) {
    const int32_t hr = st->start_result;
    lk.unlock();
    th.join(); // the thread exits right after a failed start
    return abi::Failf(IM_ERR_INTERNAL, {}, "engine worker: CoInitializeEx(COINIT_MULTITHREADED) failed (0x%08X)",
                      static_cast<unsigned>(hr));
  }
  lk.unlock();
  state_ = std::move(st);
  thread_ = std::move(th);
  return IM_OK;
}

void Worker::Stop(std::chrono::milliseconds bound) {
  std::shared_ptr<State> st;
  std::thread th;
  {
    std::lock_guard<std::mutex> lock(mu_);
    st = std::move(state_);
    th = std::move(thread_);
  }
  if (!st) {
    return;
  }
  bool exited = false;
  {
    std::unique_lock<std::mutex> lk(st->mu);
    st->stopping = true;
    st->cv.notify_all();
    if (std::this_thread::get_id() != st->id) {
      exited = st->cv.wait_for(lk, bound, [&] { return st->exited; });
    }
  }
  if (exited) {
    th.join();
    return;
  }
  // A stuck job (or a Stop from a job on the worker itself): the thread is left to finish on its own.
  th.detach();
  log::Error("engine", "worker abandoned", {{"bound_ms", static_cast<int64_t>(bound.count())}});
}

bool Worker::Running() const {
  std::lock_guard<std::mutex> lock(mu_);
  return state_ != nullptr;
}

bool Worker::OnWorkerThread() const {
  std::lock_guard<std::mutex> lock(mu_);
  return state_ && state_->id == std::this_thread::get_id();
}

int32_t Worker::Post(std::string_view what, std::chrono::milliseconds bound, Job job, LateHandler late) {
  std::shared_ptr<State> st;
  {
    std::lock_guard<std::mutex> lock(mu_);
    st = state_;
  }
  const auto what_len = static_cast<int>(what.size());
  if (!st) {
    return abi::Failf(IM_ERR_BAD_STATE, abi::token::kNotInitialized, "%.*s: the engine worker is not running", what_len,
                      what.data());
  }
  if (st->id == std::this_thread::get_id()) {
    return job(); // a job that posts again would wait for itself
  }

  auto task = std::make_shared<Task>();
  task->job = std::move(job);
  task->late = std::move(late);
  {
    std::lock_guard<std::mutex> lock(st->mu);
    if (st->stopping) {
      return abi::Failf(IM_ERR_BAD_STATE, abi::token::kNotInitialized, "%.*s: the engine is shutting down", what_len,
                        what.data());
    }
    st->queue.push_back(task);
  }
  st->cv.notify_all();

  std::unique_lock<std::mutex> lk(task->mu);
  if (!task->cv.wait_for(lk, bound, [&] { return task->done; })) {
    task->abandoned = true;
    lk.unlock();
    const auto ms = static_cast<long long>(bound.count());
    log::Error("engine", "posted job timed out", {{"call", what}, {"bound_ms", static_cast<int64_t>(ms)}});
    return abi::Failf(IM_ERR_TIMEOUT, abi::token::kEngineTimeout, "%.*s did not finish within %lld ms", what_len,
                      what.data(), ms);
  }
  const int32_t result = task->result;
  const std::string detail = std::move(task->detail);
  lk.unlock();
  if (result < 0) {
    abi::Fail(result, {}, detail);
  }
  return result;
}

void Worker::Run(std::shared_ptr<State> st) noexcept {
  const int32_t hr = os::EnterWorkerThread();
  {
    std::lock_guard<std::mutex> lock(st->mu);
    st->id = std::this_thread::get_id();
    st->start_result = hr;
    st->started = true;
    if (hr != 0) {
      st->exited = true;
    }
  }
  st->cv.notify_all();
  if (hr != 0) {
    return;
  }
  for (;;) {
    std::shared_ptr<Task> task;
    {
      std::unique_lock<std::mutex> lk(st->mu);
      st->cv.wait(lk, [&] { return st->stopping || !st->queue.empty(); });
      if (st->queue.empty()) {
        break; // stopping, and every queued job has run
      }
      task = std::move(st->queue.front());
      st->queue.pop_front();
    }
    RunTask(*task);
  }
  os::LeaveWorkerThread();
  {
    std::lock_guard<std::mutex> lock(st->mu);
    st->exited = true;
  }
  st->cv.notify_all();
}

void Worker::RunTask(Task &t) noexcept {
  abi::ClearLastError();
  const int32_t result = api::Guard("engine worker", [&]() -> int32_t { return t.job(); });
  LateHandler late;
  {
    std::lock_guard<std::mutex> lock(t.mu);
    t.result = result;
    if (result < 0) {
      try {
        t.detail.assign(abi::LastErrorText());
      } catch (...) {
        t.detail.clear(); // out of memory: the caller gets the status's name
      }
    }
    t.done = true;
    if (t.abandoned) {
      late = std::move(t.late);
    }
  }
  t.cv.notify_all();
  if (late) {
    api::Guard("engine worker (late)", [&] { late(result); });
  }
  api::Guard("engine worker (release)", [&] {
    t.job = nullptr; // the job's captures are released here, on the worker
    t.late = nullptr;
  });
}

} // namespace im::engine
