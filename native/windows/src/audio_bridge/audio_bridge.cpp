#include "audio_bridge/audio_bridge.h"

#include <opus.h>

#include <algorithm>
#include <atomic>
#include <cstring>
#include <memory>
#include <mutex>
#include <new>

#include "im/abi/status.h"
#include "im/audio/opus_decoder.h"

namespace im::audio_bridge {
namespace {

std::atomic<audio::AudioEngine *> g_override{nullptr};

// im_test_opus_decode's decoder: one per process, created by the first call.
struct TestDecoder {
  std::mutex mu;
  std::unique_ptr<audio::OpusPacketDecoder> dec; // guarded by mu
};

// Throws the first allocation's bad_alloc; the next call tries again (a static whose initializer threw stays unset).
TestDecoder &TheTestDecoder() {
  static TestDecoder *const d = new TestDecoder(); // never destroyed, like the engine (src/engine/engine.h)
  return *d;
}

// Puts the export's name in front of the detail that the decoder set for st.
int32_t DecodeFailed(int32_t st) noexcept {
  char inner[abi::kMaxErrorBytes + 1];
  const std::string_view d = abi::LastErrorText();
  const std::size_t n = std::min(d.size(), sizeof inner - 1);
  std::memcpy(inner, d.data(), n);
  return abi::Failf(st, {}, "im_test_opus_decode: %.*s", static_cast<int>(n), inner);
}

} // namespace

audio::AudioEngine &Engine() noexcept {
  if (audio::AudioEngine *e = g_override.load(std::memory_order_acquire)) {
    return *e;
  }
  return audio::AudioEngine::Get();
}

void SetEngineForTest(audio::AudioEngine *engine) noexcept { g_override.store(engine, std::memory_order_release); }

int32_t TestOpusDecode(const uint8_t *pkt, int32_t len, float *pcm, int32_t max_frames) noexcept {
  try {
    TestDecoder &t = TheTestDecoder();
    std::lock_guard<std::mutex> lock(t.mu);
    if (!t.dec) {
      if (const im_status st = audio::OpusPacketDecoder::Create(&t.dec); st != IM_OK) {
        return DecodeFailed(st);
      }
    }
    const int32_t n = t.dec->Decode(pkt, len, pcm, max_frames);
    return n >= 0 ? n : DecodeFailed(n);
  } catch (const std::bad_alloc &) { // the first call's TestDecoder
    return abi::Fail(IM_ERR_INTERNAL, {}, "im_test_opus_decode: out of memory");
  } catch (...) { // std::mutex::lock's system_error
    return abi::Fail(IM_ERR_INTERNAL, {}, "im_test_opus_decode: the decoder's lock failed");
  }
}

std::string_view OpusVersion() noexcept {
  const char *v = opus_get_version_string();
  return v != nullptr ? std::string_view(v) : std::string_view();
}

} // namespace im::audio_bridge
