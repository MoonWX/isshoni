#include "audio/startup_probe.h"

#include <algorithm>
#include <chrono>
#include <system_error>
#include <utility>
#include <vector>

#include "im/abi/status.h"
#include "im/log/ring.h"

namespace im::audio {
namespace {

constexpr uint32_t kDrainMs = 40; // what the capture still delivers after the last frame played

struct ProbeCapture {
  ProbeCapture(LoopbackIo *i, const std::atomic<bool> *a, int64_t deadline) : io(i), abort(a), deadline_ns(deadline) {}

  LoopbackIo *io;
  const std::atomic<bool> *abort;
  int64_t deadline_ns;
  std::atomic<bool> stop{false}; // PlayTone's abort: the caller's abort or the deadline
  std::vector<float> stereo;
  int32_t hr = 0;

  bool Expired() {
    if ((abort != nullptr && abort->load(std::memory_order_acquire)) || io->NowNs() >= deadline_ns) {
      stop.store(true, std::memory_order_release);
    }
    return stop.load(std::memory_order_acquire);
  }
};

void ReadInto(void *ctx) {
  auto *c = static_cast<ProbeCapture *>(ctx);
  if (c->hr == 0) {
    c->hr = c->io->ReadCapture(c->stereo);
  }
  c->Expired();
}

} // namespace

ProbeReport RunStartupProbe(LoopbackIo &io, const std::atomic<bool> *abort) {
  ProbeReport r;
  const int64_t t0 = io.NowNs();
  const auto done = [&](ProbeOutcome o) {
    r.outcome = o;
    r.ms = static_cast<uint32_t>(std::max<int64_t>(0, io.NowNs() - t0) / 1'000'000);
    io.CloseCapture();
    io.CloseRender();
    return r;
  };
  // Step 2's render client first: without one nothing can play or be captured right now (step 5: skipped).
  RenderTarget *render = nullptr;
  if (io.OpenRender(&render, &r.endpoint_id) != OpenOutcome::kOk || render == nullptr) {
    return done(ProbeOutcome::kSkipped);
  }
  ProbeCapture cap{&io, abort, t0 + int64_t{kProbeDeadlineMs} * 1'000'000};
  // Step 1: the INCLUDE stream on the DLL's own process. An activation failure or E_NOTIMPL: unsupported (but one cut
  // short by a stop decides nothing: skipped).
  const OpenOutcome opened = io.OpenProcessCapture(io.SelfPid(), &r.format);
  if (opened != OpenOutcome::kOk && abort != nullptr && abort->load(std::memory_order_acquire)) {
    return done(ProbeOutcome::kSkipped);
  }
  switch (opened) {
  case OpenOutcome::kOk:
    break;
  case OpenOutcome::kNoDevice:
  case OpenOutcome::kBusy:
    return done(ProbeOutcome::kSkipped);
  case OpenOutcome::kUnsupported:
  case OpenOutcome::kFailed:
    return done(ProbeOutcome::kUnsupported);
  }
  // Step 3: the tone, reading the capture between the writes.
  if (cap.Expired() ||
      PlayTone(*render, ProbeTone(), kProbeLeadFrames, kProbeTailFrames, &cap.stop, &ReadInto, &cap) != IM_OK) {
    abi::ClearLastError();
    return done(ProbeOutcome::kSkipped); // the render client failed under us, a stop or the deadline: nothing decided
  }
  for (uint32_t waited = 0; waited < kDrainMs && cap.hr == 0; waited += 10) {
    io.Wait(10);
    ReadInto(&cap);
    if (cap.stop.load(std::memory_order_acquire)) {
      return done(ProbeOutcome::kSkipped);
    }
  }
  if (cap.hr != 0) {
    return done(ProbeOutcome::kUnsupported); // the capture stream failed: process loopback doesn't work here
  }
  // Step 4: Goertzel (Hann) over the tone's middle, wherever it landed in the capture.
  std::vector<float> mono;
  AppendMono(cap.stereo.data(), cap.stereo.size() / 2, mono);
  r.level_db =
      MaxWindowLevelDb(mono.data(), mono.size(), kSignalRate, ProbeTone().hz, kProbeWindowFrames, kProbeHopFrames);
  if (r.level_db < -120.0) {
    r.level_db = -120.0; // the report's floor
  }
  switch (ClassifyProbeLevel(r.level_db)) {
  case ProbeLevel::kOk:
    return done(ProbeOutcome::kOk);
  case ProbeLevel::kMismatch:
    r.level_mismatch = true;
    return done(ProbeOutcome::kOk);
  case ProbeLevel::kSilent:
    break;
  }
  return done(ProbeOutcome::kFailed);
}

ProbeRunner::ProbeRunner(std::shared_ptr<ProbeBackend> backend, std::function<void()> wake)
    : backend_(std::move(backend)), wake_(std::move(wake)) {}

ProbeRunner::~ProbeRunner() { Stop(); }

void ProbeRunner::Reap() {
  if (!thread_.joinable() || !sh_) {
    return;
  }
  bool done = false;
  {
    std::lock_guard<std::mutex> l(sh_->mu);
    done = sh_->done;
  }
  if (done) {
    thread_.join();
  }
}

void ProbeRunner::ReapAborted(bool wait) noexcept {
  const auto deadline = std::chrono::steady_clock::now() + std::chrono::seconds(2);
  for (auto it = aborted_.begin(); it != aborted_.end();) {
    bool done = false;
    {
      std::unique_lock<std::mutex> l(it->sh->mu);
      done = wait ? it->sh->cv.wait_until(l, deadline, [&] { return it->sh->done; }) : it->sh->done;
    }
    try {
      if (done) {
        it->thread.join();
      } else if (wait) {
        it->thread.detach(); // stuck in a driver call: abandoned, never killed (05 §10.5)
        log::Error("audio", "audio probe thread abandoned after 2 s");
      } else {
        ++it;
        continue;
      }
    } catch (const std::system_error &) {
      log::Error("audio", "audio probe thread: join failed");
    }
    it = aborted_.erase(it);
  }
}

bool ProbeRunner::Start() {
  Reap();
  ReapAborted(false);
  if (thread_.joinable() || !backend_) {
    return false;
  }
  auto sh = std::make_shared<Shared>();
  try {
    thread_ = std::thread([sh, b = backend_, wake = wake_] {
      ProbeReport r = b->Run(sh->abort);
      {
        std::lock_guard<std::mutex> l(sh->mu);
        sh->result = std::move(r);
        sh->done = true;
      }
      sh->cv.notify_all();
      if (wake) {
        wake();
      }
    });
  } catch (const std::system_error &) {
    log::Error("audio", "audio probe: the thread did not start");
    return false;
  }
  sh_ = std::move(sh);
  return true;
}

bool ProbeRunner::TakeResult(ProbeReport *out) {
  if (!sh_) {
    return false;
  }
  bool got = false;
  {
    std::lock_guard<std::mutex> l(sh_->mu);
    if (sh_->result) {
      *out = std::move(*sh_->result);
      sh_->result.reset();
      got = true;
    }
  }
  Reap();
  return got;
}

void ProbeRunner::Abort() noexcept {
  if (!thread_.joinable() || !sh_) {
    return;
  }
  sh_->abort.store(true, std::memory_order_release);
  try {
    aborted_.reserve(aborted_.size() + 1);
  } catch (...) {
    Stop(); // out of memory for the list: wait for it after all (abort makes it end within about 10 ms)
    return;
  }
  aborted_.push_back(Aborted{std::move(sh_), std::move(thread_)}); // no reallocation: doesn't throw
  sh_.reset();
  ReapAborted(false);
}

void ProbeRunner::Stop() noexcept {
  ReapAborted(true);
  if (!thread_.joinable() || !sh_) {
    return;
  }
  sh_->abort.store(true, std::memory_order_release);
  bool done = false;
  {
    std::unique_lock<std::mutex> l(sh_->mu);
    done = sh_->cv.wait_for(l, std::chrono::seconds(2), [&] { return sh_->done; });
  }
  try {
    if (done) {
      thread_.join();
    } else {
      thread_.detach(); // stuck in a driver call: abandoned, never killed; it holds its own state (05 §10.5)
      log::Error("audio", "audio probe thread abandoned after 2 s");
    }
  } catch (const std::system_error &) {
    log::Error("audio", "audio probe thread: join failed");
  }
  sh_.reset();
}

bool ProbeRunner::running() const {
  if (!thread_.joinable() || !sh_) {
    return false;
  }
  std::lock_guard<std::mutex> l(sh_->mu);
  return !sh_->done;
}

} // namespace im::audio
