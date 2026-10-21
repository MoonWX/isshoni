// dll_search_test, delay-load part (docs/m2/04-windows-engine.md §13.1, §13.3 layer 3, §18.2; README WA11; the
// vendor-runtime and libvpl part comes with README WA44). This binary links the engine's objects, so its delay-loaded
// imports go through the engine's hook, plus an extra /DELAYLOAD:im-test-missing.dll (a name that no system folder
// holds: windows-2025 has mfplat.dll in System32, which is searched before the current directory, so a planted
// mfplat.dll could never show the bug).
//
// Each case runs this executable again as a child process (DllSearchChild.Run) whose current directory and one PATH
// entry each hold a copy of the marker DLL tests/support/marker_dll.cpp, which leaves "<its path>.loaded" behind when
// it loads:
//   - the hook refuses the planted im-test-missing.dll: LoadDelayedImports gives ERROR_MOD_NOT_FOUND, the module isn't
//     loaded and no marker file appears;
//   - the hook still loads mfplat.dll, from System32, whatever the spelling (mfplat.lib stores "MFPlat.DLL", and the
//     delay-load helper's own lookup is case-sensitive);
//   - for the record (06 §5.1's conditional load-config check): what the default delay-load helper does without the
//     hook, in a DLL linked plain and in one linked with /DEPENDENTLOADFLAG:0x800. The verdict goes to the test log
//     and to <build>/windows/dependentloadflag-verdict.txt, which the windows-native job puts in its summary.
#include <gtest/gtest.h>

#include <windows.h>

#include <cstdio>
#include <filesystem>
#include <fstream>
#include <iostream>
#include <optional>
#include <string>
#include <vector>

#include "engine/delayload.h"

// Delay-loaded imports of this binary: the marker (tests/support/marker_dll.cpp) and Media Foundation.
extern "C" __declspec(dllimport) int im_test_marker(void);
extern "C" HRESULT WINAPI MFShutdown(void);

namespace fs = std::filesystem;

namespace {

constexpr wchar_t kModeVar[] = L"IM_DLL_SEARCH_CHILD";
constexpr wchar_t kResultVar[] = L"IM_DLL_SEARCH_RESULT";
constexpr wchar_t kMarker[] = L"im-test-missing.dll";

volatile LONG g_never = 0; // keeps the delay-loaded imports referenced; never set

std::optional<std::wstring> GetEnv(const wchar_t *name) {
  const DWORD n = GetEnvironmentVariableW(name, nullptr, 0);
  if (n == 0) {
    return std::nullopt;
  }
  std::wstring v(n, L'\0');
  const DWORD m = GetEnvironmentVariableW(name, v.data(), n);
  v.resize(m);
  return v;
}

// Sets an environment variable of this process for one scope (the child inherits the environment).
class ScopedEnv {
public:
  ScopedEnv(const wchar_t *name, const std::wstring &value) : name_(name), old_(GetEnv(name)) {
    SetEnvironmentVariableW(name, value.c_str());
  }
  ~ScopedEnv() { SetEnvironmentVariableW(name_, old_ ? old_->c_str() : nullptr); }
  ScopedEnv(const ScopedEnv &) = delete;
  ScopedEnv &operator=(const ScopedEnv &) = delete;

private:
  const wchar_t *name_;
  std::optional<std::wstring> old_;
};

fs::path ModulePath(HMODULE module) {
  std::vector<wchar_t> buf(32768);
  const DWORD n = GetModuleFileNameW(module, buf.data(), static_cast<DWORD>(buf.size()));
  return fs::path(std::wstring(buf.data(), n));
}

fs::path System32() {
  wchar_t buf[MAX_PATH];
  const UINT n = GetSystemDirectoryW(buf, MAX_PATH);
  return fs::path(std::wstring(buf, n));
}

fs::path DllDir() { return fs::path(IM_TEST_DLL_DIR); }

bool SameDir(const fs::path &a, const fs::path &b) {
  std::error_code ec;
  return fs::equivalent(a, b, ec);
}

std::string Narrow(const fs::path &p) {
  const std::u8string u8 = p.u8string();
  return std::string(u8.begin(), u8.end());
}

std::string Hex(HRESULT hr) {
  char buf[16];
  std::snprintf(buf, sizeof buf, "0x%08X", static_cast<unsigned>(hr));
  return buf;
}

// ------------------------------------------------------------------------------------------------- the child

void WriteResult(const std::string &result) {
  const std::optional<std::wstring> file = GetEnv(kResultVar);
  ASSERT_TRUE(file.has_value());
  std::ofstream out(fs::path(*file), std::ios::binary);
  out << result;
}

TEST(DllSearchChild, Run) {
  const std::optional<std::wstring> mode = GetEnv(kModeVar);
  if (!mode) {
    GTEST_SKIP() << "runs only as dll_search_test's child process";
  }
  if (g_never != 0) {
    im_test_marker();
    MFShutdown();
  }

  if (*mode == L"hook-missing") {
    // ERROR_MOD_NOT_FOUND exactly: the descriptor was found and the hook refused the DLL. A name the image doesn't
    // delay-load gives ERROR_INVALID_NAME instead (DllSearch.UnknownNameIsNotADelayImport), so a lookup that missed
    // can't pass for a refusal. The upper-case spelling must reach the hook too (the lookup ignores case).
    const long hr = im::engine::LoadDelayedImports("im-test-missing.dll");
    const long upper = im::engine::LoadDelayedImports("IM-TEST-MISSING.DLL");
    const HMODULE marker = GetModuleHandleW(kMarker);
    EXPECT_EQ(hr, HRESULT_FROM_WIN32(ERROR_MOD_NOT_FOUND))
        << Hex(hr) << ": the engine's delay-load hook didn't refuse a planted im-test-missing.dll";
    EXPECT_EQ(upper, HRESULT_FROM_WIN32(ERROR_MOD_NOT_FOUND)) << "IM-TEST-MISSING.DLL: " << Hex(upper);
    EXPECT_EQ(marker, nullptr);
    WriteResult(marker != nullptr ? "loaded " + Narrow(ModulePath(marker)) : "refused " + Hex(hr));
    return;
  }
  if (*mode == L"hook-mfplat") {
    // mfplat.lib stores the name "MFPlat.DLL": every spelling finds it.
    const long hr = im::engine::LoadDelayedImports("mfplat.dll");
    ASSERT_EQ(hr, S_OK) << "mfplat.dll's delay-loaded imports: " << Hex(hr);
    EXPECT_EQ(im::engine::LoadDelayedImports("MFPLAT.DLL"), S_OK);
    EXPECT_EQ(im::engine::LoadDelayedImports("MFPlat.DLL"), S_OK);
    const HMODULE mf = GetModuleHandleW(L"mfplat.dll");
    ASSERT_NE(mf, nullptr);
    const fs::path path = ModulePath(mf);
    EXPECT_TRUE(SameDir(path.parent_path(), System32())) << "mfplat.dll came from " << Narrow(path);
    WriteResult(SameDir(path.parent_path(), System32()) ? "system32" : "loaded " + Narrow(path));
    return;
  }
  if (mode->rfind(L"probe:", 0) == 0) {
    const fs::path probe = mode->substr(6);
    const HMODULE dll =
        LoadLibraryExW(probe.c_str(), nullptr, LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR | LOAD_LIBRARY_SEARCH_SYSTEM32);
    ASSERT_NE(dll, nullptr) << Narrow(probe) << ": LoadLibraryExW failed (" << GetLastError() << ")";
    using LoadFn = HRESULT (*)();
    const auto load = reinterpret_cast<LoadFn>(reinterpret_cast<void *>(GetProcAddress(dll, "im_dlprobe_load")));
    ASSERT_NE(load, nullptr);
    const HRESULT hr = load();
    const HMODULE marker = GetModuleHandleW(kMarker);
    WriteResult(marker != nullptr ? "loaded " + Narrow(ModulePath(marker)) : "refused " + Hex(hr));
    return;
  }
  FAIL() << "unknown child mode";
}

// ------------------------------------------------------------------------------------------------- the parent

struct Child {
  DWORD exit_code = 0xFFFFFFFF;
  std::string result;      // what the child wrote: "refused 0x…", "system32" or "loaded <path>"
  std::string loaded_from; // "the current directory", "the PATH entry", "elsewhere" or "" (not loaded)
  bool marker_in_cwd = false;
  bool marker_in_path = false;
};

// Runs DllSearchChild.Run in a child process whose current directory and first PATH entry each hold the marker.
Child RunChild(const std::wstring &mode) {
  static int counter = 0;
  const fs::path root = fs::temp_directory_path() / (L"im-dll-search-" + std::to_wstring(GetCurrentProcessId()) + L"-" +
                                                     std::to_wstring(++counter));
  const fs::path cwd = root / L"cwd";
  const fs::path path_dir = root / L"path";
  fs::remove_all(root);
  fs::create_directories(cwd);
  fs::create_directories(path_dir);
  const fs::path marker = DllDir() / kMarker;
  fs::copy_file(marker, cwd / kMarker);
  fs::copy_file(marker, path_dir / kMarker);
  const fs::path result_file = root / L"result.txt";

  Child child;
  {
    const std::wstring old_path = GetEnv(L"PATH").value_or(L"");
    ScopedEnv mode_env(kModeVar, mode);
    ScopedEnv result_env(kResultVar, result_file.wstring());
    ScopedEnv path_env(L"PATH", path_dir.wstring() + L";" + old_path);

    const fs::path exe = ModulePath(nullptr);
    std::wstring cmd = L"\"" + exe.wstring() + L"\" --gtest_filter=DllSearchChild.Run";
    std::vector<wchar_t> cmdline(cmd.begin(), cmd.end());
    cmdline.push_back(L'\0');
    STARTUPINFOW si{};
    si.cb = sizeof si;
    si.dwFlags = STARTF_USESTDHANDLES; // the child's gtest output lands in this test's log
    si.hStdInput = GetStdHandle(STD_INPUT_HANDLE);
    si.hStdOutput = GetStdHandle(STD_OUTPUT_HANDLE);
    si.hStdError = GetStdHandle(STD_ERROR_HANDLE);
    PROCESS_INFORMATION pi{};
    if (!CreateProcessW(exe.c_str(), cmdline.data(), nullptr, nullptr, TRUE, 0, nullptr, cwd.c_str(), &si, &pi)) {
      ADD_FAILURE() << "CreateProcessW failed (" << GetLastError() << ")";
      return child;
    }
    if (WaitForSingleObject(pi.hProcess, 60000) != WAIT_OBJECT_0) {
      TerminateProcess(pi.hProcess, 99);
      WaitForSingleObject(pi.hProcess, 5000);
      ADD_FAILURE() << "the child did not finish within 60 s";
    }
    GetExitCodeProcess(pi.hProcess, &child.exit_code);
    CloseHandle(pi.hThread);
    CloseHandle(pi.hProcess);
  }

  std::ifstream in(result_file, std::ios::binary);
  std::getline(in, child.result);
  in.close();
  if (child.result.rfind("loaded ", 0) == 0) {
    const std::string loaded = child.result.substr(7);
    const fs::path dir = fs::path(std::u8string(loaded.begin(), loaded.end())).parent_path();
    child.loaded_from = SameDir(dir, cwd)        ? "the current directory"
                        : SameDir(dir, path_dir) ? "the PATH entry"
                                                 : "elsewhere";
  }
  child.marker_in_cwd = fs::exists(cwd / (std::wstring(kMarker) + L".loaded"));
  child.marker_in_path = fs::exists(path_dir / (std::wstring(kMarker) + L".loaded"));
  std::error_code ec;
  fs::remove_all(root, ec);
  return child;
}

TEST(DllSearch, HookRefusesAPlantedDll) {
  const Child c = RunChild(L"hook-missing");
  EXPECT_EQ(c.exit_code, 0u) << c.result;
  EXPECT_EQ(c.result.rfind("refused", 0), 0u) << c.result;
  EXPECT_FALSE(c.marker_in_cwd) << "the planted DLL in the current directory loaded";
  EXPECT_FALSE(c.marker_in_path) << "the planted DLL on PATH loaded";
}

TEST(DllSearch, HookLoadsMfplatFromSystem32) {
  if (!fs::exists(System32() / L"mfplat.dll")) {
    GTEST_SKIP() << "this Windows has no mfplat.dll in System32 (an N edition or Media Foundation not installed)";
  }
  const Child c = RunChild(L"hook-mfplat");
  EXPECT_EQ(c.exit_code, 0u) << c.result;
  EXPECT_EQ(c.result, "system32");
  EXPECT_FALSE(c.marker_in_cwd);
  EXPECT_FALSE(c.marker_in_path);
}

// A name this image doesn't delay-load is reported as such (and loads nothing), never as a refused DLL.
TEST(DllSearch, UnknownNameIsNotADelayImport) {
  EXPECT_EQ(im::engine::LoadDelayedImports("im-not-delay-loaded.dll"), HRESULT_FROM_WIN32(ERROR_INVALID_NAME));
  EXPECT_EQ(im::engine::LoadDelayedImports("mfplat"), HRESULT_FROM_WIN32(ERROR_INVALID_NAME));
  EXPECT_EQ(im::engine::LoadDelayedImports(""), HRESULT_FROM_WIN32(ERROR_INVALID_NAME));
  EXPECT_EQ(im::engine::LoadDelayedImports(nullptr), E_INVALIDARG);
  EXPECT_EQ(GetModuleHandleW(L"im-not-delay-loaded.dll"), nullptr);
}

// Records, never fails: what the default delay-load helper does without the engine's hook, and whether it honors the
// image's /DEPENDENTLOADFLAG:0x800 (README WA11 records the verdict; it decides 06 §5.1's load-config check).
TEST(DllSearch, DependentLoadFlagVerdict) {
  const Child plain = RunChild(L"probe:" + (DllDir() / L"im-test-dlprobe-plain.dll").wstring());
  const Child flag = RunChild(L"probe:" + (DllDir() / L"im-test-dlprobe-flag.dll").wstring());
  ASSERT_EQ(plain.exit_code, 0u) << plain.result;
  ASSERT_EQ(flag.exit_code, 0u) << flag.result;

  std::string verdict;
  if (plain.loaded_from.empty()) {
    verdict = "inconclusive: the delay-load helper refused the planted DLL even without the flag (";
    verdict += plain.result;
    verdict += "), so it searches neither the current directory nor PATH";
  } else if (!flag.loaded_from.empty()) {
    verdict = "not honored: with /DEPENDENTLOADFLAG:0x800 the delay-load helper still loaded the planted DLL from ";
    verdict += flag.loaded_from;
    verdict += "; the engine's hook is the guard, and 06 section 5.1's load-config check stays out";
  } else {
    verdict = "honored: with /DEPENDENTLOADFLAG:0x800 the delay-load helper refused the planted DLL, which it loads ";
    verdict += "from " + plain.loaded_from + " without the flag";
  }
  std::cout << "DEPENDENTLOADFLAG verdict: " << verdict << std::endl;
  RecordProperty("dependent_load_flag", verdict);
  std::ofstream(fs::path(IM_TEST_OUT_DIR) / "dependentloadflag-verdict.txt", std::ios::binary) << verdict << "\n";
}

} // namespace
