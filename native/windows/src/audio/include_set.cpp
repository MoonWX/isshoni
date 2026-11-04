#include "audio/include_set.h"

#include <algorithm>
#include <climits>
#include <string>
#include <utility>

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
  events_.push_back(audiomix::ReportEvent{t, code, app, std::move(detail)});
}

void StreamSet::Apply(const audiopolicy::Plan &plan, const AppOf &app_of, const Below &below, int64_t now_ns) {
  now_ns_ = now_ns;
  if (engine_ == nullptr) {
    return;
  }
  for (auto &e : entries_) {
    e->covered_wait = false; // each plan names the covered streams that still wait
    e->coverers.clear();
  }
  // Removals first: they index into the input of this plan.
  std::vector<uint64_t> drop;
  std::vector<uint64_t> covered;
  for (const audiopolicy::Removal &r : plan.remove) {
    if (r.stream >= input_ids_.size()) {
      continue;
    }
    Entry *e = Find(input_ids_[r.stream]);
    if (e == nullptr || e->phase == Phase::kRemoving) {
      continue;
    }
    if (r.reason == RemovalReason::kCovered && e->phase == Phase::kJoined) {
      // Fades once its coverer runs (Pump), so the sound has no gap; one not mixed yet goes at once. kEngine has no
      // other engine to wait for in this slice (the fast path and the endpoint engine are README WA53's): with sound
      // off it fades at once (05 §6.5).
      e->covered_wait = true;
      covered.push_back(e->id);
      continue;
    }
    if (e->phase == Phase::kJoined) {
      StartRemoval(*e);
    } else {
      drop.push_back(e->id); // still activating or waiting for its commit: cancelled, nothing was mixed
    }
  }
  for (const uint64_t id : drop) {
    for (std::size_t i = 0; i < entries_.size(); ++i) {
      if (entries_[i]->id == id) {
        if (entries_[i]->phase == Phase::kActivating) {
          deps_.pool->Cancel(id);
        }
        Drop(i);
        break;
      }
    }
  }

  std::vector<audiopolicy::ProcessKey> mismatched;
  for (const audiopolicy::Activation &a : plan.activate) {
    if (a.kind != StreamKind::kInclude) {
      // The fast path's EXCLUDE stream and the endpoint stream (README WA53). The watcher plans with the fast path
      // off and as if process loopback works, so this doesn't happen yet.
      log::Warn("audio", "exclusive capture stream not opened: not implemented yet (README WA53)");
      continue;
    }
    bool held = false;
    for (const auto &e : entries_) {
      held = held || (e->act.kind == a.kind && e->act.target == a.target);
    }
    if (held) {
      continue; // already opening it
    }
    auto e = std::make_unique<Entry>();
    e->id = next_id_++;
    e->act = a;
    e->app_id = app_of ? app_of(a.target) : std::string();
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
      log::Info("audio", "include target without a process handle: checked by snapshot");
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
  SubmitWaiting(now_ns);
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
  if (f.count >= kFailuresBeforeGivingUp) {
    given_up_.push_back(e.act.target); // capture_failed until the next device or session event (05 §8.5)
    failures_.erase(e.act.target);
    return false;
  }
  e.phase = Phase::kWaiting;
  e.retry_at = now_ns + kReopenDelaysNs[f.count - 1]; // count < kFailuresBeforeGivingUp: a delay for each reopen
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
  audiomix::ChannelOptions o;
  o.ring = &stream->ring;
  o.tag = e.id;
  o.group = audiomix::kGroupInclude;
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
    if (e.phase == Phase::kJoined) {
      if (e.stream->failed.load(std::memory_order_acquire)) {
        ++counters_.stream_failures;
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

std::vector<audiomix::ReportEvent> StreamSet::TakeEvents() { return std::exchange(events_, {}); }

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
