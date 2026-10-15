package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// pidFunc returns the PID of the isshoni process, or 0 when there is none.
type pidFunc func(ctx context.Context) (int, error)

// collectOptions configure a collector. Every field but onRow and now comes from a flag of `exittest collect`.
type collectOptions struct {
	out            string        // run.csv
	metricsURL     string        // "" scrapes nothing
	metricsDir     string        // where each scrape is saved
	procDir        string        // /proc
	nic            string        // "" picks the default route's interface; nicAll counts every one except lo
	interval       time.Duration // between samples
	scrapeInterval time.Duration // between scrapes
	pid            pidFunc
	log            *slog.Logger

	now   func() time.Time // time.Now; the fixture's simulation sets its own clock
	onRow func(*runRow)    // tests: called after each row is written, on the collector's goroutine
}

// collector samples the isshoni process and the machine every interval and scrapes the metrics every
// scrapeInterval, appending a row to run.csv for each (06 §12.3). One goroutine runs it: newCollector opens the
// file and writes the start row, run loops until its context ends, and close writes the stop row.
type collector struct {
	o      collectOptions
	f      *os.File
	proc   *procFS
	nic    string
	client *http.Client

	lastPID   int    // the last PID seen, 0 before the first
	lastStart uint64 // that process's start time (ticks after boot), 0 while unknown
	prevProc  struct {
		pid   int
		start uint64
		ticks uint64
		at    time.Time
	}
	prevCPU   cpuTimes
	down      bool // the last lookup found no process (for logging the change only)
	scrapeErr bool // the last scrape failed (for logging the change only)
}

// newCollector opens run.csv, appending when it already holds a run (the collector was started again), and
// writes the start row.
func newCollector(o collectOptions, now time.Time) (c *collector, err error) {
	if o.now == nil {
		o.now = time.Now
	}
	if o.log == nil {
		o.log = slog.New(slog.DiscardHandler)
	}
	c = &collector{o: o}
	defer func(c *collector) {
		if err != nil {
			_ = c.closeFiles()
		}
	}(c)
	if c.proc, err = openProc(o.procDir); err != nil {
		return nil, fmt.Errorf("the proc file system: %w", err)
	}
	c.nic = o.nic
	if c.nic == "" {
		nic, err := c.proc.defaultRouteNIC()
		switch {
		case err != nil:
			o.log.Warn("no default route, counting every interface except lo", "err", err)
			c.nic = nicAll
		case nic == "":
			o.log.Warn("no IPv4 default route, counting every interface except lo")
			c.nic = nicAll
		default:
			c.nic = nic
		}
	}
	if o.metricsURL != "" {
		//nolint:gosec // G301: measurements, not secrets, which the owner copies off the VPS as a normal user
		if err := os.MkdirAll(o.metricsDir, 0o755); err != nil {
			return nil, fmt.Errorf("the scrape directory: %w", err)
		}
		// A new connection per scrape: an idle one would point at the process before a restart.
		c.client = &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	}
	appending, lastPID, err := c.openOut()
	if err != nil {
		return nil, err
	}
	c.lastPID = lastPID
	note := fmt.Sprintf("exittest collect: interval=%s scrape_interval=%s nic=%s user_hz=%d metrics=%t",
		o.interval, o.scrapeInterval, c.nic, userHZ, o.metricsURL != "")
	if appending {
		note += " appending"
		if lastPID != 0 {
			note += " last_pid=" + strconv.Itoa(lastPID)
		}
	}
	if err := c.write(&runRow{t: now, kind: kindStart, note: note}); err != nil {
		return nil, err
	}
	o.log.Info("collecting", "out", o.out, "interval", o.interval, "scrape_interval", o.scrapeInterval,
		"nic", c.nic, "metrics", o.metricsURL != "", "appending", appending)
	return c, nil
}

// openOut opens run.csv for appending. A new or empty file gets the header. A file that already holds rows must
// have the same header; its last PID is returned, so that a restart while the collector was down is still marked.
func (c *collector) openOut() (appending bool, lastPID int, err error) {
	//nolint:gosec // G302, G304: the operator's -o; measurements, not secrets, which the owner copies off the VPS
	c.f, err = os.OpenFile(c.o.out, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return false, 0, err
	}
	data, err := io.ReadAll(c.f)
	if err != nil {
		return false, 0, fmt.Errorf("%s: %w", c.o.out, err)
	}
	head := encodeHeader()
	if len(data) == 0 {
		_, err := c.f.Write(head)
		return false, 0, err
	}
	if !bytes.HasPrefix(data, head) {
		return false, 0, fmt.Errorf("%s exists and is not a run.csv of this version of exittest; name another file "+
			"with -o", c.o.out)
	}
	if data[len(data)-1] != '\n' { // a row cut short: end it, so it doesn't swallow the next one
		if _, err := c.f.Write([]byte("\n")); err != nil {
			return false, 0, err
		}
	}
	rf, err := readRun(bytes.NewReader(data))
	if err != nil {
		return false, 0, fmt.Errorf("%s: %w", c.o.out, err)
	}
	for _, r := range slices.Backward(rf.rows) {
		if r.pid != 0 && (r.kind == kindSample || r.kind == kindRestart) {
			return true, r.pid, nil
		}
	}
	return true, 0, nil
}

// encodeHeader is run.csv's first line. The column names need no quoting.
func encodeHeader() []byte {
	return []byte(strings.Join(header(), ",") + "\n")
}

// write appends one row. Each row goes out in one write, so a collector killed in the middle leaves whole rows.
func (c *collector) write(r *runRow) error {
	line, err := encodeRow(r)
	if err != nil {
		return err
	}
	if _, err := c.f.Write(line); err != nil {
		return fmt.Errorf("%s: %w", c.o.out, err)
	}
	if c.o.onRow != nil {
		c.o.onRow(r)
	}
	return nil
}

// run samples and scrapes until ctx ends: at once, and then on every tick. It returns nil when ctx ends and an
// error only when run.csv can't be written. Neither the process going away nor the metrics endpoint failing stops
// it: those become rows.
func (c *collector) run(ctx context.Context) error {
	sampleTick := time.NewTicker(c.o.interval)
	defer sampleTick.Stop()
	var scrapeC <-chan time.Time
	if c.client != nil {
		scrapeTick := time.NewTicker(c.o.scrapeInterval)
		defer scrapeTick.Stop()
		scrapeC = scrapeTick.C
	}
	if err := c.sample(ctx, c.o.now()); err != nil {
		return err
	}
	if c.client != nil {
		if err := c.scrape(ctx, c.o.now()); err != nil {
			return err
		}
	}
	for {
		var err error
		select {
		case <-ctx.Done():
			return nil
		case <-sampleTick.C:
			err = c.sample(ctx, c.o.now())
		case <-scrapeC:
			err = c.scrape(ctx, c.o.now())
		}
		if err != nil {
			return err
		}
	}
}

// close writes the stop row with why, and closes the files.
func (c *collector) close(now time.Time, why string) error {
	err := c.write(&runRow{t: now, kind: kindStop, note: why})
	if err == nil {
		err = c.f.Sync()
	}
	c.o.log.Info("stopped", "why", why)
	return errors.Join(err, c.closeFiles())
}

func (c *collector) closeFiles() error {
	var errs []error
	if c.f != nil {
		errs = append(errs, c.f.Close())
	}
	if c.proc != nil {
		errs = append(errs, c.proc.Close())
	}
	if c.client != nil {
		c.client.CloseIdleConnections()
	}
	return errors.Join(errs...)
}

// stopping reports whether the collector's context has ended. A step that the stop cut short (a lookup, a scrape)
// measured nothing and writes no row.
func stopping(ctx context.Context) bool { return ctx.Err() != nil }

// lookup resolves the PID again (06 §12.3: on every sample) and writes a restart row when it changed. It returns 0
// and why when there is no process.
func (c *collector) lookup(ctx context.Context, now time.Time) (pid int, why string, err error) {
	pid, lerr := c.o.pid(ctx)
	if stopping(ctx) {
		return 0, "", nil // stopping: the caller writes nothing
	}
	switch {
	case lerr != nil:
		why = "no PID: " + oneLine(lerr.Error())
	case pid == 0:
		why = "no PID: the service has no main process"
	}
	if why != "" {
		if !c.down {
			c.o.log.Warn("isshoni is not running", "why", why)
			c.down = true
		}
		return 0, why, nil
	}
	if c.down {
		c.o.log.Info("isshoni is running", "pid", pid)
		c.down = false
	}
	if c.lastPID != 0 && pid != c.lastPID {
		if err := c.restart(now, pid, fmt.Sprintf("pid %d -> %d", c.lastPID, pid)); err != nil {
			return 0, "", err
		}
	}
	c.lastPID = pid
	return pid, "", nil
}

func (c *collector) restart(now time.Time, pid int, note string) error {
	c.o.log.Warn("isshoni restarted", "change", note)
	c.lastStart = 0
	return c.write(&runRow{t: now, kind: kindRestart, pid: pid, note: note})
}

// sample writes one sample row: the process (when there is one) and the machine.
func (c *collector) sample(ctx context.Context, now time.Time) error {
	pid, why, err := c.lookup(ctx, now)
	if err != nil {
		return err
	}
	if stopping(ctx) {
		return nil // stopping: a lookup cut short is no measurement
	}
	r := &runRow{t: now, kind: kindSample, pid: pid, vals: make(map[string]float64)}
	var notes []string
	var st processStat
	if pid == 0 {
		r.kind, notes = kindDown, append(notes, why)
	} else {
		st, err = c.proc.process(pid)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			r.kind, r.pid = kindDown, 0
			notes = append(notes, fmt.Sprintf("pid %d: the process ended", pid))
		case err != nil:
			r.kind, r.pid = kindDown, 0
			notes = append(notes, fmt.Sprintf("pid %d: %s", pid, oneLine(err.Error())))
		default:
			if c.lastStart != 0 && st.startTicks != c.lastStart {
				if err := c.restart(now, pid, fmt.Sprintf("pid %d now belongs to a process started later", pid)); err != nil {
					return err
				}
			}
			c.lastStart = st.startTicks
			r.vals[colCPUTicks] = float64(st.cpuTicks)
			r.vals[colRSS] = float64(st.rssBytes)
			r.vals[colThreads] = float64(st.threads)
			if n, err := c.proc.openFDs(pid); err == nil {
				r.vals[colFDs] = float64(n)
			} else {
				notes = append(notes, "fds: "+oneLine(err.Error()))
			}
		}
	}
	notes = append(notes, c.host(r)...)
	if r.kind == kindSample {
		// The process's CPU since the previous sample of the same process, as a share of every CPU.
		p := &c.prevProc
		cpus := r.vals[colCPUs]
		if p.pid == pid && p.start == st.startTicks && now.After(p.at) && st.cpuTicks >= p.ticks && cpus > 0 {
			secs := float64(st.cpuTicks-p.ticks) / userHZ
			r.vals[colCPUPct] = round2(100 * secs / now.Sub(p.at).Seconds() / cpus)
		}
		p.pid, p.start, p.ticks, p.at = pid, st.startTicks, st.cpuTicks, now
	}
	r.note = strings.Join(notes, "; ")
	return c.write(r)
}

// host fills the machine's columns of r and returns what could not be read.
func (c *collector) host(r *runRow) (notes []string) {
	if ct, err := c.proc.cpu(); err == nil {
		r.vals[colCPUs] = float64(ct.cpus)
		prev := c.prevCPU
		if prev.total != 0 && ct.total > prev.total && ct.busy >= prev.busy {
			r.vals[colHostCPUPct] = round2(100 * float64(ct.busy-prev.busy) / float64(ct.total-prev.total))
		}
		c.prevCPU = ct
	} else {
		notes = append(notes, "stat: "+oneLine(err.Error()))
	}
	if l, err := c.proc.loadavg(); err == nil {
		r.vals[colLoad1], r.vals[colLoad5], r.vals[colLoad15] = l[0], l[1], l[2]
	} else {
		notes = append(notes, "loadavg: "+oneLine(err.Error()))
	}
	if rx, tx, err := c.proc.netDev(c.nic); err == nil {
		r.vals[colNICRx], r.vals[colNICTx] = float64(rx), float64(tx)
	} else {
		notes = append(notes, "net/dev: "+oneLine(err.Error()))
	}
	if nc, err := c.proc.netCounters(); err == nil {
		r.vals[colUDPIn], r.vals[colUDPOut] = float64(nc.udpIn), float64(nc.udpOut)
		r.vals[colUDPRcvbufErrs], r.vals[colUDPSndbufErrs] = float64(nc.udpRcvbufErrors), float64(nc.udpSndbufErrors)
		r.vals[colUDPInErrs], r.vals[colTCPRetrans] = float64(nc.udpInErrors), float64(nc.tcpRetransSegs)
	} else {
		notes = append(notes, "snmp: "+oneLine(err.Error()))
	}
	return notes
}

// scrape fetches the metrics page, saves it whole into the scrape directory and writes a scrape row with the
// columns of metricColumns. A failure is a scrape_error row.
func (c *collector) scrape(ctx context.Context, now time.Time) error {
	pid, _, err := c.lookup(ctx, now)
	if err != nil || stopping(ctx) {
		return err
	}
	sctx, cancel := context.WithTimeout(ctx, c.scrapeTimeout())
	body, ferr := fetchMetrics(sctx, c.client, c.o.metricsURL)
	cancel()
	if stopping(ctx) {
		return nil // stopping: a scrape cut short is no failure of the endpoint
	}
	if ferr != nil {
		return c.scrapeFailed(now, pid, oneLine(ferr.Error()))
	}
	name := now.UTC().Format("20060102T150405.000Z") + ".prom"
	if err := writeFileAtomic(filepath.Join(c.o.metricsDir, name), body); err != nil {
		return c.scrapeFailed(now, pid, "saving the scrape: "+oneLine(err.Error()))
	}
	samples, skipped, perr := parsePromText(body)
	r := &runRow{t: now, kind: kindScrape, pid: pid, vals: metricValues(samples), note: name}
	if len(r.vals) == 0 {
		return c.scrapeFailed(now, pid, fmt.Sprintf("%s holds none of the exit test's metrics", name))
	}
	if skipped > 0 {
		r.note += fmt.Sprintf("; %d lines skipped, the first: %s", skipped, oneLine(perr.Error()))
	}
	if c.scrapeErr {
		c.o.log.Info("the metrics endpoint answers again")
		c.scrapeErr = false
	}
	return c.write(r)
}

func (c *collector) scrapeFailed(now time.Time, pid int, why string) error {
	if !c.scrapeErr {
		c.o.log.Warn("scrape failed", "why", why)
		c.scrapeErr = true
	}
	return c.write(&runRow{t: now, kind: kindScrapeError, pid: pid, note: why})
}

// scrapeTimeout bounds one scrape, so a hanging endpoint costs at most half a sample interval.
func (c *collector) scrapeTimeout() time.Duration {
	return min(5*time.Second, c.o.interval/2)
}

// writeFileAtomic writes data to path through a temporary file and a rename, so a reader never sees half a file.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".scrape-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o644) //nolint:gosec // G302: measurements, not secrets, as run.csv
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}

// oneLine keeps a message on one line of the CSV and short.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// unitName is what -unit accepts: a systemd unit name, which can't be read as an option of systemctl.
var unitName = regexp.MustCompile(`^[A-Za-z0-9:_.@\\][A-Za-z0-9:_.@\\-]*$`)

// systemdMainPID asks systemd for the unit's main process: `systemctl show -p MainPID --value <unit>` (06 §12.3).
func systemdMainPID(unit string, timeout time.Duration) pidFunc {
	return func(ctx context.Context) (int, error) {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		var stderr bytes.Buffer
		//nolint:gosec // G204: one -unit argument, checked by unitName, and no shell
		cmd := exec.CommandContext(ctx, "systemctl", "show", "-p", "MainPID", "--value", unit)
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			if msg := strings.TrimSpace(stderr.String()); msg != "" {
				return 0, fmt.Errorf("systemctl show %s: %w: %s", unit, err, msg)
			}
			return 0, fmt.Errorf("systemctl show %s: %w", unit, err)
		}
		return parsePID(out)
	}
}

// filePID reads the PID from a file on every lookup (-pid-file: a server that systemd doesn't run).
func filePID(path string) pidFunc {
	return func(context.Context) (int, error) {
		data, err := os.ReadFile(path) //nolint:gosec // G304: the operator's -pid-file
		if err != nil {
			return 0, err
		}
		return parsePID(data)
	}
}

func parsePID(b []byte) (int, error) {
	s := strings.TrimSpace(string(b))
	pid, err := strconv.Atoi(s)
	if err != nil || pid < 0 {
		return 0, fmt.Errorf("%q is not a PID", s)
	}
	return pid, nil
}
