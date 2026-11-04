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
#include "audio/activator.h"
#include "audio/capture_thread.h"
#include "audio/include_set.h"
#include "audio/levels.h"
#include "audio/live_mix.h"
#include "audio/probe_flags.h"
#include "audio/process_hold.h"
#include "audio/process_snapshot.h"
#include "audio/report.h"
#include "audio/sessions.h"
#include "audio/wasapi_capture.h"
#include "engine/os.h"
#include "im/abi/status.h"
#include "im/abi/struct_size.h"
#include "im/audiomix/mix_engine.h"
#include "im/audiomix/report.h"
#include "im/audiopolicy/planner.h"
#include "im/audiopolicy/process_table.h"
#include "im/audiopolicy/text.h"
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
constexpr int64_t kLivePumpNs = 20 * kMs;          // while live: the include set's streams move on at least this often
constexpr int64_t kThreadsIdleNs = 5000 * kMs;     // the capture threads stop 5 s after the last share (05 §10.5)

using audiomix::ReportEventCode;

enum class RequestKind : uint8_t {
  kRules,
  kWatch,
  kUnwatch,
  kOneShot,
  kProbeFlags,
  kAttach,    // a share's mixer started (README WA45)
  kDetach,    // its mixer thread stopped
  kLiveMode,  // im_audio_set_mode while live
  kLiveMuted, // im_audio_set_muted while live
};

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
  std::shared_ptr<const std::string> doc; // kOneShot
  uint32_t flags = 0;                     // kProbeFlags
  std::shared_ptr<LiveMix> mix;           // kAttach, kDetach
  uint32_t mode = 0;                      // kLiveMode
  bool muted = false;                     // kLiveMuted
};

// The rules before the first im_set_exclusions or im_audio_watch: none of Go's, only the implicit instance rule on this
// process that the DLL always adds (05 §4.3), so even a one-shot report before them shows isshoni's own sessions as
// kept out.
audiopolicy::Exclusions ImplicitRules() {
  im_exclusions e{};
  e.struct_size = sizeof e;
  e.flags = audiopolicy::Exclusions{}.flags;
  audiopolicy::Exclusions x;
  if (audiopolicy::ParseExclusions(&e, static_cast<uint32_t>(GetCurrentProcessId()), x) != IM_OK) {
    log::Error("audio", "audio watcher: the implicit rule was refused");
  }
  return x;
}

// The rules as the planner gets them: without the fast path, which is README WA53's (05 §6.4) along with the endpoint
// engine, so the plans of this slice use the include set alone.
audiopolicy::Exclusions PlannerRules(audiopolicy::Exclusions ex) {
  ex.flags &= ~static_cast<uint32_t>(IM_EXCLF_FAST_PATH);
  return ex;
}

uint64_t FiletimeNow() {
  FILETIME ft{};
  GetSystemTimeAsFileTime(&ft);
  return win::FiletimeValue(ft);
}

void WakeSignals(void *ctx) { static_cast<Signals *>(ctx)->waiter.Wake(); }

// The mixer's events as the report spells them (05 §12.1). kTailFade arrives at most once per second per channel
// (the mixer's limit, 05 §9.5), so the report's tail_fade has the same limit.
ReportEventCode MixEventToReport(audiomix::MixEventCode c) {
  switch (c) {
  case audiomix::MixEventCode::kTailFade:
    return ReportEventCode::kTailFade;
  case audiomix::MixEventCode::kStreamRunning:
    return ReportEventCode::kStreamRunning;
  case audiomix::MixEventCode::kStreamRemoved:
    return ReportEventCode::kStreamRemoved;
  case audiomix::MixEventCode::kUnderrun:
    return ReportEventCode::kUnderrun;
  case audiomix::MixEventCode::kTrim:
    return ReportEventCode::kTrim;
  case audiomix::MixEventCode::kMixerResync:
    return ReportEventCode::kMixerResync;
  case audiomix::MixEventCode::kReanchor:
    return ReportEventCode::kReanchor;
  case audiomix::MixEventCode::kFastpathLateCut:
    break;
  }
  return ReportEventCode::kFastpathLateCut;
}

} // namespace

struct Watcher::State {
  std::shared_ptr<Signals> signals = std::make_shared<Signals>();
  std::atomic<bool> stop{false};
  std::atomic<bool> watching{false};
  LevelStore levels;

  std::mutex mu; // guards what follows
  std::deque<std::shared_ptr<Request>> requests;
  std::shared_ptr<const std::string> report;  // the latest report while watching
  std::shared_ptr<const std::string> oneshot; // the last one-shot report, set on the thread only
  int64_t oneshot_ns = 0;

  std::mutex done_mu;
  std::condition_variable done_cv;
  bool done = false; // the thread has left its loop and released its COM objects
};

namespace {

// The thread's state: everything COM lives here and dies on the thread, before CoUninitialize.
class Loop {
public:
  Loop(std::shared_ptr<Watcher::State> st, HRESULT com)
      : st_(std::move(st)), com_(com), endpoints_(st_->signals), rules_(ImplicitRules()),
        pool_(NewWasapiOpener(), ActivatorHooks(st_->signals.get())),
        gate_(NewWindowsProcessGate(&WakeSignals, st_->signals.get())),
        set_(StreamSetDeps{&pool_, &capture_, gate_.get(), &FiletimeNow}) {
    planner_.SetExclusions(PlannerRules(rules_));
  }

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
    if (live_) {
      // Shutdown with a share still live (04 stops its sessions first, so this is a backstop): the mixer thread goes
      // first, then the streams it read.
      live_->mixer->Stop();
      DetachLive(std::shared_ptr<LiveMix>(live_), engine::os::QpcNs());
    }
    StopWatching();
    StopCaptureThreads();
    DropEnumerator();
  }

private:
  static ActivatorOptions ActivatorHooks(Signals *signals) {
    ActivatorOptions o;
    o.thread_hook = &ActivatorThreadHook;
    // The wake-up of the watcher. The Signals belong to the watcher's shared state; an activator thread abandoned in a
    // driver call at a stop (2 s) that completes after the watcher is gone would touch them, which needs a driver to
    // hang that long and then return.
    o.on_result = &WakeSignals;
    o.result_ctx = signals;
    return o;
  }

  int64_t Deadline() const {
    int64_t d = engine::os::Waiter::kNoDeadline;
    if (live_) {
      d = std::min(d, engine::os::QpcNs() + kLivePumpNs);
      d = std::min(d, set_.NextDeadline());
    } else if (threads_idle_since_ != 0) {
      d = std::min(d, threads_idle_since_ + kThreadsIdleNs);
    }
    if (watching_) {
      d = std::min({d, next_sample_ns_, next_poll_ns_}); // keeps the live pump's and the reopen delays' deadlines
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
      DropOneShot();
      r.Finish(IM_OK);
      return;
    case RequestKind::kWatch:
      if (FAILED(com_)) {
        r.Finish(IM_ERR_INTERNAL, "COM is not available on the audio watcher thread (" + win::Hr(com_) + ")");
        return;
      }
      user_watch_ = true;
      if (watching_) {
        ApplyRules(std::move(r.rules), now);
      } else {
        rules_ = std::move(r.rules);
        planner_.SetExclusions(PlannerRules(rules_));
        StartWatching(now);
      }
      DropOneShot();
      r.Finish(IM_OK);
      return;
    case RequestKind::kUnwatch:
      user_watch_ = false;
      if (!live_) {
        StopWatching(); // a live share keeps watching until it stops
      }
      DropOneShot();
      r.Finish(IM_OK);
      return;
    case RequestKind::kAttach:
      if (FAILED(com_)) {
        r.Finish(IM_ERR_INTERNAL, "COM is not available on the audio watcher thread (" + win::Hr(com_) + ")");
        return;
      }
      AttachLive(std::move(r.mix), now);
      DropOneShot();
      r.Finish(IM_OK);
      return;
    case RequestKind::kDetach:
      if (r.mix && r.mix == live_) {
        DetachLive(r.mix, now);
      }
      DropOneShot();
      r.Finish(IM_OK);
      return;
    case RequestKind::kLiveMode:
      if (live_ && r.mode != live_mode_) {
        live_mode_ = r.mode;
        Event(now, ReportEventCode::kModeChanged, {}, live_mode_ == IM_AUDIO_MODE_OFF ? "off" : "exclude-apps");
        if (live_mode_ != IM_AUDIO_MODE_OFF) {
          planner_.Restart(); // sound back on: plan and open the streams as at start (05 §6.5)
        }
        Reconcile(now); // off: engine none, the streams fade out at once (05 §6.5)
      }
      r.Finish(IM_OK);
      return;
    case RequestKind::kLiveMuted:
      if (live_) {
        Event(now, r.muted ? ReportEventCode::kMuted : ReportEventCode::kUnmuted);
        Reconcile(now);
      }
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

  // A request that changes what a report says (rules, watch, unwatch) drops the kept one-shot report once it applied.
  // The queue is FIFO, so a one-shot served before the request can't outlive it; Enqueue drops it as well, for a
  // caller that reads the cache before the request is served.
  void DropOneShot() {
    std::lock_guard<std::mutex> l(st_->mu);
    st_->oneshot.reset();
  }

  void ApplyRules(audiopolicy::Exclusions ex, int64_t now) {
    rules_ = std::move(ex);
    planner_.SetExclusions(PlannerRules(rules_));
    if (watching_) {
      Event(now, ReportEventCode::kRulesChanged);
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
    Event(now, ReportEventCode::kAudiosrvRestart);
    log::Warn("audio", "audio service restart: rebuilding");
  }

  // ---------------------------------------------------------------------------------------------- live share

  // A share's mixer started (05 §10.5): watch if nobody does, start the capture threads, plan as at an engine start.
  void AttachLive(std::shared_ptr<LiveMix> mix, int64_t now) {
    if (!mix) {
      return;
    }
    if (live_) {
      live_->mixer->Stop(); // one session with audio per process (04 §6.4): the old one can't still be running
      DetachLive(std::shared_ptr<LiveMix>(live_), now);
    }
    live_ = std::move(mix);
    live_mode_ = IM_AUDIO_MODE_EXCLUDE_APPS;
    threads_idle_since_ = 0;
    if (pool_.Start() != IM_OK || capture_.Start() != IM_OK) {
      log::Error("audio", "audio capture threads did not start: the share's audio stays silent");
    }
    set_.Attach(&live_->engine());
    cpu_mark_ns_ = now;
    cpu_mark_busy_ = BusyNs();
    cpu_ms_per_s_ = 0;
    views_.clear();
    planner_.Restart(); // an engine start: the engine's state starts over (05 §6.4)
    if (watching_) {
      Reconcile(now);
    } else {
      StartWatching(now);
    }
    log::Info("audio", "share audio live");
  }

  // The share's mixer thread stopped: every channel and stream goes; the capture threads stop 5 s later.
  void DetachLive(const std::shared_ptr<LiveMix> &mix, int64_t now) {
    DrainMixEvents();
    const audiomix::MixCounters mc = mix->engine().counters();
    mixer_base_.underruns += mc.underruns;
    mixer_base_.trims += mc.trims;
    mixer_base_.tail_fades += mc.tail_fades;
    mixer_base_.overflows += mc.overflows;
    mixer_base_.mixer_resyncs += mc.mixer_resyncs;
    mixer_base_.fastpath_late_cuts += mc.fastpath_late_cuts;
    mixer_base_.max_resample_ppm = std::max(mixer_base_.max_resample_ppm, mc.max_resample_ppm);
    if (mc.tail_fades > 0) {
      log::Info("audio", "tail fades in the share", {{"count", mc.tail_fades}});
    }
    set_.Detach();
    live_.reset();
    threads_idle_since_ = now;
    if (!user_watch_) {
      StopWatching();
    } else {
      Reconcile(now);
    }
    log::Info("audio", "share audio stopped");
  }

  void StopCaptureThreads() {
    pool_.Stop();
    capture_.Stop();
    threads_idle_since_ = 0;
  }

  int64_t BusyNs() const { return (live_ ? live_->mixer->busy_ns() : 0) + capture_.busy_ns(); }

  // While live: the mixer's events into the report, the include set's streams moved on, and a new report when a
  // stream changed state or a new plan when the streams ask for one.
  void PumpLive(int64_t now) {
    if (!live_) {
      return;
    }
    DrainMixEvents();
    std::optional<audiopolicy::ProcessTable> fresh;
    const bool replan = set_.Pump(now, [&](const audiopolicy::Activation &a) {
      if (!fresh) {
        // The commit check's fresh snapshot (05 §6.2 step 5), once per pump.
        std::vector<audiopolicy::ProcessEntry> entries;
        snapshotter_.Take(entries);
        fresh.emplace(std::move(entries));
      }
      return planner_.CommitCheck(*fresh, a);
    });
    for (ReportEvent &e : set_.TakeEvents()) {
      PushEvent(std::move(e));
    }
    std::string views;
    for (const StreamView &v : set_.Views()) {
      views += std::to_string(v.target.pid) + ':' + audiomix::ReportName(v.stream) + ';';
    }
    if (replan || views != views_) {
      views_ = std::move(views);
      Reconcile(now); // a stream left, or sessions[].stream changed
    }
  }

  // The mixer's events into the report's, named by the include set's app ids.
  void DrainMixEvents() {
    if (!live_) {
      return;
    }
    audiomix::MixEvent ev[64];
    uint64_t tails = 0;
    for (;;) {
      const uint32_t n = live_->engine().DrainEvents(ev, 64);
      for (uint32_t i = 0; i < n; ++i) {
        if (ev[i].code == audiomix::MixEventCode::kTailFade) {
          ++tails; // a sound stopped into digital silence and was ramped out (README §3 item 53)
        }
        Event(ev[i].t_ns, MixEventToReport(ev[i].code), ev[i].tag != 0 ? set_.AppOfTag(ev[i].tag) : std::string());
      }
      if (n < 64) {
        break;
      }
    }
    if (tails > 0) {
      log::Debug("audio", "sound stopped: tail fade", {{"count", tails}});
    }
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

  // The sheet closed (05 §14.2). The planner keeps its mic roots and lineage cache (Restart, not Reset): a mic root
  // stays one until its process exits (05 §5.6) and lineage lasts for the process's lifetime (§5.7), even when the
  // sheet closes in between. Both are keyed by process key (pid and creation time), and every reconcile, the next
  // watch's or a one-shot's, prunes the keys that are no longer in the table, so a reused pid inherits nothing (only a
  // process whose creation time can't be read, key time 0, could, and that keeps more out, never less).
  void StopWatching() {
    if (!watching_) {
      return;
    }
    watching_ = false;
    st_->watching.store(false, std::memory_order_release);
    endpoints_.Clear();
    endpoints_.TakeGone();
    planner_.Restart();
    snapshotter_.Clear();
    {
      std::lock_guard<std::mutex> l(st_->mu);
      st_->report.reset();
    }
    st_->signals->sessions.exchange(0, std::memory_order_acq_rel);
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
      Event(now, ReportEventCode::kNoDevice);
    }
    last_render_ = render;
  }

  void HandleSignals(int64_t now) {
    Signals &s = *st_->signals;
    if (const uint32_t devices = s.devices.exchange(0, std::memory_order_acq_rel); devices > 0) {
      const uint32_t added = s.added.exchange(0, std::memory_order_acq_rel);
      const uint32_t removed = s.removed.exchange(0, std::memory_order_acq_rel);
      const uint32_t default_render = s.default_render.exchange(0, std::memory_order_acq_rel);
      probe_flags_.reset();  // cached until the next device event (05 §11.1)
      set_.ForgetFailures(); // capture_failed lasts until the next device or session event (05 §8.5)
      if (watching_) {
        counters_.device_changes += devices;
        for (uint32_t i = 0; i < std::min<uint32_t>(added, kMaxEventsPerKind); ++i) {
          Event(now, ReportEventCode::kDeviceAdded);
        }
        for (uint32_t i = 0; i < std::min<uint32_t>(removed, kMaxEventsPerKind); ++i) {
          Event(now, ReportEventCode::kDeviceRemoved);
        }
        for (uint32_t i = 0; i < std::min<uint32_t>(default_render, kMaxEventsPerKind); ++i) {
          Event(now, ReportEventCode::kDefaultDeviceChanged);
        }
        log::Info("audio", "audio devices changed",
                  {{"added", added}, {"removed", removed}, {"default", default_render}});
        Rebuild(now);
        Reconcile(now);
      }
    }
    if (!watching_) {
      s.sessions.exchange(0, std::memory_order_acq_rel);
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
    // Reset before the refresh: an event during it starts the next round, so none is lost. The count goes first
    // (Signals::OnSession counts, then sets the time): an event counted between the two resets leaves sessions > 0
    // with first_session_ns == 0, which Deadline() turns into an immediate wake-up; one whose count the exchange took
    // happened before the refresh below, which sees it.
    s.sessions.exchange(0, std::memory_order_acq_rel);
    s.first_session_ns.store(0, std::memory_order_release);
    set_.ForgetFailures();
    if (endpoints_.RefreshSessions(true, now) == AUDCLNT_E_SERVICE_NOT_RUNNING) {
      ServiceGone(now);
    }
    Reconcile(now);
  }

  void Tick(int64_t now) {
    PumpLive(now);
    if (!live_ && threads_idle_since_ != 0 && now - threads_idle_since_ >= kThreadsIdleNs) {
      StopCaptureThreads(); // 5 s after the last share (05 §10.5)
    }
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
      if (live_ && now > cpu_mark_ns_) {
        // audioCpuMsPerS (05 §12.1): the mixer's and the capture thread's busy time per second.
        const int64_t busy = BusyNs();
        cpu_ms_per_s_ =
            static_cast<double>(busy - cpu_mark_busy_) / 1e6 / (static_cast<double>(now - cpu_mark_ns_) / 1e9);
        cpu_mark_busy_ = busy;
        cpu_mark_ns_ = now;
      }
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

  // One plan by planner_ over a fresh process table and eps' sessions, as a report (05 §7.3, §12.1). With track, the
  // sessions keep their app ids (the levels' rows) and announce themselves, and the report gets the watcher's events.
  Report Plan(EndpointSet &eps, int64_t now, bool track) {
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
    DescribeRowProcesses(entries, tracked);
    const audiopolicy::ProcessTable table(std::move(entries));
    const bool live = track && live_ != nullptr;
    std::vector<audiopolicy::LiveStream> streams;
    std::vector<audiopolicy::ProcessKey> failed;
    if (live) {
      streams = set_.Inputs();
      failed = set_.CaptureFailed();
    }
    audiopolicy::PlanInput in;
    in.now_ns = now;
    // Capture runs only while a share is live with sound on; otherwise the plan is the include set's prediction with
    // engine none (05 §6.5).
    in.mode = live && live_mode_ == IM_AUDIO_MODE_EXCLUDE_APPS ? IM_AUDIO_MODE_EXCLUDE_APPS : IM_AUDIO_MODE_OFF;
    // As if the startup probe passed (05 §6.1: the include set never leaks); the probe and the endpoint engine are
    // README WA53's.
    in.process_loopback = true;
    in.sessions = sessions;
    in.streams = streams;
    in.capture_failed = failed;
    const audiopolicy::Plan plan = planner_.Reconcile(table, in);
    if (!plan.violations.empty()) {
      log::Error("audio", "audio policy invariant broken", {{"violations", plan.violations.size()}});
    }
    if (live) {
      set_.Apply(
          plan,
          [&](const audiopolicy::ProcessKey &k) {
            const auto i = table.Find(k);
            return i ? plan.processes[*i].app_id : std::string();
          },
          [&](const audiopolicy::ProcessKey &anc, const audiopolicy::ProcessKey &desc) {
            const auto a = table.Find(anc);
            const auto d = table.Find(desc);
            return a && d && *a != *d && table.InTree(*d, *a, false);
          },
          now);
    }

    Report r;
    const uint32_t render = eps.render_count();
    const uint32_t capture = eps.capture_count();
    r.mode = live && live_mode_ == IM_AUDIO_MODE_OFF ? audiomix::ReportMode::kOff : audiomix::ReportMode::kExcludeApps;
    r.engine = ToReport(plan.engine);
    r.state = render > 0 ? audiomix::ReportState::kOk : audiomix::ReportState::kNoDevice;
    r.live = live;
    if (live) {
      const audiomix::MixEngine &e = live_->engine();
      r.muted = e.muted();
      r.paused = e.paused();
      live_->state.store(render > 0 ? IM_AUDIO_STATE_OK : IM_AUDIO_STATE_NO_DEVICE, std::memory_order_relaxed);
    }
    r.rebuilding = restart_at_ns_ != 0;
    // The startup probe (05 §11.2) is README WA53's: pending with an active render endpoint, skipped without.
    r.probe.result = render > 0 ? audiomix::ProbeResult::kPending : audiomix::ProbeResult::kSkipped;
    r.probe.os_build = win::OsBuild();
    r.device = audiomix::ReportDevice{render > 0, render, capture};
    r.apps.reserve(plan.apps.size());
    for (const audiopolicy::AppRow &a : plan.apps) {
      r.apps.push_back(ToReport(a));
    }
    for (const audiopolicy::PlanWarning &w : plan.warnings) {
      audiomix::ReportWarning rw;
      if (ToReport(w, &rw)) {
        r.warnings.push_back(std::move(rw));
      }
    }
    std::vector<StreamView> views;
    if (live) {
      views = set_.Views();
    }
    r.sessions.reserve(tracked.size());
    for (std::size_t k = 0; k < tracked.size() && k < plan.sessions.size(); ++k) {
      const audiopolicy::SessionResult &sr = plan.sessions[k];
      audiomix::ReportSession s;
      s.pid = tracked[k]->pid;
      if (sr.process >= 0) {
        const audiopolicy::Process &p = table.at(static_cast<std::size_t>(sr.process));
        s.created = p.key.created;
        s.exe = p.exe;
        for (const StreamView &v : views) {
          if (v.target == p.key) {
            s.stream = v.stream; // the session's own process is a target (05 §12.1 sessions[].stream)
            s.format = v.format;
          }
        }
      }
      s.app_id = sr.app_id;
      s.flow = ToReport(tracked[k]->flow);
      s.state = ToReport(sessions[k].state);
      s.multi_process = tracked[k]->multi_process;
      s.verdict = ToReport(sr.verdict);
      s.reason = ToReport(sr.reason);
      s.note = ToReport(sr.note);
      r.sessions.push_back(std::move(s));
      if (track) {
        tracked[k]->app_id = sr.app_id;
        if (!tracked[k]->announced && sr.verdict != audiopolicy::Verdict::kPending) {
          tracked[k]->announced = true;
          Event(now, ReportEventCode::kSessionCreated, sr.app_id);
        }
      }
    }
    // The planner counts each block once across watching and one-shots (they share it), so both add theirs.
    counters_.pid_reuse_blocks += plan.pid_reuse_blocks;
    if (track) {
      for (std::string &app : eps.TakeGone()) {
        Event(now, ReportEventCode::kSessionExpired, std::move(app));
      }
    }
    r.qpc_ns = now;
    r.counters = Counters();
    r.events.assign(events_.begin(), events_.end());
    return r;
  }

  // The report's counters (05 §12.1): the watcher's, the include set's and the mixers' (the shares' so far plus the
  // live one's).
  audiomix::ReportCounters Counters() const {
    audiomix::ReportCounters c = counters_;
    const StreamCounters sc = set_.counters();
    c.streams = sc.streams;
    c.activations = sc.activations;
    c.activation_failures = sc.activation_failures;
    c.discontinuities = sc.discontinuities;
    c.underruns = mixer_base_.underruns;
    c.trims = mixer_base_.trims;
    c.tail_fades = mixer_base_.tail_fades;
    c.overflows = mixer_base_.overflows;
    c.mixer_resyncs = mixer_base_.mixer_resyncs;
    c.fastpath_late_cuts = mixer_base_.fastpath_late_cuts;
    c.max_resample_ppm = mixer_base_.max_resample_ppm;
    if (live_) {
      const audiomix::MixCounters mc = live_->engine().counters();
      c.underruns += mc.underruns;
      c.trims += mc.trims;
      c.tail_fades += mc.tail_fades;
      c.overflows += mc.overflows;
      c.mixer_resyncs += mc.mixer_resyncs;
      c.fastpath_late_cuts += mc.fastpath_late_cuts;
      c.max_resample_ppm = std::max(c.max_resample_ppm, mc.max_resample_ppm);
      c.audio_cpu_ms_per_s = cpu_ms_per_s_;
    }
    return c;
  }

  // FileDescription for the processes a report row can be named after (05 §5.1; descriptions are cached per image
  // path), each with its parent chain, which the app walk that finds a row's app root and rule match follows: the
  // processes with a session; the shared window's process (its app is a row even when silent, 05 §12.1); and the
  // processes an exe rule without a name matches (a row without a session, such as a silent built-in, is named after
  // the process its rule matched).
  void DescribeRowProcesses(std::vector<audiopolicy::ProcessEntry> &entries,
                            const std::vector<TrackedSession *> &tracked) {
    std::unordered_map<uint32_t, std::size_t> by_pid;
    by_pid.reserve(entries.size());
    for (std::size_t i = 0; i < entries.size(); ++i) {
      by_pid.emplace(entries[i].pid, i);
    }
    std::unordered_set<std::size_t> picked;
    std::vector<std::size_t> want;
    const auto chain = [&](uint32_t pid) {
      for (int step = 0; step < audiopolicy::kMaxWalk && pid != 0; ++step) {
        const auto it = by_pid.find(pid);
        if (it == by_pid.end() || !picked.insert(it->second).second) {
          break;
        }
        want.push_back(it->second);
        pid = entries[it->second].parent_pid;
      }
    };
    for (const TrackedSession *t : tracked) {
      chain(t->pid);
    }
    chain(rules_.shared_pid);
    std::unordered_set<std::string> unnamed; // rule values are case-folded (audiopolicy::Rule)
    for (const audiopolicy::Rule &rule : rules_.rules) {
      if ((rule.kind == IM_RULE_APP_EXCLUDE || rule.kind == IM_RULE_APP_ALLOW) && rule.name.empty()) {
        unnamed.insert(rule.value);
      }
    }
    if (!unnamed.empty()) {
      for (std::size_t i = 0; i < entries.size(); ++i) {
        if (unnamed.count(audiopolicy::FoldCase(entries[i].exe)) != 0) {
          chain(entries[i].pid);
        }
      }
    }
    snapshotter_.Describe(entries, want);
  }

  void Reconcile(int64_t now) {
    if (!watching_) {
      return;
    }
    Report r = Plan(endpoints_, now, true);
    std::string visible = VisibleState(r);
    if (visible != visible_) {
      ++seq_; // anything visible changed (05 §12.1)
      visible_ = std::move(visible);
    }
    r.seq = seq_;
    auto doc = std::make_shared<const std::string>(audiomix::WriteReportJson(r));
    std::lock_guard<std::mutex> l(st_->mu);
    st_->report = std::move(doc);
  }

  // The report for im_list_audio_apps when the caller had none: the live one while watching, else a one-shot snapshot
  // that is kept for the snprintf rule's retry (ListAppsJson). It is stored here, on the thread, so it is ordered with
  // the requests that drop it (DropOneShot).
  std::shared_ptr<const std::string> OneShot(int64_t now) {
    if (watching_) {
      std::lock_guard<std::mutex> l(st_->mu);
      if (st_->report) {
        return st_->report;
      }
    }
    // A snapshot without registrations, planned with the current rules and the planner's remembered mic roots and
    // lineage (a voice app that was a mic root while the sheet was open still is), state "stopped" (05 §12.1). The
    // plan runs in mode off, so the engine state it leaves is the one Restart leaves.
    EndpointSet eps(st_->signals);
    if (EnsureEnumerator(now)) {
      if (eps.Rebuild(en_.Get(), false) == AUDCLNT_E_SERVICE_NOT_RUNNING ||
          eps.RefreshSessions(false, now) == AUDCLNT_E_SERVICE_NOT_RUNNING) {
        eps.Clear();
        ServiceGone(now);
      }
    }
    Report r = Plan(eps, now, false);
    eps.Clear();
    r.state = audiomix::ReportState::kStopped;
    r.seq = seq_;
    auto doc = std::make_shared<const std::string>(audiomix::WriteReportJson(r));
    if (!watching_) {
      std::lock_guard<std::mutex> l(st_->mu);
      st_->oneshot = doc;
      st_->oneshot_ns = engine::os::QpcNs();
    }
    return doc;
  }

  void PublishLevels(int64_t now) {
    std::vector<AppActivity> apps;
    for (const TrackedSession *t : endpoints_.sessions()) {
      if (t->flow == audiopolicy::Flow::kRender && !t->app_id.empty()) {
        apps.push_back(AppActivity{t->app_id, t->peak_db, t->last_active_ns, false});
      }
    }
    // Not live: no mix (05 §4.1: -120 when not live) and every entry is a session's activity (source 1). Live: the
    // bus, and for each app with running streams what friends hear of it, its channels summed (source 0, 05 §12.2).
    float mix_peak = kSilenceDb;
    float mix_rms = kSilenceDb;
    std::vector<std::pair<std::string, audiomix::LevelSum>> captured;
    if (live_) {
      auto snap = std::make_unique<audiomix::LevelsSnapshot>();
      if (live_->engine().ReadLevels(snap.get())) {
        mix_peak = snap->mix_peak_db;
        mix_rms = snap->mix_rms_db;
        for (uint32_t i = 0; i < snap->count; ++i) {
          const audiomix::ChannelLevel &ch = snap->channels[i];
          std::string app = ch.running ? set_.AppOfTag(ch.tag) : std::string();
          if (app.empty()) {
            continue;
          }
          auto it = std::find_if(captured.begin(), captured.end(), [&](const auto &c) { return c.first == app; });
          if (it == captured.end()) {
            captured.emplace_back(std::move(app), audiomix::LevelSum{});
            it = captured.end() - 1;
          }
          it->second.Add(ch.peak_db, ch.rms_db);
        }
      }
    }
    for (const auto &[id, sum] : captured) {
      apps.push_back(AppActivity{id, sum.peak_db(), now, true, sum.rms_db()});
    }
    st_->levels.Publish(MakeLevels(seq_, now, mix_peak, mix_rms, apps));
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

  void Event(int64_t now, ReportEventCode code, std::string app = {}, std::string detail = {}) {
    PushEvent(ReportEvent{now, code, std::move(app), std::move(detail)});
  }

  void PushEvent(ReportEvent e) {
    events_.push_back(std::move(e));
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
  audiomix::ReportCounters counters_; // the watcher's own: deviceChanges, pidReuseBlocks
  std::deque<ReportEvent> events_;

  // The live share (README WA45). live_ is declared before the include set, which feeds its engine, so the set goes
  // first; Run detaches it before the loop ends anyway.
  std::shared_ptr<LiveMix> live_;
  uint32_t live_mode_ = IM_AUDIO_MODE_EXCLUDE_APPS; // im_audio_set_mode while live
  bool user_watch_ = false;                         // im_audio_watch's, beside the live share's watching
  ActivatorPool pool_;
  CaptureThread capture_;
  std::unique_ptr<ProcessGate> gate_;
  StreamSet set_;
  int64_t threads_idle_since_ = 0;      // the last share stopped (0: threads in use or stopped)
  audiomix::ReportCounters mixer_base_; // the counters of the shares that ended
  int64_t cpu_mark_ns_ = 0;
  int64_t cpu_mark_busy_ = 0;
  double cpu_ms_per_s_ = 0;
  std::string views_; // the streams' states of the last report
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
  started_.store(true, std::memory_order_release);
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
// unwatch) also drops the kept one-shot report, here for the callers that read it before the request is served, and
// again on the thread once it applied (Loop::DropOneShot).
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
  if (started_.load(std::memory_order_acquire)) {
    Enqueue(*st_, std::move(r));
  }
}

im_status Watcher::Watch(audiopolicy::Exclusions ex) {
  auto r = std::make_shared<Request>(RequestKind::kWatch);
  r->rules = std::move(ex);
  if (!started_.load(std::memory_order_acquire) || !Enqueue(*st_, r)) {
    return abi::Fail(IM_ERR_BAD_STATE, {}, "im_audio_watch: the audio watcher is not running");
  }
  return Await(*r, "im_audio_watch", true);
}

im_status Watcher::Unwatch() {
  auto r = std::make_shared<Request>(RequestKind::kUnwatch);
  if (!started_.load(std::memory_order_acquire) || !Enqueue(*st_, r)) {
    return IM_OK; // nothing watches
  }
  return Await(*r, "im_audio_unwatch", true);
}

bool Watcher::watching() const noexcept { return st_->watching.load(std::memory_order_acquire); }

im_status Watcher::AttachLive(std::shared_ptr<LiveMix> mix) {
  auto r = std::make_shared<Request>(RequestKind::kAttach);
  r->mix = std::move(mix);
  if (!started_.load(std::memory_order_acquire) || !Enqueue(*st_, r)) {
    return abi::Fail(IM_ERR_BAD_STATE, {}, "audio mixer: the audio watcher is not running");
  }
  return IM_OK; // applied on the thread: the first activations continue asynchronously (05 §10.5)
}

im_status Watcher::DetachLive(const std::shared_ptr<LiveMix> &mix, std::chrono::milliseconds wait) {
  auto r = std::make_shared<Request>(RequestKind::kDetach);
  r->mix = mix;
  if (!started_.load(std::memory_order_acquire) || !Enqueue(*st_, r)) {
    return IM_OK; // nothing runs: nothing holds the mix
  }
  std::unique_lock<std::mutex> l(r->mu);
  if (!r->cv.wait_for(l, wait, [&] { return r->done; })) {
    log::Warn("audio", "audio watcher: the share's streams are released late");
    return IM_ERR_TIMEOUT; // the request stays queued; the mix lives on in it until the thread lets go
  }
  return IM_OK;
}

void Watcher::SetLiveMode(uint32_t mode) {
  auto r = std::make_shared<Request>(RequestKind::kLiveMode);
  r->mode = mode;
  if (started_.load(std::memory_order_acquire)) {
    Enqueue(*st_, std::move(r));
  }
}

void Watcher::SetLiveMuted(bool muted) {
  auto r = std::make_shared<Request>(RequestKind::kLiveMuted);
  r->muted = muted;
  if (started_.load(std::memory_order_acquire)) {
    Enqueue(*st_, std::move(r));
  }
}

int32_t Watcher::ListAppsJson(char *buf, int32_t cap) {
  std::shared_ptr<const std::string> doc;
  {
    std::lock_guard<std::mutex> l(st_->mu);
    if (st_->watching.load(std::memory_order_acquire) && st_->report) {
      doc = st_->report;
    } else if (st_->oneshot && engine::os::QpcNs() - st_->oneshot_ns < kOneShotReuseNs) {
      doc = st_->oneshot; // the thread keeps it (Loop::OneShot); the caller only reads it
    }
  }
  if (!doc) {
    auto r = std::make_shared<Request>(RequestKind::kOneShot);
    if (!started_.load(std::memory_order_acquire) || !Enqueue(*st_, r)) {
      return abi::Fail(IM_ERR_BAD_STATE, {}, "im_list_audio_apps: the audio watcher is not running");
    }
    if (const im_status st = Await(*r, "im_list_audio_apps", true); st != IM_OK) {
      return st;
    }
    doc = std::move(r->doc);
    if (!doc) {
      return abi::Fail(IM_ERR_INTERNAL, {}, "im_list_audio_apps: the audio watcher gave no report");
    }
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
  if (!started_.load(std::memory_order_acquire) || !Enqueue(*st_, r)) {
    return 0;
  }
  if (Await(*r, "audio probe flags", false) != IM_OK) {
    log::Warn("audio", "audio probe flags: the watcher did not answer");
    return 0;
  }
  return r->flags;
}

} // namespace im::audio
