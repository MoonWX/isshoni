// The property test of docs/m2/05-windows-audio.md §6.3, §19.1: invariants I1-I5 after randomized event sequences,
// 10 000 seeds (IM_TEST_SHORT=1: 500; IM_AUDIOPOLICY_SEEDS=<n> picks another count).
//
// Each seed builds a small world (isshoni, voice apps, launchers, browsers, WebView2, games; folders known and
// unknown; creation times unknown or on the parent's tick) and runs a fake engine against a Planner: processes start,
// exit and get reused pids (never one a held handle protects), sessions come and go, the mic is taken and released,
// rules, flags, the shared pid, the mode and the probe change, time jumps, activations fail or run without a handle,
// and the world changes between a reconcile and its commit check. After every reconcile:
//   - CheckInvariants (I1-I5 of the plan) finds nothing, and neither does the planner's own debug check;
//   - an oracle that doesn't use the classifier (a process whose own exe is a built-in, kept-out-by-user or viewer
//     exe, or that lies in isshoni's tree or data folder, must be kept out) finds no such process in a running include
//     target's tree, at the target, or outside R's tree in exclude-self, and none in a stream that just passed the
//     commit check on the changed world;
//   - I5 holds over the streams the engine holds, the fading one of a crossfade included;
//   - the engine never holds more than cap + 1 include streams, counting covered streams that wait for their coverer
//     (some seeds keep them for one more step, as the watcher does until the coverer runs);
//   - nothing heard is lost needlessly: a heard, safe render session is a target, covered by one, or cut only when
//     the include set is full (or the streams a handover still holds fill it).
// Seeds run on several threads, each with its own planner, which also gives ThreadSanitizer (host-tsan) something to
// look at: the library must keep no shared state.
#include <gtest/gtest.h>

#include <algorithm>
#include <atomic>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <deque>
#include <mutex>
#include <set>
#include <string>
#include <thread>
#include <vector>

#include "im/audiopolicy/exclusions.h"
#include "im/audiopolicy/planner.h"
#include "im/audiopolicy/process_table.h"
#include "im/audiopolicy/text.h"
#include "isshoni_media.h"

namespace {

using namespace im::audiopolicy;

// SplitMix64: the same sequence on every platform (std:: distributions differ between standard libraries).
class Rng {
public:
  explicit Rng(uint64_t seed) : s_(seed * 0x2545F4914F6CDD1Dull + 1) {}
  uint64_t Next() {
    s_ += 0x9E3779B97F4A7C15ull;
    uint64_t z = s_;
    z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9ull;
    z = (z ^ (z >> 27)) * 0x94D049BB133111EBull;
    return z ^ (z >> 31);
  }
  uint32_t Below(std::size_t n) { return static_cast<uint32_t>(Next() % n); }
  bool Chance(uint32_t percent) { return Below(100) < percent; }
  template <class T> const T &Pick(const std::vector<T> &v) { return v[Below(v.size())]; }

private:
  uint64_t s_;
};

std::string EnvString(const char *name) {
#if defined(_MSC_VER)
  char *buf = nullptr;
  std::size_t len = 0;
  std::string out;
  if (_dupenv_s(&buf, &len, name) == 0 && buf != nullptr) {
    out = buf;
    free(buf);
  }
  return out;
#else
  const char *v = std::getenv(name);
  return v != nullptr ? std::string(v) : std::string();
#endif
}

uint32_t SeedCount() {
  const std::string n = EnvString("IM_AUDIOPOLICY_SEEDS");
  if (!n.empty()) {
    return static_cast<uint32_t>(std::strtoul(n.c_str(), nullptr, 10));
  }
  return EnvString("IM_TEST_SHORT") == "1" ? 500u : 10000u;
}

const std::vector<std::string> kVoiceExes = {"discord.exe", "teamspeak.exe", "ms-teams.exe",
                                             "slack.exe",   "zoom.exe",      "mumble.exe"};
const std::vector<std::string> kLauncherExes = {"explorer.exe", "steam.exe", "cmd.exe"};
const std::vector<std::string> kBrowserExes = {"chrome.exe", "msedge.exe", "firefox.exe"};
const std::vector<std::string> kAppExes = {"game.exe",      "game2.exe", "spotify.exe", "helper.exe",
                                           "fakevoice.exe", "kook.exe",  "update.exe",  "launcherx.exe"};
const std::vector<std::string> kFolders = {R"(C:\A)",
                                           R"(C:\A\sub)",
                                           R"(C:\B)",
                                           R"(C:\WebView)",
                                           R"(D:\Games\G)",
                                           R"(D:\Games\G\bin)",
                                           "",
                                           R"(C:\Windows)",
                                           R"(C:\Users\test\AppData\Local\Discord\app-1.0)"};
const std::string kWebViewData = R"(C:\Users\test\AppData\Local\isshoni\WebView2)";
constexpr uint32_t kMe = 900;
constexpr uint32_t kVoiceParent = 800;
constexpr uint32_t kExplorerPid = 100;
constexpr int64_t kMs = 1'000'000;

struct UserRule {
  uint32_t kind;
  uint32_t source;
  std::string exe;
};

// A removal that waits (05 §6.2 step 3's covered stream until its coverer runs, an include stream until the engine's
// crossfade ends): the stream still runs after the plan.
bool Waits(const PlanInput &in, const Removal &r) {
  return in.streams[r.stream].kind == StreamKind::kInclude &&
         (r.reason == RemovalReason::kCovered || r.reason == RemovalReason::kEngine);
}

// The include streams an engine holds while a plan's handovers run: kept, waiting and new ones.
std::size_t HeldIncludeStreams(const PlanInput &in, const Plan &plan) {
  std::vector<char> gone(in.streams.size(), 0);
  for (const Removal &r : plan.remove) {
    if (!Waits(in, r)) {
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

// What the runs reached, so a generator that stopped producing interesting worlds fails instead of passing vacuously.
struct Coverage {
  uint64_t engines[4] = {};      // by Engine
  uint64_t removals[10] = {};    // by RemovalReason
  uint64_t reasons[11] = {};     // session reasons, by Reason
  uint64_t covered = 0;          // sessions with note covered
  uint64_t no_handle = 0;        // sessions with note no_handle
  uint64_t commit_rejected = 0;  // activations the commit check refused
  uint64_t lineage = 0;          // processes decided through the lineage cache
  uint64_t pid_reuse_blocks = 0; // the counter
  uint64_t fast_path_exits = 0;  // exclude-self -> include-set
  uint64_t voice_root = 0;       // exclude-self with R = the voice app that started isshoni
  uint64_t deferred = 0;         // covered streams the engine kept until their coverer ran
  uint64_t over_cap = 0;         // plans whose handover holds cap + 1 include streams

  void Add(const Coverage &o) {
    voice_root += o.voice_root;
    deferred += o.deferred;
    over_cap += o.over_cap;
    for (int i = 0; i < 4; ++i) {
      engines[i] += o.engines[i];
    }
    for (int i = 0; i < 10; ++i) {
      removals[i] += o.removals[i];
    }
    for (int i = 0; i < 11; ++i) {
      reasons[i] += o.reasons[i];
    }
    covered += o.covered;
    no_handle += o.no_handle;
    commit_rejected += o.commit_rejected;
    lineage += o.lineage;
    pid_reuse_blocks += o.pid_reuse_blocks;
    fast_path_exits += o.fast_path_exits;
  }
};

// One seed's world and fake engine.
class PropertyRun {
public:
  explicit PropertyRun(uint64_t seed) : rng_(seed), seed_(seed) {}

  // Runs the seed; returns "" or the first failure.
  std::string Run() {
    Setup();
    for (int step = 0; step < kSteps && failure_.empty(); ++step) {
      const int events = 1 + static_cast<int>(rng_.Below(3));
      for (int i = 0; i < events; ++i) {
        Event();
      }
      Reconcile(step);
    }
    return failure_;
  }

  // A hash of every plan's engine, targets and removals.
  uint64_t Digest() const noexcept { return digest_; }
  const Coverage &Covered() const noexcept { return cov_; }

private:
  static constexpr int kSteps = 24;
  static constexpr std::size_t kMaxProcs = 30;

  // ---------------------------------------------------------------------------------------- the world
  void Setup() {
    Add(kExplorerPid, 1000, "explorer.exe", R"(C:\Windows)", 4);
    // Sometimes a voice app started isshoni (05 §6.4: the fast path's R is then that voice app), and some of its
    // other children live in other folders (heard: R's tree must not hold them).
    const bool voice_parent = rng_.Chance(25);
    focus_ = voice_parent && rng_.Chance(60);
    if (voice_parent) {
      Add(kVoiceParent, 1500, rng_.Pick(kVoiceExes), R"(C:\Users\test\AppData\Local\Discord\app-1.0)", kExplorerPid);
    }
    Add(kMe, 2000, "isshoni.exe", R"(C:\Users\test\AppData\Local\Programs\isshoni)",
        voice_parent ? kVoiceParent : kExplorerPid);
    clock_ = 3000;
    const int n = 4 + static_cast<int>(rng_.Below(10));
    for (int i = 0; i < n; ++i) {
      Spawn();
    }
    PlannerOptions opts;
    opts.loose_links = !rng_.Chance(15);
    opts.late_child_own_stream = rng_.Chance(25);
    opts.fast_path_exit_on_new_descendant = rng_.Chance(25);
    opts.max_include_streams = rng_.Chance(30) ? 1 + rng_.Below(4) : 48;
    planner_ = Planner(opts);
    defer_covered_ = rng_.Chance(50);
    flags_ = (rng_.Chance(70) ? IM_EXCLF_KEEP_OUT_MIC_USERS : 0u) | (rng_.Chance(60) ? IM_EXCLF_FAST_PATH : 0u);
    for (int i = 0; i < 3; ++i) {
      if (rng_.Chance(40)) {
        AddUserRule();
      }
    }
    ApplyRules();
  }

  void Add(uint32_t pid, uint64_t created, const std::string &exe, const std::string &folder, uint32_t parent) {
    ProcessEntry e;
    e.pid = pid;
    e.parent_pid = parent;
    e.created = created;
    e.exe = exe;
    e.folder = folder;
    procs_.push_back(std::move(e));
  }

  ProcessEntry *Get(uint32_t pid) {
    for (ProcessEntry &e : procs_) {
      if (e.pid == pid) {
        return &e;
      }
    }
    return nullptr;
  }

  bool HeldHandle(uint32_t pid) const {
    return std::any_of(streams_.begin(), streams_.end(), [&](const LiveStream &s) {
      return s.kind != StreamKind::kEndpoint && s.target.pid == pid && s.has_handle;
    });
  }

  // A pid from a small pool, so pids get reused; never one in use, and never one a held handle protects.
  uint32_t FreePid() {
    for (int tries = 0; tries < 20; ++tries) {
      const uint32_t pid = 1000 + 4 * rng_.Below(40);
      if (Get(pid) == nullptr && !HeldHandle(pid)) {
        return pid;
      }
    }
    return 0;
  }

  void Spawn() {
    if (procs_.size() >= kMaxProcs) {
      return;
    }
    const uint32_t pid = FreePid();
    if (pid == 0) {
      return;
    }
    std::string exe;
    // Focused seeds keep voice apps rare outside the one that started isshoni, so the fast path's R is that app.
    const uint32_t kind = focus_ ? 22 + rng_.Below(78) : rng_.Below(100);
    if (kind < 22) {
      exe = rng_.Pick(kVoiceExes);
    } else if (kind < 32) {
      exe = rng_.Pick(kLauncherExes);
    } else if (kind < 44) {
      exe = rng_.Pick(kBrowserExes);
    } else if (kind < 56) {
      exe = "msedgewebview2.exe";
    } else if (kind < 59) {
      exe = "isshoni.exe";
    } else {
      exe = rng_.Pick(kAppExes);
    }
    // Parent: an existing process (often isshoni or Explorer), or a pid that is gone.
    uint32_t parent = kExplorerPid;
    const uint32_t p = rng_.Below(100);
    if (focus_ && p < 40) {
      parent = kVoiceParent;
    } else if (p < 15) {
      parent = kMe;
    } else if (p < 75 && !procs_.empty()) {
      parent = procs_[rng_.Below(procs_.size())].pid;
    } else if (p < 85) {
      parent = 1000 + 4 * rng_.Below(40);
    }
    clock_ += 1 + rng_.Below(20);
    uint64_t created = clock_;
    const uint32_t t = rng_.Below(100);
    if (t < 6) {
      created = 0; // couldn't be opened
    } else if (t < 12) {
      if (const ProcessEntry *pe = Get(parent)) {
        created = pe->created; // the same clock tick as its parent
      }
    }
    Add(pid, created, exe, rng_.Pick(kFolders), parent);
    if (exe == "msedgewebview2.exe" && rng_.Chance(60)) {
      procs_.back().user_data_dir = rng_.Chance(70) ? kWebViewData + R"(\EBWebView)" : kWebViewData + "2";
    }
  }

  void Exit(uint32_t pid) {
    if (pid == kMe || pid == kExplorerPid) {
      return;
    }
    procs_.erase(std::remove_if(procs_.begin(), procs_.end(), [&](const ProcessEntry &e) { return e.pid == pid; }),
                 procs_.end());
    sessions_.erase(std::remove_if(sessions_.begin(), sessions_.end(), [&](const Session &s) { return s.pid == pid; }),
                    sessions_.end());
  }

  uint32_t RandomPid() { return procs_[rng_.Below(procs_.size())].pid; }

  // Families for 05 §6.2 step 3: the parent of an include target starts playing (its stream takes over the target's,
  // a handover), a target starts a child that plays (covered by its stream), or an app starts with one to three
  // playing children (whose parent may play later).
  void Family() {
    Session s;
    s.flow = Flow::kRender;
    s.state = SessionState::kActive;
    s.playing = true;
    s.last_active_ns = now_;
    uint32_t c = rng_.Below(4);
    if (c < 2) {
      std::vector<uint32_t> parents;
      for (const LiveStream &x : streams_) {
        const ProcessEntry *t = x.kind == StreamKind::kInclude ? Get(x.target.pid) : nullptr;
        const ProcessEntry *p = t != nullptr ? Get(t->parent_pid) : nullptr;
        if (p != nullptr && p->pid != kMe &&
            std::find(kLauncherExes.begin(), kLauncherExes.end(), p->exe) == kLauncherExes.end()) {
          parents.push_back(p->pid);
        }
      }
      if (!parents.empty()) {
        s.pid = rng_.Pick(parents);
        sessions_.push_back(s);
        return;
      }
      c = 3; // no such parent yet: start an app with a playing child
    }
    if (procs_.size() + 4 > kMaxProcs) {
      return;
    }
    uint32_t parent = 0;
    std::string folder;
    if (c == 2) {
      std::vector<uint32_t> targets;
      for (const LiveStream &x : streams_) {
        if (x.kind == StreamKind::kInclude && Get(x.target.pid) != nullptr) {
          targets.push_back(x.target.pid);
        }
      }
      if (targets.empty()) {
        return;
      }
      parent = rng_.Pick(targets);
      folder = Get(parent)->folder;
    } else {
      parent = FreePid();
      if (parent == 0) {
        return;
      }
      clock_ += 1;
      folder = rng_.Pick(kFolders);
      Add(parent, clock_, rng_.Pick(kAppExes), folder, kExplorerPid);
    }
    // One to three playing children: a parent that starts playing later replaces several streams at once.
    const uint32_t kids = c == 2 ? 1 : 1 + rng_.Below(3);
    for (uint32_t k = 0; k < kids; ++k) {
      if (const uint32_t pid = FreePid()) {
        clock_ += 1;
        Add(pid, clock_, rng_.Pick(kAppExes), folder, parent);
        s.pid = pid;
        sessions_.push_back(s);
      }
    }
  }

  void AddUserRule() {
    std::vector<std::string> pool = kAppExes;
    pool.insert(pool.end(), kBrowserExes.begin(), kBrowserExes.end());
    pool.push_back("msedgewebview2.exe");
    pool.push_back(rng_.Pick(kVoiceExes));
    const uint32_t k = rng_.Below(3);
    const uint32_t kind = k == 1 ? IM_RULE_APP_ALLOW : IM_RULE_APP_EXCLUDE;
    const uint32_t source = k == 2 ? IM_RULE_SRC_VIEWER : IM_RULE_SRC_USER;
    user_rules_.push_back(UserRule{kind, source, rng_.Pick(pool)});
  }

  void ApplyRules() {
    std::vector<std::string> values;
    std::vector<im_rule> rules;
    values.reserve(256);
    const auto add = [&](uint32_t kind, uint32_t source, const std::string &value, const char *id, const char *name) {
      values.push_back(value);
      rules.push_back(im_rule{kind, source, nullptr, id, name});
    };
    add(IM_RULE_INSTANCE, IM_RULE_SRC_SELF, std::to_string(kMe), "isshoni", "isshoni");
    add(IM_RULE_APP_EXCLUDE, IM_RULE_SRC_SELF, "isshoni.exe", "isshoni", "isshoni");
    add(IM_RULE_WEBVIEW_DATA_DIR, IM_RULE_SRC_SELF, kWebViewData, "isshoni", "isshoni");
    for (const std::string &l : kLauncherExes) {
      add(IM_RULE_STOP, IM_RULE_SRC_DATA, l, nullptr, nullptr);
    }
    add(IM_RULE_SHARED_RUNTIME, IM_RULE_SRC_DATA, "msedgewebview2.exe", nullptr, nullptr);
    for (const std::string &b : kBrowserExes) {
      add(IM_RULE_BROWSER, IM_RULE_SRC_DATA, b, nullptr, nullptr);
    }
    for (const std::string &v : kVoiceExes) {
      add(IM_RULE_APP_EXCLUDE, IM_RULE_SRC_BUILTIN, v, nullptr, nullptr);
    }
    for (const UserRule &u : user_rules_) {
      add(u.kind, u.source, u.exe, nullptr, nullptr);
    }
    for (std::size_t i = 0; i < rules.size(); ++i) {
      rules[i].value = values[i].c_str();
    }
    im_exclusions ex{};
    ex.struct_size = sizeof(im_exclusions);
    ex.flags = flags_;
    ex.rules = rules.data();
    ex.rule_count = static_cast<uint32_t>(rules.size());
    ex.shared_pid = shared_;
    Exclusions out;
    if (ParseExclusions(&ex, kMe, out) != IM_OK) {
      Fail("ParseExclusions failed");
      return;
    }
    planner_.SetExclusions(std::move(out));
  }

  void Event() {
    const uint32_t e = rng_.Below(100);
    if (e < 18) {
      Spawn();
    } else if (e < 28) {
      Exit(RandomPid());
    } else if (e < 33) {
      Family();
    } else if (e < 44) {
      Session s;
      s.pid = RandomPid();
      s.flow = Flow::kRender;
      s.state = rng_.Chance(70) ? SessionState::kActive : SessionState::kInactive;
      s.playing = s.state == SessionState::kActive;
      s.multi_process = rng_.Chance(5);
      s.last_active_ns = now_ - static_cast<int64_t>(rng_.Below(5000)) * kMs;
      sessions_.push_back(s);
    } else if (e < 52) {
      if (!sessions_.empty()) {
        sessions_.erase(sessions_.begin() + rng_.Below(sessions_.size()));
      }
    } else if (e < 58) {
      Session s;
      s.pid = RandomPid();
      s.flow = Flow::kCapture;
      s.state = SessionState::kActive;
      sessions_.push_back(s);
    } else if (e < 62) {
      if (!sessions_.empty()) {
        Session &s = sessions_[rng_.Below(sessions_.size())];
        s.state = s.state == SessionState::kActive ? SessionState::kInactive : SessionState::kActive;
        s.last_active_ns = now_;
      }
    } else if (e < 68) {
      if (rng_.Chance(50) || user_rules_.empty()) {
        AddUserRule();
      } else {
        user_rules_.erase(user_rules_.begin() + rng_.Below(user_rules_.size()));
      }
      ApplyRules();
    } else if (e < 71) {
      flags_ ^= rng_.Chance(50) ? IM_EXCLF_KEEP_OUT_MIC_USERS : IM_EXCLF_FAST_PATH;
      ApplyRules();
    } else if (e < 74) {
      shared_ = rng_.Chance(50) ? 0 : RandomPid();
      ApplyRules();
    } else if (e < 77) {
      mode_ = mode_ == IM_AUDIO_MODE_OFF ? IM_AUDIO_MODE_EXCLUDE_APPS
                                         : (rng_.Chance(70) ? IM_AUDIO_MODE_OFF : IM_AUDIO_MODE_EXCLUDE_APPS);
    } else if (e < 80) {
      loopback_ = !loopback_;
    } else if (e < 82) {
      planner_.Restart(); // a new share: the engine starts again with no streams
      streams_.clear();
      waiting_.clear();
      fading_until_ = 0;
    } else if (e < 86) {
      // An include stream fails for good (05 §8.5): the watcher drops it and reports capture_failed.
      for (std::size_t j = 0; j < streams_.size(); ++j) {
        if (streams_[j].kind == StreamKind::kInclude) {
          failed_.push_back(streams_[j].target);
          streams_.erase(streams_.begin() + static_cast<std::ptrdiff_t>(j));
          if (j < waiting_.size()) {
            waiting_.erase(waiting_.begin() + static_cast<std::ptrdiff_t>(j));
          }
          break;
        }
      }
    } else if (e < 88) {
      failed_.clear(); // a device or session event: retry them
    } else if (e < 92) {
      // 05 §5.7: a target running without a held handle exits and its pid comes back as another process.
      for (const LiveStream &s : streams_) {
        if (s.kind == StreamKind::kInclude && !s.has_handle && Get(s.target.pid) != nullptr) {
          const ProcessEntry old = *Get(s.target.pid);
          Exit(old.pid);
          clock_ += 1;
          Add(old.pid, rng_.Chance(20) ? 0 : clock_, rng_.Chance(50) ? rng_.Pick(kVoiceExes) : rng_.Pick(kAppExes),
              old.folder, rng_.Chance(50) ? old.parent_pid : kExplorerPid);
          if (rng_.Chance(70)) {
            Session r;
            r.pid = old.pid;
            r.flow = Flow::kRender;
            r.state = SessionState::kActive;
            r.playing = true;
            sessions_.push_back(r);
          }
          break;
        }
      }
    } else {
      static const int64_t jumps[] = {0, 10 * kMs, 100 * kMs, 1000 * kMs, 3000 * kMs, 11000 * kMs};
      now_ += jumps[rng_.Below(6)];
    }
  }

  // ---------------------------------------------------------------------------------------- one reconcile
  void Reconcile(int step) {
    const ProcessTable table(procs_);
    PlanInput in;
    in.now_ns = now_;
    in.mode = mode_;
    in.process_loopback = loopback_;
    in.sessions = sessions_;
    in.streams = streams_;
    in.capture_failed = failed_;
    const Plan plan = planner_.Reconcile(table, in);
    const std::string where = " (seed " + std::to_string(seed_) + ", step " + std::to_string(step) + ")";
    if (!plan.violations.empty()) {
      const Violation &v = plan.violations.front();
      return Fail("planner debug check: I" + std::to_string(static_cast<int>(v.invariant)) + " " + v.what + where);
    }
    if (const auto check = CheckInvariants(table, in, plan, planner_.exclusions(), planner_.options());
        !check.empty()) {
      const Violation &v = check.front();
      return Fail("I" + std::to_string(static_cast<int>(v.invariant)) + " " + v.what + where);
    }
    if (const std::string o = OracleAfterPlan(table, in, plan); !o.empty()) {
      return Fail(o + where);
    }
    if (const std::string l = NoNeedlessLoss(table, in, plan); !l.empty()) {
      return Fail(l + where);
    }
    const std::size_t cap = planner_.options().max_include_streams;
    const std::size_t handover = HeldIncludeStreams(in, plan);
    if (handover > cap + 1) {
      return Fail("the handover holds " + std::to_string(handover) + " include streams, cap " + std::to_string(cap) +
                  where);
    }
    cov_.over_cap += handover == cap + 1 ? 1 : 0;
    digest_ = digest_ * 31 + static_cast<uint64_t>(plan.engine);
    for (const ProcessKey &k : plan.include_targets) {
      digest_ = digest_ * 31 + k.pid;
    }
    digest_ = digest_ * 31 + plan.remove.size();
    ++cov_.engines[static_cast<int>(plan.engine)];
    for (const Removal &r : plan.remove) {
      ++cov_.removals[static_cast<int>(r.reason)];
    }
    for (const SessionResult &sr : plan.sessions) {
      ++cov_.reasons[static_cast<int>(sr.reason)];
      cov_.covered += sr.note == Note::kCovered ? 1 : 0;
      cov_.no_handle += sr.note == Note::kNoHandle ? 1 : 0;
    }
    for (const ProcessResult &p : plan.processes) {
      cov_.lineage += p.lineage ? 1 : 0;
    }
    cov_.pid_reuse_blocks += plan.pid_reuse_blocks;
    cov_.fast_path_exits += plan.previous_engine == Engine::kExcludeSelf && plan.engine == Engine::kIncludeSet ? 1 : 0;
    cov_.voice_root += plan.engine == Engine::kExcludeSelf && plan.exclude_target.pid == kVoiceParent ? 1 : 0;

    // Apply: removals at once (an exclusive one fades for the 20 ms crossfade; with defer_covered_ a covered stream
    // stays one more step, until its coverer runs), then activations, each through the commit check on a snapshot
    // that may have changed meanwhile.
    waiting_.resize(streams_.size(), 0);
    std::vector<char> removed(streams_.size(), 0);
    std::vector<char> waits(streams_.size(), 0);
    for (const Removal &r : plan.remove) {
      if (defer_covered_ && r.reason == RemovalReason::kCovered && !waiting_.at(r.stream)) {
        waits.at(r.stream) = 1;
        ++cov_.deferred;
        continue;
      }
      removed.at(r.stream) = 1;
      if (streams_[r.stream].kind != StreamKind::kInclude) {
        fading_until_ = now_ + 20 * kMs;
      }
    }
    std::vector<LiveStream> next;
    std::vector<char> next_waiting;
    for (std::size_t j = 0; j < streams_.size(); ++j) {
      if (!removed[j]) {
        next.push_back(streams_[j]);
        next_waiting.push_back(waits[j]);
      }
    }
    if (rng_.Chance(20)) {
      MidActivationChange();
    }
    const ProcessTable fresh(procs_);
    for (const Activation &a : plan.activate) {
      if (rng_.Chance(5)) {
        continue; // the activation failed or timed out
      }
      if (!planner_.CommitCheck(fresh, a).ok) {
        ++cov_.commit_rejected;
        continue;
      }
      LiveStream s;
      s.kind = a.kind;
      s.target = a.target;
      s.parent_pid = a.parent_pid;
      s.exe = a.exe;
      s.has_handle = !rng_.Chance(20);
      s.activated_filetime = clock_ + 1;
      if (const std::string o = OracleCommitted(fresh, s); !o.empty()) {
        return Fail(o + where);
      }
      next.push_back(std::move(s));
      next_waiting.push_back(0);
    }
    streams_ = std::move(next);
    waiting_ = std::move(next_waiting);
    clock_ += 2;
    const auto include = static_cast<std::size_t>(std::count_if(
        streams_.begin(), streams_.end(), [](const LiveStream &x) { return x.kind == StreamKind::kInclude; }));
    if (include > cap + 1) {
      return Fail("the engine holds " + std::to_string(include) + " include streams, cap " + std::to_string(cap) +
                  where);
    }
    std::size_t exclusive = 0;
    for (const LiveStream &s : streams_) {
      exclusive += s.kind != StreamKind::kInclude ? 1 : 0;
    }
    const bool fading = now_ < fading_until_;
    const std::size_t held = exclusive + (fading ? 1 : 0);
    std::vector<LiveStream> all = streams_;
    if (fading) {
      LiveStream f;
      f.kind = StreamKind::kEndpoint;
      all.push_back(f);
    }
    if (!CheckExclusiveStreams(all, fading).empty() || exclusive > 1) {
      return Fail("I5: " + std::to_string(held) + " exclusive streams" + where);
    }
  }

  // The world changes while activations run: a voice app starts under a random process, or a process exits (its pid
  // may come back as another process when no handle holds it).
  void MidActivationChange() {
    const uint32_t c = rng_.Below(3);
    if (c == 0 && procs_.size() < kMaxProcs) {
      if (const uint32_t pid = FreePid()) {
        clock_ += 1;
        Add(pid, clock_, rng_.Pick(kVoiceExes), rng_.Pick(kFolders), RandomPid());
      }
    } else if (c == 1) {
      Exit(RandomPid());
    } else {
      const uint32_t pid = RandomPid();
      if (pid != kMe && pid != kExplorerPid && !HeldHandle(pid)) {
        ProcessEntry old = *Get(pid);
        Exit(pid);
        clock_ += 1;
        Add(pid, clock_, rng_.Pick(kAppExes), old.folder, old.parent_pid);
      }
    }
  }

  // ---------------------------------------------------------------------------------------- the oracle
  struct Oracle {
    std::vector<char> must_keep_out; // by table index
    std::vector<std::vector<std::size_t>> kids;
  };

  Oracle MakeOracle(const ProcessTable &t, bool loose) const {
    Oracle o;
    o.must_keep_out.assign(t.size(), 0);
    o.kids.assign(t.size(), {});
    std::vector<std::vector<std::size_t>> strict_kids(t.size());
    for (std::size_t i = 0; i < t.size(); ++i) {
      if (const int32_t p = t.Parent(i, loose); p >= 0) {
        o.kids[static_cast<std::size_t>(p)].push_back(i);
      }
      if (const int32_t p = t.Parent(i, false); p >= 0) {
        strict_kids[static_cast<std::size_t>(p)].push_back(i);
      }
    }
    std::set<std::string> out(kVoiceExes.begin(), kVoiceExes.end());
    for (const UserRule &u : user_rules_) {
      if (u.kind == IM_RULE_APP_EXCLUDE) {
        out.insert(u.exe);
      }
    }
    for (std::size_t i = 0; i < t.size(); ++i) {
      const Process &p = t.at(i);
      if (out.count(p.exe) != 0 || p.exe == "isshoni.exe" ||
          (!p.user_data_dir.empty() && PathEqualsOrBelow(p.user_data_dir, kWebViewData))) {
        o.must_keep_out[i] = 1;
      }
    }
    // Row 6, where nothing can override it: with the mic flag on, no allow rule (row 5) and no shared app (§5.8), a
    // process holding a capture session is kept out (its app root is a mic root).
    const bool any_allow = std::any_of(user_rules_.begin(), user_rules_.end(),
                                       [](const UserRule &u) { return u.kind == IM_RULE_APP_ALLOW; });
    if ((flags_ & IM_EXCLF_KEEP_OUT_MIC_USERS) != 0 && !any_allow && shared_ == 0) {
      for (const Session &s : sessions_) {
        if (s.flow == Flow::kCapture) {
          if (const auto i = t.Find(s.pid)) {
            o.must_keep_out[*i] = 1;
          }
        }
      }
    }
    if (const auto me = t.Find(kMe)) { // isshoni's tree by strict links: row 1 (a loose-only child is older)
      std::deque<std::size_t> q{*me};
      std::vector<char> seen(t.size(), 0);
      seen[*me] = 1;
      while (!q.empty()) {
        const std::size_t i = q.front();
        q.pop_front();
        o.must_keep_out[i] = 1;
        for (const std::size_t c : strict_kids[i]) {
          if (!seen[c]) {
            seen[c] = 1;
            q.push_back(c);
          }
        }
      }
    }
    return o;
  }

  static std::vector<char> Below(const Oracle &o, std::size_t root) {
    std::vector<char> in(o.kids.size(), 0);
    std::deque<std::size_t> q{root};
    in[root] = 1;
    while (!q.empty()) {
      const std::size_t i = q.front();
      q.pop_front();
      for (const std::size_t c : o.kids[i]) {
        if (!in[c]) {
          in[c] = 1;
          q.push_back(c);
        }
      }
    }
    return in;
  }

  std::string CheckTarget(const ProcessTable &t, const Oracle &o, std::size_t target, const char *what) const {
    if (o.must_keep_out[target]) {
      return std::string(what) + ": include target " + std::to_string(t.at(target).key.pid) + " must be kept out";
    }
    if (t.at(target).exe == "explorer.exe" || t.at(target).exe == "steam.exe" || t.at(target).exe == "cmd.exe") {
      return std::string(what) + ": include target " + std::to_string(t.at(target).key.pid) + " is a launcher";
    }
    const auto below = Below(o, target);
    for (std::size_t x = 0; x < t.size(); ++x) {
      if (below[x] && o.must_keep_out[x]) {
        return std::string(what) + ": " + std::to_string(t.at(x).key.pid) + " (must be kept out) in the tree of " +
               std::to_string(t.at(target).key.pid);
      }
    }
    return {};
  }

  std::string OracleAfterPlan(const ProcessTable &t, const PlanInput &in, const Plan &plan) const {
    const Oracle o = MakeOracle(t, planner_.options().loose_links);
    std::vector<char> removed(in.streams.size(), 0);
    for (const Removal &r : plan.remove) {
      if (!Waits(in, r)) {
        removed.at(r.stream) = 1; // a removal that waits leaves the stream running for now
      }
    }
    std::vector<ProcessKey> running;
    for (std::size_t j = 0; j < in.streams.size(); ++j) {
      if (!removed[j] && in.streams[j].kind == StreamKind::kInclude) {
        running.push_back(in.streams[j].target);
      }
    }
    for (const Activation &a : plan.activate) {
      if (a.kind == StreamKind::kInclude) {
        running.push_back(a.target);
      }
    }
    for (const ProcessKey &k : running) {
      const auto i = t.Find(k.pid);
      if (!i) {
        return "oracle: a running target is not in the table";
      }
      if (std::string r = CheckTarget(t, o, *i, "oracle"); !r.empty()) {
        return r;
      }
    }
    if (plan.engine == Engine::kExcludeSelf) {
      const auto r = t.Find(plan.exclude_target);
      if (!r) {
        return "oracle: R is not in the table";
      }
      if (std::find(kLauncherExes.begin(), kLauncherExes.end(), t.at(*r).exe) != kLauncherExes.end()) {
        return "oracle: R is a launcher"; // 05 §6.4: its tree is everything started from it
      }
      if ((planner_.exclusions().flags & IM_EXCLF_FAST_PATH) == 0) {
        return "oracle: exclude-self without the fast-path flag";
      }
      Oracle strict = MakeOracle(t, false);
      const auto below = Below(strict, *r);
      for (std::size_t x = 0; x < t.size(); ++x) {
        if (o.must_keep_out[x] && !below[x]) {
          return "oracle: " + std::to_string(t.at(x).key.pid) + " (must be kept out) outside R's tree";
        }
      }
    }
    if (plan.include_targets.size() > planner_.options().max_include_streams) {
      return "more include targets than the cap";
    }
    std::set<ProcessKey> unique(plan.include_targets.begin(), plan.include_targets.end());
    if (unique.size() != plan.include_targets.size()) {
      return "an include target appears twice";
    }
    return {};
  }

  std::string OracleCommitted(const ProcessTable &fresh, const LiveStream &s) const {
    if (s.kind != StreamKind::kInclude) {
      return {};
    }
    const auto i = fresh.Find(s.target);
    if (!i) {
      return "commit: a stream passed the check without its process";
    }
    const Oracle o = MakeOracle(fresh, planner_.options().loose_links);
    return CheckTarget(fresh, o, *i, "commit");
  }

  // A heard render session with a clean tree is captured: a target, covered by a target, or cut by a full set.
  std::string NoNeedlessLoss(const ProcessTable &t, const PlanInput &in, const Plan &plan) const {
    if (plan.engine != Engine::kIncludeSet) {
      return {};
    }
    std::vector<std::vector<std::size_t>> kids(t.size());
    const bool loose = planner_.options().loose_links;
    for (std::size_t i = 0; i < t.size(); ++i) {
      if (const int32_t p = t.Parent(i, loose); p >= 0) {
        kids[static_cast<std::size_t>(p)].push_back(i);
      }
    }
    std::set<ProcessKey> targets(plan.include_targets.begin(), plan.include_targets.end());
    std::set<ProcessKey> failed(in.capture_failed.begin(), in.capture_failed.end());
    for (std::size_t k = 0; k < in.sessions.size(); ++k) {
      const Session &s = in.sessions[k];
      const SessionResult &sr = plan.sessions[k];
      if (s.flow != Flow::kRender || sr.process < 0) {
        continue;
      }
      const auto i = static_cast<std::size_t>(sr.process);
      const ProcessResult &p = plan.processes[i];
      if (p.verdict != Verdict::kHeard || p.launcher || failed.count(t.at(i).key) != 0) {
        continue;
      }
      std::vector<char> in_tree(t.size(), 0);
      std::deque<std::size_t> q{i};
      in_tree[i] = 1;
      bool dirty = false;
      while (!q.empty() && !dirty) {
        const std::size_t c = q.front();
        q.pop_front();
        dirty = plan.processes[c].verdict == Verdict::kKeptOut;
        for (const std::size_t d : kids[c]) {
          if (!in_tree[d]) {
            in_tree[d] = 1;
            q.push_back(d);
          }
        }
      }
      if (dirty) {
        continue;
      }
      const bool is_target = targets.count(t.at(i).key) != 0;
      if (sr.verdict == Verdict::kHeard && (is_target || sr.note == Note::kCovered)) {
        if (sr.note == Note::kCovered) {
          bool ancestor = false;
          int32_t cur = t.Parent(i, false);
          for (int n = 0; cur >= 0 && n < kMaxWalk && !ancestor; ++n) {
            ancestor = targets.count(t.at(static_cast<std::size_t>(cur)).key) != 0;
            cur = t.Parent(static_cast<std::size_t>(cur), false);
          }
          if (!ancestor) {
            return "a covered session has no target among its ancestors";
          }
        }
        continue;
      }
      const std::size_t cap = planner_.options().max_include_streams;
      if (sr.verdict == Verdict::kNotCaptured && sr.reason == Reason::kLimit &&
          (plan.include_targets.size() >= cap || HeldIncludeStreams(in, plan) >= cap)) {
        continue; // the set is full, or the streams a handover still holds fill it
      }
      return "session of " + std::to_string(s.pid) + " lost: verdict " + std::string(VerdictName(sr.verdict)) + " " +
             std::string(ReasonName(sr.reason));
    }
    return {};
  }

  void Fail(std::string why) {
    if (failure_.empty()) {
      failure_ = std::move(why);
    }
  }

  Rng rng_;
  uint64_t seed_;
  Planner planner_;
  std::vector<ProcessEntry> procs_;
  std::vector<Session> sessions_;
  std::vector<LiveStream> streams_;
  std::vector<char> waiting_; // per stream: its kCovered removal was deferred in the last step
  bool defer_covered_ = false;
  std::vector<ProcessKey> failed_;
  std::vector<UserRule> user_rules_;
  uint32_t flags_ = 0;
  uint32_t shared_ = 0;
  uint32_t mode_ = IM_AUDIO_MODE_EXCLUDE_APPS;
  bool loopback_ = true;
  int64_t now_ = 1'000'000'000;
  int64_t fading_until_ = 0;
  uint64_t clock_ = 0;
  uint64_t digest_ = 0;
  bool focus_ = false;
  Coverage cov_;
  std::string failure_;
};

TEST(AudioPolicyProperty, InvariantsHoldAfterRandomEventSequences) {
  const uint32_t seeds = SeedCount();
  const unsigned threads = std::max(1u, std::min(8u, std::thread::hardware_concurrency()));
  std::atomic<uint32_t> next{0};
  std::mutex mu;
  std::vector<std::string> failures;
  Coverage cov;
  std::vector<std::thread> pool;
  for (unsigned t = 0; t < threads; ++t) {
    pool.emplace_back([&] {
      Coverage mine;
      for (uint32_t seed = next++; seed < seeds; seed = next++) {
        PropertyRun run(seed);
        std::string f = run.Run();
        mine.Add(run.Covered());
        if (!f.empty()) {
          std::lock_guard<std::mutex> lock(mu);
          failures.push_back(std::move(f));
        }
      }
      std::lock_guard<std::mutex> lock(mu);
      cov.Add(mine);
    });
  }
  for (std::thread &th : pool) {
    th.join();
  }
  RecordProperty("seeds", std::to_string(seeds));
  std::sort(failures.begin(), failures.end());
  for (std::size_t i = 0; i < failures.size() && i < 10; ++i) {
    ADD_FAILURE() << failures[i];
  }
  EXPECT_TRUE(failures.empty()) << failures.size() << " of " << seeds << " seeds failed";

  if (seeds < 500) {
    return; // a hand-picked small count: no coverage floor
  }
  const auto engine = [&](Engine e) { return cov.engines[static_cast<int>(e)]; };
  const auto removal = [&](RemovalReason r) { return cov.removals[static_cast<int>(r)]; };
  const auto reason = [&](Reason r) { return cov.reasons[static_cast<int>(r)]; };
  for (const Engine e : {Engine::kNone, Engine::kEndpoint, Engine::kExcludeSelf, Engine::kIncludeSet}) {
    EXPECT_GT(engine(e), 0u) << EngineName(e);
  }
  for (const RemovalReason r :
       {RemovalReason::kExited, RemovalReason::kKeyMismatch, RemovalReason::kKeptOut, RemovalReason::kUnsafe,
        RemovalReason::kSessionGone, RemovalReason::kCovered, RemovalReason::kEngine}) {
    EXPECT_GT(removal(r), 0u) << RemovalReasonName(r);
  }
  for (const Reason r : {Reason::kBuiltin, Reason::kUser, Reason::kMic, Reason::kViewer, Reason::kIsshoni,
                         Reason::kLauncher, Reason::kTree, Reason::kPidReuse, Reason::kLimit, Reason::kCaptureFailed}) {
    EXPECT_GT(reason(r), 0u) << ReasonName(r);
  }
  EXPECT_GT(cov.covered, 0u);
  EXPECT_GT(cov.no_handle, 0u);
  EXPECT_GT(cov.commit_rejected, 0u);
  EXPECT_GT(cov.lineage, 0u);
  EXPECT_GT(cov.pid_reuse_blocks, 0u);
  EXPECT_GT(cov.fast_path_exits, 0u);
  EXPECT_GT(cov.voice_root, 0u);
  EXPECT_GT(cov.deferred, 0u);
  EXPECT_GT(cov.over_cap, 0u);
  std::string summary;
  for (int i = 0; i < 4; ++i) {
    summary += std::string(EngineName(static_cast<Engine>(i))) + "=" + std::to_string(cov.engines[i]) + " ";
  }
  summary += "covered=" + std::to_string(cov.covered) + " commit_rejected=" + std::to_string(cov.commit_rejected) +
             " lineage=" + std::to_string(cov.lineage) + " pid_reuse_blocks=" + std::to_string(cov.pid_reuse_blocks) +
             " deferred=" + std::to_string(cov.deferred) + " over_cap=" + std::to_string(cov.over_cap);
  RecordProperty("coverage", summary);
  std::printf("coverage: %s\n", summary.c_str());
}

// The same seed gives the same run (a failure printed by CI can be replayed on any machine).
TEST(AudioPolicyProperty, RunsAreReproducible) {
  for (const uint64_t seed : {1u, 77u, 4242u}) {
    PropertyRun a(seed);
    PropertyRun b(seed);
    EXPECT_EQ(a.Run(), b.Run());
    EXPECT_EQ(a.Digest(), b.Digest());
    EXPECT_NE(a.Digest(), 0u);
  }
}

} // namespace
