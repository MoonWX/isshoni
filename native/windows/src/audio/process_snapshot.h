// The process table's Windows half (docs/m2/05-windows-audio.md §5.1, §7.4; "05" below): Toolhelp snapshots with the
// details 05's policy needs, cached per process, as im_audiopolicy's ProcessEntry rows. The policy itself
// (audiopolicy::ProcessTable, its links and trees) is portable and tested in native/common.
//
// Owned by one thread (the watcher's); not thread-safe.
#pragma once

#include <cstddef>
#include <cstdint>
#include <span>
#include <string>
#include <string_view>
#include <unordered_map>
#include <vector>

#include "im/audiopolicy/process_table.h"

namespace im::audio {

// What OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION) tells about one process (05 §7.4). Failures leave fields empty:
// an unknown folder is an app boundary, an unknown creation time (0) accepts the parent link (05 §5.2).
struct ProcessDetails {
  bool opened = false;       // OpenProcess succeeded (protected processes refuse it)
  uint64_t created = 0;      // GetProcessTimes' creation time (FILETIME), 0 = unknown
  std::wstring image;        // QueryFullProcessImageNameW (Win32 form), "" = unknown
  std::string folder;        // the image's folder, UTF-8, without a trailing separator, "" = unknown
  std::string user_data_dir; // msedgewebview2.exe only: its --user-data-dir (audiopolicy::ParseUserDataDir), as given
};

// Reads one process's details; the command line (NtQueryInformationProcess(ProcessCommandLineInformation)) only when
// read_command_line, which the table asks for msedgewebview2.exe alone (05 §7.4).
ProcessDetails QueryProcessDetails(uint32_t pid, bool read_command_line);

// The command line of an open process (PROCESS_QUERY_LIMITED_INFORMATION is enough on Windows 8.1 and later), "" when
// it can't be read.
std::wstring ReadCommandLine(void *process);

// The FileDescription of an image's version resource (first language), UTF-8; "" when it has none (05 §5.1).
std::string FileDescription(const std::wstring &image);

// Whether an exe file name is WebView2's runtime, msedgewebview2.exe (any case).
bool IsWebView2Exe(std::wstring_view exe) noexcept;

class ProcessSnapshotter {
public:
  // A fresh snapshot (CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS)): pid, parent pid and exe name of every process,
  // protected ones included, plus each one's details, read only for a pid not seen before or whose parent or exe
  // changed (05 §7.4: refresh ≤ 3 ms for 300 processes). Descriptions are left empty (Describe). False, with out
  // empty, when Toolhelp fails.
  bool Take(std::vector<audiopolicy::ProcessEntry> &out);

  // Fills entries[i].description for each i in which, cached per image path (05 §5.1); entries must come from the
  // last Take. Only the processes a report row can be named after need it (session processes and their ancestors).
  void Describe(std::vector<audiopolicy::ProcessEntry> &entries, std::span<const std::size_t> which);

  // Forgets every cached detail.
  void Clear();

  std::size_t cached() const noexcept { return by_pid_.size(); }

private:
  struct Cached {
    uint32_t parent_pid = 0;
    std::wstring exe;
    ProcessDetails details;
  };
  std::unordered_map<uint32_t, Cached> by_pid_;
  std::unordered_map<std::wstring, std::string> descriptions_; // by image path
};

} // namespace im::audio
