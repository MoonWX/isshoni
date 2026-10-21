// The marker DLL of dll_search_test (docs/m2/04-windows-engine.md §18.2), built as im-test-missing.dll: a name no
// system folder holds. When Windows loads it, DllMain writes an empty file "<its own path>.loaded", so a test sees
// that (and where) it was loaded even after the process is gone. A copy under another name writes its own marker.
// Built with the tests only, never shipped.
#include <windows.h>

#include <cwchar>

extern "C" __declspec(dllexport) int im_test_marker(void) { return 42; }

// C linkage, as the CRT's _DllMainCRTStartup declares it: a C++ name would leave the CRT's empty default in place.
extern "C" BOOL WINAPI DllMain(HINSTANCE instance, DWORD reason, LPVOID) {
  if (reason == DLL_PROCESS_ATTACH) {
    wchar_t path[MAX_PATH + 16];
    const DWORD n = GetModuleFileNameW(instance, path, MAX_PATH);
    if (n > 0 && n < MAX_PATH) {
      wcscat_s(path, L".loaded");
      const HANDLE file = CreateFileW(path, GENERIC_WRITE, 0, nullptr, CREATE_ALWAYS, FILE_ATTRIBUTE_NORMAL, nullptr);
      if (file != INVALID_HANDLE_VALUE) {
        CloseHandle(file);
      }
    }
  }
  return TRUE;
}
