#include "session/encoder.h"

#include "im/encpolicy/encoders.h"
#include "testhooks/fake_encoder.h"
#include "testhooks/testhooks.h"

namespace im::session {

std::vector<EncoderEntry> EncoderCatalog() {
  std::vector<EncoderEntry> out;
#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
  out.push_back(testhooks::FakeEncoderEntry(IM_ENC_FAKE));
  for (const uint32_t kind : testhooks::Current().fallback_kinds) {
    if (kind != IM_ENC_FAKE && kind != IM_ENC_AUTO && !encpolicy::EncoderToken(kind).empty()) {
      out.push_back(testhooks::FakeEncoderEntry(kind));
    }
  }
#endif
  // README WA44: the probe's encoders (libavcodec, MF; README WA51 OpenH264), in its order, with their open
  // functions.
  return out;
}

} // namespace im::session
