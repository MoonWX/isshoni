// The OS calls of the engine core (docs/m2/04-windows-engine.md §6.1, §12.3, §13.3), behind one header so the rest of
// src/engine/ and src/api/ is plain C++20. Implemented for Windows in os_windows.cpp.
#pragma once

#include <cstdint>
#include <string>
#include <string_view>

namespace im::engine::os {

// The engine clock: QueryPerformanceCounter in ns (04 §12.3), the clock of every capture_ts_ns and of Go's qpcNS().
int64_t QpcNs() noexcept;

// The wall clock in Unix ns (GetSystemTimePreciseAsFileTime).
int64_t UnixNs() noexcept;

// Clears ONEVPL_SEARCH_PATH and ONEVPL_PRIORITY_PATH from the process environment (04 §13.3 layer 4): libvpl's
// dispatcher inside avcodec reads them, and FFmpeg's static CRT copies the environment when the engine first loads
// it, after im_init.
void ClearLibvplSearchPaths() noexcept;

// Called first and last on the engine worker's thread (04 §6.1): COM in the multithreaded apartment and the thread's
// name. EnterWorkerThread returns 0 (S_OK) or the failing HRESULT; LeaveWorkerThread is called only after a success.
int32_t EnterWorkerThread() noexcept;
void LeaveWorkerThread() noexcept;

// The folder of isshoni-media.dll (or of the test executable the engine is linked into), UTF-8, without a trailing
// separator; "" when Windows can't tell.
std::string ModuleDirectory();

// "windows" (im_build_info's "os").
std::string_view OsName() noexcept;

} // namespace im::engine::os
