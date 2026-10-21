// A process snapshot and its two kinds of tree (docs/m2/05-windows-audio.md §5.2, §7.4).
#include "im/audiopolicy/process_table.h"

#include <utility>

#include "im/audiopolicy/text.h"

namespace im::audiopolicy {

ProcessTable::ProcessTable(std::vector<ProcessEntry> entries) {
  procs_.reserve(entries.size());
  by_pid_.reserve(entries.size());
  for (ProcessEntry &e : entries) {
    if (by_pid_.count(e.pid) != 0) {
      continue;
    }
    Process p;
    p.key = ProcessKey{e.pid, e.created};
    p.parent_pid = e.parent_pid;
    p.exe = FoldCase(e.exe);
    p.folder = NormalizePath(e.folder);
    p.user_data_dir = NormalizePath(e.user_data_dir);
    p.description = std::move(e.description);
    by_pid_.emplace(e.pid, procs_.size());
    procs_.push_back(std::move(p));
  }
  for (std::size_t i = 0; i < procs_.size(); ++i) {
    Process &p = procs_[i];
    if (p.parent_pid == 0 || p.parent_pid == p.key.pid) {
      continue;
    }
    const auto it = by_pid_.find(p.parent_pid);
    if (it == by_pid_.end()) {
      continue;
    }
    const Process &parent = procs_[it->second];
    p.loose_parent = static_cast<int32_t>(it->second);
    const bool unknown = parent.key.created == 0 || p.key.created == 0;
    if (unknown || parent.key.created <= p.key.created) {
      p.strict_parent = p.loose_parent;
    }
  }
}

std::optional<std::size_t> ProcessTable::Find(uint32_t pid) const {
  const auto it = by_pid_.find(pid);
  if (it == by_pid_.end()) {
    return std::nullopt;
  }
  return it->second;
}

std::optional<std::size_t> ProcessTable::Find(const ProcessKey &key) const {
  const auto i = Find(key.pid);
  if (!i || procs_[*i].key != key) {
    return std::nullopt;
  }
  return i;
}

bool ProcessTable::InTree(std::size_t i, std::size_t root, bool loose) const {
  int32_t cur = static_cast<int32_t>(i);
  for (int step = 0; cur >= 0 && step <= kMaxWalk; ++step) {
    if (static_cast<std::size_t>(cur) == root) {
      return true;
    }
    cur = Parent(static_cast<std::size_t>(cur), loose);
  }
  return false;
}

} // namespace im::audiopolicy
