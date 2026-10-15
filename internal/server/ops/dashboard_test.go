package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/netx"
)

// The admin dashboard (04 §11.4; README S85) with fake sources: the golden document, a failing source of every
// kind, the cache, and the rules of its derived parts (the clients' outdated flag, public_ip.changed,
// tls.renewal_failing).

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// dashboardGolden is the golden file of TestDashboardGolden.
const dashboardGolden = "testdata/dashboard.json"

// dashNow is the fake clock of the dashboard tests: the moment of 04 §11.4's example.
var dashNow = time.Date(2026, 9, 29, 20, 14, 5, 0, time.UTC)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// fakeStatus is the admin socket's status of 04 §11.4's example server. calls counts the snapshots that asked.
type fakeStatus struct {
	calls atomic.Int32
	err   error
	block bool // wait for the context to end
	panic bool
}

func (f *fakeStatus) status(ctx context.Context) (api.ServerStatus, error) {
	f.calls.Add(1)
	switch {
	case f.panic:
		panic("status exploded")
	case f.block:
		<-ctx.Done()
		return api.ServerStatus{}, ctx.Err()
	case f.err != nil:
		return api.ServerStatus{}, f.err
	}
	return api.ServerStatus{
		Version:   "0.3.0",
		StartedAt: at("2026-09-29T18:00:00Z"),
		UptimeS:   8045,
		Origin:    "https://203.0.113.7",
		TLS: api.TLSInfo{
			Mode: api.TLSModeIP, Names: []string{"203.0.113.7"}, Ready: true,
			NotAfter: at("2026-10-05T10:00:00Z"), NextRenewal: at("2026-10-02T12:00:00Z"),
		},
		PublicIPv4: "203.0.113.7",
		NAT:        api.NATKindNone,
		Advertised: []api.AdvertisedAddr{
			{Proto: "udp", Addr: "203.0.113.7:7882", Via: api.TransportUDP},
			{Proto: "tcp", Addr: "203.0.113.7:443", Via: api.TransportTCP443},
		},
		Listeners: []api.ListenerInfo{{Key: "listen.https", Network: "tcp", Addr: "[::]:443"}},
		Rooms:     1, Participants: 2, Shares: 1, SchemaVersion: 7,
	}, nil
}

// fakeLive is the hub and the SFU of the example: Lounge with alex watching b8f2's window share.
type fakeLive struct {
	roomsPanic, mediaBlock bool
}

func (f *fakeLive) Rooms(context.Context) []api.RoomLive {
	if f.roomsPanic {
		panic("rooms exploded")
	}
	return []api.RoomLive{{
		ID: "lounge", Name: "Lounge",
		Participants: []api.ParticipantLive{
			{
				UserID: "k3m9p2qxw7ht", Username: "alex",
				Connections: []api.ConnectionLive{{
					ID: "c_k3v9q2m7xw4pa8d1", Kind: "web", Role: "full", Version: "0.3.0", OS: "ios",
					Transport: api.TransportUDP, RTTMs: 38, ConnectedAt: at("2026-09-29T19:02:11Z"),
				}},
				Watching: []string{"s_q7m2x9c4v8b1n5k3"},
			},
			{
				UserID: "b8f2n4r6t0vz", Username: "sam",
				Connections: []api.ConnectionLive{
					{ID: "c_b8a1", Kind: "web", Role: "full", Version: "0.2.9", OS: "windows", Transport: api.TransportTCP443,
						RTTMs: 61, ConnectedAt: at("2026-09-29T19:04:00Z")},
					{ID: "c_b8a2", Kind: "web", Role: "viewer", Version: "0.3.0", OS: "android",
						ConnectedAt: at("2026-09-29T19:05:30Z")},
				},
			},
		},
		Shares: []api.ShareLive{{
			ShareID: "s_q7m2x9c4v8b1n5k3", OwnerUserID: "b8f2n4r6t0vz", Kind: "window",
			StartedAt: at("2026-09-29T19:05:00Z"), Codec: "h264/640c",
			Layers: []api.LayerLive{
				{RID: "f", Width: 1920, Height: 1080, FPS: 59.8, Bitrate: 7900000, LossPct: 0.1},
				{RID: "q", Width: 640, Height: 360, FPS: 15, Bitrate: 290000},
			},
			Viewers:    api.ViewerCounts{High: 3, Low: 1, Audio: 3},
			IngressBps: 8400000, EgressBps: 24300000,
		}},
	}}
}

func (f *fakeLive) Media(ctx context.Context) api.MediaTotals {
	if f.mediaBlock {
		<-ctx.Done()
	}
	return api.MediaTotals{
		IngressBps: 16800000, EgressBps: 41200000, DownTracks: 18,
		PeerConnections: api.TransportCounts{UDP: 7, TCP443: 1},
	}
}

type fakeDashAccounts struct{ err error }

func (f fakeDashAccounts) Accounts(context.Context) (api.DashboardAccounts, error) {
	if f.err != nil {
		return api.DashboardAccounts{}, f.err
	}
	return api.DashboardAccounts{
		Users:            api.DashboardUserCounts{Active: 4, Pending: 1, Admins: 1},
		Invites:          api.DashboardInviteCounts{Active: 2},
		Sessions:         api.DashboardSessionCounts{Active: 5},
		RegistrationMode: api.RegistrationMode("invite"),
		SecurityEvents: []api.AuditEntry{{
			ID: 41, At: at("2026-09-29T08:00:00Z"), Action: "auth.login_failed", Outcome: api.AuditOutcomeDenied,
			Actor: api.AuditRef{Kind: "anonymous"}, Detail: map[string]any{"username": "alex"},
		}},
	}, nil
}

type fakeTransfer struct{}

func (fakeTransfer) Info() api.TransferInfo {
	return api.TransferInfo{
		Month: "2026-09", EgressBytes: 812000000000, IngressBytes: 98000000000, AlertGB: 1000,
		ProjectedEgressBytes: 838000000000, EgressBps: 41500000, IngressBps: 17000000,
	}
}

func (fakeTransfer) Alerts() []api.Alert {
	return []api.Alert{{Code: api.AlertCodeTransfer80, Severity: api.AlertSeverityWarn,
		Params: map[string]any{"month": "2026-09", "alertGb": 1000}}}
}

type fakeRelease struct{}

func (fakeRelease) Update() *api.UpdateInfo {
	return &api.UpdateInfo{Latest: "0.3.1", URL: "https://github.com/MoonWX/isshoni/releases/tag/v0.3.1", Security: true,
		CheckedAt: at("2026-09-29T09:12:00Z")}
}

func (fakeRelease) Alerts() []api.Alert {
	return []api.Alert{{Code: api.AlertCodeReleaseSecurityUpdate, Severity: api.AlertSeverityWarn,
		Params: map[string]any{"version": "0.3.1"}}}
}

type fakeDoctor struct{ panic bool }

func (f fakeDoctor) Summary() *api.DoctorSummary {
	if f.panic {
		panic("doctor exploded")
	}
	return &api.DoctorSummary{RanAt: at("2026-09-29T18:00:07Z"), OK: 12, Warn: 1}
}

func (fakeDoctor) Alerts() []api.Alert { return nil }

// fakeProcess is the process of the example.
func fakeProcess() api.ProcessInfo {
	return api.ProcessInfo{CPUSeconds: 1834.2, RSSBytes: 187000000, NumCPU: 2, Goroutines: 412}
}

// dashboardSources is a dashboard over the example's sources, every one of them working, on a clock that stands at
// dashNow until the test moves it.
type dashboardSources struct {
	status   *fakeStatus
	live     *fakeLive
	accounts fakeDashAccounts
	doctor   fakeDoctor
	now      atomic.Pointer[time.Time]
	logs     *logRecorder
}

func newDashboardSources() *dashboardSources {
	s := &dashboardSources{status: &fakeStatus{}, live: &fakeLive{}, logs: &logRecorder{}}
	now := dashNow
	s.now.Store(&now)
	return s
}

func (s *dashboardSources) advance(d time.Duration) {
	now := s.now.Load().Add(d)
	s.now.Store(&now)
}

func (s *dashboardSources) dashboard() *Dashboard {
	return NewDashboard(DashboardOptions{
		Status:   s.status.status,
		Live:     s.live,
		Accounts: s.accounts,
		Transfer: fakeTransfer{},
		Release:  fakeRelease{},
		Doctor:   s.doctor,
		PublicAddrs: func() (netx.PublicAddrs, bool) {
			return netx.PublicAddrs{V4: netip.MustParseAddr("203.0.113.99")}, true
		},
		MinClientVersion: func() string { return "0.2.0" },
		Version:          "0.3.0",
		Process:          fakeProcess,
		Now:              func() time.Time { return *s.now.Load() },
		Logger:           slog.New(s.logs),
	})
}

// encodeGolden is the encoding of a golden file, as internal/protocol/api's: two-space indent, no HTML escaping and
// a final newline.
func encodeGolden(t *testing.T, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	return buf.Bytes()
}

// TestDashboardGolden: with every source working, the dashboard is 04 §11.4's document, testdata/dashboard.json:
// the server object from the status with the process and the release check's update; the transfer, the rooms and
// the media as their sources give them; the clients counted from the rooms' connections (sam's 0.2.9 is older than
// the server's 0.3.0, so it is outdated); doctor's summary; the accounts; and the alerts in their order: transfer,
// release, the public IPv4 address that changed since the start.
func TestDashboardGolden(t *testing.T) {
	src := newDashboardSources()
	snap, err := src.dashboard().Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := encodeGolden(t, snap)
	if *update {
		if err := os.WriteFile(filepath.FromSlash(dashboardGolden), got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(filepath.FromSlash(dashboardGolden))
	if err != nil {
		t.Fatalf("%v (run go test -run TestDashboardGolden -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the dashboard:\n%s\nwant (%s):\n%s", got, dashboardGolden, want)
	}

	// The document decodes into the DTO with no field left over.
	dec := json.NewDecoder(bytes.NewReader(want))
	dec.DisallowUnknownFields()
	var back api.OpsDashboard
	if err := dec.Decode(&back); err != nil {
		t.Fatal(err)
	}
	if len(back.Alerts) != 3 || back.Accounts == nil || back.Doctor == nil || back.Server.Update == nil {
		t.Errorf("decoded: %+v", back)
	}
	if logs := src.logs.text(); logs != "" {
		t.Errorf("a dashboard whose sources all work logged:\n%s", logs)
	}
}

// TestDashboardSourceFailed: a source that fails, whether it returns an error, panics or does not answer within its
// second, never fails the response. Its section is empty (an empty list, zero totals or an absent object), the
// alert dashboard.source_failed names it, every other section is there, and the failure is logged once while it
// lasts.
func TestDashboardSourceFailed(t *testing.T) {
	for _, tc := range []struct {
		source string
		break_ func(*dashboardSources)
		empty  func(api.OpsDashboard) bool
	}{
		{DashboardSourceServer, func(s *dashboardSources) { s.status.err = errors.New("no status") },
			func(d api.OpsDashboard) bool {
				// What ops knows by itself stays: its own version and the process.
				return d.Server.Origin == "" && d.Server.StartedAt.IsZero() && d.Server.Version == "0.3.0" &&
					d.Server.Process.NumCPU == 2 && len(d.Server.Advertised) == 0
			}},
		{DashboardSourceServer, func(s *dashboardSources) { s.status.panic = true },
			func(d api.OpsDashboard) bool { return d.Server.Origin == "" }},
		{DashboardSourceServer, func(s *dashboardSources) { s.status.block = true },
			func(d api.OpsDashboard) bool { return d.Server.Origin == "" }},
		{DashboardSourceRooms, func(s *dashboardSources) { s.live.roomsPanic = true },
			func(d api.OpsDashboard) bool { return len(d.Rooms) == 0 && len(d.Clients) == 0 && d.Rooms != nil }},
		{DashboardSourceMedia, func(s *dashboardSources) { s.live.mediaBlock = true },
			func(d api.OpsDashboard) bool { return d.Media == (api.MediaTotals{}) }},
		{DashboardSourceAccounts, func(s *dashboardSources) { s.accounts.err = errors.New("database is locked") },
			func(d api.OpsDashboard) bool { return d.Accounts == nil }},
		{DashboardSourceDoctor, func(s *dashboardSources) { s.doctor.panic = true },
			func(d api.OpsDashboard) bool { return d.Doctor == nil }},
	} {
		t.Run(tc.source, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				src := newDashboardSources()
				tc.break_(src)
				d := src.dashboard()
				snap, err := d.Snapshot(context.Background())
				if err != nil {
					t.Fatalf("a failing %s source failed the response: %v", tc.source, err)
				}
				if !tc.empty(snap) {
					t.Errorf("the %s section is not empty: %+v", tc.source, snap)
				}
				wantAlert := api.Alert{Code: api.AlertCodeDashboardSourceFailed, Severity: api.AlertSeverityWarn,
					Params: map[string]any{"source": tc.source}}
				if len(snap.Alerts) == 0 || !reflect.DeepEqual(snap.Alerts[0], wantAlert) {
					t.Errorf("alerts = %+v, want %+v first", snap.Alerts, wantAlert)
				}
				for _, a := range snap.Alerts[1:] {
					if a.Code == api.AlertCodeDashboardSourceFailed {
						t.Errorf("another source failed too: %+v", a)
					}
				}
				// The other sections are all there.
				if tc.source != DashboardSourceServer && snap.Server.Origin != "https://203.0.113.7" {
					t.Errorf("the server section is missing: %+v", snap.Server)
				}
				if tc.source != DashboardSourceRooms && len(snap.Rooms) != 1 {
					t.Errorf("the rooms are missing: %+v", snap.Rooms)
				}
				if tc.source != DashboardSourceAccounts && snap.Accounts == nil {
					t.Error("the accounts are missing")
				}
				if snap.Transfer.Month != "2026-09" || snap.Server.Update == nil {
					t.Errorf("transfer %+v, update %+v", snap.Transfer, snap.Server.Update)
				}

				// One log line while the failure lasts; the next snapshot, past the cache, does not repeat it.
				src.advance(2 * dashboardCacheFor)
				if _, err := d.Snapshot(context.Background()); err != nil {
					t.Fatal(err)
				}
				if n := strings.Count(src.logs.text(), "dashboard: a source failed"); n != 1 {
					t.Errorf("%d log lines for one failure, want 1:\n%s", n, src.logs.text())
				}
				if !strings.Contains(src.logs.text(), "source="+tc.source) {
					t.Errorf("the log does not name the source %s:\n%s", tc.source, src.logs.text())
				}
			})
		})
	}
}

// TestDashboardCache: the sources are asked at most once a second, however many pages poll; a caller that went
// away gets its context's error, and the snapshot it cut short is nobody's.
func TestDashboardCache(t *testing.T) {
	src := newDashboardSources()
	d := src.dashboard()
	first, err := d.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	src.advance(dashboardCacheFor - time.Millisecond)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			again, err := d.Snapshot(context.Background())
			if err != nil || !again.GeneratedAt.Equal(first.GeneratedAt) {
				t.Errorf("a snapshot within the second: generated at %v, %v; want the cached one of %v",
					again.GeneratedAt, err, first.GeneratedAt)
			}
		})
	}
	wg.Wait()
	if n := src.status.calls.Load(); n != 1 {
		t.Errorf("the status was asked %d times within a second, want once", n)
	}

	src.advance(time.Millisecond)
	next, err := d.Snapshot(context.Background())
	if err != nil || !next.GeneratedAt.Equal(dashNow.Add(dashboardCacheFor)) || src.status.calls.Load() != 2 {
		t.Errorf("after a second: generated at %v, %v, status asked %d times", next.GeneratedAt, err, src.status.calls.Load())
	}

	// A clock that went back does not keep an old snapshot forever.
	src.advance(-time.Hour)
	if _, err := d.Snapshot(context.Background()); err != nil || src.status.calls.Load() != 3 {
		t.Errorf("after the clock went back: %v, status asked %d times", err, src.status.calls.Load())
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	src.advance(time.Hour)
	if _, err := d.Snapshot(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("a snapshot for a caller that went away: %v, want context.Canceled", err)
	}
	if logs := src.logs.text(); logs != "" {
		t.Errorf("a caller that went away is no failing source:\n%s", logs)
	}
}

// TestDashboardWithoutSources: a dashboard over the status alone (a server without media, accounts or the other
// parts) has empty sections and no alert: a part the server does not have is not a failure.
func TestDashboardWithoutSources(t *testing.T) {
	st := &fakeStatus{}
	d := NewDashboard(DashboardOptions{Status: st.status, Version: "0.3.0", Process: fakeProcess,
		Now: func() time.Time { return dashNow }})
	snap, err := d.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Alerts) != 0 || snap.Accounts != nil || snap.Doctor != nil || len(snap.Rooms) != 0 || snap.Server.Update != nil {
		t.Errorf("snapshot = %+v", snap)
	}
	body := string(encodeGolden(t, snap))
	for _, want := range []string{`"rooms": []`, `"clients": []`, `"alerts": []`} {
		if !strings.Contains(body, want) {
			t.Errorf("no %s in:\n%s", want, body)
		}
	}
}

// TestClientCounts: connections count per kind and version; a version below minClientVersion or below the
// server's own is outdated, and one that is not SemVer never is.
func TestClientCounts(t *testing.T) {
	conns := func(versions ...string) []api.ConnectionLive {
		var out []api.ConnectionLive
		for _, v := range versions {
			kind, ver, _ := strings.Cut(v, "/")
			out = append(out, api.ConnectionLive{Kind: kind, Version: ver})
		}
		return out
	}
	rooms := []api.RoomLive{
		{Participants: []api.ParticipantLive{{Connections: conns("web/0.3.0", "web/0.2.9")}, {Connections: conns("tool/0.1.0")}}},
		{Participants: []api.ParticipantLive{{Connections: conns("web/0.3.0", "ios/1.0.0", "web/dev")}}},
	}
	for _, tc := range []struct {
		server, minimum string
		want            []api.ClientVersionCount
	}{
		{"0.3.0", "", []api.ClientVersionCount{
			{Kind: "ios", Version: "1.0.0", Count: 1},
			{Kind: "tool", Version: "0.1.0", Count: 1, Outdated: true},
			{Kind: "web", Version: "0.2.9", Count: 1, Outdated: true},
			{Kind: "web", Version: "0.3.0", Count: 2},
			{Kind: "web", Version: "dev", Count: 1},
		}},
		{"0.0.0-dev+1a2b3c4d5e6f", "1.0.0", []api.ClientVersionCount{
			{Kind: "ios", Version: "1.0.0", Count: 1},
			{Kind: "tool", Version: "0.1.0", Count: 1, Outdated: true},
			{Kind: "web", Version: "0.2.9", Count: 1, Outdated: true},
			{Kind: "web", Version: "0.3.0", Count: 2, Outdated: true},
			{Kind: "web", Version: "dev", Count: 1},
		}},
		{"not-semver", "", []api.ClientVersionCount{
			{Kind: "ios", Version: "1.0.0", Count: 1},
			{Kind: "tool", Version: "0.1.0", Count: 1},
			{Kind: "web", Version: "0.2.9", Count: 1},
			{Kind: "web", Version: "0.3.0", Count: 2},
			{Kind: "web", Version: "dev", Count: 1},
		}},
	} {
		d := NewDashboard(DashboardOptions{Status: (&fakeStatus{}).status, Version: tc.server,
			MinClientVersion: func() string { return tc.minimum }})
		if got := d.clientCounts(rooms); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("server %s, minimum %q:\n got %+v\nwant %+v", tc.server, tc.minimum, got, tc.want)
		}
	}
}

// TestPublicIPAlerts: one public_ip.changed per family whose address is not the one the server started with; an
// address that is gone, or the same, is no change.
func TestPublicIPAlerts(t *testing.T) {
	status := api.ServerStatus{PublicIPv4: "203.0.113.7"}
	for _, tc := range []struct {
		name string
		now  netx.PublicAddrs
		want []api.Alert
	}{
		{"the same", netx.PublicAddrs{V4: netip.MustParseAddr("203.0.113.7")}, nil},
		{"gone", netx.PublicAddrs{}, nil},
		{"IPv4 changed, IPv6 new", netx.PublicAddrs{V4: netip.MustParseAddr("203.0.113.99"), V6: netip.MustParseAddr("2001:db8::7")},
			[]api.Alert{
				{Code: api.AlertCodePublicIPChanged, Severity: api.AlertSeverityWarn,
					Params: map[string]any{"family": "ipv4", "from": "203.0.113.7", "to": "203.0.113.99"}},
				{Code: api.AlertCodePublicIPChanged, Severity: api.AlertSeverityWarn,
					Params: map[string]any{"family": "ipv6", "to": "2001:db8::7"}},
			}},
	} {
		if got := publicIPAlerts(status, tc.now); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", tc.name, got, tc.want)
		}
	}
}

// TestTLSAlerts: tls.renewal_failing in the modes that renew by themselves, for a server that has had a
// certificate and failed to get the next one: a warning while it serves, an error once it has expired.
func TestTLSAlerts(t *testing.T) {
	notAfter := dashNow.Add(48 * time.Hour)
	failing := api.TLSInfo{Mode: api.TLSModeAuto, Ready: true, NotAfter: notAfter, LastErrorCode: "tls.rate_limited"}
	alert := func(severity api.AlertSeverity) []api.Alert {
		return []api.Alert{{Code: api.AlertCodeTLSRenewalFailing, Severity: severity,
			Params: map[string]any{"code": "tls.rate_limited", "notAfter": "2026-10-01T20:14:05.000Z"}}}
	}
	with := func(f func(*api.TLSInfo)) api.TLSInfo {
		t := failing
		f(&t)
		return t
	}
	for _, tc := range []struct {
		name string
		tls  api.TLSInfo
		now  time.Time
		want []api.Alert
	}{
		{"renewal failing", failing, dashNow, alert(api.AlertSeverityWarn)},
		{"ip mode", with(func(t *api.TLSInfo) { t.Mode = api.TLSModeIP }), dashNow, alert(api.AlertSeverityWarn)},
		{"expired", failing, notAfter, alert(api.AlertSeverityError)},
		{"not served", with(func(t *api.TLSInfo) { t.Ready = false }), dashNow, alert(api.AlertSeverityError)},
		{"no error", with(func(t *api.TLSInfo) { t.LastErrorCode = "" }), dashNow, nil},
		{"first order failing", with(func(t *api.TLSInfo) { t.NotAfter = time.Time{}; t.Ready = false }), dashNow, nil},
		{"manual mode", with(func(t *api.TLSInfo) { t.Mode = api.TLSModeManual }), dashNow, nil},
		{"off mode", with(func(t *api.TLSInfo) { t.Mode = api.TLSModeOff }), dashNow, nil},
	} {
		if got := tlsAlerts(tc.tls, tc.now); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", tc.name, got, tc.want)
		}
	}
}

// TestProcessReader: the process's own numbers, from the runtime and the process collector.
func TestProcessReader(t *testing.T) {
	info := newProcessReader().read()
	if info.NumCPU < 1 || info.Goroutines < 1 || info.CPUSeconds < 0 || info.RSSBytes < 0 {
		t.Errorf("process = %+v", info)
	}
}

// logRecorder is a slog.Handler that keeps the text of every record, with its attributes as key=value.
type logRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (l *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (l *logRecorder) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Level.String() + " " + r.Message)
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(" " + a.String())
		return true
	})
	l.mu.Lock()
	l.lines = append(l.lines, b.String())
	l.mu.Unlock()
	return nil
}

func (l *logRecorder) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &logRecorderView{l: l, attrs: attrs}
}

func (l *logRecorder) WithGroup(string) slog.Handler { return l }

func (l *logRecorder) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// logRecorderView is a logRecorder with attributes of its own (Logger.With), writing to the same lines.
type logRecorderView struct {
	l     *logRecorder
	attrs []slog.Attr
}

func (v *logRecorderView) Enabled(context.Context, slog.Level) bool { return true }

func (v *logRecorderView) Handle(ctx context.Context, r slog.Record) error {
	r = r.Clone()
	r.AddAttrs(v.attrs...)
	return v.l.Handle(ctx, r)
}

func (v *logRecorderView) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &logRecorderView{l: v.l, attrs: append(append([]slog.Attr(nil), v.attrs...), attrs...)}
}

func (v *logRecorderView) WithGroup(string) slog.Handler { return v }
