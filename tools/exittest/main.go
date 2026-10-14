// Command exittest records and summarizes the M1 exit test (docs/m1/06-deploy-and-ci.md §12, "06" below; the run
// book is docs/m1/exit-test/README.md). It uses the standard library only, so the binary that runs on the VPS is
// one static file, built into the repository's ignored bin/:
//
//	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go -C tools build -o ../bin/exittest ./exittest
//
// # collect
//
//	exittest collect [-o run.csv] [-metrics http://127.0.0.1:9469/metrics] [-unit isshoni | -pid-file FILE]
//	  [-nic NAME] [-metrics-dir DIR] [-proc /proc] [-interval 10s] [-scrape-interval 30s]
//
// runs on the VPS as root (it lists /proc/<pid>/fd of the isshoni user's process) until SIGTERM or SIGINT,
// detached from the SSH session, because the owner's Wi-Fi drops at T+75 (06 §12.3). --collect unloads the unit
// however the collector ends, so the same command works again after a failed start:
//
//	sudo systemd-run --collect --unit=exittest-collect --working-directory="$PWD" "$PWD/exittest" collect \
//	  -o run.csv -metrics http://127.0.0.1:9469/metrics
//	sudo systemctl stop exittest-collect   # at T+120
//
// Every -interval it appends a sample row to run.csv: the isshoni PID, looked up again each time with
// `systemctl show -p MainPID --value <unit>` (or read from -pid-file), the process's CPU, RSS, threads and open
// file descriptors from /proc/<pid>, and the machine's CPU count and CPU use, load average, NIC bytes
// (/proc/net/dev; by default the interface of the IPv4 default route), UDP datagrams and socket errors
// (/proc/net/snmp plus /proc/net/snmp6) and TCP retransmissions. A PID that differs from the last one is first
// written as a restart row, which marks the T+60 restart; a sample without a process is a down row, which still
// has the machine's columns. Every -scrape-interval it GETs the metrics page, saves it whole into -metrics-dir
// (default: run.csv's name without .csv, plus "-metrics") as <UTC time>.prom, and appends a scrape row with the
// series 06 §12.3 lists (the metricColumns of scrape.go). A scrape that fails is a scrape_error row; neither a
// restart nor a metrics endpoint that is gone for a while stops the collector. It stops on SIGTERM or SIGINT with
// a stop row and exit status 0.
//
// When run.csv already holds a run of the same format (the collector was started again), it appends to it, after
// a new start row, and still marks a restart that happened in between. Each row is one write, so a collector that
// is killed leaves whole rows.
//
// # run.csv
//
// One header line, then one row per event, oldest first. time is UTC with milliseconds; kind is start, sample,
// down, restart, scrape, scrape_error or stop; pid is the isshoni process; note is free text. A cell is empty when
// the row's kind doesn't fill that column or the value could not be read. The columns:
//
//	cpu_ticks, cpu_pct          the process's utime+stime (1/100 s) and its CPU since the previous sample,
//	                            in percent of ALL CPUs (100 = every CPU busy; the pass criteria count this way)
//	rss_bytes, threads, fds     VmRSS, num_threads, entries of /proc/<pid>/fd
//	cpus, host_cpu_pct          CPUs, and the whole machine's CPU since the previous sample (percent of all)
//	load1, load5, load15        /proc/loadavg
//	nic_rx_bytes, nic_tx_bytes  counters since boot
//	udp_*, tcp_retrans_segs     counters since boot
//	go_goroutines … client_*    from the scrape; counters start at 0 when isshoni starts
//
// # summarize
//
//	exittest summarize [-t0 TIME] run.csv
//
// prints, as Markdown for the results file (no title, ### sections: it goes under the file's "## Summary" as it
// is), the pass-criteria table of 06 §12.3 with every row that the recording can measure filled in and marked
// pass, FAIL, check (measured, needs a look) or n/a (the others say "manual": the people, the admin dashboard or
// the survey answer them), then each process's CPU, memory and gauges, and every counter's total and rates per
// process and in all. A row passes only on what was recorded: a gap of more than 2 minutes in the session, CPU
// samples that miss a tenth of it or the stress, or a recording that does not span T+0 to T+120 make it "check".
// Rates and totals are per process, split where the PID changes, and a counter that goes down counts as reset, the
// Prometheus way: the increase after a reset is the new value (the scraped counters start again at zero at T+60).
// Several of them going down at once without a PID change is a restart in place (04 §6.5), which the Session row
// flags. The viewers' freezes are a share of the video tracks' time (the forwarding video DownTracks times time),
// because they report them per inbound video track. T+0, which the windows of the pass criteria count from, is
// -t0 (RFC 3339, or HH:MM[:SS] in UTC within 12 hours of the first row); without it, 60 minutes before the first
// restart (the scripted one at T+60), or 15 minutes after the first row when there is no restart (the collector
// starts at T-15). The output names no host, address or person.
//
// Exit status: 0 done (collect: stopped by a signal), 1 an error (collect: run.csv can't be written), 2 a usage
// error.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
}

// realMain is main without the exit: the tests run it in a child process to send it real signals.
func realMain(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx, args, stdout, stderr)
}

const usageLine = `usage: exittest collect [flags]       record run.csv on the VPS (runs until SIGTERM)
       exittest summarize [-t0 TIME] run.csv   print the summary as Markdown
Run "exittest collect -h" or "exittest summarize -h" for the flags.`

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usageLine)
		return 2
	}
	switch args[0] {
	case "collect":
		return runCollect(ctx, args[1:], stderr)
	case "summarize":
		return runSummarize(args[1:], stdout, stderr)
	case "help", "-h", "-help", "--help":
		fmt.Fprintln(stdout, usageLine)
		return 0
	default:
		fmt.Fprintf(stderr, "exittest: unknown command %q\n%s\n", args[0], usageLine)
		return 2
	}
}

func runCollect(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("exittest collect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("o", "run.csv", "the CSV file to append to")
	metrics := fs.String("metrics", "http://127.0.0.1:9469/metrics", `the metrics page to scrape ("" scrapes nothing)`)
	metricsDir := fs.String("metrics-dir", "",
		`where each scrape is saved (default: the -o name without ".csv", plus "-metrics")`)
	unit := fs.String("unit", "isshoni", "the systemd unit whose main process is isshoni")
	pidFile := fs.String("pid-file", "", "read isshoni's PID from this file instead of asking systemd")
	nic := fs.String("nic", "",
		`the network interface to count (default: the IPv4 default route's; "all": every one except lo)`)
	procDir := fs.String("proc", "/proc", "where the proc file system is mounted")
	interval := fs.Duration("interval", 10*time.Second, "time between two samples (06 §12.3: 10s)")
	scrapeInterval := fs.Duration("scrape-interval", 30*time.Second, "time between two scrapes (06 §12.3: 30s)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: exittest collect [flags]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	usage := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "exittest collect: "+format+"\n", a...)
		fs.Usage()
		return 2
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	switch {
	case fs.NArg() != 0:
		return usage("unexpected argument %q", fs.Arg(0))
	case *out == "":
		return usage("-o is empty")
	case *interval <= 0 || *scrapeInterval <= 0:
		return usage("-interval and -scrape-interval must be positive")
	case set["unit"] && *pidFile != "":
		return usage("-unit and -pid-file exclude each other")
	case *pidFile == "" && !unitName.MatchString(*unit):
		return usage("-unit %q is not a systemd unit name", *unit)
	}
	if *metrics != "" {
		u, err := url.Parse(*metrics)
		if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
			return usage("-metrics %q is not an http or https URL", *metrics)
		}
		if *metricsDir == "" {
			*metricsDir = strings.TrimSuffix(*out, filepath.Ext(*out)) + "-metrics"
		}
	}
	pid := systemdMainPID(*unit, min(5*time.Second, *interval/2))
	if *pidFile != "" {
		pid = filePID(*pidFile)
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	c, err := newCollector(collectOptions{
		out: *out, metricsURL: *metrics, metricsDir: *metricsDir, procDir: *procDir, nic: *nic,
		interval: *interval, scrapeInterval: *scrapeInterval, pid: pid, log: log,
	}, time.Now())
	if err != nil {
		fmt.Fprintf(stderr, "exittest collect: %v\n", err)
		return 1
	}
	runErr := c.run(ctx)
	why := "stopped by a signal"
	if runErr != nil {
		why = "error: " + oneLine(runErr.Error())
	}
	if err := errors.Join(runErr, c.close(time.Now(), why)); err != nil {
		fmt.Fprintf(stderr, "exittest collect: %v\n", err)
		return 1
	}
	return 0
}

func runSummarize(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("exittest summarize", flag.ContinueOnError)
	fs.SetOutput(stderr)
	t0 := fs.String("t0", "", "T+0 of the session: RFC 3339, or HH:MM[:SS] in UTC within 12 hours of the first row "+
		"(default: 60 minutes before the first restart, or 15 minutes after the first row)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: exittest summarize [-t0 TIME] run.csv")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "exittest summarize: name one run.csv (flags go before it)")
		fs.Usage()
		return 2
	}
	path := fs.Arg(0)
	f, err := os.Open(path) //nolint:gosec // G304: the operator's file
	if err != nil {
		fmt.Fprintf(stderr, "exittest summarize: %v\n", err)
		return 1
	}
	rf, err := readRun(f)
	_ = f.Close()
	if err != nil {
		fmt.Fprintf(stderr, "exittest summarize: %s: %v\n", path, err)
		return 1
	}
	a, err := analyze(rf, *t0)
	if err != nil {
		var ue usageError
		if errors.As(err, &ue) {
			fmt.Fprintf(stderr, "exittest summarize: %v\n", err)
			fs.Usage()
			return 2
		}
		fmt.Fprintf(stderr, "exittest summarize: %s: %v\n", path, err)
		return 1
	}
	if err := a.write(stdout, filepath.Base(path)); err != nil {
		fmt.Fprintf(stderr, "exittest summarize: %v\n", err)
		return 1
	}
	return 0
}

// usageError is an analysis error that a flag caused.
type usageError struct{ error }
