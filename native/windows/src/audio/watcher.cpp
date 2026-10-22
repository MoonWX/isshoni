#include "audio/watcher.h"

#include "audio/win_audio.h"

#include <algorithm>
#include <atomic>
#include <condition_variable>
#include <deque>
#include <limits>
#include <mutex>
#include <optional>
#include <string>
#include <system_error>
#include <unordered_map>
#include <unordered_set>
#include <utility>
#include <vector>

#include "api/guard.h"
#include "audio/levels.h"
#include "audio/probe_flags.h"
#include "audio/process_snapshot.h"
#include "audio/report.h"
#include "audio/sessions.h"
#include "engine/os.h"
#include "im/abi/status.h"
#include "im/abi/struct_size.h"
#include "im/audiopolicy/planner.h"
#include "im/audiopolicy/process_table.h"
#include "im/log/ring.h"

namespace im::audio {
namespace {

constexpr int64_t kMs = 1'000'000;
constexpr int64_t kDebounceNs = 20 * kMs;          // a burst of session events plans once (05 §7.3)
constexpr int64_t kSampleNs = 100 * kMs;           // tick_100ms: levels and playing (05 §12.2)
constexpr int64_t kPollNs = 1000 * kMs;            // tick_1s: the safety net (05 §7.3)
constexpr int64_t kRestartWaitNs = 1000 * kMs;     // the audio service restarts: wait 1 s (05 §7.5)
constexpr int64_t kRestartEveryNs = 5000 * kMs;    // at most once per 5 s
constexpr int64_t kEnumeratorRetryNs = 5000 * kMs; // a device enumerator that couldn't be created
constexpr int64_t kOneShotReuseNs = 1000 * kMs;    // a one-shot report serves the snprintf retry (ListAppsJson)
constexpr uint32_t kMaxEventsPerKind = 8;          // device events of one wake-up that become report events

enum class RequestKind : uint8_t { kRules, kWatch, kUnwatch, kOneShot, kProbeFlags };

// One call into the thread. The caller waits on it (except for kRules); the thread finishes it.
struct Request {
  explicit Request(RequestKind k) : kind(k) {}

  void Finish(im_status st, std::string why = {}) {
    std::lock_guard<std::mutex> l(mu);
    status = st;
    detail = std::move(why);
    done = true;
    cv.notify_all();
  }

  const RequestKind kind;
  audiopolicy::Exclusions rules; // kRules, kWatch
  std::mutex mu;
  std::condition_variable cv;
  bool done = false;
  im_status status = IM_OK;
  std::string detail;
  std::string doc;    // kOneShot
  uint32_t flags = 0; // kProbeFlags
};

} // namespace

struct Watcher::State {
  std::shared_ptr<Signals> signals = std::make_shared<Signals>();
  std::atomic<bool> stop{false};
  std::atomic<bool> watching{false};
  LevelStore levels;

  std::mutex mu; // guards what follows
  std::deque<std::shared_ptr<Request>> requests;
  std::shared_ptr<const std::string> report; // the latest report while watching
  std::shared_ptr<const std::string> oneshot;
  int64_t oneshot_ns = 0;

  std::mutex done_mu;
  std::condition_variable done_cv;
  bool done = false; // the thread has left its loop and released its COM objects
};

namespace {

// The thread's state: everything COM lives here and dies on the thread, before CoUninitialize.
class Loop {
public:
  Loop(std::shared_ptr<Watcher::State> st, HRESULT com) : st_(std::move(st)), com_(com), endpoints_(st_->signals) {}

  void Run() {
    EnsureEnumerator(engine::os::QpcNs());
    while (!st_->stop.load(std::memory_order_acquire)) {
      st_->signals->waiter.WaitUntil(Deadline());
      if (st_->stop.load(std::memory_order_acquire)) {
        break;
      }
      const int64_t now = engine::os::QpcNs();
      HandleRequests(now);
      HandleSignals(now);
      Tick(engine::os::QpcNs());
    }
    FailPending("the audio watcher stopped");
    StopWatching();
    DropEnumerator();
  }

private:
  int64_t Deadline() const {
    int64_t d = engine::os::Waiter::kNoDeadline;
    if (watching_) {
      d = std::min(next_sample_ns_, next_poll_ns_);
      if (st_->signals->sessions.load(std::memory_order_acquire) > 0) {
        const int64_t first = st_->signals->first_session_ns.load(std::memory_order_acquire);
        d = std::min(d, first == 0 ? 0 : first + kDebounceNs);
      }
      if (!en_ && restart_at_ns_ == 0) {
        d = std::min(d, enumerator_retry_ns_); // a pending restart re-creates it at restart_at_ns_
      }
    }
    if (restart_at_ns_ != 0) {
      d = std::min(d, restart_at_ns_);
    }
    return d;
  }

  // ------------------------------------------------------------------------------------------------ requests

  void HandleRequests(int64_t now) {
    for (;;) {
      std::shared_ptr<Request> r;
      {
        std::lock_guard<std::mutex> l(st_->mu);
        if (st_->requests.empty()) {
          return;
        }
        r = std::move(st_->requests.front());
        st_->requests.pop_front();
      }
      api::Guard("audio watcher: request", [&] { Serve(*r, now); });
      if (!r->done) {
        r->Finish(IM_ERR_INTERNAL, "the request failed on the audio watcher thread");
      }
    }
  }

  void Serve(Request &r, int64_t now) {
    switch (r.kind) {
    case RequestKind::kRules:
      ApplyRules(std::move(r.rules), now);
      r.Finish(IM_OK);
      return;
    case RequestKind::kWatch:
      if (FAILED(com_)) {
        r.Finish(IM_ERR_INTERNAL, "COM is not available on the audio watcher thread (" + win::Hr(com_) + ")");
        return;
      }
      if (watching_) {
        ApplyRules(std::move(r.rules), now);
      } else {
        rules_ = std::move(r.rules);
        planner_.SetExclusions(rules_);
        StartWatching(now);
      }
      r.Finish(IM_OK);
      return;
    case RequestKind::kUnwatch:
      StopWatching();
      r.Finish(IM_OK);
      return;
    case RequestKind::kOneShot:
      if (FAILED(com_)) {
        r.Finish(IM_ERR_INTERNAL, "COM is not available on the audio watcher thread (" + win::Hr(com_) + ")");
        return;
      }
      r.doc = OneShot(now);
      r.Finish(IM_OK);
      return;
    case RequestKind::kProbeFlags:
      r.flags = ComputeProbeFlags();
      r.Finish(IM_OK);
      return;
    }
    r.Finish(IM_ERR_INTERNAL, "unknown request");
  }

  void FailPending(const char *why) {
    std::deque<std::shared_ptr<Request>> left;
    {
      std::lock_guard<std::mutex> l(st_->mu);
      left.swap(st_->requests);
    }
    for (auto &r : left) {
      r->Finish(IM_ERR_BAD_STATE, why);
    }
  }

  void ApplyRules(audiopolicy::Exclusions ex, int64_t now) {
    rules_ = std::move(ex);
    planner_.SetExclusions(rules_);
    if (watching_) {
      Event(now, "rules_changed");
      Reconcile(now); // applied at once (05 §4.1: within 50 ms)
    }
  }

  // ----------------------------------------------------------------------------------------- the enumerator

  bool EnsureEnumerator(int64_t now) {
    if (en_) {
      return true;
    }
    if (FAILED(com_) || now < enumerator_retry_ns_) {
      return false;
    }
    const HRESULT hr = CoCreateInstance(__uuidof(MMDeviceEnumerator), nullptr, CLSCTX_ALL,
                                        __uuidof(IMMDeviceEnumerator), en_.PutVoid());
    if (FAILED(hr) || !en_) {
      en_.Reset();
      enumerator_retry_ns_ = now + kEnumeratorRetryNs;
      log::Warn("audio", "no device enumerator", {{"hr", win::Hr(hr)}});
      return false;
    }
    // Device changes (05 §7.5); also what invalidates the probe flags while nobody watches.
    win::ComPtr<IMMNotificationClient> notifier = NewDeviceNotifier(st_->signals);
    if (SUCCEEDED(en_->RegisterEndpointNotificationCallback(notifier.Get()))) {
      notifier_ = std::move(notifier);
    }
    return true;
  }

  void DropEnumerator() {
    if (notifier_ && en_) {
      en_->UnregisterEndpointNotificationCallback(notifier_.Get());
    }
    notifier_.Reset();
    en_.Reset();
  }

  // The audio service went away (AUDCLNT_E_SERVICE_NOT_RUNNING from any call, 05 §7.5): tear every COM object down
  // and re-create them after 1 s, at most once per 5 s.
  void ServiceGone(int64_t now) {
    endpoints_.Clear();
    endpoints_.TakeGone();
    DropEnumerator();
    probe_flags_.reset();
    restart_at_ns_ = std::max(now + kRestartWaitNs, last_restart_ns_ + kRestartEveryNs);
    Event(now, "audiosrv_restart");
    log::Warn("audio", "audio service restart: rebuilding");
  }

  // ------------------------------------------------------------------------------------------------ watching

  void StartWatching(int64_t now) {
    watching_ = true;
    next_sample_ns_ = now + kSampleNs;
    next_poll_ns_ = now + kPollNs;
    last_render_ = std::numeric_limits<uint32_t>::max();
    EnsureEnumerator(now);
    Rebuild(now);
    Reconcile(now);
    PublishLevels(now);
    st_->watching.store(true, std::memory_order_release);
    log::Info("audio", "watching",
              {{"render_endpoints", endpoints_.render_count()}, {"capture_endpoints", endpoints_.capture_count()}});
  }

  void StopWatching() {
    if (!watching_) {
      return;
    }
    watching_ = false;
    st_->watching.store(false, std::memory_order_release);
    endpoints_.Clear();
    endpoints_.TakeGone();
    planner_.Reset();
    snapshotter_.Clear();
    {
      std::lock_guard<std::mutex> l(st_->mu);
      st_->report.reset();
    }
    st_->signals->sessions.store(0, std::memory_order_release);
    st_->signals->first_session_ns.store(0, std::memory_order_release);
    log::Info("audio", "watch stopped");
  }

  // The endpoints with their registrations, then their sessions.
  void Rebuild(int64_t now) {
    if (!en_) {
      endpoints_.Clear();
    } else {
      HRESULT hr = endpoints_.Rebuild(en_.Get(), true);
      if (hr == AUDCLNT_E_SERVICE_NOT_RUNNING) {
        ServiceGone(now);
        return;
      }
      if (FAILED(hr)) {
        log::Warn("audio", "endpoint listing incomplete", {{"hr", win::Hr(hr)}});
      }
      hr = endpoints_.RefreshSessions(true, now);
      if (hr == AUDCLNT_E_SERVICE_NOT_RUNNING) {
        ServiceGone(now);
        return;
      }
    }
    const uint32_t render = endpoints_.render_count();
    if (render == 0 && last_render_ != 0) {
      Event(now, "no_device");
    }
    last_render_ = render;
  }

  void HandleSignals(int64_t now) {
    Signals &s = *st_->signals;
    if (const uint32_t devices = s.devices.exchange(0, std::memory_order_acq_rel); devices > 0) {
      const uint32_t added = s.added.exchange(0, std::memory_order_acq_rel);
      const uint32_t removed = s.removed.exchange(0, std::memory_order_acq_rel);
      const uint32_t default_render = s.default_render.exchange(0, std::memory_order_acq_rel);
      probe_flags_.reset(); // cached until the next device event (05 §11.1)
      if (watching_) {
        counters_.device_changes += devices;
        for (uint32_t i = 0; i < std::min<uint32_t>(added, kMaxEventsPerKind); ++i) {
          Event(now, "device_added");
        }
        for (uint32_t i = 0; i < std::min<uint32_t>(removed, kMaxEventsPerKind); ++i) {
          Event(now, "device_removed");
        }
        for (uint32_t i = 0; i < std::min<uint32_t>(default_render, kMaxEventsPerKind); ++i) {
          Event(now, "default_device_changed");
        }
        log::Info("audio", "audio devices changed",
                  {{"added", added}, {"removed", removed}, {"default", default_render}});
        Rebuild(now);
        Reconcile(now);
      }
    }
    if (!watching_) {
      s.sessions.store(0, std::memory_order_release);
      s.first_session_ns.store(0, std::memory_order_release);
      return;
    }
    if (s.sessions.load(std::memory_order_acquire) == 0) {
      return;
    }
    const int64_t first = s.first_session_ns.load(std::memory_order_acquire);
    if (first != 0 && now - first < kDebounceNs) {
      return; // more may follow: Deadline() wakes the loop when the 20 ms are up
    }
    // Reset before the refresh: an event during it starts the next round, so none is lost.
    s.first_session_ns.store(0, std::memory_order_release);
    s.sessions.store(0, std::memory_order_release);
    if (endpoints_.RefreshSessions(true, now) == AUDCLNT_E_SERVICE_NOT_RUNNING) {
      ServiceGone(now);
    }
    Reconcile(now);
  }

  void Tick(int64_t now) {
    if (restart_at_ns_ != 0 && now >= restart_at_ns_) {
      restart_at_ns_ = 0;
      last_restart_ns_ = now;
      if (EnsureEnumerator(now) && watching_) {
        Rebuild(now);
        Reconcile(now);
      }
    }
    if (!watching_) {
      return;
    }
    if (!en_ && restart_at_ns_ == 0 && now >= enumerator_retry_ns_ && EnsureEnumerator(now)) {
      Rebuild(now);
      Reconcile(now);
    }
    if (now >= next_poll_ns_) {
      next_poll_ns_ = now + kPollNs;
      if (endpoints_.RefreshSessions(true, now) == AUDCLNT_E_SERVICE_NOT_RUNNING) {
        ServiceGone(now);
      }
      Reconcile(now); // also refreshes the process table and re-checks every target (05 §7.3)
    }
    if (now >= next_sample_ns_) {
      next_sample_ns_ = now + kSampleNs;
      if (endpoints_.Sample(now)) {
        Reconcile(now); // apps[].playing changed
      }
      PublishLevels(now);
    }
  }

  // --------------------------------------------------------------------------------------------- reconcile

  // One plan over a fresh process table and eps' sessions, as a report (05 §7.3, §12.1). With track, the sessions
  // keep their app ids (the levels' rows) and announce themselves, and the report gets the watcher's events.
  Report Plan(EndpointSet &eps, audiopolicy::Planner &planner, int64_t now, bool track) {
    std::vector<audiopolicy::ProcessEntry> entries;
    snapshotter_.Take(entries);
    std::vector<TrackedSession *> tracked = eps.sessions();
    std::vector<audiopolicy::Session> sessions;
    sessions.reserve(tracked.size());
    for (const TrackedSession *t : tracked) {
      audiopolicy::Session s;
      s.pid = t->pid;
      s.flow = t->flow;
      s.state = t->state == AudioSessionStateActive ? audiopolicy::SessionState::kActive
                                                    : audiopolicy::SessionState::kInactive;
      s.multi_process = t->multi_process;
      s.playing = t->playing;
      s.last_active_ns = t->last_active_ns;
      sessions.push_back(s);
    }
    DescribeSessionLineage(entries, tracked);
    const audiopolicy::ProcessTable table(std::move(entries));
    audiopolicy::PlanInput in;
    in.now_ns = now;
    in.mode = IM_AUDIO_MODE_OFF; // no capture runs (README WA45 adds it): the include set's prediction, engine none
    in.process_loopback = true;
    in.sessions = sessions;
    const audiopolicy::Plan plan = planner.Reconcile(table, in);
    if (!plan.violations.empty()) {
      log::Error("audio", "audio policy invariant broken", {{"violations", plan.violations.size()}});
    }

    Report r;
    const uint32_t render = eps.render_count();
    const uint32_t capture = eps.capture_count();
    r.engine = plan.engine;
    r.state = render > 0 ? "ok" : "no-device";
    r.rebuilding = restart_at_ns_ != 0;
    r.probe.result = render > 0 ? "pending" : "skipped"; // the startup probe runs with a share (05 §11.2)
    r.probe.os_build = win::OsBuild();
    r.device = ReportDevice{render > 0, render, capture};
    r.apps = plan.apps;
    for (const audiopolicy::PlanWarning &w : plan.warnings) {
      r.warnings.push_back(ReportWarning{std::string(w.code), w.app});
    }
    r.sessions.reserve(tracked.size());
    for (std::size_t k = 0; k < tracked.size() && k < plan.sessions.size(); ++k) {
      const audiopolicy::SessionResult &sr = plan.sessions[k];
      ReportSession s;
      s.pid = tracked[k]->pid;
      if (sr.process >= 0) {
        const audiopolicy::Process &p = table.at(static_cast<std::size_t>(sr.process));
        s.created = p.key.created;
        s.exe = p.exe;
      }
      s.app_id = sr.app_id;
      s.flow = tracked[k]->flow;
      s.state = sessions[k].state;
      s.multi_process = tracked[k]->multi_process;
      s.verdict = sr.verdict;
      s.reason = sr.reason;
      s.note = sr.note;
      r.sessions.push_back(std::move(s));
      if (track) {
        tracked[k]->app_id = sr.app_id;
        if (!tracked[k]->announced && sr.verdict != audiopolicy::Verdict::kPending) {
          tracked[k]->announced = true;
          Event(now, "session_created", sr.app_id);
        }
      }
    }
    if (track) {
      counters_.pid_reuse_blocks += plan.pid_reuse_blocks;
      for (std::string &app : eps.TakeGone()) {
        Event(now, "session_expired", std::move(app));
      }
    }
    r.qpc_ns = now;
    r.counters = counters_;
    r.events.assign(events_.begin(), events_.end());
    return r;
  }

  // FileDescription for the processes a row can be named after: those with a session and their parent chains
  // (05 §5.1; descriptions are cached per image path).
  void DescribeSessionLineage(std::vector<audiopolicy::ProcessEntry> &entries,
                              const std::vector<TrackedSession *> &tracked) {
    std::unordered_map<uint32_t, std::size_t> by_pid;
    by_pid.reserve(entries.size());
    for (std::size_t i = 0; i < entries.size(); ++i) {
      by_pid.emplace(entries[i].pid, i);
    }
    std::unordered_set<std::size_t> picked;
    std::vector<std::size_t> want;
    for (const TrackedSession *t : tracked) {
      uint32_t pid = t->pid;
      for (int step = 0; step < audiopolicy::kMaxWalk && pid != 0; ++step) {
        const auto it = by_pid.find(pid);
        if (it == by_pid.end() || !picked.insert(it->second).second) {
          break;
        }
        want.push_back(it->second);
        pid = entries[it->second].parent_pid;
      }
    }
    snapshotter_.Describe(entries, want);
  }

  void Reconcile(int64_t now) {
    if (!watching_) {
      return;
    }
    Report r = Plan(endpoints_, planner_, now, true);
    std::string visible = VisibleState(r);
    if (visible != visible_) {
      ++seq_; // anything visible changed (05 §12.1)
      visible_ = std::move(visible);
    }
    r.seq = seq_;
    auto doc = std::make_shared<const std::string>(WriteReportJson(r));
    std::lock_guard<std::mutex> l(st_->mu);
    st_->report = std::move(doc);
  }

  std::string OneShot(int64_t now) {
    if (watching_) {
      std::lock_guard<std::mutex> l(st_->mu);
      if (st_->report) {
        return *st_->report;
      }
    }
    // A snapshot without registrations, planned with the current rules by a planner of its own (no mic stickiness
    // or lineage to remember), state "stopped" (05 §12.1).
    EndpointSet eps(st_->signals);
    if (EnsureEnumerator(now)) {
      if (eps.Rebuild(en_.Get(), false) == AUDCLNT_E_SERVICE_NOT_RUNNING ||
          eps.RefreshSessions(false, now) == AUDCLNT_E_SERVICE_NOT_RUNNING) {
        eps.Clear();
        ServiceGone(now);
      }
    }
    audiopolicy::Planner planner;
    planner.SetExclusions(rules_);
    Report r = Plan(eps, planner, now, false);
    eps.Clear();
    r.state = "stopped";
    r.seq = seq_;
    return WriteReportJson(r);
  }

  void PublishLevels(int64_t now) {
    std::vector<AppActivity> apps;
    for (const TrackedSession *t : endpoints_.sessions()) {
      if (t->flow == audiopolicy::Flow::kRender && !t->app_id.empty()) {
        apps.push_back(AppActivity{t->app_id, t->peak_db, t->last_active_ns, false});
      }
    }
    // Not live: no mix (05 §4.1: -120 when not live) and every entry is a session's activity (source 1).
    st_->levels.Publish(MakeLevels(seq_, now, kSilenceDb, kSilenceDb, apps));
  }

  // ------------------------------------------------------------------------------------------------- probe

  uint32_t ComputeProbeFlags() {
    if (probe_flags_) {
      return *probe_flags_;
    }
    if (!EnsureEnumerator(engine::os::QpcNs())) {
      return 0; // not cached: the next probe tries again
    }
    uint32_t f = 0;
    const uint32_t render = ActiveEndpoints(en_.Get(), eRender);
    const uint32_t capture = ActiveEndpoints(en_.Get(), eCapture);
    if (capture > 0) {
      f |= IM_AUDIO_MIC_DETECTION;
    }
    // Without an active render endpoint nothing plays and nothing can be captured: backend none (05 §11.1).
    if (render > 0) {
      if (watching_ ? endpoints_.all_render_registered() : SessionEventsWork(en_.Get())) {
        f |= IM_AUDIO_SESSION_EVENTS;
      }
      if (EndpointLoopbackWorks(en_.Get())) {
        f |= IM_AUDIO_ENDPOINT_LOOPBACK;
      }
      if (ProcessLoopbackWorks(static_cast<uint32_t>(GetCurrentProcessId()), kProbeActivationTimeoutMs)) {
        f |= IM_AUDIO_PROCESS_LOOPBACK;
      }
    }
    probe_flags_ = f;
    log::Info("audio", "audio probe flags",
              {{"flags", f}, {"render_endpoints", render}, {"capture_endpoints", capture}});
    return f;
  }

  // ------------------------------------------------------------------------------------------------ events

  void Event(int64_t now, std::string_view code, std::string app = {}) {
    events_.push_back(ReportEvent{now, std::string(code), std::move(app), {}});
    while (events_.size() > kReportMaxEvents) {
      events_.pop_front();
    }
  }

  std::shared_ptr<Watcher::State> st_;
  const HRESULT com_;
  win::ComPtr<IMMDeviceEnumerator> en_;
  win::ComPtr<IMMNotificationClient> notifier_; // registered while non-null
  int64_t enumerator_retry_ns_ = 0;
  EndpointSet endpoints_;
  ProcessSnapshotter snapshotter_;
  audiopolicy::Planner planner_;
  audiopolicy::Exclusions rules_;
  bool watching_ = false;
  std::optional<uint32_t> probe_flags_;
  int64_t restart_at_ns_ = 0; // a pending rebuild after the audio service went away
  int64_t last_restart_ns_ = std::numeric_limits<int64_t>::min() / 2;
  uint32_t last_render_ = std::numeric_limits<uint32_t>::max();
  int64_t next_sample_ns_ = 0;
  int64_t next_poll_ns_ = 0;
  uint32_t seq_ = 0;
  std::string visible_;
  ReportCounters counters_;
  std::deque<ReportEvent> events_;
};

void RunThread(const std::shared_ptr<Watcher::State> &st) noexcept {
  const uintptr_t token = engine::os::EnterThread("isshoni audio watcher", engine::os::ThreadClass::kNormal);
  api::Guard("audio watcher", [&] {
    const win::ComApartment com;
    if (FAILED(com.hr())) {
      log::Error("audio", "audio watcher without COM", {{"hr", win::Hr(com.hr())}});
    }
    Loop loop(st, com.hr());
    loop.Run();
  });
  engine::os::LeaveThread(token);
  {
    std::lock_guard<std::mutex> l(st->done_mu);
    st->done = true;
  }
  st->done_cv.notify_all();
}

// Waits for a request the thread finishes. set_detail: a failure sets the calling thread's im_last_error.
im_status Await(Request &r, const char *fn, bool set_detail) {
  std::unique_lock<std::mutex> l(r.mu);
  if (!r.cv.wait_for(l, kWatcherWait, [&] { return r.done; })) {
    return set_detail ? abi::Failf(IM_ERR_TIMEOUT, {}, "%s: the audio watcher did not answer within 3 s", fn)
                      : IM_ERR_TIMEOUT;
  }
  if (r.status != IM_OK && set_detail) {
    return abi::Failf(r.status, {}, "%s: %s", fn, r.detail.c_str());
  }
  return r.status;
}

} // namespace

Watcher::Watcher() : st_(std::make_shared<State>()) {}

Watcher::~Watcher() { Stop(); }

im_status Watcher::Start() {
  if (thread_.joinable()) {
    return IM_OK;
  }
  try {
    thread_ = std::thread([st = st_] { RunThread(st); });
  } catch (const std::system_error &e) {
    return abi::Failf(IM_ERR_INTERNAL, {}, "audio watcher: the thread did not start: %s", e.what());
  }
  return IM_OK;
}

void Watcher::Stop() noexcept {
  if (!thread_.joinable()) {
    return;
  }
  st_->stop.store(true, std::memory_order_release);
  st_->signals->waiter.Wake();
  bool done = false;
  {
    std::unique_lock<std::mutex> l(st_->done_mu);
    done = st_->done_cv.wait_for(l, kWatcherJoin, [&] { return st_->done; });
  }
  try {
    if (done) {
      thread_.join();
    } else {
      // Stuck in a driver call: abandoned, never killed (05 §10.5). It owns a reference to its state.
      thread_.detach();
      log::Error("audio", "audio watcher thread abandoned after 2 s");
    }
  } catch (const std::system_error &) {
    log::Error("audio", "audio watcher thread: join failed");
  }
  std::deque<std::shared_ptr<Request>> left;
  {
    std::lock_guard<std::mutex> l(st_->mu);
    left.swap(st_->requests);
  }
  for (auto &r : left) {
    r->Finish(IM_ERR_BAD_STATE, "the audio watcher stopped");
  }
}

namespace {

// Queues r; false when the thread doesn't run (any more). A request that changes what a report says (rules, watch,
// unwatch) also drops the kept one-shot report.
bool Enqueue(Watcher::State &st, std::shared_ptr<Request> r) {
  {
    std::lock_guard<std::mutex> l(st.mu);
    if (st.stop.load(std::memory_order_acquire)) {
      return false;
    }
    if (r->kind != RequestKind::kOneShot && r->kind != RequestKind::kProbeFlags) {
      st.oneshot.reset();
    }
    st.requests.push_back(std::move(r));
  }
  st.signals->waiter.Wake();
  return true;
}

} // namespace

void Watcher::SetRules(audiopolicy::Exclusions ex) {
  auto r = std::make_shared<Request>(RequestKind::kRules);
  r->rules = std::move(ex);
  if (thread_.joinable()) {
    Enqueue(*st_, std::move(r));
  }
}

im_status Watcher::Watch(audiopolicy::Exclusions ex) {
  auto r = std::make_shared<Request>(RequestKind::kWatch);
  r->rules = std::move(ex);
  if (!thread_.joinable() || !Enqueue(*st_, r)) {
    return abi::Fail(IM_ERR_BAD_STATE, {}, "im_audio_watch: the audio watcher is not running");
  }
  return Await(*r, "im_audio_watch", true);
}

im_status Watcher::Unwatch() {
  auto r = std::make_shared<Request>(RequestKind::kUnwatch);
  if (!thread_.joinable() || !Enqueue(*st_, r)) {
    return IM_OK; // nothing watches
  }
  return Await(*r, "im_audio_unwatch", true);
}

bool Watcher::watching() const noexcept { return st_->watching.load(std::memory_order_acquire); }

int32_t Watcher::ListAppsJson(char *buf, int32_t cap) {
  std::shared_ptr<const std::string> doc;
  const int64_t now = engine::os::QpcNs();
  {
    std::lock_guard<std::mutex> l(st_->mu);
    if (st_->watching.load(std::memory_order_acquire) && st_->report) {
      doc = st_->report;
    } else if (st_->oneshot && now - st_->oneshot_ns < kOneShotReuseNs) {
      doc = st_->oneshot;
    }
  }
  if (!doc) {
    auto r = std::make_shared<Request>(RequestKind::kOneShot);
    if (!thread_.joinable() || !Enqueue(*st_, r)) {
      return abi::Fail(IM_ERR_BAD_STATE, {}, "im_list_audio_apps: the audio watcher is not running");
    }
    if (const im_status st = Await(*r, "im_list_audio_apps", true); st != IM_OK) {
      return st;
    }
    doc = std::make_shared<const std::string>(std::move(r->doc));
    std::lock_guard<std::mutex> l(st_->mu);
    st_->oneshot = doc;
    st_->oneshot_ns = engine::os::QpcNs();
  }
  const int32_t n = abi::CopyOut(*doc, buf, cap);
  if (n < 0) {
    return abi::Fail(n, {}, "im_list_audio_apps: the report does not fit the snprintf rule");
  }
  return n;
}

im_status Watcher::LevelsGet(im_audio_levels *out) {
  if (!st_->watching.load(std::memory_order_acquire)) {
    return abi::Fail(IM_ERR_BAD_STATE, abi::token::kNotWatching, "im_audio_levels_get: neither watching nor live");
  }
  abi::WriteOut(out, st_->levels.Read());
  return IM_OK;
}

uint32_t Watcher::ProbeFlags() {
  auto r = std::make_shared<Request>(RequestKind::kProbeFlags);
  if (!thread_.joinable() || !Enqueue(*st_, r)) {
    return 0;
  }
  if (Await(*r, "audio probe flags", false) != IM_OK) {
    log::Warn("audio", "audio probe flags: the watcher did not answer");
    return 0;
  }
  return r->flags;
}

} // namespace im::audio
