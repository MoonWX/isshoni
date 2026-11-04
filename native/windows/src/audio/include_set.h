// The include set's streams (docs/m2/05-windows-audio.md §6.2, §6.3, §8; "05" below): the watcher's side of every
// capture stream a live share holds, from the held handle and the activation through the commit check and the join to
// the fade-out and the release.
//
// Life of a stream (one Entry):
//   1. a plan activates a target: a SYNCHRONIZE handle is taken first (process_hold.h; a pid that is another process
//      now asks for a new plan, a refused handle activates anyway with the note no_handle), then the request goes to
//      the activator threads (activator.h), at most 8 in flight, the rest waiting here;
//   2. the activation's result comes back: a failure or a 3 s timeout counts activationFailures and retries after
//      100 ms, 500 ms, then 2 s; after the third failure the target is capture_failed until the next device or session
//      event (05 §8.5);
//   3. the commit check (05 §6.2 step 5): right before the join, on a fresh snapshot, the planner re-checks the target
//      (Planner::CommitCheck: still the planned process, heard, no launcher, no kept-out process in its OS tree). A
//      refused stream never joins, and the watcher plans again;
//   4. the join: the capture thread starts and drains the client (capture_client.h), and the mixer takes the channel
//      in Preroll, dropping the ring's backlog, so nothing it captured is heard for the next 40 ms; a reconcile in that
//      window that finds the target unsafe removes it without output (05 §9.2);
//   5. a removal fades it out over 10 ms of its buffer (05 §9.4), at once except for a covered stream, which waits
//      until its coverer runs (05 §6.2 steps 3-4); a failed stream fades out too and reopens after the same delays as
//      a failed activation (05 §8.5); a held process that exits is removed at once (05 §5.7);
//   6. once the mixer let go of the channel the capture thread drops the stream and the entry goes.
// Every stream counts in the planner's input from step 1 until it is gone (PlanInput::streams).
//
// The fast path's EXCLUDE stream and the endpoint stream are README WA53's: their activations are not opened here.
//
// Portable C++ (no OS header), owned by the watcher thread, not thread-safe. Its tests run with fakes for the
// activator, the capture thread and the process handles, and the real planner
// (native/windows/tests/audio_include_set_test.cpp).
#pragma once

#include <cstdint>
#include <deque>
#include <functional>
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

// The delays before a failed stream or activation is opened again, and how many failures make it capture_failed
// (05 §8.5).
inline constexpr int64_t kReopenDelaysNs[] = {100'000'000, 500'000'000, 2'000'000'000};
inline constexpr uint32_t kFailuresBeforeGivingUp = 3;

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
  uint32_t streams = 0;             // streams joined to the mixer now (counters.streams)
};

// One stream as the report shows it (05 §12.1 sessions[].stream, .format, .note).
struct StreamView {
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

  // Acts on a plan made with the last Inputs(): opens the include activations (app_of names their apps), removes the
  // streams of plan.remove. Not attached: nothing to do.
  void Apply(const audiopolicy::Plan &plan, const AppOf &app_of, int64_t now_ns);
  // Moves the streams on (results, commit checks, joins, failures, exits, releases, reopen delays). commit runs the
  // commit check for one activation (the caller takes the fresh snapshot once per Pump, lazily). Returns whether the
  // watcher must plan again: a stream left, a commit check refused, a held process exited, a key mismatch.
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
    std::shared_ptr<CaptureStream> stream; // kJoined, kRemoving
    std::unique_ptr<audiomix::Channel> channel;
    bool covered_wait = false; // removed as covered: fades once its coverer runs
    bool reopen = false;       // kRemoving after a failure: opens again after the delay
  };

  Entry *Find(uint64_t id);
  void SubmitWaiting(int64_t now_ns);
  // A failed activation or stream: wait and open again, or give up (05 §8.5). Returns false when the entry goes.
  bool Backoff(Entry &e, int64_t now_ns);
  void StartRemoval(Entry &e);
  void Drop(std::size_t i);
  bool Join(Entry &e, OpenResult &&r);
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
};

} // namespace im::audio
