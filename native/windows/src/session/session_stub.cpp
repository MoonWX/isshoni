// The skeleton's session area (README WA11): im_start is not implemented yet (README WA23 builds sessions), so no
// session exists and every handle is unknown. Replaced as a whole by the session slice.
#include "session/session.h"

#include "im/abi/status.h"

namespace im::session {
namespace {

im_status NoSession(const char *fn, im_session s) noexcept {
  return abi::Failf(IM_ERR_INVALID_ARG, {}, "%s: no session 0x%016llx", fn, static_cast<unsigned long long>(s));
}

} // namespace

im_status Start(const api::StartRequest &, StartResult &) {
  return abi::Fail(IM_ERR_NOT_IMPLEMENTED, {}, "im_start: sessions are not implemented yet (README WA23)");
}

im_status GetInfo(im_session s, im_session_info &) { return NoSession("im_get_session_info", s); }
im_status NextPacket(im_session s, uint32_t, im_packet &) { return NoSession("im_next_packet", s); }
im_status RequestKeyframe(im_session s, uint32_t) { return NoSession("im_request_keyframe", s); }
im_status SetBitrate(im_session s, uint32_t, uint32_t) { return NoSession("im_set_bitrate", s); }
im_status SetLayerActive(im_session s, uint32_t, bool) { return NoSession("im_set_layer_active", s); }
im_status UpdateLayer(im_session s, uint32_t, const im_layer_config &) { return NoSession("im_update_layer", s); }
im_status SetProfile(im_session s, uint32_t) { return NoSession("im_set_profile", s); }
im_status SetContentHint(im_session s, uint32_t) { return NoSession("im_set_content_hint", s); }
im_status SetPaused(im_session s, bool) { return NoSession("im_set_paused", s); }
im_status SetAudioBitrate(im_session s, uint32_t) { return NoSession("im_set_audio_bitrate", s); }
im_status GetStats(im_session s, std::string &) { return NoSession("im_get_stats", s); }
im_status Stop(im_session s) { return NoSession("im_stop", s); }
void StopAll() noexcept {}

} // namespace im::session
