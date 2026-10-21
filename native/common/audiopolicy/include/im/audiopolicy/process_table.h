// A process snapshot and its two kinds of tree (docs/m2/05-windows-audio.md §5.1, §5.2, §7.4).
//
// The Windows watcher builds a ProcessTable on every reconcile from CreateToolhelp32Snapshot (pid, parent pid, exe)
// plus the details it caches per process key (creation time, image folder, msedgewebview2.exe's --user-data-dir,
// FileDescription). The table normalizes them and links every process to its parent:
//   - the loose parent is the process whose pid is the parent pid (the PID alone);
//   - the strict parent is the loose parent when it was created no later than the child, or when either creation time
//     is unknown (0), "fail toward more matches" (05 §7.4). Creation times come from the kernel's clock tick, so a
//     child started right after its parent can carry the same time: equal times are a strict link. A parent created
//     after its child holds a reused PID.
// OS trees follow these links downwards: R's strict tree holds every process whose strict-parent chain reaches R, its
// loose tree (the union of strict and loose links, 05 §5.2) every process whose pid chain reaches R.
#pragma once

#include <cstddef>
#include <cstdint>
#include <optional>
#include <string>
#include <unordered_map>
#include <vector>

#include "im/audiopolicy/types.h"

namespace im::audiopolicy {

// One process as the watcher sees it. Unknown details stay empty or 0: an unknown folder is an app boundary, an
// unknown creation time accepts the parent link (05 §7.4).
struct ProcessEntry {
  uint32_t pid = 0;
  uint32_t parent_pid = 0;
  uint64_t created = 0;      // FILETIME of GetProcessTimes, 0 = unknown
  std::string exe;           // the image file name, UTF-8, any case ("Discord.exe")
  std::string folder;        // the image's folder, full path ("C:\...\app-1.0.9"), "" = unknown
  std::string user_data_dir; // msedgewebview2.exe only: its --user-data-dir (ParseUserDataDir), "" = none
  std::string description;   // FileDescription of the version resource, "" = unknown (05 §5.1)
};

// A normalized process of the table.
struct Process {
  ProcessKey key;
  uint32_t parent_pid = 0;
  std::string exe;            // FoldCase(entry.exe)
  std::string folder;         // NormalizePath(entry.folder)
  std::string user_data_dir;  // NormalizePath(entry.user_data_dir)
  std::string description;    // as given
  int32_t loose_parent = -1;  // table index, -1 = none
  int32_t strict_parent = -1; // table index, -1 = none (no parent, or the parent was created after this process)
};

// The longest parent chain any walk follows (05 §5.2: app walks take at most 64 steps). Chains through reused PIDs
// can be cycles; every walk stops after this many steps.
inline constexpr int kMaxWalk = 64;

class ProcessTable {
public:
  ProcessTable() = default;
  // Builds the table. A pid that appears twice keeps its first entry (Toolhelp never repeats one); pid 0 (the idle
  // process) is never anyone's parent.
  explicit ProcessTable(std::vector<ProcessEntry> entries);

  std::size_t size() const noexcept { return procs_.size(); }
  const Process &at(std::size_t i) const { return procs_.at(i); }
  const std::vector<Process> &processes() const noexcept { return procs_; }

  // The index of pid, or nullopt.
  std::optional<std::size_t> Find(uint32_t pid) const;
  // The index of the process with exactly this key, or nullopt (a reused pid has another creation time).
  std::optional<std::size_t> Find(const ProcessKey &key) const;

  // The parent index the chosen kind of tree follows: strict, or loose (strict ∪ loose, 05 §5.2).
  int32_t Parent(std::size_t i, bool loose) const {
    return loose ? procs_.at(i).loose_parent : procs_.at(i).strict_parent;
  }

  // Whether process i lies in the OS tree of root (root itself included): root is on i's strict chain, or on its pid
  // chain when loose.
  bool InTree(std::size_t i, std::size_t root, bool loose) const;

private:
  std::vector<Process> procs_;
  std::unordered_map<uint32_t, std::size_t> by_pid_;
};

} // namespace im::audiopolicy
