#include "im/audiomix/report.h"

#include <charconv>
#include <cmath>
#include <string_view>

#include "im/abi/utf8.h"
#include "im/json/writer.h"

namespace im::audiomix {

const char *ReportName(ReportMode v) noexcept {
  switch (v) {
  case ReportMode::kExcludeApps:
    return "exclude-apps";
  case ReportMode::kOff:
    return "off";
  }
  return "";
}

const char *ReportName(ReportEngine v) noexcept {
  switch (v) {
  case ReportEngine::kIncludeSet:
    return "include-set";
  case ReportEngine::kExcludeSelf:
    return "exclude-self";
  case ReportEngine::kEndpoint:
    return "endpoint";
  case ReportEngine::kNone:
    return "none";
  }
  return "";
}

const char *ReportName(ReportState v) noexcept {
  switch (v) {
  case ReportState::kStarting:
    return "starting";
  case ReportState::kOk:
    return "ok";
  case ReportState::kDegraded:
    return "degraded";
  case ReportState::kNoDevice:
    return "no-device";
  case ReportState::kStopped:
    return "stopped";
  }
  return "";
}

const char *ReportName(ProbeResult v) noexcept {
  switch (v) {
  case ProbeResult::kPending:
    return "pending";
  case ProbeResult::kOk:
    return "ok";
  case ProbeResult::kFailed:
    return "failed";
  case ProbeResult::kUnsupported:
    return "unsupported";
  case ProbeResult::kSkipped:
    return "skipped";
  }
  return "";
}

const char *ReportName(AppVerdict v) noexcept {
  switch (v) {
  case AppVerdict::kHeard:
    return "heard";
  case AppVerdict::kKeptOut:
    return "kept_out";
  case AppVerdict::kNotCaptured:
    return "not_captured";
  case AppVerdict::kPending:
    return "pending";
  }
  return "";
}

const char *ReportName(AppReason v) noexcept {
  switch (v) {
  case AppReason::kNone:
    return "";
  case AppReason::kBuiltin:
    return "builtin";
  case AppReason::kUser:
    return "user";
  case AppReason::kMic:
    return "mic";
  case AppReason::kViewer:
    return "viewer";
  case AppReason::kIsshoni:
    return "isshoni";
  case AppReason::kLauncher:
    return "launcher";
  case AppReason::kTree:
    return "tree";
  case AppReason::kPidReuse:
    return "pid_reuse";
  case AppReason::kLimit:
    return "limit";
  case AppReason::kCaptureFailed:
    return "capture_failed";
  }
  return "";
}

const char *ReportName(AppWarning v) noexcept {
  switch (v) {
  case AppWarning::kSharedAppUsesMic:
    return "shared_app_uses_mic";
  case AppWarning::kSharedAppKeptOut:
    return "shared_app_kept_out";
  case AppWarning::kIncludedUsesMic:
    return "included_uses_mic";
  case AppWarning::kMultiProcessSession:
    return "multi_process_session";
  }
  return "";
}

const char *ReportName(SessionFlow v) noexcept {
  switch (v) {
  case SessionFlow::kRender:
    return "render";
  case SessionFlow::kCapture:
    return "capture";
  }
  return "";
}

const char *ReportName(SessionState v) noexcept {
  switch (v) {
  case SessionState::kInactive:
    return "inactive";
  case SessionState::kActive:
    return "active";
  }
  return "";
}

const char *ReportName(SessionStream v) noexcept {
  switch (v) {
  case SessionStream::kNone:
    return "none";
  case SessionStream::kActivating:
    return "activating";
  case SessionStream::kPreroll:
    return "preroll";
  case SessionStream::kRunning:
    return "running";
  case SessionStream::kFading:
    return "fading";
  }
  return "";
}

const char *ReportName(SessionNote v) noexcept {
  switch (v) {
  case SessionNote::kNone:
    return "";
  case SessionNote::kCovered:
    return "covered";
  case SessionNote::kNoHandle:
    return "no_handle";
  }
  return "";
}

const char *ReportName(ReportEventCode v) noexcept {
  switch (v) {
  case ReportEventCode::kSessionCreated:
    return "session_created";
  case ReportEventCode::kSessionExpired:
    return "session_expired";
  case ReportEventCode::kStreamOpened:
    return "stream_opened";
  case ReportEventCode::kStreamRunning:
    return "stream_running";
  case ReportEventCode::kStreamRemoved:
    return "stream_removed";
  case ReportEventCode::kStreamFailed:
    return "stream_failed";
  case ReportEventCode::kActivationFailed:
    return "activation_failed";
  case ReportEventCode::kFastpathEnter:
    return "fastpath_enter";
  case ReportEventCode::kFastpathExit:
    return "fastpath_exit";
  case ReportEventCode::kFastpathLateCut:
    return "fastpath_late_cut";
  case ReportEventCode::kDefaultDeviceChanged:
    return "default_device_changed";
  case ReportEventCode::kDeviceAdded:
    return "device_added";
  case ReportEventCode::kDeviceRemoved:
    return "device_removed";
  case ReportEventCode::kNoDevice:
    return "no_device";
  case ReportEventCode::kAudiosrvRestart:
    return "audiosrv_restart";
  case ReportEventCode::kProbeOk:
    return "probe_ok";
  case ReportEventCode::kProbeFailed:
    return "probe_failed";
  case ReportEventCode::kMixerResync:
    return "mixer_resync";
  case ReportEventCode::kUnderrun:
    return "underrun";
  case ReportEventCode::kTrim:
    return "trim";
  case ReportEventCode::kSilentReopen:
    return "silent_reopen";
  case ReportEventCode::kRulesChanged:
    return "rules_changed";
  case ReportEventCode::kMuted:
    return "muted";
  case ReportEventCode::kUnmuted:
    return "unmuted";
  case ReportEventCode::kModeChanged:
    return "mode_changed";
  case ReportEventCode::kReanchor:
    return "reanchor";
  }
  return "";
}

void AppendGoFloat(std::string &out, double v) {
  if (!std::isfinite(v)) {
    out += "null";
    return;
  }
  if (v == 0) {
    out += '0';
    return;
  }
  const double a = std::fabs(v);
  const bool exp = a < 1e-6 || a >= 1e21;
  char buf[64];
  const auto r =
      std::to_chars(buf, buf + sizeof buf, v, exp ? std::chars_format::scientific : std::chars_format::fixed);
  if (r.ec != std::errc{}) {
    out += "null";
    return;
  }
  std::string_view s(buf, static_cast<std::size_t>(r.ptr - buf));
  const std::size_t n = s.size();
  if (exp && n >= 4 && s[n - 4] == 'e' && s[n - 3] == '-' && s[n - 2] == '0') {
    out.append(s.data(), n - 2); // "1e-07" -> "1e-7", as encoding/json cleans it up
    out += s[n - 1];
    return;
  }
  out.append(s.data(), n);
}

namespace {

// One decimal place, as the report writes levels and CPU (§12.1: -60.1, 6.2).
double Tenths(double v) noexcept { return std::isfinite(v) ? std::round(v * 10.0) / 10.0 : 0.0; }

class ReportWriter {
public:
  void Str(std::string_view key, std::string_view value) {
    w_.Key(key);
    const std::string clean = abi::SanitizeUtf8(value);
    w_.String(std::string_view(clean).substr(0, abi::Utf8PrefixLen(clean, Report::kMaxString)));
  }
  void Name(std::string_view key, const char *value) { w_.Key(key).String(value); }
  void U(std::string_view key, uint64_t v) { w_.Key(key).Uint(v); }
  void I(std::string_view key, int64_t v) { w_.Key(key).Int(v); }
  void B(std::string_view key, bool v) { w_.Key(key).Bool(v); }
  void F(std::string_view key, double v) {
    std::string s;
    AppendGoFloat(s, Tenths(v));
    w_.Key(key).Raw(s);
  }
  json::Writer &w() { return w_; }

private:
  json::Writer w_;
};

void WriteApp(ReportWriter &o, const ReportApp &a) {
  o.w().BeginObject();
  o.Str("id", a.id);
  o.Str("name", a.name);
  o.Str("exe", a.exe);
  o.Name("verdict", ReportName(a.verdict));
  o.Name("reason", ReportName(a.reason));
  o.B("playing", a.playing);
  o.B("usesMic", a.uses_mic);
  o.B("browser", a.browser);
  o.B("shared", a.shared);
  o.U("pids", a.pids);
  o.U("sessions", a.sessions);
  o.U("streams", a.streams);
  o.Str("detail", a.detail);
  o.w().Key("warnings").BeginArray();
  for (const AppWarning wv : a.warnings) {
    o.w().String(ReportName(wv));
  }
  o.w().EndArray();
  o.w().EndObject();
}

void WriteSession(ReportWriter &o, const ReportSession &s) {
  o.w().BeginObject();
  o.U("pid", s.pid);
  o.Str("created", std::to_string(s.created));
  o.Str("exe", s.exe);
  o.Str("appId", s.app_id);
  o.Name("flow", ReportName(s.flow));
  o.Name("state", ReportName(s.state));
  o.B("multiProcess", s.multi_process);
  o.Name("verdict", ReportName(s.verdict));
  o.Name("reason", ReportName(s.reason));
  o.Name("stream", ReportName(s.stream));
  o.Str("format", s.format);
  o.Name("note", ReportName(s.note));
  o.w().EndObject();
}

} // namespace

std::string WriteReportJson(const Report &r) {
  ReportWriter o;
  json::Writer &w = o.w();
  w.BeginObject();
  o.U("v", Report::kVersion);
  o.U("seq", r.seq);
  o.I("qpcNs", r.qpc_ns);
  o.Name("mode", ReportName(r.mode));
  o.Name("engine", ReportName(r.engine));
  o.Name("state", ReportName(r.state));
  o.B("muted", r.muted);
  o.B("paused", r.paused);
  o.B("live", r.live);
  o.B("rebuilding", r.rebuilding);

  w.Key("probe").BeginObject();
  o.Name("result", ReportName(r.probe.result));
  o.U("osBuild", r.probe.os_build);
  o.Str("format", r.probe.format);
  o.F("levelDb", r.probe.level_db);
  o.U("ms", r.probe.ms);
  w.EndObject();

  w.Key("device").BeginObject();
  o.B("present", r.device.present);
  o.U("renderEndpoints", r.device.render_endpoints);
  o.U("captureEndpoints", r.device.capture_endpoints);
  w.EndObject();

  w.Key("apps").BeginArray();
  for (std::size_t i = 0; i < r.apps.size() && i < Report::kMaxApps; ++i) {
    WriteApp(o, r.apps[i]);
  }
  w.EndArray();

  w.Key("sessions").BeginArray();
  for (std::size_t i = 0; i < r.sessions.size() && i < Report::kMaxSessions; ++i) {
    WriteSession(o, r.sessions[i]);
  }
  w.EndArray();

  w.Key("warnings").BeginArray();
  for (std::size_t i = 0; i < r.warnings.size() && i < Report::kMaxWarnings; ++i) {
    const ReportWarning &x = r.warnings[i];
    w.BeginObject();
    o.Name("code", ReportName(x.code));
    o.Str("app", x.app);
    w.EndObject();
  }
  w.EndArray();

  const ReportCounters &c = r.counters;
  w.Key("counters").BeginObject();
  o.U("streams", c.streams);
  o.U("activations", c.activations);
  o.U("activationFailures", c.activation_failures);
  o.U("underruns", c.underruns);
  o.U("trims", c.trims);
  o.U("overflows", c.overflows);
  o.U("discontinuities", c.discontinuities);
  o.U("mixerResyncs", c.mixer_resyncs);
  o.U("fastpathExits", c.fastpath_exits);
  o.U("fastpathLateCuts", c.fastpath_late_cuts);
  o.U("silentReopens", c.silent_reopens);
  o.U("deviceChanges", c.device_changes);
  o.U("pidReuseBlocks", c.pid_reuse_blocks);
  o.I("maxResamplePpm", c.max_resample_ppm);
  o.F("audioCpuMsPerS", c.audio_cpu_ms_per_s);
  w.EndObject();

  w.Key("events").BeginArray();
  const std::size_t first = r.events.size() > Report::kMaxEvents ? r.events.size() - Report::kMaxEvents : 0;
  for (std::size_t i = first; i < r.events.size(); ++i) {
    const ReportEvent &e = r.events[i];
    w.BeginObject();
    o.I("tNs", e.t_ns);
    o.Name("code", ReportName(e.code));
    o.Str("app", e.app);
    o.Str("detail", e.detail);
    w.EndObject();
  }
  w.EndArray();

  w.EndObject();
  return w.Take();
}

} // namespace im::audiomix
