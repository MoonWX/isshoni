// The exclusion policy and the planner (docs/m2/05-windows-audio.md §5.3-§5.10, §6): classification of every process
// of a snapshot, mic and lineage state, engine selection, the include set, the fast path and the per-reconcile
// re-checks of live streams.
//
// The Windows watcher calls Planner::Reconcile on its thread on every reconcile (05 §7.3) with a fresh ProcessTable,
// the session list and the streams it holds, then acts on the Plan: it starts the activations (each passes
// CommitCheck on a fresh snapshot right before its stream joins the mixer, 05 §6.2 step 5), fades out the removals and
// writes the report from the plan's rows. The Planner holds what lasts across reconciles: the rules, the sticky mic
// roots (05 §5.6), the lineage cache (05 §5.7), the fast path's 10 s timer (05 §6.4), the 3 s grace of streams whose
// session vanished and the pid_reuse blocks already counted. It is not thread-safe: one thread owns it.
#pragma once

#include <cstddef>
#include <cstdint>
#include <memory>
#include <optional>
#include <span>
#include <string>
#include <string_view>
#include <unordered_map>
#include <unordered_set>
#include <vector>

#include "im/audiopolicy/exclusions.h"
#include "im/audiopolicy/process_table.h"
#include "im/audiopolicy/types.h"
#include "isshoni_media.h"

namespace im::audiopolicy {

// One audio session of the latest enumeration (05 §7.2): render sessions are what may be captured, capture sessions
// make their process a mic user (05 §5.6).
struct Session {
  uint32_t pid = 0;
  Flow flow = Flow::kRender;
  SessionState state = SessionState::kInactive;
  bool multi_process = false; // GetProcessId returned AUDCLNT_S_NO_SINGLE_PROCESS (05 §7.2)
  bool playing = false;       // apps[].playing: active, or its meter above -70 dBFS, within the last 2 s (05 §12.1)
  int64_t last_active_ns = 0; // engine clock of its last activity, 0 = never: the include set's order (05 §6.2 step 4)
};

// A stream the engine holds when it reconciles (05 §8). The exclusive stream (EXCLUDE or endpoint) is one of them.
struct LiveStream {
  StreamKind kind = StreamKind::kInclude;
  ProcessKey target;       // the key it was planned for (I3); {0, 0} for the endpoint stream
  bool has_handle = true;  // a SYNCHRONIZE handle on the target is held (05 §5.7); false: checked by snapshot
  uint32_t parent_pid = 0; // the target's parent pid and folded exe when planned: a target whose creation time is
  std::string exe;         // unknown matches a process whose time is still unknown with the same exe and parent
  uint64_t activated_filetime = 0; // when the activation completed, FILETIME: the late-child fallback (§6.2 step 3)
};

// What one reconcile's planner needs besides the table.
struct PlanInput {
  int64_t now_ns = 0;                         // engine clock (QPC ns), non-decreasing across calls
  uint32_t mode = IM_AUDIO_MODE_EXCLUDE_APPS; // IM_AUDIO_MODE_*: only EXCLUDE_APPS captures; OFF, the test tone and
                                              // the reserved EVERYTHING plan no stream (engine none)
  bool process_loopback = true;               // the startup probe passed or still runs (05 §6.1, §11.2)
  std::span<const Session> sessions;          // expired and system-sounds sessions already left out
  std::span<const LiveStream> streams;        // every stream the engine holds now
  std::span<const ProcessKey> capture_failed; // include targets the watcher gave up on (05 §8.5)
};

// A stream to open.
struct Activation {
  StreamKind kind = StreamKind::kInclude;
  ProcessKey target;       // {0, 0} for the endpoint stream
  uint32_t parent_pid = 0; // copy into LiveStream for the no-handle check
  std::string exe;
};

struct Removal {
  std::size_t stream = 0; // index into PlanInput::streams
  RemovalReason reason = RemovalReason::kExited;
};

// The classification of one process of the table (05 §5.3, §5.8). Row 7 (launchers) and the planner's reasons are
// per session (SessionResult), because they depend on the engine.
struct ProcessResult {
  Verdict verdict = Verdict::kHeard; // kKeptOut (rows 1-4, 6) or kHeard (rows 5, 8)
  Reason reason = Reason::kNone;     // kept out: builtin, user, viewer, isshoni, mic
  int32_t rule = -1;                 // the deciding rule (rows 1-5), index into Exclusions::rules; -1 none
  bool allowed = false;              // row 5: a saved "Include"
  bool launcher = false;             // its exe has an IM_RULE_STOP rule
  bool browser = false;              // its app root's exe has an IM_RULE_BROWSER rule
  bool uses_mic = false;             // its app root (or a lineage root) is a mic root, whatever the flag
  bool shared = false;               // its app root is the shared app's root (05 §5.8)
  bool lineage = false;              // the verdict needed the lineage cache (an orphan)
  int32_t app_root = -1;             // table index of the last process of its app walk
  std::string app_id;                // the deciding rule's app id, else "exe:<app root exe>" (05 §5.1)
  std::string app_name;              // the rule's name, else the app root's FileDescription, else the exe stem
  std::string app_exe;               // the deciding rule's exe, the instance's exe, else the app root's exe
  uint32_t warnings = 0;             // kWarn* bits
};

// One session's outcome (05 §12.1 sessions[]).
struct SessionResult {
  int32_t process = -1; // table index, -1 = not in the snapshot (verdict pending)
  Verdict verdict = Verdict::kPending;
  Reason reason = Reason::kNone;
  Note note = Note::kNone;
  std::string detail; // tree, pid_reuse: the kept-out app's name
  std::string app_id;
  bool target = false; // its process is an include target of this plan (or is the target R)
};

// One row of "What friends hear" (05 §12.1 apps[]): apps with a session, running apps matched by a built-in rule and
// the shared app.
struct AppRow {
  std::string id;
  std::string name;
  std::string exe;
  Verdict verdict = Verdict::kHeard;
  Reason reason = Reason::kNone;
  std::string detail;
  bool playing = false;
  bool uses_mic = false;
  bool browser = false;
  bool shared = false;
  uint32_t pids = 0;     // processes of the app
  uint32_t sessions = 0; // sessions of its processes, both flows
  uint32_t streams = 0;  // include targets among its processes
  uint32_t warnings = 0; // kWarn* bits
};

// A report-level warning (05 §12.1 warnings[]).
struct PlanWarning {
  std::string_view code; // "multi_process_session", "instance_gone"
  std::string app;       // app id
};

// An invariant of 05 §6.3 that a plan breaks (CheckInvariants).
enum class Invariant : uint8_t { kI1 = 1, kI2, kI3, kI4, kI5 };

struct Violation {
  Invariant invariant = Invariant::kI1;
  ProcessKey target;   // the stream's target (I1-I4)
  ProcessKey offender; // the kept-out or heard process that breaks it, if any
  std::string what;    // for test output; carries no exe or app name (05 §16)
};

// One reconcile's plan. The verdicts depend on the engine:
//   - include-set: the include set's own (targets, covered, tree, pid_reuse, launcher, limit, capture_failed);
//   - none (sound off, a watcher before the share, a mode without capture): the include set's verdicts as a
//     prediction, so "What friends hear" stays current (05 §6.5), with no target, activation or kept stream;
//   - exclude-self: every session outside the kept-out apps is heard, launchers included (05 §6.4 "known difference");
//   - endpoint: kept-out apps keep the policy's verdict and everything else is heard; nothing is actually kept out,
//     which the report's engine and banner say (05 §15).
struct Plan {
  Engine engine = Engine::kNone;
  Engine previous_engine = Engine::kNone;  // the engine of the previous plan (fastpath_enter, fastpath_exit events)
  ProcessKey exclude_target;               // kExcludeSelf: R
  std::vector<ProcessKey> include_targets; // kIncludeSet: every target with a stream after this plan, kept and new,
                                           // in priority order, at most PlannerOptions::max_include_streams
  std::vector<Activation> activate;        // streams to open (new include targets, or the exclusive stream)
  std::vector<Removal> remove;             // live streams to fade out
  std::vector<ProcessResult> processes;    // per table index
  std::vector<SessionResult> sessions;     // per PlanInput::sessions index
  std::vector<AppRow> apps;
  std::vector<PlanWarning> warnings;
  uint32_t pid_reuse_blocks = 0;     // sessions newly blocked with pid_reuse (counter pidReuseBlocks, 05 §5.2)
  std::vector<Violation> violations; // debug builds: CheckInvariants of this plan, which must stay empty
};

// The result of the commit check (05 §6.2 step 5).
struct CommitResult {
  bool ok = false;
  Reason reason = Reason::kNone; // why not: kTree, kPidReuse, kLauncher, a kept-out reason; kNone = target gone
  std::string detail;
};

struct PlannerOptions {
  uint32_t max_include_streams = 48;             // 05 §6.2 step 4, §17
  int64_t fast_path_enter_ns = 10'000'000'000;   // the fast path's condition must hold this long (05 §6.4)
  int64_t session_grace_ns = 3'000'000'000;      // a stream whose session vanished lives this long (05 §6.2 step 6)
  bool loose_links = true;                       // the loose union of 05 §5.2 (off if case 27 shows Windows ignores
                                                 // reused-PID links; I1 then uses strict links only)
  bool late_child_own_stream = false;            // §6.2 step 3's fallback, if case 25 fails on Windows
  bool fast_path_exit_on_new_descendant = false; // §6.4's fallback, if case 26 fails on Windows
};

namespace detail {
struct RuleIndex; // the rules by exe and kind (planner.cpp)
using KeySet = std::unordered_set<ProcessKey, ProcessKeyHash>;
// What the lineage cache remembers of a kept-out process (05 §5.7), for its lifetime: facts, not verdicts, so a later
// rule change re-decides an orphan with the current rules. Each list is bounded (kMaxWalk + 1 entries, 8 roots).
struct LineageEntry {
  std::vector<std::string> exes;     // the exes of its app walks while its links were intact
  std::vector<ProcessKey> roots;     // the app roots it had then (row 6)
  std::vector<ProcessKey> ancestors; // its pid chain then (row 1's instance trees)
};
using LineageMap = std::unordered_map<ProcessKey, LineageEntry, ProcessKeyHash>;
} // namespace detail

class Planner {
public:
  explicit Planner(PlannerOptions opts = {});

  // Replaces the rules, flags and shared pid (05 §4.3: applied on the next reconcile, which the watcher runs at once).
  void SetExclusions(Exclusions ex);
  const Exclusions &exclusions() const noexcept { return ex_; }
  const PlannerOptions &options() const noexcept { return opts_; }

  // An engine start (Mixer::Start, or sound off -> on): the next plan picks its engine as at start, so the fast path
  // is used from the first chunk when its condition holds (05 §6.4). Mic roots and the lineage cache survive.
  void Restart();
  // Forgets everything learned (watcher stop): mic roots, lineage, timers. The rules stay.
  void Reset();

  // One reconcile (05 §7.3) after the table refresh: updates mic and lineage state, classifies, plans.
  Plan Reconcile(const ProcessTable &table, const PlanInput &in);

  // The commit check of 05 §6.2 step 5 on a fresh snapshot, with the state of the last reconcile: an include target
  // must still be there with its planned key, heard, not a launcher and with no kept-out process in its OS tree; an
  // exclude target must still satisfy the fast path's condition (I4).
  CommitResult CommitCheck(const ProcessTable &fresh, const Activation &a) const;

  // State for tests and the report.
  bool IsMicRoot(const ProcessKey &k) const { return mic_roots_.count(k) != 0; }
  std::size_t MicRootCount() const noexcept { return mic_roots_.size(); }
  std::size_t LineageCount() const noexcept { return lineage_.size(); }

private:
  PlannerOptions opts_;
  Exclusions ex_;
  std::shared_ptr<const detail::RuleIndex> rules_;
  detail::KeySet mic_roots_;
  detail::LineageMap lineage_;
  Engine engine_ = Engine::kNone;     // the previous plan's engine
  ProcessKey exclude_target_;         // the previous plan's R
  std::optional<int64_t> fast_since_; // since when the fast path's condition holds without a break, for fast_r_
  ProcessKey fast_r_;
  std::unordered_map<ProcessKey, int64_t, ProcessKeyHash> missing_since_; // include targets without a render session
  detail::KeySet pid_reuse_blocked_; // session processes whose pid_reuse block was counted
  detail::KeySet render_seen_;       // render-session processes of the previous reconcile
};

// Checks I1-I5 of 05 §6.3 for a plan made from (table, in), with algorithms of its own (descendant searches over the
// table's links) so a planner bug can't hide in a shared helper:
//   I1 no include stream that runs after the plan has a kept-out process in its target's OS tree (strict ∪ loose, or
//      strict only when opts.loose_links is off);
//   I2 no include stream targets a launcher, a kept-out process or isshoni's own tree (the SELF instance rules);
//   I3 every stream that runs after the plan targets the key it was planned for: the key is in the table (a target
//      planned with an unknown creation time: the same pid, still unknown, with the same exe and parent pid);
//   I4 in exclude-self every kept-out process is in R's strict tree and no heard render-session process is in R's
//      loose tree;
//   I5 at most one exclusive (EXCLUDE or endpoint) stream is kept or opened by the plan (the one it replaces fades out
//      in the 20 ms crossfade).
// "Runs after the plan" means: a live stream the plan doesn't remove, or one it activates.
std::vector<Violation> CheckInvariants(const ProcessTable &table, const PlanInput &in, const Plan &plan,
                                       const Exclusions &ex, const PlannerOptions &opts);

// I5 over the streams an engine holds at one instant: at most one exclusive stream, or two while a crossfade between
// engines (at most 20 ms, 05 §6.5) runs.
std::vector<Violation> CheckExclusiveStreams(std::span<const LiveStream> streams, bool crossfading);

} // namespace im::audiopolicy
