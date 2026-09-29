package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func runDarwinCommand(cmd string, args []string) error {
	switch cmd {
	case "tone":
		return cmdTone(args)
	case "spawn":
		return cmdSpawn(args)
	case "openapp":
		return cmdOpenApp(args)
	case "webtone":
		return cmdWebTone(args)
	case "lancheck":
		return cmdLanCheck(args)
	}
	e, err := loadEngine()
	if err != nil {
		return err
	}
	switch cmd {
	case "probe":
		return cmdProbe(e)
	case "apps":
		return cmdApps(e, args)
	case "record":
		return cmdRecord(e, args)
	case "selftest":
		return cmdSelftest(e, args)
	}
	return fmt.Errorf("unknown command %q", cmd)
}

func writePidfile(path string) {
	if path != "" {
		writeFile(path, []byte(strconv.Itoa(os.Getpid())))
	}
}

// cmdTone plays a tone (a fake app's sound). SIGTERM fades it out.
func cmdTone(args []string) error {
	fs := flag.NewFlagSet("tone", flag.ExitOnError)
	freq := fs.Float64("f", 440, "frequency (Hz); 0 = silent")
	dur := fs.Float64("d", 10, "seconds")
	amp := fs.Float64("amp", 0.1, "amplitude (0.1 = -20 dBFS)")
	mic := fs.Bool("mic", false, "also hold the microphone open")
	pidfile := fs.String("pidfile", "", "write this process's pid here")
	fs.Parse(args)
	writePidfile(*pidfile)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, os.Interrupt)
	go func() { <-sig; stopTone() }()
	return playTone(*freq, *amp, *dur, *mic)
}

// cmdSpawn runs a child process directly (posix_spawn, like Electron starting
// its helpers), so macOS makes this process responsible for it.
func cmdSpawn(args []string) error {
	fs := flag.NewFlagSet("spawn", flag.ExitOnError)
	pidfile := fs.String("pidfile", "", "write this process's pid here")
	fs.Parse(args)
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: s2 spawn [-pidfile f] <exe> <args…>")
	}
	writePidfile(*pidfile)
	c := exec.Command(fs.Arg(0), fs.Args()[1:]...)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	if err := c.Start(); err != nil {
		return err
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, os.Interrupt)
	go func() { <-sig; c.Process.Signal(syscall.SIGTERM) }()
	return c.Wait()
}

// cmdOpenApp launches another app through LaunchServices (like a voice app
// opening a link in the browser), then stays alive for -d seconds.
func cmdOpenApp(args []string) error {
	fs := flag.NewFlagSet("openapp", flag.ExitOnError)
	pidfile := fs.String("pidfile", "", "write this process's pid here")
	dur := fs.Float64("d", 10, "seconds to stay alive")
	fs.Parse(args)
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: s2 openapp [-pidfile f] -d 10 <app> <args…>")
	}
	writePidfile(*pidfile)
	if err := openApp(fs.Arg(0), fs.Args()[1:]...); err != nil {
		return err
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, os.Interrupt)
	select {
	case <-sig:
	case <-time.After(time.Duration(*dur * float64(time.Second))):
	}
	return nil
}

// openApp starts a new instance of an app bundle via LaunchServices, hidden and
// in the background; the app becomes its own responsible process.
func openApp(app string, args ...string) error {
	a := append([]string{"-n", "-g", "-j", app, "--args"}, args...)
	out, err := exec.Command("/usr/bin/open", a...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("open %s: %v: %s", app, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func cmdWebTone(args []string) error {
	fs := flag.NewFlagSet("webtone", flag.ExitOnError)
	freq := fs.Float64("f", 907, "frequency (Hz)")
	dur := fs.Float64("d", 5, "seconds")
	fs.Parse(args)
	var state string
	var err error
	onMain(func() { state, err = playWebViewTone(*freq, *dur) })
	fmt.Println("AudioContext state:", state)
	return err
}

func cmdProbe(e *engine) error {
	p, err := e.Probe()
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(p, "", "  ")
	fmt.Println(string(b))
	fmt.Println("tcc_audio_capture (private preflight, diagnostics only): 0 = allowed, 1 = denied, 2 = not asked yet")
	return nil
}

func exclusionFlags(fs *flag.FlagSet) (*string, *bool, *bool) {
	return fs.String("exclude", "", "extra bundle IDs to keep out (comma-separated)"),
		fs.Bool("no-default", false, "don't use the built-in voice-app list"),
		fs.Bool("no-mic", false, "don't keep out apps that use the microphone")
}

func buildRules(exclude string, noDefault bool) []string {
	var apps []string
	if !noDefault {
		apps = append(apps, defaultVoiceApps...)
	}
	for _, a := range strings.Split(exclude, ",") {
		if a = strings.TrimSpace(a); a != "" {
			apps = append(apps, a)
		}
	}
	return apps
}

func cmdApps(e *engine, args []string) error {
	fs := flag.NewFlagSet("apps", flag.ExitOnError)
	exclude, noDefault, noMic := exclusionFlags(fs)
	all := fs.Bool("all", false, "also list idle audio processes")
	asJSON := fs.Bool("json", false, "print the engine's JSON")
	fs.Parse(args)
	if err := e.SetRules(buildRules(*exclude, *noDefault), nil); err != nil {
		return err
	}
	var flags uint32
	if !*noMic {
		flags |= flagExcludeMic
	}
	s, err := e.ListApps(flags)
	if err != nil {
		return err
	}
	if *asJSON {
		fmt.Println(s)
		return nil
	}
	var v struct {
		Apps []appView `json:"apps"`
	}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return err
	}
	sort.Slice(v.Apps, func(i, j int) bool { return v.Apps[i].Pid < v.Apps[j].Pid })
	fmt.Printf("%-7s %-7s %-3s %-3s %-44s %-22s %s\n", "PID", "RPID", "OUT", "IN", "APP", "RESPONSIBLE", "FRIENDS HEAR")
	defer fmt.Println("IN: ● microphone (a real input device), ○ records only from an aggregate/tap (not a mic user)")
	for _, a := range v.Apps {
		if !*all && !a.Output && !a.Input && !a.Excluded {
			continue
		}
		hear := "yes"
		if a.Excluded {
			hear = "no: " + a.Reason
		}
		in := yn(a.Mic)
		if a.Input && !a.Mic {
			in = "○"
		}
		fmt.Printf("%-7d %-7d %-3s %-3s %-44s %-22s %s\n", a.Pid, a.Rpid, yn(a.Output), in, trunc(a.label(), 44),
			trunc(a.Responsible, 22), hear)
	}
	return nil
}

func yn(b bool) string {
	if b {
		return "●"
	}
	return "·"
}

func trunc(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func cmdRecord(e *engine, args []string) error {
	fs := flag.NewFlagSet("record", flag.ExitOnError)
	exclude, noDefault, noMic := exclusionFlags(fs)
	dur := fs.Float64("d", 20, "seconds")
	out := fs.String("o", "friends-hear.wav", "output WAV")
	endpoint := fs.Bool("endpoint", false, "no exclusion at all (baseline)")
	fs.Parse(args)
	if err := e.SetRules(buildRules(*exclude, *noDefault), nil); err != nil {
		return err
	}
	var flags uint32
	if !*noMic {
		flags |= flagExcludeMic
	}
	mode := modeIncludeSet
	if *endpoint {
		mode = modeEndpoint
	}
	if err := e.Start(mode, flags); err != nil {
		return err
	}
	defer e.Stop()
	stopOnInterrupt(e)
	fmt.Printf("recording %.0f s → %s\n", *dur, *out)
	res, err := capture(e, *dur, func(sec int) {
		if s, err := e.Status(); err == nil {
			printFriendsHear(sec, s)
		}
	}, nil)
	if werr := writeWAV(*out, res.samples); werr != nil && err == nil {
		err = werr
	}
	if res.lostMs > 0 {
		fmt.Printf("note: %d ms were lost because this program fell behind\n", res.lostMs)
	}
	return err
}

type captureResult struct {
	samples []float32
	lostMs  int   // audio missing between chunks (reader fell behind or the tap was rebuilt)
	startNs int64 // capture time of the first chunk (engine events use the same clock)
}

// capture reads chunks for dur seconds, calling everySecond(sec) and at(t) hooks.
func capture(e *engine, dur float64, everySecond func(int), at func(t float64)) (captureResult, error) {
	chunks := int(dur * 100)
	var res captureResult
	samples := make([]float32, 0, chunks*chunkFrames*2)
	buf := make([]float32, chunkFrames*2)
	var lastTS int64
	for i := 0; i < chunks; i++ {
		n, ts, err := e.Read(buf, 1000)
		if err != nil {
			res.samples = samples
			return res, err
		}
		if i == 0 {
			res.startNs = ts
		}
		if lastTS != 0 && ts-lastTS > 15_000_000 {
			res.lostMs += int((ts - lastTS - 10_000_000) / 1_000_000)
		}
		lastTS = ts
		samples = append(samples, buf[:n*2]...)
		if at != nil {
			at(float64(i+1) / 100)
		}
		if everySecond != nil && (i+1)%100 == 0 {
			everySecond((i + 1) / 100)
		}
	}
	res.samples = samples
	return res, nil
}

// stopOnInterrupt makes Ctrl+C stop the engine and any fake apps before exiting.
func stopOnInterrupt(e *engine) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		e.Stop()
		killStarted()
		os.Exit(130)
	}()
}

func printFriendsHear(sec int, statusJSON string) {
	var v statusView
	if json.Unmarshal([]byte(statusJSON), &v) != nil {
		return
	}
	var hear, out []string
	for _, a := range v.Apps {
		switch {
		case a.Excluded:
			out = append(out, fmt.Sprintf("%s (%s)", a.label(), a.Reason))
		case a.Output:
			hear = append(hear, a.label())
		}
	}
	fmt.Printf("%3ds level %.3f · friends hear: %s\n     kept out: %s\n", sec, v.Peak, orNone(hear), orNone(out))
	for _, w := range v.Warnings {
		fmt.Printf("     warning: %s\n", w)
	}
}

func orNone(xs []string) string {
	if len(xs) == 0 {
		return "(nothing)"
	}
	return strings.Join(xs, ", ")
}
