// A small world of processes, sessions and rules for the audiopolicy tests: a fake snapshot (what the Windows watcher
// builds from Toolhelp), the rules Go's builder sends (docs/m2/05-windows-audio.md §5.4, §14.3) and a fake engine that
// applies plans the way the watcher does (activations become live streams, removals go away).
#pragma once

#include <gtest/gtest.h>

#include <algorithm>
#include <cstdint>
#include <optional>
#include <string>
#include <string_view>
#include <utility>
#include <vector>

#include "im/audiopolicy/exclusions.h"
#include "im/audiopolicy/planner.h"
#include "im/audiopolicy/process_table.h"
#include "isshoni_media.h"

namespace policytest {

using namespace im::audiopolicy;

inline constexpr uint32_t kExplorer = 100;
inline constexpr std::string_view kWindows = R"(C:\Windows)";
inline constexpr std::string_view kSystem32 = R"(C:\Windows\System32)";
inline constexpr std::string_view kLocal = R"(C:\Users\test\AppData\Local)";

// The built-in voice apps, launchers, shared runtimes and browsers of 05 §5.4.
struct VoiceApp {
  const char *id;
  const char *name;
  std::vector<const char *> exes;
};

inline const std::vector<VoiceApp> &VoiceApps() {
  static const std::vector<VoiceApp> apps = {
      {"discord", "Discord", {"discord.exe", "discordptb.exe", "discordcanary.exe", "discorddevelopment.exe"}},
      {"vesktop", "Vesktop", {"vesktop.exe", "equibop.exe"}},
      {"teamspeak", "TeamSpeak", {"ts3client_win64.exe", "ts3client_win32.exe", "teamspeak.exe"}},
      {"mumble", "Mumble", {"mumble.exe"}},
      {"zoom", "Zoom", {"zoom.exe"}},
      {"teams", "Microsoft Teams", {"ms-teams.exe", "teams.exe"}},
      {"slack", "Slack", {"slack.exe"}},
  };
  return apps;
}

inline const std::vector<const char *> &Launchers() {
  static const std::vector<const char *> l = {"explorer.exe",
                                              "steam.exe",
                                              "epicgameslauncher.exe",
                                              "battle.net.exe",
                                              "battle.net launcher.exe",
                                              "galaxyclient.exe",
                                              "eadesktop.exe",
                                              "ealauncher.exe",
                                              "upc.exe",
                                              "riotclientservices.exe",
                                              "heroic.exe",
                                              "playnite.desktopapp.exe",
                                              "playnite.fullscreenapp.exe",
                                              "xboxpcapp.exe",
                                              "gamelaunchhelper.exe",
                                              "overwolf.exe",
                                              "itch.exe",
                                              "services.exe",
                                              "svchost.exe",
                                              "wininit.exe",
                                              "winlogon.exe",
                                              "userinit.exe",
                                              "sihost.exe",
                                              "runtimebroker.exe",
                                              "dllhost.exe",
                                              "taskhostw.exe",
                                              "applicationframehost.exe",
                                              "cmd.exe",
                                              "powershell.exe",
                                              "pwsh.exe",
                                              "windowsterminal.exe",
                                              "openconsole.exe",
                                              "conhost.exe",
                                              "wsl.exe",
                                              "bash.exe"};
  return l;
}

inline const std::vector<const char *> &Browsers() {
  static const std::vector<const char *> b = {
      "chrome.exe", "chromium.exe", "brave.exe",     "vivaldi.exe",  "opera.exe",  "arc.exe", "thorium.exe",
      "msedge.exe", "firefox.exe",  "librewolf.exe", "waterfox.exe", "floorp.exe", "zen.exe"};
  return b;
}

// Rules as Go's builder sends them (05 §14.3), passed through ParseExclusions like the DLL does.
class RuleBuilder {
public:
  struct Spec {
    uint32_t kind;
    uint32_t source;
    std::string value;
    std::string app_id;
    std::string name;
  };

  RuleBuilder &Add(uint32_t kind, uint32_t source, std::string value, std::string app_id = {}, std::string name = {}) {
    specs_.push_back(Spec{kind, source, std::move(value), std::move(app_id), std::move(name)});
    return *this;
  }
  // Step 1: isshoni itself (the DLL adds the instance rule on its own pid too).
  RuleBuilder &Self(uint32_t pid, std::string webview_dir = {}) {
    Add(IM_RULE_INSTANCE, IM_RULE_SRC_SELF, std::to_string(pid), "isshoni", "isshoni");
    Add(IM_RULE_APP_EXCLUDE, IM_RULE_SRC_SELF, "isshoni.exe", "isshoni", "isshoni");
    if (!webview_dir.empty()) {
      Add(IM_RULE_WEBVIEW_DATA_DIR, IM_RULE_SRC_SELF, std::move(webview_dir), "isshoni", "isshoni");
    }
    return *this;
  }
  // Step 2: launchers, shared runtimes, browsers.
  RuleBuilder &Data() {
    for (const char *l : Launchers()) {
      Add(IM_RULE_STOP, IM_RULE_SRC_DATA, l);
    }
    Add(IM_RULE_SHARED_RUNTIME, IM_RULE_SRC_DATA, "msedgewebview2.exe");
    for (const char *b : Browsers()) {
      Add(IM_RULE_BROWSER, IM_RULE_SRC_DATA, b);
    }
    return *this;
  }
  // Step 3: the built-in voice apps.
  RuleBuilder &Builtins() {
    for (const VoiceApp &a : VoiceApps()) {
      for (const char *exe : a.exes) {
        Add(IM_RULE_APP_EXCLUDE, IM_RULE_SRC_BUILTIN, exe, a.id, a.name);
      }
    }
    return *this;
  }
  RuleBuilder &UserKeepOut(std::string exe) { return Add(IM_RULE_APP_EXCLUDE, IM_RULE_SRC_USER, std::move(exe)); }
  RuleBuilder &UserInclude(std::string exe) { return Add(IM_RULE_APP_ALLOW, IM_RULE_SRC_USER, std::move(exe)); }
  RuleBuilder &Viewer(std::string exe) { return Add(IM_RULE_APP_EXCLUDE, IM_RULE_SRC_VIEWER, std::move(exe)); }
  RuleBuilder &Flags(uint32_t flags) {
    flags_ = flags;
    return *this;
  }
  RuleBuilder &Shared(uint32_t pid) {
    shared_ = pid;
    return *this;
  }

  // The DLL's copy, with the implicit rule on self_pid (0: none).
  Exclusions Build(uint32_t self_pid = 0) const {
    std::vector<im_rule> rules;
    rules.reserve(specs_.size());
    for (const Spec &s : specs_) {
      rules.push_back(im_rule{s.kind, s.source, s.value.c_str(), s.app_id.empty() ? nullptr : s.app_id.c_str(),
                              s.name.empty() ? nullptr : s.name.c_str()});
    }
    im_exclusions ex{};
    ex.struct_size = sizeof(im_exclusions);
    ex.flags = flags_;
    ex.rules = rules.data();
    ex.rule_count = static_cast<uint32_t>(rules.size());
    ex.shared_pid = shared_;
    Exclusions out;
    const im_status st = ParseExclusions(&ex, self_pid, out);
    EXPECT_EQ(st, IM_OK);
    return out;
  }

private:
  std::vector<Spec> specs_;
  uint32_t flags_ = IM_EXCLF_KEEP_OUT_MIC_USERS; // tests turn the fast path on where they need it
  uint32_t shared_ = 0;
};

// A reconcile's input with sessions and no streams.
inline PlanInput Input(int64_t now_ns, const std::vector<Session> &sessions) {
  PlanInput in;
  in.now_ns = now_ns;
  in.sessions = sessions;
  return in;
}

// The rules every realistic test starts from: data and built-ins (isshoni's own rules are added by Self()).
inline RuleBuilder Standard() {
  RuleBuilder b;
  b.Data().Builtins();
  return b;
}

// A process snapshot and its sessions. Creation times come from a clock that advances by 10 per spawn, so a later
// spawn is always younger.
class World {
public:
  World() { SpawnAt(kExplorer, 1000, "explorer.exe", kWindows, 4); }

  uint32_t Spawn(std::string_view exe, std::string_view folder, uint32_t parent = kExplorer) {
    return SpawnAt(next_pid_, 0, exe, folder, parent);
  }
  // A process with an explicit pid and creation time (0: the next clock value). Reusing a pid replaces its process.
  uint32_t SpawnAt(uint32_t pid, uint64_t created, std::string_view exe, std::string_view folder, uint32_t parent) {
    Exit(pid);
    clock_ += 10;
    ProcessEntry e;
    e.pid = pid;
    e.parent_pid = parent;
    e.created = created != 0 ? created : clock_;
    e.exe = std::string(exe);
    e.folder = std::string(folder);
    procs.push_back(std::move(e));
    next_pid_ = std::max(next_pid_, pid) + 4;
    return pid;
  }
  ProcessEntry &Proc(uint32_t pid) {
    for (ProcessEntry &e : procs) {
      if (e.pid == pid) {
        return e;
      }
    }
    ADD_FAILURE() << "no process " << pid;
    return procs.front();
  }
  uint64_t Created(uint32_t pid) { return Proc(pid).created; }
  ProcessKey Key(uint32_t pid) { return ProcessKey{pid, Created(pid)}; }
  // Removes the process and its sessions (its children stay: orphans).
  void Exit(uint32_t pid) {
    procs.erase(std::remove_if(procs.begin(), procs.end(), [&](const ProcessEntry &e) { return e.pid == pid; }),
                procs.end());
    StopSessions(pid);
  }
  void Render(uint32_t pid, bool active = true, int64_t last_active_ns = 0) {
    Session s;
    s.pid = pid;
    s.flow = Flow::kRender;
    s.state = active ? SessionState::kActive : SessionState::kInactive;
    s.playing = active;
    s.last_active_ns = last_active_ns;
    sessions.push_back(s);
  }
  void Capture(uint32_t pid) {
    Session s;
    s.pid = pid;
    s.flow = Flow::kCapture;
    s.state = SessionState::kActive;
    sessions.push_back(s);
  }
  void StopSessions(uint32_t pid, std::optional<Flow> flow = std::nullopt) {
    sessions.erase(std::remove_if(sessions.begin(), sessions.end(),
                                  [&](const Session &s) { return s.pid == pid && (!flow || s.flow == *flow); }),
                   sessions.end());
  }
  ProcessTable Table() const { return ProcessTable(procs); }

  std::vector<ProcessEntry> procs;
  std::vector<Session> sessions;

private:
  uint32_t next_pid_ = 1000;
  uint64_t clock_ = 1000;
};

// The include streams an engine holds while a plan's handovers run: the live ones it keeps, the ones whose removal
// waits (RemovalReason::kCovered until the coverer runs, kEngine until the crossfade ends) and the new ones.
inline std::size_t HeldIncludeStreams(const PlanInput &in, const Plan &plan) {
  std::vector<char> gone(in.streams.size(), 0);
  for (const Removal &r : plan.remove) {
    if (r.reason != RemovalReason::kCovered && r.reason != RemovalReason::kEngine) {
      gone.at(r.stream) = 1;
    }
  }
  std::size_t held = 0;
  for (std::size_t j = 0; j < in.streams.size(); ++j) {
    held += in.streams[j].kind == StreamKind::kInclude && !gone[j] ? 1 : 0;
  }
  for (const Activation &a : plan.activate) {
    held += a.kind == StreamKind::kInclude ? 1 : 0;
  }
  return held;
}

// A planner and the streams a fake engine holds: Step() reconciles and applies the plan.
class Sim {
public:
  explicit Sim(Exclusions ex, PlannerOptions opts = {}) : planner(opts) { planner.SetExclusions(std::move(ex)); }

  // One reconcile at now_ns; activations pass the commit check against the same snapshot and become live streams
  // (with a held handle unless no_handle names their pid). Removals apply at once, except that with defer_covered a
  // kCovered stream stays for one more step, as the watcher keeps it until its coverer runs. Every step checks the
  // invariants and the include cap (cap + 1 while a handover runs).
  const Plan &Step(const World &w, int64_t now_ns = 0, uint32_t mode = IM_AUDIO_MODE_EXCLUDE_APPS,
                   bool process_loopback = true) {
    table = w.Table();
    sessions = w.sessions;
    PlanInput in;
    in.now_ns = now_ns;
    in.mode = mode;
    in.process_loopback = process_loopback;
    in.sessions = sessions;
    in.streams = streams;
    in.capture_failed = capture_failed;
    plan = planner.Reconcile(table, in);
    EXPECT_TRUE(plan.violations.empty()) << Describe(plan.violations);
    const auto check = CheckInvariants(table, in, plan, planner.exclusions(), planner.options());
    EXPECT_TRUE(check.empty()) << Describe(check);
    const std::size_t cap = planner.options().max_include_streams;
    EXPECT_LE(plan.include_targets.size(), cap);
    held = HeldIncludeStreams(in, plan);
    EXPECT_LE(held, cap + 1);
    waiting.resize(streams.size(), 0);
    std::vector<char> removed(streams.size(), 0);
    std::vector<char> waits(streams.size(), 0);
    for (const Removal &r : plan.remove) {
      if (defer_covered && r.reason == RemovalReason::kCovered && !waiting.at(r.stream)) {
        waits.at(r.stream) = 1;
      } else {
        removed.at(r.stream) = 1;
      }
    }
    std::vector<LiveStream> next;
    std::vector<char> next_waiting;
    for (std::size_t j = 0; j < streams.size(); ++j) {
      if (!removed[j]) {
        next.push_back(streams[j]);
        next_waiting.push_back(waits[j]);
      }
    }
    // Activations complete after every process of the snapshot started, unless the test sets the clock.
    uint64_t activated = activation_clock;
    if (activated == 0) {
      for (const Process &p : table.processes()) {
        activated = std::max(activated, p.key.created + 1);
      }
    }
    for (const Activation &a : plan.activate) {
      if (!planner.CommitCheck(table, a).ok) {
        continue;
      }
      LiveStream s;
      s.kind = a.kind;
      s.target = a.target;
      s.parent_pid = a.parent_pid;
      s.exe = a.exe;
      s.has_handle = std::find(no_handle.begin(), no_handle.end(), a.target.pid) == no_handle.end();
      s.activated_filetime = activated;
      next.push_back(std::move(s));
      next_waiting.push_back(0);
    }
    streams = std::move(next);
    waiting = std::move(next_waiting);
    EXPECT_LE(IncludeStreams(), cap + 1);
    return plan;
  }

  static std::string Describe(const std::vector<Violation> &v) {
    std::string s;
    for (const Violation &x : v) {
      s += "I" + std::to_string(static_cast<int>(x.invariant)) + ": " + x.what + "\n";
    }
    return s;
  }

  // The session result of pid's first session of the flow.
  const SessionResult &SessionOf(uint32_t pid, Flow flow = Flow::kRender) const {
    for (std::size_t k = 0; k < sessions.size(); ++k) {
      if (sessions[k].pid == pid && sessions[k].flow == flow) {
        return plan.sessions.at(k);
      }
    }
    ADD_FAILURE() << "no session for " << pid;
    static const SessionResult none;
    return none;
  }
  const ProcessResult &ProcessOf(uint32_t pid) const {
    const auto i = table.Find(pid);
    if (!i) {
      ADD_FAILURE() << "no process " << pid;
      static const ProcessResult none;
      return none;
    }
    return plan.processes.at(*i);
  }
  const AppRow *App(std::string_view id) const {
    for (const AppRow &r : plan.apps) {
      if (r.id == id) {
        return &r;
      }
    }
    return nullptr;
  }
  bool IsTarget(uint32_t pid) const {
    return std::any_of(plan.include_targets.begin(), plan.include_targets.end(),
                       [&](const ProcessKey &k) { return k.pid == pid; });
  }
  bool HasStream(uint32_t pid, StreamKind kind = StreamKind::kInclude) const {
    return std::any_of(streams.begin(), streams.end(),
                       [&](const LiveStream &s) { return s.kind == kind && s.target.pid == pid; });
  }
  std::size_t IncludeStreams() const {
    return static_cast<std::size_t>(std::count_if(streams.begin(), streams.end(),
                                                  [](const LiveStream &s) { return s.kind == StreamKind::kInclude; }));
  }
  // The removal reasons of this plan for streams on pid.
  std::vector<RemovalReason> RemovalsOf(uint32_t pid, const std::vector<LiveStream> &before) const {
    std::vector<RemovalReason> out;
    for (const Removal &r : plan.remove) {
      if (before.at(r.stream).target.pid == pid) {
        out.push_back(r.reason);
      }
    }
    return out;
  }

  Planner planner;
  ProcessTable table;
  std::vector<Session> sessions;
  std::vector<LiveStream> streams;
  std::vector<ProcessKey> capture_failed;
  std::vector<uint32_t> no_handle;
  uint64_t activation_clock = 0; // activated_filetime of new streams; 0: just after the snapshot's newest process
  bool defer_covered = false;    // kCovered streams stay one more step (their coverer starts running)
  std::vector<char> waiting;     // per stream: its kCovered removal was deferred in the last step
  std::size_t held = 0;          // HeldIncludeStreams of the last step's plan
  Plan plan;
};

} // namespace policytest
