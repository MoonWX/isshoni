// The watcher's portable parts (docs/m2/05-windows-audio.md §12; README WA35, WA45): the planner's vocabulary in the
// report's (report.h; the report's one writer is im_audiomix's, checked against the goldens by mix_report_test, README
// §3 item 36), what the report's seq covers, and the level store's lock-free sample (05 §12.2). Pure C++: no audio
// device needed.
#include <atomic>
#include <cmath>
#include <cstring>
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
using audiopolicy::Reason;
using audiopolicy::Verdict;

TEST(AudioReport, ThePlannersVocabularyInTheReports) {
  EXPECT_STREQ(audiomix::ReportName(ToReport(Engine::kIncludeSet)), "include-set");
  EXPECT_STREQ(audiomix::ReportName(ToReport(Engine::kExcludeSelf)), "exclude-self");
  EXPECT_STREQ(audiomix::ReportName(ToReport(Engine::kEndpoint)), "endpoint");
  EXPECT_STREQ(audiomix::ReportName(ToReport(Engine::kNone)), "none");
  for (const Verdict v : {Verdict::kHeard, Verdict::kKeptOut, Verdict::kNotCaptured, Verdict::kPending}) {
    EXPECT_EQ(std::string(audiomix::ReportName(ToReport(v))), audiopolicy::VerdictName(v));
  }
  for (const Reason r :
       {Reason::kNone, Reason::kBuiltin, Reason::kUser, Reason::kMic, Reason::kViewer, Reason::kIsshoni,
        Reason::kLauncher, Reason::kTree, Reason::kPidReuse, Reason::kLimit, Reason::kCaptureFailed}) {
    EXPECT_EQ(std::string(audiomix::ReportName(ToReport(r))), audiopolicy::ReasonName(r));
  }
  for (const audiopolicy::Note n :
       {audiopolicy::Note::kNone, audiopolicy::Note::kCovered, audiopolicy::Note::kNoHandle}) {
    EXPECT_EQ(std::string(audiomix::ReportName(ToReport(n))), audiopolicy::NoteName(n));
  }
  for (const audiopolicy::Flow f : {audiopolicy::Flow::kRender, audiopolicy::Flow::kCapture}) {
    EXPECT_EQ(std::string(audiomix::ReportName(ToReport(f))), audiopolicy::FlowName(f));
  }
  EXPECT_STREQ(audiomix::ReportName(ToReport(audiopolicy::SessionState::kActive)), "active");
  EXPECT_STREQ(audiomix::ReportName(ToReport(audiopolicy::SessionState::kInactive)), "inactive");

  // A row's warning bits by name, lowest bit first, as the planner names them.
  const uint32_t all = audiopolicy::kWarnSharedAppUsesMic | audiopolicy::kWarnSharedAppKeptOut |
                       audiopolicy::kWarnIncludedUsesMic | audiopolicy::kWarnMultiProcessSession;
  const std::vector<audiomix::AppWarning> w = WarningsOf(all);
  ASSERT_EQ(w.size(), 4u);
  for (uint32_t bit = 1, i = 0; i < 4; bit <<= 1, ++i) {
    EXPECT_EQ(std::string(audiomix::ReportName(w[i])), audiopolicy::WarningName(bit));
  }
  EXPECT_TRUE(WarningsOf(0).empty());

  // Report-level warnings, the instance rule's included (05 §5.3).
  audiomix::ReportWarning rw;
  ASSERT_TRUE(ToReport(audiopolicy::PlanWarning{"instance_gone", "isshoni"}, &rw));
  EXPECT_EQ(rw.code, audiomix::AppWarning::kInstanceGone);
  EXPECT_EQ(rw.app, "isshoni");
  ASSERT_TRUE(ToReport(audiopolicy::PlanWarning{"multi_process_session", "exe:a.exe"}, &rw));
  EXPECT_EQ(rw.code, audiomix::AppWarning::kMultiProcessSession);
  EXPECT_FALSE(ToReport(audiopolicy::PlanWarning{"something_new", ""}, &rw));
}

TEST(AudioReport, ARowThroughTheWriter) {
  AppRow a;
  a.id = "exe:launcherx.exe";
  a.name = "LauncherX";
  a.exe = "launcherx.exe";
  a.verdict = Verdict::kNotCaptured;
  a.reason = Reason::kTree;
  a.detail = "Discord";
  a.pids = 1;
  a.sessions = 1;
  a.warnings = audiopolicy::kWarnSharedAppUsesMic;
  Report r;
  r.apps.push_back(ToReport(a));
  const std::string doc = audiomix::WriteReportJson(r);
  EXPECT_NE(doc.find("{\"id\":\"exe:launcherx.exe\",\"name\":\"LauncherX\",\"exe\":\"launcherx.exe\","
                     "\"verdict\":\"not_captured\",\"reason\":\"tree\",\"playing\":false,\"usesMic\":false,"
                     "\"browser\":false,\"shared\":false,\"pids\":1,\"sessions\":1,\"streams\":0,"
                     "\"detail\":\"Discord\",\"warnings\":[\"shared_app_uses_mic\"]}"),
            std::string::npos)
      << doc;
}

// seq covers apps, sessions, warnings and the state, not qpcNs, counters or events.
TEST(AudioReport, VisibleState) {
  Report a;
  audiomix::ReportApp app;
  app.id = "discord";
  app.verdict = audiomix::AppVerdict::kKeptOut;
  a.apps.push_back(app);
  Report b = a;
  b.qpc_ns = 99;
  b.seq = 7;
  b.counters.device_changes = 3;
  b.events.push_back(ReportEvent{1, audiomix::ReportEventCode::kDeviceAdded, "", ""});
  EXPECT_EQ(VisibleState(a), VisibleState(b));
  b.apps[0].playing = true;
  EXPECT_NE(VisibleState(a), VisibleState(b));
  b = a;
  b.state = audiomix::ReportState::kNoDevice;
  EXPECT_NE(VisibleState(a), VisibleState(b));
  b = a;
  b.muted = true;
  EXPECT_NE(VisibleState(a), VisibleState(b));
  b = a;
  b.sessions.push_back(audiomix::ReportSession{});
  EXPECT_NE(VisibleState(a), VisibleState(b));
  b.sessions[0].stream = audiomix::SessionStream::kRunning;
  Report c = a;
  c.sessions.push_back(audiomix::ReportSession{});
  EXPECT_NE(VisibleState(c), VisibleState(b)) << "a stream's state is visible";
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
  apps.push_back(AppActivity{"exe:spotify.exe", -500.0f, 6000, true, -700.0f});
  const im_audio_levels l = MakeLevels(42, 123456789, kSilenceDb, kSilenceDb, apps);
  EXPECT_EQ(l.struct_size, sizeof(im_audio_levels));
  EXPECT_EQ(l.report_seq, 42u);
  EXPECT_EQ(l.qpc_ns, 123456789);
  EXPECT_FLOAT_EQ(l.mix_peak_db, kSilenceDb);
  ASSERT_EQ(l.count, IM_MAX_LEVEL_APPS);
  EXPECT_STREQ(l.apps[0].app_id, "exe:spotify.exe");
  EXPECT_EQ(l.apps[0].source, 0u);
  EXPECT_FLOAT_EQ(l.apps[0].peak_db, kSilenceDb) << "floored at -120";
  EXPECT_FLOAT_EQ(l.apps[0].rms_db, kSilenceDb) << "a captured entry's own RMS, floored";
  EXPECT_STREQ(l.apps[1].app_id, "discord");
  EXPECT_FLOAT_EQ(l.apps[1].peak_db, -12.0f);
  EXPECT_FLOAT_EQ(l.apps[1].rms_db, -12.0f);
  EXPECT_EQ(l.apps[1].source, 1u);
  EXPECT_STREQ(l.apps[2].app_id, "exe:app69.exe");
  EXPECT_STREQ(l.apps[63].app_id, "exe:app8.exe") << "the 8 least recently active apps are cut";
}

// While live, an app's captured level (source 0, its own RMS) replaces its sessions' activity (05 §12.2).
TEST(AudioLevels, CapturedReplacesActivity) {
  std::vector<AppActivity> apps = {{"exe:game.exe", -3.0f, 100, false},
                                   {"exe:game.exe", -9.0f, 50, true, -12.0f},
                                   {"exe:game.exe", -1.0f, 70, false}};
  const im_audio_levels l = MakeLevels(1, 2, -9.0f, -12.0f, apps);
  ASSERT_EQ(l.count, 1u);
  EXPECT_EQ(l.apps[0].source, 0u);
  EXPECT_FLOAT_EQ(l.apps[0].peak_db, -9.0f);
  EXPECT_FLOAT_EQ(l.apps[0].rms_db, -12.0f);
  EXPECT_FLOAT_EQ(l.mix_rms_db, -12.0f);
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
