// Linked into every Windows test binary (native/windows/CMakeLists.txt), never shipped. gtest's SEH guard covers test
// bodies on the main thread only: a crash on the engine worker or any other thread ends the process with nothing but
// ctest's "Exception: SegFault". This installs a last-chance exception filter that prints the exception code, the
// faulting address as module+offset ("no loaded module" when the code was in a DLL that has been unloaded), an access
// violation's target and a symbolized stack (DbgHelp, with the PDBs of /DEBUG:FULL) to stderr, then hands the
// exception on to the filter installed before it (the CRT's), so the crash proceeds as it would have: the test still
// fails and ctest still reports the exception. It also makes stdout unbuffered, so a test's own printf reaches ctest's
// log before a crash.
// Under win-x64-asan, ASan's own handler reports a crash, and this file is empty (an unused static function would be
// C4505 there).
#if !defined(__SANITIZE_ADDRESS__)
#include <windows.h>

#include <dbghelp.h>

#include <cstdio>
#include <cstring>

#pragma comment(lib, "dbghelp.lib")

namespace {

// The filter that was installed before ours (the CRT's, which sends an unhandled C++ exception to std::terminate):
// LastChance hands every exception on to it after reporting.
LPTOP_LEVEL_EXCEPTION_FILTER g_prev = nullptr;

LONG Chain(EXCEPTION_POINTERS *ep) noexcept { return g_prev != nullptr ? g_prev(ep) : EXCEPTION_CONTINUE_SEARCH; }

void Write(const char *s) noexcept {
  DWORD written = 0;
  ::WriteFile(::GetStdHandle(STD_ERROR_HANDLE), s, static_cast<DWORD>(std::strlen(s)), &written, nullptr);
}

bool InModule(DWORD64 addr, HMODULE *m) noexcept {
  return ::GetModuleHandleExA(GET_MODULE_HANDLE_EX_FLAG_FROM_ADDRESS | GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT,
                              reinterpret_cast<LPCSTR>(addr), m) != FALSE;
}

void Describe(HANDLE process, const char *label, DWORD64 addr) noexcept {
  char mod_name[MAX_PATH] = "<no loaded module: an unloaded DLL?>";
  DWORD64 offset = addr;
  HMODULE m = nullptr;
  if (InModule(addr, &m)) {
    char path[MAX_PATH] = "";
    ::GetModuleFileNameA(m, path, MAX_PATH);
    const char *base = std::strrchr(path, '\\');
    strncpy_s(mod_name, base != nullptr ? base + 1 : path, _TRUNCATE);
    offset = addr - reinterpret_cast<DWORD64>(m);
  }
  alignas(SYMBOL_INFO) char buf[sizeof(SYMBOL_INFO) + 512] = {};
  auto *sym = reinterpret_cast<SYMBOL_INFO *>(buf);
  sym->SizeOfStruct = sizeof(SYMBOL_INFO);
  sym->MaxNameLen = 512;
  DWORD64 disp = 0;
  const char *name = ::SymFromAddr(process, addr, &disp, sym) ? sym->Name : "?";
  IMAGEHLP_LINE64 src{};
  src.SizeOfStruct = sizeof src;
  DWORD ldisp = 0;
  const bool has_line = ::SymGetLineFromAddr64(process, addr, &ldisp, &src) != FALSE;
  char line[1024];
  std::snprintf(line, sizeof line, "  %s 0x%016llx %s+0x%llx %s+0x%llx %s:%lu\n", label, addr, mod_name, offset, name,
                disp, has_line ? src.FileName : "", has_line ? src.LineNumber : 0ul);
  Write(line);
}

LONG WINAPI LastChance(EXCEPTION_POINTERS *ep) {
  static volatile LONG once = 0;
  if (::InterlockedExchange(&once, 1) != 0) {
    return Chain(ep);
  }
  const EXCEPTION_RECORD &r = *ep->ExceptionRecord;
  char line[256];
  std::snprintf(line, sizeof line, "\n*** unhandled exception 0x%08lx at %p on thread %lu\n", r.ExceptionCode,
                r.ExceptionAddress, ::GetCurrentThreadId());
  Write(line);
  if (r.ExceptionCode == EXCEPTION_ACCESS_VIOLATION && r.NumberParameters >= 2) {
    const ULONG_PTR kind = r.ExceptionInformation[0];
    const char *access = "execute";
    if (kind == 0) {
      access = "read";
    } else if (kind == 1) {
      access = "write";
    }
    std::snprintf(line, sizeof line, "  %s of 0x%016llx\n", access,
                  static_cast<unsigned long long>(r.ExceptionInformation[1]));
    Write(line);
  }
  const HANDLE process = ::GetCurrentProcess();
  ::SymSetOptions(SYMOPT_UNDNAME | SYMOPT_DEFERRED_LOADS | SYMOPT_LOAD_LINES | SYMOPT_FAIL_CRITICAL_ERRORS);
  (void)::SymInitialize(process, nullptr, TRUE);
#if defined(_M_X64)
  CONTEXT ctx = *ep->ContextRecord;
  HMODULE m = nullptr;
  if (!InModule(ctx.Rip, &m)) {
    // A call into an unloaded DLL faults at its first instruction: the caller is the return address on the stack.
    Describe(process, "callee", ctx.Rip);
    ctx.Rip = *reinterpret_cast<const DWORD64 *>(ctx.Rsp);
    ctx.Rsp += 8;
  }
  STACKFRAME64 f{};
  f.AddrPC.Offset = ctx.Rip;
  f.AddrPC.Mode = AddrModeFlat;
  f.AddrStack.Offset = ctx.Rsp;
  f.AddrStack.Mode = AddrModeFlat;
  f.AddrFrame.Offset = ctx.Rbp;
  f.AddrFrame.Mode = AddrModeFlat;
  for (int i = 0; i < 48; ++i) {
    if (!::StackWalk64(IMAGE_FILE_MACHINE_AMD64, process, ::GetCurrentThread(), &f, &ctx, nullptr,
                       ::SymFunctionTableAccess64, ::SymGetModuleBase64, nullptr) ||
        f.AddrPC.Offset == 0) {
      break;
    }
    Describe(process, "frame", f.AddrPC.Offset);
  }
#else
  Describe(process, "at", reinterpret_cast<DWORD64>(r.ExceptionAddress));
#endif
  return Chain(ep);
}

[[maybe_unused]] const bool kInstalled = [] {
  std::setvbuf(stdout, nullptr, _IONBF, 0);
  g_prev = ::SetUnhandledExceptionFilter(LastChance);
  return true;
}();

} // namespace
#endif
