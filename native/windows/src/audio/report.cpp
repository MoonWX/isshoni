#include "audio/report.h"

#include <utility>

namespace im::audio {

audiomix::ReportEngine ToReport(audiopolicy::Engine e) noexcept {
  switch (e) {
  case audiopolicy::Engine::kIncludeSet:
    return audiomix::ReportEngine::kIncludeSet;
  case audiopolicy::Engine::kExcludeSelf:
    return audiomix::ReportEngine::kExcludeSelf;
  case audiopolicy::Engine::kEndpoint:
    return audiomix::ReportEngine::kEndpoint;
  case audiopolicy::Engine::kNone:
    break;
  }
  return audiomix::ReportEngine::kNone;
}

audiomix::AppVerdict ToReport(audiopolicy::Verdict v) noexcept {
  switch (v) {
  case audiopolicy::Verdict::kHeard:
    return audiomix::AppVerdict::kHeard;
  case audiopolicy::Verdict::kKeptOut:
    return audiomix::AppVerdict::kKeptOut;
  case audiopolicy::Verdict::kNotCaptured:
    return audiomix::AppVerdict::kNotCaptured;
  case audiopolicy::Verdict::kPending:
    break;
  }
  return audiomix::AppVerdict::kPending;
}

audiomix::AppReason ToReport(audiopolicy::Reason r) noexcept {
  switch (r) {
  case audiopolicy::Reason::kNone:
    return audiomix::AppReason::kNone;
  case audiopolicy::Reason::kBuiltin:
    return audiomix::AppReason::kBuiltin;
  case audiopolicy::Reason::kUser:
    return audiomix::AppReason::kUser;
  case audiopolicy::Reason::kMic:
    return audiomix::AppReason::kMic;
  case audiopolicy::Reason::kViewer:
    return audiomix::AppReason::kViewer;
  case audiopolicy::Reason::kIsshoni:
    return audiomix::AppReason::kIsshoni;
  case audiopolicy::Reason::kLauncher:
    return audiomix::AppReason::kLauncher;
  case audiopolicy::Reason::kTree:
    return audiomix::AppReason::kTree;
  case audiopolicy::Reason::kPidReuse:
    return audiomix::AppReason::kPidReuse;
  case audiopolicy::Reason::kLimit:
    return audiomix::AppReason::kLimit;
  case audiopolicy::Reason::kCaptureFailed:
    return audiomix::AppReason::kCaptureFailed;
  }
  return audiomix::AppReason::kNone;
}

audiomix::SessionNote ToReport(audiopolicy::Note n) noexcept {
  switch (n) {
  case audiopolicy::Note::kCovered:
    return audiomix::SessionNote::kCovered;
  case audiopolicy::Note::kNoHandle:
    return audiomix::SessionNote::kNoHandle;
  case audiopolicy::Note::kNone:
    break;
  }
  return audiomix::SessionNote::kNone;
}

audiomix::SessionFlow ToReport(audiopolicy::Flow f) noexcept {
  return f == audiopolicy::Flow::kCapture ? audiomix::SessionFlow::kCapture : audiomix::SessionFlow::kRender;
}

audiomix::SessionState ToReport(audiopolicy::SessionState s) noexcept {
  return s == audiopolicy::SessionState::kActive ? audiomix::SessionState::kActive : audiomix::SessionState::kInactive;
}

std::vector<audiomix::AppWarning> WarningsOf(uint32_t bits) {
  std::vector<audiomix::AppWarning> out;
  if ((bits & audiopolicy::kWarnSharedAppUsesMic) != 0) {
    out.push_back(audiomix::AppWarning::kSharedAppUsesMic);
  }
  if ((bits & audiopolicy::kWarnSharedAppKeptOut) != 0) {
    out.push_back(audiomix::AppWarning::kSharedAppKeptOut);
  }
  if ((bits & audiopolicy::kWarnIncludedUsesMic) != 0) {
    out.push_back(audiomix::AppWarning::kIncludedUsesMic);
  }
  if ((bits & audiopolicy::kWarnMultiProcessSession) != 0) {
    out.push_back(audiomix::AppWarning::kMultiProcessSession);
  }
  return out;
}

audiomix::ReportApp ToReport(const audiopolicy::AppRow &row) {
  audiomix::ReportApp a;
  a.id = row.id;
  a.name = row.name;
  a.exe = row.exe;
  a.verdict = ToReport(row.verdict);
  a.reason = ToReport(row.reason);
  a.playing = row.playing;
  a.uses_mic = row.uses_mic;
  a.browser = row.browser;
  a.shared = row.shared;
  a.pids = row.pids;
  a.sessions = row.sessions;
  a.streams = row.streams;
  a.detail = row.detail;
  a.warnings = WarningsOf(row.warnings);
  return a;
}

bool ToReport(const audiopolicy::PlanWarning &w, audiomix::ReportWarning *out) {
  if (w.code == "multi_process_session") {
    out->code = audiomix::AppWarning::kMultiProcessSession;
  } else if (w.code == "instance_gone") {
    out->code = audiomix::AppWarning::kInstanceGone;
  } else {
    return false;
  }
  out->app = w.app;
  return true;
}

std::string VisibleState(const Report &r) {
  // The whole report through the one writer, without what seq doesn't cover.
  Report v = r;
  v.seq = 0;
  v.qpc_ns = 0;
  v.counters = {};
  v.events.clear();
  return audiomix::WriteReportJson(v);
}

} // namespace im::audio
