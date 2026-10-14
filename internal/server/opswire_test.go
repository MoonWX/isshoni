package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/ops"
	"github.com/MoonWX/isshoni/internal/server/push"
	"github.com/MoonWX/isshoni/internal/server/sfu"
	"github.com/MoonWX/isshoni/internal/server/signal"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// The adapters of the ops data (README S85; 04 §6.6, §11.2, §11.4): the collector over the SFU's metrics, the live
// source of the dashboard over the hub and the SFU, and the small adapters over 03's store and the push service.

// sfuLabelValues are the label values of 02 §13's table, the metric contract of isshoni_sfu_*.
var sfuLabelValues = struct {
	roles, pcKinds, pcStates, transports, shareStates, mediaKinds, egressKinds, forwarding, reasons []string
}{
	roles:       []string{"full", "viewer", "publisher", "agent"},
	pcKinds:     []string{"pub", "sub"},
	pcStates:    []string{"new", "connecting", "connected", "disconnected", "failed", "closed"},
	transports:  []string{"udp", "tcp443", "tcp7882"},
	shareStates: []string{"pending", "live", "stalled"},
	mediaKinds:  []string{"video", "audio"},
	egressKinds: []string{"video", "audio", "rtx"},
	forwarding:  []string{"high", "low", "off"},
	reasons: []string{"bandwidth", "server_limit", "codec_mismatch", "decoder_unavailable", "decoder_failed",
		"no_preview_layer", "no_layer"},
}

// series2 is the key of a series with two labels, "name{a=x,b=y}", with the labels in the order a registry gathers
// them: by name.
func series2(name, l1, v1, l2, v2 string) string {
	if l2 < l1 {
		l1, v1, l2, v2 = l2, v2, l1, v1
	}
	return fmt.Sprintf("%s{%s=%s,%s=%s}", name, l1, v1, l2, v2)
}

// sfuSeriesNames are the series of 02 §13 with their label sets, each as "name{label=value,…}", in no order: what
// sfuCollector must emit for an SFU whose Metrics carry every value of the table.
func sfuSeriesNames() []string {
	v := sfuLabelValues
	var out []string
	one := func(name, label string, values []string) {
		for _, x := range values {
			out = append(out, fmt.Sprintf("%s{%s=%s}", name, label, x))
		}
	}
	two := func(name, l1 string, v1 []string, l2 string, v2 []string) {
		for _, x := range v1 {
			for _, y := range v2 {
				out = append(out, series2(name, l1, x, l2, y))
			}
		}
	}
	one("isshoni_sfu_conns", "role", v.roles)
	two("isshoni_sfu_peerconnections", "kind", v.pcKinds, "state", v.pcStates)
	one("isshoni_sfu_selected_transport", "transport", v.transports)
	one("isshoni_sfu_shares", "state", v.shareStates)
	two("isshoni_sfu_downtracks", "kind", v.mediaKinds, "forwarding", v.forwarding)
	one("isshoni_sfu_ingress_bytes_total", "kind", v.mediaKinds)
	one("isshoni_sfu_ingress_packets_total", "kind", v.mediaKinds)
	one("isshoni_sfu_egress_bytes_total", "kind", v.egressKinds)
	one("isshoni_sfu_egress_packets_total", "kind", v.egressKinds)
	one("isshoni_sfu_downgrades_total", "reason", v.reasons)
	one("isshoni_sfu_queue_drops_total", "kind", v.egressKinds)
	one("isshoni_sfu_handshake_timeouts_total", "kind", v.pcKinds)
	for _, name := range []string{
		"isshoni_sfu_ingress_duplicates_total", "isshoni_sfu_nack_received_total", "isshoni_sfu_nack_served_total",
		"isshoni_sfu_nack_missed_total", "isshoni_sfu_rtx_skipped_total", "isshoni_sfu_pli_sent_total",
		"isshoni_sfu_pli_throttled_total", "isshoni_sfu_keyframes_received_total", "isshoni_sfu_layer_switches_total",
		"isshoni_sfu_packet_cache_bytes",
	} {
		out = append(out, name+"{}")
	}
	sort.Strings(out)
	return out
}

// fakeSFUMetrics is an SFU whose Metrics carry every label value of 02 §13, each series with a value of its own
// (1, 2, 3, … in the order the fields are filled), so that a series emitted with another one's value shows.
type fakeSFUMetrics struct{ m sfu.Metrics }

func (f fakeSFUMetrics) Metrics() sfu.Metrics { return f.m }

func newFakeSFUMetrics() (fakeSFUMetrics, map[string]float64) {
	v := sfuLabelValues
	want := map[string]float64{}
	n := 0
	next := func(series string) float64 {
		n++
		want[series] = float64(n)
		return float64(n)
	}
	gauges := func(name, label string, values []string) map[string]int64 {
		m := map[string]int64{}
		for _, x := range values {
			m[x] = int64(next(fmt.Sprintf("%s{%s=%s}", name, label, x)))
		}
		return m
	}
	gauges2 := func(name, l1 string, v1 []string, l2 string, v2 []string) map[string]map[string]int64 {
		m := map[string]map[string]int64{}
		for _, x := range v1 {
			m[x] = map[string]int64{}
			for _, y := range v2 {
				m[x][y] = int64(next(series2(name, l1, x, l2, y)))
			}
		}
		return m
	}
	counters := func(name, label string, values []string) map[string]uint64 {
		m := map[string]uint64{}
		for _, x := range values {
			m[x] = uint64(next(fmt.Sprintf("%s{%s=%s}", name, label, x)))
		}
		return m
	}
	counter := func(name string) uint64 { return uint64(next(name + "{}")) }
	m := sfu.Metrics{
		Conns:             gauges("isshoni_sfu_conns", "role", v.roles),
		PeerConnections:   gauges2("isshoni_sfu_peerconnections", "kind", v.pcKinds, "state", v.pcStates),
		SelectedTransport: gauges("isshoni_sfu_selected_transport", "transport", v.transports),
		Shares:            gauges("isshoni_sfu_shares", "state", v.shareStates),
		DownTracks:        gauges2("isshoni_sfu_downtracks", "kind", v.mediaKinds, "forwarding", v.forwarding),
		IngressBytes:      counters("isshoni_sfu_ingress_bytes_total", "kind", v.mediaKinds),
		IngressPackets:    counters("isshoni_sfu_ingress_packets_total", "kind", v.mediaKinds),
		IngressDuplicates: counter("isshoni_sfu_ingress_duplicates_total"),
		EgressBytes:       counters("isshoni_sfu_egress_bytes_total", "kind", v.egressKinds),
		EgressPackets:     counters("isshoni_sfu_egress_packets_total", "kind", v.egressKinds),
		NACKReceived:      counter("isshoni_sfu_nack_received_total"),
		NACKServed:        counter("isshoni_sfu_nack_served_total"),
		NACKMissed:        counter("isshoni_sfu_nack_missed_total"),
		RTXSkipped:        counter("isshoni_sfu_rtx_skipped_total"),
		PLISent:           counter("isshoni_sfu_pli_sent_total"),
		PLIThrottled:      counter("isshoni_sfu_pli_throttled_total"),
		KeyframesReceived: counter("isshoni_sfu_keyframes_received_total"),
		LayerSwitches:     counter("isshoni_sfu_layer_switches_total"),
		Downgrades:        counters("isshoni_sfu_downgrades_total", "reason", v.reasons),
		QueueDrops:        counters("isshoni_sfu_queue_drops_total", "kind", v.egressKinds),
		HandshakeTimeouts: counters("isshoni_sfu_handshake_timeouts_total", "kind", v.pcKinds),
		PacketCacheBytes:  int64(next("isshoni_sfu_packet_cache_bytes{}")),
	}
	return fakeSFUMetrics{m: m}, want
}

// gatherSeries renders what a registry gathers as "name{label=value,…}" → value, and the type of each name.
func gatherSeries(t *testing.T, g prometheus.Gatherer) (map[string]float64, map[string]string) {
	t.Helper()
	families, err := g.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	values, types := map[string]float64{}, map[string]string{}
	for _, f := range families {
		types[f.GetName()] = f.GetType().String()
		for _, m := range f.GetMetric() {
			var labels []string
			for _, l := range m.GetLabel() {
				labels = append(labels, l.GetName()+"="+l.GetValue())
			}
			key := f.GetName() + "{" + strings.Join(labels, ",") + "}"
			switch {
			case m.GetCounter() != nil:
				values[key] = m.GetCounter().GetValue()
			case m.GetGauge() != nil:
				values[key] = m.GetGauge().GetValue()
			}
		}
	}
	return values, types
}

// TestSFUCollector: the collector over 02's SFU.Metrics() emits every series of 02 §13 with exactly the label values
// the SFU gives, each with its own value and type (gauges for the states, counters for the _total names), and no
// series or label of its own (04 §11.2). Every field of sfu.Metrics is covered: one that a later slice adds and the
// collector leaves out fails here.
func TestSFUCollector(t *testing.T) {
	fake, want := newFakeSFUMetrics()
	// Every field of the fake is set, so the values below cover every field of sfu.Metrics.
	rv := reflect.ValueOf(fake.m)
	for i := range rv.NumField() {
		if rv.Field(i).IsZero() {
			t.Errorf("sfu.Metrics.%s is not mapped to a series of 02 §13 here", rv.Type().Field(i).Name)
		}
	}

	reg := prometheus.NewPedanticRegistry() // also checks Describe against Collect
	if err := reg.Register(sfuCollector{sfu: fake}); err != nil {
		t.Fatal(err)
	}
	got, types := gatherSeries(t, reg)
	if !reflect.DeepEqual(got, want) {
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s = %v, want %v", k, got[k], v)
			}
		}
		for k := range got {
			if _, ok := want[k]; !ok {
				t.Errorf("%s is not a series of 02 §13", k)
			}
		}
	}
	if names := slices.Sorted(maps.Keys(got)); !slices.Equal(names, sfuSeriesNames()) {
		t.Errorf("series:\n got %v\nwant %v", names, sfuSeriesNames())
	}
	for name, typ := range types {
		wantType := "GAUGE"
		if strings.HasSuffix(name, "_total") {
			wantType = "COUNTER"
		}
		if typ != wantType {
			t.Errorf("%s is a %s, want a %s", name, typ, wantType)
		}
	}
	// One SFU.Metrics() per scrape.
	counting := &countingSFU{fakeSFUMetrics: fake}
	reg = prometheus.NewRegistry()
	reg.MustRegister(sfuCollector{sfu: counting})
	if _, err := reg.Gather(); err != nil || counting.calls != 1 {
		t.Errorf("a scrape called Metrics %d times (%v), want once", counting.calls, err)
	}
}

// TestSFUCollectorBadLabel: a series that can't be built (a label value that is not UTF-8) costs only that series,
// as an error of the scrape, and never panics in the registry's goroutine, where a panic would end the server
// (04 §11.2).
func TestSFUCollectorBadLabel(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(sfuCollector{sfu: fakeSFUMetrics{m: sfu.Metrics{
		Conns:            map[string]int64{"full": 2, "\xff": 1},
		PacketCacheBytes: 4096,
	}}})
	families, err := reg.Gather()
	if err == nil || !strings.Contains(err.Error(), "isshoni_sfu_conns") {
		t.Errorf("gather = %v, want the error of the bad isshoni_sfu_conns series", err)
	}
	got := map[string]float64{}
	for _, f := range families {
		for _, m := range f.GetMetric() {
			got[f.GetName()] += m.GetGauge().GetValue() + m.GetCounter().GetValue()
		}
	}
	if got["isshoni_sfu_conns"] != 2 || got["isshoni_sfu_packet_cache_bytes"] != 4096 {
		t.Errorf("the other series = %v, want isshoni_sfu_conns{role=full} 2 and the packet cache's 4096", got)
	}
}

type countingSFU struct {
	fakeSFUMetrics
	calls int
}

func (c *countingSFU) Metrics() sfu.Metrics {
	c.calls++
	return c.fakeSFUMetrics.Metrics()
}

// fakeSFUSnapshot is an SFU's Snapshot for the live source.
type fakeSFUSnapshot struct{ snap sfu.Snapshot }

func (f fakeSFUSnapshot) Snapshot() sfu.Snapshot { return f.snap }

// TestLiveSource: the dashboard's rooms join 01's hub and 02's SFU on the room and the connection id (04 §11.4):
// participants, their connections and what they watch from the hub; each connection's transport and RTT, and the
// shares with their layers, viewers and rates, from the SFU; the room's name from the store, which a rename
// reached before the hub. The media totals are the SFU's, with the PCs by transport.
func TestLiveSource(t *testing.T) {
	since := time.Date(2026, 9, 29, 19, 2, 11, 0, time.UTC)
	started := time.Date(2026, 9, 29, 19, 5, 0, 0, time.UTC)
	hub := &fakeHub{snapshot: signal.LiveSnapshot{Rooms: []signal.LiveRoom{
		{
			ID: "lounge", Name: "Lounge",
			Participants: []signal.LiveParticipant{
				{UserID: "u-alex", Name: "alex", Connections: []signal.LiveConnection{
					{ID: "c_alex", Kind: protocol.ClientKindWeb, Role: protocol.RoleFull, OS: protocol.ClientOSIOS,
						Version: "0.3.0", Since: since},
				}},
				{UserID: "u-sam", Name: "sam", Connections: []signal.LiveConnection{
					{ID: "c_sam1", Kind: protocol.ClientKindWeb, Role: protocol.RoleFull, OS: protocol.ClientOSWindows,
						Version: "0.3.0", Since: since.Add(time.Minute)},
					{ID: "c_sam2", Kind: protocol.ClientKindTool, Role: protocol.RoleViewer, OS: protocol.ClientOSLinux,
						Version: "0.1.0", Since: since.Add(2 * time.Minute)},
				}},
			},
			Shares: []signal.LiveShare{{Info: protocol.ShareInfo{
				ID: "s_1", UserID: "u-sam", Watchers: []protocol.Watcher{{UserID: "u-alex"}},
			}}},
		},
		{ID: "movies", Name: "Movies", Participants: []signal.LiveParticipant{
			{UserID: "u-kim", Name: "kim", Connections: []signal.LiveConnection{{ID: "c_kim", Kind: protocol.ClientKindWeb,
				Role: protocol.RoleViewer, Version: "0.3.0", Since: since}}},
		}},
	}}}
	media := fakeSFUSnapshot{snap: sfu.Snapshot{
		Rooms: []sfu.RoomSnapshot{{
			Room: "lounge",
			Shares: []sfu.ShareSnapshot{{
				Info: sfu.ShareInfo{
					ID: "s_1", Room: "lounge", User: "u-sam", Conn: "c_sam1", Source: sfu.SourceWindow, Profile: "640c",
					Layers: []sfu.LayerInfo{
						{RID: "f", Width: 1920, Height: 1080, FPS: 59.8, Bitrate: 7_900_000, LossPct: 0.1, Active: true},
						{RID: "q", Width: 640, Height: 360, FPS: 15, Bitrate: 290_000, Active: true},
					},
					StartedAt: started,
				},
				IngressBps: 8_400_000, EgressBps: 24_300_000,
				Viewers: sfu.ViewerCounts{High: 1, Audio: 1},
			}},
			Conns: []sfu.ConnSummary{
				{Conn: "c_alex", User: "u-alex", Transport: "udp", RTT: 38 * time.Millisecond},
				{Conn: "c_sam1", User: "u-sam", Transport: "tcp443", RTT: 61500 * time.Microsecond},
				{Conn: "c_sam2", User: "u-sam"}, // not connected yet
			},
		}},
		Totals: sfu.SnapshotTotals{
			IngressBps: 8_400_000, EgressBps: 24_300_000, DownTracks: 2,
			PCsByTransport: map[string]int{"udp": 2, "tcp443": 1, "tcp7882": 0},
		},
	}}
	live := liveSource{hub: hub, media: media, names: func(context.Context) (map[string]string, error) {
		return map[string]string{"lounge": "Living room", "movies": "Movies"}, nil
	}}

	want := []api.RoomLive{
		{
			ID: "lounge", Name: "Living room",
			Participants: []api.ParticipantLive{
				{UserID: "u-alex", Username: "alex", Watching: []string{"s_1"}, Connections: []api.ConnectionLive{
					{ID: "c_alex", Kind: "web", Role: "full", Version: "0.3.0", OS: "ios", Transport: api.TransportUDP,
						RTTMs: 38, ConnectedAt: since},
				}},
				{UserID: "u-sam", Username: "sam", Watching: []string{}, Connections: []api.ConnectionLive{
					{ID: "c_sam1", Kind: "web", Role: "full", Version: "0.3.0", OS: "windows",
						Transport: api.TransportTCP443, RTTMs: 61, ConnectedAt: since.Add(time.Minute)},
					{ID: "c_sam2", Kind: "tool", Role: "viewer", Version: "0.1.0", OS: "linux",
						ConnectedAt: since.Add(2 * time.Minute)},
				}},
			},
			Shares: []api.ShareLive{{
				ShareID: "s_1", OwnerUserID: "u-sam", Kind: "window", StartedAt: started, Codec: "h264/640c",
				Layers: []api.LayerLive{
					{RID: "f", Width: 1920, Height: 1080, FPS: 59.8, Bitrate: 7_900_000, LossPct: 0.1},
					{RID: "q", Width: 640, Height: 360, FPS: 15, Bitrate: 290_000},
				},
				Viewers:    api.ViewerCounts{High: 1, Audio: 1},
				IngressBps: 8_400_000, EgressBps: 24_300_000,
			}},
		},
		{
			ID: "movies", Name: "Movies", Shares: []api.ShareLive{},
			Participants: []api.ParticipantLive{{UserID: "u-kim", Username: "kim", Watching: []string{},
				Connections: []api.ConnectionLive{{ID: "c_kim", Kind: "web", Role: "viewer", Version: "0.3.0", ConnectedAt: since}}}},
		},
	}
	if got := live.Rooms(context.Background()); !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.MarshalIndent(got, "", " ")
		wantJSON, _ := json.MarshalIndent(want, "", " ")
		t.Errorf("rooms:\n got %s\nwant %s", gotJSON, wantJSON)
	}
	wantMedia := api.MediaTotals{IngressBps: 8_400_000, EgressBps: 24_300_000, DownTracks: 2,
		PeerConnections: api.TransportCounts{UDP: 2, TCP443: 1}}
	if got := live.Media(context.Background()); got != wantMedia {
		t.Errorf("media = %+v, want %+v", got, wantMedia)
	}

	// The store's names can't be read: the hub's stay. A share without a profile yet has no codec.
	live.names = func(context.Context) (map[string]string, error) { return nil, errors.New("database is locked") }
	media.snap.Rooms[0].Shares[0].Info.Profile = ""
	live.media = media
	got := live.Rooms(context.Background())
	if got[0].Name != "Lounge" || got[0].Shares[0].Codec != "" {
		t.Errorf("without the store's names and a profile: name %q, codec %q", got[0].Name, got[0].Shares[0].Codec)
	}

	// A server without media sockets has no SFU: the hub's rooms without shares or transports, and zero totals.
	live.media = nil
	got = live.Rooms(context.Background())
	if len(got) != 2 || len(got[0].Shares) != 0 || got[0].Participants[0].Connections[0].Transport != "" {
		t.Errorf("rooms without an SFU = %+v", got)
	}
	if m := live.Media(context.Background()); m != (api.MediaTotals{}) {
		t.Errorf("media without an SFU = %+v", m)
	}
	// An empty hub reads no names.
	live.names = func(context.Context) (map[string]string, error) {
		t.Error("the names were read for a hub without rooms")
		return nil, nil
	}
	if got := (liveSource{hub: &fakeHub{}, names: live.names}).Rooms(context.Background()); got == nil || len(got) != 0 {
		t.Errorf("rooms of an empty hub = %#v, want an empty list", got)
	}

	// A store that is slow to answer costs the names, not the section: the read has a budget of its own, well
	// inside the rooms source's second, so the dashboard shows the rooms with the hub's names, counts their
	// clients, and raises no dashboard.source_failed.
	synctest.Test(t, func(t *testing.T) {
		slow := liveSource{hub: hub, media: media, names: func(ctx context.Context) (map[string]string, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		d := ops.NewDashboard(ops.DashboardOptions{
			Status:  func(context.Context) (api.ServerStatus, error) { return api.ServerStatus{Version: "0.3.0"}, nil },
			Live:    slow,
			Process: func() api.ProcessInfo { return api.ProcessInfo{} },
			Logger:  discardLog(),
		})
		start := time.Now()
		snap, err := d.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if waited := time.Since(start); waited != roomNamesTimeout {
			t.Errorf("the snapshot waited %v for the names, want %v", waited, roomNamesTimeout)
		}
		if len(snap.Rooms) != 2 || snap.Rooms[0].Name != "Lounge" || snap.Rooms[1].Name != "Movies" ||
			len(snap.Rooms[0].Participants) != 2 || len(snap.Rooms[0].Shares) != 1 {
			t.Errorf("rooms behind a slow store = %+v, want both rooms whole with the hub's names", snap.Rooms)
		}
		if len(snap.Clients) == 0 {
			t.Error("no clients counted from the rooms behind a slow store")
		}
		for _, a := range snap.Alerts {
			if a.Code == api.AlertCodeDashboardSourceFailed {
				t.Errorf("a slow read of the names failed a source: %+v", a)
			}
		}
	})
}

// TestOpsAdapters: the ops adapters over 03's store and settings and over the push service.
func TestOpsAdapters(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	settings := db.Settings()

	// opsPolicy reads the settings at every call: an admin's change applies at the next tick.
	p := opsPolicy{settings: settings}
	if p.TransferAlertGB() != settings.Get().TransferAlertGB || p.ReleaseCheck() != settings.Get().UpdateCheck {
		t.Errorf("policy = %d %t, settings %+v", p.TransferAlertGB(), p.ReleaseCheck(), settings.Get())
	}
	patch := map[string]json.RawMessage{"transferAlertGb": json.RawMessage(`750`), "updateCheck": json.RawMessage(`false`)}
	if _, err := settings.Update(ctx, patch, store.CLIActor); err != nil {
		t.Fatal(err)
	}
	if p.TransferAlertGB() != 750 || p.ReleaseCheck() {
		t.Errorf("policy after the change = %d %t, want 750 false", p.TransferAlertGB(), p.ReleaseCheck())
	}

	// transferStore adds to a month's row and reads it; a month without a row is zero.
	ts := transferStore{db: db}
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	for range 2 {
		if err := ts.AddTransfer(ctx, "2026-10", 1000, 200, now); err != nil {
			t.Fatal(err)
		}
	}
	if e, i, err := ts.TransferMonth(ctx, "2026-10"); err != nil || e != 2000 || i != 400 {
		t.Errorf("October = %d %d, %v; want 2000 400", e, i, err)
	}
	if e, i, err := ts.TransferMonth(ctx, "2026-09"); err != nil || e != 0 || i != 0 {
		t.Errorf("September = %d %d, %v; want 0 0", e, i, err)
	}

	// opsAlerter queues ops's alert as push's, field by field.
	f := &fakePush{}
	at := now.Add(time.Minute)
	opsAlerter{svc: f}.AdminAlert(ctx, ops.AdminAlert{
		Kind: api.AdminAlertKindTransferThreshold, Actor: ops.AlertActorSystem, Target: "80", At: at,
	})
	if want := []push.AdminAlert{{Kind: "transfer_threshold", Actor: "system", Target: "80", At: at}}; !reflect.DeepEqual(f.alerts, want) {
		t.Errorf("alerts = %+v, want %+v", f.alerts, want)
	}

	// roomNames reads every room of the store.
	names, err := roomNames(db)(ctx)
	if err != nil || names[string(store.DefaultRoomID)] == "" {
		t.Errorf("room names = %v, %v", names, err)
	}
}

// TestReleaseURLProblem: an updates.release_url that the release check would refuse is a config error (exit 78),
// and the default and an http(s) URL are none. The problem, which serve prints into the journal, never shows the
// credentials it refuses (README §4 "Logging"), whether the value parses as a URL or not.
func TestReleaseURLProblem(t *testing.T) {
	for _, tc := range []struct {
		url, value string // value: the problem's, when bad
		bad        bool
	}{
		{"", "", false},
		{"https://api.github.com/repos/MoonWX/isshoni/releases", "", false},
		{"http://127.0.0.1:8080/releases", "", false},
		{"ftp://example.com/releases", `"ftp://example.com/releases"`, true},
		{"https://user:secret@example.com/releases", `"https://xxxxx@example.com/releases"`, true},
		{"https://secret@example.com/releases", `"https://xxxxx@example.com/releases"`, true}, // a token as the user
		{"https://user:secret@exa mple.com/releases", `"https://xxxxx@exa mple.com/releases"`, true},
		{"user:secret@example.com", `"xxxxx@example.com"`, true}, // the scheme user, and an opaque rest
		{"/releases", `"/releases"`, true},
	} {
		cfg := loadFlags(t, "--updates.release-url="+tc.url)
		p, bad := releaseURLProblem(cfg)
		if bad != tc.bad || (bad && (p.Key != "updates.release_url" || p.Severity != "error" || p.Value != tc.value)) {
			t.Errorf("%q: problem %+v, %t; want bad %t with the value %s", tc.url, p, bad, tc.bad, tc.value)
		}
		if strings.Contains(p.Value, "secret") {
			t.Errorf("%q: the problem shows the credentials: %s", tc.url, p.Value)
		}
		_, err := New(cfg, discardLog(), Deps{})
		if NeedsOperator(err) != tc.bad {
			t.Errorf("%q: New = %v, want a refusal %t", tc.url, err, tc.bad)
		}
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Errorf("%q: New's error shows the credentials: %v", tc.url, err)
		}
	}
}
