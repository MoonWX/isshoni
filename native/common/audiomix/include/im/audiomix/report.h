// The report "What friends hear": im_list_audio_apps' JSON v1 (docs/m2/05-windows-audio.md §12.1) and its writer.
// The watcher (README WA35, WA45) fills a Report on its own thread; WriteReportJson turns it into the exact bytes Go's
// media.ParseAudioReport reads: compact JSON without whitespace or a line break, every object's keys in §12.1's order
// and all of them always present, empty lists as [], integers in decimal, sessions[].created as a decimal string,
// levelDb and audioCpuMsPerS rounded to 0.1 and written in Go's shortest form, strings as UTF-8 escaped the way
// Go's encoding/json reads them. The golden files internal/media/testdata/audio/report-*.json are this writer's
// output for fixed inputs (mix_report_test) and Go's parse input, so the two sides can't drift.
//
// The caps of §12.1 are the writer's: the first 128 apps, the first 256 sessions, the last 300 events, and every
// string cut to 260 bytes on a UTF-8 boundary (invalid UTF-8 becomes U+FFFD). The writer allocates: never on a
// real-time thread.
#pragma once

#include <cstddef>
#include <cstdint>
#include <string>
#include <vector>

namespace im::audiomix {

enum class ReportMode : uint8_t { kExcludeApps, kOff }; // "everything" is reserved (later)
enum class ReportEngine : uint8_t { kIncludeSet, kExcludeSelf, kEndpoint, kNone };
enum class ReportState : uint8_t { kStarting, kOk, kDegraded, kNoDevice, kStopped };
enum class ProbeResult : uint8_t { kPending, kOk, kFailed, kUnsupported, kSkipped };
enum class AppVerdict : uint8_t { kHeard, kKeptOut, kNotCaptured, kPending };
enum class AppReason : uint8_t {
  kNone, // heard, pending: ""
  // kept out
  kBuiltin,
  kUser,
  kMic,
  kViewer,
  kIsshoni,
  // not captured
  kLauncher,
  kTree,
  kPidReuse,
  kLimit,
  kCaptureFailed,
};
enum class AppWarning : uint8_t { kSharedAppUsesMic, kSharedAppKeptOut, kIncludedUsesMic, kMultiProcessSession };
enum class SessionFlow : uint8_t { kRender, kCapture };
enum class SessionState : uint8_t { kInactive, kActive };
enum class SessionStream : uint8_t { kNone, kActivating, kPreroll, kRunning, kFading };
enum class SessionNote : uint8_t { kNone, kCovered, kNoHandle };
enum class ReportEventCode : uint8_t {
  kSessionCreated,
  kSessionExpired,
  kStreamOpened,
  kStreamRunning,
  kStreamRemoved,
  kStreamFailed,
  kActivationFailed,
  kFastpathEnter,
  kFastpathExit,
  kFastpathLateCut,
  kDefaultDeviceChanged,
  kDeviceAdded,
  kDeviceRemoved,
  kNoDevice,
  kAudiosrvRestart,
  kProbeOk,
  kProbeFailed,
  kMixerResync,
  kUnderrun,
  kTrim,
  kSilentReopen,
  kRulesChanged,
  kMuted,
  kUnmuted,
  kModeChanged,
  kReanchor,
};

// §12.1's spellings.
const char *ReportName(ReportMode v) noexcept;
const char *ReportName(ReportEngine v) noexcept;
const char *ReportName(ReportState v) noexcept;
const char *ReportName(ProbeResult v) noexcept;
const char *ReportName(AppVerdict v) noexcept;
const char *ReportName(AppReason v) noexcept;
const char *ReportName(AppWarning v) noexcept;
const char *ReportName(SessionFlow v) noexcept;
const char *ReportName(SessionState v) noexcept;
const char *ReportName(SessionStream v) noexcept;
const char *ReportName(SessionNote v) noexcept;
const char *ReportName(ReportEventCode v) noexcept;

struct ReportProbe {
  ProbeResult result = ProbeResult::kPending;
  uint32_t os_build = 0;
  std::string format; // "float32", "pcm16", "" before a format worked
  double level_db = -120.0;
  uint32_t ms = 0;
};

struct ReportDevice {
  bool present = false;
  uint32_t render_endpoints = 0;
  uint32_t capture_endpoints = 0;
};

struct ReportApp {
  std::string id;   // the matching rule's app id ("discord"), else "exe:" + the app root's exe
  std::string name; // local only: never logged
  std::string exe;
  AppVerdict verdict = AppVerdict::kPending;
  AppReason reason = AppReason::kNone;
  bool playing = false;
  bool uses_mic = false;
  bool browser = false;
  bool shared = false;
  uint32_t pids = 0;
  uint32_t sessions = 0;
  uint32_t streams = 0;
  std::string detail; // for kTree and kPidReuse: the kept-out app's name
  std::vector<AppWarning> warnings;
};

struct ReportSession {
  uint32_t pid = 0;
  uint64_t created = 0; // FILETIME, written as a decimal string
  std::string exe;
  std::string app_id;
  SessionFlow flow = SessionFlow::kRender;
  SessionState state = SessionState::kInactive;
  bool multi_process = false;
  AppVerdict verdict = AppVerdict::kPending;
  AppReason reason = AppReason::kNone;
  SessionStream stream = SessionStream::kNone;
  std::string format;
  SessionNote note = SessionNote::kNone;
};

struct ReportWarning {
  AppWarning code = AppWarning::kMultiProcessSession;
  std::string app; // an app id
};

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
  int32_t max_resample_ppm = 0;
  double audio_cpu_ms_per_s = 0;
};

struct ReportEvent {
  int64_t t_ns = 0; // engine clock
  ReportEventCode code = ReportEventCode::kSessionCreated;
  std::string app; // an app id, or ""
  std::string detail;
};

struct Report {
  static constexpr uint32_t kVersion = 1;
  static constexpr std::size_t kMaxApps = 128;
  static constexpr std::size_t kMaxSessions = 256;
  static constexpr std::size_t kMaxEvents = 300;
  static constexpr std::size_t kMaxString = 260; // bytes

  uint32_t seq = 0;
  int64_t qpc_ns = 0;
  ReportMode mode = ReportMode::kExcludeApps;
  ReportEngine engine = ReportEngine::kNone;
  ReportState state = ReportState::kStopped;
  bool muted = false;
  bool paused = false;
  bool live = false;
  bool rebuilding = false;
  ReportProbe probe;
  ReportDevice device;
  std::vector<ReportApp> apps;
  std::vector<ReportSession> sessions;
  std::vector<ReportWarning> warnings;
  ReportCounters counters;
  std::vector<ReportEvent> events; // oldest first; the writer keeps the last kMaxEvents
};

// The report's JSON (§12.1). Allocates.
std::string WriteReportJson(const Report &report);

// A double as Go's encoding/json writes a float64: the shortest decimal that reads back as v, in plain notation for
// 1e-6 <= |v| < 1e21 and else exponent notation without a leading zero in a negative exponent ("1e-7", "1e+21").
// 0 and -0 are both "0" (Go would write -0 as "-0"; both read back as zero). Not finite: "null" (Go refuses those;
// the writer never passes one).
void AppendGoFloat(std::string &out, double v);

} // namespace im::audiomix
