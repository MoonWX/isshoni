// 04's side of the audio boundary inside isshoni-media.dll (docs/m2/04-windows-engine.md §10.1-§10.3, 05 §10.5;
// README WA34): which AudioEngine the engine talks to, the Opus decoder behind im_test_opus_decode and libopus's
// version for im_build_info.
//
// Everything 04 asks of 05's audio goes through Engine(), so a test can put a fake AudioEngine behind every call:
// - im_init runs AudioEngine::Init(im_init_params.flags) on the engine worker, im_shutdown runs Shutdown there after
//   it stopped every session (src/engine/engine.cpp);
// - the audio exports forward to it after their argument checks, with their arguments as the header declares them
//   (src/api/exports.cpp): im_list_audio_apps -> ListAppsJson, im_audio_watch -> Watch, im_audio_unwatch -> Unwatch,
//   im_set_exclusions -> SetRules, im_audio_set_mode -> SetMode, im_audio_set_muted -> SetMuted,
//   im_audio_levels_get -> LevelsGet, im_audio_sound_check -> SoundCheck. Refused arguments (IM_AUDIO_MODE_EVERYTHING
//   among them, README §3 item 15) never reach it;
// - a session with IM_AUDIO_MODE_EXCLUDE_APPS (src/session/session_object.cpp, README WA23): SetRules with im_start's
//   copy of cfg->exclusions first when there is one, then NewMixer and Mixer::Start with the session's Opus stage (and
//   PCM tap) as the PcmSink; im_set_paused and IM_START_PAUSED -> Mixer::SetPaused; Mixer::State is the session's
//   audio state; im_stop -> Mixer::Stop. IM_AUDIO_MODE_OFF makes no mixer and IM_AUDIO_MODE_TEST_TONE feeds the
//   stage from 04's tone instead. Audio never fails a share: a refused rule set, no mixer or a mixer that doesn't
//   start is logged, and the share runs without audio packets.
// native/windows/tests/audio_bridge_test.cpp checks all of it against a fake AudioEngine (04 §18.2).
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

// im_test_opus_decode (05 §4.1, for cmd/isshoni-audiotest's case 21): decodes one Opus packet with the DLL's libopus
// into interleaved stereo float pcm (48 kHz), with room for max_frames frames per channel, and returns the frames per
// channel written (960 for one of the engine's 20 ms packets). The process has one decoder, created by the first
// call, whose state is kept between calls as one receiver's is: a caller decodes one stream's packets in order.
// Calls from several threads are serialized. Negative: IM_ERR_INVALID_ARG for a max_frames too small for the packet
// or a malformed packet, IM_ERR_INTERNAL when libopus fails otherwise; the detail starts with "im_test_opus_decode: ".
// Arguments arrive checked (pkt and pcm non-NULL, len and max_frames positive). Works before im_init.
int32_t TestOpusDecode(const uint8_t *pkt, int32_t len, float *pcm, int32_t max_frames) noexcept;

// opus_get_version_string() of the linked libopus ("libopus 1.5.2"): im_build_info's "opus" (04 §5.6).
std::string_view OpusVersion() noexcept;

} // namespace im::audio_bridge
