// The audiotest cases of docs/m2/05-windows-audio.md §18.5 as pure-logic mirrors (§19.1): the same process trees,
// rules and timelines, checked on the policy's verdicts and plans instead of on captured tones. The harness is the
// DLL's own process (the implicit instance rule); its fixtures run under Explorer, except where a case makes them the
// harness's children.
#include <gtest/gtest.h>

#include <string>
#include <vector>

#include "policy_world.h"

namespace {

using namespace policytest;

constexpr int64_t kSec = 1'000'000'000;

const std::string kRoot = std::string(kLocal) + R"(\Temp\isshoni-audiotest)";
const std::string kDiscord = kRoot + R"(\Discord\app-1.0.9999)";
const std::string kGame = kRoot + R"(\Games)";

// The harness and the rules Go's Service sends in a gate run: data, built-ins and the harness itself.
struct Case {
  World w;
  uint32_t harness = 0;
  uint32_t game = 0;

  Case() {
    harness = w.Spawn("isshoni-audiotest.exe", kRoot);
    game = w.Spawn("game.exe", kGame);
    w.Render(game);
  }
  RuleBuilder Rules(uint32_t flags = IM_EXCLF_KEEP_OUT_MIC_USERS | IM_EXCLF_FAST_PATH) const {
    RuleBuilder b = Standard();
    b.Self(harness, kRoot + R"(\SelfWebView\data)").Flags(flags);
    return b;
  }
};

TEST(AudioPolicyCases, Case01BaselineEndpointAnd19ProbeFallback) {
  Case c;
  const uint32_t d = c.w.Spawn("discord.exe", kDiscord);
  c.w.Render(d);
  Sim e(c.Rules().Build(c.harness));
  e.Step(c.w, 0, IM_AUDIO_MODE_EXCLUDE_APPS, false); // IM_AUDIOF_TEST_FAIL_PROBE
  EXPECT_EQ(e.plan.engine, Engine::kEndpoint);
  EXPECT_TRUE(e.HasStream(0, StreamKind::kEndpoint));
  EXPECT_TRUE(e.plan.include_targets.empty());
}

TEST(AudioPolicyCases, Case02BuiltinVoice) {
  Case c;
  const uint32_t d = c.w.Spawn("discord.exe", kDiscord);
  c.w.Render(d);
  Sim e(c.Rules().Build(c.harness));
  e.Step(c.w);
  EXPECT_EQ(e.plan.engine, Engine::kIncludeSet);
  EXPECT_TRUE(e.IsTarget(c.game));
  EXPECT_FALSE(e.IsTarget(d));
  ASSERT_NE(e.App("discord"), nullptr);
  EXPECT_EQ(e.App("discord")->verdict, Verdict::kKeptOut);
  EXPECT_EQ(e.App("discord")->reason, Reason::kBuiltin);
}

TEST(AudioPolicyCases, Case03TwoVoiceApps) {
  Case c;
  const uint32_t d = c.w.Spawn("discord.exe", kDiscord);
  const uint32_t ts = c.w.Spawn("teamspeak.exe", kRoot + R"(\TeamSpeak)");
  c.w.Render(d);
  c.w.Render(ts);
  Sim e(c.Rules().Build(c.harness));
  for (int t = 0; t < 12; ++t) { // "engine include-set throughout"
    e.Step(c.w, t * kSec);
    EXPECT_EQ(e.plan.engine, Engine::kIncludeSet);
  }
  EXPECT_EQ(e.SessionOf(ts).reason, Reason::kBuiltin);
  EXPECT_EQ(e.ProcessOf(ts).app_id, "teamspeak");
  EXPECT_EQ(e.plan.include_targets, std::vector<ProcessKey>{c.w.Key(c.game)});
}

TEST(AudioPolicyCases, Case04FastPathAnd05LateVoice) {
  Case c;
  const uint32_t game2 = c.w.Spawn("game2.exe", kGame + R"(2)");
  c.w.Render(game2);
  c.w.Render(c.harness); // the harness plays 907 itself
  Sim e(c.Rules().Build(c.harness));
  for (int t = 0; t < 3; ++t) {
    e.Step(c.w, t * kSec);
    EXPECT_EQ(e.plan.engine, Engine::kExcludeSelf);
    EXPECT_EQ(e.plan.exclude_target, c.w.Key(c.harness));
  }
  EXPECT_EQ(e.SessionOf(c.harness).reason, Reason::kIsshoni);
  // Case 5: at t = 3 s Discord starts and plays at once; its session event plans the exit (no debounce).
  const uint32_t d = c.w.Spawn("discord.exe", kDiscord);
  c.w.Render(d);
  const auto before = e.streams;
  e.Step(c.w, 3 * kSec);
  EXPECT_EQ(e.plan.previous_engine, Engine::kExcludeSelf);
  EXPECT_EQ(e.plan.engine, Engine::kIncludeSet);
  EXPECT_EQ(e.RemovalsOf(c.harness, before), std::vector<RemovalReason>{RemovalReason::kEngine});
  EXPECT_TRUE(e.HasStream(c.game));
  EXPECT_TRUE(e.HasStream(game2));
  EXPECT_FALSE(e.HasStream(d));
}

TEST(AudioPolicyCases, Case06HelperTree) {
  Case c;
  const uint32_t d = c.w.Spawn("discord.exe", kDiscord);
  const uint32_t vh = c.w.Spawn("voicehelper.exe", kDiscord + R"(\modules)", d);
  const uint32_t ah = c.w.Spawn("audiohelper.exe", kDiscord + R"(\modules\audio)", vh);
  const uint32_t teams = c.w.Spawn("ms-teams.exe", kRoot + R"(\Teams)");
  const uint32_t wv = c.w.Spawn("msedgewebview2.exe", kRoot + R"(\WebView2Runtime)", teams);
  c.w.Render(ah);
  c.w.Render(wv);
  Sim e(c.Rules().Build(c.harness));
  e.Step(c.w);
  EXPECT_EQ(e.SessionOf(ah).reason, Reason::kBuiltin);
  EXPECT_EQ(e.SessionOf(wv).reason, Reason::kBuiltin);
  EXPECT_EQ(e.ProcessOf(wv).app_id, "teams");
  EXPECT_TRUE(e.IsTarget(c.game));
}

TEST(AudioPolicyCases, Case07LateHelper) {
  Case c;
  const uint32_t d = c.w.Spawn("discord.exe", kDiscord);
  Sim e(c.Rules().Build(c.harness));
  e.Step(c.w, 0);
  const uint32_t ah = c.w.Spawn("audiohelper.exe", kDiscord, d); // plays 440 at once
  c.w.Render(ah);
  e.Step(c.w, 3 * kSec);
  EXPECT_EQ(e.SessionOf(ah).verdict, Verdict::kKeptOut);
  EXPECT_FALSE(e.HasStream(ah)); // the include set never captured it
}

TEST(AudioPolicyCases, Case08LateApp) {
  Case c;
  c.w.Spawn("discord.exe", kDiscord); // idle: forces the include set
  Sim e(c.Rules().Build(c.harness));
  e.Step(c.w, 0);
  const uint32_t game2 = c.w.Spawn("game2.exe", kGame + R"(2)");
  c.w.Render(game2);
  e.Step(c.w, 3 * kSec);
  ASSERT_EQ(e.plan.activate.size(), 1u);
  EXPECT_EQ(e.plan.activate[0].target, c.w.Key(game2));
}

TEST(AudioPolicyCases, Case09SteamGame) {
  World w;
  const uint32_t harness = w.Spawn("isshoni-audiotest.exe", kRoot);
  const uint32_t steam = w.Spawn("steam.exe", kRoot + R"(\Steam)");
  const uint32_t game = w.Spawn("game.exe", kRoot + R"(\Steam\steamapps\common\TestGame)", steam);
  w.Capture(steam);
  w.Render(game);
  Sim e(Standard().Self(harness).Flags(IM_EXCLF_KEEP_OUT_MIC_USERS | IM_EXCLF_FAST_PATH).Build(harness));
  e.Step(w);
  EXPECT_TRUE(e.IsTarget(game));
  ASSERT_NE(e.App("exe:steam.exe"), nullptr);
  EXPECT_EQ(e.App("exe:steam.exe")->reason, Reason::kMic);
  ASSERT_NE(e.App("exe:game.exe"), nullptr);
  EXPECT_EQ(e.App("exe:game.exe")->verdict, Verdict::kHeard);
}

TEST(AudioPolicyCases, Case10IsshoniSelf) {
  Case c;
  c.w.Render(c.harness); // 797
  const uint32_t wv = c.w.Spawn("msedgewebview2.exe", kRoot + R"(\SelfWebView)", c.harness);
  const uint32_t wv2 = c.w.Spawn("msedgewebview2.exe", kRoot + R"(\SelfWebView)", wv);
  c.w.Render(wv2); // 907
  Sim e(c.Rules().Build(c.harness));
  e.Step(c.w);
  EXPECT_EQ(e.SessionOf(c.harness).reason, Reason::kIsshoni);
  EXPECT_EQ(e.SessionOf(wv2).reason, Reason::kIsshoni);
  ASSERT_NE(e.App("isshoni"), nullptr);
  EXPECT_EQ(e.App("isshoni")->sessions, 2u);
}

TEST(AudioPolicyCases, Case11ViewerBrowserAnd24WithASavedInclude) {
  for (const bool saved_include : {false, true}) {
    SCOPED_TRACE(saved_include ? "case 24" : "case 11");
    Case c;
    const std::string edge = kRoot + R"(\Microsoft\Edge\Application)";
    const uint32_t top = c.w.Spawn("msedge.exe", edge);
    const uint32_t tab = c.w.Spawn("msedge.exe", edge, top);   // the isshoni tab, 440
    const uint32_t other = c.w.Spawn("msedge.exe", edge, tab); // another tab, 797
    c.w.Render(tab);
    c.w.Render(other);
    RuleBuilder b = c.Rules();
    if (saved_include) {
      b.UserInclude("msedge.exe");
    }
    b.Viewer("msedge.exe"); // SetViewerBrowser("c_test1", "edge", "windows", true)
    Sim e(b.Build(c.harness));
    e.Step(c.w);
    EXPECT_EQ(e.SessionOf(tab).reason, Reason::kViewer);
    EXPECT_EQ(e.SessionOf(other).reason, Reason::kViewer);
    EXPECT_TRUE(e.IsTarget(c.game));
  }
}

TEST(AudioPolicyCases, Case12BrowserMic) {
  Case c;
  const std::string chrome = kRoot + R"(\Google\Chrome\Application)";
  const uint32_t top = c.w.Spawn("chrome.exe", chrome);
  const uint32_t web = c.w.Spawn("chrome.exe", chrome, top); // Discord Web: 440 and the mic
  const uint32_t other = c.w.Spawn("chrome.exe", chrome, top);
  c.w.Render(web);
  c.w.Capture(web);
  c.w.Render(other);
  Sim e(c.Rules().Build(c.harness));
  e.Step(c.w);
  EXPECT_EQ(e.SessionOf(web).reason, Reason::kMic);
  EXPECT_EQ(e.SessionOf(other).reason, Reason::kMic);
  ASSERT_NE(e.App("exe:chrome.exe"), nullptr);
  EXPECT_TRUE(e.App("exe:chrome.exe")->browser); // the browser-mic prompt
}

TEST(AudioPolicyCases, Case13MicUserIncludedByThePrompt) {
  Case c;
  const uint32_t kook = c.w.Spawn("kook.exe", kRoot + R"(\KOOK)");
  c.w.Render(kook);
  c.w.Capture(kook);
  Sim e(c.Rules().Build(c.harness));
  e.Step(c.w, 0);
  EXPECT_EQ(e.SessionOf(kook).reason, Reason::kMic);
  // At t = 5 s the case answers "include" (Service.Decide): a row-5 allow rule.
  e.planner.SetExclusions(c.Rules().UserInclude("kook.exe").Build(c.harness));
  e.Step(c.w, 5 * kSec);
  EXPECT_EQ(e.SessionOf(kook).verdict, Verdict::kHeard);
  EXPECT_TRUE(e.HasStream(kook));
  EXPECT_TRUE(e.App("exe:kook.exe")->warnings & kWarnIncludedUsesMic);
}

TEST(AudioPolicyCases, Case14SharedAppMic) {
  Case c;
  c.w.Capture(c.game);
  Sim e(c.Rules().Shared(c.game).Build(c.harness));
  e.Step(c.w);
  EXPECT_EQ(e.plan.engine, Engine::kExcludeSelf); // the shared game's mic doesn't make it a kept-out tree
  EXPECT_EQ(e.SessionOf(c.game).verdict, Verdict::kHeard);
  EXPECT_TRUE(e.App("exe:game.exe")->warnings & kWarnSharedAppUsesMic);
}

TEST(AudioPolicyCases, Case15RuntimeRule) {
  for (const bool fast_path : {false, true}) {
    SCOPED_TRACE(fast_path ? "fast path on" : "fast path off");
    Case c;
    const uint32_t fv = c.w.Spawn("fakevoice.exe", kRoot + R"(\FakeVoice)");
    c.w.Render(fv);
    const uint32_t flags = IM_EXCLF_KEEP_OUT_MIC_USERS | (fast_path ? IM_EXCLF_FAST_PATH : 0u);
    Sim e(c.Rules(flags).Build(c.harness));
    e.Step(c.w, 0);
    EXPECT_EQ(e.SessionOf(fv).verdict, Verdict::kHeard);
    // At t = 4 s: SetAppDecision("exe:fakevoice.exe", "keep-out") -> im_set_exclusions -> a reconcile at once.
    e.planner.SetExclusions(c.Rules(flags).UserKeepOut("fakevoice.exe").Build(c.harness));
    const auto before = e.streams;
    e.Step(c.w, 4 * kSec + 50'000'000);
    EXPECT_EQ(e.SessionOf(fv).reason, Reason::kUser);
    EXPECT_EQ(e.plan.engine, Engine::kIncludeSet);
    if (fast_path) { // a second kept-out tree ends the fast path at once
      EXPECT_EQ(e.RemovalsOf(c.harness, before), std::vector<RemovalReason>{RemovalReason::kEngine});
    } else {
      EXPECT_EQ(e.RemovalsOf(fv, before), std::vector<RemovalReason>{RemovalReason::kKeptOut});
    }
    EXPECT_FALSE(e.HasStream(fv));
  }
}

TEST(AudioPolicyCases, Case16AppBoundary) {
  Case c;
  const uint32_t d = c.w.Spawn("discord.exe", kDiscord);
  const uint32_t browser = c.w.Spawn("browser.exe", kRoot + R"(\Browser)", d);
  c.w.Render(browser);
  Sim e(c.Rules().Build(c.harness));
  e.Step(c.w);
  EXPECT_TRUE(e.IsTarget(browser));
}

TEST(AudioPolicyCases, Case17TreeContainsExcluded) {
  Case c;
  const uint32_t lx = c.w.Spawn("launcherx.exe", kRoot + R"(\LauncherX)");
  const uint32_t d = c.w.Spawn("discord.exe", kDiscord, lx);
  c.w.Render(lx);
  c.w.Render(d);
  Sim e(c.Rules().Build(c.harness));
  e.Step(c.w);
  ASSERT_NE(e.App("exe:launcherx.exe"), nullptr);
  EXPECT_EQ(e.App("exe:launcherx.exe")->verdict, Verdict::kNotCaptured);
  EXPECT_EQ(e.App("exe:launcherx.exe")->reason, Reason::kTree);
  EXPECT_EQ(e.App("exe:launcherx.exe")->detail, "Discord");
}

TEST(AudioPolicyCases, Case18Orphan) {
  Case c;
  const uint32_t d = c.w.Spawn("discord.exe", kDiscord);
  const uint32_t vh = c.w.Spawn("voicehelper.exe", kDiscord, d);
  c.w.Render(vh);
  Sim e(c.Rules().Build(c.harness));
  for (int t = 0; t <= 6; ++t) {
    if (t == 3) {
      c.w.Exit(d); // the parent exits 3 s into the recording
    }
    e.Step(c.w, t * kSec);
    EXPECT_EQ(e.SessionOf(vh).verdict, Verdict::kKeptOut) << "t=" << t;
    EXPECT_FALSE(e.HasStream(vh));
  }
}

TEST(AudioPolicyCases, Case25LateChildHeard) {
  Case c;
  Sim e(c.Rules(IM_EXCLF_KEEP_OUT_MIC_USERS).Build(c.harness)); // include set
  e.Step(c.w, 0);
  const uint32_t child = c.w.Spawn("game.exe", kGame, c.game); // at t = 3 s, 683
  c.w.Render(child);
  e.Step(c.w, 3 * kSec);
  EXPECT_EQ(e.SessionOf(child).note, Note::kCovered);
  EXPECT_TRUE(e.plan.activate.empty());
  ASSERT_NE(e.App("exe:game.exe"), nullptr);
  EXPECT_EQ(e.App("exe:game.exe")->verdict, Verdict::kHeard);
  EXPECT_EQ(e.App("exe:game.exe")->sessions, 2u); // one row: same folder, same app
}

TEST(AudioPolicyCases, Case26FastPathLateChild) {
  for (const bool fallback : {false, true}) {
    SCOPED_TRACE(fallback ? "fallback on" : "fallback off");
    Case c;
    const uint32_t game2 = c.w.Spawn("game2.exe", kGame + R"(2)");
    c.w.Render(game2);
    PlannerOptions opts;
    opts.fast_path_exit_on_new_descendant = fallback;
    Sim e(c.Rules().Build(c.harness), opts);
    e.Step(c.w, 0);
    ASSERT_EQ(e.plan.engine, Engine::kExcludeSelf);
    const uint32_t wv = c.w.Spawn("msedgewebview2.exe", kRoot + R"(\SelfWebView)", c.harness);
    c.w.Render(wv); // 907, an isshoni descendant created after the EXCLUDE stream
    e.Step(c.w, 3 * kSec);
    EXPECT_EQ(e.SessionOf(wv).reason, Reason::kIsshoni);
    EXPECT_EQ(e.plan.engine, fallback ? Engine::kIncludeSet : Engine::kExcludeSelf);
  }
}

TEST(AudioPolicyCases, Case27PidReuseTree) {
  Case c;
  const uint32_t a = c.w.Spawn("launcherx.exe", kRoot + R"(\LauncherX)");
  const uint32_t b = c.w.Spawn("discord.exe", kDiscord, a); // B plays the chime partials
  c.w.Render(b);
  Sim e(c.Rules().Build(c.harness));
  e.Step(c.w, 0);
  EXPECT_TRUE(e.HasStream(c.game));
  c.w.Exit(a);                                       // A exits at 2 s; the harness closes its handles
  c.w.SpawnAt(a, 0, "reuse-pid.exe", kGame, c.game); // `fixture reuse-pid -target <A> -parent <game>` matched
  const auto before = e.streams;
  e.Step(c.w, 3 * kSec);
  EXPECT_EQ(e.SessionOf(c.game).reason, Reason::kPidReuse); // gameReason: pid_reuse, never tree
  EXPECT_EQ(e.SessionOf(c.game).detail, "Discord");
  EXPECT_EQ(e.RemovalsOf(c.game, before), std::vector<RemovalReason>{RemovalReason::kUnsafe});
  EXPECT_EQ(e.plan.pid_reuse_blocks, 1u);
}

} // namespace
