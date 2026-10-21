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

namespace {

// DLL names compared the way the loader does, without case (ASCII: system DLL names are ASCII, and no locale applies).
bool SameDllName(const char *a, const char *b) noexcept {
  for (;; ++a, ++b) {
    char x = *a;
    char y = *b;
    if (x >= 'A' && x <= 'Z') {
      x = static_cast<char>(x - 'A' + 'a');
    }
    if (y >= 'A' && y <= 'Z') {
      y = static_cast<char>(y - 'A' + 'a');
    }
    if (x != y) {
      return false;
    }
    if (x == '\0') {
      return true;
    }
  }
}

// The name under which this image's delay-import table holds `dll`, or nullptr when it has no entry for it. The
// helper's own lookup (delayhlp.cpp's PiddFromDllName) compares case-sensitively against that stored name, which is
// the import library's spelling ("MFPlat.DLL" for mfplat.lib, "AVRT.dll" for avrt.lib), not /DELAYLOAD's argument.
// The image is the one this code is linked into: found from the address of the hook variable above.
const char *DelayImportName(const char *dll) noexcept {
  HMODULE self = nullptr;
  if (!GetModuleHandleExW(GET_MODULE_HANDLE_EX_FLAG_FROM_ADDRESS | GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT,
                          reinterpret_cast<LPCWSTR>(&__pfnDliNotifyHook2), &self)) {
    return nullptr;
  }
  const auto *base = reinterpret_cast<const BYTE *>(self);
  const auto *dos = reinterpret_cast<const IMAGE_DOS_HEADER *>(base);
  const auto *nt = reinterpret_cast<const IMAGE_NT_HEADERS *>(base + dos->e_lfanew);
  const IMAGE_DATA_DIRECTORY &dir = nt->OptionalHeader.DataDirectory[IMAGE_DIRECTORY_ENTRY_DELAY_IMPORT];
  if (dir.VirtualAddress == 0 || dir.Size == 0) {
    return nullptr;
  }
  for (auto d = reinterpret_cast<PCImgDelayDescr>(base + dir.VirtualAddress); d->rvaDLLName != 0; ++d) {
    if ((d->grAttrs & dlattrRva) == 0) {
      continue; // a pre-VC7 descriptor with pointers, which no current linker writes (and the helper refuses)
    }
    const char *name = reinterpret_cast<const char *>(base + d->rvaDLLName);
    if (SameDllName(name, dll)) {
      return name;
    }
  }
  return nullptr;
}

} // namespace

namespace im::engine {

long LoadDelayedImports(const char *dll) {
  if (dll == nullptr) {
    return E_INVALIDARG;
  }
  const char *stored = DelayImportName(dll);
  if (stored == nullptr) {
    return HRESULT_FROM_WIN32(ERROR_INVALID_NAME);
  }
  HRESULT hr = E_FAIL;
  __try {
    hr = __HrLoadAllImportsForDll(stored);
  } __except (DelayLoadFilter(GetExceptionCode())) {
    hr = GetExceptionCode() == kProcNotFound ? HRESULT_FROM_WIN32(ERROR_PROC_NOT_FOUND)
                                             : HRESULT_FROM_WIN32(ERROR_MOD_NOT_FOUND);
  }
  return hr;
}

} // namespace im::engine
