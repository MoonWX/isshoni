// The activator threads (docs/m2/05-windows-audio.md §8.1; "05" below): two threads that open capture streams, at
// most kMaxActivationsInFlight requests queued or running, each activation waited for at most kActivationTimeout, and
// a stop that aborts every pending wait (S1). The result of each request goes back to the watcher, which runs the
// commit check (05 §6.2 step 5) before the stream joins the mixer (include_set.h).
//
// Portable C++ (no OS header): the OS work is a StreamOpener's (wasapi_capture.h: ActivateAudioInterfaceAsync, the
// formats of 05 §8.2), which waits for each asynchronous activation through ActivationContext::Wait, so the timeout
// and the abort are this file's and its tests run with a fake opener (native/windows/tests/audio_activator_test.cpp).
#pragma once

#include <chrono>
#include <condition_variable>
#include <cstdint>
#include <memory>
#include <mutex>
#include <string>
#include <thread>
#include <vector>

#include "audio/capture_client.h"
#include "im/audiopolicy/types.h"
#include "isshoni_media.h"

namespace im::audio {

inline constexpr std::chrono::milliseconds kActivationTimeout{3000}; // 05 §8.1 (S1: 5 s; observed: milliseconds)
inline constexpr uint32_t kActivatorThreads = 2;
inline constexpr uint32_t kMaxActivationsInFlight = 8;
// How long Stop waits for an activator thread stuck in a driver call before it abandons it (05 §10.5).
inline constexpr std::chrono::milliseconds kActivatorJoin{2000};

namespace detail {
struct ActivatorShared; // the pool's state, shared with its threads (activator.cpp)
}

// One asynchronous activation's completion. The activation's completion handler calls Complete from any thread, also
// after the activator gave up on it (a timeout or a stop): the handler holds its own reference.
class Completion {
public:
  void Complete() noexcept;
  bool done() const noexcept;
  // Waits at most timeout for Complete (outside an activator: the probe's one activation). True when done.
  bool WaitFor(std::chrono::milliseconds timeout) const;

private:
  friend class ActivationContext;
  friend struct detail::ActivatorShared;
  mutable std::mutex mu_;
  mutable std::condition_variable cv_;
  bool done_ = false;
  bool aborted_ = false; // the pool stopped or the request was cancelled: wake the waiter
};

// What ActivationContext::Wait saw.
enum class WaitResult : uint8_t { kDone, kTimeout, kAborted };

// A stream to open: an include stream on a target's tree (README WA45), later also the fast path's EXCLUDE stream and
// the endpoint stream (README WA53).
struct OpenRequest {
  uint64_t id = 0; // the caller's (include_set.h: the stream's id)
  audiopolicy::StreamKind kind = audiopolicy::StreamKind::kInclude;
  uint32_t pid = 0; // the target (kInclude, kExclude)
};

// The outcome of one request.
struct OpenResult {
  uint64_t id = 0;
  int32_t hr = 0;         // HRESULT: 0 (S_OK) with a client, else why not
  bool timed_out = false; // an activation took longer than the pool's timeout
  bool aborted = false;   // the pool stopped (or the request was cancelled) first
  std::string format;     // "float32" or "pcm16", the format Initialize took (05 §8.2, the report's sessions[].format)
  uint32_t failed_attempts = 0; // Initialize attempts that failed before the one that worked (05 §8.2)
  int32_t last_failure = 0;     // the HRESULT of the last of them, 0 without one
  std::unique_ptr<CaptureClient> client;
};

// Given to StreamOpener::Open on the activator thread.
class ActivationContext {
public:
  // Waits for c (whose handler completes it) at most the pool's timeout, or until the pool stops or the request is
  // cancelled.
  WaitResult Wait(const std::shared_ptr<Completion> &c);
  // The pool is stopping or the request was cancelled: Open should give up between its steps.
  bool aborted() const noexcept;

private:
  friend struct detail::ActivatorShared;
  ActivationContext(detail::ActivatorShared *sh, uint32_t slot) : sh_(sh), slot_(slot) {}
  detail::ActivatorShared *sh_;
  uint32_t slot_;
};

// Opens one capture stream (wasapi_capture.h; fakes in tests). Called on an activator thread (COM MTA on Windows,
// through ActivatorOptions::thread_hook), one request at a time per thread.
class StreamOpener {
public:
  virtual ~StreamOpener() = default;
  virtual OpenResult Open(const OpenRequest &req, ActivationContext &ctx) = 0;
};

struct ActivatorOptions {
  uint32_t threads = kActivatorThreads;
  std::chrono::milliseconds timeout = kActivationTimeout;
  // Called first (starting = true) and last on each activator thread: COM's MTA and the thread's name on Windows.
  void (*thread_hook)(void *ctx, bool starting) = nullptr;
  void *hook_ctx = nullptr;
  // Called on an activator thread after a result was queued (the watcher's wake-up). Must not block. result_ctx must
  // stay valid while a thread may run (an abandoned one included): the watcher passes its Signals, which outlive it.
  void (*on_result)(void *ctx) = nullptr;
  void *result_ctx = nullptr;
};

class ActivatorPool {
public:
  // An abandoned thread (Stop) keeps its own reference to the opener and the pool's state.
  explicit ActivatorPool(std::shared_ptr<StreamOpener> opener, ActivatorOptions options = {});
  ~ActivatorPool(); // Stop
  ActivatorPool(const ActivatorPool &) = delete;
  ActivatorPool &operator=(const ActivatorPool &) = delete;

  // Starts the threads (idempotent). IM_ERR_INTERNAL when a thread can't start.
  im_status Start();
  // Aborts every pending wait (their results come back aborted), drops the queue and joins the threads within
  // kActivatorJoin; a thread stuck in a driver call is abandoned with a log record, never killed (05 §10.5). Start
  // may follow.
  void Stop() noexcept;
  bool running() const noexcept { return !threads_.empty(); }

  // Queues a request. False when kMaxActivationsInFlight requests are queued or running, or the pool doesn't run.
  bool Submit(const OpenRequest &req);
  // Drops a queued request (no result), or aborts a running one's waits (its result comes back aborted).
  void Cancel(uint64_t id);
  // Requests queued or running.
  uint32_t in_flight() const;
  // The results so far, oldest first.
  std::vector<OpenResult> TakeResults();

private:
  std::shared_ptr<detail::ActivatorShared> sh_;
  std::vector<std::thread> threads_;
};

} // namespace im::audio
