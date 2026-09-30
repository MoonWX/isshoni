package config

import (
	"runtime/debug"
	"strings"
	"testing"
)

func TestCgroupMemoryLimit(t *testing.T) {
	const gib = 1 << 30
	tests := []struct {
		name   string
		files  map[string]string
		want   int64
		wantOK bool
	}{
		{"no cgroups", nil, 0, false},
		{"v2 systemd service", map[string]string{
			"/proc/self/cgroup": "0::/system.slice/isshoni.service\n",
			"/sys/fs/cgroup/system.slice/isshoni.service/memory.max": "1073741824\n",
			"/sys/fs/cgroup/system.slice/memory.max":                 "max\n",
		}, gib, true},
		{"v2 parent is lower", map[string]string{
			"/proc/self/cgroup": "0::/system.slice/isshoni.service\n",
			"/sys/fs/cgroup/system.slice/isshoni.service/memory.max": "max\n",
			"/sys/fs/cgroup/system.slice/memory.max":                 "536870912\n",
		}, gib / 2, true},
		{"v2 container (cgroup namespace)", map[string]string{
			"/proc/self/cgroup":         "0::/\n",
			"/sys/fs/cgroup/memory.max": "2147483648\n",
		}, 2 * gib, true},
		{"v2 host path not visible", map[string]string{
			"/proc/self/cgroup":         "0::/system.slice/docker-0f1e.scope\n",
			"/sys/fs/cgroup/memory.max": "2147483648\n",
		}, 2 * gib, true},
		{"v2 no limit", map[string]string{
			"/proc/self/cgroup":         "0::/user.slice\n",
			"/sys/fs/cgroup/memory.max": "max\n",
		}, 0, false},
		{"v1 container", map[string]string{
			"/proc/self/cgroup":                           "12:cpu,cpuacct:/docker/0f1e\n5:memory:/docker/0f1e\n1:name=systemd:/docker/0f1e\n",
			"/sys/fs/cgroup/memory/memory.limit_in_bytes": "536870912\n",
		}, gib / 2, true},
		{"v1 unlimited", map[string]string{
			"/proc/self/cgroup":                           "5:memory:/\n",
			"/sys/fs/cgroup/memory/memory.limit_in_bytes": "9223372036854771712\n",
		}, 0, false},
		{"garbage", map[string]string{
			"/proc/self/cgroup":         "nonsense\n0::/\n",
			"/sys/fs/cgroup/memory.max": "lots\n",
		}, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := fakeHost(t)
			for p, content := range tt.files {
				writeHostFile(t, h, p, content)
			}
			got, ok := h.CgroupMemoryLimit()
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("CgroupMemoryLimit = %d, %v; want %d, %v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestApplyMemoryLimit(t *testing.T) {
	old := debug.SetMemoryLimit(-1)
	defer debug.SetMemoryLimit(old)

	h := fakeHost(t)
	writeHostFile(t, h, "/proc/self/cgroup", "0::/\n")
	writeHostFile(t, h, "/sys/fs/cgroup/memory.max", "1000000000\n")

	pinned := Host{Environ: []string{"GOMEMLIMIT=off"}, Root: h.Root}
	if got := ApplyMemoryLimit(pinned, nil); got != 0 || debug.SetMemoryLimit(-1) != old {
		t.Errorf("with GOMEMLIMIT set: ApplyMemoryLimit = %d and the limit changed", got)
	}
	if got := ApplyMemoryLimit(fakeHost(t), nil); got != 0 || debug.SetMemoryLimit(-1) != old {
		t.Errorf("without a cgroup limit: ApplyMemoryLimit = %d and the limit changed", got)
	}
	log, buf := captureLog()
	if got := ApplyMemoryLimit(h, log); got != 800000000 || debug.SetMemoryLimit(-1) != 800000000 {
		t.Errorf("ApplyMemoryLimit = %d, runtime limit %d; want 800000000", got, debug.SetMemoryLimit(-1))
	}
	if !strings.Contains(buf.String(), "go_limit_bytes=800000000") {
		t.Errorf("no log line:\n%s", buf.String())
	}
}
