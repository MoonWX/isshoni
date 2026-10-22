#include "audio/win_audio.h"

#include <cstdio>
#include <string>

#include "im/abi/utf8.h"

namespace im::audio::win {

std::string Utf8(std::wstring_view w) {
  static_assert(sizeof(wchar_t) == sizeof(char16_t), "Windows' wchar_t is UTF-16");
  return abi::Utf16ToUtf8(std::u16string_view(reinterpret_cast<const char16_t *>(w.data()), w.size()));
}

std::wstring Wide(std::string_view s) {
  if (s.empty() || s.size() > 0x7FFFFFFF) {
    return {};
  }
  const int in = static_cast<int>(s.size());
  // Without MB_ERR_INVALID_CHARS, invalid sequences become U+FFFD.
  const int n = MultiByteToWideChar(CP_UTF8, 0, s.data(), in, nullptr, 0);
  if (n <= 0) {
    return {};
  }
  std::wstring w(static_cast<std::size_t>(n), L'\0');
  MultiByteToWideChar(CP_UTF8, 0, s.data(), in, w.data(), n);
  return w;
}

std::string Hr(HRESULT hr) {
  char b[16];
  std::snprintf(b, sizeof b, "0x%08lX", static_cast<unsigned long>(hr));
  return b;
}

bool EqualsIgnoreCase(std::wstring_view a, std::wstring_view b) noexcept {
  if (a.size() > 0x7FFFFFFF || b.size() > 0x7FFFFFFF) {
    return false;
  }
  return CompareStringOrdinal(a.data(), static_cast<int>(a.size()), b.data(), static_cast<int>(b.size()), TRUE) ==
         CSTR_EQUAL;
}

FARPROC SystemProc(const wchar_t *dll, const char *name) noexcept {
  HMODULE m = GetModuleHandleW(dll);
  if (m == nullptr) {
    m = LoadLibraryExW(dll, nullptr, LOAD_LIBRARY_SEARCH_SYSTEM32);
  }
  return m == nullptr ? nullptr : GetProcAddress(m, name);
}

uint32_t OsBuild() noexcept {
  using RtlGetVersionFn = LONG(WINAPI *)(OSVERSIONINFOW *);
  static const uint32_t build = []() -> uint32_t {
    const auto fn =
        reinterpret_cast<RtlGetVersionFn>(reinterpret_cast<void *>(SystemProc(L"ntdll.dll", "RtlGetVersion")));
    OSVERSIONINFOW v{};
    v.dwOSVersionInfoSize = sizeof v;
    if (fn != nullptr && fn(&v) == 0) {
      return static_cast<uint32_t>(v.dwBuildNumber);
    }
    return 0;
  }();
  return build;
}

} // namespace im::audio::win
