// The watcher's portable parts (docs/m2/05-windows-audio.md §12; README WA35): the report writer against the golden
// reports that Go's media.ParseAudioReport reads (internal/media/testdata/audio, 05 §13.1), byte for byte, and the
// level store's lock-free sample (05 §12.2). Pure C++: no audio device needed. The writer is interim (report.h): its
// golden tests go with it when README WA45 switches to audiomix's writer.
#include <atomic>
#include <cmath>
#include <cstring>
#include <fstream>
#include <iterator>
#include <sstream>
#include <string>
#include <thread>
#include <vector>

#include <gtest/gtest.h>

#include "audio/levels.h"
#include "audio/report.h"

namespace im::audio {
namespace {

using audiopolicy::AppRow;
using audiopolicy::Engine;
using audiopolicy::Flow;
using audiopolicy::Reason;
using audiopolicy::SessionState;
using audiopolicy::Verdict;

std::string Golden(const char *name) {
  const std::string path = std::string(IM_NATIVE_DIR) + "/../internal/media/testdata/audio/" + name;
  std::ifstream f(path, std::ios::binary);
  EXPECT_TRUE(f.good()) << "cannot read " << name;
  return std::string(std::istreambuf_iterator<char>(f), std::istreambuf_iterator<char>());
}

AppRow Row(const char *id, const char *name, const char *exe, Verdict v, Reason r, bool playing, bool mic,
           uint32_t pids, uint32_t sessions, uint32_t streams) {
  AppRow a;
  a.id = id;
  a.name = name;
  a.exe = exe;
  a.verdict = v;
  a.reason = r;
  a.playing = playing;
  a.uses_mic = mic;
  a.pids = pids;
  a.sessions = sessions;
  a.streams = streams;
  return a;
}

ReportSession Sess(uint32_t pid, uint64_t created, const char *exe, const char *app, SessionState st, Verdict v,
                   Reason r, const char *stream) {
  ReportSession s;
  s.pid = pid;
  s.created = created;
  s.exe = exe;
  s.app_id = app;
  s.flow = Flow::kRender;
  s.state = st;
  s.verdict = v;
  s.reason = r;
  s.stream = stream;
  s.format = "float32";
  return s;
}

Report LiveBase(uint32_t seq, int64_t qpc) {
  Report r;
  r.seq = seq;
  r.qpc_ns = qpc;
  r.engine = Engine::kIncludeSet;
  r.state = "ok";
  r.live = true;
  r.probe = ReportProbe{"ok", 26100, "float32", -60.1, 231};
  r.device = ReportDevice{true, 2, 1};
  return r;
}

TEST(AudioReport, NoDeviceGolden) {
  Report r;
  r.seq = 3;
  r.qpc_ns = 412000000000;
  r.engine = Engine::kNone;
  r.state = "no-device";
  r.probe = ReportProbe{"skipped", 26100, "", -120, 0};
  r.device = ReportDevice{false, 0, 0};
  r.apps.push_back(
      Row("discord", "Discord", "discord.exe", Verdict::kKeptOut, Reason::kBuiltin, false, false, 6, 0, 0));
  r.counters.device_changes = 1;
  r.events.push_back(ReportEvent{322000000000, "device_removed", "", ""});
  r.events.push_back(ReportEvent{322000000000, "no_device", "", ""});
  EXPECT_EQ(WriteReportJson(r), Golden("report-no-device.json"));
}

TEST(AudioReport, NormalGolden) {
  Report r = LiveBase(118, 81234567890123);
  r.apps.push_back(
      Row("exe:spotify.exe", "Spotify", "spotify.exe", Verdict::kHeard, Reason::kNone, true, false, 4, 1, 1));
  r.apps.push_back(Row("discord", "Discord", "discord.exe", Verdict::kKeptOut, Reason::kBuiltin, true, true, 6, 2, 0));
  r.apps.push_back(Row("exe:steam.exe", "Steam", "steam.exe", Verdict::kKeptOut, Reason::kMic, false, true, 3, 1, 0));
  AppRow l = Row("exe:launcherx.exe", "LauncherX", "launcherx.exe", Verdict::kNotCaptured, Reason::kTree, false, false,
                 1, 1, 0);
  l.detail = "Discord";
  r.apps.push_back(l);
  r.sessions.push_back(Sess(4120, 133725183450041234ull, "spotify.exe", "exe:spotify.exe", SessionState::kActive,
                            Verdict::kHeard, Reason::kNone, "running"));
  r.sessions.push_back(Sess(6604, 133725183457351234ull, "discord.exe", "discord", SessionState::kActive,
                            Verdict::kKeptOut, Reason::kBuiltin, "none"));
  r.sessions.push_back(Sess(6612, 133725183464661234ull, "discord.exe", "discord", SessionState::kInactive,
                            Verdict::kKeptOut, Reason::kBuiltin, "none"));
  r.sessions.push_back(Sess(2280, 133725183471971234ull, "steam.exe", "exe:steam.exe", SessionState::kInactive,
                            Verdict::kKeptOut, Reason::kMic, "none"));
  r.sessions.push_back(Sess(7316, 133725183479281234ull, "launcherx.exe", "exe:launcherx.exe", SessionState::kInactive,
                            Verdict::kNotCaptured, Reason::kTree, "none"));
  r.counters.streams = 1;
  r.counters.activations = 1;
  r.counters.max_resample_ppm = 41;
  r.counters.audio_cpu_ms_per_s = 6.2;
  r.events.push_back(ReportEvent{81174567890123, "session_created", "exe:spotify.exe", ""});
  r.events.push_back(ReportEvent{81174567890123, "session_created", "discord", ""});
  r.events.push_back(ReportEvent{81174567890123, "session_created", "exe:steam.exe", ""});
  r.events.push_back(ReportEvent{81175567890123, "session_created", "exe:launcherx.exe", ""});
  r.events.push_back(ReportEvent{81204567890123, "stream_opened", "exe:spotify.exe", ""});
  r.events.push_back(ReportEvent{81204607890123, "stream_running", "exe:spotify.exe", ""});
  EXPECT_EQ(WriteReportJson(r), Golden("report-normal.json"));
}

TEST(AudioReport, SharedMicGolden) {
  Report r = LiveBase(57, 1830000000000);
  AppRow v = Row("exe:valorant.exe", "VALORANT", "valorant.exe", Verdict::kHeard, Reason::kNone, true, true, 3, 2, 1);
  v.shared = true;
  v.warnings = audiopolicy::kWarnSharedAppUsesMic | audiopolicy::kWarnMultiProcessSession;
  r.apps.push_back(v);
  r.apps.push_back(
      Row("exe:spotify.exe", "Spotify", "spotify.exe", Verdict::kHeard, Reason::kNone, true, false, 4, 1, 1));
  r.apps.push_back(Row("discord", "Discord", "discord.exe", Verdict::kKeptOut, Reason::kBuiltin, true, true, 6, 2, 0));
  AppRow k = Row("exe:kook.exe", "KOOK", "kook.exe", Verdict::kHeard, Reason::kNone, true, true, 2, 1, 1);
  k.warnings = audiopolicy::kWarnIncludedUsesMic;
  r.apps.push_back(k);
  ReportSession s1 = Sess(8812, 133725184509991234ull, "valorant.exe", "exe:valorant.exe", SessionState::kActive,
                          Verdict::kHeard, Reason::kNone, "running");
  s1.multi_process = true;
  r.sessions.push_back(s1);
  ReportSession s2 = s1;
  s2.created = 133725184517301234ull;
  r.sessions.push_back(s2);
  r.sessions.push_back(Sess(4120, 133725184524611234ull, "spotify.exe", "exe:spotify.exe", SessionState::kActive,
                            Verdict::kHeard, Reason::kNone, "running"));
  r.sessions.push_back(Sess(6604, 133725184531921234ull, "discord.exe", "discord", SessionState::kActive,
                            Verdict::kKeptOut, Reason::kBuiltin, "none"));
  r.sessions.push_back(Sess(6612, 133725184539231234ull, "discord.exe", "discord", SessionState::kInactive,
                            Verdict::kKeptOut, Reason::kBuiltin, "none"));
  r.sessions.push_back(Sess(5520, 133725184546541234ull, "kook.exe", "exe:kook.exe", SessionState::kActive,
                            Verdict::kHeard, Reason::kNone, "running"));
  r.warnings.push_back(ReportWarning{"multi_process_session", "exe:valorant.exe"});
  r.counters.streams = 3;
  r.counters.activations = 3;
  r.counters.max_resample_ppm = 33;
  r.counters.audio_cpu_ms_per_s = 7.9;
  r.events.push_back(ReportEvent{1530000000000, "stream_running", "exe:valorant.exe", ""});
  r.events.push_back(ReportEvent{1530000000000, "stream_running", "exe:spotify.exe", ""});
  r.events.push_back(ReportEvent{1590000000000, "rules_changed", "", ""});
  r.events.push_back(ReportEvent{1590050000000, "stream_running", "exe:kook.exe", ""});
  EXPECT_EQ(WriteReportJson(r), Golden("report-shared-mic.json"));
}

// The caps of 05 §12.1: 128 apps, 256 sessions, the last 300 events, strings of at most 260 bytes cut on a UTF-8
// boundary; and what seq covers.
TEST(AudioReport, CapsAndVisibleState) {
  Report r;
  const std::string long_name = std::string(259, 'a') + "\xC3\xA9" + "QQQQ"; // 'é' straddles byte 260
  for (int i = 0; i < 200; ++i) {
    r.apps.push_back(
        Row("exe:a.exe", long_name.c_str(), "a.exe", Verdict::kHeard, Reason::kNone, false, false, 1, 0, 0));
  }
  for (int i = 0; i < 300; ++i) {
    r.sessions.push_back(Sess(static_cast<uint32_t>(i + 1), 1, "a.exe", "exe:a.exe", SessionState::kInactive,
                              Verdict::kHeard, Reason::kNone, "none"));
  }
  for (int i = 0; i < 400; ++i) {
    r.events.push_back(ReportEvent{i, "session_created", "", ""});
  }
  const std::string doc = WriteReportJson(r);
  auto count = [&](const std::string &needle) {
    std::size_t n = 0;
    for (std::size_t at = doc.find(needle); at != std::string::npos; at = doc.find(needle, at + 1)) {
      ++n;
    }
    return n;
  };
  EXPECT_EQ(count("\"verdict\":\"heard\",\"reason\":\"\",\"playing\""), kReportMaxApps);
  EXPECT_EQ(count("\"multiProcess\""), kReportMaxSessions);
  EXPECT_EQ(count("\"code\":\"session_created\""), kReportMaxEvents);
  EXPECT_NE(doc.find("{\"tNs\":100,"), std::string::npos) << "the last 300 events are kept";
  EXPECT_EQ(doc.find("{\"tNs\":99,"), std::string::npos);
  EXPECT_NE(doc.find("\"name\":\"" + std::string(259, 'a') + "\""), std::string::npos) << "cut before the 'é'";
  EXPECT_EQ(doc.find("QQQQ"), std::string::npos);

  // seq covers apps, sessions, warnings and the state, not qpcNs, counters or events.
  Report a, b;
  a.apps.push_back(
      Row("discord", "Discord", "discord.exe", Verdict::kKeptOut, Reason::kBuiltin, false, false, 1, 0, 0));
  b = a;
  b.qpc_ns = 99;
  b.seq = 7;
  b.counters.device_changes = 3;
  b.events.push_back(ReportEvent{1, "device_added", "", ""});
  EXPECT_EQ(VisibleState(a), VisibleState(b));
  b.apps[0].playing = true;
  EXPECT_NE(VisibleState(a), VisibleState(b));
  b = a;
  b.state = "no-device";
  EXPECT_NE(VisibleState(a), VisibleState(b));
}

TEST(AudioLevels, PeakToDb) {
  EXPECT_FLOAT_EQ(PeakToDb(1.0f), 0.0f);
  EXPECT_NEAR(PeakToDb(0.5f), -6.0206f, 1e-3);
  EXPECT_FLOAT_EQ(PeakToDb(0.0f), kSilenceDb);
  EXPECT_FLOAT_EQ(PeakToDb(1e-9f), kSilenceDb);
  EXPECT_FLOAT_EQ(PeakToDb(std::nanf("")), kSilenceDb);
  EXPECT_FLOAT_EQ(PeakToDb(-0.5f), kSilenceDb);
}

// MakeLevels (05 §12.2): one entry per app id, merged; most recently active first; at most 64; activity entries have
// rms = peak and source 1.
TEST(AudioLevels, MakeLevels) {
  std::vector<std::string> ids;
  for (int i = 0; i < 70; ++i) {
    ids.push_back("exe:app" + std::to_string(i) + ".exe");
  }
  std::vector<AppActivity> apps;
  for (int i = 0; i < 70; ++i) {
    apps.push_back(AppActivity{ids[static_cast<std::size_t>(i)], -30.0f, int64_t{1000} + i, false});
  }
  apps.push_back(AppActivity{"discord", -24.0f, 5000, false});
  apps.push_back(AppActivity{"discord", -12.0f, 4000, false}); // a second session of the same app: the loudest wins
  apps.push_back(AppActivity{"", -1.0f, 9999, false});         // no app id yet (pending): left out
  apps.push_back(AppActivity{"exe:spotify.exe", -500.0f, 6000, true});
  const im_audio_levels l = MakeLevels(42, 123456789, kSilenceDb, kSilenceDb, apps);
  EXPECT_EQ(l.struct_size, sizeof(im_audio_levels));
  EXPECT_EQ(l.report_seq, 42u);
  EXPECT_EQ(l.qpc_ns, 123456789);
  EXPECT_FLOAT_EQ(l.mix_peak_db, kSilenceDb);
  ASSERT_EQ(l.count, IM_MAX_LEVEL_APPS);
  EXPECT_STREQ(l.apps[0].app_id, "exe:spotify.exe");
  EXPECT_EQ(l.apps[0].source, 0u);
  EXPECT_FLOAT_EQ(l.apps[0].peak_db, kSilenceDb) << "floored at -120";
  EXPECT_STREQ(l.apps[1].app_id, "discord");
  EXPECT_FLOAT_EQ(l.apps[1].peak_db, -12.0f);
  EXPECT_FLOAT_EQ(l.apps[1].rms_db, -12.0f);
  EXPECT_EQ(l.apps[1].source, 1u);
  EXPECT_STREQ(l.apps[2].app_id, "exe:app69.exe");
  EXPECT_STREQ(l.apps[63].app_id, "exe:app8.exe") << "the 8 least recently active apps are cut";
}

TEST(AudioLevels, StoreRoundTrip) {
  LevelStore store;
  im_audio_levels first = store.Read();
  EXPECT_EQ(first.struct_size, sizeof(im_audio_levels));
  EXPECT_EQ(first.count, 0u);
  EXPECT_EQ(first.report_seq, 0u);
  EXPECT_FLOAT_EQ(first.mix_peak_db, kSilenceDb);
  std::vector<AppActivity> apps = {{"discord", -24.0f, 10, false}};
  im_audio_levels v = MakeLevels(7, 99, -3.0f, -6.0f, apps);
  v.struct_size = 1; // ignored by Publish
  store.Publish(v);
  const im_audio_levels got = store.Read();
  EXPECT_EQ(got.struct_size, sizeof(im_audio_levels));
  EXPECT_EQ(got.report_seq, 7u);
  EXPECT_EQ(got.qpc_ns, 99);
  EXPECT_FLOAT_EQ(got.mix_peak_db, -3.0f);
  ASSERT_EQ(got.count, 1u);
  EXPECT_STREQ(got.apps[0].app_id, "discord");
}

// Readers racing the writer never see a torn sample: every published sample is self-consistent (all its fields
// derive from one number), and so must every sample read.
TEST(AudioLevels, StoreNeverTears) {
  LevelStore store;
  std::atomic<bool> stop{false};
  std::atomic<int> torn{0};
  std::atomic<int> reads{0};
  auto reader = [&] {
    while (!stop.load(std::memory_order_relaxed)) {
      const im_audio_levels s = store.Read();
      const auto k = static_cast<int64_t>(s.report_seq);
      bool ok = s.qpc_ns == k * 3 && s.count == (s.report_seq % IM_MAX_LEVEL_APPS);
      for (uint32_t i = 0; ok && i < s.count; ++i) {
        ok = s.apps[i].peak_db == static_cast<float>(-static_cast<int>(s.report_seq % 100));
      }
      if (!ok) {
        torn.fetch_add(1);
      }
      reads.fetch_add(1, std::memory_order_relaxed);
    }
  };
  std::thread r1(reader), r2(reader);
  for (uint32_t k = 1; k <= 20000; ++k) {
    im_audio_levels v{};
    v.report_seq = k;
    v.qpc_ns = static_cast<int64_t>(k) * 3;
    v.count = k % IM_MAX_LEVEL_APPS;
    for (uint32_t i = 0; i < v.count; ++i) {
      v.apps[i].peak_db = static_cast<float>(-static_cast<int>(k % 100));
    }
    store.Publish(v);
  }
  stop.store(true);
  r1.join();
  r2.join();
  EXPECT_EQ(torn.load(), 0) << "of " << reads.load() << " reads";
  EXPECT_GT(reads.load(), 0);
}

} // namespace
} // namespace im::audio
