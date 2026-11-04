// Which capture engine a live share may use (docs/m2/05-windows-audio.md §6.1, §6.5, §7.5, §11.2, §15; "05" below):
// the Windows 10 build gate, the startup probe's result and its cache, the re-probe after a default-device change, and
// "process loopback stops working twice in 10 s". The watcher asks it for PlanInput::process_loopback and for the
// report's probe and state; the planner then picks the endpoint engine or a process-loopback one (include set or the
// fast path). Also the watcher's timing rules for the fast path (05 §6.4, §7.3).
//
// Portable C++ (no OS header), owned by the watcher thread, not thread-safe. Tests drive it on simulated time
// (native/windows/tests/audio_engine_control_test.cpp).
#pragma once

#include <cstdint>
#include <deque>
#include <optional>
#include <string>
#include <unordered_map>

#include "im/audiomix/report.h"

namespace im::audio {

// Builds below this plan the endpoint engine whatever the probe says, with the banner exclusion-unavailable (05 §6.1,
// README §3 item 52: Windows 10 takes the plan's gate). Lowering it is an owner's decision.
inline constexpr uint32_t kMinIncludeSetBuild = 22000;

// The session events' debounce (05 §7.3), skipped while the fast path runs so its exit's cut lands inside the 40 ms
// preroll (05 §6.4).
inline constexpr int64_t kSessionDebounceNs = 20'000'000;
// The fast path's retroactive cut: the EXCLUDE stream's ring is cut at C - 20 ms, C being the QPC time of the event
// that revealed the kept-out process (05 §6.4).
inline constexpr int64_t kFastPathCutLeadNs = 20'000'000;

// When a reconcile for session events that started at first_ns is due: at once while the fast path runs, else after
// the 20 ms debounce. first_ns 0 (a count without its time yet): at once.
constexpr int64_t SessionPlanDue(int64_t first_ns, bool fast_path) noexcept {
  return first_ns == 0 || fast_path ? first_ns : first_ns + kSessionDebounceNs;
}

// The startup probe's outcome (05 §11.2 steps 4-5).
enum class ProbeOutcome : uint8_t {
  kOk,          // level in [-66, -54] dBFS (or between -80 and -66 / above -54: ok with probe_level_mismatch)
  kFailed,      // below -80 dBFS: silent loopback (or IM_AUDIOF_TEST_FAIL_PROBE)
  kUnsupported, // the INCLUDE activation failed or E_NOTIMPL
  kSkipped,     // no render endpoint, or its render client can't initialize: keep the mode, re-probe at a device event
};

// One probe run as the runner reports it (startup_probe.h).
struct ProbeReport {
  ProbeOutcome outcome = ProbeOutcome::kSkipped;
  bool level_mismatch = false; // ok with the warning probe_level_mismatch
  double level_db = -120.0;
  uint32_t ms = 0;
  std::string format;      // the capture format that initialized ("float32", "pcm16")
  std::string endpoint_id; // the default render endpoint it ran on (the cache key)
};

inline constexpr int64_t kReprobeAfterDefaultChangeNs = 2'000'000'000; // 05 §7.5
inline constexpr int64_t kLoopbackFailureWindowNs = 10'000'000'000;    // twice in 10 s → endpoint (05 §6.5)
inline constexpr int64_t kDeviceSettleNs = 3'000'000'000; // after a default-device change, stream failures are the
                                                          // change's (05 §7.5: the streams stay and are watched 3 s)

class EngineGate {
public:
  // os_build: im_caps.os_build (RtlGetVersion), 0 when unknown (not gated).
  explicit EngineGate(uint32_t os_build) noexcept : os_build_(os_build) {}

  // A share's audio starts (Mixer::Start) on the default render endpoint endpoint_id ("" when there is none).
  // force_fail: IM_AUDIOF_TEST_FAIL_PROBE (the probe's result is failed without running it). Returns whether the
  // startup probe must run now: not on a gated build, not with force_fail, not when the endpoint's cached result is ok
  // (a failed one is retried once per audio start, 05 §11.2).
  bool StartAudio(int64_t now_ns, const std::string &endpoint_id, bool force_fail);
  // The share's audio stopped. The cache stays (process lifetime, 05 §11.2).
  void StopAudio() noexcept;
  // The probe started, or its result came back.
  void ProbeStarted() noexcept;
  void ProbeDone(const ProbeReport &r);

  // A default render device change to endpoint_id: the result is invalidated (the cache is per endpoint), process
  // loopback streams get kDeviceSettleNs, and a live share in the endpoint engine re-probes once 2 s later (05 §7.5).
  void DefaultDeviceChanged(int64_t now_ns, const std::string &endpoint_id);
  // Any device event: a probe that was skipped (no render endpoint, device busy) runs again (05 §11.2 step 5).
  void DeviceEvent(int64_t now_ns) noexcept;
  // A process-loopback stream failed (one call per watcher pump that saw failures). The second within 10 s (outside
  // the settle time of a device change) switches the share to the endpoint engine until a re-probe passes (05 §6.5).
  void LoopbackFailed(int64_t now_ns);

  // Whether a probe should start now (a re-probe or a device event's); the caller then calls ProbeStarted.
  bool ProbeDue(int64_t now_ns) const noexcept;
  // The next time ProbeDue may turn true, or INT64_MAX.
  int64_t NextDeadline() const noexcept;

  // PlanInput::process_loopback (05 §6.1): false on a gated build, after a failed or unsupported probe and after
  // "twice in 10 s"; true while the probe runs (the include set never leaks) and after ok or skipped.
  bool process_loopback() const noexcept;
  bool build_gated() const noexcept { return os_build_ != 0 && os_build_ < kMinIncludeSetBuild; }
  bool live() const noexcept { return live_; }
  bool probing() const noexcept { return probing_; }

  // The report's probe (05 §12.1): pending while it runs (or before any share ran: skipped without a render
  // endpoint). A gated build reports unsupported: the include set is not used on it, so Go shows
  // exclusion-unavailable (05 §14.7).
  audiomix::ReportProbe Report(bool render_present) const;
  // The report's state while live with a render endpoint (05 §12.1): starting while the probe runs, degraded when
  // process loopback can't be used, else ok.
  audiomix::ReportState LiveState() const noexcept;

private:
  const uint32_t os_build_;
  bool live_ = false;
  bool probing_ = false;
  bool force_fail_ = false;
  std::string endpoint_; // the current default render endpoint
  std::optional<ProbeReport> result_;
  bool broken_ = false;  // twice in 10 s
  bool skipped_ = false; // the last probe was skipped (or there was no render endpoint): a device event probes
  std::unordered_map<std::string, ProbeReport> cache_;
  int64_t reprobe_at_ = 0; // 0: none
  int64_t settle_until_ = 0;
  std::deque<int64_t> failures_;
};

} // namespace im::audio
