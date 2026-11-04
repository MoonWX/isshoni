// Windows audio, part 2 (docs/m2/05-windows-audio.md §6.1, §6.4, §6.5, §7.5, §8.5, §11, §13.2; README WA53) with fakes
// for the activator's opener, the capture thread, the process handles, the render client and the loopback io, and the
// real planner and mixer engine:
// - the fast path: one EXCLUDE stream from the first chunk, and its exit on a voice app's session_created without the
//   20 ms debounce, cutting the EXCLUDE stream's ring retroactively at C - 20 ms (nothing captured after the cut is
//   mixed, no late cut); a poll-found exit cuts late (fastpath_late_cut); the kill switch (IM_EXCLF_FAST_PATH off)
//   never plans it;
// - the engine gate: Windows 10 builds (a faked build number) plan the endpoint engine with the probe unsupported and
//   the state degraded (the banner exclusion-unavailable), a failed or forced-failed startup probe falls back to the
//   endpoint engine, an ok one is cached, "twice in 10 s" switches mid-share, and a default-device change re-probes
//   once 2 s later;
// - the endpoint engine: its stream crossfades in over the include set, and follows a default-device change by
//   reopening on the new default and fading the old stream once the new one runs; a passing re-probe crossfades back;
// - the mode, mute and pause flags: sound off fades the streams at once and a resume doesn't bring sound back, mute
//   and pause keep the streams and silence the bus;
// - the silent-stream check: reopen after 1 s of silent streams under a loud meter, at most once per 5 s, then
//   capture_failed after two failed reopens; the include set's ReopenApp and GiveUpApp;
// - the startup probe and the sound check against a simulated device (loopback working, silent, unsupported, busy),
//   their JSON, and the test exports' burst onsets predicted from the render clock.
// Portable: no OS call, so it also builds and runs on the Mac. Simulated time drives the mixer and the devices; the
// activator threads are real, so every wait for their results is bounded (README §4 "Testing").
#include <gtest/gtest.h>

#include <algorithm>
#include <atomic>
#include <chrono>
#include <cmath>
#include <cstdint>
#include <functional>
#include <map>
#include <memory>
#include <span>
#include <string>
#include <thread>
#include <utility>
#include <vector>

#include "audio/activator.h"
#include "audio/engine_gate.h"
#include "audio/include_set.h"
#include "audio/loopback_io.h"
#include "audio/signal.h"
#include "audio/silent_check.h"
#include "audio/sound_check.h"
#include "audio/startup_probe.h"
#include "im/audiomix/constants.h"
#include "im/audiomix/mix_engine.h"
#include "im/audiopolicy/exclusions.h"
#include "im/audiopolicy/planner.h"
#include "im/audiopolicy/process_table.h"
#include "isshoni_media.h"

namespace im::audio {
namespace {

using namespace std::chrono_literals;
using audiomix::SessionStream;
using audiopolicy::Engine;
using audiopolicy::ProcessEntry;
using audiopolicy::ProcessKey;
using audiopolicy::Reason;
using audiopolicy::StreamKind;
using audiopolicy::Verdict;

constexpr int64_t kMs = 1'000'000;
constexpr uint32_t kSelfPid = 50; // "isshoni.exe"

class FakeClient final : public CaptureClient {
public:
  void *event() const noexcept override { return nullptr; }
  int32_t Start() noexcept override { return 0; }
  int32_t Drain(audiomix::CaptureRing &) noexcept override { return 0; }
  uint64_t discontinuities() const noexcept override { return 0; }
};

// Activations that complete at once; counts them per kind.
class FakeOpener final : public StreamOpener {
public:
  OpenResult Open(const OpenRequest &req, ActivationContext &ctx) override {
    auto c = std::make_shared<Completion>();
    c->Complete();
    OpenResult r;
    r.id = req.id;
    if (ctx.Wait(c) != WaitResult::kDone) {
      r.hr = -1;
      return r;
    }
    switch (req.kind) {
    case StreamKind::kInclude:
      include.fetch_add(1);
      break;
    case StreamKind::kExclude:
      exclude.fetch_add(1);
      exclude_pid.store(req.pid);
      break;
    case StreamKind::kEndpoint:
      endpoint.fetch_add(1);
      break;
    }
    r.client = std::make_unique<FakeClient>();
    r.format = "float32";
    return r;
  }
  int opened() const { return include.load() + exclude.load() + endpoint.load(); }
  std::atomic<int> include{0}, exclude{0}, endpoint{0};
  std::atomic<uint32_t> exclude_pid{0};
};

class FakeCapture final : public CaptureHost {
public:
  bool Add(std::shared_ptr<CaptureStream> s) override {
    live.push_back(std::move(s));
    return true;
  }
  void Remove(const std::shared_ptr<CaptureStream> &s) override {
    live.erase(std::remove(live.begin(), live.end(), s), live.end());
  }
  std::vector<std::shared_ptr<CaptureStream>> live;
};

class FakeHold final : public ProcessHold {
public:
  bool exited() const noexcept override { return false; }
};

class FakeGate final : public ProcessGate {
public:
  HoldStatus Hold(const ProcessKey &, std::unique_ptr<ProcessHold> *out) override {
    *out = std::make_unique<FakeHold>();
    return HoldStatus::kHeld;
  }
};

// Every chunk's capture time and peak.
class Sink final : public audiomix::ChunkSink {
public:
  void OnChunk(const float *frames, int64_t ts) noexcept override {
    float peak = 0;
    for (uint32_t i = 0; i < audiomix::kChunkSamples; ++i) {
      peak = std::max(peak, std::fabs(frames[i]));
    }
    chunks.emplace_back(ts, peak);
  }
  void OnGap(int64_t) noexcept override {}
  std::vector<std::pair<int64_t, float>> chunks;
};

uint64_t FiletimeNow() { return 133'800'000'000'000'000ull; }

// The rules: Discord kept out by a built-in rule, explorer.exe a launcher, isshoni (pid 50) by the implicit instance
// rule; the fast path as given (the settings key audio.fastPath, 05 §6.4).
audiopolicy::Exclusions Rules(bool fast_path) {
  static im_rule rules[] = {
      {IM_RULE_APP_EXCLUDE, IM_RULE_SRC_BUILTIN, "discord.exe", "discord", "Discord"},
      {IM_RULE_STOP, IM_RULE_SRC_DATA, "explorer.exe", nullptr, nullptr},
  };
  im_exclusions ex{};
  ex.struct_size = sizeof ex;
  ex.flags = IM_EXCLF_KEEP_OUT_MIC_USERS | (fast_path ? IM_EXCLF_FAST_PATH : 0u);
  ex.rules = rules;
  ex.rule_count = 2;
  audiopolicy::Exclusions out;
  EXPECT_EQ(audiopolicy::ParseExclusions(&ex, kSelfPid, out), IM_OK);
  return out;
}

ProcessEntry Proc(uint32_t pid, uint32_t parent, uint64_t created, const char *exe, const char *folder) {
  ProcessEntry e;
  e.pid = pid;
  e.parent_pid = parent;
  e.created = created;
  e.exe = exe;
  e.folder = folder;
  return e;
}

// The capture streams with their fakes, the real planner, an engine gate and a mixer engine on simulated time. Every
// live stream's ring gets one packet per 10 ms, stamped with its capture time (the simulated QPC), of a DC level that
// value(stream kind, capture time) gives.
class Rig {
public:
  explicit Rig(bool fast_path = true, uint32_t os_build = 26100) : gate(os_build) {
    pool = std::make_unique<ActivatorPool>(opener);
    EXPECT_EQ(pool->Start(), IM_OK);
    StreamSetDeps d;
    d.pool = pool.get();
    d.capture = &capture;
    d.gate = &holds;
    d.filetime_now = &FiletimeNow;
    set = std::make_unique<StreamSet>(d);
    planner.SetExclusions(Rules(fast_path));
    procs = {Proc(4, 0, 100, "explorer.exe", "C:\\Windows"), Proc(kSelfPid, 4, 500, "isshoni.exe", "C:\\isshoni"),
             Proc(100, 4, 1000, "game.exe", "C:\\Games\\Game")};
    engine.Begin(0);
    set->Attach(&engine);
  }
  ~Rig() {
    set->Detach();
    pool->Stop();
  }

  void AddDiscord() { procs.push_back(Proc(200, 4, 2000, "discord.exe", "C:\\Apps\\Discord")); }

  audiopolicy::Plan Reconcile(int64_t reveal_ns = 0) {
    const audiopolicy::ProcessTable table(procs);
    std::vector<audiopolicy::Session> sessions;
    for (const uint32_t pid : playing) {
      audiopolicy::Session s;
      s.pid = pid;
      s.state = audiopolicy::SessionState::kActive;
      s.playing = true;
      s.last_active_ns = now;
      sessions.push_back(s);
    }
    const std::vector<audiopolicy::LiveStream> streams = set->Inputs();
    const std::vector<ProcessKey> failed = set->CaptureFailed();
    audiopolicy::PlanInput in;
    in.now_ns = now;
    in.mode = mode;
    in.process_loopback = gate.process_loopback();
    in.sessions = sessions;
    in.streams = streams;
    in.capture_failed = failed;
    audiopolicy::Plan plan = planner.Reconcile(table, in);
    EXPECT_TRUE(plan.violations.empty());
    set->Apply(
        plan,
        [&](const ProcessKey &k) {
          const auto i = table.Find(k);
          return i ? plan.processes[*i].app_id : std::string();
        },
        [&](const ProcessKey &anc, const ProcessKey &desc) {
          const auto a = table.Find(anc);
          const auto dd = table.Find(desc);
          return a && dd && *a != *dd && table.InTree(*dd, *a, false);
        },
        now, reveal_ns);
    return plan;
  }

  bool Pump() {
    return set->Pump(now, [&](const audiopolicy::Activation &a) {
      return planner.CommitCheck(audiopolicy::ProcessTable(procs), a);
    });
  }

  void PumpUntilActivated() {
    const auto end = std::chrono::steady_clock::now() + 10s;
    for (;;) {
      Pump();
      if (pool->in_flight() == 0 || std::chrono::steady_clock::now() > end) {
        break;
      }
      std::this_thread::sleep_for(1ms);
    }
    Pump();
  }

  // Advances simulated time by ms in 10 ms steps: a packet into every live stream, a mixer tick, a pump.
  void Run(int ms) {
    std::vector<float> pkt(2 * audiomix::kChunkFrames);
    for (int k = 0; k < ms / 10; ++k) {
      for (auto &s : capture.live) {
        std::fill(pkt.begin(), pkt.end(), value(now));
        s->ring.Write(pkt.data(), audiomix::kChunkFrames, now, now);
      }
      now += 10 * kMs;
      engine.Pump(now);
      Pump();
    }
  }

  // Plans and pumps until the streams the plan opened are joined, then runs ms.
  audiopolicy::Plan Settle(int ms = 100) {
    audiopolicy::Plan p = Reconcile();
    PumpUntilActivated();
    Run(ms);
    return p;
  }

  int Streams(StreamKind kind, SessionStream st) const {
    int n = 0;
    for (const StreamView &v : set->Views()) {
      n += v.kind == kind && v.stream == st ? 1 : 0;
    }
    return n;
  }
  int Streams(StreamKind kind) const {
    int n = 0;
    for (const StreamView &v : set->Views()) {
      n += v.kind == kind ? 1 : 0;
    }
    return n;
  }

  float LastPeak() const { return sink.chunks.empty() ? 0.0f : sink.chunks.back().second; }

  std::shared_ptr<FakeOpener> opener = std::make_shared<FakeOpener>();
  std::unique_ptr<ActivatorPool> pool;
  FakeCapture capture;
  FakeGate holds;
  Sink sink;
  audiomix::MixEngine engine{&sink};
  audiopolicy::Planner planner;
  EngineGate gate;
  std::unique_ptr<StreamSet> set;
  std::vector<ProcessEntry> procs;
  std::vector<uint32_t> playing{100};
  uint32_t mode = IM_AUDIO_MODE_EXCLUDE_APPS;
  std::function<float(int64_t)> value = [](int64_t) { return 0.5f; };
  int64_t now = 0;
};

// ---------------------------------------------------------------------------------------------------- the fast path

TEST(AudioFastPath, NoVoiceAppRunsOneExcludeStreamOnIsshoniFromTheFirstChunk) {
  Rig r;
  const auto plan = r.Settle(100);
  ASSERT_EQ(plan.engine, Engine::kExcludeSelf);
  EXPECT_EQ(plan.exclude_target.pid, kSelfPid);
  EXPECT_EQ(r.opener->exclude.load(), 1);
  EXPECT_EQ(r.opener->exclude_pid.load(), kSelfPid);
  EXPECT_EQ(r.opener->include.load(), 0);
  EXPECT_EQ(r.Streams(StreamKind::kExclude, SessionStream::kRunning), 1);
  EXPECT_GT(r.LastPeak(), 0.49f);
}

// 05 §6.4: Discord's session appears at C while the fast path runs. The watcher plans at once (no 20 ms debounce)
// and the planner leaves the fast path; the EXCLUDE stream's ring is cut at C - 20 ms, so the voice app's sound (every
// frame captured from C on) is never mixed, and the include set opens the game.
TEST(AudioFastPath, AVoiceAppsSessionExitsWithin15MsAndCutsAtCMinus20Ms) {
  Rig r;
  ASSERT_EQ(r.Settle(300).engine, Engine::kExcludeSelf);
  const int64_t c = r.now + 5 * kMs; // the OnSessionCreated callback's QPC time (Signals::first_session_ns)
  // From C on the EXCLUDE stream also carries Discord: the packet that holds C (and every later one) is "loud".
  r.value = [c](int64_t qpc) { return qpc + 10 * kMs > c ? 0.9f : 0.5f; };
  // The watcher's timing: a reconcile is due at C itself (the debounce would make it C + 20 ms), and one reconcile
  // takes at most 5 ms (05 §7.3): the exit is planned within 15 ms of session_created.
  EXPECT_EQ(SessionPlanDue(c, true), c);
  EXPECT_EQ(SessionPlanDue(c, false), c + kSessionDebounceNs);
  const int64_t planned = SessionPlanDue(c, true) + 5 * kMs;
  ASSERT_LE(planned - c, 15 * kMs);
  r.Run(10); // the packet of [C - 5 ms, C + 5 ms) reaches the ring and the mixer ticks meanwhile
  ASSERT_EQ(r.now, planned);
  r.AddDiscord();
  r.playing.push_back(200);
  const auto plan = r.Reconcile(c);
  r.value = [](int64_t) { return 0.5f; }; // the game alone in the include stream from now on
  ASSERT_EQ(plan.engine, Engine::kIncludeSet);
  EXPECT_EQ(plan.previous_engine, Engine::kExcludeSelf);
  EXPECT_EQ(r.set->counters().fastpath_cuts, 1u);
  EXPECT_EQ(r.Streams(StreamKind::kExclude, SessionStream::kFading), 1);
  r.PumpUntilActivated();
  r.Run(300);
  EXPECT_EQ(r.engine.counters().fastpath_late_cuts, 0u) << "detected in 5 ms: the cut point was still unmixed";
  EXPECT_EQ(r.Streams(StreamKind::kExclude), 0) << "the EXCLUDE stream left after its cut";
  EXPECT_EQ(r.Streams(StreamKind::kInclude, SessionStream::kRunning), 1)
      << "the game came back through the include set";
  EXPECT_EQ(r.opener->include.load(), 1);
  for (const auto &[ts, peak] : r.sink.chunks) {
    ASSERT_LT(peak, 0.89f) << "Discord's sound (captured from C on) was mixed at " << ts;
    if (ts >= c - 20 * kMs && ts + 10 * kMs <= planned) {
      // After the cut point and before the include stream joined (nothing it captured is older): nothing.
      EXPECT_EQ(peak, 0.0f) << "audio captured after C - 20 ms was mixed at " << (ts - c) / kMs << " ms";
    }
    if (ts + 10 * kMs <= c - 30 * kMs && ts >= 100 * kMs) {
      EXPECT_GT(peak, 0.49f) << "the game before the cut's 5 ms fade is mixed in full";
    }
  }
}

// A kept-out process found only by the 1 s poll (detection slower than about 20 ms): the cut point was already mixed,
// so the EXCLUDE stream fades out over 5 ms at once, counted as a late cut (05 §6.4).
TEST(AudioFastPath, APollFoundExitIsALateCut) {
  Rig r;
  ASSERT_EQ(r.Settle(300).engine, Engine::kExcludeSelf);
  const int64_t c = r.now;
  r.AddDiscord();
  r.playing.push_back(200);
  r.Run(200);                       // the poll comes 200 ms later
  const auto plan = r.Reconcile(c); // C: the time it was revealed... long mixed by now
  ASSERT_EQ(plan.engine, Engine::kIncludeSet);
  r.PumpUntilActivated();
  r.Run(100);
  EXPECT_EQ(r.engine.counters().fastpath_late_cuts, 1u);
  EXPECT_EQ(r.Streams(StreamKind::kExclude), 0);
}

// The kill switch (settings key audio.fastPath = false → no IM_EXCLF_FAST_PATH): the include set from the first chunk
// and after the 10 s the fast path would need, never an EXCLUDE stream.
TEST(AudioFastPath, TheKillSwitchNeverPlansTheFastPath) {
  Rig r(false);
  EXPECT_EQ(r.Settle(100).engine, Engine::kIncludeSet);
  r.Run(11000);
  const auto later = r.Reconcile();
  EXPECT_EQ(later.engine, Engine::kIncludeSet);
  EXPECT_TRUE(later.activate.empty());
  EXPECT_EQ(r.opener->exclude.load(), 0);
  EXPECT_EQ(r.opener->include.load(), 1);
  EXPECT_EQ(r.Streams(StreamKind::kInclude, SessionStream::kRunning), 1);
}

// Entering from the include set after 10 s without a break: the EXCLUDE stream crossfades in over the include mix,
// whose streams then close; the sound never drops out.
TEST(AudioFastPath, EnteringCrossfadesFromTheIncludeSetWithoutAGap) {
  Rig r;
  r.AddDiscord();
  r.playing.push_back(200);
  ASSERT_EQ(r.Settle(200).engine, Engine::kIncludeSet);
  // Discord quits: the condition holds again, from now on.
  r.procs.pop_back();
  r.playing.pop_back();
  for (int s = 0; s < 10; ++s) {
    EXPECT_EQ(r.Reconcile().engine, Engine::kIncludeSet) << "before 10 s";
    r.Run(1000);
  }
  const std::size_t from = r.sink.chunks.size();
  const auto plan = r.Reconcile();
  ASSERT_EQ(plan.engine, Engine::kExcludeSelf);
  r.PumpUntilActivated();
  r.Run(300);
  EXPECT_EQ(r.Streams(StreamKind::kExclude, SessionStream::kRunning), 1);
  EXPECT_EQ(r.Streams(StreamKind::kInclude), 0) << "the include streams closed after the crossfade";
  for (std::size_t i = from; i < r.sink.chunks.size(); ++i) {
    ASSERT_GT(r.sink.chunks[i].second, 0.2f) << "a gap in the crossfade at chunk " << i - from;
  }
}

// ---------------------------------------------------------------------------------------------- the engine gate

// README §3 item 52: a build below 22000 (a faked Windows 10 22H2) plans the endpoint engine whatever a probe would
// say; the report says probe unsupported and state degraded, which is the banner exclusion-unavailable (05 §14.7).
TEST(AudioEngineGate, Windows10BuildsPlanTheEndpointEngineWithTheBanner) {
  Rig r(true, 19045);
  EXPECT_TRUE(r.gate.build_gated());
  EXPECT_FALSE(r.gate.StartAudio(r.now, "{endpoint-a}", false)) << "no probe on a gated build";
  EXPECT_FALSE(r.gate.process_loopback());
  EXPECT_EQ(r.gate.Report(true).result, audiomix::ProbeResult::kUnsupported);
  EXPECT_EQ(r.gate.Report(true).os_build, 19045u);
  EXPECT_EQ(r.gate.LiveState(), audiomix::ReportState::kDegraded);
  const auto plan = r.Settle(100);
  EXPECT_EQ(plan.engine, Engine::kEndpoint);
  EXPECT_EQ(r.opener->endpoint.load(), 1);
  EXPECT_EQ(r.opener->include.load() + r.opener->exclude.load(), 0);
  EXPECT_EQ(r.Streams(StreamKind::kEndpoint, SessionStream::kRunning), 1);
  r.gate.LoopbackFailed(r.now);
  r.gate.DefaultDeviceChanged(r.now, "{endpoint-b}");
  EXPECT_EQ(r.gate.NextDeadline(), INT64_MAX) << "a gated build never re-probes";
  for (const uint32_t build : {22000u, 26100u, 0u}) {
    EngineGate g(build);
    EXPECT_FALSE(g.build_gated()) << build;
    EXPECT_TRUE(g.StartAudio(0, "{endpoint-a}", false)) << build;
    EXPECT_TRUE(g.process_loopback()) << "while the probe runs the include set is used (it never leaks)";
  }
}

TEST(AudioEngineGate, AFailedStartupProbeFallsBackToTheEndpointEngineWithTheBanner) {
  Rig r;
  ASSERT_TRUE(r.gate.StartAudio(r.now, "{endpoint-a}", false));
  r.gate.ProbeStarted();
  EXPECT_EQ(r.gate.Report(true).result, audiomix::ProbeResult::kPending);
  EXPECT_EQ(r.gate.LiveState(), audiomix::ReportState::kStarting);
  EXPECT_EQ(r.Settle(100).engine, Engine::kExcludeSelf) << "as if the probe passed while it runs";
  ProbeReport failed;
  failed.outcome = ProbeOutcome::kFailed;
  failed.level_db = -120;
  failed.endpoint_id = "{endpoint-a}";
  r.gate.ProbeDone(failed);
  EXPECT_FALSE(r.gate.process_loopback());
  EXPECT_EQ(r.gate.Report(true).result, audiomix::ProbeResult::kFailed);
  EXPECT_EQ(r.gate.LiveState(), audiomix::ReportState::kDegraded);
  const auto plan = r.Reconcile();
  EXPECT_EQ(plan.engine, Engine::kEndpoint);
  r.PumpUntilActivated();
  r.Run(200);
  EXPECT_EQ(r.Streams(StreamKind::kEndpoint, SessionStream::kRunning), 1);
  EXPECT_EQ(r.Streams(StreamKind::kExclude), 0) << "the endpoint stream took over";
  EXPECT_GT(r.LastPeak(), 0.49f);
  // The next audio start retries a failed result once; an unsupported one falls back the same way.
  r.gate.StopAudio();
  ASSERT_TRUE(r.gate.StartAudio(r.now, "{endpoint-a}", false));
  r.gate.ProbeStarted();
  failed.outcome = ProbeOutcome::kUnsupported;
  r.gate.ProbeDone(failed);
  EXPECT_FALSE(r.gate.process_loopback());
  EXPECT_EQ(r.gate.Report(true).result, audiomix::ProbeResult::kUnsupported);
}

TEST(AudioEngineGate, TheTestFlagForcesAFailedProbeAndAnOkResultIsCached) {
  EngineGate g(26100);
  EXPECT_FALSE(g.StartAudio(0, "{endpoint-a}", true)) << "IM_AUDIOF_TEST_FAIL_PROBE: failed without a probe";
  EXPECT_FALSE(g.process_loopback());
  EXPECT_EQ(g.Report(true).result, audiomix::ProbeResult::kFailed);
  g.StopAudio();
  ASSERT_TRUE(g.StartAudio(0, "{endpoint-a}", false));
  g.ProbeStarted();
  ProbeReport ok;
  ok.outcome = ProbeOutcome::kOk;
  ok.level_db = -60.2;
  ok.format = "float32";
  ok.ms = 231;
  ok.endpoint_id = "{endpoint-a}";
  g.ProbeDone(ok);
  EXPECT_TRUE(g.process_loopback());
  EXPECT_EQ(g.LiveState(), audiomix::ReportState::kOk);
  const audiomix::ReportProbe p = g.Report(true);
  EXPECT_EQ(p.result, audiomix::ProbeResult::kOk);
  EXPECT_EQ(p.format, "float32");
  EXPECT_EQ(p.ms, 231u);
  g.StopAudio();
  EXPECT_FALSE(g.StartAudio(0, "{endpoint-a}", false)) << "cached per endpoint for the process lifetime";
  EXPECT_EQ(g.Report(true).result, audiomix::ProbeResult::kOk);
  g.StopAudio();
  EXPECT_TRUE(g.StartAudio(0, "{endpoint-b}", false)) << "another default endpoint probes";
  g.StopAudio();
  EXPECT_FALSE(g.StartAudio(0, "", false)) << "no render endpoint: skipped";
  EXPECT_EQ(g.Report(false).result, audiomix::ProbeResult::kSkipped);
  EXPECT_TRUE(g.process_loopback()) << "skipped keeps the mode";
  g.DeviceEvent(5 * kMs);
  EXPECT_TRUE(g.ProbeDue(5 * kMs)) << "the next device event probes";
}

// 05 §6.5: process loopback stops working twice in 10 s → endpoint; failures in the 3 s after a default-device change
// are the change's (05 §7.5).
TEST(AudioEngineGate, LoopbackFailingTwiceIn10SecondsSwitchesToTheEndpointEngine) {
  EngineGate g(26100);
  ASSERT_TRUE(g.StartAudio(0, "{endpoint-a}", false));
  ProbeReport ok;
  ok.outcome = ProbeOutcome::kOk;
  ok.endpoint_id = "{endpoint-a}";
  g.ProbeStarted();
  g.ProbeDone(ok);
  g.LoopbackFailed(1000 * kMs);
  g.LoopbackFailed(11'500 * kMs); // 10.5 s later: not twice in 10 s
  EXPECT_TRUE(g.process_loopback());
  g.DefaultDeviceChanged(12'000 * kMs, "{endpoint-a}");
  g.LoopbackFailed(13'000 * kMs); // inside the 3 s after the change
  g.LoopbackFailed(14'000 * kMs);
  EXPECT_TRUE(g.process_loopback());
  g.LoopbackFailed(16'000 * kMs);
  EXPECT_FALSE(g.process_loopback()) << "11.5 s and 16 s: twice in 10 s";
  EXPECT_EQ(g.Report(true).result, audiomix::ProbeResult::kFailed);
  EXPECT_EQ(g.LiveState(), audiomix::ReportState::kDegraded);
}

// A StreamSet pump in which a joined process-loopback stream failed counts once (the gate's input).
TEST(AudioEngineGate, AFailedProcessLoopbackStreamCountsForTheGate) {
  Rig r(false);
  r.Settle(100);
  ASSERT_EQ(r.capture.live.size(), 1u);
  r.capture.live[0]->failed.store(true);
  r.Run(10);
  EXPECT_EQ(r.set->counters().loopback_failures, 1u);
  EXPECT_EQ(r.set->counters().stream_failures, 1u);
}

// ---------------------------------------------------------------------------------------------- the endpoint engine

// 05 §7.5: the default render device changes while the endpoint engine runs. Its stream reopens on the new default
// (prerolls and fades in on its own) and the old one fades out once the new one runs; the probe re-runs 2 s later and,
// when it passes, the include set (here the fast path) crossfades back in.
TEST(AudioEndpointEngine, FollowsADefaultDeviceChangeAndSwitchesBackAfterAPassingReprobe) {
  Rig r;
  ASSERT_TRUE(r.gate.StartAudio(r.now, "{endpoint-a}", false));
  r.gate.ProbeStarted();
  ProbeReport failed;
  failed.outcome = ProbeOutcome::kFailed;
  failed.endpoint_id = "{endpoint-a}";
  r.gate.ProbeDone(failed);
  ASSERT_EQ(r.Settle(200).engine, Engine::kEndpoint);
  ASSERT_EQ(r.Streams(StreamKind::kEndpoint, SessionStream::kRunning), 1);

  // The change: the endpoint stream reopens on the new default.
  const std::size_t from = r.sink.chunks.size();
  r.gate.DefaultDeviceChanged(r.now, "{endpoint-b}");
  ASSERT_TRUE(r.set->ReopenEndpoint(r.now));
  const auto plan = r.Reconcile();
  EXPECT_EQ(plan.engine, Engine::kEndpoint);
  ASSERT_EQ(plan.remove.size(), 1u) << "the old endpoint stream is replaced";
  EXPECT_EQ(plan.remove[0].reason, audiopolicy::RemovalReason::kEngine);
  r.PumpUntilActivated();
  EXPECT_EQ(r.opener->endpoint.load(), 2);
  EXPECT_EQ(r.Streams(StreamKind::kEndpoint), 2) << "the old one plays until the new one runs";
  r.Run(200);
  EXPECT_EQ(r.Streams(StreamKind::kEndpoint), 1);
  EXPECT_EQ(r.Streams(StreamKind::kEndpoint, SessionStream::kRunning), 1);
  for (std::size_t i = from; i < r.sink.chunks.size(); ++i) {
    ASSERT_GT(r.sink.chunks[i].second, 0.2f) << "a gap at the device change, chunk " << i - from;
  }

  // The re-probe 2 s after the change passes: back to process loopback.
  EXPECT_FALSE(r.gate.ProbeDue(r.now));
  r.Run(1800);
  ASSERT_TRUE(r.gate.ProbeDue(r.now));
  r.gate.ProbeStarted();
  ProbeReport ok;
  ok.outcome = ProbeOutcome::kOk;
  ok.endpoint_id = "{endpoint-b}";
  r.gate.ProbeDone(ok);
  ASSERT_TRUE(r.gate.process_loopback());
  const std::size_t back = r.sink.chunks.size();
  EXPECT_EQ(r.Reconcile().engine, Engine::kExcludeSelf);
  r.PumpUntilActivated();
  r.Run(300);
  EXPECT_EQ(r.Streams(StreamKind::kEndpoint), 0) << "the endpoint stream left after the crossfade";
  EXPECT_EQ(r.Streams(StreamKind::kExclude, SessionStream::kRunning), 1);
  for (std::size_t i = back; i < r.sink.chunks.size(); ++i) {
    ASSERT_GT(r.sink.chunks[i].second, 0.2f) << "a gap switching back, chunk " << i - back;
  }
}

// ------------------------------------------------------------------------------------- the mode, mute and pause flags

TEST(AudioLiveFlags, SoundOffFadesTheStreamsAndPauseOrMuteOnlySilenceTheBus) {
  Rig r(false);
  r.Settle(200);
  ASSERT_EQ(r.Streams(StreamKind::kInclude, SessionStream::kRunning), 1);
  ASSERT_GT(r.LastPeak(), 0.49f);

  // Pause (im_set_paused) and mute (im_audio_set_muted): the bus fades out in 10 ms, the streams stay.
  r.engine.SetPaused(true);
  r.Run(50);
  EXPECT_EQ(r.LastPeak(), 0.0f);
  EXPECT_EQ(r.Streams(StreamKind::kInclude, SessionStream::kRunning), 1);
  r.engine.SetPaused(false);
  r.Run(50);
  EXPECT_GT(r.LastPeak(), 0.49f);
  r.engine.SetMuted(true);
  r.Run(50);
  EXPECT_EQ(r.LastPeak(), 0.0f);
  EXPECT_EQ(r.Streams(StreamKind::kInclude, SessionStream::kRunning), 1);
  r.engine.SetMuted(false);
  r.Run(50);
  EXPECT_GT(r.LastPeak(), 0.49f);

  // Sound off (im_audio_set_mode(OFF)): the mode-off flag and a plan with engine none, whose removals fade at once.
  r.engine.SetSoundOff(true);
  r.mode = IM_AUDIO_MODE_OFF;
  EXPECT_EQ(r.Reconcile().engine, Engine::kNone);
  r.Run(100);
  EXPECT_EQ(r.LastPeak(), 0.0f);
  EXPECT_TRUE(r.set->Views().empty()) << "sound off closes the capture streams";
  // A resume (pause on, then off) never brings back sound that was switched off.
  r.engine.SetPaused(true);
  r.engine.SetPaused(false);
  r.Run(100);
  EXPECT_EQ(r.LastPeak(), 0.0f);

  // Sound on: plan and open the streams as at start (05 §6.5), then the bus fades in.
  r.engine.SetSoundOff(false);
  r.mode = IM_AUDIO_MODE_EXCLUDE_APPS;
  r.planner.Restart();
  EXPECT_EQ(r.Reconcile().engine, Engine::kIncludeSet);
  r.PumpUntilActivated();
  r.Run(200);
  EXPECT_EQ(r.Streams(StreamKind::kInclude, SessionStream::kRunning), 1);
  EXPECT_GT(r.LastPeak(), 0.49f);
  EXPECT_EQ(r.opener->include.load(), 2);
}

// ------------------------------------------------------------------------------------------ the silent-stream check

TEST(AudioSilentCheck, ReopensAfterOneSecondAtMostEveryFiveSecondsThenGivesUp) {
  SilentStreamCheck chk;
  const auto sample = [&](int64_t t, float meter, bool silent, bool has = true) {
    const SilentSample s{"exe:game.exe", meter, has, silent};
    return chk.Sample(t, std::span<const SilentSample>(&s, 1));
  };
  int64_t t = 0;
  std::vector<int64_t> reopens;
  std::vector<int64_t> give_ups;
  for (; t <= 20'000 * kMs; t += 100 * kMs) {
    const SilentActions a = sample(t, -20.0f, true);
    if (!a.reopen.empty()) {
      reopens.push_back(t);
    }
    if (!a.give_up.empty()) {
      give_ups.push_back(t);
    }
  }
  ASSERT_EQ(reopens.size(), 2u);
  EXPECT_EQ(reopens[0], 1000 * kMs) << "after 1 s of a loud meter over silent streams";
  EXPECT_EQ(reopens[1], 6000 * kMs) << "at most once per 5 s";
  ASSERT_EQ(give_ups.size(), 1u) << "two failed reopens in a row: capture_failed";
  EXPECT_EQ(give_ups[0], 7100 * kMs) << "1 s of silence after the second reopen";

  // A quiet meter, streams that deliver sound, or no streams: nothing.
  SilentStreamCheck quiet;
  for (int64_t u = 0; u <= 3000 * kMs; u += 100 * kMs) {
    const SilentSample q[] = {{"a", -70.0f, true, true}, {"b", -20.0f, true, false}, {"c", -20.0f, false, true}};
    const SilentActions a = quiet.Sample(u, q);
    EXPECT_TRUE(a.reopen.empty() && a.give_up.empty());
  }
  // A reopen that brings sound back starts the count over.
  SilentStreamCheck back;
  const auto s2 = [&](int64_t u, bool silent) {
    const SilentSample s{"x", -20.0f, true, silent};
    return back.Sample(u, std::span<const SilentSample>(&s, 1));
  };
  EXPECT_TRUE(s2(0, true).reopen.empty());
  EXPECT_EQ(s2(1000 * kMs, true).reopen.size(), 1u);
  EXPECT_TRUE(s2(1100 * kMs, false).reopen.empty()); // sound again
  EXPECT_TRUE(s2(6000 * kMs, true).reopen.empty());
  EXPECT_EQ(s2(7000 * kMs, true).reopen.size(), 1u);
  EXPECT_TRUE(s2(8000 * kMs, true).give_up.empty()) << "only one failed reopen in a row so far";
}

TEST(AudioSilentCheck, TheIncludeSetReopensAnAppsStreamsOrGivesUpOnThem) {
  Rig r(false);
  r.Settle(200);
  ASSERT_EQ(r.Streams(StreamKind::kInclude, SessionStream::kRunning), 1);
  EXPECT_EQ(r.set->ReopenApp("exe:game.exe", r.now), 1u);
  EXPECT_EQ(r.set->counters().silent_reopens, 1u);
  r.Run(30);
  r.PumpUntilActivated();
  EXPECT_EQ(r.opener->include.load(), 2) << "opened again at once";
  r.Run(200);
  EXPECT_EQ(r.Streams(StreamKind::kInclude, SessionStream::kRunning), 1);
  EXPECT_TRUE(r.set->CaptureFailed().empty()) << "a silent reopen is no failure";

  EXPECT_EQ(r.set->GiveUpApp("exe:game.exe"), 1u);
  r.Run(50);
  EXPECT_TRUE(r.set->Views().empty());
  const auto plan = r.Reconcile();
  EXPECT_TRUE(plan.activate.empty());
  ASSERT_EQ(plan.sessions.size(), 1u);
  EXPECT_EQ(plan.sessions[0].verdict, Verdict::kNotCaptured);
  EXPECT_EQ(plan.sessions[0].reason, Reason::kCaptureFailed);
  r.set->ForgetFailures(); // the next device or session event
  EXPECT_EQ(r.Reconcile().activate.size(), 1u);
}

// ------------------------------------------------------------------------------- simulated render and loopback device

// A shared-mode render client on simulated time: a 100 ms buffer that the device plays at 48 frames per ms, its clock
// at qpc0 + played frames, and what it played (the loopback's input).
class SimDevice final : public RenderTarget {
public:
  int32_t Writable(uint32_t *frames) noexcept override {
    *frames = static_cast<uint32_t>(kBuffer - (written - played));
    return fail_write ? -1 : 0;
  }
  int32_t Write(const float *frames, uint32_t n) noexcept override {
    stream.insert(stream.end(), frames, frames + 2 * n);
    written += n;
    return 0;
  }
  int32_t Start() noexcept override {
    started = true;
    return 0;
  }
  bool Position(uint64_t *frames, int64_t *qpc_ns) noexcept override {
    *frames = played;
    *qpc_ns = qpc0 + static_cast<int64_t>(played) * 1'000'000'000 / kSignalRate;
    return true;
  }
  void Wait(uint32_t ms) noexcept override { Advance(ms); }

  void Advance(uint32_t ms) {
    for (uint32_t i = 0; i < ms; ++i) {
      now += kMs;
      if (started) {
        played = std::min<uint64_t>(written, played + kSignalRate / 1000);
      }
    }
  }

  static constexpr uint64_t kBuffer = kSignalRate / 10;
  std::vector<float> stream; // everything written, interleaved stereo
  uint64_t written = 0;
  uint64_t played = 0;
  bool started = false;
  bool fail_write = false;
  int64_t qpc0 = 7'000 * kMs; // the device clock's QPC time at frame 0
  int64_t now = 0;
};

// The loopback io on a SimDevice: a capture returns what the device played since the last read, times gain (0: a
// silent loopback), plus, for a helper process, its chime from helper_from_ms on.
class SimIo final : public LoopbackIo {
public:
  OpenOutcome OpenProcessCapture(uint32_t pid, std::string *format) noexcept override {
    if (process != OpenOutcome::kOk) {
      return process;
    }
    *format = "float32";
    capturing = pid;
    read_at = dev.played;
    return OpenOutcome::kOk;
  }
  OpenOutcome OpenEndpointCapture(std::string *format) noexcept override {
    if (endpoint != OpenOutcome::kOk) {
      return endpoint;
    }
    *format = "float32";
    capturing = 1;
    endpoint_used = true;
    read_at = dev.played;
    return OpenOutcome::kOk;
  }
  int32_t ReadCapture(std::vector<float> &stereo) noexcept override {
    for (; read_at < dev.played; ++read_at) {
      stereo.push_back(gain * dev.stream[2 * read_at]);
      stereo.push_back(gain * dev.stream[2 * read_at + 1]);
    }
    if (helper_from_ms >= 0) {
      const Tone chime = Chime();
      const int64_t want = (dev.now / kMs - helper_from_ms) * 48;
      std::vector<float> f(2);
      for (; helper_frame < want && helper_frame < static_cast<int64_t>(chime.length); ++helper_frame) {
        ToneFrames(chime, static_cast<uint64_t>(helper_frame), 1, f.data());
        stereo.push_back(gain * f[0]);
        stereo.push_back(gain * f[1]);
      }
    }
    return 0;
  }
  void CloseCapture() noexcept override { capturing = 0; }
  OpenOutcome OpenRender(RenderTarget **out, std::string *endpoint_id) noexcept override {
    if (render != OpenOutcome::kOk) {
      return render;
    }
    *out = &dev;
    *endpoint_id = "{endpoint-a}";
    return OpenOutcome::kOk;
  }
  void CloseRender() noexcept override {}
  bool SessionSeen(uint32_t) noexcept override { return helper_from_ms >= 0 && dev.now / kMs >= helper_from_ms; }
  uint32_t SelfPid() const noexcept override { return kSelfPid; }
  int64_t NowNs() noexcept override { return dev.now; }
  void Wait(uint32_t ms) noexcept override { dev.Advance(ms); }

  SimDevice dev;
  float gain = 1.0f;
  OpenOutcome process = OpenOutcome::kOk;
  OpenOutcome endpoint = OpenOutcome::kOk;
  OpenOutcome render = OpenOutcome::kOk;
  int64_t helper_from_ms = -1; // a helper process plays the chime from then on (-1: none)
  int64_t helper_frame = 0;
  uint32_t capturing = 0;
  bool endpoint_used = false;
  uint64_t read_at = 0;
};

// ------------------------------------------------------------------------------------------------- the startup probe

TEST(AudioStartupProbe, MeasuresTheToneAndClassifiesIt) {
  SimIo io;
  ProbeReport r = RunStartupProbe(io, nullptr);
  EXPECT_EQ(r.outcome, ProbeOutcome::kOk);
  EXPECT_FALSE(r.level_mismatch);
  EXPECT_NEAR(r.level_db, -60.0, 0.5);
  EXPECT_EQ(r.format, "float32");
  EXPECT_EQ(r.endpoint_id, "{endpoint-a}");
  EXPECT_LE(r.ms, 600u) << "05 §11.2: <= 600 ms";
  EXPECT_GE(io.dev.written, 200u * 48) << "the 200 ms tone was played";

  SimIo silent;
  silent.gain = 0.0f; // a silent loopback (a broken driver or APO)
  EXPECT_EQ(RunStartupProbe(silent, nullptr).outcome, ProbeOutcome::kFailed);

  SimIo low;
  low.gain = 0.316f; // -10 dB: between -80 and -66
  r = RunStartupProbe(low, nullptr);
  EXPECT_EQ(r.outcome, ProbeOutcome::kOk);
  EXPECT_TRUE(r.level_mismatch) << "probe_level_mismatch";

  SimIo unsupported;
  unsupported.process = OpenOutcome::kUnsupported;
  EXPECT_EQ(RunStartupProbe(unsupported, nullptr).outcome, ProbeOutcome::kUnsupported);
  SimIo failed;
  failed.process = OpenOutcome::kFailed;
  EXPECT_EQ(RunStartupProbe(failed, nullptr).outcome, ProbeOutcome::kUnsupported) << "an activation failure";

  SimIo busy;
  busy.render = OpenOutcome::kBusy;
  EXPECT_EQ(RunStartupProbe(busy, nullptr).outcome, ProbeOutcome::kSkipped);
  SimIo none;
  none.render = OpenOutcome::kNoDevice;
  EXPECT_EQ(RunStartupProbe(none, nullptr).outcome, ProbeOutcome::kSkipped);

  EXPECT_EQ(ClassifyProbeLevel(-66.0), ProbeLevel::kOk);
  EXPECT_EQ(ClassifyProbeLevel(-54.0), ProbeLevel::kOk);
  EXPECT_EQ(ClassifyProbeLevel(-53.9), ProbeLevel::kMismatch);
  EXPECT_EQ(ClassifyProbeLevel(-80.0), ProbeLevel::kMismatch);
  EXPECT_EQ(ClassifyProbeLevel(-80.1), ProbeLevel::kSilent);
}

class FakeBackend final : public ProbeBackend {
public:
  ProbeReport Run(const std::atomic<bool> &abort) noexcept override {
    const auto end = std::chrono::steady_clock::now() + 5s;
    while (hold.load() && !abort.load() && std::chrono::steady_clock::now() < end) {
      std::this_thread::sleep_for(1ms);
    }
    ProbeReport r;
    r.outcome = ProbeOutcome::kFailed;
    r.endpoint_id = "{endpoint-a}";
    return r;
  }
  std::atomic<bool> hold{false};
};

TEST(AudioStartupProbe, TheRunnerRunsOneProbeAtATimeAndWakesTheWatcher) {
  auto backend = std::make_shared<FakeBackend>();
  backend->hold.store(true);
  std::atomic<int> wakes{0};
  ProbeRunner runner(backend, [&] { wakes.fetch_add(1); });
  ASSERT_TRUE(runner.Start());
  EXPECT_TRUE(runner.running());
  EXPECT_FALSE(runner.Start()) << "one at a time";
  ProbeReport r;
  EXPECT_FALSE(runner.TakeResult(&r));
  backend->hold.store(false);
  const auto end = std::chrono::steady_clock::now() + 10s;
  while (!runner.TakeResult(&r) && std::chrono::steady_clock::now() < end) {
    std::this_thread::sleep_for(1ms);
  }
  EXPECT_EQ(r.outcome, ProbeOutcome::kFailed);
  EXPECT_EQ(wakes.load(), 1);
  EXPECT_FALSE(runner.running());
  backend->hold.store(true);
  ASSERT_TRUE(runner.Start());
  runner.Stop(); // aborts it
  EXPECT_FALSE(runner.running());
  EXPECT_FALSE(runner.TakeResult(&r)) << "a stopped probe's result is dropped";
}

// ---------------------------------------------------------------------------------------------- the sound check

constexpr uint32_t kBothLoopbacks = IM_AUDIO_PROCESS_LOOPBACK | IM_AUDIO_ENDPOINT_LOOPBACK;

TEST(AudioSoundCheck, CapturesTheDllsOwnChimeThroughProcessLoopback) {
  SimIo io;
  const SoundCheckResult r = RunSoundCheck(io, SoundCheckParams{0, 3000, kBothLoopbacks}, nullptr);
  EXPECT_TRUE(r.captured);
  EXPECT_EQ(r.backend, "process-loopback");
  EXPECT_NEAR(r.level_db, -20.0, 0.5) << "each partial at half of -14 dBFS";
  EXPECT_GT(r.ms, 0u);
  EXPECT_LT(r.ms, 3000u);
  EXPECT_FALSE(io.endpoint_used);
  EXPECT_EQ(SoundCheckJson(r),
            "{\"captured\":true,\"levelDb\":-20,\"ms\":" + std::to_string(r.ms) + ",\"backend\":\"process-loopback\"}");
}

TEST(AudioSoundCheck, CapturesAHelpersChimeAndFallsBackToTheEndpoint) {
  SimIo io;
  io.helper_from_ms = 400; // RunSoundCheckHelper waits 400 ms, then plays
  SoundCheckResult r = RunSoundCheck(io, SoundCheckParams{4242, 3000, kBothLoopbacks}, nullptr);
  EXPECT_TRUE(r.captured);
  EXPECT_EQ(r.backend, "process-loopback");
  EXPECT_GE(r.ms, 400u);
  EXPECT_EQ(io.dev.written, 0u) << "with a helper the DLL plays nothing";

  SimIo unsupported; // no format initializes / E_NOTIMPL: the endpoint's loopback
  unsupported.helper_from_ms = 400;
  unsupported.process = OpenOutcome::kUnsupported;
  r = RunSoundCheck(unsupported, SoundCheckParams{4242, 3000, kBothLoopbacks}, nullptr);
  EXPECT_TRUE(r.captured);
  EXPECT_EQ(r.backend, "endpoint");
  EXPECT_TRUE(unsupported.endpoint_used);

  SimIo endpoint_only; // audio_flags without IM_AUDIO_PROCESS_LOOPBACK
  endpoint_only.helper_from_ms = 400;
  r = RunSoundCheck(endpoint_only, SoundCheckParams{4242, 3000, IM_AUDIO_ENDPOINT_LOOPBACK}, nullptr);
  EXPECT_TRUE(r.captured);
  EXPECT_EQ(r.backend, "endpoint");
}

TEST(AudioSoundCheck, ReportsWhyNothingWasCaptured) {
  SimIo never; // the helper's session never appears
  SoundCheckResult r = RunSoundCheck(never, SoundCheckParams{4242, 1000, kBothLoopbacks}, nullptr);
  EXPECT_FALSE(r.captured);
  EXPECT_EQ(r.reason, "timeout");
  EXPECT_GE(never.dev.now, 1000 * kMs) << "it waited the timeout";
  EXPECT_LT(never.dev.now, 1100 * kMs);
  EXPECT_EQ(SoundCheckJson(r), "{\"captured\":false,\"reason\":\"timeout\",\"backend\":\"process-loopback\"}");

  SimIo silent; // it plays, nothing comes through
  silent.helper_from_ms = 400;
  silent.gain = 0.0f;
  r = RunSoundCheck(silent, SoundCheckParams{4242, 2000, kBothLoopbacks}, nullptr);
  EXPECT_EQ(r.reason, "silent");

  SimIo failed;
  failed.process = OpenOutcome::kFailed;
  r = RunSoundCheck(failed, SoundCheckParams{4242, 1000, kBothLoopbacks}, nullptr);
  EXPECT_EQ(r.reason, "activation_failed");
  EXPECT_EQ(r.backend, "process-loopback");

  SimIo busy;
  busy.render = OpenOutcome::kBusy;
  r = RunSoundCheck(busy, SoundCheckParams{0, 1000, kBothLoopbacks}, nullptr);
  EXPECT_EQ(r.reason, "device_busy");

  SimIo nothing;
  r = RunSoundCheck(nothing, SoundCheckParams{0, 1000, 0}, nullptr);
  EXPECT_EQ(r.reason, "no_device");
  EXPECT_EQ(r.backend, "none");
  EXPECT_EQ(SoundCheckJson(r), "{\"captured\":false,\"reason\":\"no_device\",\"backend\":\"none\"}");
}

// ------------------------------------------------------------------------------------------------ the test exports

// im_test_render_bursts (05 §13.2): each burst's onset is predicted from the render clock's position samples, from the
// last one at or before its first frame: on a device whose clock is exact, the prediction is exact.
TEST(AudioTestExports, BurstOnsetsArePredictedFromTheRenderClock) {
  SimDevice dev;
  Bursts b;
  b.hz = 2000;
  b.amp = 0.5;
  b.burst_ms = 5;
  b.period_ms = 100;
  b.count = 5;
  int64_t onsets[8] = {};
  ASSERT_EQ(PlayBursts(dev, b, onsets, 8, nullptr), 5);
  for (uint32_t k = 0; k < 5; ++k) {
    const uint64_t f = (200 + 100 * k) * 48;
    EXPECT_EQ(onsets[k], dev.qpc0 + static_cast<int64_t>(f) * 1'000'000'000 / kSignalRate) << k;
    // The burst starts at that frame: silence before it, sound within its first ms.
    float before = 0, within = 0;
    for (uint64_t i = f - 48; i < f; ++i) {
      before = std::max(before, std::fabs(dev.stream[2 * i]));
    }
    for (uint64_t i = f; i < f + 5 * 48; ++i) {
      within = std::max(within, std::fabs(dev.stream[2 * i]));
    }
    EXPECT_EQ(before, 0.0f) << k;
    EXPECT_GT(within, 0.4f) << k;
  }
  EXPECT_GE(dev.played, dev.written) << "it returns once the last burst has played";
  EXPECT_EQ(onsets[5], 0) << "only count onsets are written";

  SimDevice small;
  int64_t two[2] = {};
  EXPECT_EQ(PlayBursts(small, b, two, 2, nullptr), 2) << "at most cap onsets";
  SimDevice broken;
  broken.fail_write = true;
  EXPECT_EQ(PlayBursts(broken, b, two, 2, nullptr), IM_ERR_INTERNAL);
}

TEST(AudioTestExports, TheToneIsPlayedInFullWithItsRamps) {
  SimDevice dev;
  Tone t;
  t.hz = 1000;
  t.amp = 0.25;
  t.length = 300 * 48;
  ASSERT_EQ(PlayTone(dev, t, 0, 0, nullptr), IM_OK);
  ASSERT_EQ(dev.written, t.length);
  EXPECT_EQ(dev.stream[0], 0.0f) << "the ramp starts at 0";
  std::vector<float> mono;
  AppendMono(dev.stream.data(), dev.written, mono);
  EXPECT_NEAR(ToneLevelDb(mono.data() + 4800, 4800, kSignalRate, 1000), 20 * std::log10(0.25), 0.1);
  std::atomic<bool> abort{true};
  SimDevice dev2;
  EXPECT_EQ(PlayTone(dev2, t, 0, 0, &abort), IM_ERR_CANCELLED);
}

} // namespace
} // namespace im::audio
