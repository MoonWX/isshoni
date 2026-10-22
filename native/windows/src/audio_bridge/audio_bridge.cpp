#include "audio_bridge/audio_bridge.h"

#include <atomic>

namespace im::audio_bridge {
namespace {

std::atomic<audio::AudioEngine *> g_override{nullptr};

} // namespace

audio::AudioEngine &Engine() noexcept {
  if (audio::AudioEngine *e = g_override.load(std::memory_order_acquire)) {
    return *e;
  }
  return audio::AudioEngine::Get();
}

void SetEngineForTest(audio::AudioEngine *engine) noexcept { g_override.store(engine, std::memory_order_release); }

} // namespace im::audio_bridge
