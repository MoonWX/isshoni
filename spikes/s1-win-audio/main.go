// Command s1 is spike S1 of the isshoni plan: capture "all system audio except
// voice apps" on Windows via the native engine (isshoni_audio.dll) and prove it
// with a tone-based self-test. See README.md.
package main

import (
	"fmt"
	"os"
)

const usage = `s1 — isshoni spike S1 (Windows audio exclusion)

  s1 selftest [-keep]            run all scenarios, print pass/fail, write s1-report.json
  s1 probe                       does this Windows build support per-app loopback?
  s1 apps [-exclude a.exe,…]     list apps playing audio and how they would be classified
  s1 record -d 20 -o out.wav     capture what friends would hear (default voice-app list + mic detection)
  s1 analyze -present 1000 -absent 440 file.wav   measure tones in a WAV (any OS)

  internal (used by selftest): s1 tone -f 440 -d 10 [-mic], s1 spawn <exe> <args…>
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "analyze":
		err = cmdAnalyze(args)
	case "probe", "apps", "record", "selftest", "tone", "spawn":
		err = runWindowsCommand(cmd, args)
	default:
		fmt.Print(usage)
		os.Exit(2)
	}
	if c, ok := err.(interface{ ExitStatus() int }); ok { // a requested exit status, not a failure message
		os.Exit(c.ExitStatus())
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func writeFile(path string, b []byte) error { return os.WriteFile(path, b, 0o644) }

// defaultVoiceApps is the built-in "always keep out" list (plan: short and
// tested); other voice apps are caught by microphone detection.
var defaultVoiceApps = []string{
	"discord.exe", "discordptb.exe", "discordcanary.exe", "vesktop.exe", "equibop.exe",
	"ts3client_win64.exe", "ts3client_win32.exe", "teamspeak.exe",
	"mumble.exe", "zoom.exe", "ms-teams.exe", "teams.exe", "slack.exe",
}
