// The exclusion policy and the planner (docs/m2/05-windows-audio.md §5.3-§5.10, §6).
#include "im/audiopolicy/planner.h"

#include <algorithm>
#include <utility>

#include "im/audiopolicy/text.h"

namespace im::audiopolicy {

namespace detail {

struct RuleIndex {
  // The first rule of each kind (and, for exclusions, each source) that names an exe.
  struct ExeRules {
    int32_t self = -1;    // IM_RULE_APP_EXCLUDE, source SELF (row 1)
    int32_t builtin = -1; // IM_RULE_APP_EXCLUDE, source BUILTIN or DATA (row 2)
    int32_t user = -1;    // IM_RULE_APP_EXCLUDE, source USER (row 3)
    int32_t viewer = -1;  // IM_RULE_APP_EXCLUDE, source VIEWER (row 4)
    int32_t allow = -1;   // IM_RULE_APP_ALLOW, any source (row 5)
    int32_t stop = -1;
    int32_t shared_runtime = -1;
    int32_t browser = -1;
  };
  std::unordered_map<std::string, ExeRules> by_exe;
  std::vector<int32_t> instances; // every IM_RULE_INSTANCE, in rule order
  std::vector<int32_t> webview_dirs;

  const ExeRules *Find(const std::string &exe) const {
    const auto it = by_exe.find(exe);
    return it == by_exe.end() ? nullptr : &it->second;
  }
  bool IsLauncher(const std::string &exe) const {
    const ExeRules *e = Find(exe);
    return e != nullptr && e->stop >= 0;
  }
  bool IsSharedRuntime(const std::string &exe) const {
    const ExeRules *e = Find(exe);
    return e != nullptr && e->shared_runtime >= 0;
  }
  bool IsBrowser(const std::string &exe) const {
    const ExeRules *e = Find(exe);
    return e != nullptr && e->browser >= 0;
  }
};

} // namespace detail

namespace {

using detail::KeySet;
using detail::LineageEntry;
using detail::LineageMap;
using detail::RuleIndex;

constexpr std::size_t kMaxLineageExes = kMaxWalk + 1;
constexpr std::size_t kMaxLineageRoots = 8;
constexpr std::size_t kMaxLineageAncestors = kMaxWalk;

std::shared_ptr<const RuleIndex> BuildIndex(const Exclusions &ex) {
  auto idx = std::make_shared<RuleIndex>();
  for (std::size_t i = 0; i < ex.rules.size(); ++i) {
    const Rule &r = ex.rules[i];
    const auto ri = static_cast<int32_t>(i);
    const auto first = [ri](int32_t &slot) {
      if (slot < 0) {
        slot = ri;
      }
    };
    switch (r.kind) {
    case IM_RULE_APP_EXCLUDE: {
      RuleIndex::ExeRules &e = idx->by_exe[r.value];
      if (r.source == IM_RULE_SRC_SELF) {
        first(e.self);
      } else if (r.source == IM_RULE_SRC_USER) {
        first(e.user);
      } else if (r.source == IM_RULE_SRC_VIEWER) {
        first(e.viewer);
      } else { // BUILTIN; DATA is never sent for an exclusion and counts as built-in (locked: more stays out)
        first(e.builtin);
      }
      break;
    }
    case IM_RULE_APP_ALLOW:
      first(idx->by_exe[r.value].allow);
      break;
    case IM_RULE_INSTANCE:
      idx->instances.push_back(ri);
      break;
    case IM_RULE_STOP:
      first(idx->by_exe[r.value].stop);
      break;
    case IM_RULE_SHARED_RUNTIME:
      first(idx->by_exe[r.value].shared_runtime);
      break;
    case IM_RULE_BROWSER:
      first(idx->by_exe[r.value].browser);
      break;
    case IM_RULE_WEBVIEW_DATA_DIR:
      idx->webview_dirs.push_back(ri);
      break;
    default:
      break;
    }
  }
  return idx;
}

// path equals dir or lies below it; both already normalized (NormalizePath).
bool BelowOrEqual(const std::string &path, const std::string &dir) {
  if (dir.empty() || path.size() < dir.size() || path.compare(0, dir.size(), dir) != 0) {
    return false;
  }
  return path.size() == dir.size() || path[dir.size()] == '\\';
}

template <class T> bool Contains(const std::vector<T> &v, const T &x) {
  return std::find(v.begin(), v.end(), x) != v.end();
}

template <class T> void AddBounded(std::vector<T> &v, const T &x, std::size_t cap) {
  if (v.size() < cap && !Contains(v, x)) {
    v.push_back(x);
  }
}

// Calls f(index) for every ancestor of i along strict or loose links, nearest first, at most kMaxWalk of them; f
// returns false to stop.
template <class F> void ForEachAncestor(const ProcessTable &t, std::size_t i, bool loose, F &&f) {
  int32_t cur = t.Parent(i, loose);
  for (int step = 0; cur >= 0 && step < kMaxWalk; ++step) {
    if (!f(static_cast<std::size_t>(cur))) {
      return;
    }
    cur = t.Parent(static_cast<std::size_t>(cur), loose);
  }
}

// P's app parent (05 §5.2): its strict parent, unless the parent is a launcher or P is not a shared runtime and its
// folder is neither the parent's folder nor below it (or either folder is unknown).
int32_t AppParent(const ProcessTable &t, const RuleIndex &rx, std::size_t i) {
  const Process &p = t.at(i);
  const int32_t sp = p.strict_parent;
  if (sp < 0) {
    return -1;
  }
  const Process &parent = t.at(static_cast<std::size_t>(sp));
  if (rx.IsLauncher(parent.exe)) {
    return -1;
  }
  if (rx.IsSharedRuntime(p.exe)) {
    return sp;
  }
  if (p.folder.empty() || parent.folder.empty()) {
    return -1;
  }
  return BelowOrEqual(p.folder, parent.folder) ? sp : -1;
}

// Row 1 of 05 §5.3 for one process.
struct Row1 {
  int32_t rule = -1;     // the deciding rule
  int32_t proc = -1;     // the process that matched it (instance root, walk member or P), -1 from the lineage
  int32_t instance = -1; // IM_RULE_INSTANCE: the instance's process
  bool lineage = false;
};

// Classification of one table (05 §5.3): the app walks, row 1, the shared app's root, then rows 2-8 given mic roots.
class Classifier {
public:
  Classifier(const ProcessTable &table, const Exclusions &rules, const RuleIndex &index, const LineageMap &cache)
      : t_(table), ex_(rules), rx_(index), lineage_(cache) {
    const std::size_t n = t_.size();
    walks_.resize(n);
    for (std::size_t i = 0; i < n; ++i) {
      std::vector<int32_t> &w = walks_[i];
      w.push_back(static_cast<int32_t>(i));
      int32_t cur = AppParent(t_, rx_, i);
      while (cur >= 0 && w.size() <= static_cast<std::size_t>(kMaxWalk)) {
        w.push_back(cur);
        cur = AppParent(t_, rx_, static_cast<std::size_t>(cur));
      }
    }
    row1_.resize(n);
    for (std::size_t i = 0; i < n; ++i) {
      row1_[i] = ComputeRow1(i);
    }
    if (ex_.shared_pid != 0) {
      if (const auto s = t_.Find(ex_.shared_pid)) {
        shared_root_ = static_cast<std::size_t>(walks_[*s].back());
      }
    }
  }

  const std::vector<int32_t> &Walk(std::size_t i) const { return walks_[i]; }
  std::size_t AppRoot(std::size_t i) const { return static_cast<std::size_t>(walks_[i].back()); }
  bool IsRow1(std::size_t i) const { return row1_[i].rule >= 0; }

  const LineageEntry *Lineage(std::size_t i) const {
    const auto it = lineage_.find(t_.at(i).key);
    return it == lineage_.end() ? nullptr : &it->second;
  }

  std::vector<ProcessResult> Classify(const KeySet &mic_roots) const;

private:
  Row1 ComputeRow1(std::size_t i) const;

  const ProcessTable &t_;
  const Exclusions &ex_;
  const RuleIndex &rx_;
  const LineageMap &lineage_;
  std::vector<std::vector<int32_t>> walks_;
  std::vector<Row1> row1_;
  std::optional<std::size_t> shared_root_;
};

Row1 Classifier::ComputeRow1(std::size_t i) const {
  const Process &p = t_.at(i);
  const LineageEntry *le = Lineage(i);
  // An instance's OS tree by strict links (05 §5.3 row 1); an instance pid that doesn't exist is ignored (§5.3).
  // Strict only, although row 1's text still says "strict or loose" (a spec write-back is pending): a process linked
  // to the instance only loosely is older than it, so it can't be one of its descendants, and an INCLUDE stream on it
  // can't capture the instance's playback. The loose union guards the other direction, which stays as it is: an
  // instance in a target's loose tree blocks the target (I1, pid_reuse), and a heard session in R's loose tree blocks
  // the fast path (I4). With loose links here, a game whose exited bootstrapper's pid isshoni.exe got was kept out
  // as `isshoni`, locked, with no way to hear it.
  for (const int32_t ri : rx_.instances) {
    const auto k = t_.Find(ex_.rules[static_cast<std::size_t>(ri)].pid);
    if (!k) {
      continue;
    }
    const auto ki = static_cast<int32_t>(*k);
    if (t_.InTree(i, *k, false)) {
      return Row1{ri, ki, ki, false};
    }
    if (le != nullptr && Contains(le->ancestors, t_.at(*k).key)) {
      return Row1{ri, ki, ki, true};
    }
  }
  // The walk hits isshoni.exe (source SELF).
  for (const int32_t w : walks_[i]) {
    if (const auto *e = rx_.Find(t_.at(static_cast<std::size_t>(w)).exe); e != nullptr && e->self >= 0) {
      return Row1{e->self, w, -1, false};
    }
  }
  if (le != nullptr) {
    for (const std::string &exe : le->exes) {
      if (const auto *e = rx_.Find(exe); e != nullptr && e->self >= 0) {
        return Row1{e->self, -1, -1, true};
      }
    }
  }
  // isshoni's WebView2 data folder or a folder below it.
  if (!p.user_data_dir.empty()) {
    for (const int32_t ri : rx_.webview_dirs) {
      if (BelowOrEqual(p.user_data_dir, ex_.rules[static_cast<std::size_t>(ri)].value)) {
        return Row1{ri, static_cast<int32_t>(i), -1, false};
      }
    }
  }
  return Row1{};
}

std::vector<ProcessResult> Classifier::Classify(const KeySet &mic_roots) const {
  struct Hit {
    int32_t rule = -1;
    int32_t proc = -1;
    bool lineage = false;
  };
  const auto take = [](Hit &h, int32_t rule, int32_t proc, bool from_lineage) {
    if (h.rule < 0 && rule >= 0) {
      h = Hit{rule, proc, from_lineage};
    }
  };
  const bool mic_flag = (ex_.flags & IM_EXCLF_KEEP_OUT_MIC_USERS) != 0;
  std::optional<ProcessKey> shared_key;
  if (shared_root_) {
    shared_key = t_.at(*shared_root_).key;
  }

  std::vector<ProcessResult> out(t_.size());
  for (std::size_t i = 0; i < t_.size(); ++i) {
    const Process &p = t_.at(i);
    const LineageEntry *le = Lineage(i);
    ProcessResult &r = out[i];
    const std::size_t root_index = AppRoot(i);
    const Process &root = t_.at(root_index);
    r.app_root = static_cast<int32_t>(root_index);
    r.launcher = rx_.IsLauncher(p.exe);
    r.browser = rx_.IsBrowser(root.exe);
    r.shared = shared_root_ && *shared_root_ == root_index;

    // Rows 2-5: the first hit of each kind along the walk (P first), then in the lineage entry.
    Hit builtin;
    Hit user;
    Hit viewer;
    Hit allow;
    for (const int32_t w : walks_[i]) {
      if (const auto *e = rx_.Find(t_.at(static_cast<std::size_t>(w)).exe)) {
        take(builtin, e->builtin, w, false);
        take(user, e->user, w, false);
        take(viewer, e->viewer, w, false);
        take(allow, e->allow, w, false);
      }
    }
    if (le != nullptr) {
      for (const std::string &exe : le->exes) {
        if (const auto *e = rx_.Find(exe)) {
          take(builtin, e->builtin, -1, true);
          take(user, e->user, -1, true);
          take(viewer, e->viewer, -1, true);
          take(allow, e->allow, -1, true);
        }
      }
    }

    // Row 6: a mic root among its app roots (its own, and those its lineage remembers), not the shared app's root.
    bool mic_hit = false;
    bool mic_from_lineage_only = true;
    const auto consider_root = [&](const ProcessKey &k, bool from_lineage) {
      if (mic_roots.count(k) == 0) {
        return;
      }
      r.uses_mic = true;
      if (shared_key && k == *shared_key) {
        return;
      }
      mic_hit = true;
      if (!from_lineage) {
        mic_from_lineage_only = false;
      }
    };
    consider_root(root.key, false);
    if (le != nullptr) {
      for (const ProcessKey &k : le->roots) {
        consider_root(k, true);
      }
    }

    const Row1 &r1 = row1_[i];
    int32_t matched = -1;
    const auto keep_out = [&](Reason why, const Hit &h) {
      r.verdict = Verdict::kKeptOut;
      r.reason = why;
      r.rule = h.rule;
      r.lineage = h.lineage;
      matched = h.proc;
    };
    if (r1.rule >= 0) {
      keep_out(Reason::kIsshoni, Hit{r1.rule, r1.proc, r1.lineage});
    } else if (builtin.rule >= 0) {
      keep_out(Reason::kBuiltin, builtin);
    } else if (user.rule >= 0) {
      keep_out(Reason::kUser, user);
    } else if (viewer.rule >= 0) {
      keep_out(Reason::kViewer, viewer);
    } else if (allow.rule >= 0) {
      r.verdict = Verdict::kHeard;
      r.allowed = true;
      r.rule = allow.rule;
      r.lineage = allow.lineage;
      matched = allow.proc;
    } else if (mic_flag && mic_hit) {
      r.verdict = Verdict::kKeptOut;
      r.reason = Reason::kMic;
      r.lineage = mic_from_lineage_only;
    } else {
      r.verdict = Verdict::kHeard;
    }

    // App id, exe and display name (05 §5.1).
    if (r.rule >= 0) {
      const Rule &rule = ex_.rules[static_cast<std::size_t>(r.rule)];
      r.app_id = rule.app_id;
      r.app_name = rule.name;
      if (rule.kind == IM_RULE_INSTANCE) {
        r.app_exe = r1.instance >= 0 ? t_.at(static_cast<std::size_t>(r1.instance)).exe : p.exe;
      } else if (rule.kind == IM_RULE_WEBVIEW_DATA_DIR) {
        r.app_exe = p.exe;
      } else {
        r.app_exe = rule.value;
      }
    } else {
      r.app_id = "exe:" + root.exe;
      r.app_exe = root.exe;
    }
    if (r.app_name.empty()) {
      // The FileDescription of the process the rule matched (for exe rules that is the app's own process), else of
      // the app root, else the exe without ".exe".
      const Process &named = matched >= 0 ? t_.at(static_cast<std::size_t>(matched)) : root;
      r.app_name = !named.description.empty() ? named.description : std::string(ExeStem(r.app_exe));
    }

    // Warnings (05 §5.8, §12.1).
    if (r.shared && r.uses_mic && r.verdict == Verdict::kHeard) {
      r.warnings |= kWarnSharedAppUsesMic;
    }
    if (r.shared && r.verdict == Verdict::kKeptOut &&
        (r.reason == Reason::kBuiltin || r.reason == Reason::kUser || r.reason == Reason::kViewer)) {
      r.warnings |= kWarnSharedAppKeptOut;
    }
    if (r.allowed && r.uses_mic) {
      r.warnings |= kWarnIncludedUsesMic;
    }
  }
  return out;
}

// Mic users (05 §5.6): processes with a capture session, except isshoni's own tree; their app roots become mic roots
// and stay so until they exit.
void UpdateMicRoots(const ProcessTable &t, const Classifier &cls, std::span<const Session> sessions,
                    const std::vector<int32_t> &sproc, KeySet &mic_roots) {
  for (auto it = mic_roots.begin(); it != mic_roots.end();) {
    if (!t.Find(*it)) {
      it = mic_roots.erase(it);
    } else {
      ++it;
    }
  }
  for (std::size_t k = 0; k < sessions.size(); ++k) {
    if (sessions[k].flow != Flow::kCapture || sproc[k] < 0) {
      continue;
    }
    const auto i = static_cast<std::size_t>(sproc[k]);
    if (cls.IsRow1(i)) {
      continue;
    }
    mic_roots.insert(t.at(cls.AppRoot(i)).key);
    if (const LineageEntry *le = cls.Lineage(i)) {
      for (const ProcessKey &r : le->roots) {
        if (t.Find(r)) {
          mic_roots.insert(r);
        }
      }
    }
  }
}

// The lineage cache (05 §5.7): every kept-out process remembers its walk, app roots and strict parent chain for its
// lifetime (the chain is what row 1's instance trees follow, by strict links).
void RecordLineage(const ProcessTable &t, const Classifier &cls, const std::vector<ProcessResult> &pr,
                   LineageMap &lineage) {
  for (std::size_t i = 0; i < t.size(); ++i) {
    if (pr[i].verdict != Verdict::kKeptOut) {
      continue;
    }
    LineageEntry &e = lineage[t.at(i).key];
    for (const int32_t w : cls.Walk(i)) {
      AddBounded(e.exes, t.at(static_cast<std::size_t>(w)).exe, kMaxLineageExes);
    }
    AddBounded(e.roots, t.at(cls.AppRoot(i)).key, kMaxLineageRoots);
    ForEachAncestor(t, i, false, [&](std::size_t a) {
      AddBounded(e.ancestors, t.at(a).key, kMaxLineageAncestors);
      return true;
    });
  }
  for (auto it = lineage.begin(); it != lineage.end();) {
    if (!t.Find(it->first)) {
      it = lineage.erase(it);
    } else {
      ++it;
    }
  }
}

// The kept-out processes x whose OS trees make their ancestors unsafe include targets (05 §6.2 step 2): for every
// process, the first x found below it along strict links, and along loose links.
struct DirtyMarks {
  std::vector<int32_t> strict;
  std::vector<int32_t> loose;
};

DirtyMarks MarkDirty(const ProcessTable &t, const std::vector<ProcessResult> &pr, bool loose_links) {
  DirtyMarks d{std::vector<int32_t>(t.size(), -1), std::vector<int32_t>(t.size(), -1)};
  for (std::size_t x = 0; x < t.size(); ++x) {
    if (pr[x].verdict != Verdict::kKeptOut) {
      continue;
    }
    const auto xi = static_cast<int32_t>(x);
    ForEachAncestor(t, x, false, [&](std::size_t a) {
      if (d.strict[a] < 0) {
        d.strict[a] = xi;
      }
      return true;
    });
    if (loose_links) {
      ForEachAncestor(t, x, true, [&](std::size_t a) {
        if (d.loose[a] < 0) {
          d.loose[a] = xi;
        }
        return true;
      });
    }
  }
  return d;
}

// The fast path's R (05 §6.4): the deepest process whose strict tree holds every kept-out process, if it is not a
// launcher and no heard render-session process lies in its loose tree. sessions may be empty (the commit check).
std::optional<std::size_t> FastPathRoot(const ProcessTable &t, const std::vector<ProcessResult> &pr,
                                        std::span<const Session> sessions, const std::vector<int32_t> &sproc) {
  std::vector<std::size_t> kept;
  for (std::size_t i = 0; i < t.size(); ++i) {
    if (pr[i].verdict == Verdict::kKeptOut) {
      kept.push_back(i);
    }
  }
  if (kept.empty()) {
    return std::nullopt;
  }
  std::vector<uint32_t> hits(t.size(), 0);
  std::vector<uint32_t> stamp(t.size(), 0);
  uint32_t gen = 0;
  for (const std::size_t x : kept) {
    ++gen;
    int32_t cur = static_cast<int32_t>(x);
    for (int step = 0; cur >= 0 && step <= kMaxWalk; ++step) {
      const auto c = static_cast<std::size_t>(cur);
      if (stamp[c] != gen) {
        stamp[c] = gen;
        ++hits[c];
      }
      cur = t.Parent(c, false);
    }
  }
  std::optional<std::size_t> r;
  int32_t cur = static_cast<int32_t>(kept.front());
  for (int step = 0; cur >= 0 && step <= kMaxWalk; ++step) {
    const auto c = static_cast<std::size_t>(cur);
    if (hits[c] == kept.size()) {
      r = c;
      break;
    }
    cur = t.Parent(c, false);
  }
  if (!r || pr[*r].launcher) {
    return std::nullopt;
  }
  for (std::size_t k = 0; k < sessions.size(); ++k) {
    if (sessions[k].flow != Flow::kRender || sproc[k] < 0) {
      continue;
    }
    const auto s = static_cast<std::size_t>(sproc[k]);
    if (pr[s].verdict == Verdict::kHeard && t.InTree(s, *r, true)) {
      return std::nullopt;
    }
  }
  return r;
}

// Matches a live stream to the table (I3, 05 §5.7): the planned key; when its creation time was unknown, a process at
// that pid whose time is still unknown, with the same exe and parent pid. A time that became known means another
// process got the pid (the watcher caches details per process, so one process doesn't change from unknown to known).
std::optional<std::size_t> MatchStream(const ProcessTable &t, const LiveStream &s, RemovalReason &why) {
  const auto i = t.Find(s.target.pid);
  if (!i) {
    why = RemovalReason::kExited;
    return std::nullopt;
  }
  const Process &p = t.at(*i);
  const bool same = s.target.created != 0 ? p.key.created == s.target.created
                                          : (p.key.created == 0 && p.exe == s.exe && p.parent_pid == s.parent_pid);
  if (!same) {
    why = RemovalReason::kKeyMismatch;
    return std::nullopt;
  }
  return i;
}

Activation MakeActivation(const ProcessTable &t, StreamKind kind, std::size_t i) {
  const Process &p = t.at(i);
  return Activation{kind, p.key, p.parent_pid, p.exe};
}

int Depth(const ProcessTable &t, std::size_t i) {
  int d = 0;
  ForEachAncestor(t, i, false, [&](std::size_t) {
    ++d;
    return true;
  });
  return d;
}

} // namespace

std::string_view VerdictName(Verdict v) noexcept {
  switch (v) {
  case Verdict::kHeard:
    return "heard";
  case Verdict::kKeptOut:
    return "kept_out";
  case Verdict::kNotCaptured:
    return "not_captured";
  case Verdict::kPending:
    return "pending";
  }
  return "";
}

std::string_view ReasonName(Reason r) noexcept {
  switch (r) {
  case Reason::kNone:
    return "";
  case Reason::kBuiltin:
    return "builtin";
  case Reason::kUser:
    return "user";
  case Reason::kMic:
    return "mic";
  case Reason::kViewer:
    return "viewer";
  case Reason::kIsshoni:
    return "isshoni";
  case Reason::kLauncher:
    return "launcher";
  case Reason::kTree:
    return "tree";
  case Reason::kPidReuse:
    return "pid_reuse";
  case Reason::kLimit:
    return "limit";
  case Reason::kCaptureFailed:
    return "capture_failed";
  }
  return "";
}

std::string_view EngineName(Engine e) noexcept {
  switch (e) {
  case Engine::kNone:
    return "none";
  case Engine::kEndpoint:
    return "endpoint";
  case Engine::kExcludeSelf:
    return "exclude-self";
  case Engine::kIncludeSet:
    return "include-set";
  }
  return "";
}

std::string_view NoteName(Note n) noexcept {
  switch (n) {
  case Note::kNone:
    return "";
  case Note::kCovered:
    return "covered";
  case Note::kNoHandle:
    return "no_handle";
  }
  return "";
}

std::string_view FlowName(Flow f) noexcept { return f == Flow::kCapture ? "capture" : "render"; }

std::string_view RemovalReasonName(RemovalReason r) noexcept {
  switch (r) {
  case RemovalReason::kExited:
    return "exited";
  case RemovalReason::kKeyMismatch:
    return "key_mismatch";
  case RemovalReason::kKeptOut:
    return "kept_out";
  case RemovalReason::kLauncher:
    return "launcher";
  case RemovalReason::kUnsafe:
    return "unsafe";
  case RemovalReason::kSessionGone:
    return "session_gone";
  case RemovalReason::kCovered:
    return "covered";
  case RemovalReason::kLimit:
    return "limit";
  case RemovalReason::kEngine:
    return "engine";
  case RemovalReason::kTarget:
    return "target";
  }
  return "";
}

std::string_view WarningName(uint32_t bit) noexcept {
  switch (bit) {
  case kWarnSharedAppUsesMic:
    return "shared_app_uses_mic";
  case kWarnSharedAppKeptOut:
    return "shared_app_kept_out";
  case kWarnIncludedUsesMic:
    return "included_uses_mic";
  case kWarnMultiProcessSession:
    return "multi_process_session";
  default:
    return "";
  }
}

Planner::Planner(PlannerOptions opts) : opts_(opts), rules_(BuildIndex(ex_)) {}

void Planner::SetExclusions(Exclusions ex) {
  ex_ = std::move(ex);
  rules_ = BuildIndex(ex_);
}

void Planner::Restart() {
  engine_ = Engine::kNone;
  exclude_target_ = ProcessKey{};
  fast_since_.reset();
  fast_r_ = ProcessKey{};
  missing_since_.clear();
}

void Planner::Reset() {
  Restart();
  mic_roots_.clear();
  lineage_.clear();
  pid_reuse_blocked_.clear();
  render_seen_.clear();
}

Plan Planner::Reconcile(const ProcessTable &t, const PlanInput &in) {
  Plan plan;
  plan.previous_engine = engine_;
  const RuleIndex &rx = *rules_;
  const std::size_t n = t.size();
  const std::size_t ns = in.sessions.size();

  // Sessions → table indices; a pid that is not in the snapshot is pending.
  std::vector<int32_t> sproc(ns, -1);
  for (std::size_t k = 0; k < ns; ++k) {
    if (const auto i = t.Find(in.sessions[k].pid)) {
      sproc[k] = static_cast<int32_t>(*i);
    }
  }

  // Classification (05 §5.3) with this reconcile's mic roots.
  Classifier cls(t, ex_, rx, lineage_);
  UpdateMicRoots(t, cls, in.sessions, sproc, mic_roots_);
  plan.processes = cls.Classify(mic_roots_);
  std::vector<ProcessResult> &pr = plan.processes;

  struct Agg {
    bool render = false;
    bool active = false;
    int64_t last = 0;
  };
  std::vector<Agg> agg(n);
  for (std::size_t k = 0; k < ns; ++k) {
    if (sproc[k] < 0) {
      continue;
    }
    const auto i = static_cast<std::size_t>(sproc[k]);
    const Session &s = in.sessions[k];
    if (s.multi_process) {
      pr[i].warnings |= kWarnMultiProcessSession;
    }
    if (s.flow != Flow::kRender) {
      continue;
    }
    agg[i].render = true;
    agg[i].active = agg[i].active || s.state == SessionState::kActive;
    agg[i].last = std::max(agg[i].last, s.last_active_ns);
  }
  KeySet failed(in.capture_failed.begin(), in.capture_failed.end());
  const DirtyMarks dirty = MarkDirty(t, pr, opts_.loose_links);

  // Engine selection (05 §6.1) and the fast path's hysteresis (05 §6.4).
  // IM_AUDIO_MODE_EVERYTHING is reserved for later ("All sound": one EXCLUDE stream on isshoni's tree, 05 §6.1); M2's
  // exports refuse it (README §3 item 15), and the planner plans no capture for it, like any mode without capture.
  const bool capture_mode = in.mode == IM_AUDIO_MODE_EXCLUDE_APPS;
  std::optional<std::size_t> r_index;
  Engine engine = Engine::kNone;
  if (!capture_mode) {
    engine = Engine::kNone;
    fast_since_.reset();
  } else if (!in.process_loopback) {
    engine = Engine::kEndpoint;
    fast_since_.reset();
  } else {
    std::optional<std::size_t> r;
    if ((ex_.flags & IM_EXCLF_FAST_PATH) != 0) {
      r = FastPathRoot(t, pr, in.sessions, sproc);
    }
    if (r) {
      const ProcessKey rk = t.at(*r).key;
      if (!fast_since_ || fast_r_ != rk) {
        fast_since_ = in.now_ns;
        fast_r_ = rk;
      }
      const bool staying = engine_ == Engine::kExcludeSelf && exclude_target_ == rk;
      const bool starting = engine_ == Engine::kNone || engine_ == Engine::kEndpoint;
      const bool waited = in.now_ns - *fast_since_ >= opts_.fast_path_enter_ns;
      bool new_descendant = false;
      if (staying && opts_.fast_path_exit_on_new_descendant) {
        for (std::size_t k = 0; k < ns && !new_descendant; ++k) {
          if (in.sessions[k].flow == Flow::kRender && sproc[k] >= 0) {
            const auto s = static_cast<std::size_t>(sproc[k]);
            new_descendant = render_seen_.count(t.at(s).key) == 0 && t.InTree(s, *r, true);
          }
        }
      }
      if (new_descendant) {
        engine = Engine::kIncludeSet;
        fast_since_ = in.now_ns; // re-enter only after the usual 10 s
      } else if (staying || starting || waited) {
        engine = Engine::kExcludeSelf;
        r_index = r;
      } else {
        engine = Engine::kIncludeSet;
      }
    } else {
      fast_since_.reset();
      engine = Engine::kIncludeSet;
    }
  }
  plan.engine = engine;
  if (engine == Engine::kExcludeSelf) {
    plan.exclude_target = t.at(*r_index).key;
  }

  // The exclusive stream (EXCLUDE on R, or the endpoint loopback): keep the right one, remove the others.
  {
    bool kept = false;
    for (std::size_t j = 0; j < in.streams.size(); ++j) {
      const LiveStream &s = in.streams[j];
      if (s.kind == StreamKind::kInclude) {
        continue;
      }
      const bool wanted =
          (engine == Engine::kExcludeSelf && s.kind == StreamKind::kExclude && s.target == plan.exclude_target) ||
          (engine == Engine::kEndpoint && s.kind == StreamKind::kEndpoint);
      if (wanted && !kept) {
        kept = true;
        continue;
      }
      const bool other_target = engine == Engine::kExcludeSelf && s.kind == StreamKind::kExclude;
      plan.remove.push_back(Removal{j, other_target ? RemovalReason::kTarget : RemovalReason::kEngine});
    }
    if (!kept && engine == Engine::kExcludeSelf) {
      plan.activate.push_back(MakeActivation(t, StreamKind::kExclude, *r_index));
    } else if (!kept && engine == Engine::kEndpoint) {
      plan.activate.push_back(Activation{StreamKind::kEndpoint, ProcessKey{}, 0, {}});
    }
  }

  // Live include streams: re-checked on every reconcile (05 §6.2 step 6). The re-check comes first, so a removal that
  // waits (kEngine for the crossfade, kCovered for the covering stream) only goes to a stream that is still safe.
  const bool include_set = engine == Engine::kIncludeSet;
  std::vector<int32_t> live_of(n, -1); // table index → the live include stream kept on it
  int64_t duplicates = 0;              // second streams on one target: covered, they wait for the first one
  std::unordered_map<ProcessKey, int64_t, ProcessKeyHash> missing;
  for (std::size_t j = 0; j < in.streams.size(); ++j) {
    const LiveStream &s = in.streams[j];
    if (s.kind != StreamKind::kInclude) {
      continue;
    }
    RemovalReason why = RemovalReason::kExited;
    const auto m = MatchStream(t, s, why);
    if (!m) {
      plan.remove.push_back(Removal{j, why});
      continue;
    }
    const std::size_t i = *m;
    if (pr[i].verdict == Verdict::kKeptOut) {
      why = RemovalReason::kKeptOut;
    } else if (pr[i].launcher) {
      why = RemovalReason::kLauncher;
    } else if (dirty.strict[i] >= 0 || dirty.loose[i] >= 0) {
      why = RemovalReason::kUnsafe;
    } else if (!include_set) {
      why = RemovalReason::kEngine;
    } else if (live_of[i] >= 0) {
      why = RemovalReason::kCovered; // a second stream on the same target
      ++duplicates;
    } else if (!agg[i].render) {
      const auto it = missing_since_.find(t.at(i).key);
      const int64_t since = it == missing_since_.end() ? in.now_ns : it->second;
      if (in.now_ns - since >= opts_.session_grace_ns) {
        why = RemovalReason::kSessionGone;
      } else {
        missing.emplace(t.at(i).key, since);
        live_of[i] = static_cast<int32_t>(j);
        continue;
      }
    } else {
      live_of[i] = static_cast<int32_t>(j);
      continue;
    }
    plan.remove.push_back(Removal{j, why});
  }
  missing_since_ = std::move(missing);

  // Session verdicts that don't depend on the include set's selection.
  plan.sessions.resize(ns);
  for (std::size_t k = 0; k < ns; ++k) {
    SessionResult &sr = plan.sessions[k];
    sr.process = sproc[k];
    if (sproc[k] < 0) {
      sr.verdict = Verdict::kPending;
      continue;
    }
    const ProcessResult &p = pr[static_cast<std::size_t>(sproc[k])];
    sr.app_id = p.app_id;
    sr.verdict = p.verdict;
    sr.reason = p.reason;
  }

  // The include set (05 §6.2), also as the prediction while no capture runs (mode off, a watcher without a share).
  std::vector<std::size_t> targets; // in priority order
  if (engine == Engine::kIncludeSet || engine == Engine::kNone) {
    // Step 2: candidates. Live include targets are candidates while they pass the re-check.
    std::vector<std::size_t> cands;
    std::vector<int8_t> blocked(n, 0); // why a heard render-session process is no candidate: a Reason value
    for (std::size_t i = 0; i < n; ++i) {
      if (live_of[i] >= 0) {
        cands.push_back(i);
        continue;
      }
      if (!agg[i].render || pr[i].verdict != Verdict::kHeard) {
        continue;
      }
      Reason why = Reason::kNone;
      if (pr[i].launcher) {
        why = Reason::kLauncher;
      } else if (dirty.strict[i] >= 0) {
        why = Reason::kTree;
      } else if (dirty.loose[i] >= 0) {
        why = Reason::kPidReuse;
      } else if (failed.count(t.at(i).key) != 0) {
        why = Reason::kCaptureFailed;
      }
      if (why != Reason::kNone) {
        blocked[i] = static_cast<int8_t>(why);
      } else {
        cands.push_back(i);
      }
    }

    // Step 3: coverage, ancestors first. A candidate is covered by a root among its strict ancestors whose stream
    // includes it: a new root always (its stream starts after the candidate exists), a live one unless the
    // late-child fallback is on and the candidate was created after that stream's activation (an activation still
    // running, activated_filetime 0, completes after every process of this snapshot started).
    std::vector<int> depth(n, 0);
    for (const std::size_t c : cands) {
      depth[c] = Depth(t, c);
    }
    std::sort(cands.begin(), cands.end(),
              [&](std::size_t a, std::size_t b) { return depth[a] != depth[b] ? depth[a] < depth[b] : a < b; });
    const auto covers = [&](std::size_t a, std::size_t c) {
      if (live_of[a] < 0 || !opts_.late_child_own_stream) {
        return true;
      }
      const uint64_t activated = in.streams[static_cast<std::size_t>(live_of[a])].activated_filetime;
      const uint64_t created = t.at(c).key.created;
      return activated == 0 || created == 0 || created < activated;
    };
    std::vector<int32_t> coverer(n, -1);
    std::vector<char> is_root(n, 0);
    std::vector<char> is_cand(n, 0);
    for (const std::size_t c : cands) {
      is_cand[c] = 1;
    }
    std::vector<std::size_t> roots;
    for (const std::size_t c : cands) {
      ForEachAncestor(t, c, false, [&](std::size_t a) {
        if (is_root[a] && covers(a, c)) {
          coverer[c] = static_cast<int32_t>(a);
          return false;
        }
        return true;
      });
      if (coverer[c] < 0) {
        is_root[c] = 1;
        roots.push_back(c);
      }
    }

    // Step 4: order the roots (live ones keep their slots; then active, most recent activity, oldest), with the
    // activity of what they cover, and take at most max_include_streams.
    struct Prio {
      bool active = false;
      int64_t last = 0;
    };
    std::vector<Prio> prio(n);
    for (const std::size_t c : cands) {
      std::size_t top = c;
      while (coverer[top] >= 0) {
        top = static_cast<std::size_t>(coverer[top]);
      }
      prio[top].active = prio[top].active || agg[c].active;
      prio[top].last = std::max(prio[top].last, agg[c].last);
    }
    std::sort(roots.begin(), roots.end(), [&](std::size_t a, std::size_t b) {
      const bool la = live_of[a] >= 0;
      const bool lb = live_of[b] >= 0;
      if (la != lb) {
        return la;
      }
      if (prio[a].active != prio[b].active) {
        return prio[a].active;
      }
      if (prio[a].last != prio[b].last) {
        return prio[a].last > prio[b].last;
      }
      const ProcessKey &ka = t.at(a).key;
      const ProcessKey &kb = t.at(b).key;
      return ka.created != kb.created ? ka.created < kb.created : ka.pid < kb.pid;
    });

    // Live streams below a root. A redundant one has a live ancestor whose stream covers it: it goes once that
    // stream runs, whatever happens to the root. A pinned one is the topmost live stream below a new root (an
    // ancestor started playing after it): it holds its slot until that root is selected, whose stream then covers it.
    std::vector<int32_t> live_coverer(n, -1); // the nearest live ancestor whose stream covers a live candidate
    for (const std::size_t c : cands) {
      if (live_of[c] < 0) {
        continue;
      }
      ForEachAncestor(t, c, false, [&](std::size_t a) {
        if (live_of[a] >= 0 && covers(a, c)) {
          live_coverer[c] = static_cast<int32_t>(a);
          return false;
        }
        return true;
      });
    }
    const auto pinned_under_new_root = [&](std::size_t c) {
      return live_of[c] >= 0 && coverer[c] >= 0 && live_of[static_cast<std::size_t>(coverer[c])] < 0 &&
             live_coverer[c] < 0;
    };

    // Slots (§17). `used` counts the include streams once the plan is applied and its handovers are over (at the
    // end, the targets); `held` the ones the engine holds while they run. A covered stream fades only once its
    // coverer's stream runs (RemovalReason::kCovered), so every live stream below a root and every second stream on
    // a target stays in `held` (the few a cut root's limit removes at once are counted anyway, erring toward fewer
    // new streams). A live root keeps its slot. A new root needs a slot in both counts; one that replaces pinned
    // streams frees their slots in `used` and may take `held` one over the cap, so a parent that starts playing above
    // its own playing children gets in when the set is full: the engine then holds at most cap + 1 include streams,
    // until the handover ends.
    std::vector<int64_t> pinned(n, 0); // per new root: the live streams it would replace
    int64_t used = 0;
    int64_t held = duplicates;
    for (const std::size_t c : cands) {
      if (live_of[c] >= 0 && coverer[c] >= 0) {
        ++held;
      }
      if (pinned_under_new_root(c)) {
        ++pinned[static_cast<std::size_t>(coverer[c])];
        ++used;
      }
    }
    const auto cap = static_cast<int64_t>(opts_.max_include_streams);
    std::vector<char> selected(n, 0);
    for (const std::size_t r : roots) {
      const bool live = live_of[r] >= 0;
      bool take = false;
      int64_t delta = 1;
      if (live) {
        take = used + 1 <= cap;
      } else {
        delta = 1 - pinned[r]; // a new root adds its stream and frees the ones it covers
        take = used + std::max<int64_t>(delta, 0) <= cap && held + 1 <= cap + (pinned[r] > 0 ? 1 : 0);
      }
      if (take) {
        selected[r] = 1;
        used += delta;
        ++held;
        targets.push_back(r);
      } else if (live) {
        plan.remove.push_back(Removal{static_cast<std::size_t>(live_of[r]), RemovalReason::kLimit});
      }
    }
    // Ancestors first, so a live coverer's fate is known before the streams below it are decided.
    std::vector<char> in_targets(n, 0);
    for (const std::size_t r : targets) {
      in_targets[r] = 1;
    }
    for (const std::size_t c : cands) {
      if (live_of[c] < 0 || coverer[c] < 0) {
        continue;
      }
      const auto a = static_cast<std::size_t>(coverer[c]);
      const auto stream = static_cast<std::size_t>(live_of[c]);
      const int32_t lc = live_coverer[c];
      if (selected[a] || (lc >= 0 && in_targets[static_cast<std::size_t>(lc)])) {
        plan.remove.push_back(Removal{stream, RemovalReason::kCovered});
      } else if (pinned_under_new_root(c) || used < cap) {
        if (!pinned_under_new_root(c)) {
          ++used; // its live coverer was cut by the limit (more live streams than the cap)
        }
        targets.push_back(c);
        in_targets[c] = 1;
      } else {
        plan.remove.push_back(Removal{stream, RemovalReason::kLimit});
      }
    }

    // Session verdicts: a target, covered by a target, blocked, or cut by the limit.
    const auto covered_by_target = [&](std::size_t c) {
      bool found = false;
      ForEachAncestor(t, c, false, [&](std::size_t a) {
        if (in_targets[a] && covers(a, c)) {
          found = true;
          return false;
        }
        return true;
      });
      return found;
    };
    for (std::size_t k = 0; k < ns; ++k) {
      SessionResult &sr = plan.sessions[k];
      if (sproc[k] < 0 || in.sessions[k].flow != Flow::kRender || sr.verdict != Verdict::kHeard) {
        continue;
      }
      const auto i = static_cast<std::size_t>(sproc[k]);
      if (blocked[i] != 0) {
        sr.verdict = Verdict::kNotCaptured;
        sr.reason = static_cast<Reason>(blocked[i]);
        if (sr.reason == Reason::kTree || sr.reason == Reason::kPidReuse) {
          const int32_t x = sr.reason == Reason::kTree ? dirty.strict[i] : dirty.loose[i];
          sr.detail = pr[static_cast<std::size_t>(x)].app_name;
        }
        continue;
      }
      if (!is_cand[i]) {
        continue;
      }
      if (in_targets[i]) {
        sr.target = engine == Engine::kIncludeSet;
        if (live_of[i] >= 0 && !in.streams[static_cast<std::size_t>(live_of[i])].has_handle) {
          sr.note = Note::kNoHandle;
        }
      } else if (covered_by_target(i)) {
        sr.note = Note::kCovered;
      } else {
        sr.verdict = Verdict::kNotCaptured;
        sr.reason = Reason::kLimit;
      }
    }
  }
  if (engine == Engine::kIncludeSet) {
    for (const std::size_t r : targets) {
      plan.include_targets.push_back(t.at(r).key);
      if (live_of[r] < 0) {
        plan.activate.push_back(MakeActivation(t, StreamKind::kInclude, r));
      }
    }
  }
  if (engine == Engine::kExcludeSelf) {
    for (std::size_t k = 0; k < ns; ++k) {
      if (sproc[k] >= 0 && in.sessions[k].flow == Flow::kRender && static_cast<std::size_t>(sproc[k]) == *r_index) {
        plan.sessions[k].target = true;
      }
    }
  }

  // The pid_reuse counter counts each block once (05 §5.2 pidReuseBlocks).
  {
    KeySet now_blocked;
    for (std::size_t k = 0; k < ns; ++k) {
      if (plan.sessions[k].reason == Reason::kPidReuse && plan.sessions[k].verdict == Verdict::kNotCaptured) {
        const ProcessKey key = t.at(static_cast<std::size_t>(sproc[k])).key;
        if (now_blocked.insert(key).second && pid_reuse_blocked_.count(key) == 0) {
          ++plan.pid_reuse_blocks;
        }
      }
    }
    pid_reuse_blocked_ = std::move(now_blocked);
  }

  // Report rows (05 §12.1): apps with a session, running apps matched by a built-in rule, the shared app.
  {
    // A row is kept out when any of its processes is (an app's processes share their verdict, and a kept-out one is
    // what the row must say); else heard when any session is heard; else pending; else the first session's
    // not-captured reason; a row without a session (a silent built-in, the shared app) is heard.
    std::unordered_map<std::string, std::size_t> row_of;
    struct RowState {
      std::optional<Reason> kept;
      bool heard = false;
      bool pending = false;
      std::optional<Reason> not_captured;
      std::string detail;
    };
    std::vector<RowState> state;
    const auto row_for = [&](const ProcessResult &p) -> std::size_t {
      const auto [it, inserted] = row_of.emplace(p.app_id, plan.apps.size());
      if (inserted) {
        AppRow row;
        row.id = p.app_id;
        row.name = p.app_name;
        row.exe = p.app_exe;
        plan.apps.push_back(std::move(row));
        state.emplace_back();
      }
      return it->second;
    };
    for (std::size_t k = 0; k < ns; ++k) {
      if (sproc[k] < 0) {
        continue;
      }
      const SessionResult &sr = plan.sessions[k];
      const std::size_t ri = row_for(pr[static_cast<std::size_t>(sproc[k])]);
      AppRow &row = plan.apps[ri];
      RowState &st = state[ri];
      ++row.sessions;
      row.playing = row.playing || in.sessions[k].playing;
      if (sr.verdict == Verdict::kKeptOut) {
        if (!st.kept) {
          st.kept = sr.reason;
        }
      } else if (sr.verdict == Verdict::kHeard) {
        st.heard = true;
      } else if (sr.verdict == Verdict::kPending) {
        st.pending = true;
      } else if (!st.not_captured) {
        st.not_captured = sr.reason;
        st.detail = sr.detail;
      }
    }
    for (std::size_t i = 0; i < n; ++i) {
      if ((pr[i].verdict == Verdict::kKeptOut && pr[i].reason == Reason::kBuiltin) || pr[i].shared) {
        row_for(pr[i]);
      }
    }
    for (std::size_t i = 0; i < n; ++i) {
      const auto it = row_of.find(pr[i].app_id);
      if (it == row_of.end()) {
        continue;
      }
      AppRow &row = plan.apps[it->second];
      RowState &st = state[it->second];
      ++row.pids;
      row.uses_mic = row.uses_mic || pr[i].uses_mic;
      row.browser = row.browser || pr[i].browser;
      row.shared = row.shared || pr[i].shared;
      row.warnings |= pr[i].warnings;
      if (pr[i].verdict == Verdict::kKeptOut && !st.kept) {
        st.kept = pr[i].reason;
      }
    }
    for (std::size_t ri = 0; ri < plan.apps.size(); ++ri) {
      AppRow &row = plan.apps[ri];
      const RowState &st = state[ri];
      if (st.kept) {
        row.verdict = Verdict::kKeptOut;
        row.reason = *st.kept;
      } else if (st.heard) {
        row.verdict = Verdict::kHeard;
      } else if (st.pending) {
        row.verdict = Verdict::kPending;
      } else if (st.not_captured) {
        row.verdict = Verdict::kNotCaptured;
        row.reason = *st.not_captured;
        row.detail = st.detail;
      } else {
        row.verdict = Verdict::kHeard;
      }
    }
    for (const ProcessKey &k : plan.include_targets) {
      if (const auto i = t.Find(k)) {
        if (const auto it = row_of.find(pr[*i].app_id); it != row_of.end()) {
          ++plan.apps[it->second].streams;
        }
      }
    }
  }

  // Report warnings: multi-process sessions (05 §7.2) and instance rules on a pid that is gone (05 §5.3).
  {
    std::unordered_set<std::string> seen;
    for (std::size_t k = 0; k < ns; ++k) {
      if (sproc[k] >= 0 && in.sessions[k].multi_process) {
        const std::string &id = pr[static_cast<std::size_t>(sproc[k])].app_id;
        if (seen.insert(id).second) {
          plan.warnings.push_back(PlanWarning{"multi_process_session", id});
        }
      }
    }
    for (const int32_t ri : rx.instances) {
      const Rule &rule = ex_.rules[static_cast<std::size_t>(ri)];
      if (!t.Find(rule.pid)) {
        plan.warnings.push_back(PlanWarning{"instance_gone", rule.app_id});
      }
    }
  }

  // State for the next reconcile.
  render_seen_.clear();
  for (std::size_t k = 0; k < ns; ++k) {
    if (sproc[k] >= 0 && in.sessions[k].flow == Flow::kRender) {
      render_seen_.insert(t.at(static_cast<std::size_t>(sproc[k])).key);
    }
  }
  engine_ = plan.engine;
  exclude_target_ = plan.exclude_target;
  RecordLineage(t, cls, pr, lineage_);

#ifndef NDEBUG
  plan.violations = CheckInvariants(t, in, plan, ex_, opts_);
#endif
  return plan;
}

CommitResult Planner::CommitCheck(const ProcessTable &fresh, const Activation &a) const {
  if (a.kind == StreamKind::kEndpoint) {
    return CommitResult{true, Reason::kNone, {}};
  }
  Classifier cls(fresh, ex_, *rules_, lineage_);
  const std::vector<ProcessResult> pr = cls.Classify(mic_roots_);
  LiveStream planned;
  planned.kind = a.kind;
  planned.target = a.target;
  planned.parent_pid = a.parent_pid;
  planned.exe = a.exe;
  RemovalReason why = RemovalReason::kExited;
  const auto m = MatchStream(fresh, planned, why);
  if (!m) {
    return CommitResult{false, Reason::kNone, {}};
  }
  const std::size_t i = *m;
  if (a.kind == StreamKind::kExclude) {
    // I4's first half: every kept-out process still lies in R's strict tree, and R is no launcher.
    if (pr[i].launcher) {
      return CommitResult{false, Reason::kLauncher, {}};
    }
    for (std::size_t x = 0; x < fresh.size(); ++x) {
      if (pr[x].verdict == Verdict::kKeptOut && !fresh.InTree(x, i, false)) {
        return CommitResult{false, Reason::kTree, pr[x].app_name};
      }
    }
    return CommitResult{true, Reason::kNone, {}};
  }
  if (pr[i].verdict == Verdict::kKeptOut) {
    return CommitResult{false, pr[i].reason, {}};
  }
  if (pr[i].launcher) {
    return CommitResult{false, Reason::kLauncher, {}};
  }
  const DirtyMarks dirty = MarkDirty(fresh, pr, opts_.loose_links);
  if (dirty.strict[i] >= 0) {
    return CommitResult{false, Reason::kTree, pr[static_cast<std::size_t>(dirty.strict[i])].app_name};
  }
  if (dirty.loose[i] >= 0) {
    return CommitResult{false, Reason::kPidReuse, pr[static_cast<std::size_t>(dirty.loose[i])].app_name};
  }
  return CommitResult{true, Reason::kNone, {}};
}

} // namespace im::audiopolicy
