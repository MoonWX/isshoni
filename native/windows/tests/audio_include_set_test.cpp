// The include set's streams (docs/m2/05-windows-audio.md §5.7, §6.2, §6.3, §8.1, §8.5, §9.2; README WA45) with fakes
// for the activator's opener, the capture thread and the process handles, and the real planner and mixer engine:
// - the commit check: a target whose OS tree gained a kept-out process between the plan and the join is refused at
//   the commit, its stream never joins the mixer and the next plan says "not captured: tree"; a target that is safe
//   joins in Preroll; the planner never activates an INCLUDE stream on a tree that already holds a kept-out process;
// - a kept-out process that appears in the 40 ms preroll window removes the stream without output, and one that
//   appears while it runs fades it out within 10 ms;
// - an activation that never completes times out (the pool's timeout), counts activationFailures, is reopened after
//   100 ms, 500 ms and 2 s and, when the third reopen fails too, makes the target capture_failed;
// - a covered stream fades once its own coverer runs, whatever other streams still activate;
// - the formats tried before the one that worked go into the stream_opened event;
// - held handles: a pid that is another process now opens nothing, a refused handle activates anyway with the note
//   no_handle (a mismatch asks for a new plan), and an exited process removes its stream at once.
// Portable: no OS call, so it also builds and runs on the Mac. Simulated time drives the mixer; the activator threads
// are real, so every wait for their results is bounded (README §4 "Testing").
#include <gtest/gtest.h>

#include <algorithm>
#include <atomic>
#include <chrono>
#include <cmath>
#include <cstdint>
#include <map>
#include <memory>
#include <mutex>
#include <string>
#include <thread>
#include <vector>

#include "audio/activator.h"
#include "audio/include_set.h"
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
using audiopolicy::ProcessEntry;
using audiopolicy::ProcessKey;
using audiopolicy::Reason;
using audiopolicy::Verdict;

constexpr int64_t kMs = 1'000'000;
constexpr int32_t kTimeoutHr = static_cast<int32_t>(0x800705B4u);
constexpr int32_t kUnsupportedFormatHr = static_cast<int32_t>(0x88890008u); // AUDCLNT_E_UNSUPPORTED_FORMAT

class FakeClient final : public CaptureClient {
public:
  void *event() const noexcept override { return nullptr; }
  int32_t Start() noexcept override { return 0; }
  int32_t Drain(audiomix::CaptureRing &) noexcept override { return 0; }
  uint64_t discontinuities() const noexcept override { return 0; }
};

// Activations that complete at once, or never (then the pool's timeout ends them).
class FakeOpener final : public StreamOpener {
public:
  OpenResult Open(const OpenRequest &req, ActivationContext &ctx) override {
    opened.fetch_add(1);
    auto c = std::make_shared<Completion>();
    if (!never.load() && req.pid != never_pid.load()) {
      c->Complete();
    }
    OpenResult r;
    r.id = req.id;
    const WaitResult w = ctx.Wait(c);
    if (w == WaitResult::kDone) {
      r.client = std::make_unique<FakeClient>();
      r.format = failed_attempts.load() > 0 ? "pcm16" : "float32";
      r.failed_attempts = failed_attempts.load();
      r.last_failure = r.failed_attempts > 0 ? kUnsupportedFormatHr : 0;
    } else {
      r.hr = kTimeoutHr;
      r.timed_out = w == WaitResult::kTimeout;
      r.aborted = w == WaitResult::kAborted;
    }
    return r;
  }
  std::atomic<bool> never{false};
  std::atomic<uint32_t> never_pid{0}; // only this pid's activations never complete
  std::atomic<uint32_t> failed_attempts{0};
  std::atomic<int> opened{0};
};

class FakeCapture final : public CaptureHost {
public:
  bool Add(std::shared_ptr<CaptureStream> s) override {
    if (refuse) {
      return false;
    }
    live.push_back(std::move(s));
    ++added;
    return true;
  }
  void Remove(const std::shared_ptr<CaptureStream> &s) override {
    live.erase(std::remove(live.begin(), live.end(), s), live.end());
    ++removed;
  }
  std::vector<std::shared_ptr<CaptureStream>> live;
  int added = 0;
  int removed = 0;
  bool refuse = false; // the capture thread doesn't take streams (it didn't start)
};

class FakeHold final : public ProcessHold {
public:
  explicit FakeHold(std::shared_ptr<std::atomic<bool>> e) : exited_(std::move(e)) {}
  bool exited() const noexcept override { return exited_->load(); }

private:
  std::shared_ptr<std::atomic<bool>> exited_;
};

class FakeGate final : public ProcessGate {
public:
  HoldStatus Hold(const ProcessKey &key, std::unique_ptr<ProcessHold> *out) override {
    ++holds;
    if (status != HoldStatus::kHeld) {
      return status;
    }
    auto &flag = exited[key.pid];
    if (!flag) {
      flag = std::make_shared<std::atomic<bool>>(false);
    }
    *out = std::make_unique<FakeHold>(flag);
    return HoldStatus::kHeld;
  }
  HoldStatus status = HoldStatus::kHeld;
  std::map<uint32_t, std::shared_ptr<std::atomic<bool>>> exited;
  int holds = 0;
};

class Sink final : public audiomix::ChunkSink {
public:
  void OnChunk(const float *frames, int64_t) noexcept override {
    ++chunks;
    float peak = 0;
    for (uint32_t i = 0; i < audiomix::kChunkSamples; ++i) {
      peak = std::max(peak, std::fabs(frames[i]));
    }
    peaks.push_back(peak);
  }
  void OnGap(int64_t) noexcept override {}
  uint64_t chunks = 0;
  std::vector<float> peaks;
};

uint64_t FiletimeNow() { return 133'800'000'000'000'000ull; }

// The rules: Discord kept out by a built-in rule, explorer.exe a launcher.
audiopolicy::Exclusions Rules() {
  static im_rule rules[] = {
      {IM_RULE_APP_EXCLUDE, IM_RULE_SRC_BUILTIN, "discord.exe", "discord", "Discord"},
      {IM_RULE_STOP, IM_RULE_SRC_DATA, "explorer.exe", nullptr, nullptr},
  };
  im_exclusions ex{};
  ex.struct_size = sizeof ex;
  ex.flags = IM_EXCLF_KEEP_OUT_MIC_USERS; // no fast path: README WA53
  ex.rules = rules;
  ex.rule_count = 2;
  audiopolicy::Exclusions out;
  EXPECT_EQ(audiopolicy::ParseExclusions(&ex, 0, out), IM_OK);
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

// The include set with its fakes, the real planner and a mixer engine on simulated time.
class Rig {
public:
  explicit Rig(std::chrono::milliseconds timeout = 3000ms) {
    ActivatorOptions o;
    o.timeout = timeout;
    pool = std::make_unique<ActivatorPool>(opener, o);
    EXPECT_EQ(pool->Start(), IM_OK);
    StreamSetDeps d;
    d.pool = pool.get();
    d.capture = &capture;
    d.gate = &gate;
    d.filetime_now = &FiletimeNow;
    set = std::make_unique<StreamSet>(d);
    planner.SetExclusions(Rules());
    procs = {Proc(4, 0, 100, "explorer.exe", "C:\\Windows"), Proc(100, 4, 1000, "game.exe", "C:\\Games\\Game")};
    engine.Begin(0);
    set->Attach(&engine);
  }
  ~Rig() {
    set->Detach();
    pool->Stop();
  }

  void AddDiscordUnderGame() { procs.push_back(Proc(200, 100, 2000, "discord.exe", "C:\\Apps\\Discord")); }

  // One reconcile: a plan over procs and the render sessions of playing (the game's), acted on.
  audiopolicy::Plan Reconcile() {
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
    in.mode = IM_AUDIO_MODE_EXCLUDE_APPS;
    in.process_loopback = true;
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
          const auto d = table.Find(desc);
          return a && d && *a != *d && table.InTree(*d, *a, false);
        },
        now);
    return plan;
  }

  // Pumps the set with the commit check on a fresh table of procs (as the watcher takes one).
  bool Pump() {
    return set->Pump(now, [&](const audiopolicy::Activation &a) {
      ++commits;
      return planner.CommitCheck(audiopolicy::ProcessTable(procs), a);
    });
  }

  // Pumps until the activator answered every request (bounded).
  void PumpUntilActivated() {
    const auto end = std::chrono::steady_clock::now() + 10s;
    for (;;) {
      Pump();
      if (pool->in_flight() == 0 || std::chrono::steady_clock::now() > end) {
        break;
      }
      std::this_thread::sleep_for(1ms);
    }
    Pump(); // the last results
  }

  // Advances simulated time by ms, a tick and a packet of a 0.5 DC level per 10 ms into every live stream's ring.
  void Run(int ms, bool sound = true) {
    std::vector<float> pkt(2 * audiomix::kChunkFrames, sound ? 0.5f : 0.0f);
    for (int k = 0; k < ms / 10; ++k) {
      for (auto &s : capture.live) {
        s->ring.Write(pkt.data(), audiomix::kChunkFrames, now, now);
      }
      now += 10 * kMs;
      engine.Pump(now);
      Pump();
    }
  }

  SessionStream StreamOf(uint32_t pid) const {
    for (const StreamView &v : set->Views()) {
      if (v.target.pid == pid) {
        return v.stream;
      }
    }
    return SessionStream::kNone;
  }
  SessionStream GameStream() const { return StreamOf(100); }

  std::shared_ptr<FakeOpener> opener = std::make_shared<FakeOpener>();
  std::unique_ptr<ActivatorPool> pool;
  FakeCapture capture;
  FakeGate gate;
  Sink sink;
  audiomix::MixEngine engine{&sink};
  audiopolicy::Planner planner;
  std::unique_ptr<StreamSet> set;
  std::vector<ProcessEntry> procs;
  std::vector<uint32_t> playing{100};
  int64_t now = 0;
  int commits = 0;
};

const audiopolicy::SessionResult &GameSession(const audiopolicy::Plan &p) { return p.sessions.at(0); }

// Waits (bounded) until the activator answered as many requests as given, pumping meanwhile.
void PumpUntilOpened(Rig &r, int opened) {
  const auto end = std::chrono::steady_clock::now() + 10s;
  while (r.opener->opened.load() < opened && std::chrono::steady_clock::now() < end) {
    r.Pump();
    std::this_thread::sleep_for(1ms);
  }
}

// ------------------------------------------------------------------------------------------------- the commit check

TEST(AudioIncludeSet, ASafeTargetPassesTheCommitCheckAndJoinsInPreroll) {
  Rig r;
  const auto plan = r.Reconcile();
  ASSERT_EQ(plan.engine, audiopolicy::Engine::kIncludeSet);
  ASSERT_EQ(plan.activate.size(), 1u);
  EXPECT_EQ(plan.activate[0].target.pid, 100u);
  EXPECT_EQ(r.GameStream(), SessionStream::kActivating);
  ASSERT_EQ(r.set->Inputs().size(), 1u);
  EXPECT_EQ(r.set->Inputs()[0].activated_filetime, 0u) << "still activating: covers every process";
  r.PumpUntilActivated();
  EXPECT_EQ(r.commits, 1);
  EXPECT_EQ(r.capture.added, 1);
  EXPECT_EQ(r.GameStream(), SessionStream::kPreroll);
  EXPECT_NE(r.set->Inputs()[0].activated_filetime, 0u);
  EXPECT_EQ(r.set->counters().activations, 1u);
  EXPECT_EQ(r.set->counters().streams, 1u);
  r.Run(60);
  EXPECT_EQ(r.GameStream(), SessionStream::kRunning);
  EXPECT_GT(r.sink.peaks.back(), 0.49f);
  const auto again = r.Reconcile(); // kept: no second activation
  EXPECT_TRUE(again.activate.empty());
  EXPECT_TRUE(again.remove.empty());
  EXPECT_EQ(GameSession(again).verdict, Verdict::kHeard);
  bool opened = false;
  for (const auto &e : r.set->TakeEvents()) {
    opened = opened || (e.code == audiomix::ReportEventCode::kStreamOpened && e.app == "exe:game.exe");
  }
  EXPECT_TRUE(opened);
}

// 05 §6.2 step 5: the plan saw a safe tree; by the time the activation completed, Discord runs under the game. The
// commit check on a fresh snapshot refuses it, the stream never joins, and the next plan keeps the game out.
TEST(AudioIncludeSet, NoIncludeOnATreeThatGainedAnExcludedProcessBeforeTheCommit) {
  Rig r;
  const auto plan = r.Reconcile();
  ASSERT_EQ(plan.activate.size(), 1u);
  r.AddDiscordUnderGame(); // after the plan, before the commit
  // Wait for the activation's result without pumping, so the result lands before the commit check.
  const auto end = std::chrono::steady_clock::now() + 10s;
  while (r.pool->in_flight() > 0 && std::chrono::steady_clock::now() < end) {
    std::this_thread::sleep_for(1ms);
  }
  ASSERT_EQ(r.opener->opened.load(), 1);
  EXPECT_TRUE(r.Pump()) << "a refusal asks for a new plan";
  EXPECT_EQ(r.commits, 1);
  EXPECT_EQ(r.capture.added, 0) << "the refused stream never reached the capture thread";
  EXPECT_TRUE(r.set->Views().empty());
  EXPECT_EQ(r.set->counters().commit_refusals, 1u);
  EXPECT_EQ(r.set->counters().streams, 0u);
  bool refused = false;
  for (const auto &e : r.set->TakeEvents()) {
    refused = refused || (e.code == audiomix::ReportEventCode::kActivationFailed && e.detail == "commit:tree");
  }
  EXPECT_TRUE(refused);
  r.Run(100);
  for (const float p : r.sink.peaks) {
    ASSERT_EQ(p, 0.0f) << "nothing was mixed";
  }
  const auto next = r.Reconcile();
  EXPECT_TRUE(next.activate.empty());
  EXPECT_EQ(GameSession(next).verdict, Verdict::kNotCaptured);
  EXPECT_EQ(GameSession(next).reason, Reason::kTree);
}

// The plan itself never activates an INCLUDE stream on a tree that already holds a kept-out process (I1, I2).
TEST(AudioIncludeSet, NoActivationOnATreeThatHoldsAnExcludedProcess) {
  Rig r;
  r.AddDiscordUnderGame();
  const auto plan = r.Reconcile();
  EXPECT_TRUE(plan.activate.empty());
  EXPECT_EQ(GameSession(plan).verdict, Verdict::kNotCaptured);
  EXPECT_EQ(GameSession(plan).reason, Reason::kTree);
  r.PumpUntilActivated();
  EXPECT_EQ(r.opener->opened.load(), 0);
  EXPECT_EQ(r.gate.holds, 0);
  EXPECT_EQ(r.capture.added, 0);
}

// A kept-out process that appears in the preroll window (after the join, before 40 ms are buffered) removes the
// stream without any output (05 §6.2 step 5, §9.2).
TEST(AudioIncludeSet, AnExcludedProcessInThePrerollWindowDropsTheStreamWithoutOutput) {
  Rig r;
  r.Reconcile();
  r.PumpUntilActivated();
  ASSERT_EQ(r.GameStream(), SessionStream::kPreroll);
  r.Run(20); // 20 ms buffered of the 40 ms preroll
  ASSERT_EQ(r.GameStream(), SessionStream::kPreroll);
  r.AddDiscordUnderGame();
  const auto plan = r.Reconcile();
  ASSERT_EQ(plan.remove.size(), 1u);
  EXPECT_EQ(plan.remove[0].reason, audiopolicy::RemovalReason::kUnsafe);
  r.Run(100);
  for (const float p : r.sink.peaks) {
    ASSERT_EQ(p, 0.0f) << "a stream in preroll leaves without output";
  }
  EXPECT_TRUE(r.set->Views().empty());
  EXPECT_EQ(r.capture.removed, 1);
  EXPECT_TRUE(r.capture.live.empty());
  EXPECT_EQ(GameSession(r.Reconcile()).reason, Reason::kTree);
}

// While it runs, the same fades it out over the next 10 ms of its buffer (05 §6.2 step 6, §9.4).
TEST(AudioIncludeSet, AnExcludedProcessUnderARunningTargetFadesItOut) {
  Rig r;
  r.Reconcile();
  r.PumpUntilActivated();
  r.Run(100);
  ASSERT_EQ(r.GameStream(), SessionStream::kRunning);
  const std::size_t loud = r.sink.peaks.size();
  ASSERT_GT(r.sink.peaks.back(), 0.49f);
  r.AddDiscordUnderGame();
  r.Reconcile();
  r.Run(50);
  ASSERT_GT(r.sink.peaks.size(), loud + 2);
  EXPECT_LE(r.sink.peaks[loud], 0.5f) << "the fade chunk";
  for (std::size_t i = loud + 1; i < r.sink.peaks.size(); ++i) {
    ASSERT_EQ(r.sink.peaks[i], 0.0f) << "silent from the chunk after the fade";
  }
  EXPECT_TRUE(r.set->Views().empty());
  EXPECT_TRUE(r.capture.live.empty());
}

// ------------------------------------------------------------------------------------------- activation failures

// An activation that never completes ends at the pool's timeout (05 §8.1), counts, and is reopened after 100 ms,
// 500 ms and 2 s; when the third reopen fails too the target is capture_failed (05 §8.5) until a device or session
// event.
TEST(AudioIncludeSet, AnActivationTimeoutCountsRetriesAndThenGivesUp) {
  Rig r(100ms);
  r.opener->never = true;
  r.Reconcile();
  r.PumpUntilActivated();
  EXPECT_EQ(r.set->counters().activation_failures, 1u);
  EXPECT_EQ(r.GameStream(), SessionStream::kActivating) << "waiting for the reopen delay";
  bool timeout_event = false;
  for (const auto &e : r.set->TakeEvents()) {
    timeout_event = timeout_event || (e.code == audiomix::ReportEventCode::kActivationFailed && e.detail == "timeout");
  }
  EXPECT_TRUE(timeout_event);
  EXPECT_EQ(r.set->NextDeadline(), r.now + kReopenDelaysNs[0]);
  r.now += kReopenDelaysNs[0] - 1;
  r.PumpUntilActivated();
  EXPECT_EQ(r.opener->opened.load(), 1) << "not before the delay";
  r.now += 1;
  r.PumpUntilActivated();
  EXPECT_EQ(r.opener->opened.load(), 2);
  EXPECT_EQ(r.set->counters().activation_failures, 2u);
  r.now += kReopenDelaysNs[1];
  r.PumpUntilActivated();
  EXPECT_EQ(r.opener->opened.load(), 3);
  EXPECT_EQ(r.set->counters().activation_failures, 3u);
  EXPECT_EQ(r.GameStream(), SessionStream::kActivating) << "one more reopen, after 2 s";
  EXPECT_TRUE(r.set->CaptureFailed().empty());
  EXPECT_EQ(r.set->NextDeadline(), r.now + kReopenDelaysNs[2]);
  r.now += kReopenDelaysNs[2] - 1;
  r.PumpUntilActivated();
  EXPECT_EQ(r.opener->opened.load(), 3) << "not before the delay";
  r.now += 1;
  r.PumpUntilActivated();
  EXPECT_EQ(r.opener->opened.load(), 4);
  EXPECT_EQ(r.set->counters().activation_failures, 4u);
  EXPECT_TRUE(r.set->Views().empty());
  ASSERT_EQ(r.set->CaptureFailed().size(), 1u);
  const auto plan = r.Reconcile();
  EXPECT_TRUE(plan.activate.empty());
  EXPECT_EQ(GameSession(plan).reason, Reason::kCaptureFailed);
  // A device or session event: tried again, and this time it works.
  r.opener->never = false;
  r.set->ForgetFailures();
  EXPECT_EQ(r.Reconcile().activate.size(), 1u);
  r.PumpUntilActivated();
  EXPECT_EQ(r.GameStream(), SessionStream::kPreroll);
}

// A stream the capture thread doesn't take never joins; it is retried like a failed activation, then given up on.
TEST(AudioIncludeSet, AStreamThatCannotJoinIsRetriedThenGivenUp) {
  Rig r;
  r.capture.refuse = true;
  r.Reconcile();
  r.PumpUntilActivated();
  EXPECT_EQ(r.set->counters().activation_failures, 1u);
  EXPECT_EQ(r.set->counters().streams, 0u);
  r.now += kReopenDelaysNs[0];
  r.PumpUntilActivated();
  r.now += kReopenDelaysNs[1];
  r.PumpUntilActivated();
  EXPECT_EQ(r.opener->opened.load(), 3);
  EXPECT_EQ(r.set->counters().activation_failures, 3u);
  EXPECT_FALSE(r.set->Views().empty()) << "one more reopen, after 2 s";
  r.now += kReopenDelaysNs[2];
  r.PumpUntilActivated();
  EXPECT_EQ(r.opener->opened.load(), 4);
  EXPECT_EQ(r.set->counters().activation_failures, 4u);
  EXPECT_TRUE(r.set->Views().empty());
  EXPECT_EQ(GameSession(r.Reconcile()).reason, Reason::kCaptureFailed);
  r.Run(50);
  for (const float p : r.sink.peaks) {
    ASSERT_EQ(p, 0.0f);
  }
}

// A joined stream that fails fades out and reopens after the delay (05 §8.5).
TEST(AudioIncludeSet, AFailedStreamFadesOutAndReopens) {
  Rig r;
  r.Reconcile();
  r.PumpUntilActivated();
  r.Run(100);
  ASSERT_EQ(r.GameStream(), SessionStream::kRunning);
  ASSERT_EQ(r.capture.live.size(), 1u);
  r.capture.live[0]->failure.store(static_cast<int32_t>(0x88890004u)); // AUDCLNT_E_DEVICE_INVALIDATED
  r.capture.live[0]->failed.store(true);
  r.Run(30);
  EXPECT_EQ(r.set->counters().stream_failures, 1u);
  EXPECT_EQ(r.sink.peaks.back(), 0.0f);
  EXPECT_EQ(r.GameStream(), SessionStream::kActivating) << "waiting to reopen";
  EXPECT_TRUE(r.capture.live.empty());
  r.now += kReopenDelaysNs[0];
  r.PumpUntilActivated();
  EXPECT_EQ(r.opener->opened.load(), 2);
  EXPECT_EQ(r.GameStream(), SessionStream::kPreroll);
}

// ------------------------------------------------------------------------------------------- covered streams

// 05 §6.2 step 4: a child's stream runs; its parent starts playing, so the plan opens the parent's stream and removes
// the child's as covered. The child fades once the parent's stream runs, not later because an unrelated app's
// activation hangs meanwhile (it would be heard twice), and not earlier (it would leave a gap).
TEST(AudioIncludeSet, ACoveredStreamFadesOnceItsOwnCovererRuns) {
  Rig r;
  r.procs.push_back(Proc(300, 100, 3000, "helper.exe", "C:\\Games\\Game")); // the game's child
  r.procs.push_back(Proc(500, 4, 4000, "music.exe", "C:\\Apps\\Music"));    // unrelated
  r.playing = {300};
  ASSERT_EQ(r.Reconcile().activate.size(), 1u);
  r.PumpUntilActivated();
  r.Run(100);
  ASSERT_EQ(r.StreamOf(300), SessionStream::kRunning);

  r.opener->never_pid = 500; // its activation hangs until the pool's 3 s timeout
  r.playing = {300, 100, 500};
  const auto plan = r.Reconcile();
  ASSERT_EQ(plan.activate.size(), 2u);
  ASSERT_EQ(plan.remove.size(), 1u);
  EXPECT_EQ(plan.remove[0].reason, audiopolicy::RemovalReason::kCovered);
  PumpUntilOpened(r, 3);
  ASSERT_EQ(r.opener->opened.load(), 3);
  // The game's activation completed; music's hangs.
  const auto end = std::chrono::steady_clock::now() + 10s;
  while (r.GameStream() != SessionStream::kPreroll && std::chrono::steady_clock::now() < end) {
    r.Pump();
    std::this_thread::sleep_for(1ms);
  }
  ASSERT_EQ(r.GameStream(), SessionStream::kPreroll);
  ASSERT_EQ(r.StreamOf(500), SessionStream::kActivating);
  int64_t running_at = -1;
  int64_t faded_at = -1;
  for (int k = 0; k < 20 && faded_at < 0; ++k) {
    if (running_at < 0) {
      EXPECT_EQ(r.StreamOf(300), SessionStream::kRunning) << "no gap: it plays until the game's stream runs";
    }
    r.Run(10);
    if (running_at < 0 && r.GameStream() == SessionStream::kRunning) {
      running_at = r.now;
    }
    const SessionStream child = r.StreamOf(300);
    if (child == SessionStream::kFading || child == SessionStream::kNone) {
      faded_at = r.now;
    }
  }
  ASSERT_GE(running_at, 0) << "the game's stream runs";
  ASSERT_GE(faded_at, 0) << "the child's stream fades though music's activation still hangs";
  EXPECT_LE(faded_at - running_at, 20 * kMs);
  EXPECT_EQ(r.StreamOf(500), SessionStream::kActivating);
  r.Run(30);
  EXPECT_EQ(r.StreamOf(300), SessionStream::kNone) << "gone once the mixer let go of it";
  EXPECT_EQ(r.GameStream(), SessionStream::kRunning);
}

// A covered stream whose only coverer goes away keeps playing: nothing else captures it.
TEST(AudioIncludeSet, ACoveredStreamWithoutARunningCovererKeepsPlaying) {
  Rig r;
  r.procs.push_back(Proc(300, 100, 3000, "helper.exe", "C:\\Games\\Game"));
  r.playing = {300};
  r.Reconcile();
  r.PumpUntilActivated();
  r.Run(100);
  ASSERT_EQ(r.StreamOf(300), SessionStream::kRunning);
  r.opener->never_pid = 100; // the game's activation hangs
  r.playing = {300, 100};
  ASSERT_EQ(r.Reconcile().remove.size(), 1u);
  r.Run(200);
  EXPECT_EQ(r.StreamOf(300), SessionStream::kRunning);
  EXPECT_GT(r.sink.peaks.back(), 0.49f);
}

// 05 §8.2: the formats that failed before the one that worked go into the stream_opened event.
TEST(AudioIncludeSet, TheFailedFormatAttemptsGoIntoTheOpenedEvent) {
  Rig r;
  r.opener->failed_attempts = 2;
  r.Reconcile();
  r.PumpUntilActivated();
  ASSERT_EQ(r.GameStream(), SessionStream::kPreroll);
  ASSERT_EQ(r.set->Views().size(), 1u);
  EXPECT_EQ(r.set->Views()[0].format, "pcm16");
  bool opened = false;
  for (const auto &e : r.set->TakeEvents()) {
    opened = opened || (e.code == audiomix::ReportEventCode::kStreamOpened && e.detail == "fallback:2");
  }
  EXPECT_TRUE(opened);
}

// --------------------------------------------------------------------------------------------- held handles

TEST(AudioIncludeSet, APidThatIsAnotherProcessNowOpensNothing) {
  Rig r;
  r.gate.status = HoldStatus::kMismatch;
  r.Reconcile();
  EXPECT_TRUE(r.Pump()) << "a mismatch asks for a new plan at once (05 §5.7)";
  EXPECT_FALSE(r.Pump()) << "once";
  r.PumpUntilActivated();
  EXPECT_EQ(r.gate.holds, 1);
  EXPECT_EQ(r.opener->opened.load(), 0);
  EXPECT_TRUE(r.set->Views().empty());
  // The new plan on a fresh snapshot meets the same mismatch: it waits for the next poll instead of planning again.
  r.Reconcile();
  EXPECT_EQ(r.gate.holds, 2);
  EXPECT_FALSE(r.Pump());
  // The snapshot caught up: the target holds now and activates.
  r.gate.status = HoldStatus::kHeld;
  r.Reconcile();
  r.PumpUntilActivated();
  EXPECT_EQ(r.GameStream(), SessionStream::kPreroll);
}

TEST(AudioIncludeSet, ARefusedHandleActivatesAnywayWithTheNoteNoHandle) {
  Rig r;
  r.gate.status = HoldStatus::kNoHandle;
  r.Reconcile();
  r.PumpUntilActivated();
  ASSERT_EQ(r.set->Inputs().size(), 1u);
  EXPECT_FALSE(r.set->Inputs()[0].has_handle);
  EXPECT_EQ(r.GameStream(), SessionStream::kPreroll);
  EXPECT_EQ(GameSession(r.Reconcile()).note, audiopolicy::Note::kNoHandle);
}

TEST(AudioIncludeSet, AHeldProcessThatExitsRemovesItsStreamAtOnce) {
  Rig r;
  r.Reconcile();
  r.PumpUntilActivated();
  r.Run(100);
  ASSERT_EQ(r.GameStream(), SessionStream::kRunning);
  r.gate.exited[100]->store(true);
  EXPECT_TRUE(r.Pump()) << "an exit asks for a new plan";
  EXPECT_EQ(r.GameStream(), SessionStream::kFading);
  r.Run(30);
  EXPECT_TRUE(r.set->Views().empty());
  EXPECT_EQ(r.sink.peaks.back(), 0.0f);
}

TEST(AudioIncludeSet, DetachLetsGoOfEverything) {
  Rig r;
  r.Reconcile();
  r.PumpUntilActivated();
  r.Run(100);
  ASSERT_EQ(r.capture.live.size(), 1u);
  r.set->Detach();
  EXPECT_FALSE(r.set->attached());
  EXPECT_TRUE(r.capture.live.empty());
  EXPECT_TRUE(r.set->Views().empty());
  EXPECT_TRUE(r.set->Inputs().empty());
  // Not attached: plans open nothing.
  r.Reconcile();
  r.PumpUntilActivated();
  EXPECT_EQ(r.opener->opened.load(), 1);
}

} // namespace
} // namespace im::audio
