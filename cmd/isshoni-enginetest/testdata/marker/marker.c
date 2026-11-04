/* im-marker.dll for isshoni-enginetest's TestDLLSearchMarker (docs/m2/04-windows-engine.md §14, §18.2): a DLL with a
 * name in no system folder. When Windows loads it, DllMain writes an empty file "<its own path>.loaded", so the test
 * sees that, and from where, it was loaded even after the process is gone. The test builds it with MinGW-w64's gcc;
 * never shipped. */
#include <windows.h>

__declspec(dllexport) int im_marker(void) { return 42; }

BOOL WINAPI DllMain(HINSTANCE instance, DWORD reason, LPVOID reserved) {
  (void)reserved;
  if (reason == DLL_PROCESS_ATTACH) {
    wchar_t path[MAX_PATH + 16];
    DWORD n = GetModuleFileNameW(instance, path, MAX_PATH);
    if (n > 0 && n < MAX_PATH) {
      lstrcatW(path, L".loaded");
      HANDLE file = CreateFileW(path, GENERIC_WRITE, 0, NULL, CREATE_ALWAYS, FILE_ATTRIBUTE_NORMAL, NULL);
      if (file != INVALID_HANDLE_VALUE) {
        CloseHandle(file);
      }
    }
  }
  return TRUE;
}
