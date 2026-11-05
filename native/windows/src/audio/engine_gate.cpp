#include "audio/engine_gate.h"

#include <climits>

namespace im::audio {

bool EngineGate::StartAudio(int64_t now_ns, const std::string &endpoint_id, bool force_fail) {
  (void)now_ns;
  live_ = true;
  probing_ = false;
  force_fail_ = force_fail;
  endpoint_ = endpoint_id;
  broken_ = false;
  skipped_ = false;
  failures_.clear();
  reprobe_at_ = 0;
  settle_until_ = 0;
  result_.reset();
  if (build_gated()) {
    return false; // the endpoint engine whatever a probe would say (05 §6.1)
  }
  if (force_fail) {
    ProbeReport r;
    r.outcome = ProbeOutcome::kFailed; // IM_AUDIOF_TEST_FAIL_PROBE (05 §11.2)
    r.endpoint_id = endpoint_id;
    result_ = r;
    return false;
  }
  if (endpoint_id.empty()) {
    skipped_ = true; // no render endpoint: nothing plays or can be captured; a device event probes (05 §11.2 step 5)
    return false;
  }
  const auto it = cache_.find(endpoint_id);
  if (it != cache_.end() && it->second.outcome == ProbeOutcome::kOk) {
    result_ = it->second; // cached for the process lifetime; a failed result is retried once per audio start
    return false;
  }
  return true; // runs as if it passed meanwhile (05 §6.1: the include set never leaks)
}

void EngineGate::StopAudio() noexcept {
  live_ = false;
  probing_ = false;
  reprobe_at_ = 0;
  failures_.clear();
}

void EngineGate::ProbeStarted() noexcept {
  probing_ = true;
  reprobe_at_ = 0;
}

void EngineGate::ProbeDone(const ProbeReport &r) {
  probing_ = false;
  if (!live_ || force_fail_ || build_gated()) {
    return; // a late result after the share stopped, or one that can't change the engine
  }
  if (r.outcome == ProbeOutcome::kSkipped) {
    skipped_ = true; // the engine keeps its mode; the next device event probes again
    return;
  }
  skipped_ = false;
  result_ = r;
  cache_[r.endpoint_id] = r;
  if (r.outcome == ProbeOutcome::kOk) {
    broken_ = false; // a re-probe that passed lifts "twice in 10 s" too (05 §6.5: endpoint -> process loopback)
    failures_.clear();
  }
}

void EngineGate::DefaultDeviceChanged(int64_t now_ns, const std::string &endpoint_id) {
  endpoint_ = endpoint_id;
  settle_until_ = now_ns + kDeviceSettleNs;
  if (live_ && !build_gated() && !force_fail_ && !endpoint_id.empty() && !process_loopback()) {
    reprobe_at_ = now_ns + kReprobeAfterDefaultChangeNs; // once, 2 s after the change (05 §7.5)
  }
}

void EngineGate::DeviceEvent(int64_t now_ns) noexcept {
  if (live_ && skipped_ && !probing_ && !build_gated() && !force_fail_ && reprobe_at_ == 0) {
    reprobe_at_ = now_ns;
  }
}

void EngineGate::LoopbackFailed(int64_t now_ns) {
  if (!live_ || !process_loopback() || now_ns < settle_until_) {
    return;
  }
  failures_.push_back(now_ns);
  while (!failures_.empty() && failures_.front() <= now_ns - kLoopbackFailureWindowNs) {
    failures_.pop_front();
  }
  if (failures_.size() < 2) {
    return;
  }
  broken_ = true;
  failures_.clear();
  ProbeReport r;
  r.outcome = ProbeOutcome::kFailed;
  r.endpoint_id = endpoint_;
  result_ = r;
  if (!endpoint_.empty()) {
    cache_[endpoint_] = r; // until a re-probe passes
  }
}

bool EngineGate::ProbeDue(int64_t now_ns) const noexcept {
  return live_ && !probing_ && reprobe_at_ != 0 && now_ns >= reprobe_at_;
}

int64_t EngineGate::NextDeadline() const noexcept {
  return live_ && !probing_ && reprobe_at_ != 0 ? reprobe_at_ : INT64_MAX;
}

bool EngineGate::process_loopback() const noexcept {
  if (build_gated() || force_fail_ || broken_) {
    return false;
  }
  return !result_ || result_->outcome == ProbeOutcome::kOk;
}

audiomix::ReportProbe EngineGate::Report(bool render_present) const {
  audiomix::ReportProbe p;
  p.os_build = os_build_;
  if (build_gated()) {
    p.result = audiomix::ProbeResult::kUnsupported;
    return p;
  }
  if (result_) {
    switch (result_->outcome) {
    case ProbeOutcome::kOk:
      p.result = audiomix::ProbeResult::kOk;
      break;
    case ProbeOutcome::kFailed:
      p.result = audiomix::ProbeResult::kFailed;
      break;
    case ProbeOutcome::kUnsupported:
      p.result = audiomix::ProbeResult::kUnsupported;
      break;
    case ProbeOutcome::kSkipped:
      p.result = audiomix::ProbeResult::kSkipped;
      break;
    }
    p.format = result_->format;
    p.level_db = result_->level_db;
    p.ms = result_->ms;
    return p;
  }
  if (probing_) {
    p.result = audiomix::ProbeResult::kPending;
  } else if (skipped_ || !render_present) {
    p.result = audiomix::ProbeResult::kSkipped;
  } else {
    p.result = audiomix::ProbeResult::kPending; // no share has run yet
  }
  return p;
}

audiomix::ReportState EngineGate::LiveState() const noexcept {
  if (!process_loopback()) {
    return audiomix::ReportState::kDegraded;
  }
  if (probing_ && !result_) {
    return audiomix::ReportState::kStarting;
  }
  return audiomix::ReportState::kOk;
}

} // namespace im::audio
