// The skeleton's AudioEngine (README WA11): it starts and stops, reports no audio capability and refuses the rest as
// not implemented. 05's engine replaces this file (README WA35, WA45, WA53).
#include "audio/mixer.h"

#include "im/abi/status.h"

namespace im::audio {
namespace {

class StubAudioEngine final : public AudioEngine {
public:
  im_status Init(uint32_t) override { return IM_OK; }
  void Shutdown() override {}
  uint32_t ProbeFlags() override { return 0; }
  int32_t ListAppsJson(uint32_t, char *, int32_t) override { return NotImplemented("im_list_audio_apps"); }
  im_status SetRules(const im_exclusions *) override { return NotImplemented("im_set_exclusions"); }
  im_status Watch(const im_exclusions *) override { return NotImplemented("im_audio_watch"); }
  im_status Unwatch() override { return NotImplemented("im_audio_unwatch"); }
  im_status SetMode(uint32_t) override { return NotImplemented("im_audio_set_mode"); }
  im_status SetMuted(bool) override { return NotImplemented("im_audio_set_muted"); }
  im_status LevelsGet(im_audio_levels *) override { return NotImplemented("im_audio_levels_get"); }
  int32_t SoundCheck(uint32_t, int32_t, char *, int32_t) override { return NotImplemented("im_audio_sound_check"); }
  std::unique_ptr<Mixer> NewMixer() override { return nullptr; }

private:
  static im_status NotImplemented(const char *fn) noexcept {
    return abi::Failf(IM_ERR_NOT_IMPLEMENTED, {}, "%s: Windows audio is not implemented yet (README WA35)", fn);
  }
};

} // namespace

AudioEngine &AudioEngine::Get() {
  static AudioEngine *const engine = new StubAudioEngine(); // never destroyed, like the engine (src/engine/engine.h)
  return *engine;
}

} // namespace im::audio
