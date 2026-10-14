package main

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

// The kinds of row in run.csv, its second column.
const (
	// kindStart: the collector started, or started again and appends to a file it wrote before. The note says how
	// it samples.
	kindStart = "start"
	// kindSample: one sample of the isshoni process and of the machine, every -interval.
	kindSample = "sample"
	// kindDown: a sample without an isshoni process. systemd named none (MainPID 0), the PID could not be read, or
	// the process ended between the lookup and the read. The machine's columns are filled all the same.
	kindDown = "down"
	// kindRestart: the PID differs from the last one seen (or the same PID now belongs to a process that started
	// later). pid is the new process; the note names the old one. 06 §12.3 asks for it to mark the T+60 restart.
	kindRestart = "restart"
	// kindScrape: one /metrics scrape, every -scrape-interval. The note names the file the whole scrape went to.
	kindScrape = "scrape"
	// kindScrapeError: a scrape that got no metrics (the endpoint refused, failed, timed out or answered with
	// something else than the text format). The note says why.
	kindScrapeError = "scrape_error"
	// kindStop: the collector stopped. The note says why (a signal, or the error it could not go on after).
	kindStop = "stop"
)

// timeLayout is the format of the time column: UTC, with milliseconds.
const timeLayout = "2006-01-02T15:04:05.000Z07:00"

// The columns of a sample about the isshoni process (06 §12.3).
const (
	colCPUTicks = "cpu_ticks" // utime + stime from /proc/<pid>/stat, in 1/100 s
	colCPUPct   = "cpu_pct"   // the process's CPU since the previous sample, in percent of all CPUs
	colRSS      = "rss_bytes" // VmRSS from /proc/<pid>/status
	colThreads  = "threads"   // num_threads from /proc/<pid>/stat
	colFDs      = "fds"       // entries of /proc/<pid>/fd
)

// The columns of a sample about the machine (06 §12.3, plus the CPU count and the machine's own CPU use, which the
// pass criterion "Server CPU" can be read against).
const (
	colCPUs          = "cpus"         // the CPUs in /proc/stat
	colHostCPUPct    = "host_cpu_pct" // the whole machine's CPU since the previous sample, in percent of all CPUs
	colLoad1         = "load1"        // /proc/loadavg
	colLoad5         = "load5"
	colLoad15        = "load15"
	colNICRx         = "nic_rx_bytes" // /proc/net/dev, the -nic interface (or every interface except lo)
	colNICTx         = "nic_tx_bytes"
	colUDPIn         = "udp_in_datagrams" // /proc/net/snmp Udp plus /proc/net/snmp6 Udp6
	colUDPOut        = "udp_out_datagrams"
	colUDPRcvbufErrs = "udp_rcvbuf_errors"
	colUDPSndbufErrs = "udp_sndbuf_errors"
	colUDPInErrs     = "udp_in_errors"
	colTCPRetrans    = "tcp_retrans_segs" // /proc/net/snmp Tcp
)

// colNote is the last column: free text, never parsed except by people.
const colNote = "note"

// processColumns and hostColumns are filled by sample rows (hostColumns by down rows too), metricColumns (scrape.go)
// by scrape rows.
var (
	processColumns = []string{colCPUTicks, colCPUPct, colRSS, colThreads, colFDs}
	hostColumns    = []string{
		colCPUs, colHostCPUPct, colLoad1, colLoad5, colLoad15, colNICRx, colNICTx,
		colUDPIn, colUDPOut, colUDPRcvbufErrs, colUDPSndbufErrs, colUDPInErrs, colTCPRetrans,
	}
)

// header is run.csv's first line, as fields.
func header() []string {
	h := []string{"time", "kind", "pid"}
	h = append(h, processColumns...)
	h = append(h, hostColumns...)
	for _, m := range metricColumns {
		h = append(h, m.col)
	}
	return append(h, colNote)
}

// runRow is one row of run.csv. vals holds the numeric columns that have a value; an empty cell has none.
type runRow struct {
	line int // the line in the file, from 1 (the header); 0 for a row not read from a file
	t    time.Time
	kind string
	pid  int // 0: none
	vals map[string]float64
	note string
}

// has reports whether the row has a value in column col.
func (r *runRow) has(col string) bool {
	_, ok := r.vals[col]
	return ok
}

// encodeRow formats r as one CSV line, in the columns of header().
func encodeRow(r *runRow) ([]byte, error) {
	cols := header()
	rec := make([]string, len(cols))
	rec[0] = r.t.UTC().Format(timeLayout)
	rec[1] = r.kind
	if r.pid != 0 {
		rec[2] = strconv.Itoa(r.pid)
	}
	for i, c := range cols[3 : len(cols)-1] {
		if v, ok := r.vals[c]; ok {
			rec[3+i] = formatValue(v)
		}
	}
	rec[len(rec)-1] = r.note
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.Write(rec); err != nil {
		return nil, err
	}
	w.Flush()
	return buf.Bytes(), w.Error()
}

// formatValue writes a value as short as it reads back exactly: integers without a decimal point.
func formatValue(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// round2 rounds a derived value (a percentage) to two decimals, so the file carries no false precision.
func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

// byteOrderMark is U+FEFF in UTF-8, which a spreadsheet may put in front of the header when it saves the file.
const byteOrderMark = "\xef\xbb\xbf"

// errNotRun is returned for a file whose first line is not a run.csv header.
var errNotRun = errors.New("not a run.csv written by exittest collect")

// runFile is a parsed run.csv.
type runFile struct {
	rows    []*runRow
	skipped []string // rows that could not be read, as "line N: why"
}

// readRun parses a run.csv. The columns are found by their names in the header, so a file with fewer columns (an
// older collector) or more (a newer one) still reads; time and kind must be there. A row that doesn't parse (a
// line cut short when the machine stopped) is skipped and listed, not fatal.
func readRun(r io.Reader) (*runFile, error) {
	cr := csv.NewReader(bufio.NewReader(r))
	cr.FieldsPerRecord = -1
	cr.ReuseRecord = false
	head, err := cr.Read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%w: the file is empty", errNotRun)
		}
		return nil, fmt.Errorf("%w: %w", errNotRun, err)
	}
	index := make(map[string]int, len(head))
	for i, name := range head {
		index[strings.TrimPrefix(name, byteOrderMark)] = i // a spreadsheet may have added one
	}
	iTime, okTime := index["time"]
	iKind, okKind := index["kind"]
	if !okTime || !okKind {
		return nil, fmt.Errorf("%w: the header has no time or kind column", errNotRun)
	}
	numeric := numericColumns()
	f := &runFile{}
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var pe *csv.ParseError
			if !errors.As(err, &pe) {
				return nil, err
			}
			f.skipped = append(f.skipped, fmt.Sprintf("line %d: %v", pe.StartLine, pe.Err))
			continue
		}
		line, _ := cr.FieldPos(0)
		row, err := parseRow(rec, index, iTime, iKind, numeric)
		if err != nil {
			f.skipped = append(f.skipped, fmt.Sprintf("line %d: %v", line, err))
			continue
		}
		row.line = line
		f.rows = append(f.rows, row)
	}
	return f, nil
}

// numericColumns lists every column that holds a number.
func numericColumns() []string {
	cols := header()
	return cols[3 : len(cols)-1]
}

// parseRow reads one record.
func parseRow(rec []string, index map[string]int, iTime, iKind int, numeric []string) (*runRow, error) {
	if len(rec) <= max(iTime, iKind) {
		return nil, fmt.Errorf("%d fields, too few", len(rec))
	}
	t, err := time.Parse(time.RFC3339Nano, rec[iTime])
	if err != nil {
		return nil, fmt.Errorf("time %q: %w", rec[iTime], err)
	}
	row := &runRow{t: t, kind: rec[iKind], vals: make(map[string]float64)}
	switch row.kind {
	case kindStart, kindSample, kindDown, kindRestart, kindScrape, kindScrapeError, kindStop:
	default:
		return nil, fmt.Errorf("unknown kind %q", row.kind)
	}
	if i, ok := index["pid"]; ok && i < len(rec) && rec[i] != "" {
		pid, err := strconv.Atoi(rec[i])
		if err != nil || pid < 0 {
			return nil, fmt.Errorf("pid %q is not a process id", rec[i])
		}
		row.pid = pid
	}
	for _, c := range numeric {
		i, ok := index[c]
		if !ok || i >= len(rec) || rec[i] == "" {
			continue
		}
		v, err := strconv.ParseFloat(rec[i], 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("%s %q is not a number", c, rec[i])
		}
		row.vals[c] = v
	}
	if i, ok := index[colNote]; ok && i < len(rec) {
		row.note = rec[i]
	}
	return row, nil
}
