#include "session/encoder.h"

#include <mutex>
#include <utility>

#include "im/abi/status.h"
#include "im/encpolicy/encoders.h"
#include "testhooks/fake_encoder.h"
#include "testhooks/testhooks.h"

namespace im::session {

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
namespace {

struct Prefix {
  std::mutex mu;
  std::vector<EncoderEntry> entries;
};

Prefix &ThePrefix() noexcept {
  static Prefix *const p = new Prefix(); // never destroyed, like the engine
  return *p;
}

} // namespace

void SetCatalogPrefixForTest(std::vector<EncoderEntry> entries) {
  Prefix &p = ThePrefix();
  std::lock_guard<std::mutex> lock(p.mu);
  p.entries = std::move(entries);
}
#endif

im_status VideoEncoder::NextTarget(gpu::Nv12Target *out) noexcept {
  *out = gpu::Nv12Target{};
  return abi::Fail(IM_ERR_NOT_IMPLEMENTED, {}, "encoder: this encoder takes no picture (FrameInput::kNone)");
}

std::vector<EncoderEntry> EncoderCatalog() {
  std::vector<EncoderEntry> out;
#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
  {
    Prefix &p = ThePrefix();
    std::lock_guard<std::mutex> lock(p.mu);
    out = p.entries;
  }
  // The fake encoders ignore pixels: a session of them runs no GPU path (encoder.h, FrameInput::kNone).
  const auto add_fake = [&](uint32_t kind) {
    EncoderEntry e = testhooks::FakeEncoderEntry(kind);
    e.input = FrameInput::kNone;
    out.push_back(e);
  };
  add_fake(IM_ENC_FAKE);
  for (const uint32_t kind : testhooks::Current().fallback_kinds) {
    if (kind != IM_ENC_FAKE && kind != IM_ENC_AUTO && !encpolicy::EncoderToken(kind).empty()) {
      add_fake(kind);
    }
  }
#endif
  // README WA44: the probe's encoders (libavcodec, MF; README WA51 OpenH264), in its order, with their open
  // functions, FrameInput::kNv12 and the device request of their adapter.
  return out;
}

} // namespace im::session
