#include "audio/include_set.h"

#include <algorithm>
#include <climits>
#include <string>
#include <utility>

#include "audio/engine_gate.h"
#include "im/audiopolicy/types.h"
#include "im/log/ring.h"

namespace im::audio {
namespace {

using audiomix::ReportEventCode;
using audiomix::SessionStream;
using audiomix::StreamState;
using audiopolicy::RemovalReason;
using audiopolicy::StreamKind;

constexpr std::size_t kGoneApps = 64;

} // namespace

StreamSet::StreamSet(const StreamSetDeps &deps) : deps_(deps) {}

StreamSet::~StreamSet() { Detach(); }

void StreamSet::Attach(audiomix::MixEngine *engine) { engine_ = engine; }

void StreamSet::Detach() noexcept {
  if (engine_ != nullptr) {
    engine_->ReleaseAll(); // the mixer thread has stopped: every channel is let go of
  }
  for (auto &e : entries_) {
    if (e->phase == Phase::kActivating && deps_.pool != nullptr) {
      deps_.pool->Cancel(e->id);
    }
    if (e->stream && deps_.capture != nullptr) {
      deps_.capture->Remove(e->stream);
    }
    if (e->stream) {
      counters_.discontinuities += e->stream->discontinuities.load(std::memory_order_relaxed);
    }
  }
  entries_.clear(); // channels, clients and holds go here (the watcher thread: COM MTA)
  inputs_.clear();
  input_ids_.clear();
  engine_ = nullptr;
  counters_.streams = 0;
  replan_ = false;
  mismatched_.clear();
}

StreamSet::Entry *StreamSet::Find(uint64_t id) {
  for (auto &e : entries_) {
    if (e->id == id) {
      return e.get();
    }
  }
  return nullptr;
}

const std::vector<audiopolicy::LiveStream> &StreamSet::Inputs() {
  inputs_.clear();
  input_ids_.clear();
  for (const auto &e : entries_) {
    audiopolicy::LiveStream s;
    s.kind = e->act.kind;
    s.target = e->act.target;
    s.has_handle = e->has_handle;
    s.parent_pid = e->act.parent_pid;
    s.exe = e->act.exe;
    s.activated_filetime = e->activated_filetime; // 0 until the activation completed: it covers every process
    inputs_.push_back(std::move(s));
    input_ids_.push_back(e->id);
  }
  return inputs_;
}

std::vector<audiopolicy::ProcessKey> StreamSet::CaptureFailed() const { return given_up_; }

void StreamSet::ForgetFailures() {
  given_up_.clear();
  failures_.clear();
}

void StreamSet::Event(int64_t t, ReportEventCode code, const std::string &app, std::string detail) {
  events_.push_back(audiomix::ReportEntry{t, code, app, std::move(detail)});
}

namespace {

// The stream kind of an engine (05 §6.1): the streams that replace the others at a switch.
bool KindOfEngine(audiopolicy::Engine e, StreamKind *kind) {
  switch (e) {
  case audiopolicy::Engine::kIncludeSet:
    *kind = StreamKind::kInclude;
    return true;
  case audiopolicy::Engine::kExcludeSelf:
    *kind = StreamKind::kExclude;
    return true;
  case audiopolicy::Engine::kEndpoint:
    *kind = StreamKind::kEndpoint;
    return true;
  case audiopolicy::Engine::kNone:
    break;
  }
  return false;
}

uint8_t GroupOf(StreamKind k) {
  switch (k) {
  case StreamKind::kInclude:
    return audiomix::kGroupInclude;
  case StreamKind::kExclude:
    return audiomix::kGroupExcludeSelf;
  case StreamKind::kEndpoint:
    return audiomix::kGroupEndpoint;
  }
  return audiomix::kGroupInclude;
}

} // namespace

void StreamSet::RemoveAt(std::size_t i, bool cancel) {
  if (cancel && entries_[i]->phase == Phase::kActivating && deps_.pool != nullptr) {
    deps_.pool->Cancel(entries_[i]->id);
  }
  Drop(i);
}

void StreamSet::Apply(const audiopolicy::Plan &plan, const AppOf &app_of, const Below &below, int64_t now_ns,
                      int64_t reveal_ns) {
  now_ns_ = now_ns;
  if (engine_ == nullptr) {
    return;
  }
  for (auto &e : entries_) {
    e->covered_wait = false; // each plan names the streams that still wait
    e->coverers.clear();
  }
  StreamKind wanted = StreamKind::kInclude;
  const bool capturing = KindOfEngine(plan.engine, &wanted);
  const int64_t cut_at = (reveal_ns != 0 ? reveal_ns : now_ns) - kFastPathCutLeadNs;
  // Removals first: they index into the input of this plan.
  std::vector<uint64_t> drop;
  std::vector<uint64_t> covered;  // wait for an include stream on their target or an ancestor (05 §6.2 step 4)
  std::vector<uint64_t> replaced; // wait for the new engine's streams (05 §6.5)
  for (const audiopolicy::Removal &r : plan.remove) {
    if (r.stream >= input_ids_.size()) {
      continue;
    }
    Entry *e = Find(input_ids_[r.stream]);
    if (e == nullptr || e->phase == Phase::kRemoving) {
      continue;
    }
    if (e->phase != Phase::kJoined) {
      drop.push_back(e->id); // still activating or waiting for its commit: cancelled, nothing was mixed
      continue;
    }
    if (r.reason == RemovalReason::kCovered) {
      // Fades once its coverer runs (Pump), so the sound has no gap.
      e->covered_wait = true;
      covered.push_back(e->id);
      continue;
    }
    const bool engine_switch = r.reason == RemovalReason::kEngine || r.reason == RemovalReason::kTarget;
    if (engine_switch && e->act.kind == StreamKind::kExclude && plan.engine == audiopolicy::Engine::kIncludeSet) {
      // The fast path's exit (05 §6.4): the EXCLUDE stream's ring is cut at C - 20 ms with a 5 ms fade ending there,
      // so a voice app whose session appeared at C is never mixed; the include streams open as usual (a 60-80 ms gap).
      // A cut point the mixer already mixed fades out over 5 ms at once (fastpath_late_cut). One not audible yet
      // leaves without output.
      const StreamState st = e->channel->state();
      if (st == StreamState::kIdle || st == StreamState::kPreroll) {
        e->channel->Remove();
      } else {
        e->channel->CutAt(cut_at);
      }
      e->phase = Phase::kRemoving;
      ++counters_.fastpath_cuts;
      continue;
    }
    if (engine_switch && capturing) {
      e->covered_wait = true; // the new engine's stream crossfades in and takes it over (05 §6.5)
      replaced.push_back(e->id);
      continue;
    }
    StartRemoval(*e); // at once: sound off (engine none), or a reason that doesn't wait
  }
  for (const uint64_t id : drop) {
    for (std::size_t i = 0; i < entries_.size(); ++i) {
      if (entries_[i]->id == id) {
        RemoveAt(i, true);
        break;
      }
    }
  }

  std::vector<audiopolicy::ProcessKey> mismatched;
  for (const audiopolicy::Activation &a : plan.activate) {
    bool held = false;
    for (const auto &e : entries_) {
      held = held || (e->act.kind == a.kind && e->act.target == a.target && e->phase != Phase::kRemoving);
    }
    if (held) {
      continue; // already opening it
    }
    auto e = std::make_unique<Entry>();
    e->id = next_id_++;
    e->act = a;
    e->app_id = app_of && a.kind == StreamKind::kInclude ? app_of(a.target) : std::string();
    if (a.kind != StreamKind::kEndpoint) {
      // The handle before the activation (05 §5.7): the pid can't be reused while it is held.
      const HoldStatus hs = deps_.gate != nullptr ? deps_.gate->Hold(a.target, &e->hold) : HoldStatus::kNoHandle;
      if (hs == HoldStatus::kMismatch) {
        // The process is gone or the pid is another process's now: the plan is recomputed (05 §5.7) on a fresh
        // snapshot right after this one, once per target in a row, so a snapshot that keeps disagreeing with the
        // process waits for the next poll instead of planning on every pump.
        if (std::find(mismatched_.begin(), mismatched_.end(), a.target) == mismatched_.end()) {
          replan_ = true;
        }
        mismatched.push_back(a.target);
        continue;
      }
      e->has_handle = hs == HoldStatus::kHeld;
      if (!e->has_handle) {
        log::Info("audio", "capture target without a process handle: checked by snapshot");
      }
    }
    e->phase = Phase::kWaiting;
    e->retry_at = now_ns;
    entries_.push_back(std::move(e));
  }
  mismatched_ = std::move(mismatched);

  // Each covered stream's coverers, now that this plan's activations are entries too (05 §6.2 step 4): the include
  // streams that stay, on its own target (a second stream on it) or on an OS ancestor of it. Pump fades it once one of
  // them runs; with none (its ancestor's activation met a key mismatch, or its coverers went) it keeps playing, alone,
  // until a plan decides again, so its sound has no gap and is never heard twice.
  for (const uint64_t id : covered) {
    Entry *c = Find(id);
    if (c == nullptr) {
      continue;
    }
    for (const auto &o : entries_) {
      if (o.get() == c || o->act.kind != StreamKind::kInclude || o->covered_wait || o->phase == Phase::kRemoving) {
        continue;
      }
      if (o->act.target == c->act.target || (below && below(o->act.target, c->act.target))) {
        c->coverers.push_back(o->id);
      }
    }
  }
  // Each replaced stream's replacements: the new engine's streams that stay (the EXCLUDE stream on R, the endpoint
  // stream, or the include streams). The first of them that runs takes the old engine over in the mixer (its 20 ms
  // crossfade); Pump fades the old stream itself then too. Until one runs, the old engine keeps playing.
  for (const uint64_t id : replaced) {
    Entry *c = Find(id);
    if (c == nullptr) {
      continue;
    }
    for (const auto &o : entries_) {
      if (o.get() != c && o->act.kind == wanted && !o->covered_wait && o->phase != Phase::kRemoving) {
        c->coverers.push_back(o->id);
      }
    }
  }
  SubmitWaiting(now_ns);
}

bool StreamSet::ReopenEndpoint(int64_t now_ns) {
  if (engine_ == nullptr) {
    return false;
  }
  bool running = false;
  for (const auto &e : entries_) {
    running = running || (e->act.kind == StreamKind::kEndpoint && e->phase == Phase::kJoined);
  }
  if (!running) {
    return false; // none, or one still opening (it opens on the new default anyway)
  }
  // The new stream goes first in the planner's input, so the next plan keeps it and replaces the old one, which then
  // fades once the new one runs (Apply's replaced streams).
  auto e = std::make_unique<Entry>();
  e->id = next_id_++;
  e->act.kind = StreamKind::kEndpoint;
  e->phase = Phase::kWaiting;
  e->retry_at = now_ns;
  entries_.insert(entries_.begin(), std::move(e));
  SubmitWaiting(now_ns);
  return true;
}

uint32_t StreamSet::ReopenApp(const std::string &app_id, int64_t now_ns) {
  now_ns_ = now_ns;
  uint32_t n = 0;
  for (auto &e : entries_) {
    if (e->act.kind == StreamKind::kInclude && e->app_id == app_id && e->phase == Phase::kJoined) {
      e->phase = Phase::kRemoving;
      e->reopen_now = true;
      e->covered_wait = false;
      e->channel->Remove();
      ++counters_.silent_reopens;
      ++n;
    }
  }
  return n;
}

bool StreamSet::ReopenExclusive(int64_t now_ns) {
  now_ns_ = now_ns;
  for (auto &e : entries_) {
    if (e->act.kind == StreamKind::kExclude && e->phase == Phase::kJoined && !e->covered_wait) {
      e->phase = Phase::kRemoving;
      e->reopen_now = true;
      e->channel->Remove();
      ++counters_.silent_reopens;
      return true;
    }
  }
  return false;
}

uint64_t StreamSet::ExclusiveTag() const noexcept {
  for (const auto &e : entries_) {
    if (e->act.kind == StreamKind::kExclude && e->phase == Phase::kJoined && !e->covered_wait) {
      return e->id;
    }
  }
  return 0;
}

uint32_t StreamSet::GiveUpApp(const std::string &app_id) {
  uint32_t n = 0;
  for (std::size_t i = 0; i < entries_.size();) {
    Entry &e = *entries_[i];
    if (e.act.kind != StreamKind::kInclude || e.app_id != app_id) {
      ++i;
      continue;
    }
    if (std::find(given_up_.begin(), given_up_.end(), e.act.target) == given_up_.end()) {
      given_up_.push_back(e.act.target); // capture_failed until the next device or session event (05 §8.5)
      ++n;
    }
    failures_.erase(e.act.target);
    if (e.phase == Phase::kJoined || e.phase == Phase::kRemoving) {
      if (e.phase == Phase::kJoined) {
        StartRemoval(e);
      }
      e.reopen = false;
      e.reopen_now = false;
      ++i;
    } else {
      RemoveAt(i, true);
    }
  }
  return n;
}

void StreamSet::SubmitWaiting(int64_t now_ns) {
  for (auto &e : entries_) {
    if (e->phase != Phase::kWaiting || e->retry_at > now_ns) {
      continue;
    }
    if (deps_.pool == nullptr || deps_.pool->in_flight() >= kMaxActivationsInFlight) {
      return;
    }
    if (!deps_.pool->Submit(OpenRequest{e->id, e->act.kind, e->act.target.pid})) {
      return;
    }
    e->phase = Phase::kActivating;
    ++counters_.activations;
  }
}

bool StreamSet::Backoff(Entry &e, int64_t now_ns) {
  Failures &f = failures_[e.act.target];
  ++f.count;
  if (e.act.kind != StreamKind::kInclude) {
    // The exclusive stream is the engine itself: reopened after the same delays, the last one repeating, never given
    // up on (the planner has no capture_failed for it; the probe and "twice in 10 s" decide the engine instead).
    f.count = std::min<uint32_t>(f.count, static_cast<uint32_t>(std::size(kReopenDelaysNs)));
  } else if (f.count >= kFailuresBeforeGivingUp) {
    given_up_.push_back(e.act.target); // capture_failed until the next device or session event (05 §8.5)
    failures_.erase(e.act.target);
    return false;
  }
  e.phase = Phase::kWaiting;
  e.retry_at = now_ns + kReopenDelaysNs[f.count - 1]; // 1 <= count <= std::size(kReopenDelaysNs) here
  e.activated_filetime = 0;
  e.client.reset();
  e.format.clear();
  e.failed_attempts = 0;
  e.last_failure = 0;
  return true;
}

void StreamSet::StartRemoval(Entry &e) {
  e.phase = Phase::kRemoving;
  e.covered_wait = false;
  if (e.channel) {
    e.channel->Remove(); // a running channel fades over 10 ms; one in Preroll leaves without output
  }
}

void StreamSet::Drop(std::size_t i) {
  Entry &e = *entries_[i];
  if (e.channel) {
    gone_apps_.emplace_back(e.id, e.app_id);
    while (gone_apps_.size() > kGoneApps) {
      gone_apps_.pop_front();
    }
  }
  if (e.stream) {
    if (deps_.capture != nullptr) {
      deps_.capture->Remove(e.stream);
    }
    counters_.discontinuities += e.stream->discontinuities.load(std::memory_order_relaxed);
  }
  entries_.erase(entries_.begin() + static_cast<std::ptrdiff_t>(i));
}

bool StreamSet::Join(Entry &e, OpenResult &&r) {
  auto stream = std::make_shared<CaptureStream>(std::move(r.client));
  // An engine switch (05 §6.5): the new engine's stream crossfades in over 20 ms and takes over from the streams that
  // wait for it (another group's, still joined). A stream that opens inside the running engine (a reopen after a
  // failure or a device change, the include set's own targets) fades in on its own.
  bool crossfade = false;
  for (const auto &other : entries_) {
    crossfade = crossfade || (other.get() != &e && other->phase == Phase::kJoined && other->covered_wait &&
                              other->act.kind != e.act.kind && other->channel && !other->channel->released());
  }
  audiomix::ChannelOptions o;
  o.ring = &stream->ring;
  o.tag = e.id;
  o.group = GroupOf(e.act.kind);
  o.crossfade = crossfade;
  std::unique_ptr<audiomix::Channel> ch;
  if (const im_status st = audiomix::Channel::Create(std::move(o), &ch); st != IM_OK) {
    log::Warn("audio", "capture stream: no mixer channel", {{"status", st}});
    return false;
  }
  if (deps_.capture == nullptr || !deps_.capture->Add(stream)) {
    log::Warn("audio", "capture stream: the capture thread did not take it");
    return false;
  }
  if (!engine_->Add(ch.get())) {
    deps_.capture->Remove(stream);
    log::Warn("audio", "capture stream: the mixer has no free channel");
    return false;
  }
  e.stream = std::move(stream);
  e.channel = std::move(ch);
  e.format = std::move(r.format);
  e.phase = Phase::kJoined;
  e.crossfade = crossfade;
  return true;
}

bool StreamSet::Pump(int64_t now_ns, const CommitFn &commit) {
  now_ns_ = now_ns;
  bool replan = false;
  if (engine_ == nullptr) {
    return false;
  }

  // Activation results.
  if (deps_.pool != nullptr) {
    for (OpenResult &r : deps_.pool->TakeResults()) {
      Entry *e = Find(r.id);
      if (e == nullptr || e->phase != Phase::kActivating) {
        continue; // cancelled meanwhile: its client goes here
      }
      if (r.aborted) {
        e->phase = Phase::kWaiting; // the pool was cancelling it while the plan kept it: open it again
        e->retry_at = now_ns;
        continue;
      }
      if (r.hr != 0 || !r.client) {
        ++counters_.activation_failures;
        Event(now_ns, ReportEventCode::kActivationFailed, e->app_id, r.timed_out ? "timeout" : "error");
        log::Warn("audio", "capture stream activation failed",
                  {{"hr", static_cast<int64_t>(r.hr)}, {"timed_out", r.timed_out}});
        if (!Backoff(*e, now_ns)) {
          for (std::size_t i = 0; i < entries_.size(); ++i) {
            if (entries_[i].get() == e) {
              Drop(i);
              break;
            }
          }
          replan = true;
        }
        continue;
      }
      e->client = std::move(r.client);
      e->format = std::move(r.format);
      e->failed_attempts = r.failed_attempts;
      e->last_failure = r.last_failure;
      e->activated_filetime = deps_.filetime_now != nullptr ? deps_.filetime_now() : 0;
      if (e->activated_filetime == 0) {
        e->activated_filetime = 1; // completed: no longer "still activating" for the planner
      }
      e->phase = Phase::kCommit;
    }
  }

  // Commit checks, then the joins (05 §6.2 step 5).
  for (std::size_t i = 0; i < entries_.size();) {
    Entry &e = *entries_[i];
    if (e.phase != Phase::kCommit) {
      ++i;
      continue;
    }
    const audiopolicy::CommitResult c = commit ? commit(e.act) : audiopolicy::CommitResult{true, {}, {}};
    if (!c.ok) {
      // No INCLUDE on a tree that holds a kept-out process: the stream never joins (its client goes here).
      ++counters_.commit_refusals;
      Event(now_ns, ReportEventCode::kActivationFailed, e.app_id,
            std::string("commit:") + std::string(audiopolicy::ReasonName(c.reason)));
      log::Info("audio", "capture stream refused by the commit check", {{"reason", audiopolicy::ReasonName(c.reason)}});
      Drop(i);
      replan = true;
      continue;
    }
    OpenResult r;
    r.client = std::move(e.client);
    r.format = e.format;
    if (!Join(e, std::move(r))) {
      // No channel, no capture thread or no free mixer slot: like a failed activation, retried after the delays.
      ++counters_.activation_failures;
      Event(now_ns, ReportEventCode::kActivationFailed, e.app_id, "join");
      if (!Backoff(e, now_ns)) {
        Drop(i);
        replan = true;
        continue;
      }
      ++i;
      continue;
    }
    if (e.failed_attempts > 0) {
      // The formats tried before the one that worked (05 §8.2): in the event and the log.
      Event(now_ns, ReportEventCode::kStreamOpened, e.app_id, "fallback:" + std::to_string(e.failed_attempts));
      log::Info("audio", "capture stream opened after failed formats",
                {{"format", e.format},
                 {"failed_attempts", static_cast<int64_t>(e.failed_attempts)},
                 {"hr", static_cast<int64_t>(e.last_failure)}});
    } else {
      Event(now_ns, ReportEventCode::kStreamOpened, e.app_id);
    }
    ++i;
  }

  // Joined and removing streams: exits, failures, covered waits, releases.
  const auto coverer_runs = [&](const Entry &e) {
    for (const uint64_t id : e.coverers) {
      const Entry *o = Find(id);
      if (o != nullptr && o->phase == Phase::kJoined && o->channel && o->channel->state() == StreamState::kRunning) {
        return true;
      }
    }
    return false;
  };
  // 05 §6.5's "process loopback stops working" is a failure of the whole system, not of one app's stream (that one
  // goes through §8.5's reopens to capture_failed). A pump counts for the engine gate when the EXCLUDE stream failed,
  // when every joined process-loopback stream failed together, or when streams of at least 2 targets failed. A
  // repeated failure of a target still in its backoff (failures_ holds it until its stream runs again) never counts.
  bool exclusive_failed = false;
  uint32_t joined_loopback = 0;
  uint32_t failed_loopback = 0;
  std::vector<audiopolicy::ProcessKey> fresh_targets; // include targets whose stream failed fresh in this pump
  for (const auto &e : entries_) {
    joined_loopback += e->phase == Phase::kJoined && e->act.kind != StreamKind::kEndpoint ? 1u : 0u;
  }
  for (std::size_t i = 0; i < entries_.size();) {
    Entry &e = *entries_[i];
    if (e.hold && e.hold->exited() && e.phase != Phase::kRemoving) {
      // The held process exited (05 §5.7): the stream goes at once.
      replan = true;
      if (e.phase == Phase::kJoined) {
        StartRemoval(e);
      } else {
        if (e.phase == Phase::kActivating) {
          deps_.pool->Cancel(e.id);
        }
        Drop(i);
        continue;
      }
    }
    if (e.phase == Phase::kJoined && e.channel->released()) {
      // The mixer let go of it on its own: an engine switch's takeover faded it out (05 §6.5).
      Drop(i);
      replan = true;
      continue;
    }
    if (e.phase == Phase::kJoined) {
      if (e.stream->failed.load(std::memory_order_acquire)) {
        ++counters_.stream_failures;
        if (e.act.kind == StreamKind::kInclude) {
          ++failed_loopback;
          if (!failures_.contains(e.act.target) &&
              std::find(fresh_targets.begin(), fresh_targets.end(), e.act.target) == fresh_targets.end()) {
            fresh_targets.push_back(e.act.target);
          }
        } else if (e.act.kind != StreamKind::kEndpoint) {
          ++failed_loopback;
          exclusive_failed = true;
        }
        Event(now_ns, ReportEventCode::kStreamFailed, e.app_id);
        log::Warn("audio", "capture stream failed",
                  {{"hr", static_cast<int64_t>(e.stream->failure.load(std::memory_order_relaxed))}});
        e.phase = Phase::kRemoving;
        e.reopen = true;
        e.channel->Fail(); // fades out over 10 ms (05 §8.5)
      } else if (e.covered_wait && coverer_runs(e)) {
        StartRemoval(e); // its coverer runs: fade it out now (05 §6.2 step 4)
      } else if (e.channel->state() == StreamState::kRunning) {
        failures_.erase(e.act.target); // running again: the failures in a row start over
      }
    }
    if (e.phase == Phase::kRemoving && e.channel->released()) {
      if (e.reopen_now) {
        // The silent-stream check's reopen (05 §8.5): opens again at once, no failure counted.
        if (deps_.capture != nullptr) {
          deps_.capture->Remove(e.stream);
        }
        counters_.discontinuities += e.stream->discontinuities.load(std::memory_order_relaxed);
        gone_apps_.emplace_back(e.id, e.app_id);
        e.stream.reset();
        e.channel.reset();
        e.reopen_now = false;
        e.phase = Phase::kWaiting;
        e.retry_at = now_ns;
        e.activated_filetime = 0;
        e.format.clear();
        replan = true;
        ++i;
        continue;
      }
      if (e.reopen) {
        // Reopen after the delay (05 §8.5): the stream and channel go, the entry (and its held handle) stays.
        if (deps_.capture != nullptr) {
          deps_.capture->Remove(e.stream);
        }
        counters_.discontinuities += e.stream->discontinuities.load(std::memory_order_relaxed);
        gone_apps_.emplace_back(e.id, e.app_id);
        e.stream.reset();
        e.channel.reset();
        e.reopen = false;
        if (Backoff(e, now_ns)) {
          ++i;
          continue;
        }
      }
      Drop(i);
      replan = true;
      continue;
    }
    ++i;
  }
  const bool all_failed = joined_loopback > 0 && failed_loopback == joined_loopback && !fresh_targets.empty();
  if (exclusive_failed || all_failed || fresh_targets.size() >= 2) {
    ++counters_.loopback_failures;
  }
  SubmitWaiting(now_ns);
  uint32_t joined = 0;
  for (const auto &e : entries_) {
    joined += e->stream ? 1u : 0u;
  }
  counters_.streams = joined;
  return replan || std::exchange(replan_, false);
}

int64_t StreamSet::NextDeadline() const noexcept {
  int64_t d = INT64_MAX;
  for (const auto &e : entries_) {
    if (e->phase == Phase::kWaiting && e->retry_at > now_ns_) {
      d = std::min(d, e->retry_at);
    }
  }
  return d;
}

std::vector<StreamView> StreamSet::Views() const {
  std::vector<StreamView> v;
  v.reserve(entries_.size());
  for (const auto &e : entries_) {
    StreamView s;
    s.kind = e->act.kind;
    s.target = e->act.target;
    s.app_id = e->app_id;
    s.format = e->format;
    s.has_handle = e->has_handle;
    switch (e->phase) {
    case Phase::kWaiting:
    case Phase::kActivating:
    case Phase::kCommit:
      s.stream = SessionStream::kActivating;
      break;
    case Phase::kJoined:
    case Phase::kRemoving: {
      s.tag = e->id;
      const StreamState st = e->channel ? e->channel->state() : StreamState::kReleased;
      s.running = st == StreamState::kRunning;
      s.stream = st == StreamState::kRunning                               ? SessionStream::kRunning
                 : st == StreamState::kIdle || st == StreamState::kPreroll ? SessionStream::kPreroll
                                                                           : SessionStream::kFading;
      if (e->phase == Phase::kRemoving && st != StreamState::kPreroll && st != StreamState::kIdle) {
        s.stream = SessionStream::kFading;
      }
      break;
    }
    }
    v.push_back(std::move(s));
  }
  return v;
}

StreamCounters StreamSet::counters() const {
  StreamCounters c = counters_;
  for (const auto &e : entries_) {
    if (e->stream) {
      c.discontinuities += e->stream->discontinuities.load(std::memory_order_relaxed);
    }
  }
  return c;
}

std::vector<audiomix::ReportEntry> StreamSet::TakeEvents() { return std::exchange(events_, {}); }

std::string StreamSet::AppOfTag(uint64_t tag) const {
  for (const auto &e : entries_) {
    if (e->id == tag) {
      return e->app_id;
    }
  }
  for (auto it = gone_apps_.rbegin(); it != gone_apps_.rend(); ++it) {
    if (it->first == tag) {
      return it->second;
    }
  }
  return {};
}

} // namespace im::audio
