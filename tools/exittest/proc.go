package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

// userHZ is the unit of the CPU times in /proc/<pid>/stat and /proc/stat (USER_HZ). It is 100 on every
// architecture a VPS runs (x86-64, arm64, riscv64); sysconf(_SC_CLK_TCK) would say so, but the standard library
// has no sysconf without cgo.
const userHZ = 100

// nicAll is the -nic value that counts every interface except lo.
const nicAll = "all"

// procFS reads the proc file system below one directory (-proc, /proc by default). Every file is opened through
// an os.Root, so nothing outside that directory is ever read.
type procFS struct {
	root *os.Root
}

func openProc(dir string) (*procFS, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &procFS{root: root}, nil
}

func (p *procFS) Close() error { return p.root.Close() }

// processStat is what /proc/<pid>/stat and /proc/<pid>/status say about one process.
type processStat struct {
	cpuTicks   uint64 // utime + stime, in 1/userHZ s
	startTicks uint64 // starttime: when the process started, in ticks after boot; tells a reused PID apart
	threads    uint64
	rssBytes   uint64 // VmRSS
}

// process reads one process's stat and status files. An error that wraps fs.ErrNotExist means the process is gone.
func (p *procFS) process(pid int) (processStat, error) {
	dir := strconv.Itoa(pid)
	data, err := p.root.ReadFile(dir + "/stat")
	if err != nil {
		return processStat{}, err
	}
	st, err := parseStat(data)
	if err != nil {
		return processStat{}, fmt.Errorf("%s/stat: %w", dir, err)
	}
	data, err = p.root.ReadFile(dir + "/status")
	if err != nil {
		return processStat{}, err
	}
	if st.rssBytes, err = parseStatusRSS(data); err != nil {
		return processStat{}, fmt.Errorf("%s/status: %w", dir, err)
	}
	return st, nil
}

// parseStat reads utime, stime, num_threads and starttime from a /proc/<pid>/stat line. The command name, field 2,
// is in parentheses and may itself hold spaces and parentheses, so the fields are counted from the last ')'.
func parseStat(data []byte) (processStat, error) {
	end := bytes.LastIndexByte(data, ')')
	if end < 0 {
		return processStat{}, errors.New("no command name")
	}
	f := strings.Fields(string(data[end+1:])) // f[0] is field 3, state
	const (
		utime     = 14 - 3
		stime     = 15 - 3
		threads   = 20 - 3
		starttime = 22 - 3
	)
	if len(f) <= starttime {
		return processStat{}, fmt.Errorf("%d fields after the command name, want more than %d", len(f), starttime)
	}
	var st processStat
	var vals [4]uint64
	for i, idx := range []int{utime, stime, threads, starttime} {
		v, err := strconv.ParseUint(f[idx], 10, 64)
		if err != nil {
			return processStat{}, fmt.Errorf("field %d: %w", idx+3, err)
		}
		vals[i] = v
	}
	st.cpuTicks = vals[0] + vals[1]
	st.threads = vals[2]
	st.startTicks = vals[3]
	return st, nil
}

// parseStatusRSS reads the VmRSS line ("VmRSS:    123456 kB") of /proc/<pid>/status.
func parseStatusRSS(data []byte) (uint64, error) {
	for line := range strings.Lines(string(data)) {
		rest, ok := strings.CutPrefix(line, "VmRSS:")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) != 2 || f[1] != "kB" {
			return 0, fmt.Errorf("VmRSS line %q", strings.TrimSpace(line))
		}
		kb, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("VmRSS: %w", err)
		}
		return kb * 1024, nil
	}
	return 0, errors.New("no VmRSS line")
}

// openFDs counts the entries of /proc/<pid>/fd. Only root (or the process's own user) may list it.
func (p *procFS) openFDs(pid int) (int, error) {
	d, err := p.root.Open(strconv.Itoa(pid) + "/fd")
	if err != nil {
		return 0, err
	}
	defer func() { _ = d.Close() }()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return 0, err
	}
	return len(names), nil
}

// cpuTimes is the first line of /proc/stat (every CPU together) and the number of CPUs.
type cpuTimes struct {
	busy, total uint64 // in 1/userHZ s; idle and iowait are not busy
	cpus        int
}

func (p *procFS) cpu() (cpuTimes, error) {
	data, err := p.root.ReadFile("stat")
	if err != nil {
		return cpuTimes{}, err
	}
	return parseProcStat(data)
}

// parseProcStat reads /proc/stat: the "cpu" line's user nice system idle iowait irq softirq steal (guest time is
// already inside user), and one "cpuN" line per CPU.
func parseProcStat(data []byte) (cpuTimes, error) {
	var ct cpuTimes
	found := false
	for line := range strings.Lines(string(data)) {
		f := strings.Fields(line)
		if len(f) == 0 || !strings.HasPrefix(f[0], "cpu") {
			continue
		}
		if f[0] != "cpu" {
			if _, err := strconv.Atoi(f[0][3:]); err == nil {
				ct.cpus++
			}
			continue
		}
		if len(f) < 9 {
			return cpuTimes{}, fmt.Errorf("the cpu line has %d fields", len(f))
		}
		var v [8]uint64
		for i := range v {
			n, err := strconv.ParseUint(f[1+i], 10, 64)
			if err != nil {
				return cpuTimes{}, fmt.Errorf("the cpu line: %w", err)
			}
			v[i] = n
		}
		for _, n := range v {
			ct.total += n
		}
		ct.busy = ct.total - v[3] - v[4]
		found = true
	}
	if !found {
		return cpuTimes{}, errors.New("no cpu line")
	}
	if ct.cpus == 0 {
		return cpuTimes{}, errors.New("no cpuN lines")
	}
	return ct, nil
}

// loadavg reads the three load averages of /proc/loadavg.
func (p *procFS) loadavg() ([3]float64, error) {
	data, err := p.root.ReadFile("loadavg")
	if err != nil {
		return [3]float64{}, err
	}
	f := strings.Fields(string(data))
	if len(f) < 3 {
		return [3]float64{}, fmt.Errorf("loadavg %q", strings.TrimSpace(string(data)))
	}
	var l [3]float64
	for i := range l {
		v, err := strconv.ParseFloat(f[i], 64)
		if err != nil {
			return [3]float64{}, fmt.Errorf("loadavg: %w", err)
		}
		l[i] = v
	}
	return l, nil
}

// netDev sums the received and sent bytes of one interface in /proc/net/dev, or of every interface except lo when
// nic is nicAll.
func (p *procFS) netDev(nic string) (rx, tx uint64, err error) {
	data, err := p.root.ReadFile("net/dev")
	if err != nil {
		return 0, 0, err
	}
	return parseNetDev(data, nic)
}

func parseNetDev(data []byte, nic string) (rx, tx uint64, err error) {
	found := false
	for line := range strings.Lines(string(data)) {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue // the two header lines have "|" and no colon
		}
		name = strings.TrimSpace(name)
		if nic == nicAll && name == "lo" || nic != nicAll && name != nic {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			return 0, 0, fmt.Errorf("net/dev: %s has %d fields", name, len(f))
		}
		r, err1 := strconv.ParseUint(f[0], 10, 64)
		t, err2 := strconv.ParseUint(f[8], 10, 64)
		if err := errors.Join(err1, err2); err != nil {
			return 0, 0, fmt.Errorf("net/dev: %s: %w", name, err)
		}
		rx += r
		tx += t
		found = true
	}
	if !found && nic != nicAll {
		return 0, 0, fmt.Errorf("net/dev has no interface %q", nic)
	}
	return rx, tx, nil
}

// defaultRouteNIC names the interface of the IPv4 default route with the lowest metric in /proc/net/route, or ""
// when there is none.
func (p *procFS) defaultRouteNIC() (string, error) {
	data, err := p.root.ReadFile("net/route")
	if err != nil {
		return "", err
	}
	return parseRoute(data)
}

func parseRoute(data []byte) (string, error) {
	const rtfUp = 0x1
	best, bestMetric := "", uint64(0)
	first := true
	for line := range strings.Lines(string(data)) {
		if first { // Iface Destination Gateway Flags RefCnt Use Metric Mask MTU Window IRTT
			first = false
			continue
		}
		f := strings.Fields(line)
		if len(f) < 8 || f[1] != "00000000" || f[7] != "00000000" {
			continue
		}
		flags, err1 := strconv.ParseUint(f[3], 16, 32)
		metric, err2 := strconv.ParseUint(f[6], 10, 64)
		if err := errors.Join(err1, err2); err != nil {
			return "", fmt.Errorf("net/route: %w", err)
		}
		if flags&rtfUp == 0 {
			continue
		}
		if best == "" || metric < bestMetric {
			best, bestMetric = f[0], metric
		}
	}
	return best, nil
}

// netCounters are the UDP and TCP counters of 06 §12.3. The UDP ones add IPv4 (/proc/net/snmp) and IPv6
// (/proc/net/snmp6), which Linux counts apart; TCP's MIB already counts both.
type netCounters struct {
	udpIn, udpOut, udpRcvbufErrors, udpSndbufErrors, udpInErrors, tcpRetransSegs uint64
}

func (p *procFS) netCounters() (netCounters, error) {
	data, err := p.root.ReadFile("net/snmp")
	if err != nil {
		return netCounters{}, err
	}
	nc, err := parseSNMP(data)
	if err != nil {
		return netCounters{}, err
	}
	data, err = p.root.ReadFile("net/snmp6")
	if errors.Is(err, fs.ErrNotExist) {
		return nc, nil // IPv6 is off
	}
	if err != nil {
		return netCounters{}, err
	}
	if err := addSNMP6(&nc, data); err != nil {
		return netCounters{}, err
	}
	return nc, nil
}

// parseSNMP reads /proc/net/snmp, where each protocol has a line of names and then a line of values.
func parseSNMP(data []byte) (netCounters, error) {
	tables := make(map[string]map[string]string)
	var names []string
	var proto string
	for line := range strings.Lines(string(data)) {
		p, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if names == nil || p != proto {
			proto, names = p, f
			continue
		}
		if len(f) != len(names) {
			return netCounters{}, fmt.Errorf("net/snmp: %s has %d names and %d values", p, len(names), len(f))
		}
		t := make(map[string]string, len(f))
		for i, n := range names {
			t[n] = f[i]
		}
		tables[p] = t
		names = nil
	}
	var nc netCounters
	for _, c := range []struct {
		proto, name string
		dst         *uint64
	}{
		{"Udp", "InDatagrams", &nc.udpIn},
		{"Udp", "OutDatagrams", &nc.udpOut},
		{"Udp", "RcvbufErrors", &nc.udpRcvbufErrors},
		{"Udp", "SndbufErrors", &nc.udpSndbufErrors},
		{"Udp", "InErrors", &nc.udpInErrors},
		{"Tcp", "RetransSegs", &nc.tcpRetransSegs},
	} {
		s, ok := tables[c.proto][c.name]
		if !ok {
			return netCounters{}, fmt.Errorf("net/snmp has no %s %s", c.proto, c.name)
		}
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return netCounters{}, fmt.Errorf("net/snmp %s %s: %w", c.proto, c.name, err)
		}
		*c.dst = v
	}
	return nc, nil
}

// addSNMP6 adds the Udp6 counters of /proc/net/snmp6 ("Udp6InDatagrams  123" lines).
func addSNMP6(nc *netCounters, data []byte) error {
	dst := map[string]*uint64{
		"Udp6InDatagrams":  &nc.udpIn,
		"Udp6OutDatagrams": &nc.udpOut,
		"Udp6RcvbufErrors": &nc.udpRcvbufErrors,
		"Udp6SndbufErrors": &nc.udpSndbufErrors,
		"Udp6InErrors":     &nc.udpInErrors,
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 {
			continue
		}
		p, ok := dst[f[0]]
		if !ok {
			continue
		}
		v, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			return fmt.Errorf("net/snmp6 %s: %w", f[0], err)
		}
		*p += v
	}
	return sc.Err()
}
