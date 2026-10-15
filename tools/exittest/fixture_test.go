package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite testdata/run.csv and testdata/summary.md")

// testdata/run.csv is a recording of a simulated exit test: the real collector (newCollector, sample, scrape,
// close, with the real /proc readers and HTTP scrapes) driven through the run book's timeline (06 §12.2) against a
// fake proc tree and a fake metrics endpoint whose numbers follow who is in the room and who shares. Only the
// clock is the simulation's. TestFixtureIsCurrent records it again and compares, so the fixture always has the
// format the collector writes; `go test ./exittest -run 'Fixture|Summarize' -update` rewrites it and the summary.
//
// The simulation counts in integers only: the bytes must come out the same on amd64 and arm64, where Go may fuse
// a floating-point multiply and add.

// simT0 is T+0 of the simulated session.
var simT0 = time.Date(2026, 10, 15, 18, 0, 0, 0, time.UTC)

func mm(minutes, seconds int) time.Duration {
	return time.Duration(minutes)*time.Minute + time.Duration(seconds)*time.Second
}

// simTimeline is who is in the room, how many share, and how many PeerConnections are on TCP 443 (E's), from
// each time on.
var simTimeline = []struct {
	from                         time.Duration
	participants, shares, tcp443 int
}{
	{mm(-15, 0), 0, 0, 0},
	{mm(-5, 0), 1, 0, 0},  // B joins
	{mm(-4, 50), 1, 1, 0}, // B shares a window
	{mm(-4, 0), 2, 1, 1},  // E joins, its sub PC on TCP 443
	{mm(0, 0), 3, 1, 1},   // A
	{mm(0, 30), 4, 1, 1},  // C
	{mm(0, 50), 5, 1, 1},  // D
	{mm(5, 0), 5, 2, 1},   // A shares the film
	{mm(19, 0), 5, 2, 0},  // C, D and E leave; E's PC closes with its tab
	{mm(19, 40), 2, 2, 0}, // their resume grace is over
	{mm(20, 0), 2, 1, 0},  // A stops and restarts the share
	{mm(20, 3), 2, 2, 0},
	{mm(20, 25), 3, 2, 0}, // C, D and E come back through the notification
	{mm(20, 35), 4, 2, 0},
	{mm(20, 50), 5, 2, 1},
	{mm(59, 58), 5, 2, 0}, // sudo systemctl restart isshoni: the old process shuts down
	{simStart, 0, 0, 0},   // the new process
	{mm(60, 16), 2, 0, 0}, // the clients reconnect
	{mm(60, 20), 3, 0, 0},
	{mm(60, 22), 3, 1, 0}, // B's share resumes
	{mm(60, 24), 5, 1, 0},
	{mm(60, 26), 5, 2, 0}, // A's
	{mm(60, 28), 5, 2, 1}, // E's PC on TCP 443 again
	{mm(86, 0), 4, 2, 1},  // C's phone is locked; its grace ends
	{mm(87, 5), 5, 2, 1},  // C unlocks
	{mm(90, 0), 5, 3, 2},  // E shares too, its pub PC on TCP 443
	{mm(100, 0), 5, 2, 1},
}

// The restart at T+60: SIGTERM, the old process gone, the new one started, its metrics listener up.
const (
	simPID1, simPID2 = 4127, 31877
	simTerm          = 59*time.Minute + 58*time.Second
	simExit          = 60*time.Minute + 6*time.Second
	simStart         = 60*time.Minute + 11*time.Second
	simMetricsUp     = 60*time.Minute + 13*time.Second
)

// simCounters are a process's counters since it started.
type simCounters struct {
	ingress, egress, trIn, trOut, nack, pli, switches, resumed, notResumed uint64
	decoded, dropped, freezeMs, lost, audio, concealed, microTicks         uint64
}

type sim struct {
	t                            time.Duration
	rng                          uint64
	participants, shares, tcp443 int
	cpuMilli                     uint64 // the process's CPU in thousandths of a percent of all CPUs
	proc                         simCounters
	// The machine, since boot.
	busyMicro, totalTicks                          uint64
	rx, tx, udpIn, udpOut, udp6In, udp6Out, rcvbuf uint64
	retrans                                        uint64
}

// jitter returns a deterministic number in [0, n) (splitmix64).
func (s *sim) jitter(n uint64) uint64 {
	s.rng += 0x9e3779b97f4a7c15
	z := s.rng
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return (z ^ (z >> 31)) % n
}

func (s *sim) mainPID() int {
	switch {
	case s.t < simExit:
		return simPID1
	case s.t < simStart:
		return 0
	default:
		return simPID2
	}
}

// watchers are the participants who watch a share: everyone, except the only sharer when there is one share.
func (s *sim) watchers() uint64 {
	switch s.shares {
	case 0:
		return 0
	case 1:
		return u(s.participants - 1)
	default:
		return u(s.participants)
	}
}

// u converts one of the simulation's small counts; a negative one is 0.
func u(i int) uint64 {
	return uint64(max(i, 0)) //nolint:gosec // G115: never negative
}

// minutes counts the whole minutes of a duration; a negative one is 0.
func minutes(d time.Duration) uint64 {
	return uint64(max(d, 0) / time.Minute) //nolint:gosec // G115: never negative
}

// videoTracks are the video tracks that the viewers receive: every share, by everyone but its sharer.
func (s *sim) videoTracks() uint64 { return u(s.shares) * u(s.participants-1) }

func (s *sim) egressKbps() uint64  { return s.watchers() * (8300 + 300*u(s.shares-1)) }
func (s *sim) ingressKbps() uint64 { return u(s.shares) * 8400 }

// second advances the simulation by one second.
func (s *sim) second() {
	s.t += time.Second
	for _, p := range simTimeline {
		if s.t >= p.from {
			s.participants, s.shares, s.tcp443 = p.participants, p.shares, p.tcp443
		}
	}
	if s.t == simStart {
		s.proc = simCounters{}
	}
	eg, in, w := s.egressKbps(), s.ingressKbps(), s.watchers()
	s.cpuMilli = 600 + 220*(eg+in)/1000 + s.jitter(1600)
	if s.t >= simStart && s.t < mm(60, 40) {
		s.cpuMilli += 5000 // the new process starts up while everyone joins again
	}
	if s.mainPID() != 0 {
		s.proc.microTicks += s.cpuMilli * 2000 // 2 CPUs: 1000 milli-percent of all is 0.02 ticks per second
	}
	s.busyMicro += (s.cpuMilli + 1200 + s.jitter(600)) * 2000
	s.totalTicks += 200
	s.tx += eg*125*1035/1000 + 2500
	s.rx += in*125*1035/1000 + 75000
	s.udpOut += eg * 125 / 1180
	s.udpIn += in*125/1150 + 60
	s.udp6In += 2
	s.udp6Out += 2
	if s.t%(3*time.Second) == 0 {
		s.retrans++
	}
	if s.tcp443 > 0 && s.t%(2*time.Second) == 0 {
		s.retrans++
	}
	if s.mainPID() == 0 || s.t >= simTerm && s.t < simStart {
		return // no process counting media
	}
	c := &s.proc
	c.ingress += in * 125 * 97 / 100
	c.egress += eg * 125 * 97 / 100
	c.trIn += in*125 + 3000
	c.trOut += eg*125 + 9000
	c.nack += eg / 3000
	if s.t%(20*time.Second) == 0 {
		c.pli += u(s.shares)
	}
	if s.t%time.Minute == 0 {
		c.switches += u(s.participants)
	}
	c.decoded += w * (60 + 15*u(s.shares-1))
	if s.t%(10*time.Second) == 0 {
		c.dropped += w * 600 / 1250
	}
	c.freezeMs += s.videoTracks() * 2 // the viewers report freezes per inbound video track
	c.lost += eg / 8000
	c.audio += w * 48000
	c.concealed += w * 30
	// What happens once.
	switch s.t {
	case mm(10, 0):
		c.switches += 12 // everyone switches focus between A and B
	case mm(60, 16), mm(60, 24):
		c.notResumed += 2 // the clients' resume tokens belong to the old process
	case mm(60, 20), mm(87, 5):
		c.notResumed++
	case mm(60, 30):
		c.freezeMs += 5 * 1800 // the stats after the restart
	case mm(70, 2), mm(75, 12), mm(80, 3):
		c.resumed++ // C to mobile data, A's Wi-Fi back, C back to Wi-Fi
	case mm(70, 10):
		c.freezeMs += 2400
	case mm(80, 10):
		c.freezeMs += 1900
	case mm(87, 10):
		c.freezeMs += 1200
	case mm(92, 10):
		s.rcvbuf += 2
	case mm(95, 40):
		s.rcvbuf++
	}
}

// writeProc writes the proc tree for the current second.
func (s *sim) writeProc(h *fakeHost) {
	load := (s.cpuMilli + 1200) * 26 / 10000 // hundredths
	h.setHost(hostState{
		cpus: 2, busy: s.busyMicro / 1_000_000, idle: s.totalTicks - s.busyMicro/1_000_000,
		load: [3]float64{float64(load) / 100, float64(load*9/10) / 100, float64(load*8/10) / 100},
		rx:   s.rx, tx: s.tx, loRx: 52_000_000, loTx: 52_000_000, udpIn: s.udpIn, udpOut: s.udpOut,
		udp6In: s.udp6In, udp6Out: s.udp6Out, rcvbuf: s.rcvbuf, inErrors: s.rcvbuf, retrans: s.retrans,
	})
	pid := s.mainPID()
	if s.t >= simExit {
		h.removeProcess(simPID1)
	}
	if pid == 0 {
		return
	}
	p, sh := u(s.participants), u(s.shares)
	st := procState{pid: pid, ticks: s.proc.microTicks / 1_000_000, threads: 9 + 2*min(s.shares, 3) + s.participants/2,
		fds: 12 + 5*s.participants + 3*s.shares}
	if pid == simPID1 {
		st.start = 8_935_120
		st.rssKiB = 47104 + 920*p + 5600*sh + 41*minutes(s.t+15*time.Minute) + s.jitter(300)
	} else {
		st.start = 9_480_377
		st.rssKiB = 40960 + 920*p + 5600*sh + 31*minutes(s.t-simStart) + s.jitter(300)
	}
	h.setProcess(st)
}

// page is the metrics page for the current second.
func (s *sim) page() string {
	c := s.proc
	p, sh := u(s.participants), u(s.shares)
	tcp443 := u(s.tcp443)
	viewers := u(s.participants - 1)
	var b strings.Builder
	f := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	f("# HELP go_goroutines Number of goroutines that currently exist.\n# TYPE go_goroutines gauge")
	f("go_goroutines %d", 38+12*p+5*sh*p+s.jitter(4))
	f("process_cpu_seconds_total %d.%02d", c.microTicks/100_000_000, c.microTicks/1_000_000%100)
	f("process_resident_memory_bytes %d", (50_000+1_000*p+5_600*sh)*1024)
	f(`isshoni_build_info{commit="0000000",go="go1.27.2",version="0.1.0-rc.1"} 1`)
	f("isshoni_participants %d", p)
	f(`isshoni_shares{status="live"} %d`, sh)
	f(`isshoni_sfu_downtracks{forwarding="high",kind="audio"} %d`, sh*viewers)
	f(`isshoni_sfu_downtracks{forwarding="high",kind="video"} %d`, min(sh, 1)*viewers)
	f(`isshoni_sfu_downtracks{forwarding="low",kind="video"} %d`, (sh-min(sh, 1))*viewers)
	f(`isshoni_sfu_ingress_bytes_total{kind="audio"} %d`, c.ingress/84)
	f(`isshoni_sfu_ingress_bytes_total{kind="video"} %d`, c.ingress-c.ingress/84)
	f(`isshoni_sfu_egress_bytes_total{kind="audio"} %d`, c.egress/83)
	f(`isshoni_sfu_egress_bytes_total{kind="video"} %d`, c.egress-c.egress/83)
	f("isshoni_sfu_nack_served_total %d", c.nack)
	f("isshoni_sfu_pli_sent_total %d", c.pli)
	f("isshoni_sfu_layer_switches_total %d", c.switches)
	f(`isshoni_transfer_bytes_total{direction="egress",path="media_tcp"} %d`, c.trOut/20)
	f(`isshoni_transfer_bytes_total{direction="egress",path="media_udp"} %d`, c.trOut-c.trOut/20)
	f(`isshoni_transfer_bytes_total{direction="ingress",path="media_udp"} %d`, c.trIn)
	f(`isshoni_ws_resume_total{result="not_resumed"} %d`, c.notResumed)
	f(`isshoni_ws_resume_total{result="resumed"} %d`, c.resumed)
	f(`isshoni_sfu_selected_transport{transport="tcp443"} %d`, tcp443)
	f(`isshoni_sfu_selected_transport{transport="tcp7882"} 0`)
	f(`isshoni_sfu_selected_transport{transport="udp"} %d`, p+sh-tcp443)
	f("isshoni_client_frames_decoded_total %d", c.decoded)
	f("isshoni_client_frames_dropped_total %d", c.dropped)
	f("isshoni_client_freeze_seconds_total %d.%03d", c.freezeMs/1000, c.freezeMs%1000)
	f(`isshoni_client_packets_lost_total{kind="audio"} %d`, c.lost/10)
	f(`isshoni_client_packets_lost_total{kind="video"} %d`, c.lost-c.lost/10)
	f("isshoni_client_audio_samples_total %d", c.audio)
	f("isshoni_client_audio_concealed_samples_total %d", c.concealed)
	return b.String()
}

// recordFixture runs the simulated session and returns what the collector recorded.
func recordFixture(t *testing.T) []byte {
	t.Helper()
	h := newFakeHost(t)
	s := &sim{
		t: mm(-15, 0) - time.Second, rng: 94,
		// The machine has run since its boot, and the first process since the pre-flight.
		busyMicro: 4_120_000 * 1_000_000, totalTicks: 52_000_000, rx: 18_400_000_000, tx: 61_300_000_000,
		udpIn: 31_000_000, udpOut: 88_000_000, udp6In: 120_000, udp6Out: 95_000, rcvbuf: 4, retrans: 55_120,
		proc: simCounters{
			ingress: 32_000_000_000, egress: 151_000_000_000, trIn: 33_100_000_000, trOut: 156_000_000_000,
			nack: 412_000, pli: 3_100, switches: 9_870, resumed: 14, notResumed: 3, decoded: 1_240_000,
			dropped: 310, freezeMs: 41_500, lost: 2_210, audio: 960_000_000, concealed: 210_000,
			microTicks: 185_000 * 1_000_000,
		},
	}
	var mu sync.Mutex
	page, up := "", true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if !up {
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(page))
	}))
	defer srv.Close()

	dir := t.TempDir()
	out := filepath.Join(dir, "run.csv")
	c, err := newCollector(collectOptions{
		out: out, metricsURL: srv.URL + "/metrics", metricsDir: filepath.Join(dir, "run-metrics"), procDir: h.dir,
		interval: 10 * time.Second, scrapeInterval: 30 * time.Second,
		pid: func(context.Context) (int, error) { return s.mainPID(), nil },
	}, simT0.Add(mm(-15, 0)))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for s.t < mm(120, 0) {
		s.second()
		if s.t%(10*time.Second) != 0 {
			continue
		}
		s.writeProc(h)
		if err := c.sample(ctx, simT0.Add(s.t)); err != nil {
			t.Fatal(err)
		}
		if s.t%(30*time.Second) == 0 {
			mu.Lock()
			page, up = s.page(), s.mainPID() != 0 && (s.t < simTerm || s.t >= simMetricsUp)
			mu.Unlock()
			if err := c.scrape(ctx, simT0.Add(s.t+250*time.Millisecond)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := c.close(simT0.Add(mm(120, 4)), "stopped by a signal"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out) //nolint:gosec // G304: the test's own file
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestFixtureIsCurrent records the simulated session again: it must give testdata/run.csv byte for byte.
func TestFixtureIsCurrent(t *testing.T) {
	data := recordFixture(t)
	path := filepath.Join("testdata", "run.csv")
	if *update {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path) //nolint:gosec // G304: the test's own fixture
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, want) {
		gotLines, wantLines := strings.Split(string(data), "\n"), strings.Split(string(want), "\n")
		for i := range min(len(gotLines), len(wantLines)) {
			if gotLines[i] != wantLines[i] {
				t.Fatalf("%s is not what the collector records (run with -update); line %d:\n got %s\nwant %s", path,
					i+1, gotLines[i], wantLines[i])
			}
		}
		t.Fatalf("%s is not what the collector records (run with -update): %d lines, want %d", path,
			len(gotLines), len(wantLines))
	}
}
