// Classification (docs/m2/05-windows-audio.md §5.2-§5.10, §19.1): every row of §5.3 and its precedence, the §5.10
// examples, the app walk (folders, launchers, shared runtimes), strict and loose links, mic stickiness, the shared app
// and the lineage cache.
#include <gtest/gtest.h>

#include <string>

#include "policy_world.h"

namespace {

using namespace policytest;

const std::string kDiscordDir = std::string(kLocal) + R"(\Discord)";
const std::string kDiscordApp = kDiscordDir + R"(\app-1.0.9999)";
const std::string kIsshoniDir = std::string(kLocal) + R"(\Programs\isshoni)";
const std::string kWebViewData = std::string(kLocal) + R"(\isshoni\WebView2)";
const std::string kEdgeWebView = R"(C:\Program Files (x86)\Microsoft\EdgeWebView\Application\130.0.2849.80)";
const std::string kSteamDir = R"(C:\Program Files (x86)\Steam)";
const std::string kChromeDir = R"(C:\Program Files\Google\Chrome\Application)";
const std::string kEdgeDir = R"(C:\Program Files (x86)\Microsoft\Edge\Application)";
const std::string kGames = R"(D:\Games)";

// --------------------------------------------------------------------------------------- 05 §5.10, row by row

TEST(AudioPolicyClassify, Example1DiscordAndItsAudioChild) {
  World w;
  const uint32_t upd = w.Spawn("Update.exe", kDiscordDir);
  const uint32_t top = w.Spawn("Discord.exe", kDiscordApp, upd);
  const uint32_t child = w.Spawn("Discord.exe", kDiscordApp, top);
  w.Render(child);
  w.Capture(top);
  Sim e(Standard().Build());
  e.Step(w);
  for (const uint32_t pid : {top, child}) {
    EXPECT_EQ(e.ProcessOf(pid).verdict, Verdict::kKeptOut);
    EXPECT_EQ(e.ProcessOf(pid).reason, Reason::kBuiltin);
    EXPECT_EQ(e.ProcessOf(pid).app_id, "discord");
  }
  EXPECT_EQ(e.SessionOf(child).verdict, Verdict::kKeptOut);
  const AppRow *row = e.App("discord");
  ASSERT_NE(row, nullptr);
  EXPECT_EQ(row->name, "Discord");
  EXPECT_EQ(row->exe, "discord.exe");
  EXPECT_EQ(row->reason, Reason::kBuiltin);
  EXPECT_TRUE(row->uses_mic);
  EXPECT_EQ(row->pids, 2u);
  EXPECT_EQ(row->sessions, 2u);
  // The updater is the app root of Discord's walk (discord.exe sits in a subfolder of its folder), so Discord's mic
  // session makes it a mic root; on its own walk no rule hits, so it is kept out for the mic, not as Discord.
  EXPECT_EQ(e.ProcessOf(upd).verdict, Verdict::kKeptOut);
  EXPECT_EQ(e.ProcessOf(upd).reason, Reason::kMic);
  EXPECT_EQ(e.ProcessOf(top).app_root, static_cast<int32_t>(*e.table.Find(upd)));
  EXPECT_TRUE(e.plan.include_targets.empty());
}

TEST(AudioPolicyClassify, Example2TeamsWebViewJoinsTeams) {
  World w;
  const uint32_t teams = w.Spawn("ms-teams.exe", R"(C:\Program Files\WindowsApps\MSTeams_x64)");
  const uint32_t wv = w.Spawn("msedgewebview2.exe", kEdgeWebView, teams);
  const uint32_t wvaudio = w.Spawn("msedgewebview2.exe", kEdgeWebView, wv);
  w.Render(wvaudio);
  Sim e(Standard().Build());
  e.Step(w);
  EXPECT_EQ(e.SessionOf(wvaudio).verdict, Verdict::kKeptOut);
  EXPECT_EQ(e.SessionOf(wvaudio).reason, Reason::kBuiltin);
  EXPECT_EQ(e.ProcessOf(wvaudio).app_id, "teams");
  EXPECT_EQ(e.ProcessOf(wvaudio).app_root, static_cast<int32_t>(*e.table.Find(teams)));
}

TEST(AudioPolicyClassify, Example3SteamKeptOutForTheMicGameHeard) {
  World w;
  const uint32_t steam = w.Spawn("steam.exe", kSteamDir);
  const uint32_t game = w.Spawn("game.exe", kSteamDir + R"(\steamapps\common\Game)", steam);
  w.Capture(steam);
  w.Render(steam, false);
  w.Render(game);
  Sim e(Standard().Build());
  e.Step(w);
  EXPECT_EQ(e.ProcessOf(steam).verdict, Verdict::kKeptOut);
  EXPECT_EQ(e.ProcessOf(steam).reason, Reason::kMic);
  EXPECT_EQ(e.SessionOf(game).verdict, Verdict::kHeard);
  EXPECT_TRUE(e.IsTarget(game));
  EXPECT_FALSE(e.ProcessOf(game).uses_mic);
}

TEST(AudioPolicyClassify, Example4BrowserWithAMicIsKeptOutAsAWhole) {
  World w;
  const uint32_t chrome = w.Spawn("chrome.exe", kChromeDir);
  const uint32_t audio = w.Spawn("chrome.exe", kChromeDir, chrome); // the audio service utility
  const uint32_t tab = w.Spawn("chrome.exe", kChromeDir, chrome);
  w.Capture(audio);
  w.Render(audio);
  Sim e(Standard().Build());
  e.Step(w);
  for (const uint32_t pid : {chrome, audio, tab}) {
    EXPECT_EQ(e.ProcessOf(pid).verdict, Verdict::kKeptOut);
    EXPECT_EQ(e.ProcessOf(pid).reason, Reason::kMic);
  }
  const AppRow *row = e.App("exe:chrome.exe");
  ASSERT_NE(row, nullptr);
  EXPECT_TRUE(row->browser);
  EXPECT_TRUE(row->uses_mic);
  EXPECT_EQ(row->verdict, Verdict::kKeptOut);
  EXPECT_EQ(row->pids, 3u);
}

TEST(AudioPolicyClassify, Example5ViewerRowBeatsASavedInclude) {
  World w;
  const uint32_t edge = w.Spawn("msedge.exe", kEdgeDir);
  const uint32_t audio = w.Spawn("msedge.exe", kEdgeDir, edge);
  w.Render(audio);
  Sim e(Standard().UserInclude("msedge.exe").Viewer("msedge.exe").Build());
  e.Step(w);
  EXPECT_EQ(e.SessionOf(audio).verdict, Verdict::kKeptOut);
  EXPECT_EQ(e.SessionOf(audio).reason, Reason::kViewer);
}

TEST(AudioPolicyClassify, Example6IsshoniWebViewByTheImplicitRule) {
  World w;
  const uint32_t me = w.Spawn("isshoni.exe", kIsshoniDir);
  const uint32_t wv = w.Spawn("msedgewebview2.exe", kEdgeWebView, me);
  const uint32_t audio = w.Spawn("msedgewebview2.exe", kEdgeWebView, wv);
  w.Render(audio);
  Sim e(Standard().Build(me)); // only the DLL's implicit rule: Go's self rules missing still keeps isshoni out
  e.Step(w);
  EXPECT_EQ(e.SessionOf(audio).verdict, Verdict::kKeptOut);
  EXPECT_EQ(e.SessionOf(audio).reason, Reason::kIsshoni);
  EXPECT_EQ(e.ProcessOf(audio).app_id, "isshoni");
  EXPECT_EQ(e.ProcessOf(audio).app_exe, "isshoni.exe");
}

TEST(AudioPolicyClassify, Example7BrowserOpenedByDiscordIsHeard) {
  World w;
  const uint32_t discord = w.Spawn("discord.exe", kDiscordApp);
  const uint32_t browser = w.Spawn("browser.exe", R"(C:\Program Files\Browser)", discord);
  w.Render(browser);
  Sim e(Standard().Build());
  e.Step(w);
  EXPECT_EQ(e.SessionOf(browser).verdict, Verdict::kHeard);
  EXPECT_TRUE(e.IsTarget(browser));
  EXPECT_EQ(e.ProcessOf(browser).app_id, "exe:browser.exe");
}

TEST(AudioPolicyClassify, Example8LauncherWhoseTreeHoldsDiscord) {
  World w;
  const uint32_t lx = w.Spawn("launcherx.exe", R"(C:\Program Files\LauncherX)");
  w.Proc(lx).description = "LauncherX";
  const uint32_t discord = w.Spawn("discord.exe", kDiscordApp, lx);
  w.Render(lx);
  w.Render(discord);
  Sim e(Standard().Build());
  e.Step(w);
  EXPECT_EQ(e.SessionOf(discord).verdict, Verdict::kKeptOut);
  EXPECT_EQ(e.SessionOf(lx).verdict, Verdict::kNotCaptured);
  EXPECT_EQ(e.SessionOf(lx).reason, Reason::kTree);
  EXPECT_EQ(e.SessionOf(lx).detail, "Discord");
  const AppRow *row = e.App("exe:launcherx.exe");
  ASSERT_NE(row, nullptr);
  EXPECT_EQ(row->name, "LauncherX");
  EXPECT_EQ(row->verdict, Verdict::kNotCaptured);
  EXPECT_EQ(row->reason, Reason::kTree);
  EXPECT_EQ(row->detail, "Discord");
  EXPECT_FALSE(e.IsTarget(lx));
}

TEST(AudioPolicyClassify, Example9ReusedPidOfDiscordsUpdaterBlocksTheGameAsPidReuse) {
  World w;
  const uint32_t upd = w.Spawn("update.exe", kDiscordDir);
  const uint32_t discord = w.Spawn("discord.exe", kDiscordApp, upd);
  w.Exit(upd);
  const uint32_t game = w.Spawn("game.exe", kGames + R"(\Game)");
  w.SpawnAt(upd, 0, "helper.exe", kGames + R"(\Game)", game); // the helper got the updater's pid
  w.Render(game);
  w.Render(discord);
  Sim e(Standard().Build());
  e.Step(w);
  EXPECT_EQ(e.SessionOf(discord).verdict, Verdict::kKeptOut);
  EXPECT_EQ(e.SessionOf(game).verdict, Verdict::kNotCaptured);
  EXPECT_EQ(e.SessionOf(game).reason, Reason::kPidReuse);
  EXPECT_EQ(e.SessionOf(game).detail, "Discord");
  EXPECT_EQ(e.plan.pid_reuse_blocks, 1u);
  // Discord's walk doesn't go through the helper (no strict link), so Discord is its own app root.
  EXPECT_EQ(e.ProcessOf(discord).app_root, static_cast<int32_t>(*e.table.Find(discord)));
}

TEST(AudioPolicyClassify, Example10SharedGameHoldingTheMicIsHeardWithAWarning) {
  World w;
  const uint32_t game = w.Spawn("game.exe", kGames + R"(\Game)");
  w.Capture(game);
  w.Render(game);
  Sim e(Standard().Shared(game).Build());
  e.Step(w);
  EXPECT_EQ(e.SessionOf(game).verdict, Verdict::kHeard);
  EXPECT_TRUE(e.ProcessOf(game).warnings & kWarnSharedAppUsesMic);
  const AppRow *row = e.App("exe:game.exe");
  ASSERT_NE(row, nullptr);
  EXPECT_TRUE(row->shared);
  EXPECT_TRUE(row->uses_mic);
  EXPECT_TRUE(row->warnings & kWarnSharedAppUsesMic);
}

// --------------------------------------------------------------------------------------- rows and precedence

TEST(AudioPolicyClassify, Row1InstanceTreeStrictAndLoose) {
  World w;
  const uint32_t me = w.Spawn("isshoni.exe", kIsshoniDir);
  const uint32_t strict_child = w.Spawn("helper.exe", R"(C:\Other)", me);
  // A process older than isshoni whose (dead) parent's pid isshoni now holds: linked only loosely.
  const uint32_t old = w.SpawnAt(5000, 10, "old.exe", R"(C:\Old)", me);
  w.Render(strict_child);
  w.Render(old);
  Sim e(Standard().Build(me));
  e.Step(w);
  EXPECT_EQ(e.SessionOf(strict_child).reason, Reason::kIsshoni);
  EXPECT_EQ(e.SessionOf(old).reason, Reason::kIsshoni); // row 1 takes strict or loose links
}

TEST(AudioPolicyClassify, Row1OtherIsshoniInstancesByExeName) {
  World w;
  const uint32_t me = w.Spawn("isshoni.exe", kIsshoniDir);
  const uint32_t other = w.Spawn("isshoni.exe", kIsshoniDir); // a second instance (another user session's tests)
  const uint32_t wv = w.Spawn("msedgewebview2.exe", kEdgeWebView, other);
  w.Render(wv);
  Sim e(Standard().Self(me).Build(me));
  e.Step(w);
  EXPECT_EQ(e.SessionOf(wv).reason, Reason::kIsshoni);
  EXPECT_EQ(e.ProcessOf(wv).app_id, "isshoni");
}

TEST(AudioPolicyClassify, Row1WebViewDataFolderCatchesOrphans) {
  World w;
  const uint32_t me = w.Spawn("isshoni.exe", kIsshoniDir);
  // An orphaned WebView2 audio utility (its browser process is gone): only the data-folder rule knows it.
  const uint32_t orphan = w.Spawn("msedgewebview2.exe", kEdgeWebView, 31337);
  w.Proc(orphan).user_data_dir = kWebViewData + R"(\EBWebView)";
  const uint32_t foreign = w.Spawn("msedgewebview2.exe", kEdgeWebView, 31341);
  w.Proc(foreign).user_data_dir = kWebViewData + R"(2\EBWebView)";
  w.Render(orphan);
  w.Render(foreign);
  Sim e(Standard().Self(me, kWebViewData).Build(me));
  e.Step(w);
  EXPECT_EQ(e.SessionOf(orphan).verdict, Verdict::kKeptOut);
  EXPECT_EQ(e.SessionOf(orphan).reason, Reason::kIsshoni);
  EXPECT_EQ(e.SessionOf(foreign).verdict, Verdict::kHeard); // <dir>2 is another folder
}

TEST(AudioPolicyClassify, Row1BeatsAnAllow) {
  World w;
  const uint32_t me = w.Spawn("isshoni.exe", kIsshoniDir);
  const uint32_t child = w.Spawn("isshoni.exe", kIsshoniDir, me);
  w.Render(child);
  Sim e(Standard().UserInclude("isshoni.exe").Build(me));
  e.Step(w);
  EXPECT_EQ(e.SessionOf(child).reason, Reason::kIsshoni);
}

TEST(AudioPolicyClassify, Row2BuiltinBeatsUserRulesAndAllow) {
  World w;
  const uint32_t d = w.Spawn("discord.exe", kDiscordApp);
  w.Render(d);
  Sim e(Standard().UserInclude("discord.exe").Viewer("discord.exe").UserKeepOut("discord.exe").Build());
  e.Step(w);
  EXPECT_EQ(e.SessionOf(d).reason, Reason::kBuiltin); // locked: M2 has no way to hear a built-in (05 §25 q3)
}

TEST(AudioPolicyClassify, Row3UserKeepOutAndRow3BeatsViewer) {
  World w;
  const uint32_t app = w.Spawn("fakevoice.exe", R"(C:\FakeVoice)");
  const uint32_t helper = w.Spawn("voicehelper.exe", R"(C:\FakeVoice\bin)", app);
  w.Proc(app).description = "Fake Voice";
  w.Render(helper);
  Sim e(Standard().Viewer("fakevoice.exe").UserKeepOut("fakevoice.exe").Build());
  e.Step(w);
  EXPECT_EQ(e.SessionOf(helper).reason, Reason::kUser);
  EXPECT_EQ(e.ProcessOf(helper).app_id, "exe:fakevoice.exe");
  EXPECT_EQ(e.ProcessOf(helper).app_name, "Fake Voice");
}

TEST(AudioPolicyClassify, Row5AllowBeatsMicWithAWarning) {
  World w;
  const uint32_t kook = w.Spawn("kook.exe", R"(C:\KOOK)");
  w.Capture(kook);
  w.Render(kook);
  Sim e(Standard().UserInclude("kook.exe").Build());
  e.Step(w);
  EXPECT_EQ(e.SessionOf(kook).verdict, Verdict::kHeard);
  EXPECT_TRUE(e.ProcessOf(kook).allowed);
  EXPECT_TRUE(e.ProcessOf(kook).warnings & kWarnIncludedUsesMic);
  EXPECT_TRUE(e.IsTarget(kook));
}

TEST(AudioPolicyClassify, Row6NeedsTheFlagButUsesMicIsAlwaysReported) {
  World w;
  const uint32_t kook = w.Spawn("kook.exe", R"(C:\KOOK)");
  w.Capture(kook);
  w.Render(kook);
  Sim e(Standard().Flags(0).Build());
  e.Step(w);
  EXPECT_EQ(e.SessionOf(kook).verdict, Verdict::kHeard);
  EXPECT_TRUE(e.ProcessOf(kook).uses_mic);
  EXPECT_EQ(e.planner.MicRootCount(), 1u);
}

TEST(AudioPolicyClassify, Row7LauncherSessionsAreNotCapturedInTheIncludeSet) {
  World w;
  const uint32_t me = w.Spawn("isshoni.exe", kIsshoniDir);
  const uint32_t discord = w.Spawn("discord.exe", kDiscordApp); // a second kept-out tree: the include set
  const uint32_t steam = w.Spawn("steam.exe", kSteamDir);
  w.Render(kExplorer);
  w.Render(steam);
  w.Render(discord);
  Sim e(Standard().Flags(IM_EXCLF_FAST_PATH).Build(me));
  e.Step(w);
  EXPECT_EQ(e.plan.engine, Engine::kIncludeSet);
  EXPECT_EQ(e.SessionOf(kExplorer).verdict, Verdict::kNotCaptured);
  EXPECT_EQ(e.SessionOf(kExplorer).reason, Reason::kLauncher);
  EXPECT_EQ(e.SessionOf(steam).reason, Reason::kLauncher);
  EXPECT_FALSE(e.IsTarget(steam));
}

TEST(AudioPolicyClassify, InstanceRuleOnAMissingPidIsIgnoredWithAWarning) {
  World w;
  const uint32_t game = w.Spawn("game.exe", kGames);
  w.Render(game);
  Sim e(Standard().Add(IM_RULE_INSTANCE, IM_RULE_SRC_SELF, "4242").Build());
  e.Step(w);
  EXPECT_EQ(e.SessionOf(game).verdict, Verdict::kHeard);
  ASSERT_EQ(e.plan.warnings.size(), 1u);
  EXPECT_EQ(e.plan.warnings[0].code, "instance_gone");
  EXPECT_EQ(e.plan.warnings[0].app, "isshoni");
}

// --------------------------------------------------------------------------------------- the app walk (05 §5.2)

TEST(AudioPolicyClassify, AppBoundaryByFolder) {
  // S1's app-boundary rule: a subfolder is the same app, a sibling folder another app, an unknown folder a boundary.
  World w;
  const uint32_t app = w.Spawn("fakevoice.exe", R"(C:\Apps\FakeVoice)");
  const uint32_t sub = w.Spawn("sub.exe", R"(C:\Apps\FakeVoice\bin\x64)", app);
  const uint32_t same = w.Spawn("same.exe", R"(c:/apps/fakevoice/)", app);
  const uint32_t sibling = w.Spawn("sibling.exe", R"(C:\Apps\FakeVoice2)", app);
  const uint32_t unknown = w.Spawn("unknown.exe", "", app);
  const uint32_t below_unknown = w.Spawn("below.exe", R"(C:\Apps\FakeVoice\x)", unknown);
  for (const uint32_t p : {sub, same, sibling, unknown, below_unknown}) {
    w.Render(p);
  }
  Sim e(Standard().UserKeepOut("fakevoice.exe").Build());
  e.Step(w);
  EXPECT_EQ(e.SessionOf(sub).reason, Reason::kUser);
  EXPECT_EQ(e.SessionOf(same).reason, Reason::kUser);
  EXPECT_EQ(e.SessionOf(sibling).verdict, Verdict::kHeard); // its own app (a child of FakeVoice, not part of it)
  EXPECT_EQ(e.ProcessOf(sibling).app_id, "exe:sibling.exe");
  EXPECT_EQ(e.SessionOf(unknown).verdict, Verdict::kHeard);
  EXPECT_EQ(e.ProcessOf(below_unknown).verdict, Verdict::kHeard); // the walk stops at the unknown folder
  EXPECT_EQ(e.SessionOf(below_unknown).note, Note::kCovered);     // unknown.exe's stream covers it
  EXPECT_TRUE(e.IsTarget(unknown));
}

TEST(AudioPolicyClassify, SharedRuntimeJoinsItsParentWhateverTheFolder) {
  World w;
  const uint32_t slack = w.Spawn("slack.exe", std::string(kLocal) + R"(\slack\app-4.41)");
  const uint32_t wv = w.Spawn("msedgewebview2.exe", kEdgeWebView, slack);
  const uint32_t other = w.Spawn("notaruntime.exe", kEdgeWebView, slack);
  w.Render(wv);
  w.Render(other);
  Sim e(Standard().Build());
  e.Step(w);
  EXPECT_EQ(e.SessionOf(wv).reason, Reason::kBuiltin);
  EXPECT_EQ(e.ProcessOf(other).verdict, Verdict::kHeard); // same folder as WebView2 but not a shared runtime
}

TEST(AudioPolicyClassify, LaunchersEndTheWalkEvenInASubfolder) {
  World w;
  const uint32_t steam = w.Spawn("steam.exe", kSteamDir);
  const uint32_t game = w.Spawn("game.exe", kSteamDir + R"(\steamapps\common\TestGame)", steam);
  w.Render(game);
  Sim e(Standard().UserKeepOut("steam.exe").Build());
  e.Step(w);
  EXPECT_EQ(e.ProcessOf(steam).reason, Reason::kUser);
  EXPECT_EQ(e.SessionOf(game).verdict, Verdict::kHeard); // a user rule on Steam doesn't reach its games
  EXPECT_EQ(e.ProcessOf(game).app_root, static_cast<int32_t>(*e.table.Find(game)));
}

TEST(AudioPolicyClassify, StrictAndLooseLinks) {
  World w;
  const uint32_t parent = w.Spawn("parent.exe", R"(C:\P)");
  const uint32_t child = w.Spawn("child.exe", R"(C:\P)", parent);
  const uint32_t older = w.SpawnAt(7000, 500, "older.exe", R"(C:\P)", parent); // older than its "parent"
  const uint32_t same_tick = w.SpawnAt(7004, w.Created(parent), "tick.exe", R"(C:\P)", parent);
  const uint32_t unknown_time = w.SpawnAt(7008, 0, "unknown.exe", R"(C:\P)", parent);
  w.Proc(unknown_time).created = 0;
  const ProcessTable t = w.Table();
  const auto pi = *t.Find(parent);
  EXPECT_EQ(t.Parent(*t.Find(child), false), static_cast<int32_t>(pi));
  EXPECT_EQ(t.Parent(*t.Find(older), false), -1); // reused pid: no strict link
  EXPECT_EQ(t.Parent(*t.Find(older), true), static_cast<int32_t>(pi));
  EXPECT_EQ(t.Parent(*t.Find(same_tick), false), static_cast<int32_t>(pi));    // equal times: strict
  EXPECT_EQ(t.Parent(*t.Find(unknown_time), false), static_cast<int32_t>(pi)); // unknown: accepted
  EXPECT_TRUE(t.InTree(*t.Find(older), pi, true));
  EXPECT_FALSE(t.InTree(*t.Find(older), pi, false));
  // A pid cycle (two reused pids pointing at each other) ends after kMaxWalk steps.
  World c;
  c.SpawnAt(8000, 20, "a.exe", R"(C:\C)", 8004);
  c.SpawnAt(8004, 10, "b.exe", R"(C:\C)", 8000);
  const ProcessTable ct = c.Table();
  EXPECT_TRUE(ct.InTree(*ct.Find(8000), *ct.Find(8004), true));
  EXPECT_FALSE(ct.InTree(*ct.Find(8000), *ct.Find(kExplorer), true));
}

TEST(AudioPolicyClassify, DuplicatePidsKeepTheFirstEntry) {
  std::vector<ProcessEntry> entries(2);
  entries[0].pid = 10;
  entries[0].exe = "first.exe";
  entries[1].pid = 10;
  entries[1].exe = "second.exe";
  const ProcessTable t(entries);
  ASSERT_EQ(t.size(), 1u);
  EXPECT_EQ(t.at(0).exe, "first.exe");
}

// --------------------------------------------------------------------------------------- mic (05 §5.6)

TEST(AudioPolicyClassify, MicStickinessAcrossCaptureGapsEndsOnExit) {
  World w;
  const uint32_t kook = w.Spawn("kook.exe", R"(C:\KOOK)");
  const uint32_t helper = w.Spawn("kookhelper.exe", R"(C:\KOOK\bin)", kook);
  w.Render(helper);
  w.Capture(kook);
  Sim e(Standard().Build());
  e.Step(w, 0);
  EXPECT_EQ(e.SessionOf(helper).reason, Reason::kMic);
  // Push-to-talk: the capture session goes away between presses, the incoming voices keep playing.
  w.StopSessions(kook, Flow::kCapture);
  e.Step(w, 30'000'000'000);
  EXPECT_EQ(e.SessionOf(helper).reason, Reason::kMic);
  EXPECT_TRUE(e.planner.IsMicRoot(w.Key(kook)));
  // The root exits and its pid comes back as another process: not a mic root.
  w.Exit(helper);
  w.Exit(kook);
  const uint32_t again = w.SpawnAt(kook, 0, "kook.exe", R"(C:\KOOK)", kExplorer);
  w.Render(again);
  e.Step(w, 31'000'000'000);
  EXPECT_EQ(e.SessionOf(again).verdict, Verdict::kHeard);
  EXPECT_EQ(e.planner.MicRootCount(), 0u);
}

TEST(AudioPolicyClassify, CaptureSessionsInIsshonisTreeMakeNoMicRoot) {
  World w;
  const uint32_t me = w.Spawn("isshoni.exe", kIsshoniDir);
  const uint32_t wv = w.Spawn("msedgewebview2.exe", kEdgeWebView, me);
  w.Capture(wv);
  Sim e(Standard().Build(me));
  e.Step(w);
  EXPECT_EQ(e.planner.MicRootCount(), 0u);
  EXPECT_EQ(e.ProcessOf(me).reason, Reason::kIsshoni);
}

TEST(AudioPolicyClassify, SharedAppSuppressesOnlyRowSix) {
  World w;
  const uint32_t d = w.Spawn("discord.exe", kDiscordApp);
  w.Render(d);
  Sim e(Standard().Shared(d).Build());
  e.Step(w);
  EXPECT_EQ(e.SessionOf(d).reason, Reason::kBuiltin); // rows 2-4 still apply to the shared app
  EXPECT_TRUE(e.ProcessOf(d).warnings & kWarnSharedAppKeptOut);
  World u;
  const uint32_t app = u.Spawn("fakevoice.exe", R"(C:\FakeVoice)");
  u.Render(app);
  u.Capture(app);
  Sim f(Standard().UserKeepOut("fakevoice.exe").Shared(app).Build());
  f.Step(u);
  EXPECT_EQ(f.SessionOf(app).reason, Reason::kUser);
  EXPECT_TRUE(f.ProcessOf(app).warnings & kWarnSharedAppKeptOut);
}

TEST(AudioPolicyClassify, ViewerOverrideHearsTheBrowserButMicStillApplies) {
  World w;
  const uint32_t edge = w.Spawn("msedge.exe", kEdgeDir);
  const uint32_t audio = w.Spawn("msedge.exe", kEdgeDir, edge);
  w.Render(audio);
  // The per-run override removes the row-4 rules; the saved Include stays.
  Sim e(Standard().UserInclude("msedge.exe").Build());
  e.Step(w);
  EXPECT_EQ(e.SessionOf(audio).verdict, Verdict::kHeard);
  // Without the saved Include, a browser on a call stays out by row 6.
  w.Capture(audio);
  Sim f(Standard().Build());
  f.Step(w);
  EXPECT_EQ(f.SessionOf(audio).reason, Reason::kMic);
  EXPECT_TRUE(f.ProcessOf(audio).browser);
}

// --------------------------------------------------------------------------------------- lineage (05 §5.7)

TEST(AudioPolicyClassify, OrphansKeepTheirLineage) {
  World w;
  const uint32_t d = w.Spawn("discord.exe", kDiscordApp);
  const uint32_t helper = w.Spawn("voicehelper.exe", kDiscordApp + R"(\modules)", d);
  w.Render(helper);
  Sim e(Standard().Build());
  e.Step(w, 0);
  EXPECT_EQ(e.SessionOf(helper).reason, Reason::kBuiltin);
  EXPECT_FALSE(e.ProcessOf(helper).lineage);
  w.Exit(d); // orphaned: its walk is now only itself
  e.Step(w, 1'000'000'000);
  EXPECT_EQ(e.SessionOf(helper).verdict, Verdict::kKeptOut);
  EXPECT_EQ(e.SessionOf(helper).reason, Reason::kBuiltin);
  EXPECT_TRUE(e.ProcessOf(helper).lineage);
  EXPECT_EQ(e.ProcessOf(helper).app_id, "discord");
  w.Exit(helper);
  e.Step(w, 2'000'000'000);
  EXPECT_EQ(e.planner.LineageCount(), 0u); // entries live as long as their process
}

TEST(AudioPolicyClassify, LineageHoldsFactsSoRuleChangesStillApply) {
  World w;
  const uint32_t app = w.Spawn("fakevoice.exe", R"(C:\FakeVoice)");
  const uint32_t helper = w.Spawn("voicehelper.exe", R"(C:\FakeVoice\bin)", app);
  w.Render(helper);
  Sim e(Standard().UserKeepOut("fakevoice.exe").Build());
  e.Step(w, 0);
  w.Exit(app);
  e.Step(w, 1);
  EXPECT_EQ(e.SessionOf(helper).reason, Reason::kUser);
  e.planner.SetExclusions(Standard().UserInclude("fakevoice.exe").Build());
  e.Step(w, 2);
  EXPECT_EQ(e.SessionOf(helper).verdict, Verdict::kHeard);
  EXPECT_TRUE(e.ProcessOf(helper).allowed);
}

TEST(AudioPolicyClassify, OrphanedMicHelperFollowsItsRootsLifetime) {
  World w;
  const uint32_t root = w.Spawn("kook.exe", R"(C:\KOOK)");
  const uint32_t mid = w.Spawn("kookhost.exe", R"(C:\KOOK)", root);
  const uint32_t helper = w.Spawn("kookaudio.exe", R"(C:\KOOK)", mid);
  w.Capture(root);
  w.Render(helper);
  Sim e(Standard().Build());
  e.Step(w, 0);
  w.Exit(mid); // helper orphaned; the mic root still runs
  e.Step(w, 1);
  EXPECT_EQ(e.SessionOf(helper).reason, Reason::kMic);
  EXPECT_TRUE(e.ProcessOf(helper).lineage);
  w.Exit(root); // the root exits: mic stickiness ends (05 §5.6)
  e.Step(w, 2);
  EXPECT_EQ(e.SessionOf(helper).verdict, Verdict::kHeard);
}

// --------------------------------------------------------------------------------------- ids and names (05 §5.1)

TEST(AudioPolicyClassify, AppIdsAndDisplayNames) {
  World w;
  const uint32_t spotify = w.Spawn("Spotify.exe", R"(C:\Spotify)");
  w.Proc(spotify).description = "Spotify";
  const uint32_t sub = w.Spawn("spotifyhelper.exe", R"(C:\Spotify\x)", spotify);
  const uint32_t plain = w.Spawn("plain.exe", R"(C:\Plain)");
  w.Render(sub);
  w.Render(plain);
  Sim e(Standard().Build());
  e.Step(w);
  EXPECT_EQ(e.ProcessOf(sub).app_id, "exe:spotify.exe");
  EXPECT_EQ(e.ProcessOf(sub).app_name, "Spotify");
  EXPECT_EQ(e.ProcessOf(sub).app_exe, "spotify.exe");
  EXPECT_EQ(e.ProcessOf(plain).app_name, "plain");
  const AppRow *row = e.App("exe:spotify.exe");
  ASSERT_NE(row, nullptr);
  EXPECT_EQ(row->pids, 2u);
  EXPECT_EQ(row->streams, 1u);
}

} // namespace
