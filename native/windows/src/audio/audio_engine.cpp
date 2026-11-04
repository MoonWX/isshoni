// 05's AudioEngine (docs/m2/05-windows-audio.md §10.5; "05" below): Init and Shutdown with the watcher thread, the
// rules, watch and unwatch, the report, the levels and the probe flags (README WA35), and a share's Mixer with the
// include set's capture (README WA45): NewMixer, and SetMode and SetMuted while it runs; the sound check (README WA53;
// the fast path, the endpoint engine and the startup probe are the watcher's). Without a running audio session SetMode
// and SetMuted answer IM_ERR_BAD_STATE (05 §4.3).
#include "audio/mixer.h"

#include "audio/win_audio.h"

#include <atomic>
#include <chrono>
#include <condition_variable>
#include <memory>
#include <mutex>
#include <optional>
#include <string>
#include <system_error>
#include <thread>
#include <utility>

#include "api/guard.h"
#include "audio/live_mix.h"
#include "audio/loopback_io.h"
#include "audio/sound_check.h"
#include "audio/watcher.h"
#include "engine/os.h"
#include "im/abi/status.h"
#include "im/audiomix/mixer.h"
#include "im/audiopolicy/exclusions.h"
#include "im/log/ring.h"

namespace im::audio {
namespace {

// How long Mixer::Stop waits for the watcher to let go of the share's streams (Stop's bound is 500 ms, 05 §10.5; the
// mixer thread's join comes first).
constexpr std::chrono::milliseconds kDetachWait{400};
// im_audio_sound_check waits timeout_ms plus this before IM_ERR_TIMEOUT (05 §11.3 step 5).
constexpr int32_t kSoundCheckGraceMs = 500;

class WindowsAudioEngine;

// A share's mixer (05 §9, §10.5): the im_audiomix mixer thread on the Windows clock, under MMCSS, feeding the session's
// PcmSink, with the include set's channels from the watcher. Every method may come from any thread.
class WindowsMixer final : public Mixer {
public:
  WindowsMixer(WindowsAudioEngine *owner, std::shared_ptr<Watcher> watcher)
      : owner_(owner), watcher_(std::move(watcher)) {}
  ~WindowsMixer() override { Stop(); } // also after a failed Start (the session then runs without it)
  WindowsMixer(const WindowsMixer &) = delete;
  WindowsMixer &operator=(const WindowsMixer &) = delete;

  im_status Start(const MixerConfig &cfg) override;
  void Stop() override;

  void SetPaused(bool paused) override {
    std::lock_guard<std::mutex> l(mu_);
    paused_ = paused;
    if (mix_) {
      mix_->engine().SetPaused(paused); // the bus fades in 10 ms; mute and mode off are their own flags (05 §9.6)
    }
  }

  uint32_t State() const override {
    std::lock_guard<std::mutex> l(mu_);
    return mix_ ? mix_->state.load(std::memory_order_relaxed) : IM_AUDIO_STATE_OFF;
  }

private:
  WindowsAudioEngine *const owner_;
  const std::shared_ptr<Watcher> watcher_;
  mutable std::mutex mu_;
  std::shared_ptr<LiveMix> mix_;
  bool paused_ = false;
};

class WindowsAudioEngine final : public AudioEngine {
public:
  im_status Init(uint32_t init_flags) override {
    std::shared_ptr<Watcher> old;
    {
      std::lock_guard<std::mutex> l(mu_);
      old = std::move(watcher_);
    }
    if (old) {
      old->Stop(); // an Init without Shutdown: start over
    }
    std::lock_guard<std::mutex> l(mu_);
    no_audio_ = (init_flags & IM_INIT_NO_AUDIO) != 0;
    initialized_ = true;
    if (no_audio_) {
      return IM_OK; // the DLL touches no audio API (05 §4.3)
    }
    auto w = std::make_shared<Watcher>();
    if (const im_status st = w->Start(); st != IM_OK) {
      return st; // 04 logs it; the audio calls then answer as without audio
    }
    if (rules_) {
      w->SetRules(*rules_);
    }
    watcher_ = std::move(w);
    return IM_OK;
  }

  void Shutdown() override {
    std::shared_ptr<Watcher> w;
    std::shared_ptr<LiveMix> live;
    {
      std::lock_guard<std::mutex> l(mu_);
      w = std::move(watcher_);
      live = std::exchange(live_, {});
      rules_.reset(); // the process-wide set belongs to this im_init
      no_audio_ = false;
      initialized_ = false;
    }
    if (live) {
      live->mixer->Stop(); // 04 stops its sessions first; a mixer still running stops here, before its streams go
    }
    if (w) {
      w->Stop(); // detaches a live share, unwatches and joins the audio threads (<= 2 s each)
    }
  }

  uint32_t ProbeFlags() override {
    const std::shared_ptr<Watcher> w = Current();
    return w ? w->ProbeFlags() : 0; // IM_INIT_NO_AUDIO: audio_flags = 0 (05 §4.3)
  }

  int32_t ListAppsJson(uint32_t, char *buf, int32_t cap) override {
    const std::shared_ptr<Watcher> w = Current();
    if (!w) {
      return NoAudio("im_list_audio_apps");
    }
    return w->ListAppsJson(buf, cap);
  }

  im_status SetRules(const im_exclusions *ex) override {
    audiopolicy::Exclusions parsed;
    if (const im_status st = Parse(ex, parsed); st != IM_OK) {
      return st;
    }
    std::shared_ptr<Watcher> w;
    {
      // Kept for the next im_audio_watch or im_start without exclusions (05 §4.3, "Before start").
      std::lock_guard<std::mutex> l(mu_);
      rules_ = parsed;
      w = watcher_;
    }
    if (w) {
      w->SetRules(std::move(parsed));
    }
    return IM_OK;
  }

  im_status Watch(const im_exclusions *ex) override {
    const std::shared_ptr<Watcher> w = Current();
    if (!w) {
      return NoAudio("im_audio_watch");
    }
    audiopolicy::Exclusions parsed;
    if (const im_status st = Parse(ex, parsed); st != IM_OK) {
      return st;
    }
    {
      std::lock_guard<std::mutex> l(mu_);
      rules_ = parsed;
    }
    return w->Watch(std::move(parsed));
  }

  im_status Unwatch() override {
    const std::shared_ptr<Watcher> w = Current();
    return w ? w->Unwatch() : IM_OK; // idempotent: nothing watches
  }

  // Sound off and on for the live share (05 §6.5, §9.6): the bus's mode-off flag at once, the plan on the watcher.
  im_status SetMode(uint32_t mode) override {
    auto [mix, w] = Live();
    if (!mix) {
      return NoAudioSession("im_audio_set_mode");
    }
    mix->engine().SetSoundOff(mode == IM_AUDIO_MODE_OFF);
    w->SetLiveMode(mode);
    return IM_OK;
  }

  // The user's (pause) mute of the live share (05 §9.6).
  im_status SetMuted(bool muted) override {
    auto [mix, w] = Live();
    if (!mix) {
      return NoAudioSession("im_audio_set_muted");
    }
    mix->engine().SetMuted(muted);
    w->SetLiveMuted(muted);
    return IM_OK;
  }

  im_status LevelsGet(im_audio_levels *out) override {
    const std::shared_ptr<Watcher> w = Current();
    if (!w) {
      return NoAudio("im_audio_levels_get");
    }
    return w->LevelsGet(out);
  }

  // The first-run sound check (05 §11.3): on a DLL thread in COM's MTA, with the capture path the probe flags allow.
  // Exempt from the 3 s limit: it waits timeout_ms + 500, then answers IM_ERR_TIMEOUT and abandons the thread, which
  // stops at its next step (05 §4.3).
  int32_t SoundCheck(uint32_t helper_pid, int32_t timeout_ms, char *buf, int32_t cap) override {
    const std::shared_ptr<Watcher> w = Current();
    if (!w) {
      return NoAudio("im_audio_sound_check");
    }
    const uint32_t flags = w->ProbeFlags(); // cached since im_probe, or computed now (05 §11.1)
    struct Shared {
      std::mutex mu;
      std::condition_variable cv;
      bool done = false;
      std::string json;
      std::atomic<bool> abort{false};
    };
    auto sh = std::make_shared<Shared>();
    try {
      std::thread t([sh, helper_pid, timeout_ms, flags] {
        const uintptr_t token = engine::os::EnterThread("isshoni sound check", engine::os::ThreadClass::kNormal);
        std::string json;
        api::Guard("sound check", [&] {
          const win::ComApartment com;
          SoundCheckResult r;
          if (FAILED(com.hr())) {
            r.reason = "activation_failed";
          } else {
            const std::unique_ptr<LoopbackIo> io = NewWindowsLoopbackIo(kSoundCheckActivationMs);
            r = RunSoundCheck(*io, SoundCheckParams{helper_pid, timeout_ms, flags}, &sh->abort);
          }
          json = SoundCheckJson(r);
        });
        engine::os::LeaveThread(token);
        {
          std::lock_guard<std::mutex> l(sh->mu);
          sh->json = std::move(json);
          sh->done = true;
        }
        sh->cv.notify_all();
      });
      bool done = false;
      {
        std::unique_lock<std::mutex> l(sh->mu);
        done = sh->cv.wait_for(l, std::chrono::milliseconds(timeout_ms + kSoundCheckGraceMs), [&] { return sh->done; });
      }
      if (!done) {
        sh->abort.store(true, std::memory_order_release);
        t.detach(); // stuck in a driver call: abandoned, never killed; it holds its own state (05 §10.5)
        return abi::Failf(IM_ERR_TIMEOUT, {}, "im_audio_sound_check: no result within %d ms",
                          static_cast<int>(timeout_ms + kSoundCheckGraceMs));
      }
      t.join();
    } catch (const std::system_error &e) {
      return abi::Failf(IM_ERR_INTERNAL, {}, "im_audio_sound_check: the thread did not start: %s", e.what());
    }
    if (sh->json.empty()) {
      return abi::Fail(IM_ERR_INTERNAL, {}, "im_audio_sound_check: the check failed on its thread");
    }
    const int32_t n = abi::CopyOut(sh->json, buf, cap);
    if (n < 0) {
      return abi::Fail(n, {}, "im_audio_sound_check: the result does not fit the snprintf rule");
    }
    return n;
  }

  // A share's mixer; nullptr after IM_INIT_NO_AUDIO or when the watcher didn't start (the share then runs without
  // audio packets, 05 §4.3).
  std::unique_ptr<Mixer> NewMixer() override {
    const std::shared_ptr<Watcher> w = Current();
    if (!w) {
      return nullptr;
    }
    return std::make_unique<WindowsMixer>(this, w);
  }

  // From WindowsMixer: the live share's mix (one session with audio per process, 04 §6.4).
  void SetLive(std::shared_ptr<LiveMix> mix) {
    std::lock_guard<std::mutex> l(mu_);
    live_ = std::move(mix);
  }
  void ClearLive(const std::shared_ptr<LiveMix> &mix) {
    std::lock_guard<std::mutex> l(mu_);
    if (live_ == mix) {
      live_.reset();
    }
  }

private:
  std::shared_ptr<Watcher> Current() const {
    std::lock_guard<std::mutex> l(mu_);
    return watcher_;
  }

  std::pair<std::shared_ptr<LiveMix>, std::shared_ptr<Watcher>> Live() const {
    std::lock_guard<std::mutex> l(mu_);
    if (!live_ || !watcher_) {
      return {};
    }
    return {live_, watcher_};
  }

  // Validates and copies ex (04 §5.1) with the implicit instance rule on this process (05 §4.3).
  static im_status Parse(const im_exclusions *ex, audiopolicy::Exclusions &out) {
    return audiopolicy::ParseExclusions(ex, static_cast<uint32_t>(GetCurrentProcessId()), out);
  }

  // IM_INIT_NO_AUDIO, or a watcher that never started: the audio calls of 05 §4.3 answer IM_ERR_BAD_STATE.
  im_status NoAudio(const char *fn) const {
    std::lock_guard<std::mutex> l(mu_);
    if (no_audio_) {
      return abi::Failf(IM_ERR_BAD_STATE, {}, "%s: audio is off (IM_INIT_NO_AUDIO)", fn);
    }
    if (!initialized_) {
      return abi::Failf(IM_ERR_BAD_STATE, abi::token::kNotInitialized, "%s: call im_init first", fn);
    }
    return abi::Failf(IM_ERR_BAD_STATE, {}, "%s: the audio watcher did not start", fn);
  }

  static im_status NoAudioSession(const char *fn) {
    return abi::Failf(IM_ERR_BAD_STATE, {}, "%s: no running audio session", fn);
  }

  mutable std::mutex mu_;
  std::shared_ptr<Watcher> watcher_;
  std::shared_ptr<LiveMix> live_;
  std::optional<audiopolicy::Exclusions> rules_;
  bool no_audio_ = false;
  bool initialized_ = false;
};

im_status WindowsMixer::Start(const MixerConfig &cfg) {
  std::lock_guard<std::mutex> l(mu_);
  if (mix_) {
    return abi::Fail(IM_ERR_BAD_STATE, {}, "audio mixer: already started");
  }
  if (cfg.sink == nullptr) {
    return abi::Fail(IM_ERR_INVALID_ARG, {}, "audio mixer: no sink");
  }
  if (cfg.mode != IM_AUDIO_MODE_EXCLUDE_APPS) {
    return abi::Failf(IM_ERR_INVALID_ARG, {}, "audio mixer: mode %u has no mixer", cfg.mode);
  }
  auto mix = std::make_shared<LiveMix>();
  mix->sink = NewPcmSinkAdapter(cfg.sink); // the two-line adapter (05 §10.5)
  mix->clock = NewWindowsTickClock();
  mix->flags = cfg.flags;
  audiomix::MixerOptions o;
  o.sink = mix->sink.get();
  o.clock = mix->clock.get();
  o.thread_hook = &MixerThreadHook;
  if (const im_status st = audiomix::Mixer::Create(o, &mix->mixer); st != IM_OK) {
    return abi::Fail(st, {}, "audio mixer: not created");
  }
  mix->engine().SetPaused(paused_); // IM_START_PAUSED reaches SetPaused before Start
  // The clock first: chunks of silence on the grid from now on (05 §10.5: Start returns once the mixer clock runs);
  // the watcher's plan and the first activations follow asynchronously, so the first chunks are silence (§17).
  if (const im_status st = mix->mixer->Start(); st != IM_OK) {
    return abi::Fail(st, {}, "audio mixer: the mixer thread did not start");
  }
  if (const im_status st = watcher_->AttachLive(mix); st != IM_OK) {
    mix->mixer->Stop();
    return st;
  }
  mix_ = mix;
  owner_->SetLive(std::move(mix));
  return IM_OK;
}

void WindowsMixer::Stop() {
  std::shared_ptr<LiveMix> mix;
  {
    std::lock_guard<std::mutex> l(mu_);
    mix = std::exchange(mix_, {});
  }
  if (!mix) {
    return;
  }
  owner_->ClearLive(mix);
  mix->mixer->Stop(); // joins the mixer thread: no OnChunk or OnGap after this
  if (watcher_->DetachLive(mix, kDetachWait) != IM_OK) {
    abi::ClearLastError();
  }
}

} // namespace

AudioEngine &AudioEngine::Get() {
  static AudioEngine *const engine = new WindowsAudioEngine(); // never destroyed, like the engine (src/engine/engine.h)
  return *engine;
}

} // namespace im::audio
