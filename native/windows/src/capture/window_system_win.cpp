#include "capture/window_system_win.h"

#include <windows.h>

#include <dwmapi.h>
#include <tlhelp32.h>

#include <iterator>

namespace im::capture {
namespace {

constexpr UINT kOwnTitleTimeoutMs = 200;

HWND Hwnd(WindowHandle w) noexcept { return reinterpret_cast<HWND>(static_cast<uintptr_t>(w)); }
WindowHandle Handle(HWND h) noexcept { return static_cast<WindowHandle>(reinterpret_cast<uintptr_t>(h)); }

BOOL CALLBACK CollectWindow(HWND h, LPARAM param) {
  auto *out = reinterpret_cast<std::vector<WindowHandle> *>(param);
  try {
    out->push_back(Handle(h));
  } catch (...) {
    return FALSE; // out of memory: stop with what we have
  }
  return TRUE;
}

BOOL CALLBACK CollectMonitor(HMONITOR m, HDC, LPRECT, LPARAM param) {
  auto *out = reinterpret_cast<std::vector<PixelSize> *>(param);
  MONITORINFO mi{};
  mi.cbSize = sizeof mi;
  if (::GetMonitorInfoW(m, &mi)) {
    try {
      out->push_back(PixelSize{mi.rcMonitor.right - mi.rcMonitor.left, mi.rcMonitor.bottom - mi.rcMonitor.top});
    } catch (...) {
      return FALSE;
    }
  }
  return TRUE;
}

} // namespace

std::vector<WindowHandle> Win32WindowSystem::TopLevelWindows() {
  std::vector<WindowHandle> out;
  out.reserve(256);
  ::EnumWindows(CollectWindow, reinterpret_cast<LPARAM>(&out));
  return out;
}

bool Win32WindowSystem::Visible(WindowHandle w) { return ::IsWindowVisible(Hwnd(w)) != FALSE; }

bool Win32WindowSystem::Cloaked(WindowHandle w) {
  DWORD cloaked = 0;
  return SUCCEEDED(::DwmGetWindowAttribute(Hwnd(w), DWMWA_CLOAKED, &cloaked, sizeof cloaked)) && cloaked != 0;
}

std::wstring Win32WindowSystem::Title(WindowHandle w) {
  // GetWindowTextW reads another process's caption without sending it a message (GetWindowTextLengthW would send
  // WM_GETTEXTLENGTH), so a hung app can't block the pick. For this process's own windows it sends WM_GETTEXT to their
  // thread, so those are asked with a timeout instead (a busy UI thread must not block the picker). A buffer that
  // comes back full is doubled.
  DWORD pid = 0;
  ::GetWindowThreadProcessId(Hwnd(w), &pid);
  const bool own = pid == ::GetCurrentProcessId();
  for (std::size_t cap = 512; cap <= 32768; cap *= 2) {
    std::wstring title(cap, L'\0');
    int got = 0;
    if (own) {
      DWORD_PTR copied = 0;
      if (::SendMessageTimeoutW(Hwnd(w), WM_GETTEXT, cap, reinterpret_cast<LPARAM>(title.data()),
                                SMTO_ABORTIFHUNG | SMTO_BLOCK, kOwnTitleTimeoutMs, &copied) != 0) {
        got = static_cast<int>(copied);
      }
    } else {
      got = ::GetWindowTextW(Hwnd(w), title.data(), static_cast<int>(cap));
    }
    if (got <= 0) {
      return {};
    }
    if (static_cast<std::size_t>(got) < cap - 1) {
      title.resize(static_cast<std::size_t>(got));
      return title;
    }
  }
  return {}; // longer than any title the picker shows in full: no match
}

std::optional<PixelSize> Win32WindowSystem::FrameSize(WindowHandle w) {
  RECT r{};
  if (FAILED(::DwmGetWindowAttribute(Hwnd(w), DWMWA_EXTENDED_FRAME_BOUNDS, &r, sizeof r))) {
    return std::nullopt;
  }
  return PixelSize{r.right - r.left, r.bottom - r.top};
}

uint32_t Win32WindowSystem::ProcessId(WindowHandle w) {
  DWORD pid = 0;
  if (::GetWindowThreadProcessId(Hwnd(w), &pid) == 0) {
    return 0;
  }
  return pid;
}

std::wstring Win32WindowSystem::ImageBaseName(uint32_t pid) {
  HANDLE p = ::OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, FALSE, pid);
  if (p == nullptr) {
    return {};
  }
  wchar_t path[MAX_PATH * 2] = {};
  DWORD size = static_cast<DWORD>(std::size(path));
  const BOOL ok = ::QueryFullProcessImageNameW(p, 0, path, &size);
  ::CloseHandle(p);
  if (!ok) {
    return {};
  }
  return std::wstring(BaseName(std::wstring_view(path, size)));
}

WindowHandle Win32WindowSystem::CoreWindowChild(WindowHandle w) {
  return Handle(::FindWindowExW(Hwnd(w), nullptr, L"Windows.UI.Core.CoreWindow", nullptr));
}

std::vector<PixelSize> MonitorSizes() {
  std::vector<PixelSize> out;
  ::EnumDisplayMonitors(nullptr, nullptr, CollectMonitor, reinterpret_cast<LPARAM>(&out));
  return out;
}

std::vector<ProcessEntry> ProcessTable() {
  std::vector<ProcessEntry> out;
  HANDLE snap = ::CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS, 0);
  if (snap == INVALID_HANDLE_VALUE) {
    return out;
  }
  PROCESSENTRY32W e{};
  e.dwSize = sizeof e;
  for (BOOL more = ::Process32FirstW(snap, &e); more; more = ::Process32NextW(snap, &e)) {
    out.push_back(ProcessEntry{e.th32ProcessID, e.th32ParentProcessID, std::wstring(BaseName(e.szExeFile))});
  }
  ::CloseHandle(snap);
  return out;
}

bool WindowMinimized(WindowHandle w) noexcept { return w != 0 && ::IsIconic(Hwnd(w)) != FALSE; }

} // namespace im::capture
