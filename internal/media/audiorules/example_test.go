package audiorules_test

import (
	"fmt"

	"github.com/MoonWX/isshoni/internal/media/audiorules"
)

func ExampleVoiceApps() {
	for _, a := range audiorules.VoiceApps() {
		fmt.Println(a.ID, a.Windows)
	}
	// Output:
	// discord [discord.exe discordptb.exe discordcanary.exe discorddevelopment.exe]
	// vesktop [vesktop.exe equibop.exe]
	// teamspeak [ts3client_win64.exe ts3client_win32.exe teamspeak.exe]
	// mumble [mumble.exe]
	// zoom [zoom.exe]
	// teams [ms-teams.exe teams.exe]
	// slack [slack.exe]
}

// The desktop app's shell relaunch (03 §13.4) checks whether its parent is a launcher.
func ExampleLaunchers() {
	parent := "steam.exe" // the parent's exe file name, lower case
	fmt.Println(audiorules.Launchers().Contains("windows", parent))
	// Output: true
}

// The viewer rule (05 §5.9) keeps out every browser of the family that plays the user's isshoni viewer.
func ExampleBrowserByFamily() {
	if b, ok := audiorules.BrowserByFamily("firefox"); ok {
		fmt.Println(b.For("windows"))
	}
	_, ok := audiorules.BrowserByFamily("safari")
	fmt.Println(ok)
	// Output:
	// [firefox.exe librewolf.exe waterfox.exe floorp.exe zen.exe]
	// false
}
