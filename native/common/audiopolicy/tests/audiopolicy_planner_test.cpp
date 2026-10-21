// The planner (docs/m2/05-windows-audio.md §6, §5.7, §17, §19.1): engine selection, the include set's steps 2-6
// (dirty trees, ancestor coverage, ordering, the 48-stream cap, the commit check, re-checks), step 3's fallback, the
// no-handle path, the fast path's condition, hysteresis and exits, and the report rows.
#include <gtest/gtest.h>

#include <algorithm>
#include <chrono>
#include <string>
#include <vector>

#include "policy_world.h"

namespace {

using namespace policytest;

constexpr int64_t kSec = 1'000'000'000;

const std::string kDiscordApp = std::string(kLocal) + R"(\Discord\app-1.0.9999)";
const std::string kIsshoniDir = std::string(kLocal) + R"(\Programs\isshoni)";
const std::string kGames = R"(D:\Games)";

// ----------------------------------------------------------------------------------------- step 2: dirty trees

TEST(AudioPolicyPlanner, DirtyTreesStrictLooseAndBoth) {
  World w;
  const uint32_t a = w.Spawn("gamea.exe", kGames + R"(\A)");
  const uint32_t da = w.Spawn("discord.exe", kDiscordApp, a); // strict child
  // B: a helper of B holds the pid of Discord DB's dead parent.
  const uint32_t dead = w.Spawn("update.exe", kDiscordApp);
  const uint32_t db = w.Spawn("discordptb.exe", kDiscordApp, dead);
  w.Exit(dead);
  const uint32_t b = w.Spawn("gameb.exe", kGames + R"(\B)");
  w.SpawnAt(dead, 0, "helper.exe", kGames + R"(\B)", b);
  // C: both a strict and a loose-only kept-out descendant.
  const uint32_t dead2 = w.Spawn("update.exe", kDiscordApp);
  w.Spawn("discordcanary.exe", kDiscordApp, dead2);
  w.Exit(dead2);
  const uint32_t c = w.Spawn("gamec.exe", kGames + R"(\C)");
  w.SpawnAt(dead2, 0, "helper.exe", kGames + R"(\C)", c);
  w.Spawn("zoom.exe", R"(C:\Zoom)", c);
  for (const uint32_t p : {a, b, c, da, db}) {
    w.Render(p);
  }
  Sim e(Standard().Build());
  e.Step(w, 0);
  EXPECT_EQ(e.SessionOf(a).reason, Reason::kTree);
  EXPECT_EQ(e.SessionOf(a).detail, "Discord");
  EXPECT_EQ(e.SessionOf(b).reason, Reason::kPidReuse);
  EXPECT_EQ(e.SessionOf(b).detail, "Discord");
  EXPECT_EQ(e.SessionOf(c).reason, Reason::kTree);
  EXPECT_EQ(e.SessionOf(c).detail, "Zoom");
  EXPECT_EQ(e.plan.pid_reuse_blocks, 1u);
  EXPECT_TRUE(e.plan.include_targets.empty());
  e.Step(w, kSec); // the same block isn't counted again
  EXPECT_EQ(e.plan.pid_reuse_blocks, 0u);
  // With the loose union dropped (case 27 showing Windows ignores reused-PID links), B is heard.
  PlannerOptions strict_links;
  strict_links.loose_links = false;
  Sim strict_only(Standard().Build(), strict_links);
  strict_only.Step(w, 0);
  EXPECT_EQ(strict_only.SessionOf(b).verdict, Verdict::kHeard);
  EXPECT_TRUE(strict_only.IsTarget(b));
}

// ----------------------------------------------------------------------------------------- step 3: coverage

TEST(AudioPolicyPlanner, AncestorCoversDescendants) {
  World w;
  const uint32_t game = w.Spawn("game.exe", kGames + R"(\G)");
  const uint32_t child = w.Spawn("game.exe", kGames + R"(\G)", game);
  const uint32_t grandchild = w.Spawn("other.exe", R"(C:\Elsewhere)", child);
  w.Render(game);
  w.Render(child);
  w.Render(grandchild);
  Sim e(Standard().Build());
  e.Step(w);
  EXPECT_EQ(e.plan.include_targets, std::vector<ProcessKey>{w.Key(game)});
  EXPECT_EQ(e.SessionOf(game).note, Note::kNone);
  EXPECT_TRUE(e.SessionOf(game).target);
  EXPECT_EQ(e.SessionOf(child).note, Note::kCovered);
  EXPECT_EQ(e.SessionOf(grandchild).note, Note::kCovered);
  EXPECT_EQ(e.SessionOf(grandchild).verdict, Verdict::kHeard);
  // A child that starts later is covered by the live stream (05 §6.2 step 3; case 25 checks Windows agrees).
  const uint32_t late = w.Spawn("game.exe", kGames + R"(\G)", game);
  w.Render(late);
  e.Step(w, kSec);
  EXPECT_EQ(e.SessionOf(late).note, Note::kCovered);
  EXPECT_TRUE(e.plan.activate.empty());
}

TEST(AudioPolicyPlanner, LateChildFallbackGivesLaterChildrenTheirOwnStream) {
  World w;
  const uint32_t game = w.Spawn("game.exe", kGames + R"(\G)");
  const uint32_t early = w.Spawn("game.exe", kGames + R"(\G)", game);
  w.Render(game);
  PlannerOptions opts;
  opts.late_child_own_stream = true;
  Sim e(Standard().Build(), opts);
  e.activation_clock = w.Created(early) + 5; // the game's stream was activated after `early` started
  e.Step(w, 0);
  ASSERT_TRUE(e.HasStream(game));
  w.Render(early);
  const uint32_t late = w.Spawn("game.exe", kGames + R"(\G)", game); // created after the activation
  w.Render(late);
  e.Step(w, kSec);
  EXPECT_EQ(e.SessionOf(early).note, Note::kCovered);
  EXPECT_TRUE(e.SessionOf(late).target);
  EXPECT_TRUE(e.HasStream(late));
  EXPECT_EQ(e.IncludeStreams(), 2u);
}

TEST(AudioPolicyPlanner, ANewAncestorRootReplacesItsLiveDescendant) {
  World w;
  const uint32_t game = w.Spawn("game.exe", kGames + R"(\G)");
  const uint32_t child = w.Spawn("game.exe", kGames + R"(\G)", game);
  w.Render(child);
  Sim e(Standard().Build());
  e.Step(w, 0);
  ASSERT_TRUE(e.HasStream(child));
  w.Render(game); // the parent starts playing too: its stream covers the child
  const auto before = e.streams;
  e.Step(w, kSec);
  EXPECT_EQ(e.RemovalsOf(child, before), std::vector<RemovalReason>{RemovalReason::kCovered});
  EXPECT_EQ(e.plan.include_targets, std::vector<ProcessKey>{w.Key(game)});
  EXPECT_EQ(e.SessionOf(child).note, Note::kCovered);
}

// ----------------------------------------------------------------------------------------- step 4: order, cap

TEST(AudioPolicyPlanner, OrderActiveThenRecentThenOldest) {
  World w;
  const uint32_t idle_recent = w.Spawn("a.exe", R"(C:\A)");
  const uint32_t active_old = w.Spawn("b.exe", R"(C:\B)");
  const uint32_t active_new = w.Spawn("c.exe", R"(C:\C)");
  const uint32_t tie_old = w.Spawn("d.exe", R"(C:\D)");
  const uint32_t tie_young = w.Spawn("e.exe", R"(C:\E)");
  w.Render(idle_recent, false, 900);
  w.Render(active_old, true, 100);
  w.Render(active_new, true, 500);
  w.Render(tie_young, false, 50);
  w.Render(tie_old, false, 50);
  PlannerOptions opts;
  opts.max_include_streams = 4;
  Sim e(Standard().Build(), opts);
  e.Step(w);
  const std::vector<ProcessKey> want = {w.Key(active_new), w.Key(active_old), w.Key(idle_recent), w.Key(tie_old)};
  EXPECT_EQ(e.plan.include_targets, want);
  EXPECT_EQ(e.SessionOf(tie_young).verdict, Verdict::kNotCaptured);
  EXPECT_EQ(e.SessionOf(tie_young).reason, Reason::kLimit);
}

TEST(AudioPolicyPlanner, FortyEightStreamCap) {
  World w;
  std::vector<uint32_t> apps;
  for (int i = 0; i < 50; ++i) {
    const std::string dir = R"(C:\Apps\app)" + std::to_string(i);
    apps.push_back(w.Spawn("app" + std::to_string(i) + ".exe", dir));
    w.Render(apps.back(), true, 1000 - i);
  }
  Sim e(Standard().Build());
  e.Step(w, 0);
  EXPECT_EQ(e.plan.include_targets.size(), 48u);
  EXPECT_EQ(e.IncludeStreams(), 48u);
  EXPECT_EQ(e.SessionOf(apps[48]).reason, Reason::kLimit);
  EXPECT_EQ(e.SessionOf(apps[49]).reason, Reason::kLimit);
  // Live streams keep their slots: a newly active app waits.
  const uint32_t hot = w.Spawn("hot.exe", R"(C:\Hot)");
  w.Render(hot, true, 99999);
  e.Step(w, kSec);
  EXPECT_EQ(e.SessionOf(hot).reason, Reason::kLimit);
  EXPECT_EQ(e.IncludeStreams(), 48u);
  // A slot frees when a target exits; the most active waiting app takes it.
  w.Exit(apps[0]);
  e.Step(w, 2 * kSec);
  EXPECT_TRUE(e.HasStream(hot));
  EXPECT_EQ(e.IncludeStreams(), 48u);
  EXPECT_EQ(e.SessionOf(apps[48]).reason, Reason::kLimit);
}

// ----------------------------------------------------------------------------------------- step 5: commit

TEST(AudioPolicyPlanner, CommitCheckOnAFreshSnapshot) {
  World w;
  const uint32_t game = w.Spawn("game.exe", kGames + R"(\G)");
  w.Render(game);
  Sim e(Standard().Build());
  e.planner.Reconcile(w.Table(), Input(0, w.sessions));
  Activation a{StreamKind::kInclude, w.Key(game), kExplorer, "game.exe"};
  EXPECT_TRUE(e.planner.CommitCheck(w.Table(), a).ok);
  // Discord started from the game while the activation ran.
  World w2 = w;
  w2.Spawn("discord.exe", kDiscordApp, game);
  CommitResult r = e.planner.CommitCheck(w2.Table(), a);
  EXPECT_FALSE(r.ok);
  EXPECT_EQ(r.reason, Reason::kTree);
  EXPECT_EQ(r.detail, "Discord");
  // Only through a reused pid.
  World w3 = w;
  const uint32_t dead = w3.Spawn("update.exe", kDiscordApp);
  w3.Spawn("discord.exe", kDiscordApp, dead);
  w3.Exit(dead);
  w3.SpawnAt(dead, 0, "helper.exe", kGames + R"(\G)", game);
  r = e.planner.CommitCheck(w3.Table(), a);
  EXPECT_FALSE(r.ok);
  EXPECT_EQ(r.reason, Reason::kPidReuse);
  // Gone, or its pid reused.
  World w4 = w;
  w4.Exit(game);
  EXPECT_FALSE(e.planner.CommitCheck(w4.Table(), a).ok);
  w4.SpawnAt(game, 0, "game.exe", kGames + R"(\G)", kExplorer);
  EXPECT_FALSE(e.planner.CommitCheck(w4.Table(), a).ok);
  // Kept out by a rule that arrived meanwhile.
  e.planner.SetExclusions(Standard().UserKeepOut("game.exe").Build());
  r = e.planner.CommitCheck(w.Table(), a);
  EXPECT_FALSE(r.ok);
  EXPECT_EQ(r.reason, Reason::kUser);
  // A launcher never passes.
  World w5;
  Activation l{StreamKind::kInclude, w5.Key(kExplorer), 4, "explorer.exe"};
  EXPECT_EQ(e.planner.CommitCheck(w5.Table(), l).reason, Reason::kLauncher);
}

// ----------------------------------------------------------------------------------------- step 6: re-checks

TEST(AudioPolicyPlanner, RunningTargetsAreRechecked) {
  World w;
  const uint32_t game = w.Spawn("game.exe", kGames + R"(\G)");
  const uint32_t game2 = w.Spawn("game2.exe", kGames + R"(\G2)");
  const uint32_t app = w.Spawn("fakevoice.exe", R"(C:\FakeVoice)");
  for (const uint32_t p : {game, game2, app}) {
    w.Render(p);
  }
  Sim e(Standard().Build());
  e.Step(w, 0);
  ASSERT_EQ(e.IncludeStreams(), 3u);
  // A kept-out process appears in a target's tree.
  w.Spawn("discord.exe", kDiscordApp, game);
  auto before = e.streams;
  e.Step(w, 1);
  EXPECT_EQ(e.RemovalsOf(game, before), std::vector<RemovalReason>{RemovalReason::kUnsafe});
  // A target's app becomes kept out.
  e.planner.SetExclusions(Standard().UserKeepOut("fakevoice.exe").Build());
  before = e.streams;
  e.Step(w, 2);
  EXPECT_EQ(e.RemovalsOf(app, before), std::vector<RemovalReason>{RemovalReason::kKeptOut});
  // A target exits.
  w.Exit(game2);
  before = e.streams;
  e.Step(w, 3);
  EXPECT_EQ(e.RemovalsOf(game2, before), std::vector<RemovalReason>{RemovalReason::kExited});
  EXPECT_EQ(e.IncludeStreams(), 0u);
}

TEST(AudioPolicyPlanner, NoHandlePathIsCheckedBySnapshot) {
  World w;
  const uint32_t game = w.Spawn("game.exe", kGames + R"(\G)");
  w.Render(game);
  Sim e(Standard().Build());
  e.no_handle = {game};
  e.Step(w, 0);
  e.Step(w, 1); // the stream runs without a held handle
  ASSERT_TRUE(e.HasStream(game));
  EXPECT_FALSE(e.streams[0].has_handle);
  EXPECT_EQ(e.SessionOf(game).note, Note::kNoHandle);
  // The pid comes back as another process (another creation time): the stream goes at once.
  w.Exit(game);
  w.SpawnAt(game, 0, "game.exe", kGames + R"(\G)", kExplorer);
  w.Render(game);
  auto before = e.streams;
  e.Step(w, 2);
  EXPECT_EQ(e.RemovalsOf(game, before), std::vector<RemovalReason>{RemovalReason::kKeyMismatch});
  // The new process gets its own stream (planned with its own key).
  EXPECT_TRUE(e.HasStream(game));
  EXPECT_EQ(e.streams.back().target, w.Key(game));
  // Unknown creation time: the same exe and parent pid keep it; another exe at that pid drops it.
  World u;
  const uint32_t p = u.Spawn("protected.exe", kGames + R"(\P)");
  u.Proc(p).created = 0;
  u.Render(p);
  Sim f(Standard().Build());
  f.no_handle = {p};
  f.Step(u, 0);
  before = f.streams;
  f.Step(u, 1);
  EXPECT_TRUE(f.RemovalsOf(p, before).empty());
  u.Proc(p).exe = "other.exe";
  before = f.streams;
  f.Step(u, 2);
  EXPECT_EQ(f.RemovalsOf(p, before), std::vector<RemovalReason>{RemovalReason::kKeyMismatch});
  // The same exe and parent at that pid, but now with a known creation time: another process (found by the
  // property test, seed 4133).
  World v;
  const uint32_t q = v.Spawn("protected.exe", kGames + R"(\P)");
  v.Proc(q).created = 0;
  v.Render(q);
  Sim g(Standard().Build());
  g.no_handle = {q};
  g.Step(v, 0);
  ASSERT_TRUE(g.HasStream(q));
  v.Proc(q).created = 123456;
  before = g.streams;
  g.Step(v, 1);
  EXPECT_EQ(g.RemovalsOf(q, before), std::vector<RemovalReason>{RemovalReason::kKeyMismatch});
}

TEST(AudioPolicyPlanner, VanishedSessionKeepsItsStreamForThreeSeconds) {
  World w;
  const uint32_t game = w.Spawn("game.exe", kGames + R"(\G)");
  w.Render(game);
  Sim e(Standard().Build());
  e.Step(w, 0);
  w.StopSessions(game);
  e.Step(w, 1 * kSec);
  e.Step(w, 2 * kSec);
  EXPECT_TRUE(e.HasStream(game));
  EXPECT_TRUE(e.IsTarget(game));
  w.Render(game); // back before the grace ended: the timer restarts
  e.Step(w, 2 * kSec + 500'000'000);
  w.StopSessions(game);
  e.Step(w, 3 * kSec);
  e.Step(w, 5 * kSec);
  EXPECT_TRUE(e.HasStream(game));
  auto before = e.streams;
  e.Step(w, 6 * kSec);
  EXPECT_EQ(e.RemovalsOf(game, before), std::vector<RemovalReason>{RemovalReason::kSessionGone});
}

TEST(AudioPolicyPlanner, CaptureFailedAndPendingSessions) {
  World w;
  const uint32_t game = w.Spawn("game.exe", kGames + R"(\G)");
  w.Render(game);
  Session ghost;
  ghost.pid = 424242; // its process started after the snapshot
  ghost.flow = Flow::kRender;
  w.sessions.push_back(ghost);
  Sim e(Standard().Build());
  e.capture_failed = {w.Key(game)};
  e.Step(w);
  EXPECT_EQ(e.SessionOf(game).verdict, Verdict::kNotCaptured);
  EXPECT_EQ(e.SessionOf(game).reason, Reason::kCaptureFailed);
  EXPECT_EQ(e.SessionOf(424242).verdict, Verdict::kPending);
  EXPECT_EQ(e.SessionOf(424242).process, -1);
  EXPECT_TRUE(e.plan.include_targets.empty());
}

// ----------------------------------------------------------------------------------------- engines

TEST(AudioPolicyPlanner, FastPathAtStartWhenOnlyIsshoniIsKeptOut) {
  World w;
  const uint32_t me = w.Spawn("isshoni.exe", kIsshoniDir);
  const uint32_t wv = w.Spawn("msedgewebview2.exe", R"(C:\WebView)", me);
  const uint32_t game = w.Spawn("game.exe", kGames + R"(\G)");
  const uint32_t game2 = w.Spawn("game2.exe", kGames + R"(\G2)");
  w.Render(wv);
  w.Render(game);
  w.Render(game2);
  w.Render(kExplorer); // launcher sounds are heard in exclude-self (05 §6.4 "known difference")
  Sim e(Standard().Flags(IM_EXCLF_FAST_PATH | IM_EXCLF_KEEP_OUT_MIC_USERS).Self(me).Build(me));
  e.Step(w, 0);
  EXPECT_EQ(e.plan.engine, Engine::kExcludeSelf);
  EXPECT_EQ(e.plan.exclude_target, w.Key(me));
  ASSERT_EQ(e.plan.activate.size(), 1u);
  EXPECT_EQ(e.plan.activate[0].kind, StreamKind::kExclude);
  EXPECT_TRUE(e.plan.include_targets.empty());
  EXPECT_EQ(e.SessionOf(game).verdict, Verdict::kHeard);
  EXPECT_EQ(e.SessionOf(kExplorer).verdict, Verdict::kHeard);
  EXPECT_EQ(e.SessionOf(wv).verdict, Verdict::kKeptOut);
  // Next reconcile: the stream is kept, nothing new opens.
  e.Step(w, kSec);
  EXPECT_EQ(e.plan.engine, Engine::kExcludeSelf);
  EXPECT_TRUE(e.plan.activate.empty());
  EXPECT_TRUE(e.plan.remove.empty());
  EXPECT_TRUE(e.HasStream(me, StreamKind::kExclude));
}

TEST(AudioPolicyPlanner, FastPathEntryNeedsTenSecondsWithoutABreakAndExitIsImmediate) {
  World w;
  const uint32_t me = w.Spawn("isshoni.exe", kIsshoniDir);
  const uint32_t game = w.Spawn("game.exe", kGames + R"(\G)");
  uint32_t discord = w.Spawn("discord.exe", kDiscordApp);
  w.Render(game);
  Sim e(Standard().Flags(IM_EXCLF_FAST_PATH).Self(me).Build(me));
  e.Step(w, 0);
  EXPECT_EQ(e.plan.engine, Engine::kIncludeSet);
  w.Exit(discord); // the condition holds from 1 s
  e.Step(w, 1 * kSec);
  EXPECT_EQ(e.plan.engine, Engine::kIncludeSet);
  e.Step(w, 5 * kSec);
  EXPECT_EQ(e.plan.engine, Engine::kIncludeSet);
  discord = w.Spawn("discord.exe", kDiscordApp); // Discord restarting: a break
  e.Step(w, 6 * kSec);
  w.Exit(discord);
  e.Step(w, 7 * kSec);
  e.Step(w, 12 * kSec);
  EXPECT_EQ(e.plan.engine, Engine::kIncludeSet); // 5 s since the break
  auto before = e.streams;
  e.Step(w, 17 * kSec);
  EXPECT_EQ(e.plan.engine, Engine::kExcludeSelf);
  EXPECT_EQ(e.plan.previous_engine, Engine::kIncludeSet);
  // The include streams close after the 20 ms crossfade into the EXCLUDE stream.
  EXPECT_EQ(e.RemovalsOf(game, before), std::vector<RemovalReason>{RemovalReason::kEngine});
  EXPECT_EQ(e.IncludeStreams(), 0u);
  EXPECT_TRUE(e.HasStream(me, StreamKind::kExclude));
  // A voice app appears: the fast path ends at once (the watcher cuts the EXCLUDE ring at C - 20 ms).
  discord = w.Spawn("discord.exe", kDiscordApp);
  w.Render(discord);
  before = e.streams;
  e.Step(w, 17 * kSec + 5'000'000);
  EXPECT_EQ(e.plan.engine, Engine::kIncludeSet);
  EXPECT_EQ(e.RemovalsOf(me, before), std::vector<RemovalReason>{RemovalReason::kEngine});
  EXPECT_TRUE(e.HasStream(game));
}

TEST(AudioPolicyPlanner, FastPathRootIsTheVoiceAppThatLaunchedIsshoni) {
  World w;
  const uint32_t discord = w.Spawn("discord.exe", kDiscordApp);
  const uint32_t me = w.Spawn("isshoni.exe", kIsshoniDir, discord);
  const uint32_t game = w.Spawn("game.exe", kGames + R"(\G)");
  w.Render(game);
  w.Render(discord);
  Sim e(Standard().Flags(IM_EXCLF_FAST_PATH).Self(me).Build(me));
  e.Step(w, 0);
  EXPECT_EQ(e.plan.engine, Engine::kExcludeSelf);
  EXPECT_EQ(e.plan.exclude_target, w.Key(discord));
  // A heard process inside R's tree (a browser Discord opened, playing) forbids it.
  const uint32_t browser = w.Spawn("browser.exe", R"(C:\Browser)", discord);
  w.Render(browser);
  Sim f(Standard().Flags(IM_EXCLF_FAST_PATH).Self(me).Build(me));
  f.Step(w, 0);
  EXPECT_EQ(f.plan.engine, Engine::kIncludeSet);
}

TEST(AudioPolicyPlanner, NoFastPathWithoutTheFlagOrUnderALauncher) {
  World w;
  const uint32_t me = w.Spawn("isshoni.exe", kIsshoniDir);
  const uint32_t game = w.Spawn("game.exe", kGames + R"(\G)");
  w.Render(game);
  Sim off(Standard().Flags(0).Self(me).Build(me));
  off.Step(w, 0);
  EXPECT_EQ(off.plan.engine, Engine::kIncludeSet);
  // Two kept-out trees whose only common ancestor is Explorer (a launcher).
  w.Spawn("mumble.exe", R"(C:\Mumble)");
  Sim two(Standard().Flags(IM_EXCLF_FAST_PATH).Self(me).Build(me));
  two.Step(w, 0);
  EXPECT_EQ(two.plan.engine, Engine::kIncludeSet);
  // Steam holding the mic is a kept-out process too (05 §25 q4: "not Steam").
  World s;
  const uint32_t me2 = s.Spawn("isshoni.exe", kIsshoniDir);
  const uint32_t steam = s.Spawn("steam.exe", R"(C:\Steam)");
  s.Capture(steam);
  Sim st(Standard().Flags(IM_EXCLF_FAST_PATH | IM_EXCLF_KEEP_OUT_MIC_USERS).Self(me2).Build(me2));
  st.Step(s, 0);
  EXPECT_EQ(st.plan.engine, Engine::kIncludeSet);
}

TEST(AudioPolicyPlanner, FastPathLateDescendantFallbackLeavesAndWaits) {
  World w;
  const uint32_t me = w.Spawn("isshoni.exe", kIsshoniDir);
  const uint32_t game = w.Spawn("game.exe", kGames + R"(\G)");
  w.Render(game);
  PlannerOptions opts;
  opts.fast_path_exit_on_new_descendant = true;
  Sim e(Standard().Flags(IM_EXCLF_FAST_PATH).Self(me).Build(me), opts);
  e.Step(w, 0);
  ASSERT_EQ(e.plan.engine, Engine::kExcludeSelf);
  const uint32_t helper = w.Spawn("msedgewebview2.exe", R"(C:\WebView)", me);
  w.Render(helper); // a new isshoni descendant's session
  e.Step(w, kSec);
  EXPECT_EQ(e.plan.engine, Engine::kIncludeSet);
  e.Step(w, 10 * kSec);
  EXPECT_EQ(e.plan.engine, Engine::kIncludeSet);
  e.Step(w, 11 * kSec);
  EXPECT_EQ(e.plan.engine, Engine::kExcludeSelf);
}

TEST(AudioPolicyPlanner, EndpointWhenTheProbeFailsAndBack) {
  World w;
  const uint32_t me = w.Spawn("isshoni.exe", kIsshoniDir);
  const uint32_t game = w.Spawn("game.exe", kGames + R"(\G)");
  const uint32_t discord = w.Spawn("discord.exe", kDiscordApp);
  w.Render(game);
  w.Render(discord);
  Sim e(Standard().Flags(IM_EXCLF_FAST_PATH).Self(me).Build(me));
  e.Step(w, 0);
  ASSERT_TRUE(e.HasStream(game));
  auto before = e.streams;
  e.Step(w, kSec, IM_AUDIO_MODE_EXCLUDE_APPS, false);
  EXPECT_EQ(e.plan.engine, Engine::kEndpoint);
  EXPECT_EQ(e.RemovalsOf(game, before), std::vector<RemovalReason>{RemovalReason::kEngine});
  EXPECT_TRUE(e.HasStream(0, StreamKind::kEndpoint));
  EXPECT_EQ(e.SessionOf(discord).verdict, Verdict::kKeptOut); // the policy's verdict; the engine says endpoint
  EXPECT_EQ(e.SessionOf(game).verdict, Verdict::kHeard);
  e.Step(w, 2 * kSec, IM_AUDIO_MODE_EXCLUDE_APPS, false);
  EXPECT_TRUE(e.plan.activate.empty());
  // The re-probe passes; Discord has quit meanwhile, so the fast path applies at once, as at start.
  w.Exit(discord);
  before = e.streams;
  e.Step(w, 3 * kSec);
  EXPECT_EQ(e.plan.engine, Engine::kExcludeSelf);
  EXPECT_EQ(e.RemovalsOf(0, before), std::vector<RemovalReason>{RemovalReason::kEngine});
}

TEST(AudioPolicyPlanner, SoundOffClosesCaptureButKeepsTheReportCurrent) {
  World w;
  const uint32_t game = w.Spawn("game.exe", kGames + R"(\G)");
  const uint32_t discord = w.Spawn("discord.exe", kDiscordApp);
  w.Render(game);
  w.Render(discord);
  Sim e(Standard().Build());
  e.Step(w, 0);
  ASSERT_TRUE(e.HasStream(game));
  auto before = e.streams;
  e.Step(w, kSec, IM_AUDIO_MODE_OFF);
  EXPECT_EQ(e.plan.engine, Engine::kNone);
  EXPECT_EQ(e.RemovalsOf(game, before), std::vector<RemovalReason>{RemovalReason::kEngine});
  EXPECT_TRUE(e.plan.activate.empty());
  EXPECT_TRUE(e.plan.include_targets.empty());
  EXPECT_EQ(e.SessionOf(game).verdict, Verdict::kHeard); // what friends will hear when sound is back on
  EXPECT_FALSE(e.SessionOf(game).target);
  EXPECT_EQ(e.SessionOf(discord).verdict, Verdict::kKeptOut);
  e.Step(w, 2 * kSec);
  EXPECT_EQ(e.plan.engine, Engine::kIncludeSet);
  EXPECT_TRUE(e.HasStream(game));
  // Modes without capture (04's test tone) plan nothing either.
  e.Step(w, 3 * kSec, IM_AUDIO_MODE_TEST_TONE);
  EXPECT_EQ(e.plan.engine, Engine::kNone);
}

TEST(AudioPolicyPlanner, RestartChoosesTheEngineAsAtStart) {
  World w;
  const uint32_t me = w.Spawn("isshoni.exe", kIsshoniDir);
  const uint32_t discord = w.Spawn("discord.exe", kDiscordApp);
  const uint32_t game = w.Spawn("game.exe", kGames + R"(\G)");
  w.Render(game);
  Sim e(Standard().Flags(IM_EXCLF_FAST_PATH).Self(me).Build(me));
  e.Step(w, 0);
  w.Exit(discord);
  e.Step(w, kSec);
  EXPECT_EQ(e.plan.engine, Engine::kIncludeSet);
  e.planner.Restart(); // a new share
  e.Step(w, 2 * kSec);
  EXPECT_EQ(e.plan.engine, Engine::kExcludeSelf);
}

TEST(AudioPolicyPlanner, ReservedEverythingModePlansNoCapture) {
  // README §3 item 15: every layer refuses IM_AUDIO_MODE_EVERYTHING in M2; should one reach the planner, it plans no
  // stream rather than a mode whose "everything except isshoni" M2 doesn't offer.
  World w;
  const uint32_t me = w.Spawn("isshoni.exe", kIsshoniDir);
  const uint32_t discord = w.Spawn("discord.exe", kDiscordApp);
  const uint32_t game = w.Spawn("game.exe", kGames + R"(\G)");
  w.Render(discord);
  w.Render(game);
  Sim e(Standard().Build(me));
  e.Step(w, 0);
  ASSERT_TRUE(e.HasStream(game));
  auto before = e.streams;
  e.Step(w, kSec, IM_AUDIO_MODE_EVERYTHING);
  EXPECT_EQ(e.plan.engine, Engine::kNone);
  EXPECT_TRUE(e.plan.activate.empty());
  EXPECT_EQ(e.RemovalsOf(game, before), std::vector<RemovalReason>{RemovalReason::kEngine});
  EXPECT_EQ(e.SessionOf(discord).verdict, Verdict::kKeptOut);
}

// ----------------------------------------------------------------------------------------- report rows

TEST(AudioPolicyPlanner, AppRows) {
  World w;
  const uint32_t me = w.Spawn("isshoni.exe", kIsshoniDir);
  const uint32_t discord = w.Spawn("discord.exe", kDiscordApp); // running, silent
  const uint32_t game = w.Spawn("game.exe", kGames + R"(\G)");  // shared, silent
  const uint32_t multi = w.Spawn("foo.exe", R"(C:\Foo)");
  const uint32_t quiet = w.Spawn("quiet.exe", R"(C:\Quiet)"); // no session: no row
  Session s;
  s.pid = multi;
  s.flow = Flow::kRender;
  s.state = SessionState::kActive;
  s.multi_process = true;
  s.playing = true;
  w.sessions.push_back(s);
  Sim e(Standard().Self(me).Shared(game).Build(me));
  e.Step(w);
  ASSERT_NE(e.App("discord"), nullptr);
  EXPECT_EQ(e.App("discord")->verdict, Verdict::kKeptOut);
  EXPECT_EQ(e.App("discord")->sessions, 0u);
  EXPECT_FALSE(e.App("discord")->playing);
  ASSERT_NE(e.App("exe:game.exe"), nullptr);
  EXPECT_TRUE(e.App("exe:game.exe")->shared);
  EXPECT_EQ(e.App("exe:game.exe")->verdict, Verdict::kHeard);
  ASSERT_NE(e.App("exe:foo.exe"), nullptr);
  EXPECT_TRUE(e.App("exe:foo.exe")->warnings & kWarnMultiProcessSession);
  EXPECT_TRUE(e.App("exe:foo.exe")->playing);
  EXPECT_EQ(e.App("exe:foo.exe")->streams, 1u);
  EXPECT_EQ(e.App("exe:quiet.exe"), nullptr);
  EXPECT_EQ(e.App("isshoni"), nullptr); // no session
  ASSERT_EQ(e.plan.warnings.size(), 1u);
  EXPECT_EQ(e.plan.warnings[0].code, "multi_process_session");
  EXPECT_EQ(e.plan.warnings[0].app, "exe:foo.exe");
  (void)discord;
  (void)quiet;
}

TEST(AudioPolicyPlanner, ReportNames) {
  EXPECT_EQ(VerdictName(Verdict::kKeptOut), "kept_out");
  EXPECT_EQ(VerdictName(Verdict::kNotCaptured), "not_captured");
  EXPECT_EQ(ReasonName(Reason::kPidReuse), "pid_reuse");
  EXPECT_EQ(ReasonName(Reason::kCaptureFailed), "capture_failed");
  EXPECT_EQ(ReasonName(Reason::kNone), "");
  EXPECT_EQ(EngineName(Engine::kExcludeSelf), "exclude-self");
  EXPECT_EQ(EngineName(Engine::kIncludeSet), "include-set");
  EXPECT_EQ(NoteName(Note::kNoHandle), "no_handle");
  EXPECT_EQ(NoteName(Note::kCovered), "covered");
  EXPECT_EQ(FlowName(Flow::kCapture), "capture");
  EXPECT_EQ(WarningName(kWarnSharedAppUsesMic), "shared_app_uses_mic");
  EXPECT_EQ(WarningName(kWarnIncludedUsesMic), "included_uses_mic");
  EXPECT_EQ(WarningName(kWarnMultiProcessSession), "multi_process_session");
  EXPECT_EQ(WarningName(kWarnSharedAppKeptOut), "shared_app_kept_out");
  EXPECT_EQ(WarningName(3), "");
  EXPECT_EQ(RemovalReasonName(RemovalReason::kSessionGone), "session_gone");
}

// ----------------------------------------------------------------------------------------- invariants, budget

TEST(AudioPolicyPlanner, InvariantCheckerCatchesBrokenPlans) {
  World w;
  const uint32_t game = w.Spawn("game.exe", kGames + R"(\G)");
  const uint32_t discord = w.Spawn("discord.exe", kDiscordApp, game);
  w.Render(game);
  Sim e(Standard().Build());
  e.Step(w);
  ASSERT_TRUE(e.plan.include_targets.empty());
  const ProcessTable t = w.Table();
  const PlanInput in = Input(0, w.sessions);
  Plan bad = e.plan;
  bad.activate.push_back(Activation{StreamKind::kInclude, w.Key(game), kExplorer, "game.exe"});
  bad.activate.push_back(Activation{StreamKind::kInclude, w.Key(discord), game, "discord.exe"});
  bad.activate.push_back(Activation{StreamKind::kInclude, w.Key(kExplorer), 4, "explorer.exe"});
  bad.activate.push_back(Activation{StreamKind::kInclude, ProcessKey{game, 1}, kExplorer, "game.exe"});
  bad.activate.push_back(Activation{StreamKind::kEndpoint, {}, 0, {}});
  bad.activate.push_back(Activation{StreamKind::kExclude, w.Key(game), kExplorer, "game.exe"});
  bad.engine = Engine::kExcludeSelf;
  bad.exclude_target = w.Key(game); // R's tree holds a heard session (the game's own)
  const auto v = CheckInvariants(t, in, bad, e.planner.exclusions(), e.planner.options());
  const auto has = [&](Invariant i) {
    return std::any_of(v.begin(), v.end(), [&](const Violation &x) { return x.invariant == i; });
  };
  EXPECT_TRUE(has(Invariant::kI1)); // Discord in the game's tree
  EXPECT_TRUE(has(Invariant::kI2)); // Discord itself; Explorer (launcher, and Discord below it)
  EXPECT_TRUE(has(Invariant::kI3)); // the game's pid with another creation time
  EXPECT_TRUE(has(Invariant::kI4)); // the game's heard session inside R's tree
  EXPECT_TRUE(has(Invariant::kI5)); // two exclusive streams
  EXPECT_TRUE(CheckInvariants(t, in, e.plan, e.planner.exclusions(), e.planner.options()).empty());
  std::vector<LiveStream> two(2);
  two[0].kind = StreamKind::kExclude;
  two[1].kind = StreamKind::kEndpoint;
  EXPECT_EQ(CheckExclusiveStreams(two, false).size(), 1u);
  EXPECT_TRUE(CheckExclusiveStreams(two, true).empty());
}

TEST(AudioPolicyPlanner, ReconcileBudget) {
  // 05 §7.3: one reconcile <= 5 ms with 300 processes and 40 sessions (activations excluded).
  World w;
  const uint32_t me = w.Spawn("isshoni.exe", kIsshoniDir);
  std::vector<uint32_t> roots;
  for (int i = 0; i < 60; ++i) {
    const std::string dir = R"(C:\Apps\app)" + std::to_string(i);
    uint32_t parent = w.Spawn("app" + std::to_string(i) + ".exe", dir);
    roots.push_back(parent);
    for (int k = 0; k < 4; ++k) {
      parent = w.Spawn("helper.exe", dir + R"(\bin)", parent);
    }
  }
  w.Spawn("discord.exe", kDiscordApp, roots[3]);
  for (int i = 0; i < 40; ++i) {
    w.Render(roots[static_cast<std::size_t>(i)], i % 2 == 0, i);
  }
  ASSERT_GE(w.procs.size(), 300u);
  Sim e(Standard().Flags(IM_EXCLF_FAST_PATH | IM_EXCLF_KEEP_OUT_MIC_USERS).Self(me).Build(me));
  const ProcessTable t = w.Table();
  std::vector<double> ms;
  for (int i = 0; i < 21; ++i) {
    const auto start = std::chrono::steady_clock::now();
    const Plan p = e.planner.Reconcile(t, Input(i * kSec, w.sessions));
    ms.push_back(std::chrono::duration<double, std::milli>(std::chrono::steady_clock::now() - start).count());
    ASSERT_EQ(p.include_targets.size(), 39u); // one app has Discord in its tree
  }
  std::sort(ms.begin(), ms.end());
  const double median = ms[ms.size() / 2];
  RecordProperty("median_ms", std::to_string(median));
#if defined(NDEBUG) && !defined(__SANITIZE_THREAD__) && !defined(__SANITIZE_ADDRESS__)
#if defined(__has_feature)
#if !__has_feature(thread_sanitizer) && !__has_feature(address_sanitizer)
  EXPECT_LE(median, 5.0);
#endif
#else
  EXPECT_LE(median, 5.0);
#endif
#endif
}

} // namespace
