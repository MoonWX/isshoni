package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"
)

// metricColumn is one scrape column of run.csv: the sum of every series of one metric, or of the series whose
// labels have the given values. The metrics are 06 §12.3's list (names from 01 §18, 02 §13 and 04 §11.2).
type metricColumn struct {
	col    string
	metric string
	match  []promLabel // the labels a series must have; none sums every series
}

// matches reports whether the series is one of the column's.
func (m *metricColumn) matches(s *promSample) bool {
	if s.name != m.metric {
		return false
	}
	for _, l := range m.match {
		if v, ok := s.label(l.name); !ok || v != l.value {
			return false
		}
	}
	return true
}

// with is the label match of a column: with("kind", "video", "forwarding", "high").
func with(nameValues ...string) []promLabel {
	ls := make([]promLabel, 0, len(nameValues)/2)
	for i := 0; i+1 < len(nameValues); i += 2 {
		ls = append(ls, promLabel{name: nameValues[i], value: nameValues[i+1]})
	}
	return ls
}

var metricColumns = []metricColumn{
	// The Go and process collectors.
	{col: "go_goroutines", metric: "go_goroutines"},
	{col: "process_resident_memory_bytes", metric: "process_resident_memory_bytes"},
	{col: "process_cpu_seconds_total", metric: "process_cpu_seconds_total"},
	// Gauges.
	{col: "participants", metric: "isshoni_participants"},
	{col: "shares", metric: "isshoni_shares"},
	{col: "sfu_downtracks", metric: "isshoni_sfu_downtracks"},
	// The video DownTracks that forward a layer (02 §13: forwarding high or low, not off): one per video track that
	// a viewer receives, the focus and the thumbnails. Their time is the watch time that the viewers' freezes,
	// which they report per inbound video track, are a share of.
	{col: "sfu_video_downtracks_high", metric: "isshoni_sfu_downtracks",
		match: with("kind", "video", "forwarding", "high")},
	{col: "sfu_video_downtracks_low", metric: "isshoni_sfu_downtracks",
		match: with("kind", "video", "forwarding", "low")},
	// Counters.
	{col: "sfu_ingress_bytes_total", metric: "isshoni_sfu_ingress_bytes_total"},
	{col: "sfu_egress_bytes_total", metric: "isshoni_sfu_egress_bytes_total"},
	{col: "sfu_nack_served_total", metric: "isshoni_sfu_nack_served_total"},
	{col: "sfu_pli_sent_total", metric: "isshoni_sfu_pli_sent_total"},
	{col: "sfu_layer_switches_total", metric: "isshoni_sfu_layer_switches_total"},
	{col: "transfer_ingress_bytes_total", metric: "isshoni_transfer_bytes_total", match: with("direction", "ingress")},
	{col: "transfer_egress_bytes_total", metric: "isshoni_transfer_bytes_total", match: with("direction", "egress")},
	// Signaling reconnects and resumes.
	{col: "ws_resume_resumed_total", metric: "isshoni_ws_resume_total", match: with("result", "resumed")},
	{col: "ws_resume_not_resumed_total", metric: "isshoni_ws_resume_total", match: with("result", "not_resumed")},
	// Connected PeerConnections by the transport of their selected candidate pair.
	{col: "transport_udp", metric: "isshoni_sfu_selected_transport", match: with("transport", "udp")},
	{col: "transport_tcp443", metric: "isshoni_sfu_selected_transport", match: with("transport", "tcp443")},
	{col: "transport_tcp7882", metric: "isshoni_sfu_selected_transport", match: with("transport", "tcp7882")},
	// The viewers' own numbers, from their stats reports (01 §8.11).
	{col: "client_frames_decoded_total", metric: "isshoni_client_frames_decoded_total"},
	{col: "client_frames_dropped_total", metric: "isshoni_client_frames_dropped_total"},
	{col: "client_freeze_seconds_total", metric: "isshoni_client_freeze_seconds_total"},
	{col: "client_packets_lost_total", metric: "isshoni_client_packets_lost_total"},
	{col: "client_audio_samples_total", metric: "isshoni_client_audio_samples_total"},
	{col: "client_audio_concealed_samples_total", metric: "isshoni_client_audio_concealed_samples_total"},
}

// promLabel and promSample are one series of the Prometheus text format.
type promLabel struct{ name, value string }

type promSample struct {
	name   string
	labels []promLabel
	value  float64
}

func (s *promSample) label(name string) (string, bool) {
	for _, l := range s.labels {
		if l.name == name {
			return l.value, true
		}
	}
	return "", false
}

// parsePromText reads the Prometheus text exposition format (version 0.0.4). It skips comments, and a line it
// cannot read is counted and skipped rather than failing the scrape: one odd series must not cost the others.
func parsePromText(data []byte) (samples []promSample, skipped int, firstErr error) {
	for n, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		line = strings.TrimLeft(line, " \t")
		if line == "" || line[0] == '#' {
			continue
		}
		s, err := parsePromLine(line)
		if err != nil {
			skipped++
			if firstErr == nil {
				firstErr = fmt.Errorf("line %d: %w", n+1, err)
			}
			continue
		}
		samples = append(samples, s)
	}
	return samples, skipped, firstErr
}

// parsePromLine reads `name{label="value",...} value [timestamp]`.
func parsePromLine(line string) (promSample, error) {
	i := 0
	for i < len(line) && isMetricNameByte(line[i], i == 0) {
		i++
	}
	if i == 0 {
		return promSample{}, errors.New("no metric name")
	}
	s := promSample{name: line[:i]}
	rest := line[i:]
	if strings.HasPrefix(rest, "{") {
		labels, after, err := parsePromLabels(rest[1:])
		if err != nil {
			return promSample{}, err
		}
		s.labels, rest = labels, after
	}
	trimmed := strings.TrimLeft(rest, " \t")
	if len(trimmed) == len(rest) && s.labels == nil {
		return promSample{}, errors.New("no blank between the name and the value")
	}
	f := strings.Fields(trimmed)
	if len(f) == 0 || len(f) > 2 {
		return promSample{}, fmt.Errorf("%d fields after the series, want a value and an optional timestamp", len(f))
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return promSample{}, fmt.Errorf("value %q", f[0])
	}
	if len(f) == 2 {
		if _, err := strconv.ParseInt(f[1], 10, 64); err != nil {
			return promSample{}, fmt.Errorf("timestamp %q", f[1])
		}
	}
	s.value = v
	return s, nil
}

func isMetricNameByte(b byte, first bool) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b == '_' || b == ':' || !first && b >= '0' && b <= '9'
}

// parsePromLabels reads the labels after the opening brace, up to and including the closing one, and returns
// what follows it. A label value may hold \\, \" and \n escapes.
func parsePromLabels(s string) ([]promLabel, string, error) {
	labels := []promLabel{}
	for {
		s = strings.TrimLeft(s, " \t")
		if strings.HasPrefix(s, "}") {
			return labels, s[1:], nil
		}
		i := 0
		for i < len(s) && isMetricNameByte(s[i], i == 0) && s[i] != ':' {
			i++
		}
		if i == 0 {
			return nil, "", errors.New("a label without a name")
		}
		name := s[:i]
		s = strings.TrimLeft(s[i:], " \t")
		if !strings.HasPrefix(s, "=") {
			return nil, "", fmt.Errorf("label %s: no =", name)
		}
		s = strings.TrimLeft(s[1:], " \t")
		if !strings.HasPrefix(s, `"`) {
			return nil, "", fmt.Errorf("label %s: the value has no opening quote", name)
		}
		var b strings.Builder
		j, closed := 1, false
		for j < len(s) && !closed {
			switch c := s[j]; {
			case c == '"':
				closed = true
			case c == '\\' && j+1 < len(s):
				j++
				switch s[j] {
				case 'n':
					b.WriteByte('\n')
				case '\\', '"':
					b.WriteByte(s[j])
				default:
					return nil, "", fmt.Errorf("label %s: escape \\%c", name, s[j])
				}
			default:
				b.WriteByte(c)
			}
			j++
		}
		if !closed {
			return nil, "", fmt.Errorf("label %s: the value has no closing quote", name)
		}
		labels = append(labels, promLabel{name: name, value: b.String()})
		s = strings.TrimLeft(s[j:], " \t")
		switch {
		case strings.HasPrefix(s, ","):
			s = s[1:]
		case strings.HasPrefix(s, "}"):
		default:
			return nil, "", fmt.Errorf("label %s: no , or } after the value", name)
		}
	}
}

// metricValues sums the scrape into the metric columns. A column whose metric is not in the scrape gets no value
// (an empty cell), never a 0 that would read as a measurement. NaN and infinite values are left out.
func metricValues(samples []promSample) map[string]float64 {
	vals := make(map[string]float64)
	for i := range metricColumns {
		m := &metricColumns[i]
		for j := range samples {
			s := &samples[j]
			if !m.matches(s) || math.IsNaN(s.value) || math.IsInf(s.value, 0) {
				continue
			}
			vals[m.col] += s.value
		}
	}
	return vals
}

// maxScrapeBytes bounds the body of one scrape: isshoni's /metrics is some tens of kilobytes.
const maxScrapeBytes = 32 << 20

// fetchMetrics GETs the metrics page in the text format.
func fetchMetrics(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	//nolint:gosec // G704: the operator's -metrics endpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/plain;version=0.0.4")
	res, err := client.Do(req) //nolint:gosec // G704: as above
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
		return nil, fmt.Errorf("status %s", res.Status)
	}
	if ct := res.Header.Get("Content-Type"); ct != "" {
		mt, _, err := mime.ParseMediaType(ct)
		if err != nil || mt != "text/plain" && mt != "application/openmetrics-text" {
			return nil, fmt.Errorf("content type %q is not the text format", ct)
		}
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxScrapeBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxScrapeBytes {
		return nil, fmt.Errorf("the page is larger than %d bytes", maxScrapeBytes)
	}
	return body, nil
}
