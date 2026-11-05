// Which capture runs a session's source (capture_source.h; README WF12, WA60): Desktop Duplication for a monitor whose
// capture path is DD (a Windows 10 monitor pick that mapped, a listed "mon:" id), WGC for a pick of this area's
// picker, the test hooks' factory first in test builds. Each falls back to the other where §7.1 allows:
//   - a Windows 11 monitor pick whose WGC fails to start (IM_ERR_INTERNAL: not a closed source, a denied capture or a
//     lost device) is captured with DD on its mapped output ("if WGC fails to start on a monitor, DD on the same
//     output");
//   - a Windows 10 monitor pick whose output isn't on the session's adapter (DD can't cross adapters, §7.4) is
//     captured with WGC, border shown, like §7.5's step 5.
#include "capture/capture_source.h"

#include <atomic>
#include <string>
#include <utility>

#include "capture/dd_capture.h"
#include "capture/picked_item.h"
#include "capture/wgc_capture.h"
#include "im/abi/status.h"
#include "im/log/ring.h"

namespace im::capture {
namespace {

class WgcFactory final : public CaptureFactory {
public:
  bool Handles(const session::SourceSpec &source) const noexcept override { return WgcCapture::Handles(source); }
  bool NeedsSlots(const session::SourceSpec &) const noexcept override { return true; }
  im_status Start(const session::SourceSpec &source, gpu::FrameSlots *slots, const CaptureOptions &options,
                  CaptureSink *sink, std::unique_ptr<CaptureSource> *out) noexcept override {
    std::unique_ptr<WgcCapture> cap;
    const im_status st = WgcCapture::Start(source, *slots, options, sink, &cap);
    if (st == IM_OK) {
      *out = std::move(cap);
      return IM_OK;
    }
    const std::shared_ptr<PickedItem> picked = PickedItemOf(source);
    if (st != IM_ERR_INTERNAL || source.kind != IM_SOURCE_MONITOR || picked == nullptr || !picked->output) {
      return st;
    }
    // §7.1: DD on the same output. The WGC detail stays in the log only.
    std::string wgc_detail(abi::LastErrorText());
    abi::ClearLastError();
    log::Info("capture", "wgc failed on a monitor, trying dd", {{"detail", wgc_detail}});
    std::unique_ptr<DdCapture> dd;
    if (DdCapture::Start(*picked->output, *slots, options, sink, &dd, nullptr) != IM_OK) {
      log::Info("capture", "dd fallback failed", {{"detail", abi::LastErrorText()}});
      return abi::Fail(st, {}, wgc_detail); // the first failure is the one to report
    }
    *out = std::move(dd);
    return IM_OK;
  }
};

class DdFactory final : public CaptureFactory {
public:
  bool Handles(const session::SourceSpec &source) const noexcept override { return DdCapture::Handles(source); }
  bool NeedsSlots(const session::SourceSpec &) const noexcept override { return true; }
  im_status Start(const session::SourceSpec &source, gpu::FrameSlots *slots, const CaptureOptions &options,
                  CaptureSink *sink, std::unique_ptr<CaptureSource> *out) noexcept override {
    const std::shared_ptr<PickedItem> picked = PickedItemOf(source);
    if (picked == nullptr || !picked->output) {
      return abi::Fail(IM_ERR_INTERNAL, {}, "capture: DD start without an output");
    }
    std::unique_ptr<DdCapture> cap;
    bool cross_adapter = false;
    const im_status st = DdCapture::Start(*picked->output, *slots, options, sink, &cap, &cross_adapter);
    if (st == IM_OK) {
      *out = std::move(cap);
      return IM_OK;
    }
    if (!cross_adapter || picked->item == nullptr) {
      return st; // a listed monitor has no WGC item to fall back to
    }
    // §7.4: DD can't cross adapters; WGC can (border shown on Windows 10).
    abi::ClearLastError();
    log::Info("capture", "dd can't run on the session's adapter, using wgc");
    std::unique_ptr<WgcCapture> wgc;
    if (const im_status w = WgcCapture::Start(source, *slots, options, sink, &wgc); w != IM_OK) {
      return w;
    }
    *out = std::move(wgc);
    return IM_OK;
  }
};

WgcFactory g_wgc;
DdFactory g_dd;

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
  if (g_dd.Handles(source)) {
    return &g_dd;
  }
  if (g_wgc.Handles(source)) {
    return &g_wgc;
  }
  return nullptr;
}

} // namespace

uint64_t OutputAdapterLuid(const session::SourceSpec &source) noexcept {
  if (!DdCapture::Handles(source)) {
    return 0;
  }
  return PickedItemOf(source)->output->adapter_luid;
}

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
