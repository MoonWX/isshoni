#include "capture/outputs_win.h"

#include <windows.h>

#include <dxgi1_6.h>
#include <shellscalingapi.h>
#include <wrl/client.h>

#include <exception>
#include <map>
#include <string>
#include <string_view>

#include "capture/window_match.h"
#include "im/log/ring.h"
#include "isshoni_media.h"

namespace im::capture {
namespace {

using Microsoft::WRL::ComPtr;

uint64_t LuidValue(const LUID &l) noexcept {
  return (static_cast<uint64_t>(static_cast<uint32_t>(l.HighPart)) << 32) | l.LowPart;
}

// The GDI device name of each active display path's source ("\\.\DISPLAY1") → its monitor's friendly name (the
// first non-empty one: a cloned source drives several monitors).
std::map<std::wstring, std::wstring> FriendlyNames() {
  std::map<std::wstring, std::wstring> out;
  UINT32 paths = 0;
  UINT32 modes = 0;
  if (::GetDisplayConfigBufferSizes(QDC_ONLY_ACTIVE_PATHS, &paths, &modes) != ERROR_SUCCESS) {
    return out;
  }
  std::vector<DISPLAYCONFIG_PATH_INFO> p(paths);
  std::vector<DISPLAYCONFIG_MODE_INFO> m(modes);
  if (::QueryDisplayConfig(QDC_ONLY_ACTIVE_PATHS, &paths, p.data(), &modes, m.data(), nullptr) != ERROR_SUCCESS) {
    return out;
  }
  for (UINT32 i = 0; i < paths; ++i) {
    DISPLAYCONFIG_SOURCE_DEVICE_NAME source{};
    source.header.type = DISPLAYCONFIG_DEVICE_INFO_GET_SOURCE_NAME;
    source.header.size = sizeof source;
    source.header.adapterId = p[i].sourceInfo.adapterId;
    source.header.id = p[i].sourceInfo.id;
    if (::DisplayConfigGetDeviceInfo(&source.header) != ERROR_SUCCESS) {
      continue;
    }
    DISPLAYCONFIG_TARGET_DEVICE_NAME target{};
    target.header.type = DISPLAYCONFIG_DEVICE_INFO_GET_TARGET_NAME;
    target.header.size = sizeof target;
    target.header.adapterId = p[i].targetInfo.adapterId;
    target.header.id = p[i].targetInfo.id;
    if (::DisplayConfigGetDeviceInfo(&target.header) != ERROR_SUCCESS || target.monitorFriendlyDeviceName[0] == 0) {
      continue;
    }
    std::wstring &name = out[std::wstring(source.viewGdiDeviceName)];
    if (name.empty()) {
      name = target.monitorFriendlyDeviceName;
    }
  }
  return out;
}

uint32_t RefreshHz(const wchar_t *device_name) noexcept {
  DEVMODEW dm{};
  dm.dmSize = sizeof dm;
  if (!::EnumDisplaySettingsW(device_name, ENUM_CURRENT_SETTINGS, &dm)) {
    return 0;
  }
  return dm.dmDisplayFrequency > 1 ? dm.dmDisplayFrequency : 0; // 0 and 1 mean "the hardware's default"
}

// GetDpiForMonitor from System32's shcore.dll, loaded on first use rather than imported (04 §13.1): the import
// linked through the SDK came out as api-ms-win-shcore-scaling-l1-1-1.dll, an API set deploy/windows/dll-imports.txt
// doesn't list. user32 has no per-monitor DPI without a window. shcore.dll is on every supported Windows (build 19041
// up); if it or the function is missing, that is logged once and every output reports 96 dpi.
using GetDpiForMonitorFn = HRESULT(WINAPI *)(HMONITOR, MONITOR_DPI_TYPE, UINT *, UINT *);

GetDpiForMonitorFn DpiForMonitorFn() noexcept {
  static const GetDpiForMonitorFn fn = []() noexcept -> GetDpiForMonitorFn {
    GetDpiForMonitorFn f = nullptr;
    if (const HMODULE m = ::LoadLibraryExW(L"shcore.dll", nullptr, LOAD_LIBRARY_SEARCH_SYSTEM32)) {
      f = reinterpret_cast<GetDpiForMonitorFn>(reinterpret_cast<void *>(::GetProcAddress(m, "GetDpiForMonitor")));
    }
    if (f == nullptr) {
      log::Warn("capture", "GetDpiForMonitor unavailable, outputs report 96 dpi",
                {{"error", static_cast<uint32_t>(::GetLastError())}});
    }
    return f;
  }();
  return fn;
}

uint32_t EffectiveDpi(HMONITOR monitor) noexcept {
  const GetDpiForMonitorFn get_dpi = DpiForMonitorFn();
  UINT x = 0;
  UINT y = 0;
  if (get_dpi == nullptr || FAILED(get_dpi(monitor, MDT_EFFECTIVE_DPI, &x, &y)) || x == 0) {
    return 96;
  }
  return x;
}

bool PrimaryMonitor(HMONITOR monitor) noexcept {
  MONITORINFO mi{};
  mi.cbSize = sizeof mi;
  return ::GetMonitorInfoW(monitor, &mi) && (mi.dwFlags & MONITORINFOF_PRIMARY) != 0;
}

bool HdrOutput(IDXGIOutput *output) noexcept {
  ComPtr<IDXGIOutput6> o6;
  if (FAILED(output->QueryInterface(IID_PPV_ARGS(&o6)))) {
    return false;
  }
  DXGI_OUTPUT_DESC1 d{};
  return SUCCEEDED(o6->GetDesc1(&d)) && d.ColorSpace == DXGI_COLOR_SPACE_RGB_FULL_G2084_NONE_P2020;
}

std::string Utf8(const wchar_t *s) { return Utf8FromWide(std::wstring_view(s)); }

} // namespace

std::vector<DisplayOutput> Win32Outputs::Outputs() {
  std::vector<DisplayOutput> out;
  try {
    const ThreadDpiScope dpi;
    ComPtr<IDXGIFactory1> factory;
    if (FAILED(::CreateDXGIFactory1(IID_PPV_ARGS(&factory)))) {
      return out;
    }
    ComPtr<IDXGIFactory6> f6;
    factory.As(&f6); // absent before Windows 10 1803: EnumAdapters1's order, as the probe
    const std::map<std::wstring, std::wstring> friendly = FriendlyNames();
    uint32_t adapter_index = 0; // the probe's index: adapters whose description it could read
    for (UINT i = 0;; ++i) {
      ComPtr<IDXGIAdapter1> a;
      const HRESULT hr = f6 ? f6->EnumAdapterByGpuPreference(i, DXGI_GPU_PREFERENCE_UNSPECIFIED, IID_PPV_ARGS(&a))
                            : factory->EnumAdapters1(i, &a);
      if (FAILED(hr)) {
        break; // DXGI_ERROR_NOT_FOUND: no more
      }
      DXGI_ADAPTER_DESC1 ad{};
      if (FAILED(a->GetDesc1(&ad))) {
        continue;
      }
      for (UINT j = 0;; ++j) {
        ComPtr<IDXGIOutput> o;
        if (FAILED(a->EnumOutputs(j, &o))) {
          break;
        }
        DXGI_OUTPUT_DESC od{};
        if (FAILED(o->GetDesc(&od)) || !od.AttachedToDesktop) {
          continue;
        }
        DisplayOutput d;
        d.device_name = Utf8(od.DeviceName);
        if (const auto it = friendly.find(std::wstring(od.DeviceName)); it != friendly.end()) {
          d.friendly_name = Utf8FromWide(it->second);
        }
        d.left = od.DesktopCoordinates.left;
        d.top = od.DesktopCoordinates.top;
        d.right = od.DesktopCoordinates.right;
        d.bottom = od.DesktopCoordinates.bottom;
        d.rotation = static_cast<uint32_t>(od.Rotation);
        d.monitor = static_cast<uint64_t>(reinterpret_cast<uintptr_t>(od.Monitor));
        d.adapter_luid = LuidValue(ad.AdapterLuid);
        d.adapter_index = adapter_index;
        d.primary = PrimaryMonitor(od.Monitor);
        d.dpi = EffectiveDpi(od.Monitor);
        d.refresh_hz = RefreshHz(od.DeviceName);
        d.hdr = HdrOutput(o.Get());
        out.push_back(std::move(d));
      }
      ++adapter_index;
    }
  } catch (const std::exception &) {
    out.clear(); // out of memory: no answer
  }
  return out;
}

uint32_t WindowsBuild() noexcept {
  using RtlGetVersionFn = LONG(WINAPI *)(OSVERSIONINFOW *);
  if (const HMODULE ntdll = ::GetModuleHandleW(L"ntdll.dll")) {
    if (const FARPROC p = ::GetProcAddress(ntdll, "RtlGetVersion")) {
      OSVERSIONINFOW info{};
      info.dwOSVersionInfoSize = sizeof info;
      if (reinterpret_cast<RtlGetVersionFn>(reinterpret_cast<void *>(p))(&info) == 0) {
        return info.dwBuildNumber;
      }
    }
  }
  return 0;
}

bool ProcessPerMonitorAware() noexcept {
  const DPI_AWARENESS a = ::GetAwarenessFromDpiAwarenessContext(::GetDpiAwarenessContextForProcess(nullptr));
  return a == DPI_AWARENESS_PER_MONITOR_AWARE;
}

bool SecureDesktopActive() noexcept {
  HDESK desk = ::OpenInputDesktop(0, FALSE, DESKTOP_READOBJECTS);
  if (desk == nullptr) {
    return true; // Winlogon's desktop (UAC's secure desktop, Ctrl+Alt+Del, the lock screen) can't be opened
  }
  wchar_t name[64] = {};
  DWORD needed = 0;
  const bool ok = ::GetUserObjectInformationW(desk, UOI_NAME, name, sizeof name, &needed) != FALSE;
  ::CloseDesktop(desk);
  return !ok || std::wstring_view(name) != L"Default";
}

} // namespace im::capture
