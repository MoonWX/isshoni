#include "audio/report.h"

#include <algorithm>
#include <string>
#include <string_view>

#include "im/abi/utf8.h"
#include "im/json/writer.h"

namespace im::audio {
namespace {

// s cut to kReportMaxStringBytes on a UTF-8 boundary (05 §12.1: "strings ≤ 260 bytes").
std::string_view Cap(std::string_view s) noexcept { return s.substr(0, abi::Utf8PrefixLen(s, kReportMaxStringBytes)); }

void Str(json::Writer &w, std::string_view key, std::string_view value) { w.Key(key).String(Cap(value)); }

void Warnings(json::Writer &w, uint32_t bits) {
  w.Key("warnings").BeginArray();
  for (uint32_t bit = 1; bit != 0; bit <<= 1) {
    if ((bits & bit) != 0) {
      if (const std::string_view name = audiopolicy::WarningName(bit); !name.empty()) {
        w.String(name);
      }
    }
  }
  w.EndArray();
}

void App(json::Writer &w, const audiopolicy::AppRow &a) {
  w.BeginObject();
  Str(w, "id", a.id);
  Str(w, "name", a.name);
  Str(w, "exe", a.exe);
  Str(w, "verdict", audiopolicy::VerdictName(a.verdict));
  Str(w, "reason", audiopolicy::ReasonName(a.reason));
  w.Key("playing").Bool(a.playing);
  w.Key("usesMic").Bool(a.uses_mic);
  w.Key("browser").Bool(a.browser);
  w.Key("shared").Bool(a.shared);
  w.Key("pids").Uint(a.pids);
  w.Key("sessions").Uint(a.sessions);
  w.Key("streams").Uint(a.streams);
  Str(w, "detail", a.detail);
  Warnings(w, a.warnings);
  w.EndObject();
}

void Session(json::Writer &w, const ReportSession &s) {
  w.BeginObject();
  w.Key("pid").Uint(s.pid);
  w.Key("created").String(std::to_string(s.created));
  Str(w, "exe", s.exe);
  Str(w, "appId", s.app_id);
  Str(w, "flow", audiopolicy::FlowName(s.flow));
  Str(w, "state", s.state == audiopolicy::SessionState::kActive ? "active" : "inactive");
  w.Key("multiProcess").Bool(s.multi_process);
  Str(w, "verdict", audiopolicy::VerdictName(s.verdict));
  Str(w, "reason", audiopolicy::ReasonName(s.reason));
  Str(w, "stream", s.stream);
  Str(w, "format", s.format);
  Str(w, "note", audiopolicy::NoteName(s.note));
  w.EndObject();
}

// The part seq covers, between "v" and "counters" (05 §12.1). Counters and events follow only in the full report.
void Visible(json::Writer &w, const Report &r) {
  Str(w, "mode", r.mode);
  Str(w, "engine", audiopolicy::EngineName(r.engine));
  Str(w, "state", r.state);
  w.Key("muted").Bool(r.muted);
  w.Key("paused").Bool(r.paused);
  w.Key("live").Bool(r.live);
  w.Key("rebuilding").Bool(r.rebuilding);
  w.Key("probe").BeginObject();
  Str(w, "result", r.probe.result);
  w.Key("osBuild").Uint(r.probe.os_build);
  Str(w, "format", r.probe.format);
  w.Key("levelDb").Double(r.probe.level_db);
  w.Key("ms").Int(r.probe.ms);
  w.EndObject();
  w.Key("device").BeginObject();
  w.Key("present").Bool(r.device.present);
  w.Key("renderEndpoints").Uint(r.device.render_endpoints);
  w.Key("captureEndpoints").Uint(r.device.capture_endpoints);
  w.EndObject();
  w.Key("apps").BeginArray();
  for (std::size_t i = 0; i < std::min(r.apps.size(), kReportMaxApps); ++i) {
    App(w, r.apps[i]);
  }
  w.EndArray();
  w.Key("sessions").BeginArray();
  for (std::size_t i = 0; i < std::min(r.sessions.size(), kReportMaxSessions); ++i) {
    Session(w, r.sessions[i]);
  }
  w.EndArray();
  w.Key("warnings").BeginArray();
  for (const ReportWarning &x : r.warnings) {
    w.BeginObject();
    Str(w, "code", x.code);
    Str(w, "app", x.app);
    w.EndObject();
  }
  w.EndArray();
}

} // namespace

std::string WriteReportJson(const Report &r) {
  json::Writer w;
  w.BeginObject();
  w.Key("v").Int(1);
  w.Key("seq").Uint(r.seq);
  w.Key("qpcNs").Int(r.qpc_ns);
  Visible(w, r);
  const ReportCounters &c = r.counters;
  w.Key("counters").BeginObject();
  w.Key("streams").Uint(c.streams);
  w.Key("activations").Uint(c.activations);
  w.Key("activationFailures").Uint(c.activation_failures);
  w.Key("underruns").Uint(c.underruns);
  w.Key("trims").Uint(c.trims);
  w.Key("overflows").Uint(c.overflows);
  w.Key("discontinuities").Uint(c.discontinuities);
  w.Key("mixerResyncs").Uint(c.mixer_resyncs);
  w.Key("fastpathExits").Uint(c.fastpath_exits);
  w.Key("fastpathLateCuts").Uint(c.fastpath_late_cuts);
  w.Key("silentReopens").Uint(c.silent_reopens);
  w.Key("deviceChanges").Uint(c.device_changes);
  w.Key("pidReuseBlocks").Uint(c.pid_reuse_blocks);
  w.Key("maxResamplePpm").Int(c.max_resample_ppm);
  w.Key("audioCpuMsPerS").Double(c.audio_cpu_ms_per_s);
  w.EndObject();
  w.Key("events").BeginArray();
  const std::size_t first = r.events.size() > kReportMaxEvents ? r.events.size() - kReportMaxEvents : 0;
  for (std::size_t i = first; i < r.events.size(); ++i) {
    const ReportEvent &e = r.events[i];
    w.BeginObject();
    w.Key("tNs").Int(e.t_ns);
    Str(w, "code", e.code);
    Str(w, "app", e.app);
    Str(w, "detail", e.detail);
    w.EndObject();
  }
  w.EndArray();
  w.EndObject();
  return w.Take();
}

std::string VisibleState(const Report &r) {
  json::Writer w;
  w.BeginObject();
  Visible(w, r);
  w.EndObject();
  return w.Take();
}

} // namespace im::audio
