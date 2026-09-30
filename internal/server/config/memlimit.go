package config

import (
	"bytes"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
)

// cgroupUnlimited: a cgroup memory limit at or above this is "no limit" (cgroup v1 reports about 2^63, rounded down
// to a page).
const cgroupUnlimited = 1 << 62

// CgroupMemoryLimit returns the memory limit of the process's cgroup in bytes, and whether there is one (§5.1).
// cgroup v2: the smallest memory.max from the process's cgroup (/proc/self/cgroup) up to the root of
// /sys/fs/cgroup; v1: the same walk over memory.limit_in_bytes under /sys/fs/cgroup/memory. Inside a container the
// walk ends at the container's own cgroup, which the container sees as the root. "max" and v1's huge value mean no
// limit. On a machine without cgroups it returns false.
func (h Host) CgroupMemoryLimit() (int64, bool) {
	data, err := os.ReadFile(h.file("/proc/self/cgroup"))
	if err != nil {
		return 0, false
	}
	best, found := int64(0), false
	take := func(v int64, ok bool) {
		if ok && (!found || v < best) {
			best, found = v, true
		}
	}
	for line := range bytes.Lines(data) {
		// hierarchy-ID:controller-list:cgroup-path
		parts := strings.SplitN(strings.TrimSpace(string(line)), ":", 3)
		if len(parts) != 3 {
			continue
		}
		switch {
		case parts[0] == "0" && parts[1] == "":
			take(h.minCgroupLimit("/sys/fs/cgroup", parts[2], "memory.max"))
		case hasController(parts[1], "memory"):
			take(h.minCgroupLimit("/sys/fs/cgroup/memory", parts[2], "memory.limit_in_bytes"))
		}
	}
	return best, found
}

// hasController reports whether the comma-separated controller list of a /proc/self/cgroup line names c.
func hasController(list, c string) bool {
	for name := range strings.SplitSeq(list, ",") {
		if name == c {
			return true
		}
	}
	return false
}

// minCgroupLimit returns the smallest limit in the file named file of the cgroup rel under base and of each of its
// parents up to base.
func (h Host) minCgroupLimit(base, rel, file string) (int64, bool) {
	rel = path.Clean("/" + rel)
	best, found := int64(0), false
	for {
		if v, ok := readCgroupLimit(h.file(path.Join(base, rel, file))); ok && (!found || v < best) {
			best, found = v, true
		}
		if rel == "/" {
			return best, found
		}
		rel = path.Dir(rel)
	}
}

// readCgroupLimit reads a memory.max or memory.limit_in_bytes file; false for a missing file, "max" or no limit.
func readCgroupLimit(file string) (int64, bool) {
	data, err := os.ReadFile(filepath.Clean(file))
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || v <= 0 || v >= cgroupUnlimited {
		return 0, false
	}
	return v, true
}

// ApplyMemoryLimit sets the Go memory limit (debug.SetMemoryLimit) to 80 % of the cgroup memory limit when GOMEMLIMIT
// is unset and there is a cgroup limit (§5.1, §6.1 step 2), logs it, and returns the limit it set, or 0 when it set
// none. Go already sizes GOMAXPROCS from the cgroup CPU limit.
func ApplyMemoryLimit(h Host, log *slog.Logger) int64 {
	if h.getenv("GOMEMLIMIT") != "" {
		return 0
	}
	limit, ok := h.CgroupMemoryLimit()
	if !ok {
		return 0
	}
	soft := limit / 5 * 4
	debug.SetMemoryLimit(soft)
	componentLogger(log).Info("set the Go memory limit to 80% of the cgroup memory limit",
		"cgroup_limit_bytes", limit, "go_limit_bytes", soft)
	return soft
}
