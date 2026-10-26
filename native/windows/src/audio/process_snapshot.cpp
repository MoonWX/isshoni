#include "audio/process_snapshot.h"

#include "audio/win_audio.h"

#include <tlhelp32.h>

#include <cstdio>
#include <cwchar>
#include <string>
#include <utility>
#include <vector>

#include "im/audiopolicy/text.h"

namespace im::audio {
namespace {

// winternl.h's PROCESSINFOCLASS value for NtQueryInformationProcess: the command line as a UNICODE_STRING followed by
// its characters (Windows 8.1 and later, with PROCESS_QUERY_LIMITED_INFORMATION).
constexpr ULONG kProcessCommandLineInformation = 60;
constexpr LONG kStatusInfoLengthMismatch = static_cast<LONG>(0xC0000004L);
// A command line is at most 32 767 UTF-16 units (CreateProcess's limit), plus the UNICODE_STRING header.
constexpr ULONG kMaxCommandLineBytes = 32767 * 2 + 64;

// The UNICODE_STRING header that NtQueryInformationProcess writes first (winternl.h's layout).
struct UnicodeStringHeader {
  USHORT length; // bytes, without a terminator
  USHORT maximum_length;
  PWSTR buffer;
};

using NtQueryInformationProcessFn = LONG(NTAPI *)(HANDLE, ULONG, PVOID, ULONG, PULONG);

// version.dll's three functions behind FileDescription (05 §5.1), loaded from System32 at run time.
using GetFileVersionInfoSizeExFn = DWORD(WINAPI *)(DWORD, LPCWSTR, LPDWORD);
using GetFileVersionInfoExFn = BOOL(WINAPI *)(DWORD, LPCWSTR, DWORD, DWORD, LPVOID);
using VerQueryValueFn = BOOL(WINAPI *)(LPCVOID, LPCWSTR, LPVOID *, PUINT);
constexpr DWORD kFileVerGetLocalised = 0x01; // FILE_VER_GET_LOCALISED (winver.h)

struct VersionApi {
  GetFileVersionInfoSizeExFn size = nullptr;
  GetFileVersionInfoExFn info = nullptr;
  VerQueryValueFn query = nullptr;
};

const VersionApi &Version() noexcept {
  static const VersionApi api = [] {
    VersionApi v;
    v.size = reinterpret_cast<GetFileVersionInfoSizeExFn>(
        reinterpret_cast<void *>(win::SystemProc(L"version.dll", "GetFileVersionInfoSizeExW")));
    v.info = reinterpret_cast<GetFileVersionInfoExFn>(
        reinterpret_cast<void *>(win::SystemProc(L"version.dll", "GetFileVersionInfoExW")));
    v.query =
        reinterpret_cast<VerQueryValueFn>(reinterpret_cast<void *>(win::SystemProc(L"version.dll", "VerQueryValueW")));
    return v;
  }();
  return api;
}

// QueryFullProcessImageNameW in its Win32 form, growing from MAX_PATH to the longest path Windows allows.
std::wstring ImagePath(HANDLE h) {
  for (DWORD cap : {DWORD{MAX_PATH}, DWORD{32768}}) {
    std::wstring buf(cap, L'\0');
    DWORD n = cap;
    if (QueryFullProcessImageNameW(h, 0, buf.data(), &n)) {
      buf.resize(n);
      return buf;
    }
    if (GetLastError() != ERROR_INSUFFICIENT_BUFFER) {
      break;
    }
  }
  return {};
}

// The string value of a version resource's block, "" when absent.
std::wstring VersionString(const VersionApi &v, const void *block, const wchar_t *sub) {
  void *p = nullptr;
  UINT len = 0;
  if (!v.query(block, sub, &p, &len) || p == nullptr || len == 0) {
    return {};
  }
  std::wstring s(static_cast<const wchar_t *>(p), len);
  while (!s.empty() && s.back() == L'\0') {
    s.pop_back();
  }
  return s;
}

} // namespace

bool IsWebView2Exe(std::wstring_view exe) noexcept { return win::EqualsIgnoreCase(exe, L"msedgewebview2.exe"); }

std::wstring ReadCommandLine(void *process) {
  static const auto query = reinterpret_cast<NtQueryInformationProcessFn>(
      reinterpret_cast<void *>(win::SystemProc(L"ntdll.dll", "NtQueryInformationProcess")));
  if (query == nullptr || process == nullptr) {
    return {};
  }
  // uint64_t storage keeps the header's pointer aligned.
  std::vector<uint64_t> buf(512);
  for (int attempt = 0; attempt < 2; ++attempt) {
    const ULONG size = static_cast<ULONG>(buf.size() * sizeof(uint64_t));
    ULONG needed = 0;
    const LONG st = query(static_cast<HANDLE>(process), kProcessCommandLineInformation, buf.data(), size, &needed);
    if (st >= 0) {
      const auto *us = reinterpret_cast<const UnicodeStringHeader *>(buf.data());
      if (us->buffer == nullptr || us->length == 0) {
        return {};
      }
      // The characters follow the header inside buf; never trust the pointer beyond it.
      const auto *base = reinterpret_cast<const unsigned char *>(buf.data());
      const auto *chars = reinterpret_cast<const unsigned char *>(us->buffer);
      if (chars < base + sizeof(UnicodeStringHeader) || chars + us->length > base + size) {
        return {};
      }
      return std::wstring(reinterpret_cast<const wchar_t *>(chars), us->length / sizeof(wchar_t));
    }
    if (st != kStatusInfoLengthMismatch || needed <= size || needed > kMaxCommandLineBytes) {
      return {};
    }
    buf.assign((needed + sizeof(uint64_t) - 1) / sizeof(uint64_t), 0);
  }
  return {};
}

ProcessDetails QueryProcessDetails(uint32_t pid, bool read_command_line) {
  ProcessDetails d;
  if (pid == 0) {
    return d; // the idle process
  }
  const win::Handle h(OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, FALSE, pid));
  if (!h) {
    return d;
  }
  d.opened = true;
  FILETIME created{}, exited{}, kernel{}, user{};
  if (GetProcessTimes(h.get(), &created, &exited, &kernel, &user)) {
    d.created = win::FiletimeValue(created);
  }
  d.image = ImagePath(h.get());
  if (const std::size_t slash = d.image.find_last_of(L"\\/"); slash != std::wstring::npos && slash > 0) {
    d.folder = win::Utf8(std::wstring_view(d.image).substr(0, slash));
  }
  if (read_command_line) {
    d.user_data_dir = audiopolicy::ParseUserDataDir(win::Utf8(ReadCommandLine(h.get())));
  }
  return d;
}

std::string FileDescription(const std::wstring &image) {
  const VersionApi &v = Version();
  if (v.size == nullptr || v.info == nullptr || v.query == nullptr || image.empty()) {
    return {};
  }
  DWORD ignored = 0;
  const DWORD size = v.size(kFileVerGetLocalised, image.c_str(), &ignored);
  if (size == 0 || size > (16u << 20)) {
    return {};
  }
  std::vector<unsigned char> block(size);
  if (!v.info(kFileVerGetLocalised, image.c_str(), 0, size, block.data())) {
    return {};
  }
  // The first language of the translation table, then the usual English fallbacks.
  std::vector<std::wstring> keys;
  void *p = nullptr;
  UINT len = 0;
  if (v.query(block.data(), L"\\VarFileInfo\\Translation", &p, &len) && p != nullptr && len >= 4) {
    const auto *t = static_cast<const WORD *>(p);
    wchar_t key[64];
    std::swprintf(key, sizeof key / sizeof key[0], L"\\StringFileInfo\\%04x%04x\\FileDescription",
                  static_cast<unsigned>(t[0]), static_cast<unsigned>(t[1]));
    keys.emplace_back(key);
  }
  keys.emplace_back(L"\\StringFileInfo\\040904b0\\FileDescription");
  keys.emplace_back(L"\\StringFileInfo\\040904e4\\FileDescription");
  for (const std::wstring &k : keys) {
    std::wstring s = VersionString(v, block.data(), k.c_str());
    while (!s.empty() && (s.back() == L' ' || s.back() == L'\t')) {
      s.pop_back();
    }
    if (!s.empty()) {
      return win::Utf8(s);
    }
  }
  return {};
}

bool ProcessSnapshotter::Take(std::vector<audiopolicy::ProcessEntry> &out) {
  out.clear();
  const win::Handle snap(CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS, 0));
  if (!snap) {
    return false;
  }
  PROCESSENTRY32W pe{};
  pe.dwSize = sizeof pe;
  std::unordered_map<uint32_t, Cached> next;
  next.reserve(by_pid_.size() + 32);
  if (Process32FirstW(snap.get(), &pe)) {
    do {
      const auto pid = static_cast<uint32_t>(pe.th32ProcessID);
      const auto ppid = static_cast<uint32_t>(pe.th32ParentProcessID);
      pe.szExeFile[MAX_PATH - 1] = L'\0';
      std::wstring exe(pe.szExeFile);
      Cached c;
      if (const auto it = by_pid_.find(pid);
          it != by_pid_.end() && it->second.parent_pid == ppid && it->second.exe == exe) {
        c = std::move(it->second);
      } else {
        // A pid not seen before, or whose parent or exe changed (05 §7.4).
        c.parent_pid = ppid;
        c.exe = exe;
        c.details = QueryProcessDetails(pid, IsWebView2Exe(exe));
      }
      audiopolicy::ProcessEntry e;
      e.pid = pid;
      e.parent_pid = ppid;
      e.created = c.details.created;
      e.exe = win::Utf8(exe);
      e.folder = c.details.folder;
      e.user_data_dir = c.details.user_data_dir;
      out.push_back(std::move(e));
      next.insert_or_assign(pid, std::move(c));
    } while (Process32NextW(snap.get(), &pe));
  }
  by_pid_.swap(next);
  return true;
}

void ProcessSnapshotter::Describe(std::vector<audiopolicy::ProcessEntry> &entries, std::span<const std::size_t> which) {
  constexpr std::size_t kMaxDescriptions = 1024;
  for (const std::size_t i : which) {
    if (i >= entries.size() || !entries[i].description.empty()) {
      continue;
    }
    const auto it = by_pid_.find(entries[i].pid);
    if (it == by_pid_.end() || it->second.details.image.empty()) {
      continue;
    }
    const std::wstring &image = it->second.details.image;
    auto d = descriptions_.find(image);
    if (d == descriptions_.end()) {
      if (descriptions_.size() >= kMaxDescriptions) {
        descriptions_.clear();
      }
      d = descriptions_.emplace(image, FileDescription(image)).first;
    }
    entries[i].description = d->second;
  }
}

void ProcessSnapshotter::Clear() {
  by_pid_.clear();
  descriptions_.clear();
}

} // namespace im::audio
