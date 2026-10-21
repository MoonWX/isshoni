// 04's side of the audio boundary (docs/m2/04-windows-engine.md §10.2, §10.3; README WA34 builds the session wiring
// here): which AudioEngine the engine talks to, the Opus decoder behind im_test_opus_decode and libopus's version
// for im_build_info. audio_bridge.cpp holds the engine reference; audio_bridge_stub.cpp the parts WA34 fills.
#pragma once

#include <cstdint>
#include <string_view>

#include "audio/mixer.h"
#include "isshoni_media.h"

namespace im::audio_bridge {

// The AudioEngine every 04 call goes to: the override when a test set one, else audio::AudioEngine::Get().
audio::AudioEngine &Engine() noexcept;

// Tests (04 §18.2's fake AudioEngine): every later call goes to engine until it is reset with nullptr. Set it while
// the engine is shut down; the caller keeps engine alive.
void SetEngineForTest(audio::AudioEngine *engine) noexcept;

// im_test_opus_decode (05 §4.1): decodes one Opus packet with the DLL's libopus into interleaved stereo float pcm.
// Returns the frames per channel, or a negative status. Arguments arrive checked.
int32_t TestOpusDecode(const uint8_t *pkt, int32_t len, float *pcm, int32_t max_frames);

// opus_get_version_string() ("libopus 1.5.2"), or "" while libopus isn't linked in.
std::string_view OpusVersion() noexcept;

} // namespace im::audio_bridge
