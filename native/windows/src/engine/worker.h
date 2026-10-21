// The engine worker (docs/m2/04-windows-engine.md §6.1): one thread per process, in COM's multithreaded apartment,
// that runs every API job touching COM, WinRT, DXGI or D3D creation, so no such work ever runs on a caller's thread
// (Go hops OS threads; the Wails UI thread is an STA).
//
// An export posts a job and waits at most its §5.3 "Blocks" bound + 1 s. A job that hasn't finished by then makes the
// export return IM_ERR_TIMEOUT with token engine_timeout; the job still runs to its end on the worker, its result is
// discarded, and its late handler (if any) cleans up after it there (a late im_start stops its own session, §6.1).
//
// Jobs own their inputs and outputs: a job never points into the caller's memory, because the caller may have
// returned (timed out) before the job runs. The export copies the input before posting and copies the job's output
// back after a success.
#pragma once

#include <chrono>
#include <condition_variable>
#include <cstdint>
#include <deque>
#include <functional>
#include <memory>
#include <mutex>
#include <string>
#include <string_view>
#include <thread>

#include "isshoni_media.h"

namespace im::engine {

class Worker {
public:
  // A job returns an im_status, or a length for a JSON-producing job. On failure it sets the thread's detail with
  // im::abi::Fail, which Post hands to the caller's thread.
  using Job = std::function<int32_t()>;
  // Runs on the worker after a job finished late, with the job's result.
  using LateHandler = std::function<void(int32_t)>;

  Worker() = default;
  ~Worker(); // Stop with a 2 s bound
  Worker(const Worker &) = delete;
  Worker &operator=(const Worker &) = delete;

  // Starts the thread and waits until it has entered COM's MTA. IM_ERR_BAD_STATE when it already runs,
  // IM_ERR_INTERNAL when the thread can't start or CoInitializeEx fails on it.
  im_status Start();

  // Lets the thread finish the jobs already queued, then waits up to bound for it to exit. A thread that doesn't exit
  // in time (a job stuck in a driver call) is abandoned with an error log, never killed (04 §6.3).
  void Stop(std::chrono::milliseconds bound);

  bool Running() const;

  // Whether the calling thread is this worker's thread.
  bool OnWorkerThread() const;

  // Runs job on the worker thread and returns its result. When the job fails, the detail it left (im_last_error's
  // text) becomes the calling thread's detail too. A job that doesn't finish within bound gives IM_ERR_TIMEOUT
  // (engine_timeout: "<what> did not finish within <bound> ms") and is handed to late when it finishes. A job posted
  // from the worker itself runs inline. Without a running worker: IM_ERR_BAD_STATE (not_initialized).
  int32_t Post(std::string_view what, std::chrono::milliseconds bound, Job job, LateHandler late = {});

private:
  struct Task;
  struct State;

  static void Run(std::shared_ptr<State> st) noexcept;
  static void RunTask(Task &t) noexcept;

  mutable std::mutex mu_; // guards state_ and thread_
  std::shared_ptr<State> state_;
  std::thread thread_;
};

} // namespace im::engine
