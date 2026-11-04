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
  LoopbackIo *io;
  std::vector<float> stereo;
  int32_t hr = 0;
};

void ReadInto(void *ctx) {
  auto *c = static_cast<ProbeCapture *>(ctx);
  if (c->hr == 0) {
    c->hr = c->io->ReadCapture(c->stereo);
  }
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
  // Step 1: the INCLUDE stream on the DLL's own process. An activation failure or E_NOTIMPL: unsupported.
  switch (io.OpenProcessCapture(io.SelfPid(), &r.format)) {
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
  ProbeCapture cap{&io, {}, 0};
  if (PlayTone(*render, ProbeTone(), kProbeLeadFrames, kProbeTailFrames, abort, &ReadInto, &cap) != IM_OK) {
    abi::ClearLastError();
    return done(ProbeOutcome::kSkipped); // the render client failed under us (or a stop): nothing was decided
  }
  for (uint32_t waited = 0; waited < kDrainMs && cap.hr == 0; waited += 10) {
    io.Wait(10);
    ReadInto(&cap);
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

bool ProbeRunner::Start() {
  Reap();
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

void ProbeRunner::Stop() noexcept {
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
