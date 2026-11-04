// Which capture runs a session's source (capture_source.h; README WF12): WGC for a pick of this area's picker, the
// test hooks' factory first in test builds.
#include "capture/capture_source.h"

#include <atomic>
#include <utility>

#include "capture/wgc_capture.h"
#include "im/abi/status.h"

namespace im::capture {
namespace {

class WgcFactory final : public CaptureFactory {
public:
  bool Handles(const session::SourceSpec &source) const noexcept override { return WgcCapture::Handles(source); }
  bool NeedsSlots(const session::SourceSpec &) const noexcept override { return true; }
  im_status Start(const session::SourceSpec &source, gpu::FrameSlots *slots, const CaptureOptions &options,
                  CaptureSink *sink, std::unique_ptr<CaptureSource> *out) noexcept override {
    std::unique_ptr<WgcCapture> cap;
    if (const im_status st = WgcCapture::Start(source, *slots, options, sink, &cap); st != IM_OK) {
      return st;
    }
    *out = std::move(cap);
    return IM_OK;
  }
};

WgcFactory g_wgc;

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
std::atomic<CaptureFactory *> g_test_factory{nullptr};
#endif

// The factory that captures source, or null.
CaptureFactory *FactoryFor(const session::SourceSpec &source) noexcept {
#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
  if (CaptureFactory *f = g_test_factory.load(std::memory_order_acquire); f != nullptr && f->Handles(source)) {
    return f;
  }
#endif
  if (g_wgc.Handles(source)) {
    return &g_wgc;
  }
  return nullptr;
}

} // namespace

bool Captures(const session::SourceSpec &source) noexcept { return FactoryFor(source) != nullptr; }

bool NeedsSlots(const session::SourceSpec &source) noexcept {
  const CaptureFactory *f = FactoryFor(source);
  return f != nullptr && f->NeedsSlots(source);
}

im_status StartSourceCapture(const session::SourceSpec &source, gpu::FrameSlots *slots, const CaptureOptions &options,
                             CaptureSink *sink, std::unique_ptr<CaptureSource> *out) noexcept {
  out->reset();
  CaptureFactory *f = FactoryFor(source);
  if (f == nullptr) {
    return abi::Fail(IM_ERR_INTERNAL, {}, "capture: no capture for this source");
  }
  if (slots == nullptr && f->NeedsSlots(source)) {
    return abi::Fail(IM_ERR_INTERNAL, {}, "capture: the source's capture needs the session's GPU path");
  }
  return f->Start(source, slots, options, sink, out);
}

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
void SetCaptureFactoryForTest(CaptureFactory *factory) noexcept {
  g_test_factory.store(factory, std::memory_order_release);
}
#endif

} // namespace im::capture
