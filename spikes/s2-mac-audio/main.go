// Command s2 is the audio part of spike S2 of the isshoni plan: capture "all
// system audio except voice apps" on macOS with a Core Audio process tap and
// prove it with a tone-based self-test. See README.md.
package main

import (
	"fmt"
	"os"
	"runtime"
)

const usage = `s2 — isshoni spike S2 (macOS audio exclusion)

  ./run-selftest.sh [-only a,b] [-mic] [-keep]   run the self-test as isshoni-s2.app (macOS asks once for permission)
  s2 probe                                  macOS version, tap API, output device, permission state
  s2 apps [-exclude id,…]                   list audio processes and how they would be classified
  s2 record -d 20 -o out.wav                capture what friends would hear (default voice-app list + mic detection)
  s2 analyze -present 1000 -absent 440 file.wav   measure tones in a WAV (any OS)
  ./run-app.sh lancheck                     Local Network permission check (connects to this Mac's LAN address)

  internal (used by selftest): s2 tone -f 440 -d 10 [-mic] [-pidfile f]
    s2 spawn [-pidfile f] <exe> <args…>    s2 openapp [-pidfile f] -d 10 <app> <args…>    s2 webtone -f 907 -d 5
`

// The main goroutine stays on the process main thread: AppKit/WebKit (the
// WKWebView playback scenario) must run there. Work is sent with onMain.
func init() { runtime.LockOSThread() }

var mainQueue = make(chan func())

// onMain runs f on the main thread and waits for it.
func onMain(f func()) {
	done := make(chan struct{})
	mainQueue <- func() {
		defer close(done)
		f()
	}
	<-done
}

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	result := make(chan error, 1)
	go func() { result <- run(os.Args[1], os.Args[2:]) }()
	for {
		select {
		case f := <-mainQueue:
			f()
		case err := <-result:
			if c, ok := err.(interface{ ExitStatus() int }); ok { // a requested exit status, not a failure message
				os.Exit(c.ExitStatus())
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			os.Exit(0)
		}
	}
}

func run(cmd string, args []string) error {
	switch cmd {
	case "analyze":
		return cmdAnalyze(args)
	case "probe", "apps", "record", "selftest", "tone", "spawn", "openapp", "webtone", "lancheck":
		return runDarwinCommand(cmd, args)
	}
	fmt.Print(usage)
	return exitCode(2)
}

type exitCode int

func (c exitCode) Error() string   { return fmt.Sprintf("exit status %d", int(c)) }
func (c exitCode) ExitStatus() int { return int(c) }

func writeFile(path string, b []byte) error { return os.WriteFile(path, b, 0o644) }

// defaultVoiceApps is the built-in "always keep out" list (plan: short and
// tested); other voice apps are caught by microphone detection. Bundle IDs
// match at a dot boundary, so "com.hnc.Discord" also covers its helpers
// (com.hnc.Discord.helper, .helper.Renderer, …). Verified on this Mac:
// com.hnc.Discord. The rest are from vendor bundles and need checking in M3.
var defaultVoiceApps = []string{
	"com.hnc.Discord", "com.hnc.DiscordPTB", "com.hnc.DiscordCanary", "dev.vencord.vesktop",
	"com.teamspeak.TeamSpeak3", "com.teamspeak.client",
	"net.sourceforge.mumble.Mumble", "info.mumble.Mumble",
	"us.zoom.xos", "com.microsoft.teams2", "com.microsoft.teams", "com.tinyspeck.slackmacgap",
}
