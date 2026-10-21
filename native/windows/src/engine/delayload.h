// The delay-load hook (docs/m2/04-windows-engine.md §13.1, §13.3 layer 3): every DLL that isshoni-media.dll
// delay-loads (mfplat.dll, avrt.dll; /DELAYLOAD in native/windows/CMakeLists.txt) is a System32 DLL, so the hook
// that delayload.cpp installs as __pfnDliNotifyHook2 loads it from System32 only, whatever the host process did.
// Without it the delay-load helper would call LoadLibraryExA(name, NULL, 0), whose default search includes the current
// directory (inherited from the browser on a link launch) and PATH: on an N edition, which has no mfplat.dll in
// System32, a planted copy would load into isshoni.exe. A DLL that isn't in System32 fails to load instead.
//
// The hook lives in the engine, so it protects every host: the app, isshoni-enginetest, isshoni-audiotest and g2g.
#pragma once

namespace im::engine {

// Resolves every delay-loaded import of `dll` (a name as given to /DELAYLOAD, "mfplat.dll") now, through
// __HrLoadAllImportsForDll, with the delay-load helper's exceptions (VcppException ERROR_MOD_NOT_FOUND or
// ERROR_PROC_NOT_FOUND) turned into HRESULT_FROM_WIN32 of that code. Returns an HRESULT: S_OK when the DLL and every
// imported function were found. The probe checks Media Foundation with it before any call (04 §13.1).
long LoadDelayedImports(const char *dll);

} // namespace im::engine
