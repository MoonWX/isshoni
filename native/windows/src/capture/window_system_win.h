// The Win32 side of window_match.h (docs/m2/04-windows-engine.md §7.2 step 4, §7.6): the real window system, the
// monitors' sizes and the process table. Call it on a thread that is per-monitor DPI aware (v2), so frame bounds and
// monitor rectangles are physical pixels like item.Size(): the picker thread sets that up (§7.4).
#pragma once

#include <cstdint>
#include <optional>
#include <string>
#include <vector>

#include "capture/window_match.h"

namespace im::capture {

class Win32WindowSystem final : public WindowSystem {
public:
  std::vector<WindowHandle> TopLevelWindows() override;
  bool Visible(WindowHandle w) override;
  bool Cloaked(WindowHandle w) override;
  std::wstring Title(WindowHandle w) override;
  std::optional<PixelSize> FrameSize(WindowHandle w) override;
  uint32_t ProcessId(WindowHandle w) override;
  std::wstring ImageBaseName(uint32_t pid) override;
  WindowHandle CoreWindowChild(WindowHandle w) override;
};

// Every monitor's size in physical pixels (EnumDisplayMonitors, GetMonitorInfoW's rcMonitor).
std::vector<PixelSize> MonitorSizes();

// A Toolhelp snapshot of the processes (pid, parent, image base name); empty when it fails.
std::vector<ProcessEntry> ProcessTable();

// IsIconic for an identified window (false for 0 or a window that is gone).
bool WindowMinimized(WindowHandle w) noexcept;

} // namespace im::capture
