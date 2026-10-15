package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeHost is a proc file system in a temporary directory, laid out and formatted as Linux writes it (the
// formats were taken from a Debian 13 container), with the values a test sets. exittest reads it with -proc.
type fakeHost struct {
	t   testing.TB
	dir string
}

// hostState is the machine's side: CPU times, load, the NIC and the UDP and TCP counters.
type hostState struct {
	cpus             int
	busy, idle       uint64 // ticks, every CPU together
	load             [3]float64
	rx, tx           uint64 // eth0, the default route's interface
	loRx, loTx       uint64
	udpIn, udpOut    uint64 // IPv4; IPv6 adds udp6In and udp6Out
	udp6In, udp6Out  uint64
	rcvbuf, sndbuf   uint64 // IPv4 and IPv6 together, split 2:1 in the files
	inErrors         uint64
	retrans          uint64
	noSNMP6          bool
	defaultRouteNone bool
}

// procState is one process.
type procState struct {
	pid       int
	ticks     uint64 // utime + stime
	start     uint64
	rssKiB    uint64
	threads   int
	fds       int
	unreadFDs bool // /proc/<pid>/fd can't be listed (exittest not run as root)
}

func newFakeHost(t testing.TB) *fakeHost {
	t.Helper()
	h := &fakeHost{t: t, dir: t.TempDir()}
	h.mkdir("net")
	h.setHost(hostState{cpus: 2, busy: 1000, idle: 9000, load: [3]float64{0.1, 0.1, 0.1}})
	return h
}

func (h *fakeHost) mkdir(name string) {
	h.t.Helper()
	if err := os.MkdirAll(filepath.Join(h.dir, name), 0o750); err != nil {
		h.t.Fatal(err)
	}
}

func (h *fakeHost) write(name, content string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.dir, name), []byte(content), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *fakeHost) setHost(s hostState) {
	h.t.Helper()
	// /proc/stat: busy split into user, system and softirq, idle into idle and iowait.
	user, system, softirq := s.busy*6/10, s.busy*3/10, s.busy-s.busy*6/10-s.busy*3/10
	idle, iowait := s.idle*99/100, s.idle-s.idle*99/100
	var stat strings.Builder
	fmt.Fprintf(&stat, "cpu  %d 0 %d %d %d 0 %d 0 0 0\n", user, system, idle, iowait, softirq)
	for i := range s.cpus {
		n := uint64(s.cpus)
		fmt.Fprintf(&stat, "cpu%d %d 0 %d %d %d 0 %d 0 0 0\n", i, user/n, system/n, idle/n, iowait/n, softirq/n)
	}
	stat.WriteString("intr 123456 0 0\nctxt 987654\nbtime 1760000000\nprocesses 4321\nprocs_running 1\nprocs_blocked 0\n")
	h.write("stat", stat.String())

	h.write("loadavg", fmt.Sprintf("%.2f %.2f %.2f 2/187 4242\n", s.load[0], s.load[1], s.load[2]))

	h.write("net/dev", "Inter-|   Receive                                                |  Transmit\n"+
		" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n"+
		fmt.Sprintf("    lo: %7d %7d    0    0    0     0          0         0 %8d %7d    0    0    0     0       0          0\n",
			s.loRx, s.loRx/100, s.loTx, s.loTx/100)+
		fmt.Sprintf("  eth0: %7d %7d    0    0    0     0          0         0 %8d %7d    0    0    0     0       0          0\n",
			s.rx, s.rx/1100, s.tx, s.tx/1100))

	route := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT                                                       \n"
	if !s.defaultRouteNone {
		route += "eth0\t00000000\t010200C0\t0003\t0\t0\t100\t00000000\t0\t0\t0                                                                               \n"
	}
	route += "eth0\t000200C0\t00000000\t0001\t0\t0\t100\t00FFFFFF\t0\t0\t0                                                                               \n"
	h.write("net/route", route)

	rcv4, snd4 := s.rcvbuf-s.rcvbuf/3, s.sndbuf-s.sndbuf/3
	err4 := s.inErrors - s.inErrors/3
	h.write("net/snmp", "Ip: Forwarding DefaultTTL InReceives InHdrErrors InAddrErrors ForwDatagrams InUnknownProtos InDiscards InDelivers OutRequests OutDiscards OutNoRoutes ReasmTimeout ReasmReqds ReasmOKs ReasmFails FragOKs FragFails FragCreates OutTransmits\n"+
		"Ip: 1 64 123 0 0 0 0 0 123 120 0 0 0 0 0 0 0 0 0 120\n"+
		"Icmp: InMsgs InErrors InCsumErrors InDestUnreachs\nIcmp: 3 0 0 3\n"+
		"IcmpMsg: InType3 OutType3\nIcmpMsg: 3 3\n"+
		"Tcp: RtoAlgorithm RtoMin RtoMax MaxConn ActiveOpens PassiveOpens AttemptFails EstabResets CurrEstab InSegs OutSegs RetransSegs InErrs OutRsts InCsumErrors\n"+
		fmt.Sprintf("Tcp: 1 200 120000 -1 10 20 0 0 5 1000 1100 %d 0 3 0\n", s.retrans)+
		"Udp: InDatagrams NoPorts InErrors OutDatagrams RcvbufErrors SndbufErrors InCsumErrors IgnoredMulti MemErrors\n"+
		fmt.Sprintf("Udp: %d 7 %d %d %d %d 0 0 0\n", s.udpIn, err4, s.udpOut, rcv4, snd4)+
		"UdpLite: InDatagrams NoPorts InErrors OutDatagrams RcvbufErrors SndbufErrors InCsumErrors IgnoredMulti MemErrors\n"+
		"UdpLite: 0 0 0 0 0 0 0 0 0\n")
	if s.noSNMP6 {
		_ = os.Remove(filepath.Join(h.dir, "net/snmp6"))
	} else {
		pad := func(name string, v uint64) string { return fmt.Sprintf("%-32s\t%d\n", name, v) }
		h.write("net/snmp6", pad("Ip6InReceives", 10)+pad("Udp6InDatagrams", s.udp6In)+pad("Udp6NoPorts", 0)+
			pad("Udp6InErrors", s.inErrors/3)+pad("Udp6OutDatagrams", s.udp6Out)+pad("Udp6RcvbufErrors", s.rcvbuf/3)+
			pad("Udp6SndbufErrors", s.sndbuf/3)+pad("Udp6InCsumErrors", 0)+pad("Udp6IgnoredMulti", 0)+
			pad("Udp6MemErrors", 0)+pad("UdpLite6InDatagrams", 0))
	}
}

func (h *fakeHost) setProcess(p procState) {
	h.t.Helper()
	dir := strconv.Itoa(p.pid)
	h.mkdir(dir)
	utime, stime := p.ticks*7/10, p.ticks-p.ticks*7/10
	// The command name holds a space and a parenthesis, as a renamed process's may: exittest counts the fields
	// from the last ')'.
	h.write(dir+"/stat", fmt.Sprintf("%d (isshoni (x)) S 1 %d %d 0 -1 4194560 52311 0 12 0 %d %d 0 0 20 0 %d 0 %d "+
		"1621495808 %d 18446744073709551615 1 1 0 0 0 0 0 0 2143420159 0 0 0 17 1 0 0 0 0 0 0 0 0 0 0 0 0 0\n",
		p.pid, p.pid, p.pid, utime, stime, p.threads, p.start, p.rssKiB/4))
	h.write(dir+"/status", fmt.Sprintf("Name:\tisshoni\nUmask:\t0022\nState:\tS (sleeping)\nTgid:\t%d\nPid:\t%d\n"+
		"VmPeak:\t 1600000 kB\nVmSize:\t 1583492 kB\nVmHWM:\t %7d kB\nVmRSS:\t %7d kB\nRssAnon:\t   40000 kB\n"+
		"Threads:\t%d\n", p.pid, p.pid, p.rssKiB, p.rssKiB, p.threads))
	fd := filepath.Join(h.dir, dir, "fd")
	if err := os.RemoveAll(fd); err != nil {
		h.t.Fatal(err)
	}
	if p.unreadFDs {
		h.write(dir+"/fd", "not a directory") // listing it fails, as without root
		return
	}
	h.mkdir(dir + "/fd")
	for i := range p.fds {
		h.write(dir+"/fd/"+strconv.Itoa(i), "")
	}
}

func (h *fakeHost) removeProcess(pid int) {
	h.t.Helper()
	if err := os.RemoveAll(filepath.Join(h.dir, strconv.Itoa(pid))); err != nil {
		h.t.Fatal(err)
	}
}
