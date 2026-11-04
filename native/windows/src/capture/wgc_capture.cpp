#include "capture/wgc_capture.h"

#include <algorithm>
#include <exception>
#include <mutex>
#include <string>
#include <utility>

#include "capture/picked_item.h"
#include "capture/winrt_support.h"
#include "gpu/device.h"
#include "im/abi/status.h"
#include "im/log/ring.h"

namespace im::capture {
namespace {

namespace d3d = winrt::Windows::Graphics::DirectX::Direct3D11;
using winrt::Windows::Graphics::SizeInt32;

constexpr auto kFormat = winrt::Windows::Graphics::DirectX::DirectXPixelFormat::B8G8R8A8UIntNormalized;
constexpr int32_t kBuffers = 2; // §7.3

std::string TakeDetail() {
  std::string d(abi::LastErrorText());
  abi::ClearLastError();
  return d;
}

} // namespace

struct WgcCapture::Impl {
  std::mutex mu;             // serializes the callbacks, SetFps and Stop's flag
  bool stopped = false;      // guarded by mu
  bool failed = false;       // guarded by mu: OnFailed was reported
  bool min_interval = false; // MinUpdateInterval is present
  gpu::FrameSlots *slots = nullptr;
  CaptureSink *sink = nullptr;
  d3d::IDirect3DDevice device{nullptr};
  wgc::GraphicsCaptureItem item{nullptr};
  wgc::Direct3D11CaptureFramePool pool{nullptr};
  wgc::GraphicsCaptureSession session{nullptr};
  SizeInt32 pool_size{};
  wgc::Direct3D11CaptureFramePool::FrameArrived_revoker arrived;
  wgc::GraphicsCaptureItem::Closed_revoker closed;

  void OnFrame(const wgc::Direct3D11CaptureFramePool &sender) noexcept;
  void OnClosed() noexcept;
  // Reports a failure once (mu held); the thread's detail is taken.
  void FailLocked(im_status st) noexcept;
  void Stop() noexcept;
};

void WgcCapture::Impl::FailLocked(im_status st) noexcept {
  const std::string detail = TakeDetail();
  if (failed) {
    return;
  }
  failed = true;
  log::Error("capture", "wgc capture failed", {{"status", st}, {"detail", detail}});
  sink->OnFailed(st, detail);
}

void WgcCapture::Impl::OnFrame(const wgc::Direct3D11CaptureFramePool &sender) noexcept {
  std::lock_guard<std::mutex> lock(mu);
  if (stopped || failed) {
    return;
  }
  try {
    const wgc::Direct3D11CaptureFrame frame = sender.TryGetNextFrame();
    if (!frame) {
      return;
    }
    const SizeInt32 cs = frame.ContentSize();
    const int64_t ts = frame.SystemRelativeTime().count() * 100; // 100 ns QPC-based units → the engine clock's ns
    winrt::com_ptr<ID3D11Texture2D> texture;
    const auto access = frame.Surface().as<::Windows::Graphics::DirectX::Direct3D11::IDirect3DDxgiInterfaceAccess>();
    winrt::check_hresult(access->GetInterface(__uuidof(ID3D11Texture2D), texture.put_void()));
    D3D11_TEXTURE2D_DESC desc{};
    texture->GetDesc(&desc);
    // Until the pool is recreated the surface keeps the old size: copy what both hold.
    const video::Size content{std::min(static_cast<uint32_t>(std::max(cs.Width, 0)), desc.Width),
                              std::min(static_cast<uint32_t>(std::max(cs.Height, 0)), desc.Height)};
    if (content.width > 0 && content.height > 0) {
      gpu::Slot *slot = nullptr;
      if (const im_status st = slots->BeginWrite(content, &slot); st != IM_OK) {
        frame.Close();
        FailLocked(st);
        return;
      }
      {
        gpu::ContextLock cl(slots->device());
        const D3D11_BOX box{0, 0, 0, content.width, content.height, 1};
        slots->device().context()->CopySubresourceRegion(slot->texture.Get(), 0, 0, 0, 0, texture.get(), 0, &box);
      }
      gpu::FrameMeta meta;
      meta.capture_ts_ns = ts;
      meta.content = content;
      slots->Publish(meta);
    }
    frame.Close();
    if (cs.Width > 0 && cs.Height > 0 && (cs.Width != pool_size.Width || cs.Height != pool_size.Height)) {
      // §7.7: recreate the pool at the new size at once; the session letterboxes until the size settles.
      pool_size = cs;
      sender.Recreate(device, kFormat, kBuffers, cs);
      sink->OnResized(video::Size{static_cast<uint32_t>(cs.Width), static_cast<uint32_t>(cs.Height)});
    }
    if (content.width > 0 && content.height > 0) {
      sink->OnFrame(ts);
    }
  } catch (const winrt::hresult_error &e) {
    // A removed device is the GPU's failure; anything else (RO_E_CLOSED when the source went away) maps through FailHr.
    FailLocked(slots->device().Removed() ? slots->device().Fail(e.code(), "WGC frame") : FailHr(e.code(), "WGC frame"));
  } catch (const std::exception &e) {
    FailLocked(abi::Failf(IM_ERR_INTERNAL, {}, "capture: WGC frame: unexpected exception: %s", e.what()));
  } catch (...) {
    FailLocked(abi::Fail(IM_ERR_INTERNAL, {}, "capture: WGC frame: unexpected exception"));
  }
}

void WgcCapture::Impl::OnClosed() noexcept {
  std::lock_guard<std::mutex> lock(mu);
  if (stopped) {
    return;
  }
  log::Info("capture", "source closed");
  sink->OnClosed();
}

void WgcCapture::Impl::Stop() noexcept {
  {
    std::lock_guard<std::mutex> lock(mu);
    if (stopped) {
      return;
    }
    stopped = true; // callbacks from here on return at once
  }
  // Outside the lock: closing may wait for a callback in flight, which takes the lock.
  try {
    arrived.revoke();
    closed.revoke();
  } catch (...) {
  }
  try {
    if (session) {
      session.Close();
    }
  } catch (...) {
  }
  try {
    if (pool) {
      pool.Close();
    }
  } catch (...) {
  }
  session = nullptr;
  pool = nullptr;
  item = nullptr;
  device = nullptr;
}

bool WgcCapture::Handles(const session::SourceSpec &source) noexcept {
  const std::shared_ptr<PickedItem> picked = PickedItemOf(source);
  return picked != nullptr && picked->item != nullptr;
}

im_status WgcCapture::Start(const session::SourceSpec &source, gpu::FrameSlots &slots, const CaptureOptions &options,
                            CaptureSink *sink, std::unique_ptr<WgcCapture> *out) noexcept {
  out->reset();
  const std::shared_ptr<PickedItem> picked = PickedItemOf(source);
  if (picked == nullptr || picked->item == nullptr || sink == nullptr) {
    return abi::Fail(IM_ERR_INTERNAL, {}, "capture: WGC start without a picked item");
  }
  gpu::Device &dev = slots.device();
  std::shared_ptr<Impl> impl;
  try {
    if (!WgcSupported()) {
      return abi::Fail(IM_ERR_UNSUPPORTED_OS, abi::token::kOsTooOld,
                       "capture: Windows.Graphics.Capture is not supported on this system");
    }
    gpu::ComPtr<IDXGIDevice> dxgi;
    if (const HRESULT hr = dev.d3d()->QueryInterface(IID_PPV_ARGS(&dxgi)); FAILED(hr)) {
      return dev.Fail(hr, "IDXGIDevice");
    }
    winrt::com_ptr<::IInspectable> inspectable;
    if (const HRESULT hr = ::CreateDirect3D11DeviceFromDXGIDevice(dxgi.Get(), inspectable.put()); FAILED(hr)) {
      return dev.Fail(hr, "CreateDirect3D11DeviceFromDXGIDevice");
    }
    impl = std::make_shared<Impl>();
    impl->slots = &slots;
    impl->sink = sink;
    impl->item = picked->item;
    impl->device = inspectable.as<d3d::IDirect3DDevice>();
    impl->pool_size = impl->item.Size();
    if (impl->pool_size.Width <= 0 || impl->pool_size.Height <= 0) {
      return abi::Fail(IM_ERR_SOURCE_GONE, {}, "capture: the picked source has no size any more");
    }
    impl->pool = wgc::Direct3D11CaptureFramePool::CreateFreeThreaded(impl->device, kFormat, kBuffers, impl->pool_size);
    impl->session = impl->pool.CreateCaptureSession(impl->item);

    // §7.8: the pointer is drawn unless IM_START_HIDE_CURSOR.
    const bool cursor_toggle = SessionHasProperty(L"IsCursorCaptureEnabled");
    if (cursor_toggle) {
      impl->session.IsCursorCaptureEnabled(!options.hide_cursor);
    }
    // §7.1: borderless only when the property exists, access is granted and the session reads it back.
    bool borderless = false;
    if (!options.keep_border && BorderlessAllowed()) {
      try {
        impl->session.IsBorderRequired(false);
        borderless = !impl->session.IsBorderRequired();
      } catch (const winrt::hresult_error &e) {
        // Access may have been revoked since BorderlessAllowed cached its answer: capture with the border.
        borderless = false;
        log::Info("capture", "borderless refused, keeping the border", {{"hr", static_cast<int64_t>(e.code().value)}});
      }
    }
    // §7.3: half the frame interval (24H2+); the pacer thins the surplus to fps.
    impl->min_interval = SessionHasProperty(L"MinUpdateInterval");
    if (impl->min_interval && options.fps > 0) {
      impl->session.MinUpdateInterval(winrt::Windows::Foundation::TimeSpan{MinUpdateInterval100ns(options.fps)});
    }
    // §7.1: the shared app's menus and popups (24H2+).
    const bool secondary = SessionHasProperty(L"IncludeSecondaryWindows");
    if (secondary) {
      impl->session.IncludeSecondaryWindows(true);
    }

    const std::weak_ptr<Impl> weak = impl;
    impl->arrived = impl->pool.FrameArrived(winrt::auto_revoke, [weak](const auto &sender, const auto &) {
      if (const std::shared_ptr<Impl> s = weak.lock()) {
        s->OnFrame(sender);
      }
    });
    impl->closed = impl->item.Closed(winrt::auto_revoke, [weak](const auto &, const auto &) {
      if (const std::shared_ptr<Impl> s = weak.lock()) {
        s->OnClosed();
      }
    });
    impl->session.StartCapture();

    std::unique_ptr<WgcCapture> cap(new WgcCapture(impl));
    cap->borderless_ = borderless;
    cap->window_ = picked->identity.window;
    log::Info("capture", "wgc started",
              {{"w", impl->pool_size.Width},
               {"h", impl->pool_size.Height},
               {"borderless", borderless},
               {"cursor", cursor_toggle ? !options.hide_cursor : true},
               {"min_interval", impl->min_interval},
               {"secondary", secondary},
               {"fps", options.fps}});
    *out = std::move(cap);
    return IM_OK;
  } catch (const winrt::hresult_error &e) {
    if (impl) {
      impl->Stop();
    }
    if (dev.Removed()) {
      return dev.Fail(e.code(), "WGC start");
    }
    return FailHr(e.code(), "WGC start");
  } catch (const std::exception &e) {
    if (impl) {
      impl->Stop();
    }
    return abi::Failf(IM_ERR_INTERNAL, {}, "capture: WGC start: unexpected exception: %s", e.what());
  } catch (...) {
    if (impl) {
      impl->Stop();
    }
    return abi::Fail(IM_ERR_INTERNAL, {}, "capture: WGC start: unexpected exception");
  }
}

WgcCapture::WgcCapture(std::shared_ptr<Impl> impl) noexcept : impl_(std::move(impl)) {}

WgcCapture::~WgcCapture() { Stop(); }

void WgcCapture::SetFps(uint32_t fps) noexcept {
  if (!impl_ || fps == 0) {
    return;
  }
  std::lock_guard<std::mutex> lock(impl_->mu);
  if (impl_->stopped || !impl_->min_interval) {
    return;
  }
  try {
    impl_->session.MinUpdateInterval(winrt::Windows::Foundation::TimeSpan{MinUpdateInterval100ns(fps)});
  } catch (const winrt::hresult_error &e) {
    log::Warn("capture", "MinUpdateInterval failed", {{"hr", static_cast<int64_t>(e.code().value)}});
  } catch (...) {
    log::Warn("capture", "MinUpdateInterval failed");
  }
}

void WgcCapture::Stop() noexcept {
  if (impl_) {
    impl_->Stop();
  }
}

} // namespace im::capture
