#include "engine/delayload.h"

#include <windows.h>

#include <delayimp.h>

namespace {

constexpr DWORD kModNotFound = VcppException(ERROR_SEVERITY_ERROR, ERROR_MOD_NOT_FOUND);
constexpr DWORD kProcNotFound = VcppException(ERROR_SEVERITY_ERROR, ERROR_PROC_NOT_FOUND);

// The hook of 04 §13.1. On dliNotePreLoadLibrary it loads the DLL from System32 only. When that fails it must not
// return NULL, which makes the helper fall back to LoadLibraryExA(name, NULL, 0) and its default search: it raises the
// helper's own "module not found" exception instead, the one __HrLoadAllImportsForDll's callers (and the helper's
// failure path) expect. Not noexcept: the SEH exception leaves through this function.
FARPROC WINAPI ImDelayHook(unsigned notify, PDelayLoadInfo info) {
  if (notify != dliNotePreLoadLibrary || info == nullptr) {
    return nullptr;
  }
  const HMODULE module = LoadLibraryExA(info->szDll, nullptr, LOAD_LIBRARY_SEARCH_SYSTEM32);
  if (module == nullptr) {
    info->dwLastError = GetLastError();
    PDelayLoadInfo args[1] = {info};
    RaiseException(kModNotFound, 0, 1, reinterpret_cast<ULONG_PTR *>(args));
  }
  return reinterpret_cast<FARPROC>(module);
}

int DelayLoadFilter(DWORD code) {
  return (code == kModNotFound || code == kProcNotFound) ? EXCEPTION_EXECUTE_HANDLER : EXCEPTION_CONTINUE_SEARCH;
}

} // namespace

// The delay-load helper (delayimp.lib) calls this hook for every delay-loaded DLL of the image it is linked into:
// isshoni-media.dll, and the test executables that link the engine's objects (dll_search_test).
ExternC const PfnDliHook __pfnDliNotifyHook2 = ImDelayHook;

namespace im::engine {

long LoadDelayedImports(const char *dll) {
  HRESULT hr = E_FAIL;
  __try {
    hr = __HrLoadAllImportsForDll(dll);
  } __except (DelayLoadFilter(GetExceptionCode())) {
    hr = GetExceptionCode() == kProcNotFound ? HRESULT_FROM_WIN32(ERROR_PROC_NOT_FOUND)
                                             : HRESULT_FROM_WIN32(ERROR_MOD_NOT_FOUND);
  }
  return hr;
}

} // namespace im::engine
