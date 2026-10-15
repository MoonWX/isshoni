package main

import (
	"errors"
	"io/fs"
	"os"
	"runtime"
	"testing"
)

func TestParseStat(t *testing.T) {
	// A real line (Debian 13, `cat /proc/self/stat`), and the same with a command name that holds ") (".
	for _, line := range []string{
		"11 (cat) R 1 1 1 0 -1 4194304 48 0 0 0 7 3 0 0 20 0 1 0 4982366 2646016 256 18446744073709551615 187650459041792 0 0 0 0 0 0 0 0 0 17 2 0 0 0 0 0 0 0 0 0 0 0 0\n",
		"11 (we (i) rd) R 1 1 1 0 -1 4194304 48 0 0 0 7 3 0 0 20 0 1 0 4982366 2646016 256 18446744073709551615 187650459041792 0 0 0 0 0 0 0 0 0 17 2 0 0 0 0 0 0 0 0 0 0 0 0\n",
	} {
		st, err := parseStat([]byte(line))
		if err != nil {
			t.Fatalf("parseStat(%q): %v", line, err)
		}
		if want := (processStat{cpuTicks: 10, startTicks: 4982366, threads: 1}); st != want {
			t.Errorf("parseStat(%q) = %+v, want %+v", line, st, want)
		}
	}
	for _, bad := range []string{"", "11 cat R 1", "11 (cat) R 1 1 1", "11 (cat) R 1 1 1 0 -1 4194304 48 0 0 0 x 3 0 0 20 0 1 0 4982366"} {
		if _, err := parseStat([]byte(bad)); err == nil {
			t.Errorf("parseStat(%q) succeeded", bad)
		}
	}
}

func TestParseStatusRSS(t *testing.T) {
	rss, err := parseStatusRSS([]byte("Name:\tcat\nVmHWM:\t    1432 kB\nVmRSS:\t    1432 kB\nThreads:\t1\n"))
	if err != nil || rss != 1432*1024 {
		t.Errorf("parseStatusRSS = %d, %v, want %d", rss, err, 1432*1024)
	}
	for _, bad := range []string{"Name:\tkthreadd\nThreads:\t1\n", "VmRSS:\t12 MB\n", "VmRSS:\tx kB\n"} {
		if _, err := parseStatusRSS([]byte(bad)); err == nil {
			t.Errorf("parseStatusRSS(%q) succeeded", bad)
		}
	}
}

func TestParseProcStat(t *testing.T) {
	ct, err := parseProcStat([]byte("cpu  2410389 0 583340 45994073 93701 0 392685 0 0 0\n" +
		"cpu0 308502 0 71047 4476930 11465 0 225439 0 0 0\ncpu1 234259 0 59267 4602277 11874 0 79292 0 0 0\n" +
		"intr 1 2 3\nctxt 4\n"))
	if err != nil {
		t.Fatal(err)
	}
	total := uint64(2410389 + 583340 + 45994073 + 93701 + 392685)
	if want := (cpuTimes{busy: total - 45994073 - 93701, total: total, cpus: 2}); ct != want {
		t.Errorf("parseProcStat = %+v, want %+v", ct, want)
	}
	for _, bad := range []string{"intr 1\n", "cpu  1 2 3\ncpu0 1 2 3 4 5 6 7 8\n", "cpu  1 2 3 4 5 6 7 8 9 10\n"} {
		if _, err := parseProcStat([]byte(bad)); err == nil {
			t.Errorf("parseProcStat(%q) succeeded", bad)
		}
	}
}

const netDevFixture = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo:     100       1    0    0    0     0          0         0      100       1    0    0    0     0       0          0
 tunl0:       0       0    0    0    0     0          0         0        0       0    0    0    0     0       0          0
  eth0:     302       3    0    0    0     0          0         0       42       1    0    0    0     0       0          0
  ens4:1000       3    0    0    0     0          0         0     2000       1    0    0    0     0       0          0
`

func TestParseNetDev(t *testing.T) {
	for _, tc := range []struct {
		nic    string
		rx, tx uint64
	}{
		{"eth0", 302, 42},
		{"ens4", 1000, 2000}, // an old kernel's line, with no blank after the colon
		{"lo", 100, 100},
		{nicAll, 1302, 2042}, // everything but lo
	} {
		rx, tx, err := parseNetDev([]byte(netDevFixture), tc.nic)
		if err != nil || rx != tc.rx || tx != tc.tx {
			t.Errorf("parseNetDev(%s) = %d, %d, %v, want %d, %d", tc.nic, rx, tx, err, tc.rx, tc.tx)
		}
	}
	if _, _, err := parseNetDev([]byte(netDevFixture), "wlan0"); err == nil {
		t.Error("parseNetDev(wlan0) succeeded for an interface that isn't there")
	}
}

func TestParseRoute(t *testing.T) {
	const head = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT   \n"
	for _, tc := range []struct {
		name, table, want string
	}{
		{"one", head + "eth0\t00000000\t010200C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n" +
			"eth0\t000200C0\t00000000\t0001\t0\t0\t100\t00FFFFFF\t0\t0\t0\n", "eth0"},
		{"lowest metric", head + "wg0\t00000000\t00000000\t0001\t0\t0\t200\t00000000\t0\t0\t0\n" +
			"ens3\t00000000\t010200C0\t0003\t0\t0\t50\t00000000\t0\t0\t0\n", "ens3"},
		{"down", head + "eth1\t00000000\t010200C0\t0002\t0\t0\t10\t00000000\t0\t0\t0\n", ""},
		{"none", head + "eth0\t000200C0\t00000000\t0001\t0\t0\t100\t00FFFFFF\t0\t0\t0\n", ""},
	} {
		got, err := parseRoute([]byte(tc.table))
		if err != nil || got != tc.want {
			t.Errorf("%s: parseRoute = %q, %v, want %q", tc.name, got, err, tc.want)
		}
	}
}

func TestNetCounters(t *testing.T) {
	h := newFakeHost(t)
	h.setHost(hostState{cpus: 2, busy: 1, idle: 1, udpIn: 1000, udpOut: 2000, udp6In: 10, udp6Out: 20, rcvbuf: 9,
		sndbuf: 3, inErrors: 6, retrans: 77})
	p, err := openProc(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	nc, err := p.netCounters()
	if err != nil {
		t.Fatal(err)
	}
	want := netCounters{udpIn: 1010, udpOut: 2020, udpRcvbufErrors: 9, udpSndbufErrors: 3, udpInErrors: 6, tcpRetransSegs: 77}
	if nc != want {
		t.Errorf("netCounters = %+v, want %+v (IPv4 and IPv6 added up)", nc, want)
	}
	// Without IPv6 the IPv4 counters alone.
	h.setHost(hostState{cpus: 2, busy: 1, idle: 1, udpIn: 1000, udpOut: 2000, rcvbuf: 9, retrans: 77, noSNMP6: true})
	nc, err = p.netCounters()
	if err != nil {
		t.Fatal(err)
	}
	if want := (netCounters{udpIn: 1000, udpOut: 2000, udpRcvbufErrors: 6, tcpRetransSegs: 77}); nc != want {
		t.Errorf("netCounters without snmp6 = %+v, want %+v", nc, want)
	}
	for _, bad := range []string{
		"Udp: InDatagrams NoPorts\nUdp: 1 2\n", // no Tcp
		"Tcp: RetransSegs\nTcp: 1\nUdp: InDatagrams NoPorts InErrors OutDatagrams RcvbufErrors SndbufErrors\nUdp: 1 2 3\n",
		"Tcp: RetransSegs\nTcp: x\nUdp: InDatagrams InErrors OutDatagrams RcvbufErrors SndbufErrors\nUdp: 1 2 3 4 5\n",
	} {
		if _, err := parseSNMP([]byte(bad)); err == nil {
			t.Errorf("parseSNMP(%q) succeeded", bad)
		}
	}
}

func TestProcessAndFDs(t *testing.T) {
	h := newFakeHost(t)
	h.setProcess(procState{pid: 4127, ticks: 1234, start: 777, rssKiB: 51200, threads: 14, fds: 23})
	p, err := openProc(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	st, err := p.process(4127)
	if err != nil {
		t.Fatal(err)
	}
	if want := (processStat{cpuTicks: 1234, startTicks: 777, threads: 14, rssBytes: 51200 * 1024}); st != want {
		t.Errorf("process = %+v, want %+v", st, want)
	}
	if n, err := p.openFDs(4127); err != nil || n != 23 {
		t.Errorf("openFDs = %d, %v, want 23", n, err)
	}
	if _, err := p.process(4128); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("process of a PID that is gone: %v, want fs.ErrNotExist", err)
	}
}

// TestRealProc reads the machine's own /proc on Linux (CI), the test process standing in for isshoni: the
// formats, and the symbolic links /proc/net -> self/net -> <pid>/net that the os.Root must follow.
func TestRealProc(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs Linux's /proc")
	}
	p, err := openProc("/proc")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	st, err := p.process(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if st.rssBytes == 0 || st.threads == 0 || st.startTicks == 0 {
		t.Errorf("process = %+v, want an RSS, threads and a start time", st)
	}
	if n, err := p.openFDs(os.Getpid()); err != nil || n < 3 {
		t.Errorf("openFDs = %d, %v, want at least stdin, stdout and stderr", n, err)
	}
	if ct, err := p.cpu(); err != nil || ct.cpus < 1 || ct.total == 0 {
		t.Errorf("cpu = %+v, %v", ct, err)
	}
	if _, err := p.loadavg(); err != nil {
		t.Errorf("loadavg: %v", err)
	}
	if _, _, err := p.netDev("lo"); err != nil {
		t.Errorf("netDev(lo): %v", err)
	}
	if _, _, err := p.netDev(nicAll); err != nil {
		t.Errorf("netDev(all): %v", err)
	}
	if _, err := p.defaultRouteNIC(); err != nil {
		t.Errorf("defaultRouteNIC: %v", err)
	}
	if _, err := p.netCounters(); err != nil {
		t.Errorf("netCounters: %v", err)
	}
}
