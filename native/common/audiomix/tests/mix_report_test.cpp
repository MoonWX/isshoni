// The report writer (docs/m2/05-windows-audio.md §12.1, §13.1, §19.1 "Report writer"): from fixed inputs it writes
// the golden files internal/media/testdata/audio/report-*.json byte for byte, the same files Go's media package
// parses (internal/media/audio_golden_test.go), so the C++ writer and the Go reader can't drift apart. Also the caps,
// the string rules and Go's float format.
#include <gtest/gtest.h>

#include <cmath>
#include <cstdio>
#include <fstream>
#include <limits>
#include <sstream>
#include <string>
#include <vector>

#include "im/audiomix/report.h"

namespace im::audiomix {
namespace {

// The repository's root, from this library's test data folder (which needn't exist).
std::string RepoRoot() {
  const std::string dir = IM_TESTDATA_DIR;
  const std::string tail = "/native/common/audiomix/tests/testdata";
  const std::size_t at = dir.rfind(tail);
  return at == std::string::npos ? dir : dir.substr(0, at);
}

std::string Golden(const std::string &name) {
  std::ifstream f(RepoRoot() + "/internal/media/testdata/audio/" + name, std::ios::binary);
  std::ostringstream s;
  s << f.rdbuf();
  return s.str();
}

ReportApp App(const char *id, const char *name, const char *exe, AppVerdict verdict, AppReason reason, bool playing,
              bool uses_mic, bool browser, bool shared, uint32_t pids, uint32_t sessions, uint32_t streams,
              const char *detail = "", std::vector<AppWarning> warnings = {}) {
  ReportApp a;
  a.id = id;
  a.name = name;
  a.exe = exe;
  a.verdict = verdict;
  a.reason = reason;
  a.playing = playing;
  a.uses_mic = uses_mic;
  a.browser = browser;
  a.shared = shared;
  a.pids = pids;
  a.sessions = sessions;
  a.streams = streams;
  a.detail = detail;
  a.warnings = std::move(warnings);
  return a;
}

ReportSession Session(uint32_t pid, uint64_t created, const char *exe, const char *app_id, SessionState state,
                      bool multi, AppVerdict verdict, AppReason reason, SessionStream stream) {
  ReportSession s;
  s.pid = pid;
  s.created = created;
  s.exe = exe;
  s.app_id = app_id;
  s.flow = SessionFlow::kRender;
  s.state = state;
  s.multi_process = multi;
  s.verdict = verdict;
  s.reason = reason;
  s.stream = stream;
  s.format = "float32";
  s.note = SessionNote::kNone;
  return s;
}

ReportEvent Event(int64_t t, ReportEventCode code, const char *app = "") {
  ReportEvent e;
  e.t_ns = t;
  e.code = code;
  e.app = app;
  return e;
}

// The parts most goldens share: a probe that passed, two render endpoints and one capture endpoint.
Report Base(uint32_t seq, int64_t qpc_ns, ReportEngine engine, ReportState state, bool live) {
  Report r;
  r.seq = seq;
  r.qpc_ns = qpc_ns;
  r.mode = ReportMode::kExcludeApps;
  r.engine = engine;
  r.state = state;
  r.live = live;
  r.probe = ReportProbe{ProbeResult::kOk, 26100, "float32", -60.1, 231};
  r.device = ReportDevice{true, 2, 1};
  return r;
}

ReportCounters Counters(uint64_t streams, uint64_t activations, int32_t ppm, double cpu) {
  ReportCounters c;
  c.streams = streams;
  c.activations = activations;
  c.max_resample_ppm = ppm;
  c.audio_cpu_ms_per_s = cpu;
  return c;
}

constexpr auto kHeard = AppVerdict::kHeard;
constexpr auto kKeptOut = AppVerdict::kKeptOut;
constexpr auto kNotCaptured = AppVerdict::kNotCaptured;
constexpr auto kActive = SessionState::kActive;
constexpr auto kInactive = SessionState::kInactive;
constexpr auto kRunning = SessionStream::kRunning;
constexpr auto kNone = SessionStream::kNone;

ReportApp Spotify(uint32_t streams) {
  return App("exe:spotify.exe", "Spotify", "spotify.exe", kHeard, AppReason::kNone, true, false, false, false, 4, 1,
             streams);
}
ReportApp Discord(AppVerdict v, AppReason why, bool playing = true, bool mic = true, uint32_t sessions = 2) {
  return App("discord", "Discord", "discord.exe", v, why, playing, mic, false, false, 6, sessions, 0);
}

Report Normal() {
  Report r = Base(118, 81234567890123, ReportEngine::kIncludeSet, ReportState::kOk, true);
  r.apps = {Spotify(1), Discord(kKeptOut, AppReason::kBuiltin),
            App("exe:steam.exe", "Steam", "steam.exe", kKeptOut, AppReason::kMic, false, true, false, false, 3, 1, 0),
            App("exe:launcherx.exe", "LauncherX", "launcherx.exe", kNotCaptured, AppReason::kTree, false, false, false,
                false, 1, 1, 0, "Discord")};
  r.sessions = {
      Session(4120, 133725183450041234, "spotify.exe", "exe:spotify.exe", kActive, false, kHeard, AppReason::kNone,
              kRunning),
      Session(6604, 133725183457351234, "discord.exe", "discord", kActive, false, kKeptOut, AppReason::kBuiltin, kNone),
      Session(6612, 133725183464661234, "discord.exe", "discord", kInactive, false, kKeptOut, AppReason::kBuiltin,
              kNone),
      Session(2280, 133725183471971234, "steam.exe", "exe:steam.exe", kInactive, false, kKeptOut, AppReason::kMic,
              kNone),
      Session(7316, 133725183479281234, "launcherx.exe", "exe:launcherx.exe", kInactive, false, kNotCaptured,
              AppReason::kTree, kNone)};
  r.counters = Counters(1, 1, 41, 6.2);
  r.events = {Event(81174567890123, ReportEventCode::kSessionCreated, "exe:spotify.exe"),
              Event(81174567890123, ReportEventCode::kSessionCreated, "discord"),
              Event(81174567890123, ReportEventCode::kSessionCreated, "exe:steam.exe"),
              Event(81175567890123, ReportEventCode::kSessionCreated, "exe:launcherx.exe"),
              Event(81204567890123, ReportEventCode::kStreamOpened, "exe:spotify.exe"),
              Event(81204607890123, ReportEventCode::kStreamRunning, "exe:spotify.exe")};
  return r;
}

Report Degraded() {
  Report r = Base(12, 9871234500000, ReportEngine::kEndpoint, ReportState::kDegraded, true);
  r.probe = ReportProbe{ProbeResult::kFailed, 26100, "float32", -97.3, 588};
  r.apps = {Spotify(0), Discord(kHeard, AppReason::kNone),
            App("exe:steam.exe", "Steam", "steam.exe", kHeard, AppReason::kNone, false, true, false, false, 3, 1, 0)};
  r.sessions = {
      Session(4120, 133725183486591234, "spotify.exe", "exe:spotify.exe", kActive, false, kHeard, AppReason::kNone,
              kNone),
      Session(6604, 133725183493901234, "discord.exe", "discord", kActive, false, kHeard, AppReason::kNone, kNone),
      Session(6612, 133725183501211234, "discord.exe", "discord", kInactive, false, kHeard, AppReason::kNone, kNone),
      Session(2280, 133725183508521234, "steam.exe", "exe:steam.exe", kInactive, false, kHeard, AppReason::kNone,
              kNone)};
  r.counters = Counters(1, 2, 0, 2.4);
  r.counters.mixer_resyncs = 1;
  r.events = {Event(9851234500000, ReportEventCode::kStreamOpened, "exe:spotify.exe"),
              Event(9851274500000, ReportEventCode::kStreamRunning, "exe:spotify.exe"),
              Event(9851834500000, ReportEventCode::kProbeFailed),
              Event(9851844500000, ReportEventCode::kStreamRemoved, "exe:spotify.exe"),
              Event(9851854500000, ReportEventCode::kStreamOpened),
              Event(9851894500000, ReportEventCode::kStreamRunning),
              Event(9851894500000, ReportEventCode::kMixerResync)};
  return r;
}

Report NoDevice() {
  Report r = Base(3, 412000000000, ReportEngine::kNone, ReportState::kNoDevice, false);
  r.probe = ReportProbe{ProbeResult::kSkipped, 26100, "", -120, 0};
  r.device = ReportDevice{false, 0, 0};
  r.apps = {Discord(kKeptOut, AppReason::kBuiltin, false, false, 0)};
  r.counters = Counters(0, 0, 0, 0);
  r.counters.device_changes = 1;
  r.events = {Event(322000000000, ReportEventCode::kDeviceRemoved), Event(322000000000, ReportEventCode::kNoDevice)};
  return r;
}

Report FastPath() {
  Report r = Base(21, 3600250000000, ReportEngine::kExcludeSelf, ReportState::kOk, true);
  r.apps = {Spotify(0), App("exe:eldenring.exe", "ELDEN RING", "eldenring.exe", kHeard, AppReason::kNone, true, false,
                            false, true, 1, 1, 0)};
  r.sessions = {Session(4120, 133725183515831234, "spotify.exe", "exe:spotify.exe", kActive, false, kHeard,
                        AppReason::kNone, kNone),
                Session(9932, 133725183523141234, "eldenring.exe", "exe:eldenring.exe", kActive, false, kHeard,
                        AppReason::kNone, kNone)};
  r.counters = Counters(1, 1, 12, 1.3);
  r.events = {Event(3480250000000, ReportEventCode::kSessionCreated, "exe:spotify.exe"),
              Event(3482250000000, ReportEventCode::kSessionCreated, "exe:eldenring.exe"),
              Event(3540250000000, ReportEventCode::kFastpathEnter),
              Event(3540290000000, ReportEventCode::kStreamRunning)};
  return r;
}

// 128 rows: Spotify, Discord, Steam and 125 numbered apps. App i: a mic user (kept out) when i % 9 == 0, idle when
// i % 13 == 0, else heard with its own stream until the 48 streams are taken, then not captured (limit).
Report ManyApps() {
  Report r = Base(960, 7200000000000, ReportEngine::kIncludeSet, ReportState::kOk, true);
  r.apps = {Spotify(1), Discord(kKeptOut, AppReason::kBuiltin),
            App("exe:steam.exe", "Steam", "steam.exe", kKeptOut, AppReason::kMic, false, true, false, false, 3, 1, 0)};
  uint64_t created = 133725183530451234;
  uint32_t pid = 1000;
  auto session = [&](const ReportApp &a, SessionState st, SessionStream stream) {
    r.sessions.push_back(Session(pid, created, a.exe.c_str(), a.id.c_str(), st, false, a.verdict, a.reason, stream));
    pid += 4;
    created += 7310000;
  };
  session(r.apps[0], kActive, kRunning);
  session(r.apps[1], kActive, kNone);
  session(r.apps[1], kInactive, kNone);
  session(r.apps[2], kInactive, kNone);
  uint32_t streams = 1;
  for (uint32_t i = 1; i <= 125; ++i) {
    char exe[16], id[24], name[16];
    std::snprintf(exe, sizeof exe, "app%03u.exe", i);
    std::snprintf(id, sizeof id, "exe:%s", exe);
    std::snprintf(name, sizeof name, "App %03u", i);
    const char *display =
        i == 7 ? "Caf\xc3\xa9 Radio" : (i == 8 ? "\xe9\x9f\xb3\xe4\xb9\x90\xe6\x92\xad\xe6\x94\xbe\xe5\x99\xa8" : name);
    const bool mic = i % 9 == 0, idle = i % 13 == 0;
    ReportApp a;
    if (mic) {
      a = App(id, display, exe, kKeptOut, AppReason::kMic, !idle, true, false, false, i % 5 + 1, 1, 0);
    } else if (idle) {
      a = App(id, display, exe, kHeard, AppReason::kNone, false, false, false, false, i % 5 + 1, 1, 0);
    } else if (streams < 48) {
      a = App(id, display, exe, kHeard, AppReason::kNone, true, false, false, false, i % 5 + 1, 1, 1);
      ++streams;
    } else {
      a = App(id, display, exe, kNotCaptured, AppReason::kLimit, true, false, false, false, i % 5 + 1, 1, 0);
    }
    r.apps.push_back(a);
    session(a, a.playing ? kActive : kInactive, a.streams > 0 ? kRunning : kNone);
  }
  r.counters = Counters(48, 52, 87, 21.4);
  r.counters.underruns = 2;
  r.counters.trims = 1;
  r.events = {Event(7197000000000, ReportEventCode::kUnderrun, "exe:app031.exe"),
              Event(7197500000000, ReportEventCode::kStreamRemoved, "exe:app044.exe"),
              Event(7197540000000, ReportEventCode::kStreamOpened, "exe:app052.exe"),
              Event(7197580000000, ReportEventCode::kStreamRunning, "exe:app052.exe")};
  return r;
}

Report BrowserMic() {
  Report r = Base(9, 15000000000, ReportEngine::kIncludeSet, ReportState::kOk, false);
  r.apps = {Spotify(0), Discord(kKeptOut, AppReason::kBuiltin),
            App("exe:chrome.exe", "Google Chrome", "chrome.exe", kKeptOut, AppReason::kMic, true, true, true, false, 14,
                1, 0),
            App("exe:msedge.exe", "Microsoft Edge", "msedge.exe", kHeard, AppReason::kNone, true, false, true, false, 9,
                1, 0)};
  r.sessions = {
      Session(4120, 133725184473441234, "spotify.exe", "exe:spotify.exe", kActive, false, kHeard, AppReason::kNone,
              kNone),
      Session(6604, 133725184480751234, "discord.exe", "discord", kActive, false, kKeptOut, AppReason::kBuiltin, kNone),
      Session(6612, 133725184488061234, "discord.exe", "discord", kInactive, false, kKeptOut, AppReason::kBuiltin,
              kNone),
      Session(11240, 133725184495371234, "chrome.exe", "exe:chrome.exe", kActive, false, kKeptOut, AppReason::kMic,
              kNone),
      Session(13008, 133725184502681234, "msedge.exe", "exe:msedge.exe", kActive, false, kHeard, AppReason::kNone,
              kNone)};
  r.counters = Counters(0, 0, 0, 0);
  r.events = {Event(0, ReportEventCode::kSessionCreated, "exe:spotify.exe"),
              Event(0, ReportEventCode::kSessionCreated, "discord"),
              Event(0, ReportEventCode::kSessionCreated, "exe:msedge.exe"),
              Event(5000000000, ReportEventCode::kSessionCreated, "exe:chrome.exe")};
  return r;
}

Report SharedMic() {
  Report r = Base(57, 1830000000000, ReportEngine::kIncludeSet, ReportState::kOk, true);
  r.apps = {App("exe:valorant.exe", "VALORANT", "valorant.exe", kHeard, AppReason::kNone, true, true, false, true, 3, 2,
                1, "", {AppWarning::kSharedAppUsesMic, AppWarning::kMultiProcessSession}),
            Spotify(1), Discord(kKeptOut, AppReason::kBuiltin),
            App("exe:kook.exe", "KOOK", "kook.exe", kHeard, AppReason::kNone, true, true, false, false, 2, 1, 1, "",
                {AppWarning::kIncludedUsesMic})};
  r.sessions = {
      Session(8812, 133725184509991234, "valorant.exe", "exe:valorant.exe", kActive, true, kHeard, AppReason::kNone,
              kRunning),
      Session(8812, 133725184517301234, "valorant.exe", "exe:valorant.exe", kActive, true, kHeard, AppReason::kNone,
              kRunning),
      Session(4120, 133725184524611234, "spotify.exe", "exe:spotify.exe", kActive, false, kHeard, AppReason::kNone,
              kRunning),
      Session(6604, 133725184531921234, "discord.exe", "discord", kActive, false, kKeptOut, AppReason::kBuiltin, kNone),
      Session(6612, 133725184539231234, "discord.exe", "discord", kInactive, false, kKeptOut, AppReason::kBuiltin,
              kNone),
      Session(5520, 133725184546541234, "kook.exe", "exe:kook.exe", kActive, false, kHeard, AppReason::kNone,
              kRunning)};
  r.warnings = {ReportWarning{AppWarning::kMultiProcessSession, "exe:valorant.exe"}};
  r.counters = Counters(3, 3, 33, 7.9);
  r.events = {Event(1530000000000, ReportEventCode::kStreamRunning, "exe:valorant.exe"),
              Event(1530000000000, ReportEventCode::kStreamRunning, "exe:spotify.exe"),
              Event(1590000000000, ReportEventCode::kRulesChanged),
              Event(1590050000000, ReportEventCode::kStreamRunning, "exe:kook.exe")};
  return r;
}

struct Case {
  const char *file;
  Report (*make)();
};

class Goldens : public ::testing::TestWithParam<Case> {};

TEST_P(Goldens, WriterMatchesByteForByte) {
  const std::string want = Golden(GetParam().file);
  ASSERT_FALSE(want.empty()) << "missing " << GetParam().file << " under " << RepoRoot();
  const std::string got = WriteReportJson(GetParam().make());
  EXPECT_EQ(got, want);
  if (got != want) {
    std::size_t i = 0;
    while (i < got.size() && i < want.size() && got[i] == want[i]) {
      ++i;
    }
    ADD_FAILURE() << "first difference at byte " << i << ":\n  got  ..." << got.substr(i > 40 ? i - 40 : 0, 120)
                  << "\n  want ..." << want.substr(i > 40 ? i - 40 : 0, 120);
  }
}

INSTANTIATE_TEST_SUITE_P(AllSeven, Goldens,
                         ::testing::Values(Case{"report-normal.json", &Normal}, Case{"report-degraded.json", &Degraded},
                                           Case{"report-no-device.json", &NoDevice},
                                           Case{"report-fast-path.json", &FastPath},
                                           Case{"report-many-apps.json", &ManyApps},
                                           Case{"report-browser-mic.json", &BrowserMic},
                                           Case{"report-shared-mic.json", &SharedMic}),
                         [](const auto &info) {
                           std::string n = info.param.file;
                           n = n.substr(7, n.size() - 12); // "report-" ... ".json"
                           for (char &c : n) {
                             c = c == '-' ? '_' : c;
                           }
                           return n;
                         });

// Every golden file is a case above (a new golden needs its fixed input here too).
TEST(Goldens, AllSevenAreCovered) {
  for (const char *f : {"report-normal.json", "report-degraded.json", "report-no-device.json", "report-fast-path.json",
                        "report-many-apps.json", "report-browser-mic.json", "report-shared-mic.json"}) {
    EXPECT_FALSE(Golden(f).empty()) << f;
  }
}

TEST(ReportWriter, CapsAppsSessionsAndEvents) {
  Report r = Base(1, 10, ReportEngine::kIncludeSet, ReportState::kOk, true);
  for (int i = 0; i < 130; ++i) {
    r.apps.push_back(Spotify(0));
  }
  for (int i = 0; i < 260; ++i) {
    r.sessions.push_back(Session(1, 2, "a.exe", "exe:a.exe", kActive, false, kHeard, AppReason::kNone, kNone));
  }
  for (int i = 0; i < 305; ++i) {
    r.events.push_back(Event(i, ReportEventCode::kMuted));
  }
  const std::string j = WriteReportJson(r);
  auto count = [&](const std::string &needle) {
    std::size_t n = 0;
    for (std::size_t at = j.find(needle); at != std::string::npos; at = j.find(needle, at + 1)) {
      ++n;
    }
    return n;
  };
  EXPECT_EQ(count("\"name\":\"Spotify\""), 128u);
  EXPECT_EQ(count("\"appId\":\"exe:a.exe\""), 256u);
  EXPECT_EQ(count("\"code\":\"muted\""), 300u);
  EXPECT_EQ(j.find("{\"tNs\":4,"), std::string::npos); // the oldest ones went
  EXPECT_NE(j.find("{\"tNs\":5,"), std::string::npos);
  EXPECT_NE(j.find("{\"tNs\":304,"), std::string::npos);
}

TEST(ReportWriter, StringsAreCutEscapedAndValidUtf8) {
  Report r = Base(1, 10, ReportEngine::kIncludeSet, ReportState::kOk, true);
  std::string long_name(259, 'a');
  long_name += "\xc3\xa9"; // a 2-byte character across the 260-byte limit
  r.apps.push_back(
      App("exe:x.exe", long_name.c_str(), "x.exe", kHeard, AppReason::kNone, true, false, false, false, 1, 1, 0));
  r.apps.push_back(
      App("exe:y.exe", "q\"b\\s\x01\n\xff", "y.exe", kHeard, AppReason::kNone, true, false, false, false, 1, 1, 0));
  const std::string j = WriteReportJson(r);
  EXPECT_NE(j.find("\"name\":\"" + std::string(259, 'a') + "\","), std::string::npos); // cut before the character
  EXPECT_NE(j.find("\"name\":\"q\\\"b\\\\s\\u0001\\n\xef\xbf\xbd\","), std::string::npos);
}

TEST(ReportWriter, LevelsAndCpuInTenths) {
  Report r = Base(1, 10, ReportEngine::kIncludeSet, ReportState::kOk, true);
  r.probe.level_db = -60.123;
  r.counters.audio_cpu_ms_per_s = 6.249;
  const std::string j = WriteReportJson(r);
  EXPECT_NE(j.find("\"levelDb\":-60.1,"), std::string::npos);
  EXPECT_NE(j.find("\"audioCpuMsPerS\":6.2}"), std::string::npos);
  r.probe.level_db = -0.04; // rounds to -0: written as 0
  EXPECT_NE(WriteReportJson(r).find("\"levelDb\":0,"), std::string::npos);
}

TEST(ReportWriter, GoFloatFormat) {
  auto f = [](double v) {
    std::string s;
    AppendGoFloat(s, v);
    return s;
  };
  EXPECT_EQ(f(0), "0");
  EXPECT_EQ(f(-0.0), "0");
  EXPECT_EQ(f(-60.1), "-60.1");
  EXPECT_EQ(f(-120), "-120");
  EXPECT_EQ(f(6.2), "6.2");
  EXPECT_EQ(f(100000), "100000"); // plain, where std::to_chars' shortest form would be 1e+05
  EXPECT_EQ(f(123456789012), "123456789012");
  EXPECT_EQ(f(0.000001), "0.000001");
  EXPECT_EQ(f(1e-7), "1e-7"); // encoding/json drops the exponent's leading zero
  EXPECT_EQ(f(-2.5e-9), "-2.5e-9");
  EXPECT_EQ(f(1e-10), "1e-10");
  EXPECT_EQ(f(1e21), "1e+21");
  EXPECT_EQ(f(1.5e300), "1.5e+300");
  EXPECT_EQ(f(std::numeric_limits<double>::quiet_NaN()), "null");
  EXPECT_EQ(f(std::numeric_limits<double>::infinity()), "null");
}

TEST(ReportWriter, EverySpelling) {
  EXPECT_STREQ(ReportName(ReportMode::kOff), "off");
  EXPECT_STREQ(ReportName(ReportState::kStarting), "starting");
  EXPECT_STREQ(ReportName(ReportState::kStopped), "stopped");
  EXPECT_STREQ(ReportName(ProbeResult::kPending), "pending");
  EXPECT_STREQ(ReportName(ProbeResult::kUnsupported), "unsupported");
  EXPECT_STREQ(ReportName(AppVerdict::kPending), "pending");
  EXPECT_STREQ(ReportName(AppReason::kUser), "user");
  EXPECT_STREQ(ReportName(AppReason::kViewer), "viewer");
  EXPECT_STREQ(ReportName(AppReason::kIsshoni), "isshoni");
  EXPECT_STREQ(ReportName(AppReason::kLauncher), "launcher");
  EXPECT_STREQ(ReportName(AppReason::kPidReuse), "pid_reuse");
  EXPECT_STREQ(ReportName(AppReason::kCaptureFailed), "capture_failed");
  EXPECT_STREQ(ReportName(AppWarning::kSharedAppKeptOut), "shared_app_kept_out");
  EXPECT_STREQ(ReportName(SessionFlow::kCapture), "capture");
  EXPECT_STREQ(ReportName(SessionStream::kActivating), "activating");
  EXPECT_STREQ(ReportName(SessionStream::kPreroll), "preroll");
  EXPECT_STREQ(ReportName(SessionStream::kFading), "fading");
  EXPECT_STREQ(ReportName(SessionNote::kCovered), "covered");
  EXPECT_STREQ(ReportName(SessionNote::kNoHandle), "no_handle");
  const char *codes[] = {"session_created",
                         "session_expired",
                         "stream_opened",
                         "stream_running",
                         "stream_removed",
                         "stream_failed",
                         "activation_failed",
                         "fastpath_enter",
                         "fastpath_exit",
                         "fastpath_late_cut",
                         "default_device_changed",
                         "device_added",
                         "device_removed",
                         "no_device",
                         "audiosrv_restart",
                         "probe_ok",
                         "probe_failed",
                         "mixer_resync",
                         "underrun",
                         "trim",
                         "silent_reopen",
                         "rules_changed",
                         "muted",
                         "unmuted",
                         "mode_changed",
                         "reanchor"};
  for (int i = 0; i <= static_cast<int>(ReportEventCode::kReanchor); ++i) {
    EXPECT_STREQ(ReportName(static_cast<ReportEventCode>(i)), codes[i]);
  }
}

} // namespace
} // namespace im::audiomix
