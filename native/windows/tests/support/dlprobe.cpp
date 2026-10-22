// The delay-load probes of dll_search_test (docs/m2/04-windows-engine.md §13.1, §18.2), built twice: as
// im-test-dlprobe-plain.dll and as im-test-dlprobe-flag.dll, the latter linked with /DEPENDENTLOADFLAG:0x800
// (LOAD_LIBRARY_SEARCH_SYSTEM32). Both delay-load the marker im-test-missing.dll with the default delay-load helper
// and no hook, so a child process that plants the marker in its current directory and on PATH sees what the helper
// does on its own: plain shows the default search the engine's hook guards against, flag shows whether the helper
// honors the image's DependentLoadFlags (06 §5.1's conditional load-config check). Never shipped.
#include <windows.h>

#include <delayimp.h>

extern "C" __declspec(dllimport) int im_test_marker(void);

namespace {

int Filter(DWORD code) {
  return (code == VcppException(ERROR_SEVERITY_ERROR, ERROR_MOD_NOT_FOUND) ||
          code == VcppException(ERROR_SEVERITY_ERROR, ERROR_PROC_NOT_FOUND))
             ? EXCEPTION_EXECUTE_HANDLER
             : EXCEPTION_CONTINUE_SEARCH;
}

} // namespace

// Resolves the marker's delay-loaded import now: S_OK when the helper found and loaded a copy, else the error.
extern "C" __declspec(dllexport) HRESULT im_dlprobe_load(void) {
  HRESULT hr = E_FAIL;
  __try {
    hr = __HrLoadAllImportsForDll("im-test-missing.dll");
  } __except (Filter(GetExceptionCode())) {
    hr = HRESULT_FROM_WIN32(ERROR_MOD_NOT_FOUND);
  }
  return hr;
}

// Keeps the delay-loaded import in the image (an export is never discarded); dll_search_test never calls it.
extern "C" __declspec(dllexport) int im_dlprobe_marker(void) { return im_test_marker(); }
