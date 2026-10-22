// "What friends hear" (docs/m2/05-windows-audio.md §12.1; "05" below): the report's model and its JSON writer, which
// im_list_audio_apps hands out. Portable C++ (no OS header): the watcher fills a Report from its plan and state, and
// this file only writes it, in the field order of internal/media/testdata/audio's golden files (Go's
// media.ParseAudioReport reads them; audio_report_test.cpp checks the writer against them byte for byte).
//
// The rows come straight from im_audiopolicy's plan (AppRow); names, exe names and app ids are local data that the
// report carries to Go and never into a log (05 §16).
#pragma once

#include <cstddef>
#include <cstdint>
#include <string>
#include <vector>

#include "im/audiopolicy/planner.h"
#include "im/audiopolicy/types.h"

namespace im::audio {

// The report's limits (05 §12.1, §17): rows, the event ring, and the bytes of any string.
inline constexpr std::size_t kReportMaxApps = 128;
inline constexpr std::size_t kReportMaxSessions = 256;
inline constexpr std::size_t kReportMaxEvents = 300;
inline constexpr std::size_t kReportMaxStringBytes = 260;

// probe (05 §11.2).
struct ReportProbe {
  std::string result = "pending"; // "pending" | "ok" | "failed" | "unsupported" | "skipped"
  uint32_t os_build = 0;
  std::string format; // the capture format the probe initialized ("float32"), "" before a probe
  double level_db = -120;
  int32_t ms = 0;
};

// device.
struct ReportDevice {
  bool present = false; // an active render endpoint exists
  uint32_t render_endpoints = 0;
  uint32_t capture_endpoints = 0;
};

// sessions[]: one audio session of a process on an endpoint (05 §7.2).
struct ReportSession {
  uint32_t pid = 0;
  uint64_t created = 0; // FILETIME, written as a decimal string
  std::string exe;
  std::string app_id;
  audiopolicy::Flow flow = audiopolicy::Flow::kRender;
  audiopolicy::SessionState state = audiopolicy::SessionState::kInactive;
  bool multi_process = false;
  audiopolicy::Verdict verdict = audiopolicy::Verdict::kPending;
  audiopolicy::Reason reason = audiopolicy::Reason::kNone;
  std::string stream = "none"; // "none" | "activating" | "preroll" | "running" | "fading"
  std::string format;          // the stream's capture format, "" without a stream
  audiopolicy::Note note = audiopolicy::Note::kNone;
};

// warnings[].
struct ReportWarning {
  std::string code;
  std::string app;
};

// counters: the mixer's (05 §12.1); the watcher fills deviceChanges and pidReuseBlocks.
struct ReportCounters {
  uint64_t streams = 0;
  uint64_t activations = 0;
  uint64_t activation_failures = 0;
  uint64_t underruns = 0;
  uint64_t trims = 0;
  uint64_t overflows = 0;
  uint64_t discontinuities = 0;
  uint64_t mixer_resyncs = 0;
  uint64_t fastpath_exits = 0;
  uint64_t fastpath_late_cuts = 0;
  uint64_t silent_reopens = 0;
  uint64_t device_changes = 0;
  uint64_t pid_reuse_blocks = 0;
  int64_t max_resample_ppm = 0;
  double audio_cpu_ms_per_s = 0;
};

// events[]: code is one of 05 §12.1's event codes, app an app id or "".
struct ReportEvent {
  int64_t t_ns = 0; // engine clock
  std::string code;
  std::string app;
  std::string detail;
};

struct Report {
  uint32_t seq = 0;
  int64_t qpc_ns = 0;                // engine clock of the snapshot
  std::string mode = "exclude-apps"; // "exclude-apps" | "off"
  audiopolicy::Engine engine = audiopolicy::Engine::kNone;
  std::string state = "ok"; // "starting" | "ok" | "degraded" | "no-device" | "stopped"
  bool muted = false;
  bool paused = false;
  bool live = false;
  bool rebuilding = false;
  ReportProbe probe;
  ReportDevice device;
  std::vector<audiopolicy::AppRow> apps;
  std::vector<ReportSession> sessions;
  std::vector<ReportWarning> warnings;
  ReportCounters counters;
  std::vector<ReportEvent> events; // oldest first; the writer keeps the last kReportMaxEvents
};

// The report as im_list_audio_apps' JSON v1: compact UTF-8, the goldens' field order, at most kReportMaxApps apps and
// kReportMaxSessions sessions, the last kReportMaxEvents events, every string cut to kReportMaxStringBytes on a UTF-8
// boundary. A row's warnings are its kWarn* bits by name, lowest bit first.
std::string WriteReportJson(const Report &r);

// What seq covers (05 §12.1: everything except levels, counters, qpcNs and events), as a string to compare: the
// watcher increments seq when it changes.
std::string VisibleState(const Report &r);

} // namespace im::audio
