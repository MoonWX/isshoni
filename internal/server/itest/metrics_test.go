package itest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/servertest"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
)

// What the metrics endpoint and the admin dashboard say about a session with media (README S85; 04 §11.2,
// §11.4): the series that the exit test reads (06 §12.3), and the selected transport of each PeerConnection.

// withMetrics turns the metrics listener on, on a port the kernel picks.
func withMetrics(c *config.Config) {
	c.Metrics.Enabled = true
	c.Metrics.Listen = "127.0.0.1:0"
}

// scrape reads GET /metrics from the server's metrics listener: every sample as `name{labels}` → value, the
// labels as the text format writes them.
func scrape(t *testing.T, w *world) map[string]float64 {
	t.Helper()
	addr := w.srv.Srv.Addrs().Metrics
	if addr == nil {
		t.Fatal("the server has no metrics listener")
	}
	req, err := http.NewRequestWithContext(waitCtx(t), http.MethodGet, "http://"+addr.String()+"/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("scrape: %d %s", res.StatusCode, b)
	}
	out := map[string]float64{}
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		if i < 0 {
			t.Fatalf("scrape: a line that is no sample: %q", line)
		}
		v, err := strconv.ParseFloat(line[i+1:], 64)
		if err != nil {
			t.Fatalf("scrape: a line that is no sample: %q", line)
		}
		out[line[:i]] = v
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scrape: %v", err)
	}
	return out
}

// series returns the samples of one metric name.
func series(samples map[string]float64, name string) map[string]float64 {
	out := map[string]float64{}
	for k, v := range samples {
		if k == name || strings.HasPrefix(k, name+"{") {
			out[k] = v
		}
	}
	return out
}

// dashboard is the admin's GET /api/v1/admin/dashboard: newWorld's harness client is the admin's browser.
func dashboard(t *testing.T, w *world) api.OpsDashboard {
	t.Helper()
	req, err := http.NewRequestWithContext(waitCtx(t), http.MethodGet, w.srv.URL+"/api/v1/admin/dashboard", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := w.srv.Client.Do(req)
	if err != nil {
		t.Fatalf("GET /admin/dashboard: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/dashboard = %d %s, %v", res.StatusCode, body, err)
	}
	var d api.OpsDashboard
	if err := json.Unmarshal(body, &d); err != nil {
		t.Fatalf("GET /admin/dashboard: %v: %s", err, body)
	}
	return d
}

// TestMetricsSeries: after a session with everything the exit test measures (06 §12.3), the metrics endpoint has
// each of its series: the Go and process collectors; the hub's participants and shares; the SFU's DownTracks, its
// ingress and egress, NACKs, PLIs and layer switches; the bytes the sockets counted; the signaling resumes; the
// selected transports; and the viewers' own numbers from their stats reports. Those that something happened to
// have counted it.
func TestMetricsSeries(t *testing.T) {
	w := newWorld(t, servertest.Options{Config: withMetrics})
	alice := w.connect(w.member("alice"), protocol.RoleFull)
	sh := alice.publish(11, sfutest.LoopbackSettings())
	alice.waitLive(sh.id)
	bob := w.connect(w.member("bob"), protocol.RoleViewer)
	viewer := bob.watch(sfutest.LoopbackSettings(), 0)
	bob.waitLive(sh.id)
	bob.subscribe(want(sh.id, true))
	video, audio := track(t, viewer, sh.id, webrtc.RTPCodecTypeVideo), track(t, viewer, sh.id, webrtc.RTPCodecTypeAudio)
	waitFrames(t, video, 0, "f", 30)
	waitPackets(t, audio, 50)
	bob.waitStatus(want(sh.id, true))

	// The viewer's stats report, as the web client sends one every few seconds (01 §8.11).
	if err := bob.send(protocol.MessageTypeStats, protocol.ClientStats{
		IntervalMs: 2000,
		PCs:        []protocol.PCStats{{PC: protocol.PCKindSub, Gen: 1, State: "connected", RTTMs: 1, Transport: "udp"}},
		Inbound: []protocol.InboundStats{
			{ShareID: sh.id, Kind: protocol.TrackKindVideo, Bitrate: 400_000, PacketsLost: 3, FramesDecoded: 120,
				FramesDropped: 2, FreezeDurationMs: 250},
			{ShareID: sh.id, Kind: protocol.TrackKindAudio, Bitrate: 32_000, PacketsLost: 1, TotalSamples: 96_000,
				ConcealedSamples: 480},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// The report has arrived before the socket goes: a notification still queued in the client when its socket dies
	// is lost, as in a browser.
	var samples map[string]float64
	eventually(t, func() bool {
		samples = scrape(t, w)
		return samples[`isshoni_client_frames_decoded_total`] == 120
	}, func() string {
		return fmt.Sprintf("the stats report is not in the scrape: %v", series(samples, "isshoni_client_frames_decoded_total"))
	})
	// The viewer's socket dies and it resumes (01 §10.2): isshoni_ws_resume_total{result="resumed"}.
	bob.killSocket()
	if resumed := bob.waitWelcomes(1); !resumed[0] {
		t.Fatalf("bob's welcome after the cut did not resume (states %+v)", bob.stateLog())
	}
	eventually(t, func() bool {
		samples = scrape(t, w)
		return samples[`isshoni_ws_resume_total{result="resumed"}`] >= 1
	}, func() string {
		return fmt.Sprintf("the resume is not in the scrape: %v", series(samples, "isshoni_ws_resume_total"))
	})

	// Every series of 06 §12.3.
	names := []string{
		"go_goroutines", "isshoni_participants", "isshoni_shares", "isshoni_sfu_downtracks",
		"isshoni_sfu_ingress_bytes_total", "isshoni_sfu_egress_bytes_total", "isshoni_sfu_nack_served_total",
		"isshoni_sfu_pli_sent_total", "isshoni_sfu_layer_switches_total", "isshoni_transfer_bytes_total",
		"isshoni_ws_resume_total", "isshoni_sfu_selected_transport", "isshoni_client_frames_decoded_total",
		"isshoni_client_frames_dropped_total", "isshoni_client_freeze_seconds_total",
		"isshoni_client_packets_lost_total", "isshoni_client_audio_samples_total",
		"isshoni_client_audio_concealed_samples_total",
	}
	if runtime.GOOS == "linux" { // the process collector's platform; elsewhere it may have none
		names = append(names, "process_resident_memory_bytes", "process_cpu_seconds_total")
	}
	for _, name := range names {
		if len(series(samples, name)) == 0 {
			t.Errorf("no %s in the scrape", name)
		}
	}
	for key, want := range map[string]float64{
		`isshoni_participants`:                                2,
		`isshoni_shares{status="live"}`:                       1,
		`isshoni_client_frames_decoded_total`:                 120,
		`isshoni_client_frames_dropped_total`:                 2,
		`isshoni_client_freeze_seconds_total`:                 0.25,
		`isshoni_client_packets_lost_total{kind="video"}`:     3,
		`isshoni_client_packets_lost_total{kind="audio"}`:     1,
		`isshoni_client_audio_samples_total`:                  96_000,
		`isshoni_client_audio_concealed_samples_total`:        480,
		`isshoni_sfu_selected_transport{transport="tcp443"}`:  0,
		`isshoni_sfu_selected_transport{transport="tcp7882"}`: 0,
	} {
		if got, ok := samples[key]; !ok || got != want {
			t.Errorf("%s = %v (present: %t), want %v", key, got, ok, want)
		}
	}
	// What flowed was counted: the media on the SFU's tracks and at the UDP sockets, both PCs over UDP, and the
	// DownTracks that forward it.
	for _, key := range []string{
		`isshoni_sfu_ingress_bytes_total{kind="video"}`, `isshoni_sfu_ingress_bytes_total{kind="audio"}`,
		`isshoni_sfu_egress_bytes_total{kind="video"}`, `isshoni_sfu_egress_bytes_total{kind="audio"}`,
		`isshoni_transfer_bytes_total{direction="ingress",path="media_udp"}`,
		`isshoni_transfer_bytes_total{direction="egress",path="media_udp"}`,
		`isshoni_sfu_downtracks{forwarding="high",kind="video"}`, `isshoni_sfu_downtracks{forwarding="high",kind="audio"}`,
	} {
		if samples[key] <= 0 {
			t.Errorf("%s = %v, want what flowed", key, samples[key])
		}
	}
	if got := samples[`isshoni_sfu_selected_transport{transport="udp"}`]; got != 2 {
		t.Errorf(`isshoni_sfu_selected_transport{transport="udp"} = %v, want both PeerConnections: %v`, got,
			series(samples, "isshoni_sfu_selected_transport"))
	}
	alice.check()
	bob.check()
}

// wantTransport checks that the dashboard and the metrics agree on how the session's two PeerConnections reach the
// server: both on transport (04 §11.4, 02 §13), counted by the SFU's selected pairs and shown on the connections.
func wantTransport(t *testing.T, w *world, transport api.Transport, conns ...*client) {
	t.Helper()
	samples := scrape(t, w)
	for _, tr := range []api.Transport{api.TransportUDP, api.TransportTCP443, api.TransportTCP7882} {
		key := fmt.Sprintf(`isshoni_sfu_selected_transport{transport=%q}`, tr)
		want := 0.0
		if tr == transport {
			want = 2 // the publisher's pub PC and the viewer's sub PC
		}
		if got, ok := samples[key]; !ok || got != want {
			t.Errorf("%s = %v (present: %t), want %v", key, got, ok, want)
		}
	}

	d := dashboard(t, w)
	want := api.TransportCounts{}
	switch transport {
	case api.TransportUDP:
		want.UDP = 2
	case api.TransportTCP443:
		want.TCP443 = 2
	case api.TransportTCP7882:
		want.TCP7882 = 2
	}
	if d.Media.PeerConnections != want {
		t.Errorf("the dashboard's peerConnections = %+v, want %+v", d.Media.PeerConnections, want)
	}
	byID := map[string]api.ConnectionLive{}
	for _, r := range d.Rooms {
		for _, p := range r.Participants {
			for _, c := range p.Connections {
				byID[c.ID] = c
			}
		}
	}
	for _, c := range conns {
		if got := byID[c.connID()]; got.Transport != transport {
			t.Errorf("the dashboard shows %s's connection with transport %q, want %q", c.name, got.Transport, transport)
		}
	}
	for _, a := range d.Alerts {
		if a.Code == api.AlertCodeDashboardSourceFailed {
			t.Errorf("a dashboard source failed: %+v", a)
		}
	}
}
