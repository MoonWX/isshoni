// 05's AudioEngine (docs/m2/05-windows-audio.md §10.5; "05" below), the watcher half (README WA35): Init and Shutdown
// with the watcher thread, the rules, watch and unwatch, the report, the levels and the probe flags. The capture
// streams and the mixer (NewMixer, SetMode, SetMuted while live) are README WA45's, the sound check WA53's; until then
// no audio session can run, so NewMixer gives none (04's session then runs without audio packets, audio state off),
// and SetMode and SetMuted answer IM_ERR_BAD_STATE as 05 §4.3 says for "no running audio session".
#include "audio/mixer.h"

#include <windows.h>

#include <memory>
#include <mutex>
#include <optional>
#include <utility>

#include "audio/watcher.h"
#include "im/abi/status.h"
#include "im/audiopolicy/exclusions.h"
#include "im/log/ring.h"

namespace im::audio {
namespace {

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
    {
      std::lock_guard<std::mutex> l(mu_);
      w = std::move(watcher_);
      rules_.reset(); // the process-wide set belongs to this im_init
      no_audio_ = false;
      initialized_ = false;
    }
    if (w) {
      w->Stop(); // unwatches and joins the watcher thread (<= 2 s)
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

  im_status SetMode(uint32_t) override { return NoAudioSession("im_audio_set_mode"); }

  im_status SetMuted(bool) override { return NoAudioSession("im_audio_set_muted"); }

  im_status LevelsGet(im_audio_levels *out) override {
    const std::shared_ptr<Watcher> w = Current();
    if (!w) {
      return NoAudio("im_audio_levels_get");
    }
    return w->LevelsGet(out);
  }

  int32_t SoundCheck(uint32_t, int32_t, char *, int32_t) override {
    if (!Current()) {
      return NoAudio("im_audio_sound_check");
    }
    return abi::Fail(IM_ERR_NOT_IMPLEMENTED, {}, "im_audio_sound_check: not implemented yet (README WA53)");
  }

  // The include-set capture and the mixer are README WA45's: no mixer yet, so a share runs without audio packets.
  std::unique_ptr<Mixer> NewMixer() override { return nullptr; }

private:
  std::shared_ptr<Watcher> Current() const {
    std::lock_guard<std::mutex> l(mu_);
    return watcher_;
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
  std::optional<audiopolicy::Exclusions> rules_;
  bool no_audio_ = false;
  bool initialized_ = false;
};

} // namespace

AudioEngine &AudioEngine::Get() {
  static AudioEngine *const engine = new WindowsAudioEngine(); // never destroyed, like the engine (src/engine/engine.h)
  return *engine;
}

} // namespace im::audio
