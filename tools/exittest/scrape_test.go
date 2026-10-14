package main

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// metricsPage is a /metrics page as client_golang writes it, with every series of 06 §12.3 and some others.
func metricsPage(participants, shares, tcp443 int, scale float64) string {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	p("# HELP go_goroutines Number of goroutines that currently exist.")
	p("# TYPE go_goroutines gauge")
	p("go_goroutines %d", 40+participants*14+shares*participants*6)
	p(`go_info{version="go1.27.2"} 1`)
	p("# HELP go_gc_duration_seconds A summary of the wall-time pause (stop-the-world) duration in garbage collection cycles.")
	p("# TYPE go_gc_duration_seconds summary")
	p(`go_gc_duration_seconds{quantile="0"} 2.6667e-05`)
	p(`go_gc_duration_seconds{quantile="1"} 0.000311458`)
	p("go_gc_duration_seconds_sum %g", 0.01*scale)
	p("go_gc_duration_seconds_count %d", int(12*scale))
	p("process_cpu_seconds_total %g", 12.5*scale)
	p("process_resident_memory_bytes %d", 50<<20)
	p("process_start_time_seconds 1.76e+09")
	p(`isshoni_build_info{commit="0123456",go="go1.27.2",version="0.1.0-rc.1"} 1`)
	p("isshoni_ready 1")
	p("isshoni_participants %d", participants)
	p("isshoni_rooms 1")
	p(`isshoni_shares{status="live"} %d`, shares)
	p(`isshoni_shares{status="starting"} 0`)
	p(`isshoni_sfu_downtracks{forwarding="high",kind="audio"} %d`, shares*(participants-1))
	p(`isshoni_sfu_downtracks{forwarding="high",kind="video"} %d`, shares*(participants-1))
	p(`isshoni_sfu_downtracks{forwarding="low",kind="video"} 0`)
	p(`isshoni_sfu_ingress_bytes_total{kind="audio"} %g`, 1e6*scale)
	p(`isshoni_sfu_ingress_bytes_total{kind="video"} %g`, 1e9*scale)
	p(`isshoni_sfu_egress_bytes_total{kind="audio"} %g`, 4e6*scale)
	p(`isshoni_sfu_egress_bytes_total{kind="video"} %g`, 4e9*scale)
	p("isshoni_sfu_nack_served_total %g", 300*scale)
	p("isshoni_sfu_nack_received_total %g", 310*scale)
	p("isshoni_sfu_pli_sent_total %g", 20*scale)
	p("isshoni_sfu_layer_switches_total %g", 30*scale)
	p(`isshoni_transfer_bytes_total{direction="egress",path="media_tcp"} %g`, 1e8*scale)
	p(`isshoni_transfer_bytes_total{direction="egress",path="media_udp"} %g`, 4.1e9*scale)
	p(`isshoni_transfer_bytes_total{direction="egress",path="web"} %g`, 1e6*scale)
	p(`isshoni_transfer_bytes_total{direction="ingress",path="media_udp"} %g`, 1.05e9*scale)
	p(`isshoni_ws_resume_total{result="resumed"} %g`, 2*scale)
	p(`isshoni_ws_resume_total{result="not_resumed"} %g`, 1*scale)
	p(`isshoni_sfu_selected_transport{transport="tcp443"} %d`, tcp443)
	p(`isshoni_sfu_selected_transport{transport="tcp7882"} 0`)
	p(`isshoni_sfu_selected_transport{transport="udp"} %d`, participants+shares-tcp443)
	p("isshoni_client_frames_decoded_total %g", 1e5*scale)
	p("isshoni_client_frames_dropped_total %g", 50*scale)
	p("isshoni_client_freeze_seconds_total %g", 1.5*scale)
	p(`isshoni_client_packets_lost_total{kind="audio"} %g`, 3*scale)
	p(`isshoni_client_packets_lost_total{kind="video"} %g`, 40*scale)
	p("isshoni_client_audio_samples_total %g", 4.8e7*scale)
	p("isshoni_client_audio_concealed_samples_total %g", 2400*scale)
	p(`isshoni_http_responses_total{class="2xx",route="GET /api/v1/info"} 7`)
	return b.String()
}

func TestParsePromText(t *testing.T) {
	samples, skipped, err := parsePromText([]byte(metricsPage(5, 2, 1, 1)))
	if skipped != 0 || err != nil {
		t.Fatalf("parsePromText skipped %d lines: %v", skipped, err)
	}
	vals := metricValues(samples)
	want := map[string]float64{
		"go_goroutines": 40 + 70 + 60, "process_resident_memory_bytes": 50 << 20, "process_cpu_seconds_total": 12.5,
		"participants": 5, "shares": 2, "sfu_downtracks": 16,
		"sfu_ingress_bytes_total": 1.001e9, "sfu_egress_bytes_total": 4.004e9, "sfu_nack_served_total": 300,
		"sfu_pli_sent_total": 20, "sfu_layer_switches_total": 30,
		"transfer_ingress_bytes_total": 1.05e9, "transfer_egress_bytes_total": 4.201e9,
		"ws_resume_resumed_total": 2, "ws_resume_not_resumed_total": 1,
		"transport_udp": 6, "transport_tcp443": 1, "transport_tcp7882": 0,
		"client_frames_decoded_total": 1e5, "client_frames_dropped_total": 50, "client_freeze_seconds_total": 1.5,
		"client_packets_lost_total": 43, "client_audio_samples_total": 4.8e7, "client_audio_concealed_samples_total": 2400,
	}
	if len(vals) != len(metricColumns) || len(want) != len(metricColumns) {
		t.Errorf("%d values for %d columns (want %d)", len(vals), len(metricColumns), len(want))
	}
	for col, w := range want {
		if got, ok := vals[col]; !ok || math.Abs(got-w) > 1e-6*math.Max(1, w) {
			t.Errorf("%s = %v (present %t), want %v", col, got, ok, w)
		}
	}
}

func TestParsePromLine(t *testing.T) {
	for _, tc := range []struct {
		line   string
		name   string
		labels []promLabel
		value  float64
	}{
		{"up 1", "up", nil, 1},
		{"up\t1 1760000000000", "up", nil, 1},
		{`a_b:c{x="1"} -2.5e3`, "a_b:c", []promLabel{{"x", "1"}}, -2500},
		{`m{ a = "q\"u\\o\nte" , b="}{,"  ,} +Inf`, "m", []promLabel{{"a", "q\"u\\o\nte"}, {"b", "}{,"}}, math.Inf(1)},
		{`m{} 0`, "m", []promLabel{}, 0},
	} {
		s, err := parsePromLine(tc.line)
		if err != nil {
			t.Errorf("parsePromLine(%q): %v", tc.line, err)
			continue
		}
		if s.name != tc.name || s.value != tc.value || fmt.Sprint(s.labels) != fmt.Sprint(tc.labels) {
			t.Errorf("parsePromLine(%q) = %+v, want %s %v %v", tc.line, s, tc.name, tc.labels, tc.value)
		}
	}
	nan, err := parsePromLine("m NaN")
	if err != nil || !math.IsNaN(nan.value) {
		t.Errorf("parsePromLine(m NaN) = %+v, %v", nan, err)
	}
	for _, bad := range []string{
		"1up 1", "up", "up1", "up 1 2 3", "up x", "up 1 1.5", `m{x} 1`, `m{x="1} 1`, `m{x=1} 1`, `m{x="1" y="2"} 1`,
		`m{="1"} 1`, `m{x="\q"} 1`, `m{x="1"`, "{x=\"1\"} 1",
	} {
		if s, err := parsePromLine(bad); err == nil {
			t.Errorf("parsePromLine(%q) = %+v, want an error", bad, s)
		}
	}
}

func TestParsePromTextSkipsBadLines(t *testing.T) {
	page := "# a comment\n\nisshoni_participants 3\r\nthis is not a series\nisshoni_shares{status=\"live\"} 1\n  go_goroutines 9\nbad{ 1\n"
	samples, skipped, err := parsePromText([]byte(page))
	if skipped != 2 || err == nil || !strings.HasPrefix(err.Error(), "line 4:") {
		t.Errorf("skipped %d, first error %v; want 2 lines skipped, the first on line 4", skipped, err)
	}
	vals := metricValues(samples)
	if vals["participants"] != 3 || vals["shares"] != 1 || vals["go_goroutines"] != 9 || len(vals) != 3 {
		t.Errorf("values %v, want participants 3, shares 1, go_goroutines 9 and nothing else", vals)
	}
	// A metric that isn't on the page gets no value at all, not a 0.
	if _, ok := vals["transport_tcp443"]; ok {
		t.Error("transport_tcp443 has a value without a series")
	}
	// NaN and infinite values are left out of the sums.
	samples, _, _ = parsePromText([]byte("isshoni_participants NaN\nisshoni_shares{status=\"live\"} +Inf\nisshoni_shares{status=\"x\"} 2\n"))
	vals = metricValues(samples)
	if _, ok := vals["participants"]; ok || vals["shares"] != 2 {
		t.Errorf("values %v, want no participants and shares 2", vals)
	}
}

func FuzzParsePromText(f *testing.F) {
	f.Add([]byte(metricsPage(5, 2, 1, 1)))
	f.Add([]byte(`m{a="\\\"",b="x"} 1 2` + "\n# x\n{"))
	f.Add([]byte("isshoni_participants 3\nm{x=\"\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		samples, skipped, err := parsePromText(data)
		if (skipped == 0) != (err == nil) {
			t.Fatalf("skipped %d lines with first error %v", skipped, err)
		}
		vals := metricValues(samples)
		for col, v := range vals {
			if math.IsNaN(v) {
				t.Fatalf("%s is NaN", col)
			}
		}
		lines := strings.Count(string(data), "\n") + 1
		if len(samples)+skipped > lines {
			t.Fatalf("%d samples and %d skipped from %d lines", len(samples), skipped, lines)
		}
	})
}

func TestFetchMetrics(t *testing.T) {
	page := metricsPage(1, 0, 0, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metrics":
			if got := r.Header.Get("Accept"); got != "text/plain;version=0.0.4" {
				http.Error(w, "accept "+got, http.StatusNotAcceptable)
				return
			}
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8; escaping=underscores")
			_, _ = w.Write([]byte(page))
		case "/openmetrics":
			w.Header().Set("Content-Type", "application/openmetrics-text; version=1.0.0; charset=utf-8")
			_, _ = w.Write([]byte(page + "# EOF\n"))
		case "/proto":
			w.Header().Set("Content-Type", "application/vnd.google.protobuf; proto=io.prometheus.client.MetricFamily")
			_, _ = w.Write([]byte{1, 2, 3})
		case "/big":
			w.Header().Set("Content-Type", "text/plain")
			chunk := []byte(strings.Repeat("#", 1<<20) + "\n")
			for range maxScrapeBytes>>20 + 1 {
				if _, err := w.Write(chunk); err != nil {
					return
				}
			}
		default:
			http.Error(w, "starting", http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	body, err := fetchMetrics(ctx, client, srv.URL+"/metrics")
	if err != nil || string(body) != page {
		t.Errorf("fetchMetrics = %d bytes, %v; want the page", len(body), err)
	}
	if _, err := fetchMetrics(ctx, client, srv.URL+"/openmetrics"); err != nil {
		t.Errorf("OpenMetrics text: %v", err)
	}
	for path, want := range map[string]string{
		"/unavailable": "status 503 Service Unavailable",
		"/proto":       "is not the text format",
		"/big":         "larger than",
	} {
		if _, err := fetchMetrics(ctx, client, srv.URL+path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("fetchMetrics(%s) = %v, want an error with %q", path, err, want)
		}
	}
}
