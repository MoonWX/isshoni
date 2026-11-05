// The Win32 side of outputs.h (docs/m2/04-windows-engine.md §5.6, §7.4, §7.5 step 1, §7.9): the DXGI outputs with
// their friendly names, DPI, refresh rate and HDR state, and the process and thread facts Desktop Duplication needs.
#pragma once

#include <windows.h>

#include <cstdint>
#include <vector>

#include "capture/outputs.h"

namespace im::capture {

// §7.5 step 1: CreateDXGIFactory1, the adapters in the probe's order (IDXGIFactory6::EnumAdapterByGpuPreference
// with DXGI_GPU_PREFERENCE_UNSPECIFIED, EnumAdapters1 before Windows 10 1803), each adapter's EnumOutputs; outputs
// not attached to the desktop are left out. Friendly names come from QueryDisplayConfig
// (DISPLAYCONFIG_SOURCE_DEVICE_NAME's GDI name → DISPLAYCONFIG_TARGET_DEVICE_NAME); DPI from GetDpiForMonitor
// (MDT_EFFECTIVE_DPI); the refresh rate from EnumDisplaySettingsW (ENUM_CURRENT_SETTINGS); HDR from
// IDXGIOutput6::GetDesc1's color space. The calling thread is made per-monitor DPI aware (v2) for the call, so every
// coordinate is in physical pixels.
class Win32Outputs final : public OutputSystem {
public:
  std::vector<DisplayOutput> Outputs() override;
};

// The Windows build (RtlGetVersion: GetVersionEx lies to a process without a compatibility manifest); 0 when unknown.
uint32_t WindowsBuild() noexcept;

// Whether the process is per-monitor DPI aware (v1 or v2), which Desktop Duplication needs (§7.4): the app's manifest
// declares PerMonitorV2, isshoni-enginetest calls SetProcessDpiAwarenessContext at startup.
bool ProcessPerMonitorAware() noexcept;

// Whether the input desktop is not the user's ("Default"): the secure desktop of UAC or Ctrl+Alt+Del, or the lock
// screen (Winlogon). A desktop this process may not open counts as secure.
bool SecureDesktopActive() noexcept;

// Makes the calling thread per-monitor DPI aware (v2) for its lifetime and restores the thread's context after.
class ThreadDpiScope {
public:
  ThreadDpiScope() noexcept : previous_(::SetThreadDpiAwarenessContext(DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2)) {}
  ~ThreadDpiScope() {
    if (previous_ != nullptr) {
      ::SetThreadDpiAwarenessContext(previous_);
    }
  }
  ThreadDpiScope(const ThreadDpiScope &) = delete;
  ThreadDpiScope &operator=(const ThreadDpiScope &) = delete;

private:
  DPI_AWARENESS_CONTEXT previous_;
};

} // namespace im::capture
