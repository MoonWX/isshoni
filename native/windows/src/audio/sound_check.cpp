#include "audio/sound_check.h"

#include <algorithm>
#include <cmath>
#include <vector>

#include "im/abi/status.h"
#include "im/audiomix/report.h"
#include "isshoni_media.h"

namespace im::audio {
namespace {

constexpr uint32_t kStepMs = 10;       // capture reads
constexpr uint32_t kAnalyzeEvery = 10; // steps between analyses (100 ms)

// The capture so far and the best level of each partial over the windows analyzed.
struct Analysis {
  LoopbackIo *io = nullptr;
  uint32_t pid = 0;
  bool track_session = false;
  std::vector<float> stereo;
  std::vector<float> mono;
  std::size_t next = 0; // the first window start not analyzed yet
  double best1 = kSignalFloorDb;
  double best2 = kSignalFloorDb;
  int32_t hr = 0;
  bool session_seen = false;
  uint32_t steps = 0;

  bool Passed() const { return best1 >= kSoundCheckPassDb && best2 >= kSoundCheckPassDb; }

  void Read() {
    if (hr != 0) {
      return;
    }
    const std::size_t before = stereo.size();
    hr = io->ReadCapture(stereo);
    AppendMono(stereo.data() + before, (stereo.size() - before) / 2, mono);
    stereo.clear(); // only the mono mix is kept
    if (++steps % kAnalyzeEvery == 0) {
      Analyze();
    }
  }

  void Analyze() {
    const Tone chime = Chime();
    for (; next + kSoundCheckWindowFrames <= mono.size(); next += kSoundCheckHopFrames) {
      best1 = std::max(best1, ToneLevelDb(mono.data() + next, kSoundCheckWindowFrames, kSignalRate, chime.hz));
      best2 = std::max(best2, ToneLevelDb(mono.data() + next, kSoundCheckWindowFrames, kSignalRate, chime.hz2));
    }
    if (track_session && !session_seen) {
      session_seen = io->SessionSeen(pid);
    }
  }
};

void ReadStep(void *ctx) { static_cast<Analysis *>(ctx)->Read(); }

} // namespace

SoundCheckResult RunSoundCheck(LoopbackIo &io, const SoundCheckParams &p, const std::atomic<bool> *abort) {
  SoundCheckResult r;
  const int64_t t0 = io.NowNs();
  const int64_t deadline = t0 + int64_t{p.timeout_ms} * 1'000'000;
  const uint32_t pid = p.helper_pid != 0 ? p.helper_pid : io.SelfPid();
  const auto fail = [&](const char *reason) {
    r.captured = false;
    r.reason = reason;
    io.CloseCapture();
    io.CloseRender();
    return r;
  };
  const auto failed_open = [&](OpenOutcome o) {
    switch (o) {
    case OpenOutcome::kNoDevice:
      return fail("no_device");
    case OpenOutcome::kBusy:
      return fail("device_busy");
    default:
      return fail("activation_failed");
    }
  };

  // Step 3: the capture path. INCLUDE on the helper's (or the DLL's own) process when process loopback works; the
  // endpoint's loopback when it doesn't, or when the INCLUDE activation says unsupported.
  std::string format;
  bool endpoint = (p.audio_flags & IM_AUDIO_PROCESS_LOOPBACK) == 0;
  if (!endpoint) {
    r.backend = "process-loopback";
    const OpenOutcome o = io.OpenProcessCapture(pid, &format);
    if (o == OpenOutcome::kUnsupported && (p.audio_flags & IM_AUDIO_ENDPOINT_LOOPBACK) != 0) {
      endpoint = true;
    } else if (o != OpenOutcome::kOk) {
      return failed_open(o);
    }
  }
  if (endpoint) {
    if ((p.audio_flags & IM_AUDIO_ENDPOINT_LOOPBACK) == 0) {
      r.backend = "none"; // neither loopback works: no active render endpoint or no audio service (05 §11.1)
      return fail("no_device");
    }
    r.backend = "endpoint";
    if (const OpenOutcome o = io.OpenEndpointCapture(&format); o != OpenOutcome::kOk) {
      return failed_open(o);
    }
  }

  Analysis a;
  a.io = &io;
  a.pid = pid;
  a.track_session = p.helper_pid != 0;

  // helper_pid 0: the DLL's own chime from a private render session (step 1), read and analyzed while it plays.
  if (p.helper_pid == 0) {
    RenderTarget *render = nullptr;
    std::string endpoint_id;
    if (const OpenOutcome o = io.OpenRender(&render, &endpoint_id); o != OpenOutcome::kOk || render == nullptr) {
      return failed_open(o == OpenOutcome::kOk ? OpenOutcome::kNoDevice : o);
    }
    if (PlayTone(*render, Chime(), kSoundCheckChimeLeadFrames, 0, abort, &ReadStep, &a) != IM_OK) {
      abi::ClearLastError(); // a render failure mid-way: whatever was captured decides below
    }
    io.CloseRender();
  }

  // Step 4: both partials at -40 dBFS or more within the timeout.
  while (!a.Passed() && a.hr == 0 && io.NowNs() < deadline &&
         (abort == nullptr || !abort->load(std::memory_order_acquire))) {
    io.Wait(kStepMs);
    a.Read();
  }
  a.Analyze();
  io.CloseCapture();
  if (a.Passed()) {
    r.captured = true;
    r.level_db = std::min(a.best1, a.best2);
    r.ms = static_cast<uint32_t>(std::max<int64_t>(0, io.NowNs() - t0) / 1'000'000);
    return r;
  }
  if (a.hr != 0) {
    return fail("activation_failed"); // the stream failed under the check
  }
  // Step 6: "timeout" when the helper's session never appeared, else nothing came through: "silent".
  return fail(a.track_session && !a.session_seen ? "timeout" : "silent");
}

std::string SoundCheckJson(const SoundCheckResult &r) {
  std::string out = "{\"captured\":";
  if (r.captured) {
    out += "true,\"levelDb\":";
    audiomix::AppendGoFloat(out, std::round(r.level_db * 10.0) / 10.0);
    out += ",\"ms\":" + std::to_string(r.ms);
  } else {
    out += "false,\"reason\":\"" + r.reason + "\"";
  }
  out += ",\"backend\":\"" + r.backend + "\"}";
  return out;
}

} // namespace im::audio
