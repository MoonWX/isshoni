#include "capture/dd_capture.h"

#include <windows.h>

#include <d3d11.h>
#include <dxgi1_6.h>

#include <algorithm>
#include <atomic>
#include <cstddef>
#include <exception>
#include <limits>
#include <mutex>
#include <span>
#include <string>
#include <string_view>
#include <thread>
#include <utility>
#include <vector>

#include "capture/dd_policy.h"
#include "capture/outputs_win.h"
#include "capture/picked_item.h"
#include "engine/clock.h"
#include "engine/os.h"
#include "gpu/cursor.h"
#include "gpu/cursor_shape.h"
#include "gpu/device.h"
#include "gpu/geometry.h"
#include "im/abi/status.h"
#include "im/log/ring.h"

namespace im::capture {
namespace {

using gpu::ComPtr;

constexpr int64_t kNoTime = std::numeric_limits<int64_t>::min();

std::string TakeDetail() {
  std::string d(abi::LastErrorText());
  abi::ClearLastError();
  return d;
}

unsigned long Hex(HRESULT hr) noexcept { return static_cast<unsigned long>(hr); }

int64_t QpcFrequency() noexcept {
  static const int64_t f = [] {
    LARGE_INTEGER li{};
    ::QueryPerformanceFrequency(&li);
    return li.QuadPart > 0 ? li.QuadPart : int64_t{10'000'000};
  }();
  return f;
}

// A DXGI QPC reading in the engine clock's ns (04 §12.3).
int64_t TicksToNs(const LARGE_INTEGER &t) noexcept { return engine::QpcToNs(t.QuadPart, QpcFrequency()); }

video::Size UprightOf(const DXGI_OUTPUT_DESC &d) noexcept {
  return video::Size{static_cast<uint32_t>(std::max<LONG>(d.DesktopCoordinates.right - d.DesktopCoordinates.left, 0)),
                     static_cast<uint32_t>(std::max<LONG>(d.DesktopCoordinates.bottom - d.DesktopCoordinates.top, 0))};
}

// The DXGI adapter of a D3D11 device.
ComPtr<IDXGIAdapter> AdapterOf(ID3D11Device *device) noexcept {
  ComPtr<IDXGIDevice> dxgi;
  ComPtr<IDXGIAdapter> adapter;
  if (FAILED(device->QueryInterface(IID_PPV_ARGS(&dxgi))) || FAILED(dxgi->GetAdapter(&adapter))) {
    return nullptr;
  }
  return adapter;
}

// The attached output named device_name (UTF-8) among adapter's, or null; *desc gets its description.
ComPtr<IDXGIOutput1> OutputOn(IDXGIAdapter *adapter, std::string_view device_name, DXGI_OUTPUT_DESC *desc) {
  ComPtr<IDXGIOutput> o;
  for (UINT j = 0; SUCCEEDED(adapter->EnumOutputs(j, o.ReleaseAndGetAddressOf())); ++j) {
    DXGI_OUTPUT_DESC d{};
    if (FAILED(o->GetDesc(&d)) || !d.AttachedToDesktop ||
        Utf8FromWide(std::wstring_view(d.DeviceName)) != device_name) {
      continue;
    }
    ComPtr<IDXGIOutput1> o1;
    if (FAILED(o.As(&o1))) {
      return nullptr;
    }
    *desc = d;
    return o1;
  }
  return nullptr; // DXGI_ERROR_NOT_FOUND: no such output on this adapter
}

// Whether an adapter other than the one with LUID `except` has the output (§7.4's cross-adapter case).
bool OutputElsewhere(std::string_view device_name, uint64_t except) {
  ComPtr<IDXGIFactory1> factory;
  if (FAILED(::CreateDXGIFactory1(IID_PPV_ARGS(&factory)))) {
    return false;
  }
  ComPtr<IDXGIAdapter1> a;
  for (UINT i = 0; SUCCEEDED(factory->EnumAdapters1(i, a.ReleaseAndGetAddressOf())); ++i) {
    DXGI_ADAPTER_DESC1 ad{};
    if (FAILED(a->GetDesc1(&ad))) {
      continue;
    }
    const uint64_t luid =
        (static_cast<uint64_t>(static_cast<uint32_t>(ad.AdapterLuid.HighPart)) << 32) | ad.AdapterLuid.LowPart;
    DXGI_OUTPUT_DESC d{};
    if (luid != except && OutputOn(a.Get(), device_name, &d) != nullptr) {
      return true;
    }
  }
  return false;
}

// DuplicateOutput1 with 8-bit BGRA only (§7.4, §7.9); DuplicateOutput where IDXGIOutput5 is missing.
HRESULT DuplicateOn(ID3D11Device *device, IDXGIOutput1 *output, ComPtr<IDXGIOutputDuplication> *out) noexcept {
  ComPtr<IDXGIOutput5> o5;
  if (SUCCEEDED(output->QueryInterface(IID_PPV_ARGS(&o5)))) {
    const DXGI_FORMAT formats[] = {DXGI_FORMAT_B8G8R8A8_UNORM};
    return o5->DuplicateOutput1(device, 0, 1, formats, out->ReleaseAndGetAddressOf());
  }
  return output->DuplicateOutput(device, out->ReleaseAndGetAddressOf());
}

// The failures that interrupt a duplication rather than end it (§7.4): access lost (a mode change, the secure desktop,
// a full-screen exclusive app), access denied (the secure desktop when the duplication is created), a disconnected
// session (remote desktop).
bool Interrupts(HRESULT hr) noexcept {
  return hr == DXGI_ERROR_ACCESS_LOST || hr == E_ACCESSDENIED || hr == DXGI_ERROR_SESSION_DISCONNECTED;
}

} // namespace

struct DdCapture::Impl {
  Impl(gpu::FrameSlots &s, CaptureSink *k, const DisplayOutput &t, const CaptureOptions &o)
      : slots(s), dev(s.device()), sink(k), target(t), hide_cursor(o.hide_cursor), fps(o.fps) {}
  ~Impl() {
    if (stop_event != nullptr) {
      ::CloseHandle(stop_event);
    }
  }
  Impl(const Impl &) = delete;
  Impl &operator=(const Impl &) = delete;

  gpu::FrameSlots &slots;
  gpu::Device &dev;
  CaptureSink *const sink;
  const DisplayOutput target;
  const bool hide_cursor;
  std::atomic<uint32_t> fps;

  HANDLE stop_event = ::CreateEventW(nullptr, TRUE, FALSE, nullptr);
  std::atomic<bool> stopping{false};
  std::mutex stop_mu; // one Stop joins
  std::thread th;

  // The DD thread's (Start's before the thread runs).
  std::unique_ptr<gpu::CursorOverlay> overlay; // null with hide_cursor
  ComPtr<IDXGIOutputDuplication> dup;          // null while interrupted
  gpu::Rotation rotation = gpu::Rotation::kIdentity;
  uint32_t dxgi_rotation = kDxgiRotationIdentity;
  video::Size upright;           // the size last reported (Start's, then OnResized's)
  ComPtr<ID3D11Texture2D> clean; // the last desktop image without the pointer (the pointer's own frames)
  video::Size clean_size;
  bool have_clean = false;
  gpu::CursorState cursor;
  bool cursor_dirty = false; // the pointer changed since the last published frame
  int64_t cursor_ts = kNoTime;
  std::vector<uint8_t> shape;
  bool shape_warned = false;
  bool blend_warned = false;
  int64_t last_ts = kNoTime;      // the last published frame's capture time
  int64_t last_publish = kNoTime; // when it was published (engine clock)
  DuplicationRecovery recovery;
  std::string_view cause = kCauseAccessLost;
  bool start_interrupted = false;
  bool failed = false;

  void Run() noexcept;
  void Loop();
  bool Wait(uint32_t ms) noexcept { return ::WaitForSingleObject(stop_event, ms) == WAIT_OBJECT_0; }
  void Fail(im_status st) noexcept;
  // The duplication is lost: CAPTURE_INTERRUPTED once.
  void Lose(int64_t now);
  // One re-creation attempt; false when the thread ends (gone, failed).
  bool Recover(int64_t now);
  void UpdatePointer(const DXGI_OUTDUPL_FRAME_INFO &fi);
  im_status CopyDesktop(IDXGIResource *resource, int64_t ts);
  im_status PublishPointer();
  im_status Publish(gpu::Slot *slot, video::Size region, int64_t ts);
  bool ModeChanged();
};

void DdCapture::Impl::Fail(im_status st) noexcept {
  const std::string detail = TakeDetail();
  if (failed) {
    return;
  }
  failed = true;
  log::Error("capture", "dd capture failed", {{"status", st}, {"detail", detail}});
  sink->OnFailed(st, detail);
}

bool DdCapture::Impl::ModeChanged() {
  const ComPtr<IDXGIAdapter> adapter = AdapterOf(dev.d3d());
  DXGI_OUTPUT_DESC d{};
  if (!adapter || OutputOn(adapter.Get(), target.device_name, &d) == nullptr) {
    return false; // gone (or unplugged): the recovery tells
  }
  return !(UprightOf(d) == upright) || static_cast<uint32_t>(d.Rotation) != dxgi_rotation;
}

void DdCapture::Impl::Lose(int64_t now) {
  dup.Reset();
  have_clean = false; // the next duplication's first frame brings a whole desktop again
  const std::string_view why = InterruptCause(SecureDesktopActive(), ModeChanged());
  if (recovery.Lost(now)) {
    cause = why;
    log::Info("capture", "dd interrupted", {{"cause", cause}});
    sink->OnInterrupted(cause);
  }
}

bool DdCapture::Impl::Recover(int64_t now) {
  const ComPtr<IDXGIAdapter> adapter = AdapterOf(dev.d3d());
  DXGI_OUTPUT_DESC d{};
  const ComPtr<IDXGIOutput1> o = adapter ? OutputOn(adapter.Get(), target.device_name, &d) : nullptr;
  ComPtr<IDXGIOutputDuplication> fresh;
  bool recreated = false;
  if (o) {
    const HRESULT hr = DuplicateOn(dev.d3d(), o.Get(), &fresh);
    if (SUCCEEDED(hr)) {
      recreated = true;
    } else if (dev.Removed()) {
      Fail(dev.Fail(hr, "DuplicateOutput1"));
      return false;
    }
    // Otherwise still the secure desktop, a mode switch in progress or too many duplications: try again.
  }
  switch (recovery.Attempt(now, o != nullptr, recreated)) {
  case DuplicationRecovery::Outcome::kGone:
    log::Info("capture", "dd output gone");
    sink->OnClosed();
    return false;
  case DuplicationRecovery::Outcome::kWaiting:
    return true;
  case DuplicationRecovery::Outcome::kResumed:
    break;
  }
  dup = std::move(fresh);
  DXGI_OUTDUPL_DESC dd{};
  dup->GetDesc(&dd);
  dxgi_rotation = static_cast<uint32_t>(dd.Rotation);
  rotation = gpu::RotationFromDxgi(dxgi_rotation);
  const video::Size now_upright = UprightOf(d);
  log::Info("capture", "dd resumed",
            {{"cause", cause},
             {"w", now_upright.width},
             {"h", now_upright.height},
             {"rotation", RotationDegrees(dxgi_rotation)}});
  start_interrupted = false;
  sink->OnResumed(cause);
  if (!(now_upright == upright) && now_upright.width > 0 && now_upright.height > 0) {
    upright = now_upright;
    sink->OnResized(upright); // §7.7: a mode change takes the resize path
  }
  return true;
}

void DdCapture::Impl::UpdatePointer(const DXGI_OUTDUPL_FRAME_INFO &fi) {
  if (hide_cursor) {
    return; // §7.8: nothing is drawn, so nothing is read
  }
  if (fi.LastMouseUpdateTime.QuadPart != 0) {
    gpu::CursorState c;
    c.visible = fi.PointerPosition.Visible != FALSE; // false while the pointer is on another monitor
    c.x = fi.PointerPosition.Position.x;
    c.y = fi.PointerPosition.Position.y;
    if (c.visible != cursor.visible || (c.visible && (c.x != cursor.x || c.y != cursor.y))) {
      cursor_dirty = true;
    }
    cursor = c;
    cursor_ts = TicksToNs(fi.LastMouseUpdateTime);
  }
  if (fi.PointerShapeBufferSize == 0) {
    return;
  }
  shape.resize(fi.PointerShapeBufferSize);
  UINT needed = 0;
  DXGI_OUTDUPL_POINTER_SHAPE_INFO si{};
  const HRESULT hr = dup->GetFramePointerShape(fi.PointerShapeBufferSize, shape.data(), &needed, &si);
  gpu::CursorImage image;
  const gpu::PointerShape ps{si.Type, si.Width, si.Height, si.Pitch};
  if (SUCCEEDED(hr) &&
      gpu::ConvertPointerShape(ps, std::span<const uint8_t>(shape.data(), std::min<std::size_t>(needed, shape.size())),
                               &image)) {
    overlay->SetShape(image);
    cursor_dirty = true;
  } else if (!shape_warned) {
    shape_warned = true; // the previous shape stays
    log::Warn("capture", "dd pointer shape unreadable", {{"hr", Hex(hr)}, {"type", si.Type}});
  }
}

im_status DdCapture::Impl::Publish(gpu::Slot *slot, video::Size region, int64_t ts) {
  gpu::FrameMeta meta;
  meta.capture_ts_ns = ts;
  meta.content = region;
  meta.rotation = rotation;
  meta.cursor = cursor;
  if (overlay && cursor.visible) {
    // §7.8: into the slot copy before it is published (the reader can't see this slot yet).
    slot->meta = meta;
    if (const im_status st = overlay->Blend(*slot); st != IM_OK) {
      if (dev.Removed()) {
        return st;
      }
      if (!blend_warned) {
        blend_warned = true;
        log::Warn("capture", "dd pointer blend failed", {{"detail", abi::LastErrorText()}});
      }
      abi::ClearLastError();
    }
  }
  meta.cursor.visible = false; // drawn already: nothing is left for the video thread to draw (gpu/cursor.h)
  slots.Publish(meta);
  last_ts = ts;
  last_publish = engine::os::QpcNs();
  cursor_dirty = false;
  return IM_OK;
}

im_status DdCapture::Impl::CopyDesktop(IDXGIResource *resource, int64_t ts) {
  ComPtr<ID3D11Texture2D> texture;
  if (const HRESULT hr = resource->QueryInterface(IID_PPV_ARGS(&texture)); FAILED(hr)) {
    return dev.Fail(hr, "the duplicated desktop's texture");
  }
  D3D11_TEXTURE2D_DESC desc{};
  texture->GetDesc(&desc);
  const video::Size region{desc.Width, desc.Height}; // the panel's orientation
  if (region.width == 0 || region.height == 0) {
    return IM_OK;
  }
  if (const video::Size up = gpu::Upright(region, rotation); !(up == upright)) {
    upright = up; // a size change without ACCESS_LOST first: the same resize path (§7.7)
    sink->OnResized(upright);
  }
  if (!hide_cursor && (!clean || !(clean_size == region))) {
    D3D11_TEXTURE2D_DESC cd = desc;
    cd.MipLevels = 1;
    cd.ArraySize = 1;
    cd.Usage = D3D11_USAGE_DEFAULT;
    cd.BindFlags = 0;
    cd.CPUAccessFlags = 0;
    cd.MiscFlags = 0;
    clean.Reset();
    have_clean = false;
    if (const HRESULT hr = dev.d3d()->CreateTexture2D(&cd, nullptr, clean.GetAddressOf()); FAILED(hr)) {
      return dev.Fail(hr, "CreateTexture2D (the desktop's copy)");
    }
    clean_size = region;
  }
  gpu::Slot *slot = nullptr;
  if (const im_status st = slots.BeginWrite(region, &slot); st != IM_OK) {
    return st;
  }
  {
    gpu::ContextLock cl(dev);
    const D3D11_BOX box{0, 0, 0, region.width, region.height, 1};
    dev.context()->CopySubresourceRegion(slot->texture.Get(), 0, 0, 0, 0, texture.Get(), 0, &box);
    if (clean) {
      dev.context()->CopyResource(clean.Get(), texture.Get());
    }
  }
  have_clean = clean != nullptr;
  return Publish(slot, region, ts);
}

im_status DdCapture::Impl::PublishPointer() {
  gpu::Slot *slot = nullptr;
  if (const im_status st = slots.BeginWrite(clean_size, &slot); st != IM_OK) {
    return st;
  }
  {
    gpu::ContextLock cl(dev);
    const D3D11_BOX box{0, 0, 0, clean_size.width, clean_size.height, 1};
    dev.context()->CopySubresourceRegion(slot->texture.Get(), 0, 0, 0, 0, clean.Get(), 0, &box);
  }
  const int64_t ts = MonotonicTs(last_ts, cursor_ts == kNoTime ? engine::os::QpcNs() : cursor_ts);
  if (const im_status st = Publish(slot, clean_size, ts); st != IM_OK) {
    return st;
  }
  sink->OnFrame(ts);
  return IM_OK;
}

void DdCapture::Impl::Loop() {
  while (!stopping.load(std::memory_order_acquire)) {
    const int64_t now = engine::os::QpcNs();
    if (!dup) {
      if (const int64_t due = recovery.NextRetryNs(); due > now) {
        if (Wait(AcquireTimeoutMs(now, due))) {
          return;
        }
        continue;
      }
      if (!Recover(now)) {
        return;
      }
      continue;
    }
    const int64_t pointer_due =
        cursor_dirty && have_clean ? PointerDueNs(last_publish, fps.load(std::memory_order_relaxed)) : kDdNever;
    if (pointer_due <= now) {
      if (const im_status st = PublishPointer(); st != IM_OK) {
        Fail(st);
        return;
      }
      continue;
    }
    DXGI_OUTDUPL_FRAME_INFO fi{};
    ComPtr<IDXGIResource> resource;
    const HRESULT hr = dup->AcquireNextFrame(AcquireTimeoutMs(now, pointer_due), &fi, &resource);
    if (hr == DXGI_ERROR_WAIT_TIMEOUT) {
      continue; // a static screen
    }
    if (FAILED(hr)) {
      if (dev.Removed()) {
        Fail(dev.Fail(hr, "AcquireNextFrame"));
        return;
      }
      if (Interrupts(hr)) {
        Lose(engine::os::QpcNs());
        continue;
      }
      Fail(abi::Failf(IM_ERR_INTERNAL, {}, "capture: AcquireNextFrame failed (0x%08lX)", Hex(hr)));
      return;
    }
    UpdatePointer(fi); // before ReleaseFrame: GetFramePointerShape reads the acquired frame
    im_status st = IM_OK;
    int64_t frame_ts = kNoTime;
    if (fi.LastPresentTime.QuadPart != 0 && resource) {
      frame_ts = MonotonicTs(last_ts, TicksToNs(fi.LastPresentTime));
      st = CopyDesktop(resource.Get(), frame_ts);
    }
    resource.Reset();
    const HRESULT released = dup->ReleaseFrame(); // right after the copy (§7.4)
    if (st != IM_OK) {
      Fail(st);
      return;
    }
    if (frame_ts != kNoTime && last_ts == frame_ts) {
      sink->OnFrame(frame_ts);
    }
    if (FAILED(released)) {
      if (dev.Removed()) {
        Fail(dev.Fail(released, "ReleaseFrame"));
        return;
      }
      if (Interrupts(released)) {
        Lose(engine::os::QpcNs());
      }
    }
  }
}

void DdCapture::Impl::Run() noexcept {
  const uintptr_t token = engine::os::EnterThread("isshoni dd", engine::os::ThreadClass::kCapture);
  ::SetThreadDpiAwarenessContext(DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2); // §7.4
  try {
    if (start_interrupted) {
      log::Info("capture", "dd interrupted", {{"cause", cause}});
      sink->OnInterrupted(cause);
    }
    Loop();
  } catch (const std::exception &e) {
    Fail(abi::Failf(IM_ERR_INTERNAL, {}, "capture: DD thread: unexpected exception: %s", e.what()));
  } catch (...) {
    Fail(abi::Fail(IM_ERR_INTERNAL, {}, "capture: DD thread: unexpected exception"));
  }
  engine::os::LeaveThread(token);
}

bool DdCapture::Handles(const session::SourceSpec &source) noexcept {
  if (source.capture_path != IM_CAPTURE_DD) {
    return false;
  }
  const std::shared_ptr<PickedItem> picked = PickedItemOf(source);
  return picked != nullptr && picked->output.has_value();
}

im_status DdCapture::Start(const DisplayOutput &output, gpu::FrameSlots &slots, const CaptureOptions &options,
                           CaptureSink *sink, std::unique_ptr<DdCapture> *out, bool *cross_adapter) noexcept {
  out->reset();
  if (cross_adapter != nullptr) {
    *cross_adapter = false;
  }
  if (sink == nullptr) {
    return abi::Fail(IM_ERR_INTERNAL, {}, "capture: DD start without a sink");
  }
  gpu::Device &dev = slots.device();
  try {
    if (!ProcessPerMonitorAware()) {
      return abi::Fail(IM_ERR_INTERNAL, abi::token::kDpiUnaware,
                       "capture: Desktop Duplication needs a per-monitor DPI-aware process");
    }
    const ThreadDpiScope dpi;
    auto impl = std::make_unique<Impl>(slots, sink, output, options);
    if (impl->stop_event == nullptr) {
      return abi::Fail(IM_ERR_INTERNAL, {}, "capture: CreateEvent failed");
    }
    const ComPtr<IDXGIAdapter> adapter = AdapterOf(dev.d3d());
    if (!adapter) {
      return abi::Fail(IM_ERR_INTERNAL, {}, "capture: the session's device has no DXGI adapter");
    }
    DXGI_OUTPUT_DESC od{};
    const ComPtr<IDXGIOutput1> o = OutputOn(adapter.Get(), output.device_name, &od);
    if (!o) {
      if (OutputElsewhere(output.device_name, dev.luid())) {
        if (cross_adapter != nullptr) {
          *cross_adapter = true;
        }
        return abi::Fail(IM_ERR_INTERNAL, {},
                         "capture: the monitor is on another adapter than the session's device (DD can't cross "
                         "adapters)");
      }
      return abi::Fail(IM_ERR_SOURCE_GONE, {}, "capture: the monitor is no longer attached");
    }
    impl->upright = UprightOf(od);
    impl->dxgi_rotation = static_cast<uint32_t>(od.Rotation);
    impl->rotation = gpu::RotationFromDxgi(impl->dxgi_rotation);
    if (impl->upright.width == 0 || impl->upright.height == 0) {
      return abi::Fail(IM_ERR_SOURCE_GONE, {}, "capture: the monitor has no desktop area");
    }
    const HRESULT hr = DuplicateOn(dev.d3d(), o.Get(), &impl->dup);
    if (SUCCEEDED(hr)) {
      DXGI_OUTDUPL_DESC dd{};
      impl->dup->GetDesc(&dd);
      impl->dxgi_rotation = static_cast<uint32_t>(dd.Rotation);
      impl->rotation = gpu::RotationFromDxgi(impl->dxgi_rotation);
    } else if (dev.Removed()) {
      return dev.Fail(hr, "DuplicateOutput1");
    } else if (hr == DXGI_ERROR_UNSUPPORTED) {
      if (cross_adapter != nullptr) {
        *cross_adapter = true; // a hybrid laptop's output that DXGI won't duplicate on this adapter
      }
      return abi::Failf(IM_ERR_INTERNAL, {}, "capture: DuplicateOutput1 is unsupported here (0x%08lX)", Hex(hr));
    } else if (hr == DXGI_ERROR_NOT_CURRENTLY_AVAILABLE) {
      return abi::Fail(IM_ERR_LIMIT, {}, "capture: the monitor has too many duplications already");
    } else if (Interrupts(hr)) {
      // The secure desktop or a mode switch right now: start interrupted; the thread re-creates it (§7.4).
      impl->start_interrupted = true;
      impl->cause = InterruptCause(SecureDesktopActive(), false);
      impl->recovery.Lost(engine::os::QpcNs());
    } else {
      return abi::Failf(IM_ERR_INTERNAL, {}, "capture: DuplicateOutput1 failed (0x%08lX)", Hex(hr));
    }
    if (!options.hide_cursor) {
      if (const im_status st = gpu::CursorOverlay::Create(dev, 0, &impl->overlay); st != IM_OK) {
        return st;
      }
    }
    const video::Size size = impl->upright;
    const uint32_t degrees = RotationDegrees(impl->dxgi_rotation);
    const bool interrupted = impl->start_interrupted;
    Impl *raw = impl.get();
    impl->th = std::thread([raw] { raw->Run(); });
    std::unique_ptr<DdCapture> cap(new DdCapture(std::move(impl)));
    cap->size_ = size;
    log::Info("capture", "dd started",
              {{"w", size.width},
               {"h", size.height},
               {"rotation", degrees},
               {"cursor", !options.hide_cursor},
               {"interrupted", interrupted},
               {"fps", options.fps}});
    *out = std::move(cap);
    return IM_OK;
  } catch (const std::exception &e) {
    return abi::Failf(IM_ERR_INTERNAL, {}, "capture: DD start: unexpected exception: %s", e.what());
  } catch (...) {
    return abi::Fail(IM_ERR_INTERNAL, {}, "capture: DD start: unexpected exception");
  }
}

DdCapture::DdCapture(std::unique_ptr<Impl> impl) noexcept : impl_(std::move(impl)) {}

DdCapture::~DdCapture() { Stop(); }

void DdCapture::SetFps(uint32_t fps) noexcept {
  if (impl_ && fps > 0) {
    impl_->fps.store(fps, std::memory_order_relaxed);
  }
}

void DdCapture::Stop() noexcept {
  if (!impl_) {
    return;
  }
  std::lock_guard<std::mutex> lock(impl_->stop_mu);
  impl_->stopping.store(true, std::memory_order_release);
  ::SetEvent(impl_->stop_event);
  if (impl_->th.joinable()) {
    impl_->th.join(); // AcquireNextFrame waits at most kDdMaxWaitMs, every other wait is on stop_event
  }
  // Nothing of the device is used from here on (the session frees the slots and the device after Stop).
  impl_->dup.Reset();
  impl_->clean.Reset();
  impl_->overlay.reset();
}

} // namespace im::capture
