// The include set's streams (docs/m2/05-windows-audio.md §6.2, §6.3, §8; "05" below): the watcher's side of every
// capture stream a live share holds, from the held handle and the activation through the commit check and the join to
// the fade-out and the release.
//
// Life of a stream (one Entry):
//   1. a plan activates a target: a SYNCHRONIZE handle is taken first (process_hold.h; a pid that is another process
//      now asks for a new plan at once (05 §5.7), a refused handle activates anyway with the note no_handle), then the
//      request goes to the activator threads (activator.h), at most 8 in flight, the rest waiting here;
//   2. the activation's result comes back: a failure or a 3 s timeout counts activationFailures and is reopened after
//      100 ms, 500 ms, then 2 s; when the third reopen fails too the target is capture_failed until the next device or
//      session event (05 §8.5);
//   3. the commit check (05 §6.2 step 5): right before the join, on a fresh snapshot, the planner re-checks the target
//      (Planner::CommitCheck: still the planned process, heard, no launcher, no kept-out process in its OS tree). A
//      refused stream never joins, and the watcher plans again;
//   4. the join: the capture thread starts and drains the client (capture_client.h), and the mixer takes the channel
//      in Preroll, dropping the ring's backlog, so nothing it captured is heard for the next 40 ms; a reconcile in that
//      window that finds the target unsafe removes it without output (05 §9.2);
//   5. a removal fades it out over 10 ms of its buffer (05 §9.4), at once except for a covered stream, which waits
//      until one of its own coverers (a stream on the same target or on an OS ancestor of it) runs (05 §6.2 steps
//      3-4); a failed stream fades out too and reopens after the same delays as a failed activation (05 §8.5); a held
//      process that exits is removed at once (05 §5.7);
//   6. once the mixer let go of the channel the capture thread drops the stream and the entry goes.
// Every stream counts in the planner's input from step 1 until it is gone (PlanInput::streams).
//
// The exclusive streams (README WA53) live here too: the fast path's EXCLUDE stream on the tree of R (05 §6.4) and the
// endpoint stream (05 §6.1), each in its own mixer group (audiomix::kGroupExcludeSelf, kGroupEndpoint). An engine
// switch (05 §6.5) opens the new engine's stream with a 20 ms crossfade and lets the old engine's streams wait
// (RemovalReason::kEngine) until the mixer's takeover fades them out, with two exceptions: sound off fades everything
// at once, and the fast path's exit cuts the EXCLUDE stream's ring retroactively at C - 20 ms (05 §6.4, Apply's
// reveal_ns). A default-device change reopens the endpoint stream on the new default (ReopenEndpoint, 05 §7.5), the
// old one fading once the new one runs. An exclusive stream that fails is reopened after the same delays, the last
// one repeating, and never given up on (the planner has no capture_failed for it). The silent-stream check (05 §8.5,
// silent_check.h) reopens an app's include streams (ReopenApp) or gives up on them (GiveUpApp).
//
// Portable C++ (no OS header), owned by the watcher thread, not thread-safe. Its tests run with fakes for the
// activator, the capture thread and the process handles, and the real planner
// (native/windows/tests/audio_include_set_test.cpp).
#pragma once

#include <cstdint>
#include <deque>
#include <functional>
#include <iterator>
#include <memory>
#include <string>
#include <unordered_map>
#include <vector>

#include "audio/activator.h"
#include "audio/capture_client.h"
#include "audio/process_hold.h"
#include "im/audiomix/channel.h"
#include "im/audiomix/mix_engine.h"
#include "im/audiomix/report.h"
#include "im/audiopolicy/planner.h"

namespace im::audio {

// The delays before a failed stream or activation is opened again, and how many failures in a row make it
// capture_failed (05 §8.5): the first failure and the three reopens after 100 ms, 500 ms and 2 s.
inline constexpr int64_t kReopenDelaysNs[] = {100'000'000, 500'000'000, 2'000'000'000};
inline constexpr uint32_t kFailuresBeforeGivingUp = static_cast<uint32_t>(std::size(kReopenDelaysNs)) + 1;

struct StreamSetDeps {
  ActivatorPool *pool = nullptr; // started and stopped by the owner
  CaptureHost *capture = nullptr;
  ProcessGate *gate = nullptr;
  uint64_t (*filetime_now)() = nullptr; // the system time as a FILETIME (100 ns since 1601), for activated_filetime
};

struct StreamCounters {
  uint64_t activations = 0;         // activations started (05 §12.1 counters.activations)
  uint64_t activation_failures = 0; // failed or timed out (counters.activationFailures)
  uint64_t commit_refusals = 0;     // refused by the commit check (logged)
  uint64_t stream_failures = 0;     // joined streams that failed (05 §8.5)
  uint64_t discontinuities = 0;     // of the streams gone; Views' streams add theirs
  uint64_t fastpath_cuts = 0;       // EXCLUDE streams cut at the fast path's exit (05 §6.4)
  uint64_t loopback_failures = 0; // pumps in which a joined process-loopback stream failed (05 §6.5: "twice in 10 s")
  uint64_t silent_reopens = 0;    // include streams reopened by the silent-stream check (05 §8.5)
  uint32_t streams = 0;           // streams joined to the mixer now (counters.streams)
};

// One stream as the report shows it (05 §12.1 sessions[].stream, .format, .note).
struct StreamView {
  audiopolicy::StreamKind kind = audiopolicy::StreamKind::kInclude; // only include streams belong to a session
  audiopolicy::ProcessKey target;
  audiomix::SessionStream stream = audiomix::SessionStream::kNone;
  std::string format;
  std::string app_id;
  uint64_t tag = 0; // the mixer channel's tag (levels), 0 before the join
  bool has_handle = true;
  bool running = false; // its channel is Running
};

class StreamSet {
public:
  using CommitFn = std::function<audiopolicy::CommitResult(const audiopolicy::Activation &)>;
  using AppOf = std::function<std::string(const audiopolicy::ProcessKey &)>;
  // Whether desc lies in the strict OS tree of anc, anc != desc (ProcessTable::InTree on the plan's table).
  using Below = std::function<bool(const audiopolicy::ProcessKey &anc, const audiopolicy::ProcessKey &desc)>;

  explicit StreamSet(const StreamSetDeps &deps);
  ~StreamSet(); // Detach
  StreamSet(const StreamSet &) = delete;
  StreamSet &operator=(const StreamSet &) = delete;

  // A share starts: channels join engine from now on. engine must stay valid until Detach.
  void Attach(audiomix::MixEngine *engine);
  // The share stops; its mixer thread must have stopped (MixEngine::ReleaseAll's rule). Lets go of every channel,
  // drops every stream from the capture thread and cancels every activation.
  void Detach() noexcept;
  bool attached() const noexcept { return engine_ != nullptr; }

  // Every stream held, in a stable order (PlanInput::streams); Apply's removals index into the last call's list.
  const std::vector<audiopolicy::LiveStream> &Inputs();
  // The targets given up on (PlanInput::capture_failed).
  std::vector<audiopolicy::ProcessKey> CaptureFailed() const;
  // A device or session event: targets given up on are tried again (05 §8.5).
  void ForgetFailures();

  // Acts on a plan made with the last Inputs(): opens the activations (app_of names the include streams' apps),
  // removes the streams of plan.remove. A stream removed as covered waits for its own coverers: the other include
  // streams on its target or, by below, on an OS ancestor of it. A stream removed for an engine switch (kEngine,
  // kTarget) waits for the new engine's streams to run (the mixer's crossfade takes it over), except with engine none
  // (sound off: it fades at once) and for the EXCLUDE stream when the fast path exits into the include set: its ring is
  // cut at reveal_ns - kFastPathCutLeadNs, reveal_ns being the QPC time of the event that revealed the kept-out process
  // (0: now_ns, a poll's). Not attached: nothing to do.
  void Apply(const audiopolicy::Plan &plan, const AppOf &app_of, const Below &below, int64_t now_ns,
             int64_t reveal_ns = 0);
  // A default render device change (05 §7.5): a running endpoint stream reopens on the new default, the old one fading
  // out once the new one runs. Returns whether it did (the watcher then plans).
  bool ReopenEndpoint(int64_t now_ns);
  // The silent-stream check (05 §8.5): app_id's joined include streams fade out and open again at once (no failure
  // counted). Returns how many.
  uint32_t ReopenApp(const std::string &app_id, int64_t now_ns);
  // ...or app_id's include targets become capture_failed (until the next device or session event, ForgetFailures) and
  // their streams go. Returns how many targets.
  uint32_t GiveUpApp(const std::string &app_id);
  // Moves the streams on (results, commit checks, joins, failures, exits, releases, reopen delays). commit runs the
  // commit check for one activation (the caller takes the fresh snapshot once per Pump, lazily). Returns whether the
  // watcher must plan again: a stream left, a commit check refused, a held process exited, a key mismatch (also one
  // the last Apply met).
  bool Pump(int64_t now_ns, const CommitFn &commit);
  // The earliest reopen delay that ends, or INT64_MAX.
  int64_t NextDeadline() const noexcept;

  std::vector<StreamView> Views() const;
  StreamCounters counters() const;
  // Events for the report since the last call (stream_opened, activation_failed, stream_failed), oldest first.
  std::vector<audiomix::ReportEvent> TakeEvents();
  // The app id of a channel tag, also shortly after its stream went (the mixer's events name it); "" if unknown.
  std::string AppOfTag(uint64_t tag) const;

private:
  enum class Phase : uint8_t {
    kWaiting,    // to submit (8 in flight, or a reopen delay)
    kActivating, // at the activator
    kCommit,     // activated, waiting for the commit check
    kJoined,     // in the mixer
    kRemoving,   // its channel fades out; it goes once the mixer let go of it
  };
  struct Entry {
    uint64_t id = 0;
    audiopolicy::Activation act;
    std::string app_id;
    bool has_handle = true;
    std::unique_ptr<ProcessHold> hold;
    uint64_t activated_filetime = 0;
    Phase phase = Phase::kWaiting;
    int64_t retry_at = 0;
    std::unique_ptr<CaptureClient> client; // kCommit
    std::string format;
    uint32_t failed_attempts = 0;          // Initialize attempts that failed before the format that worked (05 §8.2)
    int32_t last_failure = 0;              // the HRESULT of the last of them
    std::shared_ptr<CaptureStream> stream; // kJoined, kRemoving
    std::unique_ptr<audiomix::Channel> channel;
    bool covered_wait = false;      // removed as covered, or for an engine switch: fades once a coverer runs
    std::vector<uint64_t> coverers; // covered_wait: the ids of the entries that cover (or replace) it
    bool reopen = false;            // kRemoving after a failure: opens again after the delay
    bool reopen_now = false;        // kRemoving for the silent-stream check: opens again at once
    bool crossfade = false;         // joined for an engine switch: its channel takes over from the other groups
  };

  Entry *Find(uint64_t id);
  void SubmitWaiting(int64_t now_ns);
  // A failed activation or stream: wait and open again, or give up (05 §8.5). Returns false when the entry goes.
  bool Backoff(Entry &e, int64_t now_ns);
  void StartRemoval(Entry &e);
  void Drop(std::size_t i);
  bool Join(Entry &e, OpenResult &&r);
  void RemoveAt(std::size_t i, bool cancel);
  void Event(int64_t t, audiomix::ReportEventCode code, const std::string &app, std::string detail = {});

  StreamSetDeps deps_;
  audiomix::MixEngine *engine_ = nullptr;
  std::vector<std::unique_ptr<Entry>> entries_;
  std::vector<audiopolicy::LiveStream> inputs_;
  std::vector<uint64_t> input_ids_; // entries_' ids in the order of inputs_
  uint64_t next_id_ = 1;
  struct Failures {
    uint32_t count = 0;
  };
  std::unordered_map<audiopolicy::ProcessKey, Failures, audiopolicy::ProcessKeyHash> failures_;
  std::vector<audiopolicy::ProcessKey> given_up_;
  std::deque<std::pair<uint64_t, std::string>> gone_apps_; // tags of entries gone, newest last (at most 64)
  StreamCounters counters_;
  std::vector<audiomix::ReportEvent> events_;
  int64_t now_ns_ = 0;
  bool replan_ = false;                             // Apply met a key mismatch: the next Pump asks for a plan
  std::vector<audiopolicy::ProcessKey> mismatched_; // the targets the last Apply met a mismatch on
};

} // namespace im::audio
