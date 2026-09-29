//go:build windows

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// exitCode lets a command request a process exit status after its defers ran.
type exitCode int

func (c exitCode) Error() string   { return fmt.Sprintf("exit status %d", int(c)) }
func (c exitCode) ExitStatus() int { return int(c) }

func runWindowsCommand(cmd string, args []string) error {
	// Resolve DLLs (including isshoni_audio.dll's imports) from System32 only,
	// never from the folder s1.exe was downloaded to.
	windows.SetDefaultDllDirectories(windows.LOAD_LIBRARY_SEARCH_SYSTEM32)
	switch cmd {
	case "tone":
		return cmdTone(args)
	case "spawn":
		return cmdSpawn(args)
	}
	e, err := loadEngine()
	if err != nil {
		return err
	}
	switch cmd {
	case "probe":
		p, err := e.Probe()
		if err != nil {
			return err
		}
		fmt.Printf("Windows build %d · per-app loopback: %v (format %s) · output %q · volume %.0f%%%s %s\n",
			p.OSBuild, p.ProcessLoopback, p.Format, p.Device, p.MasterVolume*100, map[bool]string{true: " MUTED", false: ""}[p.MasterMuted], p.Error)
	case "apps":
		return cmdApps(e, args)
	case "record":
		return cmdRecord(e, args)
	case "selftest":
		return cmdSelftest(e, args)
	}
	return nil
}

// s1 tone -f 440 -d 10 [-amp 0.25] [-mic]: a fake app that plays a sine (and optionally holds the mic).
func cmdTone(args []string) error {
	fs := flag.NewFlagSet("tone", flag.ExitOnError)
	freq := fs.Float64("f", 440, "frequency (Hz)")
	dur := fs.Float64("d", 10, "duration (s)")
	amp := fs.Float64("amp", 0.25, "amplitude (0..1)")
	mic := fs.Bool("mic", false, "also hold the default microphone open (like a voice app)")
	fs.Parse(args)
	if *mic {
		closeMic, err := openMic()
		if err != nil {
			return fmt.Errorf("mic: %w", err) // non-zero exit: the self-test notices
		}
		defer closeMic()
	}
	if err := playTone(*freq, *amp, *dur); err != nil {
		return err
	}
	time.Sleep(time.Duration((*dur + 0.1) * float64(time.Second)))
	stopSound()
	return nil
}

// s1 spawn <exe> <args…>: run a normal child process and wait (models an app
// whose audio comes from a helper). The child's exit status is propagated.
func cmdSpawn(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: s1 spawn <exe> <args…>")
	}
	c := exec.Command(args[0], args[1:]...)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return exitCode(ee.ExitCode())
		}
		return err
	}
	return nil
}

func exclusionFlags(fs *flag.FlagSet) (*string, *bool, *bool) {
	exclude := fs.String("exclude", "", "extra apps to keep out, comma-separated exe names")
	noDefault := fs.Bool("no-default", false, "don't use the built-in voice-app list")
	mic := fs.Bool("mic", true, "keep out apps that use the microphone")
	return exclude, noDefault, mic
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

// friendsHear turns a classified session into the user-facing verdict.
func friendsHear(excluded bool, reason, note string) (bool, string) {
	switch {
	case excluded:
		return false, reason
	case strings.HasPrefix(note, "NOT captured: "):
		return false, strings.TrimPrefix(note, "NOT captured: ")
	case strings.HasPrefix(note, "captured via"):
		return true, note
	}
	return true, ""
}

func cmdApps(e *engine, args []string) error {
	fs := flag.NewFlagSet("apps", flag.ExitOnError)
	exclude, noDefault, mic := exclusionFlags(fs)
	fs.Parse(args)
	if err := e.SetRules(buildRules(*exclude, *noDefault), nil); err != nil {
		return err
	}
	var flags uint32
	if *mic {
		flags |= flagExcludeMic
	}
	s, err := e.ListApps(flags)
	if err != nil {
		return err
	}
	var v statusView
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return err
	}
	fmt.Printf("%-8s %-28s %-9s %s\n", "PID", "APP", "STATE", "FRIENDS HEAR IT?")
	for _, x := range v.Sessions {
		hear, why := friendsHear(x.Excluded, x.Reason, x.Note)
		verdict := "yes"
		if !hear {
			verdict = "no"
		}
		if why != "" {
			verdict += " — " + why
		}
		fmt.Printf("%-8d %-28s %-9s %s\n", x.Pid, x.Exe, x.State, verdict)
	}
	for _, w := range v.Warnings {
		fmt.Println("warning:", w)
	}
	return nil
}

// s1 record -d 20 -o out.wav: capture "what friends would hear" with the real apps you have open.
func cmdRecord(e *engine, args []string) error {
	fs := flag.NewFlagSet("record", flag.ExitOnError)
	dur := fs.Float64("d", 20, "seconds to record")
	out := fs.String("o", "capture.wav", "output WAV")
	mode := fs.String("mode", "include-set", "include-set | exclude-one | endpoint")
	exclude, noDefault, mic := exclusionFlags(fs)
	fs.Parse(args)
	m := -1
	for k, v := range modeNames {
		if v == *mode {
			m = k
		}
	}
	if m < 0 {
		return fmt.Errorf("unknown mode %q", *mode)
	}
	if err := e.SetRules(buildRules(*exclude, *noDefault), nil); err != nil {
		return err
	}
	var flags uint32
	if *mic {
		flags |= flagExcludeMic
	}
	if err := e.Start(m, flags); err != nil {
		return err
	}
	defer e.Stop()
	stopOnInterrupt(e)
	rec, err := capture(e, *dur, func(sec int) {
		if s, err := e.Status(); err == nil {
			printFriendsHear(sec, s)
		}
	}, nil)
	if err != nil {
		return err
	}
	if rec.lostMs > 0 {
		fmt.Printf("note: %d ms of audio were lost because this program stalled (not an isshoni click)\n", rec.lostMs)
	}
	if err := writeWAV(*out, rec.samples); err != nil {
		return err
	}
	fmt.Printf("wrote %s (%.1f s) — listen to it: that is what friends would hear\n", *out, float64(len(rec.samples))/2/sampleRate)
	return nil
}

// captureResult is a recording plus what's needed to line engine events up with it.
type captureResult struct {
	samples []float32
	lostMs  int   // audio lost between chunks because this program stalled (not engine clicks)
	startNs int64 // capture_ns of the first chunk (engine events use the same clock)
}

// capture reads chunks for dur seconds, calling everySecond(sec) and at(t) hooks.
func capture(e *engine, dur float64, everySecond func(int), at func(t float64)) (captureResult, error) {
	chunks := int(dur * 100)
	var res captureResult
	samples := make([]float32, 0, chunks*chunkFrames*2)
	buf := make([]float32, chunkFrames*2)
	var lastTS int64
	gapMs := 0
	for i := 0; i < chunks; i++ {
		n, ts, err := e.Read(buf, 1000)
		if err != nil {
			res.samples, res.lostMs = samples, gapMs
			return res, err
		}
		if i == 0 {
			res.startNs = ts
		}
		if lastTS != 0 && ts-lastTS > 15_000_000 {
			gapMs += int((ts - lastTS - 10_000_000) / 1_000_000)
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
	res.samples, res.lostMs = samples, gapMs
	return res, nil
}

// stopOnInterrupt makes Ctrl+C / closing the console stop the engine and any
// fake apps before exiting.
func stopOnInterrupt(e *engine) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		e.Stop()
		killStarted()
		stopSound()
		os.Exit(130)
	}()
}

func printFriendsHear(sec int, statusJSON string) {
	var v statusView
	if json.Unmarshal([]byte(statusJSON), &v) != nil {
		return
	}
	var hear, out, notCaptured []string
	for _, s := range v.Streams {
		hear = append(hear, fmt.Sprintf("%s[%s %.2f]", s.Label, s.State, s.Peak))
	}
	for _, x := range v.Sessions {
		if hearIt, why := friendsHear(x.Excluded, x.Reason, x.Note); !hearIt {
			if x.Excluded {
				out = append(out, fmt.Sprintf("%s (%s)", x.Exe, why))
			} else {
				notCaptured = append(notCaptured, fmt.Sprintf("%s (%s)", x.Exe, why))
			}
		}
	}
	fmt.Printf("%3ds friends hear: %s\n     kept out: %s\n", sec, orNone(hear), orNone(out))
	if len(notCaptured) > 0 {
		fmt.Printf("     not captured: %s\n", strings.Join(notCaptured, ", "))
	}
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
