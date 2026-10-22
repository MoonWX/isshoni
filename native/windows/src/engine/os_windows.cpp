#include "engine/os.h"

#include <windows.h>

#include <objbase.h>

#include <algorithm>
#include <iterator>
#include <stdexcept>
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

// SetThreadDescription (Windows 10 1607) through GetProcAddress: its declaration depends on NTDDI_VERSION.
void NameThread(const wchar_t *name) noexcept {
  using SetThreadDescriptionFn = HRESULT(WINAPI *)(HANDLE, PCWSTR);
  if (const HMODULE kernel32 = GetModuleHandleW(L"kernel32.dll")) {
    if (const FARPROC p = GetProcAddress(kernel32, "SetThreadDescription")) {
      reinterpret_cast<SetThreadDescriptionFn>(reinterpret_cast<void *>(p))(GetCurrentThread(), name);
    }
  }
}

#ifndef CREATE_WAITABLE_TIMER_HIGH_RESOLUTION
#define CREATE_WAITABLE_TIMER_HIGH_RESOLUTION 0x00000002 // Windows 10 1803 (synchapi.h of newer SDKs)
#endif

// MMCSS (avrt.dll), loaded from System32 at run time on first use rather than imported: a system without it (none
// is known) only loses the scheduling class (04 §6.1).
struct Avrt {
  using SetFn = HANDLE(WINAPI *)(LPCWSTR, LPDWORD);
  using RevertFn = BOOL(WINAPI *)(HANDLE);
  SetFn set = nullptr;
  RevertFn revert = nullptr;
};

const Avrt &GetAvrt() noexcept {
  static const Avrt avrt = [] {
    Avrt a;
    if (const HMODULE m = LoadLibraryExW(L"avrt.dll", nullptr, LOAD_LIBRARY_SEARCH_SYSTEM32)) {
      a.set =
          reinterpret_cast<Avrt::SetFn>(reinterpret_cast<void *>(GetProcAddress(m, "AvSetMmThreadCharacteristicsW")));
      a.revert = reinterpret_cast<Avrt::RevertFn>(
          reinterpret_cast<void *>(GetProcAddress(m, "AvRevertMmThreadCharacteristics")));
      if (a.set == nullptr || a.revert == nullptr) {
        a = Avrt{};
      }
    }
    return a;
  }();
  return avrt;
}

} // namespace

// ------------------------------------------------------------------------------------------------------------ Waiter

struct Waiter::State {
  HANDLE event = nullptr; // auto-reset: Wake
  HANDLE timer = nullptr; // the deadline
};

Waiter::Waiter() : state_(std::make_unique<State>()) {
  state_->event = CreateEventW(nullptr, FALSE, FALSE, nullptr);
  state_->timer = CreateWaitableTimerExW(nullptr, nullptr, CREATE_WAITABLE_TIMER_HIGH_RESOLUTION, TIMER_ALL_ACCESS);
  if (state_->timer == nullptr) {
    // Before Windows 10 1803 (below isshoni's minimum, but harmless): a timer at the system tick.
    state_->timer = CreateWaitableTimerExW(nullptr, nullptr, 0, TIMER_ALL_ACCESS);
  }
  if (state_->event == nullptr || state_->timer == nullptr) {
    const DWORD err = GetLastError();
    if (state_->event != nullptr) {
      CloseHandle(state_->event);
    }
    if (state_->timer != nullptr) {
      CloseHandle(state_->timer);
    }
    throw std::runtime_error("engine waiter: CreateEventW or CreateWaitableTimerExW failed (" + std::to_string(err) +
                             ")");
  }
}

Waiter::~Waiter() {
  CloseHandle(state_->timer);
  CloseHandle(state_->event);
}

void Waiter::Wake() noexcept { SetEvent(state_->event); }

bool Waiter::WaitUntil(int64_t deadline_ns) noexcept {
  if (deadline_ns == kNoDeadline) {
    return WaitForSingleObject(state_->event, INFINITE) == WAIT_OBJECT_0;
  }
  const int64_t left = deadline_ns - QpcNs();
  if (left <= 0) {
    return WaitForSingleObject(state_->event, 0) == WAIT_OBJECT_0;
  }
  LARGE_INTEGER due;
  due.QuadPart = -((left + 99) / 100); // relative, in 100 ns units
  if (!SetWaitableTimer(state_->timer, &due, 0, nullptr, nullptr, FALSE)) {
    // No timer: wait on the event with a millisecond timeout instead.
    const auto ms = static_cast<DWORD>(std::min<int64_t>((left + 999'999) / 1'000'000, 0x7FFFFFFF));
    return WaitForSingleObject(state_->event, ms) == WAIT_OBJECT_0;
  }
  const HANDLE handles[2] = {state_->event, state_->timer};
  const DWORD r = WaitForMultipleObjects(2, handles, FALSE, INFINITE);
  if (r == WAIT_OBJECT_0) {
    CancelWaitableTimer(state_->timer);
    return true;
  }
  return false;
}

// ----------------------------------------------------------------------------------------------- session threads

uintptr_t EnterThread(const char *name, ThreadClass cls) noexcept {
  if (name != nullptr) {
    wchar_t wide[64] = {};
    for (std::size_t i = 0; i + 1 < std::size(wide) && name[i] != '\0'; ++i) {
      wide[i] = static_cast<wchar_t>(static_cast<unsigned char>(name[i]) & 0x7F); // ASCII only
    }
    NameThread(wide);
  }
  const wchar_t *task = nullptr;
  switch (cls) {
  case ThreadClass::kCapture:
    SetThreadPriority(GetCurrentThread(), THREAD_PRIORITY_ABOVE_NORMAL);
    task = L"Capture";
    break;
  case ThreadClass::kProAudio:
    task = L"Pro Audio";
    break;
  case ThreadClass::kNormal:
    break;
  }
  if (task == nullptr) {
    return 0;
  }
  const Avrt &avrt = GetAvrt();
  if (avrt.set == nullptr) {
    return 0;
  }
  DWORD index = 0;
  return reinterpret_cast<uintptr_t>(avrt.set(task, &index));
}

void LeaveThread(uintptr_t token) noexcept {
  if (token != 0) {
    if (const Avrt &avrt = GetAvrt(); avrt.revert != nullptr) {
      avrt.revert(reinterpret_cast<HANDLE>(token));
    }
  }
}

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
  NameThread(L"isshoni engine worker");
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
