#include "engine/os.h"

#include <windows.h>

#include <objbase.h>

#include <string>
#include <vector>

#include "engine/clock.h"
#include "im/abi/utf8.h"

namespace im::engine::os {
namespace {

int64_t QpcFrequency() noexcept {
  static const int64_t f = [] {
    LARGE_INTEGER v;
    QueryPerformanceFrequency(&v); // never fails on Windows XP and later
    return static_cast<int64_t>(v.QuadPart);
  }();
  return f;
}

// FILETIME counts 100 ns since 1601-01-01; Unix time starts 11 644 473 600 s later.
constexpr int64_t kFiletimeUnixEpoch = 116444736000000000LL;

// Any address inside this module, for GetModuleHandleExW.
const char kModuleAnchor = 0;

} // namespace

int64_t QpcNs() noexcept {
  LARGE_INTEGER q;
  QueryPerformanceCounter(&q);
  return QpcToNs(static_cast<int64_t>(q.QuadPart), QpcFrequency());
}

int64_t UnixNs() noexcept {
  FILETIME ft;
  GetSystemTimePreciseAsFileTime(&ft);
  ULARGE_INTEGER u;
  u.LowPart = ft.dwLowDateTime;
  u.HighPart = ft.dwHighDateTime;
  return (static_cast<int64_t>(u.QuadPart) - kFiletimeUnixEpoch) * 100;
}

void ClearLibvplSearchPaths() noexcept {
  SetEnvironmentVariableW(L"ONEVPL_SEARCH_PATH", nullptr);
  SetEnvironmentVariableW(L"ONEVPL_PRIORITY_PATH", nullptr);
}

int32_t EnterWorkerThread() noexcept {
  const HRESULT hr = CoInitializeEx(nullptr, COINIT_MULTITHREADED);
  if (FAILED(hr)) {
    return static_cast<int32_t>(hr);
  }
  // SetThreadDescription (Windows 10 1607) through GetProcAddress: its declaration depends on NTDDI_VERSION.
  using SetThreadDescriptionFn = HRESULT(WINAPI *)(HANDLE, PCWSTR);
  if (const HMODULE kernel32 = GetModuleHandleW(L"kernel32.dll")) {
    if (const FARPROC p = GetProcAddress(kernel32, "SetThreadDescription")) {
      reinterpret_cast<SetThreadDescriptionFn>(reinterpret_cast<void *>(p))(GetCurrentThread(),
                                                                            L"isshoni engine worker");
    }
  }
  return 0;
}

void LeaveWorkerThread() noexcept { CoUninitialize(); }

std::string ModuleDirectory() {
  HMODULE module = nullptr;
  if (!GetModuleHandleExW(GET_MODULE_HANDLE_EX_FLAG_FROM_ADDRESS | GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT,
                          reinterpret_cast<LPCWSTR>(&kModuleAnchor), &module)) {
    return {};
  }
  std::vector<wchar_t> buf(MAX_PATH);
  for (;;) {
    const DWORD n = GetModuleFileNameW(module, buf.data(), static_cast<DWORD>(buf.size()));
    if (n == 0) {
      return {};
    }
    if (n < buf.size()) {
      std::wstring path(buf.data(), n);
      const auto slash = path.find_last_of(L"\\/");
      if (slash == std::wstring::npos) {
        return {};
      }
      path.resize(slash);
      return abi::Utf16ToUtf8(std::u16string_view(reinterpret_cast<const char16_t *>(path.data()), path.size()));
    }
    if (buf.size() >= 32768) {
      return {}; // longer than any Windows path
    }
    buf.resize(buf.size() * 2);
  }
}

std::string_view OsName() noexcept { return "windows"; }

} // namespace im::engine::os
