#include "gpu/device.h"

#include <utility>

#include "api/guard.h"
#include "im/abi/status.h"
#include "im/log/ring.h"

namespace im::gpu {
namespace {

constexpr D3D_FEATURE_LEVEL kLevels[] = {D3D_FEATURE_LEVEL_11_1, D3D_FEATURE_LEVEL_11_0};

unsigned long Hex(HRESULT hr) noexcept { return static_cast<unsigned long>(hr); }

uint64_t LuidValue(const LUID &l) noexcept {
  return (static_cast<uint64_t>(static_cast<uint32_t>(l.HighPart)) << 32) | l.LowPart;
}

// The adapter a request names; null for WARP. IM_OK or IM_ERR_INTERNAL with the detail set.
im_status FindAdapter(const DeviceRequest &req, ComPtr<IDXGIAdapter1> *out) {
  if (req.adapter == DeviceRequest::Adapter::kWarp) {
    return IM_OK;
  }
  ComPtr<IDXGIFactory1> factory;
  HRESULT hr = CreateDXGIFactory1(IID_PPV_ARGS(&factory));
  if (FAILED(hr)) {
    return abi::Failf(IM_ERR_INTERNAL, {}, "gpu: CreateDXGIFactory1 failed (0x%08lx)", Hex(hr));
  }
  if (req.adapter == DeviceRequest::Adapter::kLuid) {
    ComPtr<IDXGIFactory4> factory4;
    hr = factory.As(&factory4);
    if (FAILED(hr)) {
      return abi::Failf(IM_ERR_INTERNAL, {}, "gpu: IDXGIFactory4 is missing (0x%08lx)", Hex(hr));
    }
    LUID luid;
    luid.LowPart = static_cast<DWORD>(req.luid & 0xFFFFFFFFu);
    luid.HighPart = static_cast<LONG>(static_cast<uint32_t>(req.luid >> 32));
    hr = factory4->EnumAdapterByLuid(luid, IID_PPV_ARGS(out->ReleaseAndGetAddressOf()));
    if (FAILED(hr)) {
      // The LUID changes after a driver update (04 §9.8): the probe runs again before the next start relies on it.
      return abi::Failf(IM_ERR_INTERNAL, {}, "gpu: no adapter with LUID 0x%016llx (0x%08lx)",
                        static_cast<unsigned long long>(req.luid), Hex(hr));
    }
    return IM_OK;
  }
  hr = factory->EnumAdapters1(0, out->ReleaseAndGetAddressOf());
  if (FAILED(hr)) {
    return abi::Failf(IM_ERR_INTERNAL, {}, "gpu: no display adapter (0x%08lx)", Hex(hr));
  }
  return IM_OK;
}

} // namespace

Device::Device() noexcept { InitializeCriticalSection(&lock_); }

Device::~Device() {
  video_context_.Reset();
  video_device_.Reset();
  context_.Reset();
  device_.Reset();
  DeleteCriticalSection(&lock_);
}

im_status Device::Create(const DeviceRequest &request, std::unique_ptr<Device> *out) noexcept {
  out->reset();
  return api::Guard("gpu device", [&]() -> im_status {
    ComPtr<IDXGIAdapter1> adapter;
    if (const im_status st = FindAdapter(request, &adapter); st != IM_OK) {
      return st;
    }
    const D3D_DRIVER_TYPE type =
        request.adapter == DeviceRequest::Adapter::kWarp ? D3D_DRIVER_TYPE_WARP : D3D_DRIVER_TYPE_UNKNOWN;
    std::unique_ptr<Device> d(new Device());
    bool video = request.video_support;
    UINT flags = D3D11_CREATE_DEVICE_BGRA_SUPPORT | (video ? D3D11_CREATE_DEVICE_VIDEO_SUPPORT : 0u);
    HRESULT hr =
        D3D11CreateDevice(adapter.Get(), type, nullptr, flags, kLevels, ARRAYSIZE(kLevels), D3D11_SDK_VERSION,
                          d->device_.ReleaseAndGetAddressOf(), &d->level_, d->context_.ReleaseAndGetAddressOf());
    if (FAILED(hr) && video) {
      // A device without video support still runs the shader converter (04 §8.3).
      log::Warn("gpu", "device without video support", {{"hr", static_cast<uint32_t>(hr)}});
      video = false;
      flags = D3D11_CREATE_DEVICE_BGRA_SUPPORT;
      hr = D3D11CreateDevice(adapter.Get(), type, nullptr, flags, kLevels, ARRAYSIZE(kLevels), D3D11_SDK_VERSION,
                             d->device_.ReleaseAndGetAddressOf(), &d->level_, d->context_.ReleaseAndGetAddressOf());
    }
    if (FAILED(hr)) {
      return abi::Failf(IM_ERR_INTERNAL, {}, "gpu: D3D11CreateDevice failed (0x%08lx)", Hex(hr));
    }

    // The adapter the device runs on (WARP's too): its LUID and PCI ids.
    ComPtr<IDXGIDevice> dxgi;
    ComPtr<IDXGIAdapter> used;
    DXGI_ADAPTER_DESC desc{};
    if (SUCCEEDED(d->device_.As(&dxgi)) && SUCCEEDED(dxgi->GetAdapter(used.GetAddressOf())) &&
        SUCCEEDED(used->GetDesc(&desc))) {
      d->luid_ = LuidValue(desc.AdapterLuid);
      d->vendor_id_ = desc.VendorId;
      d->device_id_ = desc.DeviceId;
    }
    d->warp_ = type == D3D_DRIVER_TYPE_WARP ||
               (d->vendor_id_ == kVendorMicrosoft && d->device_id_ == kBasicRenderDriverDevice);

    // A safety net: our lock already serializes every use of the context (04 §8.1).
    ComPtr<ID3D11Multithread> mt;
    if (SUCCEEDED(d->context_.As(&mt))) {
      mt->SetMultithreadProtected(TRUE);
    }

    if (video) {
      ComPtr<ID3D11VideoContext> vc;
      if (FAILED(d->device_.As(&d->video_device_)) || FAILED(d->context_.As(&vc)) ||
          FAILED(vc.As(&d->video_context_))) {
        d->video_device_.Reset();
        d->video_context_.Reset();
      }
    }

    UINT support = 0;
    if (SUCCEEDED(d->device_->CheckFormatSupport(DXGI_FORMAT_NV12, &support))) {
      d->nv12_render_target_ =
          (support & D3D11_FORMAT_SUPPORT_TEXTURE2D) != 0 && (support & D3D11_FORMAT_SUPPORT_RENDER_TARGET) != 0;
    }

    log::Info("gpu", "device created",
              {{"vendor", d->vendor_id_},
               {"device", d->device_id_},
               {"warp", d->warp_},
               {"level", static_cast<uint32_t>(d->level_)},
               {"video", d->video_device_.Get() != nullptr},
               {"nv12_rt", d->nv12_render_target_}});
    *out = std::move(d);
    return IM_OK;
  });
}

void Device::Lock() noexcept { EnterCriticalSection(&lock_); }

void Device::Unlock() noexcept { LeaveCriticalSection(&lock_); }

void Device::LockCallback(void *ctx) noexcept { static_cast<Device *>(ctx)->Lock(); }

void Device::UnlockCallback(void *ctx) noexcept { static_cast<Device *>(ctx)->Unlock(); }

bool Device::Removed() const noexcept { return device_ && device_->GetDeviceRemovedReason() != S_OK; }

im_status Device::Fail(HRESULT hr, const char *what) const noexcept {
  if (hr == DXGI_ERROR_DEVICE_REMOVED || hr == DXGI_ERROR_DEVICE_RESET || Removed()) {
    const HRESULT reason = device_ ? device_->GetDeviceRemovedReason() : hr;
    return abi::Failf(IM_ERR_ENCODER_LOST, abi::token::kDeviceRemoved,
                      "gpu: %s: the device was removed or reset (0x%08lx, reason 0x%08lx)", what, Hex(hr), Hex(reason));
  }
  return abi::Failf(IM_ERR_INTERNAL, {}, "gpu: %s failed (0x%08lx)", what, Hex(hr));
}

} // namespace im::gpu
