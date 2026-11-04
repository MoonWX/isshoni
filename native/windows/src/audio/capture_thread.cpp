#include "audio/capture_thread.h"

#include "audio/win_audio.h"

#include <algorithm>
#include <chrono>
#include <condition_variable>
#include <mutex>
#include <system_error>
#include <utility>
#include <vector>

#include "api/guard.h"
#include "engine/os.h"
#include "im/log/ring.h"

namespace im::audio {
namespace {

constexpr DWORD kWaitMs = 20;                        // 05 §8.4
constexpr std::chrono::milliseconds kJoinWait{2000}; // 05 §10.5

} // namespace

struct CaptureThread::Shared {
  Shared() : control(CreateEventW(nullptr, FALSE, FALSE, nullptr)) {}

  win::Handle control; // auto-reset: commands and stop
  std::atomic<bool> stop{false};
  std::atomic<int64_t> busy_ns{0};

  std::mutex mu; // guards what follows
  std::vector<std::shared_ptr<CaptureStream>> adds;
  std::vector<std::shared_ptr<CaptureStream>> removes;
  uint32_t count = 0; // streams the thread has or will have (adds included, removes not)
  bool done = false;
  std::condition_variable done_cv;

  void Run() noexcept {
    const uintptr_t token = engine::os::EnterThread("isshoni audio capture", engine::os::ThreadClass::kProAudio);
    api::Guard("audio capture thread", [&] {
      const win::ComApartment com;
      if (FAILED(com.hr())) {
        log::Error("audio", "audio capture thread without COM", {{"hr", win::Hr(com.hr())}});
      }
      Loop();
    });
    engine::os::LeaveThread(token);
    std::lock_guard<std::mutex> l(mu);
    done = true;
    done_cv.notify_all();
  }

  void Loop() {
    std::vector<std::shared_ptr<CaptureStream>> streams;
    streams.reserve(kMaxCaptureStreams);
    HANDLE handles[MAXIMUM_WAIT_OBJECTS];
    while (!stop.load(std::memory_order_acquire)) {
      Commands(streams);
      DWORD n = 0;
      handles[n++] = control.get();
      for (const auto &s : streams) {
        if (n < MAXIMUM_WAIT_OBJECTS && !s->failed.load(std::memory_order_relaxed) && s->client->event() != nullptr) {
          handles[n++] = static_cast<HANDLE>(s->client->event());
        }
      }
      WaitForMultipleObjects(n, handles, FALSE, kWaitMs);
      if (stop.load(std::memory_order_acquire)) {
        break;
      }
      // Drain every stream with a packet ready (S1's loop): one wake-up often has several.
      const int64_t t0 = engine::os::QpcNs();
      for (const auto &s : streams) {
        if (s->failed.load(std::memory_order_relaxed)) {
          continue;
        }
        const int32_t hr = s->client->Drain(s->ring);
        s->discontinuities.store(s->client->discontinuities(), std::memory_order_relaxed);
        if (hr < 0) {
          s->failure.store(hr, std::memory_order_relaxed);
          s->failed.store(true, std::memory_order_release); // the watcher fades it out and reopens it (05 §8.5)
        }
      }
      busy_ns.fetch_add(engine::os::QpcNs() - t0, std::memory_order_relaxed);
    }
    // Stopping: every client is released here, on the thread that started it.
    Commands(streams);
    for (auto &s : streams) {
      s->client.reset();
    }
    streams.clear();
  }

  void Commands(std::vector<std::shared_ptr<CaptureStream>> &streams) {
    std::vector<std::shared_ptr<CaptureStream>> add, rem;
    {
      std::lock_guard<std::mutex> l(mu);
      add.swap(adds);
      rem.swap(removes);
    }
    for (auto &s : add) {
      if (std::find(rem.begin(), rem.end(), s) != rem.end()) {
        continue; // removed before it started
      }
      const int32_t hr = s->client->Start();
      if (hr < 0) {
        s->failure.store(hr, std::memory_order_relaxed);
        s->failed.store(true, std::memory_order_release);
      }
      streams.push_back(std::move(s));
    }
    for (auto &s : rem) {
      const auto it = std::find(streams.begin(), streams.end(), s);
      if (it != streams.end()) {
        streams.erase(it);
      }
      if (s->client) {
        s->client.reset(); // the stream's COM objects go on this thread (05 §8.4)
      }
    }
  }
};

CaptureThread::CaptureThread() : sh_(std::make_shared<Shared>()) {}

CaptureThread::~CaptureThread() { Stop(); }

im_status CaptureThread::Start() {
  if (thread_.joinable()) {
    return IM_OK;
  }
  if (!sh_->control) {
    return IM_ERR_INTERNAL;
  }
  try {
    thread_ = std::thread([sh = sh_] { sh->Run(); });
  } catch (const std::system_error &) {
    return IM_ERR_INTERNAL;
  }
  return IM_OK;
}

void CaptureThread::Stop() noexcept {
  if (!thread_.joinable()) {
    return;
  }
  sh_->stop.store(true, std::memory_order_release);
  SetEvent(sh_->control.get());
  bool done = false;
  {
    std::unique_lock<std::mutex> l(sh_->mu);
    done = sh_->done_cv.wait_for(l, kJoinWait, [&] { return sh_->done; });
  }
  try {
    if (done) {
      thread_.join();
    } else {
      thread_.detach(); // stuck in a driver call: abandoned, never killed; it holds its own reference to the state
      log::Error("audio", "audio capture thread abandoned after 2 s");
    }
  } catch (const std::system_error &) {
    log::Error("audio", "audio capture thread: join failed");
  }
  sh_ = std::make_shared<Shared>(); // a later Start begins with fresh state
}

bool CaptureThread::Add(std::shared_ptr<CaptureStream> s) {
  if (!s || !s->client || !thread_.joinable()) {
    return false;
  }
  {
    std::lock_guard<std::mutex> l(sh_->mu);
    if (sh_->count >= kMaxCaptureStreams) {
      return false;
    }
    ++sh_->count;
    sh_->adds.push_back(std::move(s));
  }
  SetEvent(sh_->control.get());
  return true;
}

void CaptureThread::Remove(const std::shared_ptr<CaptureStream> &s) {
  if (!s) {
    return;
  }
  {
    std::lock_guard<std::mutex> l(sh_->mu);
    if (sh_->count > 0) {
      --sh_->count;
    }
    sh_->removes.push_back(s);
  }
  SetEvent(sh_->control.get());
}

int64_t CaptureThread::busy_ns() const noexcept { return sh_->busy_ns.load(std::memory_order_relaxed); }

} // namespace im::audio
